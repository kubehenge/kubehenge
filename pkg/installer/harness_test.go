package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type rule struct {
	prefix string
	out    string
	code   int
}

// fakeExec scripts the node's commands. The rule registered last that prefixes a command wins.
type fakeExec struct {
	mu    sync.Mutex
	rules []rule
	calls []string
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	for _, v := range slices.Backward(f.rules) {
		if strings.HasPrefix(cmd, v.prefix) {
			if v.code != 0 {
				return "", &CmdError{Command: cmd, Code: v.code, Err: fmt.Errorf("exit status %d", v.code)}
			}
			return v.out, nil
		}
	}
	return "", nil
}

func (f *fakeExec) on(prefix, out string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, rule{prefix: prefix, out: out})
}

func (f *fakeExec) fail(prefix string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, rule{prefix: prefix, code: code})
}

func (f *fakeExec) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeExec) last(prefix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range slices.Backward(f.calls) {
		if strings.HasPrefix(v, prefix) {
			return v
		}
	}
	return ""
}

// changed counts the commands that change the node.
func (f *fakeExec) changed() int {
	n := 0
	for _, p := range []string{"pct create", "pct start", "pct set", "pct push", "pct destroy", "pct stop"} {
		n += f.count(p)
	}
	return n
}

type opts struct {
	dhcp             bool
	rootStorages     int
	templateStorages int
	templateHeld     bool
	// names and ips are what the API's certificate carries.
	names, ips []string
	// untrusted has the API serve a certificate no CA of this node vouches for.
	untrusted bool
	// hosts maps names to addresses.
	hosts map[string][]string
}

type harness struct {
	t       *testing.T
	exec    *fakeExec
	node    *Node
	root    string
	work    string
	dir     string
	pin     TemplatePin
	tofu    string
	tofuSum string
	srv     *httptest.Server
	chain   []*x509.Certificate
	hits    int
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T, cn string) ca {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour * 365),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return ca{cert, key}
}

func (c ca) issue(t *testing.T, dns, ips []string) *x509.Certificate {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "pve"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour * 30),
		DNSNames: dns, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func storageTable(n int, kind string) string {
	table := "Name Type Status Total Used Available %\n"
	names := []string{"local-lvm", "tank", "share"}
	types := []string{"lvmthin", "zfspool", "nfs"}
	var tableSb194 strings.Builder
	for i := range n {
		fmt.Fprintf(&tableSb194, "%s %s active 500000000 1000 %d 1%%\n", names[i], types[i], 400000000-i*1000)
	}
	table += tableSb194.String()
	_ = kind
	return table
}

