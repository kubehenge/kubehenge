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
	veleroV1        = schema.GroupVersion{Group: "velero.io", Version: "v1"}
	veleroBackups   = veleroV1.WithResource("backups")
	veleroRestores  = veleroV1.WithResource("restores")
	veleroSchedule  = veleroV1.WithResource("schedules")
	veleroLocation  = veleroV1.WithResource("backupstoragelocations")
	veleroVolumes   = veleroV1.WithResource("podvolumerestores")
	veleroDeletions = veleroV1.WithResource("deletebackuprequests")
	veleroDownloads = veleroV1.WithResource("downloadrequests")
)

const (
	// veleroTimeout bounds each request to a workload cluster's API, so that a cluster that does
	// not answer holds up the view for no longer.
	veleroTimeout = 10 * time.Second
	// veleroListLimit bounds what one list reads: with a backup a day kept a week, far more than
	// a cluster holds.
	veleroListLimit = 500
	// requestedBy on a backup, restore or deletion names the Proxmox VE user who asked for it.
	requestedBy = "kubehenge.dev/requested-by"
	// ownLocation is the cluster's own backup location; the others hold the backups of the
	// clusters it reads.
	ownLocation = "default"
	// Velero's labels on a deletion, which its CLI sets too, naming the backup it deletes.
	veleroBackupName = "velero.io/backup-name"
	veleroBackupUID  = "velero.io/backup-uid"
)

// Velero's phases of a backup or restore that the interface tells apart.
const (
	veleroCompleted       = "Completed"
	veleroDeleting        = "Deleting"
	veleroPartiallyFailed = "PartiallyFailed"
	veleroFailed          = "Failed"
	veleroInvalid         = "FailedValidation"
	// veleroProcessed is where a DeleteBackupRequest or DownloadRequest ends.
	veleroProcessed = "Processed"
)

// veleroFinished tells whether a backup or restore in phase has ended, well or not.
func veleroFinished(phase string) bool {
	return slices.Contains([]string{veleroCompleted, veleroPartiallyFailed, veleroFailed, veleroInvalid}, phase)
}

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
	// From names the cluster whose backup it is, when it is not the cluster's own.
	From string `json:"from,omitempty"`
	// Deleting is set once a deletion is asked for, until Velero has deleted it.
	Deleting bool `json:"deleting,omitempty"`
	// DeleteErrors are why Velero did not delete it when last asked to.
	DeleteErrors []string `json:"deleteErrors,omitempty"`

	location string
}

// restoreView is a restore with how far its volumes have come.
type restoreView struct {
	Name               string     `json:"name"`
	Backup             string     `json:"backup"`
	Phase              string     `json:"phase"`
	By                 string     `json:"by,omitempty"`
	Namespaces         []string   `json:"namespaces,omitempty"`
	ExcludedNamespaces []string   `json:"excludedNamespaces,omitempty"`
	Resources          []string   `json:"resources,omitempty"`
	ExcludedResources  []string   `json:"excludedResources,omitempty"`
	VolumeData         bool       `json:"volumeData"`
	From               string     `json:"from,omitempty"`
	Started            *time.Time `json:"started,omitempty"`
	Completed          *time.Time `json:"completed,omitempty"`
	Items              int64      `json:"items"`
	Total              int64      `json:"total"`
	Warnings           int64      `json:"warnings"`
	Errors             int64      `json:"errors"`
	Failure            string     `json:"failure,omitempty"`
	// Refused are why Velero refused the restore before it began.
	Refused []string `json:"refused,omitempty"`
	// Volumes counts the pod volumes being restored, those done and the bytes of them all.
	Volumes     int   `json:"volumes"`
	VolumesDone int   `json:"volumesDone"`
	BytesDone   int64 `json:"bytesDone"`
	BytesTotal  int64 `json:"bytesTotal"`
}

