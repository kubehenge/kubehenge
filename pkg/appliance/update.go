package appliance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/release"
)

// The spool between the interface and the updater. The interface (unprivileged) may only drop
// requests into Requests. The updater (root) writes Status, which the interface reads.
const (
	maxRequest   = 4 << 10
	maxIndex     = 1 << 20
	maxSignature = 4 << 10
	maxArchive   = 512 << 20
)

// The updater's states besides succeeded and failed.
const (
	StateUpdating = "updating"
	StateCurrent  = "current"
)

// UpdateRequest is what the interface asks of the updater: a named release, or the newest one a
// policy allows.
type UpdateRequest struct {
	Version string         `json:"version,omitempty"`
	Policy  release.Policy `json:"policy,omitempty"`
}

// UpdateStatus is the updater's last result.
type UpdateStatus struct {
	// State is updating, succeeded, current (nothing newer allowed) or failed.
	State string    `json:"state"`
	From  string    `json:"from,omitempty"`
	To    string    `json:"to,omitempty"`
	Time  time.Time `json:"time"`
	Error string    `json:"error,omitempty"`
	// RolledBack tells that a failed upgrade had replaced the software and put the old back.
	RolledBack bool `json:"rolledBack,omitempty"`
	// Step is the one of UpdateSteps the upgrade is in, or failed in.
	Step string `json:"step,omitempty"`
}

// The steps of an upgrade, in the order it takes them.
const (
	StepIndex    = "Read the release index"
	StepDownload = "Download the release"
	StepInstall  = "Install"
	StepRestart  = "Restart the interface"
	StepCheck    = "Check that it serves"
)

// UpdateSteps are the steps of an upgrade, in order.
var UpdateSteps = []string{StepIndex, StepDownload, StepInstall, StepRestart, StepCheck}

// Spool is the updater's directory: requests/ (group-writable by the interface) and status.json.
type Spool string

// StatusPath is where the updater writes its result.
func (s Spool) StatusPath() string { return filepath.Join(string(s), "status.json") }

// Request asks the updater for an upgrade, as the interface does.
func (s Spool) Request(r UpdateRequest) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding the upgrade request: %w", err)
	}
	return writeFileAtomic(filepath.Join(s.requests(), "upgrade.json"), raw)
}

// Status reads the updater's last result, or nil.
func (s Spool) Status() *UpdateStatus {
	raw, err := os.ReadFile(s.StatusPath())
	if err != nil {
		return nil
	}
	var st UpdateStatus
	if json.Unmarshal(raw, &st) != nil {
		return nil
	}
	return &st
}

// Pending tells whether a request waits for the updater to take it.
func (s Spool) Pending() bool {
	_, err := os.Lstat(filepath.Join(s.requests(), "upgrade.json"))
	return err == nil
}

func (s Spool) requests() string { return filepath.Join(string(s), "requests") }

// Updater replaces the appliance's software with a signed newer release. It runs as root, so it
// trusts nothing the interface wrote beyond a small JSON request.
type Updater struct {
	Spool Spool
	// Index is the release index's URL. Its signature is at Index + ".sig".
	Index string
	// Root is the file system root, "/" but for tests.
	Root string
	HTTP *http.Client
	// Restart restarts the interface on the new software, and Reload reloads systemd's units.
	Restart func(ctx context.Context) error
	Reload  func(ctx context.Context) error
	// ApplianceDir is the interface's state directory. Its job lock keeps a restart from cutting
	// off a run of the management-cluster module.
	ApplianceDir string
	// Current is the installed release and Keys the release keys it trusts: release.Version and
	// release.PublicKeys() of this build.
	Current string
	Keys    []ed25519.PublicKey
	// Healthy tells whether the restarted interface serves. An upgrade it refuses is rolled back.
	Healthy func(ctx context.Context) error
	// FreeSpace tells how many bytes may still be written where the software lives. It is the
	// file system's own answer when nil.
	FreeSpace func(path string) (uint64, error)

	// status is the upgrade under way, which each step records as it begins.
	status *UpdateStatus
}

