package installer

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		for _, want := range []string{"is valid for 192.0.2.10, pve1.example.com, not 192.0.2.99", "pvecm updatecerts --force", "--proxmox-url"} {
			if !strings.Contains(refused.Error(), want) {
				t.Errorf("%s: %q lacks %q", name, refused, want)
			}
		}
	}

	_, err := h.node.DecideCA(ctx, &Config{}, "https://pve2.example.com:8006")
	if err == nil || !strings.Contains(err.Error(), "not pve2.example.com") || !strings.Contains(err.Error(), "install a certificate for pve2.example.com") || strings.Contains(err.Error(), "pvecm") {
		t.Errorf("a name the certificate lacks: %v, want a certificate of its own asked for, never pvecm", err)
	}
	if _, err = h.node.DecideCA(ctx, &Config{}, "https://192.0.2.10:8006"); err != nil {
		t.Errorf("an address the certificate lists: %v", err)
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
			if err := os.Rename(filepath.Join(h.root, pveServedPEM), filepath.Join(h.root, pveNodePEM)); err != nil {
				t.Fatal(err)
			}
		}
		s := h.session(Config{APIHost: c.host, Bridge: "vmbr0", CTID: "105"})
		s.Facts = &Facts{Prefix: netip.MustParsePrefix("192.0.2.10/24")}
		s.noteAPIFamily()
		if !c.want {
			if len(s.Notices) != 0 {
				t.Errorf("%s: noted %q", c.name, s.Notices)
			}
			continue
		}
		want := "does not list this node's address 192.0.2.10 on vmbr0, so the appliance and the clusters reach the API at 2001:db8::10, which clusters without IPv6 cannot. To use 192.0.2.10, make this node's name resolve to it first (/etc/hosts), run pvecm updatecerts --force and systemctl restart pveproxy, then: sh install.sh --upgrade --ctid 105 --proxmox-url https://192.0.2.10:8006"
		if len(s.Notices) != 1 || !strings.Contains(s.Notices[0], want) {
			t.Errorf("%s: noted %q, want it to contain %q", c.name, s.Notices, want)
		}
	}
}
