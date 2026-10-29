package ui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// Velero's resources the backups view reads and makes.
var (
	veleroV1       = schema.GroupVersion{Group: "velero.io", Version: "v1"}
	veleroBackups  = veleroV1.WithResource("backups")
	veleroRestores = veleroV1.WithResource("restores")
	veleroSchedule = veleroV1.WithResource("schedules")
	veleroLocation = veleroV1.WithResource("backupstoragelocations")
	veleroVolumes  = veleroV1.WithResource("podvolumerestores")
)

const (
	// veleroTimeout bounds each request to a workload cluster's API, so that a cluster that does
	// not answer holds up the view for no longer.
	veleroTimeout = 10 * time.Second
	// veleroListLimit bounds what one list reads: with a backup a day kept a week, far more than
	// a cluster holds.
	veleroListLimit = 500
	// requestedBy on a backup or restore names the Proxmox VE user who started it.
	requestedBy = "kubehenge.dev/requested-by"
	// maxRestoreNamespaces bounds the namespaces one restore names.
	maxRestoreNamespaces = 100
)

// Velero's phases of a backup or restore that the interface tells apart.
const (
	veleroCompleted = "Completed"
	veleroDeleting  = "Deleting"
)

// backupView is a backup as the view lists it.
type backupView struct {
	Name      string     `json:"name"`
	Phase     string     `json:"phase"`
	Scheduled bool       `json:"scheduled"`
	By        string     `json:"by,omitempty"`
	Started   *time.Time `json:"started,omitempty"`
	Completed *time.Time `json:"completed,omitempty"`
	Expires   *time.Time `json:"expires,omitempty"`
	Items     int64      `json:"items"`
	Total     int64      `json:"total"`
	Warnings  int64      `json:"warnings"`
	Errors    int64      `json:"errors"`
	Failure   string     `json:"failure,omitempty"`
}

// restoreView is a restore with how far its volumes have come.
type restoreView struct {
	Name       string     `json:"name"`
	Backup     string     `json:"backup"`
	Phase      string     `json:"phase"`
	By         string     `json:"by,omitempty"`
	Namespaces []string   `json:"namespaces,omitempty"`
	Started    *time.Time `json:"started,omitempty"`
	Completed  *time.Time `json:"completed,omitempty"`
	Items      int64      `json:"items"`
	Total      int64      `json:"total"`
	Warnings   int64      `json:"warnings"`
	Errors     int64      `json:"errors"`
	Failure    string     `json:"failure,omitempty"`
	// Volumes counts the pod volumes being restored, those done and the bytes of them all.
	Volumes     int   `json:"volumes"`
	VolumesDone int   `json:"volumesDone"`
	BytesDone   int64 `json:"bytesDone"`
	BytesTotal  int64 `json:"bytesTotal"`
}

// backupsView is what the backups view shows of a cluster.
type backupsView struct {
	Enabled bool   `json:"enabled"`
	Target  string `json:"target,omitempty"`
	// Reachable is false while Velero cannot be read, with the reason in Message.
	Reachable bool   `json:"reachable"`
	Message   string `json:"message,omitempty"`
	// Location is the phase of the backup location: Available once Velero reaches the target.
	Location        string        `json:"location,omitempty"`
	LocationMessage string        `json:"locationMessage,omitempty"`
	LastBackup      *time.Time    `json:"lastBackup,omitempty"`
	Backups         []backupView  `json:"backups"`
	Restores        []restoreView `json:"restores"`
	CanBackUp       bool          `json:"canBackUp"`
	CanRestore      bool          `json:"canRestore"`
}

// velero reaches the cluster's Velero as one of its backup identities, or says why it cannot yet.
func (s *Server) velero(ctx context.Context, u user, c *clusterv1.Cluster, id provisioner.Identity) (dynamic.Interface, string, error) {
	w, ranOut, err := s.reach(ctx, u.kube, c, id)
	switch {
	case err != nil:
		return nil, "", err
	case w != nil:
		return w.Dynamic, "", nil
	case !ranOut.IsZero():
		return nil, "Kubehenge has not reached the cluster for a while; its access to Velero has run out.", nil
	}
	found, err := backupCredentialStored(ctx, u.kube, c)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "Velero waits for the target's access key: store one in the cluster's Options, or have an administrator store one for every cluster.", nil
	}
	return nil, "Velero is being set up in the cluster.", nil
}

