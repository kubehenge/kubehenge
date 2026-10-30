package ui

import (
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	// maxRestoreNames bounds the namespaces, or the kinds, one list of a restore names.
	maxRestoreNames = 100
	// maxResultMessages bounds the warnings, or the errors, a restore's results carry to the page.
	maxResultMessages = 500
	// maxResultsSize bounds a restore's results as Velero wrote them, unpacked.
	maxResultsSize = 16 << 20
	// downloadPoll is how often the interface looks whether Velero has signed the link it asked for.
	downloadPoll = 250 * time.Millisecond
)

// restoreRequest is what a restore is asked for: a backup, what of it to bring back, every
// namespace and kind of it when none are named, and whether the volumes' data comes back with it.
type restoreRequest struct {
	Backup             string   `json:"backup"`
	Namespaces         []string `json:"namespaces"`
	ExcludedNamespaces []string `json:"excludedNamespaces"`
	Resources          []string `json:"resources"`
	ExcludedResources  []string `json:"excludedResources"`
	// VolumeData unset brings the volumes' data back.
	VolumeData *bool `json:"volumeData"`
}

func (req *restoreRequest) volumeData() bool { return req.VolumeData == nil || *req.VolumeData }

// restore brings back a backup into the cluster, of the cluster's own or of one whose backups it
// reads, whole or in part. What the cluster still has is left as it is. It needs Sys.Modify on the
// cluster's pool, as the admin kubeconfig does: a restore writes whatever the backup holds, Secrets
// and RBAC among it; and VM.Audit on the pool of the cluster whose backup it is.
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
	v, ok := s.openVelero(ctx, w, u, c, provisioner.IdentityRestores)
	if !ok {
		return
	}
	backup, err := v.Resource(veleroBackups).Namespace(provisioner.VeleroNamespace).Get(ctx, req.Backup, metav1.GetOptions{})
	if err != nil {
		s.veleroError(w, c, err)
		return
	}
	if phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase"); phase != veleroCompleted && phase != veleroPartiallyFailed {
		writeError(w, http.StatusConflict, fmt.Sprintf("backup %s is %s; only a finished backup can be restored", req.Backup, cmp.Or(phase, "new")))
		return
	}
	from, ok := s.restoreSource(ctx, w, r, u, c, v, backup, &req)
	if !ok {
		return
	}
	made, err := v.Resource(veleroRestores).Namespace(provisioner.VeleroNamespace).Create(ctx, req.restore(u.ticket.Username), metav1.CreateOptions{})
	if err != nil {
		s.veleroError(w, c, err)
		return
	}
	s.Log.Info("started a restore", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name, "backup", req.Backup, "from", from, "restore", made.GetName())
	view := restoreOf(made, nil)
	view.From = from
	writeJSON(w, http.StatusCreated, view)
}

// restoreSource checks a restore of a backup of another cluster's: the user has to see that
// cluster, and its volumes' data comes back only where the cluster's Velero can read it. It
// answers the request when a check fails, and returns the cluster, or "" for the cluster's own.
func (s *Server) restoreSource(ctx context.Context, w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster,
	v dynamic.Interface, backup *unstructured.Unstructured, req *restoreRequest,
) (string, bool) {
	location, _, _ := unstructured.NestedString(backup.Object, "spec", "storageLocation")
	if location == "" || location == ownLocation {
		return "", true
	}
	l, err := v.Resource(veleroLocation).Namespace(provisioner.VeleroNamespace).Get(ctx, location, metav1.GetOptions{})
	if err != nil {
		s.veleroError(w, c, err)
		return "", false
	}
	from := l.GetAnnotations()[provisioner.BackupSourceAnnotation]
	if from == "" {
		writeError(w, http.StatusConflict, fmt.Sprintf("backup %s is in location %s, which Kubehenge did not set up", req.Backup, location))
		return "", false
	}
	pool, err := s.poolPath(c.Namespace, from)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return "", false
	}
	if !s.require(w, r, u, pool, privSee) {
		return "", false
	}
	if req.volumeData() && l.GetAnnotations()[provisioner.VolumeDataAnnotation] != "true" {
		writeError(w, http.StatusConflict, fmt.Sprintf("this cluster's Velero cannot read the volumes' data of %s's backups; restore without it", from))
		return "", false
	}
	return from, true
}