func newHarness(t *testing.T, o opts) *harness { //nolint:gocritic // tests pass an opts literal.
	t.Helper()
	root, work, dir := t.TempDir(), t.TempDir(), t.TempDir()
	h := &harness{t: t, exec: &fakeExec{}, root: root, work: work, dir: dir}
	if o.rootStorages == 0 {
		o.rootStorages = 1
	}
	if o.templateStorages == 0 {
		o.templateStorages = 1
	}
	if o.names == nil {
		o.names = []string{"pve1.example.com"}
	}
	if o.ips == nil {
		o.ips = []string{"192.0.2.10"}
	}
	if o.hosts == nil {
		o.hosts = map[string][]string{"pve1.example.com": {"192.0.2.10"}}
	}

	// The cluster's CA and the certificate the API serves.
	cluster := newCA(t, "Proxmox Virtual Environment")
	writeTestFile(t, filepath.Join(root, clusterCA), encodeCerts(cluster.cert))
	keyDER, _ := x509.MarshalPKCS8PrivateKey(cluster.key)
	writeTestFile(t, filepath.Join(root, clusterCAKey), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	signer := cluster
	if o.untrusted {
		signer = newCA(t, "Corporate Root")
	}
	leaf := signer.issue(t, o.names, o.ips)
	writeTestFile(t, filepath.Join(root, pveServedPEM), encodeCerts(leaf))
	h.chain = []*x509.Certificate{leaf}
	if o.untrusted {
		h.chain = append(h.chain, signer.cert)
	}

	ifaces := "auto vmbr0\niface vmbr0 inet static\n\taddress 192.0.2.10/24\n"
	if o.dhcp {
		ifaces = "auto vmbr0\niface vmbr0 inet dhcp\n"
	}
	writeTestFile(t, filepath.Join(root, interfaces), []byte(ifaces))
	writeTestFile(t, filepath.Join(root, resolvConf), []byte("nameserver 192.0.2.53\n"))

	// The template, served over HTTP from loopback.
	body := []byte("template")
	sum := sha512.Sum512(body)
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.hits++
		_, _ = w.Write(body)
	}))
	t.Cleanup(h.srv.Close)
	h.pin = TemplatePin{Name: "test.tar.zst", SHA512: hex.EncodeToString(sum[:]), URL: h.srv.URL + "/test.tar.zst"}
	tmplPath := filepath.Join(root, "tmpl", "test.tar.zst")
	if o.templateHeld {
		writeTestFile(t, tmplPath, body)
	}

	// OpenTofu and the release.
	var tarball bytes.Buffer
	zw := gzip.NewWriter(&tarball)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "tofu", Mode: 0o755, Size: 4, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("tofu"))
	_ = tw.Close()
	_ = zw.Close()
	h.tofu = filepath.Join(work, "tofu-source.tar.gz")
	writeTestFile(t, h.tofu, tarball.Bytes())
	tofuSum := sha256.Sum256(tarball.Bytes())
	h.tofuSum = hex.EncodeToString(tofuSum[:])
	writeTestFile(t, filepath.Join(dir, "share", "kubehenge", "VERSION"), []byte("9.9.9\n"))
	for u := range strings.FieldsSeq(units) {
		writeTestFile(t, filepath.Join(dir, "share", "kubehenge", "appliance", u), []byte("[Unit]\n"))
	}
	writeTestFile(t, filepath.Join(dir, "bin", "kubehenge"), []byte("binary"))

	// The node's commands.
	x := h.exec
	x.on("hostname -f", "pve1.example.com\n")
	x.on("pvesh get /nodes/pve1/network", `[{"type":"bridge","iface":"vmbr0"},{"type":"eth","iface":"eno1"}]`)
	x.fail("pvesh get /cluster/sdn/vnets", 2)
	x.on("ip -4 route show default", "default via 192.0.2.1 dev vmbr0 proto kernel\n")
	x.on("ip -4 -o addr show dev vmbr0", "5: vmbr0    inet 192.0.2.10/24 brd 192.0.2.255 scope global vmbr0\n")
	x.on("ip -4 -o addr show\n", "")
	x.on("ip -4 -o addr show", "1: lo    inet 127.0.0.1/8 scope host lo\n5: vmbr0    inet 192.0.2.10/24 scope global vmbr0\n")
	x.on("ip -4 -o addr show dev vmbr0", "5: vmbr0    inet 192.0.2.10/24 brd 192.0.2.255 scope global vmbr0\n")
	x.on("pvesm status --content rootdir", storageTable(o.rootStorages, "rootdir"))
	x.on("pvesm status --content vztmpl", storageTable(o.templateStorages, "vztmpl"))
	if o.templateHeld {
		x.on("pvesm list local-lvm --content vztmpl", "local-lvm:vztmpl/"+Template+" tzst 1\n")
	}
	x.on("pvesm path", tmplPath+"\n")
	x.on("pvesh get /cluster/nextid", "100\n")
	x.on("pvesh get /cluster/resources", "[]")
	x.fail("pvecm status", 2)
	x.on("pvesh get /cluster/status", `[{"type":"cluster"},{"type":"node","name":"pve1","local":1,"ip":"192.0.2.10"}]`)
	x.on("pct create", "")
	x.on("pct exec 100 -- ip -4 route show default", "default via 192.0.2.1 dev eth0\n")
	x.fail("pct exec 100 -- dpkg-query", 1)
	x.on("pct config 100", "hostname: kubehenge\n")
	h.exec.on("pct exec 100 -- ip -4 -o addr show dev eth0", "2: eth0    inet 192.0.2.77/24 scope global eth0\n")

	h.node = &Node{
		Exec: x, Root: root, Hostname: "pve1",
		LookupHost: func(_ context.Context, name string) ([]string, error) {
			if a, ok := o.hosts[name]; ok {
				return a, nil
			}
			return nil, errors.New("no such host")
		},
		Dial: func(context.Context, string, string) ([]*x509.Certificate, error) { return h.chain, nil },
	}
	return h
}

func (h *harness) session(cfg Config) *Session { //nolint:gocritic // tests pass a Config literal.
	if cfg.Hostname == "" {
		cfg.Hostname = DefaultHostname
	}
	cfg.Tofu = h.tofu
	return &Session{Node: h.node, Cfg: cfg}
}

func (h *harness) runner(s *Session, emit func(Event)) *Runner {
	fetch := NewFetcher("")
	fetch.Sleep = func(time.Duration) {}
	return &Runner{
		S: s, Exec: h.exec, Fetch: fetch, Work: h.work, Dir: h.dir, Emit: emit,
		Sleep: func(time.Duration) {}, Now: func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
		TofuV: "1.0.0", TofuSH: h.tofuSum,
		Realms:  func(context.Context, string, string, []byte) int { return http.StatusOK },
		LogPath: "/var/log/kubehenge-install.log", Pin: h.pin,
	}
}
