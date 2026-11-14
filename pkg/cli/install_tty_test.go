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

// runInstall runs `kubehenge install` with the machine's terminals and files stood in for, and
// returns what the installer read its answers from and whether it could ask.
func runInstall(t *testing.T, stdinTerminal, stdoutTerminal bool, open func(name string) (io.ReadCloser, error)) (string, bool) {
	t.Helper()
	savedTerminal, savedOpen, savedExecute := isTerminal, openFile, executeInstall
	t.Cleanup(func() { isTerminal, openFile, executeInstall = savedTerminal, savedOpen, savedExecute })
	isTerminal = func(f *os.File) bool {
		if f == os.Stdin {
			return stdinTerminal
		}
		return stdoutTerminal
	}
	openFile = open
	var answers string
	var asked bool
	executeInstall = func(_ context.Context, _ *installer.Config, in io.Reader, _ io.Writer, interactive bool) error {
		data, err := io.ReadAll(in)
		answers, asked = string(data), interactive
		return err
	}
	root := NewRoot()
	root.SetIn(strings.NewReader("script"))
	root.SetOut(io.Discard)
	root.SetArgs([]string{"install"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	return answers, asked
}

// With install.sh piped into sh, standard input is the pipe and the questions go to /dev/tty.
func TestInstallCommandAsksOnTheControllingTerminal(t *testing.T) { //nolint:paralleltest // stands in for the machine's terminals
	var opened []string
	open := func(name string) (io.ReadCloser, error) {
		opened = append(opened, name)
		if name != "/dev/tty" {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return io.NopCloser(strings.NewReader("keys")), nil
	}
	if in, asked := runInstall(t, false, true, open); !asked || in != "keys" || len(opened) != 1 || opened[0] != "/dev/tty" {
		t.Errorf("a pipe with a terminal output: asked=%v, read %q, opened %v", asked, in, opened)
	}
	if _, asked := runInstall(t, false, true, func(string) (io.ReadCloser, error) { return nil, errors.New("no controlling terminal") }); asked {
		t.Error("asked questions with no terminal to ask on")
	}
	if in, asked := runInstall(t, false, false, open); asked || in != "script" || len(opened) != 1 {
		t.Errorf("output to a file: asked=%v, read %q, opened %v", asked, in, opened)
	}
	if in, asked := runInstall(t, true, true, open); !asked || in != "script" || len(opened) != 1 {
		t.Errorf("a terminal on both sides: asked=%v, read %q, opened %v", asked, in, opened)
	}
}