// restore is the Velero restore the request asks for. Without the volumes' data the pods stay out,
// as Velero would otherwise hold each restored pod until its volumes' data is back; their
// controllers start them again on new, empty volumes.
func (req *restoreRequest) restore(by string) *unstructured.Unstructured {
	spec := map[string]any{
		"backupName": req.Backup, "includedNamespaces": listOr(req.Namespaces, "*"), "restorePVs": req.volumeData(),
	}
	if len(req.ExcludedNamespaces) > 0 {
		spec["excludedNamespaces"] = listOr(req.ExcludedNamespaces)
	}
	if len(req.Resources) > 0 {
		spec["includedResources"] = listOr(req.Resources)
	}
	excluded := slices.Clone(req.ExcludedResources)
	if !req.volumeData() && !slices.Contains(excluded, "pods") {
		excluded = append(excluded, "pods")
	}
	if len(excluded) > 0 {
		spec["excludedResources"] = listOr(excluded)
	}
	restore := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	restore.SetAPIVersion(veleroRestores.GroupVersion().String())
	restore.SetKind("Restore")
	restore.SetGenerateName(req.Backup + "-")
	restore.SetAnnotations(map[string]string{requestedBy: by})
	return restore
}

// listOr is names as a JSON list, or the fallbacks when there are none.
func listOr(names []string, fallback ...string) []any {
	if len(names) == 0 {
		names = fallback
	}
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

func (req *restoreRequest) valid() error {
	if len(validation.IsDNS1123Subdomain(req.Backup)) > 0 {
		return errors.New("name a backup")
	}
	if err := names("namespace", req.Namespaces, validation.IsDNS1123Label); err != nil {
		return err
	}
	if err := names("namespace", req.ExcludedNamespaces, validation.IsDNS1123Label); err != nil {
		return err
	}
	if err := names("kind", req.Resources, validation.IsDNS1123Subdomain); err != nil {
		return err
	}
	if err := names("kind", req.ExcludedResources, validation.IsDNS1123Subdomain); err != nil {
		return err
	}
	if both := overlap(req.Namespaces, req.ExcludedNamespaces); both != "" {
		return fmt.Errorf("namespace %s is both restored and left out", both)
	}
	if both := overlap(req.Resources, req.ExcludedResources); both != "" {
		return fmt.Errorf("kind %s is both restored and left out", both)
	}
	if !req.volumeData() && slices.Contains(req.Resources, "pods") {
		return errors.New("without the volumes' data the pods are left for their controllers to start again: take pods out of the kinds")
	}
	return nil
}

// names checks a list of a restore's names: each valid, each once, a bounded number.
func names(what string, list []string, check func(string) []string) error {
	if len(list) > maxRestoreNames {
		return fmt.Errorf("at most %d of each list of %ss", maxRestoreNames, what)
	}
	seen := map[string]bool{}
	for _, n := range list {
		if len(check(n)) > 0 {
			return fmt.Errorf("%q is not a %s's name", n, what)
		}
		if seen[n] {
			return fmt.Errorf("%s %s is named twice", what, n)
		}
		seen[n] = true
	}
	return nil
}

func overlap(a, b []string) string {
	for _, n := range a {
		if slices.Contains(b, n) {
			return n
		}
	}
	return ""
}

// veleroResult is one half of a restore's results as Velero writes them: its own messages, those
// of cluster-wide resources, and those of each namespace.
type veleroResult struct {
	Velero     []string            `json:"velero"`
	Cluster    []string            `json:"cluster"`
	Namespaces map[string][]string `json:"namespaces"`
}

// resultsView is a restore's warnings, or its errors, by where they arose.
type resultsView struct {
	Velero     []string           `json:"velero"`
	Cluster    []string           `json:"cluster"`
	Namespaces []namespaceResults `json:"namespaces"`
	// More counts the messages past those carried.
	More int `json:"more,omitempty"`
}

type namespaceResults struct {
	Namespace string   `json:"namespace"`
	Messages  []string `json:"messages"`
}

// restoreResults are what a restore that has finished left in its results.
type restoreResults struct {
	Warnings resultsView `json:"warnings"`
	Errors   resultsView `json:"errors"`
}

// restoreResultsOf reads a restore's results: the messages of each place, at most
// maxResultMessages of them.
func restoreResultsOf(r io.Reader) (restoreResults, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return restoreResults{}, fmt.Errorf("reading the results: %w", err)
	}
	var raw map[string]veleroResult
	if err := json.NewDecoder(io.LimitReader(zr, maxResultsSize)).Decode(&raw); err != nil {
		return restoreResults{}, fmt.Errorf("reading the results: %w", err)
	}
	return restoreResults{Warnings: resultsOf(raw["warnings"]), Errors: resultsOf(raw["errors"])}, nil
}

