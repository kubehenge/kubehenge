package acme

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a time the test moves. A wait the manager asks for reaches the test, which ends it.
type clock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan wait
}

type wait struct {
	d    time.Duration
	fire chan time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), waits: make(chan wait, 64)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.waits <- wait{d, ch}
	return ch
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// next is the wait the manager asks for next. A save's wake-up can make the loop ask twice, the first
// wait then being abandoned, so it gives the last one asked for once no other follows.
func (c *clock) next(t *testing.T) wait {
	t.Helper()
	select {
	case w := <-c.waits:
		return c.settle(w)
	case <-time.After(30 * time.Second):
		t.Fatal("the manager asked for no wait")
		return wait{}
	}
}

func (c *clock) settle(w wait) wait {
	for {
		select {
		case w = <-c.waits:
		case <-time.After(25 * time.Millisecond):
			return w
		}
	}
}

// elapse moves the clock by the wait and ends it.
func (c *clock) elapse(w wait) {
	c.advance(w.d)
	w.fire <- c.Now()
}

// selfSigned makes a certificate for testHost valid from notBefore for lifetime, and gives the
// certificate and its key in PEM.
func selfSigned(t *testing.T, notBefore time.Time, lifetime time.Duration) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: testHost}, DNSNames: []string{testHost},
		NotBefore: notBefore, NotAfter: notBefore.Add(lifetime),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// bench is a manager with a clock the test moves and a server that is a function.
type bench struct {
	m     *Manager
	clock *clock
	logs  *bytes.Buffer
	dir   string

	mu        sync.Mutex
	serving   Served
	obtained  []*x509.Certificate // the certificate each order asked to replace
	delivered int
	obtain    func(call int) error
	ari       func(leaf *x509.Certificate) (time.Time, error)
	gate      chan struct{} // an order waits on it when it is not nil
	entered   chan struct{}
	lifetime  time.Duration
}

func newBench(t *testing.T, dir string) *bench {
	t.Helper()
	b := &bench{clock: newClock(), logs: &bytes.Buffer{}, dir: dir, lifetime: 90 * 24 * time.Hour}
	b.obtain = func(int) error { return nil }
	b.m = b.manager(t)
	return b
}

// manager makes a manager over the bench's directory, as a process start does.
func (b *bench) manager(t *testing.T) *Manager {
	t.Helper()
	m := newManager(t, b.dir)
	m.Log = slog.New(slog.NewTextHandler(&lockedWriter{w: b.logs}, nil))
	m.Now, m.After = b.clock.Now, b.clock.After
	m.Obtain = func(ctx context.Context, _ *Setting, _ string, replaces *x509.Certificate) ([]byte, []byte, error) {
		b.mu.Lock()
		b.obtained = append(b.obtained, replaces)
		call, gate, entered := len(b.obtained), b.gate, b.entered
		b.mu.Unlock()
		if gate != nil {
			entered <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		if err := b.obtain(call); err != nil {
			return nil, nil, err
		}
		c, k := selfSigned(t, b.clock.Now(), b.lifetime)
		return c, k, nil
	}
	m.ARI = func(_ context.Context, _ *Setting, leaf *x509.Certificate) (time.Time, error) {
		if b.ari == nil {
			return time.Time{}, errors.New("the server has no renewal information")
		}
		return b.ari(leaf)
	}
	m.Served = func() Served {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.serving
	}
	m.Deliver = func(_ context.Context, in *Issued) error {
		if !m.Current(in.Generation) {
			return errors.New("superseded")
		}
		leaf := leafOf(t, in)
		b.mu.Lock()
		b.serving = Served{Leaf: leaf, Source: SourceACME, Order: in.OrderID}
		b.delivered++
		b.mu.Unlock()
		return nil
	}
	return m
}

func (b *bench) orders() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.obtained)
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func httpRequest() *Request { return &Request{Directory: DirectoryLetsEncrypt, Proof: ProofHTTP} }

