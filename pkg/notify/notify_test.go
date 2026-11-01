package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

var notice = Notice{
	Key: "c/web/VM paused", Severity: Error, Title: "clusters/web: VM paused",
	Text: "web-cp-0 is paused. Résumé it in Proxmox VE.", Cluster: "clusters/web",
	Link: "https://kubehenge.example/clusters/clusters/web/health", At: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC),
}

func TestAWebhookPostsTheNoticeAsJSON(t *testing.T) {
	t.Parallel()
	var got Notice
	var kind string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	n := notice
	if err := (&Webhook{URL: srv.URL + "/hook/s3cret"}).Send(t.Context(), &n); err != nil {
		t.Fatal(err)
	}
	if kind != "application/json" || got != notice {
		t.Errorf("posted %v as %q, want %v", got, kind, notice)
	}
}

func TestAFailedPostSaysWhatTheServerAnsweredAndNotTheURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such hook", http.StatusNotFound)
	}))
	defer srv.Close()
	n := notice
	err := (&Webhook{URL: srv.URL + "/hook/s3cret"}).Send(t.Context(), &n)
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "no such hook") {
		t.Errorf("Send gave %v, want the 404 and its text", err)
	}
	srv.Close()
	err = (&Webhook{URL: srv.URL + "/hook/s3cret"}).Send(t.Context(), &n)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("Send to a closed server gave %v, want an error without the URL", err)
	}
}

func TestNtfyGetsTheTitlePriorityTagsLinkAndToken(t *testing.T) {
	t.Parallel()
	var h http.Header
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	n := notice
	n.Title = "clusters/wéb: VM paused"
	if err := (&Ntfy{URL: srv.URL + "/kubehenge", Token: "tk_1"}).Send(t.Context(), &n); err != nil {
		t.Fatal(err)
	}
	title, err := new(mime.WordDecoder).DecodeHeader(h.Get("Title"))
	if err != nil || title != n.Title {
		t.Errorf("Title %q decodes to %q (%v), want %q", h.Get("Title"), title, err, n.Title)
	}
	for k, want := range map[string]string{"Priority": "high", "Tags": "rotating_light", "Click": n.Link, "Authorization": "Bearer tk_1"} {
		if h.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, h.Get(k), want)
		}
	}
	if body != n.Text {
		t.Errorf("body %q, want %q", body, n.Text)
	}
}

func TestNtfyWithoutATokenSendsNoAuthorization(t *testing.T) {
	t.Parallel()
	var h http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { h = r.Header.Clone() }))
	defer srv.Close()
	n := notice
	n.Severity, n.Link = Resolved, ""
	if err := (&Ntfy{URL: srv.URL}).Send(t.Context(), &n); err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "" || h.Get("Click") != "" || h.Get("Priority") != "low" {
		t.Errorf("headers %v", h)
	}
}

// smtpServer is enough of an SMTP server for one mail: STARTTLS and AUTH PLAIN when given a
// certificate, and what it was told.
type smtpServer struct {
	addr string
	tls  *tls.Config

	mu    sync.Mutex
	auth  string
	from  string
	to    []string
	data  string
	tlsOn bool
}

func newSMTPServer(t *testing.T, cfg *tls.Config) *smtpServer {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s := &smtpServer{addr: l.Addr().String(), tls: cfg}
	go func() {
		for {
			c, aerr := l.Accept()
			if aerr != nil {
				return
			}
			go s.serve(t.Context(), c)
		}
	}()
	return s
}

func (s *smtpServer) serve(ctx context.Context, c net.Conn) {
	defer func() { _ = c.Close() }()
	tp := textproto.NewConn(c)
	_ = tp.PrintfLine("220 test")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		s.mu.Lock()
		switch strings.ToUpper(verb) {
		case "EHLO":
			ext := "250-test\r\n250 8BITMIME"
			if s.tls != nil && !s.tlsOn {
				ext = "250-test\r\n250-STARTTLS\r\n250 AUTH PLAIN"
			} else if s.tlsOn {
				ext = "250-test\r\n250 AUTH PLAIN"
			}
			_ = tp.PrintfLine("%s", ext)
		case "STARTTLS":
			_ = tp.PrintfLine("220 go")
			tc := tls.Server(c, s.tls)
			if tc.HandshakeContext(ctx) != nil {
				s.mu.Unlock()
				return
			}
			s.tlsOn = true
			c = tc
			tp = textproto.NewConn(tc)
		case "AUTH":
			s.auth = arg
			_ = tp.PrintfLine("235 ok")
		case "MAIL":
			s.from = arg
			_ = tp.PrintfLine("250 ok")
		case "RCPT":
			s.to = append(s.to, arg)
			_ = tp.PrintfLine("250 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go")
			s.mu.Unlock()
			b, _ := io.ReadAll(tp.DotReader())
			s.mu.Lock()
			s.data = string(b)
			_ = tp.PrintfLine("250 ok")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			s.mu.Unlock()
			return
		default:
			_ = tp.PrintfLine("502 no")
		}
		s.mu.Unlock()
	}
}

