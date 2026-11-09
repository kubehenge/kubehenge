package appliance

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ipv6Settings is a valid IPv6-only cluster.
func ipv6Settings() *Settings {
	s := settings("pve1")
	s.IPFamilies = []string{"IPv6"}
	s.NodeAddresses, s.Gateway = nil, ""
	s.NodeAddressesIPv6, s.GatewayIPv6 = []string{"fd00:1::11/64"}, "fd00:1::1"
	s.APIAddress = "fd00:1::10"
	s.DNSServers = []string{"fd00:1::53"}
	return s
}

// dualSettings is a valid dual-stack cluster whose primary family is the first.
func dualSettings(families ...string) *Settings {
	s := settings("pve1")
	s.IPFamilies = families
	s.NodeAddressesIPv6, s.GatewayIPv6 = []string{"fd00:1::11/64"}, "fd00:1::1"
	s.DNSServers = []string{"10.0.0.1", "fd00:1::53"}
	if families[0] == "IPv6" {
		s.APIAddress = "fd00:1::10"
	}
	return s
}

func TestIPFamiliesAreValidated(t *testing.T) {
	t.Parallel()
	threeNodes := dualSettings("IPv4", "IPv6")
	threeNodes.Nodes = []string{"pve1", "pve2", "pve3"}
	threeNodes.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
	threeNodes.NodeAddressesIPv6 = []string{"fd00:1::11/64", "fd00:1::12/64", "fd00:1::13/64"}
	linkLocal := ipv6Settings()
	linkLocal.GatewayIPv6 = "fe80::1"
	ranges := dualSettings("IPv4", "IPv6")
	ranges.PodSubnets = []string{"10.244.0.0/16", "fd5e:4b1c:9a20::/56"}
	ranges.ServiceSubnets = []string{"10.96.0.0/12", "fd5e:4b1c:9a21::/112"}
	ipv6Ranges := ipv6Settings()
	ipv6Ranges.PodSubnets, ipv6Ranges.ServiceSubnets = []string{"fd5e:4b1c:9a20::/48"}, []string{"fd5e:4b1c:9a21::/108"}
	for name, s := range map[string]*Settings{
		"IPv4 by default":          settings("pve1"),
		"IPv4 named":               func() *Settings { s := settings("pve1"); s.IPFamilies = []string{"IPv4"}; return s }(),
		"dual-stack IPv4 first":    dualSettings("IPv4", "IPv6"),
		"dual-stack IPv6 first":    dualSettings("IPv6", "IPv4"),
		"dual-stack three nodes":   threeNodes,
		"IPv6 only":                ipv6Settings(),
		"IPv6 link-local gateway":  linkLocal,
		"a range of each family":   ranges,
		"IPv6 ranges at the edges": ipv6Ranges,
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestIPv6SettingsAreRefused(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		base   func() *Settings
		change func(*Settings)
		want   string
	}{
		"IPv4 address given as an IPv6 one": {ipv6Settings, func(s *Settings) { s.NodeAddressesIPv6 = []string{"10.0.0.11/24"} }, "Node addresses: 10.0.0.11/24 is no IPv6 address with its prefix length, such as 2001:db8::11/64"},
		"IPv6 address given as an IPv4 one": {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) { s.NodeAddresses = []string{"fd00:1::11/64"} }, "IPv4 node addresses: fd00:1::11/64 is no IPv4 address"},
		"IPv4-mapped address":               {ipv6Settings, func(s *Settings) { s.NodeAddressesIPv6 = []string{"::ffff:10.0.0.11/120"} }, "is no IPv6 address"},
		"IPv6 gateway an IPv4 one":          {ipv6Settings, func(s *Settings) { s.GatewayIPv6 = "10.0.0.1" }, "Gateway: an IPv6 address"},
		"IPv4 gateway an IPv6 one":          {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) { s.Gateway = "fd00:1::1" }, "IPv4 gateway: an IPv4 address"},
		"IPv6 network address":              {ipv6Settings, func(s *Settings) { s.NodeAddressesIPv6 = []string{"fd00:1::/64"} }, "Node addresses: fd00:1::/64 is its network's own address"},
		"IPv6 node outside the gateway":     {ipv6Settings, func(s *Settings) { s.NodeAddressesIPv6 = []string{"fd00:2::11/64"} }, "Node addresses: fd00:2::11/64 is outside the gateway's network"},
		"IPv6 node twice": {ipv6Settings, func(s *Settings) {
			s.NodeAddressesIPv6 = []string{"fd00:1::11/64", "FD00:1:0::11/64", "fd00:1::12/64"}
		}, "is listed twice"},
		"IPv6 nodes in two networks": {ipv6Settings, func(s *Settings) {
			s.NodeAddressesIPv6 = []string{"fd00:1::11/64", "fd00:1::12/48", "fd00:1::13/64"}
		}, "is in another network than fd00:1::/64"},
		"two IPv6 nodes":               {ipv6Settings, func(s *Settings) { s.NodeAddressesIPv6 = []string{"fd00:1::11/64", "fd00:1::12/64"} }, "Node addresses: one, or three for high availability"},
		"IPv4 addresses in IPv6 only":  {ipv6Settings, func(s *Settings) { s.NodeAddresses = []string{"10.0.0.11/24"} }, "IPv4 node addresses: leave empty, the cluster has no IPv4"},
		"IPv4 gateway in IPv6 only":    {ipv6Settings, func(s *Settings) { s.Gateway = "10.0.0.1" }, "IPv4 gateway: leave empty, the cluster has no IPv4"},
		"IPv6 addresses in IPv4 only":  {func() *Settings { return settings("pve1") }, func(s *Settings) { s.NodeAddressesIPv6 = []string{"fd00:1::11/64"} }, "IPv6 node addresses: leave empty, the cluster has no IPv6"},
		"dual-stack without IPv6":      {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) { s.NodeAddressesIPv6 = nil }, "IPv6 node addresses: one, or three for high availability"},
		"dual-stack without IPv4":      {func() *Settings { return dualSettings("IPv6", "IPv4") }, func(s *Settings) { s.NodeAddresses = nil }, "IPv4 node addresses: one, or three for high availability"},
		"dual-stack uneven counts":     {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) { s.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"} }, "IPv6 node addresses: as many as the IPv4 ones"},
		"dual-stack without a gateway": {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) { s.GatewayIPv6 = "" }, "IPv6 gateway: an IPv6 address"},
		"dual-stack without IPv4 gw":   {func() *Settings { return dualSettings("IPv6", "IPv4") }, func(s *Settings) { s.Gateway = "" }, "IPv4 gateway: an IPv4 address"},
		"dual-stack without IPv6 DNS":  {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) { s.DNSServers = []string{"10.0.0.1"} }, "DNS servers: at least one IPv6 address"},
		"dual-stack without IPv4 DNS":  {func() *Settings { return dualSettings("IPv6", "IPv4") }, func(s *Settings) { s.DNSServers = []string{"fd00:1::53"} }, "DNS servers: at least one IPv4 address"},
		"IPv4 DNS in IPv6 only":        {ipv6Settings, func(s *Settings) { s.DNSServers = []string{"10.0.0.1"} }, "DNS servers: 10.0.0.1 is of an address family the cluster does not have"},
		"IPv6 DNS in IPv4 only":        {func() *Settings { return settings("pve1") }, func(s *Settings) { s.DNSServers = []string{"fd00:1::53"} }, "DNS servers: fd00:1::53 is of an address family the cluster does not have"},
		"API in the other family":      {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) { s.APIAddress = "fd00:1::10" }, "API address: an IPv4 address"},
		"API an IPv4 one, IPv6 first":  {func() *Settings { return dualSettings("IPv6", "IPv4") }, func(s *Settings) { s.APIAddress = "10.0.0.10" }, "API address: an IPv6 address"},
		"IPv6 API with a prefix":       {ipv6Settings, func(s *Settings) { s.APIAddress = "fd00:1::10/64" }, "API address: an IPv6 address"},
		"IPv6 API a node":              {ipv6Settings, func(s *Settings) { s.APIAddress = "fd00:1::11" }, "API address: unused"},
		"IPv6 API the gateway":         {ipv6Settings, func(s *Settings) { s.APIAddress = "fd00:1::1" }, "API address: unused"},
		"IPv6 API elsewhere":           {ipv6Settings, func(s *Settings) { s.APIAddress = "fd00:2::10" }, "API address: inside the nodes' network, fd00:1::/64"},
		"IPv6 pod range on the nodes":  {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) { s.PodSubnets = []string{"10.244.0.0/16", "fd00:1::/56"} }, "Pod ranges: fd00:1::/56 overlaps the nodes' network fd00:1::/64"},
		"IPv6 service range on a node": {ipv6Settings, func(s *Settings) { s.ServiceSubnets = []string{"fd00:1::/112"} }, "Service ranges: fd00:1::/112 overlaps"},
		"IPv6 pod range on the API": {ipv6Settings, func(s *Settings) {
			s.NodeAddressesIPv6, s.GatewayIPv6, s.APIAddress = []string{"fd00:1::11/120"}, "fd00:1::1", "fd00:1::10"
			s.PodSubnets = []string{"fd00:1::/48"}
		}, "Pod ranges: fd00:1::/48 overlaps the API address fd00:1::10"},
		"pod and service ranges overlap": {ipv6Settings, func(s *Settings) {
			s.PodSubnets, s.ServiceSubnets = []string{"fd5e:4b1c:9a20::/56"}, []string{"fd5e:4b1c:9a20:1::/112"}
		}, "Pod ranges: fd5e:4b1c:9a20::/56 overlaps the service range fd5e:4b1c:9a20:1::/112"},
		"IPv4 pod range on the nodes": {func() *Settings { return settings("pve1") }, func(s *Settings) { s.PodSubnets = []string{"10.0.0.0/8"} }, "Pod ranges: 10.0.0.0/8 overlaps the nodes' network 10.0.0.0/24"},
		"ranges in the wrong order": {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) {
			s.PodSubnets = []string{"fd5e:4b1c:9a20::/56", "10.244.0.0/16"}
		}, "Pod ranges: fd5e:4b1c:9a20::/56 is no IPv4 network"},
		"service ranges in the wrong order": {func() *Settings { return dualSettings("IPv6", "IPv4") }, func(s *Settings) {
			s.ServiceSubnets = []string{"10.96.0.0/12", "fd5e:4b1c:9a21::/112"}
		}, "Service ranges: 10.96.0.0/12 is no IPv6 network"},
		"too few ranges":                 {func() *Settings { return dualSettings("IPv4", "IPv6") }, func(s *Settings) { s.PodSubnets = []string{"10.244.0.0/16"} }, "Pod ranges: one per address family, in this order: IPv4, IPv6"},
		"too many ranges":                {func() *Settings { return settings("pve1") }, func(s *Settings) { s.ServiceSubnets = []string{"10.96.0.0/12", "fd5e:4b1c:9a21::/112"} }, "Service ranges: one per address family, in this order: IPv4"},
		"IPv6 range with hostbits":       {ipv6Settings, func(s *Settings) { s.PodSubnets = []string{"fd5e:4b1c:9a20::1/56"} }, "Pod ranges: fd5e:4b1c:9a20::1/56 is no network such as fd5e:4b1c:9a20::/56, without host bits"},
		"IPv6 pod range too long":        {ipv6Settings, func(s *Settings) { s.PodSubnets = []string{"fd5e:4b1c:9a20::/64"} }, "Pod ranges: fd5e:4b1c:9a20::/64 is /64, and an IPv6 range is /48 to /63"},
		"IPv6 pod range too short":       {ipv6Settings, func(s *Settings) { s.PodSubnets = []string{"fd5e:4b1c::/47"} }, "is /47, and an IPv6 range is /48 to /63"},
		"IPv6 service range long":        {ipv6Settings, func(s *Settings) { s.ServiceSubnets = []string{"fd5e:4b1c:9a21::/121"} }, "Service ranges: fd5e:4b1c:9a21::/121 is /121, and an IPv6 range is /108 to /120"},
		"IPv6 service range short":       {ipv6Settings, func(s *Settings) { s.ServiceSubnets = []string{"fd5e:4b1c:9a21::/107"} }, "is /107, and an IPv6 range is /108 to /120"},
		"a family named twice":           {func() *Settings { return settings("pve1") }, func(s *Settings) { s.IPFamilies = []string{"IPv4", "IPv4"} }, "Address families:"},
		"a third family":                 {func() *Settings { return settings("pve1") }, func(s *Settings) { s.IPFamilies = []string{"IPv4", "IPv6", "IPv4"} }, "Address families:"},
		"an unknown family":              {func() *Settings { return settings("pve1") }, func(s *Settings) { s.IPFamilies = []string{"IPv5"} }, "Address families:"},
		"more nodes than IPv6 addresses": {ipv6Settings, func(s *Settings) { s.Nodes = []string{"pve1", "pve2"} }, "Proxmox VE nodes: at least one, and no more than there are node addresses"},
	} {
		s := c.base()
		c.change(s)
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestTemplateRefreshTakesAnIPv6Address(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.TemplateRefresh = &TemplateRefresh{Enabled: true, Address: "fd00:1::50/64", Gateway: "fd00:1::1", DNS: []string{"fd00:1::53"}}
	if err := s.Validate(); err != nil {
		t.Errorf("an IPv6 build network: %v", err)
	}
	s.TemplateRefresh.Gateway = "fe80::1"
	if err := s.Validate(); err != nil {
		t.Errorf("an IPv6 build network with a link-local gateway: %v", err)
	}
	for name, c := range map[string]struct {
		refresh TemplateRefresh
		want    string
	}{
		"no prefix":               {TemplateRefresh{Address: "fd00:1::50", Gateway: "fd00:1::1"}, "Template build address: an IPv4 address with prefix, such as 192.0.2.50/24, or an IPv6 one such as 2001:db8::50/64"},
		"gateway elsewhere":       {TemplateRefresh{Address: "fd00:1::50/64", Gateway: "fd00:2::1"}, "Template build gateway: an address in fd00:1::/64"},
		"gateway of the other":    {TemplateRefresh{Address: "fd00:1::50/64", Gateway: "10.0.0.1"}, "Template build gateway: an address in fd00:1::/64"},
		"IPv4 with IPv6 gateway":  {TemplateRefresh{Address: "10.0.0.50/24", Gateway: "fe80::1"}, "Template build gateway: an address in 10.0.0.0/24"},
		"IPv4-mapped address":     {TemplateRefresh{Address: "::ffff:10.0.0.50/120", Gateway: "::ffff:10.0.0.1"}, "Template build address: an IPv4 address with prefix"},
		"DNS server not an addr":  {TemplateRefresh{Address: "fd00:1::50/64", Gateway: "fd00:1::1", DNS: []string{"dns"}}, "Template build DNS servers"},
		"IPv6 gateway with no IP": {TemplateRefresh{Gateway: "fd00:1::1"}, "Template build address: needed"},
	} {
		s := settings("pve1")
		c.refresh.Enabled = true
		s.TemplateRefresh = &c.refresh
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestRangesFollowTheFamilies(t *testing.T) {
	t.Parallel()
	r, err := ipv6Settings().ranges()
	if err != nil || !slices.Equal(r.pods, []string{"fd5e:4b1c:9a20::/56"}) || !slices.Equal(r.services, []string{"fd5e:4b1c:9a21::/112"}) {
		t.Errorf("IPv6 only: %+v %v", r, err)
	}
	r, err = dualSettings("IPv4", "IPv6").ranges()
	if err != nil || !slices.Equal(r.pods, []string{"10.244.0.0/16", "fd5e:4b1c:9a20::/56"}) || !slices.Equal(r.services, []string{"10.96.0.0/12", "fd5e:4b1c:9a21::/112"}) {
		t.Errorf("dual-stack, IPv4 first: %+v %v", r, err)
	}
	r, err = dualSettings("IPv6", "IPv4").ranges()
	if err != nil || !slices.Equal(r.pods, []string{"fd5e:4b1c:9a20::/56", "10.244.0.0/16"}) || !slices.Equal(r.services, []string{"fd5e:4b1c:9a21::/112", "10.96.0.0/12"}) {
		t.Errorf("dual-stack, IPv6 first: %+v %v", r, err)
	}
	// The nodes sit in the first candidates' own range.
	s := ipv6Settings()
	s.NodeAddressesIPv6, s.GatewayIPv6, s.APIAddress = []string{"fd5e:4b1c:9a20::11/64"}, "fd5e:4b1c:9a20::1", "fd5e:4b1c:9a20::10"
	r, err = s.ranges()
	if err != nil || !slices.Equal(r.pods, []string{"fd5e:4b1c:9a30::/56"}) || !slices.Equal(r.services, []string{"fd5e:4b1c:9a21::/112"}) {
		t.Errorf("nodes in the default pod range: %+v %v", r, err)
	}
	s.NodeAddressesIPv6 = []string{"fd5e::11/16"}
	s.GatewayIPv6, s.APIAddress = "fd5e::1", "fd5e::10"
	if _, err = s.ranges(); err == nil {
		t.Error("a node network covering every IPv6 candidate was accepted")
	}
}

func TestIPv6FamilyIsFixedOnceCreated(t *testing.T) {
	t.Parallel()
	was := dualSettings("IPv4", "IPv6")
	for name, c := range map[string]struct {
		change func(*Settings)
		want   string
	}{
		"IPv6 first":        {func(s *Settings) { s.IPFamilies = []string{"IPv6", "IPv4"} }, "Address families: stay IPv4, IPv6"},
		"IPv6 dropped":      {func(s *Settings) { s.IPFamilies = nil }, "Address families: stay IPv4, IPv6"},
		"IPv6 address":      {func(s *Settings) { s.NodeAddressesIPv6 = []string{"fd00:1::12/64"} }, "IPv6 node addresses: the nodes there are keep theirs (fd00:1::11/64)"},
		"IPv6 node removed": {func(s *Settings) { s.NodeAddressesIPv6 = nil }, "IPv6 node addresses: the nodes there are keep theirs"},
		"IPv6 gateway":      {func(s *Settings) { s.GatewayIPv6 = "fd00:1::254" }, "IPv6 gateway: stays fd00:1::1"},
	} {
		s := dualSettings("IPv4", "IPv6")
		c.change(s)
		if err := s.KeepsFixed(was); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	grown := dualSettings("IPv4", "IPv6")
	grown.Nodes = []string{"pve1", "pve2", "pve3"}
	grown.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
	grown.NodeAddressesIPv6 = []string{"fd00:1::11/64", "fd00:1::12/64", "fd00:1::13/64"}
	grown.Nodes = was.Nodes
	if err := grown.KeepsFixed(was); err != nil {
		t.Errorf("adding nodes after the existing ones: %v", err)
	}
}

func TestIPv6NodeCount(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		s    *Settings
		want int
	}{
		"IPv4":        {settings("pve1"), 1},
		"IPv6 only":   {ipv6Settings(), 1},
		"dual-stack":  {dualSettings("IPv6", "IPv4"), 1},
		"no families": {&Settings{}, 0},
	} {
		if got := c.s.NodeCount(); got != c.want {
			t.Errorf("%s: %d nodes, want %d", name, got, c.want)
		}
	}
	three := ipv6Settings()
	three.NodeAddressesIPv6 = []string{"fd00:1::11/64", "fd00:1::12/64", "fd00:1::13/64"}
	if got := three.NodeCount(); got != 3 {
		t.Errorf("three IPv6 nodes: %d", got)
	}
}

// An IPv4 cluster's variables and saved settings are byte for byte what they were before IPv6, so
// that a running management cluster plans no change.
func TestIPv4SettingsKeepTheirModuleVariables(t *testing.T) {
	t.Parallel()
	plain := settings("pve1")
	rich := settings("pve1")
	rich.Nodes = []string{"pve1", "pve2", "pve3"}
	rich.NodeAddresses = []string{"10.0.0.11/24", "10.0.0.12/24", "10.0.0.13/24"}
	rich.MTU = 1450
	rich.SearchDomains = []string{"corp.example"}
	rich.TimeServers = []string{"ntp.example.org"}
	rich.PodSubnets = []string{"10.50.0.0/16"}
	rich.ServiceSubnets = []string{"10.60.0.0/16"}
	// Naming IPv4 changes the saved settings, not what the module is given.
	named := settings("pve1")
	named.IPFamilies = []string{"IPv4"}
	for _, c := range []struct {
		name, fixture string
		s             *Settings
		settingsFile  bool
	}{
		{"plain", "plain", plain, true},
		{"rich", "rich", rich, true},
		{"IPv4 named", "plain", named, false},
	} {
		a, _ := newAppliance(t)
		if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
			t.Fatal(err)
		}
		if err := a.writeConfig(c.s, VMIDRange{DefaultVMIDStart, DefaultVMIDEnd}); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"terraform.tfvars.json": "ipv4-" + c.fixture + ".json"}
		if c.settingsFile {
			files["settings.json"] = "ipv4-" + c.fixture + "-settings.json"
		}
		for file, fixture := range files {
			got, err := os.ReadFile(filepath.Join(a.workdir(), file)) //nolint:gosec // the test's own temporary directory
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "variables", fixture)) //nolint:gosec // the test's own fixtures
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: %s differs from what an IPv4 cluster always had:\n%s\nwant\n%s", c.name, file, got, want)
			}
		}
	}
}