func (s *Server) clusterBackups(w http.ResponseWriter, r *http.Request, u user) {
	c, access, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	b := backupsOf(c)
	view := backupsView{Enabled: b.Enabled, Target: b.Target, Backups: []backupView{}, Restores: []restoreView{}}
	if !b.Enabled {
		writeJSON(w, http.StatusOK, view)
		return
	}
	// The interface reads Secrets only in its own namespace, where it makes its clusters.
	if c.Namespace != s.Namespace {
		view.Message = "The interface shows the backups only of the clusters in its own namespace."
		writeJSON(w, http.StatusOK, view)
		return
	}
	view.CanBackUp = access.held.Has(privChange)
	view.CanRestore = access.held.Has(privKubeconfig)
	ctx, cancel := context.WithTimeout(r.Context(), veleroTimeout)
	defer cancel()
	v, why, err := s.velero(ctx, u, c, provisioner.IdentityBackups)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	if v == nil {
		view.Message = why
		writeJSON(w, http.StatusOK, view)
		return
	}
	if err := readVelero(ctx, v, &view); err != nil {
		s.Log.InfoContext(r.Context(), "reading a cluster's backups", "cluster", c.Namespace+"/"+c.Name, "error", err)
		view.Message = "The cluster's API did not answer, or Velero is not running in it yet."
		writeJSON(w, http.StatusOK, view)
		return
	}
	view.Reachable = true
	writeJSON(w, http.StatusOK, view)
}

// backupCredentialStored tells whether one of the Secrets the controller takes the store's keys from
// exists; whether it holds the right keys is the controller's to say.
func backupCredentialStored(ctx context.Context, kube client.Client, c *clusterv1.Cluster) (bool, error) {
	// A Secret of the owner's own is in the workload cluster, where the interface does not look.
	if backupsOf(c).ExistingSecret != "" {
		return true, nil
	}
	for _, name := range provisioner.BackupCredentialNames(c.Name) {
		err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, &corev1.Secret{})
		if err == nil {
			return true, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("reading Secret %s: %w", name, err)
		}
	}
	return false, nil
}

// backupsOf reads a cluster's backups variable; unset or unreadable, they are off.
func backupsOf(c *clusterv1.Cluster) provisioner.Backups {
	var b provisioner.Backups
	if raw := variableValue(c, "backups"); raw != nil && json.Unmarshal(raw, &b) != nil {
		return provisioner.Backups{}
	}
	return b
}

// readVelero fills the view from Velero's resources.
func readVelero(ctx context.Context, v dynamic.Interface, view *backupsView) error {
	ns := provisioner.VeleroNamespace
	location, err := v.Resource(veleroLocation).Namespace(ns).Get(ctx, "default", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading the backup location: %w", err)
	}
	if err == nil {
		view.Location, _, _ = unstructured.NestedString(location.Object, "status", "phase")
		view.LocationMessage, _, _ = unstructured.NestedString(location.Object, "status", "message")
	}
	schedule, err := v.Resource(veleroSchedule).Namespace(ns).Get(ctx, provisioner.VeleroSchedule, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading the schedule: %w", err)
	}
	if err == nil {
		view.LastBackup = timeAt(schedule, "status", "lastBackup")
	}
	backups, err := v.Resource(veleroBackups).Namespace(ns).List(ctx, metav1.ListOptions{Limit: veleroListLimit})
	if err != nil {
		return fmt.Errorf("listing the backups: %w", err)
	}
	for i := range backups.Items {
		view.Backups = append(view.Backups, backupOf(&backups.Items[i]))
	}
	restores, err := v.Resource(veleroRestores).Namespace(ns).List(ctx, metav1.ListOptions{Limit: veleroListLimit})
	if err != nil {
		return fmt.Errorf("listing the restores: %w", err)
	}
	volumes, err := v.Resource(veleroVolumes).Namespace(ns).List(ctx, metav1.ListOptions{Limit: veleroListLimit})
	if err != nil {
		return fmt.Errorf("listing the volume restores: %w", err)
	}
	for i := range restores.Items {
		view.Restores = append(view.Restores, restoreOf(&restores.Items[i], volumes.Items))
	}
	newest := func(a, b *time.Time, an, bn string) int {
		if c := cmpTime(b, a); c != 0 {
			return c
		}
		return strings.Compare(bn, an)
	}
	slices.SortFunc(view.Backups, func(a, b backupView) int { return newest(a.Started, b.Started, a.Name, b.Name) })
	slices.SortFunc(view.Restores, func(a, b restoreView) int { return newest(a.Started, b.Started, a.Name, b.Name) })
	return nil
}

