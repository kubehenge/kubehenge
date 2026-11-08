package appliance

import (
	"log/slog"
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
	a.Addresses = func() ([]string, error) { return slices.Clone(own), nil }
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

// A ConfigMap the chart has not made yet is an error to retry, never one the appliance makes: the
// interface may not create ConfigMaps.
func TestRecordAddressesNeedsTheConfigMap(t *testing.T) {
	t.Parallel()
	a := &Appliance{Dir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	a.Addresses = func() ([]string, error) { return []string{"192.0.2.5"}, nil }
	if err := os.MkdirAll(filepath.Dir(a.KubeconfigPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.KubeconfigPath(), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := a.recordAddresses(t.Context(), fake.NewClientset())
	if err == nil || !strings.Contains(err.Error(), "reading kubehenge-system/"+provisioner.ApplianceAddressesName) {
		t.Fatalf("err = %v", err)
	}
}
