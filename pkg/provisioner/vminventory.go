package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VMsMissingCondition is true on a Cluster none of whose machines has a VM in Proxmox VE any more,
// as after the management cluster was restored to a snapshot taken before the cluster was deleted.
const VMsMissingCondition = "VMsMissing"

// VMsMissingReason is the reason of VMsMissingCondition.
const VMsMissingReason = "NoVMs"

// UnownedVMsName is the ConfigMap, beside the ClusterClasses, in which the controller lists the VMs
// in Kubehenge's pools that no Cluster owns, under UnownedVMsKey as JSON.
const (
	UnownedVMsName = "kubehenge-unowned-vms"
	UnownedVMsKey  = "vms"
)

// UnownedVM is a VM in one of Kubehenge's pools that no Cluster owns.
type UnownedVM struct {
	VMID int64  `json:"vmid"`
	Name string `json:"name,omitempty"`
	Node string `json:"node,omitempty"`
	Pool string `json:"pool"`
}

// UnownedVMs is what the last look at Proxmox VE found.
type UnownedVMs struct {
	Checked time.Time   `json:"checked"`
	VMs     []UnownedVM `json:"vms"`
}

// ParseUnownedVMs reads the list out of the ConfigMap.
func ParseUnownedVMs(cm *corev1.ConfigMap) (UnownedVMs, error) {
	var u UnownedVMs
	if err := json.Unmarshal([]byte(cm.Data[UnownedVMsKey]), &u); err != nil {
		return UnownedVMs{}, fmt.Errorf("%s: %w", UnownedVMsName, err)
	}
	return u, nil
}

// VMsMissingMessage is the VMsMissing condition's message for a cluster whose n VMs are all gone,
// with what to do about it.
func VMsMissingMessage(n int) string {
	return fmt.Sprintf("None of the cluster's %d VMs exists in Proxmox VE. "+
		"Delete the cluster, or restore its VMs in Proxmox VE. Create it again from its exported spec if it should run.", n)
}

// inventoryGuest is a VM as Proxmox VE's resource list shows it, with the pool it is in.
type inventoryGuest struct {
	VMID int64  `json:"vmid"`
	Name string `json:"name"`
	Node string `json:"node"`
	Type string `json:"type"`
	Lock string `json:"lock"`
	Pool string `json:"pool"`
}

// VMInventory compares the cluster list with Proxmox VE: after a restore of the management
// cluster, which returns the list to an older day, and then daily. It marks a Cluster whose VMs are
// all gone and lists the VMs in Kubehenge's pools that no Cluster owns. It deletes nothing.
type VMInventory struct {
	Client         client.Client
	Reader         client.Reader
	ClassNamespace string
	// WorkloadPool is the pool the clusters' own pools nest under.
	WorkloadPool string
	Proxmox      proxmoxReader
	Events       events.EventRecorder
	Interval     time.Duration
	Now          func() time.Time
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*VMInventory) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. The controller starts again when the management cluster is
// restored, so the first look is the one after a restore.
func (v *VMInventory) Start(ctx context.Context) error {
	return every(ctx, v.Interval, func(ctx context.Context) {
		if err := v.Check(ctx); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).WithName("vm-inventory").Error(err, "comparing the clusters with Proxmox VE")
		}
	})
}

