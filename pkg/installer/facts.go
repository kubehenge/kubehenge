package installer

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Storage is a storage of the node that takes a kind of content.
type Storage struct {
	Name    string
	Type    string
	FreeKiB int64
	rank    int
}

// Appliance is an installed Kubehenge container.
type Appliance struct {
	ID   string
	Node string
}

// Facts is everything detected on the node before the first question.
type Facts struct {
	Node          string
	FQDN          string
	Bridges       []string
	DefaultBridge string
	// NodeDHCP is whether the node's own bridge address comes from DHCP, which is the best
	// sign that its network has a DHCP server.
	NodeDHCP         bool
	Prefix           netip.Prefix
	Gateway          string
	DNS              []string
	LoopbackDNSOnly  bool
	RootStorages     []Storage
	ShortRoot        string
	TemplateStorages []Storage
	TemplateHeld     string
	NextID           string
	Existing         []Appliance
	SuggestedIP      string
	Candidates       []APICandidate
	Members          []Member
}

// Member is an address of one of the cluster's nodes.
type Member struct {
	Address string
	Node    string
}

// Node reads and changes one Proxmox VE node.
type Node struct {
	Exec Exec
	// Root is prepended to the node's file paths, empty on a real node.
	Root string
	// Hostname is the node's short name.
	Hostname string
	// LookupHost resolves a name, as the node's resolver does.
	LookupHost func(ctx context.Context, name string) ([]string, error)
	// Dial returns the certificates the API at host:port serves.
	Dial func(ctx context.Context, host, port string) ([]*x509.Certificate, error)
}

const (
	rootDiskKiB  = 9437184
	templateKiB  = 204800
	clusterCA    = "/etc/pve/pve-root-ca.pem"
	systemCA     = "/etc/ssl/certs/ca-certificates.crt"
	resolvConf   = "/etc/resolv.conf"
	interfaces   = "/etc/network/interfaces"
	pveServedPEM = "/etc/pve/local/pveproxy-ssl.pem"
	pveNodePEM   = "/etc/pve/local/pve-ssl.pem"
)

var storageRank = map[string]int{"zfspool": rankLocal, "lvmthin": rankLocal, "lvm": rankLocal, "rbd": rankLocal, "dir": rankDir, "btrfs": rankDir, "nfs": rankNFS, "cifs": rankCIFS, "cephfs": rankCeph}

// Storages lists the enabled, active storages that take the content and have that much free,
// best first: local block storage, then directories, NFS, CIFS, CephFS and the rest, and within a
// kind the most free space. A share is slower and goes away with its network.
func (n *Node) Storages(ctx context.Context, content string, leastKiB int64) ([]Storage, []Storage, error) {
	out, err := n.run(ctx, "pvesm", "status", "--content", content, "--enabled", "1")
	if err != nil {
		return nil, nil, err
	}
	var fit, short []Storage
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, line := range lines {
		f := strings.Fields(line)
		if i == 0 || len(f) < 6 || f[2] != "active" {
			continue
		}
		free, convErr := strconv.ParseInt(f[5], 10, 64)
		if convErr != nil {
			continue
		}
		rank, ok := storageRank[f[1]]
		if !ok {
			rank = 6
		}
		s := Storage{Name: f[0], Type: f[1], FreeKiB: free, rank: rank}
		if free >= leastKiB {
			fit = append(fit, s)
		} else {
			short = append(short, s)
		}
	}
	slices.SortStableFunc(fit, func(a, b Storage) int {
		if a.rank != b.rank {
			return cmp.Compare(a.rank, b.rank)
		}
		return cmp.Compare(b.FreeKiB, a.FreeKiB)
	})
	return fit, short, nil
}

// FreeText formats a storage's free space.
func (s Storage) FreeText() string { return fmt.Sprintf("%.1f GiB free", float64(s.FreeKiB)/kibPerGiB) }

func jsonList(out string) []map[string]any {
	var list []map[string]any
	_ = json.Unmarshal([]byte(out), &list)
	return list
}

