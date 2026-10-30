package provisioner

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/yaml"
)

func TestKubernetesMinors(t *testing.T) {
	t.Parallel()
	m, err := ParseKubernetesMinors("34-36")
	if err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]bool{
		"v1.34.0": true, "v1.36.5+rke2r1": true, "v1.36.5+k3s1": true,
		"v1.33.9": false, "v1.37.1+rke2r1": false, "v2.36.0": false, "nonsense": false,
	} {
		if got := m.Allows(v); got != want {
			t.Errorf("%s: %v", v, got)
		}
	}
	for _, bad := range []string{"", "36", "36-34", "a-b"} {
		if _, err := ParseKubernetesMinors(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestRancherName(t *testing.T) {
	t.Parallel()
	c := cluster()
	if got := rancherName(c); !regexp.MustCompile(`^team-a-web-[0-9a-f]{8}$`).MatchString(got) {
		t.Errorf("got %q", got)
	}
	a, b := cluster(), cluster()
	a.Namespace, a.Name, b.Namespace, b.Name = "a-b", "c", "a", "b-c"
	if rancherName(a) == rancherName(b) {
		t.Errorf("a-b/c and a/b-c share %q", rancherName(a))
	}
	c.Name = strings.Repeat("a", 60)
	long := rancherName(c)
	if len(long) > 63 || !strings.HasPrefix(long, "team-a-") {
		t.Errorf("got %q", long)
	}
	c.Name += "b"
	if rancherName(c) == long {
		t.Error("two long names share one name in Rancher")
	}
}

// clusterWithRancher is a cluster with Rancher switched on or off, on control planes as large as
// Rancher needs.
func clusterWithRancher(name string, install bool) *clusterv1.Cluster {
	c := networkCluster("192.168.1.1", 16)
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "rancher", Value: apiextensionsv1.JSON{
		Raw: []byte(`{"install":` + map[bool]string{true: "true", false: "false"}[install] + `,"hostname":"rancher.example.org"}`),
	}}, clusterv1.ClusterVariable{Name: "controlPlaneMachine", Value: apiextensionsv1.JSON{Raw: []byte(`{"cores":4,"memoryMiB":8192}`)}})
	c.Spec.Topology.Workers.MachineDeployments = nil
	c.Name = name
	return c
}

func TestRancherNeedsFourVCPUsOnEveryNode(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	h.RancherKubernetes = KubernetesMinors{Min: 34, Max: 37}
	setCores := func(c *clusterv1.Cluster, name, value string) {
		c.Spec.Topology.Variables = slices.DeleteFunc(c.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool { return v.Name == name })
		if value != "" {
			c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(value)}})
		}
	}
	workers := func(c *clusterv1.Cluster, replicas int32, override string) {
		md := clusterv1.MachineDeploymentTopology{Class: "worker", Name: "md-0", Replicas: &replicas}
		if override != "" {
			md.Variables.Overrides = []clusterv1.ClusterVariable{{Name: "workerMachine", Value: apiextensionsv1.JSON{Raw: []byte(override)}}}
		}
		c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{md}
	}
	for _, tc := range []struct {
		name   string
		change func(c *clusterv1.Cluster)
		want   string
	}{
		{"four on the control planes", func(*clusterv1.Cluster) {}, ""},
		{"the class's default two", func(c *clusterv1.Cluster) { setCores(c, "controlPlaneMachine", "") }, "control planes have 2"},
		{"cores left to the default", func(c *clusterv1.Cluster) { setCores(c, "controlPlaneMachine", `{"memoryMiB":8192}`) }, "control planes have 2"},
		{"workers on the default", func(c *clusterv1.Cluster) { workers(c, 2, "") }, "pool md-0 has 2"},
		{"no workers running", func(c *clusterv1.Cluster) { workers(c, 0, "") }, ""},
		{"workers made large", func(c *clusterv1.Cluster) { workers(c, 2, `{"cores":8}`) }, ""},
		{"control plane override", func(c *clusterv1.Cluster) {
			c.Spec.Topology.ControlPlane.Variables.Overrides = []clusterv1.ClusterVariable{{Name: "controlPlaneMachine", Value: apiextensionsv1.JSON{Raw: []byte(`{"cores":3}`)}}}
		}, "control planes have 3"},
	} {
		c := clusterWithRancher("rancher", true)
		c.Spec.Topology.Version = "v1.36.5+rke2r1"
		tc.change(c)
		created := callBeforeClusterCreate(t, h, c)
		switch {
		case tc.want == "" && created.Status == runtimehooksv1.ResponseStatusFailure:
			t.Errorf("%s: refused: %s", tc.name, created.Message)
		case tc.want != "" && (created.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(created.Message, tc.want) || !strings.Contains(created.Message, "at least 4 vCPUs")):
			t.Errorf("%s: %s %q, want a refusal naming %q", tc.name, created.Status, created.Message, tc.want)
		}
	}
	plain := networkCluster("192.168.1.1", 16)
	if created := callBeforeClusterCreate(t, h, plain); created.Status == runtimehooksv1.ResponseStatusFailure {
		t.Errorf("a cluster without Rancher on two vCPUs: %s", created.Message)
	}
}