func resultsOf(r veleroResult) resultsView {
	left := maxResultMessages
	take := func(m []string) []string {
		n := min(left, len(m))
		left -= n
		return slices.Clone(m[:n])
	}
	v := resultsView{Namespaces: []namespaceResults{}}
	total := len(r.Velero) + len(r.Cluster)
	v.Velero, v.Cluster = append([]string{}, take(r.Velero)...), append([]string{}, take(r.Cluster)...)
	for _, ns := range slices.Sorted(maps.Keys(r.Namespaces)) {
		total += len(r.Namespaces[ns])
		if m := take(r.Namespaces[ns]); len(m) > 0 {
			v.Namespaces = append(v.Namespaces, namespaceResults{Namespace: ns, Messages: m})
		}
	}
	v.More = total - (maxResultMessages - left)
	return v
}

// showRestoreResults answers with a finished restore's warnings and errors by namespace, which
// Velero keeps in the target beside the backup, through a DownloadRequest as its CLI does. Seeing
// them needs what seeing the restore does: VM.Audit on the cluster's pool.
func (s *Server) showRestoreResults(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	name := r.PathValue("restore")
	if len(validation.IsDNS1123Subdomain(name)) > 0 {
		writeError(w, http.StatusNotFound, "no such restore")
		return
	}
	if !backupsOf(c).Enabled || c.Namespace != s.Namespace {
		writeError(w, http.StatusConflict, "the interface reads no backups of this cluster")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), veleroTimeout)
	defer cancel()
	v, ok := s.openVelero(ctx, w, u, c, provisioner.IdentityBackups)
	if !ok {
		return
	}
	of, link, ok := s.restoreResultsLink(ctx, w, v, c, name)
	if !ok {
		return
	}
	s.writeRestoreResults(ctx, w, u, c, name, of, link)
}

// restoreResultsLink finds a finished restore's results: the cluster in whose folder of the target
// they are, and the link Velero signed to them. It answers the request when it cannot.
func (s *Server) restoreResultsLink(ctx context.Context, w http.ResponseWriter, v dynamic.Interface, c *clusterv1.Cluster, name string) (string, string, bool) {
	var of, link string
	restore, err := v.Resource(veleroRestores).Namespace(provisioner.VeleroNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		s.veleroError(w, c, err)
		return "", "", false
	}
	x := restoreOf(restore, nil)
	switch {
	case x.Phase == veleroInvalid:
		writeError(w, http.StatusConflict, "Velero refused the restore before it began, so it has no results")
		return "", "", false
	case !veleroFinished(x.Phase):
		writeError(w, http.StatusConflict, "the results come once the restore has finished")
		return "", "", false
	}
	if of, err = restoredFrom(ctx, v, c, x.Backup); err != nil {
		s.veleroError(w, c, err)
		return "", "", false
	}
	if link, err = veleroDownloadLink(ctx, v, "RestoreResults", name); err != nil {
		s.veleroError(w, c, err)
		return "", "", false
	}
	return of, link, true
}

