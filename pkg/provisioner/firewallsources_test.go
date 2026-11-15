package provisioner

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// firewallCluster is a Kubehenge cluster named name, with the security groups given, and its
// ProxmoxCluster.
func firewallCluster(name, groups string) (*clusterv1.Cluster, *unstructured.Unstructured) {
	c := &clusterv1.Cluster{Name: name, Namespace: "team"}
	c.Spec.Topology.ClassRef = clusterv1.ClusterClassRef{Name: "kubehenge-kubeadm", Namespace: "kubehenge-system"}
	if groups != "" {
		c.Spec.Topology.Variables = []clusterv1.ClusterVariable{{Name: SecurityGroupsVariable, Value: apiextensionsv1.JSON{Raw: []byte(groups)}}}
	}
	pc := proxmoxClusterObject()
	pc.SetName(name + "-x1")
	pc.SetNamespace("team")
	pc.SetLabels(map[string]string{clusterv1.ClusterNameLabel: name})
	return c, pc
}

// The management cluster's nodes and the appliance's addresses reach the ProxmoxCluster of every
// cluster firewalled by its groups, and follow the appliance's ConfigMap when an address is added,
// changed or removed. A cluster without a firewall is left alone, and one whose groups go loses
// the annotation. A run that finds nothing changed writes nothing.
func TestFirewallSourcesFollowTheAppliance(t *testing.T) {
	t.Parallel()
	web, webPC := firewallCluster("web", `["k8s"]`)
	plain, plainPC := firewallCluster("plain", "")
	appliance := &corev1.ConfigMap{Name: ApplianceAddressesName, Namespace: "kubehenge-system"}
	patches := 0
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(web, plain, webPC, plainPC, appliance).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			return c.Patch(ctx, obj, patch, opts...)
		}}).Build()
	f := &FirewallSources{Client: kube, Reader: kube, Namespace: "kubehenge-system", Management: []string{"192.0.2.11", "2001:db8::11"}}
	held := func(pc *unstructured.Unstructured) (string, bool) {
		t.Helper()
		got := proxmoxClusterObject()
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pc), got); err != nil {
			t.Fatal(err)
		}
		v, ok := got.GetAnnotations()[FirewallSourcesAnnotation]
		return v, ok
	}
	setAppliance := func(addresses string) {
		t.Helper()
		appliance.Data = map[string]string{ApplianceAddressesKey: addresses}
		if err := kube.Update(t.Context(), appliance); err != nil {
			t.Fatal(err)
		}
	}

	for _, step := range []struct {
		name, appliance, want string
		patches               int
	}{
		{name: "the appliance has written nothing yet", want: "192.0.2.11,2001:db8::11", patches: 1},
		{name: "nothing changed", want: "192.0.2.11,2001:db8::11", patches: 1},
		{name: "the appliance's addresses", appliance: "192.0.2.5,2001:db8::5", want: "192.0.2.11,2001:db8::11,192.0.2.5,2001:db8::5", patches: 2},
		{name: "an IPv4 address renewed", appliance: "192.0.2.9,2001:db8::5", want: "192.0.2.11,2001:db8::11,192.0.2.9,2001:db8::5", patches: 3},
		{name: "an IPv6 address removed", appliance: "192.0.2.9", want: "192.0.2.11,2001:db8::11,192.0.2.9", patches: 4},
		{name: "an address a management node has too", appliance: "192.0.2.9,192.0.2.11", want: "192.0.2.11,2001:db8::11,192.0.2.9", patches: 4},
	} {
		if step.appliance != "" {
			setAppliance(step.appliance)
		}
		if err := f.runOnce(t.Context()); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got, ok := held(webPC); !ok || got != step.want {
			t.Errorf("%s: web's ProxmoxCluster holds %q (%v), want %q", step.name, got, ok, step.want)
		}
		if got, ok := held(plainPC); ok {
			t.Errorf("%s: the cluster without groups holds %q", step.name, got)
		}
		if patches != step.patches {
			t.Errorf("%s: %d writes, want %d", step.name, patches, step.patches)
		}
	}

	web.Spec.Topology.Variables = nil
	if err := kube.Update(t.Context(), web); err != nil {
		t.Fatal(err)
	}
	if err := f.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, ok := held(webPC); ok {
		t.Errorf("groups removed, the ProxmoxCluster still holds %q", got)
	}
}

