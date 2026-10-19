package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

// lbCluster is a cluster on 10.0.0.0/24 behind the gateway 10.0.0.1, on vmbr0 and the VLAN, with
// these nodes' addresses, control-plane endpoint and loadBalancer variable, raw JSON, if any.
func lbCluster(name string, vlan int, machines, endpoint, lb string) *clusterv1.Cluster {
	c := cluster(
		clusterv1.ClusterVariable{Name: "network", Value: apiextensionsv1.JSON{Raw: fmt.Appendf(nil,
			`{"bridge":"vmbr0","vlan":%d,"addresses":["%s"],"prefix":24,"gateway":"10.0.0.1","dnsServers":["10.0.0.1"]}`, vlan, machines)}},
		clusterv1.ClusterVariable{Name: "controlPlaneEndpoint", Value: apiextensionsv1.JSON{Raw: fmt.Appendf(nil, "%q", endpoint)}},
	)
	c.Name = name
	if lb != "" {
		c = withAddons(c, map[string]string{loadBalancerVariable: lb})
	}
	return c
}

func onAt(addresses string) string {
	return `{"enabled":true,"addresses":[` + addresses + `]}`
}

func TestParseAddresses(t *testing.T) {
	t.Parallel()
	spans, err := parseAddresses([]string{"10.0.0.10-10.0.0.20", " 10.0.1.7/30 ", "10.0.2.1", "10.0.3.4 - 10.0.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(spans))
	for i, s := range spans {
		got[i] = s.String()
	}
	if want := []string{"10.0.0.10-10.0.0.20", "10.0.1.4-10.0.1.7", "10.0.2.1", "10.0.3.4"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"10.0.0.20-10.0.0.10", "fd00::1", "fd00::/64", "10.0.0.1-fd00::1", "10.0.0", "10.0.0.0/33", ""} {
		if _, err := parseAddresses([]string{bad}); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestCheckLoadBalancer(t *testing.T) {
	t.Parallel()
	web := func(lb string) *clusterv1.Cluster { return lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", lb) }
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		others  []*clusterv1.Cluster
		want    string
	}{
		"beside everything":  {cluster: web(onAt(`"10.0.0.100-10.0.0.120"`))},
		"off the network":    {cluster: web(onAt(`"10.0.1.100-10.0.1.120"`)), want: "10.0.1.100-10.0.1.120 are not on the nodes' network 10.0.0.0/24"},
		"the gateway":        {cluster: web(onAt(`"10.0.0.0/30"`)), want: "10.0.0.0-10.0.0.3 hold the gateway 10.0.0.1"},
		"the endpoint":       {cluster: web(onAt(`"10.0.0.5"`)), want: "10.0.0.5 hold the control-plane endpoint 10.0.0.5"},
		"the nodes":          {cluster: web(onAt(`"10.0.0.15-10.0.0.30"`)), want: "10.0.0.15-10.0.0.30 overlap the nodes' addresses 10.0.0.10-10.0.0.20"},
		"each other":         {cluster: web(onAt(`"10.0.0.100-10.0.0.120","10.0.0.110"`)), want: "10.0.0.100-10.0.0.120 and 10.0.0.110 overlap"},
		"the cluster itself": {cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)), others: []*clusterv1.Cluster{web(onAt(`"10.0.0.100-10.0.0.120"`))}},
		"another cluster's nodes": {
			cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.100-10.0.0.110", "10.0.0.6", "")},
			want:    "10.0.0.100-10.0.0.120 overlap cluster team-a/db's 10.0.0.100-10.0.0.110",
		},
		"another cluster's endpoint": {
			cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.30-10.0.0.40", "10.0.0.101", "")},
			want:    "overlap cluster team-a/db's 10.0.0.101",
		},
		"another cluster's load balancer": {
			cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.30-10.0.0.40", "10.0.0.6", onAt(`"10.0.0.120-10.0.0.130"`))},
			want:    "overlap cluster team-a/db's 10.0.0.120-10.0.0.130",
		},
		"another VLAN": {
			cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)),
			others:  []*clusterv1.Cluster{lbCluster("db", 7, "10.0.0.100-10.0.0.110", "10.0.0.6", onAt(`"10.0.0.10-10.0.0.20"`))},
		},
		"another cluster's load balancer on this one's nodes": {
			cluster: web(""),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.30-10.0.0.40", "10.0.0.6", onAt(`"10.0.0.12"`))},
			want:    "cluster team-a/db's load-balancer addresses 10.0.0.12 overlap this cluster's 10.0.0.10-10.0.0.20",
		},
		"another cluster's load balancer switched off": {
			cluster: web(""),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.30-10.0.0.40", "10.0.0.6", `{"enabled":false,"addresses":["10.0.0.12"]}`)},
		},
		"another cluster whose variables don't read": {
			cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)),
			others:  []*clusterv1.Cluster{withAddons(cluster(), map[string]string{loadBalancerVariable: onAt(`"10.0.0.100"`)})},
		},
		"no addresses of its own, switched off": {cluster: cluster(), others: []*clusterv1.Cluster{web(onAt(`"10.0.0.100-10.0.0.120"`))}},
		"no addresses of its own, switched on":  {cluster: withAddons(cluster(), map[string]string{loadBalancerVariable: onAt(`"10.0.0.100"`)}), want: "network"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			others := make([]clusterv1.Cluster, len(tc.others))
			for i, o := range tc.others {
				others[i] = *o
			}
			err := checkLoadBalancer(tc.cluster, others)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAddressesChanged(t *testing.T) {
	t.Parallel()
	c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", onAt(`"10.0.0.100"`))
	if !addressesChanged(nil, c) {
		t.Error("a new cluster has no addresses to compare with")
	}
	same := withAddons(c.DeepCopy(), map[string]string{"headlamp": `{"enabled":true}`})
	if addressesChanged(c, same) {
		t.Error("another variable changed the addresses")
	}
	for name, changed := range map[string]*clusterv1.Cluster{
		"network":       lbCluster("web", 0, "10.0.0.10-10.0.0.30", "10.0.0.5", onAt(`"10.0.0.100"`)),
		"endpoint":      lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.6", onAt(`"10.0.0.100"`)),
		"load balancer": lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", onAt(`"10.0.0.101"`)),
		"switched off":  lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", ""),
	} {
		if !addressesChanged(c, changed) {
			t.Errorf("%s: not a change", name)
		}
	}
}

// proxyValues are the spec and values of the proxy of that name, if there is one.
func proxyValues(t *testing.T, proxies []*unstructured.Unstructured, name string) (map[string]any, map[string]any, bool) {
	t.Helper()
	for _, p := range proxies {
		if p.GetName() != name {
			continue
		}
		spec := lookup[map[string]any](t, p.Object, "spec")
		values := map[string]any{}
		if err := yaml.Unmarshal([]byte(lookup[string](t, spec, "valuesTemplate")), &values); err != nil {
			t.Fatal(err)
		}
		return spec, values, true
	}
	return nil, nil, false
}

func TestMetalLBAnswersFromEveryNodeOnLayer2(t *testing.T) {
	t.Parallel()
	lb := LoadBalancer{Enabled: true, Addresses: []string{"10.0.0.100-10.0.0.120"}}
	proxies, err := helmChartProxies(cluster(), &testCharts, "kubeadm", "local-lvm", CNI{}, &Addons{LoadBalancer: lb})
	if err != nil {
		t.Fatal(err)
	}
	spec, values, found := proxyValues(t, proxies, "web-load-balancer")
	if !found || spec["chartName"] != "metallb" || spec["releaseName"] != "metallb" || spec["namespace"] != metalLBNamespace {
		t.Fatalf("MetalLB's proxy: %v", spec)
	}
	if !lookup[bool](t, values, "speaker", "ignoreExcludeLB") || lookup[bool](t, values, "speaker", "frr", "enabled") {
		t.Errorf("values %v", values)
	}
	if _, _, found := proxyValues(t, proxies, "web-load-balancer-addresses"); found {
		t.Error("MetalLB got kube-vip's cloud provider")
	}
}

func TestKubeVipAnnouncesWhatItsCloudProviderHandsOut(t *testing.T) {
	t.Parallel()
	lb := LoadBalancer{Enabled: true, Implementation: kubeVip, Addresses: []string{"10.0.0.100-10.0.0.120", "10.0.0.130/31", "10.0.0.140"}}
	proxies, err := helmChartProxies(cluster(), &testCharts, "rke2", "local-lvm", CNI{}, &Addons{LoadBalancer: lb})
	if err != nil {
		t.Fatal(err)
	}
	spec, values, found := proxyValues(t, proxies, "web-load-balancer")
	if !found || spec["chartName"] != "kube-vip" || spec["namespace"] != metav1.NamespaceSystem {
		t.Fatalf("kube-vip's proxy: %v", spec)
	}
	env := lookup[map[string]any](t, values, "env")
	for k, want := range map[string]string{"svc_enable": "true", "svc_election": "true", "cp_enable": "false", "prometheus_server": kubeVipMetrics} {
		if env[k] != want {
			t.Errorf("env %s = %v, want %s", k, env[k], want)
		}
	}
	if image := lookup[map[string]any](t, values, "image"); image["repository"] != "ghcr.io/kube-vip/kube-vip" ||
		image["tag"] != "v1.3.1@sha256:"+testCharts.KubeVipImage.Digest {
		t.Errorf("kube-vip's image %v", image)
	}
	spec, values, found = proxyValues(t, proxies, "web-load-balancer-addresses")
	if !found || spec["chartName"] != "kube-vip-cloud-provider" || spec["namespace"] != metav1.NamespaceSystem {
		t.Fatalf("the cloud provider's proxy: %v", spec)
	}
	if got := lookup[string](t, values, "cm", "data", "range-global"); got != "10.0.0.100-10.0.0.120,10.0.0.130-10.0.0.131,10.0.0.140-10.0.0.140" {
		t.Errorf("range-global %q", got)
	}
	if tag := lookup[string](t, values, "image", "tag"); tag != "v0.0.13@sha256:"+testCharts.KubeVipCloudProviderImage.Digest {
		t.Errorf("the cloud provider's image %q", tag)
	}

	lb.Addresses = []string{"10.0.0.120-10.0.0.100"}
	if _, err := helmChartProxies(cluster(), &testCharts, "rke2", "local-lvm", CNI{}, &Addons{LoadBalancer: lb}); err == nil {
		t.Error("a backwards range was taken")
	}
}

func TestMetalLBPoolHoldsTheAddresses(t *testing.T) {
	t.Parallel()
	s, err := metalLBPool(cluster(), LoadBalancer{Enabled: true, Addresses: []string{"10.0.0.100-10.0.0.120", "10.0.0.130"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "web-metallb-pool" || s.Namespace != "team-a" || s.Type != addonsv1.ClusterResourceSetSecretType {
		t.Errorf("Secret %s/%s of type %s", s.Namespace, s.Name, s.Type)
	}
	kinds := map[string]map[string]any{}
	for doc := range strings.SplitSeq(string(s.Data["metallb-pool.yaml"]), "---\n") {
		if doc == "" {
			continue
		}
		var r map[string]any
		if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatal(err)
		}
		if r[apiVersionKey] != "metallb.io/v1beta1" || lookup[string](t, r, "metadata", "namespace") != metalLBNamespace ||
			lookup[string](t, r, "metadata", "name") != "kubehenge" {
			t.Errorf("not MetalLB's: %v", r)
		}
		kind, ok := r["kind"].(string)
		if !ok {
			t.Fatalf("no kind: %v", r)
		}
		kinds[kind] = r
	}
	if got := lookup[[]any](t, kinds["IPAddressPool"], "spec", "addresses"); len(got) != 2 || got[0] != "10.0.0.100-10.0.0.120" || got[1] != "10.0.0.130-10.0.0.130" {
		t.Errorf("addresses %v", got)
	}
	if got := lookup[[]any](t, kinds["L2Advertisement"], "spec", "ipAddressPools"); len(got) != 1 || got[0] != "kubehenge" {
		t.Errorf("advertised pools %v", got)
	}
}

func TestReconcileSwitchesTheLoadBalancer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := withAddons(storageCluster(), map[string]string{loadBalancerVariable: onAt(`"10.0.0.100-10.0.0.120"`)})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	exists := func(name string) bool {
		t.Helper()
		hcp := &unstructured.Unstructured{}
		hcp.SetGroupVersionKind(helmChartProxyGVK)
		err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: name}, hcp)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return err == nil
	}
	set := func(raw string) {
		t.Helper()
		current := &clusterv1.Cluster{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
			t.Fatal(err)
		}
		for i, v := range current.Spec.Topology.Variables {
			if v.Name == loadBalancerVariable {
				current.Spec.Topology.Variables[i].Value.Raw = []byte(raw)
			}
		}
		if err := kube.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(current)}); err != nil {
			t.Fatal(err)
		}
	}

	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if !exists("web-load-balancer") || exists("web-load-balancer-addresses") {
		t.Error("MetalLB is not the load balancer")
	}
	crs := &addonsv1.ClusterResourceSet{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name + "-proxmox"}, crs); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(crs.Spec.Resources, addonsv1.ResourceRef{Kind: secretKind, Name: metalLBPoolName(c)}) {
		t.Errorf("MetalLB's addresses are not applied to the cluster: %v", crs.Spec.Resources)
	}

	set(`{"enabled":true,"implementation":"kube-vip","addresses":["10.0.0.100-10.0.0.120"]}`)
	if !exists("web-load-balancer") || !exists("web-load-balancer-addresses") {
		t.Error("kube-vip is not the load balancer")
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(crs), crs); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(crs.Spec.Resources, addonsv1.ResourceRef{Kind: secretKind, Name: metalLBPoolName(c)}) {
		t.Errorf("kube-vip's cluster is given MetalLB's addresses: %v", crs.Spec.Resources)
	}

	set(`{"enabled":true,"implementation":"metallb","addresses":["10.0.0.100-10.0.0.120"]}`)
	if !exists("web-load-balancer") || exists("web-load-balancer-addresses") {
		t.Error("kube-vip's cloud provider stayed beside MetalLB")
	}

	set(`{"enabled":false}`)
	if exists("web-load-balancer") || exists("web-load-balancer-addresses") {
		t.Error("the load balancer stayed after it was switched off")
	}
}

