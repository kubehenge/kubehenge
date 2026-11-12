package installer

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// StepState is where a step of the run stands.
type StepState int

// The states of a step.
const (
	StepPending StepState = iota
	StepRunning
	StepDone
	StepFailed
	StepSkipped
)

// Event reports a step's progress.
type Event struct {
	Step   string
	State  StepState
	Detail string
	// Done and Total are bytes of a download in progress, Total 0 when unknown.
	Done, Total int64
}

// StepInfo names a step of the run.
type StepInfo struct{ ID, Label string }

// NoAddressError is the container getting no address, which DHCP can cause.
type NoAddressError struct {
	UserError

	DHCP bool
}

func (e *NoAddressError) Error() string { return e.UserError.Error() }

// Result is what the end screen shows.
type Result struct {
	CTID        string
	URL         string
	Address     string
	Name        string
	Fingerprint string
	ValidUntil  time.Time
	Version     string
	Warnings    []string
	LogPath     string
}

// TemplatePin names a container template, its checksum and where it is downloaded from.
type TemplatePin struct{ Name, SHA512, URL string }

// Runner makes the changes, step by step. A step that finished is not run again by a second Run,
// so a retry after a failed address skips the downloads.
type Runner struct {
	S      *Session
	Exec   Exec
	Fetch  *Fetcher
	Work   string
	Dir    string
	Emit   func(Event)
	Sleep  func(time.Duration)
	Now    func() time.Time
	Proxy  string
	TofuV  string
	TofuSH string
	// Realms returns the HTTP status of the appliance's realms list.
	Realms func(ctx context.Context, public, address string, certPEM []byte) int
	// LogPath is shown at the end.
	LogPath string
	// Pin replaces the pinned container template, for tests.
	Pin TemplatePin

	done        map[string]bool
	created     bool
	started     bool
	partial     string
	address     string
	address6    string
	template    string
	templateSHA string
	templateURL string
	cert        *Certificate
	version     string
	warnings    []string
}

const keyDir = "/var/lib/kubehenge-keys"

const units = "kubehenge.service kubehenge-update.service kubehenge-update.path kubehenge-headlamp.service kubehenge-headlamp.socket kubehenge-headlamp-proxy.service"

// Steps lists the steps of this run, in order.
func (r *Runner) Steps() []StepInfo {
	list := []StepInfo{
		{"tofu", "OpenTofu"},
		{"template", "Container template"},
		{"create", "Create container"},
		{stepStartName, "Start container"},
		{stepAddressName, "Wait for address"},
		{stepNetworkName, "Check network"},
		{stepCertificateName, "Certificate"},
		{stepInstallName, "Install software"},
		{"service", "Start Kubehenge"},
		{stepUpdatesName, "Security updates"},
	}
	if r.S.Cfg.PVEEntry == optYes {
		list = append(list, StepInfo{stepEntryName, "Proxmox VE entry"})
	}
	return list
}

// Run executes the steps that have not finished, and returns the result.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	if r.done == nil {
		r.done = map[string]bool{}
	}
	if r.Sleep == nil {
		r.Sleep = time.Sleep
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	do := map[string]func(context.Context) error{
		"tofu": r.stepTofu, "template": r.stepTemplate, "create": r.stepCreate, "start": r.stepStart,
		"address": r.stepAddress, "network": r.stepNetwork, "certificate": r.stepCertificate,
		"install": r.stepInstall, "service": r.stepService, "updates": r.stepUpdates, stepEntryName: r.stepEntry,
	}
	if err := r.unpackRelease(); err != nil {
		return nil, err
	}
	for _, st := range r.Steps() {
		if r.done[st.ID] {
			continue
		}
		r.emit(Event{Step: st.ID, State: StepRunning})
		if err := do[st.ID](ctx); err != nil {
			r.emit(Event{Step: st.ID, State: StepFailed, Detail: err.Error()})
			return nil, err
		}
		r.done[st.ID] = true
		if st.ID == "updates" {
			if err := r.Protect(ctx); err != nil {
				return nil, err
			}
		}
	}
	public := r.public()
	return &Result{
		CTID: r.S.Cfg.CTID, URL: "https://" + urlHost(public) + "/", Address: r.address, Name: r.S.Cfg.Name,
		Fingerprint: r.cert.Fingerprint, ValidUntil: r.cert.NotAfter, Version: r.version,
		Warnings: r.warnings, LogPath: r.LogPath,
	}, nil
}

