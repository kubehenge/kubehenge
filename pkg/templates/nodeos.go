package templates

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// NodeOS is the operating system the nodes of an RKE2, k3s or kubeadm cluster run, as the cluster's
// nodeOS variable names it: a family and one of its releases.
type NodeOS struct {
	Family  string `json:"family"`
	Release string `json:"release"`
}

// DefaultNodeOS is what nodes run where a cluster names nothing else.
var DefaultNodeOS = NodeOS{Family: almaLinuxName, Release: "10"}

// String is the node OS as one word, such as almalinux-10, as template names and requests carry it.
func (o NodeOS) String() string { return o.Family + "-" + o.Release }

// Tag is the template tag of the node OS.
func (o NodeOS) Tag() string { return nodeOSTagPrefix + o.String() }

const nodeOSTagPrefix = "os-"

// ParseNodeOS reads a node OS written as one word, such as almalinux-10 or leap-16.1. A family's
// name holds no dash, so the first one ends it.
func ParseNodeOS(s string) (NodeOS, error) {
	family, release, ok := strings.Cut(s, "-")
	if !ok || family == "" || release == "" {
		return NodeOS{}, fmt.Errorf("node OS %q is not of the form <family>-<release>, such as %s", s, DefaultNodeOS)
	}
	return NodeOS{Family: family, Release: release}, nil
}

// NodeOSOf is the node OS a template's tags name, if they name one.
func NodeOSOf(tags []string) (NodeOS, bool) {
	for _, tag := range tags {
		if s, ok := strings.CutPrefix(tag, nodeOSTagPrefix); ok {
			if o, err := ParseNodeOS(s); err == nil {
				return o, true
			}
		}
	}
	return NodeOS{}, false
}

// MAC is the mandatory access control a family's nodes enforce.
type MAC string

// SELinux in enforcing mode, with the targeted policy.
const SELinux MAC = "selinux"

// BuildMode is how a template of a release is made.
type BuildMode uint8

const (
	// ImageMode builds the node as a bootable container image (bootc): a VM booted from the
	// family's cloud image composes it from the family's signed packages, adds what a node needs
	// and installs it onto the disk that becomes the template. The node's /usr is read-only, and
	// the image's digest is its whole operating system.
	ImageMode BuildMode = iota + 1
	// Transactional installs what a node needs into the family's immutable image with
	// transactional-update, as a new snapshot of its read-only root that the bake boots into.
	Transactional
)

func (m BuildMode) String() string {
	switch m {
	case ImageMode:
		return "image"
	case Transactional:
		return "transactional"
	}
	return fmt.Sprintf("BuildMode(%d)", m)
}

// Release is one release of a family that templates are made of.
type Release struct {
	// Name is the release in the nodeOS variable, such as 10.
	Name string
	// Build pins the published image a bake starts from, by its build in the publisher's file
	// names, which never changes once published. Renovate moves it.
	Build string
	// CPUType is the QEMU CPU type the release needs at the least; "" means DefaultCPUType.
	CPUType string
	Mode    BuildMode
	// Unreleased, when set, says why no template of the release can be built yet: the family is
	// complete, but its publisher has not released it.
	Unreleased string
}

// Family is an operating system family Kubehenge builds node templates of. Everything that differs
// between families lives behind it: where the image comes from and how its signature is checked,
// how a template is made of it, which CPU type it needs, and what a node enforces.
type Family interface {
	// Name is the family in the nodeOS variable and in template tags; it holds no dash.
	Name() string
	// Title is the family as people read it, such as AlmaLinux.
	Title() string
	// Releases are those templates are made of, newest first.
	Releases() []Release
	// MAC is what the family's nodes enforce.
	MAC() MAC
	// Image resolves the image a bake of the release boots, checked against the publisher's
	// signature over its checksum.
	Image(ctx context.Context, s *Sources, r Release) (Resolved, error)
	// Keys are the signing keys of the package repositories a bake of the release adds, fetched
	// and checked against their fingerprints before the bake starts.
	Keys(r Release, b Bake) []RepositoryKey
	// Recipe is what the bake of a template does: keys holds each of Keys's, armoured, by name.
	Recipe(r Release, b Bake, keys map[string]string) (Recipe, error)
}

