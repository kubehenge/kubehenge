package appliance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const defaultDNSTimeout = 3 * time.Second

// dnsAnswer is what one DNS server said of the Proxmox VE URL's host.
type dnsAnswer struct {
	server string
	addrs  []netip.Addr
	// known is false when the server did not answer, so that it said nothing of the name.
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

// dnsCandidates are the names a management VM's resolver tries for the host: a name without dots as
// given and then with each search domain of the settings, any other name as given only.
func (s *Settings) dnsCandidates(host string) []string {
	names := []string{host}
	if strings.Contains(host, ".") {
		return names
	}
	for _, d := range s.SearchDomains {
		if d = strings.Trim(d, "."); d != "" {
			names = append(names, host+"."+d)
		}
	}
	return names
}

// askDNS asks one server for the candidate names in turn, and the first answer with addresses wins.
// A server that does not answer ends the asking, as its next query would wait as long. The queries
// go to that server alone and to rooted names, so that no search domain of the appliance applies.
func (inv *Inventory) askDNS(server string, names []string) dnsAnswer {
	answer := dnsAnswer{server: server, known: true}
	for _, name := range names {
		a := inv.askDNSName(server, name)
		if len(a.addrs) > 0 {
			return a
		}
		answer = a
		if !a.known {
			break
		}
	}
	return answer
}

func (inv *Inventory) askDNSName(server, host string) dnsAnswer {
	port, timeout := inv.DNSPort, inv.DNSTimeout
	if port == "" {
		port = "53"
	}
	if timeout <= 0 {
		timeout = defaultDNSTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort(server, port))
	}}
	addrs, err := resolver.LookupNetIP(ctx, "ip", strings.TrimSuffix(host, ".")+".")
	a := dnsAnswer{server: server, known: true}
	var dnsErr *net.DNSError
	switch {
	case err == nil:
		for _, ip := range addrs {
			a.addrs = append(a.addrs, ip.Unmap())
		}
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		a.problem = "does not know it"
	default:
		a.problem, a.known = "did not answer", false
	}
	return a
}