// Run takes one request from the spool, if any, and carries it out.
func (u *Updater) Run(ctx context.Context) error {
	req, ok, err := u.take()
	if err != nil || !ok {
		return err
	}
	// Setting the software aside again would delete the only copy of the old release.
	if err = u.recoverRestarting(ctx); err != nil {
		return fmt.Errorf("undoing an earlier upgrade cut off in the middle: %w", err)
	}
	if exists(filepath.Join(u.aside(), pendingFile)) {
		return errors.New("an earlier upgrade cut off in the middle is still to be undone, once the management cluster run under way ends. Ask again then")
	}
	if exists(filepath.Join(u.aside(), "shell-pending")) {
		return errors.New("an upgrade by install.sh was cut off in the middle. Run install.sh --upgrade again, which undoes it first")
	}
	st := UpdateStatus{State: StateUpdating, From: u.Current, Time: time.Now()}
	u.status = &st
	defer func() { u.status = nil }()
	u.at(StepIndex)
	to, err := u.update(ctx, req)
	st.Time, st.To = time.Now(), to
	switch {
	case err != nil:
		st.State, st.Error, st.RolledBack = StateFailed, err.Error(), errors.Is(err, ErrRolledBack)
	case to == "":
		st.State = StateCurrent
	default:
		st.State = StateSucceeded
	}
	u.write(&st)
	return err
}

// ErrRecovered is an upgrade cut off in the middle and undone by Recover, which reloaded systemd's
// units. The interface's start fails once with it, so that systemd starts it again on the units of
// the release put back.
var ErrRecovered = errors.New("undone")

// Recover undoes an upgrade cut off in the middle, as by a reboot or a kill, and records it as
// failed. An upgrade that stopped before it replaced anything is only recorded. It does nothing
// while an upgrade holds the job lock. Having undone one, it reloads systemd's units and returns
// ErrRecovered. kubehenge.service runs it before it starts.
func (u *Updater) Recover(ctx context.Context) error {
	p, undone, err := u.undoCutOff()
	if err != nil || !undone {
		return err
	}
	if err = u.Reload(ctx); err != nil {
		return err
	}
	return fmt.Errorf("the upgrade to %s was cut off in the middle and is %w, so the interface starts again on %s", p.To, ErrRecovered, p.From)
}

