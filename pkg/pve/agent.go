package pve

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
)

// Most tasks and commands, such as a configuration change, end within a fraction of a second; a
// download or a disk import takes minutes. So polling asks again soon at first and less often the
// longer one runs.
const (
	PollFirst   = 100 * time.Millisecond
	PollLongest = 2 * time.Second
	pollGrowth  = 2
)

// Backoff is the wait before the next poll after one of interval.
func Backoff(interval time.Duration) time.Duration {
	return min(pollGrowth*interval, PollLongest)
}

// ExecStatus is the guest agent's report on a command. go-proxmox's own type declares
// err-truncated a bool, which Proxmox VE sends as a number, so commands with long error output fail
// to decode there.
type ExecStatus struct {
	Exited       int    `json:"exited"`
	ExitCode     int    `json:"exitcode"`
	OutData      string `json:"out-data"`
	ErrData      string `json:"err-data"`
	OutTruncated any    `json:"out-truncated"`
	ErrTruncated any    `json:"err-truncated"`
}

// Output is what the command printed, on standard output and standard error.
func (s *ExecStatus) Output() string {
	return strings.TrimSpace(s.OutData + "\n" + s.ErrData)
}

// GuestExec runs a command in a VM through its QEMU guest agent, with input on its standard input,
// and waits for it to end. Proxmox VE asks VM.GuestAgent.Unrestricted on the VM for it, which is a
// root shell there.
func GuestExec(ctx context.Context, c *proxmox.Client, vm *proxmox.VirtualMachine, command []string, input string, timeout time.Duration) (*ExecStatus, error) {
	pid, err := vm.AgentExec(ctx, command, input)
	if err != nil {
		return nil, fmt.Errorf("starting it through the guest agent: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for interval := PollFirst; ; interval = Backoff(interval) {
		var status ExecStatus
		path := fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec-status?pid=%d", vm.Node, vm.VMID, pid)
		if err := c.Get(ctx, path, &status); err != nil {
			return nil, fmt.Errorf("reading its status from the guest agent: %w", err)
		}
		if status.Exited != 0 {
			return &status, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("still running after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for it to end: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}
