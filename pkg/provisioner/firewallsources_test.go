package provisioner

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// addVariable sets a cluster's variable to raw.
func addVariable(c *clusterv1.Cluster, name, raw string) {
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(raw)}})
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
