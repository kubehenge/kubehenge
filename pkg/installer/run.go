package installer

import (
	"archive/tar"
	"cmp"
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
	"sync"
	"time"

	"github.com/kubehenge/kubehenge/pkg/appliance"
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
	CTID    string
	URL     string
	Address string
	Name    string
	// AddressURL is the appliance's address as a URL, set where the DNS name does not lead to it yet.
	AddressURL  string
	Fingerprint string
	ValidUntil  time.Time
	// MadeHere tells that the certificate served is the one the installer made, which browsers
	// warn about until they trust the Proxmox VE CA.
	MadeHere bool
	Version  string
	Warnings []string
	LogPath  string
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

	// mu guards created, unprotected and what Cleanup resets, since a failed run's own cleanup and
	// the shutdown after a signal can run at once.
	mu          sync.Mutex
	unprotected bool
	done        map[string]bool
	created     bool
	// ipv4Only says the container's IPv6 was switched off at its start.
	ipv4Only bool
	// removed and stuck say how Cleanup's removal of the container went, fetched that this run
	// downloaded the template, which stays for the next run.
	removed     bool
	stuck       bool
	kept        bool
	fetched     bool
	started     bool
	partial     string
	address     string
	address6    string
	template    string
	templateSHA string
	templateURL string
	cert        *Certificate
	// acmeLeaf is the certificate the first ACME order issued, nil while the one made here is served.
	acmeLeaf *x509.Certificate
	version  string
	warnings []string
}

const keyDir = "/var/lib/kubehenge-keys"

// stateDir is the interface's state directory in the container.
const stateDir = "/var/lib/kubehenge"

// aptTimeouts cuts each try at a mirror an air-gapped or filtered network drops without an answer
// to 20 seconds, from apt's default of 120. Apt keeps its 3 retries with a backoff of 1, 2 and 4
// seconds, so such a mirror still costs about 90 seconds per index file.
const aptTimeouts = "-o Acquire::http::Timeout=20 -o Acquire::https::Timeout=20"

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
	}
	if r.S.Cfg.ACME.Directory != "" {
		list = append(list, StepInfo{stepACMEName, "ACME certificate"})
	}
	list = append(list, StepInfo{stepUpdatesName, "Security updates"})
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
		"install": r.stepInstall, "service": r.stepService, stepACMEName: r.stepACME, stepUpdatesName: r.stepUpdates, stepEntryName: r.stepEntry,
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
				id := r.S.Cfg.CTID
				return nil, userErr("the appliance is installed in container "+id+" but could not be protected against deletion",
					"Protect it with: pct set "+id+" --protection 1, or remove it with: pct destroy "+id+" --purge")
			}
		}
	}
	public := r.public()
	res := &Result{
		CTID: r.S.Cfg.CTID, URL: "https://" + urlHost(public) + "/", Address: r.address, Name: r.S.Cfg.Name,
		Fingerprint: r.cert.Fingerprint, ValidUntil: r.cert.NotAfter, MadeHere: r.S.Cfg.CertFile == "", Version: r.version,
		Warnings: r.warnings, LogPath: r.LogPath,
	}
	if r.acmeLeaf != nil {
		res.Fingerprint, res.ValidUntil, res.MadeHere = fingerprint(r.acmeLeaf), r.acmeLeaf.NotAfter, false
	}
	if res.Name != "" && !nameResolves(ctx, r.S.Node.LookupHost, res.Name, r.address, r.address6) {
		res.AddressURL = "https://" + urlHost(cmp.Or(r.address, r.address6)) + "/"
	}
	return res, nil
}

