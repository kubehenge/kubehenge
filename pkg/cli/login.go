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
)

const (
	// interfaceTimeout bounds each request to the interface.
	interfaceTimeout = 2 * time.Minute
	// maxInterfaceAnswer bounds what is read of one answer.
	maxInterfaceAnswer = 4 << 20
	// loginFileEnv names the file the login is kept in, where the user's config folder is not the place.
	loginFileEnv = "KUBEHENGE_LOGIN_FILE"
	loginDirMode = 0o700
	heldTries    = 100
	heldPause    = 10 * time.Millisecond
	csrfHeader   = "X-Kubehenge-Csrf"
	notLoggedIn  = "not logged in: run kubehenge login <url>"
)

// login is what `kubehenge login` keeps: where the interface is and how to reach it as one user. A
// password sign-in keeps the session it made, a token is kept as it is.
type login struct {
	URL    string `json:"url"`
	CA     string `json:"ca,omitempty"`
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
		raw, readErr = os.ReadFile(path) //nolint:gosec // the user's own login file
		return readErr
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New(notLoggedIn)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the login: %w", err)
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
}

// openInterface is a client for the login on file.
func openInterface(ctx context.Context) (*interfaceClient, error) {
	l, err := readLogin(ctx)
	if err != nil {
		return nil, err
	}
	return newInterfaceClient(l)
}

func newInterfaceClient(l *login) (*interfaceClient, error) {
	defaults, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport has been replaced by one that cannot be cloned")
	}
	transport := defaults.Clone()
	if l.CA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(l.CA)) {
			return nil, errors.New("the login's CA holds no certificate: run kubehenge login <url> again")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, ServerName: servedName(l.CA)}
	}
	return &interfaceClient{login: l, http: &http.Client{Transport: transport, Timeout: interfaceTimeout}}, nil
}

// servedName is the name the first certificate of pemData is valid for, so that the interface's own
// certificate, given as the CA, is trusted by any address it is reached at. A CA names no host and
// gives "", then the address in the URL is checked.
func servedName(pemData string) string {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
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

// open sends a request and returns the answer, its body unread, once it says the request went
// through. The caller closes the body.
func (c *interfaceClient) open(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var payload io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding the request: %w", err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.login.URL, "/")+path, payload)
	if err != nil {
		return nil, fmt.Errorf("the interface's address: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	if res.StatusCode == http.StatusUnauthorized && c.login.Cookie != "" {
		return nil, errors.New("the login has ended: run kubehenge login <url> again")
	}
	if json.Unmarshal(answer, &e) == nil && e.Error != "" {
		return nil, fmt.Errorf("%s (%d)", e.Error, res.StatusCode)
	}
	return nil, fmt.Errorf("the interface answered %s", res.Status)
}

// call sends a request with body as JSON and returns the answer, decoded into out when given.
func (c *interfaceClient) call(ctx context.Context, method, path string, body, out any) ([]byte, error) {
	res, err := c.open(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxInterfaceAnswer))
	if err != nil {
		return nil, fmt.Errorf("reading the interface's answer: %w", err)
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
		Short: "Sign in to Kubehenge's interface for every command",
		Long: `Sign in to Kubehenge's interface once. The appliance and cluster commands then use the login
until it ends or kubehenge logout removes it.

Give --user to sign in as a Proxmox VE user, with the password from --password-file or
KUBEHENGE_PASSWORD, and --totp for an account with a second factor. The login is the session that
makes, which the interface ends after its session time. Give --token-file, or set
KUBEHENGE_PROXMOX_TOKEN, to a Proxmox VE API token (user@realm!name=secret) instead, which lasts
as long as the token does. --ca names the PEM file with the CA of the interface's certificate, else
the system trust store is used.

The login is kept in the config folder's kubehenge/login.json, readable by you alone. Set
` + loginFileEnv + ` to keep it elsewhere.`,
		Example: `  kubehenge login https://kubehenge.example.org --user root@pam --password-file ~/password
  kubehenge login https://kubehenge.example.org --token-file ~/token`,
		Args: argsSay(cobra.ExactArgs(1), "give the interface's address"),
		RunE: func(cmd *cobra.Command, args []string) error {
			l := &login{URL: strings.TrimSuffix(args[0], "/")}
			if caFile != "" {
				pem, err := os.ReadFile(caFile) //nolint:gosec // the file the user named
				if err != nil {
					return fmt.Errorf("--ca: %w", err)
				}
				l.CA = string(pem)
			}
			if err := signInTo(cmd.Context(), l, user, passwordFile, tokenFile, totp); err != nil {
				return err
			}
			if err := l.save(cmd.Context()); err != nil {
				return err
			}
			return say(cmd, "Logged in to "+l.URL)
		},
	}
	f := cmd.Flags()
	f.StringVar(&caFile, "ca", "", "PEM file with the CA of the interface's certificate, else the system trust store")
	f.StringVar(&user, "user", "", "Proxmox VE user to sign in as, such as root@pam")
	f.StringVar(&passwordFile, "password-file", "", "file holding the user's password, else KUBEHENGE_PASSWORD holds it")
	f.StringVar(&tokenFile, "token-file", "", "file holding a Proxmox VE API token, else KUBEHENGE_PROXMOX_TOKEN holds it")
	f.StringVar(&totp, "totp", "", "a TOTP code, for an account with a second factor")
	return cmd
}

// signInTo fills in how l reaches the interface, by user and password or by token, and checks it.
func signInTo(ctx context.Context, l *login, user, passwordFile, tokenFile, totp string) error {
	// A flag beats the environment, so --user signs in by password with the token variable set.
	if user != "" && tokenFile != "" || user == "" && tokenFile == "" && os.Getenv("KUBEHENGE_PROXMOX_TOKEN") == "" {
		return errors.New("give --user, or a token in --token-file or KUBEHENGE_PROXMOX_TOKEN")
	}
	c, err := newInterfaceClient(l)
	if err != nil {
		return err
	}
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
	var sess struct {
		CSRF         string `json:"csrf"`
		SecondFactor any    `json:"secondFactor"`
	}
	res, err := c.open(ctx, http.MethodPost, "/api/session", map[string]string{"username": user, "password": password})
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(res.Body, maxInterfaceAnswer)).Decode(&sess); err != nil {
		return fmt.Errorf("the interface's answer: %w", err)
	}
	for _, cookie := range res.Cookies() {
		l.Cookie, l.Value = cookie.Name, cookie.Value
	}
	l.CSRF = sess.CSRF
	if l.Cookie == "" {
		return errors.New("the interface set no session")
	}
	if sess.SecondFactor == nil {
		return nil
	}
	if totp == "" {
		return fmt.Errorf("%s has a second factor: give --totp", user)
	}
	c.login = l
	var after struct {
		CSRF string `json:"csrf"`
	}
	if _, err := c.call(ctx, http.MethodPost, "/api/session/second-factor", map[string]string{"kind": "totp", "code": totp}, &after); err != nil {
		return err
	}
	if after.CSRF != "" {
		l.CSRF = after.CSRF
	}
	return nil
}

func newLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "End the login and forget it",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if c, err := openInterface(cmd.Context()); err == nil && c.login.Cookie != "" {
				_, _ = c.call(cmd.Context(), http.MethodDelete, "/api/session", nil, nil)
			}
			path, err := loginPath(cmd.Context())
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("removing the login: %w", err)
			}
			return say(cmd, "Logged out")
		},
	}
}
