package provisioner

import (
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/Masterminds/sprig/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// talosProxies are the values of a Talos cluster's add-on proxies, by name.
func talosProxies(t *testing.T, cni CNI, addons *Addons) map[string]map[string]any {
	t.Helper()
	c := cluster()
	c.Spec.Topology.ClassRef.Name = ClassPrefix + "talos"
	c.Spec.Topology.Version = "v1.36.5"
	proxies, err := helmChartProxies(c, &testCharts, templates.Talos, "local-lvm", cni, addons)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]map[string]any{}
	for _, p := range proxies {
		v := map[string]any{}
		if err := yaml.Unmarshal([]byte(lookup[string](t, p.Object, "spec", "valuesTemplate")), &v); err != nil {
			t.Fatal(err)
		}
		values[p.GetName()] = v
	}
	return values
}

func TestCiliumOnTalosLeavesTheCgroupsAndModulesToTalos(t *testing.T) {
	t.Parallel()
	agent := talosProxies(t, CNI{Name: cniCilium, Pods: "10.244.0.0/16"}, &Addons{})["web-cni"]
	if agent == nil {
		t.Fatal("no Cilium")
	}
	if lookup[bool](t, agent, "cgroup", "autoMount", "enabled") || lookup[string](t, agent, "cgroup", "hostRoot") != "/sys/fs/cgroup" {
		t.Errorf("Cilium mounts the cgroups Talos mounts: %v", agent["cgroup"])
	}
	capabilities := lookup[[]any](t, agent, "securityContext", "capabilities", "ciliumAgent")
	if slices.Contains(capabilities, any("SYS_MODULE")) || !slices.Contains(capabilities, any("NET_ADMIN")) {
		t.Errorf("Cilium's agent capabilities %v", capabilities)
	}

	_, other := proxySpecs(t, templates.Kubeadm, CNI{Name: cniCilium, Pods: "10.244.0.0/16"}, &Addons{})
	if _, set := other["web-cni"]["cgroup"]; set {
		t.Errorf("Cilium is fitted to Talos on kubeadm: %v", other["web-cni"]["cgroup"])
	}
}

func TestCalicoInstallsNoFlexVolumeDriver(t *testing.T) {
	t.Parallel()
	installation := func(class string) map[string]any {
		c := cluster()
		c.Spec.Topology.ClassRef.Name = class
		s, err := calicoResources(c, CNI{Name: "calico", Pods: "10.244.0.0/16"}, testCharts.CalicoImages)
		if err != nil {
			t.Fatal(err)
		}
		for doc := range strings.SplitSeq(string(s.Data["calico.yaml"]), "---\n") {
			var d map[string]any
			if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
				t.Fatal(err)
			}
			if d["kind"] == "Installation" {
				return lookup[map[string]any](t, d, "spec")
			}
		}
		t.Fatal("no Installation")
		return nil
	}
	for _, class := range []string{"talos", "kubeadm", "rke2", "k3s"} {
		if installation(ClassPrefix + class)["flexVolumePath"] != "None" {
			t.Errorf("Calico installs its FlexVolume driver into the read-only /usr of %s's nodes", class)
		}
	}
}

func TestTalosGetsTraefik(t *testing.T) {
	t.Parallel()
	if talosProxies(t, CNI{}, &Addons{})["web-traefik"] == nil {
		t.Error("a Talos cluster, which brings no ingress controller, gets no Traefik")
	}
}

func TestMonitoringOnTalosScrapesWhatTalosLets(t *testing.T) {
	t.Parallel()
	v := talosProxies(t, CNI{}, &Addons{Monitoring: Monitoring{Enabled: true}})["web-monitoring"]
	if v == nil {
		t.Fatal("no monitoring")
	}
	for _, component := range []string{"kubeControllerManager", "kubeScheduler"} {
		if lookup[bool](t, v, component, "enabled") {
			t.Errorf("%s is scraped, which no proxy can verify on Talos", component)
		}
	}
	rendered := manifests(t, v)
	proxied := make([]string, 0, len(rendered))
	for key := range rendered {
		proxied = append(proxied, key)
	}
	joined := strings.Join(proxied, " ")
	if !strings.Contains(joined, kubeProxyMetrics) || !strings.Contains(joined, "etcd") {
		t.Errorf("kube-proxy or etcd unscraped: %v", proxied)
	}
	if strings.Contains(joined, controllerMetrics) || strings.Contains(joined, schedulerMetrics) {
		t.Errorf("proxies for components Talos gives no verifiable certificate: %v", proxied)
	}

	served := talosProxies(t, CNI{}, &Addons{Monitoring: Monitoring{Enabled: true}, talosServing: true})["web-monitoring"]
	for _, component := range []string{"kubeControllerManager", "kubeScheduler"} {
		if _, off := lookup[map[string]any](t, served, component)["enabled"]; off {
			t.Errorf("%s unscraped on a Talos release that gives it a certificate of the cluster CA", component)
		}
	}
	rendered = manifests(t, served)
	for _, component := range []string{controllerMetrics, schedulerMetrics} {
		found := false
		for key := range rendered {
			found = found || strings.Contains(key, component)
		}
		if !found {
			t.Errorf("no proxy for %s on a Talos release that gives it a certificate of the cluster CA", component)
		}
	}
}