func TestIPv6ReachesTheModule(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		s       *Settings
		want    map[string]any
		present bool
	}{
		"dual-stack IPv4 first": {dualSettings("IPv4", "IPv6"), map[string]any{
			"ipFamilies":        []any{"IPv4", "IPv6"},
			"nodeAddresses":     []any{"10.0.0.11/24"},
			"nodeAddressesIPv6": []any{"fd00:1::11/64"},
			"gateway":           "10.0.0.1",
			"gatewayIPv6":       "fd00:1::1",
			"apiAddress":        "10.0.0.10",
			"dnsServers":        []any{"10.0.0.1", "fd00:1::53"},
			"podSubnets":        []any{"10.244.0.0/16", "fd5e:4b1c:9a20::/56"},
			"serviceSubnets":    []any{"10.96.0.0/12", "fd5e:4b1c:9a21::/112"},
		}, true},
		"dual-stack IPv6 first": {dualSettings("IPv6", "IPv4"), map[string]any{
			"ipFamilies":        []any{"IPv6", "IPv4"},
			"nodeAddresses":     []any{"10.0.0.11/24"},
			"nodeAddressesIPv6": []any{"fd00:1::11/64"},
			"gateway":           "10.0.0.1",
			"gatewayIPv6":       "fd00:1::1",
			"apiAddress":        "fd00:1::10",
			"podSubnets":        []any{"fd5e:4b1c:9a20::/56", "10.244.0.0/16"},
			"serviceSubnets":    []any{"fd5e:4b1c:9a21::/112", "10.96.0.0/12"},
		}, true},
		"IPv6 only": {ipv6Settings(), map[string]any{
			"ipFamilies":        []any{"IPv6"},
			"nodeAddresses":     []any{},
			"nodeAddressesIPv6": []any{"fd00:1::11/64"},
			"gateway":           nil,
			"gatewayIPv6":       "fd00:1::1",
			"apiAddress":        "fd00:1::10",
			"dnsServers":        []any{"fd00:1::53"},
			"podSubnets":        []any{"fd5e:4b1c:9a20::/56"},
			"serviceSubnets":    []any{"fd5e:4b1c:9a21::/112"},
		}, true},
		"IPv4 only": {settings("pve1"), map[string]any{"nodeAddresses": []any{"10.0.0.11/24"}, "gateway": "10.0.0.1"}, false},
	} {
		a, _ := newAppliance(t)
		if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
			t.Fatal(err)
		}
		if err := a.writeConfig(c.s, VMIDRange{DefaultVMIDStart, DefaultVMIDEnd}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, err := os.ReadFile(filepath.Join(a.workdir(), "terraform.tfvars.json"))
		if err != nil {
			t.Fatal(err)
		}
		var vars struct {
			Settings map[string]any `json:"settings"`
		}
		if err = json.Unmarshal(raw, &vars); err != nil {
			t.Fatal(err)
		}
		for key, want := range c.want {
			got, ok := vars.Settings[key]
			if !ok || !reflect.DeepEqual(got, want) {
				t.Errorf("%s: %s = %v, want %v", name, key, got, want)
			}
		}
		for _, key := range []string{"ipFamilies", "nodeAddressesIPv6", "gatewayIPv6"} {
			if _, has := vars.Settings[key]; has != c.present {
				t.Errorf("%s: %s present = %t, want %t", name, key, has, c.present)
			}
		}
		main, err := os.ReadFile(filepath.Join(a.workdir(), "main.tf"))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{
			`ip_families            = try(var.settings.ipFamilies, ["IPv4"])`,
			`node_addresses_ipv6    = try(var.settings.nodeAddressesIPv6, [])`,
			`gateway_ipv6           = try(var.settings.gatewayIPv6, null)`,
		} {
			if !strings.Contains(string(main), line) {
				t.Errorf("%s: root module lacks %s", name, line)
			}
		}
	}
}