func urlHost(h string) string {
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}

var (
	cfgIP4RE = regexp.MustCompile(`(?m)^net0: (?:.*,)?ip=([0-9.]+)/`)
	cfgIP6RE = regexp.MustCompile(`(?m)^net0: (?:.*,)?ip6=([0-9A-Fa-f:]+)/`)
)

var gatewayRE = regexp.MustCompile(`(?m)^default via (\S+)`)

func isAddress(s string) bool { return net.ParseIP(s) != nil }

const swapScript = `
set -e
rm -rf /usr/local/share/kubehenge.new && mkdir /usr/local/share/kubehenge.new
tar -xf /tmp/share.tar -C /usr/local/share/kubehenge.new && rm /tmp/share.tar
rm -rf /usr/local/share/kubehenge.old
mv /usr/local/share/kubehenge /usr/local/share/kubehenge.old
mv /usr/local/share/kubehenge.new /usr/local/share/kubehenge
rm -rf /usr/local/share/kubehenge.old
mv /usr/local/bin/kubehenge.new /usr/local/bin/kubehenge
[ ! -e /usr/local/bin/tofu.new ] || mv /usr/local/bin/tofu.new /usr/local/bin/tofu
for unit in ` + units + `; do mv /etc/systemd/system/$unit.new /etc/systemd/system/$unit; done
if [ -e /etc/kubehenge/proxmox-ca.pem.new ]; then
    mv /etc/kubehenge/proxmox-ca.pem.new /etc/kubehenge/proxmox-ca.pem
    mv /usr/local/share/ca-certificates/proxmox-ca.crt.new /usr/local/share/ca-certificates/proxmox-ca.crt
    update-ca-certificates > /dev/null
fi
systemctl daemon-reload
`

// startScript runs inside the container. The appliance is reached only through its interface and
// pct, so the template's SSH and mail servers are switched off.
const startScript = `
set -e
for unit in ssh.service ssh.socket postfix.service; do
    systemctl disable --now "$unit" >/dev/null 2>&1 || true
    systemctl mask "$unit" >/dev/null 2>&1 || true
done
id kubehenge >/dev/null 2>&1 || useradd --system --home-dir /var/lib/kubehenge --shell /usr/sbin/nologin kubehenge
install -d -o kubehenge -g kubehenge -m 0700 /var/lib/kubehenge /var/lib/kubehenge-keys
install -d -o root -g root -m 0755 /var/lib/kubehenge-update
install -d -o root -g kubehenge -m 0770 /var/lib/kubehenge-update/requests
chown kubehenge:kubehenge /etc/kubehenge/tls.key
systemctl daemon-reload
systemctl enable --now kubehenge-update.path >/dev/null 2>&1
systemctl enable --now kubehenge.service >/dev/null 2>&1
`

