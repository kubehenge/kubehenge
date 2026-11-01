package provisioner

import (
	"context"
	"encoding/json"
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
	// what it printed. A script that exits other than 0 is an error.
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

// JobGuests runs each command through a token Job, whose restore token alone may use the guest
// agent. The controller's own token tells whether a VM runs.
type JobGuests struct {
	Jobs    TokenRunner
	Proxmox *proxmox.Client
}

var _ Guests = &JobGuests{}

// GuestRequest is one command for a VM's guest agent.
type GuestRequest struct {
	VMID    int64         `json:"vmid"`
	Script  string        `json:"script"`
	Timeout time.Duration `json:"timeout"`
}

// Running implements Guests.
func (g *JobGuests) Running(ctx context.Context, vmid int64) (bool, error) {
	return (&ProxmoxGuests{Client: g.Proxmox}).Running(ctx, vmid)
}

// Run implements Guests.
func (g *JobGuests) Run(ctx context.Context, vmid int64, script string, timeout time.Duration) (string, error) {
	return g.Jobs.Run(ctx, GuestRequest{VMID: vmid, Script: script, Timeout: timeout}, timeout) //nolint:wrapcheck // as ProxmoxGuests gives it, naming the VM
}

// ServeGuestRequest runs a GuestRequest with the restore token.
func ServeGuestRequest(ctx context.Context, restore *proxmox.Client, raw []byte) (string, error) {
	var req GuestRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("the request: %w", err)
	}
	if req.VMID <= 0 || req.Timeout <= 0 || req.Timeout > guestStepTimeout {
		return "", fmt.Errorf("the request: VM %d for %s", req.VMID, req.Timeout)
	}
	return (&ProxmoxGuests{Client: restore}).Run(ctx, req.VMID, req.Script, req.Timeout)
}

func (g *ProxmoxGuests) locate(ctx context.Context, vmid int64) (string, bool, error) {
	return locateVM(ctx, g.Client, vmid)
}

// GuestFiles reads files inside clusters' VMs.
type GuestFiles interface {
	// ReadFile reads up to limit bytes from the start of a file in a VM, and tells whether the
	// file goes on past them.
	ReadFile(ctx context.Context, vmid int64, file string, limit int) (string, bool, error)
	// Running tells whether a VM is running.
	Running(ctx context.Context, vmid int64) (bool, error)
}

// ProxmoxGuestFiles reads files through the VMs' QEMU guest agents, with the controller's own
// token, which holds VM.GuestAgent.FileRead on the workload pool: no commands, and only the files
// the node's agent policy lets it open.
type ProxmoxGuestFiles struct {
	Client *proxmox.Client
}

var _ GuestFiles = &ProxmoxGuestFiles{}

// Running implements GuestFiles.
func (g *ProxmoxGuestFiles) Running(ctx context.Context, vmid int64) (bool, error) {
	_, running, err := locateVM(ctx, g.Client, vmid)
	return running, err
}

// ReadFile implements GuestFiles.
func (g *ProxmoxGuestFiles) ReadFile(ctx context.Context, vmid int64, file string, limit int) (string, bool, error) {
	node, _, err := locateVM(ctx, g.Client, vmid)
	if err != nil {
		return "", false, err
	}
	content, truncated, err := pve.GuestFileRead(ctx, g.Client, node, vmid, file, limit)
	if err != nil {
		return "", false, fmt.Errorf("VM %d: %w", vmid, err)
	}
	return content, truncated, nil
}

// locateVM finds the node a VM is on, and whether it runs.
func locateVM(ctx context.Context, c *proxmox.Client, vmid int64) (string, bool, error) {
	var vms []struct {
		VMID   int64  `json:"vmid"`
		Node   string `json:"node"`
		Status string `json:"status"`
	}
	if err := c.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return "", false, fmt.Errorf("listing the VMs: %w", err)
	}
	for _, vm := range vms {
		if vm.VMID == vmid {
			return vm.Node, vm.Status == guestRunning, nil
		}
	}
	return "", false, fmt.Errorf("VM %d is not there, or not one this token may see", vmid)
}

// tail is the end of s, at most n bytes of it.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
