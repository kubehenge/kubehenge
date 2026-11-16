package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VMIDRangeVariable is the ClusterClass variable giving the VM IDs a cluster's VMs take, which the
// class hands to capmox as vmIDRange. Unset, Proxmox VE's next free ID is taken.
const VMIDRangeVariable = "vmIDRange"

// SiteVMIDRangeAnnotation on a class names the site's VM ID range as start-end. A new cluster that
// sets none takes it.
const SiteVMIDRangeAnnotation = "kubehenge.dev/vm-id-range"

// VMIDRangeFullCondition on a Cluster says that capmox found no free VM ID in the cluster's range.
const VMIDRangeFullCondition = "VMIDRangeFull"

// The VM IDs Proxmox VE accepts.
const (
	MinVMID = 100
	MaxVMID = 999_999_999
)

// capmoxNoFreeID is what capmox says on a ProxmoxMachine when its range holds no free ID.
const capmoxNoFreeID = "No free vmid found in vmIDRange"

// VMIDRange is the VM IDs a cluster's VMs and a site's templates take, both ends included, as
// capmox's vmIDRange.
type VMIDRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// ParseVMIDRange reads start-end, and refuses a range Check refuses.
func ParseVMIDRange(text string) (VMIDRange, error) {
	from, to, found := strings.Cut(strings.TrimSpace(text), "-")
	start, err1 := strconv.ParseInt(strings.TrimSpace(from), 10, 64)
	end, err2 := strconv.ParseInt(strings.TrimSpace(to), 10, 64)
	if !found || err1 != nil || err2 != nil {
		return VMIDRange{}, fmt.Errorf("VM ID range %q is not start-end. Write it like 10000-10999", text)
	}
	r := VMIDRange{Start: start, End: end}
	return r, r.Check()
}

func (r VMIDRange) String() string { return fmt.Sprintf("%d-%d", r.Start, r.End) }

// Size is how many IDs the range holds.
func (r VMIDRange) Size() int64 { return r.End - r.Start + 1 }

// Check refuses a range that starts above its end or reaches outside the IDs Proxmox VE accepts.
func (r VMIDRange) Check() error {
	switch {
	case r.Start > r.End:
		return fmt.Errorf("VM ID range %s starts above its end. Swap them", r)
	case r.Start < MinVMID || r.End > MaxVMID:
		return fmt.Errorf("VM ID range %s is outside %d to %d. Use IDs within it", r, MinVMID, MaxVMID)
	}
	return nil
}

// nodesAtMost is how many nodes the cluster runs at most: its control planes and each pool at its
// largest.
func nodesAtMost(c *clusterv1.Cluster) int64 {
	n := int64(1)
	if r := c.Spec.Topology.ControlPlane.Replicas; r != nil {
		n = int64(*r)
	}
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		n += int64(workerRange(&c.Spec.Topology.Workers.MachineDeployments[i]).most)
	}
	return n
}

// checkVMIDRange refuses a cluster's VM ID range that Check refuses, or that holds fewer IDs than the
// cluster's nodes and the one more a rolling upgrade has beside them.
func checkVMIDRange(c *clusterv1.Cluster) error {
	r, set, err := optionalVariable[VMIDRange](c, VMIDRangeVariable)
	if err != nil {
		return errors.New("VM ID range is start and end, whole numbers")
	}
	if !set {
		return nil
	}
	if err = r.Check(); err != nil {
		return err
	}
	if need := nodesAtMost(c) + 1; r.Size() < need {
		return fmt.Errorf("VM ID range %s holds %d IDs. The nodes need %d, one of them for an upgrade. Widen it", r, r.Size(), need)
	}
	return nil
}

// validateVMIDRange holds a cluster's VM ID range to checkVMIDRange when it is created or the range
// or the machines change, so that a cluster that already fell short is not held to it for unrelated
// edits.
func validateVMIDRange(old, c *clusterv1.Cluster) field.ErrorList {
	if old != nil && !variableChanged(old, c, VMIDRangeVariable) && !machinesChanged(old, c) {
		return nil
	}
	if err := checkVMIDRange(c); err != nil {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "topology", "variables").Key(VMIDRangeVariable), VMIDRangeVariable, err.Error())}
	}
	return nil
}

// giveSiteVMIDRange gives a new cluster that sets no VM ID range the site's, which its class names and
// has a variable for. Only a new cluster takes it: a range changed under running VMs would replace
// every node.
func (d *ClusterDefaulter) giveSiteVMIDRange(ctx context.Context, c *clusterv1.Cluster) error {
	if _, set, _ := optionalVariable[json.RawMessage](c, VMIDRangeVariable); set {
		return nil
	}
	class, err := d.Hooks.classOf(c)(ctx)
	if err != nil || class == nil {
		return err
	}
	site := class.Annotations[SiteVMIDRangeAnnotation]
	hasVariable := slices.ContainsFunc(class.Spec.Variables, func(v clusterv1.ClusterClassVariable) bool { return v.Name == VMIDRangeVariable })
	if site == "" || !hasVariable {
		return nil
	}
	r, err := ParseVMIDRange(site)
	if err != nil {
		return fmt.Errorf("the site's %w", err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding the VM ID range: %w", err)
	}
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: VMIDRangeVariable, Value: apiextensionsv1.JSON{Raw: raw}})
	return nil
}

// rangeIsFull tells whether capmox reports on one of the cluster's ProxmoxMachines that its VM ID
// range holds no free ID.
func rangeIsFull(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) (bool, error) {
	machines := proxmoxMachineList()
	if err := c.List(ctx, machines, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return false, fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	for i := range machines.Items {
		conditions, _, _ := unstructured.NestedSlice(machines.Items[i].Object, "status", "conditions")
		for _, entry := range conditions {
			cond, _ := entry.(map[string]any)
			if message, _ := cond["message"].(string); cond["status"] == string(metav1.ConditionFalse) && strings.Contains(message, capmoxNoFreeID) {
				return true, nil
			}
		}
	}
	return false, nil
}

// markVMIDRange records on the cluster that its VM ID range holds no free ID for a machine, and clears
// the record once capmox stops saying so.
func (p *Placer) markVMIDRange(ctx context.Context, c *clusterv1.Cluster) {
	log := ctrl.LoggerFrom(ctx).WithName("placer")
	r, set, err := optionalVariable[VMIDRange](c, VMIDRangeVariable)
	have := meta.FindStatusCondition(c.Status.Conditions, VMIDRangeFullCondition)
	if err != nil || !set {
		if have == nil {
			return
		}
	}
	var cond *metav1.Condition
	if set {
		full, ferr := rangeIsFull(ctx, p.Client, c)
		if ferr != nil {
			log.Error(ferr, "reading the cluster's VM ID range", "cluster", client.ObjectKeyFromObject(c))
			return
		}
		if full {
			cond = &metav1.Condition{
				Status: metav1.ConditionTrue, Reason: "NoFreeVMID",
				Message: fmt.Sprintf("No free VM ID in %s. Widen the cluster's VM ID range, or free IDs in it", r),
			}
		}
	}
	if cond == nil && have == nil || cond != nil && have != nil && have.Message == cond.Message {
		return
	}
	if _, err = markCluster(ctx, p.Client, p.Client, client.ObjectKeyFromObject(c), VMIDRangeFullCondition, cond); err != nil && !apierrors.IsNotFound(err) && ctx.Err() == nil {
		log.Error(err, "recording the full VM ID range", "cluster", client.ObjectKeyFromObject(c))
	}
}
