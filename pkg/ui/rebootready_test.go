package ui

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// controlPlaneNode is a management cluster node with its kube-apiserver pod, which is ready or not.
func controlPlaneNode(name string, status corev1.ConditionStatus) []client.Object {
	node := managementNodeObject(name, corev1.ConditionTrue, "")
	node.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
	pod := &corev1.Pod{
		Namespace: metav1.NamespaceSystem, Name: "kube-apiserver-" + name, Spec: corev1.PodSpec{NodeName: name},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
	return []client.Object{node, pod}
}

// etcdRecord is the controller's record, taken at checked, with the clusters' members.
func etcdRecord(t *testing.T, checked time.Time, clusters ...provisioner.EtcdSet) *corev1.ConfigMap {
	t.Helper()
	raw, err := json.Marshal(provisioner.EtcdHealth{Checked: checked, Nodes: []string{"pve1", "pve2", "pve3"}, Clusters: clusters})
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{Namespace: "kubehenge-system", Name: provisioner.EtcdHealthName, Data: map[string]string{provisioner.EtcdHealthKey: string(raw)}}
}

// webMembers is cluster web's members on pve1 to pve3, each healthy or not.
func webMembers(healthy ...bool) provisioner.EtcdSet {
	s := provisioner.EtcdSet{Cluster: "default/web"}
	for i, ok := range healthy {
		m := provisioner.EtcdMember{Machine: "web-cp-" + strconv.Itoa(i), Node: "pve" + strconv.Itoa(i+1), Healthy: ok}
		if !ok {
			m.Reason = "its VM is stopped"
		}
		s.Members = append(s.Members, m)
	}
	return s
}

// managementNodes are the management cluster's control-plane nodes, whose kube-apiserver pods are ready or not.
func managementNodes(apiserverReady ...bool) []client.Object {
	out := make([]client.Object, 0, 2*len(apiserverReady))
	for i, ok := range apiserverReady {
		status := corev1.ConditionFalse
		if ok {
			status = corev1.ConditionTrue
		}
		out = append(out, controlPlaneNode("kubehenge-cp-"+strconv.Itoa(i), status)...)
	}
	return out
}

func readRebootReady(t *testing.T, h *harness, user, query string) (rebootReadyView, int) {
	t.Helper()
	b := h.browser()
	b.signIn(user)
	rec := b.do("GET", "/api/appliance/reboot-ready"+query, "")
	var v rebootReadyView
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
	}
	return v, rec.Code
}

func TestRebootReady(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	fresh := clock.Add(-time.Minute)
	solo := provisioner.EtcdSet{Cluster: "default/solo", Members: []provisioner.EtcdMember{{Machine: "solo-cp-0", Node: "pve3", Healthy: true}}}
	for _, tc := range []struct {
		name    string
		objects []client.Object
		query   string
		ready   bool
		blocker string
	}{
		{"everything healthy", append(managementNodes(true, true, true), etcdRecord(t, fresh, webMembers(true, true, true))), "", true, ""},
		{"a node, everything healthy", append(managementNodes(true, true, true), etcdRecord(t, fresh, webMembers(true, true, true))), "?node=pve2", true, ""},
		{
			"a workload member is down",
			append(managementNodes(true, true, true), etcdRecord(t, fresh, webMembers(true, false, true))), "", false,
			"default/web: etcd member web-cp-1 on pve2 is not healthy: its VM is stopped",
		},
		{
			"a management member is down",
			append(managementNodes(true, false, true), etcdRecord(t, fresh, webMembers(true, true, true))), "?node=pve1", false,
			"management: etcd member kubehenge-cp-1 is not healthy",
		},
		{
			"the node holds the quorum",
			append(managementNodes(true, true, true), etcdRecord(t, fresh, solo)), "?node=pve3", false,
			"default/solo: stopping pve3 would leave 0 of 1 etcd members",
		},
		{
			"a record the controller let go stale",
			append(managementNodes(true, true, true), etcdRecord(t, clock.Add(-time.Hour), webMembers(true, true, true))), "", false, "has not checked",
		},
		{"no record", managementNodes(true, true, true), "", false, "not recorded"},
		{"a node outside the cluster", append(managementNodes(true, true, true), etcdRecord(t, fresh, webMembers(true, true, true))), "?node=pve9", false, "pve9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.objects...)
			v, code := readRebootReady(t, h, "admin@pve", tc.query)
			if code != http.StatusOK {
				t.Fatalf("status %d", code)
			}
			if v.Answer.Ready != tc.ready || !strings.Contains(v.Answer.Blocker, tc.blocker) || (tc.blocker == "") != (v.Answer.Blocker == "") {
				t.Errorf("answer %+v, want ready %v with %q", v.Answer, tc.ready, tc.blocker)
			}
		})
	}
}

