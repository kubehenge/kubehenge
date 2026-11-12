package installer

import (
	"bytes"
	"context"
	"crypto/x509"
	_ "embed" // the in-container upgrade script
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

//go:embed upgrade.sh
var upgradeScript string

// Exit statuses of the upgrade script.
const (
	exitBusy       = 75
	exitRolledBack = 76
	exitStuck      = 77
	exitCutOff     = 78
)

const certRenewBefore = 60 * 24 * time.Hour

// requireAppliance stops unless the container is an appliance on this node.
func (r *Runner) requireAppliance(ctx context.Context) error {
	id := r.S.Cfg.CTID
	if id == "" {
		return userErr("--upgrade and --certificate need --ctid", "")
	}
	for _, a := range r.S.Node.ApplianceContainers(ctx) {
		if a.ID == id {
			if a.Node != r.S.Node.Hostname {
				return userErr(fmt.Sprintf("container %s runs on node %s", id, a.Node), "Run this there")
			}
			return nil
		}
	}
	conf, _ := r.pct(ctx, "config", id)
	for line := range strings.SplitSeq(conf, "\n") {
		if tags, ok := strings.CutPrefix(line, "tags:"); ok {
			if slices.Contains(strings.FieldsFunc(tags, func(c rune) bool { return c == ';' || c == ' ' }), "kubehenge") {
				return nil
			}
		}
	}
	return userErr(fmt.Sprintf("container %s is not Kubehenge's appliance", id), "")
}

func (r *Runner) startIfStopped(ctx context.Context) error {
	if out, _ := r.pct(ctx, "status", r.S.Cfg.CTID); !strings.Contains(out, "running") {
		if _, err := r.pct(ctx, "start", r.S.Cfg.CTID); err != nil {
			return userErr("container "+r.S.Cfg.CTID+" did not start", "See "+r.LogPath+" for Proxmox VE's message")
		}
	}
	return nil
}

func (r *Runner) say(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format+"\n", args...)
}