// realmsStatus asks the appliance for its realms over the address, trusting the certificate just
// installed. The container is on this node's network, so no proxy sees the request.
func realmsStatus(ctx context.Context, public, address string, certPEM []byte) int {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	dialer := &net.Dialer{Timeout: dialTimeout}
	client := &http.Client{Timeout: httpTimeout, Transport: &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, ServerName: public},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(address, "443"))
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+urlHost(public)+"/api/realms", http.NoBody)
	if err != nil {
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Protect makes the finished appliance undeletable until its protection is taken off.
func (r *Runner) Protect(ctx context.Context) error {
	r.created = false
	_, err := r.pct(ctx, "set", r.S.Cfg.CTID, "--protection", "1")
	return err
}

// Cleanup removes what a failed run made, so running it again starts clean. It is safe to call
// when nothing was made.
func (r *Runner) Cleanup(ctx context.Context) {
	if r.partial != "" {
		_ = os.Remove(r.partial)
		r.partial = ""
	}
	if !r.created {
		return
	}
	id := r.S.Cfg.CTID
	_, _ = r.pct(ctx, "unlock", id)
	_, _ = r.pct(ctx, "stop", id)
	_, _ = r.pct(ctx, "destroy", id, "--purge", "--destroy-unreferenced-disks", "1")
	r.created, r.started = false, false
	for _, s := range []string{"create", "start", "address", "network", "certificate", "install", "service", "updates", "entry"} {
		delete(r.done, s)
	}
}

func (r *Runner) emit(e Event) {
	if r.Emit != nil {
		r.Emit(e)
	}
}

func (r *Runner) pct(ctx context.Context, args ...string) (string, error) {
	return r.Exec.Run(ctx, "pct", args...) //nolint:wrapcheck // the caller names the step, and CmdError carries the command.
}

func (r *Runner) inCT(ctx context.Context, args ...string) (string, error) {
	return r.Exec.Run(ctx, "pct", append([]string{"exec", r.S.Cfg.CTID, "--"}, args...)...) //nolint:wrapcheck // the caller names the step, and CmdError carries the command.
}

func (r *Runner) public() string {
	if r.S.Cfg.Name != "" {
		return r.S.Cfg.Name
	}
	if r.address != "" {
		return r.address
	}
	return r.address6
}

func (r *Runner) finish(step, detail string) {
	r.emit(Event{Step: step, State: StepDone, Detail: detail})
}

func (r *Runner) progress(step string) func(done, total int64) {
	return func(done, total int64) { r.emit(Event{Step: step, State: StepRunning, Done: done, Total: total}) }
}

func (r *Runner) unpackRelease() error {
	data, err := os.ReadFile(filepath.Join(r.Dir, "share", "kubehenge", "VERSION"))
	if err != nil {
		return userErr("the release in "+r.Dir+" holds no share/kubehenge directory", "Run the installer through install.sh")
	}
	r.version = strings.Map(func(c rune) rune {
		if strings.ContainsRune("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz.+-", c) {
			return c
		}
		return -1
	}, string(data))
	return nil
}

func (r *Runner) stepTofu(ctx context.Context) error {
	if r.TofuV == "" || r.TofuSH == "" {
		return userErr("this run carries no OpenTofu checksum", "Run the installer through install.sh")
	}
	archive := filepath.Join(r.Work, "tofu.tar.gz")
	if r.S.Cfg.Tofu != "" {
		if err := copyFile(r.S.Cfg.Tofu, archive); err != nil {
			return err
		}
		if err := checkSum(archive, r.TofuSH, r.S.Cfg.Tofu); err != nil {
			return err
		}
	} else {
		from := fmt.Sprintf("https://github.com/opentofu/opentofu/releases/download/v%s/tofu_%s_linux_amd64.tar.gz", r.TofuV, r.TofuV)
		if err := r.Fetch.Fetch(ctx, from, archive, r.TofuSH, HTTPSOnly, r.progress("tofu")); err != nil {
			return err
		}
	}
	if err := extractOne(archive, "tofu", filepath.Join(r.Work, "tofu")); err != nil {
		return err
	}
	r.finish("tofu", "OpenTofu "+r.TofuV+", checksum verified")
	return nil
}

func (r *Runner) templatePath(ctx context.Context) (string, error) {
	out, err := r.Exec.Run(ctx, "pvesm", "path", r.S.Cfg.TemplateStorage+":vztmpl/"+r.template)
	if err != nil {
		return "", userErr("storage "+r.S.Cfg.TemplateStorage+" gives no path for the container template", "Name another with --template-storage")
	}
	return strings.TrimSpace(out), nil
}

func (r *Runner) stepTemplate(ctx context.Context) error {
	r.template, r.templateSHA, r.templateURL = Template, TemplateSHA512, TemplateMirror+"/system/"+Template
	if r.Pin.Name != "" {
		r.template, r.templateSHA, r.templateURL = r.Pin.Name, r.Pin.SHA512, r.Pin.URL
	}
	file, err := r.templatePath(ctx)
	if err != nil {
		return err
	}
	there := func() bool { return checkSum(file, r.templateSHA, "") == nil }
	if !there() && r.Fetch.TemplateGone(ctx, r.templateURL) {
		indexed, err := r.Fetch.TemplateFromIndex(ctx, r.Exec, r.Work, TemplateMirror)
		if err != nil {
			return err
		}
		r.template, r.templateSHA, r.templateURL = indexed.File, indexed.SHA512, TemplateMirror+"/"+indexed.Location
		if file, err = r.templatePath(ctx); err != nil {
			return err
		}
	}
	if there() {
		r.finish("template", r.template+", already there")
		return nil
	}
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, permExec); err != nil {
		return fmt.Errorf("mkdir all: %w", err)
	}
	if err := r.needSpace(ctx, dir, templateKiB, "the container template", "Free space on storage "+r.S.Cfg.TemplateStorage+", or name another with --template-storage"); err != nil {
		return err
	}
	// Downloaded beside its place and moved there once checked, so no half or wrong file is left as the template.
	r.partial = file + ".part"
	if err := r.Fetch.Fetch(ctx, r.templateURL, r.partial, r.templateSHA, HTTPOrHTTPS, r.progress("template")); err != nil {
		return err
	}
	if err := os.Rename(r.partial, file); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	r.partial = ""
	r.finish("template", r.template+", checksum verified")
	return nil
}