// Every node is answered for itself, and the whole answer is the all-healthy one.
func TestRebootReadyAnswersEachNode(t *testing.T) {
	t.Parallel()
	solo := provisioner.EtcdSet{Cluster: "default/solo", Members: []provisioner.EtcdMember{{Machine: "solo-cp-0", Node: "pve3", Healthy: true}}}
	h := newHarness(t, append(managementNodes(true, true, true), etcdRecord(t, time.Date(2026, 10, 13, 8, 59, 0, 0, time.UTC), solo))...)
	v, _ := readRebootReady(t, h, "admin@pve", "")
	if !v.Ready || len(v.Nodes) != 3 {
		t.Fatalf("readiness %+v", v.RebootReadiness)
	}
	var blocked []string
	for _, n := range v.Nodes {
		if !n.Ready {
			blocked = append(blocked, n.Node)
		}
	}
	if !slices.Equal(blocked, []string{"pve3"}) {
		t.Errorf("blocked %v, want pve3 alone: %+v", blocked, v.Nodes)
	}
}

func TestRebootReadyNeedsSetupPrivileges(t *testing.T) {
	t.Parallel()
	h := newHarness(t, managementNodes(true, true, true)...)
	if _, code := readRebootReady(t, h, "viewer@pve", ""); code != http.StatusForbidden {
		t.Errorf("status %d, want 403", code)
	}
}

func TestManagementHealthCountsEtcdMembers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, managementNodes(true, false, true)...)
	got := readHealth(t, h, "admin@pve", http.StatusOK)
	if got.Etcd == nil || got.Etcd.Healthy != 2 || got.Etcd.Members != 3 {
		t.Errorf("etcd %+v", got.Etcd)
	}
}

// loseQuorum marks cluster one as the controller does when its API is silent and a majority of its
// control-plane machines do not run.
func (h *etcdHarness) loseQuorum() {
	h.t.Helper()
	c := h.cluster()
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
		Type: provisioner.QuorumLostCondition, Status: metav1.ConditionTrue, Reason: provisioner.QuorumLostReason,
		Message:            "1 of 3 control-plane machines have a running VM (one-cp-a), and etcd needs 2. The API has been silent for 10m0s or more. Start what stopped if it can be.",
		LastTransitionTime: metav1.Now(),
	})
	if err := h.kube.Update(h.t.Context(), c); err != nil {
		h.t.Fatal(err)
	}
}

func TestRecoverControlPlaneAfterQuorumLoss(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	h.setClass(map[string]string{provisioner.AutomaticRestoreAnnotation: "true"})
	b := h.browser()
	b.signIn("dev@pve")
	body := `{"machine":"one-cp-a","confirm":"one"}`
	expect(t, b.do("POST", "/api/clusters/default/one/control-plane-recovery", body), http.StatusConflict)
	h.loseQuorum()
	expect(t, b.do("POST", "/api/clusters/default/one/control-plane-recovery", body), http.StatusAccepted)
	req, ok, err := provisioner.EtcdRestoreRequestOf(h.cluster())
	if !ok || err != nil || req.Machine != "one-cp-a" || req.By != "dev@pve" {
		t.Errorf("request %+v, %v", req, err)
	}
}

func TestHealthSaysTheQuorumIsLost(t *testing.T) {
	t.Parallel()
	c := &clusterv1.Cluster{Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
		Type: provisioner.QuorumLostCondition, Status: metav1.ConditionTrue, Reason: provisioner.QuorumLostReason,
		Message: "1 of 3 control-plane machines have a running VM (web-cp-0), and etcd needs 2. The API has been silent for 10m0s or more. Start what stopped if it can be.",
	}}}}
	var a attention
	watchAttention(c, &a)
	if len(a) != 1 || a[0].Severity != severityError || a[0].Section != sectionControlPlane || a[0].What != "Control-plane quorum lost" ||
		!strings.HasSuffix(a[0].Message, "silent for 10m0s or more.") {
		t.Errorf("attention %+v", a)
	}
}
