package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/appliance"
)

const (
	// interfaceTimeout bounds each request to the interface.
	interfaceTimeout = 2 * time.Minute
	// replacedLoginTimeout bounds the ending of the login a new one replaces.
	replacedLoginTimeout = 10 * time.Second
	// maxInterfaceAnswer bounds what is read of one answer. An export the interface takes back is
	// as large.
	maxInterfaceAnswer = maxExportFile
	// loginFileEnv names the file the login is kept in, where the user's config folder is not the place.
	loginFileEnv = "KUBEHENGE_LOGIN_FILE"
	loginDirMode = 0o700
	heldTries    = 100
	heldPause    = 10 * time.Millisecond
	csrfHeader   = "X-Kubehenge-Csrf"
	clientHeader = "X-Kubehenge-Client"
	notLoggedIn  = "not signed in: run kubehenge login <url>"
)

// login is what `kubehenge login` keeps: where the interface is and how to reach it as one user. A
// password sign-in keeps the session it made, a token is kept as it is. CAFile, where CA was read
// from, is read again on every run, so that a certificate the appliance renews in place is
// trusted. CA stands in while the file is gone.
type login struct {
	URL    string `json:"url"`
	CA     string `json:"ca,omitempty"`
	CAFile string `json:"caFile,omitempty"`
	Token  string `json:"token,omitempty"`
	Cookie string `json:"cookie,omitempty"`
	Value  string `json:"value,omitempty"`
	CSRF   string `json:"csrf,omitempty"`
}

// loginFileKey carries a login file's path in a context, for a caller that runs commands side by
// side and so cannot share the environment.
type loginFileKey struct{}

func loginPath(ctx context.Context) (string, error) {
	if p, ok := ctx.Value(loginFileKey{}).(string); ok {
		return p, nil
	}
	if p := os.Getenv(loginFileEnv); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the config folder: %w (set %s)", err, loginFileEnv)
	}
	return filepath.Join(dir, "kubehenge", "login.json"), nil
}

func readLogin(ctx context.Context) (*login, error) {
	path, err := loginPath(ctx)
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = whileHeld(func() error {
		var readErr error
		if raw, readErr = os.ReadFile(path); readErr != nil { //nolint:gosec // the user's own login file
			return fmt.Errorf("reading the login: %w", readErr)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New(notLoggedIn)
	}
	if err != nil {
		return nil, err
	}
	var l login
	if err := json.Unmarshal(raw, &l); err != nil || l.URL == "" {
		return nil, errors.New("the login file is damaged: run kubehenge login <url> again")
	}
	return &l, nil
}

func (l *login) save(ctx context.Context) error {
	path, err := loginPath(ctx)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), loginDirMode); err != nil {
		return fmt.Errorf("keeping the login: %w", err)
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("keeping the login: %w", err)
	}
	// A command reading the login while another saves it sees the old file or the new one, never
	// half of one. CreateTemp makes the file readable by its owner alone.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".login-*.json")
	if err != nil {
		return fmt.Errorf("keeping the login: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(raw)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = whileHeld(func() error { return os.Rename(tmp.Name(), path) })
	}
	if err != nil {
		return fmt.Errorf("keeping the login: %w", err)
	}
	return nil
}

// whileHeld runs do again while it fails other than for a missing file, for a second at most.
// Windows refuses to open or replace a file while another command replaces or reads it, and lets
// go of it a moment later. Elsewhere such a failure lasts.
func whileHeld(do func() error) error {
	err := do()
	for try := 0; err != nil && !errors.Is(err, fs.ErrNotExist) && try < heldTries; try++ {
		time.Sleep(heldPause)
		err = do()
	}
	return err
}

// interfaceClient talks to Kubehenge's interface as the user of the login. Every command that
// reaches the interface uses it, so the interface's checks hold for all of them alike.
type interfaceClient struct {
	login *login
	http  *http.Client
	// signingIn is set while `kubehenge login` makes the login, where a refused certificate is fixed
	// by the --ca it is given and not by repeating the command.
	signingIn bool
}

// openInterface is a client for the login on file.
func openInterface(ctx context.Context) (*interfaceClient, error) {
	l, err := readLogin(ctx)
	if err != nil {
		return nil, err
	}
	return newInterfaceClient(ctx, l)
}

// interfaceTrust is what the CLI trusts the interface's certificate by, beside a login's CA.
type interfaceTrust struct {
	// systemRoots vouch for a certificate with or without the login's CA, so that a change to one
	// from a public or company CA needs no new login.
	systemRoots func() (*x509.CertPool, error)
	// installerCert is the appliance's own certificate. A login made with it as its CA, as one in
	// the appliance is by default, trusts the certificate the interface serves instead: storedCert
	// while that reads cleanly.
	installerCert, storedCert string
}

// trustKey carries an interfaceTrust in a context, for a test that cannot use the system's.
type trustKey struct{}

func trustIn(ctx context.Context) interfaceTrust {
	if t, ok := ctx.Value(trustKey{}).(interfaceTrust); ok {
		return t
	}
	return interfaceTrust{systemRoots: x509.SystemCertPool, installerCert: appliance.InstallerCertificatePath, storedCert: appliance.InterfaceCertificatePath}
}

// caOf reads a login's CA file again, so that a certificate renewed or changed in place is trusted.
func (t interfaceTrust) caOf(file string) (string, error) {
	if file != t.installerCert {
		raw, err := os.ReadFile(file) //nolint:gosec // the file the login was made with
		if err != nil {
			return "", fmt.Errorf("reading the login's CA: %w", err)
		}
		return string(raw), nil
	}
	leaf, _, err := appliance.ServedCertificate(t.installerCert, t.storedCert, time.Now())
	if err != nil {
		return "", err //nolint:wrapcheck // the error names the file
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})), nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func newInterfaceClient(ctx context.Context, l *login) (*interfaceClient, error) {
	defaults, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport has been replaced by one that cannot be cloned")
	}
	transport := defaults.Clone()
	trust := trustIn(ctx)
	if l.CAFile != "" {
		if ca, err := trust.caOf(l.CAFile); err == nil {
			l.CA = ca
		}
	}
	// A system whose roots do not read gives nil, which leaves Go to find them itself.
	roots, _ := trust.systemRoots()
	if l.CA != "" {
		if roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM([]byte(l.CA)) {
			name := l.CAFile
			if name == "" {
				name = "the login's CA"
			}
			return nil, fmt.Errorf("%s holds no PEM certificate", name)
		}
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, ServerName: servedName(l.CA)}
	return &interfaceClient{login: l, http: &http.Client{Transport: transport, Timeout: interfaceTimeout}}, nil
}

