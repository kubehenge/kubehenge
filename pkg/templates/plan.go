// Package templates creates the Proxmox VE VM templates that Kubehenge's clusters are cloned from.
//
// A template is identified by its tag set alone: Cluster API's Proxmox VE provider selects templates
// by tags and needs exactly one match, so each distribution and version maps to one unique set.
package templates

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
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

// Distributions lists every supported distribution.
var Distributions = []Distribution{Talos, RKE2, K3s, Kubeadm}

const (
	// DefaultTalosVersion is the Talos release templates are made from unless another is asked for.
	DefaultTalosVersion = "v1.14.4"
	// DefaultOS is the cloud image RKE2, k3s and kubeadm templates are made from.
	DefaultOS = "ubuntu-26.04"
)

// OperatingSystems maps each supported cloud image to its Ubuntu release codename.
var OperatingSystems = map[string]string{
	"ubuntu-26.04": "resolute",
	"ubuntu-24.04": "noble",
}

// Request asks for the template of one distribution.
type Request struct {
	Distribution      Distribution
	TalosVersion      string // Talos only
	OS                string // RKE2, k3s and kubeadm
	KubernetesVersion string // kubeadm only
}

// Template describes a template before it exists on Proxmox VE.
type Template struct {
	Name string
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
	return Template{Name: name, Tags: tags, Image: image, Bake: bake}, nil
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

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