// take reads and removes the request. A symbolic link or anything but a small regular file is
// refused: the interface's user owns the directory, not the files root acts on.
func (u *Updater) take() (UpdateRequest, bool, error) {
	p := filepath.Join(u.Spool.requests(), "upgrade.json")
	f, err := os.OpenFile(p, os.O_RDONLY|noFollow, 0) //nolint:gosec // a fixed name in the spool, opened without following links
	if errors.Is(err, fs.ErrNotExist) {
		return UpdateRequest{}, false, nil
	}
	if err != nil {
		_ = os.Remove(p)
		return UpdateRequest{}, false, fmt.Errorf("upgrade request: %w", err)
	}
	// One request is taken once, whatever becomes of it.
	defer func() {
		_ = f.Close()
		_ = os.Remove(p)
	}()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxRequest {
		return UpdateRequest{}, false, errors.New("upgrade request: not a small regular file")
	}
	var req UpdateRequest
	dec := json.NewDecoder(io.LimitReader(f, maxRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return UpdateRequest{}, false, fmt.Errorf("upgrade request: %w", err)
	}
	switch {
	case req.Version != "" && req.Policy == "":
	case req.Version == "" && (req.Policy == release.PolicyPatch || req.Policy == release.PolicyMinor):
	default:
		return UpdateRequest{}, false, errors.New("upgrade request: a version or a policy of patch or minor")
	}
	return req, true, nil
}

// at records that the upgrade under way has begun step.
func (u *Updater) at(step string) {
	if u.status != nil {
		u.status.Step = step
		u.write(u.status)
	}
}

// write records st for the interface, which runs as another user and reads it.
func (u *Updater) write(st *UpdateStatus) {
	raw, err := json.Marshal(st)
	if err == nil {
		err = writeAtomicFrom(u.Spool.StatusPath(), bytes.NewReader(raw), publicFile)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubehenge update: status:", err)
	}
}

// FetchIndex downloads the release index and verifies it with keys, and that it has not expired at
// now.
func FetchIndex(ctx context.Context, client *http.Client, url string, keys []ed25519.PublicKey, now time.Time) (*release.Index, error) {
	index, err := get(ctx, client, url, maxIndex)
	if err != nil {
		return nil, err
	}
	sig, err := get(ctx, client, url+".sig", maxSignature)
	if err != nil {
		return nil, err
	}
	ix, err := release.Parse(index, string(sig), keys, now)
	if err != nil {
		return nil, fmt.Errorf("the release index at %s: %w", url, err)
	}
	return ix, nil
}

func (u *Updater) update(ctx context.Context, req UpdateRequest) (string, error) {
	ix, err := FetchIndex(ctx, u.HTTP, u.Index, u.Keys, time.Now())
	if err != nil {
		return "", err
	}
	var r *release.Release
	if req.Version != "" {
		r, err = ix.Find(u.Current, req.Version)
		if err != nil {
			return "", fmt.Errorf("release %s: %w", req.Version, err)
		}
	} else {
		r = ix.Newest(u.Current, req.Policy)
	}
	if r == nil {
		return "", nil
	}
	if err = u.checkSpace(); err != nil {
		return "", err
	}
	// Downloads go to disk, not memory: the appliance is small, the archive and OpenTofu large, and
	// /tmp may be memory too.
	downloads := filepath.Join(u.Root, "usr", "local", "share", ".kubehenge-download")
	err = os.RemoveAll(downloads)
	if err == nil {
		err = os.MkdirAll(downloads, privateDir)
	}
	if err != nil {
		return "", fmt.Errorf("preparing the download directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(downloads) }()
	u.at(StepDownload)
	archive, err := download(ctx, u.HTTP, r.URL, r.SHA256, downloads)
	if err != nil {
		return "", err
	}
	tofu := ""
	if r.Tofu != nil {
		tofu, err = downloadTofu(ctx, u.HTTP, r.Tofu, downloads)
		if err != nil {
			return "", err
		}
	}
	// Held until the new software serves or the old one is back: the old interface could otherwise
	// start a run in between.
	unlock, err := LockJobs(u.ApplianceDir)
	if err != nil {
		return "", err
	}
	defer unlock()
	return r.Version, u.replace(ctx, r.Version, archive, tofu)
}

const (
	mebibyte = 1 << 20
	// spareSpace is what an upgrade leaves free on top of its own files.
	spareSpace = 64 * mebibyte
	// upgradeCopies is how many copies of the software an upgrade holds at once: the installed one,
	// the download unpacked beside it and the old one kept aside until the new one serves.
	upgradeCopies = 3
)

// checkSpace refuses an upgrade the disk has no room for. A disk filling up halfway would leave the
// old software half replaced.
func (u *Updater) checkSpace() error {
	var installed uint64
	for _, p := range []string{u.share(), u.binary("kubehenge"), u.binary("tofu")} {
		err := filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
				installed += uint64(info.Size()) //nolint:gosec // a file's size is never negative
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("measuring the installed software: %w", err)
		}
	}
	free := u.FreeSpace
	if free == nil {
		free = freeSpace
	}
	have, err := free(filepath.Dir(u.share()))
	if err != nil {
		// Where the free space is not known, a full disk still stops the upgrade, only later.
		return nil //nolint:nilerr // the check is a forewarning, not the only guard
	}
	if need := upgradeCopies*installed + spareSpace; have < need {
		return fmt.Errorf("the appliance has %d MiB free and the upgrade needs %d MiB. Free space in it, or grow its disk with: pct resize <ctid> rootfs +4G", have/mebibyte, need/mebibyte)
	}
	return nil
}

func (u *Updater) binary(name string) string {
	return filepath.Join(u.Root, "usr", "local", "bin", name)
}

func (u *Updater) share() string { return filepath.Join(u.Root, "usr", "local", "share", "kubehenge") }

// replace installs the archive and OpenTofu's binary at tofu, when not empty, restarts the
// interface on them and checks that it serves. Failing anywhere, it puts the old software back.
func (u *Updater) replace(ctx context.Context, version, archive, tofu string) error {
	sw, err := u.setAside(version, tofu)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(sw.aside) }()
	u.at(StepInstall)
	err = u.install(archive, sw.aside)
	if err == nil && tofu != "" {
		err = copyAtomic(tofu, u.binary("tofu"), publicExecutable)
	}
	if err == nil {
		sw.restarted = true
		u.at(StepRestart)
		err = u.Restart(ctx)
	}
	if err == nil && u.Healthy != nil {
		u.at(StepCheck)
		err = u.Healthy(ctx)
	}
	if err == nil {
		return nil
	}
	if sw.restarted {
		err = fmt.Errorf("release %s did not start: %w", version, err)
	}
	return sw.rollBack(ctx, err)
}

// swap is an upgrade's replacement of the installed software, kept in aside until the new one
// serves: a copy of the binary and, where the upgrade replaces it, of OpenTofu, and the share
// directory install moves there, which holds the units too.
type swap struct {
	u     *Updater
	aside string
	// tofu tells that the upgrade replaces OpenTofu, and restarted that it restarted the interface.
	tofu, restarted bool
}

func (u *Updater) aside() string {
	return filepath.Join(u.Root, "usr", "local", "share", ".kubehenge-rollback")
}

// pendingFile, in the directory the old software is set aside in, marks a swap under way. Found
// while no upgrade holds the job lock, it is an upgrade cut off in the middle, by a reboot or a
// kill, and the old software is all there to put back.
const pendingFile = "pending.json"

// pendingSwap is what pendingFile records.
type pendingSwap struct {
	From string `json:"from"`
	To   string `json:"to"`
	Tofu bool   `json:"tofu,omitempty"`
}

// setAside makes the swap of the software for release version, whose OpenTofu is at tofu, or
// keeps OpenTofu when tofu is empty.
func (u *Updater) setAside(version, tofu string) (*swap, error) {
	sw := &swap{u: u, aside: u.aside(), tofu: tofu != ""}
	err := os.RemoveAll(sw.aside)
	if err == nil {
		err = os.MkdirAll(sw.aside, privateDir)
	}
	if err != nil {
		return nil, fmt.Errorf("setting the installed software aside: %w", err)
	}
	names := []string{"kubehenge"}
	if sw.tofu {
		names = append(names, "tofu")
	}
	for _, name := range names {
		err = copyAtomic(u.binary(name), filepath.Join(sw.aside, name), publicExecutable)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("setting the installed software aside: %w", err)
		}
	}
	raw, err := json.Marshal(pendingSwap{From: u.Current, To: version, Tofu: sw.tofu})
	if err == nil {
		err = writeFileAtomic(filepath.Join(sw.aside, pendingFile), raw)
	}
	if err != nil {
		return nil, fmt.Errorf("setting the installed software aside: %w", err)
	}
	return sw, nil
}