// servedName is the name the first certificate of pemData is valid for, so that the interface's own
// certificate, given as the CA, is trusted by any address it is reached at. A CA names no host and
// gives "", then the address in the URL is checked.
func servedName(pemData string) string {
	cert := firstCertificate(pemData)
	if cert == nil {
		return ""
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0]
	}
	if len(cert.IPAddresses) > 0 {
		return cert.IPAddresses[0].String()
	}
	return ""
}

func firstCertificate(pemData string) *x509.Certificate {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return cert
}

// openAppliance is openInterface for the appliance commands, which keep reaching the appliance
// once its own certificate has expired (pinExpired).
func openAppliance(ctx context.Context) (*interfaceClient, error) {
	c, err := openInterface(ctx)
	if err != nil {
		return nil, err
	}
	c.pinExpired()
	return c, nil
}

// errOtherCertificate refuses an interface that serves a certificate other than the expired one
// the login trusts.
var errOtherCertificate = errors.New("the interface serves a certificate other than the login's expired one")

// pinExpired trusts the login's CA after it has expired, as the appliance's own certificate given
// to login --ca does one day, by verifying it as of its first valid moment. Verifying it against the
// clock would lock the appliance's shell out of login and the appliance commands, recovery
// included, and the interface warns about the expiry itself. The cluster commands keep checking the
// clock. Only the very certificate is trusted then. An expired CA vouches for nothing it signed,
// which a clock turned back would otherwise take.
func (c *interfaceClient) pinExpired() {
	leaf := firstCertificate(c.login.CA)
	transport, ok := c.http.Transport.(*http.Transport)
	if leaf == nil || !ok || transport.TLSClientConfig == nil || time.Now().Before(leaf.NotAfter) {
		return
	}
	pinned := x509.NewCertPool()
	pinned.AddCert(leaf)
	transport.TLSClientConfig.RootCAs = pinned
	transport.TLSClientConfig.Time = func() time.Time { return leaf.NotBefore }
	transport.TLSClientConfig.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, leaf.Raw) {
			return errOtherCertificate
		}
		return nil
	}
}

