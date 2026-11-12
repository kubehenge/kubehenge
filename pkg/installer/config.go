// Package installer installs Kubehenge's appliance on a Proxmox VE node: a small unprivileged
// container running the web interface. It asks its questions first, on a terminal or by flags,
// and changes the node only after the user confirms the summary.
package installer

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
)

// Mode is what a run does.
type Mode int

// The modes of `kubehenge install`.
const (
	ModeInstall Mode = iota
	ModeUpgrade
	ModeCertificate
)

// Config holds every value the install takes, from flags and from answers.
type Config struct {
	CTID            string
	Storage         string
	TemplateStorage string
	Bridge          string
	VLAN            string
	MTU             string
	// IP is dhcp, none or an address with its prefix. Empty means not decided yet.
	IP             string
	Gateway        string
	IP6            string
	Gateway6       string
	DNS            string
	Hostname       string
	Name           string
	NoName         bool
	CertFile       string
	KeyFile        string
	ProxmoxURL     string
	ProxmoxCA      string
	Insecure       bool
	TrustedProxies []string
	NoTrustedProxy bool
	Release        string
	ReleaseSHA     string
	Tofu           string
	// PVEEntry is yes, no or empty for not decided.
	PVEEntry    string
	Upgrade     bool
	Certificate bool
	// Plain forces the one-line-per-step output.
	Plain bool
	// APIHost is the host picked from the API candidates, empty until chosen.
	APIHost string
	// TrustServedCA is the user's yes to the CA the API serves.
	TrustServedCA bool

	pveEntryFlags [2]*bool
}

// DefaultHostname is the container's host name when none is given.
const DefaultHostname = "kubehenge"

// Mode returns what the options ask for.
func (c *Config) Mode() Mode {
	switch {
	case c.Certificate:
		return ModeCertificate
	case c.Upgrade:
		return ModeUpgrade
	default:
		return ModeInstall
	}
}

// BindFlags registers every option of the shell installer, with its meaning.
func BindFlags(f *pflag.FlagSet, c *Config) {
	f.StringVar(&c.CTID, "ctid", "", "the container's ID (default the next free one)")
	f.StringVar(&c.Storage, "storage", "", "where the container's disks go, 9 GiB")
	f.StringVar(&c.TemplateStorage, "template-storage", "", "where the container template goes")
	f.StringVar(&c.Bridge, "bridge", "", "the bridge, Open vSwitch bridge or SDN VNet the container attaches to")
	f.StringVar(&c.VLAN, "vlan", "", "the VLAN tag of the container's network, 1 to 4094")
	f.StringVar(&c.MTU, "mtu", "", "the MTU of the container's network, 576 to 65535")
	f.StringVar(&c.IP, "ip", "", "the IPv4 address: dhcp, none or an address with its prefix, such as 192.0.2.40/24")
	f.StringVar(&c.Gateway, "gateway", "", "the default gateway, an IPv4 address, with a static --ip")
	f.StringVar(&c.IP6, "ip6", "", "the IPv6 address: auto, dhcp or an address with its prefix, such as 2001:db8::40/64")
	f.StringVar(&c.Gateway6, "gateway6", "", "the default IPv6 gateway, with a static --ip6")
	f.StringVar(&c.DNS, "dns", "", "the name servers the container asks, comma-separated (default this node's)")
	f.StringVar(&c.Hostname, "hostname", DefaultHostname, "the container's host name, also in its certificate")
	f.StringVar(&c.Name, "name", "", "a DNS name people reach the appliance by, put into its certificate")
	f.BoolVar(&c.NoName, "no-name", false, "with --certificate, drop the name the certificate was given")
	f.StringVar(&c.CertFile, "cert", "", "serve this certificate instead of one signed by this cluster's CA")
	f.StringVar(&c.KeyFile, "key", "", "the key of --cert")
	f.StringVar(&c.ProxmoxURL, "proxmox-url", "", "the Proxmox VE API, such as https://pve.example.org:8006")
	f.StringVar(&c.ProxmoxCA, "proxmox-ca", "", "the CA that signed the API's certificate, alone or in a bundle")
	f.BoolVar(&c.Insecure, "proxmox-insecure", false, "reach the Proxmox VE API without verifying its certificate")
	f.StringSliceVar(&c.TrustedProxies, "trusted-proxy", nil, "a reverse proxy in front of the interface, an address or CIDR, repeatable")
	f.BoolVar(&c.NoTrustedProxy, "no-trusted-proxy", false, "with --upgrade, remove the list of trusted proxies")
	f.StringVar(&c.Release, "release", "", "install this release archive instead of downloading it")
	f.StringVar(&c.ReleaseSHA, "sha256", "", "the archive's SHA-256, with --release")
	f.StringVar(&c.Tofu, "tofu", "", "install OpenTofu from this release archive instead of downloading it")
	pveEntry := f.Bool("pve-entry", false, "add the Kubernetes entry to this node's Proxmox VE interface")
	noPVEEntry := f.Bool("no-pve-entry", false, "do not add the Kubernetes entry")
	f.BoolVar(&c.Upgrade, "upgrade", false, "upgrade the appliance in container --ctid")
	f.BoolVar(&c.Certificate, "certificate", false, "replace the certificate of the appliance in container --ctid")
	f.BoolVar(&c.Plain, "plain", false, "print one line per step instead of the full-screen installer")
	// The entry answers are applied by Normalise once the flags are parsed.
	c.pveEntryFlags = [2]*bool{pveEntry, noPVEEntry}
}

// Normalise folds flags that cannot be read directly into the Config.
func (c *Config) Normalise() error {
	yes, no := c.pveEntryFlags[0], c.pveEntryFlags[1]
	switch {
	case yes != nil && no != nil && *yes && *no:
		return userErr("--pve-entry and --no-pve-entry exclude each other", "")
	case yes != nil && *yes:
		c.PVEEntry = optYes
	case no != nil && *no:
		c.PVEEntry = "no"
	}
	c.Name = strings.TrimSpace(c.Name)
	return nil
}

// containerNet returns the container's net0 setting. A gateway goes with a static address only.
// Without an IPv6 setting there is none, and IP none leaves the IPv4 one out.
func (c *Config) containerNet() string {
	var b strings.Builder
	fmt.Fprintf(&b, "name=eth0,bridge=%s", c.Bridge)
	if c.VLAN != "" {
		fmt.Fprintf(&b, ",tag=%s", c.VLAN)
	}
	if c.MTU != "" {
		fmt.Fprintf(&b, ",mtu=%s", c.MTU)
	}
	if c.IP != optNone {
		fmt.Fprintf(&b, ",ip=%s", c.IP)
		if c.IP != optDHCP && c.Gateway != "" {
			fmt.Fprintf(&b, ",gw=%s", c.Gateway)
		}
	}
	if c.IP6 != "" {
		fmt.Fprintf(&b, ",ip6=%s", c.IP6)
		if c.IP6 != optAuto && c.IP6 != optDHCP && c.Gateway6 != "" {
			fmt.Fprintf(&b, ",gw6=%s", c.Gateway6)
		}
	}
	return b.String()
}
