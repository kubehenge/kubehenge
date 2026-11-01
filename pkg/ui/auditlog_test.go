package ui

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
		auditLine("a7", "patch", "system:node:mgmt-1", "ResponseComplete", `,"objectRef":{"resource":"nodes","name":"mgmt-1"}`),
		auditLine("a8", "create", "bob", "ResponseComplete",
			`,"objectRef":{"resource":"selfsubjectaccessreviews","apiGroup":"authorization.k8s.io"}`),
		auditLine("a9", "update", "bob", "ResponseComplete",
			`,"objectRef":{"resource":"leases","namespace":"x","name":"y","apiGroup":"coordination.k8s.io"}`),
		auditLine("a10", "create", "bob", "ResponseComplete", `,"objectRef":{"resource":"events","namespace":"x","name":"y"}`),
		`{"not":"an event"}`,
	}, "\n")
	got, err := auditEntries(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("entries = %+v, want the four changes that completed", got)
	}
	cluster, refused, exec, gitops := got[0], got[1], got[2], got[3]
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
}

// A line read again is kept once, the next read starts a second before the newest line kept, and a
// read with nothing new leaves the start where it was. A new follower starts after what the
// activity log holds already.
func TestTheAuditLogKeepsEachChangeOnce(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.init()
	at := time.Date(2026, 10, 13, 8, 30, 0, 0, time.UTC)
	entry := func(id string, after time.Duration) activityEntry {
		return activityEntry{At: at.Add(after), User: "admin", What: "changed clusters " + id, Details: map[string]string{auditVia: auditViaAPI, auditIDKey: id}}
	}
	f := &auditFollower{since: at.Add(-time.Hour), seen: map[string]time.Time{}}
	if err := s.keepAuditEntries(f, []activityEntry{entry("b", time.Second), entry("a", 0)}); err != nil {
		t.Fatal(err)
	}
	if err := s.keepAuditEntries(f, []activityEntry{entry("b", time.Second), entry("c", 2*time.Second)}); err != nil {
		t.Fatal(err)
	}
	if !f.since.Equal(at.Add(time.Second)) {
		t.Errorf("the next read starts at %v", f.since)
	}
	if err := s.keepAuditEntries(f, nil); err != nil || !f.since.Equal(at.Add(time.Second)) {
		t.Errorf("a read with nothing new moved the start to %v", f.since)
	}
	kept, _ := s.activityLog.since(at.Add(-time.Hour), 10, func(*activityEntry) bool { return true })
	if len(kept) != 3 || kept[0].What != "changed clusters c" || kept[2].What != "changed clusters a" {
		t.Errorf("kept %+v", kept)
	}

	s.clock = func() time.Time { return at.Add(time.Hour) }
	again := &auditFollower{seen: map[string]time.Time{}}
	s.startAuditFollower(again)
	if !again.since.Equal(at.Add(time.Second)) {
		t.Errorf("a new follower starts at %v", again.since)
	}
	if err := s.keepAuditEntries(again, []activityEntry{entry("c", 2*time.Second)}); err != nil {
		t.Fatal(err)
	}
	if kept, _ := s.activityLog.since(at.Add(-time.Hour), 10, func(*activityEntry) bool { return true }); len(kept) != 3 {
		t.Errorf("a new follower kept a change twice: %+v", kept)
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