func (r *Runner) needSpace(ctx context.Context, dir string, kib int64, what, fix string) error {
	out, err := r.Exec.Run(ctx, "df", "-Pk", dir)
	if err != nil {
		return nil //nolint:nilerr // without a df reading there is nothing to check the space against.
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < dfMinLines {
		return nil
	}
	f := strings.Fields(lines[1])
	if len(f) < dfFields {
		return nil
	}
	if free, err := strconv.ParseInt(f[3], 10, 64); err == nil && free < kib {
		return userErr(fmt.Sprintf("%s has %d MiB free and %s needs %d MiB", dir, free/kibPerMiB, what, kib/kibPerMiB), fix)
	}
	return nil
}

func (r *Runner) stepCreate(ctx context.Context) error {
	c := &r.S.Cfg
	if !r.S.Node.IDFree(ctx, c.CTID) {
		return userErr(fmt.Sprintf("id %s was taken by another guest while the installer ran", c.CTID), "Run it again, or name a free id with --ctid")
	}
	r.created = true
	args := []string{
		"create", c.CTID, c.TemplateStorage + ":vztmpl/" + r.template,
		"--hostname", c.Hostname, "--cores", "2", "--memory", "1024", "--swap", "512",
		"--rootfs", c.Storage + ":8", "--mp0", c.Storage + ":1,mp=" + keyDir + ",backup=0",
		"--net0", c.containerNet(),
	}
	if c.DNS != "" {
		args = append(args, "--nameserver", strings.ReplaceAll(c.DNS, ",", " "))
	}
	args = append(args, "--unprivileged", "1", "--features", "nesting=1", "--onboot", "1", "--tags", "kubehenge",
		"--description", "Kubehenge's appliance: its web interface and the management cluster's state. Include it in backups.")
	if _, err := r.pct(ctx, args...); err != nil {
		return userErr("Proxmox VE could not create container "+c.CTID, "See "+r.LogPath+" for its message")
	}
	r.finish("create", "container "+c.CTID)
	return nil
}

func (r *Runner) stepStart(ctx context.Context) error {
	if _, err := r.pct(ctx, "start", r.S.Cfg.CTID); err != nil {
		return userErr("container "+r.S.Cfg.CTID+" did not start", "See "+r.LogPath+" for Proxmox VE's message")
	}
	r.finish("start", "")
	return nil
}

// addresses returns the container's IPv4 and global IPv6 addresses: those its configuration gives,
// else those DHCP, SLAAC or DHCPv6 gave it, as it reports them.
func (r *Runner) addresses(ctx context.Context) (string, string) {
	var v4, v6 string
	conf, _ := r.pct(ctx, "config", r.S.Cfg.CTID)
	if m := cfgIP4RE.FindStringSubmatch(conf); m != nil {
		v4 = m[1]
	} else if out, err := r.inCT(ctx, "ip", "-4", "-o", "addr", "show", "dev", "eth0"); err == nil {
		if m := regexp.MustCompile(`\binet ([0-9.]+)`).FindStringSubmatch(out); m != nil {
			v4 = m[1]
		}
	}
	if m := cfgIP6RE.FindStringSubmatch(conf); m != nil {
		v6 = m[1]
	} else if out, err := r.inCT(ctx, "ip", "-6", "-o", "addr", "show", "dev", "eth0", "scope", "global"); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			if strings.Contains(line, "temporary") || strings.Contains(line, "deprecated") || strings.Contains(line, "tentative") {
				continue
			}
			if m := regexp.MustCompile(`\binet6 ([0-9A-Fa-f:]+)/`).FindStringSubmatch(line); m != nil {
				v6 = m[1]
				break
			}
		}
	}
	return v4, v6
}

