package cli

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// With install.sh piped into sh, standard input is the pipe and the questions go to /dev/tty.
func TestQuestionInput(t *testing.T) {
	t.Parallel()
	pipe := strings.NewReader("script")
	terminal := io.NopCloser(strings.NewReader("keys"))
	opens := 0
	open := func() (io.ReadCloser, error) {
		opens++
		return terminal, nil
	}
	refuse := func() (io.ReadCloser, error) { return nil, errors.New("no controlling terminal") }

	if in, ok, closeIn := questionInput(pipe, false, true, open); !ok || in != terminal || opens != 1 {
		t.Errorf("a pipe with a terminal output: ok=%v, input %v, opens %d", ok, in, opens)
	} else {
		closeIn()
	}
	if _, ok, _ := questionInput(pipe, false, true, refuse); ok {
		t.Error("asked questions with no terminal to ask on")
	}
	if _, ok, _ := questionInput(pipe, false, false, open); ok || opens != 1 {
		t.Errorf("output to a file still opened the terminal: ok=%v, opens %d", ok, opens)
	}
	if in, ok, _ := questionInput(pipe, true, true, open); !ok || in != io.Reader(pipe) || opens != 1 {
		t.Errorf("a terminal on both sides: ok=%v, opens %d", ok, opens)
	}
}
