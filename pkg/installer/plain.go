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
	Proxy   string
	TofuV   string
	TofuSH  string
	Log     *os.File
	Cleanup func()
}

// LogFile is where the commands' output goes.
const LogFile = "/var/log/kubehenge-install.log"

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
	e := &Env{LogPath: LogFile}
	var err error
	if e.Log, err = os.OpenFile(LogFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, permFile); err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
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
	e.Node = &Node{Exec: e.Exec, Hostname: host, LookupHost: lookupHost, Dial: netDial}
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
	return &Runner{
		S: s, Exec: e.Exec, Fetch: e.Fetch, Work: e.Work, Dir: e.Dir, Emit: emit, Proxy: e.Proxy,
		TofuV: e.TofuV, TofuSH: e.TofuSH, LogPath: e.LogPath,
	}
}

// RunPlain installs with one line per step and no questions. What only a question could settle
// is an error naming the flag that settles it.
func RunPlain(ctx context.Context, s *Session, newRunner func(emit func(Event)) *Runner, out io.Writer) error {
	if err := s.detect(ctx); err != nil {
		return err
	}
	if err := s.CheckStaticFree(ctx); err != nil {
		return err
	}
	if err := s.FillStorages(PickBest); err != nil {
		return err
	}
	if err := s.ResolveAPI(ctx, PickBest); err != nil {
		return err
	}
	if err := s.UnattendedCheck(); err != nil {
		return err
	}
	if s.Cfg.PVEEntry == "" {
		s.Cfg.PVEEntry = "no"
	}
	for _, n := range s.Notices {
		_, _ = fmt.Fprintln(out, "kubehenge: "+n)
	}
	runner := newRunner(nil)
	labels := map[string]string{}
	for _, st := range runner.Steps() {
		labels[st.ID] = st.Label
	}
	runner.Emit = func(e Event) {
		switch e.State {
		case StepDone:
			_, _ = fmt.Fprintf(out, "ok   %s%s\n", labels[e.Step], suffix(e.Detail))
		case StepSkipped:
			_, _ = fmt.Fprintf(out, "skip %s%s\n", labels[e.Step], suffix(e.Detail))
		case StepFailed:
			_, _ = fmt.Fprintf(out, "fail %s\n", labels[e.Step])
		case StepPending, StepRunning:
		}
	}
	res, err := runner.Run(ctx)
	if err != nil {
		// The cleanup must run even when the run was cancelled.
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_, _ = fmt.Fprintf(out, "kubehenge: removing container %s after the failure\n", s.Cfg.CTID)
		runner.Cleanup(cleanCtx)
		return err
	}
	WriteEnd(out, res)
	return nil
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
