package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

var rke2ControlPlane = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta1", Kind: "RKE2ControlPlane"}

// fakeWorkloads stands in for the workload clusters: one fake API server whose answers the test
// sets, and a record of which tokens connected.
type fakeWorkloads struct {
	mu     sync.Mutex
	kube   *fake.Clientset
	velero dynamic.Interface
	readyz string
	// alerts is Alertmanager's answer, and alertsBy the token it answers.
	alerts   string
	alertsBy string
	tokens   []string
}

func (f *fakeWorkloads) connect(server string, ca []byte, token string) (*Workload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if server != "https://10.0.0.20:6443" || string(ca) != "the cluster CA" {
		return nil, errors.New("connected to the wrong cluster")
	}
	f.tokens = append(f.tokens, token)
	alerts := func(context.Context) ([]byte, error) {
		if token != f.alertsBy {
			return nil, errors.New("403 Forbidden")
		}
		return []byte(f.alerts), nil
	}
	return &Workload{Kube: f.kube, Dynamic: f.velero, Readyz: func(context.Context) (string, error) { return f.readyz, nil }, Alerts: alerts}, nil
}

func (f *fakeWorkloads) connected() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tokens)
}

func condition(kind string, status metav1.ConditionStatus, message string, since time.Time) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: "Test", Message: message, LastTransitionTime: metav1.NewTime(since)}
}

