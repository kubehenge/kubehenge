package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	// maxImport bounds an export bundle, which carries the management cluster's encrypted state.
	maxImport = 64 << 20
	// specEvery is how often the clusters' specs are compared with those stored.
	specEvery = time.Minute
)

// dismissBackupWarning silences the warning that the management cluster's etcd is not backed up,
// for those who may set the backups up.
func (s *Server) dismissBackupWarning(w http.ResponseWriter, r *http.Request, u user) {
	if !s.mayBackUpManagement(w, r, u) {
		return
	}
	switch err := s.Appliance.DismissEtcdBackupWarning(u.ticket.Username); {
	case errors.Is(err, appliance.ErrNoWarning):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Warn("management cluster backup warning dismissed", "user", u.ticket.Username)
	writeJSON(w, http.StatusOK, s.managementBackupsOf())
}

// exportManagement hands out the export bundle, encrypted with a passphrase the user sets.
func (s *Server) exportManagement(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Passphrase       string `json:"passphrase"`
		ExportPassphrase string `json:"exportPassphrase"`
	}
	if !readJSON(w, r, &req) || !s.mayBackUpManagement(w, r, u) {
		return
	}
	done, ok := s.reservePassphrase(w, u)
	if !ok {
		return
	}
	bundle, err := s.Appliance.ExportState(r.Context(), req.Passphrase, req.ExportPassphrase)
	if errors.Is(err, appliance.ErrNoEtcdBackups) {
		done(false)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if !checkPassphrase(w, done, err) {
		return
	}
	s.Log.Warn("management cluster state exported", "user", u.ticket.Username)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="kubehenge-export.age"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(bundle)
}

// importManagement puts an export into an appliance with no management cluster, so that it can
// restore the management cluster's etcd from the target.
func (s *Server) importManagement(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Bundle           []byte `json:"bundle"`
		ExportPassphrase string `json:"exportPassphrase"`
	}
	if !s.mayBackUpManagement(w, r, u) {
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImport))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request: "+decodeProblem(err))
		return
	}
	done, ok := s.reservePassphrase(w, u)
	if !ok {
		return
	}
	err := s.Appliance.ImportState(r.Context(), req.Bundle, req.ExportPassphrase)
	done(errors.Is(err, appliance.ErrExportPassphrase))
	switch {
	case errors.Is(err, appliance.ErrExportPassphrase):
		writeError(w, http.StatusForbidden, err.Error())
		return
	case jobConflict(err), errors.Is(err, appliance.ErrHasState):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.Log.Warn("management cluster state imported", "user", u.ticket.Username)
	writeJSON(w, http.StatusOK, s.managementBackupsOf())
}

