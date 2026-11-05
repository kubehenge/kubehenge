package ui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
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
	slice, err := readAuditSlice(strings.NewReader(stamped), auditReadBytes)
	if err != nil {
		t.Fatal(err)
	}
	got := slice.entries
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

type auditLogLine struct {
	at   time.Time
	text string
}

// fakeAuditLogs serves pods' logs as the kubelet does: with its timestamps, from SinceTime in whole
// seconds, and cut after the byte limit, in the middle of a line if need be.
type fakeAuditLogs struct {
	lines map[string][]auditLogLine
	opens map[string][]time.Time
}

func newFakeAuditLogs() *fakeAuditLogs {
	return &fakeAuditLogs{lines: map[string][]auditLogLine{}, opens: map[string][]time.Time{}}
}

// add appends a line n seconds after the base, a change of a person's when id is not empty and
// filler of about 250 bytes otherwise.
func (l *fakeAuditLogs) add(pod string, n int, id string) {
	text := auditLine("filler", "get", "admin", "ResponseComplete",
		`,"objectRef":{"resource":"pods","namespace":"x","name":"y"},"annotations":{"pad":"`+strings.Repeat("p", 60)+`"}`)
	if id != "" {
		text = auditLine(id, "create", "admin", "ResponseComplete",
			`,"objectRef":{"resource":"configmaps","namespace":"default","name":"`+id+`"},"responseStatus":{"code":201}`)
	}
	at := auditBase.Add(time.Duration(n) * time.Second)
	text = strings.Replace(text, "2026-10-13T08:30:00.123456Z", at.Format(time.RFC3339Nano), 1)
	l.lines[pod] = append(l.lines[pod], auditLogLine{at: at, text: text})
}

func (l *fakeAuditLogs) open(_ context.Context, pod string, since time.Time, limit int64) (io.ReadCloser, error) {
	l.opens[pod] = append(l.opens[pod], since)
	var b strings.Builder
	for _, line := range l.lines[pod] {
		if line.at.Before(since.Truncate(time.Second)) {
			continue
		}
		b.WriteString(line.at.Format(time.RFC3339Nano) + " " + line.text + "\n")
	}
	out := b.String()
	if int64(len(out)) > limit {
		out = out[:limit]
	}
	return io.NopCloser(strings.NewReader(out)), nil
}

// auditRig is a server that follows fake API server pods.
type auditRig struct {
	s    *Server
	f    *auditFollower
	logs *fakeAuditLogs
	out  *strings.Builder
}

func newAuditRig(t *testing.T, limit int64, pods ...string) *auditRig {
	t.Helper()
	on := &corev1.ConfigMap{Data: map[string]string{apiAuditLogKey: "true"}}
	on.Namespace, on.Name = "kubehenge-system", interfaceConfigMap
	objects := make([]runtime.Object, 0, len(pods))
	for _, name := range pods {
		objects = append(objects, &corev1.Pod{
			Name: name, Namespace: metav1.NamespaceSystem, UID: types.UID("uid-" + name), Labels: map[string]string{"k8s-app": "kube-apiserver"},
		})
	}
	r := &auditRig{logs: newFakeAuditLogs(), out: &strings.Builder{}}
	r.s = &Server{
		Kube: fake.NewClientBuilder().WithObjects(on).Build(), ClassNamespace: "kubehenge-system",
		Log:          slog.New(slog.NewTextHandler(r.out, nil)),
		NewClientset: func() (kubernetes.Interface, error) { return kubefake.NewClientset(objects...), nil },
	}
	r.s.init()
	r.s.clock = func() time.Time { return auditBase.Add(time.Hour) }
	r.f = newAuditFollower()
	r.f.open, r.f.limit = r.logs.open, limit
	return r
}

func (r *auditRig) read(t *testing.T) {
	t.Helper()
	if err := r.s.readAuditLog(t.Context(), r.f); err != nil {
		t.Fatal(err)
	}
}

