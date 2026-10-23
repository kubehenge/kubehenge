package provisioner

import (
	"slices"
	"strings"
	"testing"

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

func TestCalicoOnTalosInstallsNoFlexVolumeDriver(t *testing.T) {
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
	if installation(ClassPrefix + "talos")["flexVolumePath"] != "None" {
		t.Error("Calico installs its FlexVolume driver into Talos's read-only /usr")
	}
	if _, set := installation(ClassPrefix + "kubeadm")["flexVolumePath"]; set {
		t.Error("Calico goes without its FlexVolume driver on kubeadm")
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
}

func TestNodeCollectorOnTalosReadsTheAuditLogAndNoJournal(t *testing.T) {
	t.Parallel()
	node := talosProxies(t, CNI{}, logsOnWith(Logs{Collector: "node"}))["web-alloy-node"]
	if node == nil {
		t.Fatal("no node collector")
	}
	config := lookup[string](t, node, "alloy", "configMap", "content")
	if strings.Contains(config, "loki.source.journal") || !strings.Contains(config, `"/var/log/audit/kube/kube-apiserver.log"`) {
		t.Errorf("the node collector's configuration on Talos:\n%s", config)
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
