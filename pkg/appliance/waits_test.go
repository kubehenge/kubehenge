package appliance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func pod(name string, status corev1.PodStatus) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kubehenge-system", Name: name}, Status: status}
}

func TestPodWait(t *testing.T) {
	t.Parallel()
	unscheduled := corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		Message: "0/1 nodes are available: 1 Insufficient memory.",
	}}}
	waiting := func(reason, message string) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "manager", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
		}}}
	}
	ready := func(s corev1.ConditionStatus) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}, {Type: corev1.PodReady, Status: s},
		}}
	}
	for _, tc := range []struct {
		name   string
		status corev1.PodStatus
		want   string
	}{
		{"not scheduled", unscheduled, "is not scheduled: 0/1 nodes are available: 1 Insufficient memory."},
		{"an image it cannot pull", waiting("ImagePullBackOff", `Back-off pulling image "registry.example/x"`), `waits: container manager is in ImagePullBackOff: Back-off pulling image "registry.example/x"`},
		{"a container being created", waiting("ContainerCreating", ""), "waits: container manager is in ContainerCreating"},
		{"init containers running", waiting("PodInitializing", ""), ""},
		{"a message cut short", waiting("ErrImagePull", strings.Repeat("é", maxWaitMessage+5)), "waits: container manager is in ErrImagePull: " + strings.Repeat("é", maxWaitMessage) + "…"},
		{"failed", corev1.PodStatus{Phase: corev1.PodFailed, Message: "The node was low on resource: memory."}, "failed: The node was low on resource: memory."},
		{"running, not ready", ready(corev1.ConditionFalse), "runs but is not ready"},
		{"running and ready", ready(corev1.ConditionTrue), ""},
		{"done", corev1.PodStatus{Phase: corev1.PodSucceeded}, ""},
	} {
		if got := podWait(pod("p", tc.status)); got != tc.want {
			t.Errorf("%s: podWait = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestWaitingPodsNamesAtMostMaxWaits(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset()
	for i := range maxWaits + 2 {
		p := pod(fmt.Sprintf("p%02d", i), corev1.PodStatus{Phase: corev1.PodFailed, Message: "evicted"})
		if _, err := kube.CoreV1().Pods(p.Namespace).Create(t.Context(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	got := waitingPods(t.Context(), kube)
	if len(got) != maxWaits+1 || got[0] != "Pod kubehenge-system/p00 failed: evicted" || got[maxWaits] != "and more" {
		t.Errorf("waitingPods = %q", got)
	}
}

// TestAFailedApplyNamesWhatThePodsWaitFor fails an apply as a Helm release's wait does, against a
// management cluster whose controller has no room.
func TestAFailedApplyNamesWhatThePodsWaitFor(t *testing.T) {
	t.Parallel()
	pods := corev1.PodList{Items: []corev1.Pod{*pod("kubehenge-controller-1", corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/1 nodes are available: 1 Insufficient memory.",
	}}})}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pods" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pods)
	}))
	t.Cleanup(srv.Close)
	a, _ := newAppliance(t)
	admin := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: m\n  cluster:\n    server: %s\ncontexts:\n- name: m\n  context:\n    cluster: m\n    user: u\nusers:\n- name: u\n  user:\n    token: t\ncurrent-context: m\n", srv.URL)
	if err := os.WriteFile(filepath.Join(a.Dir, "admin.kubeconfig"), []byte(admin), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(settings("fail"), "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	log := strings.Join(j.Log, "\n")
	if j.State != "failed" || !strings.Contains(log, "What the management cluster's pods wait for:\nPod kubehenge-system/kubehenge-controller-1 is not scheduled: 0/1 nodes are available: 1 Insufficient memory.") {
		t.Errorf("job = %+v", j)
	}
}
