package provisioner

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

func TestNamesFor(t *testing.T) {
	t.Parallel()
	n, err := NamesFor("kubehenge", "team-a", "web")
	if err != nil {
		t.Fatal(err)
	}
	want := Names{Pool: "kubehenge/team-a.web", User: "kubehenge.team-a.web@pve", Token: "cluster", VolumeOwner: n.VolumeOwner}
	if n != want {
		t.Errorf("got %+v, want %+v", n, want)
	}
	if n.TokenID() != "kubehenge.team-a.web@pve!cluster" {
		t.Errorf("token ID %q", n.TokenID())
	}
	if !regexp.MustCompile(`^web-[0-9a-f]{8}-volumes$`).MatchString(n.VolumeOwner) {
		t.Errorf("volume owner %q", n.VolumeOwner)
	}
	// Removing a cluster deletes its owner's volumes; one of the same name elsewhere must never match.
	if other, _ := NamesFor("kubehenge", "team-b", "web"); other.VolumeOwner == n.VolumeOwner {
		t.Errorf("clusters in two namespaces share volume owner %q", n.VolumeOwner)
	}
}

func TestNamesForNestedWorkloadPool(t *testing.T) {
	t.Parallel()
	n, err := NamesFor("infra/kubehenge", "team-a", "web")
	if err != nil {
		t.Fatal(err)
	}
	if n.Pool != "infra/kubehenge/team-a.web" || n.User != "infra~kubehenge.team-a.web@pve" {
		t.Errorf("got %+v", n)
	}
	// Two management clusters on one Proxmox VE must never give their clusters the same user.
	if other, err := NamesFor("infra-kubehenge", "team-a", "web"); err != nil || other.User == n.User {
		t.Errorf("workload pools infra/kubehenge and infra-kubehenge share user %q (%v)", n.User, err)
	}
}

func TestValidWorkloadPool(t *testing.T) {
	t.Parallel()
	for _, pool := range []string{"kubehenge", "infra/kubehenge", "a.b_c-d"} {
		if err := ValidWorkloadPool(pool); err != nil {
			t.Errorf("%q: %v", pool, err)
		}
	}
	for _, pool := range []string{"", "a/b/c", "/kubehenge", "kubehenge/", "a b", "a~b", "a:b"} {
		if err := ValidWorkloadPool(pool); err == nil {
			t.Errorf("%q accepted", pool)
		}
		if _, err := NamesFor(pool, "team-a", "web"); err == nil {
			t.Errorf("NamesFor accepted workload pool %q", pool)
		}
	}
}

func TestNamesForLongClusterStaysWithinProxmoxLimits(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 63)
	n, err := NamesFor("kubehenge", long, long)
	if err != nil {
		t.Fatal(err)
	}
	user, _, _ := strings.Cut(n.User, "@")
	if len(user) > maxUserName {
		t.Errorf("user %q is longer than %d", user, maxUserName)
	}
	if len(n.VolumeOwner) > maxVMName {
		t.Errorf("volume owner %q is longer than %d", n.VolumeOwner, maxVMName)
	}
	other, _ := NamesFor("kubehenge", long, long[:62]+"b")
	if other.User == n.User {
		t.Errorf("two clusters share user %q", n.User)
	}
	if other.VolumeOwner == n.VolumeOwner {
		t.Errorf("two clusters share volume owner %q", n.VolumeOwner)
	}
	if n.Pool != "kubehenge/"+long+"."+long {
		t.Errorf("pool %q", n.Pool)
	}
}

func TestNamesForRejectsWhatAPoolCannotHold(t *testing.T) {
	t.Parallel()
	if _, err := NamesFor("kubehenge", "ns", "a/b"); err == nil {
		t.Error("accepted a slash")
	}
}

func cluster(variables ...clusterv1.ClusterVariable) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		Name: "web", Namespace: "team-a",
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef:  clusterv1.ClusterClassRef{Name: "kubehenge-rke2", Namespace: "kubehenge-system"},
			Version:   "v1.37.1+rke2r1",
			Variables: variables,
		}},
	}
}

