package ui

import (
	"context"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// serveLocalhostAt answers every A query with 10.9.9.9 and every other query with no records, on a
// loopback UDP port it returns.
func serveLocalhostAt(t *testing.T) string {
	t.Helper()
	c, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			var q dnsmessage.Message
			if q.Unpack(buf[:n]) != nil || len(q.Questions) != 1 {
				continue
			}
			var reply dnsmessage.Message
			reply.ID, reply.Response, reply.RecursionAvailable = q.ID, true, true
			reply.Questions = q.Questions
			if q.Questions[0].Type == dnsmessage.TypeA {
				h := dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}
				reply.Answers = []dnsmessage.Resource{{Header: h, Body: &dnsmessage.AResource{A: [4]byte{10, 9, 9, 9}}}}
			}
			if out, err := reply.Pack(); err == nil {
				_, _ = c.WriteTo(out, from)
			}
		}
	}()
	_, port, _ := net.SplitHostPort(c.LocalAddr().String())
	return port
}

// The appliance's own resolution of the URL's host is read whether or not the user may read the
// firewall, so the check can tell that the cluster's DNS servers disagree with it. The user here
// may not read it.
func TestSetupCheckComparesTheServersAnswerWithTheAppliancesOwn(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	h.accounts.reachErr = pve.ErrForbidden
	h.server.Appliance.ProxmoxURL = "https://localhost:8006"
	h.server.DNSPort = serveLocalhostAt(t)
	f := checkSettings(t, b, strings.Replace(preflightSettings, `"dnsServers":["10.0.0.1"]`, `"dnsServers":["127.0.0.1"]`, 1))
	if !strings.Contains(strings.Join(f.Warnings, "\n"), "resolve localhost to 10.9.9.9") {
		t.Errorf("findings = %+v, want the servers and the appliance to disagree", f)
	}
}
