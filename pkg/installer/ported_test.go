package installer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIPValidators(t *testing.T) {
	t.Parallel()
	yes := func(name string, fn func(string) bool, in ...string) {
		for _, s := range in {
			if !fn(s) {
				t.Errorf("%s: %q refused", name, s)
			}
		}
	}
	no := func(name string, fn func(string) bool, in ...string) {
		for _, s := range in {
			if fn(s) {
				t.Errorf("%s: %q accepted", name, s)
			}
		}
	}
	yes("ipv4", IsIPv4, "192.0.2.1")
	no("ipv4", IsIPv4, "2001:db8::1", "192.0.2.300", "", "192.0.2")
	yes("ipv4 cidr", IsIPv4CIDR, "192.0.2.1/24", "192.0.2.1/32")
	no("ipv4 cidr", IsIPv4CIDR, "2001:db8::1/64", "192.0.2.1/0", "192.0.2.1", "192.0.2.1/33")
	yes("ipv6", IsIPv6, "2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::1", "::1", "fd00:1::", "::", "FD00:AB::1", "1:2:3:4:5:6:7::")
	no("ipv6", IsIPv6, "192.0.2.1", "2001::db8::1", "2001:::1", "1:2:3:4:5:6:7:8:9", "1:2:3:4:5:6:7", "1:2:3:4:5:6:7:8::",
		"2001:db80a::1", "2001:dbg::1", ":2001:db8::1", "2001:db8::1:", "fe80::1%eth0", "::ffff:192.0.2.1", "pve.example.org", "")
	yes("ipv6 cidr", IsIPv6CIDR, "2001:db8::5/64", "2001:db8::5/128")
	no("ipv6 cidr", IsIPv6CIDR, "2001:db8::5/129", "2001:db8::5")
	yes("proxy range", IsProxyRange, "192.0.2.10", "10.0.0.0/8", "2001:db8::/32")
	no("proxy range", IsProxyRange, "10.0.0.0/33", "pve", "2001:db8::/129")
}

func TestContainerNet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{Bridge: "vmbr0", IP: "dhcp", Gateway: "192.0.2.1"}, "name=eth0,bridge=vmbr0,ip=dhcp"},
		{Config{Bridge: "vmbr0", VLAN: "20", MTU: "1400", IP: "192.0.2.40/24", Gateway: "192.0.2.1"}, "name=eth0,bridge=vmbr0,tag=20,mtu=1400,ip=192.0.2.40/24,gw=192.0.2.1"},
		{Config{Bridge: "vmbr0", MTU: "1400", IP: "192.0.2.40/24"}, "name=eth0,bridge=vmbr0,mtu=1400,ip=192.0.2.40/24"},
		{Config{Bridge: "vmbr0", IP: "none", IP6: "2001:db8::40/64", Gateway6: "2001:db8::1"}, "name=eth0,bridge=vmbr0,ip6=2001:db8::40/64,gw6=2001:db8::1"},
		{Config{Bridge: "vmbr0", IP: "dhcp", IP6: "auto", Gateway6: "2001:db8::1"}, "name=eth0,bridge=vmbr0,ip=dhcp,ip6=auto"},
	}
	for _, c := range cases {
		if got := c.cfg.containerNet(); got != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
}