// A cluster whose ProxmoxCluster is not made yet and one of another class are passed over without
// an error. The next run reaches a ProxmoxCluster made since.
func TestFirewallSourcesPassOverClustersNotReady(t *testing.T) {
	t.Parallel()
	early, earlyPC := firewallCluster("early", `["k8s"]`)
	foreign, foreignPC := firewallCluster("foreign", `["k8s"]`)
	foreign.Spec.Topology.ClassRef.Name = "someone-elses"
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(early, foreign, foreignPC).Build()
	f := &FirewallSources{Client: kube, Reader: kube, Namespace: "kubehenge-system", Management: []string{"192.0.2.11"}}
	if err := f.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(t.Context(), earlyPC); err != nil {
		t.Fatal(err)
	}
	if err := f.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	for pc, want := range map[*unstructured.Unstructured]bool{earlyPC: true, foreignPC: false} {
		got := proxmoxClusterObject()
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pc), got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got.GetAnnotations()[FirewallSourcesAnnotation]; ok != want {
			t.Errorf("%s annotated: %v, want %v", pc.GetName(), ok, want)
		}
	}
}

// addVariable sets a cluster's variable to raw.
func addVariable(c *clusterv1.Cluster, name, raw string) {
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(raw)}})
}

// annotationsOf reads a ProxmoxCluster's firewall annotations.
func annotationsOf(t *testing.T, kube client.Client, pc *unstructured.Unstructured) map[string]string {
	t.Helper()
	got := proxmoxClusterObject()
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(pc), got); err != nil {
		t.Fatal(err)
	}
	held := map[string]string{}
	for _, key := range []string{FirewallSourcesAnnotation, FirewallDestinationsAnnotation} {
		if v, ok := got.GetAnnotations()[key]; ok {
			held[key] = v
		}
	}
	return held
}

// A cluster with the firewall on by default gets the management side as its sources and its
// load-balancer addresses as its destinations, without any security group. Off, it gets neither.
func TestFirewallSourcesOfADefaultCluster(t *testing.T) {
	t.Parallel()
	on, onPC := firewallCluster("on", "")
	addVariable(on, FirewallVariable, `{"enabled": true}`)
	addVariable(on, loadBalancerVariable, `{"enabled": true, "addresses": ["10.0.0.30-10.0.0.40", "2001:db8::30/124"]}`)
	off, offPC := firewallCluster("off", "")
	addVariable(off, FirewallVariable, `{"enabled": false}`)
	addVariable(off, loadBalancerVariable, `{"enabled": true, "addresses": ["10.0.0.50"]}`)
	disabledLB, disabledLBPC := firewallCluster("nolb", "")
	addVariable(disabledLB, FirewallVariable, `{"enabled": true}`)
	addVariable(disabledLB, loadBalancerVariable, `{"enabled": false, "addresses": ["10.0.0.60"]}`)
	// With its serving ports closed, the load balancer is reached only through the groups.
	closed, closedPC := firewallCluster("closed", "")
	addVariable(closed, FirewallVariable, `{"enabled": true, "servingPorts": false}`)
	addVariable(closed, loadBalancerVariable, `{"enabled": true, "addresses": ["10.0.0.70"]}`)
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(on, onPC, off, offPC, disabledLB, disabledLBPC, closed, closedPC).Build()
	f := &FirewallSources{Client: kube, Reader: kube, Namespace: "kubehenge-system", Management: []string{"192.0.2.11", "2001:db8::11"}}
	if err := f.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	for pc, want := range map[*unstructured.Unstructured]map[string]string{
		onPC: {
			FirewallSourcesAnnotation:      "192.0.2.11,2001:db8::11",
			FirewallDestinationsAnnotation: "10.0.0.30-10.0.0.40,2001:db8::30/124",
		},
		offPC:        {},
		disabledLBPC: {FirewallSourcesAnnotation: "192.0.2.11,2001:db8::11"},
		closedPC:     {FirewallSourcesAnnotation: "192.0.2.11,2001:db8::11"},
	} {
		if got := annotationsOf(t, kube, pc); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", pc.GetName(), got, want)
		}
	}
}

