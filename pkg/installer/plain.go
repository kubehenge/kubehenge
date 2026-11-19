package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Env is what a run needs from the machine, built once and shared by both front ends.
type Env struct {
	Node    *Node
	Exec    Exec
	Fetch   *Fetcher
	Dir     string
	Work    string
	LogPath string
	// AnswersPath is the file the answers of a run that did not finish are kept in.
	AnswersPath string
	Proxy       string
	TofuV       string
	TofuSH      string
	Log         *os.File
	Cleanup     func()
	// Tune adjusts every runner built from this environment, for tests.
	Tune func(*Runner)
}

// LogDir and LogPrefix name the log of a run: one file per run, so a later run keeps the log of an
// earlier failure.
const (
	LogDir    = "/var/log"
	LogPrefix = "kubehenge-install-"
)

// AnswersFile is where a run that did not finish keeps its answers.
const AnswersFile = "/var/lib/kubehenge-install/answers.json"

// keptLogs is how many run logs stay in the log directory, the new one included.
const keptLogs = 10

// openLog creates the log of a run that started at t, in dir, and removes all but the newest
// keptLogs. A random part after the time keeps two runs started in the same second apart.
func openLog(dir string, t time.Time) (*os.File, error) {
	f, err := os.CreateTemp(dir, LogPrefix+t.UTC().Format("20060102-150405")+"-*.log")
	if err != nil {
		return nil, fmt.Errorf("create the log: %w", err)
	}
	// The names start with the time, so they sort oldest first.
	if logs, err := filepath.Glob(filepath.Join(dir, LogPrefix+"*.log")); err == nil && len(logs) > keptLogs {
		for _, old := range logs[:len(logs)-keptLogs] {
			if old != f.Name() {
				_ = os.Remove(old)
			}
		}
	}
	return f, nil
}

// NewEnv opens the log, the work directory and the node, ready for a run on this machine.
func NewEnv(ctx context.Context) (*Env, error) {
	if os.Geteuid() != 0 {
		return nil, userErr("this needs root", "Run it as root in a Proxmox VE node's shell")
	}
	for _, tool := range []string{"pct", "pvesm", "pvesh"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, userErr("this is not a Proxmox VE node", "Run it in the shell of the node you want the appliance on")
		}
	}
	e := &Env{AnswersPath: AnswersFile}
	var err error
	if e.Log, err = openLog(LogDir, time.Now()); err != nil {
		return nil, err
	}
	e.LogPath = e.Log.Name()
	if e.Work, err = os.MkdirTemp("", "kubehenge-install-"); err != nil {
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	e.Exec = &OSExec{Log: e.Log}
	out, _ := e.Exec.Run(ctx, "pveversion")
	if !strings.HasPrefix(strings.TrimSpace(out), "pve-manager/9.") {
		return nil, userErr("Kubehenge needs Proxmox VE 9", "Run it on a Proxmox VE 9 node")
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	e.Node = &Node{Exec: e.Exec, Hostname: host, LookupHost: lookupHost, Dial: netDial, Probe: ProbeARP}
	if data, err := os.ReadFile("/etc/pve/datacenter.cfg"); err == nil {
		e.Proxy = DatacenterProxy(data)
	}
	e.Fetch = NewFetcher(e.Proxy)
	e.Dir = os.Getenv("KUBEHENGE_RELEASE_DIR")
	if e.Dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("executable: %w", err)
		}
		e.Dir = filepath.Dir(filepath.Dir(exe))
	}
	e.TofuV, e.TofuSH = strings.TrimPrefix(os.Getenv("KUBEHENGE_TOFU_TAG"), "v"), os.Getenv("KUBEHENGE_TOFU_SHA256")
	e.Cleanup = func() {
		_ = e.Log.Close()
		_ = os.RemoveAll(e.Work)
		// install.sh unpacked the release here and leaves its removal to the installer.
		if dir := os.Getenv("KUBEHENGE_BOOTSTRAP_DIR"); dir != "" {
			_ = os.RemoveAll(dir) //nolint:gosec // the directory is the one install.sh made and handed over.
		}
	}
	return e, nil
}

func lookupHost(ctx context.Context, name string) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupHost(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("look up host: %w", err)
	}
	return addrs, nil
}

// NewRunner builds the runner of a prepared session.
func (e *Env) NewRunner(s *Session, emit func(Event)) *Runner {
	r := &Runner{
		S: s, Exec: e.Exec, Fetch: e.Fetch, Work: e.Work, Dir: e.Dir, Emit: emit, Proxy: e.Proxy,
		TofuV: e.TofuV, TofuSH: e.TofuSH, LogPath: e.LogPath,
	}
	if e.Tune != nil {
		e.Tune(r)
	}
	return r
}