// recoverRestarting undoes an upgrade cut off in the middle as Recover does, then restarts the
// interface on the software put back and checks it.
func (u *Updater) recoverRestarting(ctx context.Context) error {
	_, undone, err := u.undoCutOff()
	if err != nil || !undone {
		return err
	}
	if err = u.Restart(ctx); err != nil {
		return err
	}
	if u.Healthy != nil {
		return u.Healthy(ctx)
	}
	return nil
}

// undoCutOff puts back the software of an upgrade cut off in the middle and returns that upgrade,
// and whether there was one to undo.
func (u *Updater) undoCutOff() (pendingSwap, bool, error) {
	unlock, err := LockJobs(u.ApplianceDir)
	if errors.Is(err, ErrBusy) {
		return pendingSwap{}, false, nil
	}
	if err != nil {
		return pendingSwap{}, false, err
	}
	defer unlock()
	raw, err := os.ReadFile(filepath.Join(u.aside(), pendingFile))
	if errors.Is(err, fs.ErrNotExist) {
		if st := u.Spool.Status(); st != nil && st.State == StateUpdating {
			st.State, st.Time = StateFailed, time.Now()
			st.Error = "the appliance stopped during the upgrade, before it replaced the software"
			u.write(st)
		}
		return pendingSwap{}, false, nil
	}
	var p pendingSwap
	if err == nil {
		err = json.Unmarshal(raw, &p)
	}
	if err != nil {
		return p, false, fmt.Errorf("reading the upgrade cut off in the middle: %w", err)
	}
	sw := &swap{u: u, aside: u.aside(), tofu: p.Tofu}
	if err = sw.restore(); err != nil {
		return p, false, fmt.Errorf("undoing the upgrade to %s, cut off in the middle: %w", p.To, err)
	}
	if err = os.RemoveAll(sw.aside); err != nil {
		return p, false, fmt.Errorf("undoing the upgrade to %s: %w", p.To, err)
	}
	u.write(&UpdateStatus{
		State: StateFailed, From: p.From, To: p.To, Time: time.Now(), RolledBack: true, Step: StepInstall,
		Error: fmt.Sprintf("the appliance stopped during the upgrade to %s, so it rolled back to %s", p.To, p.From),
	})
	return p, true, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// rollBack puts the software set aside in place again after an upgrade failed with cause, and
// restarts the interface on it when the upgrade had restarted it. The job lock stays held
// throughout, so the interface starts no run on either release meanwhile.
func (sw *swap) rollBack(ctx context.Context, cause error) error {
	u := sw.u
	err := sw.restore()
	if err == nil && sw.restarted {
		err = u.Restart(ctx)
		if err == nil && u.Healthy != nil {
			err = u.Healthy(ctx)
		}
	}
	if err != nil {
		return fmt.Errorf("%w; rolling back to %s failed too: %w", cause, u.Current, err)
	}
	return fmt.Errorf("%w; %w to %s", cause, ErrRolledBack, u.Current)
}

// ErrRolledBack is an upgrade undone: the release installed before serves again.
var ErrRolledBack = errors.New("rolled back")

// restore puts the software set aside back. OpenTofu the upgrade brought where there was none
// before goes, as does a unit the old software does not have.
func (sw *swap) restore() error {
	u := sw.u
	if err := copyAtomic(filepath.Join(sw.aside, "kubehenge"), u.binary("kubehenge"), publicExecutable); err != nil {
		return err
	}
	if sw.tofu {
		if err := putBack(filepath.Join(sw.aside, "tofu"), u.binary("tofu"), publicExecutable); err != nil {
			return err
		}
	}
	old := filepath.Join(sw.aside, "share")
	if _, err := os.Stat(old); err == nil {
		if err := os.RemoveAll(u.share()); err != nil {
			return fmt.Errorf("removing the new software: %w", err)
		}
		if err := os.Rename(old, u.share()); err != nil {
			return fmt.Errorf("putting the software back: %w", err)
		}
	}
	for _, name := range Units {
		if err := putBack(filepath.Join(u.share(), "appliance", name), filepath.Join(u.Root, "etc", "systemd", "system", name), publicFile); err != nil {
			return err
		}
	}
	return nil
}

// putBack replaces dest with a copy of src, or removes it when there is no src.
func putBack(src, dest string, mode os.FileMode) error {
	err := copyAtomic(src, dest, mode)
	if errors.Is(err, fs.ErrNotExist) {
		err = os.Remove(dest)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("putting %s back: %w", filepath.Base(dest), err)
	}
	return nil
}

// install unpacks the archive beside the installed software and swaps it in: the share directory
// exchanged whole, the old one moved aside, the binary and the units replaced.
func (u *Updater) install(archive, aside string) error {
	// Staged beside its destination: renames cannot cross file systems, and the service's sandbox
	// mounts each writable directory on its own.
	staging := filepath.Join(u.Root, "usr", "local", "share", ".kubehenge-staging")
	err := os.RemoveAll(staging)
	if err != nil {
		return fmt.Errorf("clearing the staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	err = unpack(archive, staging)
	if err == nil {
		err = checkUnpacked(staging)
	}
	if err != nil {
		return err
	}
	err = os.Rename(u.share(), filepath.Join(aside, "share"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("setting the installed software aside: %w", err)
	}
	err = os.Rename(filepath.Join(staging, "share", "kubehenge"), u.share())
	if err != nil {
		return fmt.Errorf("putting the new software in place: %w", err)
	}
	// Copied, not renamed: the service's sandbox mounts /usr/local/bin apart from the staging
	// directory, and a rename cannot cross mounts.
	err = copyAtomic(filepath.Join(staging, "bin", "kubehenge"), u.binary("kubehenge"), publicExecutable)
	if err != nil {
		return err
	}
	return u.installUnits()
}

// installUnits copies the units of the software in place to systemd.
func (u *Updater) installUnits() error {
	for _, name := range Units {
		err := copyAtomic(filepath.Join(u.share(), "appliance", name), filepath.Join(u.Root, "etc", "systemd", "system", name), publicFile)
		if err != nil {
			return err
		}
	}
	return nil
}

// Units are the appliance's systemd units, shipped in share/kubehenge/appliance.
var Units = []string{
	"kubehenge.service", "kubehenge-update.service", "kubehenge-update.path",
	"kubehenge-headlamp.service", "kubehenge-headlamp.socket", "kubehenge-headlamp-proxy.service",
}

// checkUnpacked makes sure an unpacked release holds everything install puts in place.
func checkUnpacked(staging string) error {
	for _, need := range []string{"bin/kubehenge", "share/kubehenge/management-cluster", "share/kubehenge/appliance/kubehenge.service"} {
		if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(need))); err != nil {
			return fmt.Errorf("the release archive has no %s", need)
		}
	}
	return nil
}

// downloadTofu fetches the release's OpenTofu into dir and returns the path of its binary.
func downloadTofu(ctx context.Context, client *http.Client, t *release.Artefact, dir string) (string, error) {
	tarball, err := download(ctx, client, t.URL, t.SHA256, dir)
	if err != nil {
		return "", err
	}
	tofu := filepath.Join(dir, "tofu")
	err = extractFile(tarball, "tofu", tofu)
	if err != nil {
		return "", fmt.Errorf("OpenTofu %s: %w", t.Version, err)
	}
	return tofu, nil
}

// download fetches url into a new file in dir, checking it against the signed index's checksum
// as it arrives, and returns the file's path.
func download(ctx context.Context, client *http.Client, url, want, dir string) (string, error) {
	var name string
	err := retrying(ctx, func() error {
		var err error
		name, err = downloadOnce(ctx, client, url, want, dir)
		return err
	})
	return name, err
}

func downloadOnce(ctx context.Context, client *http.Client, url, want, dir string) (string, error) {
	resp, err := fetch(ctx, client, url)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	f, err := os.CreateTemp(dir, "download-*")
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	sum := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, sum), io.LimitReader(resp.Body, maxArchive+1))
	err = f.Close()
	switch {
	case copyErr != nil:
		err = transientError{fmt.Errorf("GET %s: %w", url, copyErr)}
	case err != nil:
		err = fmt.Errorf("GET %s: %w", url, err)
	case n > maxArchive:
		err = fmt.Errorf("GET %s: larger than %d bytes", url, maxArchive)
	case hex.EncodeToString(sum.Sum(nil)) != strings.ToLower(want):
		err = fmt.Errorf("%s does not match the checksum in the signed index", url)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// extractFile writes one regular file at the top of a gzipped tar to dest.
func extractFile(archive, name, dest string) error {
	tr, closeArchive, err := openTarball(archive)
	if err != nil {
		return err
	}
	defer closeArchive()
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("the archive holds no %s", name)
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && path.Clean(strings.TrimPrefix(h.Name, "./")) == name {
			return writeAtomicFrom(dest, io.LimitReader(tr, maxArchive), privateFile)
		}
	}
}

// copyAtomic replaces dest with a copy of src, with the given mode.
func copyAtomic(src, dest string, mode os.FileMode) error {
	f, err := os.Open(src) //nolint:gosec // the updater's own files
	if err != nil {
		return fmt.Errorf("installing %s: %w", filepath.Base(dest), err)
	}
	defer func() { _ = f.Close() }()
	return writeAtomicFrom(dest, f, mode)
}

// unpack extracts a gzipped tar of directories and regular files only, every path inside dir.
func unpack(archive, dir string) error {
	tr, closeArchive, err := openTarball(archive)
	if err != nil {
		return err
	}
	defer closeArchive()
	if err = os.MkdirAll(dir, publicDir); err != nil {
		return fmt.Errorf("unpacking the release archive: %w", err)
	}
	// Every write goes through the root, which refuses any path that would leave dir, through
	// ".." or a symbolic link alike.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("unpacking the release archive: %w", err)
	}
	defer func() { _ = root.Close() }()
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the release archive: %w", err)
		}
		if err := unpackEntry(tr, h, root); err != nil {
			return err
		}
	}
}