func TestValidateOptions(t *testing.T) {
	t.Parallel()
	bad := []struct {
		cfg  Config
		want string
	}{
		{Config{Name: "nodot"}, "--name is a DNS name"},
		{Config{DNS: "not an address"}, "--dns is a comma-separated list"},
		{Config{ProxmoxURL: "http://pve:8006"}, "--proxmox-url is the Proxmox VE API's https:// URL"},
		{Config{ProxmoxCA: "x", Insecure: true}, "exclude each other"},
		{Config{Insecure: true, Certificate: true}, "--proxmox-insecure goes with an install or --upgrade"},
		{Config{TrustedProxies: []string{"pve"}}, "--trusted-proxy pve: use an IPv4 or IPv6 address"},
		{Config{NoTrustedProxy: true}, "--no-trusted-proxy goes with --upgrade"},
		{Config{IP: "192.0.2.5"}, "--ip is dhcp or an IPv4 address with its prefix length"},
		{Config{Gateway: "x"}, "--gateway is an IPv4 address"},
		{Config{IP6: "x"}, "--ip6 is auto, dhcp or an IPv6 address"},
		{Config{Gateway6: "x", IP6: "2001:db8::5/64"}, "--gateway6 is an IPv6 address"},
		{Config{IP: "none"}, "--ip none needs --ip6"},
		{Config{Gateway6: "2001:db8::1", IP6: "auto"}, "--gateway6 goes with a static --ip6"},
		{Config{VLAN: "5000"}, "--vlan is a VLAN tag from 1 to 4094"},
		{Config{MTU: "100"}, "--mtu is a number of bytes from 576 to 65535"},
		{Config{NoName: true}, "--no-name goes with --certificate and without --name"},
		{Config{Release: "r.tar.gz"}, "--release needs --sha256"},
	}
	for _, b := range bad {
		err := b.cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%+v: got %v, want %q", b.cfg, err, b.want)
		}
	}
	good := Config{IP: "192.0.2.5/24", Gateway: "192.0.2.1", IP6: "2001:db8::5/64", VLAN: "20", MTU: "1400", DNS: "192.0.2.53,192.0.2.54", Name: "kubehenge.example.org"}
	if err := good.Validate(); err != nil {
		t.Errorf("valid options refused: %v", err)
	}
}

func TestExistingApplianceTagMatching(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		`[{"type":"lxc","vmid":105,"node":"pve2","tags":"a;kubehenge;b"}]`:   true,
		`[{"type":"lxc","vmid":105,"node":"pve2","tags":"kubehenge-test"}]`:  false,
		`[{"type":"qemu","vmid":105,"node":"pve2","tags":"kubehenge"}]`:      false,
		`[{"type":"lxc","vmid":105,"node":"pve2","tags":"old-kubehenge;x"}]`: false,
	}
	for body, want := range cases {
		h := newHarness(t, opts{})
		h.exec.on("pvesh get /cluster/resources", body)
		if got := len(h.node.ApplianceContainers(context.Background())) > 0; got != want {
			t.Errorf("%s: found=%v", body, got)
		}
	}
}

func TestNoQuorumRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvecm status", "Cluster information\nQuorate:          No\n")
	err := h.session(Config{}).Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "this node has no quorum in its Proxmox VE cluster") {
		t.Fatalf("got %v", err)
	}
}

func candidateHosts(t *testing.T, o opts) []string {
	t.Helper()
	facts, err := newHarness(t, o).node.Detect(context.Background(), &Config{})
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, c := range facts.Candidates {
		hosts = append(hosts, c.Host)
	}
	return hosts
}

