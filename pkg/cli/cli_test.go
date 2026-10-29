package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/kubehenge/kubehenge/pkg/release"
)

const archiveSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

func TestEveryCommandIsDescribed(t *testing.T) {
	t.Parallel()
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Short == "" {
			t.Errorf("%q has no short description", c.CommandPath())
		}
		if !c.HasSubCommands() && c.RunE == nil {
			t.Errorf("%q does nothing", c.CommandPath())
		}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Usage == "" {
				t.Errorf("%s --%s has no usage", c.CommandPath(), f.Name)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(NewRoot())
}

func TestReleaseKeySignsAnIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "release.key")
	out, err := run(t, "release", "keygen", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("keygen printed %q, not a public key: %v", out, err)
	}
	if info, statErr := os.Stat(keyFile); statErr != nil || (os.PathSeparator == '/' && info.Mode().Perm() != privateKeyMode) {
		t.Fatalf("private key file: %v, %v", info, statErr)
	}
	if _, err = run(t, "release", "keygen", keyFile); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}

	index, err := run(t, "release", "index", "--version", "1.0.0", "--url", "https://example.org/kubehenge-1.0.0.tar.gz", "--sha256", archiveSHA)
	if err != nil {
		t.Fatal(err)
	}
	indexFile := filepath.Join(dir, "index.json")
	err = os.WriteFile(indexFile, []byte(index), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run(t, "release", "sign", keyFile, indexFile); err != nil {
		t.Fatal(err)
	}
	sig, err := fs.ReadFile(os.DirFS(dir), "index.json.sig")
	if err != nil {
		t.Fatal(err)
	}
	ix, err := release.Parse([]byte(index), strings.TrimSpace(string(sig)), pub)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Releases) != 1 || ix.Releases[0].Version != "1.0.0" || ix.Releases[0].Tofu != nil {
		t.Fatalf("index = %+v", ix)
	}
}

func TestReleaseIndexRefusesWhatItCannotPublish(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"no version":   {"--url", "https://example.org/a.tar.gz", "--sha256", archiveSHA},
		"plain http":   {"--version", "1.0.0", "--url", "http://example.org/a.tar.gz", "--sha256", archiveSHA},
		"short digest": {"--version", "1.0.0", "--url", "https://example.org/a.tar.gz", "--sha256", "abc"},
		"half of tofu": {"--version", "1.0.0", "--url", "https://example.org/a.tar.gz", "--sha256", archiveSHA, "--tofu-version", "1.10.0"},
		"missing base": {"--version", "1.0.0", "--url", "https://example.org/a.tar.gz", "--sha256", archiveSHA, "--previous", "missing.json"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if out, err := run(t, append([]string{"release", "index"}, args...)...); err == nil {
				t.Fatalf("wrote an index: %s", out)
			}
		})
	}
}

func TestSignRefusesAFileWithoutASeed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyFile, indexFile := filepath.Join(dir, "key"), filepath.Join(dir, "index.json")
	for path, content := range map[string]string{keyFile: "not a key\n", indexFile: "{}"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := run(t, "release", "sign", keyFile, indexFile)
	if err == nil || !strings.Contains(err.Error(), "no Ed25519 seed") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(indexFile + ".sig"); !os.IsNotExist(statErr) {
		t.Fatalf("a signature was written: %v", statErr)
	}
}

func TestSignTakesOnlyTheTrustedKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyFile, indexFile := filepath.Join(dir, "release.key"), filepath.Join(dir, "index.json")
	out, err := run(t, "release", "keygen", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(indexFile, []byte(`{"releases":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	other, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(trusted ed25519.PublicKey) error {
		cmd := newSignCommand(trusted)
		cmd.SetArgs([]string{keyFile, indexFile})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		return cmd.ExecuteContext(t.Context())
	}
	if err = sign(other); err == nil || !strings.Contains(err.Error(), "not the release key") {
		t.Fatalf("signed with a key the build does not trust: %v", err)
	}
	if _, statErr := os.Stat(indexFile + ".sig"); !os.IsNotExist(statErr) {
		t.Fatalf("a signature was written: %v", statErr)
	}
	if err = sign(pub); err != nil {
		t.Fatalf("refused the trusted key: %v", err)
	}
}

func TestWaitOperatorNeedsANamespace(t *testing.T) {
	t.Parallel()
	_, err := run(t, "wait", "operator")
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiagnoseNeedsNamespaceAndCluster(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"web", "team-a/", "/web"} {
		_, err := run(t, "diagnose", arg)
		if err == nil || !strings.Contains(err.Error(), "<namespace>/<cluster>") {
			t.Errorf("%s: err = %v", arg, err)
		}
	}
}

func TestChartValuesGoToTheirCharts(t *testing.T) {
	t.Parallel()
	const digest = "@sha256:0805a5273118c436c9772bb9566d87ec558d8b25cc2ce5dc32918ecdd36f2c7c"
	options := func(values string) *controllerOptions {
		o := &controllerOptions{
			kubeRBACProxy: "ghcr.io/example/proxy:v1" + digest, kubeVipImage: "ghcr.io/example/vip:v1" + digest,
			turtlesImage: "docker.io/example/turtles:v1" + digest, turtlesCAPIImage: "registry.example.test/example/capi:v1" + digest,
			alloyImage:  "docker.io/example/alloy:v1" + digest,
			veleroImage: "docker.io/example/velero:v1" + digest, veleroPlugin: "docker.io/example/plugin:v1" + digest,
			calicoImages: []string{"quay.io/calico/node:v3.33.0" + digest}, chartValues: values,
			autoscalerImages: []string{"registry.k8s.io/autoscaling/cluster-autoscaler:v1.36.1" + digest},
		}
		for _, f := range []*string{
			&o.ccmChart, &o.csiChart, &o.csiNewestChart, &o.snapshotChart, &o.ciliumChart, &o.calicoChart, &o.headlampChart, &o.certManagerChart, &o.rancherChart,
			&o.monitoringChart, &o.lokiChart, &o.alloyChart, &o.veleroChart, &o.kyvernoChart, &o.kyvernoPolicies, &o.istioChart,
			&o.istioBaseChart, &o.istioCNIChart, &o.istioZtunnelChart, &o.traefikChart, &o.metalLBChart, &o.kubeVipChart, &o.kubeVipCloudChart,
			&o.argoCDChart, &o.fluxOperatorChart,
		} {
			*f = "https://charts.example.test/chart:1.0.0" + digest
		}
		return o
	}
	o := options(`{"cert-manager":{"image":{"tag":"v1.21.2` + digest + `"}},"istio-ztunnel":{"image":"ztunnel"},"snapshot-controller":{"controller":{}}}`)
	if err := o.parseCharts(); err != nil {
		t.Fatal(err)
	}
	if o.r.Charts.CertManager.Values["image"] == nil || o.r.Charts.IstioZtunnel.Values["image"] != "ztunnel" || o.r.Charts.CCM.Values != nil {
		t.Errorf("cert-manager %v, ztunnel %v, the CCM %v", o.r.Charts.CertManager.Values, o.r.Charts.IstioZtunnel.Values, o.r.Charts.CCM.Values)
	}
	if o.r.Charts.SnapshotController.Values["controller"] == nil {
		t.Errorf("the snapshot controller's values %v", o.r.Charts.SnapshotController.Values)
	}
	for values, want := range map[string]string{`{"certmanager":{}}`: "certmanager", `[]`: "--chart-values"} {
		if err := options(values).parseCharts(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", values, err, want)
		}
	}
}

func TestParseNetwork(t *testing.T) {
	t.Parallel()
	n, err := parseNetwork("192.0.2.50/24", "192.0.2.1", []string{"192.0.2.1", "198.51.100.53"})
	if err != nil || n.Address.String() != "192.0.2.50/24" || n.Gateway.String() != "192.0.2.1" || len(n.DNS) != 2 {
		t.Fatalf("network = %+v, %v", n, err)
	}
	for name, c := range map[string]struct {
		address, gateway string
		dns              []string
		want             string
	}{
		"address":         {"192.0.2.50", "192.0.2.1", nil, "--build-address"},
		"gateway":         {"192.0.2.50/24", "gateway", nil, "--build-gateway"},
		"gateway outside": {"192.0.2.50/24", "198.51.100.1", nil, "outside"},
		"dns":             {"192.0.2.50/24", "192.0.2.1", []string{"dns.example.org"}, "--build-dns"},
	} {
		if _, err = parseNetwork(c.address, c.gateway, c.dns); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestListenPort(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]int{":8443": 8443, "127.0.0.1:443": 443, "[::1]:": defaultHTTPSPort} {
		if got, err := listenPort(addr); err != nil || got != want {
			t.Errorf("listenPort(%q) = %d, %v; want %d", addr, got, err, want)
		}
	}
	for _, addr := range []string{"8443", ":https"} {
		if _, err := listenPort(addr); err == nil {
			t.Errorf("listenPort(%q) took it", addr)
		}
	}
}

func TestServerTimeouts(t *testing.T) {
	t.Parallel()
	bounded := newServer(":443", http.NotFoundHandler(), time.Minute)
	if bounded.ReadTimeout == 0 || bounded.WriteTimeout != time.Minute || bounded.ReadHeaderTimeout == 0 || bounded.TLSConfig.MinVersion == 0 {
		t.Fatalf("bounded server = %+v", bounded)
	}
	// A long-lived stream must not be cut, but a client still has to send its headers in time.
	open := newServer(":443", http.NotFoundHandler(), 0)
	if open.ReadTimeout != 0 || open.WriteTimeout != 0 || open.ReadHeaderTimeout == 0 || open.IdleTimeout == 0 {
		t.Fatalf("open server = %+v", open)
	}
}

func TestTrimNewline(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"key\n": "key", "key\r\n": "key", "key": "key", "\n": ""} {
		if got := string(trimNewline([]byte(in))); got != want {
			t.Errorf("trimNewline(%q) = %q, want %q", in, got, want)
		}
	}
}
