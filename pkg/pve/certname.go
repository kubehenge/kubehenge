package pve

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// CertificateNameError is a certificate the Proxmox VE API serves that is not valid for the name or
// address its URL reaches it at.
type CertificateNameError struct {
	// Host is the name or address in the URL.
	Host string
	// Names are the names and addresses the certificate is valid for, loopback left out: addresses
	// first, then names, each in the certificate's order.
	Names []string
}

func (e *CertificateNameError) Error() string {
	listed := "no name or address but loopback"
	if len(e.Names) > 0 {
		listed = strings.Join(e.Names, ", ")
	}
	msg := fmt.Sprintf("Proxmox VE's certificate is valid for %s, not %s. Use a Proxmox VE URL with one of those", listed, e.Host)
	if _, err := netip.ParseAddr(e.Host); err != nil {
		return msg + ", or install a certificate for " + e.Host + " on Proxmox VE."
	}
	// Proxmox VE names in its own certificate only the first address the node's name resolves to,
	// so on a dual-stack node it is often the IPv6 one.
	return msg + ", or reissue the certificate for " + e.Host + ": make the node's name resolve to " + e.Host +
		" first, then run `pvecm updatecerts --force` and `systemctl restart pveproxy` on it."
}

// certificateNamed turns a failed verification of the certificate's names into a
// CertificateNameError, and passes any other error on.
func certificateNamed(err error) error {
	var mismatch x509.HostnameError
	if err == nil || !errors.As(err, &mismatch) || mismatch.Certificate == nil {
		return err
	}
	return &CertificateNameError{Host: strings.Trim(mismatch.Host, "[]"), Names: certificateNames(mismatch.Certificate)}
}

// certificateNames are the names and addresses a certificate is valid for, as CertificateNameError
// lists them.
func certificateNames(cert *x509.Certificate) []string {
	var names []string
	for _, ip := range cert.IPAddresses {
		if a, ok := netip.AddrFromSlice(ip); ok && !a.Unmap().IsLoopback() {
			names = append(names, a.Unmap().String())
		}
	}
	for _, n := range cert.DNSNames {
		if !strings.EqualFold(n, "localhost") {
			names = append(names, n)
		}
	}
	return slices.Compact(names)
}
