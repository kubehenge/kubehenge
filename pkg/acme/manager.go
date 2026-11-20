package acme

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// SourceACME is the source the interface's certificate is given when ACME issued it.
const SourceACME = "acme"

const (
	// retryFirst is the wait after a first failure. Each failure doubles it up to retryCap.
	retryFirst = time.Minute
	retryCap   = time.Hour
	// giveUpAfter is how long failures may go on before the interface stops trying.
	giveUpAfter = 24 * time.Hour
	// ariEvery is how often the server is asked again when to renew, as RFC 9773 recommends.
	ariEvery = 6 * time.Hour
	// orderTimeout bounds one order, DNS propagation included.
	orderTimeout = 30 * time.Minute
)

// Served is the certificate the interface serves now.
type Served struct {
	Leaf *x509.Certificate
	// Source is how it came: installer, uploaded, self-signed or acme.
	Source string
	// Order is the id of the order that issued it, for a certificate from ACME.
	Order string
}

// Manager holds the ACME setting and keeps the certificate renewed. Its files are read again at each
// decision, so what it does follows what is on disk.
type Manager struct {
	// Name gives the appliance's public DNS name, empty when it has none.
	Name func() string
	// HTTPListening tells that the interface answers on port 80, which the http-01 proof needs.
	HTTPListening bool
	Log           *slog.Logger
	// Roots are the roots that vouch for the ACME directory, with the setting's own CA added. Nil takes
	// the system's.
	Roots *x509.CertPool
	// Served tells which certificate is served.
	Served func() Served
	// Deliver puts a certificate in place. It refuses one that is not valid for the public name, and
	// one whose generation is no longer current.
	Deliver func(ctx context.Context, in *Issued) error
	Now     func() time.Time
	// After gives a channel that receives once the duration has passed.
	After func(time.Duration) <-chan time.Time
	// Obtain asks the server for a certificate for name. Nil asks as lego does.
	Obtain func(ctx context.Context, set *Setting, name string, replaces *x509.Certificate) (certPEM, keyPEM []byte, err error)
	// ARI asks when the server wants the certificate renewed. Nil asks as lego does.
	ARI func(ctx context.Context, set *Setting, leaf *x509.Certificate) (time.Time, error)
	// NewProvider turns a DNS provider's name and credentials into a provider. Nil takes lego's.
	NewProvider ProviderFunc

	store      store
	challenges challenges
	// mu guards the files and the fields below it. It is never held while a request goes out.
	mu       sync.Mutex
	cancel   context.CancelFunc
	ordering bool
	wake     chan struct{}
}

// New makes a manager that keeps its files in dir, or in memory when dir is empty.
func New(dir string) *Manager {
	m := &Manager{Now: time.Now, After: time.After, wake: make(chan struct{}, 1)}
	if dir == "" {
		m.store = &memoryStore{}
		return m
	}
	ds := dirStore{dir: dir}
	ds.leftovers()
	m.store = ds
	return m
}

