package appliance

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// fakeDNS is a DNS server on a loopback address. It answers the names in records, says NXDOMAIN to
// any other, and never answers when silent.
type fakeDNS struct {
	records map[string][]netip.Addr
	silent  bool
}

// serveDNS starts the fakes on 127.0.0.1, 127.0.0.2 and so on, all on one port, and returns that port
// and the addresses.
func serveDNS(t *testing.T, fakes ...fakeDNS) (string, []string) {
	t.Helper()
	port := "0"
	servers := make([]string, 0, len(fakes))
	for i, fake := range fakes {
		host := fmt.Sprintf("127.0.0.%d", i+1)
		c := listenUDP(t, net.JoinHostPort(host, port))
		_, port, _ = net.SplitHostPort(c.LocalAddr().String())
		servers = append(servers, host)
		go fake.serve(c)
	}
	return port, servers
}

// listenUDP listens on a loopback address and closes the socket when the test ends.
func listenUDP(t *testing.T, address string) net.PacketConn {
	t.Helper()
	c, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", address)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", address, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (d fakeDNS) serve(c net.PacketConn) {
	buf := make([]byte, 512)
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return
		}
		var q dnsmessage.Message
		if d.silent || q.Unpack(buf[:n]) != nil || len(q.Questions) != 1 {
			continue
		}
		question := q.Questions[0]
		var reply dnsmessage.Message
		reply.ID, reply.Response, reply.Authoritative, reply.RCode = q.ID, true, true, dnsmessage.RCodeNameError
		reply.Questions = q.Questions
		if addrs, ok := d.records[question.Name.String()]; ok {
			reply.RCode = dnsmessage.RCodeSuccess
			for _, a := range addrs {
				h := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 60}
				switch {
				case a.Is4() && question.Type == dnsmessage.TypeA:
					h.Type = dnsmessage.TypeA
					reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.AResource{A: a.As4()}})
				case a.Is6() && question.Type == dnsmessage.TypeAAAA:
					h.Type = dnsmessage.TypeAAAA
					reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
				}
			}
		}
		if out, err := reply.Pack(); err == nil {
			_, _ = c.WriteTo(out, from)
		}
	}
}

const pveName = "pve.example.org."

func knows(addrs ...string) fakeDNS {
	d := fakeDNS{records: map[string][]netip.Addr{pveName: {}}}
	for _, a := range addrs {
		d.records[pveName] = append(d.records[pveName], netip.MustParseAddr(a))
	}
	return d
}

// dnsCase runs the preflight against fake servers and returns its findings.
func dnsCase(t *testing.T, url string, fakes ...fakeDNS) Findings {
	t.Helper()
	port, servers := serveDNS(t, fakes...)
	s := settings("pve1")
	s.DNSServers = servers
	inv := healthy()
	inv.ProxmoxURL, inv.DNSPort, inv.DNSTimeout = url, port, 500*time.Millisecond
	return s.Preflight(inv, false)
}

const nameURL = "https://pve.example.org:8006"

func TestPreflightDNSAnIPInTheURLNeedsNoLookup(t *testing.T) {
	t.Parallel()
	f := dnsCase(t, "https://10.0.0.5:8006", fakeDNS{silent: true})
	if len(f.Errors) != 0 {
		t.Errorf("errors = %v", f.Errors)
	}
}

func TestPreflightDNSEveryServerResolvesTheName(t *testing.T) {
	t.Parallel()
	f := dnsCase(t, nameURL, knows("10.0.0.5"), knows("10.0.0.5"))
	if len(f.Errors) != 0 || len(f.Warnings) != 0 {
		t.Errorf("findings = %+v", f)
	}
}

func TestPreflightDNSNoServerResolvesTheName(t *testing.T) {
	t.Parallel()
	f := dnsCase(t, nameURL, fakeDNS{}, fakeDNS{})
	mentions(t, f.Errors, "pve.example.org", "127.0.0.1 does not know it", "127.0.0.2 does not know it", "IP address in the URL", "DNS record")
}

func TestPreflightDNSOneOfTwoServersIsEnough(t *testing.T) {
	t.Parallel()
	f := dnsCase(t, nameURL, fakeDNS{}, knows("10.0.0.5"))
	if len(f.Errors) != 0 {
		t.Errorf("errors = %v", f.Errors)
	}
}

func TestPreflightDNSASilentServerIsUnreachableNotUnknown(t *testing.T) {
	t.Parallel()
	f := dnsCase(t, nameURL, fakeDNS{silent: true}, fakeDNS{})
	mentions(t, f.Errors, "127.0.0.1 did not answer", "127.0.0.2 does not know it")
}

func TestPreflightDNSEverySilentServerWarnsInsteadOfRefusing(t *testing.T) {
	t.Parallel()
	f := dnsCase(t, nameURL, fakeDNS{silent: true}, fakeDNS{silent: true})
	if len(f.Errors) != 0 {
		t.Errorf("errors = %v", f.Errors)
	}
	mentions(t, f.Warnings, "Could not check from the appliance", "pve.example.org", "127.0.0.1, 127.0.0.2", "cannot reach Proxmox VE")
}

