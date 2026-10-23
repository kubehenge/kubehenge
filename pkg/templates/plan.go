// Package templates creates the Proxmox VE VM templates that Kubehenge's clusters are cloned from.
//
// A template is identified by its tag set alone: Cluster API's Proxmox VE provider selects templates
// by tags and needs exactly one match, so each distribution and version maps to one unique set, its
// line. Ubuntu templates are built again to bring in their updates, and each build keeps a tag of
// its own beside the line's (build-<YYYYMMDDhhmm>, UTC), so that a cluster's machines move to a new
// build only when the cluster's patch names it.
package templates

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Distribution is a Kubernetes distribution Kubehenge creates clusters with.
type Distribution string

// The distributions, by the names their templates and ClusterClasses use.
const (
	Talos   Distribution = "talos"
	RKE2    Distribution = "rke2"
	K3s     Distribution = "k3s"
	Kubeadm Distribution = "kubeadm"
)

// kubehengeTag is on every template Kubehenge makes, which is how List tells them from others.
const kubehengeTag = "kubehenge"

// buildingTag marks, beside kubehengeTag, a VM the builder is still making into a template.
const buildingTag = "kubehenge-building"

// Distributions lists every supported distribution.
var Distributions = []Distribution{Talos, RKE2, K3s, Kubeadm}

const (
	// DefaultTalosVersion is the Talos release templates are made from unless another is asked for.
	// renovate: datasource=github-releases depName=siderolabs/talos
	DefaultTalosVersion = "v1.14.4"
	// DefaultOS is the cloud image RKE2, k3s and kubeadm templates are made from.
	DefaultOS = "ubuntu-26.04"
)

// UbuntuRelease is an Ubuntu release and the build of its cloud image templates are made from.
type UbuntuRelease struct {
	Codename string
	// Build names the build's directory on cloud-images.ubuntu.com, which never changes once
	// published, and whose SHA256SUMS Ubuntu signs.
	Build string
}

// OperatingSystems maps each supported cloud image to its Ubuntu release.
var OperatingSystems = map[string]UbuntuRelease{
	"ubuntu-26.04": {Codename: "resolute", Build: "20261016"},
	"ubuntu-24.04": {Codename: "noble", Build: "20261017"},
}

// Request asks for the template of one distribution.
type Request struct {
	Distribution      Distribution
	TalosVersion      string // Talos only
	OS                string // RKE2, k3s and kubeadm
	KubernetesVersion string // kubeadm only
}

// Template describes a template before it exists on Proxmox VE: a line, or one build of it.
type Template struct {
	Name string
	// LineName is the line's name, which every build's name starts with.
	LineName string
	// Tags are lower case and sorted, as Proxmox VE stores them.
	Tags []string
	// Image is where the template's disk comes from.
	Image ImageRef
	// Bake, if set, boots the image once to install what it lacks before it becomes a template.
	Bake *Bake
}

// ImageRef names the disk image a template starts from. The source resolves it to a URL and checksum.
type ImageRef struct {
	Kind    ImageKind
	Version string // Talos version, or the OS name for cloud images
}

// ImageKind is where an image comes from.
type ImageKind string

const (
	// TalosFactory is an image from the Talos Image Factory.
	TalosFactory ImageKind = "talos-factory"
	// UbuntuCloud is one of Ubuntu's cloud images.
	UbuntuCloud ImageKind = "ubuntu-cloud"
)

// Bake is what a template's image gets before it becomes a template.
type Bake struct {
	// KubernetesVersion, if set, preinstalls kubeadm, the kubelet and containerd at that version.
	KubernetesVersion string
}

var (
	versionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	// Proxmox VE's tag format, which capmox also enforces on its selectors.
	tagRE = regexp.MustCompile(`^[a-z0-9_][a-z0-9_+.-]*$`)
)