func TestAnEmailReachesEveryRecipientWithItsSubjectAndText(t *testing.T) {
	t.Parallel()
	s := newSMTPServer(t, nil)
	e := &Email{Server: s.addr, From: "Kubehenge <kubehenge@example.com>", To: []string{"Ops <ops@example.com>", "me@example.com"}}
	n := notice
	if err := e.Send(t.Context(), &n); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.HasPrefix(s.from, "FROM:<kubehenge@example.com>") || strings.Join(s.to, " ") != "TO:<ops@example.com> TO:<me@example.com>" {
		t.Errorf("from %q to %q", s.from, s.to)
	}
	msg, err := textproto.NewReader(bufio.NewReader(strings.NewReader(s.data))).ReadMIMEHeader()
	if err != nil {
		t.Fatal(err)
	}
	if subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Get("Subject")); subject != n.Title {
		t.Errorf("Subject %q, want %q", subject, n.Title)
	}
	// The dot reader hands lines over ending in LF.
	_, text, _ := strings.Cut(s.data, "\n\n")
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(text)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), n.Text) || !strings.Contains(string(decoded), n.Link) {
		t.Errorf("text %q, want the notice's text and link", decoded)
	}
}

func TestAnEmailSignsInOnlyAfterSTARTTLS(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	s := newSMTPServer(t, &tls.Config{Certificates: srv.TLS.Certificates, MinVersion: tls.VersionTLS12})
	transport, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the test server's client has a %T", srv.Client().Transport)
	}
	trusted := transport.TLSClientConfig.RootCAs
	e := &Email{
		Server: s.addr, From: "kubehenge@example.com", To: []string{"ops@example.com"}, Username: "kh", Password: "pw",
		TLS: &tls.Config{RootCAs: trusted, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12},
	}
	n := notice
	if err := e.Send(t.Context(), &n); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := "PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00kh\x00pw"))
	if !s.tlsOn || s.auth != want {
		t.Errorf("TLS %v, AUTH %q; want TLS and %q", s.tlsOn, s.auth, want)
	}
}

func TestAnEmailServerThatIsNotTrustedIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	s := newSMTPServer(t, &tls.Config{Certificates: srv.TLS.Certificates, MinVersion: tls.VersionTLS12})
	e := &Email{Server: s.addr, From: "kubehenge@example.com", To: []string{"ops@example.com"}, Username: "kh", Password: "pw"}
	n := notice
	if err := e.Send(t.Context(), &n); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("Send gave %v, want STARTTLS refused", err)
	}
}

func TestParseChecksEveryTargetsValues(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		purpose string
		values  map[string]string
		want    string
	}{
		{PurposeWebhook, map[string]string{"url": "https://hooks.example.com/x"}, ""},
		{PurposeWebhook, map[string]string{"url": "ftp://hooks.example.com/x"}, "https://"},
		{PurposeNtfy, map[string]string{"url": "https://ntfy.sh/kh"}, ""},
		{PurposeNtfy, map[string]string{"url": "ntfy.sh/kh"}, "https://"},
		{PurposeEmail, map[string]string{"server": "smtp.example.com:587", "from": "kh@example.com", "to": "a@example.com, b@example.com"}, ""},
		{PurposeEmail, map[string]string{"server": "smtp.example.com", "from": "kh@example.com", "to": "a@example.com"}, "host:port"},
		{PurposeEmail, map[string]string{"server": "smtp.example.com:587", "from": "kh@example.com", "to": " , "}, "recipient"},
		{PurposeEmail, map[string]string{"server": "smtp.example.com:587", "from": "kh@example.com", "to": "a@example.com", "username": "kh"}, "both"},
	} {
		values := map[string][]byte{}
		for k, v := range c.values {
			values[k] = []byte(v)
		}
		_, err := Parse(c.purpose, values, nil)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s %v gave %v, want %q", c.purpose, c.values, err, c.want)
		}
	}
}