// cmpTime orders times, an unset one, not yet started, after every other.
func cmpTime(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return a.Compare(*b)
}

func timeAt(o *unstructured.Unstructured, fields ...string) *time.Time {
	s, _, _ := unstructured.NestedString(o.Object, fields...)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

func intAt(o *unstructured.Unstructured, fields ...string) int64 {
	n, _, _ := unstructured.NestedFieldNoCopy(o.Object, fields...)
	switch n := n.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

func backupOf(o *unstructured.Unstructured) backupView {
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	failure, _, _ := unstructured.NestedString(o.Object, "status", "failureReason")
	return backupView{
		Name: o.GetName(), Phase: cmp.Or(phase, "New"),
		Scheduled: o.GetLabels()["velero.io/schedule-name"] != "",
		By:        o.GetAnnotations()[requestedBy],
		Started:   timeAt(o, "status", "startTimestamp"), Completed: timeAt(o, "status", "completionTimestamp"),
		Expires: timeAt(o, "status", "expiration"),
		Items:   intAt(o, "status", "progress", "itemsBackedUp"), Total: intAt(o, "status", "progress", "totalItems"),
		Warnings: intAt(o, "status", "warnings"), Errors: intAt(o, "status", "errors"), Failure: failure,
	}
}

func restoreOf(o *unstructured.Unstructured, volumes []unstructured.Unstructured) restoreView {
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	failure, _, _ := unstructured.NestedString(o.Object, "status", "failureReason")
	backup, _, _ := unstructured.NestedString(o.Object, "spec", "backupName")
	namespaces, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "includedNamespaces")
	if len(namespaces) == 1 && namespaces[0] == "*" {
		namespaces = nil
	}
	view := restoreView{
		Name: o.GetName(), Backup: backup, Phase: cmp.Or(phase, "New"), By: o.GetAnnotations()[requestedBy],
		Namespaces: namespaces,
		Started:    timeAt(o, "status", "startTimestamp"), Completed: timeAt(o, "status", "completionTimestamp"),
		Items: intAt(o, "status", "progress", "itemsRestored"), Total: intAt(o, "status", "progress", "totalItems"),
		Warnings: intAt(o, "status", "warnings"), Errors: intAt(o, "status", "errors"), Failure: failure,
	}
	for i := range volumes {
		pv := &volumes[i]
		if pv.GetLabels()["velero.io/restore-name"] != o.GetName() {
			continue
		}
		view.Volumes++
		if p, _, _ := unstructured.NestedString(pv.Object, "status", "phase"); p == veleroCompleted {
			view.VolumesDone++
		}
		view.BytesDone += intAt(pv, "status", "progress", "bytesDone")
		view.BytesTotal += intAt(pv, "status", "progress", "totalBytes")
	}
	return view
}

// backUp starts a backup at once, as the schedule would make one.
func (s *Server) backUp(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return
	}
	if !backupsOf(c).Enabled {
		writeError(w, http.StatusConflict, "the cluster does not back up")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), veleroTimeout)
	defer cancel()
	v, why, err := s.velero(ctx, u, c, provisioner.IdentityBackups)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	if v == nil {
		writeError(w, http.StatusConflict, why)
		return
	}
	schedule, err := v.Resource(veleroSchedule).Namespace(provisioner.VeleroNamespace).Get(ctx, provisioner.VeleroSchedule, metav1.GetOptions{})
	if err != nil {
		s.veleroError(w, r, c, err)
		return
	}
	template, _, _ := unstructured.NestedMap(schedule.Object, "spec", "template")
	backup := &unstructured.Unstructured{Object: map[string]any{"spec": template}}
	backup.SetAPIVersion(veleroBackups.GroupVersion().String())
	backup.SetKind("Backup")
	backup.SetGenerateName("kubehenge-")
	backup.SetAnnotations(map[string]string{requestedBy: u.ticket.Username})
	made, err := v.Resource(veleroBackups).Namespace(provisioner.VeleroNamespace).Create(ctx, backup, metav1.CreateOptions{})
	if err != nil {
		s.veleroError(w, r, c, err)
		return
	}
	s.Log.InfoContext(r.Context(), "started a backup", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "backup", made.GetName())
	writeJSON(w, http.StatusCreated, backupOf(made))
}

