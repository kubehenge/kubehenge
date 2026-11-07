package provisioner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const inventoryPool = "kubehenge/team-a.web"

func (f *vmFixture) inventory(guests string) *VMInventory {
	f.t.Helper()
	f.pve["/cluster/resources?type=vm"] = "[" + guests + "]"
	return &VMInventory{
		Client: f.kube, ClassNamespace: "kubehenge-system", WorkloadPool: "kubehenge", Proxmox: f.pve,
		Events: f.events, Now: func() time.Time { return f.now },
	}
}

func guestIn(vmid int, name, pool string) string {
	return fmt.Sprintf(`{"vmid":%d,"name":%q,"node":"pve1","type":"qemu","status":"running","pool":%q}`, vmid, name, pool)
}

func (f *vmFixture) missing() bool {
	f.t.Helper()
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), webKey, c); err != nil {
		f.t.Fatal(err)
	}
	cond := meta.FindStatusCondition(c.Status.Conditions, VMsMissingCondition)
	return cond != nil && cond.Status == "True"
}

func (f *vmFixture) unowned() []UnownedVM {
	f.t.Helper()
	cm := &corev1.ConfigMap{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: UnownedVMsName}, cm); err != nil {
		f.t.Fatal(err)
	}
	u, err := ParseUnownedVMs(cm)
	if err != nil {
		f.t.Fatal(err)
	}
	return u.VMs
}

func TestVMInventoryMarksAClusterWhoseVMsAreAllGone(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	other := guestIn(300, "unrelated", "elsewhere")
	i := f.inventory(guestIn(101, "web-cp-0", inventoryPool) + "," + other)
	if err := i.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.missing() {
		t.Fatal("one of the cluster's VMs exists, yet the cluster counts as missing its VMs")
	}
	i = f.inventory(other)
	if err := i.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.missing() {
		t.Fatal("no VM of the cluster exists, yet it is not marked")
	}
	i = f.inventory(guestIn(102, "web-cp-1", inventoryPool) + "," + other)
	if err := i.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.missing() {
		t.Fatal("the mark stays after a VM came back")
	}
}

func TestVMInventoryTrustsNothingFromABlindToken(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	i := f.inventory("")
	if err := i.Check(context.Background()); err == nil {
		t.Fatal("a list without a single VM was believed")
	}
	if f.missing() {
		t.Fatal("a blind token marked the cluster")
	}
}

func TestVMInventoryListsVMsNoClusterOwns(t *testing.T) {
	t.Parallel()
	f := newVMFixture(t)
	names, err := NamesFor("kubehenge", "team-a", "web")
	if err != nil {
		t.Fatal(err)
	}
	owned := []string{
		guestIn(101, "web-cp-0", inventoryPool),
		guestIn(150, names.VolumeOwner, inventoryPool),
	}
	stray := []string{
		guestIn(201, "old-cp-0", "kubehenge/team-b.gone"),
		guestIn(202, "stray", inventoryPool),
		guestIn(300, "template", "kubehenge-templates"),
	}
	i := f.inventory(strings.Join(slices.Concat(owned, stray), ","))
	if err := i.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	unowned := f.unowned()
	ids := make([]int64, 0, len(unowned))
	for _, vm := range unowned {
		ids = append(ids, vm.VMID)
	}
	if !slices.Equal(ids, []int64{201, 202}) {
		t.Fatalf("unowned VMs = %v, want [201 202]", ids)
	}
}
