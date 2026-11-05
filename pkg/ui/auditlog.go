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
	"sync"
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
	// auditFollowEvery is how often the interface looks for new or restarted API servers and
	// reopens a stream that ended.
	auditFollowEvery = time.Minute
	// auditQuiet is how long a stream may bring no line before it is reopened. The API server logs
	// more than a line a minute, so a stream this quiet hangs on a connection that died.
	auditQuiet = 5 * time.Minute
	// auditFirstLook is how far back the first read goes when the activity log holds no change of
	// the API server's yet.
	auditFirstLook = 24 * time.Hour
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

// stampedLine splits a line the kubelet served with timestamps into its time and the API server's
// message.
func stampedLine(line []byte) (time.Time, []byte, bool) {
	stamp, message, ok := bytes.Cut(line, []byte{' '})
	if !ok {
		return time.Time{}, nil, false
	}
	at, err := time.Parse(time.RFC3339Nano, string(stamp))
	if err != nil {
		return time.Time{}, nil, false
	}
	return at, message, true
}

// auditOpener opens one API server pod's log from since (the whole log when zero), with the
// kubelet's timestamps. It follows the current container's log as the API server writes it, or
// reads the previous container's to its end.
type auditOpener func(ctx context.Context, pod string, since time.Time, previous bool) (io.ReadCloser, error)

// auditPod is how far the follower has read one API server pod, and the stream that reads it.
type auditPod struct {
	// uid and restarts tell one run of the pod's container from another, each with a log of its own.
	uid      string
	restarts int
	// previous says the stream first reads the previous container's log from from, for the lines
	// written after the last read. The current container's log is then read from its start.
	previous bool
	// from is where the next stream starts, the last line read cut to the whole second SinceTime has.
	from time.Time
	// last is the time of the last line read.
	last time.Time
	// heard is when the stream last brought a line, by the interface's clock.
	heard time.Time
	// stop ends the stream, and done is closed once it has ended. Both are nil before the first.
	stop context.CancelFunc
	done chan struct{}
	// finished says the stream reached the end of a container that stopped, so it read every line.
	finished bool
	// fresh says the stream has brought no line yet.
	fresh bool
}

func (p *auditPod) streaming() bool {
	if p.done == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *auditPod) end() {
	if p.stop != nil {
		p.stop()
	}
}

// auditFollower keeps one stream on each API server's log and what it has read of them. The
// streams change it, so mu guards it.
type auditFollower struct {
	mu sync.Mutex
	// start is where the first stream of a pod starts.
	start time.Time
	// pods are the positions in the logs, by pod name.
	pods map[string]*auditPod
	// seen are the audit IDs of the lines kept, so a line read again is not kept twice.
	seen    map[string]time.Time
	streams sync.WaitGroup
}

func newAuditFollower() *auditFollower {
	return &auditFollower{pods: map[string]*auditPod{}, seen: map[string]time.Time{}}
}

// stopAll ends every stream and waits until they have. The positions stay.
func (f *auditFollower) stopAll() {
	f.mu.Lock()
	for _, p := range f.pods {
		p.end()
	}
	f.mu.Unlock()
	f.streams.Wait()
}

