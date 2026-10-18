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
	proxies, err := helmChartProxies(cluster(), &testCharts, distribution, "local-lvm", CNI{}, &Addons{Monitoring: m})
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

// checkControlPlaneScrapes checks how the controller manager and kube-proxy are reached: through
// the kubelet on k3s, or verified at their own ports elsewhere.
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
	if !lookup[bool](t, kcm, "serviceMonitor", "https") || lookup[bool](t, kcm, "serviceMonitor", "tlsConfig", "insecureSkipVerify") || !kubeProxy {
		t.Errorf("the controller manager is not verified, or kube-proxy has no proxy: %v", kcm)
	}
	name, ok := lookup[map[string]any](t, kcm, "serviceMonitor", "tlsConfig")["serverName"].(string)
	if (ok && name == "localhost") != (distribution == templates.RKE2) {
		t.Errorf("controller manager verified for %q", name)
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
