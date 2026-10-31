package pve

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeTicketAPI answers /access/ticket like Proxmox VE: alice signs in with a password alone, bob
// also needs TOTP 123456, and any issued ticket renews itself.
func fakeTicketAPI(t *testing.T) *Accounts {
	t.Helper()
	// Proxmox VE escapes only the colons of a ticket's data (PVE::Ticket, uri_escape($data, ':')).
	challenge := "PVE:!tfa!" + strings.ReplaceAll(`{"totp":true,"recovery":[0,2],"webauthn":null}`, ":", "%3A") + ":68000000::c2ln"
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}
	mux.HandleFunc("POST /api2/json/access/ticket", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		user, password, answer := r.PostForm.Get("username"), r.PostForm.Get("password"), r.PostForm.Get("tfa-challenge")
		switch {
		case user == "alice@pve" && (password == "secret" || password == "PVE:alice@pve:1"): //nolint:gosec // test data
			reply(w, map[string]any{"username": user, "ticket": "PVE:alice@pve:1", "CSRFPreventionToken": "csrf"})
		case user == "bob@pve" && answer == "" && password == "secret":
			reply(w, map[string]any{"username": user, "ticket": challenge, "CSRFPreventionToken": "csrf", "NeedTFA": 1})
		case user == "bob@pve" && answer == challenge && password == "totp:123456":
			reply(w, map[string]any{"username": user, "ticket": "PVE:bob@pve:1", "CSRFPreventionToken": "csrf"})
		default:
			http.Error(w, "authentication failure", http.StatusUnauthorized)
		}
	})
	mux.HandleFunc("GET /api2/json/access/permissions", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("PVEAuthCookie"); err != nil || c.Value != "PVE:alice@pve:1" {
			http.Error(w, "no ticket", http.StatusUnauthorized)
			return
		}
		if !r.URL.Query().Has("path") {
			reply(w, map[string]any{"/": map[string]int{"Sys.Audit": 1}, "/pool/kubehenge/default.one": map[string]int{"VM.Audit": 1, "VM.Allocate": 0}})
			return
		}
		path := r.URL.Query().Get("path")
		reply(w, map[string]any{path: map[string]int{"VM.Audit": 1, "VM.Allocate": 0}})
	})
	mux.HandleFunc("GET /api2/json/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{
			{"vmid": 150, "node": "one", "pool": "infra"},
			{"vmid": 151, "node": "one"},
			{"vmid": 152, "node": "one"},
			{"vmid": 153, "node": "one"},
			{"vmid": 154, "node": "two"},
			{"vmid": 160, "node": "one", "type": "qemu", "name": "kubehenge-rke2-almalinux-10", "template": 1, "tags": "kubehenge;os-almalinux-10;rke2", "pool": "kubehenge-templates"},
			{"vmid": 161, "node": "one", "type": "qemu", "name": "kubehenge-rke2-almalinux-10", "template": 1, "tags": "kubehenge;os-almalinux-10;rke2", "pool": "elsewhere"},
			{"vmid": 162, "node": "one", "type": "qemu", "name": "being-built", "template": 0, "pool": "kubehenge-templates"},
			{"vmid": 163, "node": "one", "type": "lxc", "name": "a-container-template", "template": 1, "pool": "kubehenge-templates"},
			// Behind the configurations: 164's VM ID was reused by a VM being built, 165 was removed.
			{"vmid": 164, "node": "one", "type": "qemu", "name": "kubehenge-old", "template": 1, "tags": "kubehenge;os-rocky-10;rke2", "pool": "kubehenge-templates"},
			{"vmid": 165, "node": "one", "type": "qemu", "name": "kubehenge-gone", "template": 1, "tags": "k3s;kubehenge;os-rocky-10", "pool": "kubehenge-templates"},
		})
	})
	mux.HandleFunc("GET /api2/json/nodes/one/qemu/{vmid}/config", func(w http.ResponseWriter, r *http.Request) {
		configs := map[string]map[string]any{
			"160": {"name": "kubehenge-rke2-almalinux-10", "template": 1},
			"162": {"name": "being-built"},
			"164": {"name": "being-built-too"},
		}
		config, ok := configs[r.PathValue("vmid")]
		if !ok {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.WriteString(conn, "HTTP/1.1 500 Configuration file 'nodes/one/qemu-server/"+r.PathValue("vmid")+".conf' does not exist\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		reply(w, config)
	})
	mux.HandleFunc("GET /api2/json/cluster/backup", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{
			{"id": "off", "enabled": 0, "vmid": "151"},
			{"id": "all", "all": 1, "node": "one", "exclude": "152,999"},
			{"id": "pool", "pool": "infra"},
		})
	})
	// As Proxmox VE 9 answers, for alice only; bob lacks Sys.Audit on /.
	mux.HandleFunc("GET /api2/json/cluster/ha/rules", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("PVEAuthCookie"); err != nil || c.Value != "PVE:alice@pve:1" {
			http.Error(w, "Permission check failed (/, Sys.Audit)", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("type") != "node-affinity" {
			t.Errorf("rules asked for with %s", r.URL.RawQuery)
		}
		reply(w, []map[string]any{
			{"rule": "racks", "type": "node-affinity", "affinity": "positive", "comment": "rack A", "nodes": "one:2,two", "resources": "vm:100", "strict": 1, "order": 1},
			{"rule": "off-three", "type": "node-affinity", "affinity": "negative", "nodes": "three", "resources": "vm:101", "order": 2},
			{"rule": "old", "type": "node-affinity", "affinity": "positive", "nodes": "two", "resources": "vm:102", "disable": 1, "order": 3},
		})
	})
	mux.HandleFunc("GET /api2/json/pools", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("poolid") != "kubehenge/clusters.web" {
			// Proxmox VE puts its message in the status line, which net/http cannot write.
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.WriteString(conn, "HTTP/1.1 500 pool '"+r.URL.Query().Get("poolid")+"' does not exist\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		reply(w, []map[string]any{{"poolid": "kubehenge/clusters.web", "members": []map[string]any{
			{"vmid": 170, "node": "two", "type": "qemu", "name": "web-cp-a", "status": "running", "cpu": 0.25, "maxcpu": 4, "mem": 2147483648, "maxmem": 8589934592, "uptime": 3600},
			{"node": "two", "type": "storage", "storage": "local"},
		}}})
	})
	// A VM started half way through the hour: the steps before it have no load.
	mux.HandleFunc("GET /api2/json/nodes/two/qemu/170/rrddata", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("timeframe") != "hour" || q.Get("cf") != "AVERAGE" {
			t.Errorf("load asked for with %s", r.URL.RawQuery)
		}
		reply(w, []map[string]any{{"time": 1760000000}, {"time": 1760000060, "cpu": 0.5, "maxcpu": 4}})
	})
	// As Proxmox VE answers: a bridge carrying the node's address, one without, and a node with two
	// DNS servers, the third unset.
	mux.HandleFunc("GET /api2/json/nodes/one/network", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "any_bridge" {
			t.Errorf("network asked for with %s", r.URL.RawQuery)
		}
		reply(w, []map[string]any{
			{"iface": "vmbr0", "type": "bridge", "cidr": "192.0.2.5/24", "address": "192.0.2.5", "netmask": "24", "gateway": "192.0.2.1", "comments": "uplink\n"},
			{"iface": "vmbr1", "type": "OVSBridge", "bridge_vlan_aware": 1},
		})
	})
	mux.HandleFunc("GET /api2/json/nodes/one/dns", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"search": "example.org", "dns1": "192.0.2.53", "dns2": "2001:db8::53"})
	})
	mux.HandleFunc("GET /api2/json/access/domains", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{{"realm": "pve", "type": "pve", "default": 1}, {"realm": "pam", "type": "pam"}})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAccounts(Config{URL: srv.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSignInWithPassword(t *testing.T) {
	t.Parallel()
	a, ctx := fakeTicketAPI(t), context.Background()
	ticket, challenge, err := a.SignIn(ctx, "alice@pve", "secret")
	if err != nil || challenge != nil || ticket.Ticket != "PVE:alice@pve:1" || ticket.CSRF != "csrf" || ticket.Username != "alice@pve" {
		t.Fatalf("SignIn = %+v, %+v, %v", ticket, challenge, err)
	}
	renewed, err := a.Renew(ctx, ticket)
	if err != nil || renewed.Ticket != "PVE:alice@pve:1" || renewed.Issued.Before(ticket.Issued) {
		t.Fatalf("Renew = %+v, %v", renewed, err)
	}
	if _, _, err := a.SignIn(ctx, "alice@pve", "wrong"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong password: %v, want ErrAuthentication", err)
	}
}

func TestSignInWithSecondFactor(t *testing.T) {
	t.Parallel()
	a, ctx := fakeTicketAPI(t), context.Background()
	ticket, challenge, err := a.SignIn(ctx, "bob@pve", "secret")
	if err != nil || ticket != nil {
		t.Fatalf("SignIn = %+v, %v; want a challenge", ticket, err)
	}
	if !challenge.TOTP || challenge.Yubico || challenge.WebAuthn || !reflect.DeepEqual(challenge.Recovery, []int{0, 2}) {
		t.Fatalf("challenge = %+v", challenge)
	}
	if _, err = a.Answer(ctx, challenge, "totp", "000000"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong code: %v, want ErrAuthentication", err)
	}
	if _, err = a.Answer(ctx, challenge, "webauthn", "{}"); err == nil {
		t.Fatal("a WebAuthn answer was relayed")
	}
	ticket, err = a.Answer(ctx, challenge, "totp", "123456")
	if err != nil || ticket.Ticket != "PVE:bob@pve:1" {
		t.Fatalf("Answer = %+v, %v", ticket, err)
	}
}

func TestParseChallengeRejectsAFullTicket(t *testing.T) {
	t.Parallel()
	if _, err := parseChallenge("PVE:alice@pve:68000000::c2ln"); err == nil {
		t.Fatal("a ticket without a challenge parsed")
	}
}

func TestPrivileges(t *testing.T) {
	t.Parallel()
	a, ctx := fakeTicketAPI(t), context.Background()
	ticket, _, err := a.SignIn(ctx, "alice@pve", "secret")
	if err != nil {
		t.Fatal(err)
	}
	privs, err := a.Privileges(ctx, ticket, "/pool/kubehenge/default.one")
	if err != nil {
		t.Fatal(err)
	}
	if !privs.Has("VM.Audit") || privs.Has("VM.Allocate") || privs.Has("VM.Audit", "Sys.Modify") {
		t.Fatalf("privileges = %v", privs)
	}
	if _, err = a.Privileges(ctx, &Ticket{Username: "alice@pve", Ticket: "forged"}, "/"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("forged ticket: %v, want ErrAuthentication", err)
	}
	all, err := a.AllPrivileges(ctx, ticket)
	if err != nil || len(all) != 2 || !all["/"].Has("Sys.Audit") || !all["/pool/kubehenge/default.one"].Has("VM.Audit") || all["/pool/kubehenge/default.one"].Has("VM.Allocate") {
		t.Fatalf("all privileges = %v, %v", all, err)
	}
}

func TestBackupCovers(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	tk := &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"}
	// 150 by its pool, 151 only by a disabled job but by "all", 152 excluded from "all", 153 by "all",
	// 154 on a node the "all" job leaves out.
	for vmid, want := range map[int]bool{150: true, 151: true, 152: false, 153: true, 154: false} {
		got, err := a.BackupCovers(context.Background(), tk, vmid)
		if err != nil || got != want {
			t.Errorf("BackupCovers(%d) = %v, %v; want %v", vmid, got, err, want)
		}
	}
}

func TestTemplatesOfAPoolWithTheirTags(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	got, err := a.Templates(context.Background(), &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"}, "kubehenge-templates")
	want := []Member{{VMID: 160, Node: "one", Type: "qemu", Name: "kubehenge-rke2-almalinux-10", Template: 1, Tags: "kubehenge;os-almalinux-10;rke2"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Templates = %+v, %v; want %+v", got, err, want)
	}
}

func TestMembersOfANestedPool(t *testing.T) {
	t.Parallel()
	a, ctx := fakeTicketAPI(t), context.Background()
	tk := &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"}
	got, err := a.PoolMembers(ctx, tk, "kubehenge/clusters.web")
	want := []Member{{
		VMID: 170, Node: "two", Type: "qemu", Name: "web-cp-a",
		Status: "running", CPU: 0.25, Cores: 4, Memory: 2 << 30, MaxMem: 8 << 30, Uptime: 3600,
	}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("PoolMembers = %+v, %v; want %+v", got, err, want)
	}
	if got, err := a.PoolMembers(ctx, tk, "kubehenge/clusters.gone"); err != nil || got != nil {
		t.Fatalf("missing pool: %+v, %v; want none", got, err)
	}
}

func TestVMLoad(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	got, err := a.VMLoad(context.Background(), &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"}, "two", 170)
	if err != nil || len(got) != 2 || got[0].CPU != nil || got[1].CPU == nil || *got[1].CPU != 0.5 || got[1].Time != 1760000060 {
		t.Fatalf("VMLoad = %+v, %v", got, err)
	}
}

func TestBridgesAndDNSOfANode(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	tk := &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"}
	bridges, err := a.Bridges(context.Background(), tk, "one")
	want := []Bridge{
		{Bridge: "vmbr0", Node: "one", Comment: "uplink", CIDR: "192.0.2.5/24", Gateway: "192.0.2.1"},
		{Bridge: "vmbr1", Node: "one", VLANAware: true},
	}
	if err != nil || !reflect.DeepEqual(bridges, want) {
		t.Fatalf("Bridges = %+v, %v; want %+v", bridges, err, want)
	}
	dns, err := a.DNS(context.Background(), tk, "one")
	if err != nil || !reflect.DeepEqual(dns, []string{"192.0.2.53", "2001:db8::53"}) {
		t.Fatalf("DNS = %v, %v", dns, err)
	}
}

func TestNodeAffinityRules(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	rules, err := a.NodeAffinityRules(context.Background(), &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []HARule{
		{Rule: "racks", Comment: "rack A", Nodes: []string{"one", "two"}, Strict: true},
		{Rule: "off-three", Nodes: []string{"three"}, Negative: true},
	}
	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("rules = %+v, want %+v", rules, want)
	}
	if _, err = a.NodeAffinityRules(context.Background(), &Ticket{Username: "bob@pve", Ticket: "PVE:bob@pve:1"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("without Sys.Audit: %v, want ErrForbidden", err)
	}
}

func TestReleaseHARemovesOnlyThePrefixed(t *testing.T) {
	t.Parallel()
	var removed []string
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}
	mux.HandleFunc("GET /api2/json/cluster/ha/rules", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{
			{"rule": "kubehenge-1-apart", "comment": "Kubehenge cluster kubehenge/a.web"},
			{"rule": "racks", "comment": "rack A"},
			{"rule": "bare"},
		})
	})
	mux.HandleFunc("GET /api2/json/cluster/ha/resources", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{
			{"sid": "vm:101", "comment": "Kubehenge cluster kubehenge/a.web"},
			{"sid": "vm:102", "comment": "Kubehenge cluster kubehenge-other/a.web"},
			{"sid": "ct:103"},
		})
	})
	mux.HandleFunc("DELETE /api2/json/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Csrfpreventiontoken") != "csrf" {
			http.Error(w, "no CSRF token", http.StatusUnauthorized)
			return
		}
		removed = append(removed, strings.TrimPrefix(r.URL.Path, "/api2/json/cluster/ha/"))
		reply(w, nil)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAccounts(Config{URL: srv.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseHA(t.Context(), &Ticket{Username: "alice@pve", Ticket: "PVE:alice@pve:1", CSRF: "csrf"}, "Kubehenge cluster kubehenge/"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"rules/kubehenge-1-apart", "resources/vm:101"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
}

func TestAnAPITokenActsInPlaceOfATicket(t *testing.T) {
	t.Parallel()
	const token = "alice@pve!ci=0b6f3c1e" //nolint:gosec // the fake Proxmox VE's own
	var removed int
	mux := http.NewServeMux()
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "PVEAPIToken="+token || r.Header.Get("Cookie") != "" || r.Header.Get("Csrfpreventiontoken") != "" {
			http.Error(w, "authentication failure", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api2/json/version", func(w http.ResponseWriter, r *http.Request) {
		if authorized(w, r) {
			_, _ = w.Write([]byte(`{"data":{"version":"9.1"}}`))
		}
	})
	mux.HandleFunc("DELETE /api2/json/", func(w http.ResponseWriter, r *http.Request) {
		if authorized(w, r) {
			removed++
			_, _ = w.Write([]byte(`{"data":null}`))
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAccounts(Config{URL: srv.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := a.TokenUser(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Username != "alice@pve!ci" {
		t.Errorf("username = %q", ticket.Username)
	}
	if err := a.remove(t.Context(), ticket, "/cluster/ha/rules/x"); err != nil || removed != 1 {
		t.Fatalf("remove: %v, %d removed", err, removed)
	}
	if renewed, err := a.Renew(t.Context(), ticket); err != nil || renewed != ticket {
		t.Errorf("renewing a token = %v, %v", renewed, err)
	}
	for _, bad := range []string{"alice@pve!ci=wrong", "alice@pve=0b6f3c1e", "nonsense"} {
		if _, err := a.TokenUser(t.Context(), bad); !errors.Is(err, ErrAuthentication) {
			t.Errorf("TokenUser(%q) = %v", bad, err)
		}
	}
}

func TestRealms(t *testing.T) {
	t.Parallel()
	a := fakeTicketAPI(t)
	realms, err := a.Realms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Realm{{Realm: "pam", Type: "pam"}, {Realm: "pve", Type: "pve", Default: true}}
	if !reflect.DeepEqual(realms, want) {
		t.Fatalf("realms = %+v, want %+v", realms, want)
	}
}