func (r *Runner) envValue(ctx context.Context, key string) string {
	out, _ := r.inCT(ctx, "cat", "/etc/kubehenge/appliance.env")
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (r *Runner) currentCertificate(ctx context.Context) (*x509.Certificate, []byte) {
	out, err := r.inCT(ctx, "cat", "/etc/kubehenge/tls.crt")
	if err != nil {
		return nil, nil
	}
	c, err := parseCertFile([]byte(out))
	if err != nil {
		return nil, nil
	}
	return c, []byte(out)
}

// signedHere reports whether this cluster's CA signed the certificate.
func (r *Runner) signedHere(c *x509.Certificate) bool {
	data, err := r.S.Node.readFile(clusterCA)
	if err != nil {
		return false
	}
	_, err = c.Verify(x509.VerifyOptions{Roots: poolOf(data), CurrentTime: c.NotBefore.Add(time.Hour), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err == nil
}

// madeHere reports whether the installer made the certificate: signed by this cluster's CA or by itself.
func (r *Runner) madeHere(c *x509.Certificate) bool {
	return r.signedHere(c) || bytes.Equal(c.RawIssuer, c.RawSubject)
}

var hostLabelRE = regexp.MustCompile(`^[0-9A-Za-z.-]+$`)

// publicName returns the DNS name an installed appliance is reached by, empty when it is its
// address. A certificate this cluster's CA signed carries the name the administrator gave on this
// node, beside the host name. A name from anywhere else comes from inside the container, whose root
// can write any, so it is taken only where this node's DNS resolves it to the appliance's address.
func (r *Runner) publicName(ctx context.Context, c *x509.Certificate, address string, out io.Writer) string {
	if c != nil && r.signedHere(c) {
		dns, _ := certNames(c)
		if len(dns) > 1 {
			return dns[1]
		}
		return ""
	}
	host := strings.TrimPrefix(r.envValue(ctx, "KUBEHENGE_PUBLIC_URL"), "https://")
	if host == "" || strings.HasPrefix(host, "[") || dottedRE.MatchString(host) || !hostLabelRE.MatchString(host) {
		return ""
	}
	if addrs, err := r.S.Node.LookupHost(ctx, host); err == nil {
		if slices.Contains(addrs, address) {
			return host
		}
	}
	r.say(out, "kubehenge: leaving %s out of the certificate: this node's DNS does not resolve it to %s; give it with --name to keep it", host, address)
	return ""
}

var dottedRE = regexp.MustCompile(`^[0-9.]+$`)

// installCertificate puts the staged certificate into an installed appliance and points the
// interface and Headlamp at the address users reach them by. Both read them when they start.
func (r *Runner) installCertificate(ctx context.Context, cert *Certificate, public string) error {
	host := urlHost(public)
	if err := r.pushData(ctx, "tls.crt", cert.CertPEM, "/etc/kubehenge/tls.crt", "0644"); err != nil {
		return err
	}
	if err := r.pushData(ctx, "tls.key", cert.KeyPEM, "/etc/kubehenge/tls.key", "0600"); err != nil {
		return err
	}
	script := `
set -e
chown kubehenge:kubehenge /etc/kubehenge/tls.key
env=/etc/kubehenge/appliance.env
{ grep -v "^KUBEHENGE_PUBLIC_URL=" "$env" || true; echo "KUBEHENGE_PUBLIC_URL=https://$1"; } > "$env.new"
chmod 0644 "$env.new" && mv "$env.new" "$env"
echo "HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://$1:8443/oidc-callback" > /etc/kubehenge/headlamp.env
`
	_, err := r.inCT(ctx, "sh", "-c", script, "sh", host)
	return err
}

// applianceAddresses returns the container's addresses for the certificate, IPv4 first.
func (r *Runner) applianceAddresses(ctx context.Context) []string {
	v4, v6 := r.addresses(ctx)
	var list []string
	for _, a := range []string{v4, v6} {
		if a != "" {
			list = append(list, a)
		}
	}
	return list
}

// ReplaceCertificate replaces the certificate of an installed appliance.
func (r *Runner) ReplaceCertificate(ctx context.Context, out io.Writer) (*Result, error) {
	c := &r.S.Cfg
	r.initDefaults()
	if err := r.requireAppliance(ctx); err != nil {
		return nil, err
	}
	if err := r.startIfStopped(ctx); err != nil {
		return nil, err
	}
	addrs := r.applianceAddresses(ctx)
	if len(addrs) == 0 {
		return nil, userErr("container "+c.CTID+" has no IPv4 or IPv6 address", "")
	}
	name := c.Name
	if name == "" && !c.NoName {
		current, _ := r.currentCertificate(ctx)
		name = r.publicName(ctx, current, addrs[0], out)
	}
	public := name
	if public == "" {
		public = addrs[0]
	}
	var cert *Certificate
	var err error
	if c.CertFile != "" {
		certPEM, rerr := os.ReadFile(c.CertFile)
		keyPEM, kerr := os.ReadFile(c.KeyFile)
		if rerr != nil || kerr != nil {
			return nil, userErr("--cert and --key name a readable PEM certificate and its key", "")
		}
		cert, err = OwnCertificate(certPEM, keyPEM, c.CertFile, c.KeyFile, public, r.Now())
	} else {
		cert, err = r.S.Node.MakeCertificate(r.hostnameOf(ctx), addrs, name, r.Now())
	}
	if err != nil {
		return nil, err
	}
	r.say(out, "ok   Certificate for %s", public)
	if err := r.installCertificate(ctx, cert, public); err != nil {
		return nil, userErr("copying the certificate into container "+c.CTID+" failed", "See "+r.LogPath)
	}
	// Not while the management cluster is applied or removed: the restart would cut the run off.
	_, err = r.inCT(ctx, "flock", "-n", "-E", "75", "/var/lib/kubehenge", "sh", "-c", "systemctl restart kubehenge.service && systemctl stop kubehenge-headlamp.service")
	var ce *CmdError
	switch {
	case errors.As(err, &ce) && ce.Code == exitBusy:
		return nil, userErr("the certificate is in place but the interface is busy applying or removing the management cluster", "Run this again once that has finished")
	case err != nil:
		return nil, userErr("restarting the interface failed", "See: pct exec "+c.CTID+" -- journalctl -u kubehenge")
	}
	r.updateEntry(ctx, "https://"+urlHost(public), out)
	return &Result{CTID: c.CTID, URL: "https://" + urlHost(public) + "/", Fingerprint: cert.Fingerprint, ValidUntil: cert.NotAfter, LogPath: r.LogPath}, nil
}

func (r *Runner) initDefaults() {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Sleep == nil {
		r.Sleep = time.Sleep
	}
	if r.done == nil {
		r.done = map[string]bool{}
	}
}

func (r *Runner) hostnameOf(ctx context.Context) string {
	conf, _ := r.pct(ctx, "config", r.S.Cfg.CTID)
	for line := range strings.SplitSeq(conf, "\n") {
		if h, ok := strings.CutPrefix(line, "hostname: "); ok {
			return strings.TrimSpace(h)
		}
	}
	return DefaultHostname
}

// updateEntry points this node's Kubernetes entry at the appliance's address when it leads elsewhere.
func (r *Runner) updateEntry(ctx context.Context, url string, out io.Writer) {
	conf, err := r.S.Node.readFile("/etc/kubehenge-pve-entry.conf")
	if err != nil {
		return
	}
	if _, err := os.Stat(r.S.Node.path("/usr/local/sbin/kubehenge-pve-entry")); err != nil {
		return
	}
	current := ""
	for line := range strings.SplitSeq(string(conf), "\n") {
		if v, ok := strings.CutPrefix(line, "url="); ok {
			current = v
		}
	}
	if current == url {
		return
	}
	if _, err := r.Exec.Run(ctx, "/usr/local/sbin/kubehenge-pve-entry", "--url", url); err != nil {
		r.say(out, "kubehenge: the entry still leads to %s; see %s", current, r.LogPath)
		return
	}
	r.say(out, "On every other node with the entry, run: kubehenge-pve-entry --url %s", url)
}

// stageSoftware pushes the binaries, the module and the service units into the container beside
// their places. The upgrade script, or the swap on install, renames them over the old ones.
func (r *Runner) stageSoftware(ctx context.Context, pushTofu bool) error {
	if _, err := r.inCT(ctx, "sh", "-c", "mkdir -p /etc/kubehenge /usr/local/share/kubehenge /usr/local/share/ca-certificates"); err != nil {
		return err
	}
	share := filepath.Join(r.Dir, "share", "kubehenge")
	shareTar := filepath.Join(r.Work, "share.tar")
	if err := tarDir(share, shareTar); err != nil {
		return err
	}
	if err := r.push(ctx, filepath.Join(r.Dir, "bin", "kubehenge"), "/usr/local/bin/kubehenge.new", "0755"); err != nil {
		return err
	}
	if pushTofu {
		if err := r.push(ctx, filepath.Join(r.Work, "tofu", "tofu"), "/usr/local/bin/tofu.new", "0755"); err != nil {
			return err
		}
	}
	if err := r.push(ctx, shareTar, "/tmp/share.tar", ""); err != nil {
		return err
	}
	for unit := range strings.FieldsSeq(units) {
		if err := r.push(ctx, filepath.Join(share, "appliance", unit), "/etc/systemd/system/"+unit+".new", "0644"); err != nil {
			return err
		}
	}
	return nil
}

// Upgrade replaces the software of an installed appliance with a release no older, keeps its
// state, and stops if the management cluster is being applied.
func (r *Runner) Upgrade(ctx context.Context, out io.Writer) (*Result, error) {
	c := &r.S.Cfg
	r.initDefaults()
	if err := r.unpackRelease(); err != nil {
		return nil, err
	}
	if err := r.requireAppliance(ctx); err != nil {
		return nil, err
	}
	r.say(out, "== upgrading container %s to Kubehenge %s", c.CTID, r.version)
	if err := r.startIfStopped(ctx); err != nil {
		return nil, err
	}
	installedOut, _ := r.inCT(ctx, "cat", "/usr/local/share/kubehenge/VERSION")
	installed := strings.TrimSpace(installedOut)
	if installed != "" && versionLess(r.version, installed) {
		return nil, userErr(fmt.Sprintf("container %s runs Kubehenge %s, newer than %s", c.CTID, installed, r.version), "")
	}
	if err := r.needUpgradeSpace(ctx); err != nil {
		return nil, err
	}
	// The appliance's own upgrades may have brought a newer OpenTofu, and an older one cannot
	// read the state a newer one wrote.
	pushTofu := true
	if v, _ := r.inCT(ctx, "/usr/local/bin/tofu", "version"); strings.HasPrefix(v, "OpenTofu v") {
		have := strings.TrimPrefix(strings.SplitN(v, "\n", firstLineParts)[0], "OpenTofu v")
		if versionLess(r.TofuV, have) {
			r.say(out, "keeping OpenTofu %s, newer than the installer's %s", have, r.TofuV)
			pushTofu = false
		}
	}
	if pushTofu {
		if err := r.stepTofu(ctx); err != nil {
			return nil, err
		}
	}
	// --proxmox-url moves the appliance to another address of the API, such as once the node it
	// named is gone.
	currentURL := r.envValue(ctx, "KUBEHENGE_PROXMOX_URL")
	newURL := ""
	if c.ProxmoxURL != "" && c.ProxmoxURL != currentURL {
		newURL = c.ProxmoxURL
	}
	if c.ProxmoxURL == "" {
		c.ProxmoxURL = currentURL
	}
	insecure, newCA := "", false
	var ca *CAChoice
	var err error
	switch {
	case c.Insecure:
		insecure = "true"
	case c.ProxmoxCA != "":
		insecure, newCA = "false", true
	case newURL != "" && r.envValue(ctx, "KUBEHENGE_PROXMOX_INSECURE") != "true":
		newCA = true
	}
	if newCA {
		if ca, err = r.S.Node.DecideCA(ctx, c, c.ProxmoxURL); err != nil {
			return nil, err
		}
		if ca.NeedsTrust && !c.TrustServedCA {
			return nil, userErr(fmt.Sprintf("the Proxmox VE API at %s serves a certificate neither this cluster's CA nor this node's trust store vouches for, resting on %s with the SHA-256 fingerprint %s",
				c.ProxmoxURL, ca.Subject, ca.Fingerprint), "Once you have checked that this is your CA, give it with --proxmox-ca <file>")
		}
	}
	if newURL != "" {
		host, port, _ := splitAPI(newURL)
		if !isAddress(host) {
			if _, err := r.inCT(ctx, "getent", "hosts", host); err != nil {
				return nil, userErr("the container cannot resolve any host the Proxmox VE API could be reached at (tried "+host+")",
					"Give an address its certificate is valid for with --proxmox-url, or name servers the container reaches with --dns")
			}
		}
		if _, err := r.inCT(ctx, "timeout", "5", "bash", "-c", `exec 3<>"/dev/tcp/$1/$2"`, "bash", host, port); err != nil {
			return nil, userErr(fmt.Sprintf("container %s cannot connect to the Proxmox VE API at %s on port %s", c.CTID, host, port),
				"Check the node's firewall for that port and pveproxy's LISTEN_IP in /etc/default/pveproxy")
		}
	}
	if conf, _ := r.pct(ctx, "config", c.CTID); !regexp.MustCompile(`(?m)^mp[0-9]+: .*mp=` + regexp.QuoteMeta(keyDir) + `(,|$)`).MatchString(conf) {
		r.say(out, "== a volume for the key of automatic upgrades, left out of backups")
		free := 0
		for strings.Contains(conf, fmt.Sprintf("\nmp%d: ", free)) || strings.HasPrefix(conf, fmt.Sprintf("mp%d: ", free)) {
			free++
		}
		storage := ""
		if m := regexp.MustCompile(`(?m)^rootfs: ([^:]*):`).FindStringSubmatch(conf); m != nil {
			storage = m[1]
		}
		if _, err := r.pct(ctx, "set", c.CTID, fmt.Sprintf("--mp%d", free), storage+":1,mp="+keyDir+",backup=0"); err != nil {
			return nil, userErr("adding the key volume to container "+c.CTID+" failed", "See "+r.LogPath)
		}
	}
	if err := r.stageSoftware(ctx, pushTofu); err != nil {
		return nil, userErr("copying the software into container "+c.CTID+" failed", "See "+r.LogPath+" for the command's message")
	}
	if newCA {
		if err := r.pushData(ctx, "proxmox-ca.pem", ca.PEM, "/etc/kubehenge/proxmox-ca.pem.new", "0644"); err != nil {
			return nil, err
		}
		if err := r.pushData(ctx, "proxmox-ca.crt", ca.PEM, "/usr/local/share/ca-certificates/proxmox-ca.crt.new", "0644"); err != nil {
			return nil, err
		}
	}
	// A certificate the installer made is renewed once it has less than 60 days left. The restart
	// below puts it into use.
	if current, _ := r.currentCertificate(ctx); current != nil && current.NotAfter.Before(r.Now().Add(certRenewBefore)) {
		if err := r.renewCertificate(ctx, current, out); err != nil {
			return nil, err
		}
	}
	r.say(out, "== starting Kubehenge %s, and checking that it serves", r.version)
	proxies := strings.Join(c.TrustedProxies, ",")
	if c.NoTrustedProxy {
		proxies = optNone
	}
	reason, err := r.inCT(ctx, "flock", "-n", "-E", "75", "/var/lib/kubehenge", "sh", "-c", upgradeScript, "sh", "", c.CTID, insecure, units, newURL, proxies)
	reason = strings.TrimSpace(reason)
	before := installed
	if before == "" {
		before = "the release installed before"
	}
	if err != nil {
		code := -1
		if ce, ok := errors.AsType[*CmdError](err); ok {
			code = ce.Code
		}
		cleanup := "rm -rf /usr/local/bin/kubehenge.new /usr/local/bin/tofu.new /tmp/share.tar /etc/systemd/system/*.new /etc/kubehenge/proxmox-ca.pem.new /usr/local/share/ca-certificates/proxmox-ca.crt.new"
		switch code {
		case exitBusy:
			_, _ = r.inCT(ctx, "sh", "-c", cleanup)
			return nil, userErr("the management cluster is being applied or removed, or the appliance upgraded, in container "+c.CTID, "Run this again once that has finished")
		case exitCutOff:
			_, _ = r.inCT(ctx, "sh", "-c", cleanup)
			return nil, userErr(reason, "Undo it with: pct exec "+c.CTID+" -- systemctl restart kubehenge.service. Then run this again")
		case exitRolledBack:
			return nil, userErr(fmt.Sprintf("release %s did not start: %s", r.version, reason), "Rolled back to "+before)
		case exitStuck:
			return nil, userErr(fmt.Sprintf("release %s did not start: %s. Rolling back to %s failed too", r.version, reason, before), "See: pct exec "+c.CTID+" -- journalctl -u kubehenge")
		default:
			return nil, userErr("upgrading container "+c.CTID+" failed: "+reason, "")
		}
	}
	r.address, r.address6 = r.addresses(ctx)
	_ = r.stepUpdates(ctx)
	r.say(out, "\nKubehenge %s is running in container %s. Apply the management cluster again from the interface to upgrade it too.", r.version, c.CTID)
	if newURL != "" {
		r.say(out, "The appliance reaches the Proxmox VE API at %s. The management cluster and the clusters do from its next apply.", newURL)
	}
	return &Result{CTID: c.CTID, Version: r.version, LogPath: r.LogPath, Warnings: r.warnings}, nil
}

func (r *Runner) renewCertificate(ctx context.Context, current *x509.Certificate, out io.Writer) error {
	if !r.madeHere(current) {
		r.say(out, "kubehenge: the appliance's own certificate is valid until %s. Install a new one with: kubehenge install --certificate --ctid %s --cert <file> --key <file>",
			current.NotAfter.UTC().Format(time.RFC1123), r.S.Cfg.CTID)
		return nil
	}
	r.say(out, "== renewing the certificate, valid until %s", current.NotAfter.UTC().Format(time.RFC1123))
	addrs := r.applianceAddresses(ctx)
	if len(addrs) == 0 {
		return userErr("container "+r.S.Cfg.CTID+" has no IPv4 or IPv6 address", "")
	}
	name := r.publicName(ctx, current, addrs[0], out)
	cert, err := r.S.Node.MakeCertificate(r.hostnameOf(ctx), addrs, name, r.Now())
	if err != nil {
		return err
	}
	public := name
	if public == "" {
		public = addrs[0]
	}
	return r.installCertificate(ctx, cert, public)
}

// needUpgradeSpace stops unless the container has room for an upgrade. It puts the new release
// beside the installed one and keeps that aside until the new one serves, so it needs about three
// times what is installed, and 64 MiB to spare.
func (r *Runner) needUpgradeSpace(ctx context.Context) error {
	df, _ := r.inCT(ctx, "df", "-Pk", "/usr/local/share")
	du, _ := r.inCT(ctx, "du", "-sk", "/usr/local/share/kubehenge", "/usr/local/bin/kubehenge", "/usr/local/bin/tofu")
	var room, installed int64
	if lines := strings.Split(strings.TrimSpace(df), "\n"); len(lines) > 1 {
		if f := strings.Fields(lines[1]); len(f) > dfAvailable {
			_, _ = fmt.Sscan(f[dfAvailable], &room)
		}
	}
	for line := range strings.SplitSeq(du, "\n") {
		var n int64
		if _, err := fmt.Sscan(line, &n); err == nil {
			installed += n
		}
	}
	want := upgradeFactor*installed + upgradeSlackKiB
	if room != 0 && room < want {
		return userErr(fmt.Sprintf("container %s has %d MiB free and the upgrade needs %d MiB", r.S.Cfg.CTID, room/kibPerMiB, want/kibPerMiB),
			fmt.Sprintf("Free space in it, or grow its disk with: pct resize %s rootfs +4G", r.S.Cfg.CTID))
	}
	return nil
}