// healthCluster is cluster one with its control plane up, a ready control-plane machine and a
// worker that is not, the tokens Kubehenge's controller keeps for it, a pool rolling out, a health
// check that stopped, a failed add-on, and events about it and another cluster.
func healthCluster(now time.Time) []client.Object {
	c := cluster("default", "one")
	c.UID = "uid-one"
	c.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{APIGroup: rke2ControlPlane.Group, Kind: rke2ControlPlane.Kind, Name: "one-cp"}
	c.Status.Conditions = []metav1.Condition{condition(clusterv1.ClusterControlPlaneInitializedCondition, metav1.ConditionTrue, "", now.Add(-time.Hour))}
	machine := func(name, node string, controlPlane, ready bool) *clusterv1.Machine {
		m := &clusterv1.Machine{}
		m.Namespace, m.Name, m.CreationTimestamp = "default", name, metav1.NewTime(now.Add(-2*time.Hour))
		m.Labels = map[string]string{clusterv1.ClusterNameLabel: "one", clusterv1.ClusterTopologyMachineDeploymentNameLabel: "md-0"}
		if controlPlane {
			m.Labels = map[string]string{clusterv1.ClusterNameLabel: "one", clusterv1.MachineControlPlaneLabel: ""}
		}
		m.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{Kind: "ProxmoxMachine", Name: name + "-vm"}
		m.Status.NodeRef.Name, m.Status.Phase = node, "Running"
		status := metav1.ConditionTrue
		if !ready {
			status = metav1.ConditionFalse
		}
		m.Status.Conditions = []metav1.Condition{condition(clusterv1.MachineReadyCondition, status, "NodeHealthy: Kubelet stopped posting node status", now.Add(-10*time.Minute))}
		return m
	}
	token := func(name, value string) *corev1.Secret {
		return &corev1.Secret{
			Namespace: "default", Name: name,
			Annotations: map[string]string{provisioner.IdentityExpiresAnnotation: now.Add(time.Hour).Format(time.RFC3339)},
			Data: map[string][]byte{
				provisioner.IdentityServerKey: []byte("https://10.0.0.20:6443"), provisioner.IdentityCAKey: []byte("the cluster CA"),
				provisioner.IdentityTokenKey: []byte(value),
			},
		}
	}
	controlPlane := &unstructured.Unstructured{}
	controlPlane.SetGroupVersionKind(rke2ControlPlane)
	controlPlane.SetNamespace("default")
	controlPlane.SetName("one-cp")
	_ = unstructured.SetNestedSlice(controlPlane.Object, []any{
		map[string]any{"type": "Available", "status": "True"},
		map[string]any{"type": "EtcdClusterHealthy", "status": "False", "message": "etcd member one-cp-a has no quorum", "lastTransitionTime": now.Add(-3 * time.Minute).Format(time.RFC3339)},
	}, "status", "conditions")
	release := func(name, addon, status string, ready bool, message string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{"releaseName": addon, "releaseNamespace": addon, "chartName": addon, "version": "1.2.3@sha256:0123"},
			"status": map[string]any{"status": status, "revision": int64(2), "conditions": []any{map[string]any{
				"type": "Ready", "status": map[bool]string{true: "True", false: "False"}[ready], "message": message,
			}}},
		}}
		u.SetGroupVersionKind(helmReleaseProxyList.GroupVersion().WithKind("HelmReleaseProxy"))
		u.SetNamespace("default")
		u.SetName(name)
		u.SetLabels(map[string]string{clusterv1.ClusterNameLabel: "one"})
		u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "addons.cluster.x-k8s.io/v1alpha1", Kind: "HelmChartProxy", Name: "one-" + addon, UID: "hcp"}})
		return u
	}
	pool := &clusterv1.MachineDeployment{
		Namespace: "default", Name: "one-md-0-x1",
		Labels: map[string]string{clusterv1.ClusterNameLabel: "one", clusterv1.ClusterTopologyMachineDeploymentNameLabel: "md-0"},
	}
	pool.Spec.Replicas = new(int32(2))
	pool.Status.ReadyReplicas, pool.Status.UpToDateReplicas, pool.Status.AvailableReplicas = new(int32(1)), new(int32(1)), new(int32(1))
	pool.Status.Conditions = []metav1.Condition{condition(clusterv1.MachineDeploymentRollingOutCondition, metav1.ConditionTrue, "Rolling out 1 not up-to-date replicas", now)}
	check := &clusterv1.MachineHealthCheck{Namespace: "default", Name: "one-md-0", Labels: map[string]string{clusterv1.ClusterNameLabel: "one"}}
	check.Status.ExpectedMachines, check.Status.CurrentHealthy, check.Status.Targets = new(int32(1)), new(int32(0)), []string{"one-md-0-a"}
	check.Status.Conditions = []metav1.Condition{condition(clusterv1.MachineHealthCheckRemediationAllowedCondition, metav1.ConditionFalse, "Remediation is not allowed: too many unhealthy machines", now)}
	event := func(name, machine, message string) *corev1.Event {
		return &corev1.Event{
			Namespace: "default", Name: name, Type: corev1.EventTypeWarning, Reason: "NodeNotReady", Message: message, Count: 1,
			InvolvedObject: corev1.ObjectReference{Kind: "Machine", Name: machine}, LastTimestamp: metav1.NewTime(now.Add(-5 * time.Minute)),
			Action: "WatchNodes",
		}
	}
	worker := machine("one-md-0-a", "one-md-0-a", false, false)
	worker.Status.Conditions = append(worker.Status.Conditions, condition(clusterv1.MachineHealthCheckSucceededCondition, metav1.ConditionFalse, "Node has been NotReady for 6m", now.Add(-time.Minute)))
	return []client.Object{
		c, machine("one-cp-a", "one-cp-a", true, true), worker,
		token(provisioner.IdentitySecretName("one", provisioner.IdentityHealth), "health-token"),
		token(provisioner.IdentitySecretName("one", provisioner.IdentityLogs), "logs-token"),
		token(provisioner.IdentitySecretName("one", provisioner.IdentityNodes), "nodes-token"),
		controlPlane, release("one-monitoring-r1", "monitoring", "failed", false, "upgrade failed: timed out waiting for the condition"),
		release("one-headlamp-r1", "headlamp", "deployed", true, ""), pool, check,
		event("e1", "one-md-0-a", "Node one-md-0-a is not ready"), event("e2", "two-md-0-a", "Node two-md-0-a is not ready"),
	}
}

