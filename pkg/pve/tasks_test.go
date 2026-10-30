package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const failedUPID = "UPID:pve1:0000A1B2:00C3D4E5:6700AABB:qmclone:101:kubehenge@pve!controller:"

func TestTaskFailedCarriesTheEndOfItsLog(t *testing.T) {
	t.Parallel()
	var asked string
	get := func(_ context.Context, path string, out any) error {
		asked = path
		var lines []map[string]any
		for n := 1; n <= 40; n++ {
			lines = append(lines, map[string]any{"n": n, "t": fmt.Sprintf("line %d", n)})
		}
		lines = append(lines, map[string]any{"n": 41, "t": "TASK ERROR: storage 'tank' does not exist"})
		raw, err := json.Marshal(lines)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, out)
	}
	err := TaskFailed(t.Context(), get, "pve1", failedUPID, "storage 'tank' does not exist")
	if !strings.HasPrefix(asked, "/nodes/pve1/tasks/") || !strings.Contains(asked, "/log?start=0&limit=") {
		t.Errorf("read %s, want the task's log", asked)
	}
	if len(err.Log) != taskLogTail || err.Log[len(err.Log)-1] != "TASK ERROR: storage 'tank' does not exist" || err.Log[0] != "line 27" {
		t.Errorf("log = %q, want its last %d lines in order", err.Log, taskLogTail)
	}
	msg := err.Error()
	for _, want := range []string{failedUPID, "failed: storage 'tank' does not exist", "its log ends:\n", "\nTASK ERROR"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if _, ok := errors.AsType[*TaskError](fmt.Errorf("cloning: %w", err)); !ok {
		t.Error("a wrapped task failure is no longer a *TaskError")
	}
}

func TestTaskFailedWithoutItsLog(t *testing.T) {
	t.Parallel()
	get := func(context.Context, string, any) error { return ErrForbidden }
	err := TaskFailed(t.Context(), get, "pve1", failedUPID, "unexpected status")
	if want := "Proxmox VE task " + failedUPID + " failed: unexpected status"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestTaskLogEndOrdersByLine(t *testing.T) {
	t.Parallel()
	got := TaskLogEnd(map[int]string{2: "c", 0: "a", 1: "b"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("TaskLogEnd = %q, want a,b,c", got)
	}
}
