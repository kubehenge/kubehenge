// Package installer installs Kubehenge's appliance on a Proxmox VE node: a small unprivileged
// container running the web interface. It asks its questions first, on a terminal or by flags,
// and changes the node only after the user confirms the summary.
package installer

import (
	"fmt"
	"strings"
	"text/tabwriter"

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
	IP       string
	Gateway  string
	IP6      string
	Gateway6 string
	DNS      string
	Hostname string
	Name     string
	NoName   bool
	// CertFile is the comma-separated files of the administrator's certificate, in any format, and
	// KeyFile the key's file where it is a file of its own.
	CertFile string
	KeyFile  string
	// CertPassphraseFile is the file whose first line is the passphrase of encrypted files.
	CertPassphraseFile string
	ProxmoxURL         string
	ProxmoxCA          string
	Insecure           bool
	TrustedProxies     []string
	NoTrustedProxy     bool
	Release            string
	ReleaseSHA         string
	Tofu               string
	// PVEEntry is yes, no or empty for not decided.
	PVEEntry    string
	Upgrade     bool
	Certificate bool
	// Plain forces the one-line-per-step output.
	Plain bool
	// Yes installs without the confirmation of the summary, and accepts an address outside the
	// subnet the node knows of the network.
	Yes bool
	// DryRun prints the plan and changes nothing.
	DryRun bool
	// KeepFailed leaves a container the install failed in for inspection, where it is otherwise removed.
	KeepFailed bool
	// APIHost is the host picked from the API candidates, empty until chosen.
	APIHost string
	// TrustServedCA is the user's yes to the CA the API serves.
	TrustServedCA bool

	// passphrase is what the user typed for encrypted files. It lives in memory only: no answers
	// file, repeat command, log or error holds it.
	passphrase    string
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
	f.StringVar(&c.IP, "ip", "", "the IPv4 address: dhcp, none or an address with or without /prefix, such as 192.0.2.40/24")
	f.StringVar(&c.Gateway, "gateway", "", "the default gateway, an IPv4 address, with a static --ip")
	f.StringVar(&c.IP6, "ip6", "", "the IPv6 address: auto, dhcp or an address with its prefix, such as 2001:db8::40/64")
	f.StringVar(&c.Gateway6, "gateway6", "", "the default IPv6 gateway, with a static --ip6")
	f.StringVar(&c.DNS, "dns", "", "the name servers the container asks, comma-separated (default this node's)")
	f.StringVar(&c.Hostname, "hostname", "", "the container's host name, also in its certificate (default "+DefaultHostname+")")
	f.StringVar(&c.Name, "name", "", "a DNS name people open the interface at, put into its certificate")
	f.BoolVar(&c.NoName, "no-name", false, "with --certificate, drop the name the certificate was given")
	f.StringVar(&c.CertFile, "cert", "", "serve the certificate in these files, comma-separated, in any format, instead of one signed by this cluster's CA")
	f.StringVar(&c.KeyFile, "key", "", "the key of --cert, where it is a file of its own")
	f.StringVar(&c.CertPassphraseFile, "cert-passphrase-file", "", "a file whose first line is the passphrase of encrypted --cert and --key files")
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
	f.BoolVar(&c.Plain, "plain", false, "run without the full-screen installer: ask nothing, print a line per step")
	f.BoolVar(&c.Yes, "yes", false, "skip the summary confirmation and accept an address outside the network's subnet")
	f.BoolVar(&c.DryRun, "dry-run", false, "print what would be installed and change nothing")
	f.BoolVar(&c.KeepFailed, "keep-failed", false, "keep the container of a failed install for inspection instead of removing it")
	// The entry answers are applied by Normalise once the flags are parsed.
	c.pveEntryFlags = [2]*bool{pveEntry, noPVEEntry}
	// Each group holds the flag of its mode, named as the group, and the flag only that mode takes.
	for group, only := range map[string]string{groupUpgrade: "no-trusted-proxy", groupCertificate: "no-name"} {
		for _, name := range []string{strings.ToLower(group), only} {
			_ = f.SetAnnotation(name, flagGroup, []string{group})
		}
	}
}