// Plan works out the template a request needs. It does no I/O.
func Plan(req Request) (Template, error) {
	switch req.Distribution {
	case Talos:
		v := req.TalosVersion
		if v == "" {
			v = DefaultTalosVersion
		}
		if !versionRE.MatchString(v) {
			return Template{}, fmt.Errorf("talos version %q is not of the form v1.2.3", v)
		}
		return newTemplate("kubehenge-talos-"+v, []string{kubehengeTag, "talos", "talos-" + v},
			ImageRef{Kind: TalosFactory, Version: v}, nil)

	case RKE2, K3s:
		os, err := checkOS(req.OS)
		if err != nil {
			return Template{}, err
		}
		// RKE2 and k3s install themselves at boot, so both share one template per OS.
		return newTemplate("kubehenge-"+os, []string{kubehengeTag, "cloud-image", os},
			ImageRef{Kind: UbuntuCloud, Version: os}, &Bake{})

	case Kubeadm:
		os, err := checkOS(req.OS)
		if err != nil {
			return Template{}, err
		}
		v := req.KubernetesVersion
		if !versionRE.MatchString(v) {
			return Template{}, fmt.Errorf("kubernetes version %q is not of the form v1.2.3", v)
		}
		return newTemplate("kubehenge-kubeadm-"+v+"-"+os, []string{kubehengeTag, "kubeadm", os, "kubernetes-" + v},
			ImageRef{Kind: UbuntuCloud, Version: os}, &Bake{KubernetesVersion: v})
	}
	return Template{}, fmt.Errorf("unknown distribution %q, expected one of %v", req.Distribution, Distributions)
}

func checkOS(os string) (string, error) {
	if os == "" {
		return DefaultOS, nil
	}
	if _, ok := OperatingSystems[os]; !ok {
		return "", fmt.Errorf("unsupported operating system %q, expected one of %v", os, sortedKeys(OperatingSystems))
	}
	return os, nil
}

func newTemplate(name string, tags []string, image ImageRef, bake *Bake) (Template, error) {
	tags = NormaliseTags(tags)
	for _, t := range tags {
		if !tagRE.MatchString(t) {
			return Template{}, fmt.Errorf("tag %q is not a valid Proxmox VE tag", t)
		}
	}
	return Template{Name: name, LineName: name, Tags: tags, Image: image, Bake: bake}, nil
}

// buildTagPrefix and buildLayout make a build's tag. Minutes in UTC sort as they read; a second
// build started in the same minute finds the first and keeps it.
const (
	buildTagPrefix = "build-"
	buildLayout    = "200601021504"
)

// Refreshed tells whether the template is built again for its updates: the Ubuntu ones, which a
// bake brings up to date. A Talos template is its release's image, the same at every build.
func (t *Template) Refreshed() bool {
	return t.Bake != nil
}

// Build is the build of the line t made at the given time, tagged and named for it.
func (t *Template) Build(at time.Time) (Template, error) {
	if !t.Refreshed() {
		return Template{}, fmt.Errorf("%s is not built again for updates", t.LineName)
	}
	if _, built := BuildOf(t.Tags); built {
		return Template{}, fmt.Errorf("%s is already a build", t.Name)
	}
	stamp := at.UTC().Format(buildLayout)
	out := *t
	out.Name = t.LineName + "-" + buildTagPrefix + stamp
	out.Tags = NormaliseTags(append(slices.Clone(t.Tags), buildTagPrefix+stamp))
	return out, nil
}

// BuildTag is the tag of the build made at the given time.
func BuildTag(at time.Time) string {
	return buildTagPrefix + at.UTC().Format(buildLayout)
}

// BuildOf is the time of the build a template's tags name, if they name one.
func BuildOf(tags []string) (time.Time, bool) {
	for _, tag := range tags {
		if stamp, ok := strings.CutPrefix(tag, buildTagPrefix); ok {
			if at, err := time.Parse(buildLayout, stamp); err == nil {
				return at, true
			}
		}
	}
	return time.Time{}, false
}

// LineOf is the tags of a template's line: its own without the build tag.
func LineOf(tags []string) []string {
	return slices.DeleteFunc(slices.Clone(tags), func(tag string) bool {
		_, built := BuildOf([]string{tag})
		return built
	})
}

// LineKey names a line by its tags, as the ClusterClasses' record of the builds does.
func LineKey(tags []string) string {
	return strings.Join(NormaliseTags(LineOf(tags)), ";")
}

// NormaliseTags lower-cases, sorts and de-duplicates tags, the form Proxmox VE stores them in.
func NormaliseTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ParseTags splits a Proxmox VE tag string, which uses ';' (and accepts ',' and spaces) as separators.
func ParseTags(s string) []string {
	return NormaliseTags(strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }))
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
