package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrAuthentication is Proxmox VE refusing a sign-in. It says no more than that, so a caller cannot
// tell an unknown user from a wrong password.
var ErrAuthentication = errors.New("authentication failure")

// ErrForbidden is Proxmox VE refusing a request for want of privileges.
var ErrForbidden = errors.New("permission denied")

// TicketLifetime is how long Proxmox VE accepts a ticket after issuing it.
const TicketLifetime = 2 * time.Hour

const (
	// userRequestTimeout bounds each request, as a signed-in user waits on it.
	userRequestTimeout = 30 * time.Second
	// maxResponseBytes caps what is read of an answer. Proxmox VE's largest, the cluster's
	// resources, stay far below it.
	maxResponseBytes = 1 << 20
	// halfTicketParts are those of PVE:!tfa!<challenge>::<signature> when split at the first two
	// colons.
	halfTicketParts = 3
)

// Accounts signs Proxmox VE users in and asks the API on their behalf, with their tickets.
type Accounts struct {
	base string
	http *http.Client
}

// NewAccounts returns Accounts for the API cfg locates. cfg.Token is not used. It makes no request.
func NewAccounts(cfg Config) (*Accounts, error) {
	base, httpClient, err := cfg.transport()
	if err != nil {
		return nil, err
	}
	httpClient.Timeout = userRequestTimeout
	return &Accounts{base: base, http: httpClient}, nil
}

// Ticket is a signed-in user's session with Proxmox VE, or an API token acting as one.
type Ticket struct {
	// Username is the user ID as Proxmox VE resolved it, user@realm, or a token's user@realm!name.
	Username string
	Ticket   string
	CSRF     string
	Issued   time.Time
	// Token is an API token, user@realm!name=secret, which Proxmox VE takes in place of the ticket.
	// It needs no CSRF token and is not renewed.
	Token string
}

// authorize lets req act as the ticket's user.
func (t *Ticket) authorize(req *http.Request) {
	if t.Token != "" {
		req.Header.Set("Authorization", "PVEAPIToken="+t.Token)
		return
	}
	req.Header.Set("Cookie", "PVEAuthCookie="+t.Ticket)
	if req.Method != http.MethodGet {
		// Go sends every header name in this canonical form. Proxmox VE reads them regardless of case.
		req.Header.Set("Csrfpreventiontoken", t.CSRF)
	}
}

// TokenUser checks an API token, user@realm!name=secret, with Proxmox VE, which takes it only with
// its own secret, and returns it as a ticket for its user@realm!name.
func (a *Accounts) TokenUser(ctx context.Context, token string) (*Ticket, error) {
	id, _, ok := strings.Cut(token, "=")
	if !ok || !strings.Contains(id, "@") || !strings.Contains(id, "!") {
		return nil, ErrAuthentication
	}
	t := &Ticket{Username: id, Token: token, Issued: time.Now()}
	var version map[string]any
	if err := a.get(ctx, t, "/version", &version); err != nil {
		return nil, err
	}
	return t, nil
}

// Challenge is the second factor Proxmox VE asks of a user after the password.
type Challenge struct {
	Username string
	// TOTP, Yubico and WebAuthn are the factors the user has set up. A WebAuthn credential is bound
	// to Proxmox VE's own origin, so it can only be answered there.
	TOTP     bool
	Yubico   bool
	WebAuthn bool
	// Recovery holds the indices of the recovery keys still unused. Nil means none were set up.
	Recovery []int
	ticket   string
	issued   time.Time
}

// Expired tells whether the challenge can no longer be answered.
func (c *Challenge) Expired(now time.Time) bool {
	return now.Sub(c.issued) >= TicketLifetime
}

type ticketResponse struct {
	Username string `json:"username"`
	Ticket   string `json:"ticket"`
	CSRF     string `json:"CSRFPreventionToken"`
	NeedTFA  int    `json:"NeedTFA"`
}

// APIError is any other refusal. Proxmox VE puts its reason in the HTTP status line.
type APIError struct {
	Method, Path, Status string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("proxmox VE %s %s: %s", e.Method, e.Path, e.Status)
}