// followAuditLog copies the changes people make through the management cluster's API server into
// the activity log, while the chart says the API server writes them, until ctx ends.
func (s *Server) followAuditLog(ctx context.Context, every time.Duration) {
	f := newAuditFollower()
	defer f.stopAll()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.followAuditPods(ctx, f); err != nil {
			s.Log.WarnContext(ctx, "reading the API server's audit log", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// followAuditPods keeps a stream open on each API server's log, from where the last one stopped.
// A stream reads each line as the API server writes it. The kubelet serves only a log's current
// file, so lines left unread until it rotates the file are lost.
func (s *Server) followAuditPods(ctx context.Context, f *auditFollower) error {
	kube := s.currentKube()
	if kube == nil || s.NewClientset == nil {
		return nil
	}
	on, err := auditLogOn(ctx, kube, s.ClassNamespace)
	if err != nil {
		return err
	}
	if !on {
		f.stopAll()
		return nil
	}
	cs, err := s.NewClientset()
	if err != nil {
		return fmt.Errorf("reaching the management cluster: %w", err)
	}
	pods, err := cs.CoreV1().Pods(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-apiserver"})
	if err != nil {
		return fmt.Errorf("listing the API servers: %w", err)
	}
	open := podLogOpener(cs)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.start.IsZero() {
		s.startAuditFollower(f)
	}
	listed := map[string]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		listed[pod.Name] = true
		uid, restarts := podRun(pod)
		s.keepStreaming(ctx, f, open, pod.Name, uid, restarts)
	}
	maps.DeleteFunc(f.pods, func(name string, p *auditPod) bool {
		if listed[name] {
			return false
		}
		p.end()
		return true
	})
	f.forgetOld()
	return nil
}

// podRun is the pod's UID and its containers' restarts. Each restart starts a new log, and the
// kubelet keeps the one before it. The API server's pod has one container.
func podRun(pod *corev1.Pod) (string, int) {
	restarts := 0
	for i := range pod.Status.ContainerStatuses {
		restarts += int(pod.Status.ContainerStatuses[i].RestartCount)
	}
	return string(pod.UID), restarts
}

// keepStreaming opens a stream on one API server's log unless one is open and live. f.mu is held.
func (s *Server) keepStreaming(ctx context.Context, f *auditFollower, open auditOpener, name, uid string, restarts int) {
	p := f.pods[name]
	switch {
	case p == nil:
		p = &auditPod{uid: uid, restarts: restarts, from: f.start}
		f.pods[name] = p
	case p.uid != uid:
		if !p.finished {
			s.warnLost(ctx, name, p.last, "a new API server pod replaced it")
		}
		p.end()
		p = &auditPod{uid: uid, restarts: restarts}
		f.pods[name] = p
	case p.restarts != restarts:
		p.end()
		p = s.restarted(ctx, name, p, restarts)
		f.pods[name] = p
	case p.streaming() && s.now().Sub(p.heard) < auditQuiet:
		return
	case p.streaming():
		s.Log.WarnContext(ctx, "the API server's log brought no line for a while, so the interface opens it again", "pod", name, "lastRead", p.last)
		p.end()
	}
	streamCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	p.stop, p.done, p.heard, p.finished = stop, done, s.now(), false
	f.streams.Go(func() {
		defer close(done)
		defer stop()
		s.streamPod(streamCtx, f, open, name, p)
	})
}

// restarted is the position after the pod's container restarted. The kubelet keeps only the log
// of the container before the current one.
//   - One restart: that log is the one the stream read. It is read on from the last line read.
//   - Two: the stream's own container was read to its end or its last lines are gone. The
//     previous log is read whole.
//   - More: the logs of the containers in between are gone.
func (s *Server) restarted(ctx context.Context, name string, p *auditPod, restarts int) *auditPod {
	next := &auditPod{uid: p.uid, restarts: restarts, previous: true}
	switch n := restarts - p.restarts; {
	case n == 1 && p.finished:
		next.previous = false
	case n == 1:
		next.from, next.last = p.from, p.last
	case n == 2 && p.finished:
	default:
		s.warnLost(ctx, name, p.last, "the API server restarted more often than the kubelet keeps logs for")
	}
	return next
}

func (s *Server) warnLost(ctx context.Context, name string, last time.Time, why string) {
	s.Log.WarnContext(ctx, "the interface could not read all of an API server's log, so its audit lines after the last read are lost",
		"pod", name, "lastRead", last, "why", why)
}

// streamPod reads the previous container's log where asked, then follows the current one's as
// it is written, until the stream or ctx ends.
func (s *Server) streamPod(ctx context.Context, f *auditFollower, open auditOpener, name string, p *auditPod) {
	f.mu.Lock()
	from, previous, last := p.from, p.previous, p.last
	f.mu.Unlock()
	if previous {
		err := s.readPodLog(ctx, f, open, name, p, from, true)
		switch {
		case apierrors.IsBadRequest(err) || apierrors.IsNotFound(err):
			s.warnLost(ctx, name, last, "the previous container's log is gone")
		case err != nil:
			// Read again at the next tick, as the previous log may still be there.
			s.warnStream(ctx, name, err)
			return
		}
		f.mu.Lock()
		p.previous, p.from, p.last = false, time.Time{}, time.Time{}
		f.mu.Unlock()
		from = time.Time{}
	}
	err := s.readPodLog(ctx, f, open, name, p, from, false)
	if err != nil {
		s.warnStream(ctx, name, err)
		return
	}
	f.mu.Lock()
	p.finished = true
	f.mu.Unlock()
}

// readPodLog reads one log of a pod from from until it ends, nil when it ended whole.
func (s *Server) readPodLog(ctx context.Context, f *auditFollower, open auditOpener, name string, p *auditPod, from time.Time, previous bool) error {
	stream, err := open(ctx, name, from, previous)
	if err != nil {
		return err //nolint:wrapcheck // podLogOpener names the step
	}
	defer func() { _ = stream.Close() }()
	f.mu.Lock()
	p.fresh = true
	f.mu.Unlock()
	br := bufio.NewReaderSize(stream, scanStart)
	for {
		line, err := br.ReadBytes('\n')
		// A line the stream cut short is left out. The next stream starts in its second and reads
		// it whole.
		at, message, ok := stampedLine(bytes.TrimRight(line, "\r\n"))
		if ok && bytes.HasSuffix(line, []byte{'\n'}) {
			if keepErr := s.auditLine(ctx, f, name, p, at, message); keepErr != nil {
				return keepErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the log: %w", err)
		}
	}
}

// auditLine moves the pod's position to a line read and keeps its change, if it is one. A stream
// starts in the second of the last line read before, so a first line later than that one says the
// lines in between were rotated away.
func (s *Server) auditLine(ctx context.Context, f *auditFollower, name string, p *auditPod, at time.Time, message []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.fresh && !p.last.IsZero() && at.After(p.last) {
		s.Log.WarnContext(ctx, "the API server's log was rotated while the interface did not follow it, so its audit lines in this time are lost",
			"pod", name, "from", p.last, "to", at)
	}
	p.fresh = false
	p.last = maxTime(p.last, at)
	p.from = p.last.Truncate(time.Second)
	p.heard = s.now()
	if e := auditEntry(message); e != nil {
		return s.keepAuditEntries(f, []activityEntry{*e})
	}
	return nil
}

func (s *Server) warnStream(ctx context.Context, name string, err error) {
	if ctx.Err() == nil {
		s.Log.WarnContext(ctx, "reading the API server's audit log", "pod", name, "error", err)
	}
}

func podLogOpener(cs kubernetes.Interface) auditOpener {
	return func(ctx context.Context, pod string, since time.Time, previous bool) (io.ReadCloser, error) {
		opts := &corev1.PodLogOptions{Timestamps: true, Follow: !previous, Previous: previous}
		if !since.IsZero() {
			from := metav1.NewTime(since)
			opts.SinceTime = &from
		}
		stream, err := cs.CoreV1().Pods(metav1.NamespaceSystem).GetLogs(pod, opts).Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("opening the log: %w", err)
		}
		return stream, nil
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
