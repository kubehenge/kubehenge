package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func TestParseImage(t *testing.T) {
	t.Parallel()
	ref := "ghcr.io/kube-rbac-proxy/kube-rbac-proxy:v0.23.0@sha256:296fd0a10fb635414b4533b983f0d5a686ce0ce73567b7079b6595ba75034f1a"
	got, err := ParseImage(ref)
	if err != nil || got != testCharts.KubeRBACProxy || got.String() != ref {
		t.Errorf("ParseImage(%q) = %+v, %v", ref, got, err)
	}
	for _, bad := range []string{
		"ghcr.io/kube-rbac-proxy/kube-rbac-proxy:v0.23.0",
		"kube-rbac-proxy:v0.23.0@sha256:296fd0a10fb635414b4533b983f0d5a686ce0ce73567b7079b6595ba75034f1a",
		"ghcr.io/kube-rbac-proxy/kube-rbac-proxy@sha256:296fd0a10fb635414b4533b983f0d5a686ce0ce73567b7079b6595ba75034f1a",
		"ghcr.io/kube-rbac-proxy/kube-rbac-proxy:@sha256:296fd0a10fb635414b4533b983f0d5a686ce0ce73567b7079b6595ba75034f1a",
		"ghcr.io/kube-rbac-proxy/kube-rbac-proxy:v0.23.0@sha256:296fd0a1",
	} {
		if _, err := ParseImage(bad); err == nil {
			t.Errorf("ParseImage(%q) took it", bad)
		}
	}
}

// monitoringProxies are the cluster's proxies with monitoring on, by name, each with its values.
func monitoringProxies(t *testing.T, distribution templates.Distribution, m Monitoring) (map[string]map[string]any, map[string]string) {
	t.Helper()
	m.Enabled = true
	return proxiesOf(t, distribution, &Addons{Monitoring: m})
}

func proxiesOf(t *testing.T, distribution templates.Distribution, addons *Addons) (map[string]map[string]any, map[string]string) {
	t.Helper()
	proxies, err := helmChartProxies(cluster(), &testCharts, distribution, "local-lvm", CNI{}, addons)
	if err != nil {
		t.Fatal(err)
	}
	values, raw := map[string]map[string]any{}, map[string]string{}
	for _, p := range proxies {
		spec := lookup[map[string]any](t, p.Object, "spec")
		raw[p.GetName()] = lookup[string](t, spec, "valuesTemplate")
		v := map[string]any{}
		if err := yaml.Unmarshal([]byte(raw[p.GetName()]), &v); err != nil {
			t.Fatal(err)
		}
		v["namespace"] = spec["namespace"]
		values[p.GetName()] = v
	}
	return values, raw
}

// manifests are the release's extra manifests, by kind and name.
func manifests(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for i := range lookup[[]any](t, values, "extraManifests") {
		m := lookup[map[string]any](t, values, "extraManifests", i)
		out[lookup[string](t, m, "kind")+"/"+lookup[string](t, m, "metadata", "name")] = m
	}
	return out
}

