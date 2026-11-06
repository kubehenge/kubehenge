package ui

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// forwardedHeader is the header reverse proxies and load balancers append the address they were
// reached from to.
const forwardedHeader = "X-Forwarded-For"

// ParseTrustedProxies reads the addresses and CIDR ranges of the reverse proxies that may tell the
// interface who a client is. A bare address is a range of one. Empty entries are skipped.
func ParseTrustedProxies(specs []string) ([]netip.Prefix, error) {
	var proxies []netip.Prefix
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(spec)
		if err != nil {
			addr, addrErr := netip.ParseAddr(spec)
			if addrErr != nil {
				return nil, fmt.Errorf("%q is neither an address nor a CIDR range", spec)
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		proxies = append(proxies, prefix.Masked())
	}
	return proxies, nil
}

type clientAddressKey struct{}

// withClientAddress resolves the address of the client behind trusted proxies, for clientAddress.
// A request whose peer is not listed is left as it is: its forwarding headers are the client's own
// words.
func (s *Server) withClientAddress(next http.Handler) http.Handler {
	if len(s.TrustedProxies) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if client, ok := forwardedClient(r, s.TrustedProxies); ok {
			r = r.WithContext(context.WithValue(r.Context(), clientAddressKey{}, client))
		}
		next.ServeHTTP(w, r)
	})
}

// forwardedClient is the client of a request that came through trusted proxies: the right-most
// X-Forwarded-For entry that is not a trusted proxy itself, since each proxy appends the address
// it was reached from and only the entries its trusted neighbours wrote can be believed. ok is
// false when the peer is not a trusted proxy, and so is every header.
func forwardedClient(r *http.Request, trusted []netip.Prefix) (string, bool) {
	peer, ok := peerAddress(r.RemoteAddr)
	if !ok || !isTrusted(peer, trusted) {
		return "", false
	}
	var entries []string
	for _, line := range r.Header.Values(forwardedHeader) {
		for entry := range strings.SplitSeq(line, ",") {
			entries = append(entries, strings.TrimSpace(entry))
		}
	}
	client := peer
	for i := len(entries) - 1; i >= 0; i-- {
		addr, valid := forwardedAddress(entries[i])
		if !valid {
			// A proxy only writes addresses, so the entry came from before one this chain trusts.
			// Nothing to its left can be believed either.
			break
		}
		client = addr
		if !isTrusted(addr, trusted) {
			break
		}
	}
	return client.String(), true
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// peerAddress is the address of a connection's remote end, as http.Request.RemoteAddr holds it.
func peerAddress(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(host)
	return addr.Unmap().WithZone(""), err == nil
}

// forwardedAddress reads one X-Forwarded-For entry: an address, with or without a port, and an
// IPv6 one with or without brackets.
func forwardedAddress(entry string) (netip.Addr, bool) {
	if addr, err := netip.ParseAddr(entry); err == nil {
		return addr.Unmap().WithZone(""), true
	}
	if ap, err := netip.ParseAddrPort(entry); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	if inner, ok := strings.CutSuffix(strings.TrimPrefix(entry, "["), "]"); ok {
		if addr, err := netip.ParseAddr(inner); err == nil {
			return addr.Unmap().WithZone(""), true
		}
	}
	return netip.Addr{}, false
}
