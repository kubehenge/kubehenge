package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// extraObjects are a release's extraObjects, by kind and name.
func extraObjects(t *testing.T, values map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for i := range lookup[[]any](t, values, "extraObjects") {
		m := lookup[map[string]any](t, values, "extraObjects", i)
		out[lookup[string](t, m, "kind")+"/"+lookup[string](t, m, "metadata", "name")] = m
	}
	return out
}

func logsOnWith(logs Logs) *Addons {
	logs.Enabled = true
	return &Addons{Monitoring: Monitoring{Enabled: true}, Logs: logs}
}

func TestLogsNeedMonitoring(t *testing.T) {
	t.Parallel()
	values, _ := proxiesOf(t, templates.Kubeadm, &Addons{Logs: Logs{Enabled: true}})
	for _, name := range []string{"web-loki", "web-alloy", "web-alloy-node"} {
		if _, ok := values[name]; ok {
			t.Errorf("%s without monitoring", name)
		}
	}
	if err := checkLogs(cluster(variableOf("logs", `{"enabled":true}`))); err == nil || !strings.Contains(err.Error(), "monitoring") {
		t.Errorf("logs without monitoring were let in: %v", err)
	}
	if err := checkLogs(cluster(variableOf("logs", `{"enabled":true}`), variableOf("monitoring", `{"enabled":true}`))); err != nil {
		t.Errorf("logs beside monitoring were refused: %v", err)
	}
	if err := checkLogs(cluster(variableOf("logs", `{"enabled":false}`))); err != nil {
		t.Errorf("logs switched off were refused: %v", err)
	}
}

func TestValidatorAndHookRefuseLogsWithoutMonitoring(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	c := cluster(variableOf("logs", `{"enabled":true}`))
	if _, err := (&ClusterValidator{Hooks: h}).ValidateCreate(context.Background(), c); err == nil || !strings.Contains(err.Error(), "logs need monitoring") {
		t.Errorf("the validator let logs in without monitoring: %v", err)
	}
	created := networkCluster("192.168.1.1", 24)
	created.Spec.Topology.Variables = append(created.Spec.Topology.Variables, variableOf("logs", `{"enabled":true}`))
	if resp := callBeforeClusterCreate(h, created); !strings.Contains(resp.Message, "logs need monitoring") {
		t.Errorf("BeforeClusterCreate let logs in without monitoring: %s %q", resp.Status, resp.Message)
	}
}

func TestLogsInstallLokiAndBothAlloys(t *testing.T) {
	t.Parallel()
	values, raw := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{}))
	for name, namespace := range map[string]string{"web-loki": "logging", "web-alloy": "logging", "web-alloy-node": "logging-node"} {
		v, ok := values[name]
		if !ok {
			t.Fatalf("no %s", name)
		}
		if v["namespace"] != namespace {
			t.Errorf("%s in %v, want %s", name, v["namespace"], namespace)
		}
		// The Add-on Provider for Helm reads values as a template of its own.
		if strings.Contains(raw[name], "{{") {
			t.Errorf("%s's values hold a template action", name)
		}
	}
}

func TestLokiListensOnlyBehindItsProxy(t *testing.T) {
	t.Parallel()
	values, _ := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{RetentionDays: 14, SizeGiB: 25}))
	loki := values["web-loki"]
	if lookup[string](t, loki, "deploymentMode") != "Monolithic" {
		t.Errorf("deploymentMode %v", loki["deploymentMode"])
	}
	for _, key := range []string{"http_listen_address", "grpc_listen_address"} {
		if lookup[string](t, loki, "loki", "server", key) != "127.0.0.1" {
			t.Errorf("Loki's %s is not loopback", key)
		}
	}
	if lookup[string](t, loki, "loki", "limits_config", "retention_period") != "14d" || !lookup[bool](t, loki, "loki", "compactor", "retention_enabled") {
		t.Errorf("retention %v", lookup[map[string]any](t, loki, "loki", "limits_config"))
	}
	proxy := lookup[map[string]any](t, loki, "singleBinary", "extraContainers", 0)
	args := strings.Join(toStrings(t, lookup[[]any](t, proxy, "args")), " ")
	for _, want := range []string{"--secure-listen-address=$(POD_IP):3100", "--upstream=http://127.0.0.1:3100/", "--client-ca-file=", "--tls-min-version=VersionTLS13", "--ignore-paths=/ready,/loki/api/v1/status/buildinfo"} {
		if !strings.Contains(args, want) {
			t.Errorf("the proxy's arguments %q lack %q", args, want)
		}
	}
	if lookup[string](t, loki, "loki", "readinessProbe", "httpGet", "scheme") != "HTTPS" {
		t.Error("readiness does not go through the proxy")
	}

	objects := extraObjects(t, loki)
	claim := objects["PersistentVolumeClaim/loki-storage"]
	if claim == nil || lookup[string](t, claim, "spec", "resources", "requests", "storage") != "25Gi" ||
		lookup[string](t, claim, "metadata", "annotations", "helm.sh/resource-policy") != "keep" {
		t.Errorf("Loki's volume %v", claim)
	}
	if lookup[string](t, loki, "singleBinary", "persistence", "dataVolumeParameters", "persistentVolumeClaim", "claimName") != "loki-storage" {
		t.Error("Loki does not run on its volume")
	}
	push := objects["ClusterRole/kubehenge-loki-push"]
	if lookup[string](t, push, "rules", 0, "nonResourceURLs", 0) != "/loki/api/v1/push" || lookup[string](t, push, "rules", 0, "verbs", 0) != "create" {
		t.Errorf("push rule %v", push)
	}
	if lookup[string](t, objects["ClusterRoleBinding/kubehenge-loki-push"], "subjects", 0, "name") != alloyUser ||
		lookup[string](t, objects["ClusterRoleBinding/kubehenge-loki-read"], "subjects", 0, "name") != grafanaUser {
		t.Error("push and read are not Alloy's and Grafana's")
	}
	if lookup[string](t, objects["ClusterRole/kubehenge-loki-read"], "rules", 0, "verbs", 0) != "get" {
		t.Error("Grafana may do more than read")
	}
	if lookup[string](t, objects["Certificate/loki-tls"], "spec", "dnsNames", 0) != "loki.logging.svc" {
		t.Error("Loki's certificate is not for its Service")
	}
	from := lookup[[]any](t, objects["NetworkPolicy/loki"], "spec", "ingress", 0, "from")
	if len(from) != 3 {
		t.Errorf("Loki is reachable from %v", from)
	}
}

