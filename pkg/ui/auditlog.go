package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// apiAuditLogKey is where the chart says, in the interface's ConfigMap, that the management
	// cluster's API server writes the changes people make to its output.
	apiAuditLogKey = "apiAuditLog"
	// auditFollowEvery is how often the interface reads the API servers' new lines.
	auditFollowEvery = time.Minute
	// auditFirstLook is how far back the first read goes when the activity log holds no change of
	// the API server's yet.
	auditFirstLook = 24 * time.Hour
	// auditReadBytes bounds one read of one API server's log.
	auditReadBytes = 8 << 20
	// auditVia marks an entry as the API server's, in its details.
	auditVia    = "via"
	auditViaAPI = "kubectl or the API"
	// auditIDKey keeps an entry's audit ID, so a line read twice is kept once.
	auditIDKey = "auditId"
	// auditLineMax bounds one line of the log.
	auditLineMax = 1 << 20
	// auditRefused is the lowest status code of a change the API server turned down.
	auditRefused = 400
)

// auditEvent is the part of a Kubernetes audit event the activity log keeps.
type auditEvent struct {
	Kind           string       `json:"kind"`
	AuditID        string       `json:"auditID"`
	Stage          string       `json:"stage"`
	Verb           string       `json:"verb"`
	User           auditUser    `json:"user"`
	Impersonated   *auditUser   `json:"impersonatedUser,omitempty"`
	SourceIPs      []string     `json:"sourceIPs"`
	ObjectRef      *auditObject `json:"objectRef"`
	ResponseStatus *auditStatus `json:"responseStatus"`
	StageTimestamp time.Time    `json:"stageTimestamp"`
}

type auditUser struct {
	Username string `json:"username"`
}

