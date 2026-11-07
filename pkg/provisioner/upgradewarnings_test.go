package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

var removedPSP = UpgradeFindings{RemovedAPIs: []RemovedAPI{{API: "policy/v1beta1 podsecuritypolicies", RemovedIn: "1.36"}}}

// The controller stores what an upgrade to the next minor and to the next patch would run into, as
// conditions, and takes them away once nothing is found. What could not be read is no finding.
func TestControllerStoresWhatTheNextUpgradesMeet(t *testing.T) {
	t.Parallel()
	c := settledCluster("web", "v1.35.4+rke2r1", nil)
	u, kube, _ := upgraderFixture(t, c)
	checker := &fakeChecker{found: removedPSP}
	u.Checker = checker

	u.RunOnce(context.Background())
	got := getCluster(t, kube, "web")
	minor := meta.FindStatusCondition(got.Status.Conditions, UpgradeFindingsMinorCondition)
	patch := meta.FindStatusCondition(got.Status.Conditions, UpgradeFindingsPatchCondition)
	if minor == nil || !strings.HasPrefix(minor.Message, "v1.36.5+rke2r1\nRemoved APIs still in use: policy/v1beta1 podsecuritypolicies") {
		t.Errorf("next minor: %+v", minor)
	}
	if patch == nil || !strings.HasPrefix(patch.Message, "v1.35.9+rke2r1\n") {
		t.Errorf("next patch: %+v", patch)
	}
	if !slices.Equal(checker.asked, []string{"v1.36.5+rke2r1", "v1.35.9+rke2r1"}) {
		t.Errorf("asked %v", checker.asked)
	}

	checker.found = UpgradeFindings{Unchecked: []string{"the deprecated APIs in use (403 Forbidden)"}}
	u.supportAt = u.supportAt.Add(-supportEvery)
	u.RunOnce(context.Background())
	got = getCluster(t, kube, "web")
	if meta.FindStatusCondition(got.Status.Conditions, UpgradeFindingsMinorCondition) != nil || meta.FindStatusCondition(got.Status.Conditions, UpgradeFindingsPatchCondition) != nil {
		t.Errorf("findings stayed for a check that could not be made: %+v", got.Status.Conditions)
	}
}

func withFindings(kind, target string) *clusterv1.Cluster {
	c := settledCluster("web", "v1.35.4+rke2r1", nil)
	if kind != "" {
		c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
			Type: kind, Status: metav1.ConditionTrue, Reason: UpgradeFound,
			Message: findingsMessage(target, []string{"Removed APIs still in use: a.", "The validating webhook w fails closed."}),
		})
	}
	return c
}

func movedTo(c *clusterv1.Cluster, version string) *clusterv1.Cluster {
	n := c.DeepCopy()
	n.Spec.Topology.Version = version
	return n
}

// A version change gets the stored findings of its step as warnings, and only that step's. A
// cluster without stored findings, a change of anything else and a target of another minor get none.
func TestUpgradeWarningsFollowTheVersionChange(t *testing.T) {
	t.Parallel()
	minor := withFindings(UpgradeFindingsMinorCondition, "v1.36.5+rke2r1")
	patch := withFindings(UpgradeFindingsPatchCondition, "v1.35.9+rke2r1")
	for _, tc := range []struct {
		name string
		old  *clusterv1.Cluster
		now  *clusterv1.Cluster
		want int
	}{
		{"the next minor", minor, movedTo(minor, "v1.36.1+rke2r1"), 2},
		{"the next patch", patch, movedTo(patch, "v1.35.6+rke2r1"), 2},
		{"a patch is not the minor's step", minor, movedTo(minor, "v1.35.6+rke2r1"), 0},
		{"a minor is not the patch's step", patch, movedTo(patch, "v1.36.1+rke2r1"), 0},
		{"two minors on", minor, movedTo(minor, "v1.37.1+rke2r1"), 0},
		{"findings of another minor", withFindings(UpgradeFindingsMinorCondition, "v1.37.1+rke2r1"), movedTo(minor, "v1.36.1+rke2r1"), 0},
		{"nothing stored", withFindings("", ""), movedTo(minor, "v1.36.1+rke2r1"), 0},
		{"version unchanged", minor, minor.DeepCopy(), 0},
		{"a new cluster", nil, minor, 0},
	} {
		got := upgradeWarnings(tc.old, tc.now)
		if len(got) != tc.want {
			t.Errorf("%s: %q, want %d warnings", tc.name, got, tc.want)
		}
	}
	got := upgradeWarnings(minor, movedTo(minor, "v1.36.1+rke2r1"))
	if got[0] != "Upgrading to v1.36.1+rke2r1: Removed APIs still in use: a." {
		t.Errorf("warning %q", got[0])
	}
}

// kubectl and GitOps hear what the stored findings say, and the change goes through.
func TestClusterValidatorWarnsOfStoredUpgradeFindings(t *testing.T) {
	t.Parallel()
	h, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: h}
	old := networkCluster("192.168.1.1", 16)
	running, ok := parseKubeVersion(old.Spec.Topology.Version)
	if !ok {
		t.Fatalf("version %q", old.Spec.Topology.Version)
	}
	target := fmt.Sprintf("v1.%d.1+rke2r1", running.Minor+1)
	old.Status.Conditions = append(old.Status.Conditions, metav1.Condition{
		Type: UpgradeFindingsMinorCondition, Status: metav1.ConditionTrue, Reason: UpgradeFound,
		Message: findingsMessage(target, []string{"Removed APIs still in use: a."}),
	})
	newer := movedTo(old, target)
	warnings, err := v.ValidateUpdate(context.Background(), old, newer)
	if err != nil {
		t.Fatalf("the change is refused: %v", err)
	}
	if !slices.Contains([]string(warnings), "Upgrading to "+target+": Removed APIs still in use: a.") {
		t.Errorf("warnings %q", warnings)
	}
	old.Status.Conditions = nil
	if warnings, err = v.ValidateUpdate(context.Background(), old, newer); err != nil || slices.ContainsFunc(warnings, func(w string) bool { return strings.HasPrefix(w, "Upgrading to") }) {
		t.Errorf("without stored findings: %q, %v", warnings, err)
	}
}
