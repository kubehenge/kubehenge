package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Exec runs the node's own commands (pct, pvesm, pvesh, ip). It is the one seam between the
// installer and the machine, so tests script the node with a fake.
type Exec interface {
	// Run returns the command's standard output. A failure is a *CmdError carrying its
	// standard error.
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// CmdError is a failed command.
type CmdError struct {
	Command string
	Stderr  string
	// Code is the command's exit status, -1 when it did not run.
	Code int
	Err  error
}

func (e *CmdError) Error() string {
	msg := fmt.Sprintf("%s: %v", e.Command, e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

func (e *CmdError) Unwrap() error { return e.Err }

// OSExec runs commands on this machine and writes each one with its output to Log, so the screen
// shows none of it and the log file keeps all of it.
type OSExec struct {
	Log io.Writer
	mu  sync.Mutex
}

// Run implements Exec.
func (e *OSExec) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	line := strings.Join(append([]string{name}, args...), " ")
	e.logf("$ %s\n%s%s", line, stdout.String(), stderr.String())
	if err != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
			err = fmt.Errorf("exit status %d", code)
		}
		return stdout.String(), &CmdError{Command: line, Stderr: stderr.String(), Code: code, Err: err}
	}
	return stdout.String(), nil
}

func (e *OSExec) logf(format string, args ...any) {
	if e.Log == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = fmt.Fprintf(e.Log, format, args...)
}
