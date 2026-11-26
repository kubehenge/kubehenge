package provisioner

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// AddressUnavailableCondition is set on a Machine whose VM runs without the address Cluster API
// gave it, so that its node can never join: another host on the network holds the address, and the
// VM's network manager refuses it. It is True while that is so and absent otherwise.
const AddressUnavailableCondition = "AddressUnavailable"

// Reasons of AddressUnavailableCondition.
const (
	// AddressInUseReason is set where the node's own log names the host that holds the address.
	AddressInUseReason = "InUse"
	// AddressMissingReason is set where the network card shows no such address after boot.
	AddressMissingReason = "NoAddress"
)

const (
	// addressWait is how long a machine's VM runs, by Proxmox VE's uptime of it, without its node
	// joining before the card is looked at, and how long a look that found a problem or could not be
	// made waits to be made again. A look that finds the address is final.
	addressWait = 5 * time.Minute
	// duplicateLogRead bounds how much of cloud-init's output is read for the holder's address.
	duplicateLogRead = 1 << 20
)

// duplicateHost is what NetworkManager logs for an address another host answers for, with the
// hardware address of that host.
var duplicateHost = regexp.MustCompile(`already in use in the network by host ([0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5})`)

// addressLook is what the watch remembers of one machine's address check.
type addressLook struct {
	// found is set once the VM's card showed the address: nothing is asked of that VM again.
	found bool
	next  time.Time
}

// watchAddresses says on a machine whose VM has run for addressWait without its node joining that
// the address it was given is not on the VM's network card, which is what a duplicate address on
// the network does: the VM's network manager refuses it, and the node never joins, with nothing
// else saying why. The wait counts from the VM's start, as Proxmox VE's uptime gives it, so a
// controller restart neither shortens nor restarts it. A machine stays watched until its card shows
// its address or its node joins, a machine given no address yet included. The card is read through
// the guest agent once, and again every addressWait while the machine shows the problem or the
// agent did not answer. It is off where the controller's token may not read the guest agents
// (Files is nil).
func (w *VMWatch) watchAddresses(ctx context.Context, c *clusterv1.Cluster, look *vmLook) error {
	if w.Files == nil {
		return nil
	}
	machines, err := clusterMachines(ctx, w.Client, c)
	if err != nil {
		return err
	}
	pms, err := w.proxmoxMachines(ctx, c)
	if err != nil {
		return err
	}
	if w.addresses == nil {
		w.addresses = map[types.NamespacedName]addressLook{}
	}
	var errs []error
	for i := range machines {
		errs = append(errs, w.watchMachineAddress(ctx, c, &machines[i], pms[machines[i].Spec.InfrastructureRef.Name], look))
	}
	return errors.Join(errs...)
}

// watchMachineAddress is watchAddresses for one machine and its ProxmoxMachine, which may be nil.
func (w *VMWatch) watchMachineAddress(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine, pm *unstructured.Unstructured, look *vmLook) error {
	key := client.ObjectKeyFromObject(m)
	cond := meta.FindStatusCondition(m.Status.Conditions, AddressUnavailableCondition)
	if m.Status.NodeRef.Name != "" {
		delete(w.addresses, key)
		if cond == nil {
			return nil
		}
		return w.markAddress(ctx, c, m, nil)
	}
	if pm == nil {
		return nil
	}
	vmid, _, _ := unstructured.NestedInt64(pm.Object, "spec", "virtualMachineID")
	guest, listed := look.guests[vmid]
	seen := w.addresses[key]
	if !listed || guest.Status != guestRunning || time.Duration(guest.Uptime)*time.Second < addressWait || seen.found || w.now().Before(seen.next) {
		return nil
	}
	expected := expectedAddresses(m, pm)
	if len(expected) == 0 {
		// Cluster API sets the addresses after the VM starts: the machine is looked at once it has them.
		return nil
	}
	problem, found, err := w.checkAddress(ctx, guest.Node, vmid, expected)
	w.addresses[key] = addressLook{found: found, next: w.now().Add(addressWait)}
	if err != nil {
		ctrl.LoggerFrom(ctx).WithName("vm-watch").Info("could not read a VM's network card", "machine", m.Name, "error", err.Error())
		return nil
	}
	if problem == nil && cond == nil {
		return nil
	}
	return w.markAddress(ctx, c, m, problem)
}

// checkAddress reads the VM's addresses through its guest agent and compares them to the ones the
// machine was given. It returns the condition for an address the card lacks, or whether every
// address is there.
func (w *VMWatch) checkAddress(ctx context.Context, node string, vmid int64, expected []netip.Addr) (*metav1.Condition, bool, error) {
	have, err := w.guestAddresses(ctx, node, vmid)
	if err != nil {
		return nil, false, err
	}
	for _, a := range expected {
		if slices.Contains(have, a) {
			continue
		}
		if mac := w.holderOf(ctx, vmid); mac != "" {
			return &metav1.Condition{
				Status: metav1.ConditionTrue, Reason: AddressInUseReason,
				Message: fmt.Sprintf("%s is in use by another host (%s)", a, mac),
			}, false, nil
		}
		return &metav1.Condition{
			Status: metav1.ConditionTrue, Reason: AddressMissingReason,
			Message: fmt.Sprintf("%s: the VM has no address after boot, so another host may hold it", a),
		}, false, nil
	}
	return nil, true, nil
}

