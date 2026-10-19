package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
		"beside everything":   {cluster: web(onAt(`"10.0.0.100-10.0.0.120"`))},
		"off the network":     {cluster: web(onAt(`"10.0.1.100-10.0.1.120"`)), want: "10.0.1.100-10.0.1.120 are not on the nodes' network 10.0.0.0/24"},
		"the gateway":         {cluster: web(onAt(`"10.0.0.0/30"`)), want: "10.0.0.0-10.0.0.3 hold the gateway 10.0.0.1"},
		"the endpoint":        {cluster: web(onAt(`"10.0.0.5"`)), want: "10.0.0.5 hold the control-plane endpoint 10.0.0.5"},
		"the broadcast":       {cluster: web(onAt(`"10.0.0.240/28"`)), want: "10.0.0.240-10.0.0.255 hold 10.0.0.255, the network's own or broadcast address"},
		"the network itself":  {cluster: web(onAt(`"10.0.0.0"`)), want: "10.0.0.0 hold 10.0.0.0, the network's own or broadcast address"},
		"up to the broadcast": {cluster: web(onAt(`"10.0.0.240-10.0.0.254"`))},
		"the nodes":           {cluster: web(onAt(`"10.0.0.15-10.0.0.30"`)), want: "10.0.0.15-10.0.0.30 overlap the nodes' addresses 10.0.0.10-10.0.0.20"},
		"each other":          {cluster: web(onAt(`"10.0.0.100-10.0.0.120","10.0.0.110"`)), want: "10.0.0.100-10.0.0.120 and 10.0.0.110 overlap"},
		"the cluster itself":  {cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)), others: []*clusterv1.Cluster{web(onAt(`"10.0.0.100-10.0.0.120"`))}},
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
	if !lookup[bool](t, values, "speaker", "ignoreExcludeLB") || lookup[bool](t, values, "frrk8s", "enabled") {
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
	spans, err := parseAddresses([]string{"10.0.0.100-10.0.0.120", "10.0.0.130"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]map[string]any{}
	for _, o := range metalLBPool(spans) {
		if o.GetAPIVersion() != "metallb.io/v1beta1" || o.GetNamespace() != metalLBNamespace || o.GetName() != "kubehenge" {
			t.Errorf("not MetalLB's: %v", o.Object)
		}
		kinds[o.GetKind()] = o.Object
	}
	if got := lookup[[]any](t, kinds["IPAddressPool"], "spec", "addresses"); len(got) != 2 || got[0] != "10.0.0.100-10.0.0.120" || got[1] != "10.0.0.130-10.0.0.130" {
		t.Errorf("addresses %v", got)
	}
	if got := lookup[[]any](t, kinds["L2Advertisement"], "spec", "ipAddressPools"); len(got) != 1 || got[0] != "kubehenge" {
		t.Errorf("advertised pools %v", got)
	}
}

// lbWorkload is a workload cluster holding objs, whose server-side applies go to applied by kind,
// and fail as they would before MetalLB's CRDs exist while installed is false.
type lbWorkload struct {
	client.WithWatch

	mu        sync.Mutex
	installed bool
	applied   map[string]map[string]any
}

func newLBWorkload(t *testing.T, objs ...client.Object) *lbWorkload {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	w := &lbWorkload{applied: map[string]map[string]any{}}
	w.WithWatch = interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			u, ok := obj.(interface{ UnstructuredContent() map[string]any })
			if !ok {
				return fmt.Errorf("applied %T", obj)
			}
			o := &unstructured.Unstructured{Object: u.UnstructuredContent()}
			w.mu.Lock()
			defer w.mu.Unlock()
			if !w.installed {
				return &meta.NoKindMatchError{GroupKind: o.GroupVersionKind().GroupKind()}
			}
			w.applied[o.GetKind()] = o.Object
			return nil
		},
	})
	return w
}

func (w *lbWorkload) install() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.installed = true
}

// uninstall stands for removing MetalLB, whose CRDs take the pool with them.
func (w *lbWorkload) uninstall() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.installed = false
	w.applied = map[string]map[string]any{}
}

func (w *lbWorkload) pool() []any {
	w.mu.Lock()
	defer w.mu.Unlock()
	addresses, _, _ := unstructured.NestedSlice(w.applied["IPAddressPool"], "spec", "addresses")
	if _, ok := w.applied["L2Advertisement"]; !ok {
		return nil
	}
	return addresses
}

func TestReconcileAppliesMetalLBPoolWheneverMetalLBRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := withAddons(storageCluster(), map[string]string{loadBalancerVariable: onAt(`"10.0.0.100-10.0.0.120"`)})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	workload := newLBWorkload(t)
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	run := func() ctrl.Result {
		t.Helper()
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	if result := run(); result.RequeueAfter != waitInterval || workload.pool() != nil {
		t.Errorf("before MetalLB's CRDs: requeued after %v, pool %v", result.RequeueAfter, workload.pool())
	}
	workload.install()
	if result := run(); result.RequeueAfter == waitInterval || !slices.Equal(workload.pool(), []any{"10.0.0.100-10.0.0.120"}) {
		t.Errorf("with MetalLB running: requeued after %v, pool %v", result.RequeueAfter, workload.pool())
	}

	setLoadBalancer(t, kube, c, `{"enabled":false,"addresses":["10.0.0.100-10.0.0.120"]}`)
	workload.uninstall()
	run()
	setLoadBalancer(t, kube, c, onAt(`"10.0.0.100-10.0.0.120"`))
	workload.install()
	run()
	if !slices.Equal(workload.pool(), []any{"10.0.0.100-10.0.0.120"}) {
		t.Errorf("MetalLB switched on again has the pool %v", workload.pool())
	}
}