func TestAPICandidates(t *testing.T) {
	t.Parallel()
	same := func(names ...string) map[string][]string {
		m := map[string][]string{}
		for _, n := range names {
			m[n] = []string{"192.0.2.10"}
		}
		return m
	}
	cases := []struct {
		name string
		o    opts
		want string
	}{
		{"this node's name, then its bridge address", opts{names: []string{"x.example.com", "pve1.example.com"}, hosts: same("x.example.com", "pve1.example.com")}, "pve1.example.com,x.example.com,192.0.2.10"},
		{"only this cluster's names and addresses", opts{names: []string{"pve1.example.com"}, ips: []string{"192.0.2.10", "203.0.113.50"}}, "pve1.example.com,192.0.2.10"},
		{"a certificate of no node of the cluster", opts{names: []string{"web.example.net"}, ips: []string{"203.0.113.50"}, hosts: map[string][]string{"web.example.net": {"203.0.113.50"}}}, ""},
		{"a wildcard covering this node's name", opts{names: []string{"*.example.com"}, ips: []string{"203.0.113.50"}}, "pve1.example.com"},
		{"a wildcard not covering it", opts{names: []string{"*.example.org"}, ips: []string{"203.0.113.50"}}, ""},
		{"a name that also leads outside is passed over", opts{names: []string{"a.example.com", "pve1.example.com"}, hosts: map[string][]string{"a.example.com": {"192.0.2.10", "203.0.113.9"}, "pve1.example.com": {"192.0.2.10"}}, ips: []string{"203.0.113.50"}}, "pve1.example.com"},
		{"a name without a domain after the addresses", opts{names: []string{"pve1"}, hosts: map[string][]string{"pve1": {"192.0.2.10"}}}, "192.0.2.10,pve1"},
	}
	for _, c := range cases {
		if got := strings.Join(candidateHosts(t, c.o), ","); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecideCA(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, opts{})
	if ca, err := h.node.DecideCA(ctx, &Config{}, "https://pve1.example.com:8006"); err != nil || ca.Kind != CACluster {
		t.Fatalf("the cluster's CA where it verifies the API: %v %+v", err, ca)
	}
	ca, err := h.node.DecideCA(ctx, &Config{Insecure: true}, "https://pve1.example.com:8006")
	if err != nil || ca.Kind != CAInsecure {
		t.Fatalf("insecure: %v %+v", err, ca)
	}

	// A CA in this node's trust store the chain rests on: that CA alone.
	u := newHarness(t, opts{untrusted: true})
	writeTestFile(t, filepath.Join(u.root, systemCA), encodeCerts(u.chain[1]))
	ca, err = u.node.DecideCA(ctx, &Config{}, "https://pve1.example.com:8006")
	if err != nil || ca.Kind != CASystem || ca.NeedsTrust || ca.Subject != "CN=Corporate Root" {
		t.Fatalf("trust store: %v %+v", err, ca)
	}

	// A CA given with --proxmox-ca that signed it, and one that did not.
	given := filepath.Join(t.TempDir(), "ca.pem")
	writeTestFile(t, given, encodeCerts(u.chain[1]))
	if ca, err = u.node.DecideCA(ctx, &Config{ProxmoxCA: given}, "https://pve1.example.com:8006"); err != nil || ca.Kind != CAGiven {
		t.Fatalf("given: %v %+v", err, ca)
	}
	other := newCA(t, "Other")
	writeTestFile(t, given, encodeCerts(other.cert))
	if _, err = u.node.DecideCA(ctx, &Config{ProxmoxCA: given}, "https://pve1.example.com:8006"); err == nil || !strings.Contains(err.Error(), "serves no certificate that "+given+" signed for it") {
		t.Fatalf("a CA that did not sign: %v", err)
	}

	// A CA nothing here trusts needs the user's yes, and shows subject, issuer and fingerprint.
	n := newHarness(t, opts{untrusted: true})
	if ca, err = n.node.DecideCA(ctx, &Config{}, "https://pve1.example.com:8006"); err != nil || !ca.NeedsTrust || ca.Fingerprint == "" {
		t.Fatalf("served CA: %v %+v", err, ca)
	}

	// A refused connection names the port, the firewall and pveproxy's address.
	n.node.Dial = func(context.Context, string, string) ([]*x509.Certificate, error) { return nil, os.ErrNotExist }
	_, err = n.node.DecideCA(ctx, &Config{}, "https://pve1.example.com:8006")
	if err == nil || !strings.Contains(err.Error(), "cannot connect to the Proxmox VE API") || !strings.Contains(err.Error(), "LISTEN_IP") {
		t.Fatalf("a refused connection: %v", err)
	}
}

func TestMakeCertificate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	cert, err := h.node.MakeCertificate("kubehenge", []string{"203.0.113.7", "2001:db8::77"}, "kubehenge.example.org", now)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseCertFile(cert.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if d := now.Sub(parsed.NotBefore); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("starts %v back", d)
	}
	if d := parsed.NotAfter.Sub(parsed.NotBefore); d > 825*24*time.Hour {
		t.Errorf("valid %v", d)
	}
	dns, ips := certNames(parsed)
	if strings.Join(dns, ",") != "kubehenge,kubehenge.example.org" || strings.Join(ips, ",") != "203.0.113.7,2001:db8::77" {
		t.Errorf("names %v %v", dns, ips)
	}
	if parsed.Subject.CommonName != "kubehenge.example.org" {
		t.Errorf("subject %s", parsed.Subject)
	}
	r := h.runner(h.session(Config{}), nil)
	if !r.signedHere(parsed) || !r.madeHere(parsed) {
		t.Error("not signed by this cluster's CA")
	}
}

