package installer

import (
	"context"
	"fmt"
	"io"
	"os"

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
	switch cfg.Mode() {
	case ModeUpgrade, ModeCertificate:
		return userErr("--upgrade and --certificate are not available in this installer yet", "Use install.sh of the release that is installed")
	case ModeInstall:
	}
	env, err := NewEnv(cfg)
	if err != nil {
		return err
	}
	defer env.Cleanup()
	s := &Session{Node: env.Node, Cfg: *cfg}
	newRunner := func(emit func(Event)) *Runner { return env.NewRunner(s, emit) }

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
		return err
	}
	if model.Cancelled() {
		_, _ = fmt.Fprintln(os.Stderr, "Cancelled. Nothing was changed.")
		return nil
	}
	return model.Err()
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
