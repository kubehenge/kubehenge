package ui

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const teamNodes = `{"labels":[{"key":"team","value":"a"}],"taints":[{"key":"dedicated","value":"db","effect":"NoSchedule"}]}`

func TestAPoolCarriesItsLabelsAndTaints(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")

	rec := b.do("PATCH", "/api/clusters/default/one", `{"pools":{"md-0":{"nodes":`+teamNodes+`}}}`)
	expect(t, rec, http.StatusOK)
	var view clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(view.WorkerPools[0].Nodes, json.RawMessage(teamNodes)) {
		t.Fatalf("pool shown with nodes %s", view.WorkerPools[0].Nodes)
	}
	if pools := poolsOf(t, h); len(pools[0].Variables.Overrides) != 1 || pools[0].Variables.Overrides[0].Name != workerNodesVariable {
		t.Fatalf("the pool = %+v", pools[0])
	}
	// A pool added with them, and the pool back on the cluster's.
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"pools":{"db":{"replicas":1,"nodes":`+teamNodes+`}}}`), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"pools":{"md-0":{"nodes":null}}}`), http.StatusOK)
	if pools := poolsOf(t, h); len(pools[0].Variables.Overrides) != 0 || len(pools[1].Variables.Overrides) != 1 {
		t.Fatalf("pools after the change = %+v", pools)
	}
}

func TestLabelsAndTaintsReplaceThePoolsNodes(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	c.Spec.Topology.Workers.MachineDeployments[0].Variables.Overrides = []clusterv1.ClusterVariable{
		{Name: workerNodesVariable, Value: apiextensionsv1.JSON{Raw: []byte(teamNodes)}},
	}
	h := newHarness(t, c)
	b := h.browser()
	b.signIn("dev@pve")
	plan := func(body string) []plannedChange {
		t.Helper()
		rec := b.do("POST", "/api/clusters/default/one/plan", body)
		expect(t, rec, http.StatusOK)
		var p planView
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p.Changes
	}
	replaces := []plannedChange{{Setting: "pools", Effect: effectWorkers}}
	if got := plan(`{"pools":{"md-0":{"nodes":{"labels":[{"key":"team","value":"b"}],"taints":[]}}}}`); !slices.Equal(got, replaces) {
		t.Errorf("new labels plan %+v", got)
	}
	if got := plan(`{"pools":{"md-0":{"nodes":null}}}`); !slices.Equal(got, replaces) {
		t.Errorf("taking the cluster's plan %+v", got)
	}
	if got := plan(`{"pools":{"md-0":{"nodes":` + teamNodes + `}}}`); !slices.Equal(got, []plannedChange{{Setting: "pools", Effect: effectScales}}) {
		t.Errorf("the same labels and taints plan %+v", got)
	}
}

