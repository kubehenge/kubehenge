package notify

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
)

// The purposes of the credentials that hold the targets, one each.
const (
	PurposeWebhook = "notify-webhook"
	PurposeNtfy    = "notify-ntfy"
	PurposeEmail   = "notify-email"
)

// Purposes are every target's, in the order they are shown.
var Purposes = []string{PurposeWebhook, PurposeNtfy, PurposeEmail}

// Stored reads the values of the credential of one purpose, and whether it is stored.
type Stored func(purpose string) (map[string][]byte, bool, error)

// Targets are those whose credentials are stored. One whose values are wrong is left out and
// named in the errors, so that the others still send.
func Targets(read Stored, client *http.Client) ([]Target, []error) {
	var targets []Target
	var errs []error
	for _, purpose := range Purposes {
		values, ok, err := read(purpose)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", purpose, err))
			continue
		}
		if !ok {
			continue
		}
		t, err := Parse(purpose, values, client)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", purpose, err))
			continue
		}
		targets = append(targets, t)
	}
	return targets, errs
}

// Parse makes a target of its credential's values.
func Parse(purpose string, values map[string][]byte, client *http.Client) (Target, error) { //nolint:ireturn // one of the targets, by its purpose
	value := func(k string) string { return strings.TrimSpace(string(values[k])) }
	switch purpose {
	case PurposeWebhook:
		if err := CheckURL(value("url")); err != nil {
			return nil, err
		}
		return &Webhook{URL: value("url"), Client: client}, nil
	case PurposeNtfy:
		if err := CheckURL(value("url")); err != nil {
			return nil, err
		}
		return &Ntfy{URL: value("url"), Token: value("token"), Client: client}, nil
	case PurposeEmail:
		e := &Email{Server: value("server"), From: value("from"), Username: value("username"), Password: string(values["password"])}
		for to := range strings.SplitSeq(value("to"), ",") {
			if to = strings.TrimSpace(to); to != "" {
				e.To = append(e.To, to)
			}
		}
		return e, CheckEmail(e)
	}
	return nil, fmt.Errorf("no target is called %s", purpose)
}

// CheckURL accepts an http or https address with a host.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("the url must start with https:// or http:// and name a host")
	}
	return nil
}

// CheckEmail accepts a server as host:port, a sender and at least one recipient.
func CheckEmail(e *Email) error {
	if _, port, err := net.SplitHostPort(e.Server); err != nil || port == "" {
		return errors.New("the server must be host:port, as smtp.example.com:587")
	}
	if _, err := mail.ParseAddress(e.From); err != nil {
		return fmt.Errorf("the sender %q is not an address", e.From)
	}
	if len(e.To) == 0 {
		return errors.New("name at least one recipient")
	}
	for _, to := range e.To {
		if _, err := mail.ParseAddress(to); err != nil {
			return fmt.Errorf("the recipient %q is not an address", to)
		}
	}
	if (e.Username == "") != (e.Password == "") {
		return errors.New("give both the username and the password, or neither")
	}
	return nil
}