func toStrings(t *testing.T, list []any) []string {
	t.Helper()
	out := make([]string, len(list))
	for i, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%v is not a string", v)
		}
		out[i] = s
	}
	return out
}

func TestGrafanaReadsLokiWithItsCertificate(t *testing.T) {
	t.Parallel()
	values, _ := proxiesOf(t, templates.Kubeadm, logsOnWith(Logs{}))
	grafana := lookup[map[string]any](t, values["web-monitoring"], "grafana")
	source := lookup[map[string]any](t, grafana, "additionalDataSources", 0)
	if source["type"] != "loki" || source["url"] != "https://loki.logging.svc:3100" ||
		lookup[string](t, source, "secureJsonData", "tlsClientKey") != "$__file{/etc/kubehenge/loki-client/tls.key}" {
		t.Errorf("data source %v", source)
	}
	if lookup[string](t, grafana, "extraSecretMounts", 0, "secretName") != lokiClientSecret {
		t.Error("Grafana does not mount its client certificate")
	}
	if manifests(t, values["web-monitoring"])["Certificate/"+lokiClientSecret] == nil {
		t.Error("Grafana gets no client certificate")
	}

	without, _ := monitoringProxies(t, templates.Kubeadm, Monitoring{})
	if _, ok := without["web-monitoring"]["grafana"]; ok {
		t.Error("Grafana has Loki without logs")
	}
}

func TestNodeCollectorReadsTheHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		distribution templates.Distribution
		audit        string
	}{
		{templates.Kubeadm, "/var/log/kubernetes/audit"},
		{templates.RKE2, "/var/lib/rancher/rke2/server/logs"},
		{templates.K3s, "/var/lib/rancher/k3s/server/logs"},
	} {
		distribution, audit := tc.distribution, tc.audit
		values, _ := proxiesOf(t, distribution, logsOnWith(Logs{Collector: "node"}))
		node := values["web-alloy-node"]
		if lookup[string](t, node, "controller", "type") != "daemonset" {
			t.Fatalf("%s: the node collector is a %v", distribution, lookup[any](t, node, "controller", "type"))
		}
		config := lookup[string](t, node, "alloy", "configMap", "content")
		for _, want := range []string{`"/var/log/pods/*/*/*.log"`, "loki.source.journal", `"` + audit + `/audit.log"`, "stage.cri", `min_version = "TLS13"`} {
			if !strings.Contains(config, want) {
				t.Errorf("%s: the node collector's configuration lacks %s", distribution, want)
			}
		}
		for i := range lookup[[]any](t, node, "alloy", "mounts", "extra") {
			m := lookup[map[string]any](t, node, "alloy", "mounts", "extra", i)
			readOnly, ok := m["readOnly"].(bool)
			if writable := !ok || !readOnly; writable != (m["mountPath"] == alloyNodeStorage) {
				t.Errorf("%s: mount %v", distribution, m)
			}
		}
		security := lookup[map[string]any](t, node, "alloy", "securityContext")
		if lookup[float64](t, security, "runAsUser") != 0 || lookup[string](t, security, "capabilities", "drop", 0) != "ALL" ||
			!lookup[bool](t, security, "readOnlyRootFilesystem") || lookup[bool](t, security, "allowPrivilegeEscalation") ||
			lookup[string](t, security, "seLinuxOptions", "type") != "spc_t" {
			t.Errorf("%s: security %v", distribution, security)
		}
		if lookup[bool](t, node, "serviceAccount", "automountServiceAccountToken") || lookup[bool](t, node, "rbac", "create") {
			t.Errorf("%s: the node collector gets rights", distribution)
		}
		if extraObjects(t, node)["Certificate/"+lokiClientSecret] == nil {
			t.Errorf("%s: the node collector has no client certificate", distribution)
		}

		events := values["web-alloy"]
		if strings.Contains(lookup[string](t, events, "alloy", "configMap", "content"), "loki.source.kubernetes \"pods\"") {
			t.Errorf("%s: with the node collector, the API is read for pods' logs too", distribution)
		}
		role := extraObjects(t, events)["ClusterRole/kubehenge-alloy"]
		if rules := lookup[[]any](t, role, "rules"); len(rules) != 1 || lookup[string](t, role, "rules", 0, "resources", 0) != "events" {
			t.Errorf("%s: the events collector's rules %v", distribution, rules)
		}
	}
}

