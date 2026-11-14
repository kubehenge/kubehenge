package appliance

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
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
	// servfail names get SERVFAIL.
	servfail []string
	// truncate has UDP replies carry no answers and the truncation flag, so that a client asks again
	// over TCP.
	truncate bool
	// strays has each answer preceded by a reply with another ID and one to another question.
	strays bool
	// asked counts the queries, when set.
	asked *atomic.Int32
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
		if fake.truncate {
			l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort(host, port))
			if err != nil {
				t.Skipf("cannot listen on %s: %v", host, err)
			}
			t.Cleanup(func() { _ = l.Close() })
			go fake.serveTCP(l)
		}
	}
	return port, servers
}

func (d fakeDNS) serveTCP(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			var head [2]byte
			if _, err := io.ReadFull(conn, head[:]); err != nil {
				return
			}
			raw := make([]byte, binary.BigEndian.Uint16(head[:]))
			if _, err := io.ReadFull(conn, raw); err != nil {
				return
			}
			var q dnsmessage.Message
			if q.Unpack(raw) != nil || len(q.Questions) != 1 {
				return
			}
			if out, err := d.answer(&q).Pack(); err == nil {
				_, _ = conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...)) //nolint:gosec // a short test reply
			}
		}()
	}
}

// answer is the reply to a query.
func (d fakeDNS) answer(q *dnsmessage.Message) *dnsmessage.Message {
	if d.asked != nil {
		d.asked.Add(1)
	}
	question := q.Questions[0]
	var reply dnsmessage.Message
	reply.ID, reply.Response, reply.Authoritative, reply.RCode = q.ID, true, true, dnsmessage.RCodeNameError
	reply.Questions = q.Questions
	if slices.Contains(d.servfail, question.Name.String()) {
		reply.RCode = dnsmessage.RCodeServerFailure
		return &reply
	}
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
	return &reply
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

// sendStrays sends two replies that are not the answer, each with 10.9.9.9: one with another ID and
// one to another question.
func sendStrays(c net.PacketConn, to net.Addr, q *dnsmessage.Message) {
	for _, other := range []bool{false, true} {
		var stray dnsmessage.Message
		stray.ID, stray.Response, stray.RCode = q.ID, true, dnsmessage.RCodeSuccess
		stray.Questions = slices.Clone(q.Questions)
		name := q.Questions[0].Name
		if other {
			stray.Questions[0].Name = dnsmessage.MustNewName("other.example.")
		} else {
			stray.ID++
		}
		h := dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}
		stray.Answers = []dnsmessage.Resource{{Header: h, Body: &dnsmessage.AResource{A: [4]byte{10, 9, 9, 9}}}}
		if out, err := stray.Pack(); err == nil {
			_, _ = c.WriteTo(out, to)
		}
	}
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
		reply := d.answer(&q)
		if d.truncate {
			reply.Answers, reply.Truncated, reply.RCode = nil, true, dnsmessage.RCodeSuccess
		}
		if d.strays {
			sendStrays(c, from, &q)
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

// hostsFileName is a dotted name this machine's hosts file holds, or empty.
func hostsFileName() string {
	path := "/etc/hosts"
	if runtime.GOOS == "windows" {
		path = os.Getenv("SystemRoot") + `\System32\drivers\etc\hosts`
	}
	text, err := os.ReadFile(path) //nolint:gosec // the machine's own hosts file
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(text), "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		for _, name := range fields[min(1, len(fields)):] {
			if strings.Contains(name, ".") && !strings.HasSuffix(name, ".") {
				return name
			}
		}
	}
	return ""
}

func TestPreflightDNSIgnoresTheAppliancesHostsFile(t *testing.T) {
	t.Parallel()
	name := hostsFileName()
	if name == "" {
		t.Skip("the hosts file holds no dotted name")
	}
	var asked atomic.Int32
	f := dnsCase(t, "https://"+name+":8006", fakeDNS{asked: &asked})
	mentions(t, f.Errors, name+" does not resolve", "127.0.0.1 does not know it")
	if asked.Load() == 0 {
		t.Error("the server was never asked")
	}
}