func TestMonitoringVerifiesEveryScrape(t *testing.T) {
	t.Parallel()
	for _, distribution := range []templates.Distribution{templates.Kubeadm, templates.RKE2, templates.K3s} {
		t.Run(string(distribution), func(t *testing.T) {
			t.Parallel()
			values, raw := monitoringProxies(t, distribution, Monitoring{})
			if !lookup[bool](t, values["web-cert-manager"], "enableCertificateOwnerRef") {
				t.Error("cert-manager is not installed with monitoring, or keeps Secrets past their Certificates")
			}
			v := values["web-monitoring"]
			if v == nil || v["namespace"] != "monitoring" {
				t.Fatalf("no monitoring proxy in the monitoring namespace: %v", values)
			}
			if strings.Contains(raw["web-monitoring"], "insecureSkipVerify: true") {
				t.Error("a scrape skips verification")
			}
			if lookup[bool](t, v, "kubelet", "serviceMonitor", "tlsConfig", "insecureSkipVerify") {
				t.Error("the kubelets' certificates are not verified")
			}
			for _, component := range []string{"kubeEtcd", "coreDns"} {
				if a, set := lookup[map[string]any](t, v, component, "serviceMonitor")["authorization"]; !set || a != nil {
					t.Errorf("%s is sent the chart's token: %v", component, a)
				}
			}
			etcd := lookup[map[string]any](t, v, "kubeEtcd", "serviceMonitor")
			if etcd["scheme"] != "https" || lookup[string](t, etcd, "tlsConfig", "cert", "secret", "name") != metricsClientSecret ||
				lookup[string](t, etcd, "tlsConfig", "serverName") != "kubehenge-etcd-metrics.kube-system.svc" {
				t.Errorf("etcd is not scraped through its proxy with Prometheus's certificate: %v", etcd)
			}
			if lookup[string](t, v, "kubeEtcd", "service", "selector", appName) != etcdMetrics {
				t.Error("etcd's Service selects etcd itself, which serves only on loopback")
			}
			exporter := lookup[map[string]any](t, v, "prometheus-node-exporter")
			if !lookup[bool](t, exporter, "kubeRBACProxy", "tls", "tlsClientAuth") || exporter["namespaceOverride"] != nodeExporterNamespace ||
				lookup[string](t, exporter, "prometheus", "monitor", "scheme") != "https" {
				t.Errorf("node-exporter serves without client certificates: %v", exporter)
			}
			if !strings.HasSuffix(lookup[string](t, exporter, "kubeRBACProxy", "image", "sha"), testCharts.KubeRBACProxy.Digest) {
				t.Error("node-exporter's proxy is not pinned by digest")
			}
			objects := manifests(t, v)
			if lookup[string](t, objects, "Namespace/node-exporter", "metadata", "labels", "pod-security.kubernetes.io/enforce") != "privileged" {
				t.Error("node-exporter's namespace does not allow what it reads of the host")
			}
			for _, c := range []string{"Certificate/" + metricsClientSecret, "Certificate/" + metricsProxyTLS, "ClusterIssuer/" + metricsIssuer} {
				if objects[c] == nil {
					t.Errorf("no %s", c)
				}
			}
			proxy := lookup[map[string]any](t, objects, "DaemonSet/"+etcdMetrics, "spec", "template", "spec")
			args := lookup[[]any](t, proxy, "containers", 0, "args")
			for _, want := range []string{"--secure-listen-address=$(HOST_IP):2381", "--upstream=http://127.0.0.1:2381/", "--allow-paths=/metrics", "--client-ca-file=/etc/kube-rbac-proxy/ca.crt"} {
				if !slices.Contains(args, any(want)) {
					t.Errorf("etcd's proxy lacks %s: %v", want, args)
				}
			}
			if proxy["affinity"] == nil || !lookup[bool](t, proxy, "securityContext", "runAsNonRoot") {
				t.Errorf("etcd's proxy runs beyond the control planes or as root: %v", proxy)
			}
			checkControlPlaneScrapes(t, distribution, v, objects)
			if _, ok := v["grafana"]; ok {
				t.Error("an Ingress without a hostname")
			}
		})
	}
}

// checkControlPlaneScrapes checks how the controller manager, the scheduler and kube-proxy are
// reached: through the kubelet on k3s, or through their proxies elsewhere, which verify the two
// that serve HTTPS against the cluster CA.
func checkControlPlaneScrapes(t *testing.T, distribution templates.Distribution, v, objects map[string]any) {
	t.Helper()
	kcm := lookup[map[string]any](t, v, "kubeControllerManager")
	_, kubeProxy := objects["DaemonSet/"+kubeProxyMetrics]
	if distribution == templates.K3s {
		if kcm["jobNameOverride"] != "kubelet" || lookup[bool](t, kcm, "serviceMonitor", "enabled") || kubeProxy {
			t.Errorf("k3s's components are scraped apart from its kubelet: %v", kcm)
		}
		return
	}
	if !kubeProxy {
		t.Error("kube-proxy has no proxy")
	}
	for component, c := range map[string]struct {
		proxy string
		port  string
	}{"kubeControllerManager": {controllerMetrics, "10257"}, "kubeScheduler": {schedulerMetrics, "10259"}} {
		monitor := lookup[map[string]any](t, v, component, "serviceMonitor")
		if !lookup[bool](t, monitor, "https") || monitor["authorization"] != nil ||
			lookup[string](t, monitor, "tlsConfig", "serverName") != c.proxy+".kube-system.svc" ||
			lookup[string](t, v, component, "service", "selector", appName) != c.proxy {
			t.Errorf("%s is not scraped through its proxy with Prometheus's certificate: %v", component, v[component])
		}
		proxy := lookup[map[string]any](t, objects, "DaemonSet/"+c.proxy, "spec", "template", "spec")
		args := lookup[[]any](t, proxy, "containers", 0, "args")
		for _, want := range []string{"--secure-listen-address=$(HOST_IP):" + c.port, "--upstream=https://localhost:" + c.port + "/", "--upstream-ca-file=/etc/kube-rbac-proxy-upstream/ca.crt"} {
			if !slices.Contains(args, any(want)) {
				t.Errorf("%s's proxy lacks %s: %v", component, want, args)
			}
		}
		if proxy["affinity"] == nil || lookup[string](t, proxy, "volumes", 1, "configMap", "name") != "kube-root-ca.crt" {
			t.Errorf("%s's proxy runs beyond the control planes or trusts another CA: %v", component, proxy)
		}
	}
}

