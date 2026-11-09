package templates

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		failures  []error
		wantRuns  int
		wantWaits []time.Duration
		wantErr   string
	}{
		{name: "first time", wantRuns: 1},
		{name: "after a failure", failures: []error{errors.New("task failed")}, wantRuns: 2, wantWaits: []time.Duration{2 * time.Minute}},
		{
			name: "never", failures: []error{errors.New("a"), errors.New("b"), errors.New("c")}, wantRuns: 3,
			wantWaits: []time.Duration{2 * time.Minute, 8 * time.Minute}, wantErr: "after 3 attempts: c",
		},
		{name: "final", failures: []error{lasting(errors.New("CPU too old"))}, wantRuns: 1, wantErr: "CPU too old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var waits []time.Duration
			var logged []string
			r := &Retry{
				Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
				Log:   func(format string, _ ...any) { logged = append(logged, format) },
			}
			runs := 0
			vmid, err := r.Do(context.Background(), "t", func(context.Context) (int, error) {
				runs++
				if runs <= len(tc.failures) {
					return 0, tc.failures[runs-1]
				}
				return 9001, nil
			})
			if runs != tc.wantRuns || !slices.Equal(waits, tc.wantWaits) || len(logged) != len(waits) {
				t.Errorf("runs %d, waits %v, logged %d", runs, waits, len(logged))
			}
			if tc.wantErr == "" {
				if err != nil || vmid != 9001 {
					t.Errorf("%d, %v", vmid, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestRetryingTriesOnlyTransientFailures(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	t.Run("transient twice, then fine", func(t *testing.T) {
		t.Parallel()
		runs := 0
		err := Retrying(t.Context(), 4, time.Millisecond, func() error {
			runs++
			if runs < 3 {
				return Transient(cause)
			}
			return nil
		})
		if err != nil || runs != 3 {
			t.Errorf("%d runs, %v", runs, err)
		}
	})
	t.Run("a plain error ends it", func(t *testing.T) {
		t.Parallel()
		runs := 0
		err := Retrying(t.Context(), 4, time.Millisecond, func() error { runs++; return cause })
		if !errors.Is(err, cause) || runs != 1 {
			t.Errorf("%d runs, %v", runs, err)
		}
	})
	t.Run("the last attempt ends it, unmarked", func(t *testing.T) {
		t.Parallel()
		runs := 0
		err := Retrying(t.Context(), 4, time.Millisecond, func() error { runs++; return Transient(cause) })
		var mark transientError
		if runs != 4 || !errors.Is(err, cause) || errors.As(err, &mark) {
			t.Errorf("%d runs, %v, marked %t", runs, err, errors.As(err, &mark))
		}
	})
	t.Run("a cancelled context stops the wait", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		runs := 0
		err := Retrying(ctx, 4, time.Hour, func() error {
			runs++
			cancel()
			return Transient(cause)
		})
		if runs != 1 || !errors.Is(err, cause) {
			t.Errorf("%d runs, %v", runs, err)
		}
	})
	t.Run("the mark does not reach an outer Retrying", func(t *testing.T) {
		t.Parallel()
		inner, outer := 0, 0
		_ = Retrying(t.Context(), 3, time.Millisecond, func() error {
			outer++
			return Retrying(t.Context(), 4, time.Millisecond, func() error { inner++; return Transient(cause) })
		})
		if inner != 4 || outer != 1 {
			t.Errorf("inner %d, outer %d, want 4 and 1", inner, outer)
		}
	})
	t.Run("the wait doubles from the first", func(t *testing.T) {
		t.Parallel()
		var stamps []time.Time
		_ = Retrying(t.Context(), 3, 40*time.Millisecond, func() error { stamps = append(stamps, time.Now()); return Transient(cause) })
		if len(stamps) != 3 || stamps[1].Sub(stamps[0]) < 40*time.Millisecond || stamps[2].Sub(stamps[1]) < 80*time.Millisecond {
			t.Errorf("tries at %v", stamps)
		}
	})
}

// A run cut off while waiting stops at once.
func TestRetryStopsWithItsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runs := 0
	_, err := (&Retry{}).Do(ctx, "t", func(context.Context) (int, error) { runs++; return 0, errors.New("task failed") })
	if runs != 1 || !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "task failed") {
		t.Errorf("runs %d, %v", runs, err)
	}
}
