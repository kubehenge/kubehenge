package installer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func prepared(t *testing.T, h *harness, cfg Config) *Session {
	t.Helper()
	s := h.session(cfg)
	if err := s.detect(context.Background()); err != nil {
		t.Fatalf("detect: %v", err)
	}
	return s
}

func plainOn(t *testing.T, h *harness, cfg Config) (string, *Session, error) { //nolint:gocritic // tests pass a Config literal.
	t.Helper()
	s := h.session(cfg)
	var out bytes.Buffer
	err := RunPlain(context.Background(), s, func(emit func(Event)) *Runner { return h.runner(s, emit) }, &out)
	return out.String(), s, err
}

// The road with DHCP: the node's own network has a DHCP server, so --ip is left out.
func TestPlainDHCPRoad(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	out, _, err := plainOn(t, h, Config{PVEEntry: "no"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Kubehenge is running in container 100: https://192.0.2.77/") {
		t.Fatalf("no end line:\n%s", out)
	}
	if create := h.exec.last("pct create"); !strings.Contains(create, "ip=dhcp") {
		t.Fatalf("container not created with DHCP: %s", create)
	}
	if h.exec.count("pct set 100 --protection 1") != 1 {
		t.Fatal("appliance not protected")
	}
}

// The road with a static address given by flags, as the end-to-end tests call it.
func TestPlainStaticRoad(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pct config 100", "net0: name=eth0,bridge=vmbr0,ip=192.0.2.40/24,gw=192.0.2.1\n")
	out, _, err := plainOn(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", DNS: "192.0.2.53", PVEEntry: "no"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if create := h.exec.last("pct create"); !strings.Contains(create, "ip=192.0.2.40/24,gw=192.0.2.1") || !strings.Contains(create, "--nameserver 192.0.2.53") {
		t.Fatalf("container not created with the static address: %s", create)
	}
	if !strings.Contains(out, "https://192.0.2.40/") {
		t.Fatalf("end line names no static address:\n%s", out)
	}
}

// Unattended on a node without DHCP and without --ip: refused before anything is created.
func TestUnattendedWithoutIPOnNodeWithoutDHCP(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	out, _, err := plainOn(t, h, Config{PVEEntry: "no"})
	if err == nil {
		t.Fatalf("installed without an address:\n%s", out)
	}
	for _, want := range []string{"no sign of DHCP", "--ip <address>/<prefix> --gateway <address>"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q: %v", want, err)
		}
	}
	if h.exec.changed() != 0 {
		t.Fatalf("the node was changed: %v", h.exec.calls)
	}
}

// Under DHCP no address arrives: the unattended run names the fix and removes the container.
func TestUnattendedDHCPTimeoutCleansUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "")
	out, _, err := plainOn(t, h, Config{PVEEntry: "no"})
	var na *NoAddressError
	if err == nil || !asNoAddress(err, &na) || !na.DHCP {
		t.Fatalf("want a DHCP address failure, got %v\n%s", err, out)
	}
	if !strings.Contains(err.Error(), "--ip <address>/<prefix> --gateway <address>") {
		t.Fatalf("error names no fix: %v", err)
	}
	if h.exec.count("pct destroy 100") != 1 {
		t.Fatalf("container not removed: %v", h.exec.calls)
	}
}

func asNoAddress(err error, target **NoAddressError) bool {
	na := &NoAddressError{}
	ok := errors.As(err, &na)
	if ok {
		*target = na
	}
	return ok
}

// Unattended and the API's certificate is vouched for by nothing here: refused with the
// fingerprint and the flag that settles it, nothing created.
func TestUnattendedUntrustedCA(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{untrusted: true})
	_, _, err := plainOn(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 fingerprint") || !strings.Contains(err.Error(), "--proxmox-ca") {
		t.Fatalf("want the CA refusal, got %v", err)
	}
	if h.exec.changed() != 0 {
		t.Fatal("the node was changed")
	}
}

func TestExistingApplianceRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvesh get /cluster/resources", `[{"type":"lxc","vmid":105,"node":"pve2","tags":"a;kubehenge"}]`)
	s := h.session(Config{})
	err := s.Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "an appliance exists as container 105 on pve2") {
		t.Fatalf("got %v", err)
	}
}

