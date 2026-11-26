package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// fakeClock moves only when the check waits, so no test waits for real.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.at
	return ch
}

// healthRun is one run of "kubehenge appliance health" against a fake interface.
type healthRun struct {
	stdout, stderr string
	err            error
	address        string
}

// runHealth runs the command against a server that answers each request with status(n), the
// n-th request from 1, on a terminal or not. The certificate the command expects is the
// server's unless other, the DER of another certificate, is set.
func runHealth(t *testing.T, terminal bool, status func(n int) int, other []byte, args ...string) healthRun {
	t.Helper()
	savedTerminal := isTerminal
	t.Cleanup(func() { isTerminal = savedTerminal })
	isTerminal = func(*os.File) bool { return terminal }
	t.Setenv("TERM", "xterm")

	var asked atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status(int(asked.Add(1))))
	}))
	t.Cleanup(srv.Close)
	expected := srv.Certificate().Raw
	if other != nil {
		expected = other
	}
	cert := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: expected}), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	o := &applianceOptions{check: appliance.ServiceCheck{
		Now: clock.now, After: clock.after,
		Restarts: func(_ context.Context) (int, error) { return 0, nil },
	}}
	cmd := o.healthCommand()
	var stdout, stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetContext(t.Context())
	address := srv.Listener.Addr().String()
	cmd.SetArgs(append([]string{"--cert", cert, "--stored-cert", filepath.Join(t.TempDir(), "none"), "--address", address}, args...))
	err := cmd.Execute()
	return healthRun{stdout: stdout.String(), stderr: stderr.String(), err: err, address: address}
}

// otherCertificate is a self-signed certificate that names the host the check asks for.
func otherCertificate(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "expected.example.test"},
		DNSNames: []string{"expected.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// answerAfter answers 502 to the first n requests and 204 after.
func answerAfter(n int) func(int) int {
	return func(i int) int {
		if i <= n {
			return http.StatusBadGateway
		}
		return http.StatusNoContent
	}
}

func TestHealthShowsEachStageWithoutATerminal(t *testing.T) { //nolint:paralleltest // stands in for the machine's terminal
	got := runHealth(t, false, answerAfter(2), nil, "--wait", "2m", "--settle", "3s")
	if got.err != nil {
		t.Fatal(got.err)
	}
	lines := strings.Split(strings.TrimSuffix(got.stderr, "\n"), "\n")
	want := []string{"Connecting to " + got.address + ", waiting up to 2 min", "", "Answered after 4 s", "Staying up for 3 s", "Stayed up for 3 s"}
	if len(lines) != len(want) {
		t.Fatalf("stages: %q", got.stderr)
	}
	for i, line := range want {
		if line != "" && lines[i] != line {
			t.Errorf("stage %d is %q, want %q", i, lines[i], line)
		}
	}
	if !strings.HasPrefix(lines[1], "Certificate matches ") || !strings.HasSuffix(lines[1], "tls.crt") {
		t.Errorf("certificate line: %q", lines[1])
	}
	if strings.ContainsAny(got.stderr, "\r\x1b") {
		t.Errorf("control characters without a terminal: %q", got.stderr)
	}
	if want := "Healthy: Kubehenge " + release.Version + " answers on " + got.address + "\n"; got.stdout != want {
		t.Errorf("verdict %q, want %q", got.stdout, want)
	}
}

func TestHealthCountsDownOnATerminal(t *testing.T) { //nolint:paralleltest // stands in for the machine's terminal
	got := runHealth(t, true, answerAfter(1), nil, "--wait", "2m", "--settle", "2s")
	if got.err != nil {
		t.Fatal(got.err)
	}
	for _, want := range []string{
		clearLine + "Connecting to " + got.address + ", 2 min left",
		clearLine + "Connecting to " + got.address + ", 1 min 58 s left",
		clearLine + "Staying up, 2 s left",
		clearLine + "Staying up, 1 s left",
		clearLine + "Staying up, 0 s left",
		"Answered after 2 s\n",
		"Stayed up for 2 s\n",
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("missing %q in %q", want, got.stderr)
		}
	}
	if !strings.HasSuffix(got.stderr, clearLine+"Stayed up for 2 s\n") {
		t.Errorf("the screen ends on %q", got.stderr)
	}
	if !strings.HasPrefix(got.stdout, "Healthy: Kubehenge ") {
		t.Errorf("verdict %q", got.stdout)
	}
}