// restoreRequest is what a restore is asked for: a backup, and the namespaces to bring back of it,
// every one when none is named.
type restoreRequest struct {
	Backup     string   `json:"backup"`
	Namespaces []string `json:"namespaces"`
}

// restore brings back a backup, of the whole cluster or of the namespaces named. What the cluster
// still has is left as it is. It needs Sys.Modify on the cluster's pool, as the admin kubeconfig
// does: a restore writes whatever the backup holds, Secrets and RBAC among it.
func (s *Server) restore(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	var req restoreRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := req.valid(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !backupsOf(c).Enabled {
		writeError(w, http.StatusConflict, "the cluster does not back up")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), veleroTimeout)
	defer cancel()
	v, why, err := s.velero(ctx, u, c, provisioner.IdentityRestores)
	if err != nil {
		s.kubeError(w, r, err)
		return
	}
	if v == nil {
		writeError(w, http.StatusConflict, why)
		return
	}
	backup, err := v.Resource(veleroBackups).Namespace(provisioner.VeleroNamespace).Get(ctx, req.Backup, metav1.GetOptions{})
	if err != nil {
		s.veleroError(w, r, c, err)
		return
	}
	if phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase"); phase != veleroCompleted && phase != "PartiallyFailed" {
		writeError(w, http.StatusConflict, fmt.Sprintf("backup %s is %s; only a finished backup can be restored", req.Backup, cmp.Or(phase, "new")))
		return
	}
	namespaces := []any{"*"}
	if len(req.Namespaces) > 0 {
		namespaces = make([]any, len(req.Namespaces))
		for i, n := range req.Namespaces {
			namespaces[i] = n
		}
	}
	restore := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"backupName": req.Backup, "includedNamespaces": namespaces, "restorePVs": true,
	}}}
	restore.SetAPIVersion(veleroRestores.GroupVersion().String())
	restore.SetKind("Restore")
	restore.SetGenerateName(req.Backup + "-")
	restore.SetAnnotations(map[string]string{requestedBy: u.ticket.Username})
	made, err := v.Resource(veleroRestores).Namespace(provisioner.VeleroNamespace).Create(ctx, restore, metav1.CreateOptions{})
	if err != nil {
		s.veleroError(w, r, c, err)
		return
	}
	s.Log.InfoContext(r.Context(), "started a restore", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "backup", req.Backup, "restore", made.GetName())
	writeJSON(w, http.StatusCreated, restoreOf(made, nil))
}

func (req *restoreRequest) valid() error {
	if len(validation.IsDNS1123Subdomain(req.Backup)) > 0 {
		return errors.New("name a backup")
	}
	if len(req.Namespaces) > maxRestoreNamespaces {
		return fmt.Errorf("at most %d namespaces", maxRestoreNamespaces)
	}
	seen := map[string]bool{}
	for _, n := range req.Namespaces {
		if len(validation.IsDNS1123Label(n)) > 0 {
			return fmt.Errorf("%q is not a namespace's name", n)
		}
		if seen[n] {
			return fmt.Errorf("namespace %s is named twice", n)
		}
		seen[n] = true
	}
	return nil
}

// veleroError answers with what the workload cluster refused.
func (s *Server) veleroError(w http.ResponseWriter, r *http.Request, c *clusterv1.Cluster, err error) {
	switch {
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, "no such backup, or Velero is not running in the cluster yet")
	case apierrors.IsInvalid(err) || apierrors.IsAlreadyExists(err):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		s.Log.InfoContext(r.Context(), "a cluster's Velero request failed", "cluster", c.Namespace+"/"+c.Name, "error", err)
		writeError(w, http.StatusBadGateway, "the cluster's API did not answer")
	}
}