// caPath is the --ca a new login takes: the file the login was made with, where it names one.
func caPath(file string) string {
	if file == "" {
		return "<path>"
	}
	return file
}

// versionHeader carries the interface's version, which it tells only a request whose session or
// token it took.
const versionHeader = "X-Kubehenge-Version"

// errLoginEnded says the interface no longer takes the login's session.
var errLoginEnded = errors.New("the login has ended")

// open sends a request and returns the answer, its body unread, once it says the request went
// through. The caller closes the body. A command still running when kubehenge login replaces its
// login is told that the login has ended, and carries on with the one on file.
func (c *interfaceClient) open(ctx context.Context, method, path string, body any) (*http.Response, error) {
	res, err := c.send(ctx, method, path, body)
	if errors.Is(err, errLoginEnded) && c.adoptLoginOnFile(ctx) {
		return c.send(ctx, method, path, body)
	}
	return res, err
}

// adoptLoginOnFile takes over the login on file where that is another one for the same interface,
// and tells whether it did. A login for another interface is not taken: the command would go on
// there. The interface stays trusted as the command found it.
func (c *interfaceClient) adoptLoginOnFile(ctx context.Context) bool {
	fresh, err := readLogin(ctx)
	if err != nil || fresh.URL != c.login.URL || fresh.Value == c.login.Value && fresh.Token == c.login.Token {
		return false
	}
	c.login = fresh
	return true
}

// formBody is a request body that is already encoded, as an upload of files is, in place of the
// JSON a body is otherwise sent as.
type formBody struct {
	contentType string
	data        []byte
}

func (c *interfaceClient) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var payload io.Reader = http.NoBody
	contentType := ""
	switch b := body.(type) {
	case nil:
	case formBody:
		payload, contentType = bytes.NewReader(b.data), b.contentType
	default:
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding the request: %w", err)
		}
		payload, contentType = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.login.URL, "/")+path, payload)
	if err != nil {
		return nil, fmt.Errorf("the interface's address: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// A password sign-in that says it is the CLI's lasts longer than a browser's and outlives a
	// restart of the interface.
	req.Header.Set(clientHeader, "cli")
	switch {
	case c.login.Token != "":
		req.Header.Set("Authorization", "PVEAPIToken="+c.login.Token)
	case c.login.Cookie != "":
		// A request cookie carries only a name and a value. Secure, HttpOnly and SameSite are
		// attributes of a Set-Cookie, which the server sets.
		req.AddCookie(&http.Cookie{Name: c.login.Cookie, Value: c.login.Value}) //nolint:gosec // a request cookie has no attributes
		req.Header.Set(csrfHeader, c.login.CSRF)
	}
	res, err := c.http.Do(req)
	if verr, refused := errors.AsType[*tls.CertificateVerificationError](err); refused || errors.Is(err, errOtherCertificate) {
		return nil, c.untrusted(err, verr)
	}
	if err != nil {
		return nil, fmt.Errorf("reaching the interface: %w", err)
	}
	if res.StatusCode < http.StatusBadRequest {
		return res, nil
	}
	defer func() { _ = res.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(res.Body, maxInterfaceAnswer))
	if err != nil {
		return nil, fmt.Errorf("reading the interface's answer: %w", err)
	}
	var e struct {
		Error string `json:"error"`
	}
	// A 401 without the version refused the login itself. One with it refused something else, such
	// as nodes whose clocks disagree, and its reason says what. So does a sign-in step's refusal.
	signInStep := method != http.MethodDelete && strings.HasPrefix(path, "/api/session")
	if res.StatusCode == http.StatusUnauthorized && res.Header.Get(versionHeader) == "" && !signInStep {
		return nil, fmt.Errorf("%w: run kubehenge login %s again", errLoginEnded, c.login.URL)
	}
	if json.Unmarshal(answer, &e) == nil && e.Error != "" {
		return nil, errors.New(e.Error)
	}
	return nil, fmt.Errorf("the interface answered %s", res.Status)
}

// untrusted says how to reach an interface whose certificate the login does not trust. verr, when
// the handshake gives one, names the certificate presented.
func (c *interfaceClient) untrusted(err error, verr *tls.CertificateVerificationError) error {
	if c.signingIn {
		return fmt.Errorf("reaching the interface: %w: give --ca the CA of the interface's certificate", err)
	}
	if verr != nil && len(verr.UnverifiedCertificates) > 0 {
		return fmt.Errorf("reaching the interface: %w: it presents the certificate %s, which this login does not trust. Run kubehenge login %s --ca %s",
			err, appliance.Fingerprint(verr.UnverifiedCertificates[0]), c.login.URL, caPath(c.login.CAFile))
	}
	return fmt.Errorf("reaching the interface: %w: run kubehenge login %s --ca %s", err, c.login.URL, caPath(c.login.CAFile))
}

// call sends a request with body as JSON and returns the answer, decoded into out when given.
func (c *interfaceClient) call(ctx context.Context, method, path string, body, out any) ([]byte, error) {
	res, err := c.open(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxInterfaceAnswer+1))
	if err != nil {
		return nil, fmt.Errorf("reading the interface's answer: %w", err)
	}
	if len(raw) > maxInterfaceAnswer {
		return nil, errors.New("the interface's answer is over 64 MiB, the most a command reads")
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("the interface's answer: %w", err)
		}
	}
	return raw, nil
}