// guestInterfaces is the guest agent's answer to network-get-interfaces, as far as the addresses go.
type guestInterfaces struct {
	Result []guestCard `json:"result"`
}

type guestCard struct {
	IPAddresses []guestIP `json:"ip-addresses"`
}

type guestIP struct {
	IPAddress string `json:"ip-address"`
}

// guestAddresses are the addresses on the VM's network cards, as its guest agent reports them.
func (w *VMWatch) guestAddresses(ctx context.Context, node string, vmid int64) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, watchReadTimeout)
	defer cancel()
	var out guestInterfaces
	if err := w.Proxmox.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", node, vmid), &out); err != nil {
		return nil, fmt.Errorf("asking VM %d's guest agent for its addresses: %w", vmid, err)
	}
	var have []netip.Addr
	for _, card := range out.Result {
		for _, a := range card.IPAddresses {
			if ip, err := netip.ParseAddr(a.IPAddress); err == nil {
				have = append(have, ip.Unmap())
			}
		}
	}
	return have, nil
}

// holderOf is the hardware address of the host the VM's network manager says holds its address, as
// the node's boot log or cloud-init's output has it, or empty.
func (w *VMWatch) holderOf(ctx context.Context, vmid int64) string {
	for file, limit := range map[string]int{templates.BootLogFile: templates.BootLogRead, templates.CloudInitOutput: duplicateLogRead} {
		text, _, err := w.Files.ReadFile(ctx, vmid, file, limit)
		if err != nil {
			continue
		}
		if found := duplicateHost.FindStringSubmatch(text); found != nil {
			return found[1]
		}
	}
	return ""
}

// expectedAddresses are the addresses Cluster API gave the machine: those on the Machine and on its
// ProxmoxMachine, without link-local ones.
func expectedAddresses(m *clusterv1.Machine, pm *unstructured.Unstructured) []netip.Addr {
	var texts []string
	for _, a := range m.Status.Addresses {
		if a.Type == clusterv1.MachineInternalIP {
			texts = append(texts, a.Address)
		}
	}
	listed, _, _ := unstructured.NestedSlice(pm.Object, "status", "addresses")
	for _, l := range listed {
		entry, ok := l.(map[string]any)
		if !ok || entry["type"] != string(clusterv1.MachineInternalIP) {
			continue
		}
		if text, ok := entry["address"].(string); ok {
			texts = append(texts, text)
		}
	}
	var out []netip.Addr
	for _, text := range texts {
		ip, err := netip.ParseAddr(text)
		if err == nil && ip.Zone() == "" && !ip.IsLinkLocalUnicast() && !slices.Contains(out, ip.Unmap()) {
			out = append(out, ip.Unmap())
		}
	}
	return out
}

// markAddress sets AddressUnavailableCondition on the machine, or takes it off when cond is nil,
// with a Warning event on the cluster when it starts.
func (w *VMWatch) markAddress(ctx context.Context, c *clusterv1.Cluster, m *clusterv1.Machine, cond *metav1.Condition) error {
	before := m.DeepCopy()
	if cond == nil {
		meta.RemoveStatusCondition(&m.Status.Conditions, AddressUnavailableCondition)
	} else {
		set := *cond
		set.Type = AddressUnavailableCondition
		meta.SetStatusCondition(&m.Status.Conditions, set)
	}
	if equalConditions(before.Status.Conditions, m.Status.Conditions) {
		return nil
	}
	if err := w.Client.Status().Patch(ctx, m, client.MergeFrom(before)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("saying that machine %s has no address: %w", m.Name, err)
	}
	if cond != nil && meta.FindStatusCondition(before.Status.Conditions, AddressUnavailableCondition) == nil && w.Events != nil {
		w.Events.Eventf(c, nil, corev1.EventTypeWarning, AddressUnavailableCondition, "WatchVMs", "Machine %s: %s", m.Name, shorten(cond.Message, watchEventMax))
	}
	return nil
}

// proxmoxMachines are the cluster's ProxmoxMachines by name.
func (w *VMWatch) proxmoxMachines(ctx context.Context, c *clusterv1.Cluster) (map[string]*unstructured.Unstructured, error) {
	list := proxmoxMachineList()
	if err := w.Client.List(ctx, list, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		return nil, fmt.Errorf("listing the cluster's ProxmoxMachines: %w", err)
	}
	byName := make(map[string]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		byName[list.Items[i].GetName()] = &list.Items[i]
	}
	return byName, nil
}
