//go:build integration

package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/test/integration"
)

// interfaceNamespace is the chart's interface.namespace.
const interfaceNamespace = "clusters"

// startInterface starts an API server with Cluster API's webhooks and ClusterClass controller and
// Kubehenge's cluster admission, installs the charts, and points the interface at it as the
// interface's own account, which the chart's RBAC holds to what the appliance's holds to.
func startInterface(t *testing.T) (*harness, client.Client) {
	t.Helper()
	// The package's one integration test starts no process before this one.
	if err := integration.OwnChildren(); err != nil {
		t.Fatal(err)
	}
	objects, webhooks := integration.Charts(t, "../..", "../provisioner/testdata/clusterapi-values.yaml")
	api := integration.Start(t, &integration.Options{
		Objects: objects, Webhooks: webhooks, ClusterAPI: true,
		Namespaces: []string{integration.ClassNamespace, interfaceNamespace},
		Serve: func(c client.Client) map[string]http.Handler {
			hooks := &provisioner.Hooks{Reader: c, ClassNamespace: integration.ClassNamespace, RancherKubernetes: provisioner.KubernetesMinors{Min: 34, Max: 37}}
			return provisioner.AdmissionWebhooks(c.Scheme(), hooks)
		},
	})
	as := rest.CopyConfig(api.Config)
	as.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + integration.ClassNamespace + ":kubehenge-interface",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + integration.ClassNamespace, "system:authenticated"},
	}
	kube, err := client.New(as, client.Options{Scheme: integration.Scheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.server.Namespace = interfaceNamespace
	h.server.setKube(kube)
	h.kube = api.Client
	return h, api.Client
}

// createRequest is the body the interface's create form sends for a cluster of class on a network
// of its own, 10.1.<net>.0/24, with variables on top as JSON members.
func createRequestBody(name, class, version string, net int, variables string) string {
	vars := fmt.Sprintf(`"controlPlaneEndpoint":"10.1.%d.5","storage":"local-lvm",`+
		`"network":{"bridge":"vmbr0","addresses":["10.1.%d.10-10.1.%d.30"],"prefix":24,"gateway":"10.1.%d.1","dnsServers":["10.1.%d.1"]}`,
		net, net, net, net, net)
	if variables != "" {
		vars += "," + variables
	}
	return fmt.Sprintf(`{"name":%q,"class":%q,"version":%q,"controlPlaneReplicas":1,"workerReplicas":1,"variables":{%s}}`, name, class, version, vars)
}

func rangesOf(c *clusterv1.Cluster) string {
	return strings.Join(c.Spec.ClusterNetwork.Pods.CIDRBlocks, ",") + " " + strings.Join(c.Spec.ClusterNetwork.Services.CIDRBlocks, ",")
}

// expectRefused checks that the interface answers a request with the management cluster's own
// reason, never as a management cluster that did not answer.
func expectRefused(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
		t.Errorf("status %d, %s; want 422 saying %q", rec.Code, rec.Body, want)
	}
}