func newLoginCommand() *cobra.Command {
	var caFile, user, passwordFile, tokenFile, totp string
	cmd := &cobra.Command{
		Use:   "login <url>",
		Short: "Sign in to Kubehenge's interface for the appliance and cluster commands",
		Long: `Sign in to Kubehenge's interface once. The appliance and cluster commands then use the login
until it ends or kubehenge logout removes it.

Give --user to sign in as a Proxmox VE user, with the password from --password-file or
KUBEHENGE_PASSWORD, and --totp for an account with a second factor. That login lasts seven days
by default and ends sooner if Proxmox VE stops accepting the user. On the appliance it survives
interface restarts and breaks of up to an hour and a half. A new login ends the one it replaces.

Give --token-file or set KUBEHENGE_PROXMOX_TOKEN to sign in with a Proxmox VE API token
(user@realm!name=secret) instead. That login lasts as long as the token.

The login is kept in kubehenge/login.json in your config folder (~/.config on Linux), readable by
you alone. It holds the session or the token. Set ` + loginFileEnv + ` to keep it elsewhere.`,
		Example: `  kubehenge login https://kubehenge.example.org --user root@pam --password-file ~/password
  kubehenge login https://kubehenge.example.org --token-file ~/token`,
		Args: argsSay(cobra.ExactArgs(1), "give the interface's address"),
		RunE: func(cmd *cobra.Command, args []string) error {
			l := &login{URL: interfaceURL(args[0])}
			// In the appliance the login trusts the certificate the interface serves, whichever it is.
			if caFile == "" {
				if installer := trustIn(cmd.Context()).installerCert; isFile(installer) {
					caFile = installer
				}
			}
			if caFile != "" {
				pem, err := os.ReadFile(caFile) //nolint:gosec // the file the user named
				if err != nil {
					return fmt.Errorf("--ca: %w", err)
				}
				if l.CAFile, err = filepath.Abs(caFile); err != nil {
					return fmt.Errorf("--ca: %w", err)
				}
				l.CA = string(pem)
			}
			if err := signInTo(cmd.Context(), l, user, passwordFile, tokenFile, totp); err != nil {
				return err
			}
			// One of two sessions is left without a file to use it, and would stay signed in for the
			// rest of its seven days: the one this login replaces or, when the new one cannot be
			// kept, the new one. An interface that is gone must not hold the command up for long.
			unused, _ := readLogin(cmd.Context())
			saveErr := l.save(cmd.Context())
			if saveErr != nil {
				unused = l
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), replacedLoginTimeout)
			_ = endSession(ctx, unused)
			cancel()
			if saveErr != nil {
				return saveErr
			}
			return say(cmd, "Signed in to "+l.URL)
		},
	}
	f := cmd.Flags()
	f.StringVar(&caFile, "ca", "", "PEM file with the CA of the interface's certificate, trusted beside the system trust store. "+
		"In the appliance, the interface's own certificate is trusted without it")
	f.StringVar(&user, "user", "", "Proxmox VE user to sign in as, such as root@pam")
	f.StringVar(&passwordFile, "password-file", "", "file holding the user's password, else KUBEHENGE_PASSWORD holds it")
	f.StringVar(&tokenFile, "token-file", "", "file holding a Proxmox VE API token, else KUBEHENGE_PROXMOX_TOKEN holds it")
	f.StringVar(&totp, "totp", "", "a TOTP code, for an account with a second factor")
	return cmd
}