// A failed order keeps the certificate served, records the error, waits a minute, then doubles up to
// an hour, and after a day of failures stops with one logged event.
func TestFailedOrdersBackOffAndGiveUpAfterADay(t *testing.T) {
	t.Parallel()
	b := newBench(t, t.TempDir())
	current := Served{Leaf: leafOf(t, &Issued{CertPEM: first(selfSigned(t, b.clock.Now(), 24*time.Hour))}), Source: "uploaded"}
	b.serving = current
	b.obtain = func(int) error { return errors.New("the server is down") }
	runUntilCleanup(t, b.m)
	if _, err := b.m.Save(httpRequest()); err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	for !b.m.View().GivenUp {
		var w wait
		select {
		case w = <-b.clock.waits:
			w = b.clock.settle(w)
		case <-time.After(20 * time.Millisecond):
			continue
		}
		waits = append(waits, w.d)
		v := b.m.View()
		if !strings.Contains(v.Error, "The server is down") || v.State != "failing" || v.NextAttempt == nil {
			t.Fatalf("the view is %+v", v)
		}
		b.clock.elapse(w)
	}
	v := b.m.View()
	if v.NextAttempt != nil || v.State != "failing" || !strings.Contains(v.Error, "The server is down") {
		t.Errorf("after giving up the view is %+v", v)
	}
	if msg, gaveUp := b.m.Failed(); !gaveUp || !strings.Contains(msg, "The server is down") {
		t.Errorf("Failed gives %q, %v", msg, gaveUp)
	}
	if got := strings.Count(b.logs.String(), "giving up"); got != 1 {
		t.Errorf("%d lines say the interface gives up, want 1", got)
	}
	b.mu.Lock()
	servedNow, delivered := b.serving, b.delivered
	b.mu.Unlock()
	if servedNow.Leaf != current.Leaf || delivered != 0 {
		t.Error("a failed order replaced the certificate served")
	}
	// Saving the setting again starts afresh.
	before := b.orders()
	if _, err := b.m.Save(httpRequest()); err != nil {
		t.Fatal(err)
	}
	b.clock.next(t)
	if b.orders() <= before || b.m.View().GivenUp {
		t.Errorf("a new save made %d orders and left GivenUp %v", b.orders()-before, b.m.View().GivenUp)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	if len(waits) < len(want) || !equalDurations(waits[:len(want)], want) {
		t.Errorf("the waits are %v, want %v first\n%s", waits, want, b.logs.String())
	}
	for _, d := range waits {
		if d > time.Hour {
			t.Errorf("a wait of %v is longer than an hour", d)
		}
	}
}

func first(a, _ []byte) []byte { return a }

// stateRefused is a store that cannot write the progress file while refuse is set, as on a full disk.
type stateRefused struct {
	store

	refuse atomic.Bool
}

func (s *stateRefused) write(name string, data []byte) error {
	if name == StateFile && s.refuse.Load() {
		return errors.New("no space left on device")
	}
	return s.store.write(name, data) //nolint:wrapcheck // the test's own seam
}

// With the progress file not written, as on a full disk, a failed order and a question for renewal
// information each wait the back-off. The files alone would make them due again at once.
func TestProgressThatCannotBeKeptStillBacksOff(t *testing.T) {
	t.Parallel()
	b := newBench(t, t.TempDir())
	full := &stateRefused{store: b.m.store}
	b.m.store = full
	b.obtain = func(int) error { return errors.New("the server is down") }
	if _, err := b.m.Save(httpRequest()); err != nil {
		t.Fatal(err)
	}
	full.refuse.Store(true)
	runUntilCleanup(t, b.m)
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute} {
		w := b.clock.next(t)
		if w.d != want || b.orders() != i+1 {
			t.Fatalf("after %d orders the wait is %v, want %d orders and %v", b.orders(), w.d, i+1, want)
		}
		b.clock.elapse(w)
	}

	a := newBench(t, t.TempDir())
	asked := atomic.Int32{}
	a.ari = func(*x509.Certificate) (time.Time, error) {
		asked.Add(1)
		return a.clock.Now().Add(10 * 24 * time.Hour), nil
	}
	full = &stateRefused{store: a.m.store}
	a.m.store = full
	runUntilCleanup(t, a.m)
	a.issueFirst(t, a.m)
	w := a.clock.next(t)
	full.refuse.Store(true)
	before := asked.Load()
	a.clock.elapse(w)
	if w = a.clock.next(t); w.d != time.Minute || asked.Load() != before+1 {
		t.Errorf("renewal information was asked %d times, then the wait is %v, want once and a minute", asked.Load()-before, w.d)
	}
}