func (r *Runner) stepAddress(ctx context.Context) error {
	c := &r.S.Cfg
	for range 60 {
		r.address, r.address6 = r.addresses(ctx)
		if (r.address != "" || r.address6 != "") && (c.IP6 == "" || r.address6 != "") {
			break
		}
		r.Sleep(stepPause)
	}
	if r.address == "" && r.address6 == "" {
		if c.IP == optNone {
			return &NoAddressError{
				Cause: "container " + c.CTID + " got no IPv6 address",
				Fix:   "Check that the router advertises a prefix or a DHCPv6 server answers, or give --ip6 and --gateway6",
			}
		}
		return &NoAddressError{
			DHCP:  c.IP == optDHCP,
			Cause: "container " + c.CTID + " got no IPv4 address",
			Fix:   "Check that a DHCP server answers on this network, or give --ip <address>/<prefix> --gateway <address>",
		}
	}
	if c.IP6 != "" && r.address6 == "" {
		r.warnings = append(r.warnings, "container "+c.CTID+" has no IPv6 address yet, so its certificate names its IPv4 address only")
	}
	shown := r.address
	if shown == "" {
		shown = r.address6
	}
	r.finish("address", shown)
	return nil
}

func (r *Runner) containerGateway(ctx context.Context) string {
	for _, family := range []string{"-4", "-6"} {
		if out, err := r.inCT(ctx, "ip", family, "route", "show", "default"); err == nil {
			if m := gatewayRE.FindStringSubmatch(out); m != nil {
				gw := m[1]
				if strings.HasPrefix(gw, "fe80:") {
					gw += "%eth0"
				}
				return gw
			}
		}
	}
	return ""
}

func (r *Runner) stepNetwork(ctx context.Context) error {
	c := &r.S.Cfg
	if gw := r.containerGateway(ctx); gw != "" {
		_, err := r.inCT(ctx, "sh", "-c", `command -v ping > /dev/null || exit 127; ping -c 1 -W 3 "$1" > /dev/null`, "sh", gw)
		var ce *CmdError
		if err != nil && errors.As(err, &ce) && ce.Code != 127 {
			option := "--gateway"
			if strings.Contains(gw, ":") {
				option = "--gateway6"
			}
			return userErr(fmt.Sprintf("container %s at %s cannot reach its gateway %s", c.CTID, r.address, gw),
				"Check that the VLAN matches the network of the address, and that "+option+" is on it")
		}
	}
	hosts := r.apiHostsOrdered()
	reachable := ""
	for _, h := range hosts {
		if !isAddress(h) {
			if _, err := r.inCT(ctx, "getent", "hosts", h); err != nil {
				continue
			}
		}
		reachable = h
		break
	}
	if reachable == "" {
		return userErr("the container cannot resolve any host the Proxmox VE API could be reached at (tried "+strings.Join(hosts, ", ")+")",
			"Give an address its certificate is valid for with --proxmox-url, or name servers the container reaches with --dns")
	}
	if reachable != hosts[0] {
		r.S.notice("the container cannot resolve %s, so the appliance and the clusters reach the Proxmox VE API at %s", hosts[0], reachable)
		c.ProxmoxURL = APIURL(reachable)
	}
	_, port, _ := splitAPI(c.ProxmoxURL)
	if _, err := r.inCT(ctx, "timeout", "5", "bash", "-c", `exec 3<>"/dev/tcp/$1/$2"`, "bash", reachable, port); err != nil {
		hint := ""
		if r.containerGateway(ctx) == "" {
			hint = " The container has no default route, so give --gateway."
		}
		return userErr(fmt.Sprintf("container %s cannot connect to the Proxmox VE API at %s on port %s", c.CTID, reachable, port),
			"Check the node's firewall for that port and pveproxy's LISTEN_IP in /etc/default/pveproxy."+hint)
	}
	r.finish("network", "gateway and API reachable")
	return nil
}