func TestReconcileRemovesWhatServiceLBLeftOnK3s(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := withAddons(storageCluster(), map[string]string{loadBalancerVariable: onAt(`"10.0.0.100-10.0.0.120"`)})
	c.Spec.Topology.ClassRef.Name = "kubehenge-k3s"
	r, _ := newReconciler(t, &fakeCloud{}, c)

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	daemonSet := func(name string, labels map[string]string) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{Name: name, Namespace: metav1.NamespaceSystem, Labels: labels}
	}
	workload := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		daemonSet("svclb-traefik-0b1c2d3e", map[string]string{k3sServiceLB: "traefik"}),
		daemonSet("kube-vip", nil),
	).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}

	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	left := &appsv1.DaemonSetList{}
	if err := workload.List(ctx, left); err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 1 || left.Items[0].Name != "kube-vip" {
		t.Errorf("DaemonSets left %v", left.Items)
	}
}

func TestValidatorHoldsOnlyChangedAddresses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := lbCluster("db", 0, "10.0.0.100-10.0.0.110", "10.0.0.6", "")
	h, _ := hooksFor(t, db)
	v := &ClusterValidator{Hooks: h}
	clashing := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", onAt(`"10.0.0.100-10.0.0.120"`))
	if _, err := v.ValidateCreate(ctx, clashing); err == nil || !strings.Contains(err.Error(), "team-a/db") {
		t.Errorf("a clash was let in: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, clashing, withAddons(clashing.DeepCopy(), map[string]string{"headlamp": `{"enabled":true}`})); err != nil {
		t.Errorf("a clash that was there before held up another change: %v", err)
	}
	beside := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", onAt(`"10.0.0.150-10.0.0.160"`))
	if _, err := v.ValidateUpdate(ctx, beside, clashing); err == nil {
		t.Error("a change into a clash was let in")
	}
	if _, err := v.ValidateUpdate(ctx, clashing, beside); err != nil {
		t.Errorf("a change out of a clash was refused: %v", err)
	}
}

func TestBeforeClusterCreateRefusesClashingLoadBalancers(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t, lbCluster("db", 0, "10.0.0.100-10.0.0.110", "10.0.0.6", ""))
	c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", onAt(`"10.0.0.105"`))
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
	if resp := callBeforeClusterCreate(hooks, c); resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "team-a/db") {
		t.Errorf("a clash went through: %+v", resp)
	}
}
