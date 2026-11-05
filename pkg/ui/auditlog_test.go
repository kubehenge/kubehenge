package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func auditLine(id, verb, user, stage, extra string) string {
	return `{"kind":"Event","apiVersion":"audit.k8s.io/v1","level":"Metadata","auditID":"` + id + `","stage":"` + stage +
		`","verb":"` + verb + `","user":{"username":"` + user + `","groups":["system:masters"]},"sourceIPs":["10.0.0.9"],` +
		`"stageTimestamp":"2026-10-13T08:30:00.123456Z"` + extra + `}`
}

// Among the API server's own lines, the audit events of people's changes become entries: a
// Cluster's with the cluster, an impersonated change as the user acted for, and a refused one
// marked. A GitOps tool's service account counts. The platform's controllers, nodes, access
// reviews, leases and events are left out.
func TestTheAuditLogReadsTheChangesPeopleMade(t *testing.T) {
	t.Parallel()
	log := strings.Join([]string{
		`I1013 08:30:00.000000       1 controller.go:615] quota admission added evaluator for: leases`,
		auditLine("a1", "patch", "admin", "ResponseComplete",
			`,"objectRef":{"resource":"clusters","namespace":"clusters","name":"web","apiGroup":"cluster.x-k8s.io"},"responseStatus":{"code":200}`),
		auditLine("a2", "delete", "admin", "ResponseComplete",
			`,"impersonatedUser":{"username":"alice"},"objectRef":{"resource":"secrets","namespace":"clusters","name":"web-kubeconfig"},"responseStatus":{"code":403}`),
		auditLine("a3", "create", "admin", "ResponseStarted", `,"objectRef":{"resource":"pods","namespace":"x","name":"y"}`),
		auditLine("a4", "get", "admin", "ResponseComplete", `,"objectRef":{"resource":"pods","namespace":"x","name":"y"}`),
		auditLine("a5", "create", "bob", "ResponseComplete", `,"objectRef":{"resource":"pods","namespace":"x","name":"y","subresource":"exec"}`),
		auditLine("a6", "update", "system:serviceaccount:capi-system:capi-manager", "ResponseComplete",
			`,"objectRef":{"resource":"clusters","namespace":"clusters","name":"web","apiGroup":"cluster.x-k8s.io"}`),
		auditLine("a11", "update", "system:serviceaccount:kubehenge-system:kubehenge-interface", "ResponseComplete",
			`,"objectRef":{"resource":"clusters","namespace":"clusters","name":"web","apiGroup":"cluster.x-k8s.io"}`),
		auditLine("a12", "update", "system:serviceaccount:kube-system:generic-garbage-collector", "ResponseComplete",
			`,"objectRef":{"resource":"secrets","namespace":"clusters","name":"web"}`),
		auditLine("a13", "patch", "system:serviceaccount:argocd:argocd-application-controller", "ResponseComplete",
			`,"objectRef":{"resource":"clusters","namespace":"clusters","name":"db","apiGroup":"cluster.x-k8s.io"}`),
		auditLine("a14", "patch", "system:serviceaccount:flux-system:kustomize-controller", "ResponseComplete",
			`,"objectRef":{"resource":"clusters","namespace":"clusters","name":"cache","apiGroup":"cluster.x-k8s.io"}`),
		auditLine("a7", "patch", "system:node:mgmt-1", "ResponseComplete", `,"objectRef":{"resource":"nodes","name":"mgmt-1"}`),
		auditLine("a8", "create", "bob", "ResponseComplete",
			`,"objectRef":{"resource":"selfsubjectaccessreviews","apiGroup":"authorization.k8s.io"}`),
		auditLine("a9", "update", "bob", "ResponseComplete",
			`,"objectRef":{"resource":"leases","namespace":"x","name":"y","apiGroup":"coordination.k8s.io"}`),
		auditLine("a10", "create", "bob", "ResponseComplete", `,"objectRef":{"resource":"events","namespace":"x","name":"y"}`),
		`{"not":"an event"}`,
	}, "\n")
	stamped := "2026-10-13T08:30:00.123456789Z " + strings.ReplaceAll(log, "\n", "\n2026-10-13T08:30:00.123456789Z ") + "\n"
	var got []activityEntry
	for line := range strings.Lines(stamped) {
		at, message, ok := stampedLine([]byte(strings.TrimSuffix(line, "\n")))
		if !ok || !at.Equal(time.Date(2026, 10, 13, 8, 30, 0, 123456789, time.UTC)) {
			t.Fatalf("the kubelet's time on %q = %v, %v", line, at, ok)
		}
		if e := auditEntry(message); e != nil {
			got = append(got, *e)
		}
	}
	if len(got) != 5 {
		t.Fatalf("entries = %+v, want the five changes that completed", got)
	}
	cluster, refused, exec, gitops, flux := got[0], got[1], got[2], got[3], got[4]
	if cluster.User != "admin" || cluster.What != "changed clusters web" || cluster.Cluster != "clusters/web" ||
		cluster.Details["address"] != "10.0.0.9" || cluster.Details[auditIDKey] != "a1" || cluster.Details[auditVia] != auditViaAPI {
		t.Errorf("the Cluster's change = %+v", cluster)
	}
	if !cluster.At.Equal(time.Date(2026, 10, 13, 8, 30, 0, 123456000, time.UTC)) {
		t.Errorf("at %v", cluster.At)
	}
	if refused.User != "alice" || refused.Details["by"] != "admin" || refused.Details["refused"] != "403" ||
		refused.Details["namespace"] != "clusters" || refused.Cluster != "" || refused.What != "deleted secrets web-kubeconfig" {
		t.Errorf("the refused change = %+v", refused)
	}
	if exec.What != "created pods/exec y" || exec.User != "bob" {
		t.Errorf("the exec = %+v", exec)
	}
	if gitops.User != "system:serviceaccount:argocd:argocd-application-controller" || gitops.Cluster != "clusters/db" {
		t.Errorf("the GitOps change = %+v", gitops)
	}
	if flux.User != "system:serviceaccount:flux-system:kustomize-controller" || flux.Cluster != "clusters/cache" {
		t.Errorf("Flux's change = %+v", flux)
	}
}