// The nodes and pods of a cluster linked by Cluster Mesh reach a firewalled cluster's nodes. A
// cluster it lists that does not list it back adds nothing.
func TestFirewallSourcesLetInClusterMeshPeers(t *testing.T) {
	t.Parallel()
	mesh := func(name string, id int, endpoint, pods, peers string) (*clusterv1.Cluster, *unstructured.Unstructured) {
		c := meshCluster(name, id, endpoint, pods, `{"enabled": true, "peers": `+peers+`}`)
		c.Namespace = "team"
		addVariable(c, FirewallVariable, `{"enabled": true}`)
		_, pc := firewallCluster(name, "")
		pc.Object["spec"] = map[string]any{
			"controlPlaneEndpoint": map[string]any{"host": endpoint},
			"ipv4Config":           map[string]any{"addresses": []any{"10.0." + strconv.Itoa(id) + ".10-10.0." + strconv.Itoa(id) + ".20"}},
			"zoneConfigs":          []any{map[string]any{"ipv6Config": map[string]any{"addresses": []any{"2001:db8:" + strconv.Itoa(id) + "::/120"}}}},
		}
		return c, pc
	}
	a, aPC := mesh("a", 1, "10.0.1.5", "10.244.0.0/16", `["b", "c"]`)
	b, bPC := mesh("b", 2, "10.0.2.5", "10.245.0.0/16", `["a"]`)
	c, cPC := mesh("c", 3, "10.0.3.5", "10.246.0.0/16", `[]`)
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, aPC, b, bPC, c, cPC).Build()
	f := &FirewallSources{Client: kube, Reader: kube, Namespace: "kubehenge-system", Management: []string{"192.0.2.11"}}
	if err := f.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := annotationsOf(t, kube, aPC)[FirewallSourcesAnnotation], "192.0.2.11,10.0.2.10-10.0.2.20,10.0.2.5,10.245.0.0/16,2001:db8:2::/120"; got != want {
		t.Errorf("a's sources = %q, want %q", got, want)
	}
	if got, want := annotationsOf(t, kube, cPC)[FirewallSourcesAnnotation], "192.0.2.11"; got != want {
		t.Errorf("c's sources = %q, want %q", got, want)
	}
}