// TestIntegrationInterfaceRequests sends the interface's requests to a real API server as the
// interface's own account, with Cluster API's admission and Kubehenge's beside it: a cluster of
// every class is created as the create form sends it and stored with Cluster API's defaults, and
// what the management cluster refuses (a class's CEL rule, its schema, Kubehenge's own checks, a
// rule on changes) reaches the user as the reason, in a change and its dry-run plan alike, from the
// first cluster of a class on.
func TestIntegrationInterfaceRequests(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	h, admin := startInterface(t)
	ctx := context.Background()
	b := h.browser()
	b.signIn("admin@pve")

	// The first cluster of a class comes before Cluster API has passed the class.
	expectRefused(t, b.do("POST", "/api/clusters", createRequestBody("first", "kubehenge-k3s", "v1.36.5+k3s1", 1,
		`"gitops":{"enabled":true,"auth":"deployKey","repository":{"url":"https://git.example.org/team/cluster.git"}}`)),
		"A deploy key reads the repository over ssh://.")

	for _, class := range []string{"kubehenge-kubeadm", "kubehenge-rke2", "kubehenge-k3s", "kubehenge-talos"} {
		integration.ApplyClassTemplates(t, admin, integration.ClassNamespace, class, "templates.yaml")
		integration.WaitReconciled(t, admin, integration.ClassNamespace, class)
	}

	// The controller's admission gives each its distribution's own ranges, as it does kubectl's.
	for i, tc := range []struct{ class, version, vars, ranges string }{
		{"kubehenge-kubeadm", "v1.36.5", `"cni":"cilium"`, "10.244.0.0/16 10.96.0.0/12"},
		{"kubehenge-rke2", "v1.36.5+rke2r1", "", "10.42.0.0/16 10.43.0.0/16"},
		{"kubehenge-k3s", "v1.36.5+k3s1", `"nodeOS":{"family":"rocky","release":"10"}`, "10.42.0.0/16 10.43.0.0/16"},
		{"kubehenge-talos", "v1.36.5", `"talosVersion":"v1.14.5"`, "10.244.0.0/16 10.96.0.0/12"},
	} {
		name := strings.TrimPrefix(tc.class, "kubehenge-")
		rec := b.do("POST", "/api/clusters", createRequestBody(name, tc.class, tc.version, 10+i, tc.vars))
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", tc.class, rec.Code, rec.Body)
		}
		got := &clusterv1.Cluster{}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: interfaceNamespace, Name: name}, got); err != nil {
			t.Fatal(err)
		}
		defaulted := map[string]bool{}
		for _, v := range got.Spec.Topology.Variables {
			defaulted[v.Name] = true
		}
		if !defaulted["cni"] || !defaulted["cis"] || !defaulted["controlPlaneMachine"] || (tc.class != "kubehenge-talos" && !defaulted["nodeOS"]) {
			t.Errorf("%s was stored without Cluster API's defaults: %v", tc.class, defaulted)
		}
		if got := rangesOf(got); got != tc.ranges {
			t.Errorf("%s got ranges %s, want %s", tc.class, got, tc.ranges)
		}
	}

	addons := `"workerMachine":{"cores":8,"memoryMiB":32768},"controlPlaneMachine":{"cores":8,"memoryMiB":16384},` +
		`"cni":"cilium","headlamp":{"enabled":true},"monitoring":{"enabled":true},"logs":{"enabled":true},` +
		`"backups":{"enabled":true,"target":"store"},"etcdSnapshots":{"enabled":true,"target":"store"},` +
		`"podSecurity":{"admission":"restricted","kyverno":"baseline"},"istio":{"enabled":true},"clusterMesh":{"enabled":true},` +
		`"loadBalancer":{"enabled":true,"addresses":["10.1.20.100-10.1.20.110"]},` +
		`"certificates":{"enabled":true,"issuer":"ca","domain":"apps.example.org"},` +
		`"gitops":{"enabled":true,"engine":"flux","repository":{"url":"https://git.example.org/team/cluster.git"}}`
	if rec := b.do("POST", "/api/clusters", createRequestBody("everything", "kubehenge-k3s", "v1.36.5+k3s1", 20, addons)); rec.Code != http.StatusCreated {
		t.Fatalf("every add-on: %d %s", rec.Code, rec.Body)
	}
	everything := &clusterv1.Cluster{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: interfaceNamespace, Name: "everything"}, everything); err != nil {
		t.Fatal(err)
	}
	// A Cilium cluster keeps its pods clear of the four before's pods and services.
	if got := rangesOf(everything); got != "172.21.0.0/16 10.43.0.0/16" {
		t.Errorf("a Cilium cluster beside the others got ranges %s", got)
	}

	for _, tc := range []struct{ name, vars, want string }{
		{"rule", `"backups":{"enabled":true}`, "Backups need a target."},
		{"schema", `"cni":"weave"`, `spec.topology.variables[cni].value: Unsupported value`},
		{"kubehenge", `"clusterMesh":{"enabled":true}`, "Cluster Mesh needs Cilium"},
		{"nodeos", `"nodeOS":{"family":"rocky","release":"9"}`, "node OS"},
	} {
		expectRefused(t, b.do("POST", "/api/clusters", createRequestBody("refused-"+tc.name, "kubehenge-k3s", "v1.36.5+k3s1", 30, tc.vars)), tc.want)
	}
	overlapping := strings.Replace(createRequestBody("refused-ranges", "kubehenge-k3s", "v1.36.5+k3s1", 30, ""),
		`"controlPlaneReplicas"`, `"pods":["172.30.0.0/16"],"services":["172.30.128.0/17"],"controlPlaneReplicas"`, 1)
	expectRefused(t, b.do("POST", "/api/clusters", overlapping), "the service range 172.30.128.0/17 overlaps the pod range 172.30.0.0/16")

	for _, tc := range []struct{ body, want string }{
		{`{"variables":{"cni":"flannel"}}`, "cni cannot change after the cluster is created"},
		{`{"variables":{"logs":{"enabled":true,"sizeGiB":5}}}`, "Loki's volume only grows."},
		{`{"variables":{"gitops":{"enabled":true,"engine":"flux","auth":"deployKey","repository":{"url":"https://git.example.org/team/cluster.git"}}}}`, "A deploy key reads the repository over ssh://."},
	} {
		expectRefused(t, b.do("POST", "/api/clusters/"+interfaceNamespace+"/everything/plan", tc.body), tc.want)
		expectRefused(t, b.do("PATCH", "/api/clusters/"+interfaceNamespace+"/everything", tc.body), tc.want)
	}
	expect(t, b.do("POST", "/api/clusters/"+interfaceNamespace+"/everything/plan", `{"workerPools":{"md-0":2}}`), http.StatusOK)
	expect(t, b.do("PATCH", "/api/clusters/"+interfaceNamespace+"/everything", `{"workerPools":{"md-0":2},"variables":{"logs":{"enabled":true,"sizeGiB":20}}}`), http.StatusOK)
	expect(t, b.do("DELETE", "/api/clusters/"+interfaceNamespace+"/everything", `{"confirm":"everything"}`), http.StatusAccepted)
}