// Save keeps the setting and starts the first order. It refuses a setting the public name or the
// DNS provider cannot satisfy, with the reason. The caller holds the certificate's change lock.
func (m *Manager) Save(req *Request) (*View, error) {
	name := m.PublicName()
	if name == "" {
		return nil, refuse(CodeNoPublicName, "ACME needs the appliance's public DNS name. Start the interface with --public-url set to an https address with a DNS name")
	}
	if req.Proof == ProofHTTP && !m.HTTPListening {
		return nil, refuse(CodeNoHTTP, "The %s proof needs the interface to listen on port 80. Start it with --http-listen :80", ProofHTTP)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, _, err := m.load()
	if err != nil {
		return nil, err
	}
	set, bad := build(req, old)
	if bad != nil {
		return nil, bad
	}
	if set.Proof == ProofDNS {
		if bad = m.checkProvider(set); bad != nil {
			return nil, bad
		}
	}
	set.Generation = newID()
	raw, err := set.marshal()
	if err != nil {
		return nil, err
	}
	// The account belongs to a directory. Another directory registers a new one.
	if acct, ok := m.readAccount(); ok && acct.Directory != set.Directory {
		if err = m.store.remove(accountFile); err != nil {
			return nil, err
		}
	}
	if err = m.store.write(settingFile, raw); err != nil {
		return nil, err
	}
	st := &State{Want: set.Generation}
	if err = m.saveState(st); err != nil {
		return nil, err
	}
	m.stopOrderLocked()
	m.poke()
	return m.viewLocked(set, st), nil
}

// OrderNow asks for a certificate at once, as a renewal on demand.
func (m *Manager) OrderNow() (*View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, st, err := m.load()
	if err != nil {
		return nil, err
	}
	if set == nil {
		return nil, refuse(CodeNotSet, "ACME is not set. Save a setting first")
	}
	if m.ordering {
		return nil, refuse(CodeOrdering, "An order is already under way. Wait for it to end")
	}
	*st = State{Want: newID(), Delivered: st.Delivered, LastSuccess: st.LastSuccess, RenewalFor: st.RenewalFor, NextRenewal: st.NextRenewal, ARIChecked: st.ARIChecked}
	if err = m.saveState(st); err != nil {
		return nil, err
	}
	m.poke()
	return m.viewLocked(set, st), nil
}

// Snapshot is what Disable removed, to put back when the change that asked for it fails.
type Snapshot struct {
	m     *Manager
	files map[string][]byte
}

// Disable stops renewals and removes the setting, its secrets, its progress and its account. A
// certificate chosen after ACME must never be replaced by it. The caller holds the certificate's
// change lock.
func (m *Manager) Disable() *Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := &Snapshot{m: m, files: map[string][]byte{}}
	for _, name := range files {
		if raw, err := m.store.read(name); err == nil {
			snap.files[name] = raw
		}
		if err := m.store.remove(name); err != nil {
			m.log().Error("removing the ACME setting", "file", name, "error", err)
		}
	}
	m.stopOrderLocked()
	return snap
}

// Restore puts back what Disable removed.
func (s *Snapshot) Restore() {
	if s == nil || len(s.files) == 0 {
		return
	}
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	for name, raw := range s.files {
		if err := s.m.store.write(name, raw); err != nil {
			s.m.log().Error("restoring the ACME setting", "file", name, "error", err)
		}
	}
	s.m.poke()
}

// Current tells whether generation is the saved setting's.
func (m *Manager) Current(generation string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, _, err := m.load()
	return err == nil && set != nil && set.Generation == generation
}

// PublicName is the DNS name the certificate is ordered for, empty when the appliance has none.
func (m *Manager) PublicName() string {
	if m.Name == nil {
		return ""
	}
	name := m.Name()
	if _, err := netip.ParseAddr(name); err == nil {
		return ""
	}
	return name
}

// Providers are the names of the DNS providers lego knows.
func Providers() []string {
	names := providerNames()
	slices.Sort(names)
	return names
}

// Fingerprint is the SHA-256 of a certificate, as hex.
func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

// Run orders and renews until ctx ends. One order runs at a time.
func (m *Manager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		due, recheck, ok := m.decide(ctx)
		if ok && !due.After(m.now()) {
			m.order(ctx)
			continue
		}
		var timer <-chan time.Time
		if ok {
			// The server's renewal window can move, so it is asked again before the order is due.
			if !recheck.IsZero() && recheck.Before(due) {
				due = recheck
			}
			timer = m.After(due.Sub(m.now()))
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-timer:
		}
	}
}

// View is the setting and its progress, or nil when ACME is not set.
func (m *Manager) View() *View {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, st, err := m.load()
	if err != nil || set == nil {
		return nil
	}
	return m.viewLocked(set, st)
}

// Failed tells whether the interface gave up, with the error it gave up on.
func (m *Manager) Failed() (string, bool) {
	v := m.View()
	if v == nil || !v.GivenUp {
		return "", false
	}
	return strings.TrimSpace(v.Error), true
}