// A firewalled cluster says when Proxmox VE's datacenter firewall is off, and Unknown when that
// cannot be read. Once it is on, or the cluster's firewall is off, the condition goes.
func TestFirewallSourcesWarnWhileTheDatacenterFirewallIsOff(t *testing.T) {
	t.Parallel()
	on, onPC := firewallCluster("on", "")
	addVariable(on, FirewallVariable, `{"enabled": true}`)
	off, offPC := firewallCluster("off", "")
	addVariable(off, FirewallVariable, `{"enabled": false}`)
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(on, onPC, off, offPC).
		WithStatusSubresource(&clusterv1.Cluster{}).Build()
	reach := &fakeAPIReach{reach: &pve.APIReach{Enabled: false}, ok: true}
	recorder := events.NewFakeRecorder(10)
	f := &FirewallSources{Client: kube, Reader: kube, Namespace: "kubehenge-system", Datacenter: reach, Events: recorder}
	condition := func(c *clusterv1.Cluster) *metav1.Condition {
		t.Helper()
		got := &clusterv1.Cluster{}
		if err := kube.Get(t.Context(), client.ObjectKeyFromObject(c), got); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(got.Status.Conditions, FirewallInactiveCondition)
	}
	timeout := fmt.Errorf("reading the Proxmox VE firewall: %w", context.DeadlineExceeded)
	// What the controller's go-proxmox client gives for a 401 and a 403, as Proxmox.APIReach wraps it.
	revoked := fmt.Errorf("reading the Proxmox VE firewall: %w", &url.Error{Op: "Get", URL: "https://pve", Err: &pve.RefusedError{StatusCode: http.StatusUnauthorized}})
	denied := fmt.Errorf("reading the Proxmox VE firewall: %w", &url.Error{Op: "Get", URL: "https://pve", Err: &pve.RefusedError{StatusCode: http.StatusForbidden}})
	const sysAudit = "Kubehenge cannot read whether Proxmox VE's datacenter firewall is on, which needs Sys.Audit on /"
	for _, step := range []struct {
		name     string
		reach    *pve.APIReach
		readable bool
		err      error
		// runs is how many passes the step makes, two where zero: a second pass over the same
		// state records no second event.
		runs    int
		want    metav1.ConditionStatus
		reason  string
		message string
		events  int
	}{
		{name: "datacenter off", reach: &pve.APIReach{}, readable: true, want: metav1.ConditionTrue, reason: "DatacenterFirewallOff", message: FirewallInactiveMessage, events: 1},
		{name: "refused", want: metav1.ConditionUnknown, reason: "SysAuditMissing", message: sysAudit, events: 1},
		{
			name: "a 401", err: revoked, want: metav1.ConditionUnknown, reason: "TokenRefused", events: 1,
			message: "Proxmox VE refused Kubehenge's API token, which may have expired, been revoked or be wrong. Make a new token, or apply the management cluster again",
		},
		{name: "a 403", err: denied, want: metav1.ConditionUnknown, reason: "SysAuditMissing", message: sysAudit, events: 1},
		{name: "datacenter on", reach: &pve.APIReach{Enabled: true}, readable: true},
		{name: "a flaky read", err: timeout},
		{name: "datacenter on again", reach: &pve.APIReach{Enabled: true}, readable: true},
		{name: "datacenter off again", reach: &pve.APIReach{}, readable: true, want: metav1.ConditionTrue, reason: "DatacenterFirewallOff", message: FirewallInactiveMessage, events: 1},
		{name: "a flaky read while off", err: timeout, want: metav1.ConditionTrue, reason: "DatacenterFirewallOff", message: FirewallInactiveMessage},
		{name: "no answer", err: timeout, runs: unreachedRuns, want: metav1.ConditionUnknown, reason: "ProxmoxUnreachable", message: "Kubehenge cannot reach Proxmox VE to read whether its datacenter firewall is on", events: 1},
	} {
		reach.reach, reach.ok, reach.err = step.reach, step.readable, step.err
		runs := step.runs
		if runs == 0 {
			runs = 2
		}
		for range runs {
			if err := f.runOnce(t.Context()); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
		}
		got := condition(on)
		switch {
		case step.want == "" && got != nil:
			t.Errorf("%s: %+v, want none", step.name, got)
		case step.want != "" && (got == nil || got.Status != step.want || got.Reason != step.reason || got.Message != step.message):
			t.Errorf("%s: %+v, want %s for %s: %s", step.name, got, step.want, step.reason, step.message)
		}
		var recorded []string
		for len(recorder.Events) > 0 {
			recorded = append(recorded, <-recorder.Events)
		}
		if len(recorded) != step.events {
			t.Errorf("%s: events %q, want %d", step.name, recorded, step.events)
		}
		if len(recorded) == 1 && !strings.HasPrefix(recorded[0], "Warning "+FirewallInactiveCondition) {
			t.Errorf("%s: event %q", step.name, recorded[0])
		}
		if got := condition(off); got != nil {
			t.Errorf("%s: the cluster without a firewall has %+v", step.name, got)
		}
	}
}