func TestRangesClearOfABridgesIPv6Network(t *testing.T) {
	t.Parallel()
	s := ipv6Settings()
	s.PodSubnets, s.ServiceSubnets = []string{"fd00:2::/56"}, []string{"fd00:3::/112"}
	inv := healthy()
	inv.Bridges["pve1"][0].CIDR6, inv.Bridges["pve1"][0].Gateway6 = "fd00:2:0:5::5/64", "fd00:3::1"
	f := s.Preflight(inv, false)
	mentions(t, f.Warnings, "The pod range fd00:2::/56 overlaps the network fd00:2:0:5::/64 on bridge vmbr0 of node pve1")
	mentions(t, f.Warnings, "The service range fd00:3::/112 overlaps the gateway fd00:3::1 of bridge vmbr0 on node pve1")
}

func TestIPv6AddressesAreChecked(t *testing.T) {
	t.Parallel()
	s := dualSettings("IPv4", "IPv6")
	f := Findings{Errors: []string{}, Warnings: []string{}}
	inv := &Inventory{Answering: map[netip.Addr]bool{netip.MustParseAddr("fd00:1::11"): true}}
	s.checkAddressesFree(inv, &f)
	if len(f.Errors) != 1 || !strings.Contains(f.Errors[0], "Address fd00:1::11 answers") {
		t.Errorf("an IPv6 node address that answers: %v", f.Errors)
	}
	list := dualSettings("IPv4", "IPv6")
	list.Proxy = &Proxy{URL: "http://proxy.example:3128"}
	got := list.NoProxyList("https://pve.example:8006")
	for _, want := range []string{"10.0.0.0/24", "fd00:1::/64", "10.244.0.0/16", "fd5e:4b1c:9a20::/56", "10.96.0.0/12", "fd5e:4b1c:9a21::/112"} {
		if !slices.Contains(got, want) {
			t.Errorf("the proxy exceptions %v lack %s", got, want)
		}
	}
}