// The groups the options are listed in. An option without a group is an install's.
const (
	flagGroup        = "group"
	groupInstall     = "Install"
	groupUpgrade     = "Upgrade"
	groupCertificate = "Certificate"
)

// FlagUsages lists the options under Install, Upgrade and Certificate.
func FlagUsages(f *pflag.FlagSet) string {
	groups := map[string][]*pflag.Flag{}
	f.VisitAll(func(fl *pflag.Flag) {
		group := groupInstall
		if g := fl.Annotations[flagGroup]; len(g) > 0 {
			group = g[0]
		}
		groups[group] = append(groups[group], fl)
	})
	var parts []string
	for _, group := range []string{groupInstall, groupUpgrade, groupCertificate} {
		if list := groups[group]; list != nil {
			parts = append(parts, group+":\n"+flagLines(list))
		}
	}
	return strings.Join(parts, "\n")
}

// flagGap is the spaces between a flag and its meaning, as pflag puts them.
const flagGap = 3

// flagLines lists flags in pflag's layout, without the [=true|false] pflag puts after a switch.
func flagLines(list []*pflag.Flag) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, flagGap, ' ', 0)
	for _, fl := range list {
		name := "      --" + fl.Name
		if fl.Shorthand != "" {
			name = "  -" + fl.Shorthand + ", --" + fl.Name
		}
		varname, usage := pflag.UnquoteUsage(fl)
		if varname != "" {
			name += " " + varname
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\n", name, usage)
	}
	_ = w.Flush()
	return b.String()
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
	c.Name = strings.ToLower(strings.TrimSpace(c.Name))
	return nil
}

// repeatBase is RepeatCommand with no answers.
const repeatBase = "sh install.sh"

// RepeatCommand is the install.sh command that runs an install again with this one's answers.
// The options hold no secret: a certificate's key and its passphrase are named by their files.
func (c *Config) RepeatCommand() string {
	args := []string{repeatBase}
	add := func(name, value string) {
		if value != "" {
			args = append(args, name, shellWord(value))
		}
	}
	add("--ctid", c.CTID)
	add("--storage", c.Storage)
	add("--template-storage", c.TemplateStorage)
	add("--bridge", c.Bridge)
	add("--vlan", c.VLAN)
	add("--mtu", c.MTU)
	add("--ip", c.IP)
	add("--gateway", c.Gateway)
	add("--ip6", c.IP6)
	add("--gateway6", c.Gateway6)
	add("--dns", c.DNS)
	if c.Hostname != DefaultHostname {
		add("--hostname", c.Hostname)
	}
	add("--name", c.Name)
	add("--cert", c.CertFile)
	add("--key", c.KeyFile)
	add("--cert-passphrase-file", c.CertPassphraseFile)
	add("--proxmox-url", c.ProxmoxURL)
	add("--proxmox-ca", c.ProxmoxCA)
	for _, p := range c.TrustedProxies {
		add("--trusted-proxy", p)
	}
	add("--release", c.Release)
	add("--sha256", c.ReleaseSHA)
	add("--tofu", c.Tofu)
	switches := []struct {
		flag string
		on   bool
	}{
		{"--proxmox-insecure", c.Insecure},
		{"--pve-entry", c.PVEEntry == optYes},
		{"--no-pve-entry", c.PVEEntry == "no"},
		{"--plain", c.Plain},
		{"--yes", c.Yes},
		{"--keep-failed", c.KeepFailed},
	}
	for _, s := range switches {
		if s.on {
			args = append(args, s.flag)
		}
	}
	return strings.Join(args, " ")
}

// shellWord quotes a value for a POSIX shell where it holds more than letters, digits and the
// marks addresses and paths use.
func shellWord(s string) string {
	if strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%_+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