func equalDurations(a, b []time.Duration) bool {
	for i := range b {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// until polls for a condition the manager's goroutine brings about.
func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (b *bench) delivers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.delivered
}

// issueFirst saves the setting and waits for the first certificate.
func (b *bench) issueFirst(t *testing.T, m *Manager) *x509.Certificate {
	t.Helper()
	if _, err := m.Save(httpRequest()); err != nil {
		t.Fatal(err)
	}
	until(t, "the first certificate", func() bool { return b.delivers() == 1 })
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.serving.Leaf
}

// The loop renews at the time the server's renewal information gives, asks the server again every
// six hours, and asks it to replace the certificate it renews.
func TestRenewalHappensAtTheTimeTheServerGives(t *testing.T) {
	t.Parallel()
	b := newBench(t, t.TempDir())
	b.ari = func(*x509.Certificate) (time.Time, error) { return b.clock.Now().Add(10 * 24 * time.Hour), nil }
	runUntilCleanup(t, b.m)
	leaf := b.issueFirst(t, b.m)
	w := b.clock.next(t)
	if w.d != ariEvery {
		t.Fatalf("the first wait is %v, want %v, the time to ask the server again", w.d, ariEvery)
	}
	// Six hours on, the server moves its window to two hours from then.
	b.ari = func(*x509.Certificate) (time.Time, error) { return b.clock.Now().Add(2 * time.Hour), nil }
	b.clock.elapse(w)
	w = b.clock.next(t)
	if w.d != 2*time.Hour || b.orders() != 1 {
		t.Fatalf("the next wait is %v after %d orders, want 2h after 1", w.d, b.orders())
	}
	b.clock.elapse(w)
	until(t, "the renewal", func() bool { return b.delivers() == 2 })
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.obtained[1] != leaf {
		t.Error("the renewal did not name the certificate it replaces")
	}
}

// With no renewal information the loop renews when a third of the lifetime is left.
func TestRenewalFallsBackToAThirdOfTheLifetime(t *testing.T) {
	t.Parallel()
	b := newBench(t, t.TempDir())
	runUntilCleanup(t, b.m)
	leaf := b.issueFirst(t, b.m)
	w := b.clock.next(t)
	wantAt := leaf.NotAfter.Add(-30 * 24 * time.Hour)
	if v := b.m.View(); v.NextRenewal == nil || !v.NextRenewal.Equal(wantAt) {
		t.Errorf("the next renewal is %v, want %v", v.NextRenewal, wantAt)
	}
	if w.d != ariEvery {
		t.Errorf("the wait is %v, want the time to ask again", w.d)
	}
	b.clock.advance(wantAt.Sub(b.clock.Now()))
	w.fire <- b.clock.Now()
	until(t, "the renewal", func() bool { return b.delivers() == 2 })
}

// A restart in the middle of an order carries it on under the same id, and a restart between orders
// renews at the time already worked out.
func TestARestartCarriesTheOrderOn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := newBench(t, dir)
	b.gate, b.entered = make(chan struct{}), make(chan struct{}, 8)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.m.Run(ctx); close(done) }()
	if _, err := b.m.Save(httpRequest()); err != nil {
		t.Fatal(err)
	}
	<-b.entered
	_, st, err := b.m.load()
	if err != nil || st.Want == "" {
		t.Fatalf("the progress is %+v (%v)", st, err)
	}
	want := st.Want
	stop()
	<-done
	if v := b.m.View(); v == nil || v.Error != "" {
		t.Fatalf("a stop recorded %+v as a failure", v)
	}
	// A new process reads the files.
	m2 := b.manager(t)
	close(b.gate)
	runUntilCleanup(t, m2)
	until(t, "the order to finish after the restart", func() bool { return b.delivers() == 1 })
	b.mu.Lock()
	got := b.serving.Order
	b.mu.Unlock()
	if got != want {
		t.Errorf("the certificate carries order %q, want the one started before the restart, %q", got, want)
	}
	// Another restart finds a certificate served and an order done, and orders nothing.
	m3 := b.manager(t)
	runUntilCleanup(t, m3)
	w := b.clock.next(t)
	if b.orders() != 2 || w.d <= 0 {
		t.Errorf("%d orders and a wait of %v after the restart", b.orders(), w.d)
	}
}

// One order runs at a time: a request to order while one runs is refused, and saving anew ends the
// order under way, whose certificate is never served.
func TestOneOrderRunsAtATime(t *testing.T) {
	t.Parallel()
	b := newBench(t, t.TempDir())
	b.gate, b.entered = make(chan struct{}), make(chan struct{}, 8)
	runUntilCleanup(t, b.m)
	if _, err := b.m.Save(httpRequest()); err != nil {
		t.Fatal(err)
	}
	<-b.entered
	if _, err := b.m.OrderNow(); refusalCode(t, err) != CodeOrdering {
		t.Errorf("ordering during an order gives %v", err)
	}
	if v := b.m.View(); v.State != "ordering" {
		t.Errorf("the state is %q", v.State)
	}
	if _, err := b.m.Save(httpRequest()); err != nil {
		t.Fatal(err)
	}
	<-b.entered
	close(b.gate)
	until(t, "the second order's certificate", func() bool { return b.delivers() >= 1 })
	if b.delivers() != 1 {
		t.Errorf("%d certificates were served, want only the second order's", b.delivers())
	}
	if b.m.View().Error != "" {
		t.Errorf("an order that was ended recorded %q", b.m.View().Error)
	}
}

// Switching away from ACME removes the setting, its secrets, its progress and its account, and the
// loop never orders again. A change that fails puts them back.
func TestDisablingRemovesTheSettingAndItsSecrets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := newBench(t, dir)
	runUntilCleanup(t, b.m)
	if _, err := b.m.Save(httpRequest()); err != nil {
		t.Fatal(err)
	}
	until(t, "the first certificate", func() bool { return b.delivers() == 1 })
	if err := b.m.store.write(accountFile, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	snap := b.m.Disable()
	if left := acmeFiles(t, dir); len(left) != 0 || b.m.View() != nil {
		t.Errorf("the directory keeps %v and the view is %v", left, b.m.View())
	}
	snap.Restore()
	if left := acmeFiles(t, dir); len(left) != 3 {
		t.Errorf("a restore left %v", left)
	}
	// An administrator's own certificate after ACME's ends it without a request.
	b.mu.Lock()
	b.serving.Source = "uploaded"
	b.mu.Unlock()
	b.m.poke()
	until(t, "the setting to be removed", func() bool { return len(acmeFiles(t, dir)) == 0 })
	if b.orders() != 1 {
		t.Errorf("%d orders", b.orders())
	}
}

func acmeFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "acme-") {
			out = append(out, e.Name())
		}
	}
	return out
}