// nameResolves reports whether the DNS name leads to the appliance now.
func nameResolves(ctx context.Context, lookup func(context.Context, string) ([]string, error), name string, own ...string) bool {
	if lookup == nil {
		return false
	}
	addrs, err := lookup(ctx, name)
	return err == nil && anyOf(addrs, own)
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

// The link in /usr/bin is there because `pct exec` starts its command on the PATH of the caller's
// shell, which need not hold /usr/local/bin. The link follows the binary the updater replaces.
const swapScript = `
set -e
ln -sfn /usr/local/bin/kubehenge /usr/bin/kubehenge
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
	return realmsStatusAt(ctx, public, net.JoinHostPort(address, "443"), certPEM)
}

// realmsStatusAt is realmsStatus for the host and port to connect to, whatever the name in the URL.
func realmsStatusAt(ctx context.Context, public, connectTo string, certPEM []byte) int {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	dialer := &net.Dialer{Timeout: dialTimeout}
	client := &http.Client{Timeout: httpTimeout, Transport: &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, ServerName: public},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, connectTo)
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

// Protect makes the finished appliance undeletable until its protection is taken off. A finished
// appliance is no longer the run's to remove, and Left names it until the flag is set.
func (r *Runner) Protect(ctx context.Context) error {
	r.mu.Lock()
	r.created, r.unprotected = false, true
	r.mu.Unlock()
	if _, err := r.pct(ctx, "set", r.S.Cfg.CTID, "--protection", "1"); err != nil {
		return err
	}
	r.mu.Lock()
	r.unprotected = false
	r.mu.Unlock()
	return nil
}

// Made reports whether the run holds a container of its own that Cleanup would remove.
func (r *Runner) Made() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created
}

// Stuck reports whether Cleanup's last removal of the container failed.
func (r *Runner) Stuck() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stuck
}

// HoldsID reports whether a container of this run still holds its ID: kept, not yet removed, its
// removal refused, or finished but not protected.
func (r *Runner) HoldsID() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.kept || r.created || r.stuck || r.unprotected
}

// Left says what a stopped run left behind that the user must look after, empty for nothing.
func (r *Runner) Left() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unprotectedText()
}

// Outcome says what a stopped run undid and what it left, once its cleanup has run.
func (r *Runner) Outcome() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.S.Cfg.CTID
	var said string
	switch {
	case r.unprotected:
		return r.unprotectedText()
	case r.kept:
		said = "Container " + id + " is kept. Inspect it with: pct enter " + id + ". Remove it with: pct destroy " + id + " --purge"
	case r.created:
		said = "Container " + id + " is still there. Remove it with: pct destroy " + id + " --purge"
	case r.stuck:
		said = "Container " + id + " could not be removed, or was gone already. Check with: pct status " + id
	case r.removed:
		said = "Container " + id + " was removed."
	default:
		said = "No container was made."
	}
	if r.fetched {
		said += " The container template stays on storage " + r.S.Cfg.TemplateStorage + " for the next run."
	}
	return said
}

// Keep leaves the container of a failed run where it is, for inspection. Cleanup then removes only
// the half-downloaded template.
func (r *Runner) Keep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.created {
		r.kept, r.created = true, false
	}
}

// Renet gives the kept container the network settings now in the options, and has the steps from
// its start on run again. The container is stopped for it, since a running one takes some of the
// changes only at its next start.
func (r *Runner) Renet(ctx context.Context) error {
	c := &r.S.Cfg
	_, _ = r.pct(ctx, "stop", c.CTID)
	// A refused change leaves the container stopped, so the next try starts it again either way.
	r.Redo(stepStartName)
	args := []string{"set", c.CTID, "--net0", c.containerNet()}
	if c.DNS != "" {
		args = append(args, "--nameserver", strings.ReplaceAll(c.DNS, ",", " "))
	} else {
		// No name servers given means the node's, which the container uses once its own are gone.
		args = append(args, "--delete", "nameserver")
	}
	if _, err := r.pct(ctx, args...); err != nil {
		return settingErr(settingNetwork, "Proxmox VE would not change container "+c.CTID+"'s network", "See "+r.LogPath+" for its message")
	}
	return nil
}

// Redo has the step named and every step after it run again, for a setting the container takes as
// it is.
func (r *Runner) Redo(from string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	again := false
	for _, st := range r.Steps() {
		again = again || st.ID == from
		if again {
			delete(r.done, st.ID)
		}
	}
}

// Cleanup removes what a failed run made, so running it again starts clean. It is safe to call
// when nothing was made, and from two places at once: the second call finds nothing left to do.
func (r *Runner) Cleanup(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	_, err := r.pct(ctx, "destroy", id, "--purge", "--destroy-unreferenced-disks", "1")
	r.removed, r.stuck = err == nil, err != nil
	r.created, r.started, r.ipv4Only = false, false, false
	for _, s := range []string{"create", "start", "address", "network", "certificate", "install", "service", "updates", "entry"} {
		delete(r.done, s)
	}
}

// unprotectedText is Left with mu held.
func (r *Runner) unprotectedText() string {
	if !r.unprotected {
		return ""
	}
	id := r.S.Cfg.CTID
	return "The appliance is installed in container " + id + " but not protected against deletion. Protect it with: pct set " + id + " --protection 1, or remove it with: pct destroy " + id + " --purge"
}

// pause waits one step interval, and ends at once when the run was cancelled.
func (r *Runner) pause(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("run cancelled: %w", err)
	}
	r.Sleep(stepPause)
	return nil
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
	if err := r.needSpace(ctx, r.Work, workKiB, "OpenTofu's download and its unpacked copy", "Free space there, or set TMPDIR to a directory on a larger file system"); err != nil {
		return err
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
		return "", settingErr(settingTemplate, "storage "+r.S.Cfg.TemplateStorage+" gives no path for the container template", "Name another with --template-storage")
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
	r.mu.Lock()
	r.fetched = true
	r.mu.Unlock()
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
		return settingErr(settingCTID, fmt.Sprintf("ID %s was taken by another guest during the install", c.CTID), "Run it again, or name a free ID with --ctid")
	}
	args := []string{
		"create", c.CTID, c.TemplateStorage + ":vztmpl/" + r.template,
		"--hostname", c.Hostname, "--cores", ctCores, "--memory", ctMemoryMiB, "--swap", ctSwapMiB,
		"--rootfs", c.Storage + ":" + ctDiskGiB, "--mp0", c.Storage + ":" + ctKeysGiB + ",mp=" + keyDir + ",backup=0",
		"--net0", c.containerNet(),
	}
	if c.DNS != "" {
		args = append(args, "--nameserver", strings.ReplaceAll(c.DNS, ",", " "))
	}
	args = append(args, "--unprivileged", "1", "--features", "nesting=1", "--onboot", "1", "--tags", "kubehenge",
		"--description", "Kubehenge's appliance: its web interface and the management cluster's state. Include it in backups.")
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("run cancelled: %w", err)
	}
	out, err := r.pct(ctx, args...)
	if err != nil && idTaken(c.CTID, out, err) {
		return settingErr(settingCTID, fmt.Sprintf("ID %s was taken by another guest during the install", c.CTID), "Run it again, or name a free ID with --ctid")
	}
	// Past Proxmox VE's existence check the id is locked for this create alone. Whatever stopped
	// the create after that, a cancel included, what it left is this run's to remove.
	r.mu.Lock()
	r.created = true
	r.mu.Unlock()
	if err != nil {
		return userErr("Proxmox VE could not create container "+c.CTID, "See "+r.LogPath+" for its message")
	}
	r.finish("create", "container "+c.CTID)
	return nil
}

// idTaken reports whether Proxmox VE refused a create because another guest holds the id. Its
// existence check says "CT 100 already exists on node 'pve1'". A create that waits on another's
// lock of the id says "can't lock file '/run/lock/lxc/pve-config-100.lock' - got timeout". Both
// come before the create makes anything.
func idTaken(id, out string, err error) bool {
	text := out
	if ce, ok := errors.AsType[*CmdError](err); ok {
		text += "\n" + ce.Stderr
	}
	q := regexp.QuoteMeta(id)
	return regexp.MustCompile(`\b(?:CT|VM) ` + q + ` already exists on node\b|/pve-config-` + q + `\.lock' - got timeout`).MatchString(text)
}

