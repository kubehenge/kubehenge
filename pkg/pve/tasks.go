package pve

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
)

const (
	// TaskLogRead bounds how much of a failed task's log is read to find its end: Proxmox VE hands
	// a task's log out from its start only, and a download or an import logs a progress line every
	// few seconds.
	TaskLogRead = 2000
	// taskLogTail is how many of a failed task's last log lines its error carries: where Proxmox VE
	// says why, which the exit status alone often does not.
	taskLogTail = 15
)

// TaskError is a Proxmox VE task that ended other than OK, with the end of its log, which the
// task log in Proxmox VE's own interface shows under its UPID.
type TaskError struct {
	UPID       string
	ExitStatus string
	// Log is the end of the task's log. It is empty when it could not be read.
	Log []string
}

func (e *TaskError) Error() string {
	msg := fmt.Sprintf("Proxmox VE task %s failed: %s", e.UPID, e.ExitStatus)
	if len(e.Log) == 0 {
		return msg
	}
	return msg + "; its log ends:\n" + strings.Join(e.Log, "\n")
}

// Getter reads a path of the Proxmox VE API into out, as go-proxmox's Client.Get does.
type Getter func(ctx context.Context, path string, out any) error

// TaskFailed is the error of a task on node that ended with exitStatus, with the end of its log
// when get can read it. A log that cannot be read leaves the error with the exit status alone: the
// task's failure is what the caller reports, not the read.
func TaskFailed(ctx context.Context, get Getter, node, upid, exitStatus string) *TaskError {
	var lines []struct {
		N int    `json:"n"`
		T string `json:"t"`
	}
	path := fmt.Sprintf("/nodes/%s/tasks/%s/log?start=0&limit=%d", url.PathEscape(node), url.PathEscape(upid), TaskLogRead)
	if err := get(ctx, path, &lines); err != nil {
		return &TaskError{UPID: upid, ExitStatus: exitStatus}
	}
	byLine := make(map[int]string, len(lines))
	for _, l := range lines {
		byLine[l.N] = l.T
	}
	return &TaskError{UPID: upid, ExitStatus: exitStatus, Log: TaskLogEnd(byLine)}
}

// TaskLogEnd is the last lines of a task log by line number, as go-proxmox's Task.Log reads it.
func TaskLogEnd(log map[int]string) []string {
	numbers := slices.Sorted(maps.Keys(log))
	numbers = numbers[max(0, len(numbers)-taskLogTail):]
	end := make([]string, 0, len(numbers))
	for _, n := range numbers {
		end = append(end, log[n])
	}
	return end
}