func TestOwns(t *testing.T) {
	t.Parallel()
	c := cluster()
	if !Owns(c, "kubehenge-system") {
		t.Error("does not own a cluster of its class")
	}
	if Owns(c, "elsewhere") {
		t.Error("owns a cluster of a class in another namespace")
	}
	c.Spec.Topology.ClassRef = clusterv1.ClusterClassRef{Name: "kubehenge-rke2"}
	if Owns(c, "kubehenge-system") {
		t.Error("owns a cluster whose class lives in the cluster's own namespace")
	}
	c.Spec.Topology.ClassRef = clusterv1.ClusterClassRef{Name: "other", Namespace: "kubehenge-system"}
	if Owns(c, "kubehenge-system") {
		t.Error("owns a cluster of a class that is not Kubehenge's")
	}
}

func TestVariable(t *testing.T) {
	t.Parallel()
	c := cluster(clusterv1.ClusterVariable{Name: "storage", Value: apiextensionsv1.JSON{Raw: []byte(`"local-lvm"`)}})
	s, err := variable[string](c, "storage")
	if err != nil || s != "local-lvm" {
		t.Errorf("got %q, %v", s, err)
	}
	if _, err := variable[string](c, "missing"); err == nil {
		t.Error("no error for a missing variable")
	}
}

func TestResourceSetSecret(t *testing.T) {
	t.Parallel()
	creds := Credentials{TokenID: "kubehenge.team-a.web@pve!cluster", TokenSecret: "s3cret", VolumeOwner: 4242, Region: "lab"} //nolint:gosec // test data
	s, err := resourceSetSecret(cluster(), Endpoint{URL: "https://pve.example.org:8006", CA: "PEM"}, creds)
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != "addons.cluster.x-k8s.io/resource-set" || s.Namespace != "team-a" {
		t.Errorf("type %q namespace %q", s.Type, s.Namespace)
	}
	var inner corev1.Secret
	if err := yaml.UnmarshalStrict(s.Data["kubehenge-proxmox.yaml"], &inner); err != nil {
		t.Fatal(err)
	}
	if inner.Namespace != "kube-system" || inner.Name != "kubehenge-proxmox" || inner.StringData["ca.crt"] != "PEM" {
		t.Errorf("workload secret %s/%s ca %q", inner.Namespace, inner.Name, inner.StringData["ca.crt"])
	}
	for key, owner := range map[string]int{"ccm.yaml": 0, "csi.yaml": 4242} {
		var cfg proxmoxConfig
		if err := yaml.UnmarshalStrict([]byte(inner.StringData[key]), &cfg); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		want := proxmoxConfig{
			Features: proxmoxFeatures{Provider: "capmox", ControllerVMID: owner},
			Clusters: []proxmoxCluster{{URL: "https://pve.example.org:8006/api2/json", TokenID: creds.TokenID, TokenSecret: "s3cret", Region: "lab"}},
		}
		if cfg.Features != want.Features || len(cfg.Clusters) != 1 || cfg.Clusters[0] != want.Clusters[0] {
			t.Errorf("%s: got %+v, want %+v", key, cfg, want)
		}
	}
}

var testCharts = Charts{
	CCM:    Chart{Repository: "oci://ghcr.io/sergelogvinov/charts", Name: "proxmox-cloud-controller-manager", Version: "0.2.32"},
	CSI:    Chart{Repository: "oci://ghcr.io/sergelogvinov/charts", Name: "proxmox-csi-plugin", Version: "0.5.12"},
	Cilium: Chart{Repository: "https://helm.cilium.io", Name: "cilium", Version: "1.20.2"},
	Calico: Chart{Repository: "https://docs.tigera.io/calico/charts", Name: "tigera-operator", Version: "v3.33.0"},

	Headlamp:      Chart{Repository: "https://kubernetes-sigs.github.io/headlamp", Name: "headlamp", Version: "0.45.0"},
	HeadlampImage: "v0.45.0@sha256:db3f0e0fc58d358d41daa3fe7fc852437552c7ee873c3645470f7b86a8e0db49",

	CertManager: Chart{Repository: "https://charts.jetstack.io", Name: "cert-manager", Version: "v1.21.2"},
	Monitoring:  Chart{Repository: "https://prometheus-community.github.io/helm-charts", Name: "kube-prometheus-stack", Version: "92.10.1"},
	KubeRBACProxy: Image{
		Registry: "ghcr.io", Repository: "kube-rbac-proxy/kube-rbac-proxy", Tag: "v0.23.0",
		Digest: "296fd0a10fb635414b4533b983f0d5a686ce0ce73567b7079b6595ba75034f1a",
	},
	Kyverno:           Chart{Repository: "https://kyverno.github.io/kyverno", Name: "kyverno", Version: "3.9.1"},
	KyvernoPolicies:   Chart{Repository: "https://kyverno.github.io/kyverno", Name: "kyverno-policies", Version: "3.9.1"},
	Istio:             Chart{Repository: "oci://ghcr.io/istio/release/charts", Name: "istiod", Version: "1.31.2"},
	PodSecurityExempt: []string{"kube-system", "node-exporter"},
}

