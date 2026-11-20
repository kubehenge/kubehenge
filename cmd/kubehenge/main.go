// Command kubehenge manages Kubehenge, Kubernetes for Proxmox VE.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kubehenge/kubehenge/pkg/cli"
)

// shutdownSignals are the signals that end a run. SIGHUP is what a dropped SSH session sends, and
// the installer removes its half-made container on it as on Ctrl+C. A process started with SIGHUP
// ignored (nohup) keeps ignoring it, so an install it runs survives a dropped session.
func shutdownSignals(ignored func(os.Signal) bool) []os.Signal {
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if hangUp != nil && !ignored(hangUp) {
		signals = append(signals, hangUp)
	}
	return signals
}

func main() {
	limitMemory()
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals(signal.Ignored)...)
	err := cli.Execute(ctx, cli.NewRoot())
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubehenge:", err)
		os.Exit(1)
	}
}