// ipv4OnlyScript switches IPv6 off in the container, now and at every boot. Without it the kernel
// takes an address from the router's advertisements whatever the container's configuration says.
const ipv4OnlyScript = `
set -e
mkdir -p /etc/sysctl.d
printf 'net.ipv6.conf.all.disable_ipv6 = 1\nnet.ipv6.conf.default.disable_ipv6 = 1\n' > /etc/sysctl.d/60-kubehenge-ipv4-only.conf
for scope in all default eth0; do
    echo 1 > "/proc/sys/net/ipv6/conf/$scope/disable_ipv6"
done
`

// ipv6OnScript undoes ipv4OnlyScript, for a container switched IPv6 off whose network now has IPv6.
const ipv6OnScript = `
set -e
rm -f /etc/sysctl.d/60-kubehenge-ipv4-only.conf
for scope in all default eth0; do
    echo 0 > "/proc/sys/net/ipv6/conf/$scope/disable_ipv6"
done
`

func (r *Runner) stepStart(ctx context.Context) error {
	c := &r.S.Cfg
	if _, err := r.pct(ctx, "start", c.CTID); err != nil {
		return userErr("container "+c.CTID+" did not start", "See "+r.LogPath+" for Proxmox VE's message")
	}
	switch {
	case c.IP6 == "":
		if _, err := r.inCT(ctx, "sh", "-c", ipv4OnlyScript); err != nil {
			return userErr("container "+c.CTID+" would not switch IPv6 off", "See "+r.LogPath+" for the command's message")
		}
		r.ipv4Only = true
	case r.ipv4Only:
		if _, err := r.inCT(ctx, "sh", "-c", ipv6OnScript); err != nil {
			return userErr("container "+c.CTID+" would not switch IPv6 on", "See "+r.LogPath+" for the command's message")
		}
		r.ipv4Only = false
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
	if r.S.Cfg.IP6 == "" {
		return v4, ""
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
		if err := r.pause(ctx); err != nil {
			return err
		}
	}
	if r.address == "" && r.address6 == "" {
		if c.IP == optNone {
			return &NoAddressError{
				Cause:   "container " + c.CTID + " got no IPv6 address",
				Fix:     "Check --vlan and that a router or DHCPv6 server answers on it, or give --ip6 and --gateway6",
				Setting: settingV6Mode,
			}
		}
		return &NoAddressError{
			DHCP:    c.IP == optDHCP,
			Cause:   "container " + c.CTID + " got no IPv4 address",
			Fix:     "Check --vlan and that a DHCP server answers on it, or give --ip <address>/<prefix> --gateway <address>",
			Setting: settingV4Mode,
		}
	}
	if c.IP6 != "" && r.address6 == "" {
		r.warnings = append(r.warnings, "container "+c.CTID+" has no IPv6 address, so its certificate names IPv4 only. Fix IPv6, then run: sh install.sh --certificate --ctid "+c.CTID)
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

// stepNetwork waits for the container's link, then tests what the appliance needs: a name of the
// Proxmox VE API that resolves and a connection to its port, retried for netTries pauses. A gateway
// that drops ping is no fault while the API connects. Only a failure is diagnosed.
func (r *Runner) stepNetwork(ctx context.Context) error {
	c := &r.S.Cfg
	if err := r.waitLink(ctx); err != nil {
		return err
	}
	hosts := r.apiHostsOrdered()
	_, port, _ := splitAPI(c.ProxmoxURL)
	reachable, resolved := "", false
	for try := range netTries {
		if reachable, resolved = r.reachAPI(ctx, hosts, port); reachable != "" {
			break
		}
		if try < netTries-1 {
			if err := r.pause(ctx); err != nil {
				return err
			}
		}
	}
	if reachable == "" && !resolved {
		return settingErr(settingDNS, "the container cannot resolve any name of the Proxmox VE API (tried "+strings.Join(hosts, ", ")+")",
			"Give --dns name servers the container reaches, or a --proxmox-url address the certificate covers")
	}
	if reachable == "" {
		return r.networkFailure(ctx, hosts, port)
	}
	if reachable != hosts[0] {
		r.S.noticef("the container cannot use %s, so the appliance and the clusters reach the Proxmox VE API at %s", hosts[0], reachable)
		c.ProxmoxURL = APIURL(reachable)
	}
	r.finish("network", "Proxmox VE API reachable")
	return nil
}

// waitLink waits for the container's interface to have a carrier.
func (r *Runner) waitLink(ctx context.Context) error {
	for try := range netTries {
		if _, err := r.inCT(ctx, "sh", "-c", "ip -o link show dev eth0 | grep -q LOWER_UP"); err == nil {
			return nil
		}
		if try < netTries-1 {
			if err := r.pause(ctx); err != nil {
				return err
			}
		}
	}
	return settingErr(settingNetwork, "container "+r.S.Cfg.CTID+"'s network interface has no link", "Check --bridge, --vlan and the uplink of the network")
}

// reachAPI tries each name of the API in order and returns the first one that resolves and takes a
// connection on the port. The second result is whether any name resolved.
func (r *Runner) reachAPI(ctx context.Context, hosts []string, port string) (string, bool) {
	resolved := false
	for _, h := range hosts {
		if !isAddress(h) {
			if _, err := r.inCT(ctx, "getent", "hosts", h); err != nil {
				continue
			}
		}
		resolved = true
		if _, err := r.inCT(ctx, "timeout", "5", "bash", "-c", `exec 3<>"/dev/tcp/$1/$2"`, "bash", h, port); err == nil {
			return h, true
		}
	}
	return "", resolved
}

// neighbourRE reads the state of an entry of the container's neighbour table.
var neighbourRE = regexp.MustCompile(`\b(REACHABLE|STALE|DELAY|PERMANENT|PROBE|FAILED|INCOMPLETE)\b`)

// networkFailure explains why the API cannot be reached although a name of it resolves. It pings
// the gateway three times, and when that fails reads the container's neighbour table, which shows
// whether the gateway answered ARP.
func (r *Runner) networkFailure(ctx context.Context, hosts []string, port string) error {
	c := &r.S.Cfg
	gw := r.containerGateway(ctx)
	if gw == "" {
		setting, fix := r.noRouteFix()
		return settingErr(setting, fmt.Sprintf("container %s has no default route, so it cannot connect to the Proxmox VE API at %s on port %s", c.CTID, hosts[0], port), fix)
	}
	ping, state := "its gateway did not answer ping", ""
	if _, err := r.inCT(ctx, "ping", "-c", "3", "-W", "2", gw); err == nil {
		// A gateway that answers ping has answered ARP, whatever the neighbour table shows.
		ping, state = "its gateway answers ping", "REACHABLE"
	} else if out, err := r.inCT(ctx, "ip", "neigh", "show", gw, "dev", "eth0"); err == nil {
		state = neighbourRE.FindString(out)
	}
	switch state {
	case "", "FAILED", "INCOMPLETE":
		address, probe := r.address, "ARP"
		if strings.Contains(gw, ":") {
			address, probe = r.address6, "neighbour discovery"
		}
		return settingErr(settingNetwork, fmt.Sprintf("container %s at %s cannot reach gateway %s (no answer to %s, %s)", c.CTID, address, gw, probe, ping),
			"Check that --bridge and --vlan carry the address and that the gateway is on that network")
	}
	return settingErr(settingAPI, fmt.Sprintf("container %s reaches its gateway but not the Proxmox VE API at %s", c.CTID, net.JoinHostPort(hosts[0], port)),
		"Check the route and firewall to port "+port+", LISTEN_IP in /etc/default/pveproxy, or give another URL with --proxmox-url")
}

// noRouteFix is the setting and the fix for a container without a default route, by how it gets
// its address: a gateway to give for a static one, else the network whose DHCP server or router
// should have sent one.
func (r *Runner) noRouteFix() (string, string) {
	c := &r.S.Cfg
	switch {
	case c.IP != optNone && modeOf(c.IP) == optStatic:
		return settingV4Gateway, "Give the container a gateway with --gateway"
	case c.IP != optNone:
		return settingNetwork, "Check that the DHCP server on --bridge and --vlan sends a gateway, or give --ip <address>/<prefix> --gateway <address>"
	case modeOf(c.IP6) == optStatic:
		return settingV6Gateway, "Give the container an IPv6 gateway with --gateway6"
	}
	return settingV6Mode, "Check that a router advertises on --bridge and --vlan, or give --ip6 and --gateway6"
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
		b, err := r.S.Node.readOwn(c, r.Now())
		if err != nil {
			return plainCertErr(err)
		}
		if r.cert, err = OwnCertificate(b, public, r.Now()); err != nil {
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
	if err := os.WriteFile(local, data, permFile); err != nil {
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
	if err := r.recordNetwork(ctx); err != nil {
		return userErr("recording container "+c.CTID+"'s network failed", "See "+r.LogPath+" for the command's message")
	}
	r.Sleep(stepPause)
	if _, err := r.inCT(ctx, "systemctl", "is-active", "--quiet", "kubehenge.service"); err != nil {
		return r.serviceFailure(ctx, "the service did not start")
	}
	// The realms are what the interface asks Proxmox VE before anyone signs in: an answer shows
	// that it reaches the API and trusts its certificate.
	status := 0
	for i := 0; i < 10 && status != http.StatusOK; i++ {
		if status = r.realms(ctx); status != http.StatusOK {
			if err := r.pause(ctx); err != nil {
				return err
			}
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
	return userErr(cause, "See "+r.LogPath+". Give a URL the container reaches, and that the certificate names, with --proxmox-url")
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
	_, err := r.inCT(ctx, "sh", "-c", "apt-get -q "+aptTimeouts+" update && DEBIAN_FRONTEND=noninteractive apt-get -q "+aptTimeouts+" -y install unattended-upgrades")
	if err != nil {
		r.warnings = append(r.warnings, "container "+r.S.Cfg.CTID+" could not install unattended-upgrades from Debian's mirrors ("+err.Error()+"). Update it with: pct exec "+r.S.Cfg.CTID+" -- sh -c 'apt-get update && apt-get -y upgrade'")
		r.emit(Event{Step: stepUpdatesName, State: StepSkipped, Detail: "not installed"})
		return nil //nolint:nilerr // the appliance works without automatic updates, so this is a warning.
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
			return nil //nolint:nilerr // the appliance works without the entry, so this is a warning.
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
	if _, err = io.Copy(out, src); err != nil {
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

// recordNetwork writes the network cards Proxmox VE configures for the container into the
// appliance's appliance.NetworkFile, where it reads the bridge and VLAN each of its addresses is
// on. A configuration that cannot be read, or holds no card, leaves the file as it is.
func (r *Runner) recordNetwork(ctx context.Context) error {
	conf, err := r.pct(ctx, "config", r.S.Cfg.CTID)
	if err != nil {
		return nil //nolint:nilerr // what was recorded stays, and the interface records it again
	}
	cards := appliance.NetworkCards(configEntries(conf))
	if len(cards) == 0 {
		return nil
	}
	return r.pushData(ctx, appliance.NetworkFile, cards, stateDir+"/"+appliance.NetworkFile, "0644")
}

// configEntries are the keys and values of a guest's configuration as pct config prints it. The
// snapshots' sections after it are not the guest's.
func configEntries(conf string) map[string]string {
	entries := map[string]string{}
	for line := range strings.Lines(conf) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			break
		}
		if key, value, ok := strings.Cut(line, ": "); ok {
			entries[key] = value
		}
	}
	return entries
}