func TestParseChart(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]Chart{
		"oci://ghcr.io/sergelogvinov/charts/proxmox-csi-plugin:0.5.12": testCharts.CSI,
		"https://helm.cilium.io/cilium:1.20.2":                         testCharts.Cilium,
	} {
		if got, err := ParseChart(s); err != nil || got != want {
			t.Errorf("%s: got %+v, %v", s, got, err)
		}
	}
	for _, bad := range []string{"cilium:1.20.2", "https://helm.cilium.io/cilium", "https://helm.cilium.io/:1"} {
		if _, err := ParseChart(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestCNIProxy(t *testing.T) {
	t.Parallel()
	c := cluster()
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.50.0.0/16"}
	values := func(t *testing.T, c *clusterv1.Cluster, cni CNI, chart Chart) string {
		t.Helper()
		proxies, err := helmChartProxies(c, &testCharts, "kubeadm", "local-lvm", cni, &Addons{})
		if err != nil {
			t.Fatal(err)
		}
		specs := map[string]map[string]any{}
		for _, p := range proxies {
			specs[strings.TrimPrefix(p.GetName(), c.Name+"-")] = lookup[map[string]any](t, p.Object, "spec")
		}
		spec := specs["cni"]
		if spec == nil || spec["chartName"] != chart.Name || spec["repoURL"] != chart.Repository {
			t.Fatalf("%s: %v", cni.Name, specs)
		}
		if len(specs) != 3 {
			t.Fatalf("%s: %v", cni.Name, specs)
		}
		return lookup[string](t, spec, "valuesTemplate")
	}
	if v := values(t, c, CNI{Name: "cilium", Pods: "10.42.0.0/16"}, testCharts.Cilium); !strings.Contains(v, "10.50.0.0/16") || strings.Contains(v, "10.42.") {
		t.Errorf("Cilium ignores the cluster's own range: %s", v)
	}
	if v := values(t, cluster(), CNI{Name: "cilium", Pods: "10.42.0.0/16"}, testCharts.Cilium); !strings.Contains(v, "10.42.0.0/16") {
		t.Errorf("Cilium without the class's default range: %s", v)
	}
	// The chart would render the operator's resources before the operator has created their CRDs.
	calico := values(t, c, CNI{Name: "calico", Pods: "10.42.0.0/16"}, testCharts.Calico)
	var operator map[string]any
	if err := yaml.Unmarshal([]byte(calico), &operator); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"installation", "apiServer", "goldmane", "whisker"} {
		if lookup[bool](t, operator, r, "enabled") {
			t.Errorf("the chart renders %s: %s", r, calico)
		}
	}
	if !lookup[bool](t, operator, "manageCRDs") {
		t.Errorf("the operator leaves Calico's CRDs to nobody: %s", calico)
	}
	if _, err := helmChartProxies(c, &testCharts, "kubeadm", "local-lvm", CNI{Name: "weave", Pods: "10.42.0.0/16"}, &Addons{}); err == nil {
		t.Error("no error for a CNI without a chart")
	}
	if _, err := helmChartProxies(cluster(), &testCharts, "kubeadm", "local-lvm", CNI{Name: "cilium"}, &Addons{}); err == nil {
		t.Error("no error for a CNI without a pod range")
	}
}

func TestCalicoResources(t *testing.T) {
	t.Parallel()
	manifest := func(t *testing.T, c *clusterv1.Cluster, cni CNI) []map[string]any {
		t.Helper()
		s, err := calicoResources(c, cni)
		if err != nil {
			t.Fatal(err)
		}
		if s.Type != addonsv1.ClusterResourceSetSecretType || s.Name != c.Name+"-calico" {
			t.Errorf("Secret %s of type %s", s.Name, s.Type)
		}
		parts := strings.Split(string(s.Data["calico.yaml"]), "---\n")[1:]
		docs := make([]map[string]any, 0, len(parts))
		for _, doc := range parts {
			var d map[string]any
			if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
				t.Fatal(err)
			}
			docs = append(docs, d)
		}
		return docs
	}
	c := cluster()
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.50.0.0/16"}
	docs := manifest(t, c, CNI{Name: "calico", Pods: "10.42.0.0/16"})
	kinds := make([]string, 0, len(docs))
	for _, d := range docs {
		kinds = append(kinds, lookup[string](t, d, "kind"))
		if d["apiVersion"] != "operator.tigera.io/v1" || lookup[string](t, d, "metadata", "name") != "default" {
			t.Errorf("resource %v", d)
		}
	}
	if strings.Join(kinds, ",") != "Installation,APIServer,Goldmane,Whisker" {
		t.Errorf("kinds %v", kinds)
	}
	network := func(docs []map[string]any) map[string]any {
		return lookup[map[string]any](t, docs[0], "spec", "calicoNetwork")
	}
	if pool := lookup[map[string]any](t, network(docs), "ipPools", 0); pool["cidr"] != "10.50.0.0/16" {
		t.Errorf("pool %v ignores the cluster's own range", pool)
	}
	if _, ok := network(docs)["containerIPForwarding"]; ok {
		t.Error("pods may forward without the class asking")
	}
	docs = manifest(t, cluster(), CNI{Name: "calico", Pods: "10.42.0.0/16", ContainerForwarding: true})
	if pool := lookup[map[string]any](t, network(docs), "ipPools", 0); pool["cidr"] != "10.42.0.0/16" {
		t.Errorf("pool %v without the class's default range", pool)
	}
	if network(docs)["containerIPForwarding"] != "Enabled" {
		t.Error("pods may not forward although the class asks")
	}
	if _, err := calicoResources(cluster(), CNI{Name: "calico"}); err == nil {
		t.Error("no error without a pod range")
	}
}

