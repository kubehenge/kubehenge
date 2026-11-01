// Package notify sends what needs a look to a webhook, an ntfy topic or by email.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

// The severities of a notice.
const (
	Error    = "error"
	Warning  = "warning"
	Resolved = "resolved"
	Info     = "info"
)

const (
	// timeout bounds one sending, so that a target that never answers holds up no other.
	timeout = 30 * time.Second
	// answerShown is how much of a refusing server's answer an error carries.
	answerShown = 200
	// implicitTLSPort is SMTP's port that speaks TLS from the start (RFC 8314).
	implicitTLSPort = "465"
)

// Notice is one thing to tell.
type Notice struct {
	// Key names what the notice is about, the same for its problem and its resolution.
	Key      string    `json:"key"`
	Severity string    `json:"severity"`
	Title    string    `json:"title"`
	Text     string    `json:"text,omitempty"`
	Cluster  string    `json:"cluster,omitempty"`
	Link     string    `json:"link,omitempty"`
	At       time.Time `json:"at"`
}

// Target is somewhere notices go.
type Target interface {
	// Kind names the target as its credential's purpose does.
	Kind() string
	Send(ctx context.Context, n *Notice) error
}

// Webhook posts each notice as JSON.
type Webhook struct {
	URL    string
	Client *http.Client
}

// Kind is the webhook's purpose.
func (*Webhook) Kind() string { return PurposeWebhook }

// Send posts the notice.
func (w *Webhook) Send(ctx context.Context, n *Notice) error {
	body, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("encoding the notice: %w", err)
	}
	return post(ctx, w.Client, w.URL, body, http.Header{"Content-Type": {"application/json"}})
}

// Ntfy publishes each notice to an ntfy topic.
type Ntfy struct {
	// URL is the topic's, as https://ntfy.sh/topic.
	URL string
	// Token is an access token, for a topic that needs one.
	Token  string
	Client *http.Client
}

// Kind is ntfy's purpose.
func (*Ntfy) Kind() string { return PurposeNtfy }

// ntfyLook are the priority and tags ntfy shows a notice of each severity with.
var ntfyLook = map[string][2]string{
	Error:    {"high", "rotating_light"},
	Warning:  {"default", "warning"},
	Resolved: {"low", "white_check_mark"},
	Info:     {"low", "information_source"},
}

// Send publishes the notice.
func (t *Ntfy) Send(ctx context.Context, n *Notice) error {
	look := ntfyLook[n.Severity]
	h := http.Header{
		// ntfy reads a header's UTF-8 only as RFC 2047 encoded words.
		"Title":    {mime.QEncoding.Encode("utf-8", n.Title)},
		"Priority": {look[0]},
		"Tags":     {look[1]},
	}
	if n.Link != "" {
		h.Set("Click", n.Link)
	}
	if t.Token != "" {
		h.Set("Authorization", "Bearer "+t.Token)
	}
	text := n.Text
	if text == "" {
		text = n.Title
	}
	return post(ctx, t.Client, t.URL, []byte(text), h)
}

func post(ctx context.Context, c *http.Client, target string, body []byte, h http.Header) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("the address: %w", err)
	}
	req.Header = h
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		// The URL can carry a secret, as a webhook's path often does.
		if u, ok := errors.AsType[*url.Error](err); ok {
			return fmt.Errorf("posting: %w", u.Err)
		}
		return fmt.Errorf("posting: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		said, _ := io.ReadAll(io.LimitReader(resp.Body, answerShown))
		return fmt.Errorf("answered %s: %s", resp.Status, strings.TrimSpace(string(said)))
	}
	return nil
}

// Email sends each notice as a mail through an SMTP server.
type Email struct {
	// Server is host:port. Port 465 speaks TLS from the start, any other STARTTLS where the server
	// offers it.
	Server string
	From   string
	To     []string
	// Username and Password sign in, only over TLS.
	Username, Password string
	// TLS is for tests, which trust a server of their own.
	TLS *tls.Config
}

// Kind is email's purpose.
func (*Email) Kind() string { return PurposeEmail }

// Send mails the notice.
func (e *Email) Send(ctx context.Context, n *Notice) error {
	host, port, err := net.SplitHostPort(e.Server)
	if err != nil {
		return fmt.Errorf("the server %q is not host:port", e.Server)
	}
	msg, err := e.message(n)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := e.dial(ctx, host, port)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if derr := conn.SetDeadline(deadline); derr != nil {
		return fmt.Errorf("the connection: %w", derr)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := e.open(c, conn, host); err != nil {
		return err
	}
	if err := e.deliver(c, msg); err != nil {
		return err
	}
	if err := c.Quit(); err != nil {
		return fmt.Errorf("closing: %w", err)
	}
	return nil
}

// open greets the server, turns TLS on where it can and signs in.
func (e *Email) open(c *smtp.Client, conn net.Conn, host string) error {
	if err := c.Hello("kubehenge"); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	if _, isTLS := conn.(*tls.Conn); !isTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(e.tlsConfig(host)); err != nil {
				return fmt.Errorf("STARTTLS: %w", err)
			}
		}
	}
	if e.Username != "" {
		// PlainAuth refuses a connection without TLS, other than to localhost.
		if err := c.Auth(smtp.PlainAuth("", e.Username, e.Password, host)); err != nil {
			return fmt.Errorf("signing in: %w", err)
		}
	}
	return nil
}

// deliver hands the message over. The envelope takes bare addresses, the headers keep the names.
func (e *Email) deliver(c *smtp.Client, msg []byte) error {
	from, err := mail.ParseAddress(e.From)
	if err != nil {
		return fmt.Errorf("the sender %q: %w", e.From, err)
	}
	if merr := c.Mail(from.Address); merr != nil {
		return fmt.Errorf("the sender: %w", merr)
	}
	for _, to := range e.To {
		addr, perr := mail.ParseAddress(to)
		if perr != nil {
			return fmt.Errorf("the recipient %q: %w", to, perr)
		}
		if rerr := c.Rcpt(addr.Address); rerr != nil {
			return fmt.Errorf("%s: %w", to, rerr)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("the message: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("the message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the message: %w", err)
	}
	return nil
}

func (e *Email) dial(ctx context.Context, host, port string) (net.Conn, error) {
	var d net.Dialer
	addr := net.JoinHostPort(host, port)
	var conn net.Conn
	var err error
	if port == implicitTLSPort {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: e.tlsConfig(host)}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting: %w", err)
	}
	return conn, nil
}

func (e *Email) tlsConfig(host string) *tls.Config {
	if e.TLS != nil {
		return e.TLS.Clone()
	}
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// message is the mail's headers and its text, quoted-printable so that any line length and
// character arrive whole.
func (e *Email) message(n *Notice) ([]byte, error) {
	from, err := mail.ParseAddress(e.From)
	if err != nil {
		return nil, fmt.Errorf("the sender %q: %w", e.From, err)
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(e.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", n.Title))
	fmt.Fprintf(&b, "Date: %s\r\n", n.At.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	q := quotedprintable.NewWriter(&b)
	text := n.Text
	if n.Link != "" {
		text = strings.TrimSpace(text + "\n\n" + n.Link)
	}
	if _, err := q.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n"))); err != nil {
		return nil, fmt.Errorf("the message: %w", err)
	}
	if err := q.Close(); err != nil {
		return nil, fmt.Errorf("the message: %w", err)
	}
	return b.Bytes(), nil
}
