package pve

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/luthermonson/go-proxmox"
	"golang.org/x/text/encoding/charmap"
)

// Most tasks and commands, such as a configuration change, end within a fraction of a second. A
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

// guestFile is the guest agent's answer to a file read. Proxmox VE sends truncated as a number.
type guestFile struct {
	Content   string `json:"content"`
	Truncated any    `json:"truncated"`
}

// GuestFileRead reads up to limit bytes from the start of a file in a VM on node through its QEMU
// guest agent, and tells whether the file goes on past them. Proxmox VE asks only
// VM.GuestAgent.FileRead on the VM for it, and the guest's own policy decides which files the
// agent may open.
func GuestFileRead(ctx context.Context, c *proxmox.Client, node string, vmid int64, file string, limit int) (string, bool, error) {
	query := url.Values{"file": {file}, "count": {strconv.Itoa(limit)}}
	var f guestFile
	if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/agent/file-read?%s", url.PathEscape(node), vmid, query.Encode()), &f); err != nil {
		return "", false, fmt.Errorf("reading %s through the guest agent: %w", file, err)
	}
	truncated := false
	switch t := f.Truncated.(type) {
	case bool:
		truncated = t
	case float64:
		truncated = t != 0
	case string:
		truncated = t == "1" || t == "true"
	}
	return fileBytes(f.Content), truncated, nil
}

// fileBytes turns the content Proxmox VE sends back into the file's text. Proxmox VE decodes the
// agent's base64 into bytes and encodes them into its JSON one character per byte, so that a UTF-8
// file arrives encoded twice, its multi-byte characters as runs of U+0080 to U+00FF, among them C1
// control characters. Taken back to one byte per character, the content is the file's own bytes.
// A character cut in half where the read stopped, or a file that is not UTF-8, is then replaced by
// U+FFFD. Content with a character beyond U+00FF was not sent that way, and is left as it is.
func fileBytes(content string) string {
	raw, err := charmap.ISO8859_1.NewEncoder().String(content)
	if err != nil {
		return content
	}
	return strings.ToValidUTF8(raw, string(utf8.RuneError))
}
