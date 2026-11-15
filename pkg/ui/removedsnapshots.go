package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const volumeWaking = "the backup volume is waking. Try again in a minute"

// removedEtcdSnapshots lists the removed clusters of a namespace whose snapshots the backup volume
// keeps, those the user may see in the pool the cluster had.
func (s *Server) removedEtcdSnapshots(w http.ResponseWriter, r *http.Request, u user) {
	ctx, cancel := context.WithTimeout(r.Context(), etcdStoreTimeout)
	defer cancel()
	volume, err := s.snapshotVolume()
	var store provisioner.VolumeStore
	holder := "the list of removed clusters in " + r.PathValue("namespace")
	if err == nil {
		store, err = volume.Open(ctx, holder)
	}
	switch {
	case errors.Is(err, provisioner.ErrVolumeAsleep):
		writeError(w, http.StatusConflict, volumeWaking)
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer func() { _ = volume.Done(context.WithoutCancel(ctx), holder) }()
	removed, err := provisioner.RemovedClusters(ctx, u.kube, store, r.PathValue("namespace"))
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	seen := []provisioner.RemovedCluster{}
	for _, c := range removed {
		pool, err := s.poolPath(c.Namespace, c.Name)
		if err != nil {
			continue
		}
		held, err := s.Accounts.Privileges(ctx, u.ticket, pool)
		if err != nil {
			s.proxmoxError(w, r, err)
			return
		}
		if held.Has(privSee) {
			seen = append(seen, c)
		}
	}
	writeJSON(w, http.StatusOK, seen)
}

// deleteRemovedEtcdSnapshots deletes a removed cluster's snapshots on the backup volume and its
// backup keys. It takes the privilege a deletion of the cluster took, and the typed name.
func (s *Server) deleteRemovedEtcdSnapshots(w http.ResponseWriter, r *http.Request, u user) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	pool, err := s.poolPath(namespace, name)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such cluster")
		return
	}
	held, err := s.Accounts.Privileges(r.Context(), u.ticket, pool)
	if err != nil {
		s.proxmoxError(w, r, err)
		return
	}
	if !held.Has(privSee) {
		writeError(w, http.StatusNotFound, "no such cluster")
		return
	}
	if !held[privChange] || !u.namespaces.has(namespace) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("%s needs %s on %s", u.ticket.Username, privChange, pool))
		return
	}
	var req confirmRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Confirm != name {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("confirm with the cluster's name, %q", name))
		return
	}
	err = u.kube.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &clusterv1.Cluster{})
	if err == nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("cluster %s still exists, and only a removed cluster's snapshots are deleted here. Delete the cluster with its volume snapshots instead", name))
		return
	}
	if !apierrors.IsNotFound(err) {
		s.kubeError(w, r, err)
		return
	}
	volume, err := s.snapshotVolume()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	n, err := provisioner.DeleteVolumeSnapshots(r.Context(), volume, namespace, name)
	switch {
	case errors.Is(err, provisioner.ErrVolumeAsleep):
		writeError(w, http.StatusConflict, volumeWaking)
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	keys := &corev1.Secret{}
	keys.Namespace, keys.Name = namespace, provisioner.BackupKeysSecretName(name)
	if err := client.IgnoreNotFound(u.kube.Delete(r.Context(), keys)); err != nil {
		s.kubeError(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "deleted a removed cluster's etcd snapshots", "user", u.ticket.Username, "cluster", namespace+"/"+name, "snapshots", n)
	writeJSON(w, http.StatusOK, struct {
		Deleted int `json:"deleted"`
	}{n})
}