func TestHelmChartProxiesHoldNoSecret(t *testing.T) {
	t.Parallel()
	proxies, err := helmChartProxies(cluster(), &testCharts, "kubeadm", "local-lvm", CNI{}, &Addons{})
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 {
		t.Fatalf("%d proxies", len(proxies))
	}
	for _, p := range proxies {
		spec := lookup[map[string]any](t, p.Object, "spec")
		if p.GetNamespace() != "team-a" || spec["namespace"] != "kube-system" {
			t.Errorf("%s: proxy in %q installing into %q", p.GetName(), p.GetNamespace(), spec["namespace"])
		}
		sel := lookup[map[string]any](t, spec, "clusterSelector", "matchLabels")
		if sel["cluster.x-k8s.io/cluster-name"] != "web" {
			t.Errorf("%s: selects %v", p.GetName(), sel)
		}
		values := lookup[string](t, spec, "valuesTemplate")
		if !strings.Contains(values, "existingConfigSecret: kubehenge-proxmox") {
			t.Errorf("%s: values do not point at the workload secret:\n%s", p.GetName(), values)
		}
	}
	csi := lookup[string](t, proxies[1].Object, "spec", "valuesTemplate")
	if !strings.Contains(csi, "storage: local-lvm") || !strings.Contains(csi, "existingConfigSecretKey: csi.yaml") {
		t.Errorf("CSI values:\n%s", csi)
	}

	// Both hold the cluster's Proxmox VE token, so both stay on the control plane; the CCM also
	// reaches Proxmox VE before any CNI runs.
	for i, name := range []string{"CCM", "CSI"} {
		values := map[string]any{}
		if err := yaml.Unmarshal([]byte(lookup[string](t, proxies[i].Object, "spec", "valuesTemplate")), &values); err != nil {
			t.Fatal(err)
		}
		expr := lookup[map[string]any](t, values, "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms", 0, "matchExpressions", 0)
		if expr["key"] != "node-role.kubernetes.io/control-plane" || expr["operator"] != "Exists" {
			t.Errorf("%s affinity: %v", name, values["affinity"])
		}
		tolerated := map[string]bool{}
		for _, tol := range lookup[[]any](t, values, "tolerations") {
			tolerated[lookup[string](t, tol, "key")] = true
		}
		if !tolerated["node-role.kubernetes.io/control-plane"] || tolerated["node.cloudprovider.kubernetes.io/uninitialized"] != (name == "CCM") {
			t.Errorf("%s tolerations: %v", name, values["tolerations"])
		}
		if daemonSet, set := values["useDaemonSet"].(bool); (set && daemonSet) != (name == "CCM") {
			t.Errorf("%s useDaemonSet: %v", name, values["useDaemonSet"])
		}
	}
}