func str(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

var (
	viaDevRE  = regexp.MustCompile(`^default via (\S+)(?: dev (\S+))?`)
	devRE     = regexp.MustCompile(`\bdev (\S+)`)
	inetRE    = regexp.MustCompile(`\binet (\S+)`)
	inet6RE   = regexp.MustCompile(`\binet6 (\S+)`)
	nameserRE = regexp.MustCompile(`^nameserver\s+(\S+)`)
)

// ApplianceContainers lists the containers of the cluster tagged kubehenge.
func (n *Node) ApplianceContainers(ctx context.Context) []Appliance {
	out, err := n.run(ctx, "pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json")
	if err != nil {
		return nil
	}
	var found []Appliance
	for _, m := range jsonList(out) {
		if str(m, "type") != "lxc" {
			continue
		}
		for tag := range strings.SplitSeq(str(m, "tags"), ";") {
			if tag == "kubehenge" {
				found = append(found, Appliance{ID: str(m, "vmid"), Node: str(m, "node")})
			}
		}
	}
	return found
}

// IDFree reports whether no guest in the cluster has the id.
func (n *Node) IDFree(ctx context.Context, id string) bool {
	_, err := n.run(ctx, "pvesh", "get", "/cluster/nextid", "--vmid", id)
	return err == nil
}

// TemplateHeld returns the storage that already holds the container template, if one does.
func (n *Node) TemplateHeld(ctx context.Context, storages []Storage, template string) string {
	for _, s := range storages {
		out, err := n.run(ctx, "pvesm", "list", s.Name, "--content", "vztmpl")
		if err == nil && strings.Contains(out, "vztmpl/"+template+" ") {
			return s.Name
		}
	}
	return ""
}

// Quorate reports whether this node can change the cluster's configuration. A node in no
// cluster always can.
func (n *Node) Quorate(ctx context.Context) bool {
	out, err := n.run(ctx, "pvecm", "status")
	if err != nil {
		out = ""
	}
	if regexp.MustCompile(`(?m)^Quorate:\s+Yes`).MatchString(out) {
		return true
	}
	_, statErr := os.Stat(n.path("/etc/pve/corosync.conf"))
	return strings.TrimSpace(out) == "" && errors.Is(statErr, os.ErrNotExist)
}

// Detect gathers the facts the questions rest on. It changes nothing.
func (n *Node) Detect(ctx context.Context, cfg *Config) (*Facts, error) {
	f := &Facts{Node: n.Hostname}
	if out, err := n.run(ctx, "hostname", "-f"); err == nil {
		f.FQDN = strings.ToLower(strings.TrimSpace(out))
	}
	f.Bridges = n.bridges(ctx)
	gw, dev := n.defaultRoute(ctx)
	f.Gateway = gw
	f.DefaultBridge = pickDefaultBridge(f.Bridges, dev)
	f.DNS, f.LoopbackDNSOnly = n.resolvers()
	var err error
	if f.RootStorages, _, err = n.Storages(ctx, "rootdir", rootDiskKiB); err != nil {
		return nil, fmt.Errorf("listing storages: %w", err)
	}
	_, short, _ := n.Storages(ctx, "rootdir", rootDiskKiB)
	f.ShortRoot = shortText(short)
	all, _, err := n.Storages(ctx, "vztmpl", 0)
	if err != nil {
		return nil, fmt.Errorf("listing storages: %w", err)
	}
	f.TemplateStorages = all
	f.TemplateHeld = n.TemplateHeld(ctx, all, Template)
	if out, err := n.run(ctx, "pvesh", "get", "/cluster/nextid"); err == nil {
		f.NextID = strings.Trim(strings.TrimSpace(out), `"`)
	}
	f.Existing = n.ApplianceContainers(ctx)
	bridge := cfg.Bridge
	if bridge == "" {
		bridge = f.DefaultBridge
	}
	if bridge != "" {
		f.Prefix = n.bridgeAddress4(ctx, bridge)
		f.NodeDHCP = n.bridgeIsDHCP(bridge)
		if f.Prefix.IsValid() {
			f.SuggestedIP = n.SuggestAddress(ctx, bridge, f)
		}
		f.Members = n.clusterAddresses(ctx)
		f.Candidates = n.APICandidates(ctx, f, bridge)
	}
	return f, nil
}

func shortText(short []Storage) string {
	parts := make([]string, 0, len(short))
	for _, s := range short {
		parts = append(parts, fmt.Sprintf("%s (%s)", s.Name, s.FreeText()))
	}
	return strings.Join(parts, ", ")
}

// pickDefaultBridge returns the bridge holding the default route, else vmbr0 where it exists.
func pickDefaultBridge(bridges []string, routeDev string) string {
	for _, want := range []string{routeDev, "vmbr0"} {
		for _, b := range bridges {
			if want != "" && b == want {
				return b
			}
		}
	}
	return ""
}

// AddressInUse probes the bridge for the address: the neighbour table first, then an ARP
// duplicate-address probe, else a ping where arping is missing.
func (n *Node) AddressInUse(ctx context.Context, bridge, address string) bool {
	if out, err := n.run(ctx, "ip", "neigh", "show", address, "dev", bridge); err == nil {
		for _, state := range []string{"REACHABLE", "STALE", "DELAY", "PERMANENT", "PROBE"} {
			if strings.Contains(out, state) {
				return true
			}
		}
	}
	_, err := n.run(ctx, "arping", "-c", "2", "-w", "3", "-I", bridge, "-D", address)
	if err == nil {
		return false
	}
	var ce *CmdError
	if errors.As(err, &ce) && ce.Code == 1 {
		return true
	}
	_, err = n.run(ctx, "ping", "-c", "1", "-W", "1", "-I", bridge, address)
	return err == nil
}

// SuggestAddress returns a free-looking address in the bridge's subnet, empty for none. It walks
// down from the top of the subnet, past the gateway, the node and every known neighbour.
func (n *Node) SuggestAddress(ctx context.Context, bridge string, f *Facts) string {
	prefix := f.Prefix.Masked()
	if prefix.Bits() > maxSuggestBits {
		return ""
	}
	skip := map[netip.Addr]bool{f.Prefix.Addr(): true}
	if g, err := netip.ParseAddr(f.Gateway); err == nil {
		skip[g] = true
	}
	if out, err := n.run(ctx, "ip", "-4", "neigh", "show", "dev", bridge); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			if fields := strings.Fields(line); len(fields) > 0 {
				if a, err := netip.ParseAddr(fields[0]); err == nil {
					skip[a] = true
				}
			}
		}
	}
	last := lastAddress(prefix).Prev()
	tries := 0
	for a := last; prefix.Contains(a) && tries < 64; a = a.Prev() {
		if skip[a] {
			continue
		}
		tries++
		if !n.AddressInUse(ctx, bridge, a.String()) {
			return a.String()
		}
	}
	return ""
}