// sourceView is a cluster whose backups the cluster reads, as its Velero's location for them stands.
type sourceView struct {
	Cluster string `json:"cluster"`
	// Phase is Available once Velero reaches the location.
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	// VolumeData tells whether the volumes' data of its backups can be restored here.
	VolumeData bool `json:"volumeData"`
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
	Sources         []sourceView  `json:"sources"`
	// Attention is what the health page says of the backups: overdue, or the last one failing.
	Attention  []attentionView `json:"attention"`
	CanBackUp  bool            `json:"canBackUp"`
	CanDelete  bool            `json:"canDelete"`
	CanRestore bool            `json:"canRestore"`

	schedule *unstructured.Unstructured
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

// openVelero reaches the cluster's Velero for a request that cannot do without it, answering the
// request when it cannot.
func (s *Server) openVelero(ctx context.Context, w http.ResponseWriter, u user, c *clusterv1.Cluster, id provisioner.Identity) (dynamic.Interface, bool) {
	v, why, err := s.velero(ctx, u, c, id)
	if err != nil {
		s.kubeError(w, err)
		return nil, false
	}
	if v == nil {
		writeError(w, http.StatusConflict, why)
		return nil, false
	}
	return v, true
}

func (s *Server) clusterBackups(w http.ResponseWriter, r *http.Request, u user) {
	c, access, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	b := backupsOf(c)
	view := backupsView{
		Enabled: b.Enabled, Target: b.Target,
		Backups: []backupView{}, Restores: []restoreView{}, Sources: []sourceView{}, Attention: []attentionView{},
	}
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
	view.CanDelete = access.held.Has(privChange)
	view.CanRestore = access.held.Has(privKubeconfig)
	ctx, cancel := context.WithTimeout(r.Context(), veleroTimeout)
	defer cancel()
	v, why, err := s.velero(ctx, u, c, provisioner.IdentityBackups)
	if err != nil {
		s.kubeError(w, err)
		return
	}
	if v == nil {
		view.Message = why
		writeJSON(w, http.StatusOK, view)
		return
	}
	if err := readVelero(ctx, v, &view); err != nil {
		s.Log.Info("reading a cluster's backups", "cluster", c.Namespace+"/"+c.Name, "error", err)
		view.Message = "The cluster's API did not answer, or Velero is not running in it yet."
		writeJSON(w, http.StatusOK, view)
		return
	}
	view.Reachable = true
	view.Attention = append(view.Attention, backupsAttention(view.schedule, view.Backups, s.now())...)
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

// readBackupRuns reads the schedule, which is nil when there is none, and the backups, of the
// cluster's own location and of those of the clusters it reads, newest first.
func readBackupRuns(ctx context.Context, v dynamic.Interface) (*unstructured.Unstructured, []backupView, error) {
	ns := provisioner.VeleroNamespace
	schedule, err := v.Resource(veleroSchedule).Namespace(ns).Get(ctx, provisioner.VeleroSchedule, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		schedule = nil
	case err != nil:
		return nil, nil, fmt.Errorf("reading the schedule: %w", err)
	}
	backups, err := v.Resource(veleroBackups).Namespace(ns).List(ctx, metav1.ListOptions{Limit: veleroListLimit})
	if err != nil {
		return nil, nil, fmt.Errorf("listing the backups: %w", err)
	}
	out := make([]backupView, 0, len(backups.Items))
	for i := range backups.Items {
		out = append(out, backupOf(&backups.Items[i]))
	}
	slices.SortFunc(out, func(a, b backupView) int { return newest(a.Started, b.Started, a.Name, b.Name) })
	return schedule, out, nil
}

// newest orders the newest first, then by name; one not yet started goes first.
func newest(a, b *time.Time, an, bn string) int {
	if c := cmpTime(b, a); c != 0 {
		return c
	}
	return strings.Compare(bn, an)
}

// readVelero fills the view from Velero's resources.
func readVelero(ctx context.Context, v dynamic.Interface, view *backupsView) error {
	ns := provisioner.VeleroNamespace
	locations, err := v.Resource(veleroLocation).Namespace(ns).List(ctx, metav1.ListOptions{Limit: veleroListLimit})
	if err != nil {
		return fmt.Errorf("listing the backup locations: %w", err)
	}
	sources := map[string]string{}
	for i := range locations.Items {
		l := &locations.Items[i]
		phase, _, _ := unstructured.NestedString(l.Object, "status", "phase")
		message, _, _ := unstructured.NestedString(l.Object, "status", "message")
		if l.GetName() == ownLocation {
			view.Location, view.LocationMessage = phase, message
			continue
		}
		source := l.GetAnnotations()[provisioner.BackupSourceAnnotation]
		if source == "" {
			continue
		}
		sources[l.GetName()] = source
		view.Sources = append(view.Sources, sourceView{
			Cluster: source, Phase: phase, Message: message,
			VolumeData: l.GetAnnotations()[provisioner.VolumeDataAnnotation] == "true",
		})
	}
	slices.SortFunc(view.Sources, func(a, b sourceView) int { return strings.Compare(a.Cluster, b.Cluster) })
	schedule, backups, err := readBackupRuns(ctx, v)
	if err != nil {
		return err
	}
	view.schedule = schedule
	if schedule != nil {
		view.LastBackup = timeAt(schedule, "status", "lastBackup")
	}
	deletions, err := v.Resource(veleroDeletions).Namespace(ns).List(ctx, metav1.ListOptions{Limit: veleroListLimit})
	if err != nil {
		return fmt.Errorf("listing the deletions: %w", err)
	}
	from := map[string]string{}
	for i := range backups {
		b := &backups[i]
		if b.location != "" && b.location != ownLocation {
			b.From = cmp.Or(sources[b.location], strings.TrimPrefix(b.location, "cluster-"))
		}
		from[b.Name] = b.From
		deletionOf(b, deletions.Items)
	}
	view.Backups = backups
	restores, err := v.Resource(veleroRestores).Namespace(ns).List(ctx, metav1.ListOptions{Limit: veleroListLimit})
	if err != nil {
		return fmt.Errorf("listing the restores: %w", err)
	}
	volumes, err := v.Resource(veleroVolumes).Namespace(ns).List(ctx, metav1.ListOptions{Limit: veleroListLimit})
	if err != nil {
		return fmt.Errorf("listing the volume restores: %w", err)
	}
	for i := range restores.Items {
		x := restoreOf(&restores.Items[i], volumes.Items)
		x.From = from[x.Backup]
		view.Restores = append(view.Restores, x)
	}
	slices.SortFunc(view.Restores, func(a, b restoreView) int { return newest(a.Started, b.Started, a.Name, b.Name) })
	return nil
}

// deletionOf marks a backup being deleted while a deletion of it waits or runs, and says why the
// newest that ended did not delete it.
func deletionOf(b *backupView, deletions []unstructured.Unstructured) {
	var last *unstructured.Unstructured
	for i := range deletions {
		d := &deletions[i]
		if name, _, _ := unstructured.NestedString(d.Object, "spec", "backupName"); name != b.Name {
			continue
		}
		if phase, _, _ := unstructured.NestedString(d.Object, "status", "phase"); phase != veleroProcessed {
			b.Deleting = true
			continue
		}
		if last == nil || d.GetCreationTimestamp().After(last.GetCreationTimestamp().Time) {
			last = d
		}
	}
	if b.Phase == veleroDeleting {
		b.Deleting = true
	}
	if last != nil && !b.Deleting {
		b.DeleteErrors, _, _ = unstructured.NestedStringSlice(last.Object, "status", "errors")
	}
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
	location, _, _ := unstructured.NestedString(o.Object, "spec", "storageLocation")
	return backupView{
		Name: o.GetName(), Phase: cmp.Or(phase, "New"),
		Scheduled: o.GetLabels()["velero.io/schedule-name"] != "",
		By:        o.GetAnnotations()[requestedBy],
		Started:   timeAt(o, "status", "startTimestamp"), Completed: timeAt(o, "status", "completionTimestamp"),
		Expires: timeAt(o, "status", "expiration"),
		Items:   intAt(o, "status", "progress", "itemsBackedUp"), Total: intAt(o, "status", "progress", "totalItems"),
		Warnings: intAt(o, "status", "warnings"), Errors: intAt(o, "status", "errors"), Failure: failure,
		location: location,
	}
}

func restoreOf(o *unstructured.Unstructured, volumes []unstructured.Unstructured) restoreView {
	text := func(fields ...string) string { s, _, _ := unstructured.NestedString(o.Object, fields...); return s }
	list := func(fields ...string) []string {
		s, _, _ := unstructured.NestedStringSlice(o.Object, fields...)
		return s
	}
	namespaces := list("spec", "includedNamespaces")
	if len(namespaces) == 1 && namespaces[0] == "*" {
		namespaces = nil
	}
	resources := list("spec", "includedResources")
	if len(resources) == 1 && resources[0] == "*" {
		resources = nil
	}
	// Velero restores the volumes unless told otherwise; Kubehenge's restores without them leave the
	// pods out too, for their controllers to start again on empty volumes.
	excluded := list("spec", "excludedResources")
	restorePVs, set, _ := unstructured.NestedBool(o.Object, "spec", "restorePVs")
	view := restoreView{
		Name: o.GetName(), Backup: text("spec", "backupName"), Phase: cmp.Or(text("status", "phase"), "New"),
		By:         o.GetAnnotations()[requestedBy],
		Namespaces: namespaces, ExcludedNamespaces: list("spec", "excludedNamespaces"),
		Resources: resources, ExcludedResources: excluded,
		VolumeData: (!set || restorePVs) && !slices.Contains(excluded, "pods"),
		Started:    timeAt(o, "status", "startTimestamp"), Completed: timeAt(o, "status", "completionTimestamp"),
		Items: intAt(o, "status", "progress", "itemsRestored"), Total: intAt(o, "status", "progress", "totalItems"),
		Warnings: intAt(o, "status", "warnings"), Errors: intAt(o, "status", "errors"), Failure: text("status", "failureReason"),
		Refused: list("status", "validationErrors"),
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
	v, ok := s.openVelero(ctx, w, u, c, provisioner.IdentityBackups)
	if !ok {
		return
	}
	schedule, err := v.Resource(veleroSchedule).Namespace(provisioner.VeleroNamespace).Get(ctx, provisioner.VeleroSchedule, metav1.GetOptions{})
	if err != nil {
		s.veleroError(w, c, err)
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
		s.veleroError(w, c, err)
		return
	}
	s.Log.Info("started a backup", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "backup", made.GetName())
	writeJSON(w, http.StatusCreated, backupOf(made))
}

// deleteRequest confirms a backup's deletion with the backup's name, typed again.
type deleteRequest struct {
	Confirm string `json:"confirm"`
}

// deleteBackup has Velero delete one of the cluster's own backups before it runs out, with what it
// holds in the target, through a DeleteBackupRequest, as Velero's CLI does. It needs VM.Allocate on
// the cluster's pool, as deleting the cluster does.
func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return
	}
	name := r.PathValue("backup")
	var req deleteRequest
	if !readJSON(w, r, &req) {
		return
	}
	if len(validation.IsDNS1123Subdomain(name)) > 0 {
		writeError(w, http.StatusNotFound, "no such backup")
		return
	}
	if req.Confirm != name {
		writeError(w, http.StatusUnprocessableEntity, "type the backup's name to confirm")
		return
	}
	if !backupsOf(c).Enabled {
		writeError(w, http.StatusConflict, "the cluster does not back up")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), veleroTimeout)
	defer cancel()
	v, ok := s.openVelero(ctx, w, u, c, provisioner.IdentityBackups)
	if !ok {
		return
	}
	backup, view, ok := s.deletableBackup(ctx, w, v, c, name)
	if !ok {
		return
	}
	deletion := deleteBackupRequest(backup, u.ticket.Username)
	if _, err := v.Resource(veleroDeletions).Namespace(provisioner.VeleroNamespace).Create(ctx, deletion, metav1.CreateOptions{}); err != nil {
		s.veleroError(w, c, err)
		return
	}
	s.Log.Info("asked to delete a backup", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "backup", name)
	view.Deleting, view.DeleteErrors = true, nil
	writeJSON(w, http.StatusAccepted, view)
}

