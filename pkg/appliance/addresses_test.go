package appliance

import (
	"errors"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestAddressesFailureLoggedOnceUntilItRecovers(t *testing.T) {
	t.Parallel()
	var logged strings.Builder
	a := &Appliance{Log: slog.New(slog.NewTextHandler(&logged, nil))}
	down := errors.New("connection refused")
	failing := ""
	for _, err := range []error{down, down, down, nil, nil, down, errors.New("forbidden"), nil} {
		failing = a.logAddressesOutcome(failing, err)
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	want := []string{"connection refused", "works again", "connection refused", "forbidden", "works again"}
	if len(lines) != len(want) {
		t.Fatalf("logged %d lines, want %d:\n%s", len(lines), len(want), logged.String())
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Errorf("line %d = %q, want it to hold %q", i, lines[i], w)
		}
	}
}

// Go lists a machine's interfaces and their addresses over a netlink socket, which the interface's
// unit must allow.
func TestServiceUnitAllowsNetlink(t *testing.T) {
	t.Parallel()
	unit, err := os.ReadFile(filepath.Join("..", "..", "appliance", "kubehenge.service"))
	if err != nil {
		t.Fatal(err)
	}
	var families []string
	for line := range strings.Lines(string(unit)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "RestrictAddressFamilies="); ok {
			families = append(families, strings.Fields(value)...)
		}
	}
	if !slices.Contains(families, "AF_NETLINK") {
		t.Errorf("kubehenge.service restricts the address families to %q, without AF_NETLINK", families)
	}
}

