package installer

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// UserError is a failure in the user's terms: what is wrong, then the one thing to do.
type UserError struct {
	Cause string
	Fix   string
}

func (e *UserError) Error() string {
	if e.Fix == "" {
		return e.Cause
	}
	return e.Cause + ". " + e.Fix
}

func userErr(cause, fix string) error { return &UserError{Cause: cause, Fix: fix} }

var (
	dnsNameRE   = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	dnsListRE   = regexp.MustCompile(`^[0-9A-Fa-f.:]+(,[0-9A-Fa-f.:]+)*$`)
	apiURLRE    = regexp.MustCompile(`^https://[A-Za-z0-9.:\[-]+\]?(:[0-9]+)?$`)
	hostnameRE  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	dottedIPv4  = regexp.MustCompile(`^([0-9]{1,3}\.){3}[0-9]{1,3}$`)
	colonHexRE  = regexp.MustCompile(`^[0-9A-Fa-f:]+$`)
	ipv6TailRE  = regexp.MustCompile(`^[0-9A-Fa-f:]*:[0-9A-Fa-f:]*(:[0-9]{1,3}(\.[0-9]{1,3}){3})?$`)
	maxPrefix4  = 32
	maxPrefix6  = 128
	digitsOnlyR = regexp.MustCompile(`^[0-9]+$`)
)

// IsIPv4 reports whether s is a dotted IPv4 address.
func IsIPv4(s string) bool {
	if !dottedIPv4.MatchString(s) {
		return false
	}
	for _, octet := range strings.Split(s, ".") {
		n, err := strconv.Atoi(octet)
		if err != nil || n > 255 {
			return false
		}
	}
	return true
}

// inRange reports whether s is a whole number from least to most.
func inRange(s string, least, most int) bool {
	if !digitsOnlyR.MatchString(s) || len(s) > 6 {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= least && n <= most
}

// IsIPv4CIDR reports whether s is an IPv4 address with a prefix length of 1 to 32.
func IsIPv4CIDR(s string) bool {
	addr, prefix, ok := strings.Cut(s, "/")
	return ok && IsIPv4(addr) && inRange(prefix, 1, maxPrefix4)
}

// IsIPv6 reports whether s is an IPv6 address in colon-hex form, without a zone or a dotted tail.
func IsIPv6(s string) bool {
	if !colonHexRE.MatchString(s) || !strings.Contains(s, ":") || strings.Contains(s, ":::") {
		return false
	}
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is6()
}

// IsIPv6CIDR reports whether s is an IPv6 address with a prefix length of 1 to 128.
func IsIPv6CIDR(s string) bool {
	addr, prefix, ok := strings.Cut(s, "/")
	return ok && IsIPv6(addr) && inRange(prefix, 1, maxPrefix6)
}

// IsProxyRange reports whether s is an IPv4 or IPv6 address, with a prefix length that fits it
// when given.
func IsProxyRange(s string) bool {
	addr, prefix, hasPrefix := strings.Cut(s, "/")
	bits := 0
	switch {
	case IsIPv4(addr):
		bits = maxPrefix4
	case ipv6TailRE.MatchString(addr):
		bits = maxPrefix6
	default:
		return false
	}
	return !hasPrefix || inRange(prefix, 0, bits)
}

// ValidateAddressCIDR checks a static IPv4 address typed as address/prefix.
func ValidateAddressCIDR(s string) error {
	if !IsIPv4CIDR(s) {
		return fmt.Errorf("an address with its prefix, such as 192.0.2.40/24")
	}
	return nil
}

// ValidateIPv4 checks a single IPv4 address.
func ValidateIPv4(s string) error {
	if !IsIPv4(s) {
		return fmt.Errorf("an IPv4 address, such as 192.0.2.1")
	}
	return nil
}

// Validate checks every value the options give, as the shell installer did, before anything is
// detected or changed.
func (c *Config) Validate() error {
	checks := []struct {
		bad       bool
		cause, to string
	}{
		{c.Name != "" && !dnsNameRE.MatchString(c.Name), "--name is a DNS name such as kubehenge.example.org", ""},
		{c.DNS != "" && !dnsListRE.MatchString(c.DNS), "--dns is a comma-separated list of name server addresses, such as 192.0.2.53,192.0.2.54", ""},
		{c.ProxmoxURL != "" && !apiURLRE.MatchString(c.ProxmoxURL), "--proxmox-url is the Proxmox VE API's https:// URL, such as https://pve.example.org:8006", ""},
		{c.ProxmoxCA != "" && c.Insecure, "--proxmox-ca and --proxmox-insecure exclude each other", ""},
		{c.Insecure && c.Certificate, "--proxmox-insecure goes with an install or --upgrade", ""},
		{len(c.TrustedProxies) > 0 && c.Certificate, "--trusted-proxy goes with an install or --upgrade", ""},
		{c.NoTrustedProxy && len(c.TrustedProxies) > 0, "--trusted-proxy and --no-trusted-proxy exclude each other", ""},
		{c.NoTrustedProxy && !c.Upgrade, "--no-trusted-proxy goes with --upgrade", ""},
		{c.IP != "" && c.IP != "dhcp" && c.IP != "none" && !IsIPv4CIDR(c.IP), "--ip is dhcp or an IPv4 address with its prefix length, such as 192.0.2.5/24, or none on an IPv6-only network", ""},
		{c.Gateway != "" && !IsIPv4(c.Gateway), "--gateway is an IPv4 address, such as 192.0.2.1", ""},
		{c.IP6 != "" && c.IP6 != "auto" && c.IP6 != "dhcp" && !IsIPv6CIDR(c.IP6), "--ip6 is auto, dhcp or an IPv6 address with its prefix length, such as 2001:db8::5/64", ""},
		{c.Gateway6 != "" && !IsIPv6(c.Gateway6), "--gateway6 is an IPv6 address, such as 2001:db8::1", ""},
		{c.IP == "none" && c.IP6 == "", "--ip none needs --ip6, as the container needs an address", ""},
		{c.IP == "none" && c.Gateway != "", "--gateway goes with a static --ip", ""},
		{c.Gateway6 != "" && (c.IP6 == "" || c.IP6 == "auto" || c.IP6 == "dhcp"), "--gateway6 goes with a static --ip6", ""},
		{c.VLAN != "" && !inRange(c.VLAN, 1, 4094), "--vlan is a VLAN tag from 1 to 4094", ""},
		{c.MTU != "" && !inRange(c.MTU, 576, 65535), "--mtu is a number of bytes from 576 to 65535", ""},
		{c.NoName && (!c.Certificate || c.Name != ""), "--no-name goes with --certificate and without --name", ""},
		{c.Hostname != "" && !hostnameRE.MatchString(c.Hostname), "--hostname is a host name such as kubehenge", ""},
		{(c.CertFile != "") != (c.KeyFile != ""), "--cert and --key name a readable PEM certificate and its key", ""},
	}
	for _, check := range checks {
		if check.bad {
			return userErr(check.cause, check.to)
		}
	}
	for _, entry := range c.TrustedProxies {
		if !IsProxyRange(entry) {
			return userErr(fmt.Sprintf("--trusted-proxy %s: use an IPv4 or IPv6 address or CIDR range, such as 192.0.2.10 or 10.0.0.0/8", entry), "")
		}
	}
	if c.Mode() == ModeInstall && c.Release != "" && c.ReleaseSHA == "" {
		return userErr("--release needs --sha256", "")
	}
	return nil
}
