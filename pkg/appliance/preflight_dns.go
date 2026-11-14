package appliance

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	defaultDNSTimeout = 3 * time.Second
	// maxUDPReply is the largest reply read over UDP. Larger ones come truncated, and are asked again
	// over TCP.
	maxUDPReply = 4096
)

// dnsAnswer is what one DNS server said of the Proxmox VE URL's host.
type dnsAnswer struct {
	server string
	addrs  []netip.Addr
	// known is false when the server gave no usable answer to any name asked, so that it said nothing
	// of the host.
	known bool
	// problem is empty for an answer. Otherwise it says what the server did.
	problem string
}

// checkURLResolves checks that the DNS servers the management VMs use resolve the Proxmox VE URL's
// host. The appliance may know a name from its own hosts file or resolver, while Cluster API, the
// cloud controller manager and the CSI plugin resolve it with these servers only.
func (s *Settings) checkURLResolves(inv *Inventory, f *Findings) {
	u, err := url.Parse(inv.ProxmoxURL)
	if err != nil || u.Hostname() == "" || len(s.DNSServers) == 0 {
		return
	}
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return
	}
	answers := make([]dnsAnswer, len(s.DNSServers))
	var wg sync.WaitGroup
	for i, server := range s.DNSServers {
		wg.Go(func() { answers[i] = inv.askDNS(server, s.dnsCandidates(host)) })
	}
	wg.Wait()
	var got []netip.Addr
	var failures, silent []string
	denied := false
	for _, a := range answers {
		got = append(got, a.addrs...)
		if a.problem != "" {
			failures = append(failures, a.server+" "+a.problem)
			denied = denied || a.known
			if !a.known {
				silent = append(silent, a.server)
			}
		}
	}
	if len(got) == 0 && !denied {
		// The appliance may sit on a network the servers do not answer, or behind a firewall that drops
		// its queries, while the management VMs reach them.
		f.Warnings = append(f.Warnings, fmt.Sprintf("Could not check from the appliance whether %s resolves, since the DNS servers for the management cluster (%s) did not answer it. If they do not know the name, Cluster API, the cloud controller manager and the CSI plugin cannot reach Proxmox VE once setup is done. Use an IP address in the URL, or check the name from a management VM's network.", host, strings.Join(silent, ", ")))
		return
	}
	if len(got) == 0 {
		f.Errors = append(f.Errors, fmt.Sprintf("The Proxmox VE address %s does not resolve on the DNS servers for the management cluster (%s). Cluster API, the cloud controller manager and the CSI plugin resolve it there and cannot reach Proxmox VE. Use an IP address in the URL, or add a DNS record for %s to a server.", host, strings.Join(failures, ", "), host))
		return
	}
	if len(inv.Route.Endpoints) > 0 && !slices.ContainsFunc(got, func(a netip.Addr) bool { return slices.Contains(inv.Route.Endpoints, a) }) {
		f.Warnings = append(f.Warnings, fmt.Sprintf("The DNS servers for the management cluster resolve %s to %s, but the appliance resolves it to %s. The management cluster would reach another host than the appliance does. Make the records match, or use an IP address in the URL.", host, joinAddrs(got), joinAddrs(inv.Route.Endpoints)))
	}
}

func joinAddrs(addrs []netip.Addr) string {
	sorted := slices.Compact(slices.SortedFunc(slices.Values(addrs), netip.Addr.Compare))
	out := make([]string, len(sorted))
	for i, a := range sorted {
		out[i] = a.String()
	}
	return strings.Join(out, ", ")
}

// dnsCandidates are the names a management VM's resolver may try for the host: the name as given and
// then with each search domain of the settings.
func (s *Settings) dnsCandidates(host string) []string {
	names := []string{host}
	for _, d := range s.SearchDomains {
		if d = strings.Trim(d, "."); d != "" {
			names = append(names, host+"."+d)
		}
	}
	return names
}

// outcome is what a server did with one query.
type outcome int

const (
	// found is an answer with addresses.
	found outcome = iota
	// absent is NXDOMAIN, or an answer without addresses.
	absent
	// failed is silence, an error or a SERVFAIL or REFUSED, which say nothing of the name.
	failed
)

// askDNS asks one server for every candidate name, which are asked at once. The first candidate with
// addresses wins. The server does not know the host only when it said so of every candidate.
func (inv *Inventory) askDNS(server string, names []string) dnsAnswer {
	results := make([]dnsAnswer, len(names))
	outcomes := make([]outcome, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() { results[i], outcomes[i] = inv.askDNSName(server, name) })
	}
	wg.Wait()
	for i := range names {
		if outcomes[i] == found {
			return results[i]
		}
	}
	if slices.Contains(outcomes, failed) {
		return dnsAnswer{server: server, problem: "did not answer"}
	}
	return dnsAnswer{server: server, known: true, problem: "does not know it"}
}