func TestTalosControlPlaneMetricsAreScrapedOnceTheirCertificatesComeFromTheClusterCA(t *testing.T) {
	t.Parallel()
	pod := func(component string, args ...string) *corev1.Pod {
		return &corev1.Pod{
			Namespace: metav1.NamespaceSystem, Name: component + "-web-cp-0",
			Labels: map[string]string{"component": component},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: component, Command: append([]string{"/usr/local/bin/" + component}, args...)}}},
		}
	}
	const serving = "--tls-cert-file=/system/secrets/kubernetes/serving/tls.crt"
	for name, tc := range map[string]struct {
		pods []client.Object
		want bool
	}{
		"both served":         {[]client.Object{pod("kube-controller-manager", serving), pod("kube-scheduler", serving)}, true},
		"signing their own":   {[]client.Object{pod("kube-controller-manager", "--bind-address=127.0.0.1"), pod("kube-scheduler", "--bind-address=127.0.0.1")}, false},
		"one of them served":  {[]client.Object{pod("kube-controller-manager", serving), pod("kube-scheduler")}, false},
		"no scheduler to see": {[]client.Object{pod("kube-controller-manager", serving)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := talosCluster("web", "v1.14.4")
			workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(tc.pods...).Build()
			r := &Reconciler{Remotes: &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}}
			if got := r.talosServesControlPlaneMetrics(t.Context(), c); got != tc.want {
				t.Errorf("scraped %v, want %v", got, tc.want)
			}
		})
	}
	if (&Reconciler{}).talosServesControlPlaneMetrics(t.Context(), talosCluster("web", "v1.14.4")) {
		t.Error("a cluster that cannot be reached counts as one whose certificates the proxies verify")
	}
}

// helmRendered is the text as a Go template with Sprig's functions makes of it, which is how the
// Add-on Provider for Helm renders a proxy's values and Helm's tpl a chart's content.
func helmRendered(t *testing.T, text string) string {
	t.Helper()
	tpl, err := template.New("values").Funcs(sprig.TxtFuncMap()).Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := tpl.Execute(&out, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestTalosLogsKeepTheirUnitThroughTheProviderAndTheChart(t *testing.T) {
	t.Parallel()
	c := cluster()
	c.Spec.Topology.ClassRef.Name = ClassPrefix + "talos"
	c.Spec.Topology.Version = "v1.36.5"
	proxies, err := helmChartProxies(c, &testCharts, templates.Talos, "local-lvm", CNI{}, logsOnWith(Logs{Collector: "node"}))
	if err != nil {
		t.Fatal(err)
	}
	const unit = `template = "{{ if .unit }}{{ .unit }}{{ else if .facility }}kernel{{ end }}"`
	for _, p := range proxies {
		if p.GetName() != "web-alloy-node" {
			continue
		}
		values := map[string]any{}
		if err := yaml.Unmarshal([]byte(helmRendered(t, lookup[string](t, p.Object, "spec", "valuesTemplate"))), &values); err != nil {
			t.Fatal(err)
		}
		if config := helmRendered(t, lookup[string](t, values, "alloy", "configMap", "content")); !strings.Contains(config, unit) {
			t.Errorf("Alloy runs on Talos without the unit of what Talos sends:\n%s", config)
		}
		return
	}
	t.Fatal("no node collector")
}

func TestNodeCollectorOnTalosReadsTheAuditLogAndWhatTalosSends(t *testing.T) {
	t.Parallel()
	node := talosProxies(t, CNI{}, logsOnWith(Logs{Collector: "node"}))["web-alloy-node"]
	if node == nil {
		t.Fatal("no node collector")
	}
	config := lookup[string](t, node, "alloy", "configMap", "content")
	if strings.Contains(config, "loki.source.journal") || !strings.Contains(config, `"/var/log/audit/kube/kube-apiserver.log"`) ||
		!strings.Contains(config, `listen_address = "0.0.0.0:5170"`) || !strings.Contains(config, `"job" = "talos"`) {
		t.Errorf("the node collector's configuration on Talos:\n%s", config)
	}
	if lookup[string](t, node, "alloy", "stabilityLevel") != "experimental" {
		t.Error("otelcol.receiver.tcplog needs Alloy's experimental components")
	}
	port := lookup[map[string]any](t, node, "alloy", "extraPorts", 0)
	if port["hostPort"] != float64(talosLogsPort) || port["targetPort"] != float64(talosLogsPort) || port["protocol"] != "TCP" {
		t.Errorf("Talos's log port %v", port)
	}
	var policy map[string]any
	for i := range lookup[[]any](t, node, "extraObjects") {
		if o := lookup[map[string]any](t, node, "extraObjects", i); o["kind"] == "NetworkPolicy" {
			policy = o
		}
	}
	if policy == nil || lookup[[]any](t, policy, "spec", "policyTypes")[0] != "Ingress" || lookup[map[string]any](t, policy, "spec")["ingress"] != nil {
		t.Errorf("the node collector's port is open beyond its node: %v", policy)
	}
	for i := range lookup[[]any](t, node, "alloy", "mounts", "extra") {
		if m := lookup[map[string]any](t, node, "alloy", "mounts", "extra", i); m["name"] == "journal" {
			t.Errorf("Talos's missing journal is mounted: %v", m)
		}
	}
	security := lookup[map[string]any](t, node, "alloy", "securityContext")
	if add := lookup[[]any](t, security, "capabilities", "add"); !slices.Equal(add, []any{"DAC_READ_SEARCH"}) ||
		lookup[string](t, security, "capabilities", "drop", 0) != "ALL" || !lookup[bool](t, security, "readOnlyRootFilesystem") {
		t.Errorf("security on Talos %v", security)
	}

	values, _ := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{Collector: "node"}))
	if _, set := lookup[map[string]any](t, values["web-alloy-node"], "alloy", "securityContext", "capabilities")["add"]; set {
		t.Error("the node collector gets a capability on kubeadm")
	}
}