// deletableBackup reads one of the cluster's own backups that has finished and is not being deleted
// yet, with its view. It answers the request when there is no such backup.
func (s *Server) deletableBackup(ctx context.Context, w http.ResponseWriter, v dynamic.Interface, c *clusterv1.Cluster, name string) (*unstructured.Unstructured, backupView, bool) {
	ns := provisioner.VeleroNamespace
	backup, err := v.Resource(veleroBackups).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		s.veleroError(w, c, err)
		return nil, backupView{}, false
	}
	view := backupOf(backup)
	deletions, err := v.Resource(veleroDeletions).Namespace(ns).List(ctx, metav1.ListOptions{Limit: veleroListLimit})
	if err != nil {
		s.veleroError(w, c, err)
		return nil, backupView{}, false
	}
	deletionOf(&view, deletions.Items)
	var why string
	switch {
	case view.location != "" && view.location != ownLocation:
		why = fmt.Sprintf("backup %s is another cluster's, and is deleted there", name)
	case view.Deleting:
		why = fmt.Sprintf("backup %s is being deleted already", name)
	case !veleroFinished(view.Phase):
		why = fmt.Sprintf("backup %s is still running; delete it once it has finished", name)
	}
	if why != "" {
		writeError(w, http.StatusConflict, why)
		return nil, backupView{}, false
	}
	return backup, view, true
}