// healthWorkload is cluster one inside: a ready control plane, a worker gone not ready under memory
// pressure, a crash-looping pod whose spec carries a secret, a pod nobody can schedule, a healthy
// one, and its warnings.
func healthWorkload(now time.Time) *fake.Clientset {
	since := func(d time.Duration) metav1.Time { return metav1.NewTime(now.Add(-d)) }
	node := func(name string, controlPlane, ready bool) *corev1.Node {
		n := &corev1.Node{Name: name}
		if controlPlane {
			n.Labels = map[string]string{"node-role.kubernetes.io/control-plane": "true"}
			n.Spec.Taints = []corev1.Taint{{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}}
		}
		status := corev1.ConditionTrue
		if !ready {
			status = corev1.ConditionUnknown
		}
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status, Message: "Kubelet stopped posting node status.", LastTransitionTime: since(10 * time.Minute)}}
		n.Status.NodeInfo.KubeletVersion = "v1.36.5+rke2r1"
		n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi")}
		return n
	}
	worker := node("one-md-0-a", false, false)
	worker.Status.Conditions = append(worker.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})
	requests := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")}
	crashing := &corev1.Pod{Namespace: "web", Name: "api-7d9", CreationTimestamp: since(time.Hour)}
	crashing.Spec.NodeName = "one-md-0-a"
	crashing.Spec.Containers = []corev1.Container{
		{Name: "proxy", Resources: corev1.ResourceRequirements{Requests: requests}},
		{Name: "api", Command: []string{"/api", "--token=s3cret"}, Env: []corev1.EnvVar{{Name: "PASSWORD", Value: "hunter2"}}, Resources: corev1.ResourceRequirements{Requests: requests}},
	}
	crashing.Status.Phase = corev1.PodRunning
	crashing.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "proxy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		{
			Name: "api", RestartCount: 14,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: since(time.Minute)}},
		},
	}
	pending := &corev1.Pod{Namespace: "web", Name: "big", CreationTimestamp: since(time.Hour)}
	pending.Spec.Containers = []corev1.Container{{Name: "big"}}
	pending.Status.Phase = corev1.PodPending
	pending.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/2 nodes are available: 2 Insufficient memory.", LastTransitionTime: since(time.Hour)}}
	well := &corev1.Pod{Namespace: "kube-system", Name: "coredns", CreationTimestamp: since(time.Hour)}
	well.Spec.NodeName = "one-cp-a"
	well.Spec.Containers = []corev1.Container{{Name: "coredns", Resources: corev1.ResourceRequirements{Requests: requests}}}
	well.Status.Phase = corev1.PodRunning
	well.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	warning := func(name string, count int32) *corev1.Event {
		return &corev1.Event{
			Namespace: "web", Name: name, Type: corev1.EventTypeWarning, Reason: "BackOff", Message: "Back-off restarting failed container api",
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "web", Name: "api-7d9"}, Count: count, LastTimestamp: since(time.Minute),
		}
	}
	normal := &corev1.Event{Namespace: "web", Name: "pulled", Type: corev1.EventTypeNormal, Reason: "Pulled", LastTimestamp: since(time.Minute)}
	old := warning("old", 1)
	old.Name, old.Message, old.LastTimestamp = "old", "long ago", since(2*time.Hour)
	return fake.NewClientset(node("one-cp-a", true, true), worker, crashing, pending, well, warning("w1", 3), warning("w2", 4), normal, old)
}

func healthHarness(t *testing.T, more ...client.Object) (*harness, *fakeWorkloads) {
	t.Helper()
	h := newHarness(t, append(healthCluster(time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)), more...)...)
	workloads := &fakeWorkloads{kube: healthWorkload(h.clock), readyz: "[+]ping ok\n[-]etcd failed: reason withheld\nreadyz check failed\n"}
	h.server.ConnectWorkload = workloads.connect
	h.accounts.members = []pve.Member{
		{VMID: 201, Node: "pve1", Type: "qemu", Name: "one-cp-a-vm", Status: "running", CPU: 0.1, Cores: 4, Memory: 2 << 30, MaxMem: 8 << 30, Uptime: 7200},
		{VMID: 202, Node: "pve2", Type: "qemu", Name: "one-md-0-a-vm", Status: "stopped", Cores: 2, MaxMem: 4 << 30},
	}
	return h, workloads
}