// The scheduler books a host network port for the whole node, and the components' static pods
// declare theirs for their probes, so a proxy that declared the same port would never be placed on
// a control-plane node.
func TestMetricsProxiesDeclareNoPorts(t *testing.T) {
	t.Parallel()
	for _, distribution := range []templates.Distribution{templates.Kubeadm, templates.RKE2} {
		values, _ := monitoringProxies(t, distribution, Monitoring{})
		proxies := 0
		objects := manifests(t, values["web-monitoring"])
		for name := range objects {
			if !strings.HasPrefix(name, "DaemonSet/") {
				continue
			}
			proxies++
			if ports := lookup[map[string]any](t, objects, name, "spec", "template", "spec", "containers", 0)["ports"]; ports != nil {
				t.Errorf("%s: %s declares %v", distribution, name, ports)
			}
		}
		if proxies == 0 {
			t.Errorf("%s: no metrics proxies", distribution)
		}
	}
}

func TestMonitoringReachesCoreDNSPastRKE2sCISPolicies(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		distribution templates.Distribution
		cis, policy  bool
	}{
		{templates.RKE2, true, true},
		// Elsewhere no policy isolates CoreDNS, and this one would.
		{templates.RKE2, false, false},
		{templates.Kubeadm, true, false},
		{templates.K3s, true, false},
	} {
		values, _ := proxiesOf(t, c.distribution, &Addons{Monitoring: Monitoring{Enabled: true}, CIS: c.cis})
		policy, ok := manifests(t, values["web-monitoring"])["NetworkPolicy/"+coreDNSMetricsPolicy].(map[string]any)
		if ok != c.policy {
			t.Errorf("%s, cis %t: policy %t", c.distribution, c.cis, ok)
			continue
		}
		if !ok {
			continue
		}
		rule := lookup[map[string]any](t, policy, "spec", "ingress", 0)
		if lookup[string](t, policy, "metadata", "namespace") != "kube-system" ||
			lookup[string](t, policy, "spec", "podSelector", "matchLabels", "k8s-app") != "kube-dns" ||
			lookup[string](t, rule, "from", 0, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != monitoringAddon ||
			lookup[float64](t, rule, "ports", 0, "port") != coreDNSMetricsPort {
			t.Errorf("policy %v", policy)
		}
	}
}

func TestMonitoringServesGrafanaAtItsHostname(t *testing.T) {
	t.Parallel()
	values, _ := monitoringProxies(t, "kubeadm", Monitoring{Hostname: "grafana.example.org", TLSSecret: "grafana-tls"}) //nolint:gosec // names a Secret
	grafana := lookup[map[string]any](t, values["web-monitoring"], "grafana")
	if lookup[string](t, grafana, "ingress", "tls", 0, "secretName") != "grafana-tls" ||
		lookup[string](t, grafana, "ingress", "hosts", 0) != "grafana.example.org" ||
		lookup[string](t, grafana, "grafana.ini", "server", "root_url") != "https://grafana.example.org" {
		t.Errorf("grafana %v", grafana)
	}
}

func TestReconcileSwitchesMonitoringOnAndOff(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "monitoring", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)}})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	for _, name := range []string{"web-monitoring", "web-cert-manager"} {
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, hcp); err != nil {
			t.Fatalf("monitoring switched on, %s: %v", name, err)
		}
	}

	current := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Topology.Variables[len(current.Spec.Topology.Variables)-1].Value.Raw = []byte(`{"enabled":false}`)
	if err := kube.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, current); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web-monitoring", "web-cert-manager"} {
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, hcp); err == nil {
			t.Errorf("%s is still there after switching monitoring off", name)
		}
	}
}