// Every namespace the management cluster module puts a controller in is among the platform's, so
// a provider the module gains never shows up in Activity as a person.
func TestThePlatformNamespacesCoverTheModule(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("../../management-cluster/*.tf")
	if err != nil || len(files) == 0 {
		t.Fatalf("the module's files: %v", err)
	}
	namespace := regexp.MustCompile(`namespace\s*=\s*"([a-z0-9-]+)"`)
	found := map[string]bool{"kube-system": true}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // the module's own files
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range namespace.FindAllSubmatch(raw, -1) {
			found[string(m[1])] = true
			if !platformNamespaces[string(m[1])] {
				t.Errorf("%s installs into %s, which Activity would list as a person", filepath.Base(f), m[1])
			}
		}
	}
	for ns := range platformNamespaces {
		if !found[ns] {
			t.Errorf("%s is no namespace of the module's, so Activity leaves out whoever uses it", ns)
		}
	}
}

// A line read again is kept once. A new follower starts a second before the newest change the
// activity log holds already.
func TestTheAuditLogKeepsEachChangeOnce(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.init()
	at := time.Date(2026, 10, 13, 8, 30, 0, 0, time.UTC)
	entry := func(id string, after time.Duration) activityEntry {
		return activityEntry{At: at.Add(after), User: "admin", What: "changed clusters " + id, Details: map[string]string{auditVia: auditViaAPI, auditIDKey: id}}
	}
	f := newAuditFollower()
	if err := s.keepAuditEntries(f, []activityEntry{entry("b", time.Second), entry("a", 0)}); err != nil {
		t.Fatal(err)
	}
	if err := s.keepAuditEntries(f, []activityEntry{entry("b", time.Second), entry("c", 2*time.Second)}); err != nil {
		t.Fatal(err)
	}
	kept, _ := s.activityLog.since(at.Add(-time.Hour), 10, func(*activityEntry) bool { return true })
	if len(kept) != 3 || kept[0].What != "changed clusters c" || kept[2].What != "changed clusters a" {
		t.Errorf("kept %+v", kept)
	}

	s.clock = func() time.Time { return at.Add(time.Hour) }
	again := newAuditFollower()
	s.startAuditFollower(again)
	if !again.start.Equal(at.Add(time.Second)) {
		t.Errorf("a new follower starts at %v", again.start)
	}
	if err := s.keepAuditEntries(again, []activityEntry{entry("c", 2*time.Second)}); err != nil {
		t.Fatal(err)
	}
	if kept, _ := s.activityLog.since(at.Add(-time.Hour), 10, func(*activityEntry) bool { return true }); len(kept) != 3 {
		t.Errorf("a new follower kept a change twice: %+v", kept)
	}
}

var auditBase = time.Date(2026, 10, 13, 8, 30, 0, 0, time.UTC)

const apiServerA = "kube-apiserver-a"

var errConnectionLost = errors.New("connection lost")

// auditRig is a follower of the fake API server's pods, whose ticks the test makes.
type auditRig struct {
	api   *apiServerFake
	s     *Server
	f     *auditFollower
	out   *syncBuffer
	clock atomic.Int64
}

