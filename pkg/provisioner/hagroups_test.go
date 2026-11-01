package provisioner

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testSysAuditGroup = "kubehenge-clusters-audit"

// auditing asks for the cloud controller manager's HA group labels.
var auditing = variableOf(ccmVariable, `{"haGroups":true}`)

func TestSysAudit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		group   string
		want    bool
		refused string
	}{
		"not asking":            {storedCluster("team-a", "web", "ceph"), testSysAuditGroup, false, ""},
		"switched off":          {storedCluster("team-a", "web", "ceph", variableOf(ccmVariable, `{"haGroups":false}`)), "", false, ""},
		"asking":                {storedCluster("team-a", "web", "ceph", auditing), testSysAuditGroup, true, ""},
		"asking where none may": {storedCluster("team-a", "web", "ceph", auditing), "", false, "clusters may not hold Sys.Audit on /"},
	} {
		got, refused, err := sysAudit(tc.cluster, tc.group)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != tc.want || tc.refused == "" && refused != "" || !strings.Contains(refused, tc.refused) {
			t.Errorf("%s: %t, refused with %q; want %t, %q", name, got, refused, tc.want, tc.refused)
		}
	}
	if _, _, err := sysAudit(storedCluster("team-a", "web", "ceph", variableOf(ccmVariable, `"yes"`)), testSysAuditGroup); err == nil {
		t.Error("a ccm variable that is no object was read")
	}
}

func TestSysAuditRefusedAtAdmissionAndCreation(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "ceph", auditing)
	web.Spec.Topology.Workers.MachineDeployments = []clusterv1.MachineDeploymentTopology{{Class: "worker", Name: "md-0", Replicas: new(int32(1))}}
	h, _ := hooksFor(t, web)
	v := &ClusterValidator{Hooks: h}
	if _, err := v.ValidateCreate(context.Background(), web); err == nil || !strings.Contains(err.Error(), "clusters may not hold Sys.Audit on /") {
		t.Errorf("the validator let in a cluster asking for Sys.Audit on / where none may hold it: %v", err)
	}
	created := networkCluster("192.168.1.1", 24)
	created.Spec.Topology.Variables = append(created.Spec.Topology.Variables, auditing)
	if resp := callBeforeClusterCreate(t, h, created); resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "Sys.Audit on /") {
		t.Errorf("BeforeClusterCreate let Sys.Audit on / through where none may hold it: %s %q", resp.Status, resp.Message)
	}

	// A cluster that asked before its administrator withdrew the choice is not held to it for
	// unrelated changes, and may switch it off.
	again := web.DeepCopy()
	again.Spec.Topology.Variables = append(again.Spec.Topology.Variables, variableOf("headlamp", `{"enabled":true}`))
	if _, err := v.ValidateUpdate(context.Background(), web, again); err != nil {
		t.Errorf("an unrelated change was refused: %v", err)
	}
	off := storedCluster("team-a", "web", "ceph", variableOf(ccmVariable, `{"haGroups":false}`))
	off.Spec.Topology.Workers = web.Spec.Topology.Workers
	if _, err := v.ValidateUpdate(context.Background(), web, off); err != nil {
		t.Errorf("switching it off was refused: %v", err)
	}

	h.SysAuditGroup = testSysAuditGroup
	if _, err := v.ValidateCreate(context.Background(), web); err != nil {
		t.Errorf("refused where clusters may hold it: %v", err)
	}
	if resp := callBeforeClusterCreate(t, h, created); strings.Contains(resp.Message, "Sys.Audit") {
		t.Errorf("BeforeClusterCreate refused where clusters may hold it: %q", resp.Message)
	}
}

func TestReconcileGivesSysAuditAsTheCCMVariableAsks(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "ceph", auditing)
	cloud := &fakeCloud{}
	r, kube := newReconciler(t, cloud, web)
	r.SysAuditGroup = testSysAuditGroup
	recorder := events.NewFakeRecorder(10)
	r.Events = recorder
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if !cloud.access.SysAudit {
		t.Error("the cluster's user was not given Sys.Audit on /")
	}

	// Without the group, as when the administrator withdraws the choice, the user goes without and
	// the Cluster says why.
	r.SysAuditGroup = ""
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if cloud.access.SysAudit {
		t.Error("the cluster's user kept Sys.Audit on / without the group")
	}
	select {
	case e := <-recorder.Events:
		if !strings.Contains(e, "SysAuditRefused") || !strings.Contains(e, "Sys.Audit on /") {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("no event says why")
	}

	r.SysAuditGroup = testSysAuditGroup
	switched := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(web), switched); err != nil {
		t.Fatal(err)
	}
	switched.Spec.Topology.Variables = slices.DeleteFunc(switched.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool { return v.Name == ccmVariable })
	if err := kube.Update(context.Background(), switched); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, switched); err != nil {
		t.Fatal(err)
	}
	if cloud.access.SysAudit {
		t.Error("the cluster's user kept Sys.Audit on / after its ccm variable went")
	}
}

func TestUserJoinsTheSysAuditGroupWhileAccessAsks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakePVE("a", nil)
	f.userMissing = true
	p := &Proxmox{Client: serve(t, f), Settings: Settings{Group: "kubehenge-clusters", Role: "kubehenge-cluster", SysAuditGroup: testSysAuditGroup}}
	n := Names{Pool: "kubehenge/team-a.web", User: "kubehenge.team-a.web@pve"}

	// Created in both groups at once, then left as it is.
	for range 2 {
		if err := p.ensureUser(ctx, n, Access{SysAudit: true}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.userWrites) != 1 || f.userWrites[0]["groups"] != "kubehenge-clusters,"+testSysAuditGroup {
		t.Fatalf("writes %v", f.userWrites)
	}

	// Out again. The user's other groups stay.
	f.groups = append(f.groups, "operators")
	if err := p.ensureUser(ctx, n, Access{}); err != nil {
		t.Fatal(err)
	}
	if got := f.userWrites[len(f.userWrites)-1]; len(f.userWrites) != 2 || !maps.Equal(got, map[string]any{"groups": "kubehenge-clusters,operators"}) {
		t.Fatalf("writes %v", f.userWrites)
	}
	if !slices.Equal(f.groups, []string{"kubehenge-clusters", "operators"}) {
		t.Fatalf("groups %v", f.groups)
	}

	// Back in, appended so that the other groups stay.
	if err := p.ensureUser(ctx, n, Access{SysAudit: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.userWrites[len(f.userWrites)-1]; len(f.userWrites) != 3 || !maps.Equal(got, map[string]any{"groups": testSysAuditGroup, "append": float64(1)}) {
		t.Fatalf("writes %v", f.userWrites)
	}
	if !slices.Contains(f.groups, testSysAuditGroup) || !slices.Contains(f.groups, "operators") {
		t.Fatalf("groups %v", f.groups)
	}

	// Without the group nothing is changed, and Sys.Audit is refused.
	p.Settings.SysAuditGroup = ""
	if err := p.ensureUser(ctx, n, Access{}); err != nil || len(f.userWrites) != 3 {
		t.Errorf("without the group: %v, writes %v", err, f.userWrites)
	}
	if err := p.ensureUser(ctx, n, Access{SysAudit: true}); err == nil {
		t.Error("gave Sys.Audit without a group for it")
	}
}
