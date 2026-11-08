package appliance

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// addressesCheckEvery is how often the appliance looks at its own addresses. With the controller's
// own interval it bounds how long a renewed address waits to reach the clusters' firewalls.
const addressesCheckEvery = 15 * time.Second

// KeepAddresses records the appliance's own addresses in the management cluster when it starts and
// whenever one is added, removed or changed, as by a DHCP renewal, until ctx ends. The controller
// adds them to the sources of every cluster's firewall. The appliance is their one writer.
func (a *Appliance) KeepAddresses(ctx context.Context) {
	t := time.NewTicker(addressesCheckEvery)
	defer t.Stop()
	failing := ""
	for {
		failing = a.logAddressesOutcome(failing, a.recordAddresses(ctx, nil))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// logAddressesOutcome logs a failure to record the addresses once, not on every check while the
// management cluster stays unreachable, and logs the recovery. failing is the error logged last, or
// empty. It returns the one to pass next time.
func (a *Appliance) logAddressesOutcome(failing string, err error) string {
	switch {
	case err != nil && err.Error() != failing:
		a.Log.Error("recording the appliance's addresses in the management cluster", "error", err)
		return err.Error()
	case err != nil:
		return failing
	case failing != "":
		a.Log.Info("recording the appliance's addresses in the management cluster works again")
	}
	return ""
}

// recordAddresses writes the appliance's addresses into provisioner.ApplianceAddressesName where
// they differ from what it holds, through kube, or when nil, a client made from the interface's
// kubeconfig. Without a management cluster there is nothing to write to.
func (a *Appliance) recordAddresses(ctx context.Context, kube kubernetes.Interface) error {
	if !a.Ready() {
		return nil
	}
	own, err := a.addresses()
	if err != nil {
		return err
	}
	if kube == nil {
		if kube, err = a.interfaceClient(); err != nil {
			return err
		}
	}
	configMaps := kube.CoreV1().ConfigMaps(interfaceNamespace)
	held, err := configMaps.Get(ctx, provisioner.ApplianceAddressesName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading %s/%s: %w", interfaceNamespace, provisioner.ApplianceAddressesName, err)
	}
	want := strings.Join(own, ",")
	if got, ok := held.Data[provisioner.ApplianceAddressesKey]; ok && got == want {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{provisioner.ApplianceAddressesKey: want}})
	if err != nil {
		return fmt.Errorf("the appliance's addresses: %w", err)
	}
	if _, err := configMaps.Patch(ctx, provisioner.ApplianceAddressesName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("writing %s/%s: %w", interfaceNamespace, provisioner.ApplianceAddressesName, err)
	}
	a.Log.Info("recorded the appliance's addresses for the clusters' firewalls", "addresses", want)
	return nil
}

// OwnAddresses are the addresses the appliance reaches Proxmox VE from.
func (a *Appliance) OwnAddresses() ([]netip.Addr, error) {
	texts, err := a.addresses()
	if err != nil {
		return nil, err
	}
	addrs := make([]netip.Addr, 0, len(texts))
	for _, text := range texts {
		if ip, perr := netip.ParseAddr(text); perr == nil {
			addrs = append(addrs, ip)
		}
	}
	return addrs, nil
}

// addresses are the appliance's own addresses, which the workload clusters' firewalls accept: its
// interface and its Headlamp reach their API servers directly.
func (a *Appliance) addresses() ([]string, error) {
	if a.Addresses != nil {
		return a.Addresses()
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing the appliance's network interfaces: %w", err)
	}
	var addrs []net.Addr
	for i := range interfaces {
		if interfaces[i].Flags&net.FlagUp == 0 || interfaces[i].Flags&net.FlagLoopback != 0 {
			continue
		}
		own, err := interfaces[i].Addrs()
		if err != nil {
			return nil, fmt.Errorf("reading the addresses of %s: %w", interfaces[i].Name, err)
		}
		addrs = append(addrs, own...)
	}
	return sourceAddresses(addrs), nil
}

// sourceAddresses are the addresses among addrs that traffic to another network leaves from: no
// loopback or link-local ones. IPv4 come first, and each comes once.
func sourceAddresses(addrs []net.Addr) []string {
	// The module takes no null for a list.
	v4, v6 := []string{}, []string{}
	for _, addr := range addrs {
		n, ok := addr.(*net.IPNet)
		if !ok || !n.IP.IsGlobalUnicast() {
			continue
		}
		if ip := n.IP.To4(); ip != nil {
			if !slices.Contains(v4, ip.String()) {
				v4 = append(v4, ip.String())
			}
		} else if !slices.Contains(v6, n.IP.String()) {
			v6 = append(v6, n.IP.String())
		}
	}
	return append(v4, v6...)
}
