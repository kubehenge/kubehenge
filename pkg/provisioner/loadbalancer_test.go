package provisioner

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
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

func TestCheckAddresses(t *testing.T) {
	t.Parallel()
	web := func(lb string) *clusterv1.Cluster { return lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", lb) }
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		others  []*clusterv1.Cluster
		want    string
	}{
		"beside everything":   {cluster: web(onAt(`"10.0.0.100-10.0.0.120"`))},
		"off the network":     {cluster: web(onAt(`"10.0.1.100-10.0.1.120"`)), want: "10.0.1.100-10.0.1.120 are outside the network 10.0.0.0/24"},
		"the gateway":         {cluster: web(onAt(`"10.0.0.0/30"`)), want: "10.0.0.0-10.0.0.3 include the gateway 10.0.0.1"},
		"the endpoint":        {cluster: web(onAt(`"10.0.0.5"`)), want: "10.0.0.5 include the control-plane endpoint 10.0.0.5"},
		"the broadcast":       {cluster: web(onAt(`"10.0.0.240/28"`)), want: "10.0.0.240-10.0.0.255 include the network or broadcast address 10.0.0.255"},
		"the network itself":  {cluster: web(onAt(`"10.0.0.0"`)), want: "10.0.0.0 include the network or broadcast address 10.0.0.0"},
		"up to the broadcast": {cluster: web(onAt(`"10.0.0.240-10.0.0.254"`))},
		"the nodes":           {cluster: web(onAt(`"10.0.0.15-10.0.0.30"`)), want: "10.0.0.15-10.0.0.30 overlap node addresses 10.0.0.10-10.0.0.20"},
		"each other":          {cluster: web(onAt(`"10.0.0.100-10.0.0.120","10.0.0.110"`)), want: "10.0.0.100-10.0.0.120 and 10.0.0.110 overlap"},
		"the cluster itself":  {cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)), others: []*clusterv1.Cluster{web(onAt(`"10.0.0.100-10.0.0.120"`))}},
		"another cluster's nodes": {
			cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.100-10.0.0.110", "10.0.0.6", "")},
			want:    "10.0.0.100-10.0.0.120 overlap 10.0.0.100-10.0.0.110 of cluster team-a/db",
		},
		"another cluster's endpoint": {
			cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.30-10.0.0.40", "10.0.0.101", "")},
			want:    "overlap 10.0.0.101 of cluster team-a/db",
		},
		"another cluster's load balancer": {
			cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.30-10.0.0.40", "10.0.0.6", onAt(`"10.0.0.120-10.0.0.130"`))},
			want:    "overlap 10.0.0.120-10.0.0.130 of cluster team-a/db",
		},
		"another VLAN": {
			cluster: web(onAt(`"10.0.0.100-10.0.0.120"`)),
			others:  []*clusterv1.Cluster{lbCluster("db", 7, "10.0.0.100-10.0.0.110", "10.0.0.6", onAt(`"10.0.0.10-10.0.0.20"`))},
		},
		"another cluster's load balancer on this one's nodes": {
			cluster: web(""),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.30-10.0.0.40", "10.0.0.6", onAt(`"10.0.0.12"`))},
			want:    "addresses 10.0.0.10-10.0.0.20 overlap load-balancer addresses 10.0.0.12 of cluster team-a/db",
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
		"nodes off the network":                 {cluster: lbCluster("web", 0, "10.0.1.10-10.0.1.20", "10.0.0.5", ""), want: "node addresses 10.0.1.10-10.0.1.20 are outside the network 10.0.0.0/24"},
		"node addresses that don't read":        {cluster: lbCluster("web", 0, "10.0.0.10-", "10.0.0.5", ""), want: "network.addresses"},
		"the endpoint among the nodes":          {cluster: lbCluster("web", 0, "10.0.0.0/24", "10.0.0.5", ""), want: "control-plane endpoint 10.0.0.5 is inside node addresses 10.0.0.0-10.0.0.255"},
		"the endpoint off the network":          {cluster: lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.1.5", ""), want: "control-plane endpoint 10.0.1.5 is outside the network"},
		"the endpoint on the gateway":           {cluster: lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.1", ""), want: "control-plane endpoint 10.0.0.1 is the gateway"},
		"the nodes of another cluster": {
			cluster: web(""),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.20-10.0.0.30", "10.0.0.6", "")},
			want:    "addresses 10.0.0.10-10.0.0.20 overlap 10.0.0.20-10.0.0.30 of cluster team-a/db",
		},
		"the endpoint of another cluster": {
			cluster: web(""),
			others:  []*clusterv1.Cluster{lbCluster("db", 0, "10.0.0.30-10.0.0.40", "10.0.0.5", "")},
			want:    "addresses 10.0.0.5 overlap 10.0.0.5 of cluster team-a/db",
		},
		"another cluster's nodes on another VLAN": {
			cluster: web(""),
			others:  []*clusterv1.Cluster{lbCluster("db", 7, "10.0.0.10-10.0.0.20", "10.0.0.5", "")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			others := make([]clusterv1.Cluster, len(tc.others))
			for i, o := range tc.others {
				others[i] = *o
			}
			err := checkAddresses(tc.cluster, others)
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
	for _, workload := range []string{"speaker", "controller"} {
		if lookup[string](t, values, workload, "resources", "requests", "cpu") == "" {
			t.Errorf("MetalLB's %s requests no CPU", workload)
		}
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
	if tag := lookup[string](t, values, "image", "tag"); tag != "v0.0.13@sha256:b8b6a5180e678b410e2c5dba86b87fa8099ada16e2f067d83f04e749f8ea54ab" {
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
	for _, o := range metalLBPool(spans, LoadBalancer{}) {
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
	// announcers are MetalLB's advertisements and peers in the cluster, by kind and name.
	announcers map[string]*unstructured.Unstructured
}

func (w *lbWorkload) announcing(kind string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var names []string
	for key, o := range w.announcers {
		if o.GetKind() == kind {
			names = append(names, strings.TrimPrefix(key, kind+"/"))
		}
	}
	slices.Sort(names)
	return names
}

func newLBWorkload(t *testing.T, objs ...client.Object) *lbWorkload {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	w := &lbWorkload{applied: map[string]map[string]any{}, announcers: map[string]*unstructured.Unstructured{}}
	w.WithWatch = interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			l, ok := list.(*unstructured.UnstructuredList)
			if !ok || l.GroupVersionKind().Group != metalLBGroup {
				return c.List(ctx, list, opts...)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			if !w.installed {
				return &meta.NoKindMatchError{GroupKind: l.GroupVersionKind().GroupKind()}
			}
			kind := strings.TrimSuffix(l.GetKind(), "List")
			for _, o := range w.announcers {
				if o.GetKind() == kind {
					l.Items = append(l.Items, *o.DeepCopy())
				}
			}
			return nil
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetObjectKind().GroupVersionKind().Group != metalLBGroup {
				return c.Delete(ctx, obj, opts...)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			delete(w.announcers, obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
			return nil
		},
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
			if _, announces := metalLBAnnouncers[o.GetKind()]; announces {
				w.announcers[o.GetKind()+"/"+o.GetName()] = o.DeepCopy()
			}
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

// bgpOf is a loadBalancer variable in BGP mode with these routers, raw JSON, and addresses off the nodes' network.
func bgpOf(asn string, peers ...string) string {
	return `{"enabled":true,"mode":"bgp","addresses":["198.51.100.0/28"],"bgp":{"asn":` + asn + `,"peers":[` + strings.Join(peers, ",") + `]}}`
}

func peerOf(address, asn string) string {
	return `{"address":"` + address + `","asn":` + asn + `}`
}

func TestMetalLBPoolAnnouncesOverBGPFromTheWorkers(t *testing.T) {
	t.Parallel()
	spans, err := parseAddresses([]string{"198.51.100.0/28"})
	if err != nil {
		t.Fatal(err)
	}
	for name, peers := range map[string][]BGPPeer{
		"one router":  {{Address: "10.0.0.1", ASN: 64512}},
		"two routers": {{Address: "10.0.0.1", ASN: 64512}, {Address: "10.0.1.1", ASN: 4_200_000_000}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lb := LoadBalancer{Enabled: true, Mode: modeBGP, BGP: &BGP{ASN: 65001, Peers: peers}}
			byName := map[string]*unstructured.Unstructured{}
			for _, o := range metalLBPool(spans, lb) {
				byName[o.GetKind()+"/"+o.GetName()] = o
			}
			if len(byName) != 2+len(peers) || byName["L2Advertisement/kubehenge"] != nil {
				t.Fatalf("objects %v", slices.Collect(maps.Keys(byName)))
			}
			if got := lookup[[]any](t, byName["BGPAdvertisement/kubehenge"].Object, "spec", "ipAddressPools"); len(got) != 1 || got[0] != "kubehenge" {
				t.Errorf("advertised pools %v", got)
			}
			for _, p := range peers {
				o := byName["BGPPeer/"+peerName(p.Address)]
				if o == nil || o.GetAPIVersion() != "metallb.io/v1beta2" || o.GetNamespace() != metalLBNamespace {
					t.Fatalf("no peer for %s: %v", p.Address, o)
				}
				spec := lookup[map[string]any](t, o.Object, "spec")
				if spec["myASN"] != int64(65001) || spec["peerASN"] != int64(p.ASN) || spec["peerAddress"] != p.Address {
					t.Errorf("peer %s: %v", p.Address, spec)
				}
				// kube-vip on the control planes holds a session to the same routers from the same addresses.
				selector := lookup[[]any](t, spec, "nodeSelectors")
				want := []any{map[string]any{"matchExpressions": []any{map[string]any{"key": controlPlaneLabel, "operator": "DoesNotExist"}}}}
				if !reflect.DeepEqual(selector, want) {
					t.Errorf("peer %s selects %v", p.Address, selector)
				}
			}
		})
	}
}

func TestMetalLBRunsFRRK8sOnlyInBGPMode(t *testing.T) {
	t.Parallel()
	if !lookup[bool](t, metalLBValues(LoadBalancer{Mode: modeBGP}), "frrk8s", "enabled") ||
		lookup[bool](t, metalLBValues(LoadBalancer{Mode: modeL2}), "frrk8s", "enabled") ||
		lookup[bool](t, metalLBValues(LoadBalancer{}), "frrk8s", "enabled") {
		t.Error("frr-k8s does not follow the mode")
	}
}

// The pinned chart gives frr-k8s no requests, so each of its five containers and the status
// cleaner, which shares the controller's value, gets one, and the class figures count them.
func TestMetalLBGivesFRRK8sRequestsInBGPModeOnly(t *testing.T) {
	t.Parallel()
	if _, has := metalLBValues(LoadBalancer{Mode: modeL2})["frr-k8s"]; has {
		t.Error("layer 2 sets values for a subchart that is off")
	}
	frr := lookup[map[string]any](t, metalLBValues(LoadBalancer{Mode: modeBGP}), "frr-k8s", "frrk8s")
	cpu, memory := 0, 0
	for _, container := range [][]any{{"resources"}, {"frr", "resources"}, {"reloader", "resources"}, {"frrMetrics", "resources"}, {"frrStatus", "resources"}, {"resources"}} {
		requests := lookup[map[string]any](t, frr, append(container, "requests")...)
		c, err := strconv.Atoi(strings.TrimSuffix(lookup[string](t, requests, "cpu"), "m"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := strconv.Atoi(strings.TrimSuffix(lookup[string](t, requests, "memory"), "Mi"))
		if err != nil {
			t.Fatal(err)
		}
		cpu, memory = cpu+c, memory+m
	}
	if wantCPU, wantMemory := BGPBackendRequests(); cpu != wantCPU || memory != wantMemory {
		t.Errorf("values sum to %dm and %d MiB, BGPBackendRequests says %dm and %d MiB", cpu, memory, wantCPU, wantMemory)
	}
}

func TestReconcileSwitchesBetweenLayer2AndBGP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workload := newLBWorkload(t)
	workload.install()
	spans, err := parseAddresses([]string{"198.51.100.0/28"})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(lb LoadBalancer) {
		t.Helper()
		if waiting, err := applyMetalLBPool(ctx, workload, spans, lb); err != nil || waiting {
			t.Fatalf("waiting %v, %v", waiting, err)
		}
	}
	both := &BGP{ASN: 65001, Peers: []BGPPeer{{Address: "10.0.0.1", ASN: 64512}, {Address: "10.0.1.1", ASN: 64513}}}
	apply(LoadBalancer{Enabled: true})
	for kind, want := range map[string][]string{"L2Advertisement": {"kubehenge"}, "BGPAdvertisement": nil, "BGPPeer": nil} {
		if got := workload.announcing(kind); !slices.Equal(got, want) {
			t.Errorf("layer 2: %s = %v, want %v", kind, got, want)
		}
	}
	apply(LoadBalancer{Enabled: true, Mode: modeBGP, BGP: both})
	for kind, want := range map[string][]string{
		"L2Advertisement": nil, "BGPAdvertisement": {"kubehenge"}, "BGPPeer": {"kubehenge-peer-10-0-0-1", "kubehenge-peer-10-0-1-1"},
	} {
		if got := workload.announcing(kind); !slices.Equal(got, want) {
			t.Errorf("bgp: %s = %v, want %v", kind, got, want)
		}
	}
	apply(LoadBalancer{Enabled: true, Mode: modeBGP, BGP: &BGP{ASN: 65001, Peers: both.Peers[1:]}})
	if got := workload.announcing("BGPPeer"); !slices.Equal(got, []string{"kubehenge-peer-10-0-1-1"}) {
		t.Errorf("a router taken out stays: %v", got)
	}
	apply(LoadBalancer{Enabled: true})
	for kind, want := range map[string][]string{"L2Advertisement": {"kubehenge"}, "BGPAdvertisement": nil, "BGPPeer": nil} {
		if got := workload.announcing(kind); !slices.Equal(got, want) {
			t.Errorf("back to layer 2: %s = %v, want %v", kind, got, want)
		}
	}
}

func TestCheckAddressesLetsBGPRouteBeyondTheNodesNetwork(t *testing.T) {
	t.Parallel()
	web := func(lb, endpoint string) *clusterv1.Cluster {
		return lbCluster("web", 0, "10.0.0.10-10.0.0.20", endpoint, lb)
	}
	routed := bgpOf("65001", peerOf("10.0.0.1", "64512"))
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		want    string
	}{
		"a pool off the network in layer 2":      {cluster: web(onAt(`"198.51.100.0/28"`), "10.0.0.5"), want: "are outside the network 10.0.0.0/24"},
		"a pool off the network in bgp":          {cluster: web(routed, "10.0.0.5")},
		"an endpoint off the network in bgp":     {cluster: web(routed, "198.51.100.200")},
		"an endpoint off the network in l2":      {cluster: web(onAt(`"10.0.0.100"`), "198.51.100.200"), want: "is outside the network"},
		"the gateway still counts in bgp":        {cluster: web(strings.Replace(routed, "198.51.100.0/28", "10.0.0.0/30", 1), "10.0.0.5"), want: "include the gateway 10.0.0.1"},
		"an endpoint on the gateway in bgp":      {cluster: web(routed, "10.0.0.1"), want: "is the gateway"},
		"an endpoint among the nodes in bgp":     {cluster: web(routed, "10.0.0.15"), want: "is inside node addresses"},
		"a pool on the endpoint in bgp":          {cluster: web(strings.Replace(routed, "198.51.100.0/28", "198.51.100.200", 1), "198.51.100.200"), want: "include the control-plane endpoint"},
		"a pool on the nodes in bgp":             {cluster: web(strings.Replace(routed, "198.51.100.0/28", "10.0.0.15", 1), "10.0.0.5"), want: "overlap node addresses"},
		"bgp settings without the load balancer": {cluster: web(`{"mode":"bgp","bgp":{"asn":65001,"peers":[`+peerOf("10.0.0.1", "64512")+`]}}`, "198.51.100.200")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := checkAddresses(tc.cluster, nil)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCheckBGP(t *testing.T) {
	t.Parallel()
	peer := peerOf("10.0.0.1", "64512")
	web := func(lb string) *clusterv1.Cluster {
		c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", lb)
		c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0"}}
		return c
	}
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		want    string
	}{
		"one router":                       {cluster: web(bgpOf("65001", peer))},
		"layer 2 needs no settings":        {cluster: web(onAt(`"10.0.0.100"`))},
		"no load balancer":                 {cluster: web("")},
		"the largest AS number":            {cluster: web(bgpOf("4294967295", peerOf("10.0.0.1", "4294967295")))},
		"no settings":                      {cluster: web(`{"enabled":true,"mode":"bgp","addresses":["198.51.100.0/28"]}`), want: "needs bgp settings"},
		"no routers":                       {cluster: web(bgpOf("65001")), want: "at least one router"},
		"AS number zero":                   {cluster: web(bgpOf("0", peer)), want: "ASN 0 is outside 1 to 4294967295"},
		"router AS number zero":            {cluster: web(bgpOf("65001", peerOf("10.0.0.1", "0"))), want: "router 10.0.0.1: ASN 0 is outside 1 to 4294967295"},
		"AS number past 32 bits":           {cluster: web(bgpOf("4294967296", peer)), want: "loadBalancer"},
		"router that is no address":        {cluster: web(bgpOf("65001", peerOf("router.example", "64512"))), want: `router "router.example" is not an IPv4 address`},
		"IPv6 router":                      {cluster: web(bgpOf("65001", peerOf("fd00::1", "64512"))), want: "is not an IPv4 address"},
		"router listed twice":              {cluster: web(bgpOf("65001", peer, peerOf("10.0.0.1", "64513"))), want: "router 10.0.0.1 is listed twice"},
		"router inside the pool":           {cluster: web(strings.Replace(bgpOf("65001", peerOf("198.51.100.3", "64512")), "198.51.100.0/28", "198.51.100.0/24", 1)), want: "router 198.51.100.3 is an address the cluster announces"},
		"router that is the endpoint":      {cluster: web(bgpOf("65001", peerOf("10.0.0.5", "64512"))), want: "router 10.0.0.5 is an address the cluster announces"},
		"kube-vip":                         {cluster: web(strings.Replace(bgpOf("65001", peer), `"mode"`, `"implementation":"kube-vip","mode"`, 1)), want: "BGP needs MetalLB"},
		"control planes only":              {cluster: lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", bgpOf("65001", peer)), want: "needs worker nodes"},
		"control planes only, no balancer": {cluster: lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", `{"mode":"bgp","bgp":{"asn":65001,"peers":[`+peer+`]}}`)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := checkBGP(tc.cluster)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("Talos", func(t *testing.T) {
		t.Parallel()
		c := web(bgpOf("65001", peer))
		c.Spec.Topology.ClassRef.Name = ClassPrefix + "talos"
		if err := checkBGP(c); err == nil || !strings.Contains(err.Error(), "not available on Talos clusters") {
			t.Errorf("got %v", err)
		}
	})
}

func TestValidatorRefusesBGPWhereItCannotWork(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", bgpOf("65001", peerOf("10.0.0.1", "64512")))
	if _, err := v.ValidateCreate(context.Background(), c); err == nil || !strings.Contains(err.Error(), "needs worker nodes") {
		t.Errorf("a cluster without workers was let in: %v", err)
	}
}

func TestBeforeClusterCreateRefusesClashingLoadBalancers(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t, lbCluster("db", 0, "10.0.0.100-10.0.0.110", "10.0.0.6", ""))
	c := lbCluster("web", 0, "10.0.0.10-10.0.0.20", "10.0.0.5", onAt(`"10.0.0.105"`))
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
	if resp := callBeforeClusterCreate(t, hooks, c); resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "team-a/db") {
		t.Errorf("a clash went through: %+v", resp)
	}
}