func TestHealthNamesTheAddressAndTheTimeItWaited(t *testing.T) { //nolint:paralleltest // stands in for the machine's terminal
	got := runHealth(t, false, func(int) int { return http.StatusBadGateway }, nil, "--wait", "10s", "--settle", "3s")
	if got.err == nil {
		t.Fatal("an interface that never answered passed")
	}
	if !strings.HasPrefix(got.err.Error(), "not healthy: the interface at "+got.address+" did not answer within 10s: the interface answered 502") ||
		!strings.HasSuffix(got.err.Error(), ". Read the interface's log with journalctl -u kubehenge.service") {
		t.Errorf("error %q", got.err)
	}
	if got.stdout != "" {
		t.Errorf("a verdict for a failure: %q", got.stdout)
	}
	if strings.Contains(got.stderr, "Answered") {
		t.Errorf("stages of an answer that never came: %q", got.stderr)
	}
}

func TestHealthNamesTheCertificatesWhenTheWrongOneAnswers(t *testing.T) { //nolint:paralleltest // stands in for the machine's terminal
	got := runHealth(t, false, func(int) int { return http.StatusNoContent }, otherCertificate(t), "--wait", "6s", "--settle", "3s")
	if got.err == nil {
		t.Fatal("the wrong certificate passed")
	}
	msg := got.err.Error()
	for _, want := range []string{"serves a certificate other than ", "tls.crt: expected expected.example.test (issued by expected.example.test, sha256 ", ", presented example.com (issued by O=Acme Co, sha256 ", "Check that --cert and --stored-cert"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
}

func TestHealthSaysWhenTheInterfaceWentDownWhileSettling(t *testing.T) { //nolint:paralleltest // stands in for the machine's terminal
	got := runHealth(t, false, func(n int) int {
		if n >= 4 {
			return http.StatusBadGateway
		}
		return http.StatusNoContent
	}, nil, "--wait", "10s", "--settle", "20s")
	if got.err == nil {
		t.Fatal("an interface that went down passed")
	}
	if want := "the interface stopped answering 3s into the 20s it had to stay up: the interface answered 502"; !strings.Contains(got.err.Error(), want) {
		t.Errorf("error %q lacks %q", got.err, want)
	}
	if strings.Contains(got.stderr, "Stayed up") || got.stdout != "" {
		t.Errorf("a pass for a failure: %q, %q", got.stderr, got.stdout)
	}
}

func TestHealthQuietPrintsNothing(t *testing.T) { //nolint:paralleltest // stands in for the machine's terminal
	got := runHealth(t, true, answerAfter(1), nil, "--quiet", "--wait", "10s", "--settle", "2s")
	if got.err != nil || got.stdout != "" || got.stderr != "" {
		t.Errorf("quiet run: err %v, stdout %q, stderr %q", got.err, got.stdout, got.stderr)
	}
	got = runHealth(t, true, func(int) int { return http.StatusBadGateway }, nil, "--quiet", "--wait", "4s", "--settle", "2s")
	if got.err == nil || strings.Contains(got.err.Error(), "not healthy") || got.stdout != "" || got.stderr != "" {
		t.Errorf("quiet failure: err %v, stdout %q, stderr %q", got.err, got.stdout, got.stderr)
	}
}

func TestHealthJSONPrintsOneObject(t *testing.T) { //nolint:paralleltest // stands in for the machine's terminal
	got := runHealth(t, false, answerAfter(1), nil, "--json", "--wait", "10s", "--settle", "2s")
	if got.err != nil {
		t.Fatal(got.err)
	}
	var v healthVerdict
	if err := json.Unmarshal([]byte(got.stdout), &v); err != nil || strings.Count(got.stdout, "\n") != 1 {
		t.Fatalf("stdout %q: %v", got.stdout, err)
	}
	if want := (healthVerdict{Healthy: true, Address: got.address, Version: release.Version, AnsweredAfterSeconds: 2, StayedUpSeconds: 2}); v != want {
		t.Errorf("verdict %+v, want %+v", v, want)
	}

	got = runHealth(t, false, func(int) int { return http.StatusBadGateway }, nil, "--json", "--wait", "4s", "--settle", "2s")
	v = healthVerdict{}
	if err := json.Unmarshal([]byte(got.stdout), &v); err != nil || got.err == nil {
		t.Fatalf("failure: stdout %q, err %v", got.stdout, got.err)
	}
	if v.Healthy || !strings.Contains(v.Error, "did not answer within 4s") || !strings.Contains(v.Next, "journalctl") {
		t.Errorf("failed verdict %+v", v)
	}
}