func (n *Node) path(p string) string { return filepath.Join(n.Root, p) }

func (n *Node) readFile(p string) ([]byte, error) {
	data, err := os.ReadFile(n.path(p))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return data, nil
}

func (n *Node) run(ctx context.Context, name string, args ...string) (string, error) {
	return n.Exec.Run(ctx, name, args...) //nolint:wrapcheck // the caller names the command, and CmdError carries it.
}

// bridges lists the bridges, Open vSwitch bridges and SDN VNets a container can attach to.
func (n *Node) bridges(ctx context.Context) []string {
	seen := map[string]bool{}
	var list []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			list = append(list, name)
		}
	}
	if out, err := n.run(ctx, "pvesh", "get", "/nodes/"+n.Hostname+"/network", "--output-format", "json"); err == nil {
		for _, m := range jsonList(out) {
			if t := str(m, "type"); t == "bridge" || t == "OVSBridge" {
				add(str(m, "iface"))
			}
		}
	}
	// Where SDN is not set up, the call fails and there are none.
	if out, err := n.run(ctx, "pvesh", "get", "/cluster/sdn/vnets", "--output-format", "json"); err == nil {
		for _, m := range jsonList(out) {
			add(str(m, "vnet"))
		}
	}
	slices.Sort(list)
	return list
}

func (n *Node) defaultRoute(ctx context.Context) (string, string) {
	out, err := n.run(ctx, "ip", "-4", "route", "show", "default")
	if err != nil {
		return "", ""
	}
	for line := range strings.SplitSeq(out, "\n") {
		if m := viaDevRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			dev := ""
			if d := devRE.FindStringSubmatch(line); d != nil {
				dev = d[1]
			}
			return m[1], dev
		}
	}
	return "", ""
}