func TestOwnCertificate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	now := time.Now()
	cert, _ := h.node.MakeCertificate("kubehenge", []string{"192.0.2.40"}, "", now)
	other, _ := h.node.MakeCertificate("kubehenge", []string{"192.0.2.41"}, "", now)
	if _, err := OwnCertificate(cert.CertPEM, cert.KeyPEM, "c.pem", "k.pem", "192.0.2.40", now); err != nil {
		t.Errorf("a matching pair: %v", err)
	}
	if _, err := OwnCertificate(cert.CertPEM, other.KeyPEM, "c.pem", "k.pem", "192.0.2.40", now); err == nil || !strings.Contains(err.Error(), "is not the key of c.pem") {
		t.Errorf("a key of another certificate: %v", err)
	}
	if _, err := OwnCertificate(cert.CertPEM, cert.KeyPEM, "c.pem", "k.pem", "192.0.2.99", now); err == nil || !strings.Contains(err.Error(), "is not valid for 192.0.2.99") {
		t.Errorf("another host: %v", err)
	}
	if _, err := OwnCertificate(cert.CertPEM, cert.KeyPEM, "c.pem", "k.pem", "192.0.2.40", now.Add(900*24*time.Hour)); err == nil {
		t.Error("an expired certificate")
	}
}

func TestApplianceAddresses(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pct config 100", "net0: name=eth0,bridge=vmbr0,ip=192.0.2.40/24,gw=192.0.2.1,ip6=2001:db8::40/64\n")
	r := h.runner(h.session(Config{CTID: "100"}), nil)
	if v4, v6 := r.addresses(context.Background()); v4 != "192.0.2.40" || v6 != "2001:db8::40" {
		t.Errorf("configured: %s %s", v4, v6)
	}
	h.exec.on("pct config 100", "net0: name=eth0,bridge=vmbr0,ip=dhcp,ip6=auto\n")
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "2: eth0    inet 203.0.113.7/24 scope global eth0\n")
	h.exec.on("pct exec 100 -- ip -6 -o addr show dev eth0", "2: eth0    inet6 2001:db8::99/64 scope global temporary dynamic\n2: eth0    inet6 2001:db8::77/64 scope global dynamic\n")
	if v4, v6 := r.addresses(context.Background()); v4 != "203.0.113.7" || v6 != "2001:db8::77" {
		t.Errorf("from the container: %s %s", v4, v6)
	}
}

func TestCleanupAfterCreate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	r := h.runner(h.session(Config{CTID: "991"}), nil)
	r.Cleanup(context.Background())
	if h.exec.count("pct destroy") != 0 {
		t.Fatal("a failure before the container removed something")
	}
	r.created = true
	r.Cleanup(context.Background())
	calls := strings.Join(h.exec.calls, "\n")
	for _, want := range []string{"pct unlock 991", "pct stop 991", "pct destroy 991 --purge --destroy-unreferenced-disks 1"} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in\n%s", want, calls)
		}
	}
}

func TestPlainSuccessKeepsTheContainer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{dhcp: true})
	if out, err, _ := runPlain(t, h, Config{PVEEntry: "no"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if h.exec.count("pct destroy") != 0 {
		t.Fatal("a successful install removed the container")
	}
}

func TestEntryUpdate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	r := h.runner(h.session(Config{}), nil)
	var out bytes.Buffer
	r.updateEntry(context.Background(), "https://192.0.2.41", &out)
	if out.Len() != 0 || h.exec.count("/usr/local/sbin/kubehenge-pve-entry") != 0 {
		t.Fatal("no entry on this node, but something was done")
	}
	writeTestFile(t, filepath.Join(h.root, "etc/kubehenge-pve-entry.conf"), []byte("url=https://192.0.2.5\n"))
	r.updateEntry(context.Background(), "https://192.0.2.41", &out)
	if h.exec.count("/usr/local/sbin/kubehenge-pve-entry") != 0 {
		t.Fatal("an entry without its script was run")
	}
	writeTestFile(t, filepath.Join(h.root, "usr/local/sbin/kubehenge-pve-entry"), []byte("#!/bin/sh\n"))
	r.updateEntry(context.Background(), "https://192.0.2.41", &out)
	if h.exec.last("/usr/local/sbin/kubehenge-pve-entry") != "/usr/local/sbin/kubehenge-pve-entry --url https://192.0.2.41" || !strings.Contains(out.String(), "On every other node") {
		t.Fatalf("the entry was not pointed at the new address: %q", out.String())
	}
	writeTestFile(t, filepath.Join(h.root, "etc/kubehenge-pve-entry.conf"), []byte("url=https://192.0.2.41\n"))
	before := h.exec.count("/usr/local/sbin/kubehenge-pve-entry")
	r.updateEntry(context.Background(), "https://192.0.2.41", &out)
	if h.exec.count("/usr/local/sbin/kubehenge-pve-entry") != before {
		t.Fatal("an entry leading to the appliance already was run again")
	}
	writeTestFile(t, filepath.Join(h.root, "etc/kubehenge-pve-entry.conf"), []byte("url=https://old\n"))
	h.exec.fail("/usr/local/sbin/kubehenge-pve-entry", 1)
	out.Reset()
	r.updateEntry(context.Background(), "https://kubehenge.example.org", &out)
	if !strings.Contains(out.String(), "the entry still leads to https://old") {
		t.Fatalf("a failed update is not said: %q", out.String())
	}
}

func TestProxyHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"http://user:p%40ss@proxy.example.org:3128/path?x=1": "http://proxy.example.org:3128",
		"http://user:p@s/s@proxy.example.org:3128/":          "http://proxy.example.org:3128",
		"https://proxy.example.org:3128":                     "https://proxy.example.org:3128",
		"proxy.example.org:3128":                             "http://proxy.example.org:3128",
	} {
		if got := ProxyLabel(in); got != want {
			t.Errorf("%s: got %s", in, got)
		}
	}
	t.Setenv("https_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("http_proxy", "")
	if got := DatacenterProxy([]byte("keyboard: de\nhttp_proxy: http://u:secret@proxy.example.org:3128 \r\n")); got != "http://u:secret@proxy.example.org:3128" {
		t.Errorf("datacenter proxy %q", got)
	}
	t.Setenv("http_proxy", "http://env.example.org:3128")
	if got := DatacenterProxy([]byte("http_proxy: http://dc.example.org:3128\n")); got != "" {
		t.Errorf("the environment's proxy must win, got %q", got)
	}
}

func TestSecurityUpdates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	s := h.session(Config{CTID: "900"})
	r := h.runner(s, nil)
	r.Proxy = "http://user:secret@proxy.example.org:3128"
	h.exec.on("pct exec 900 -- dpkg-query", "install ok installed")
	if err := r.stepUpdates(context.Background()); err != nil || h.exec.count("pct exec 900 -- sh -c apt-get") != 0 {
		t.Fatal("installed already: left as it is")
	}
	h.exec.fail("pct exec 900 -- dpkg-query", 1)
	if err := r.stepUpdates(context.Background()); err != nil || h.exec.count("pct exec 900 -- sh -c apt-get") != 1 {
		t.Fatalf("unattended-upgrades is not installed: %v", err)
	}
	if !strings.Contains(h.exec.last("pct push 900"), "20auto-upgrades") {
		t.Fatalf("not set to run each day: %s", h.exec.last("pct push 900"))
	}
	if h.exec.count("pct push 900 "+filepath.Join(h.work, "apt-proxy.conf")) != 1 {
		t.Fatal("apt is not given the installer's proxy")
	}
	h.exec.fail("pct exec 900 -- sh -c apt-get", 100)
	r.warnings = nil
	if err := r.stepUpdates(context.Background()); err != nil || len(r.warnings) != 1 || !strings.Contains(r.warnings[0], "could not install unattended-upgrades") {
		t.Fatalf("a container without Debian's mirrors is not warned about: %v", r.warnings)
	}
}

