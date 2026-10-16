package main

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	// limitPercent of the memory the process may use is Go's soft limit; the rest is for what the
	// limit does not count, such as the kernel's buffers for the process.
	limitPercent = 90
	kibibyte     = 1024
)

// limitMemory sets Go's soft memory limit to 90% of the memory the process may use, unless
// GOMEMLIMIT is set. Go otherwise lets the heap grow to twice what is live before collecting, past a
// limit it does not know of, into the kernel's out-of-memory kill; with it, the collector works
// harder instead.
func limitMemory() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	if limit, ok := memoryLimit("/sys/fs/cgroup", "/proc/self/cgroup", "/proc/meminfo"); ok {
		debug.SetMemoryLimit(limit / 100 * limitPercent)
	}
}

// memoryLimit is the smaller of the process's cgroup limit and the memory the system reports. A
// pod's limit sits on its own cgroup and a systemd unit's on it or its slice. A Proxmox VE
// container's sits on a parent the container cannot see, but lxcfs reports it as the container's
// total memory.
func memoryLimit(root, self, meminfo string) (int64, bool) {
	limit, ok := cgroupLimit(root, self)
	if total, known := memTotal(meminfo); known && (!ok || total < limit) {
		return total, true
	}
	return limit, ok
}

// cgroupLimit is the smallest memory.max of the process's cgroup v2 and of every parent up to the
// root its cgroup namespace shows.
func cgroupLimit(root, self string) (int64, bool) {
	raw, err := os.ReadFile(self) //nolint:gosec // a fixed system file, or a test's
	if err != nil {
		return 0, false
	}
	root = filepath.Clean(root)
	var limit int64
	for line := range strings.Lines(string(raw)) {
		path, ok := strings.CutPrefix(strings.TrimSpace(line), "0::")
		if !ok {
			continue
		}
		dir := filepath.Join(root, path)
		// A process moved out of its namespace's cgroup sees a path above the root.
		if rel, err := filepath.Rel(root, dir); err != nil || !filepath.IsLocal(rel) {
			dir = root
		}
		for {
			if n, set := cgroupMax(filepath.Join(dir, "memory.max")); set && (limit == 0 || n < limit) {
				limit = n
			}
			if dir == root {
				break
			}
			dir = filepath.Dir(dir)
		}
	}
	return limit, limit > 0
}

// cgroupMax reads a cgroup memory.max file, which holds a number of bytes or "max" for none.
func cgroupMax(file string) (int64, bool) {
	raw, err := os.ReadFile(file) //nolint:gosec // a fixed system file, or a test's
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return n, err == nil && n > 0
}

// memTotal reads MemTotal from /proc/meminfo, given in KiB.
func memTotal(file string) (int64, bool) {
	raw, err := os.ReadFile(file) //nolint:gosec // a fixed system file, or a test's
	if err != nil {
		return 0, false
	}
	for line := range strings.Lines(string(raw)) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "MemTotal:"); ok {
			kib, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kib * kibibyte, err == nil && kib > 0
		}
	}
	return 0, false
}
