package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// etcdStoreTimeout bounds reading a cluster's snapshots from its target; a download takes as
// long as it takes.
const etcdStoreTimeout = 30 * time.Second

// etcdSnapshotsView is what the backups view shows of a cluster's etcd snapshots.
type etcdSnapshotsView struct {
	Enabled  bool   `json:"enabled"`
	Target   string `json:"target,omitempty"`
	Schedule string `json:"schedule,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
	Keep     int    `json:"keep,omitempty"`
	// DistributionUpload is set while RKE2's or k3s's servers upload snapshots of their own to the
	// target, into the <distribution>-etcd folder beside these.
	DistributionUpload bool `json:"distributionUpload,omitempty"`
	// Last is the last snapshot the controller took, and Condition how its last try went.
	Last      *provisioner.EtcdSnapshot `json:"last,omitempty"`
	Condition *conditionView            `json:"condition,omitempty"`
	// Requested is set while a snapshot asked for has not been taken.
	Requested bool `json:"requested"`
	// Snapshots are those in the target, newest first; Message says why they could not be read.
	Snapshots []provisioner.EtcdSnapshot `json:"snapshots"`
	Message   string                     `json:"message,omitempty"`
	// Restore is the cluster's last restore, and Restoring is set while one runs.
	Restore   *provisioner.EtcdRestoreStatus `json:"restore,omitempty"`
	Restoring bool                           `json:"restoring"`
	// Automatic is set when the controller restores through the guest agent; otherwise the
	// interface shows the steps to restore by hand.
	Automatic   bool `json:"automatic"`
	CanSnapshot bool `json:"canSnapshot"`
	CanRestore  bool `json:"canRestore"`
}

// etcdSnapshots lists a cluster's etcd snapshots in its target, with how the last went.
func (s *Server) etcdSnapshots(w http.ResponseWriter, r *http.Request, u user) {
	c, access, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	settings, _ := provisioner.EtcdSnapshotsOf(c)
	view := etcdSnapshotsView{
		Enabled: settings.Enabled, Target: settings.Target, Schedule: settings.Schedule, TimeZone: settings.TimeZone, Keep: settings.Keep,
		DistributionUpload: settings.Enabled && settings.DistributionUpload, Restoring: provisioner.RestoreInProgress(c),
		Snapshots: []provisioner.EtcdSnapshot{}, CanSnapshot: access.held.Has(privChange), CanRestore: access.held.Has(privKubeconfig),
	}
	_, view.Requested = c.Annotations[provisioner.EtcdSnapshotRequestAnnotation]
	if last, ok := provisioner.LastEtcdSnapshot(c); ok {
		view.Last = &last
	}
	if cond := meta.FindStatusCondition(c.Status.Conditions, provisioner.EtcdSnapshotCondition); cond != nil {
		view.Condition = &conditionView{
			Type: cond.Type, Status: string(cond.Status), Reason: cond.Reason, Message: cond.Message, Since: cond.LastTransitionTime.Time,
		}
	}
	if st, ok := provisioner.EtcdRestoreStatusOf(c); ok {
		view.Restore = &st
	}
	ctx, cancel := context.WithTimeout(r.Context(), etcdStoreTimeout)
	defer cancel()
	automatic, err := s.automaticRestore(ctx, u, c)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	view.Automatic = automatic
	if settings.Target == "" {
		writeJSON(w, http.StatusOK, view)
		return
	}
	if c.Namespace != s.Namespace {
		view.Message = "etcd snapshots show only for clusters in namespace " + s.Namespace + "."
		writeJSON(w, http.StatusOK, view)
		return
	}
	t, err := s.etcdTarget(ctx, u, c, settings.Target)
	if err == nil {
		view.Snapshots, err = provisioner.ListEtcdSnapshots(ctx, t.Store, t.Folder)
	}
	if err != nil {
		s.Log.InfoContext(r.Context(), "reading a cluster's etcd snapshots", "cluster", c.Namespace+"/"+c.Name, "error", err)
		view.Snapshots = []provisioner.EtcdSnapshot{}
		view.Message = "Target " + settings.Target + " could not be read: " + err.Error()
	}
	writeJSON(w, http.StatusOK, view)
}

// etcdTarget opens a cluster's snapshot folder with its own credential and keys, never making them.
func (s *Server) etcdTarget(ctx context.Context, u user, c *clusterv1.Cluster, target string) (*provisioner.ClusterTarget, error) {
	open := s.OpenStore
	if open == nil {
		open = provisioner.OpenTarget
	}
	targets := &provisioner.ClusterTargets{Client: u.kube, ClassNamespace: s.ClassNamespace, OpenStore: open}
	t, err := targets.Open(ctx, c, target, false)
	if err != nil {
		return nil, fmt.Errorf("opening target %s: %w", target, err)
	}
	return t, nil
}

// automaticRestore tells whether the controller restores the cluster's etcd through the guest
// agent, as its class says.
func (s *Server) automaticRestore(ctx context.Context, u user, c *clusterv1.Cluster) (bool, error) {
	class := &clusterv1.ClusterClass{}
	if err := u.kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: c.Spec.Topology.ClassRef.Name}, class); err != nil {
		return false, fmt.Errorf("reading the cluster's class: %w", err)
	}
	return class.Annotations[provisioner.AutomaticRestoreAnnotation] == "true", nil
}

// takeEtcdSnapshot asks the controller for a snapshot at once.
func (s *Server) takeEtcdSnapshot(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return
	}
	if settings, _ := provisioner.EtcdSnapshotsOf(c); !settings.Enabled {
		writeError(w, http.StatusConflict, "etcd snapshots are off for this cluster")
		return
	}
	if provisioner.RestoreInProgress(c) {
		writeError(w, http.StatusConflict, "an etcd restore is running")
		return
	}
	req := provisioner.EtcdSnapshotRequest{By: u.ticket.Username, At: time.Now().UTC()}
	if !s.annotate(w, r, u, c, provisioner.EtcdSnapshotRequestAnnotation, req) {
		return
	}
	s.Log.InfoContext(r.Context(), "asked for an etcd snapshot", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name)
	writeJSON(w, http.StatusAccepted, req)
}

// downloadEtcdSnapshot hands out a snapshot decrypted. It holds every Secret of the cluster, so it
// needs Sys.Modify on the cluster's pool, as the admin kubeconfig does.
func (s *Server) downloadEtcdSnapshot(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	name := r.PathValue("snapshot")
	target, ok := snapshotTarget(w, c, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), etcdStoreTimeout)
	t, err := s.etcdTarget(ctx, u, c, target)
	var plain io.ReadCloser
	if err == nil {
		plain, err = t.OpenEtcdSnapshot(ctx, name)
	}
	cancel()
	if err != nil {
		s.snapshotError(w, r, c, err)
		return
	}
	defer func() { _ = plain.Close() }()
	s.Log.InfoContext(r.Context(), "downloading an etcd snapshot", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "snapshot", name)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+c.Name+"-"+strings.TrimSuffix(name, ".age")+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, plain); err != nil {
		s.Log.InfoContext(r.Context(), "an etcd snapshot download ended early", "cluster", c.Namespace+"/"+c.Name, "snapshot", name, "error", err)
	}
}

// etcdRestorePlan writes out the steps that restore the cluster from a snapshot by hand.
func (s *Server) etcdRestorePlan(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	snapshot, ok := s.findSnapshot(w, r, u, c, r.PathValue("snapshot"))
	if !ok {
		return
	}
	plan, err := provisioner.PlanEtcdRestore(r.Context(), u.kube, c, snapshot)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// etcdRestoreRequest is the snapshot a restore is asked for, and the cluster's name to confirm it.
type etcdRestoreRequest struct {
	Snapshot string `json:"snapshot"`
	Confirm  string `json:"confirm"`
}

// restoreEtcd asks the controller to restore the cluster's etcd from a snapshot, through the guest
// agent. It needs Sys.Modify on the cluster's pool: the restore takes every object of the cluster
// back to the snapshot.
func (s *Server) restoreEtcd(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	var req etcdRestoreRequest
	if !readJSON(w, r, &req) || !confirmed(w, c, req.Confirm) {
		return
	}
	if provisioner.RestoreInProgress(c) {
		writeError(w, http.StatusConflict, "an etcd restore is already running")
		return
	}
	automatic, err := s.automaticRestore(r.Context(), u, c)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	if !automatic {
		writeError(w, http.StatusConflict, "automatic restore is off. Restore by hand as the steps say")
		return
	}
	snapshot, ok := s.findSnapshot(w, r, u, c, req.Snapshot)
	if !ok {
		return
	}
	if err := provisioner.Restorable(c, snapshot); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	settings, _ := provisioner.EtcdSnapshotsOf(c)
	restore := provisioner.EtcdRestoreRequest{Snapshot: snapshot.Name, Target: settings.Target, By: u.ticket.Username, At: time.Now().UTC()}
	if !s.annotate(w, r, u, c, provisioner.EtcdRestoreAnnotation, restore) {
		return
	}
	s.Log.InfoContext(r.Context(), "asked for an etcd restore", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "snapshot", snapshot.Name)
	writeJSON(w, http.StatusAccepted, restore)
}

// findSnapshot reads one of the cluster's snapshots from its target and checks the cluster can be
// restored from it.
func (s *Server) findSnapshot(w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster, name string) (*provisioner.EtcdSnapshot, bool) {
	target, ok := snapshotTarget(w, c, name)
	if !ok {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), etcdStoreTimeout)
	defer cancel()
	t, err := s.etcdTarget(ctx, u, c, target)
	var all []provisioner.EtcdSnapshot
	if err == nil {
		all, err = provisioner.ListEtcdSnapshots(ctx, t.Store, t.Folder)
	}
	if err != nil {
		s.snapshotError(w, r, c, err)
		return nil, false
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], true
		}
	}
	writeError(w, http.StatusNotFound, "no such snapshot")
	return nil, false
}

// snapshotTarget is the target a cluster's snapshots are in, once name is a snapshot's.
func snapshotTarget(w http.ResponseWriter, c *clusterv1.Cluster, name string) (string, bool) {
	if !provisioner.ValidSnapshotName(name) {
		writeError(w, http.StatusNotFound, "no such snapshot")
		return "", false
	}
	settings, _ := provisioner.EtcdSnapshotsOf(c)
	if settings.Target == "" {
		writeError(w, http.StatusConflict, "etcd snapshots have no target")
		return "", false
	}
	return settings.Target, true
}

func (s *Server) snapshotError(w http.ResponseWriter, r *http.Request, c *clusterv1.Cluster, err error) {
	if errors.Is(err, provisioner.ErrNoObject) {
		writeError(w, http.StatusNotFound, "no such snapshot")
		return
	}
	s.Log.InfoContext(r.Context(), "reading a cluster's etcd snapshot", "cluster", c.Namespace+"/"+c.Name, "error", err)
	writeError(w, http.StatusBadGateway, err.Error())
}

// annotate sets one of Kubehenge's requests on the cluster, as JSON.
func (s *Server) annotate(w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster, key string, value any) bool {
	raw, err := json.Marshal(value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the request could not be encoded")
		return false
	}
	patch := client.MergeFromWithOptions(c.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[key] = string(raw)
	if err := u.kube.Patch(r.Context(), c, patch); err != nil {
		s.kubeError(w, r, err)
		return false
	}
	return true
}