// unpackEntry writes one entry of the release archive, whose content r reads, under root.
func unpackEntry(r io.Reader, h *tar.Header, root *os.Root) error {
	name := path.Clean(strings.TrimPrefix(h.Name, "./"))
	if name == "." {
		return nil
	}
	target := filepath.FromSlash(name)
	if !filepath.IsLocal(target) {
		return fmt.Errorf("the release archive holds %q, outside itself", h.Name)
	}
	// The software is world-readable: the interface runs as its own user and reads the module and
	// charts.
	switch h.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(target, publicDir); err != nil {
			return fmt.Errorf("unpacking %s: %w", h.Name, err)
		}
		return nil
	case tar.TypeReg:
		if err := root.MkdirAll(filepath.Dir(target), publicDir); err != nil {
			return fmt.Errorf("unpacking %s: %w", h.Name, err)
		}
		mode := os.FileMode(publicFile)
		if h.Mode&anyExecute != 0 {
			mode = publicExecutable
		}
		f, err := root.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return fmt.Errorf("unpacking %s: %w", h.Name, err)
		}
		_, err = io.Copy(f, io.LimitReader(r, maxArchive))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("unpacking %s: %w", h.Name, err)
		}
		return nil
	default:
		return fmt.Errorf("the release archive holds %q, which is neither a file nor a directory", h.Name)
	}
}

