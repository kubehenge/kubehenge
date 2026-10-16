package main

import (
	"os"
	"path/filepath"
	"testing"
)

const mib = 1 << 20

func TestMemoryLimit(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		cgroup  string
		maxes   map[string]string
		meminfo string
		want    int64
	}{
		{"a pod's own limit", "0::/\n", map[string]string{".": "268435456\n"}, "MemTotal:       16777216 kB\n", 256 * mib},
		{
			"a slice's limit over the unit", "0::/system.slice/kubehenge.service\n",
			map[string]string{".": "max\n", "system.slice": "536870912\n", "system.slice/kubehenge.service": "max\n"},
			"MemTotal:       16777216 kB\n", 512 * mib,
		},
		{
			"a container's memory through lxcfs", "0::/system.slice/kubehenge.service\n",
			map[string]string{".": "max\n", "system.slice": "max\n", "system.slice/kubehenge.service": "max\n"},
			"MemFree:          524288 kB\nMemTotal:        1048576 kB\n", 1024 * mib,
		},
		{
			"a path above the namespace's root", "0::/../../elsewhere\n",
			map[string]string{".": "134217728\n"},
			"", 128 * mib,
		},
		{"cgroup v1 only", "4:memory:/user.slice\n", map[string]string{".": "134217728\n"}, "MemTotal: 2048 kB\n", 2 * mib},
		{"nothing known", "", nil, "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			root := filepath.Join(dir, "cgroup")
			for path, content := range c.maxes {
				write(t, filepath.Join(root, path, "memory.max"), content)
			}
			write(t, filepath.Join(dir, "self"), c.cgroup)
			write(t, filepath.Join(dir, "meminfo"), c.meminfo)
			got, ok := memoryLimit(root, filepath.Join(dir, "self"), filepath.Join(dir, "meminfo"))
			if got != c.want || ok != (c.want > 0) {
				t.Errorf("%d, %v; want %d", got, ok, c.want)
			}
		})
	}
}

func TestCgroupMax(t *testing.T) {
	t.Parallel()
	for content, want := range map[string]int64{"268435456\n": 268435456, "max\n": 0, "": 0, "-1": 0} {
		file := filepath.Join(t.TempDir(), "memory.max")
		write(t, file, content)
		if got, ok := cgroupMax(file); ok != (want > 0) || ok && got != want {
			t.Errorf("%q: %d, %v; want %d", content, got, ok, want)
		}
	}
}

func write(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
