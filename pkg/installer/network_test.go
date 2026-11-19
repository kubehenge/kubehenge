package installer

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An address typed without a prefix takes the prefix of the network. Without a known network it
// needs one, and nothing but an IPv4 address passes.
func TestParseAddress4PrefixIsOptional(t *testing.T) {
	t.Parallel()
	known := netip.MustParsePrefix("192.0.2.9/24")
	for _, c := range []struct {
		text  string
		known netip.Prefix
		want  string
	}{
		{"192.0.2.40", known, "192.0.2.40/24"},
		{"192.0.2.40/25", known, "192.0.2.40/25"},
		{"192.0.2.40", netip.Prefix{}, ""},
		{"192.0.2.40/25", netip.Prefix{}, "192.0.2.40/25"},
		{"2001:db8::1", known, ""},
		{"192.0.2.400", known, ""},
	} {
		got, ok := ParseAddress4(c.text, c.known)
		if (c.want == "") == ok || (ok && got.String() != c.want) {
			t.Errorf("%q: got %v %v, want %q", c.text, got, ok, c.want)
		}
	}
}

// A gateway found for another subnet is moved onto the address's subnet with its host part, and
// none is formed where the result would be the address itself or a network address.
func TestGuessGatewayKeepsTheHostPart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		addr, known, want string
	}{
		{"198.51.100.40/24", "192.0.2.1", "198.51.100.1"},
		{"192.0.2.40/24", "192.0.2.1", "192.0.2.1"},
		{"198.51.100.1/24", "192.0.2.1", ""},
		{"198.51.100.40/24", "", ""},
		{"198.51.100.40/24", "2001:db8::1", ""},
	} {
		if got := GuessGateway(netip.MustParsePrefix(c.addr), c.known); got != c.want {
			t.Errorf("%s with %s: %q, want %q", c.addr, c.known, got, c.want)
		}
	}
}

// Addresses that cannot work are refused with the reason: a network or broadcast address, a gateway
// outside the subnet or equal to the address, and for IPv6 a link-local address.
func TestAddressPairsThatCannotWork(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		addr, gateway, want string
	}{
		{"192.0.2.0/24", "192.0.2.1", "network or broadcast address"},
		{"192.0.2.255/24", "192.0.2.1", "network or broadcast address"},
		{"192.0.2.40/24", "192.0.2.40", "both 192.0.2.40"},
		{"192.0.2.40/24", "198.51.100.1", "outside 192.0.2.0/24"},
		{"192.0.2.40/24", "192.0.2.255", "network or broadcast address"},
		{"192.0.2.40/24", "192.0.2.1", ""},
		{"192.0.2.40/24", "", ""},
	} {
		err := checkPair(netip.MustParsePrefix(c.addr), c.gateway)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s via %s: %v, want %q", c.addr, c.gateway, err, c.want)
		}
	}
	if err := checkPair6(netip.MustParsePrefix("fe80::1/64"), ""); err == nil {
		t.Error("a link-local address was accepted")
	}
	if err := checkPair6(netip.MustParsePrefix("2001:db8::5/64"), "fe80::1"); err != nil {
		t.Errorf("a link-local gateway was refused: %v", err)
	}
	if err := checkPair6(netip.MustParsePrefix("2001:db8::5/64"), "2001:db9::1"); err == nil {
		t.Error("an IPv6 gateway outside the prefix was accepted")
	}
}

func TestOutsideSubnetSaysSo(t *testing.T) {
	t.Parallel()
	known := netip.MustParsePrefix("192.0.2.9/24")
	if msg := OutsideSubnet(known, netip.MustParsePrefix("198.51.100.4/24")); !strings.Contains(msg, "outside 192.0.2.0/24") {
		t.Errorf("outside: %q", msg)
	}
	if msg := OutsideSubnet(known, netip.MustParsePrefix("192.0.2.4/24")); msg != "" {
		t.Errorf("inside: %q", msg)
	}
	if msg := OutsideSubnet(netip.Prefix{}, netip.MustParsePrefix("192.0.2.4/24")); msg != "" {
		t.Errorf("unknown subnet: %q", msg)
	}
}

// A neighbour the node already knows holds its address, whatever a probe would find.
func TestNeighbourTableMakesAnAddressInUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("ip neigh show 192.0.2.60 dev vmbr0", "192.0.2.60 lladdr 02:00:00:00:00:01 STALE\n")
	got := h.node.CheckAddress(context.Background(), "vmbr0", "192.0.2.60", "192.0.2.1", true)
	if !got.Taken() || !strings.Contains(got.Detail, "neighbour table") {
		t.Errorf("a neighbour-table entry was not in use: %+v", got)
	}
}