// deleteBackupRequest is the DeleteBackupRequest that has Velero delete the backup, labelled as
// Velero's CLI labels one; a name too long for a label's value goes by the backup's UID alone.
func deleteBackupRequest(backup *unstructured.Unstructured, by string) *unstructured.Unstructured {
	name := backup.GetName()
	labels := map[string]string{veleroBackupUID: string(backup.GetUID())}
	if len(validation.IsValidLabelValue(name)) == 0 {
		labels[veleroBackupName] = name
	}
	deletion := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"backupName": name}}}
	deletion.SetAPIVersion(veleroDeletions.GroupVersion().String())
	deletion.SetKind("DeleteBackupRequest")
	deletion.SetGenerateName(name + "-")
	deletion.SetLabels(labels)
	deletion.SetAnnotations(map[string]string{requestedBy: by})
	return deletion
}

// allowBackupSources checks the rights a cluster's backups variable needs to read the backups of
// the clusters it newly names in readFrom: Sys.Modify on each one's pool, as the admin kubeconfig
// does, since the cluster's Velero then holds the key to whatever their backups hold, Secrets
// among it. It answers the request when they fail; the validator checks the rest.
func (s *Server) allowBackupSources(w http.ResponseWriter, r *http.Request, u user, namespace string, value, old json.RawMessage) bool {
	var now, before provisioner.Backups
	if json.Unmarshal(value, &now) != nil {
		return true
	}
	if old != nil && json.Unmarshal(old, &before) != nil {
		before = provisioner.Backups{}
	}
	for _, source := range now.ReadFrom {
		if slices.Contains(before.ReadFrom, source) {
			continue
		}
		pool, err := s.poolPath(namespace, source)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return false
		}
		if !s.require(w, r, u, pool, privKubeconfig) {
			return false
		}
	}
	return true
}

// veleroError answers with what the workload cluster refused.
func (s *Server) veleroError(w http.ResponseWriter, c *clusterv1.Cluster, err error) {
	switch {
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, "no such backup, or Velero is not running in the cluster yet")
	case apierrors.IsInvalid(err) || apierrors.IsAlreadyExists(err):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "Velero did not answer in time")
	default:
		s.Log.Info("a cluster's Velero request failed", "cluster", c.Namespace+"/"+c.Name, "error", err)
		writeError(w, http.StatusBadGateway, "the cluster's API did not answer")
	}
}