// interfaceURL is the address a user typed, as one with a scheme. An address without one is taken
// as https, which the interface serves, so that a bare host name works.
func interfaceURL(typed string) string {
	typed = strings.TrimSuffix(typed, "/")
	if !strings.Contains(typed, "://") {
		return "https://" + typed
	}
	return typed
}

// signInTo fills in how l reaches the interface, by user and password or by token, and checks it.
func signInTo(ctx context.Context, l *login, user, passwordFile, tokenFile, totp string) error {
	if user != "" && tokenFile != "" {
		return errors.New("give --user or --token-file, not both")
	}
	// A flag beats the environment, so --user signs in by password with the token variable set.
	if user == "" && tokenFile == "" && os.Getenv("KUBEHENGE_PROXMOX_TOKEN") == "" {
		return errors.New("give --user, or a token in --token-file or KUBEHENGE_PROXMOX_TOKEN")
	}
	c, err := newInterfaceClient(ctx, l)
	if err != nil {
		return err
	}
	c.signingIn = true
	c.pinExpired()
	if user == "" {
		if l.Token, err = secretFrom(tokenFile, "KUBEHENGE_PROXMOX_TOKEN"); err != nil {
			return err
		}
		_, err = c.call(ctx, http.MethodGet, "/api/session", nil, nil)
		return err
	}
	password, err := secretFrom(passwordFile, "KUBEHENGE_PASSWORD")
	if err != nil {
		return err
	}
	if password == "" {
		return errors.New("no password: give --password-file or KUBEHENGE_PASSWORD")
	}
	pending, err := c.signInStep(ctx, "/api/session", map[string]string{"username": user, "password": password})
	if err != nil || !pending {
		return err
	}
	if totp == "" {
		return fmt.Errorf("%s has a second factor: give --totp", user)
	}
	_, err = c.signInStep(ctx, "/api/session/second-factor", map[string]string{"kind": "totp", "code": totp})
	return err
}

// signInStep sends a sign-in step and keeps in the login the session its answer starts, and tells
// whether that waits for a second factor. Each step ends the session it came with and starts
// another, so the last cookie the answer sets is the live one, as a browser would keep it.
func (c *interfaceClient) signInStep(ctx context.Context, path string, body map[string]string) (bool, error) {
	res, err := c.open(ctx, http.MethodPost, path, body)
	if err != nil {
		return false, err
	}
	defer func() { _ = res.Body.Close() }()
	l := c.login
	var sess struct {
		CSRF         string `json:"csrf"`
		SecondFactor any    `json:"secondFactor"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxInterfaceAnswer)).Decode(&sess); err != nil {
		return false, fmt.Errorf("the interface's answer: %w", err)
	}
	for _, cookie := range res.Cookies() {
		l.Cookie, l.Value = cookie.Name, cookie.Value
	}
	l.CSRF = sess.CSRF
	if l.Value == "" {
		return false, errors.New("the interface set no session")
	}
	return sess.SecondFactor != nil, nil
}

// endSession ends the session l holds, if it is a login with one. It reaches the interface as login
// does, through the appliance's own certificate once that has expired. A session the interface has
// ended already is no failure. The request is sent as it is, since taking over the login on file
// would end that one.
func endSession(ctx context.Context, l *login) error {
	if l == nil || l.Cookie == "" {
		return nil
	}
	c, err := newInterfaceClient(ctx, l)
	if err != nil {
		return err
	}
	c.pinExpired()
	res, err := c.send(ctx, http.MethodDelete, "/api/session", nil)
	if errors.Is(err, errLoginEnded) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	return nil
}

func newLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "End the login and forget it",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, _ := readLogin(cmd.Context())
			endErr := endSession(cmd.Context(), l)
			path, err := loginPath(cmd.Context())
			if err != nil {
				return err
			}
			switch err := os.Remove(path); {
			case errors.Is(err, fs.ErrNotExist):
				return say(cmd, "Not signed in")
			case err != nil:
				return fmt.Errorf("removing the login: %w", err)
			case endErr != nil:
				return fmt.Errorf("the login is forgotten, but its session at %s was not ended: %w", l.URL, endErr)
			}
			return say(cmd, "Signed out")
		},
	}
}
