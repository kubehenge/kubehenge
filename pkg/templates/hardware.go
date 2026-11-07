package templates

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Hardware is what a template's VM options say about its clones' machine: they keep the CPU type and
// the template disk's options.
type Hardware struct {
	// CPUType is the QEMU CPU type.
	CPUType string
	// SSD presents the disk as an SSD, so that the guest does not treat it as a spinning one.
	SSD bool
	// Discard passes the guest's trims on to the storage, which gives back thin-provisioned space.
	Discard bool
	// IOThread serves the disk from a thread of its own, on a controller that has one per disk.
	IOThread bool
	// NUMA gives the guest a NUMA topology that follows its sockets.
	NUMA bool
}

// HardwareChoice is what a build is asked for. A zero field means the newest build's of the line,
// and the default for a line without one.
type HardwareChoice struct {
	CPUType                      string
	SSD, Discard, IOThread, NUMA *bool
}

// DefaultHardware is the hardware of a line's first build unless it is asked for another. The CPU
// type is the node OS's own (see Builder.CPUType).
func DefaultHardware(cpuType string) Hardware {
	return Hardware{CPUType: cpuType, SSD: true, Discard: true}
}

// NUMAMinCores is the fewest cores a machine of a NUMA template has: with one there is nothing to
// spread across nodes.
const NUMAMinCores = 2

// hardwareTagPrefix starts the tags that record a template's hardware. They describe a template and
// are no part of its line: Cluster API's Proxmox VE provider selects by the other tags
// (matchPolicy uniqueSubset, which still refuses two matches), and the line of a template is the same
// whatever hardware a build was made with.
const hardwareTagPrefix = "hw-"

// The tags of the options that are on. A CPU type has its own, hardwareTagPrefix + "cpu-" + the type.
const (
	hardwareCPUTagPrefix = hardwareTagPrefix + "cpu-"
	ssdTag               = hardwareTagPrefix + "ssd"
	discardTag           = hardwareTagPrefix + "discard"
	ioThreadTag          = hardwareTagPrefix + "iothread"
	numaTag              = hardwareTagPrefix + "numa"
)

// Tags are the tags that record the hardware, sorted.
func (h Hardware) Tags() []string {
	var tags []string
	if h.CPUType != "" {
		tags = append(tags, hardwareCPUTagPrefix+strings.ToLower(h.CPUType))
	}
	for _, o := range h.options() {
		if o.on {
			tags = append(tags, o.tag)
		}
	}
	return NormaliseTags(tags)
}

type hardwareOption struct {
	tag string
	on  bool
}

// IsHardwareTag tells whether a tag records hardware rather than naming the template's line.
func IsHardwareTag(tag string) bool {
	return strings.HasPrefix(tag, hardwareTagPrefix)
}

// HardwareOf reads the hardware a template's tags record, and whether they record any.
func HardwareOf(tags []string) (Hardware, bool) {
	var h Hardware
	var found bool
	for _, tag := range tags {
		if !IsHardwareTag(tag) {
			continue
		}
		found = true
		switch tag {
		case ssdTag:
			h.SSD = true
		case discardTag:
			h.Discard = true
		case ioThreadTag:
			h.IOThread = true
		case numaTag:
			h.NUMA = true
		default:
			if t, ok := strings.CutPrefix(tag, hardwareCPUTagPrefix); ok {
				h.CPUType = canonicalCPUType(t)
			}
		}
	}
	return h, found
}

// IdentityOf is the tags of a template that are not hardware: those a line and a build are named by.
func IdentityOf(tags []string) []string {
	return slices.DeleteFunc(slices.Clone(tags), IsHardwareTag)
}

// String is the hardware as one word for a listing: the CPU type, then the options that are on,
// separated by commas.
func (h Hardware) String() string {
	parts := []string{}
	if h.CPUType != "" {
		parts = append(parts, h.CPUType)
	}
	for _, o := range h.options() {
		if o.on {
			parts = append(parts, strings.TrimPrefix(o.tag, hardwareTagPrefix))
		}
	}
	return strings.Join(parts, ",")
}

// Describe lists the hardware as a person reads it: the CPU type, then the options that are on.
func (h Hardware) Describe() string {
	parts := []string{}
	if h.CPUType != "" {
		parts = append(parts, h.CPUType)
	}
	for _, o := range []struct {
		name string
		on   bool
	}{{"SSD", h.SSD}, {"discard", h.Discard}, {"IO thread", h.IOThread}, {"NUMA", h.NUMA}} {
		if o.on {
			parts = append(parts, o.name)
		}
	}
	return strings.Join(parts, ", ")
}

func (h Hardware) options() []hardwareOption {
	return []hardwareOption{{ssdTag, h.SSD}, {discardTag, h.Discard}, {ioThreadTag, h.IOThread}, {numaTag, h.NUMA}}
}