func TestAPICollectorNeedsNothingOfTheHost(t *testing.T) {
	t.Parallel()
	values, _ := proxiesOf(t, templates.RKE2, logsOnWith(Logs{Collector: "api"}))
	if _, ok := values["web-alloy-node"]; ok {
		t.Error("the api collector runs on the nodes")
	}
	alloy := values["web-alloy"]
	config := lookup[string](t, alloy, "alloy", "configMap", "content")
	for _, want := range []string{`loki.source.kubernetes "pods"`, "loki.source.kubernetes_events", `target_label = "job"`} {
		if !strings.Contains(config, want) {
			t.Errorf("configuration lacks %s", want)
		}
	}
	security := lookup[map[string]any](t, alloy, "alloy", "securityContext")
	if !lookup[bool](t, security, "runAsNonRoot") || lookup[string](t, security, "seccompProfile", "type") != "RuntimeDefault" {
		t.Errorf("security %v", security)
	}
	if lookup[string](t, alloy, "image", "digest") != "sha256:"+testCharts.AlloyImage.Digest {
		t.Errorf("image %v", alloy["image"])
	}
	role := extraObjects(t, alloy)["ClusterRole/"+alloyRole]
	rules := lookup[[]any](t, role, "rules")
	resources := make([]string, 0, len(rules))
	for i := range rules {
		resources = append(resources, toStrings(t, lookup[[]any](t, role, "rules", i, "resources"))...)
	}
	slices.Sort(resources)
	if !slices.Equal(resources, []string{"events", "pods", "pods/log"}) {
		t.Errorf("the api collector reads %v", resources)
	}
}

func TestLogsFigureOnlyBesideMonitoring(t *testing.T) {
	t.Parallel()
	alone, err := addonsOn(cluster(variableOf("logs", `{"enabled":true}`)))
	if err != nil || slices.Contains(alone, logsAddon) {
		t.Errorf("logs without monitoring count: %v, %v", alone, err)
	}
	beside, err := addonsOn(cluster(variableOf("logs", `{"enabled":true}`), variableOf("monitoring", `{"enabled":true}`)))
	if err != nil || !slices.Contains(beside, logsAddon) {
		t.Errorf("logs beside monitoring do not count: %v, %v", beside, err)
	}
}

func TestReconcileSwitchesLogsAndTheirCollector(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables,
		variableOf("monitoring", `{"enabled":true}`), variableOf("logs", `{"enabled":true,"collector":"node"}`))
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	present := func(name string) bool {
		hcp := &unstructured.Unstructured{}
		hcp.SetGroupVersionKind(helmChartProxyGVK)
		return kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, hcp) == nil
	}
	set := func(raw string) {
		t.Helper()
		current := &clusterv1.Cluster{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), current); err != nil {
			t.Fatal(err)
		}
		current.Spec.Topology.Variables[len(current.Spec.Topology.Variables)-1].Value.Raw = []byte(raw)
		if err := kube.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		if err := reconcile(t, r, current); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"web-loki", "web-alloy", "web-alloy-node"} {
		if !present(name) {
			t.Errorf("logs on, no %s", name)
		}
	}
	set(`{"enabled":true,"collector":"api"}`)
	if present("web-alloy-node") || !present("web-alloy") || !present("web-loki") {
		t.Error("switching to the api collector did not leave only Loki and the one Alloy")
	}
	set(`{"enabled":false}`)
	for _, name := range []string{"web-loki", "web-alloy", "web-alloy-node"} {
		if present(name) {
			t.Errorf("logs off, %s is still there", name)
		}
	}
	if !present("web-monitoring") {
		t.Error("switching logs off took monitoring too")
	}
}