// apiHostsOrdered returns the chosen API host, then the other candidates in rank order.
func (r *Runner) apiHostsOrdered() []string {
	host, _, _ := splitAPI(r.S.Cfg.ProxmoxURL)
	hosts := []string{host}
	if r.S.Cfg.APIHost != "" {
		for _, c := range r.S.Facts.Candidates {
			if c.Host != host {
				hosts = append(hosts, c.Host)
			}
		}
	}
	return hosts
}

func (r *Runner) stepCertificate(_ context.Context) error {
	c := &r.S.Cfg
	public := r.public()
	if c.CertFile != "" {
		certPEM, err := os.ReadFile(c.CertFile)
		if err != nil {
			return fmt.Errorf("read file: %w", err)
		}
		keyPEM, err := os.ReadFile(c.KeyFile)
		if err != nil {
			return fmt.Errorf("read file: %w", err)
		}
		if r.cert, err = OwnCertificate(certPEM, keyPEM, c.CertFile, c.KeyFile, public, r.Now()); err != nil {
			return err
		}
		r.finish("certificate", "yours, valid for "+public)
		return nil
	}
	var addrs []string
	for _, a := range []string{r.address, r.address6} {
		if a != "" {
			addrs = append(addrs, a)
		}
	}
	cert, err := r.S.Node.MakeCertificate(c.Hostname, addrs, c.Name, r.Now())
	if err != nil {
		return err
	}
	r.cert = cert
	r.finish("certificate", "signed for "+public)
	return nil
}

func (r *Runner) push(ctx context.Context, local, remote, perms string) error {
	args := []string{"push", r.S.Cfg.CTID, local, remote}
	if perms != "" {
		args = append(args, "--perms", perms)
	}
	_, err := r.pct(ctx, args...)
	return err
}

func (r *Runner) pushData(ctx context.Context, name string, data []byte, remote, perms string) error {
	local := filepath.Join(r.Work, name)
	if err := os.WriteFile(local, data, permFile); err != nil { //nolint:gosec // the path is in the installer's own work directory.
		return fmt.Errorf("write file: %w", err)
	}
	return r.push(ctx, local, remote, perms)
}