func headlampProxyValues(t *testing.T, h Headlamp) (map[string]any, map[string]any) {
	t.Helper()
	proxies, err := helmChartProxies(cluster(), &testCharts, "kubeadm", "local-lvm", CNI{}, &Addons{Headlamp: h})
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 3 || proxies[2].GetName() != "web-headlamp" {
		t.Fatalf("proxies %v", proxies)
	}
	spec := lookup[map[string]any](t, proxies[2].Object, "spec")
	values := map[string]any{}
	if err := yaml.Unmarshal([]byte(lookup[string](t, spec, "valuesTemplate")), &values); err != nil {
		t.Fatal(err)
	}
	return spec, values
}

func TestHeadlampHasNoRightsOfItsOwn(t *testing.T) {
	t.Parallel()
	spec, values := headlampProxyValues(t, Headlamp{Enabled: true})
	if spec["namespace"] != "headlamp" || spec["chartName"] != "headlamp" {
		t.Errorf("proxy %v", spec)
	}
	if lookup[bool](t, values, "clusterRoleBinding", "create") {
		t.Error("Headlamp's service account is bound to a role")
	}
	if lookup[bool](t, values, "pluginsManager", "enabled") {
		t.Error("Headlamp fetches plugins at run time")
	}
	sc := lookup[map[string]any](t, values, "securityContext")
	if lookup[bool](t, sc, "allowPrivilegeEscalation") || !lookup[bool](t, sc, "readOnlyRootFilesystem") || sc["seccompProfile"] == nil {
		t.Errorf("security context does not meet Pod Security's restricted level: %v", sc)
	}
	if !strings.Contains(lookup[string](t, values, "image", "tag"), "@sha256:") {
		t.Error("Headlamp's image is not pinned by digest")
	}
	if _, ok := values["ingress"]; ok {
		t.Error("an Ingress without a hostname")
	}
}

func TestHeadlampIngressOnlyWithAHostname(t *testing.T) {
	t.Parallel()
	_, values := headlampProxyValues(t, Headlamp{Enabled: true, Hostname: "headlamp.example.org", TLSSecret: "headlamp-tls"}) //nolint:gosec // names a Secret
	ingress := lookup[map[string]any](t, values, "ingress")
	tls := lookup[map[string]any](t, ingress, "tls", 0)
	if !lookup[bool](t, ingress, "enabled") || tls["secretName"] != "headlamp-tls" || lookup[string](t, tls, "hosts", 0) != "headlamp.example.org" {
		t.Errorf("ingress %v", ingress)
	}
}

// networkCluster is a cluster on that network with one worker, so its control planes run no
// workloads.
func networkCluster(gateway string, prefix int) *clusterv1.Cluster {
	c := cluster(clusterv1.ClusterVariable{Name: "network", Value: apiextensionsv1.JSON{
		Raw: fmt.Appendf(nil, `{"bridge":"vmbr0","addresses":["%s"],"prefix":%d,"gateway":"%s","dnsServers":["%s"]}`, gateway, prefix, gateway, gateway),
	}})
	c.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
	return c
}

// lookup walks a decoded document by object keys and list indexes, failing the test where the
// document has another shape.
func lookup[T any](t *testing.T, doc any, path ...any) T {
	t.Helper()
	for _, step := range path {
		switch step := step.(type) {
		case string:
			m, ok := doc.(map[string]any)
			if !ok {
				t.Fatalf("%v has no %q: not an object", doc, step)
			}
			doc = m[step]
		case int:
			list, ok := doc.([]any)
			if !ok || step >= len(list) {
				t.Fatalf("%v has no item %d", doc, step)
			}
			doc = list[step]
		default:
			t.Fatalf("path step %v is neither a key nor an index", step)
		}
	}
	v, ok := doc.(T)
	if !ok {
		t.Fatalf("%v is a %T, not a %T", doc, doc, v)
	}
	return v
}

