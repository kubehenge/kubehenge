package ui

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/kubehenge/kubehenge/pkg/acme"
	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/certfiles"
)

// PublicAddress is the address users reach the interface at: the one an administrator set on the
// certificate page, or else PublicURL.
func (s *Server) PublicAddress() string {
	if s.Certificate != nil {
		if set := s.Certificate.address.Load(); set != nil {
			return *set
		}
	}
	return s.PublicURL
}

// loadAddress reads the public address an administrator set. One that does not read is logged and
// left alone, and PublicURL holds. The caller holds mu.
func (c *InterfaceCertificate) loadAddress(log *slog.Logger) {
	raw, err := os.ReadFile(c.addressPath())
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	set := strings.TrimSpace(string(raw))
	if u, parseErr := url.Parse(set); err != nil || parseErr != nil || u.Hostname() == "" {
		log.Error("the stored public address is not used", "file", c.addressPath(), "address", set, "error", err)
		return
	}
	c.address.Store(&set)
}

func (c *InterfaceCertificate) addressPath() string {
	return filepath.Join(c.dir, appliance.PublicURLFile)
}

// keepAddress stores the public address, and Headlamp's callback at it, then uses it. Nil goes back
// to PublicURL. The caller holds mu.
func (c *InterfaceCertificate) keepAddress(address *string) error {
	if !c.memory {
		if err := c.storeAddress(address); err != nil {
			return err
		}
	}
	c.address.Store(address)
	return nil
}

func (c *InterfaceCertificate) storeAddress(address *string) error {
	if address == nil {
		for _, name := range []string{appliance.PublicURLFile, appliance.HeadlampEnvFile} {
			if err := c.fileOps().Remove(filepath.Join(c.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err //nolint:wrapcheck // the file operations name what failed
			}
		}
		return nil
	}
	if c.HeadlampPort != "" {
		u, err := url.Parse(*address)
		if err != nil {
			return fmt.Errorf("reading the public address: %w", err)
		}
		callback := "https://" + net.JoinHostPort(u.Hostname(), c.HeadlampPort) + "/oidc-callback"
		if err = c.write(appliance.HeadlampEnvFile, []byte("HEADLAMP_CONFIG_OIDC_CALLBACK_URL="+callback+"\n")); err != nil {
			return err
		}
	}
	return c.write(appliance.PublicURLFile, []byte(*address+"\n"))
}

// addressRequest is a new public address: a DNS name or an IP address.
type addressRequest struct {
	Name string `json:"name"`
}

// addressHost is name as the host of a URL, lower case and without a final dot, and whether it is
// a DNS name or an IP address.
func addressHost(name string) (string, bool) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if addr, err := netip.ParseAddr(strings.Trim(name, "[]")); err == nil {
		return addr.Unmap().String(), addr.Zone() == ""
	}
	return name, name != "" && len(validation.IsDNS1123Subdomain(name)) == 0
}

// schemeHTTPS is the scheme of every address the interface gives out or takes.
const schemeHTTPS = "https"

// withHost is address with its host replaced, keeping its port and path. https when there is none.
func withHost(address, host string) string {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		u = &url.URL{Scheme: schemeHTTPS}
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	return u.String()
}

// renamed is the template of a certificate for the names of old with from replaced by to.
func renamed(old *x509.Certificate, from, to string) *x509.Certificate {
	next := &x509.Certificate{Subject: old.Subject}
	if addr, err := netip.ParseAddr(to); err == nil {
		next.IPAddresses = []net.IP{addr.AsSlice()}
	} else {
		next.DNSNames = []string{to}
		next.Subject.CommonName = to
	}
	next.DNSNames = append(next.DNSNames, slices.DeleteFunc(slices.Clone(old.DNSNames), func(n string) bool { return n == from || n == to })...)
	for _, ip := range old.IPAddresses {
		if !slices.ContainsFunc(next.IPAddresses, ip.Equal) {
			next.IPAddresses = append(next.IPAddresses, ip)
		}
	}
	return next
}

// setPublicAddress changes the address users reach the interface at. A certificate the appliance
// made, self-signed or signed by its CA, is made again for the new name, and ACME orders one. Any
// other certificate stays, and the page says when it does not cover the new name.
func (s *Server) setPublicAddress(w http.ResponseWriter, r *http.Request, u user) {
	c := s.certificateToChange(w, r, u)
	if c == nil {
		return
	}
	var req addressRequest
	if !readJSON(w, r, &req) {
		return
	}
	host, ok := addressHost(req.Name)
	if !ok {
		writeCertificateError(w, http.StatusUnprocessableEntity, "bad-address",
			fmt.Sprintf("%q is not a DNS name or an IP address. Give one, such as kubehenge.example.org", req.Name), nil)
		return
	}
	if !lockChange(w, c) {
		return
	}
	defer c.mu.Unlock()
	before, old := c.address.Load(), s.PublicAddress()
	next := withHost(old, host)
	m, err := s.remadeFor(c, old, host)
	if err != nil {
		s.changeFailed(w, r, err)
		return
	}
	if err = c.keepAddress(&next); err != nil {
		s.addressNotChanged(w, r, c, before, m, err)
		return
	}
	if m.bundle != nil {
		if _, err = c.replace(m.bundle, m.source); err != nil {
			s.addressNotChanged(w, r, c, before, m, err)
			return
		}
	}
	if c.acme != nil && c.acme.View() != nil {
		if _, err = c.acme.OrderNow(); err != nil && !isRefusal(err) {
			s.Log.ErrorContext(r.Context(), "ordering a certificate for the new public address", "error", err)
		}
	}
	s.Log.InfoContext(r.Context(), "interface public address changed", "user", u.ticket.Username, "address", next)
	writeJSON(w, http.StatusOK, s.certificateViewOf(r.Context(), c))
}

// addressNotChanged answers a change cut short, with the address files, Headlamp's included, and
// the CA put back as they were. The caller holds mu.
func (s *Server) addressNotChanged(w http.ResponseWriter, r *http.Request, c *InterfaceCertificate, before *string, m remade, err error) {
	if undo := errors.Join(c.keepAddress(before), m.undoCA()); undo != nil {
		s.Log.ErrorContext(r.Context(), "putting the public address and its CA back", "error", undo)
	}
	s.changeFailed(w, r, err)
}

// remade is the certificate to serve at a new address, with its source.
type remade struct {
	bundle *certfiles.Bundle
	source string
	// undoCA puts back the CA there was before one was made for the new address.
	undoCA func() error
}

// remadeFor is the certificate to serve at host instead of the current one, none when it covers host
// already or only its owner can make another. The caller holds mu.
func (s *Server) remadeFor(c *InterfaceCertificate, old, host string) (remade, error) {
	m := remade{undoCA: func() error { return nil }}
	served := c.served()
	if served.pair.Leaf.VerifyHostname(host) == nil || (c.acme != nil && c.acme.View() != nil) {
		return m, nil
	}
	from := ""
	if u, err := url.Parse(old); err == nil {
		from = u.Hostname()
	}
	tmpl := renamed(served.pair.Leaf, from, host)
	var err error
	switch served.source {
	case certificateSelfSigned:
		m.bundle, err = selfSigned(tmpl, s.now())
	case certificateApplianceCA:
		m.bundle, m.undoCA, err = c.signedByCA(tmpl, s.now())
	default:
		return m, nil
	}
	m.source = served.source
	return m, err
}

func isRefusal(err error) bool {
	_, ok := errors.AsType[*acme.RefusalError](err)
	return ok
}