func (r *Runner) stepInstall(ctx context.Context) error {
	c := &r.S.Cfg
	if _, err := r.inCT(ctx, "sh", "-c", "mkdir -p /etc/kubehenge /usr/local/share/kubehenge /usr/local/share/ca-certificates"); err != nil {
		return err
	}
	share := filepath.Join(r.Dir, "share", "kubehenge")
	shareTar := filepath.Join(r.Work, "share.tar")
	if err := tarDir(share, shareTar); err != nil {
		return err
	}
	steps := []func() error{
		func() error {
			return r.push(ctx, filepath.Join(r.Dir, "bin", "kubehenge"), "/usr/local/bin/kubehenge.new", "0755")
		},
		func() error {
			return r.push(ctx, filepath.Join(r.Work, "tofu", "tofu"), "/usr/local/bin/tofu.new", "0755")
		},
		func() error { return r.push(ctx, shareTar, "/tmp/share.tar", "") },
	}
	for unit := range strings.FieldsSeq(units) {
		steps = append(steps, func() error {
			return r.push(ctx, filepath.Join(share, "appliance", unit), "/etc/systemd/system/"+unit+".new", "0644")
		})
	}
	steps = append(steps,
		func() error {
			return r.pushData(ctx, "proxmox-ca.pem", r.S.CA.PEM, "/etc/kubehenge/proxmox-ca.pem.new", "0644")
		},
		func() error {
			return r.pushData(ctx, "proxmox-ca.crt", r.S.CA.PEM, "/usr/local/share/ca-certificates/proxmox-ca.crt.new", "0644")
		},
		func() error { _, err := r.inCT(ctx, "sh", "-c", swapScript); return err },
		func() error { return r.pushData(ctx, "tls.crt", r.cert.CertPEM, "/etc/kubehenge/tls.crt", "0644") },
		func() error { return r.pushData(ctx, "tls.key", r.cert.KeyPEM, "/etc/kubehenge/tls.key", "0600") },
	)
	for _, step := range steps {
		if err := step(); err != nil {
			return userErr("copying the software into container "+c.CTID+" failed", "See "+r.LogPath+" for the command's message")
		}
	}
	public := urlHost(r.public())
	env := fmt.Sprintf("KUBEHENGE_PROXMOX_URL=%s\nKUBEHENGE_PUBLIC_URL=https://%s\nKUBEHENGE_APPLIANCE_VMID=%s\n", c.ProxmoxURL, public, c.CTID)
	if c.Insecure {
		env += "KUBEHENGE_PROXMOX_INSECURE=true\n"
	}
	if len(c.TrustedProxies) > 0 {
		env += "KUBEHENGE_TRUSTED_PROXIES=" + strings.Join(c.TrustedProxies, ",") + "\n"
	}
	if err := r.pushData(ctx, "appliance.env", []byte(env), "/etc/kubehenge/appliance.env", "0644"); err != nil {
		return err
	}
	headlamp := "HEADLAMP_CONFIG_OIDC_CALLBACK_URL=https://" + public + ":8443/oidc-callback\n"
	if err := r.pushData(ctx, "headlamp.env", []byte(headlamp), "/etc/kubehenge/headlamp.env", "0644"); err != nil {
		return err
	}
	r.finish("install", "Kubehenge "+r.version)
	return nil
}

func (r *Runner) stepService(ctx context.Context) error {
	c := &r.S.Cfg
	if _, err := r.inCT(ctx, "sh", "-c", startScript); err != nil {
		return r.serviceFailure(ctx, "the service did not start")
	}
	r.started = true
	r.Sleep(stepPause)
	if _, err := r.inCT(ctx, "systemctl", "is-active", "--quiet", "kubehenge.service"); err != nil {
		return r.serviceFailure(ctx, "the service did not start")
	}
	// The realms are what the interface asks Proxmox VE before anyone signs in: an answer shows
	// that it reaches the API and trusts its certificate.
	status := 0
	for i := 0; i < 10 && status != http.StatusOK; i++ {
		if status = r.realms(ctx); status != http.StatusOK {
			r.Sleep(stepPause)
		}
	}
	if status != http.StatusOK {
		return r.serviceFailure(ctx, "the appliance cannot use Proxmox VE's API at "+c.ProxmoxURL)
	}
	r.finish("service", "reaches the Proxmox VE API")
	return nil
}

func (r *Runner) serviceFailure(ctx context.Context, cause string) error {
	_, _ = r.inCT(ctx, "journalctl", "-u", "kubehenge", "--no-pager", "-n", "60")
	return userErr(cause, "Its log is in "+r.LogPath+". Give a URL it reaches with --proxmox-url, whose certificate names it")
}

func (r *Runner) realms(ctx context.Context) int {
	if r.Realms != nil {
		return r.Realms(ctx, r.public(), r.address, r.cert.CertPEM)
	}
	return realmsStatus(ctx, r.public(), r.addressOrV6(), r.cert.CertPEM)
}

func (r *Runner) addressOrV6() string {
	if r.address != "" {
		return r.address
	}
	return r.address6
}

