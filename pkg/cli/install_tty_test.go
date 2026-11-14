package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/installer"
)

// runInstall runs the install command against a stand-in machine and returns what Execute got.
func runInstall(t *testing.T, stdinTerminal, stdoutTerminal bool, open func() (io.ReadCloser, error)) (io.Reader, bool) {
	t.Helper()
	var gotIn io.Reader
	var gotInteractive bool
	cmd := newInstallCommandOn(installHost{
		isTerminal: func(f *os.File) bool {
			if f == os.Stdin {
				return stdinTerminal
			}
			return stdoutTerminal
		},
		openTerminal: open,
		execute: func(_ context.Context, _ *installer.Config, in io.Reader, _ io.Writer, interactive bool) error {
			gotIn, gotInteractive = in, interactive
			return nil
		},
	})
	cmd.SetIn(strings.NewReader("script"))
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	return gotIn, gotInteractive
}

// With install.sh piped into sh, standard input is the pipe and the questions go to /dev/tty.
func TestInstallCommandAsksOnTheControllingTerminal(t *testing.T) {
	t.Parallel()
	terminal := io.NopCloser(strings.NewReader("keys"))
	opens := 0
	open := func() (io.ReadCloser, error) {
		opens++
		return terminal, nil
	}
	if in, interactive := runInstall(t, false, true, open); !interactive || in != io.Reader(terminal) || opens != 1 {
		t.Errorf("a pipe with a terminal output: interactive=%v, opens %d", interactive, opens)
	}
	if _, interactive := runInstall(t, false, true, func() (io.ReadCloser, error) { return nil, errors.New("no controlling terminal") }); interactive {
		t.Error("asked questions with no terminal to ask on")
	}
	if _, interactive := runInstall(t, false, false, open); interactive || opens != 1 {
		t.Errorf("output to a file still opened the terminal: interactive=%v, opens %d", interactive, opens)
	}
	if _, interactive := runInstall(t, true, true, open); !interactive || opens != 1 {
		t.Errorf("a terminal on both sides: interactive=%v, opens %d", interactive, opens)
	}
}