func newAuditRig(t *testing.T, pods ...string) *auditRig {
	t.Helper()
	r := &auditRig{api: newAPIServerFake(t, pods...), out: &syncBuffer{}, f: newAuditFollower()}
	r.clock.Store(auditBase.Add(time.Hour).UnixNano())
	r.s = r.api.server(t, r.out, func() time.Time { return time.Unix(0, r.clock.Load()).UTC() })
	t.Cleanup(r.f.stopAll)
	return r
}

func (r *auditRig) tick(t *testing.T) {
	t.Helper()
	if err := r.s.followAuditPods(t.Context(), r.f); err != nil {
		t.Fatal(err)
	}
}

// wait waits until done holds of the follower, and fails after a few seconds.
func (r *auditRig) wait(t *testing.T, what string, done func(f *auditFollower) bool) {
	t.Helper()
	for range 300 {
		r.f.mu.Lock()
		ok := done(r.f)
		r.f.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited in vain for %s", what)
}

// waitRead waits until the follower has read a pod's log to the line n seconds after auditBase.
func (r *auditRig) waitRead(t *testing.T, pod string, n int) {
	t.Helper()
	want := auditBase.Add(time.Duration(n) * time.Second)
	r.wait(t, fmt.Sprintf("%s's log read to %v", pod, want), func(f *auditFollower) bool {
		return f.pods[pod] != nil && !f.pods[pod].last.Before(want)
	})
}

// cut ends the stream on a pod's log, with err as a connection lost and nil as a container that
// stopped, and waits until the follower has seen it end.
func (r *auditRig) cut(t *testing.T, pod string, err error) {
	t.Helper()
	r.api.end(pod, err)
	r.wait(t, pod+"'s stream to end", func(f *auditFollower) bool { return !f.pods[pod].streaming() })
}

func (r *auditRig) warnings(about string) int { return strings.Count(r.out.String(), about) }

// A stream that ended opens again at the next tick, from the second of the last line read, and the
// change written meanwhile is kept.
func TestAnAuditStreamOpensAgainWhereTheLastEnded(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, apiServerA)
	for n := range 20 {
		r.api.add(apiServerA, n, "")
	}
	r.tick(t)
	r.waitRead(t, apiServerA, 19)
	r.cut(t, apiServerA, errConnectionLost)
	r.api.add(apiServerA, 25, "late")
	r.tick(t)
	waitAuditKept(t, r.s, "created configmaps late")
	if opens := r.api.openedFrom(apiServerA); len(opens) != 2 || !opens[1].Equal(auditBase.Add(19*time.Second)) {
		t.Errorf("the streams opened from %v, want the second from the last line read", opens)
	}
}

// A change written to an open stream is kept without waiting for a tick, and each API server pod
// has a position of its own.
func TestAuditPositionsAreKeptPerPod(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, apiServerA, "kube-apiserver-b")
	for n := range 10 {
		r.api.add(apiServerA, n, "")
	}
	for n := range 50 {
		r.api.add("kube-apiserver-b", n, "")
	}
	r.tick(t)
	r.waitRead(t, apiServerA, 9)
	r.waitRead(t, "kube-apiserver-b", 49)
	r.f.mu.Lock()
	a, b := r.f.pods[apiServerA].from, r.f.pods["kube-apiserver-b"].from
	r.f.mu.Unlock()
	if !a.Equal(auditBase.Add(9*time.Second)) || !b.Equal(auditBase.Add(49*time.Second)) {
		t.Errorf("positions = %v and %v", a, b)
	}
	r.api.add(apiServerA, 20, "behind")
	waitAuditKept(t, r.s, "created configmaps behind")
}

// Lines the kubelet rotated away while no stream followed the log are warned about once.
func TestAnAuditRotationGapIsWarnedAboutOnce(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, apiServerA)
	for n := range 10 {
		r.api.add(apiServerA, n, "")
	}
	r.tick(t)
	r.waitRead(t, apiServerA, 9)
	r.cut(t, apiServerA, errConnectionLost)
	r.api.rotate(apiServerA)
	r.api.add(apiServerA, 100, "")
	r.api.add(apiServerA, 101, "after")
	r.tick(t)
	waitAuditKept(t, r.s, "created configmaps after")
	r.api.add(apiServerA, 102, "")
	r.waitRead(t, apiServerA, 102)
	r.cut(t, apiServerA, errConnectionLost)
	r.api.add(apiServerA, 103, "")
	r.tick(t)
	r.waitRead(t, apiServerA, 103)
	if n := r.warnings("rotated"); n != 1 {
		t.Errorf("%d warnings of a rotation, want 1: %s", n, r.out)
	}
}

