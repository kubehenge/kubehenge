package ui

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/acme"
	"github.com/kubehenge/kubehenge/pkg/certfiles"
	"github.com/kubehenge/kubehenge/pkg/notify"
)

// errSuperseded is a delivery whose setting was saved over or removed while its order ran.
var errSuperseded = errors.New("the ACME setting changed while the order ran")

// UseACME has the manager order and renew this certificate. Its certificates reach the interface
// through the same change lock and the same swap an upload takes.
func (c *InterfaceCertificate) UseACME(m *acme.Manager) {
	c.acme = m
	m.Served = func() acme.Served {
		served := c.served()
		// The stored certificate, as one from ACME that ran out while the appliance was off, could
		// not be served. Nothing usable is served, so the manager orders rather than taking the
		// installer's stand-in for an administrator's choice.
		if served == nil || served.notServed != "" {
			return acme.Served{}
		}
		return acme.Served{Leaf: served.pair.Leaf, Source: served.source, Order: served.order}
	}
	m.Deliver = c.deliverACME
}

// demoACMEFailure is what the demo's ACME server says to a DNS-01 order, so that the page has a
// failing state to show.
const demoACMEFailure = "the DNS provider could not add the record _acme-challenge.kubehenge.example.org: dial udp 192.0.2.53:53: i/o timeout"

// useDemoACME gives the demo an ACME manager that keeps its files in memory and answers at once. An
// order for a DNS-01 proof fails, any other issues a certificate from the demo's CA, so the page
// can show both states.
func (c *InterfaceCertificate) useDemoACME(now func() time.Time) {
	if c.acme != nil || c.demoIssue == nil {
		return
	}
	m := acme.New("")
	m.Name = func() string { return demoHost }
	m.HTTPListening = true
	m.Now = now
	m.Obtain = func(_ context.Context, set *acme.Setting, _ string, _ *x509.Certificate) ([]byte, []byte, error) {
		if set.Proof == acme.ProofDNS {
			return nil, nil, errors.New(demoACMEFailure)
		}
		return c.demoIssue(now())
	}
	m.ARI = func(context.Context, *acme.Setting, *x509.Certificate) (time.Time, error) {
		return time.Time{}, errors.New("the demo's ACME server gives no renewal information")
	}
	c.UseACME(m)
}

// ACME is the manager that orders this certificate, nil when none does.
func (c *InterfaceCertificate) ACME() *acme.Manager { return c.acme }

// stopACME ends ACME's renewals and removes its setting, for a certificate that is chosen in its
// place. The snapshot puts them back when the change fails.
func (c *InterfaceCertificate) stopACME() *acme.Snapshot {
	if c.acme == nil {
		return nil
	}
	return c.acme.Disable()
}

// deliverACME serves the certificate an order got, once it reads cleanly, is valid for the public
// name and still belongs to the setting that asked for it.
func (c *InterfaceCertificate) deliverACME(_ context.Context, in *acme.Issued) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.acme.Current(in.Generation) {
		return errSuperseded
	}
	now := c.acme.Now()
	b, err := certfiles.Read([]certfiles.File{{Name: "acme.pem", Data: slices.Concat(in.KeyPEM, in.CertPEM)}}, "", c.roots(), now)
	if err != nil {
		return fmt.Errorf("the certificate the ACME server gave does not read: %w", err)
	}
	if name := c.acme.PublicName(); name != "" {
		if err = b.Leaf.VerifyHostname(name); err != nil {
			return fmt.Errorf("the certificate the ACME server gave is not valid for %s", name)
		}
	}
	next, err := newServed(b, certificateACME, in.OrderID)
	if err != nil {
		return err
	}
	_, err = c.install(next, b)
	return err
}

// ConfigForClient gives the listener that serves base its per-handshake configuration, which adds
// acme-tls/1 while a tls-alpn-01 challenge is pending. It changes nothing when ACME is not in use.
func (c *InterfaceCertificate) ConfigForClient(base *tls.Config) func(*tls.ClientHelloInfo) (*tls.Config, error) {
	if c.acme == nil {
		return func(*tls.ClientHelloInfo) (*tls.Config, error) { return nil, nil }
	}
	return c.acme.ConfigForClient(base)
}

func (s *Server) acmeTarget(w http.ResponseWriter, r *http.Request, u user) *InterfaceCertificate {
	c := s.certificateToChange(w, r, u)
	if c == nil {
		return nil
	}
	if c.acme == nil {
		writeError(w, http.StatusNotFound, "the certificate is managed in the appliance")
		return nil
	}
	return c
}

// refuseACME answers a request ACME refuses, or fails when it could not keep the change.
func (s *Server) refuseACME(w http.ResponseWriter, r *http.Request, err error) {
	if refusal, ok := errors.AsType[*acme.RefusalError](err); ok {
		status := http.StatusUnprocessableEntity
		switch refusal.Code {
		case acme.CodeNoPublicName, acme.CodeNoHTTP, acme.CodeNotSet, acme.CodeOrdering:
			status = http.StatusConflict
		}
		writeCertificateError(w, status, refusal.Code, refusal.Message, nil)
		return
	}
	s.changeFailed(w, r, err)
}

// setACME saves the ACME setting and starts the first order, which runs on after the answer.
func (s *Server) setACME(w http.ResponseWriter, r *http.Request, u user) {
	c := s.acmeTarget(w, r, u)
	if c == nil {
		return
	}
	var req acme.Request
	if !readJSON(w, r, &req) {
		return
	}
	if !lockChange(w, c) {
		return
	}
	defer c.mu.Unlock()
	view, err := c.acme.Save(&req)
	if err != nil {
		s.refuseACME(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "interface certificate ordering set", "user", u.ticket.Username, "directory", view.Directory, "proof", view.Proof, "provider", view.DNSProvider)
	writeJSON(w, http.StatusAccepted, s.certificateViewOf(r.Context(), c))
}

// orderACME asks for a certificate at once.
func (s *Server) orderACME(w http.ResponseWriter, r *http.Request, u user) {
	c := s.acmeTarget(w, r, u)
	if c == nil {
		return
	}
	if !lockChange(w, c) {
		return
	}
	defer c.mu.Unlock()
	view, err := c.acme.OrderNow()
	if err != nil {
		s.refuseACME(w, r, err)
		return
	}
	s.Log.InfoContext(r.Context(), "interface certificate ordered", "user", u.ticket.Username, "directory", view.Directory, "proof", view.Proof)
	writeJSON(w, http.StatusAccepted, s.certificateViewOf(r.Context(), c))
}

// acmeProviders lists the DNS providers the ACME client knows. The variables each reads are in the
// ACME client's own documentation, which it does not offer to a program, so variablesListed is false.
func (s *Server) acmeProviders(w http.ResponseWriter, r *http.Request, u user) {
	c := s.certificateTarget(w, r, u)
	if c == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": acme.Providers(), "variablesListed": false})
}

// findCertificate adds the problem that the interface stopped ordering its certificate from ACME
// after a day of failures, until an administrator saves the setting or orders again.
func (s *Server) findCertificate(found *notifyFound) {
	if s.Certificate == nil || s.Certificate.acme == nil {
		return
	}
	msg, gaveUp := s.Certificate.acme.Failed()
	if !gaveUp {
		return
	}
	found.problems = append(found.problems, notify.Notice{
		Key: notifyCertificateKey, Severity: notify.Error, Title: "Certificate not renewed", Text: strings.TrimSpace(msg), Link: s.notifyLink("/administration"),
	})
}
