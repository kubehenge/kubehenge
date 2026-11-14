package appliance

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// tofuConfig is what the root config the appliance writes and the module it calls declare, read
// from their files, so that an address the fake OpenTofu takes is checked as the real one checks it.
type tofuConfig struct {
	rootVars, moduleVars, resources, modules map[string]bool
}

var (
	tofuBlock   = regexp.MustCompile(`(?m)^(resource|data|module|variable)\s+"([^"]+)"(?:\s+"([^"]+)")?\s*\{`)
	moduleInput = regexp.MustCompile(`(?m)^ {2}(\w+)\s+=`)
	tofuIndex   = regexp.MustCompile(`\[[^\]]*\]$`)
)

func readTofuConfig(t *testing.T) tofuConfig {
	t.Helper()
	c := tofuConfig{map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}}
	files, err := filepath.Glob(filepath.Join("..", "..", "management-cluster", "*.tf"))
	if err != nil || len(files) == 0 {
		t.Fatalf("the module's files: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // the repository's own module
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tofuBlock.FindAllStringSubmatch(string(raw), -1) {
			switch m[1] {
			case "resource":
				c.resources[m[2]+"."+m[3]] = true
			case "module":
				c.modules[m[2]] = true
			case "variable":
				c.moduleVars[m[2]] = true
			}
		}
	}
	root := fmt.Sprintf(rootConfig, "../management-cluster")
	calls := 0
	for _, m := range tofuBlock.FindAllStringSubmatch(root, -1) {
		switch m[1] {
		case "variable":
			c.rootVars[m[2]] = true
		case "module":
			calls++
			if m[2] != strings.TrimSuffix(strings.TrimPrefix(moduleAddress, "module."), ".") {
				t.Fatalf("the root config calls module %q, not the one moduleAddress names", m[2])
			}
		default:
			t.Fatalf("the root config declares %s %q of its own", m[1], m[2])
		}
	}
	if calls != 1 {
		t.Fatalf("the root config calls %d modules", calls)
	}
	_, rest, found := strings.Cut(root, "module \"management\"")
	block, _, closed := strings.Cut(rest, "\n}\n")
	if !found || !closed {
		t.Fatal("the root config has no whole module \"management\" block")
	}
	for _, m := range moduleInput.FindAllStringSubmatch(block, -1) {
		if m[1] != "source" && !c.moduleVars[m[1]] {
			t.Errorf("the root config passes %s, which the module does not declare", m[1])
		}
	}
	return c
}

// address tells why addr names nothing in the root config's state, or "" when it names something.
func (c tofuConfig) address(addr string) string {
	rest, ok := strings.CutPrefix(addr, moduleAddress)
	if !ok {
		return "outside module.management, where the root config holds nothing"
	}
	rest = tofuIndex.ReplaceAllString(rest, "")
	if name, ok := strings.CutPrefix(rest, "module."); ok {
		if !c.modules[name] {
			return "no such module call in the module"
		}
		return ""
	}
	if !c.resources[rest] {
		return "no such resource in the module"
	}
	return ""
}

// check returns what in one call's arguments OpenTofu would refuse, or match nothing with.
func (c tofuConfig) check(args []string) []string {
	var bad []string
	for i, arg := range args {
		switch {
		case strings.HasPrefix(arg, "-target="), strings.HasPrefix(arg, "-replace="):
			if why := c.address(arg[strings.Index(arg, "=")+1:]); why != "" {
				bad = append(bad, arg+": "+why)
			}
		case strings.HasPrefix(arg, "-var="):
			name, _, _ := strings.Cut(strings.TrimPrefix(arg, "-var="), "=")
			if !c.rootVars[name] {
				bad = append(bad, arg+": the root config declares no variable "+name)
			}
		case i >= 2 && args[0] == "state" && !strings.HasPrefix(arg, "-"):
			if why := c.address(arg); why != "" {
				bad = append(bad, "state "+args[1]+" "+arg+": "+why)
			}
		}
	}
	return bad
}

func tofuCalls(t *testing.T, a *Appliance) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(a.Dir, "fake-tofu.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var calls [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var rec struct {
			Args []string `json:"args"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, rec.Args)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return calls
}

// Every address and variable the appliance hands OpenTofu names something the root config it
// writes declares: Remove, with the backup volume kept or deleted, and the recreation's targets.
func TestTofuAddressesNameTheRootConfig(t *testing.T) {
	t.Parallel()
	c := readTofuConfig(t)
	for nodes := 1; nodes <= 5; nodes++ {
		targets := recreateTargets(nodes)
		if bad := c.check(targets); len(bad) > 0 {
			t.Errorf("recreating %d nodes: %q", nodes, bad)
		}
		if !slices.Contains(targets, fmt.Sprintf("-target=%smodule.talos_node_%d", moduleAddress, nodes-1)) ||
			!slices.Contains(targets, "-target="+moduleAddress+"proxmox_virtual_environment_vm.node") {
			t.Errorf("recreating %d nodes targets %q, not the VMs and the last node", nodes, targets)
		}
	}
	for _, deleteVolume := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete volume %v", deleteVolume), func(t *testing.T) {
			t.Parallel()
			a, ready := newAppliance(t)
			if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
				t.Fatal(err)
			}
			waitFor(t, a)
			for name, body := range map[string]string{
				filepath.Join(a.Dir, "management", "terraform.tfstate"): "{}",
				filepath.Join(a.Dir, "state-list"):                      moduleAddress + "proxmox_virtual_environment_vm.node[0]\n" + backupVolumeAddress + "\n",
			} {
				if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.Remove("correct horse battery staple", backupsOf[deleteVolume], ticket(), func(context.Context, *Job) error { return nil }, func() {}); err != nil {
				t.Fatal(err)
			}
			if j := waitFor(t, a); j.State != StateSucceeded {
				t.Fatalf("remove: %+v", j)
			}
			var lifted, dropped bool
			for _, args := range tofuCalls(t, a) {
				if bad := c.check(args); len(bad) > 0 {
					t.Errorf("tofu %q: %q", args, bad)
				}
				lifted = lifted || args[0] == "apply" && slices.Contains(args, "-target="+backupVolumeAddress) && slices.Contains(args, "-var=backup_volume_protection=false")
				dropped = dropped || slices.Equal(args, []string{"state", "rm", backupVolumeAddress})
			}
			if lifted != deleteVolume || dropped == deleteVolume {
				t.Errorf("protection lifted %v, volume left the state %v, with the volume deleted %v", lifted, dropped, deleteVolume)
			}
		})
	}
}

// A setup that stopped before the backups VM leaves a state without it. Removing that cluster with
// its backups runs no apply, which would make the VM only for the destroy to delete it.
func TestRemovingAPartMadeClusterAppliesNothing(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	for name, body := range map[string]string{
		filepath.Join(a.Dir, "management", "terraform.tfstate"): "{}",
		filepath.Join(a.Dir, "state-list"):                      moduleAddress + "proxmox_virtual_environment_pool.management\n",
	} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	applied := len(tofuCalls(t, a))
	if err := a.Remove("correct horse battery staple", DeleteBackups, ticket(), func(context.Context, *Job) error { return nil }, func() {}); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != StateSucceeded {
		t.Fatalf("remove: %+v", j)
	}
	for _, args := range tofuCalls(t, a)[applied:] {
		if args[0] == "apply" {
			t.Errorf("the removal ran tofu %q", args)
		}
	}
}
