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

func main() {
	limitMemory()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cli.NewRoot().ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubehenge:", err)
		os.Exit(1)
	}
}