func TestClusterValidatorHoldsChangesToRancher(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	other := clusterWithRancher("other", false)
	h, _ := hooksFor(t, other)
	h.RancherKubernetes = KubernetesMinors{Min: 34, Max: 37}
	v := &ClusterValidator{Hooks: h}
	small := func(c *clusterv1.Cluster) *clusterv1.Cluster {
		c = c.DeepCopy()
		for i, cv := range c.Spec.Topology.Variables {
			if cv.Name == "controlPlaneMachine" {
				c.Spec.Topology.Variables[i].Value.Raw = []byte(`{"cores":2,"memoryMiB":8192}`)
			}
		}
		return c
	}

	off := clusterWithRancher("web", false)
	on := clusterWithRancher("web", true)
	if _, err := v.ValidateUpdate(ctx, small(off), small(on)); err == nil || !strings.Contains(err.Error(), "at least 4 vCPUs") {
		t.Errorf("Rancher switched on over two vCPUs: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, off, on); err != nil {
		t.Errorf("Rancher switched on over four vCPUs: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, on, small(on)); err == nil {
		t.Error("a Rancher cluster's control planes cut to two vCPUs")
	}
	// Already short before the change: an unrelated edit goes through.
	edited := small(on)
	edited.Labels = map[string]string{"team": "a"}
	if _, err := v.ValidateUpdate(ctx, small(on), edited); err != nil {
		t.Errorf("an unrelated edit to a cluster already short: %v", err)
	}
	if _, err := v.ValidateCreate(ctx, small(on)); err == nil {
		t.Error("a new Rancher cluster on two vCPUs")
	}
	// With a worker, so that its control planes run no workloads.
	plain := small(off)
	plain.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
	if _, err := v.ValidateCreate(ctx, plain); err != nil {
		t.Errorf("a new cluster without Rancher on two vCPUs: %v", err)
	}

	// A second Rancher, switched on after creation.
	first := clusterWithRancher("rancher", true)
	h2, _ := hooksFor(t, first)
	h2.RancherKubernetes = KubernetesMinors{Min: 34, Max: 37}
	v2 := &ClusterValidator{Hooks: h2}
	second, secondOff := clusterWithRancher("rancher-2", true), clusterWithRancher("rancher-2", false)
	second.Spec.Topology.Version, secondOff.Spec.Topology.Version = "v1.36.5+rke2r1", "v1.36.5+rke2r1"
	if _, err := v2.ValidateUpdate(ctx, secondOff, second); err == nil || !strings.Contains(err.Error(), "already runs Rancher") {
		t.Errorf("a second Rancher switched on: %v", err)
	}
	foreign := small(on)
	foreign.Spec.Topology.ClassRef.Name = "someone-elses"
	if _, err := v.ValidateCreate(ctx, foreign); err != nil {
		t.Errorf("a cluster of another class: %v", err)
	}
}

func TestRancherProxies(t *testing.T) {
	t.Parallel()
	proxies, err := helmChartProxies(cluster(), &testCharts, "kubeadm", "local-lvm", CNI{}, &Addons{Rancher: Rancher{Install: true, Hostname: "rancher.example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 5 || proxies[2].GetName() != "web-cert-manager" || proxies[3].GetName() != "web-rancher" {
		t.Fatalf("proxies %v", proxies)
	}
	namespace, _, _ := unstructured.NestedString(proxies[3].Object, "spec", "namespace")
	valuesTemplate, _, _ := unstructured.NestedString(proxies[3].Object, "spec", "valuesTemplate")
	values := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesTemplate), &values); err != nil {
		t.Fatal(err)
	}
	if namespace != "cattle-system" || values["hostname"] != "rancher.example.org" || values["agentTLSMode"] != "strict" {
		t.Errorf("Rancher proxy in %q with %v", namespace, values)
	}
	want := []any{rancherAgentEnv, map[string]any{"name": "CATTLE_SERVER_URL", "value": "https://rancher.example.org"}}
	if env := values["extraEnv"]; !reflect.DeepEqual(env, want) {
		t.Errorf("Rancher's environment is %v, want the chart's and the server URL fixed to its hostname", env)
	}
	if own, ok := testCharts.Rancher.Values["extraEnv"].([]any); !ok || len(own) != 1 {
		t.Error("the chart's own environment was changed")
	}
	if probe, ok := values["startupProbe"].(map[string]any); !ok || probe["failureThreshold"] != float64(60) || probe["periodSeconds"] != float64(10) {
		t.Errorf("Rancher's first start has %v, want ten minutes", values["startupProbe"])
	}
	if _, ok := values["bootstrapPassword"]; ok {
		t.Error("a password in plain-text values")
	}
	if _, err := helmChartProxies(cluster(), &testCharts, "kubeadm", "local-lvm", CNI{}, &Addons{Rancher: Rancher{Install: true}}); err == nil {
		t.Error("Rancher without a hostname")
	}
}

func TestRancherStaysWithinItsKubernetes(t *testing.T) {
	t.Parallel()
	r := clusterWithRancher("rancher", true)
	h, _ := hooksFor(t, r)
	h.RancherKubernetes = KubernetesMinors{Min: 34, Max: 36}
	if created := callBeforeClusterCreate(t, h, r); created.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(created.Message, "1.34 to 1.36") {
		t.Errorf("Rancher at v1.37: %s %q", created.Status, created.Message)
	}
	r.Spec.Topology.Version = "v1.36.5+rke2r1"
	if created := callBeforeClusterCreate(t, h, r); created.Status != runtimehooksv1.ResponseStatusSuccess {
		t.Errorf("Rancher at v1.36: %s %q", created.Status, created.Message)
	}
	r.Spec.Topology.Version = "v1.37.1+rke2r1"
	storeCluster(t, h.Reader, r)
	resp := &runtimehooksv1.BeforeClusterUpgradeResponse{}
	h.BeforeClusterUpgrade(context.Background(), &runtimehooksv1.BeforeClusterUpgradeRequest{Cluster: *r, ToKubernetesVersion: "v1.37.1+rke2r1"}, resp)
	if resp.RetryAfterSeconds == 0 || !strings.Contains(resp.Message, "1.34 to 1.36") {
		t.Errorf("upgrade of Rancher's cluster past its Kubernetes: %+v", resp)
	}
	resp = &runtimehooksv1.BeforeClusterUpgradeResponse{}
	plain := cluster()
	storeCluster(t, h.Reader, plain)
	h.BeforeClusterUpgrade(context.Background(), &runtimehooksv1.BeforeClusterUpgradeRequest{Cluster: *plain, ToKubernetesVersion: "v1.37.1+rke2r1"}, resp)
	if resp.RetryAfterSeconds != 0 {
		t.Errorf("a cluster without Rancher held back: %+v", resp)
	}
}

func TestOneRancherOnly(t *testing.T) {
	t.Parallel()
	first := clusterWithRancher("rancher", true)
	second := clusterWithRancher("rancher-2", true)
	second.Spec.Topology.Version = "v1.36.5+rke2r1"
	h, _ := hooksFor(t, first)
	h.RancherKubernetes = KubernetesMinors{Min: 34, Max: 36}
	if err := h.checkRancher(context.Background(), second, second.Spec.Topology.Version); err == nil || !strings.Contains(err.Error(), "already runs Rancher") {
		t.Errorf("a second Rancher: %v", err)
	}
}

// A client that skips the hooks' and the interface's checks gets no second Rancher, and the first
// stays the one every cluster registers with.
func TestSecondRancherIsNeverInstalled(t *testing.T) {
	t.Parallel()
	withRancher := func(name string, created time.Time) *clusterv1.Cluster {
		c := storageCluster()
		c.Name, c.CreationTimestamp = name, metav1.NewTime(created)
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "rancher", Value: apiextensionsv1.JSON{
			Raw: []byte(`{"install":true,"hostname":"rancher.example.org"}`),
		}})
		return c
	}
	now := time.Now().Truncate(time.Second)
	first, second := withRancher("zeta", now.Add(-time.Hour)), withRancher("alpha", now)
	if got := RancherCluster([]clusterv1.Cluster{*second, *first}, "kubehenge-system"); got.Name != "zeta" {
		t.Fatalf("Rancher's cluster is %s, not the one created first", got.Name)
	}
	r, kube := newReconciler(t, &fakeCloud{}, first, second)
	r.Remotes = &Remotes{byCluster: map[string]remote{}}
	for _, c := range []*clusterv1.Cluster{first, second} {
		r.Remotes.byCluster[remoteKey(c)] = remote{client: fake.NewClientBuilder().WithScheme(remoteScheme).Build(), until: time.Now().Add(time.Hour)}
	}
	for _, c := range []*clusterv1.Cluster{first, second} {
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
	}
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "zeta-rancher"}, hcp); err != nil {
		t.Errorf("the first cluster's Rancher: %v", err)
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "alpha-rancher"}, hcp); err == nil {
		t.Error("a second Rancher was installed")
	}
}