// shortNameCase asks for the name pvevip with the search domain corp.example.
func shortNameCase(t *testing.T, fake fakeDNS) Findings {
	t.Helper()
	port, servers := serveDNS(t, fake)
	s := settings("pve1")
	s.DNSServers, s.SearchDomains = servers, []string{"corp.example", "lab.example."}
	inv := healthy()
	inv.ProxmoxURL, inv.DNSPort, inv.DNSTimeout = "https://pvevip:8006", port, 500*time.Millisecond
	return s.Preflight(inv, false)
}

func TestPreflightDNSShortNameResolvesThroughASearchDomain(t *testing.T) {
	t.Parallel()
	f := shortNameCase(t, fakeDNS{records: map[string][]netip.Addr{"pvevip.lab.example.": {netip.MustParseAddr("10.0.0.5")}}})
	if len(f.Errors) != 0 || len(f.Warnings) != 0 {
		t.Errorf("findings = %+v", f)
	}
}

func TestPreflightDNSShortNameResolvingNowhereIsRefused(t *testing.T) {
	t.Parallel()
	f := shortNameCase(t, fakeDNS{})
	mentions(t, f.Errors, "pvevip does not resolve", "127.0.0.1 does not know it")
}

func TestPreflightDNSWarnsWhenTheServersAnswerDiffers(t *testing.T) {
	t.Parallel()
	port, servers := serveDNS(t, knows("10.9.9.9"))
	s := settings("pve1")
	s.DNSServers = servers
	inv := healthy()
	inv.ProxmoxURL, inv.DNSPort = nameURL, port
	inv.Route = APIRoute{Endpoints: []netip.Addr{netip.MustParseAddr("10.0.0.5")}}
	f := s.Preflight(inv, false)
	mentions(t, f.Warnings, "resolve pve.example.org to 10.9.9.9", "the appliance resolves it to 10.0.0.5")
	inv.Route = APIRoute{Endpoints: []netip.Addr{netip.MustParseAddr("10.9.9.9")}}
	if f = s.Preflight(inv, false); len(f.Warnings) != 0 {
		t.Errorf("same answer, warnings = %v", f.Warnings)
	}
}

func TestPreflightDNSIPv6OnlyServer(t *testing.T) {
	t.Parallel()
	c := listenUDP(t, "[::1]:0")
	go knows("fd00::5").serve(c)
	_, port, _ := net.SplitHostPort(c.LocalAddr().String())
	s := settings("pve1")
	s.DNSServers = []string{"::1"}
	inv := healthy()
	inv.ProxmoxURL, inv.DNSPort = nameURL, port
	if f := s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("errors = %v", f.Errors)
	}
	s.DNSServers = []string{"::2"}
	inv.DNSTimeout = 300 * time.Millisecond
	mentions(t, s.Preflight(inv, false).Warnings, "Could not check from the appliance", "::2")
}

func TestPreflightDNSDualStackServerList(t *testing.T) {
	t.Parallel()
	c := listenUDP(t, "[::1]:0")
	go knows("fd00::5").serve(c)
	_, port, _ := net.SplitHostPort(c.LocalAddr().String())
	go fakeDNS{}.serve(listenUDP(t, net.JoinHostPort("127.0.0.1", port)))
	s := settings("pve1")
	s.DNSServers = []string{"127.0.0.1", "::1"}
	inv := healthy()
	inv.ProxmoxURL, inv.DNSPort = nameURL, port
	if f := s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("the IPv6 server answers: %v", f.Errors)
	}
}

// isoCase returns the warnings for storages: disks and cloud-init images, shared or not, on the
// nodes.
func isoCase(vmShared, isoShared bool, nodes ...string) []string {
	s := settings(nodes[0])
	s.Nodes = nodes
	s.NodeAddresses = nil
	inv := healthy()
	inv.Storages = nil
	for _, n := range nodes {
		s.NodeAddresses = append(s.NodeAddresses, fmt.Sprintf("10.0.0.%d/24", 11+len(s.NodeAddresses)))
		inv.Storages = append(inv.Storages,
			pve.Storage{Storage: "local-lvm", Node: n, Type: "lvm", Shared: vmShared, Content: []string{"images"}, Active: true, Avail: 200 * gib},
			pve.Storage{Storage: "local", Node: n, Shared: isoShared, Content: []string{"iso", "import"}, Active: true, Avail: 50 * gib})
	}
	inv.Nodes = nil
	inv.Bridges = nil
	return s.Preflight(inv, false).Warnings
}

func TestPreflightISOStorageOneNodeNeedsNoSharing(t *testing.T) {
	t.Parallel()
	if w := isoCase(true, false, "pve1"); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}

func TestPreflightISOStorageSharedAndSharedIsFine(t *testing.T) {
	t.Parallel()
	if w := isoCase(true, true, "pve1", "pve2", "pve3"); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}

func TestPreflightISOStorageLocalBesideSharedDisksWarns(t *testing.T) {
	t.Parallel()
	mentions(t, isoCase(true, false, "pve1", "pve2", "pve3"), "storage local", "local to a node", "shared storage local-lvm", "shared storage with the iso content type")
}

func TestPreflightISOStorageLocalEverywhereIsUnchanged(t *testing.T) {
	t.Parallel()
	if w := isoCase(false, false, "pve1", "pve2", "pve3"); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}

func TestPreflightISOStorageHAAloneCountsAsMoreNodes(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.ProxmoxHA = true
	inv := healthy()
	inv.Storages[0].Shared = true
	mentions(t, s.Preflight(inv, false).Warnings, "local to a node")
}