func TestSourceAddressesLeaveOutLoopbackAndLinkLocal(t *testing.T) {
	t.Parallel()
	addr := func(cidr string) net.Addr {
		ip, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	got := sourceAddresses([]net.Addr{
		addr("2001:db8::5/64"),
		addr("127.0.0.1/8"),
		addr("::1/128"),
		addr("fe80::1/64"),
		addr("169.254.10.1/16"),
		addr("192.0.2.5/24"),
		addr("10.0.0.5/8"),
		addr("192.0.2.5/24"),
		&net.IPAddr{IP: net.ParseIP("198.51.100.1")},
	})
	if want := []string{"192.0.2.5", "10.0.0.5", "2001:db8::5"}; !slices.Equal(got, want) {
		t.Errorf("sourceAddresses = %q, want %q", got, want)
	}
	if got := sourceAddresses(nil); got == nil || len(got) != 0 {
		t.Errorf("no addresses = %#v, want an empty list for the module", got)
	}
}

// The appliance records its addresses in the ConfigMap the chart makes, writing only when they
// differ from what it holds: an address added, changed or removed, on IPv4 and IPv6. A restart
// with the same addresses writes nothing, and neither does a check that finds no change.
func TestRecordAddresses(t *testing.T) {
	t.Parallel()
	a := &Appliance{Dir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	own := []string{"192.0.2.5"}
	a.Addresses = func() (map[string][]string, error) { return map[string][]string{"eth0": slices.Clone(own)}, nil }
	kube := fake.NewClientset(&corev1.ConfigMap{Name: provisioner.ApplianceAddressesName, Namespace: interfaceNamespace})
	writes := func() int {
		n := 0
		for _, action := range kube.Actions() {
			if action.GetVerb() == "patch" {
				n++
			}
		}
		return n
	}
	held := func() string {
		cm, err := kube.CoreV1().ConfigMaps(interfaceNamespace).Get(t.Context(), provisioner.ApplianceAddressesName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return cm.Data[provisioner.ApplianceAddressesKey]
	}

	if err := a.recordAddresses(t.Context(), kube); err != nil || len(kube.Actions()) != 0 {
		t.Fatalf("without a management cluster: %v, %d calls", err, len(kube.Actions()))
	}
	if err := os.MkdirAll(filepath.Dir(a.KubeconfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.KubeconfigPath(), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		name   string
		own    []string
		held   string
		writes int
	}{
		{name: "first start", own: []string{"192.0.2.5"}, held: "192.0.2.5", writes: 1},
		{name: "no change", own: []string{"192.0.2.5"}, held: "192.0.2.5", writes: 1},
		{name: "IPv6 added", own: []string{"192.0.2.5", "2001:db8::5"}, held: "192.0.2.5,2001:db8::5", writes: 2},
		{name: "IPv4 renewed", own: []string{"192.0.2.9", "2001:db8::5"}, held: "192.0.2.9,2001:db8::5", writes: 3},
		{name: "IPv6 changed", own: []string{"192.0.2.9", "2001:db8::9"}, held: "192.0.2.9,2001:db8::9", writes: 4},
		{name: "IPv6 removed", own: []string{"192.0.2.9"}, held: "192.0.2.9", writes: 5},
		{name: "no change again", own: []string{"192.0.2.9"}, held: "192.0.2.9", writes: 5},
	} {
		own = step.own
		if err := a.recordAddresses(t.Context(), kube); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := held(); got != step.held {
			t.Errorf("%s: the ConfigMap holds %q, want %q", step.name, got, step.held)
		}
		if got := writes(); got != step.writes {
			t.Errorf("%s: %d writes, want %d", step.name, got, step.writes)
		}
	}

	restarted := &Appliance{Dir: a.Dir, Log: a.Log, Addresses: a.Addresses}
	if err := restarted.recordAddresses(t.Context(), kube); err != nil || writes() != 5 {
		t.Fatalf("a restart with the same addresses: %v, %d writes", err, writes())
	}
	own = []string{"192.0.2.10"}
	if err := restarted.recordAddresses(t.Context(), kube); err != nil || held() != "192.0.2.10" || writes() != 6 {
		t.Fatalf("a restart with a renewed address: %v, holds %q after %d writes", err, held(), writes())
	}
}

// eth0 holds the addresses, as an appliance with one network card has them.
func eth0(own ...string) func() (map[string][]string, error) {
	return func() (map[string][]string, error) { return map[string][]string{"eth0": own}, nil }
}

// readyAppliance is an appliance whose management cluster exists, reading its addresses from read.
func readyAppliance(t *testing.T, read func() (map[string][]string, error)) *Appliance {
	t.Helper()
	a := &Appliance{Dir: t.TempDir(), Log: slog.New(slog.DiscardHandler), Addresses: read}
	if err := os.MkdirAll(filepath.Dir(a.KubeconfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.KubeconfigPath(), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	return a
}

// Each address is recorded on the bridge and VLAN of the card that holds it, a second card's on
// its own. A ConfigMap written before the networks were is written once more, and a check that
// finds no change writes nothing.
func TestRecordAddressesRecordsTheNetworkOfEachCard(t *testing.T) {
	t.Parallel()
	a := readyAppliance(t, func() (map[string][]string, error) {
		return map[string][]string{"eth1": {"10.0.0.31", "2001:db8::31"}, "eth0": {"192.0.2.5"}, "eth2": {"198.51.100.7"}}, nil
	})
	config := map[string]string{
		"hostname": "kubehenge",
		"net1":     "name=eth1,bridge=vmbr0,hwaddr=BC:24:11:00:00:02,ip=dhcp,type=veth",
		"net0":     "name=eth0,bridge=vmbr1,hwaddr=BC:24:11:00:00:01,ip=192.0.2.5/24,tag=30,type=veth",
	}
	if err := a.RecordNetwork(config); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientset(&corev1.ConfigMap{
		Name: provisioner.ApplianceAddressesName, Namespace: interfaceNamespace,
		Data: map[string]string{provisioner.ApplianceAddressesKey: "192.0.2.5,10.0.0.31,198.51.100.7,2001:db8::31"},
	})
	for range 2 {
		if err := a.recordAddresses(t.Context(), kube); err != nil {
			t.Fatal(err)
		}
	}
	cm, err := kube.CoreV1().ConfigMaps(interfaceNamespace).Get(t.Context(), provisioner.ApplianceAddressesName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "192.0.2.5=vmbr1/30,10.0.0.31=vmbr0/0,2001:db8::31=vmbr0/0"; cm.Data[provisioner.ApplianceNetworksKey] != want {
		t.Errorf("the ConfigMap holds the networks %q, want %q", cm.Data[provisioner.ApplianceNetworksKey], want)
	}
	if got := patches(kube); got != 1 {
		t.Errorf("%d writes, want 1", got)
	}
}

// An appliance that knows no network of its own, as after a restore without its file, leaves the
// networks it recorded before and still records its addresses.
func TestRecordAddressesKeepsTheNetworksWithoutTheFile(t *testing.T) {
	t.Parallel()
	a := readyAppliance(t, eth0("192.0.2.9"))
	kube := fake.NewClientset(&corev1.ConfigMap{
		Name: provisioner.ApplianceAddressesName, Namespace: interfaceNamespace,
		Data: map[string]string{provisioner.ApplianceAddressesKey: "192.0.2.5", provisioner.ApplianceNetworksKey: "192.0.2.5=vmbr1/30"},
	})
	if err := a.recordAddresses(t.Context(), kube); err != nil {
		t.Fatal(err)
	}
	cm, err := kube.CoreV1().ConfigMaps(interfaceNamespace).Get(t.Context(), provisioner.ApplianceAddressesName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data[provisioner.ApplianceAddressesKey] != "192.0.2.9" || cm.Data[provisioner.ApplianceNetworksKey] != "192.0.2.5=vmbr1/30" {
		t.Errorf("the ConfigMap holds %v", cm.Data)
	}
}

// The file holds the cards in the order of their number, and a configuration without cards, as
// one that could not be read, leaves what it holds.
func TestRecordNetworkKeepsTheCards(t *testing.T) {
	t.Parallel()
	a := &Appliance{Dir: t.TempDir()}
	config := map[string]string{"net10": "name=eth10,bridge=vmbr10", "net2": "name=eth2,bridge=vmbr2", "network": "x", "net0": "name=eth0,bridge=vmbr0,tag=7"}
	if err := a.RecordNetwork(config); err != nil {
		t.Fatal(err)
	}
	want := "net0: name=eth0,bridge=vmbr0,tag=7\nnet2: name=eth2,bridge=vmbr2\nnet10: name=eth10,bridge=vmbr10\n"
	for _, empty := range []map[string]string{nil, {"hostname": "kubehenge"}, {"net0": " "}} {
		if err := a.RecordNetwork(empty); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(a.Dir, NetworkFile))
		if err != nil || string(got) != want {
			t.Errorf("after %v the file holds %q, %v, want %q", empty, got, err, want)
		}
	}
	segments, err := a.segments()
	if err != nil || !maps.Equal(segments, map[string]string{"eth0": "vmbr0/7", "eth2": "vmbr2/0", "eth10": "vmbr10/0"}) {
		t.Errorf("segments = %v, %v", segments, err)
	}
	for _, line := range []string{"net0: bridge=vmbr0", "net0: name=eth0", "net0: name=eth0,bridge=vmbr0,tag=x", "garbage"} {
		if name, segment := cardSegment(line); name != "" {
			t.Errorf("%q gave %s on %s", line, name, segment)
		}
	}
}

// patches counts the writes to the ConfigMap.
func patches(kube *fake.Clientset) int {
	n := 0
	for _, action := range kube.Actions() {
		if action.GetVerb() == "patch" {
			n++
		}
	}
	return n
}

// A ConfigMap the chart has not made yet is an error to retry, never one the appliance makes: the
// interface may not create ConfigMaps.
func TestRecordAddressesNeedsTheConfigMap(t *testing.T) {
	t.Parallel()
	a := readyAppliance(t, eth0("192.0.2.5"))
	err := a.recordAddresses(t.Context(), fake.NewClientset())
	if err == nil || !strings.Contains(err.Error(), "reading kubehenge-system/"+provisioner.ApplianceAddressesName) {
		t.Fatalf("err = %v", err)
	}
}

func TestOwnAddressesAreTheAppliancesOwn(t *testing.T) {
	t.Parallel()
	a := &Appliance{Addresses: func() (map[string][]string, error) {
		return map[string][]string{"eth1": {"2001:db8::31", "192.0.2.31"}, "eth0": {"192.0.2.31"}}, nil
	}}
	if got := a.OwnAddresses(); !slices.Equal(got, []string{"192.0.2.31", "2001:db8::31"}) {
		t.Errorf("OwnAddresses = %v", got)
	}
	a.Addresses = func() (map[string][]string, error) { return nil, errors.New("no netlink") }
	if got := a.OwnAddresses(); got != nil {
		t.Errorf("an unreadable list gave %v", got)
	}
}