func TestCISMayBeSwitchedOnLater(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("POST", "/api/clusters/default/one/plan", `{"variables":{"cis":true}}`)
	expect(t, rec, http.StatusOK)
	var p planView
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || p.Changes[0].Setting != "cis" {
		t.Errorf("plan = %+v", p.Changes)
	}
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"variables":{"cis":true}}`), http.StatusOK)
}

// The roll is the cluster's webhook, which shows in the integration tier: the interface only lets
// the variable through.
func TestLabelsAndTaintsAreListedAsChangeable(t *testing.T) {
	t.Parallel()
	for _, name := range []string{workerNodesVariable, cisVariable} {
		if !changeableVariables[name] {
			t.Errorf("%s cannot change after creation", name)
		}
	}
}

func nodeWith(name string, taints ...corev1.Taint) corev1.Node {
	return corev1.Node{
		Name:   name,
		Spec:   corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func notReadyNode(name string) corev1.Node {
	n := nodeWith(name)
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	return n
}

func workerMachine(name, node string) clusterv1.Machine {
	m := clusterv1.Machine{Name: name, Labels: map[string]string{clusterv1.ClusterTopologyMachineDeploymentNameLabel: "md-0"}}
	m.Status.NodeRef = clusterv1.MachineNodeReference{Name: node}
	return m
}

func attentionWhat(a attention) []string {
	out := make([]string, 0, len(a))
	for _, item := range a {
		out = append(out, item.What)
	}
	return out
}

func TestHealthNotesATaintThePoolDoesNotSet(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := cluster("default", "one")
	c.Spec.Topology.Workers.MachineDeployments[0].Variables.Overrides = []clusterv1.ClusterVariable{
		{Name: workerNodesVariable, Value: apiextensionsv1.JSON{Raw: []byte(teamNodes)}},
	}
	m := &managementRead{machines: []clusterv1.Machine{workerMachine("one-md-0-a", "w1"), workerMachine("one-md-0-b", "w2"), workerMachine("one-md-0-c", "w3")}}
	wl := &workloadRead{nodes: []corev1.Node{
		// By hand: a taint the pool lacks, beside the scheduler's own, which are not listed.
		nodeWith("w1", corev1.Taint{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}, corev1.Taint{Key: "gpu", Effect: corev1.TaintEffectNoExecute},
			corev1.Taint{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule}),
		// As the pool sets it.
		nodeWith("w2", corev1.Taint{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}),
		// Taken off by hand.
		nodeWith("w3"),
	}}
	var a attention
	nodesSection(c, m, wl, now, &a)
	var got []attentionView
	for _, item := range a {
		if strings.Contains(item.What, "taints differ") {
			got = append(got, item)
		}
	}
	if len(got) != 2 {
		t.Fatalf("notes %v", attentionWhat(a))
	}
	byMachine := map[string]attentionView{got[0].Machine: got[0], got[1].Machine: got[1]}
	if n := byMachine["one-md-0-a"]; !strings.Contains(n.What, "pool md-0") || !strings.Contains(n.Message, "Not in the pool's taints: gpu:NoExecute") ||
		strings.Contains(n.Message, "dedicated") || strings.Contains(n.Message, "unschedulable") || n.Severity != severityWarning {
		t.Errorf("the hand-set taint: %+v", n)
	}
	if n := byMachine["one-md-0-c"]; !strings.Contains(n.Message, "Missing from the node: dedicated=db:NoSchedule") {
		t.Errorf("the missing taint: %+v", n)
	}
}

func TestHealthLeavesControlPlaneAndClusterTaintsAlone(t *testing.T) {
	t.Parallel()
	c := cluster("default", "one")
	cp := clusterv1.Machine{Name: "one-cp-a", Labels: map[string]string{clusterv1.MachineControlPlaneLabel: ""}}
	cp.Status.NodeRef = clusterv1.MachineNodeReference{Name: "cp1"}
	m := &managementRead{machines: []clusterv1.Machine{cp, workerMachine("one-md-0-a", "w1")}}
	wl := &workloadRead{nodes: []corev1.Node{
		nodeWith("cp1", corev1.Taint{Key: "custom", Effect: corev1.TaintEffectNoSchedule}),
		nodeWith("w1", corev1.Taint{Key: "ToBeDeletedByClusterAutoscaler", Effect: corev1.TaintEffectNoSchedule},
			corev1.Taint{Key: "node.cloudprovider.kubernetes.io/uninitialized", Effect: corev1.TaintEffectNoSchedule},
			corev1.Taint{Key: controlPlaneTaint, Effect: corev1.TaintEffectNoSchedule}),
	}}
	var a attention
	nodesSection(c, m, wl, time.Now(), &a)
	for _, what := range attentionWhat(a) {
		if strings.Contains(what, "taints differ") {
			t.Errorf("a note %q", what)
		}
	}
}

func TestHealthNotesAClockOffTheControllers(t *testing.T) {
	t.Parallel()
	now := time.Now()
	c := cluster("default", "one")
	m := &managementRead{machines: []clusterv1.Machine{
		workerMachine("one-md-0-a", "w1"), workerMachine("one-md-0-b", "w2"),
		workerMachine("one-md-0-c", "w3"), workerMachine("one-md-0-d", "w4"),
	}}
	wl := &workloadRead{
		nodes: []corev1.Node{nodeWith("w1"), nodeWith("w2"), nodeWith("w3"), notReadyNode("w4")},
		leases: leaseRenewals([]coordinationv1.Lease{
			lease("w1", now.Add(-3*time.Minute)), // behind
			lease("w2", now.Add(-25*time.Second)),
			lease("w3", now.Add(90*time.Second)), // ahead
			lease("w4", now.Add(-time.Hour)),     // not ready: its own finding
		}),
	}
	var a attention
	nodesSection(c, m, wl, now, &a)
	skewed := map[string]string{}
	for _, item := range a {
		if strings.Contains(item.What, "clock is off by") {
			skewed[item.Machine] = item.What + " " + item.Message
		}
	}
	if len(skewed) != 2 || !strings.Contains(skewed["one-md-0-a"], "off by 3m0s") || !strings.Contains(skewed["one-md-0-a"], "behind the controller's") ||
		!strings.Contains(skewed["one-md-0-c"], "off by 1m30s") || !strings.Contains(skewed["one-md-0-c"], "ahead of the controller's") {
		t.Errorf("clock notes %v", skewed)
	}
	// A cluster whose leases cannot be read shows no skew.
	wl.leases = nil
	a = nil
	nodesSection(c, m, wl, now, &a)
	for _, what := range attentionWhat(a) {
		if strings.Contains(what, "clock") {
			t.Errorf("a clock note without leases: %q", what)
		}
	}
}

func lease(name string, renewed time.Time) coordinationv1.Lease {
	t := metav1.NewMicroTime(renewed)
	return coordinationv1.Lease{Name: name, Spec: coordinationv1.LeaseSpec{RenewTime: &t}}
}

func TestRestrictedCheckNamesThePodsItWouldRefuse(t *testing.T) {
	t.Parallel()
	hardened := corev1.Pod{
		Namespace: "apps", Name: "safe",
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: new(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "c", SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			}}},
		},
	}
	root := corev1.Pod{Namespace: "apps", Name: "plain", Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}}
	privileged := corev1.Pod{
		Namespace: "kube-system", Name: "cni",
		Spec: corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{Name: "c", SecurityContext: &corev1.SecurityContext{Privileged: new(true)}}}},
	}
	done := root
	done.Name, done.Status.Phase = "done", corev1.PodSucceeded

	got, err := restrictedCheck([]corev1.Pod{hardened, root, privileged, done}, []string{"kube-system"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Checked != 2 || len(got.Refused) != 1 || got.Refused[0].Namespace != "apps" || got.Refused[0].Name != "plain" || got.Refused[0].Reasons == "" {
		t.Errorf("check = %+v", got)
	}
	if none, err := restrictedCheck([]corev1.Pod{hardened}, nil); err != nil || len(none.Refused) != 0 || none.Checked != 1 {
		t.Errorf("a hardened pod: %+v, %v", none, err)
	}
}