// diskOptions are the options of the template's disks, each led by a comma.
func (h Hardware) diskOptions() string {
	var s string
	if h.Discard {
		s += ",discard=on"
	}
	if h.SSD {
		s += ",ssd=1"
	}
	if h.IOThread {
		s += ",iothread=1"
	}
	return s
}

// controller is the disk controller. An IO thread per disk needs the single-disk controller.
func (h Hardware) controller() string {
	if h.IOThread {
		return "virtio-scsi-single"
	}
	return "virtio-scsi-pci"
}

// The x86-64 microarchitecture levels a CPU model needs of the host.
const (
	level1 = iota + 1
	level2
	level3
	level4
)

// cpuModels are the x86 CPU types Proxmox VE offers, each with the x86-64 microarchitecture level
// its CPU model needs of the host.
var cpuModels = map[string]int{
	"x86-64-v2": level2, "x86-64-v2-AES": level2, "x86-64-v3": level3, "x86-64-v4": level4,
	"host": level1, "max": level1, "kvm32": level1, "kvm64": level1, "qemu32": level1, "qemu64": level1,
	"486": level1, "pentium": level1, "pentium2": level1, "pentium3": level1, "coreduo": level1, "core2duo": level1, "Conroe": level1, "Penryn": level1,
	"Nehalem": level2, "Westmere": level2, "SandyBridge": level2, "IvyBridge": level2,
	"Haswell": level3, "Broadwell": level3, "Skylake-Client": level3,
	"Skylake-Server": level4, "Cascadelake-Server": level4, "Cooperlake": level4, "KnightsMill": level4,
	"Icelake-Client": level4, "Icelake-Server": level4, "SapphireRapids": level4, "GraniteRapids": level4,
	"SierraForest": level3, "ClearwaterForest": level3,
	"athlon": level1, "phenom": level1, "Opteron_G1": level1, "Opteron_G2": level1, "Opteron_G3": level1, "Opteron_G4": level2, "Opteron_G5": level2,
	"EPYC": level3, "EPYC-Rome": level3, "EPYC-Milan": level3, "EPYC-Genoa": level4, "EPYC-Turin": level4,
}

// cpuModelSuffix is what Proxmox VE appends to a model for a mitigation or a revision.
var cpuModelSuffix = regexp.MustCompile(`(?i)(-ibrs|-ibpb|-notsx|-v\d+)+$`)

// customCPUType is a CPU model an administrator defined in Proxmox VE. Tags are lower case, so only
// a lower case name survives a round trip through them.
var customCPUType = regexp.MustCompile(`^custom-[a-z0-9][a-z0-9_.-]*$`)

// baseCPUModel is the model of a CPU type without a mitigation or revision suffix, and whether
// Proxmox VE offers it. The comparison ignores case, since tags are lower case.
func baseCPUModel(cpuType string) (string, bool) {
	for model := range cpuModels {
		if strings.EqualFold(model, cpuType) {
			return model, true
		}
	}
	trimmed := cpuModelSuffix.ReplaceAllString(cpuType, "")
	for model := range cpuModels {
		if strings.EqualFold(model, trimmed) && trimmed != cpuType {
			return model, true
		}
	}
	return "", false
}

// canonicalCPUType is a CPU type as Proxmox VE spells it, from its lower case spelling in a tag.
func canonicalCPUType(lower string) string {
	for model := range cpuModels {
		if strings.EqualFold(model, lower) {
			return model
		}
	}
	base, ok := baseCPUModel(lower)
	if !ok {
		return lower
	}
	suffix := lower[len(base):]
	return base + canonicalSuffix(suffix)
}

// canonicalSuffix restores the case of a mitigation suffix.
func canonicalSuffix(lower string) string {
	r := strings.NewReplacer("-ibrs", "-IBRS", "-ibpb", "-IBPB", "-notsx", "-noTSX")
	return r.Replace(lower)
}

// ValidateCPUType refuses a CPU type Proxmox VE does not know. An empty one is the builder's to
// choose.
func ValidateCPUType(cpuType string) error {
	if cpuType == "" || customCPUType.MatchString(cpuType) {
		return nil
	}
	// Proxmox VE compares model names case-sensitively.
	if _, ok := baseCPUModel(cpuType); ok && canonicalCPUType(strings.ToLower(cpuType)) == cpuType {
		return nil
	}
	return fmt.Errorf("the CPU type %q is not one Proxmox VE offers. Use host, an x86-64-v2-AES style level, a model such as Haswell, or custom- and a lower case name", cpuType)
}

// CPUTypeLevel is the x86-64 microarchitecture level a CPU type needs of the Proxmox VE node its VMs
// run on. A custom model and host ask for none, since they name no level.
func CPUTypeLevel(cpuType string) int {
	if base, ok := baseCPUModel(cpuType); ok {
		return cpuModels[base]
	}
	return 1
}