// readHealthView reads healthHarness's cluster's health page.
func readHealthView(t *testing.T, b *browser) (healthView, string) {
	t.Helper()
	rec := b.do("GET", "/api/clusters/default/one/health", "")
	expect(t, rec, http.StatusOK)
	var v healthView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v, rec.Body.String()
}

func TestHealthSaysWhatIsWrong(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	b := h.browser()
	b.signIn("viewer@pve")
	v, body := readHealthView(t, b)

	if v.Verdict != verdictDegraded || v.Workload != "" || v.Logs {
		t.Errorf("verdict %q, workload %q, logs %v; want degraded, read, no logs for a viewer", v.Verdict, v.Workload, v.Logs)
	}
	// Names, reasons and messages only: nothing of a pod's spec leaves.
	for _, secret := range []string{"hunter2", "s3cret", "PASSWORD"} {
		if strings.Contains(body, secret) {
			t.Errorf("the answer carries %q from a pod's spec", secret)
		}
	}
	whats := make([]string, 0, len(v.Attention))
	for _, a := range v.Attention {
		whats = append(whats, a.Severity+": "+a.What)
		if strings.HasPrefix(a.What, "Worker ") && a.Machine != "one-md-0-a" {
			t.Errorf("%q names machine %q, want one-md-0-a, whose row acts on it", a.What, a.Machine)
		}
	}
	for _, want := range []string{
		"error: Pod web/api-7d9: CrashLoopBackOff, 14 restarts",
		"error: Worker one-md-0-a not ready",
		"error: Worker one-md-0-a failed health check",
		"error: Add-on monitoring failed",
		"error: Health check md-0 stopped replacing machines",
		"error: API server check etcd failing",
		"warning: Pod web/big: Unschedulable",
		"warning: Worker one-md-0-a: memory pressure",
		"warning: Control plane: EtcdClusterHealthy is False",
		"progress: Pool md-0 rolling out: 1 of 2 up to date",
	} {
		if !slices.Contains(whats, want) {
			t.Errorf("needs attention lacks %q: %q", want, whats)
		}
	}
	if v.Attention[0].Severity != severityError || v.Attention[len(v.Attention)-1].Severity != severityProgress {
		t.Errorf("not worst first: %q", whats)
	}
	if slices.ContainsFunc(whats, func(w string) bool { return strings.Contains(w, "headlamp") || strings.Contains(w, "coredns") }) {
		t.Errorf("what is well needs attention: %q", whats)
	}
	checkHealthNodes(t, h, &v)
	checkHealthSections(t, &v)
}

// checkHealthNodes checks the nodes of healthHarness's cluster as the page shows them.
func checkHealthNodes(t *testing.T, h *harness, v *healthView) {
	t.Helper()
	if len(v.Nodes.Items) != 2 {
		t.Fatalf("nodes = %+v", v.Nodes.Items)
	}
	cp, worker := v.Nodes.Items[0], v.Nodes.Items[1]
	if cp.Name != "one-cp-a" || cp.Role != controlPlaneRole || !cp.Ready || cp.Down || cp.Silent || len(cp.Taints) != 0 || cp.Pods != 1 || *cp.CPU != (resourceUse{500, 4000}) {
		t.Errorf("control plane = %+v", cp)
	}
	if cp.VM == nil || cp.VM.ID != 201 || cp.VM.Node != "pve1" || cp.VM.MaxMem != 8<<30 || len(cp.VM.Load) != 2 || cp.VM.Load[0] != nil || *cp.VM.Load[1] != 0.25 {
		t.Errorf("control plane's VM = %+v", cp.VM)
	}
	// Not ready for ten minutes, its kubelet silent: down, not on its way.
	if worker.Ready || !worker.Down || !worker.Silent || worker.Pool != "md-0" || !slices.Equal(worker.Pressure, []string{"MemoryPressure"}) || worker.Memory.Requested != 2<<30 {
		t.Errorf("worker = %+v", worker)
	}
	// A stopped VM has no load to read.
	if worker.VM == nil || worker.VM.Status != "stopped" || worker.VM.Load != nil || !slices.Equal(h.accounts.loadReads, []int{201}) {
		t.Errorf("worker's VM = %+v, loads read %v", worker.VM, h.accounts.loadReads)
	}
}