func (r *Runner) stepUpdates(ctx context.Context) error {
	if out, err := r.inCT(ctx, "dpkg-query", "-W", "-f", "${Status}", "unattended-upgrades"); err == nil && strings.Contains(out, "ok installed") {
		r.finish("updates", "already on")
		return nil
	}
	if proxy := r.Proxy; proxy != "" {
		conf := fmt.Sprintf("Acquire::http::Proxy %q;\nAcquire::https::Proxy %q;\n", proxy, proxy)
		_ = r.pushData(ctx, "apt-proxy.conf", []byte(conf), "/etc/apt/apt.conf.d/80kubehenge-proxy", "0600")
	}
	_, err := r.inCT(ctx, "sh", "-c", "apt-get -q update && DEBIAN_FRONTEND=noninteractive apt-get -q -y install unattended-upgrades")
	if err != nil {
		r.warnings = append(r.warnings, "container "+r.S.Cfg.CTID+" could not install unattended-upgrades from Debian's mirrors ("+err.Error()+"). Update it with: pct exec "+r.S.Cfg.CTID+" -- sh -c 'apt-get update && apt-get -y upgrade'")
		r.emit(Event{Step: stepUpdatesName, State: StepSkipped, Detail: "not installed"})
		return nil
	}
	_ = r.pushData(ctx, "auto-upgrades.conf", []byte("APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"), "/etc/apt/apt.conf.d/20auto-upgrades", "0644")
	r.finish("updates", "daily")
	return nil
}

func (r *Runner) stepEntry(ctx context.Context) error {
	// From here the appliance works, and nothing after this removes it. It holds the only copy of
	// the management cluster's state, so Proxmox VE refuses to destroy it until its protection is off.
	if r.S.Cfg.PVEEntry == optYes {
		script := filepath.Join(r.Dir, "share", "kubehenge", "pve-entry", "install.sh")
		if _, err := r.Exec.Run(ctx, "sh", script, "--url", "https://"+urlHost(r.public())); err != nil {
			r.warnings = append(r.warnings, "the appliance is installed, but adding the entry to this node's Proxmox VE interface failed ("+err.Error()+"). See "+r.LogPath)
			r.emit(Event{Step: stepEntryName, State: StepSkipped, Detail: "failed"})
			return nil
		}
	}
	r.finish("entry", "")
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from) //nolint:gosec // the path is in the installer's own work directory.
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to) //nolint:gosec // the path is in the installer's own work directory.
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer func() { _ = out.Close() }()
	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// extractOne unpacks the named file of a .tar.gz into dir.
func extractOne(archive, name, dir string) error {
	f, err := os.Open(archive) //nolint:gosec // the path is in the installer's own work directory.
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("new reader: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s holds no %s", archive, name)
		}
		if err != nil {
			return fmt.Errorf("next: %w", err)
		}
		if filepath.Base(h.Name) != name || h.Typeflag != tar.TypeReg {
			continue
		}
		return writePrivateExecutable(filepath.Join(dir, name), tr)
	}
}

// writePrivateExecutable writes src to path, creating its directory, readable by its owner alone.
func writePrivateExecutable(path string, src io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), permDir); err != nil {
		return fmt.Errorf("mkdir all: %w", err)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, permDir) //nolint:gosec // the file is the OpenTofu binary the installer pushes.
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = out.Close() }()
	if _, err = io.Copy(out, src); err != nil { //nolint:gosec // the archive is checked against a pinned checksum.
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// tarDir writes the directory's tree into a tar file, as the container unpacks it.
func tarDir(dir, dest string) error {
	out, err := os.Create(dest) //nolint:gosec // the path is in the installer's own work directory.
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer func() { _ = out.Close() }()
	tw := tar.NewWriter(out)
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel: %w", err)
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("file info header: %w", err)
		}
		h.Name = filepath.ToSlash(rel)
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if werr := tw.WriteHeader(h); werr != nil {
			return fmt.Errorf("write header: %w", werr)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path) //nolint:gosec // the path is in the installer's own work directory.
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		defer func() { _ = in.Close() }()
		if _, err = io.Copy(tw, in); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	return nil
}