func (r *auditRig) kept(t *testing.T) []string {
	t.Helper()
	got, err := r.s.activityLog.since(auditBase.Add(-time.Hour), 1000, func(*activityEntry) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	whats := make([]string, 0, len(got))
	for i := range got {
		whats = append(whats, got[i].What)
	}
	slices.Sort(whats)
	return whats
}

// A read of lines nobody made still moves the position on, so the next read starts after them and
// finds the change that follows.
func TestAnAuditReadWithNoChangeMovesOn(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, 1<<20, "kube-apiserver-a")
	for n := range 20 {
		r.logs.add("kube-apiserver-a", n, "")
	}
	r.read(t)
	if got := r.f.pods["kube-apiserver-a"].from; !got.Equal(auditBase.Add(19 * time.Second)) {
		t.Errorf("the position after lines of nobody's changes = %v", got)
	}
	r.logs.add("kube-apiserver-a", 25, "late")
	r.read(t)
	opens := r.logs.opens["kube-apiserver-a"]
	if got := opens[len(opens)-1]; !got.Equal(auditBase.Add(19 * time.Second)) {
		t.Errorf("the second read started at %v", got)
	}
	if got := r.kept(t); !slices.Equal(got, []string{"created configmaps late"}) {
		t.Errorf("kept %v", got)
	}
}

// A log of many reads' size is read whole in one go, so a change at its end is kept.
func TestAnAuditReadGoesToTheEndOfALongLog(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, 2048, "kube-apiserver-a")
	for n := range 80 {
		r.logs.add("kube-apiserver-a", n, "")
	}
	r.logs.add("kube-apiserver-a", 80, "last")
	r.read(t)
	if got := r.kept(t); !slices.Equal(got, []string{"created configmaps last"}) {
		t.Errorf("kept %v", got)
	}
	if n := len(r.logs.opens["kube-apiserver-a"]); n < 5 {
		t.Errorf("the log was read in %d slices, want it cut up", n)
	}
}

// Each API server pod has a position of its own: a change in the log of one that lags behind the
// other is kept.
func TestAuditPositionsAreKeptPerPod(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, 1<<20, "kube-apiserver-a", "kube-apiserver-b")
	for n := range 10 {
		r.logs.add("kube-apiserver-a", n, "")
	}
	for n := range 50 {
		r.logs.add("kube-apiserver-b", n, "")
	}
	r.read(t)
	a, b := r.f.pods["kube-apiserver-a"].from, r.f.pods["kube-apiserver-b"].from
	if !a.Equal(auditBase.Add(9*time.Second)) || !b.Equal(auditBase.Add(49*time.Second)) {
		t.Errorf("positions = %v and %v", a, b)
	}
	r.logs.add("kube-apiserver-a", 20, "behind")
	r.read(t)
	if got := r.kept(t); !slices.Equal(got, []string{"created configmaps behind"}) {
		t.Errorf("kept %v", got)
	}
}

// Lines the kubelet rotated away before they were read are warned about once, as is a restarted
// API server.
func TestAnAuditRotationGapIsWarnedAboutOnce(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, 1<<20, "kube-apiserver-a")
	for n := range 10 {
		r.logs.add("kube-apiserver-a", n, "")
	}
	r.read(t)
	r.logs.lines["kube-apiserver-a"] = nil
	r.logs.add("kube-apiserver-a", 100, "")
	r.logs.add("kube-apiserver-a", 101, "after")
	r.read(t)
	r.logs.add("kube-apiserver-a", 102, "")
	r.read(t)
	r.read(t)
	if n := strings.Count(r.out.String(), "rotated"); n != 1 {
		t.Errorf("%d warnings of a rotation, want 1: %s", n, r.out)
	}
	if got := r.kept(t); !slices.Equal(got, []string{"created configmaps after"}) {
		t.Errorf("kept %v", got)
	}
}

// A line is kept once however often it is read: on the boundary second of a slice, and on the
// reads that follow.
func TestAnAuditLineIsNeverKeptTwice(t *testing.T) {
	t.Parallel()
	r := newAuditRig(t, 1500, "kube-apiserver-a")
	for n := range 30 {
		r.logs.add("kube-apiserver-a", n, "")
		if n%3 == 0 {
			r.logs.add("kube-apiserver-a", n, fmt.Sprintf("c%02d", n))
		}
	}
	r.read(t)
	r.read(t)
	r.logs.add("kube-apiserver-a", 30, "c30")
	r.read(t)
	r.read(t)
	got := r.kept(t)
	if len(got) != 11 || len(slices.Compact(slices.Clone(got))) != 11 {
		t.Errorf("kept %d changes, want 11 different ones: %v", len(got), got)
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
