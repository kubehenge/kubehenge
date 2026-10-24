package provisioner

import (
	"context"
	"fmt"
	"time"

	"github.com/luthermonson/go-proxmox"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// outputTail is how much of what a command printed an error quotes: its end, where it failed.
const outputTail = 1500

// Guests runs commands inside clusters' VMs.
type Guests interface {
	// Run runs a bash script as root in a VM, the script on bash's standard input, and returns
	// what it printed; a script that exits other than 0 is an error.
	Run(ctx context.Context, vmid int64, script string, timeout time.Duration) (string, error)
	// Running tells whether a VM is running.
	Running(ctx context.Context, vmid int64) (bool, error)
}

// ProxmoxGuests runs commands through the VMs' QEMU guest agents, signed in with a token that holds
// VM.Audit and VM.GuestAgent.Unrestricted on the workload pool.
type ProxmoxGuests struct {
	Client *proxmox.Client
}

var _ Guests = &ProxmoxGuests{}

// Running implements Guests.
func (g *ProxmoxGuests) Running(ctx context.Context, vmid int64) (bool, error) {
	_, running, err := g.locate(ctx, vmid)
	return running, err
}

// Run implements Guests.
func (g *ProxmoxGuests) Run(ctx context.Context, vmid int64, script string, timeout time.Duration) (string, error) {
	node, _, err := g.locate(ctx, vmid)
	if err != nil {
		return "", err
	}
	vm, err := (&proxmox.Node{}).New(g.Client, node).VirtualMachine(ctx, int(vmid))
	if err != nil {
		return "", fmt.Errorf("reading VM %d: %w", vmid, err)
	}
	status, err := pve.GuestExec(ctx, g.Client, vm, []string{"/bin/bash", "-s"}, script, timeout)
	if err != nil {
		return "", fmt.Errorf("VM %d: %w", vmid, err)
	}
	out := status.Output()
	if status.ExitCode != 0 {
		return out, fmt.Errorf("VM %d: exited %d: %s", vmid, status.ExitCode, tail(out, outputTail))
	}
	return out, nil
}

// locate finds the node a VM is on, and whether it runs.
func (g *ProxmoxGuests) locate(ctx context.Context, vmid int64) (string, bool, error) {
	var vms []struct {
		VMID   int64  `json:"vmid"`
		Node   string `json:"node"`
		Status string `json:"status"`
	}
	if err := g.Client.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return "", false, fmt.Errorf("listing the VMs: %w", err)
	}
	for _, vm := range vms {
		if vm.VMID == vmid {
			return vm.Node, vm.Status == "running", nil
		}
	}
	return "", false, fmt.Errorf("VM %d is not there, or not one the restore token may see", vmid)
}

// tail is the end of s, at most n bytes of it.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