// openTarball opens a gzipped tar. The returned function closes it.
func openTarball(archive string) (*tar.Reader, func(), error) {
	f, err := os.Open(archive) //nolint:gosec // the updater's own download
	if err != nil {
		return nil, nil, fmt.Errorf("opening the archive: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s: %w", archive, err)
	}
	return tar.NewReader(gz), func() { _ = f.Close() }, nil
}

func get(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	var body []byte
	err := retrying(ctx, func() error {
		resp, err := fetch(ctx, client, url)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return transientError{fmt.Errorf("GET %s: %w", url, err)}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return body, nil
}

const (
	// downloadAttempts is how often a download is tried in all. Proxies and mirrors drop
	// connections and answer 502 or 504 now and then.
	downloadAttempts = 5
	// retryDelay is the wait before the second try, doubled before each further one.
	retryDelay = time.Second
)

// transientError is a failed download that may succeed when tried again.
type transientError struct{ err error }

func (e transientError) Error() string { return e.err.Error() }
func (e transientError) Unwrap() error { return e.err }

// retrying calls try until it succeeds, fails for good, or has been tried downloadAttempts times.
func retrying(ctx context.Context, try func() error) error {
	delay := retryDelay
	for attempt := 1; ; attempt++ {
		err := try()
		var transient transientError
		if err == nil || !errors.As(err, &transient) || attempt == downloadAttempts || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// stallTimeout ends a download that receives nothing for that long. A slow link may take as long
// as it needs, as long as data keeps arriving.
var stallTimeout = 2 * time.Minute //nolint:gochecknoglobals // a test shortens it

var errStalled = errors.New("nothing arrived for two minutes")

// watchedBody cancels its request once no data has arrived for stallTimeout.
type watchedBody struct {
	io.ReadCloser

	// cause is the request context's cause, errStalled once the timer fired.
	cause func() error
	timer *time.Timer
	stop  context.CancelCauseFunc
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(stallTimeout)
	}
	if err != nil && errors.Is(b.cause(), errStalled) {
		err = errStalled
	}
	return n, err
}

func (b *watchedBody) Close() error {
	b.timer.Stop()
	b.stop(nil)
	return b.ReadCloser.Close() //nolint:wrapcheck // the body's own error
}

// fetch starts a GET of url, which has to be https, and returns the response once its status is
// OK. The caller closes its body. A failed connection, a stalled one and an answer a busy or
// failing server gives are transientError.
func fetch(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("%s is not https", url)
	}
	ctx, stop := context.WithCancelCause(ctx)
	timer := time.AfterFunc(stallTimeout, func() { stop(errStalled) })
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		timer.Stop()
		stop(nil)
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		timer.Stop()
		if errors.Is(context.Cause(ctx), errStalled) {
			err = errStalled
		}
		stop(nil)
		return nil, transientError{fmt.Errorf("downloading %s: %w", url, err)}
	}
	resp.Body = &watchedBody{ReadCloser: resp.Body, cause: func() error { return context.Cause(ctx) }, timer: timer, stop: stop}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		err = fmt.Errorf("GET %s: %s", url, resp.Status)
		if resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
			return nil, transientError{err}
		}
		return nil, err
	}
	return resp, nil
}

// ReloadUnits has systemd read the units again.
func ReloadUnits(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RestartService restarts the interface's systemd unit after reloading the units.
func RestartService(ctx context.Context) error {
	for _, args := range [][]string{{"daemon-reload"}, {"restart", "kubehenge.service"}} {
		if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed arguments
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