// writeRestoreResults answers with the results behind Velero's link, read from the folder of the
// cluster of in the target.
func (s *Server) writeRestoreResults(ctx context.Context, w http.ResponseWriter, u user, c *clusterv1.Cluster, name, of, link string) {
	targets := &provisioner.ClusterTargets{Client: u.kube, ClassNamespace: s.ClassNamespace}
	body, err := targets.VeleroDownload(ctx, c, of, link)
	if err != nil {
		s.Log.Info("reading a restore's results", "cluster", c.Namespace+"/"+c.Name, "restore", name, "error", err)
		if errors.Is(err, provisioner.ErrNoObject) {
			writeError(w, http.StatusNotFound, "Velero left no results of the restore in the target")
			return
		}
		writeError(w, http.StatusBadGateway, "the results could not be read from the target")
		return
	}
	defer func() { _ = body.Close() }()
	results, err := restoreResultsOf(body)
	if err != nil {
		s.Log.Info("reading a restore's results", "cluster", c.Namespace+"/"+c.Name, "restore", name, "error", err)
		writeError(w, http.StatusBadGateway, "the results in the target could not be read")
		return
	}
	writeJSON(w, http.StatusOK, results)
}

// restoredFrom is the cluster in whose folder of the target a restore's backup is: the cluster's
// own, or the one whose backups its location holds. A backup gone since was the cluster's own as far
// as can be told.
func restoredFrom(ctx context.Context, v dynamic.Interface, c *clusterv1.Cluster, backup string) (string, error) {
	ns := provisioner.VeleroNamespace
	b, err := v.Resource(veleroBackups).Namespace(ns).Get(ctx, backup, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.Name, nil
		}
		return "", fmt.Errorf("reading backup %s: %w", backup, err)
	}
	location, _, _ := unstructured.NestedString(b.Object, "spec", "storageLocation")
	if location == "" || location == ownLocation {
		return c.Name, nil
	}
	l, err := v.Resource(veleroLocation).Namespace(ns).Get(ctx, location, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading location %s: %w", location, err)
	}
	if from := l.GetAnnotations()[provisioner.BackupSourceAnnotation]; from != "" {
		return from, nil
	}
	return "", fmt.Errorf("location %s is none of Kubehenge's", location)
}

// veleroDownloadLink asks Velero for a link to one of its files, and waits until it has signed one.
func veleroDownloadLink(ctx context.Context, v dynamic.Interface, kind, name string) (string, error) {
	req := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"target": map[string]any{"kind": kind, "name": name},
	}}}
	req.SetAPIVersion(veleroDownloads.GroupVersion().String())
	req.SetKind("DownloadRequest")
	req.SetGenerateName(name + "-")
	downloads := v.Resource(veleroDownloads).Namespace(provisioner.VeleroNamespace)
	made, err := downloads.Create(ctx, req, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("asking Velero for its file: %w", err)
	}
	tick := time.NewTicker(downloadPoll)
	defer tick.Stop()
	for {
		phase, _, _ := unstructured.NestedString(made.Object, "status", "phase")
		link, _, _ := unstructured.NestedString(made.Object, "status", "downloadURL")
		if phase == veleroProcessed && link != "" {
			return link, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for Velero's link: %w", ctx.Err())
		case <-tick.C:
		}
		if made, err = downloads.Get(ctx, made.GetName(), metav1.GetOptions{}); err != nil {
			return "", fmt.Errorf("reading Velero's link: %w", err)
		}
	}
}
