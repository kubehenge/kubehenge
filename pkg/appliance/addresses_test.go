package appliance

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

func TestApplyPassesTheAppliancesAddresses(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	a.Addresses = func() ([]string, error) { return []string{"192.0.2.5", "2001:db8::5"}, nil }
	ready := func(context.Context, []byte) error { return nil }
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), ready); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" {
		t.Fatalf("job = %+v", j)
	}
	vars, err := os.ReadFile(filepath.Join(a.Dir, "management", "terraform.tfvars.json"))
	if err != nil || !strings.Contains(string(vars), "\"applianceAddresses\": [\n      \"192.0.2.5\",\n      \"2001:db8::5\"\n    ]") {
		t.Fatalf("tfvars without the appliance's addresses: %v\n%s", err, vars)
	}
	main, err := os.ReadFile(filepath.Join(a.Dir, "management", "main.tf"))
	if err != nil || !strings.Contains(string(main), "appliance_addresses    = try(var.settings.applianceAddresses, [])") {
		t.Fatalf("main.tf: %v\n%s", err, main)
	}
}
