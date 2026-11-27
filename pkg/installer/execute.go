package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Execute runs the install the options ask for: the full-screen installer on a terminal, else one
// line per step. It prints its own failures' cause and fix through the returned error.
func Execute(ctx context.Context, cfg *Config, stdin io.Reader, stdout io.Writer, interactive bool) error {
	if err := cfg.Normalise(); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.CheckOptionFiles(); err != nil {
		return err
	}
	env, err := newEnv(ctx)
	if err != nil {
		return err
	}
	defer env.Cleanup()
	return executeOn(ctx, env, cfg, ioStreams{in: stdin, out: stdout, errs: os.Stderr}, interactive, os.Getenv("TERM"))
}

// newEnv prepares this machine for the run. Tests stand in a scripted node.
var newEnv = NewEnv

type ioStreams struct {
	in   io.Reader
	out  io.Writer
	errs io.Writer
	// size gives the screen's size where out is no terminal to ask, as in tests.
	size []tea.ProgramOption
}

// executeOn is Execute on a prepared machine, for the terminal named by term.
func executeOn(ctx context.Context, env *Env, cfg *Config, streams ioStreams, interactive bool, term string) error {
	stdin, stdout := streams.in, streams.out
	var err error
	s := &Session{Node: env.Node, Cfg: *cfg}
	newRunner := func(emit func(Event)) *Runner { return env.NewRunner(s, emit) }
	if cfg.Mode() == ModeInstall && !cfg.DryRun && len(s.Node.ApplianceContainers(ctx)) > 0 {
		// The cluster has its appliance, which an install would only refuse to duplicate. A dry run
		// keeps that refusal, since an upgrade changes the container.
		s.Cfg.Upgrade = true
		if err = s.Cfg.Validate(); err != nil {
			return err
		}
		r := newRunner(nil)
		if err = r.requireAppliance(ctx); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "Upgrading the appliance in container %s\n", s.Cfg.CTID)
		return maintain(ctx, s, r, stdout)
	}
	if cfg.Mode() != ModeInstall {
		return maintain(ctx, s, newRunner(nil), stdout)
	}

	plain := !interactive || cfg.Plain || !CanDrawScreen(term)
	if !plain && cfg.Yes && cfg.IP != "" && cfg.PVEEntry != "" {
		// Every flag the road needs is given and the summary needs no yes: detect once, and ask only
		// what a tie leaves open.
		if err = s.detect(ctx); err != nil {
			return err
		}
		plain = !s.Questions().Any()
	}
	if plain {
		return RunPlain(ctx, s, newRunner, stdout)
	}
	s.AnswersPath = env.AnswersPath
	if s.Facts == nil && s.AnswersPath != "" {
		s.Saved = loadAnswers(s.AnswersPath)
	}
	model := NewModel(s, newRunner)
	prog := tea.NewProgram(model, append([]tea.ProgramOption{tea.WithInput(stdin), tea.WithOutput(stdout), tea.WithContext(ctx)}, streams.size...)...)
	_, err = prog.Run()
	if errors.Is(err, tea.ErrProgramPanic) {
		// The user never chose to remove what a crashed installer made.
		model.keep = true
	}
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	model.Shutdown(cleanCtx)
	return model.ended(err, streams)
}

// ended is what an interactive install returns once the program ended with err: the run's error
// with what is left of it, or nil with the dry run's plan or the cancel printed. An install that
// stopped with answers given also prints the command that runs it again.
func (m *Model) ended(err error, streams ioStreams) error {
	if err != nil {
		if m.Finished() {
			// A signal on the end screen, such as a dropped SSH session, ends a finished install.
			return nil
		}
		m.repeatOnStop(streams)
		if outcome := m.Outcome(); outcome != "" {
			return fmt.Errorf("run: %w. %s", err, outcome)
		}
		return fmt.Errorf("run: %w", err)
	}
	if m.DryRun() {
		for _, c := range m.S.AllCautions() {
			_, _ = fmt.Fprintln(streams.out, "kubehenge: "+c)
		}
		_, _ = fmt.Fprint(streams.out, dryRunHeading+m.S.PlanText(true))
		return nil
	}
	m.repeatOnStop(streams)
	outcome := m.Outcome()
	if m.Cancelled() {
		if outcome == "" {
			outcome = "Nothing was changed."
		}
		_, _ = fmt.Fprintln(streams.errs, "Cancelled. "+outcome)
		return nil
	}
	if err = m.Err(); err != nil && outcome != "" && outcome != m.Left() {
		return fmt.Errorf("%w. %s", err, outcome)
	}
	return err
}

// repeatOnStop prints the command that runs an install again, once it stopped with answers given.
func (m *Model) repeatOnStop(streams ioStreams) {
	if !m.Finished() && (m.runner != nil || len(m.answered) > 0) {
		printRepeat(streams.errs, &m.S.Cfg, m.runner)
	}
}

// CanDrawScreen reports whether a terminal of this TERM value can show the full-screen installer.
// A dumb terminal, such as a serial console or an editor's shell, gets the one-line-per-step output.
func CanDrawScreen(term string) bool {
	return term != "" && term != "dumb"
}

// maintain runs --upgrade or --certificate, which print one line per step.
func maintain(ctx context.Context, s *Session, r *Runner, out io.Writer) error {
	if !s.Node.Quorate(ctx) {
		return userErr("this node has no quorum in its Proxmox VE cluster, so it cannot change any container", "Start the other nodes, then run this again")
	}
	if s.Cfg.Mode() == ModeUpgrade {
		_, err := r.Upgrade(ctx, out)
		return err
	}
	res, err := r.ReplaceCertificate(ctx, out)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\nKubehenge in container %s: %s\nCertificate SHA-256 fingerprint: %s\nValid until: %s\n",
		res.CTID, res.URL, res.Fingerprint, res.ValidUntil.UTC().Format(time.RFC1123))
	return nil
}

// detect runs the detection every road shares, once.
func (s *Session) detect(ctx context.Context) error {
	if s.Facts != nil {
		return nil
	}
	if err := s.Prepare(ctx); err != nil {
		return err
	}
	if err := s.FillStorages(AskTies); err != nil {
		return err
	}
	// Restored before the API is checked, so that a failed check still offers the restored answers.
	s.Restore(ctx, s.Saved)
	return s.ResolveAPI(ctx, AskTies)
}
