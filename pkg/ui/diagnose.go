package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/diagnose"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// supportBundleDeadline bounds the writing of a support bundle, most of which is reading the
// providers' logs.
const supportBundleDeadline = 3 * time.Minute

// supportBundle hands out a cluster's support bundle. It carries the lines of the controller's and
// the providers' logs that name the cluster, which can hold secrets the redaction misses, so it
// needs what the admin kubeconfig does, Sys.Modify on the cluster's pool. Every download is logged
// with who made it.
func (s *Server) supportBundle(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), supportBundleDeadline)
	defer cancel()
	src := &diagnose.Source{Kube: u.kube, ControllerNamespace: s.ClassNamespace, Now: s.now}
	if s.NewClientset != nil {
		cs, err := s.NewClientset()
		if err != nil {
			s.Log.ErrorContext(ctx, "a client for a support bundle", "error", err)
			writeError(w, http.StatusBadGateway, "the management cluster did not answer")
			return
		}
		src.Clientset = cs
	}
	if s.Appliance != nil {
		src.Journal = diagnose.ApplianceJournal
	}
	// Written whole before it goes out, so that a bundle that fails is an error, not half a file.
	var buf bytes.Buffer
	if err := diagnose.Write(ctx, &buf, src, c.Namespace, c.Name); err != nil {
		s.Log.ErrorContext(ctx, "writing a support bundle", "cluster", c.Namespace+"/"+c.Name, "error", err)
		writeError(w, http.StatusBadGateway, "the support bundle could not be written")
		return
	}
	s.Log.InfoContext(ctx, "handed out support bundle", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", diagnose.Name(c.Namespace, c.Name, s.now())))
	_, _ = w.Write(buf.Bytes())
}

// bootLogView is a machine's boot log as the controller last read it, and whether it is asked to
// read it again.
type bootLogView struct {
	Asked bool                 `json:"asked"`
	Log   *provisioner.BootLog `json:"log,omitempty"`
}

// bootLog answers with the boot log the controller last read for a machine. Logs carry secrets
// often enough, so this needs what the admin kubeconfig does.
func (s *Server) bootLog(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	v, err := readBootLog(r.Context(), u.kube, c, r.PathValue("machine"))
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// askBootLog has the controller read a machine's boot log, by adding it to the cluster's request,
// as kubectl would.
func (s *Server) askBootLog(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	name := r.PathValue("machine")
	m := &clusterv1.Machine{}
	err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: name}, m)
	if apierrors.IsNotFound(err) || err == nil && m.Labels[clusterv1.ClusterNameLabel] != c.Name {
		writeError(w, http.StatusNotFound, "the cluster has no machine "+name)
		return
	}
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	c, err = askForBootLog(r.Context(), u.kube, client.ObjectKeyFromObject(c), name)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "asked for a boot log", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "machine", name)
	v, err := readBootLog(r.Context(), u.kube, c, name)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

// askForBootLog adds the machine to the cluster's boot-log request unless it is on it already, and
// returns the cluster as it then stands.
func askForBootLog(ctx context.Context, kube client.Client, key client.ObjectKey, machine string) (*clusterv1.Cluster, error) {
	c := &clusterv1.Cluster{}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := kube.Get(ctx, key, c); err != nil {
			return fmt.Errorf("reading the cluster: %w", err)
		}
		asked := provisioner.BootLogsAsked(c)
		if slices.Contains(asked, machine) {
			return nil
		}
		patch := client.MergeFromWithOptions(c.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations[provisioner.BootLogRequestAnnotation] = strings.Join(append(asked, machine), ",")
		if err := kube.Patch(ctx, c, patch); err != nil {
			return fmt.Errorf("asking for the boot log of %s: %w", machine, err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cluster %s: %w", key, err)
	}
	return c, nil
}

func readBootLog(ctx context.Context, kube client.Client, c *clusterv1.Cluster, machine string) (bootLogView, error) {
	v := bootLogView{Asked: slices.Contains(provisioner.BootLogsAsked(c), machine)}
	cm := &corev1.ConfigMap{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: provisioner.BootLogsConfigMap(c.Name)}, cm)
	if apierrors.IsNotFound(err) {
		return v, nil
	}
	if err != nil {
		return v, fmt.Errorf("reading the boot logs: %w", err)
	}
	if raw, ok := cm.Data[machine]; ok {
		var log provisioner.BootLog
		if err := json.Unmarshal([]byte(raw), &log); err == nil {
			v.Log = &log
		}
	}
	return v, nil
}