// RunPlain installs with one line per step and no questions. What only a question could settle
// is an error naming the flag that settles it.
func RunPlain(ctx context.Context, s *Session, newRunner func(emit func(Event)) *Runner, out io.Writer) error {
	if err := plainChecks(ctx, s); err != nil {
		printRepeat(out, &s.Cfg, false)
		return err
	}
	if s.Cfg.PVEEntry == "" {
		s.Cfg.PVEEntry = "no"
	}
	for _, n := range append(append([]string(nil), s.Notices...), s.Cautions...) {
		_, _ = fmt.Fprintln(out, "kubehenge: "+n)
	}
	if s.Cfg.DryRun {
		_, _ = fmt.Fprint(out, dryRunHeading+s.PlanText(false))
		return nil
	}
	_, _ = fmt.Fprint(out, "Installing:\n"+s.PlanText(false))
	runner := newRunner(nil)
	labels := map[string]string{}
	for _, st := range runner.Steps() {
		labels[st.ID] = st.Label
	}
	runner.Emit = func(e Event) {
		switch e.State {
		case StepRunning:
			_, _ = fmt.Fprintf(out, "...  %s\n", labels[e.Step])
		case StepDone:
			_, _ = fmt.Fprintf(out, "ok   %s%s\n", labels[e.Step], suffix(e.Detail))
		case StepSkipped:
			_, _ = fmt.Fprintf(out, "skip %s%s\n", labels[e.Step], suffix(e.Detail))
		case StepFailed:
			_, _ = fmt.Fprintf(out, "fail %s%s\n", labels[e.Step], suffix(e.Detail))
		case StepPending:
		}
	}
	res, err := runner.Run(ctx)
	if err != nil {
		plainFailed(ctx, s, runner, out)
		return err
	}
	WriteEnd(out, res)
	return nil
}

// plainChecks settles what the run needs before it changes anything, or refuses.
func plainChecks(ctx context.Context, s *Session) error {
	if err := s.detect(ctx); err != nil {
		return err
	}
	if err := s.CheckAddressing(ctx); err != nil {
		return err
	}
	if err := s.FillStorages(PickBest); err != nil {
		return err
	}
	if err := s.ResolveAPI(ctx, PickBest); err != nil {
		return err
	}
	return s.UnattendedCheck()
}

// dryRunHeading starts the plan a dry run prints.
const dryRunHeading = "Dry run, nothing changed. The install would be:\n"

// plainFailed keeps or removes the container of a failed plain run, then names the log and the
// command that runs the install again.
func plainFailed(ctx context.Context, s *Session, runner *Runner, out io.Writer) {
	// The cleanup must run even when the run was cancelled.
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	id := s.Cfg.CTID
	switch {
	case s.Cfg.KeepFailed && runner.Made():
		runner.Keep()
		_, _ = fmt.Fprintf(out, "kubehenge: keeping container %s. Inspect it with: pct enter %s\n", id, id)
	case runner.Made():
		_, _ = fmt.Fprintf(out, "kubehenge: removing container %s after the failure\n", id)
	}
	runner.Cleanup(cleanCtx)
	if runner.Stuck() {
		_, _ = fmt.Fprintf(out, "kubehenge: container %s could not be removed. Remove it with: pct destroy %s --purge\n", id, id)
	}
	_, _ = fmt.Fprintf(out, "kubehenge: log %s\n", runner.LogPath)
	printRepeat(out, &s.Cfg, runner.HoldsID())
}

// printRepeat prints the install.sh command that runs the install again with the answers given,
// when there are any. A container still holding the ID leaves --ctid out, so the next run takes a
// free one.
func printRepeat(out io.Writer, c *Config, holdsID bool) {
	again := *c
	if holdsID {
		again.CTID = ""
	}
	if cmd := again.RepeatCommand(); cmd != repeatBase {
		_, _ = fmt.Fprintf(out, "kubehenge: to run it again with the same answers: %s\n", cmd)
	}
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// WriteEnd prints the result as plain lines.
func WriteEnd(out io.Writer, res *Result) {
	for _, w := range res.Warnings {
		_, _ = fmt.Fprintln(out, "kubehenge: "+w)
	}
	_, _ = fmt.Fprintf(out, "\nKubehenge is running in container %s: %s\n", res.CTID, res.URL)
	_, _ = fmt.Fprintf(out, "Certificate SHA-256 fingerprint: %s\nValid until: %s\n", res.Fingerprint, res.ValidUntil.UTC().Format(time.RFC1123))
	if res.Name != "" {
		_, _ = fmt.Fprintf(out, "Point %s at %s in your DNS.\n", res.Name, res.Address)
	}
	_, _ = fmt.Fprintf(out, "Sign in with a Proxmox VE administrator to create the management cluster.\nLog: %s\n", res.LogPath)
}

// IsUserError reports whether err already says what to do.
func IsUserError(err error) bool {
	var u *UserError
	var n *NoAddressError
	return errors.As(err, &u) || errors.As(err, &n)
}
