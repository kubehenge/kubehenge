package ui

import (
	"bufio"
	"context"
	"encoding/json"
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

// auditEntry is an audit event as an activity entry, or nil for a line that is none, a stage other
// than the answer's, or a change made by no person: by a service account (the interface's own
// included, whose changes its record holds), a node or the control plane.
func auditEntry(line []byte) *activityEntry {
	var ev auditEvent
	if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &ev) != nil || ev.Kind != "Event" || ev.Stage != "ResponseComplete" {
		return nil
	}
	did, ok := auditVerbs[ev.Verb]
	if !ok || ev.ObjectRef == nil || ev.User.Username == "" || strings.HasPrefix(ev.User.Username, "system:") ||
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

// auditEntries reads the audit events among a log's lines, oldest first.
func auditEntries(r io.Reader) ([]activityEntry, error) {
	var out []activityEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, scanStart), auditLineMax)
	for sc.Scan() {
		if e := auditEntry(sc.Bytes()); e != nil {
			out = append(out, *e)
		}
	}
	return out, sc.Err() //nolint:wrapcheck // the caller names the log
}

// auditFollower keeps what it has read of the API servers' logs.
type auditFollower struct {
	// since is the time the next read starts at.
	since time.Time
	// seen are the audit IDs read at or after since, so a line read again is not kept twice.
	seen map[string]time.Time
}

// followAuditLog copies the changes people make through the management cluster's API server into
// the activity log, while the chart says the API server writes them, until ctx ends.
func (s *Server) followAuditLog(ctx context.Context, every time.Duration) {
	f := &auditFollower{seen: map[string]time.Time{}}
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

// readAuditLog reads each API server's lines since the last read.
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
	if f.since.IsZero() {
		s.startAuditFollower(f)
	}
	pods, err := cs.CoreV1().Pods(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-apiserver"})
	if err != nil {
		return fmt.Errorf("listing the API servers: %w", err)
	}
	var read []activityEntry
	for i := range pods.Items {
		got, err := podAuditEntries(ctx, cs, pods.Items[i].Name, f.since)
		if err != nil {
			return err
		}
		read = append(read, got...)
	}
	return s.keepAuditEntries(f, read)
}

// keepAuditEntries adds the entries not kept before to the activity log, oldest first, and moves
// the next read's start on to the newest.
func (s *Server) keepAuditEntries(f *auditFollower, read []activityEntry) error {
	slices.SortFunc(read, func(a, b activityEntry) int { return a.At.Compare(b.At) })
	var newest time.Time
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
		newest = maxTime(newest, e.At)
	}
	f.moveOn(newest)
	return nil
}

// moveOn starts the next read at newest, the time of the newest line kept, when it is later. The
// log's times are whole seconds, so the read starts a second early, and the IDs of that second keep
// its lines from being kept twice.
func (f *auditFollower) moveOn(newest time.Time) {
	if from := newest.Add(-time.Second); from.After(f.since) {
		f.since = from
	}
	maps.DeleteFunc(f.seen, func(_ string, at time.Time) bool { return at.Before(f.since) })
}

// startAuditFollower starts after the newest change the activity log holds of the API server's,
// or a day back.
func (s *Server) startAuditFollower(f *auditFollower) {
	f.since = s.now().Add(-auditFirstLook)
	kept, err := s.activityLog.since(f.since, activityInMemory, func(e *activityEntry) bool { return e.Details[auditVia] == auditViaAPI })
	if err != nil {
		return
	}
	var newest time.Time
	for i := range kept {
		f.seen[kept[i].Details[auditIDKey]] = kept[i].At
		newest = maxTime(newest, kept[i].At)
	}
	f.moveOn(newest)
}

func podAuditEntries(ctx context.Context, cs kubernetes.Interface, pod string, since time.Time) ([]activityEntry, error) {
	from := metav1.NewTime(since)
	limit := int64(auditReadBytes)
	stream, err := cs.CoreV1().Pods(metav1.NamespaceSystem).GetLogs(pod, &corev1.PodLogOptions{SinceTime: &from, LimitBytes: &limit}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading %s's log: %w", pod, err)
	}
	defer func() { _ = stream.Close() }()
	got, err := auditEntries(stream)
	if err != nil {
		return nil, fmt.Errorf("reading %s's log: %w", pod, err)
	}
	return got, nil
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
