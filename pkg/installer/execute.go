package installer

import (
	"context"
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
	env, err := NewEnv(cfg)
	if err != nil {
		return err
	}
	defer env.Cleanup()
	s := &Session{Node: env.Node, Cfg: *cfg}
	newRunner := func(emit func(Event)) *Runner { return env.NewRunner(s, emit) }
	if cfg.Mode() != ModeInstall {
		return maintain(ctx, s, newRunner(nil), stdout)
	}

	plain := !interactive || cfg.Plain
	if !plain && cfg.IP != "" && cfg.PVEEntry != "" {
		// Every flag the road needs is given: detect once, and ask only what a tie leaves open.
		if err := s.detect(ctx); err != nil {
			return err
		}
		plain = !s.Questions().Any()
	}
	if plain {
		return RunPlain(ctx, s, newRunner, stdout)
	}
	model := NewModel(s, newRunner)
	prog := tea.NewProgram(model, tea.WithInput(stdin), tea.WithOutput(stdout), tea.WithContext(ctx))
	if _, err := prog.Run(); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if model.Cancelled() {
		_, _ = fmt.Fprintln(os.Stderr, "Cancelled. Nothing was changed.")
		return nil
	}
	return model.Err()
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
	if err := s.FillStorages(false); err != nil {
		return err
	}
	return s.ResolveAPI(ctx, false)
}
