package provisioner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// guestFiles answers a file read with the text it holds for the file, and counts the reads.
type guestFiles struct {
	files map[string]string
	reads int
}

func (g *guestFiles) ReadFile(_ context.Context, vmid int64, file string, _ int) (string, bool, error) {
	g.reads++
	text, ok := g.files[file]
	if !ok {
		return "", false, fmt.Errorf("VM %d: no %s", vmid, file)
	}
	return text, false, nil
}

func (*guestFiles) Running(context.Context, int64) (bool, error) { return true, nil }

// addressFixture is the VM fixture's worker web-md-0, VM 104, whose node has not joined, given the
// address 192.0.2.9 and created addressWait ago.
func addressFixture(t *testing.T) (*vmFixture, *guestFiles) {
	t.Helper()
	f := newVMFixture(t)
	files := &guestFiles{files: map[string]string{}}
	f.watch.Files = files
	m := f.machine("web-md-0")
	m.Status.Addresses = []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "192.0.2.9"}}
	if err := f.kube.Status().Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	f.cardHolds("192.0.2.9")
	return f, files
}

// cardHolds sets the addresses on VM 104's network cards, after the loopback's.
func (f *vmFixture) cardHolds(addresses ...string) {
	entries := make([]string, 0, len(addresses))
	for _, a := range addresses {
		entries = append(entries, fmt.Sprintf(`{"ip-address":%q,"ip-address-type":"ipv4","prefix":24}`, a))
	}
	f.pve["/nodes/pve1/qemu/104/agent/network-get-interfaces"] = `{"result":[{"name":"lo","ip-addresses":[{"ip-address":"127.0.0.1"}]},{"name":"eth0","hardware-address":"02:00:00:00:00:04","ip-addresses":[` +
		strings.Join(entries, ",") + `]}]}`
}

// addressCondition has the watch look once, after, and returns the worker's condition.
func (f *vmFixture) addressCondition(after time.Duration) *metav1.Condition {
	f.t.Helper()
	f.look(after, VMsCondition)
	return meta.FindStatusCondition(f.machine("web-md-0").Status.Conditions, AddressUnavailableCondition)
}

// A VM whose card has no address after it ran for a while shows it on its Machine, with the host
// that holds the address where the node's log names it, and a VM that has its address shows nothing.
func TestVMWatchSaysAVMHasNoAddress(t *testing.T) {
	t.Parallel()
	f, files := addressFixture(t)
	f.cardHolds("192.0.2.77")
	// The machine is younger than addressWait: nothing is asked.
	f.now = f.machine("web-md-0").CreationTimestamp.Add(addressWait - time.Second)
	if cond := f.addressCondition(0); cond != nil {
		t.Fatalf("a young machine: %+v", cond)
	}
	cond := f.addressCondition(time.Second)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != AddressMissingReason ||
		cond.Message != "192.0.2.9: the VM has no address after boot, so another host may hold it" {
		t.Fatalf("no address: %+v", cond)
	}
	if e := f.event(); !strings.HasPrefix(e, "Warning AddressUnavailable Machine web-md-0: 192.0.2.9:") {
		t.Errorf("event %q", e)
	}
	files.files[templates.BootLogFile] = "NetworkManager: IP address 192.0.2.9 cannot be configured because it is already in use in the network by host BC:24:11:10:8A:E7\n"
	cond = f.addressCondition(addressWait)
	if cond == nil || cond.Reason != AddressInUseReason || cond.Message != "192.0.2.9 is in use by another host (BC:24:11:10:8A:E7)" {
		t.Fatalf("the holder named: %+v", cond)
	}
	// The other host goes away and the VM takes its address: the condition goes.
	f.cardHolds("192.0.2.9")
	if cond := f.addressCondition(addressWait); cond != nil {
		t.Fatalf("the address is there: %+v", cond)
	}
}