// bridgeAddress4 is the node's IPv4 address on the bridge, with its prefix.
func (n *Node) bridgeAddress4(ctx context.Context, bridge string) netip.Prefix {
	out, err := n.run(ctx, "ip", "-4", "-o", "addr", "show", "dev", bridge)
	if err != nil {
		return netip.Prefix{}
	}
	if m := inetRE.FindStringSubmatch(out); m != nil {
		p, _ := netip.ParsePrefix(m[1])
		return p
	}
	return netip.Prefix{}
}

func (n *Node) bridgeAddress6(ctx context.Context, bridge string) string {
	out, err := n.run(ctx, "ip", "-6", "-o", "addr", "show", "dev", bridge, "scope", "global")
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "temporary") || strings.Contains(line, "deprecated") || strings.Contains(line, "tentative") {
			continue
		}
		if m := inet6RE.FindStringSubmatch(line); m != nil {
			if p, err := netip.ParsePrefix(m[1]); err == nil {
				return p.Addr().String()
			}
		}
	}
	return ""
}

// bridgeIsDHCP reports whether /etc/network/interfaces configures the bridge by DHCP.
func (n *Node) bridgeIsDHCP(bridge string) bool {
	data, err := n.readFile(interfaces)
	if err != nil {
		return false
	}
	re := regexp.MustCompile(`(?m)^\s*iface\s+` + regexp.QuoteMeta(bridge) + `\s+inet\s+dhcp\b`)
	return re.Match(data)
}

func (n *Node) resolvers() ([]string, bool) {
	data, err := n.readFile(resolvConf)
	if err != nil {
		return nil, false
	}
	var all []string
	loopbackOnly := true
	for line := range strings.SplitSeq(string(data), "\n") {
		if m := nameserRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			all = append(all, m[1])
			if !strings.HasPrefix(m[1], "127.") && m[1] != "::1" {
				loopbackOnly = false
			}
		}
	}
	return all, loopbackOnly && len(all) > 0
}

func lastAddress(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := ipv4Bits - p.Bits()
	v := binary.BigEndian.Uint32(a[:])
	v |= (1 << host) - 1
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v)
	return netip.AddrFrom4(out)
}

// clusterAddresses lists the addresses this node knows of the cluster's nodes: its own, and each
// other node's cluster address and those its name resolves to.
func (n *Node) clusterAddresses(ctx context.Context) []Member {
	seen := map[Member]bool{}
	var members []Member
	add := func(address, node string) {
		m := Member{Address: address, Node: node}
		if address != "" && !strings.HasPrefix(address, "127.") && !seen[m] {
			seen[m] = true
			members = append(members, m)
		}
	}
	n.ownAddresses(ctx, func(address string) { add(address, n.Hostname) })
	n.peerAddresses(ctx, add)
	return members
}

// ownAddresses reports the global addresses of this node's interfaces.
func (n *Node) ownAddresses(ctx context.Context, add func(address string)) {
	for _, args := range [][]string{{"-4", "-o", "addr", "show"}, {"-6", "-o", "addr", "show", "scope", "global"}} {
		out, err := n.run(ctx, "ip", args...)
		if err != nil {
			continue
		}
		re := inetRE
		if args[0] == "-6" {
			re = inet6RE
		}
		for line := range strings.SplitSeq(out, "\n") {
			if strings.Contains(line, "temporary") || strings.Contains(line, "deprecated") || strings.Contains(line, "tentative") {
				continue
			}
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if p, perr := netip.ParsePrefix(m[1]); perr == nil {
				add(p.Addr().String())
			}
		}
	}
}

// peerAddresses reports the cluster address of every other node and the addresses its name
// resolves to.
func (n *Node) peerAddresses(ctx context.Context, add func(address, node string)) {
	out, err := n.run(ctx, "pvesh", "get", "/cluster/status", "--output-format", "json")
	if err != nil {
		return
	}
	for _, m := range jsonList(out) {
		name := str(m, "name")
		if str(m, "type") != "node" || str(m, "local") == "1" || name == "" {
			continue
		}
		add(str(m, "ip"), name)
		addrs, lerr := n.LookupHost(ctx, name)
		if lerr != nil {
			continue
		}
		for _, a := range addrs {
			if p, perr := netip.ParseAddr(a); perr == nil && !p.Is4In6() {
				add(p.String(), name)
			}
		}
	}
}