// SignIn checks a user's password. When the user has a second factor, it returns the challenge to
// answer with Answer instead of a ticket. username is user@realm.
func (a *Accounts) SignIn(ctx context.Context, username, password string) (*Ticket, *Challenge, error) {
	now := time.Now()
	res, err := a.postTicket(ctx, username, password, "")
	if err != nil {
		return nil, nil, err
	}
	if res.NeedTFA == 0 {
		return &Ticket{Username: res.Username, Ticket: res.Ticket, CSRF: res.CSRF, Issued: now}, nil, nil
	}
	c, err := parseChallenge(res.Ticket)
	if err != nil {
		return nil, nil, err
	}
	c.Username, c.ticket, c.issued = res.Username, res.Ticket, now
	return nil, c, nil
}

// Answer completes a sign-in with a second factor: kind is totp, yubico or recovery.
func (a *Accounts) Answer(ctx context.Context, c *Challenge, kind, code string) (*Ticket, error) {
	switch kind {
	case "totp", "yubico", "recovery":
	default:
		return nil, fmt.Errorf("unknown second factor %q", kind)
	}
	now := time.Now()
	res, err := a.postTicket(ctx, c.Username, kind+":"+code, c.ticket)
	if err != nil {
		return nil, err
	}
	if res.NeedTFA != 0 {
		return nil, ErrAuthentication
	}
	return &Ticket{Username: res.Username, Ticket: res.Ticket, CSRF: res.CSRF, Issued: now}, nil
}

// OpenIDAuthURL starts a sign-in through an OpenID realm: the URL to send the user to, at which
// the identity provider returns them to redirect with a code and the state.
func (a *Accounts) OpenIDAuthURL(ctx context.Context, realm, redirect string) (string, error) {
	var authURL string
	if err := a.postForm(ctx, "/access/openid/auth-url", url.Values{"realm": {realm}, "redirect-url": {redirect}}, &authURL); err != nil {
		return "", err
	}
	if !strings.HasPrefix(authURL, "https://") && !strings.HasPrefix(authURL, "http://") {
		return "", fmt.Errorf("proxmox VE returned no sign-in URL for realm %s", realm)
	}
	return authURL, nil
}

// OpenIDLogin completes an OpenID sign-in with what the identity provider returned.
func (a *Accounts) OpenIDLogin(ctx context.Context, code, state, redirect string) (*Ticket, error) {
	now := time.Now()
	var res ticketResponse
	err := a.postForm(ctx, "/access/openid/login", url.Values{"code": {code}, "state": {state}, "redirect-url": {redirect}}, &res)
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		// Proxmox VE's reason is for the log. The user learns no more than with a password.
		return nil, fmt.Errorf("%w: %s", ErrAuthentication, apiErr.Status)
	}
	if err != nil {
		return nil, err
	}
	if res.Ticket == "" || res.Username == "" {
		return nil, ErrAuthentication
	}
	return &Ticket{Username: res.Username, Ticket: res.Ticket, CSRF: res.CSRF, Issued: now}, nil
}

// Renew exchanges a ticket for a new one. Proxmox VE neither asks the realm nor a second factor
// again, so only a disabled or expired user stops it. How long a session lasts is the caller's to
// limit.
func (a *Accounts) Renew(ctx context.Context, t *Ticket) (*Ticket, error) {
	if t.Token != "" {
		return t, nil
	}
	now := time.Now()
	res, err := a.postTicket(ctx, t.Username, t.Ticket, "")
	if err != nil {
		return nil, err
	}
	if res.NeedTFA != 0 {
		return nil, ErrAuthentication
	}
	return &Ticket{Username: res.Username, Ticket: res.Ticket, CSRF: res.CSRF, Issued: now}, nil
}

// Privileges are a user's effective privileges on one path, such as VM.Audit on /pool/kubehenge.
type Privileges map[string]bool

// Has tells whether every privilege named is held.
func (p Privileges) Has(privs ...string) bool {
	for _, priv := range privs {
		if !p[priv] {
			return false
		}
	}
	return true
}

// Privileges returns the user's effective privileges on path, inherited ones included, as Proxmox VE
// itself evaluates them.
func (a *Accounts) Privileges(ctx context.Context, t *Ticket, path string) (Privileges, error) {
	var res map[string]map[string]int
	if err := a.get(ctx, t, "/access/permissions?"+url.Values{"path": {path}}.Encode(), &res); err != nil {
		return nil, err
	}
	return held(res[path]), nil
}