// recreateManagement makes the management cluster's VMs again and restores a snapshot into them.
func (s *Server) recreateManagement(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if !readJSON(w, r, &req) || !s.mayBackUpManagement(w, r, u) {
		return
	}
	a := s.Appliance
	saved := a.SavedSettings()
	if saved == nil {
		writeError(w, http.StatusConflict, "there is no management cluster to recreate")
		return
	}
	privileges := slices.Clone(setupPrivileges)
	if saved.ProxmoxHA {
		privileges = append(privileges, haPrivilege)
	}
	if saved.EtcdRestore {
		privileges = append(privileges, restorePrivilege)
	}
	if !s.require(w, r, u, "/", privileges...) {
		return
	}
	done, ok := s.reservePassphrase(w, u)
	if !ok {
		return
	}
	// The run needs the whole of a ticket's life, so it gets a new one rather than the session's.
	ticket, err := s.Accounts.Renew(r.Context(), u.ticket)
	if err != nil {
		done(false)
		s.proxmoxError(w, r, err)
		return
	}
	err = a.RecreateManagement(r.Context(), r.PathValue("name"), req.Passphrase, ticket, s.applied(ticket))
	if jobConflict(err) {
		done(false)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if !checkPassphrase(w, done, err) {
		return
	}
	s.Log.Warn("management cluster recreation started", "user", u.ticket.Username, "snapshot", r.PathValue("name"))
	writeJSON(w, http.StatusAccepted, a.Current())
}

// clusterSpec hands out a cluster's spec as the backup target keeps it, to those who may see the
// cluster. A Secret's values are in it sealed with the appliance's recovery key, and the spec's
// first line says so.
func (s *Server) clusterSpec(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	objects, err := provisioner.ClusterSpecObjects(r.Context(), u.kube, c)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	raw, err := provisioner.ClusterSpecYAML(c, objects, s.specSealer())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", c.Name+".cluster.yaml"))
	_, _ = io.Copy(w, bytes.NewReader(raw))
}

// specSealer seals Secrets' values with the appliance's recovery key. Without an appliance a spec
// holds no value.
func (s *Server) specSealer() provisioner.SecretSealer {
	if s.Appliance == nil {
		return nil
	}
	return s.Appliance.SealSpecSecret
}

// createRequestOf is the wizard's request for the cluster an exported spec describes.
func createRequestOf(c *clusterv1.Cluster) createRequest {
	t := &c.Spec.Topology
	req := createRequest{
		Namespace: c.Namespace, Name: c.Name, Class: t.ClassRef.Name, Version: t.Version,
		Variables: map[string]json.RawMessage{},
		Pods:      c.Spec.ClusterNetwork.Pods.CIDRBlocks, Services: c.Spec.ClusterNetwork.Services.CIDRBlocks,
	}
	if t.ControlPlane.Replicas != nil {
		req.ControlPlaneReplicas = *t.ControlPlane.Replicas
	}
	for _, v := range t.Variables {
		req.Variables[v.Name] = json.RawMessage(v.Value.Raw)
	}
	if len(t.Workers.MachineDeployments) > 0 {
		md := &t.Workers.MachineDeployments[0]
		if md.Replicas != nil {
			req.WorkerReplicas = *md.Replicas
		}
		minimum, errMin := strconv.ParseInt(md.Metadata.Annotations[provisioner.AutoscalerMinAnnotation], 10, 32)
		maximum, errMax := strconv.ParseInt(md.Metadata.Annotations[provisioner.AutoscalerMaxAnnotation], 10, 32)
		if errMin == nil && errMax == nil {
			req.WorkerAutoscaling = &poolAutoscaling{Min: int32(minimum), Max: int32(maximum)}
		}
	}
	return req
}

// storeClusterSpecs keeps every cluster's spec in the management backups' target, after a change
// to it. A spec whose bytes are the stored ones is not written again.
func (s *Server) storeClusterSpecs(ctx context.Context) {
	if s.Appliance == nil {
		return
	}
	stored := map[string]string{}
	for {
		s.storeSpecsOnce(ctx, stored)
		select {
		case <-ctx.Done():
			return
		case <-time.After(specEvery):
		}
	}
}

// storeSpecsOnce writes the specs that differ from stored, which it updates.
func (s *Server) storeSpecsOnce(ctx context.Context, stored map[string]string) {
	kube := s.currentKube()
	if kube == nil || s.Appliance.EtcdBackups().Settings == nil {
		return
	}
	var list clusterv1.ClusterList
	if err := kube.List(ctx, &list); err != nil {
		s.Log.Warn("listing the clusters to store their specs", "error", err)
		return
	}
	for i := range list.Items {
		c := &list.Items[i]
		if !provisioner.Owns(c, s.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
			continue
		}
		key := c.Namespace + "/" + c.Name
		objects, err := provisioner.ClusterSpecObjects(ctx, kube, c)
		if err != nil {
			s.Log.Warn("reading a cluster's spec", "cluster", key, "error", err)
			continue
		}
		sum, err := provisioner.ClusterSpecSum(c, objects)
		if err != nil {
			s.Log.Warn("reading a cluster's spec", "cluster", key, "error", err)
			continue
		}
		sum += "|" + strings.Join(s.Appliance.SpecRecipients(), ",")
		if stored[key] == sum {
			continue
		}
		raw, err := provisioner.ClusterSpecYAML(c, objects, s.specSealer())
		if err != nil {
			s.Log.Warn("sealing a cluster's spec", "cluster", key, "error", err)
			continue
		}
		if err := s.Appliance.StoreClusterSpec(ctx, c.Namespace, c.Name, raw); err != nil {
			s.Log.Warn("storing a cluster's spec", "cluster", key, "error", err)
			continue
		}
		stored[key] = sum
	}
}

// unownedVMs reads the controller's list of the VMs in Kubehenge's pools that no Cluster owns.
func (s *Server) unownedVMs(ctx context.Context, kube client.Client) *provisioner.UnownedVMs {
	cm := &corev1.ConfigMap{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: provisioner.UnownedVMsName}, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			s.Log.WarnContext(ctx, "reading the VMs no cluster owns", "error", err)
		}
		return nil
	}
	u, err := provisioner.ParseUnownedVMs(cm)
	if err != nil {
		s.Log.WarnContext(ctx, "reading the VMs no cluster owns", "error", err)
		return nil
	}
	return &u
}