// decide reads the files and the served certificate and gives when the next order is due, when the
// server is to be asked for renewal information again (zero when it is not to be), and whether
// anything is to be ordered at all. It asks the server for renewal information when that is due.
func (m *Manager) decide(ctx context.Context) (time.Time, time.Time, bool) {
	m.mu.Lock()
	set, st, err := m.load()
	if err != nil {
		m.mu.Unlock()
		m.log().Error("reading the ACME setting", "error", err)
		return time.Time{}, time.Time{}, false
	}
	if set == nil {
		m.mu.Unlock()
		return time.Time{}, time.Time{}, false
	}
	served := m.served()
	// A setting whose certificate has since been replaced by an administrator is no longer wanted.
	if st.Delivered && served.Source != SourceACME && served.Leaf != nil {
		m.mu.Unlock()
		m.Disable()
		return time.Time{}, time.Time{}, false
	}
	if st.Want != "" && served.Source == SourceACME && served.Order == st.Want {
		st.Want, st.Delivered = "", true
		_ = m.saveState(st)
	}
	if st.GivenUp {
		m.mu.Unlock()
		return time.Time{}, time.Time{}, false
	}
	now := m.now()
	due, askARI := dueAt(st, served, now)
	var recheck time.Time
	if st.NextAttempt.IsZero() && st.Want == "" && served.Leaf != nil && served.Source == SourceACME && !st.ARIChecked.IsZero() {
		recheck = st.ARIChecked.Add(ariEvery)
	}
	m.mu.Unlock()
	if askARI && due.After(now) {
		m.askARI(ctx, set, served.Leaf)
		return m.decide(ctx)
	}
	return due, recheck, true
}

func (m *Manager) served() Served {
	if m.Served == nil {
		return Served{}
	}
	return m.Served()
}

// dueAt is when the next order is due, and whether the server is to be asked first when to renew.
func dueAt(st *State, served Served, now time.Time) (time.Time, bool) {
	if !st.NextAttempt.IsZero() {
		return st.NextAttempt, false
	}
	if st.Want != "" || served.Leaf == nil || served.Source != SourceACME {
		return now, false
	}
	leaf := served.Leaf
	due := thirdLeft(leaf)
	if st.RenewalFor == fingerprint(leaf) && !st.NextRenewal.IsZero() {
		due = st.NextRenewal
	}
	return due, st.ARIChecked.IsZero() || now.Sub(st.ARIChecked) >= ariEvery || st.RenewalFor != fingerprint(leaf)
}

