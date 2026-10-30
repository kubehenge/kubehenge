package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLogLevelsAreChecked(t *testing.T) {
	t.Parallel()
	for _, level := range logLevels {
		l := logOptions{level: level}
		if _, err := l.zapLogger(); err != nil {
			t.Fatalf("zap at %s: %v", level, err)
		}
		if _, err := l.slogLogger(&bytes.Buffer{}); err != nil {
			t.Fatalf("slog at %s: %v", level, err)
		}
	}
	for _, level := range []string{"", "trace", "INFO", "warning"} {
		l := logOptions{level: level}
		if _, err := l.zapLogger(); err == nil {
			t.Fatalf("zap took --log-level %q", level)
		}
		if _, err := l.slogLogger(&bytes.Buffer{}); err == nil {
			t.Fatalf("slog took --log-level %q", level)
		}
	}
}

func TestInterfaceLogsAtItsLevel(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	l := logOptions{level: "warn", development: true}
	logger, err := l.slogLogger(&out)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("left out")
	logger.Warn("kept")
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("logged %q, want only the warning", out.String())
	}
	var line struct {
		Msg    string         `json:"msg"`
		Source map[string]any `json:"source"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("%q is no JSON: %v", lines[0], err)
	}
	if line.Msg != "kept" || line.Source["file"] == nil {
		t.Fatalf("logged %+v, want the warning with where it was logged", line)
	}
}

func TestServiceCommandsTakeLogFlags(t *testing.T) {
	t.Parallel()
	root := NewRoot()
	for _, name := range []string{"controller", "serve"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"log-level", "log-development"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Fatalf("%s has no --%s", name, flag)
			}
		}
	}
}