// An API server pod replaced, or its container restarted, starts a new log, which is read from its
// start. Lines are said to be lost only when the stream did not reach the old container's end.
func TestARestartedAPIServerIsReadFromItsStart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		change func(*corev1.Pod)
		end    error
		warns  int
	}{
		"a new pod, the old log read to its end": {func(p *corev1.Pod) { p.UID = "replacement" }, nil, 0},
		"a restart, the stream cut": {
			func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "kube-apiserver", RestartCount: 1}}
			}, errConnectionLost, 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newAuditRig(t, apiServerA)
			for n := range 10 {
				r.api.add(apiServerA, n, "")
			}
			r.tick(t)
			r.waitRead(t, apiServerA, 9)
			r.cut(t, apiServerA, tc.end)
			r.api.pod(apiServerA, tc.change)
			r.api.rotate(apiServerA)
			r.api.add(apiServerA, 1, "fresh")
			r.tick(t)
			waitAuditKept(t, r.s, "created configmaps fresh")
			r.tick(t)
			if n := r.warnings("restarted"); n != tc.warns {
				t.Errorf("%d warnings of a restart, want %d: %s", n, tc.warns, r.out)
			}
			if opens := r.api.openedFrom(apiServerA); len(opens) != 2 || !opens[1].IsZero() {
				t.Errorf("the streams opened from %v, want the second at the log's start", opens)
			}
		})
	}
}

// A line is kept once however often it is read: on the boundary second a stream opens again at,
// and on the streams that follow.
func TestAnAuditLineIsNeverKeptTwice(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, apiServerA)
	for n := range 30 {
		r.api.add(apiServerA, n, "")
		if n%3 == 0 {
			r.api.add(apiServerA, n, fmt.Sprintf("c%02d", n))
		}
	}
	r.tick(t)
	r.api.add(apiServerA, 30, "c30")
	r.waitRead(t, apiServerA, 30)
	for n := 31; n < 33; n++ {
		r.cut(t, apiServerA, errConnectionLost)
		r.tick(t)
		r.api.add(apiServerA, n, "")
		r.waitRead(t, apiServerA, n)
	}
	want := []string{"created configmaps c30"}
	for n := 0; n < 30; n += 3 {
		want = append(want, fmt.Sprintf("created configmaps c%02d", n))
	}
	slices.Sort(want)
	waitAuditKept(t, r.s, want...)
}

// A stream that brings no line for a while hangs on a dead connection, so the next tick opens it
// again.
func TestAQuietAuditStreamOpensAgain(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, apiServerA)
	r.api.add(apiServerA, 0, "")
	r.tick(t)
	r.waitRead(t, apiServerA, 0)
	r.tick(t)
	if opens := r.api.openedFrom(apiServerA); len(opens) != 1 {
		t.Fatalf("a live stream was opened again: %v", opens)
	}
	r.clock.Add(int64(auditQuiet))
	r.tick(t)
	r.api.add(apiServerA, 1, "after")
	waitAuditKept(t, r.s, "created configmaps after")
	if opens := r.api.openedFrom(apiServerA); len(opens) != 2 || r.warnings("no line") != 1 {
		t.Errorf("the streams opened from %v, with %d warnings of a quiet stream", opens, r.warnings("no line"))
	}
}

// Switching the audit log off ends the streams.
func TestSwitchingTheAuditLogOffEndsItsStreams(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, apiServerA)
	r.api.add(apiServerA, 0, "")
	r.tick(t)
	r.waitRead(t, apiServerA, 0)
	off := &corev1.ConfigMap{}
	if err := r.s.Kube.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: interfaceConfigMap}, off); err != nil {
		t.Fatal(err)
	}
	off.Data[apiAuditLogKey] = "false"
	if err := r.s.Kube.Update(t.Context(), off); err != nil {
		t.Fatal(err)
	}
	r.tick(t)
	r.f.mu.Lock()
	streaming := r.f.pods[apiServerA].streaming()
	r.f.mu.Unlock()
	if streaming {
		t.Error("a stream still follows the API server's log with the audit log off")
	}
}

// The interface reads the audit log only once the chart says the API server writes it.
func TestTheAuditLogIsReadOnlyWhenWritten(t *testing.T) {
	t.Parallel()
	on := &corev1.ConfigMap{Data: map[string]string{apiAuditLogKey: "true"}}
	on.Namespace, on.Name = "kubehenge-system", interfaceConfigMap
	for _, tc := range []struct {
		kube *fake.ClientBuilder
		want bool
	}{
		{fake.NewClientBuilder(), false},
		{fake.NewClientBuilder().WithObjects(on), true},
	} {
		got, err := auditLogOn(t.Context(), tc.kube.Build(), "kubehenge-system")
		if err != nil || got != tc.want {
			t.Errorf("auditLogOn = %v, %v, want %v", got, err, tc.want)
		}
	}
}
