package installer

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"
)

func TestDecideCANamesWhatTheCertificateIsValidFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, opts{})
	given := filepath.Join(h.root, clusterCA)

	for name, cfg := range map[string]*Config{"the cluster's CA": {}, "a CA given": {ProxmoxCA: given}} {
		_, refused := h.node.DecideCA(ctx, cfg, "https://192.0.2.99:8006")
		if refused == nil {
			t.Fatalf("%s: an address the certificate lacks was accepted", name)
		}
		want := "Proxmox VE's certificate is valid for 192.0.2.10, pve1.example.com, not 192.0.2.99. Pass one of those with --proxmox-url"
		if refused.Error() != want {
			t.Errorf("%s: %q, want %q", name, refused, want)
		}
	}

	_, err := h.node.DecideCA(ctx, &Config{}, "https://pve2.example.com:8006")
	if err == nil || err.Error() != "Proxmox VE's certificate is valid for 192.0.2.10, pve1.example.com, not pve2.example.com. Pass one of those with --proxmox-url" {
		t.Errorf("a name the certificate lacks: %v", err)
	}
	if _, err = h.node.DecideCA(ctx, &Config{}, "https://192.0.2.10:8006"); err != nil {
		t.Errorf("an address the certificate lists: %v", err)
	}
}

func TestDecideCAOfACertificateThatListsOnlyLoopback(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{names: []string{"localhost"}, ips: []string{"127.0.0.1", "::1"}})
	_, err := h.node.DecideCA(context.Background(), &Config{}, "https://192.0.2.10:8006")
	want := "Proxmox VE's certificate lists no reachable address, only loopback, so it is not valid for 192.0.2.10. Reissue it on the node with pvecm updatecerts --force, then systemctl restart pveproxy"
	if err == nil || err.Error() != want {
		t.Errorf("a certificate of loopback alone: %v, want %q", err, want)
	}
}

// servedByNode makes the node's own certificate the one the API serves, as Proxmox VE leaves it.
func servedByNode(t *testing.T, h *harness) {
	t.Helper()
	if err := os.Rename(filepath.Join(h.root, pveServedPEM), filepath.Join(h.root, pveNodePEM)); err != nil {
		t.Fatal(err)
	}
}

// drawn is the screen as a terminal of this size shows it: each line clipped at the width, and the
// top lines gone when there are more than the height.
func drawn(screen string, width, height int) []string {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "")
	}
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	return lines
}

func TestTheSummaryShowsTheIPv6CautionOnA80By24Screen(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	s := h.session(Config{PVEEntry: "no", ProxmoxURL: "https://pve1.example.com:8006"})
	s.Cautions = []string{apiFamilyCaution("192.0.2.10", "2001:db8::10")}
	model := NewModel(s, func(emit func(Event)) *Runner { return h.runner(s, emit) })
	rec := &recorder{inner: model}
	tm := teatest.NewTestModel(t, rec, teatest.WithInitialTermSize(80, 24))
	u := &ui{t: t, h: h, s: s, rec: rec, tm: tm, w: 80, l: 24}
	u.waitFor("Node")
	u.settle()
	for range 12 {
		if strings.Contains(u.rec.screen(), "If it fails") {
			break
		}
		u.enter(1)
	}
	u.waitFor("If it fails")
	u.settle()
	screen := strings.Join(drawn(rec.screen(), 80, 24), "\n")
	flat := strings.Join(strings.Fields(screen), " ")
	for _, want := range []string{"Kubehenge", "Note", "cannot reach the API at 2001:db8::10", "--proxmox-url https://192.0.2.10:8006", "Install"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the 80 by 24 screen lacks %q:\n%s", want, screen)
		}
	}
}

func TestAPIFamilyNote(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		own  bool
		ips  []string
		host string
		want bool
	}{
		{"IPv6 chosen, the bridge's IPv4 address unlisted", true, []string{"2001:db8::10"}, "2001:db8::10", true},
		{"the bridge's IPv4 address listed too", true, []string{"2001:db8::10", "192.0.2.10"}, "2001:db8::10", false},
		{"IPv4 chosen", true, []string{"192.0.2.10"}, "192.0.2.10", false},
		{"a name chosen", true, []string{"2001:db8::10"}, "pve1.example.com", false},
		{"a certificate of the administrator's", false, []string{"2001:db8::10"}, "2001:db8::10", false},
	} {
		h := newHarness(t, opts{names: []string{"pve1.example.com"}, ips: c.ips})
		if c.own {
			servedByNode(t, h)
		}
		s := h.session(Config{APIHost: c.host, Bridge: "vmbr0", CTID: "105"})
		s.Facts = &Facts{Prefix: netip.MustParsePrefix("192.0.2.10/24")}
		s.noteAPIFamily()
		if !c.want {
			if len(s.Cautions) != 0 {
				t.Errorf("%s: noted %q", c.name, s.Cautions)
			}
			continue
		}
		want := "The certificate lacks 192.0.2.10, so clusters without IPv6 cannot reach the API at 2001:db8::10. Add it to the certificate, then pass --proxmox-url https://192.0.2.10:8006"
		if len(s.Cautions) != 1 || s.Cautions[0] != want {
			t.Errorf("%s: noted %q, want %q", c.name, s.Cautions, want)
		}
	}
}

func TestChangingTheAPIAddressRecomputesTheCautions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, opts{names: []string{"pve1.example.com"}, ips: []string{"2001:db8::10"}, hosts: map[string][]string{"pve1.example.com": {"2001:db8::10"}}})
	servedByNode(t, h)
	s := h.session(Config{Bridge: "vmbr0", APIHost: "2001:db8::10"})
	s.Facts = &Facts{Prefix: netip.MustParsePrefix("192.0.2.10/24")}
	change := func(host string) {
		t.Helper()
		s.Cfg.APIHost, s.Cfg.ProxmoxURL, s.CA = host, "", nil
		if err := s.ResolveAPI(ctx, AskTies); err != nil {
			t.Fatal(err)
		}
	}
	change("2001:db8::10")
	change("2001:db8::10")
	if len(s.Cautions) != 1 {
		t.Errorf("the IPv6 address picked twice: %d cautions, want 1", len(s.Cautions))
	}
	change("pve1.example.com")
	if len(s.Cautions) != 0 {
		t.Errorf("a name picked after it: cautions %q, want none", s.Cautions)
	}
}