// askARI asks the server when to renew and records the answer, or that it was asked.
func (m *Manager) askARI(ctx context.Context, set *Setting, leaf *x509.Certificate) {
	var at time.Time
	var err error
	if m.ARI != nil {
		at, err = m.ARI(ctx, set, leaf)
	} else {
		at, err = m.legoARI(ctx, set, leaf)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, st, loadErr := m.load()
	if loadErr != nil || cur == nil || cur.Generation != set.Generation {
		return
	}
	st.ARIChecked = m.now()
	st.RenewalFor = fingerprint(leaf)
	if err == nil {
		st.NextRenewal = at
	} else {
		st.NextRenewal = time.Time{}
	}
	if err = m.saveState(st); err != nil {
		m.log().Error("keeping the ACME progress", "error", err)
	}
}

// order runs one order for the saved setting and records how it ended.
func (m *Manager) order(ctx context.Context) {
	m.mu.Lock()
	set, st, err := m.load()
	if err != nil || set == nil || m.ordering {
		m.mu.Unlock()
		return
	}
	served := m.served()
	id := st.Want
	if id == "" {
		id = newID()
		st.Want = id
		if err = m.saveState(st); err != nil {
			m.log().Error("keeping the ACME progress", "error", err)
		}
	}
	orderCtx, cancel := context.WithTimeout(ctx, orderTimeout)
	m.cancel, m.ordering = cancel, true
	m.mu.Unlock()
	defer cancel()

	err = m.obtainAndDeliver(orderCtx, set, id, served.Leaf)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancel, m.ordering = nil, false
	cur, st, loadErr := m.load()
	if loadErr != nil || cur == nil || cur.Generation != set.Generation {
		return
	}
	if ctx.Err() != nil || (errors.Is(err, context.Canceled) && orderCtx.Err() != nil) {
		return
	}
	if err != nil {
		m.fail(set, st, err)
		return
	}
	now := m.now()
	*st = State{Delivered: true, LastSuccess: now}
	if err = m.saveState(st); err != nil {
		m.log().Error("keeping the ACME progress", "error", err)
	}
	m.log().Info("the interface's certificate was issued by ACME", "directory", set.Directory)
}

func (m *Manager) obtainAndDeliver(ctx context.Context, set *Setting, id string, replaces *x509.Certificate) error {
	name := m.PublicName()
	if name == "" {
		return errors.New("the appliance has no public DNS name")
	}
	obtain := m.Obtain
	if obtain == nil {
		obtain = m.legoObtain
	}
	cert, key, err := obtain(ctx, set, name, replaces)
	if err != nil {
		return err
	}
	return m.Deliver(ctx, &Issued{Generation: set.Generation, OrderID: id, CertPEM: cert, KeyPEM: key})
}

// fail records a failed order, with the time it ends, and gives up after a day of failures.
func (m *Manager) fail(set *Setting, st *State, err error) {
	now := m.now()
	st.Error, st.ErrorAt = set.scrub(err.Error()), now
	if st.FailingSince.IsZero() {
		st.FailingSince = now
	}
	st.Attempts++
	if now.Sub(st.FailingSince) >= giveUpAfter {
		st.GivenUp, st.NextAttempt = true, time.Time{}
		m.log().Error("giving up on the interface's certificate from ACME after a day of failures. Save the setting again or order again once the cause is fixed",
			"directory", set.Directory, "error", st.Error)
	} else {
		st.NextAttempt = now.Add(backoff(st.Attempts))
		m.log().Warn("an ACME order failed", "directory", set.Directory, "error", st.Error, "retry", st.NextAttempt.UTC().Format(time.RFC3339))
	}
	if err = m.saveState(st); err != nil {
		m.log().Error("keeping the ACME progress", "error", err)
	}
}

func backoff(attempts int) time.Duration {
	d := retryFirst
	for range attempts - 1 {
		if d >= retryCap {
			break
		}
		d *= 2
	}
	return min(d, retryCap)
}

// View is the setting and its progress, as administrators see it. It names which secrets are set and
// never holds one.
type View struct {
	Directory              string       `json:"directory"`
	Email                  string       `json:"email,omitempty"`
	Proof                  string       `json:"proof"`
	Profile                string       `json:"profile,omitempty"`
	ExternalAccountBinding *BindingView `json:"externalAccountBinding,omitempty"`
	DirectoryCASet         bool         `json:"directoryCASet"`
	DNSProvider            string       `json:"dnsProvider,omitempty"`
	// DNSCredentialsSet names the variables that have a value.
	DNSCredentialsSet []string `json:"dnsCredentialsSet"`
	Resolvers         []string `json:"resolvers"`
	// State is ordering, issued or failing.
	State string `json:"state"`
	// GivenUp tells that the interface stopped trying after a day of failures.
	GivenUp     bool       `json:"givenUp"`
	Error       string     `json:"error,omitempty"`
	ErrorAt     *time.Time `json:"errorAt,omitempty"`
	NextAttempt *time.Time `json:"nextAttempt,omitempty"`
	LastSuccess *time.Time `json:"lastSuccess,omitempty"`
	NextRenewal *time.Time `json:"nextRenewal,omitempty"`
	PublicName  string     `json:"publicName,omitempty"`
}

// BindingView says which external account binding is set.
type BindingView struct {
	KeyID      string `json:"keyID"`
	HMACKeySet bool   `json:"hmacKeySet"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func (m *Manager) viewLocked(set *Setting, st *State) *View {
	v := &View{
		Directory: set.Directory, Email: set.Email, Proof: set.Proof, Profile: set.Profile, DirectoryCASet: set.DirectoryCA != "",
		DNSProvider: set.DNSProvider, DNSCredentialsSet: []string{}, Resolvers: slices.Clone(set.Resolvers), GivenUp: st.GivenUp,
		Error: st.Error, ErrorAt: timePtr(st.ErrorAt), NextAttempt: timePtr(st.NextAttempt), LastSuccess: timePtr(st.LastSuccess),
		PublicName: m.PublicName(),
	}
	if v.Resolvers == nil {
		v.Resolvers = []string{}
	}
	if set.KeyID != "" {
		v.ExternalAccountBinding = &BindingView{KeyID: set.KeyID, HMACKeySet: set.HMACKey != ""}
	}
	for name, value := range set.DNSCredentials {
		if value != "" {
			v.DNSCredentialsSet = append(v.DNSCredentialsSet, name)
		}
	}
	slices.Sort(v.DNSCredentialsSet)
	served := m.served()
	if served.Leaf != nil && served.Source == SourceACME && st.RenewalFor == fingerprint(served.Leaf) {
		v.NextRenewal = timePtr(st.NextRenewal)
	}
	if v.NextRenewal == nil && served.Leaf != nil && served.Source == SourceACME && st.Want == "" {
		v.NextRenewal = timePtr(thirdLeft(served.Leaf))
	}
	switch {
	case m.ordering || (st.Want != "" && st.Error == "" && !st.GivenUp):
		v.State = "ordering"
	case st.Error != "":
		v.State = "failing"
	default:
		v.State = "issued"
	}
	return v
}

func (m *Manager) log() *slog.Logger {
	if m.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.Log
}

func (m *Manager) now() time.Time { return m.Now() }

func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// load reads the setting and the progress. A nil setting means ACME is not set.
func (m *Manager) load() (*Setting, *State, error) {
	raw, err := m.store.read(settingFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var set Setting
	if err = json.Unmarshal(raw, &set); err != nil {
		return nil, nil, fmt.Errorf("reading the ACME setting: %w", err)
	}
	st := &State{}
	switch raw, err = m.store.read(stateFile); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, nil, err
	default:
		if err = json.Unmarshal(raw, st); err != nil {
			return nil, nil, fmt.Errorf("reading the ACME progress: %w", err)
		}
	}
	return &set, st, nil
}

func (m *Manager) saveState(st *State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encoding the ACME progress: %w", err)
	}
	return m.store.write(stateFile, raw)
}

// stopOrderLocked ends the order under way, if any. The order's result is then not delivered.
func (m *Manager) stopOrderLocked() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *Manager) newProvider() ProviderFunc {
	if m.NewProvider != nil {
		return m.NewProvider
	}
	return legoProvider
}

func (m *Manager) checkProvider(set *Setting) *RefusalError {
	_, err := m.newProvider()(set.DNSProvider, set.DNSCredentials)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnknownProvider):
		return refuse(CodeDNSProvider, "%s is not a DNS provider the ACME client knows. Choose one from the list of providers. The list names no variables, so each provider's documentation says which credentials it reads", set.DNSProvider)
	default:
		return refuse(CodeDNSCredential, "The DNS provider %s refuses its credentials: %s. Give each variable it reads", set.DNSProvider, set.scrub(err.Error()))
	}
}

// idBytes is the length of an order's or a setting's random id.
const idBytes = 12

func newID() string {
	b := make([]byte, idBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// thirdLeft is when a third of the certificate's lifetime is left, the renewal time when the server
// gives none.
func thirdLeft(leaf *x509.Certificate) time.Time {
	return leaf.NotAfter.Add(-leaf.NotAfter.Sub(leaf.NotBefore) / 3) //nolint:mnd // a third of the lifetime
}