// Saved answers fill what the command line left open and nothing it gave, and keep only what this
// node still has. The file holds no secret.
func TestRestoreKeepsWhatStillFits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	s := prepared(t, h, Config{Name: "given.example.org"})
	saved := &savedAnswers{
		Name: "saved.example.org", DNS: "192.0.2.53", Bridge: "vmbr9", Storage: "nowhere", Hostname: "kept",
		TrustedProxies: []string{"192.0.2.99"},
	}
	if !s.Restore(context.Background(), saved) || !s.Restored {
		t.Fatal("nothing was restored")
	}
	c := s.Cfg
	switch {
	case c.Name != "given.example.org":
		t.Errorf("a flag was overwritten: %q", c.Name)
	case c.DNS != "192.0.2.53" || c.Hostname != "kept" || len(c.TrustedProxies) != 1:
		t.Errorf("open options were not restored: %+v", c)
	case c.Bridge == "vmbr9" || c.Storage == "nowhere":
		t.Errorf("a network or storage this node lacks was restored: %q %q", c.Bridge, c.Storage)
	}
	if s2 := prepared(t, h, Config{}); s2.Restore(context.Background(), nil) || s2.Restored {
		t.Error("restored from nothing")
	}
}

func TestSavedAnswersRoundTripWithoutSecrets(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "answers.json")
	cfg := Config{CTID: "105", IP: "192.0.2.40/24", Gateway: "192.0.2.1", CertFile: "/root/c.pem", KeyFile: "/root/secret.key", Name: "kubehenge.example.org"}
	if err := saveAnswers(path, &cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret.key") || strings.Contains(string(data), "c.pem") {
		t.Errorf("the file names the certificate or its key: %s", data)
	}
	got := loadAnswers(path)
	if got == nil || got.IP != "192.0.2.40/24" || got.CTID != "105" || got.Name != "kubehenge.example.org" {
		t.Errorf("loaded %+v", got)
	}
	removeAnswers(path)
	if loadAnswers(path) != nil {
		t.Error("the answers stayed after their removal")
	}
}

// The plan is the same rows for the screen, the plain output and the dry run.
func TestPlanTextListsWhatIsCreated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	s := prepared(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	plan := s.PlanText(true)
	for _, want := range []string{"IPv4", "192.0.2.40/24 via 192.0.2.1", "IPv6", "DNS", "Resources", "Storage", "Proxmox VE API", "you choose to keep or remove"} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan lacks %q:\n%s", want, plan)
		}
	}
	s.Cfg.KeepFailed = true
	if plan = s.PlanText(true); !strings.Contains(plan, "the container is kept") {
		t.Errorf("--keep-failed is not in the plan:\n%s", plan)
	}
	if plan = s.PlanText(false); strings.Contains(plan, "you choose") {
		t.Errorf("the plain plan offers a choice:\n%s", plan)
	}
}

// Every option of the command line that sets an answer comes back in the repeat command.
func TestRepeatArgsCarryTheAnswers(t *testing.T) {
	t.Parallel()
	cfg := Config{CTID: "105", Bridge: "vnet1", VLAN: "20", IP: "192.0.2.40/24", Gateway: "192.0.2.1", IP6: "2001:db8::5/64", Gateway6: "2001:db8::1", PVEEntry: "no"}
	got := strings.Join(cfg.RepeatArgs(), " ")
	for _, want := range []string{"--ctid 105", "--bridge vnet1", "--vlan 20", "--ip 192.0.2.40/24", "--gateway 192.0.2.1", "--ip6 2001:db8::5/64", "--gateway6 2001:db8::1", "--no-pve-entry"} {
		if !strings.Contains(got, want) {
			t.Errorf("the repeat command lacks %q: %s", want, got)
		}
	}
}

// Without an IPv6 setting the container takes no IPv6, and the address read back from it is IPv4 only.
func TestIPv4OnlyContainerHasNoIPv6(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	if _, err := plainOn(t, h, Config{IP: "dhcp", PVEEntry: "no"}); err != nil {
		t.Fatal(err)
	}
	if create := h.exec.last("pct create"); strings.Contains(create, "ip6=") {
		t.Errorf("an IPv4-only container was given IPv6: %s", create)
	}
}
