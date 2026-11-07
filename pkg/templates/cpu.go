package templates

import (
	"fmt"
	"slices"
	"strings"
)

// The x86-64 microarchitecture levels' CPU flags, as Linux names them in /proc/cpuinfo and
// Proxmox VE reports them for each node: each level needs its own and every lower level's.
var cpuLevelFlags = [...][]string{
	2: {"cx16", "lahf_lm", "popcnt", "sse4_1", "sse4_2", "ssse3"},
	3: {"avx", "avx2", "bmi1", "bmi2", "f16c", "fma", "abm", "movbe", "xsave"},
	4: {"avx512f", "avx512bw", "avx512cd", "avx512dq", "avx512vl"},
}

// CPULevelOf is the x86-64 microarchitecture level a CPU with the flags reaches, from 1 to 4.
func CPULevelOf(flags string) int {
	have := strings.Fields(flags)
	level := 1
	for l := 2; l < len(cpuLevelFlags); l++ {
		if !allIn(cpuLevelFlags[l], have) {
			break
		}
		level = l
	}
	return level
}

func allIn(want, have []string) bool {
	for _, f := range want {
		if !slices.Contains(have, f) {
			return false
		}
	}
	return true
}

// CPULevel is the x86-64 microarchitecture level a node OS needs of the Proxmox VE nodes its VMs
// run on: that of its release's CPU type.
func CPULevel(o NodeOS) int {
	_, r, err := ReleaseOf(o)
	if err != nil {
		return 1
	}
	return x86LevelOf(CPUTypeOf(r))
}

// TalosCPULevel is the x86-64 microarchitecture level Talos's templates need of the Proxmox VE
// nodes. Talos is its own OS, of no node OS family, and its templates are made with DefaultCPUType.
func TalosCPULevel() int {
	return x86LevelOf(DefaultCPUType)
}

// SettingLevel is the level a template's `cpu` setting presents, and false for a setting that
// names none: empty, `host` (the node's own CPU, whatever it is) or a custom model.
func SettingLevel(setting string) (int, bool) {
	model := SettingModel(setting)
	if model == "" || model == "host" || strings.HasPrefix(model, "custom-") {
		return 0, false
	}
	return CPUTypeLevel(model), true
}

// SettingModel is the CPU type a template's `cpu` setting names, without its flags.
func SettingModel(setting string) string {
	model, _, _ := strings.Cut(strings.TrimPrefix(setting, "cputype="), ",")
	return model
}

// x86LevelOf is the level a QEMU CPU type of Proxmox VE's x86-64-v* models presents.
func x86LevelOf(cpuType string) int {
	for l := len(cpuLevelFlags) - 1; l >= 2; l-- {
		model := fmt.Sprintf("x86-64-v%d", l)
		if cpuType == model || strings.HasPrefix(cpuType, model+"-") {
			return l
		}
	}
	return 1
}
