package provisioner

import (
	"context"
	"maps"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// The conditions on a Cluster that hold what an upgrade to its next minor version, and to the next
// patch of its minor version, would run into inside the cluster. The controller reads them on its
// schedule so that the admission webhook can warn without calling the cluster. Absent means
// nothing was found, or nothing was read yet.
const (
	UpgradeFindingsMinorCondition = "UpgradeFindingsNextMinor"
	UpgradeFindingsPatchCondition = "UpgradeFindingsNextPatch"
)

// UpgradeFound is the reason of the two conditions.
const UpgradeFound = "Found"

// findingsMessage is what the conditions carry: the target release, then one finding a line.
func findingsMessage(target string, lines []string) string {
	return target + "\n" + strings.Join(lines, "\n")
}

// recordChecks stores, as conditions on the cluster, what an upgrade to the next minor and the next
// patch would run into. What could not be read is left out, since it is no finding.
func (u *Upgrader) recordChecks(ctx context.Context, c *clusterv1.Cluster, channels map[string]Channels) {
	if u.Checker == nil || !Owns(c, u.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
		return
	}
	running, ok := parseKubeVersion(c.Spec.Topology.Version)
	if !ok {
		return
	}
	ch, err := u.channelsOf(ctx, c, channels)
	if err != nil {
		return
	}
	log := ctrl.LoggerFrom(ctx).WithName("upgrader")
	history, read := APIHistory{}, false
	defer func() {
		if read {
			u.keepHistory(ctx, c, history)
		}
	}()
	for _, tc := range []struct {
		condition string
		target    kubeVersion
		offered   bool
	}{
		{UpgradeFindingsMinorCondition, ch.Latest[running.Minor+1], ch.Latest[running.Minor+1].Raw != ""},
		{UpgradeFindingsPatchCondition, ch.Latest[running.Minor], running.less(ch.Latest[running.Minor])},
	} {
		var cond *metav1.Condition
		if tc.offered {
			found := u.Checker.Check(ctx, c, tc.target.Raw)
			if found.History != nil {
				maps.Copy(history, found.History)
				read = true
			}
			found.Unchecked = nil
			if lines := found.Lines(); len(lines) > 0 {
				cond = &metav1.Condition{Status: metav1.ConditionTrue, Reason: UpgradeFound, Message: findingsMessage(tc.target.Raw, lines)}
			}
		}
		if _, err := markCluster(ctx, u.Client, nil, client.ObjectKeyFromObject(c), tc.condition, cond); err != nil {
			log.Error(err, "recording what an upgrade would run into", "cluster", client.ObjectKeyFromObject(c))
		}
	}
}

// keepHistory stores the readings of the deprecated APIs' request counts on the cluster, which is
// how the next look tells an API still in use from one used once. A merge patch of the annotation
// alone leaves the rest of the Cluster, and the optimistic locks on it, as they are.
func (u *Upgrader) keepHistory(ctx context.Context, c *clusterv1.Cluster, history APIHistory) {
	have := c.Annotations[DeprecatedUseAnnotation]
	want := ""
	if len(history) > 0 {
		want = history.Encode()
	}
	if have == want {
		return
	}
	changed := c.DeepCopy()
	if want == "" {
		delete(changed.Annotations, DeprecatedUseAnnotation)
	} else {
		if changed.Annotations == nil {
			changed.Annotations = map[string]string{}
		}
		changed.Annotations[DeprecatedUseAnnotation] = want
	}
	if err := u.Client.Patch(ctx, changed, client.MergeFrom(c)); err != nil {
		ctrl.LoggerFrom(ctx).WithName("upgrader").Error(err, "keeping the deprecated APIs' request counts", "cluster", client.ObjectKeyFromObject(c))
	}
}

// upgradeWarnings are the stored findings for the version a change sets, one warning each, empty
// when the version does not change or nothing is stored for it. They are only as fresh as the last
// look of the controller, and never refuse the change.
func upgradeWarnings(old, c *clusterv1.Cluster) admission.Warnings {
	if old == nil || old.Spec.Topology.Version == c.Spec.Topology.Version {
		return nil
	}
	from, okFrom := parseKubeVersion(old.Spec.Topology.Version)
	to, okTo := parseKubeVersion(c.Spec.Topology.Version)
	if !okFrom || !okTo {
		return nil
	}
	kind := UpgradeFindingsPatchCondition
	switch {
	case to.Minor == from.Minor+1:
		kind = UpgradeFindingsMinorCondition
	case to.Minor != from.Minor:
		return nil
	}
	cond := meta.FindStatusCondition(old.Status.Conditions, kind)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return nil
	}
	target, rest, _ := strings.Cut(cond.Message, "\n")
	if stored, ok := parseKubeVersion(target); !ok || stored.Minor != to.Minor {
		return nil
	}
	var out admission.Warnings
	for line := range strings.SplitSeq(rest, "\n") {
		if line != "" {
			out = append(out, "Upgrading to "+c.Spec.Topology.Version+": "+line)
		}
	}
	return out
}