// checkHealthSections checks the rest of healthHarness's cluster as the page shows it.
func checkHealthSections(t *testing.T, v *healthView) {
	t.Helper()
	if len(v.Pods.Items) != 2 || v.Pods.Items[0].Name != "api-7d9" || !slices.Equal(v.Pods.Items[0].Containers, []string{"api", "proxy"}) {
		t.Fatalf("pods = %+v", v.Pods.Items)
	}
	if p := v.Pods.Items[0]; p.Restarts != 14 || !strings.HasPrefix(p.Message, "It last stopped with exit code 1 (Error).") {
		t.Errorf("crash-looping pod = %+v", p)
	}
	// A pod no node takes has never run, so it has no log to offer.
	if p := v.Pods.Items[1]; p.Name != "big" || len(p.Containers) != 0 {
		t.Errorf("unschedulable pod = %+v, want no containers with logs", p)
	}
	if len(v.Addons.Items) != 2 || v.Addons.Items[1].Name != "monitoring" || v.Addons.Items[1].Version != "1.2.3" || v.Addons.Items[1].Ready {
		t.Errorf("add-ons = %+v", v.Addons.Items)
	}
	if len(v.HealthChecks.Items) != 1 || v.HealthChecks.Items[0].Allowed || !slices.Equal(v.HealthChecks.Items[0].Remediating, []string{"one-md-0-a"}) {
		t.Errorf("health checks = %+v", v.HealthChecks.Items)
	}
	if v.ControlPlane.Kind != "RKE2ControlPlane" || len(v.ControlPlane.Conditions) != 2 {
		t.Errorf("control plane = %+v", v.ControlPlane)
	}
	// Repeats counted once. The other cluster's, the normal and the old events left out.
	events := map[string]int32{}
	for i := range v.Events.Items {
		e := &v.Events.Items[i]
		events[e.Source+" "+e.Object] += e.Count
		// A run's page picks its own events by what the controller was doing.
		if e.Source == sourceManagement && e.Action != "WatchNodes" {
			t.Errorf("event %+v lost its action", e)
		}
	}
	if len(v.Events.Items) != 2 || events["workload Pod web/api-7d9"] != 7 || events["management Machine one-md-0-a"] != 1 {
		t.Errorf("events = %+v", v.Events.Items)
	}
}

// A monitored cluster's firing alerts show, critical and warning ones alone, read as the alerts
// identity.
func TestHealthShowsTheAlertsThatFire(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	identity := &corev1.Secret{
		Namespace: "default", Name: provisioner.IdentitySecretName("one", provisioner.IdentityAlerts),
		Annotations: map[string]string{provisioner.IdentityExpiresAnnotation: now.Add(time.Hour).Format(time.RFC3339)},
		Data: map[string][]byte{
			provisioner.IdentityServerKey: []byte("https://10.0.0.20:6443"), provisioner.IdentityCAKey: []byte("the cluster CA"),
			provisioner.IdentityTokenKey: []byte("alerts-token"),
		},
	}
	h, workloads := healthHarness(t, identity)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: "monitoring", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)},
	})
	if err := h.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	workloads.alertsBy = "alerts-token"
	workloads.alerts = `[
		{"labels":{"alertname":"Watchdog","severity":"none"},"annotations":{},"startsAt":"` + at(time.Hour) + `"},
		{"labels":{"alertname":"KubePodCrashLooping","severity":"warning","namespace":"web"},"annotations":{"description":"Pod web/api is crash looping."},"startsAt":"` + at(20*time.Minute) + `"},
		{"labels":{"alertname":"TargetDown","severity":"warning","namespace":"kube-system"},"annotations":{"summary":"One or more targets are unreachable."},"startsAt":"` + at(30*time.Minute) + `"},
		{"labels":{"alertname":"KubeNodeNotReady","severity":"critical"},"annotations":{"summary":"Node is not ready."},"startsAt":"` + at(10*time.Minute) + `"}
	]`
	b := h.browser()
	b.signIn("viewer@pve")
	v, _ := readHealthView(t, b)
	if v.Alerts == nil || v.Alerts.Error != "" || len(v.Alerts.Items) != 3 {
		t.Fatalf("alerts = %+v", v.Alerts)
	}
	names := []string{v.Alerts.Items[0].Name, v.Alerts.Items[1].Name, v.Alerts.Items[2].Name}
	if !slices.Equal(names, []string{"KubeNodeNotReady", "TargetDown", "KubePodCrashLooping"}) || v.Alerts.Items[2].Summary != "Pod web/api is crash looping." {
		t.Errorf("alerts = %+v, want the worst and oldest first", v.Alerts.Items)
	}
	i := slices.IndexFunc(v.Attention, func(a attentionView) bool { return a.What == "Alert KubePodCrashLooping in web" })
	if i < 0 || v.Attention[i].Section != sectionAlerts || v.Attention[i].Severity != severityWarning {
		t.Errorf("needs attention %+v", v.Attention)
	}
	if !slices.Contains(workloads.connected(), "alerts-token") {
		t.Errorf("read with %v, not the alerts identity", workloads.connected())
	}
}

