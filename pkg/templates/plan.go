// Package templates creates the Proxmox VE VM templates that Kubehenge's clusters are cloned from.
//
// A template is identified by its tags alone: Cluster API's Proxmox VE provider selects templates by
// tags and needs exactly one match. A template carries kubehenge and one tag naming its line, the
// distribution and node OS together, such as rke2-almalinux-10, talos-v1.14.6 or
// kubeadm-v1.37.1-rocky-10-fips. The node OS templates are built again to bring in their updates, and
// each build adds a tag of its own (build-<YYYYMMDDhhmm>, UTC), so that a cluster's machines move to
// a new build only when the cluster's patch names it. What only a reader needs, such as the
// hardware and the kernel, is in the template's notes.
package templates

import (
	"fmt"
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

// DistributionNames are the distributions as they are typed.
func DistributionNames() []string {
	names := make([]string, len(Distributions))
	for i, d := range Distributions {
		names[i] = string(d)
	}
	return names
}

// DefaultTalosVersion is the Talos release templates are made from unless another is asked for.
// renovate: datasource=github-releases depName=siderolabs/talos
const DefaultTalosVersion = "v1.14.6"

// Request asks for the template of one distribution.
type Request struct {
	Distribution Distribution
	TalosVersion string // Talos only
	// NodeOS is what the nodes of RKE2, k3s and kubeadm run. Empty means DefaultNodeOS.
	NodeOS            NodeOS
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
	// Bake, if set, boots the image to make the node of it before it becomes a template.
	Bake *Bake
}

// ImageRef names the disk image a template starts from. The source resolves it to a URL and checksum.
type ImageRef struct {
	Kind   ImageKind
	NodeOS NodeOS // the node OS whose image a bake boots
}

// ImageKind is where an image comes from.
type ImageKind string

const (
	// NodeOSImage is the image a node OS family publishes that a bake starts from.
	NodeOSImage ImageKind = "node-os"
)

// Bake is what makes a node of a family's image before it becomes a template.
type Bake struct {
	NodeOS       NodeOS
	Distribution Distribution
	// KubernetesVersion, for kubeadm, preinstalls kubeadm, the kubelet and containerd at that
	// version.
	KubernetesVersion string
	// TalosVersion, for Talos, is the release whose image the bake writes onto the template's disk.
	TalosVersion string
	// Talos is that release's pin. Like Proxy it is no part of the template's identity, so Plan
	// never sets it.
	Talos TalosPin
	// Proxy is what the builder's guests download through. It is no part of the template's
	// identity, so Plan never sets it.
	Proxy *GuestProxy
}

var (
	versionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	// Proxmox VE's tag format, which capmox also enforces on its selectors.
	tagRE = regexp.MustCompile(`^[a-z0-9_][a-z0-9_+.-]*$`)
)

// Plan works out the template a request needs. It does no I/O.
func Plan(req *Request) (Template, error) {
	switch req.Distribution {
	case Talos:
		v := req.TalosVersion
		if v == "" {
			v = DefaultTalosVersion
		}
		if !versionRE.MatchString(v) {
			return Template{}, fmt.Errorf("the Talos version %q is not of the form v1.2.3", v)
		}
		return newTemplate("kubehenge-talos-"+v, []string{kubehengeTag, talosTagPrefix + v},
			ImageRef{Kind: NodeOSImage, NodeOS: DefaultNodeOS.Base()},
			&Bake{NodeOS: DefaultNodeOS.Base(), Distribution: Talos, TalosVersion: v})

	case RKE2, K3s:
		os, err := checkNodeOS(req.Distribution, req.NodeOS)
		if err != nil {
			return Template{}, err
		}
		// RKE2 and k3s install themselves at boot, but each needs its own SELinux policy, so each
		// has its own template per node OS.
		return newTemplate(fmt.Sprintf("kubehenge-%s-%s", req.Distribution, os), []string{kubehengeTag, lineTag(req.Distribution, "", os)},
			ImageRef{Kind: NodeOSImage, NodeOS: os.Base()}, &Bake{NodeOS: os, Distribution: req.Distribution})

	case Kubeadm:
		os, err := checkNodeOS(req.Distribution, req.NodeOS)
		if err != nil {
			return Template{}, err
		}
		v := req.KubernetesVersion
		if !versionRE.MatchString(v) {
			return Template{}, fmt.Errorf("the Kubernetes version %q is not of the form v1.2.3", v)
		}
		return newTemplate(fmt.Sprintf("kubehenge-kubeadm-%s-%s", v, os), []string{kubehengeTag, lineTag(Kubeadm, v, os)},
			ImageRef{Kind: NodeOSImage, NodeOS: os.Base()}, &Bake{NodeOS: os, Distribution: Kubeadm, KubernetesVersion: v})
	}
	return Template{}, fmt.Errorf("unknown distribution %q: %s", req.Distribution, orList(DistributionNames()))
}

// checkNodeOS is the node OS a request asks for, DefaultNodeOS when it names none, once it is one
// templates of the distribution can be made of.
func checkNodeOS(d Distribution, o NodeOS) (NodeOS, error) {
	if o.Base() == (NodeOS{}) {
		o = NodeOS{Family: DefaultNodeOS.Family, Release: DefaultNodeOS.Release, FIPS: o.FIPS}
	}
	if o.FIPS {
		if err := FIPSDistribution(d); err != nil {
			return NodeOS{}, err
		}
	}
	if _, _, err := Available(o); err != nil {
		return NodeOS{}, err
	}
	return o, nil
}

// FIPSDistributions are the distributions whose nodes may run a node OS in FIPS mode: their own
// binaries use a FIPS 140 validated module.
var FIPSDistributions = []Distribution{RKE2}

// FIPSDistribution refuses FIPS mode to a distribution that cannot run in it.
func FIPSDistribution(d Distribution) error {
	if slices.Contains(FIPSDistributions, d) {
		return nil
	}
	names := make([]string, len(FIPSDistributions))
	for i, f := range FIPSDistributions {
		names[i] = f.Title()
	}
	return fmt.Errorf("%s has no FIPS mode. Choose %s for FIPS mode", d.Title(), orList(names))
}

// Title is the distribution as its project writes it.
func (d Distribution) Title() string {
	switch d {
	case RKE2:
		return "RKE2"
	case Talos:
		return "Talos"
	case K3s, Kubeadm:
	}
	return string(d)
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

// buildTagPrefix and buildLayout make a build's tag. Minutes in UTC sort as they read. A second
// build started in the same minute finds the first and keeps it.
const (
	buildTagPrefix = "build-"
	buildLayout    = "200601021504"
)

// Refreshed tells whether the template is built again for its updates: the node OS ones, which a
// bake makes of the family's packages as they stand. A Talos template is its release's image, the
// same at every build.
func (t *Template) Refreshed() bool {
	return t.Bake != nil && t.Bake.Distribution != Talos
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

// talosTagPrefix and a release make a Talos line's tag.
const talosTagPrefix = "talos-"

// lineTag names a node OS line: the distribution, for kubeadm its Kubernetes version, and the node
// OS, FIPS mode included.
func lineTag(d Distribution, kubernetesVersion string, os NodeOS) string {
	if kubernetesVersion != "" {
		return fmt.Sprintf("%s-%s-%s", d, kubernetesVersion, os)
	}
	return fmt.Sprintf("%s-%s", d, os)
}

// ParseTags splits a Proxmox VE tag string, which uses ';' (and accepts ',' and spaces) as
// separators, and keeps only the tags Kubehenge names a template by: kubehenge, a line's and a
// build's. A tag a user adds of their own is no part of the template's identity, and Cluster API's
// Proxmox VE provider matches the selector as a subset, so it changes nothing.
func ParseTags(s string) []string {
	tags := NormaliseTags(strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }))
	return slices.DeleteFunc(tags, func(tag string) bool { return !isIdentityTag(tag) })
}

// isIdentityTag tells whether a tag is one Kubehenge names a template by.
func isIdentityTag(tag string) bool {
	if tag == kubehengeTag {
		return true
	}
	if _, built := BuildOf([]string{tag}); built {
		return true
	}
	if v, ok := strings.CutPrefix(tag, talosTagPrefix); ok && versionRE.MatchString(v) {
		return true
	}
	_, ok := requestOfTag(tag)
	return ok
}

// RequestOf is the request a node OS line was planned from, read back from its tags.
func RequestOf(tags []string) (Request, bool) {
	for _, tag := range tags {
		if req, ok := requestOfTag(tag); ok {
			return req, true
		}
	}
	return Request{}, false
}

// requestOfTag reads a node OS line's tag, such as rke2-almalinux-10-fips or kubeadm-v1.37.1-rocky-10.
func requestOfTag(tag string) (Request, bool) {
	d, rest, ok := strings.Cut(tag, "-")
	if !ok {
		return Request{}, false
	}
	req := Request{Distribution: Distribution(d)}
	if req.Distribution == Kubeadm {
		v, os, cut := strings.Cut(rest, "-")
		if !cut || !versionRE.MatchString(v) {
			return Request{}, false
		}
		req.KubernetesVersion, rest = v, os
	} else if req.Distribution != RKE2 && req.Distribution != K3s {
		return Request{}, false
	}
	o, err := ParseNodeOS(rest)
	if _, known := FamilyOf(o.Family); err != nil || !known {
		return Request{}, false
	}
	req.NodeOS = o
	return req, true
}