func TestStorageRankingAndShortage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvesm status --content rootdir", `Name Type Status Total Used Available %
nfs-big nfs active 9 9 900000000 1%
zfs-more zfspool active 9 9 20000000 1%
local-lvm lvmthin active 9 9 30000000 1%
ceph rbd active 9 9 31000000 1%
dir-ok dir active 9 9 9437184 1%
exact lvm active 9 9 9437184 1%
lvm-small lvm active 9 9 5000000 1%
nfs-down nfs inactive 9 9 900000000 1%
`)
	fit, short, err := h.node.Storages(context.Background(), "rootdir", rootDiskKiB)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range fit {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "ceph,local-lvm,zfs-more,exact,dir-ok,nfs-big" {
		t.Errorf("ranking: %s", got)
	}
	if shortText(short) != "lvm-small (4.8 GiB free)" {
		t.Errorf("short: %s", shortText(short))
	}
	s := prepared(t, h, Config{})
	if s.Cfg.Storage != "" || !s.Questions().Storage {
		t.Error("a tie must be asked")
	}
	if err := s.FillStorages(true); err != nil || s.Cfg.Storage != "ceph" || !strings.Contains(strings.Join(s.Notices, "\n"), "the best of 6 by type and free space") {
		t.Errorf("unattended picks the best: %v %q %v", err, s.Cfg.Storage, s.Notices)
	}
	named := h.session(Config{Storage: "lvm-small"})
	named.Facts = s.Facts
	if err := named.FillStorages(false); err == nil || !strings.Contains(err.Error(), "storage lvm-small is disabled, inactive, takes no container disks here or has under 9 GiB free") {
		t.Errorf("a named storage without room: %v", err)
	}
}

func TestNoStorageFits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvesm status --content rootdir", "Name Type Status Total Used Available %\nlvm-small lvm active 9 9 5000000 1%\n")
	s := h.session(Config{})
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := s.FillStorages(true)
	if err == nil || !strings.Contains(err.Error(), "lvm-small (4.8 GiB free)") {
		t.Fatalf("got %v", err)
	}
}

func TestFetchChecksums(t *testing.T) {
	t.Parallel()
	body := []byte("the file")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	f := NewFetcher("")
	f.Client = srv.Client()
	f.Sleep = func(time.Duration) {}
	s256, s512 := sha256.Sum256(body), sha512.Sum512(body)
	dest := filepath.Join(t.TempDir(), "file")
	for _, sum := range []string{hex.EncodeToString(s256[:]), hex.EncodeToString(s512[:])} {
		if err := f.Fetch(context.Background(), srv.URL+"/x", dest, sum, HTTPSOnly, nil); err != nil {
			t.Errorf("a matching sum of %d digits: %v", len(sum), err)
		}
	}
	wrong := strings.Repeat("0", 128)
	if err := f.Fetch(context.Background(), srv.URL+"/x", dest, wrong, HTTPSOnly, nil); err == nil || !strings.Contains(err.Error(), "does not match its checksum") {
		t.Errorf("a wrong sum: %v", err)
	}
	if err := f.Fetch(context.Background(), "http://example.com/x", dest, wrong, HTTPSOnly, nil); err == nil {
		t.Error("plain HTTP was accepted for a file that must come over HTTPS")
	}
	missing := httptest.NewTLSServer(http.NotFoundHandler())
	defer missing.Close()
	f.Client = missing.Client()
	f.Retries = 2
	if err := f.Fetch(context.Background(), missing.URL+"/x", dest, wrong, HTTPSOnly, nil); err == nil || !strings.Contains(err.Error(), "could not download "+missing.URL+"/x (HTTP status 404)") {
		t.Errorf("a missing file: %v", err)
	}
}

func TestTemplateIndex(t *testing.T) {
	t.Parallel()
	mirror := t.TempDir()
	writeIndex := func(entries ...[4]string) {
		var b bytes.Buffer
		for _, e := range entries {
			b.WriteString("Package: " + e[0] + "\nVersion: " + e[1] + "\nType: lxc\nArchitecture: " + e[2] + "\nLocation: system/" + e[3] + "\nsha512sum: " + strings.Repeat("a", 128) + "\nDescription: a template\n a long description\n\n")
		}
		var z bytes.Buffer
		zw := gzip.NewWriter(&z)
		_, _ = zw.Write(b.Bytes())
		_ = zw.Close()
		writeTestFile(t, filepath.Join(mirror, "aplinfo-pve-9.dat.gz"), z.Bytes())
		writeTestFile(t, filepath.Join(mirror, "aplinfo-pve-9.dat.asc"), nil)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(mirror)))
	defer srv.Close()
	h := newHarness(t, opts{})
	f := NewFetcher("")
	f.Sleep = func(time.Duration) {}
	f.Retries = 1
	writeIndex([4]string{"debian-13-standard", "13.9-1", "amd64", "debian-13-standard_13.9-1_amd64.tar.zst"},
		[4]string{"debian-13-standard", "13.10-1", "amd64", "debian-13-standard_13.10-1_amd64.tar.zst"},
		[4]string{"debian-13-standard", "13.11-1", "arm64", "debian-13-standard_13.11-1_arm64.tar.zst"},
		[4]string{"debian-12-standard", "12.12-1", "amd64", "debian-12-standard_12.12-1_amd64.tar.zst"})
	got, err := f.TemplateFromIndex(context.Background(), h.exec, t.TempDir(), srv.URL)
	if err != nil || got.File != "debian-13-standard_13.10-1_amd64.tar.zst" || got.Location != "system/"+got.File {
		t.Fatalf("the newest listed, by version order: %v %s %s", err, got.File, got.Location)
	}
	h.exec.fail("sqv", 1)
	if _, err = f.TemplateFromIndex(context.Background(), h.exec, t.TempDir(), srv.URL); err == nil || !strings.Contains(err.Error(), "fails its signature check") {
		t.Errorf("an index whose signature fails: %v", err)
	}
	h2 := newHarness(t, opts{})
	writeIndex([4]string{"debian-13-standard", "13.9-1", "amd64", "../../etc/passwd"})
	if _, err = f.TemplateFromIndex(context.Background(), h2.exec, t.TempDir(), srv.URL); err == nil || !strings.Contains(err.Error(), "lists no amd64 Debian 13 standard template") {
		t.Errorf("an index without a usable template: %v", err)
	}
	_ = os.Remove(filepath.Join(mirror, "aplinfo-pve-9.dat.asc"))
	if _, err = f.TemplateFromIndex(context.Background(), h2.exec, t.TempDir(), srv.URL); err == nil || !strings.Contains(err.Error(), "could not download Proxmox's template index") {
		t.Errorf("an index without its signature: %v", err)
	}
}

func TestNetworkDetection(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	h.exec.on("pvesh get /nodes/pve1/network", `[{"type":"bridge","iface":"vmbr7"},{"type":"OVSBridge","iface":"vmbr0"},{"type":"bond","iface":"bond0"}]`)
	h.exec.on("pvesh get /cluster/sdn/vnets", `[{"vnet":"vn1"}]`)
	if got := strings.Join(h.node.bridges(context.Background()), ", "); got != "vmbr0, vmbr7, vn1" {
		t.Errorf("bridges: %s", got)
	}
	for route, want := range map[string]string{
		"default via 192.0.2.1 dev vmbr7\n":    "vmbr7",
		"default via 192.0.2.1 dev vmbr0.20\n": "vmbr0",
		"":                                     "vmbr0",
	} {
		_, dev := func() (string, string) {
			h.exec.on("ip -4 route show default", route)
			return h.node.defaultRoute(context.Background())
		}()
		if got := pickDefaultBridge([]string{"vmbr0", "vmbr7", "vn1"}, dev); got != want {
			t.Errorf("route %q: %s, want %s", route, got, want)
		}
	}
	if got := pickDefaultBridge([]string{"vn1"}, "bond0"); got != "" {
		t.Errorf("neither the route's bridge nor vmbr0: %q", got)
	}
}

func TestRefusedBridgeAndLoopbackDNS(t *testing.T) {
	t.Parallel()
	h := newHarness(t, opts{})
	err := h.session(Config{Bridge: "bond0"}).Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bond0 is no bridge") {
		t.Errorf("a bond: %v", err)
	}
	writeTestFile(t, filepath.Join(h.root, resolvConf), []byte("nameserver 127.0.0.53\n"))
	_, perr, _ := runPlain(t, h, Config{IP: "192.0.2.40/24", Gateway: "192.0.2.1", PVEEntry: "no"})
	if perr == nil || !strings.Contains(perr.Error(), "only name servers on this node's loopback") {
		t.Errorf("loopback resolvers: %v", perr)
	}
	if !prepared(t, h, Config{}).Questions().DNS {
		t.Error("loopback-only resolvers are not asked for")
	}
}

func upgradeHarness(t *testing.T) (*harness, *Runner, *bytes.Buffer) {
	t.Helper()
	h := newHarness(t, opts{})
	h.exec.on("pvesh get /cluster/resources", `[{"type":"lxc","vmid":100,"node":"pve1","tags":"kubehenge"}]`)
	h.exec.on("pct status 100", "status: running\n")
	h.exec.on("pct exec 100 -- cat /usr/local/share/kubehenge/VERSION", "0.1.0\n")
	h.exec.on("pct config 100", "hostname: kubehenge\nrootfs: local-lvm:vm-100-disk-0,size=8G\nmp0: local-lvm:vm-100-disk-1,mp=/var/lib/kubehenge-keys,backup=0\ntags: kubehenge\n")
	h.exec.on("pct exec 100 -- cat /etc/kubehenge/appliance.env", "KUBEHENGE_PROXMOX_URL=https://pve1.example.com:8006\n")
	h.exec.on("pct exec 100 -- df", "Filesystem 1K-blocks Used Available Use% Mounted\n/dev 9 9 900000 1% /\n")
	h.exec.on("pct exec 100 -- du", "100\t/usr/local/share/kubehenge\n")
	s := h.session(Config{CTID: "100", Upgrade: true})
	var out bytes.Buffer
	return h, h.runner(s, nil), &out
}

func TestUpgradeRoads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, r, out := upgradeHarness(t)
	if _, err := r.Upgrade(ctx, out); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "Kubehenge 9.9.9 is running in container 100") || h.exec.count("pct exec 100 -- flock -n -E 75") != 1 {
		t.Fatalf("an upgrade that serves: %s", out)
	}
	for code, want := range map[int]string{
		exitBusy:       "the management cluster is being applied or removed",
		exitRolledBack: "Rolled back to 0.1.0",
		exitStuck:      "Rolling back to 0.1.0 failed too",
		exitCutOff:     "Undo it with: pct exec 100 -- systemctl restart kubehenge.service",
	} {
		h, r, out = upgradeHarness(t)
		h.exec.fail("pct exec 100 -- flock", code)
		_, err := r.Upgrade(ctx, out)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("exit %d: got %v, want %q", code, err, want)
		}
	}
	h, r, out = upgradeHarness(t)
	h.exec.on("pct exec 100 -- cat /usr/local/share/kubehenge/VERSION", "99.0.0\n")
	if _, err := r.Upgrade(ctx, out); err == nil || !strings.Contains(err.Error(), "runs Kubehenge 99.0.0, newer than 9.9.9") {
		t.Errorf("an older release: %v", err)
	}
	h, r, out = upgradeHarness(t)
	h.exec.on("pvesh get /cluster/resources", `[{"type":"lxc","vmid":100,"node":"elsewhere","tags":"kubehenge"}]`)
	if _, err := r.Upgrade(ctx, out); err == nil || !strings.Contains(err.Error(), "container 100 runs on node elsewhere. Run this there") {
		t.Errorf("an appliance on another node: %v", err)
	}
	h, r, out = upgradeHarness(t)
	h.exec.on("pvesh get /cluster/resources", "[]")
	h.exec.on("pct config 100", "hostname: other\n")
	if _, err := r.Upgrade(ctx, out); err == nil || !strings.Contains(err.Error(), "container 100 is not Kubehenge's appliance") {
		t.Errorf("a container that is no appliance: %v", err)
	}
	h, r, out = upgradeHarness(t)
	h.exec.on("pct config 100", "rootfs: local-lvm:vm-100-disk-0,size=8G\ntags: kubehenge\n")
	if _, err := r.Upgrade(ctx, out); err != nil || h.exec.count("pct set 100 --mp0 local-lvm:1,mp=/var/lib/kubehenge-keys,backup=0") != 1 {
		t.Errorf("the key volume is added: %v %v", err, h.exec.calls)
	}
}

func TestReplaceCertificate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, r, out := upgradeHarness(t)
	h.exec.on("pct config 100", "hostname: kubehenge\ntags: kubehenge\nnet0: name=eth0,bridge=vmbr0,ip=192.0.2.40/24\n")
	r.S.Cfg.Upgrade, r.S.Cfg.Certificate, r.S.Cfg.Name = false, true, "kubehenge.example.org"
	res, err := r.ReplaceCertificate(ctx, out)
	if err != nil || res.URL != "https://kubehenge.example.org/" {
		t.Fatalf("%v %+v", err, res)
	}
	if h.exec.count("pct push 100") != 2 || h.exec.count("pct exec 100 -- flock -n -E 75") != 1 {
		t.Errorf("certificate and key are pushed and the interface restarted: %v", h.exec.calls)
	}
	h.exec.fail("pct exec 100 -- flock", exitBusy)
	if _, err = r.ReplaceCertificate(ctx, out); err == nil || !strings.Contains(err.Error(), "the interface is busy") {
		t.Errorf("a busy interface: %v", err)
	}
}