// askDNSName asks for a name's A and AAAA records at once. The resolver of the appliance is left
// out, as its hosts file and search list are not the VMs'.
func (inv *Inventory) askDNSName(server, name string) (dnsAnswer, outcome) {
	var results [2]struct {
		addrs []netip.Addr
		out   outcome
	}
	var wg sync.WaitGroup
	for i, t := range [...]dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		wg.Go(func() { results[i].addrs, results[i].out = inv.queryDNS(server, name, t) })
	}
	wg.Wait()
	addrs := slices.Concat(results[0].addrs, results[1].addrs)
	switch {
	case len(addrs) > 0:
		return dnsAnswer{server: server, addrs: addrs, known: true}, found
	case results[0].out == failed || results[1].out == failed:
		return dnsAnswer{server: server}, failed
	default:
		return dnsAnswer{server: server}, absent
	}
}

// queryDNS sends one query over UDP, and over TCP when the reply is truncated.
func (inv *Inventory) queryDNS(server, name string, qtype dnsmessage.Type) ([]netip.Addr, outcome) {
	port, timeout := inv.DNSPort, inv.DNSTimeout
	if port == "" {
		port = "53"
	}
	if timeout <= 0 {
		timeout = defaultDNSTimeout
	}
	qname, err := dnsmessage.NewName(strings.TrimSuffix(name, ".") + ".")
	if err != nil {
		return nil, absent
	}
	var id [2]byte
	if _, err = rand.Read(id[:]); err != nil {
		return nil, failed
	}
	var query dnsmessage.Message
	query.ID, query.RecursionDesired = binary.BigEndian.Uint16(id[:]), true
	query.Questions = []dnsmessage.Question{{Name: qname, Type: qtype, Class: dnsmessage.ClassINET}}
	packet, err := query.Pack()
	if err != nil {
		return nil, absent
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	address := net.JoinHostPort(server, port)
	reply, ok := exchange(ctx, "udp", address, packet)
	if ok && reply.Truncated {
		reply, ok = exchange(ctx, "tcp", address, packet)
	}
	if !ok || reply.ID != query.ID || !reply.Response {
		return nil, failed
	}
	if reply.RCode == dnsmessage.RCodeNameError {
		return nil, absent
	}
	if reply.RCode != dnsmessage.RCodeSuccess {
		return nil, failed
	}
	var addrs []netip.Addr
	for i := range reply.Answers {
		if a, isA := reply.Answers[i].Body.(*dnsmessage.AResource); isA {
			addrs = append(addrs, netip.AddrFrom4(a.A))
		}
		if a, isAAAA := reply.Answers[i].Body.(*dnsmessage.AAAAResource); isAAAA {
			addrs = append(addrs, netip.AddrFrom16(a.AAAA).Unmap())
		}
	}
	if len(addrs) == 0 {
		return nil, absent
	}
	return addrs, found
}

// exchange sends a packet and reads the reply, over TCP with the two-byte length that stream
// transport adds.
// ok is false for any failure, which the check files as a server that did not answer.
func exchange(ctx context.Context, network, address string, packet []byte) (dnsmessage.Message, bool) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, address)
	if err != nil {
		return dnsmessage.Message{}, false
	}
	defer func() { _ = conn.Close() }()
	if deadline, has := ctx.Deadline(); has && conn.SetDeadline(deadline) != nil {
		return dnsmessage.Message{}, false
	}
	var raw []byte
	if network == "tcp" {
		raw = exchangeStream(conn, packet)
	} else {
		raw = exchangeDatagram(conn, packet)
	}
	var m dnsmessage.Message
	if raw == nil || m.Unpack(raw) != nil {
		return dnsmessage.Message{}, false
	}
	return m, true
}

func exchangeDatagram(conn net.Conn, packet []byte) []byte {
	if _, err := conn.Write(packet); err != nil {
		return nil
	}
	buf := make([]byte, maxUDPReply)
	n, err := conn.Read(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

func exchangeStream(conn net.Conn, packet []byte) []byte {
	framed := binary.BigEndian.AppendUint16(nil, uint16(len(packet))) //nolint:gosec // a query is a few hundred bytes
	if _, err := conn.Write(append(framed, packet...)); err != nil {
		return nil
	}
	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return nil
	}
	raw := make([]byte, binary.BigEndian.Uint16(head[:]))
	if _, err := io.ReadFull(conn, raw); err != nil {
		return nil
	}
	return raw
}
