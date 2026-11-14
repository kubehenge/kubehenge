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
// the installer removes its half-made container on it as on Ctrl+C.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

func main() {
	limitMemory()
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	err := cli.NewRoot().ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubehenge:", err)
		os.Exit(1)
	}
}