func TestPreflightDNSSkipsRepliesThatAreNotTheAnswer(t *testing.T) {
	t.Parallel()
	port, servers := serveDNS(t, fakeDNS{strays: true, records: map[string][]netip.Addr{pveName: {netip.MustParseAddr("10.0.0.5")}}})
	s := settings("pve1")
	s.DNSServers = servers
	inv := healthy()
	inv.ProxmoxURL, inv.DNSPort, inv.DNSTimeout = nameURL, port, 500*time.Millisecond
	inv.Route = APIRoute{Endpoints: []netip.Addr{netip.MustParseAddr("10.0.0.5")}}
	if f := s.Preflight(inv, false); len(f.Errors) != 0 || len(f.Warnings) != 0 {
		t.Errorf("findings = %+v", f)
	}
}

func TestPreflightDNSDottedNameTriesTheSearchDomains(t *testing.T) {
	t.Parallel()
	port, servers := serveDNS(t, fakeDNS{records: map[string][]netip.Addr{"pve1.dc1.example.com.": {netip.MustParseAddr("10.0.0.5")}}})
	s := settings("pve1")
	s.DNSServers, s.SearchDomains = servers, []string{"example.com"}
	inv := healthy()
	inv.ProxmoxURL, inv.DNSPort = "https://pve1.dc1:8006", port
	if f := s.Preflight(inv, false); len(f.Errors) != 0 || len(f.Warnings) != 0 {
		t.Errorf("findings = %+v", f)
	}
}

func TestPreflightDNSServfailOnOneNameDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()
	port, servers := serveDNS(t,
		fakeDNS{servfail: []string{"pvevip."}, records: map[string][]netip.Addr{"pvevip.corp.example.": {netip.MustParseAddr("10.0.0.5")}}},
		fakeDNS{})
	s := settings("pve1")
	s.DNSServers, s.SearchDomains = servers, []string{"corp.example"}
	inv := healthy()
	inv.ProxmoxURL, inv.DNSPort, inv.DNSTimeout = "https://pvevip:8006", port, 500*time.Millisecond
	if f := s.Preflight(inv, false); len(f.Errors) != 0 {
		t.Errorf("errors = %v", f.Errors)
	}
}

func TestPreflightDNSAServerThatOnlyServfailsWarnsLikeASilentOne(t *testing.T) {
	t.Parallel()
	f := dnsCase(t, nameURL, fakeDNS{servfail: []string{pveName}})
	if len(f.Errors) != 0 {
		t.Errorf("errors = %v", f.Errors)
	}
	mentions(t, f.Warnings, "Could not check from the appliance", "127.0.0.1")
}

func TestPreflightDNSAsksAgainOverTCPWhenTheReplyIsTruncated(t *testing.T) {
	t.Parallel()
	f := dnsCase(t, nameURL, fakeDNS{truncate: true, records: map[string][]netip.Addr{pveName: {netip.MustParseAddr("10.0.0.5")}}})
	if len(f.Errors) != 0 || len(f.Warnings) != 0 {
		t.Errorf("findings = %+v", f)
	}
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

func TestPreflightISOStorageCountsTheProxmoxVENodesNotTheManagementNodes(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.ProxmoxHA = true
	inv := healthy()
	inv.Nodes = append(inv.Nodes, pve.Node{Node: "pve3", Online: true, MaxMemory: 64 * gib})
	inv.Storages[0].Shared = true
	mentions(t, s.Preflight(inv, false).Warnings, "local to a node")
}

func TestPreflightISOStorageOneManagementNodeWithoutHAStillMovesAcrossProxmoxVENodes(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	inv := healthy()
	inv.Nodes = append(inv.Nodes, pve.Node{Node: "pve3", Online: true, MaxMemory: 64 * gib})
	inv.Storages[0].Shared = true
	mentions(t, s.Preflight(inv, false).Warnings, "local to a node")
}

func TestPreflightISOStorageOneProxmoxVENodeHasNowhereToMoveTo(t *testing.T) {
	t.Parallel()
	s := settings("pve1")
	s.ProxmoxHA = true
	inv := healthy()
	inv.Nodes = inv.Nodes[:1]
	inv.Storages[0].Shared = true
	for _, w := range s.Preflight(inv, false).Warnings {
		t.Errorf("warning = %s", w)
	}
}