// Recipe is the work of one bake, which the builder carries out through Proxmox VE and the guest
// agent.
type Recipe struct {
	// UserData is the cloud-config of the bake's first boot.
	UserData string
	// Checks run through the guest agent once cloud-init has finished; each must exit 0.
	Checks [][]string
	// Image is set in ImageMode: the bake's first boot built the node image onto the target disk,
	// which is then booted on its own for its first-boot checks.
	Image *NodeImageBoot
	// Reboot asks for a reboot after Checks, into what the bake installed, followed by
	// RebootChecks.
	Reboot       bool
	RebootChecks [][]string
	// Cleanup leaves the template as a fresh first boot.
	Cleanup [][]string
	// DiskGiB is the template's disk. Clones can only grow it, so it stays modest.
	DiskGiB int
	// BuilderDiskGiB is, in ImageMode, the disk of the VM that builds the image.
	BuilderDiskGiB int
	// MemoryMiB is what the bake's VM gets; 0 means the builder's default.
	MemoryMiB int
	// Log is the bake's own log in the guest, whose end a failed check reports.
	Log string
	// BootLog has the bake prove, once the node's checks pass, that the guest agent hands out its
	// boot log (BootLogFile) through Proxmox VE.
	BootLog bool
}

// NodeImageBoot is the boot of a node image the bake installed, with cloud-init as a node's first
// boot runs it.
type NodeImageBoot struct {
	UserData string
	Checks   [][]string
}

// RepositoryKey is the signing key of a package repository: where it is published, and the
// fingerprints it may have. It is fetched rather than embedded where its publisher extends its
// expiry in place.
type RepositoryKey struct {
	Name         string
	URL          string
	Fingerprints []string
}

// Families are the families templates are made of.
var Families = []Family{almaLinux{}, rocky{}, leap{}}

// FamilyOf finds a family by its name.
func FamilyOf(name string) (Family, bool) {
	i := slices.IndexFunc(Families, func(f Family) bool { return f.Name() == name })
	if i < 0 {
		return nil, false
	}
	return Families[i], true
}

// ReleaseOf finds a node OS's family and release, released or not.
func ReleaseOf(o NodeOS) (Family, Release, error) {
	f, ok := FamilyOf(o.Family)
	if !ok {
		return nil, Release{}, fmt.Errorf("unknown node OS family %q, expected one of %v", o.Family, familyNames())
	}
	i := slices.IndexFunc(f.Releases(), func(r Release) bool { return r.Name == o.Release })
	if i < 0 {
		return nil, Release{}, fmt.Errorf("%s has no release %q Kubehenge builds, expected one of %v", f.Title(), o.Release, releaseNames(f))
	}
	return f, f.Releases()[i], nil
}

// Available finds a node OS that templates can be made of, or says why not.
func Available(o NodeOS) (Family, Release, error) {
	f, r, err := ReleaseOf(o)
	if err != nil {
		return nil, Release{}, err
	}
	if r.Unreleased != "" {
		return nil, Release{}, fmt.Errorf("%s %s: %s", f.Title(), r.Name, r.Unreleased)
	}
	return f, r, nil
}

// NodeOSInfo describes a node OS to the interface and the command line.
type NodeOSInfo struct {
	NodeOS

	Title   string `json:"title"`
	CPUType string `json:"cpuType"`
	MAC     MAC    `json:"mac"`
	Default bool   `json:"default,omitempty"`
	// Unreleased says why the node OS cannot be chosen yet.
	Unreleased string `json:"unreleased,omitempty"`
}

// NodeOSes lists every family's releases, in the families' order and newest first.
func NodeOSes() []NodeOSInfo {
	var out []NodeOSInfo
	for _, f := range Families {
		for _, r := range f.Releases() {
			o := NodeOS{Family: f.Name(), Release: r.Name}
			out = append(out, NodeOSInfo{
				NodeOS: o, Title: f.Title() + " " + r.Name, CPUType: CPUTypeOf(r), MAC: f.MAC(),
				Default: o == DefaultNodeOS, Unreleased: r.Unreleased,
			})
		}
	}
	return out
}

// AvailableNodeOSes are the node OSes templates can be made of, as one word each.
func AvailableNodeOSes() []string {
	var out []string
	for _, o := range NodeOSes() {
		if o.Unreleased == "" {
			out = append(out, o.String())
		}
	}
	return out
}

// CPUTypeOf is the QEMU CPU type a release's templates and machines get.
func CPUTypeOf(r Release) string {
	if r.CPUType != "" {
		return r.CPUType
	}
	return DefaultCPUType
}

func familyNames() []string {
	out := make([]string, len(Families))
	for i, f := range Families {
		out[i] = f.Name()
	}
	return out
}

func releaseNames(f Family) []string {
	releases := f.Releases()
	out := make([]string, len(releases))
	for i, r := range releases {
		out[i] = r.Name
	}
	return out
}