// A VM that got its address is looked at once, and a machine whose node joined has no condition.
func TestVMWatchLooksAtAVMWithItsAddressOnce(t *testing.T) {
	t.Parallel()
	f, _ := addressFixture(t)
	asked := 0
	f.watch.Proxmox = countingProxmox{editingProxmox: f.edit, path: "/nodes/pve1/qemu/104/agent/network-get-interfaces", count: &asked}
	f.now = f.machine("web-md-0").CreationTimestamp.Add(addressWait)
	for range 5 {
		if cond := f.addressCondition(addressWait); cond != nil {
			t.Fatalf("a VM with its address: %+v", cond)
		}
	}
	if asked != 1 {
		t.Errorf("the guest agent was asked %d times, want once", asked)
	}
}

// A machine whose node joined loses the condition, and a guest agent that does not answer is asked
// again no sooner than addressWait.
func TestVMWatchClearsAndRetriesTheAddressCheck(t *testing.T) {
	t.Parallel()
	f, _ := addressFixture(t)
	f.cardHolds("192.0.2.77")
	f.now = f.machine("web-md-0").CreationTimestamp.Add(addressWait)
	if f.addressCondition(0) == nil {
		t.Fatal("no condition")
	}
	m := f.machine("web-md-0")
	m.Status.NodeRef = clusterv1.MachineNodeReference{Name: "web-md-0"}
	if err := f.kube.Status().Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if cond := f.addressCondition(time.Second); cond != nil {
		t.Fatalf("the node joined: %+v", cond)
	}

	g, _ := addressFixture(t)
	delete(g.pve, "/nodes/pve1/qemu/104/agent/network-get-interfaces")
	asked := 0
	g.watch.Proxmox = countingProxmox{editingProxmox: g.edit, path: "/nodes/pve1/qemu/104/agent/network-get-interfaces", count: &asked}
	g.now = g.machine("web-md-0").CreationTimestamp.Add(addressWait)
	for _, after := range []time.Duration{0, time.Minute, time.Minute} {
		if cond := g.addressCondition(after); cond != nil {
			t.Fatalf("an agent that does not answer: %+v", cond)
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times within addressWait, want once", asked)
	}
	g.cardHolds("192.0.2.77")
	if cond := g.addressCondition(addressWait); cond == nil {
		t.Error("not asked again after addressWait")
	}
}

// Without guest agent reads, and for a VM that is not running or has no address given, nothing is asked.
func TestVMWatchLeavesAddressesAloneWithoutGuestAgentReads(t *testing.T) {
	t.Parallel()
	f, _ := addressFixture(t)
	f.cardHolds("192.0.2.77")
	f.now = f.machine("web-md-0").CreationTimestamp.Add(addressWait)
	f.watch.Files = nil
	if cond := f.addressCondition(0); cond != nil {
		t.Fatalf("without Files: %+v", cond)
	}
	g, files := addressFixture(t)
	g.cardHolds("192.0.2.77")
	g.vms(map[int64]string{101: "running", 102: "running", 103: "running", 104: "stopped"})
	g.now = g.machine("web-md-0").CreationTimestamp.Add(addressWait)
	if cond := g.addressCondition(0); cond != nil {
		t.Fatalf("a stopped VM: %+v", cond)
	}
	h, _ := addressFixture(t)
	h.cardHolds("192.0.2.77")
	m := h.machine("web-md-0")
	m.Status.Addresses = nil
	if err := h.kube.Status().Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	h.now = m.CreationTimestamp.Add(addressWait)
	if cond := h.addressCondition(0); cond != nil {
		t.Fatalf("no address given: %+v", cond)
	}
	if files.reads != 0 {
		t.Errorf("read %d files", files.reads)
	}
}

// countingProxmox counts the reads of one path.
type countingProxmox struct {
	*editingProxmox

	path  string
	count *int
}

func (c countingProxmox) Get(ctx context.Context, path string, v any) error {
	if path == c.path {
		*c.count++
	}
	return c.editingProxmox.Get(ctx, path, v)
}