func TestReconcileReleasesServicesOnceKubeVipIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := withAddons(storageCluster(), map[string]string{loadBalancerVariable: `{"enabled":true,"implementation":"kube-vip","addresses":["10.0.0.100-10.0.0.120"]}`})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	provider := &appsv1.Deployment{Name: kubeVipProvider, Namespace: metav1.NamespaceSystem}
	service := func(name string, labels map[string]string) *corev1.Service {
		return &corev1.Service{Name: name, Namespace: "default", Labels: labels, Finalizers: []string{loadBalancerCleanup}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}
	}
	workload := newLBWorkload(t, provider,
		service("handled", map[string]string{kubeVipImplementation: kubeVip}),
		service("someone-elses", nil),
	)
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	run := func() ctrl.Result {
		t.Helper()
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	state := func() (bool, []string, []string) {
		t.Helper()
		current := &clusterv1.Cluster{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
			t.Fatal(err)
		}
		var finalizers [2][]string
		for i, name := range []string{"handled", "someone-elses"} {
			s := &corev1.Service{}
			if err := workload.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, s); err != nil {
				t.Fatal(err)
			}
			finalizers[i] = s.Finalizers
		}
		_, marked := current.Annotations[kubeVipServicesAnnotation]
		return marked, finalizers[0], finalizers[1]
	}

	run()
	if marked, _, _ := state(); !marked {
		t.Error("the cluster running kube-vip is not marked")
	}
	setLoadBalancer(t, kube, c, `{"enabled":false,"addresses":["10.0.0.100-10.0.0.120"]}`)
	if result := run(); result.RequeueAfter != waitInterval {
		t.Errorf("requeued after %v while kube-vip's cloud provider still runs", result.RequeueAfter)
	}
	if marked, handled, _ := state(); !marked || len(handled) != 1 {
		t.Errorf("released while kube-vip's cloud provider still runs: marked %v, finalizers %v", marked, handled)
	}
	if err := workload.Delete(ctx, provider); err != nil {
		t.Fatal(err)
	}
	run()
	if marked, handled, others := state(); marked || len(handled) != 0 || len(others) != 1 {
		t.Errorf("after kube-vip: marked %v, its Service's finalizers %v, another's %v", marked, handled, others)
	}
}

// setLoadBalancer gives the stored cluster another loadBalancer variable, raw JSON.
func setLoadBalancer(t *testing.T, kube client.Client, c *clusterv1.Cluster, raw string) {
	t.Helper()
	current := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	for i, v := range current.Spec.Topology.Variables {
		if v.Name == loadBalancerVariable {
			current.Spec.Topology.Variables[i].Value.Raw = []byte(raw)
		}
	}
	if err := kube.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
}

func TestFreeStaleAddressesLeavesThoseInTheRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service := func(name, labelled, annotated, requested string) *corev1.Service {
		s := &corev1.Service{Name: name, Namespace: "default", Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerIP: requested}}
		if labelled != "" {
			s.Labels = map[string]string{kubeVipImplementation: labelled}
		}
		if annotated != "" {
			s.Annotations = map[string]string{kubeVipAddresses: annotated}
		}
		return s
	}
	workload := newLBWorkload(t,
		service("inside", kubeVip, "10.0.0.105", "10.0.0.105"),
		service("outside", kubeVip, "10.0.0.35", "10.0.0.35"),
		service("partly-outside", kubeVip, "10.0.0.106,10.0.0.36", "10.0.0.106"),
		service("requested-by-its-owner", "", "", "10.0.0.37"),
		service("another-implementation", "metallb", "10.0.0.38", "10.0.0.38"),
	)
	spans, err := parseAddresses([]string{"10.0.0.100-10.0.0.120"})
	if err != nil {
		t.Fatal(err)
	}
	if err := freeStaleAddresses(ctx, workload, spans); err != nil {
		t.Fatal(err)
	}
	for name, kept := range map[string]bool{
		"inside": true, "outside": false, "partly-outside": false, "requested-by-its-owner": true, "another-implementation": true,
	} {
		s := &corev1.Service{}
		if err := workload.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, s); err != nil {
			t.Fatal(err)
		}
		_, labelled := s.Labels[kubeVipImplementation]
		_, annotated := s.Annotations[kubeVipAddresses]
		if has := s.Spec.LoadBalancerIP != ""; has != kept || (name != "requested-by-its-owner" && (labelled != kept || annotated != kept)) {
			t.Errorf("%s: kept %v, labelled %v, annotated %v, requested %q", name, kept, labelled, annotated, s.Spec.LoadBalancerIP)
		}
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
	workload := newLBWorkload(t)
	workload.install()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	set := func(raw string) {
		t.Helper()
		setLoadBalancer(t, kube, c, raw)
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
	}

	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if !exists("web-load-balancer") || exists("web-load-balancer-addresses") || workload.pool() == nil {
		t.Error("MetalLB is not the load balancer")
	}

	workload.uninstall()
	workload.install()
	set(`{"enabled":true,"implementation":"kube-vip","addresses":["10.0.0.100-10.0.0.120"]}`)
	if !exists("web-load-balancer") || !exists("web-load-balancer-addresses") {
		t.Error("kube-vip is not the load balancer")
	}
	if workload.pool() != nil {
		t.Errorf("kube-vip's cluster is given MetalLB's addresses: %v", workload.pool())
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

	daemonSet := func(name string, labels map[string]string) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{Name: name, Namespace: metav1.NamespaceSystem, Labels: labels}
	}
	workload := newLBWorkload(t,
		daemonSet("svclb-traefik-0b1c2d3e", map[string]string{k3sServiceLB: "traefik"}),
		daemonSet("kube-vip", nil),
	)
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