// A monitored cluster whose alerts cannot be read says why, and has nothing to show without it.
func TestHealthSaysWhyNoAlertsShow(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: "monitoring", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)},
	})
	if err := h.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("viewer@pve")
	v, _ := readHealthView(t, b)
	if v.Alerts == nil || v.Alerts.Error != notSetUp || len(v.Alerts.Items) != 0 {
		t.Errorf("alerts = %+v, want why none show", v.Alerts)
	}
}

// A cluster that backs up needs attention once its last backup failed, here and in the fleet's rail,
// read as the backups identity.
func TestHealthSaysWhenBackupsFail(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	identity := &corev1.Secret{
		Namespace: "default", Name: provisioner.IdentitySecretName("one", provisioner.IdentityBackups),
		Annotations: map[string]string{provisioner.IdentityExpiresAnnotation: now.Add(time.Hour).Format(time.RFC3339)},
		Data: map[string][]byte{
			provisioner.IdentityServerKey: []byte("https://10.0.0.20:6443"), provisioner.IdentityCAKey: []byte("the cluster CA"),
			provisioner.IdentityTokenKey: []byte("backups-token"),
		},
	}
	h, workloads := healthHarness(t, identity)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: "backups", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true,"target":"store"}`)},
	})
	if err := h.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	workloads.velero = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), veleroKinds,
		velero("Schedule", provisioner.VeleroSchedule, nil, map[string]any{
			"metadata": map[string]any{"creationTimestamp": "2026-10-01T00:00:00Z"},
			"spec":     map[string]any{"schedule": "CRON_TZ=UTC 0 2 * * *"},
		}),
		backupAt("velero-scheduled-20261013020000", "default", "Failed", "2026-10-13T02:00:00Z"),
		backupAt("velero-scheduled-20261012020000", "default", "Completed", "2026-10-12T02:00:00Z"),
	)
	b := h.browser()
	b.signIn("viewer@pve")
	v, _ := readHealthView(t, b)
	i := slices.IndexFunc(v.Attention, func(a attentionView) bool { return a.What == "Last backup failed" })
	if i < 0 || v.Attention[i].Section != sectionBackups || v.Attention[i].Severity != severityError {
		t.Errorf("needs attention %+v", v.Attention)
	}
	if !slices.Contains(workloads.connected(), "backups-token") {
		t.Errorf("read with %v, not the backups identity", workloads.connected())
	}
}

func TestHealthReadOnceForEveryViewer(t *testing.T) {
	t.Parallel()
	h, workloads := healthHarness(t)
	lists := 0
	var mu sync.Mutex
	workloads.kube.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		lists++
		return false, nil, nil
	})
	for _, user := range []string{"viewer@pve", "dev@pve", "admin@pve"} {
		b := h.browser()
		b.signIn(user)
		readHealthView(t, b)
	}
	if lists != 1 || len(workloads.connected()) != 1 {
		t.Errorf("%d reads with %d connections for three viewers at once, want one", lists, len(workloads.connected()))
	}
	h.clock = h.clock.Add(healthFresh + time.Second)
	b := h.browser()
	b.signIn("viewer@pve")
	readHealthView(t, b)
	// Read again, on the connection kept for the same token.
	if lists != 2 || len(workloads.connected()) != 1 || workloads.connected()[0] != "health-token" {
		t.Errorf("after %v: %d reads, connections %v", healthFresh, lists, workloads.connected())
	}
}

func TestFleetHealthSumsUpTheClustersTheUserSees(t *testing.T) {
	t.Parallel()
	two := cluster("default", "two")
	two.UID = "uid-two"
	h, workloads := healthHarness(t, two)
	lists := 0
	var mu sync.Mutex
	workloads.kube.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		lists++
		return false, nil, nil
	})
	fleet := func(user string) []fleetHealthView {
		t.Helper()
		b := h.browser()
		b.signIn(user)
		rec := b.do("GET", "/api/fleet/health", "")
		expect(t, rec, http.StatusOK)
		var v []fleetHealthView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	viewer := fleet("viewer@pve")
	if len(viewer) != 1 || viewer[0].Name != "one" {
		t.Fatalf("viewer's fleet = %+v, want cluster one alone", viewer)
	}
	one := viewer[0]
	if one.Verdict != verdictDegraded || len(one.Attention) != fleetHealthItems || one.More == 0 {
		t.Errorf("one = %+v, want degraded with its %d worst items and a count of the rest", one, fleetHealthItems)
	}
	if one.Attention[0].Severity != severityError {
		t.Errorf("first item %+v, want the worst", one.Attention[0])
	}
	if names := []string{fleet("admin@pve")[0].Name, fleet("admin@pve")[1].Name}; !slices.Equal(names, []string{"one", "two"}) {
		t.Errorf("admin's fleet = %v", names)
	}

	// The fleet takes a read up to fleetHealthFresh old, where the health page reads again.
	h.clock = h.clock.Add(healthFresh + time.Second)
	fleet("viewer@pve")
	if lists != 1 {
		t.Errorf("%d reads of cluster one after %v, want the fleet to keep the first", lists, healthFresh)
	}
	b := h.browser()
	b.signIn("viewer@pve")
	readHealthView(t, b)
	if lists != 2 {
		t.Errorf("%d reads of cluster one, want the health page to read it again", lists)
	}
}

func TestHealthOfAnUnreachableCluster(t *testing.T) {
	t.Parallel()
	h, workloads := healthHarness(t)
	workloads.kube.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp 10.0.0.20:6443: i/o timeout")
	})
	b := h.browser()
	b.signIn("dev@pve")
	v, _ := readHealthView(t, b)
	if v.Verdict != verdictUnreachable || v.Workload != "The cluster's API server did not answer: dial tcp 10.0.0.20:6443: i/o timeout" || v.Logs {
		t.Errorf("verdict %q, workload %q, logs %v", v.Verdict, v.Workload, v.Logs)
	}
	// What the management cluster knows still shows.
	if len(v.Addons.Items) != 2 || len(v.Nodes.Items) != 2 || v.Nodes.Items[0].Joined || v.Nodes.Items[0].VM == nil {
		t.Errorf("add-ons %+v, nodes %+v", v.Addons.Items, v.Nodes.Items)
	}
}

func TestHealthWithoutAWayIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(h *harness)
		want   string
	}{
		{"its token ran out", func(h *harness) { h.clock = h.clock.Add(2 * time.Hour) }, "Access to the cluster expired at 2026-10-13 10:00:00 UTC"},
		{"no token yet", func(h *harness) {
			if err := h.kube.Delete(context.Background(), &corev1.Secret{Namespace: "default", Name: provisioner.IdentitySecretName("one", provisioner.IdentityHealth)}); err != nil {
				t.Fatal(err)
			}
		}, notSetUp},
		{"outside the interface's namespace", func(h *harness) { h.server.Namespace = "clusters" }, "Nodes and pods show only for clusters in namespace clusters"},
		{"outside the interface's namespaces", func(h *harness) {
			if err := h.kube.Create(context.Background(), managedConfigMap(`["clusters","team-a"]`)); err != nil {
				t.Fatal(err)
			}
		}, "Nodes and pods show only for clusters in namespaces clusters, team-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, workloads := healthHarness(t)
			tc.change(h)
			b := h.browser()
			b.signIn("dev@pve")
			v, _ := readHealthView(t, b)
			if !strings.HasPrefix(v.Workload, tc.want) || v.Verdict == verdictUnreachable || v.Logs || len(workloads.connected()) != 0 {
				t.Errorf("workload %q, verdict %q, logs %v, connected %v; want %q", v.Workload, v.Verdict, v.Logs, workloads.connected(), tc.want)
			}
			if len(v.Addons.Items) != 2 {
				t.Errorf("add-ons = %+v", v.Addons.Items)
			}
		})
	}
}

func TestHealthNeedsToSeeTheCluster(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	h.accounts.passwords["nobody@pve"] = "secret"
	b := h.browser()
	b.signIn("nobody@pve")
	expect(t, b.do("GET", "/api/clusters/default/one/health", ""), http.StatusNotFound)
	expect(t, b.do("GET", "/api/clusters/default/one/pods/web/api-7d9/logs?container=api", ""), http.StatusNotFound)
}

func TestPodLogsForThoseWhoMayTakeTheKubeconfig(t *testing.T) {
	t.Parallel()
	h, workloads := healthHarness(t)
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("GET", "/api/clusters/default/one/pods/web/api-7d9/logs?container=api", ""), http.StatusForbidden)

	dev := h.browser()
	dev.signIn("dev@pve")
	v, _ := readHealthView(t, dev)
	if !v.Logs {
		t.Error("the page offers no logs to a user with Sys.Modify on the pool")
	}
	rec := dev.do("GET", "/api/clusters/default/one/pods/web/api-7d9/logs?container=api", "")
	expect(t, rec, http.StatusOK)
	var logs podLogView
	if err := json.Unmarshal(rec.Body.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	// A crash-looping container has only its last run's lines.
	if logs.Container != "api" || !logs.Previous || !slices.Equal(logs.Lines, []string{"fake logs"}) {
		t.Errorf("logs = %+v", logs)
	}
	var read *k8stesting.GenericActionImpl
	for _, a := range workloads.kube.Actions() {
		if g, ok := a.(k8stesting.GenericActionImpl); ok && a.GetSubresource() == "log" {
			read = &g
		}
	}
	if read == nil {
		t.Fatalf("no log read: %v", workloads.kube.Actions())
	}
	opts, ok := read.Value.(*corev1.PodLogOptions)
	// The kubelet's newest run is the last one to stop. Previous would name the one before it.
	if !ok || opts.TailLines == nil || *opts.TailLines != podLogLines || opts.Previous || opts.Container != "api" {
		t.Errorf("log read with %+v", read.Value)
	}
	if got := workloads.connected(); !slices.Contains(got, "logs-token") {
		t.Errorf("logs read with %v, want the logs identity's token", got)
	}
	expect(t, dev.do("GET", "/api/clusters/default/one/pods/web/api-7d9/logs?container=sidecar", ""), http.StatusNotFound)
	expect(t, dev.do("GET", "/api/clusters/default/one/pods/web/gone/logs?container=api", ""), http.StatusNotFound)
}

func TestReadLinesKeepsTheLast(t *testing.T) {
	t.Parallel()
	var log strings.Builder
	for i := range podLogLines + 5 {
		log.WriteString(strings.Repeat("x", i) + "\n")
	}
	lines, err := readLines(strings.NewReader(log.String()))
	if err != nil || len(lines) != podLogLines || lines[0] != strings.Repeat("x", 5) {
		t.Fatalf("readLines = %d lines from %q, %v", len(lines), lines[0], err)
	}
}
