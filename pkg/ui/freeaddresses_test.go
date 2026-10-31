package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// answeringOn has hosts answer on the addresses given, and records what is probed.
type answeringOn struct {
	mu     sync.Mutex
	hosts  map[netip.Addr]bool
	probed []netip.Addr
}

func (a *answeringOn) probe(_ context.Context, addrs []netip.Addr) map[netip.Addr]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.probed = append(a.probed, addrs...)
	out := map[netip.Addr]bool{}
	for _, x := range addrs {
		if a.hosts[x] {
			out[x] = true
		}
	}
	return out
}

func TestFreeAddresses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	hosts := &answeringOn{hosts: map[netip.Addr]bool{netip.MustParseAddr("10.0.0.31"): true, netip.MustParseAddr("10.0.0.41"): true}}
	h.server.Probe = hosts.probe
	b := h.browser()
	b.signIn("dev@pve")
	// The cluster takes 10.0.0.20 to 10.0.0.29 on vmbr0, whose network the node reports as 10.0.0.0/24.
	expect(t, b.do("POST", "/api/clusters", createBody), http.StatusCreated)

	rec := b.do("POST", "/api/proxmox/nodes/pve1/bridges/vmbr0/free", `{"nodes":3,"check":["10.0.0.40-10.0.0.42","10.0.0.300","192.168.0.9"]}`)
	expect(t, rec, http.StatusOK)
	var view freeView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	// Past the other cluster's addresses, and past 10.0.0.31, where a host answers.
	want := freeView{Network: "10.0.0.0/24", Endpoint: "10.0.0.32", Addresses: "10.0.0.33-10.0.0.35", Answering: []string{"10.0.0.41"}}
	if !slices.Equal(view.Answering, want.Answering) || view.Network != want.Network || view.Endpoint != want.Endpoint || view.Addresses != want.Addresses {
		t.Fatalf("free = %+v, want %+v", view, want)
	}
	hosts.mu.Lock()
	for _, a := range hosts.probed {
		if a.Compare(netip.MustParseAddr("10.0.0.20")) >= 0 && a.Compare(netip.MustParseAddr("10.0.0.29")) <= 0 || a.String() == "192.168.0.9" {
			t.Errorf("%s was probed", a)
		}
	}
	hosts.mu.Unlock()

	for body, bridge := range map[string]string{`{"nodes":3,"vlan":10,"check":["10.0.0.41"]}`: "vmbr0", `{"nodes":3}`: "vmbr1"} {
		rec = b.do("POST", "/api/proxmox/nodes/pve1/bridges/"+bridge+"/free", body)
		expect(t, rec, http.StatusOK)
		if got := strings.TrimSpace(rec.Body.String()); got != `{"answering":[]}` {
			t.Errorf("%s on %s = %s, want nothing probed", body, bridge, got)
		}
	}
	rec = b.do("POST", "/api/proxmox/nodes/pve1/bridges/vmbr0/free", `{"nodes":0,"check":["10.0.0.41"]}`)
	expect(t, rec, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"network":"10.0.0.0/24","answering":["10.0.0.41"]}` {
		t.Errorf("checking alone = %s", got)
	}
	expect(t, b.do("POST", "/api/proxmox/nodes/pve1/bridges/vmbr0/free", `{"nodes":65}`), http.StatusBadRequest)

	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("POST", "/api/proxmox/nodes/pve1/bridges/vmbr0/free", `{"nodes":3}`), http.StatusForbidden)
}

func TestFreeRunSkipsWhatIsSpokenFor(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		network string
		taken   []string
		answer  []string
		n       int
		want    []string
	}{
		"from the twentieth":   {network: "192.0.2.0/24", n: 2, want: []string{"192.0.2.20", "192.0.2.21"}},
		"small network":        {network: "192.0.2.0/27", n: 2, want: []string{"192.0.2.1", "192.0.2.2"}},
		"around a taken range": {network: "192.0.2.0/27", taken: []string{"192.0.2.2-192.0.2.4"}, n: 2, want: []string{"192.0.2.5", "192.0.2.6"}},
		"across windows":       {network: "10.0.0.0/16", answer: []string{"10.0.0.51"}, n: 33, want: []string{"10.0.0.52", "10.0.0.84"}},
		"none free":            {network: "192.0.2.0/30", answer: []string{"192.0.2.1"}, n: 2},
		"point to point":       {network: "192.0.2.0/31", n: 2, want: []string{"192.0.2.0", "192.0.2.1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hosts := &answeringOn{hosts: map[netip.Addr]bool{}}
			for _, a := range c.answer {
				hosts.hosts[netip.MustParseAddr(a)] = true
			}
			var taken [][2]netip.Addr
			for _, r := range c.taken {
				from, to, _ := strings.Cut(r, "-")
				taken = append(taken, [2]netip.Addr{netip.MustParseAddr(from), netip.MustParseAddr(to)})
			}
			s := &Server{Probe: hosts.probe}
			run := s.freeRun(t.Context(), netip.MustParsePrefix(c.network), taken, c.n)
			var got []string
			if len(run) > 0 {
				got = []string{run[0].String(), run[len(run)-1].String()}
			}
			if len(run) > 0 && len(run) != c.n {
				t.Fatalf("run of %d, want %d", len(run), c.n)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("run from %v, want %v", got, c.want)
			}
		})
	}
}

func TestCheckedStaysOnTheNetwork(t *testing.T) {
	t.Parallel()
	network := netip.MustParsePrefix("10.0.0.0/24")
	got := checked([]string{"9.255.255.250-10.0.0.2", "10.0.0.255", "10.0.0.0/8", "nonsense"}, network)
	if len(got) != checkMost || got[0].String() != "10.0.0.1" || got[1].String() != "10.0.0.2" || got[2].String() != "10.0.0.1" {
		t.Fatalf("checked = %v", got)
	}
}

func TestAnswered(t *testing.T) {
	t.Parallel()
	refused := &net.OpError{Op: "dial", Net: "tcp4", Err: fmt.Errorf("connect: %w", syscall.ECONNREFUSED)}
	timeout := &net.OpError{Op: "dial", Net: "tcp4", Err: errors.New("i/o timeout")}
	if !answered(nil) || !answered(refused) || answered(timeout) || answered(context.DeadlineExceeded) {
		t.Fatal("answered tells a refused or made connection from one that reached no host wrongly")
	}
}