type auditObject struct {
	Resource    string `json:"resource"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	APIGroup    string `json:"apiGroup"`
	Subresource string `json:"subresource"`
}

type auditStatus struct {
	Code int `json:"code"`
}

// auditVerbs are the changes as the activity page words them.
var auditVerbs = map[string]string{
	"create": "created", "update": "changed", "patch": "changed", "delete": "deleted", "deletecollection": "deleted",
}

// auditNotChanges are the API groups and resources whose creates and updates change nothing a
// person set: access reviews, leases and events.
var auditNotChanges = map[string]bool{
	"authentication.k8s.io/": true, "authorization.k8s.io/": true, "coordination.k8s.io/": true,
	"/events": true, "events.k8s.io/": true,
}

// platformNamespaces are the namespaces of the management cluster's controllers: Kubernetes',
// Kubehenge's (the interface's account among them, its changes being in its record already) and
// those of the providers the management cluster module installs.
var platformNamespaces = map[string]bool{
	"kube-system": true, "kubehenge-system": true, "cert-manager": true, "capi-operator-system": true,
	"capi-system": true, "proxmox-infrastructure-system": true, "in-cluster-ipam-system": true, "helm-addon-system": true,
	"kubeadm-bootstrap-system": true, "kubeadm-control-plane-system": true, "rke2-bootstrap-system": true,
	"rke2-control-plane-system": true, "k3s-bootstrap-system": true, "k3s-control-plane-system": true,
	"talos-bootstrap-system": true, "talos-control-plane-system": true,
}

// auditByPeople says whether a user's changes are people's: their own, or a GitOps tool's or a
// script's through a service account. Nodes, the control plane and the platform's controllers are
// left out.
func auditByPeople(user string) bool {
	sa, ok := strings.CutPrefix(user, "system:serviceaccount:")
	if !ok {
		return user != "" && !strings.HasPrefix(user, "system:")
	}
	ns, _, _ := strings.Cut(sa, ":")
	return !platformNamespaces[ns]
}

// auditEntry is an audit event as an activity entry, or nil for a line that is none, a stage other
// than the answer's, or a change not made by people.
func auditEntry(line []byte) *activityEntry {
	var ev auditEvent
	if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &ev) != nil || ev.Kind != "Event" || ev.Stage != "ResponseComplete" {
		return nil
	}
	did, ok := auditVerbs[ev.Verb]
	if !ok || ev.ObjectRef == nil || !auditByPeople(ev.User.Username) ||
		auditNotChanges[ev.ObjectRef.APIGroup+"/"] || auditNotChanges[ev.ObjectRef.APIGroup+"/"+ev.ObjectRef.Resource] {
		return nil
	}
	o := ev.ObjectRef
	resource := o.Resource
	if o.Subresource != "" {
		resource += "/" + o.Subresource
	}
	e := &activityEntry{
		At:      ev.StageTimestamp.UTC(),
		User:    ev.User.Username,
		What:    strings.TrimSpace(did + " " + resource + " " + o.Name),
		Details: map[string]string{auditVia: auditViaAPI, auditIDKey: ev.AuditID},
	}
	if ev.Impersonated != nil && ev.Impersonated.Username != "" {
		e.User = ev.Impersonated.Username
		e.Details["by"] = ev.User.Username
	}
	if o.APIGroup == "cluster.x-k8s.io" && o.Resource == "clusters" && o.Namespace != "" && o.Name != "" {
		e.Cluster = o.Namespace + "/" + o.Name
	} else if o.Namespace != "" {
		e.Details["namespace"] = o.Namespace
	}
	if len(ev.SourceIPs) > 0 {
		e.Details["address"] = ev.SourceIPs[0]
	}
	if ev.ResponseStatus != nil && ev.ResponseStatus.Code >= auditRefused {
		e.Details["refused"] = strconv.Itoa(ev.ResponseStatus.Code)
	}
	return e
}

// auditSlice is what one read of an API server's log holds.
type auditSlice struct {
	entries []activityEntry
	// first and last are the kubelet's times on the first and last complete line read, whatever the
	// line was.
	first, last time.Time
	// full says the read hit its byte limit, so the log may go on.
	full bool
}

// readAuditSlice reads a log the kubelet served with timestamps, up to limit bytes. A last line the
// limit cut short is not read.
func readAuditSlice(r io.Reader, limit int64) (auditSlice, error) {
	var out auditSlice
	br := bufio.NewReaderSize(r, scanStart)
	var n int64
	for {
		line, err := br.ReadBytes('\n')
		n += int64(len(line))
		out.full = n >= limit
		complete := bytes.HasSuffix(line, []byte{'\n'})
		if len(line) > 0 && (complete || !out.full) {
			out.addLine(bytes.TrimRight(line, "\r\n"))
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err //nolint:wrapcheck // the caller names the log
		}
	}
}

func (s *auditSlice) addLine(line []byte) {
	stamp, message, ok := bytes.Cut(line, []byte{' '})
	if !ok {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, string(stamp))
	if err != nil {
		return
	}
	if s.first.IsZero() {
		s.first = at
	}
	s.last = at
	if e := auditEntry(message); e != nil {
		s.entries = append(s.entries, *e)
	}
}

// auditOpener opens one API server pod's log from since (the whole log when zero), with the
// kubelet's timestamps, at most limit bytes of it.
type auditOpener func(ctx context.Context, pod string, since time.Time, limit int64) (io.ReadCloser, error)

// auditPod is how far the follower has read one API server pod.
type auditPod struct {
	// identity tells one run of the pod from another: a restarted container starts a new log.
	identity string
	// from is where the next read starts, the last line read cut to the whole second SinceTime has.
	from time.Time
	// last is the time of the last line read.
	last time.Time
}

// auditFollower keeps what it has read of the API servers' logs.
type auditFollower struct {
	// start is where the first read of a pod starts.
	start time.Time
	// pods are the positions in the logs, by pod name.
	pods map[string]*auditPod
	// seen are the audit IDs of the lines kept, so a line read again is not kept twice.
	seen map[string]time.Time
	// open reads a pod's log. Nil reads it from the cluster. limit is a read's size, or
	// auditReadBytes when zero. Tests set both.
	open  auditOpener
	limit int64
}

func newAuditFollower() *auditFollower {
	return &auditFollower{pods: map[string]*auditPod{}, seen: map[string]time.Time{}}
}

func (f *auditFollower) readBytes() int64 {
	if f.limit > 0 {
		return f.limit
	}
	return auditReadBytes
}

// followAuditLog copies the changes people make through the management cluster's API server into
// the activity log, while the chart says the API server writes them, until ctx ends.
func (s *Server) followAuditLog(ctx context.Context, every time.Duration) {
	f := newAuditFollower()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.readAuditLog(ctx, f); err != nil {
			s.Log.WarnContext(ctx, "reading the API server's audit log", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// readAuditLog reads each API server's log to its end, from where the last read stopped.
func (s *Server) readAuditLog(ctx context.Context, f *auditFollower) error {
	kube := s.currentKube()
	if kube == nil || s.NewClientset == nil {
		return nil
	}
	on, err := auditLogOn(ctx, kube, s.ClassNamespace)
	if err != nil || !on {
		return err
	}
	cs, err := s.NewClientset()
	if err != nil {
		return fmt.Errorf("reaching the management cluster: %w", err)
	}
	if f.start.IsZero() {
		s.startAuditFollower(f)
	}
	pods, err := cs.CoreV1().Pods(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-apiserver"})
	if err != nil {
		return fmt.Errorf("listing the API servers: %w", err)
	}
	open := f.open
	if open == nil {
		open = podLogOpener(cs)
	}
	listed := map[string]bool{}
	var read []activityEntry
	var readErr error
	for i := range pods.Items {
		pod := &pods.Items[i]
		listed[pod.Name] = true
		got, err := s.followPod(ctx, f, open, pod.Name, podIdentity(pod))
		read = append(read, got...)
		if err != nil {
			readErr = errors.Join(readErr, err)
		}
	}
	maps.DeleteFunc(f.pods, func(name string, _ *auditPod) bool { return !listed[name] })
	if err := s.keepAuditEntries(f, read); err != nil {
		return err
	}
	f.forgetOld()
	return readErr
}

// podIdentity changes when the pod's API server container restarts, which starts a new log.
func podIdentity(pod *corev1.Pod) string {
	var id strings.Builder
	id.WriteString(string(pod.UID))
	for i := range pod.Status.ContainerStatuses {
		id.WriteString("/" + strconv.Itoa(int(pod.Status.ContainerStatuses[i].RestartCount)))
	}
	return id.String()
}

// followPod reads one API server's log until a read comes back short of the limit. The position
// moves by every line read, kept or not, so a stretch of lines nobody made is not read again. The
// entries read before an error are returned with it, since the position has moved past them.
func (s *Server) followPod(ctx context.Context, f *auditFollower, open auditOpener, name, identity string) ([]activityEntry, error) {
	st := f.pods[name]
	switch {
	case st == nil:
		st = &auditPod{identity: identity, from: f.start}
		f.pods[name] = st
	case st.identity != identity:
		s.Log.WarnContext(ctx, "an API server restarted, so its audit log lines since the last read are gone", "pod", name, "lastRead", st.last)
		st = &auditPod{identity: identity}
		f.pods[name] = st
	}
	var read []activityEntry
	for {
		slice, err := readAuditPod(ctx, open, name, st.from, f.readBytes())
		if err != nil {
			return read, err
		}
		read = append(read, slice.entries...)
		if !st.last.IsZero() && slice.first.After(st.last) {
			s.Log.WarnContext(ctx, "the API server's log was rotated before the interface read all of it, so its audit lines in this time are lost",
				"pod", name, "from", st.last, "to", slice.first)
		}
		if !slice.last.IsZero() {
			st.last = maxTime(st.last, slice.last)
		}
		next := st.last.Truncate(time.Second)
		if !slice.full {
			if !st.last.IsZero() {
				st.from = next
			}
			return read, nil
		}
		if slice.last.IsZero() || !next.After(st.from) {
			return read, fmt.Errorf("reading %s's log: a second of it is larger than one read of %d bytes", name, f.readBytes())
		}
		st.from = next
	}
}

func readAuditPod(ctx context.Context, open auditOpener, pod string, from time.Time, limit int64) (auditSlice, error) {
	stream, err := open(ctx, pod, from, limit)
	if err != nil {
		return auditSlice{}, fmt.Errorf("reading %s's log: %w", pod, err)
	}
	defer func() { _ = stream.Close() }()
	slice, err := readAuditSlice(stream, limit)
	if err != nil {
		return auditSlice{}, fmt.Errorf("reading %s's log: %w", pod, err)
	}
	return slice, nil
}

func podLogOpener(cs kubernetes.Interface) auditOpener {
	return func(ctx context.Context, pod string, since time.Time, limit int64) (io.ReadCloser, error) {
		opts := &corev1.PodLogOptions{Timestamps: true, LimitBytes: &limit}
		if !since.IsZero() {
			from := metav1.NewTime(since)
			opts.SinceTime = &from
		}
		return cs.CoreV1().Pods(metav1.NamespaceSystem).GetLogs(pod, opts).Stream(ctx)
	}
}

// keepAuditEntries adds the entries not kept before to the activity log, oldest first.
func (s *Server) keepAuditEntries(f *auditFollower, read []activityEntry) error {
	slices.SortFunc(read, func(a, b activityEntry) int { return a.At.Compare(b.At) })
	for i := range read {
		e := &read[i]
		id := e.Details[auditIDKey]
		if _, dup := f.seen[id]; dup {
			continue
		}
		f.seen[id] = e.At
		if err := s.activityLog.add(e); err != nil {
			return fmt.Errorf("keeping the activity log: %w", err)
		}
	}
	return nil
}

// auditSeenMargin is how long before a read's start an audit ID is still remembered. An event's
// own time precedes the kubelet's time on its line.
const auditSeenMargin = time.Minute

// forgetOld drops the IDs of lines no later read can reach.
func (f *auditFollower) forgetOld() {
	var oldest time.Time
	for _, st := range f.pods {
		if oldest.IsZero() || st.from.Before(oldest) {
			oldest = st.from
		}
	}
	if oldest.IsZero() {
		return
	}
	maps.DeleteFunc(f.seen, func(_ string, at time.Time) bool { return at.Before(oldest.Add(-auditSeenMargin)) })
}

// startAuditFollower starts after the newest change the activity log holds of the API server's,
// or a day back. A second early, since the kubelet's times are whole seconds.
func (s *Server) startAuditFollower(f *auditFollower) {
	f.start = s.now().Add(-auditFirstLook)
	kept, err := s.activityLog.since(f.start, activityInMemory, func(e *activityEntry) bool { return e.Details[auditVia] == auditViaAPI })
	if err != nil {
		return
	}
	for i := range kept {
		f.seen[kept[i].Details[auditIDKey]] = kept[i].At
		if from := kept[i].At.Add(-time.Second); from.After(f.start) {
			f.start = from
		}
	}
}

// auditLogOn learns from the interface's ConfigMap whether the API server writes its audit log
// for the interface to read.
func auditLogOn(ctx context.Context, kube client.Client, namespace string) (bool, error) {
	cm := &corev1.ConfigMap{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: interfaceConfigMap}, cm)
	switch {
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading whether the API server writes its audit log: %w", err)
	}
	return cm.Data[apiAuditLogKey] == "true", nil
}