// AllPrivileges returns, in one request, the user's privileges on the paths Proxmox VE lists for
// them: the top-level ones, every path with an ACL entry, and the guests and storages of pools,
// each only where the user holds a privilege (PVE::RPCEnvironment::get_effective_permissions). A
// path with no ACL entry of its own, such as a pool created a moment ago, is not listed even where
// it inherits privileges from above.
func (a *Accounts) AllPrivileges(ctx context.Context, t *Ticket) (map[string]Privileges, error) {
	var res map[string]map[string]int
	if err := a.get(ctx, t, "/access/permissions", &res); err != nil {
		return nil, err
	}
	all := make(map[string]Privileges, len(res))
	for path, privs := range res {
		all[path] = held(privs)
	}
	return all, nil
}

// postTicket signs in with a password, a ticket to renew or, with challenge, a second factor's
// answer.
func (a *Accounts) postTicket(ctx context.Context, username, password, challenge string) (*ticketResponse, error) {
	form := url.Values{"username": {username}, "password": {password}}
	if challenge != "" {
		form.Set("tfa-challenge", challenge)
	}
	var res ticketResponse
	if err := a.postForm(ctx, "/access/ticket", form, &res); err != nil {
		return nil, err
	}
	if res.Ticket == "" || res.Username == "" {
		return nil, errors.New("proxmox VE returned no ticket")
	}
	return &res, nil
}

func (a *Accounts) postForm(ctx context.Context, path string, form url.Values, out any) error {
	req, err := a.newRequest(ctx, http.MethodPost, path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.do(req, out)
}

func (a *Accounts) get(ctx context.Context, t *Ticket, path string, out any) error {
	req, err := a.newRequest(ctx, http.MethodGet, path, http.NoBody)
	if err != nil {
		return err
	}
	t.authorize(req)
	return a.do(req, out)
}

func (a *Accounts) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("proxmox VE %s %s: %w", method, path, err)
	}
	return req, nil
}

func (a *Accounts) do(req *http.Request, out any) error {
	resp, err := a.http.Do(req)
	if err != nil {
		// The client's error names the method and URL already.
		return fmt.Errorf("asking proxmox VE: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("proxmox VE %s %s: reading the answer: %w", req.Method, req.URL.Path, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrAuthentication
	case resp.StatusCode == http.StatusForbidden:
		return ErrForbidden
	case resp.StatusCode != http.StatusOK:
		return &APIError{Method: req.Method, Path: req.URL.Path, Status: resp.Status}
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("proxmox VE %s %s: %w", req.Method, req.URL.Path, err)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("proxmox VE %s %s: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// parseChallenge reads the factors from a half ticket, PVE:!tfa!<URL-encoded JSON>::<signature>.
func parseChallenge(ticket string) (*Challenge, error) {
	parts := strings.SplitN(ticket, ":", halfTicketParts)
	if len(parts) < halfTicketParts || !strings.HasPrefix(parts[1], "!tfa!") {
		return nil, errors.New("proxmox VE asked for a second factor without a challenge")
	}
	raw, err := url.PathUnescape(strings.TrimPrefix(parts[1], "!tfa!"))
	if err != nil {
		return nil, fmt.Errorf("second-factor challenge: %w", err)
	}
	var data struct {
		TOTP     bool            `json:"totp"`
		Yubico   bool            `json:"yubico"`
		WebAuthn json.RawMessage `json:"webauthn"`
		Recovery *[]int          `json:"recovery"`
	}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("second-factor challenge: %w", err)
	}
	c := &Challenge{TOTP: data.TOTP, Yubico: data.Yubico, WebAuthn: len(data.WebAuthn) > 0 && string(data.WebAuthn) != "null"}
	if data.Recovery != nil {
		c.Recovery = append([]int{}, *data.Recovery...)
	}
	return c, nil
}

func held(granted map[string]int) Privileges {
	privs := Privileges{}
	for name, g := range granted {
		if g != 0 {
			privs[name] = true
		}
	}
	return privs
}