func TestQuestionsNetwork(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	if !prepared(t, h, Config{}).Questions().Network {
		t.Fatal("DHCP or static is not asked without --ip")
	}
	if prepared(t, h, Config{IP: "dhcp"}).Questions().Network {
		t.Fatal("asked although --ip was given")
	}
}

func TestQuestionsStorageTie(t *testing.T) {
	t.Parallel()
	two := prepared(t, newHarness(t, opts{rootStorages: 2}), Config{})
	if !two.Questions().Storage {
		t.Fatal("two fitting storages are not asked")
	}
	if prepared(t, newHarness(t, opts{rootStorages: 1}), Config{}).Questions().Storage {
		t.Fatal("one fitting storage is asked")
	}
	if prepared(t, newHarness(t, opts{rootStorages: 2}), Config{Storage: "tank"}).Questions().Storage {
		t.Fatal("asked although --storage was given")
	}
}

func TestQuestionsTemplateStorageTie(t *testing.T) {
	t.Parallel()
	if !prepared(t, newHarness(t, opts{templateStorages: 2}), Config{}).Questions().TemplateStorage {
		t.Fatal("two template storages are not asked")
	}
	if prepared(t, newHarness(t, opts{templateStorages: 2, templateHeld: true}), Config{}).Questions().TemplateStorage {
		t.Fatal("asked although a storage holds the template")
	}
	if prepared(t, newHarness(t, opts{templateStorages: 1}), Config{}).Questions().TemplateStorage {
		t.Fatal("one template storage is asked")
	}
}

func TestQuestionsAPIHostTie(t *testing.T) {
	t.Parallel()
	tie := opts{
		names: []string{"a.example.com", "b.example.com"},
		hosts: map[string][]string{"a.example.com": {"192.0.2.10"}, "b.example.com": {"192.0.2.10"}},
	}
	s := prepared(t, newHarness(t, tie), Config{})
	if !s.Questions().APIHost {
		t.Fatalf("two equal names are not asked: %+v", s.Facts.Candidates)
	}
	if prepared(t, newHarness(t, tie), Config{ProxmoxURL: "https://a.example.com:8006"}).Questions().APIHost {
		t.Fatal("asked although --proxmox-url was given")
	}
	if prepared(t, newHarness(t, opts{}), Config{}).Questions().APIHost {
		t.Fatal("a single best name is asked")
	}
}

func TestQuestionsTrustCA(t *testing.T) {
	t.Parallel()
	if prepared(t, newHarness(t, opts{}), Config{}).Questions().TrustCA {
		t.Fatal("the cluster's own CA is asked")
	}
	s := prepared(t, newHarness(t, opts{untrusted: true}), Config{})
	if !s.Questions().TrustCA {
		t.Fatal("a CA nothing vouches for is not asked")
	}
	if s.CA.Subject == "" || s.CA.Issuer == "" || s.CA.Fingerprint == "" {
		t.Fatalf("the prompt lacks subject, issuer or fingerprint: %+v", s.CA)
	}
	s.Cfg.TrustServedCA = true
	if s.Questions().TrustCA {
		t.Fatal("asked again after a yes")
	}
}

func TestStaticPrefillAndAddressInUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	s := prepared(t, h, Config{})
	if s.Facts.SuggestedIP != "192.0.2.254" || s.Facts.Prefix.Bits() != 24 || s.Facts.Gateway != "192.0.2.1" {
		t.Fatalf("prefill %s/%d via %s", s.Facts.SuggestedIP, s.Facts.Prefix.Bits(), s.Facts.Gateway)
	}
	h.exec.fail("arping -c 2 -w 3 -I vmbr0 -D 192.0.2.254", 1)
	if !h.node.AddressInUse(context.Background(), "vmbr0", "192.0.2.254") {
		t.Fatal("an address that answers is free")
	}
	if got := h.node.SuggestAddress(context.Background(), "vmbr0", s.Facts); got != "192.0.2.253" {
		t.Fatalf("the next suggestion is %q", got)
	}
}