// Rancher set on a cluster whose Kubernetes it does not run on is not installed; one installed
// before stays.
func TestRancherOutsideItsKubernetesIsNotInstalled(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Version = "v1.37.1+rke2r1"
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "rancher", Value: apiextensionsv1.JSON{
		Raw: []byte(`{"install":true,"hostname":"rancher.example.org"}`),
	}})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	r.RancherKubernetes = KubernetesMinors{Min: 34, Max: 36}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-rancher"}, hcp); err == nil {
		t.Fatal("Rancher installed on a Kubernetes it does not run on")
	}

	installed := &unstructured.Unstructured{}
	installed.SetGroupVersionKind(helmChartProxyGVK)
	installed.SetNamespace("team-a")
	installed.SetName("web-rancher")
	if err := kube.Create(context.Background(), installed); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-rancher"}, hcp); err != nil {
		t.Errorf("an installed Rancher was touched: %v", err)
	}
}

// The registrar waits for no timer: a cluster becoming available, or Rancher's cluster changing,
// brings the clusters concerned back.
func TestRegistrarFollowsRancherAndAvailability(t *testing.T) {
	t.Parallel()
	rancher := clusterWithRancher("rancher", true)
	one, two := clusterWithRancher("one", false), clusterWithRancher("two", false)
	foreign := clusterWithRancher("foreign", false)
	foreign.Spec.Topology.ClassRef.Name = "someone-elses"
	_, kube := newReconciler(t, &fakeCloud{}, rancher, one, two, foreign)
	reg := &Registrar{Client: kube, ClassNamespace: "kubehenge-system"}
	reqs := reg.othersOfRancher(context.Background(), rancher)
	names := make([]string, 0, len(reqs))
	for _, req := range reqs {
		names = append(names, req.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"one", "two"}) {
		t.Errorf("a change to Rancher's cluster brings back %v", names)
	}
	if reqs := reg.othersOfRancher(context.Background(), one); reqs != nil {
		t.Errorf("a change to another cluster brings back %v", reqs)
	}

	available := one.DeepCopy()
	meta.SetStatusCondition(&available.Status.Conditions, metav1.Condition{Type: clusterv1.AvailableCondition, Status: metav1.ConditionTrue, Reason: "Available"})
	if !availabilityChanged.Update(event.UpdateEvent{ObjectOld: one, ObjectNew: available}) {
		t.Error("becoming available passed unseen")
	}
	replicas := available.DeepCopy()
	replicas.Status.Conditions[0].Message = "3 of 3 ready"
	if availabilityChanged.Update(event.UpdateEvent{ObjectOld: available, ObjectNew: replicas}) {
		t.Error("a status change that leaves availability as it was passed")
	}
}

