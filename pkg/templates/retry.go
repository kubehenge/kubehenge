package templates

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// finalError is a build failure another attempt would meet again, such as a node whose CPU is too
// old for the node OS; a retry only delays saying so.
type finalError struct{ err error }

func (e finalError) Error() string { return e.err.Error() }
func (e finalError) Unwrap() error { return e.err }

// lasting marks err as one no retry helps with.
func lasting(err error) error { return finalError{err: err} }

// Final tells whether a build failed in a way another attempt would too.
func Final(err error) bool {
	var f finalError
	return errors.As(err, &f)
}

// Build retries are spaced out so that what fails for a while, a mirror being synced or a node
// busy with backups, has passed: 2 minutes, then 8.
const (
	DefaultBuildAttempts = 3
	defaultBuildBackoff  = 2 * time.Minute
	buildBackoffFactor   = 4
)

// Retry runs a build again after it fails, waiting longer each time, unless it failed for good
// (Final) or its context ended.
type Retry struct {
	// Attempts is how many times a build runs at most; DefaultBuildAttempts when 0.
	Attempts int
	// Backoff is the wait after the first failure; it grows fourfold after each.
	Backoff time.Duration
	Log     func(format string, args ...any)
	// Sleep waits d or until ctx ends; time's own when nil.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Do runs build, naming it what in the log, and returns its last result.
func (r *Retry) Do(ctx context.Context, what string, build func(context.Context) (int, error)) (int, error) {
	attempts := r.Attempts
	if attempts <= 0 {
		attempts = DefaultBuildAttempts
	}
	wait := r.Backoff
	if wait <= 0 {
		wait = defaultBuildBackoff
	}
	for attempt := 1; ; attempt++ {
		vmid, err := build(ctx)
		if err == nil || attempt == attempts || Final(err) || ctx.Err() != nil {
			if err != nil && attempt > 1 {
				return 0, fmt.Errorf("after %d attempts: %w", attempt, err)
			}
			return vmid, err
		}
		if r.Log != nil {
			r.Log("building %s failed (attempt %d of %d): %v; trying again in %s", what, attempt, attempts, err, wait)
		}
		if err := r.sleep(ctx, wait); err != nil {
			return 0, fmt.Errorf("building %s: %w", what, err)
		}
		wait *= buildBackoffFactor
	}
}

func (r *Retry) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("waiting to build again: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}