// Check looks once.
func (v *VMInventory) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	var list []inventoryGuest
	if err := v.Proxmox.Get(ctx, "/cluster/resources?type=vm", &list); err != nil {
		return fmt.Errorf("listing the VMs: %w", err)
	}
	guests := map[int64]*inventoryGuest{}
	for i := range list {
		if g := &list[i]; g.Type == qemuType {
			guests[g.VMID] = g
		}
	}
	// A token that sees no VM at all is blind rather than right, so nothing is said of it.
	if len(guests) == 0 {
		return errors.New("Proxmox VE lists no VMs to this token")
	}
	clusters := &clusterv1.ClusterList{}
	if err := v.Client.List(ctx, clusters); err != nil {
		return fmt.Errorf("listing the clusters: %w", err)
	}
	owned := map[int64]bool{}
	pools := map[string]bool{}
	kept := map[string]bool{}
	var errs []error
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if !Owns(c, v.ClassNamespace) {
			continue
		}
		ids, err := v.machineVMs(ctx, c)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, id := range ids {
			owned[id] = true
		}
		if names, err := NamesFor(v.WorkloadPool, c.Namespace, c.Name); err == nil {
			pools[names.Pool] = true
			kept[names.VolumeOwner], kept[names.VolumeHolders()] = true, true
		}
		errs = append(errs, v.markMissing(ctx, c, ids, guests))
	}
	errs = append(errs, v.publishUnowned(ctx, guests, owned, pools, kept))
	return errors.Join(errs...)
}

// machineVMs lists the VMs the cluster's ProxmoxMachines record.
func (v *VMInventory) machineVMs(ctx context.Context, c *clusterv1.Cluster) ([]int64, error) {
	pms := proxmoxMachineList()
	if err := v.Client.List(ctx, pms, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return nil, fmt.Errorf("listing the ProxmoxMachines of %s/%s: %w", c.Namespace, c.Name, err)
	}
	var ids []int64
	for i := range pms.Items {
		if id, found, _ := unstructured.NestedInt64(pms.Items[i].Object, "spec", "virtualMachineID"); found && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (v *VMInventory) markMissing(ctx context.Context, c *clusterv1.Cluster, ids []int64, guests map[int64]*inventoryGuest) error {
	if !c.DeletionTimestamp.IsZero() {
		return nil
	}
	var cond *metav1.Condition
	if len(ids) > 0 && !slices.ContainsFunc(ids, func(id int64) bool { _, ok := guests[id]; return ok }) {
		cond = &metav1.Condition{
			Status: metav1.ConditionTrue, Reason: VMsMissingReason,
			Message: VMsMissingMessage(len(ids)),
		}
	}
	w := &VMWatch{Client: v.Client, Reader: v.Reader, Events: v.Events}
	return w.mark(ctx, c, VMsMissingCondition, cond, "VMsFound", "the cluster's VMs exist in Proxmox VE again")
}

// publishUnowned records the VMs in a pool under the workload pool that belong to no Cluster: those
// of a pool no Cluster has, and those in a Cluster's pool that no machine and no volume holder
// names. A VM being created or cloned is left for the next look.
func (v *VMInventory) publishUnowned(ctx context.Context, guests map[int64]*inventoryGuest, owned map[int64]bool, pools, kept map[string]bool) error {
	out := UnownedVMs{Checked: v.now().UTC(), VMs: []UnownedVM{}}
	for _, g := range guests {
		if !strings.HasPrefix(g.Pool, v.WorkloadPool+"/") || owned[g.VMID] || slices.Contains(transientLocks, g.Lock) {
			continue
		}
		if pools[g.Pool] && kept[g.Name] {
			continue
		}
		out.VMs = append(out.VMs, UnownedVM{VMID: g.VMID, Name: g.Name, Node: g.Node, Pool: g.Pool})
	}
	slices.SortFunc(out.VMs, func(a, b UnownedVM) int { return int(a.VMID - b.VMID) })
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("encoding the unowned VMs: %w", err)
	}
	cm := &corev1.ConfigMap{Namespace: v.ClassNamespace, Name: UnownedVMsName}
	err = v.Client.Get(ctx, client.ObjectKeyFromObject(cm), cm)
	switch {
	case apierrors.IsNotFound(err):
		cm.Data = map[string]string{UnownedVMsKey: string(raw)}
		if err = v.Client.Create(ctx, cm); err != nil {
			return fmt.Errorf("recording the unowned VMs: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading the unowned VMs' record: %w", err)
	}
	cm.Data = map[string]string{UnownedVMsKey: string(raw)}
	if err = v.Client.Update(ctx, cm); err != nil {
		return fmt.Errorf("recording the unowned VMs: %w", err)
	}
	return nil
}

func (v *VMInventory) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}
