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

// ParseVMIDRange reads start-end, and refuses a range Check refuses. Every ID Proxmox VE accepts
// fits in 32 bits, so the parse stops at them and the IDs convert to int on any platform.
func ParseVMIDRange(text string) (VMIDRange, error) {
	from, to, found := strings.Cut(strings.TrimSpace(text), "-")
	start, err1 := strconv.ParseInt(strings.TrimSpace(from), 10, 32)
	end, err2 := strconv.ParseInt(strings.TrimSpace(to), 10, 32)
	if found && (errors.Is(err1, strconv.ErrRange) || errors.Is(err2, strconv.ErrRange)) {
		return VMIDRange{}, fmt.Errorf("VM ID range %s is outside %d to %d. Use IDs within it", strings.TrimSpace(text), MinVMID, MaxVMID)
	}
	if !found || err1 != nil || err2 != nil {
		return VMIDRange{}, fmt.Errorf("VM ID range %q is not start-end. Write it like 100000-100999", text)
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

// vmIDDemand is the IDs a cluster can hold at once: its nodes at most, and a spare for the control
// plane and for each pool, since each rolls one machine beyond its size at the same time as the
// others.
func vmIDDemand(c *clusterv1.Cluster) int64 {
	return nodesAtMost(c) + 1 + int64(len(c.Spec.Topology.Workers.MachineDeployments))
}

// checkVMIDRange refuses a cluster's VM ID range that Check refuses, or that holds fewer IDs than the
// cluster's demand.
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
	if need := vmIDDemand(c); r.Size() < need {
		return fmt.Errorf("VM ID range %s holds %d IDs. The nodes need %d, with a spare for the control plane and each pool to roll. Widen it", r, r.Size(), need)
	}
	return nil
}

// vmIDRangeChecked tells whether a change brings a cluster's VM ID range up for judging: its creation,
// or a change of the range or the machines, so that a cluster that already fell short is not held to
// it for unrelated edits.
func vmIDRangeChecked(old, c *clusterv1.Cluster) bool {
	return old == nil || variableChanged(old, c, VMIDRangeVariable) || machinesChanged(old, c)
}

// validateVMIDRange holds a cluster's VM ID range to what the cluster needs of it alone, then to what
// the others that share it leave.
func (v *ClusterValidator) validateVMIDRange(ctx context.Context, reads *pveReads, old, c *clusterv1.Cluster, class *clusterv1.ClusterClass) field.ErrorList {
	if !vmIDRangeChecked(old, c) {
		return nil
	}
	err := checkVMIDRange(c)
	if err == nil {
		err = v.checkSharedVMIDRange(ctx, reads, c, class)
	}
	if err == nil {
		return nil
	}
	return field.ErrorList{field.Invalid(field.NewPath("spec", "topology", "variables").Key(VMIDRangeVariable), VMIDRangeVariable, err.Error())}
}

func (r VMIDRange) overlaps(o VMIDRange) bool { return r.Start <= o.End && o.Start <= r.End }

// checkSharedVMIDRange refuses a range that overlaps the site's range or another cluster's without
// being the same, and one that, after the templates already in it, cannot hold the cluster and every
// other cluster on the same range. Sharing whole ranges keeps every cluster's IDs countable.
func (v *ClusterValidator) checkSharedVMIDRange(ctx context.Context, reads *pveReads, c *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	r, set, err := optionalVariable[VMIDRange](c, VMIDRangeVariable)
	if err != nil || !set || v.Hooks.Reader == nil {
		return nil //nolint:nilerr // checkVMIDRange has judged the variable
	}
	if site, parseErr := ParseVMIDRange(class.Annotations[SiteVMIDRangeAnnotation]); parseErr == nil && site != r && site.overlaps(r) {
		return fmt.Errorf("VM ID range %s overlaps the site's range %s without being the same. Use the site's range, or one that does not overlap it", r, site)
	}
	clusters := &clusterv1.ClusterList{}
	if err = v.Hooks.Reader.List(ctx, clusters); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "reading the clusters that share a VM ID range; admitting without that check")
		return nil
	}
	var others, sharing int64
	for i := range clusters.Items {
		o := &clusters.Items[i]
		if o.Namespace == c.Namespace && o.Name == c.Name || !Owns(o, v.Hooks.ClassNamespace) || !o.DeletionTimestamp.IsZero() {
			continue
		}
		theirs, ok, _ := optionalVariable[VMIDRange](o, VMIDRangeVariable)
		switch {
		case !ok:
		case theirs == r:
			others += vmIDDemand(o)
			sharing++
		case theirs.overlaps(r):
			return fmt.Errorf("VM ID range %s overlaps %s of cluster %s/%s without being the same. Use that range, or one that does not overlap it", r, theirs, o.Namespace, o.Name)
		}
	}
	templates := v.templatesIn(ctx, reads, r)
	if need, free := vmIDDemand(c)+others, r.Size()-templates; free < need {
		return fmt.Errorf("VM ID range %s has %d IDs free after %d templates. This cluster needs %d and the %d other clusters on it %d. Widen it",
			r, free, templates, vmIDDemand(c), sharing, others)
	}
	return nil
}

// templatesIn counts the templates that hold an ID of the range, none where Proxmox VE cannot be read.
func (v *ClusterValidator) templatesIn(ctx context.Context, reads *pveReads, r VMIDRange) int64 {
	if v.Hooks.Sites == nil {
		return 0
	}
	sites, err := reads.lookAtSites(ctx)
	if err != nil {
		return 0
	}
	var n int64
	for i := range sites.Templates {
		if id := int64(sites.Templates[i].VMID); id >= r.Start && id <= r.End {
			n++
		}
	}
	return n
}

// giveSiteVMIDRange gives a new cluster that sets no VM ID range the site's, which its class names and
// has a variable for. Only a new cluster takes it: a range changed under running VMs would replace
// every node.
func giveSiteVMIDRange(ctx context.Context, c *clusterv1.Cluster, classOf classReader) error {
	if _, set, _ := optionalVariable[json.RawMessage](c, VMIDRangeVariable); set {
		return nil
	}
	class, err := classOf(ctx)
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
			cond, ok := entry.(map[string]any)
			if !ok || cond["status"] != string(metav1.ConditionFalse) {
				continue
			}
			if message, isText := cond["message"].(string); isText && strings.Contains(message, capmoxNoFreeID) {
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
				Message: fmt.Sprintf("No free VM ID in %s, and the machine that asked stays failed. Widen the range, then delete the failed machine so Cluster API makes it again", r),
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
