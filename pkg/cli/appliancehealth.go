package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/installer"
	"github.com/kubehenge/kubehenge/pkg/release"
)

// healthReporter shows the stages of the health check on standard error. On a terminal the stage
// that waits is one line redrawn with the time left, and it gives way to the line that ends it.
// Elsewhere each stage is one plain line.
type healthReporter struct {
	w    io.Writer
	live bool
	// started is the waiting stage whose plain start line was printed last.
	started appliance.HealthStage
	printed bool
	drawn   bool
}

// clearLine returns to the start of the line and erases it.
const clearLine = "\r\x1b[K"

func (r *healthReporter) report(e appliance.HealthEvent) {
	switch e.Stage {
	case appliance.StageConnecting:
		r.wait(e.Stage, fmt.Sprintf("Connecting to %s, waiting up to %s", e.Address, span(e.Total)),
			fmt.Sprintf("Connecting to %s, %s left", e.Address, span(e.Left)))
	case appliance.StageCertificate:
		r.line("Certificate matches " + e.File)
	case appliance.StageAnswered:
		r.line("Answered after " + span(e.Elapsed))
	case appliance.StageSettling:
		r.wait(e.Stage, "Staying up for "+span(e.Total), fmt.Sprintf("Staying up, %s left", span(e.Left)))
	case appliance.StageStayedUp:
		r.line("Stayed up for " + span(e.Total))
	}
}

// wait shows a stage that takes time. Plainly it prints its start once, and live it redraws.
func (r *healthReporter) wait(stage appliance.HealthStage, start, tick string) {
	if r.live {
		r.print(clearLine + tick)
		r.drawn = true
		return
	}
	if !r.printed || r.started != stage {
		r.print(start + "\n")
	}
	r.started, r.printed = stage, true
}

func (r *healthReporter) line(text string) {
	r.finish()
	r.print(text + "\n")
}

// finish erases a line that is still counting down.
func (r *healthReporter) finish() {
	if r.drawn {
		r.print(clearLine)
		r.drawn = false
	}
}

func (r *healthReporter) print(s string) { _, _ = io.WriteString(r.w, s) }

// secondsPerMinute is where span switches to minutes.
const secondsPerMinute = 60

// span writes a duration in whole seconds, rounded up, with minutes from a minute on.
func span(d time.Duration) string {
	secs := int(math.Ceil(d.Seconds()))
	switch {
	case secs < secondsPerMinute:
		return fmt.Sprintf("%d s", secs)
	case secs%secondsPerMinute == 0:
		return fmt.Sprintf("%d min", secs/secondsPerMinute)
	}
	return fmt.Sprintf("%d min %d s", secs/secondsPerMinute, secs%secondsPerMinute)
}

// healthVerdict is what --json prints.
type healthVerdict struct {
	Healthy              bool    `json:"healthy"`
	Address              string  `json:"address"`
	Version              string  `json:"version,omitempty"`
	AnsweredAfterSeconds float64 `json:"answeredAfterSeconds,omitempty"`
	StayedUpSeconds      float64 `json:"stayedUpSeconds,omitempty"`
	Error                string  `json:"error,omitempty"`
	Next                 string  `json:"next,omitempty"`
}

// healthError is the check's error with the one thing to do about it.
type healthError struct {
	err  error
	next string
}

func (f *healthError) Error() string { return "not healthy: " + f.err.Error() + ". " + f.next }
func (f *healthError) Unwrap() error { return f.err }

// nextStep is what to do about a failed check.
func nextStep(err error) string {
	if _, ok := errors.AsType[*appliance.CertificateError](err); ok {
		return "Check that --cert and --stored-cert name the certificate the interface should serve"
	}
	return "Read the interface's log with journalctl -u kubehenge.service"
}

// runHealth runs the check and tells the user how it went: the stages on standard error, the
// verdict on standard output.
func (o *applianceOptions) runHealth(cmd *cobra.Command) error {
	check := o.check
	var verdict healthVerdict
	var reporter *healthReporter
	if !o.quiet {
		out := cmd.ErrOrStderr()
		reporter = &healthReporter{w: out, live: isTerminal(os.Stderr) && installer.CanDrawScreen(os.Getenv("TERM"))}
	}
	check.Report = func(e appliance.HealthEvent) {
		if e.Stage == appliance.StageAnswered {
			verdict.AnsweredAfterSeconds = e.Elapsed.Seconds()
		}
		if e.Stage == appliance.StageStayedUp {
			verdict.StayedUpSeconds = e.Total.Seconds()
		}
		if reporter != nil {
			reporter.report(e)
		}
	}
	err := check.Healthy(cmd.Context())
	if reporter != nil {
		reporter.finish()
	}
	verdict.Address = check.Address
	if err != nil {
		if o.quiet {
			return err //nolint:wrapcheck // the check's own message, which the upgrade script prints
		}
		failure := &healthError{err: err, next: nextStep(err)}
		verdict.Error, verdict.Next = err.Error(), failure.next
		if o.json {
			if jerr := printVerdict(cmd, &verdict); jerr != nil {
				return jerr
			}
		}
		return failure
	}
	verdict.Healthy, verdict.Version = true, release.Version
	switch {
	case o.quiet:
		return nil
	case o.json:
		return printVerdict(cmd, &verdict)
	}
	return say(cmd, fmt.Sprintf("Healthy: Kubehenge %s answers on %s", release.Version, check.Address))
}

func printVerdict(cmd *cobra.Command, v *healthVerdict) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding the verdict: %w", err)
	}
	return printJSON(cmd, raw)
}