func hooksFor(t *testing.T, objs ...client.Object) (*Hooks, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	class := &clusterv1.ClusterClass{
		Name: "kubehenge-rke2", Namespace: "kubehenge-system",
		Annotations: map[string]string{
			DefaultPodsAnnotation: "10.42.0.0/16", DefaultServicesAnnotation: "10.43.0.0/16",
			ControlPlaneSizesAnnotation: rke2Sizes,
		},
		Spec: clusterv1.ClusterClassSpec{Variables: []clusterv1.ClusterClassVariable{
			machineVariable("controlPlaneMachine"), machineVariable("workerMachine"),
		}},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, class)...).Build()
	return &Hooks{Reader: reader, ClassNamespace: "kubehenge-system"}, reader
}

// machineVariable is a machine variable as the RKE2 class declares it: two vCPUs and 4096 MiB by
// default and at least.
func machineVariable(name string) clusterv1.ClusterClassVariable {
	return clusterv1.ClusterClassVariable{Name: name, Schema: clusterv1.VariableSchema{OpenAPIV3Schema: clusterv1.JSONSchemaProps{
		Type:    "object",
		Default: &apiextensionsv1.JSON{Raw: []byte(`{"cores":2,"memoryMiB":4096,"diskGiB":30}`)},
		Properties: map[string]clusterv1.JSONSchemaProps{
			"cores":     {Type: "integer", Default: &apiextensionsv1.JSON{Raw: []byte("2")}, Minimum: new(int64(2))},
			"memoryMiB": {Type: "integer", Minimum: new(int64(4096))},
		},
	}}}
}

func callBeforeClusterCreate(h *Hooks, c *clusterv1.Cluster) *runtimehooksv1.BeforeClusterCreateResponse {
	resp := &runtimehooksv1.BeforeClusterCreateResponse{}
	h.BeforeClusterCreate(context.Background(), &runtimehooksv1.BeforeClusterCreateRequest{Cluster: *c}, resp)
	return resp
}

func TestBeforeClusterCreateRefusesOverlappingRanges(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	// The class's default service range, 10.43.0.0/16, holds these nodes.
	resp := callBeforeClusterCreate(hooks, networkCluster("10.43.7.1", 24))
	if resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "service range 10.43.0.0/16") {
		t.Errorf("default range: %+v", resp)
	}
	c := networkCluster("192.168.1.1", 24)
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"192.168.0.0/16"}
	resp = callBeforeClusterCreate(hooks, c)
	if resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "pod range 192.168.0.0/16") {
		t.Errorf("own range: %+v", resp)
	}
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.50.0.0/16"}
	if resp := callBeforeClusterCreate(hooks, c); resp.Status != runtimehooksv1.ResponseStatusSuccess {
		t.Errorf("ranges beside the network: %+v", resp)
	}
}

func TestBeforeClusterCreateWaitsForCredentials(t *testing.T) {
	t.Parallel()
	hooks, reader := hooksFor(t)
	c := networkCluster("192.0.2.1", 24)
	call := func(c *clusterv1.Cluster) *runtimehooksv1.BeforeClusterCreateResponse {
		return callBeforeClusterCreate(hooks, c)
	}
	if resp := call(c); resp.Status != runtimehooksv1.ResponseStatusSuccess || resp.RetryAfterSeconds == 0 {
		t.Errorf("without credentials: %+v", resp)
	}
	// Recorded before the volume owner is created, and not yet ready.
	partial := credentialsSecret(c, Credentials{VolumeOwner: 4242, VolumeOwnerNode: "pve"}, false)
	if err := reader.Create(context.Background(), partial); err != nil {
		t.Fatal(err)
	}
	if resp := call(c); resp.Status != runtimehooksv1.ResponseStatusSuccess || resp.RetryAfterSeconds == 0 {
		t.Errorf("with credentials not yet ready: %+v", resp)
	}
	if err := reader.Update(context.Background(), credentialsSecret(c, Credentials{VolumeOwner: 4242, VolumeOwnerNode: "pve"}, true)); err != nil {
		t.Fatal(err)
	}
	if resp := call(c); resp.Status != runtimehooksv1.ResponseStatusSuccess || resp.RetryAfterSeconds != 0 {
		t.Errorf("with credentials ready: %+v", resp)
	}
	other := cluster()
	other.Name = "foreign"
	other.Spec.Topology.ClassRef.Name = "someone-elses"
	if resp := call(other); resp.Status != runtimehooksv1.ResponseStatusSuccess || resp.RetryAfterSeconds != 0 {
		t.Errorf("foreign cluster: %+v", resp)
	}
}