// rancherAPI is a fake of the Kubernetes API of Rancher's cluster.
func rancherAPI(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(provisioningClusterGVK, &unstructured.Unstructured{})
	list := provisioningClusterGVK
	list.Kind += "List"
	scheme.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	metav1.AddToGroupVersion(scheme, provisioningClusterGVK.GroupVersion())
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func registered(name, owner string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(provisioningClusterGVK)
	u.SetNamespace(rancherWorkspace)
	u.SetName(name)
	if owner != "" {
		u.SetAnnotations(map[string]string{rancherClusterAnnotation: owner})
	}
	return u
}

func TestDeregister(t *testing.T) {
	t.Parallel()
	other := cluster()
	other.Name = "other"
	api := rancherAPI(t, registered(rancherName(cluster()), "team-a/web"), registered(rancherName(other), "someone/else"))
	rs := &rancherSide{client: api}
	ctx := context.Background()

	if done, err := rs.deregister(ctx, cluster()); err != nil || done {
		t.Fatalf("first pass: done %v, %v", done, err)
	}
	// The fake deletes at once; Rancher keeps the object until its clean-up in the cluster ran.
	if done, err := rs.deregister(ctx, cluster()); err != nil || !done {
		t.Fatalf("second pass: done %v, %v", done, err)
	}

	if done, err := rs.deregister(ctx, other); err != nil || !done {
		t.Errorf("another's cluster in Rancher: done %v, %v", done, err)
	}
	u := registered(rancherName(other), "")
	if err := api.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
		t.Error("deleted a cluster in Rancher that stands for another")
	}
}

func TestDeregisterAllLeavesRanchersOwn(t *testing.T) {
	t.Parallel()
	api := rancherAPI(t, registered("team-a-web", "team-a/web"), registered("team-b-db", "team-b/db"), registered("by-hand", ""))
	rs := &rancherSide{client: api}
	ctx := context.Background()
	if done, err := rs.deregisterAll(ctx); err != nil || done {
		t.Fatalf("first pass: done %v, %v", done, err)
	}
	if done, err := rs.deregisterAll(ctx); err != nil || !done {
		t.Fatalf("second pass: done %v, %v", done, err)
	}
	if err := api.Get(ctx, client.ObjectKey{Namespace: rancherWorkspace, Name: "by-hand"}, registered("by-hand", "")); err != nil {
		t.Error("deleted a cluster Kubehenge did not register")
	}
}
