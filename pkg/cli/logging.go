package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// logLevels are the levels --log-level takes, as zap and slog both name them.
var logLevels = []string{"debug", "info", "warn", "error"}

// logOptions are the logging flags of the commands that run as services.
type logOptions struct {
	level       string
	development bool
}

// addFlags adds the flags, whose defaults the environment gives, so that the appliance's own
// appliance.env sets them for its unit.
func (l *logOptions) addFlags(f *pflag.FlagSet, development string) {
	level := os.Getenv("KUBEHENGE_LOG_LEVEL")
	if level == "" {
		level = "info"
	}
	dev, _ := strconv.ParseBool(os.Getenv("KUBEHENGE_LOG_DEVELOPMENT"))
	f.StringVar(&l.level, "log-level", level, "the least severe lines logged: debug, info, warn or error (env KUBEHENGE_LOG_LEVEL)")
	f.BoolVar(&l.development, "log-development", dev, development+" (env KUBEHENGE_LOG_DEVELOPMENT)")
}

func (l *logOptions) check() error {
	if !slices.Contains(logLevels, l.level) {
		return fmt.Errorf("--log-level: %q is none of debug, info, warn and error", l.level)
	}
	return nil
}

// zapLogger is the controller's logger. It stays in JSON in development too, so that nothing a user
// sends can forge a line: development only adds stack traces from warnings on, and panics on the
// lines zap calls DPanic.
func (l *logOptions) zapLogger() (logr.Logger, error) {
	if err := l.check(); err != nil {
		return logr.Logger{}, err
	}
	level, err := zapcore.ParseLevel(l.level)
	if err != nil {
		return logr.Logger{}, fmt.Errorf("--log-level: %w", err)
	}
	return zap.New(zap.Level(level), zap.JSONEncoder(), func(o *zap.Options) { o.Development = l.development }), nil
}

// slogLogger is the interface's logger, in JSON. Development adds where in the code each line was
// logged.
func (l *logOptions) slogLogger(w io.Writer) (*slog.Logger, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(l.level)); err != nil {
		return nil, fmt.Errorf("--log-level: %w", err)
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, AddSource: l.development})), nil
}
