package provisioner

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Annotations on a ClusterClass naming its distribution's pod and service ranges, used for a Cluster
// that sets none.
const (
	DefaultPodsAnnotation     = "kubehenge.dev/default-pods"
	DefaultServicesAnnotation = "kubehenge.dev/default-services"
)

// Candidate IPv4 ranges, tried after the distribution's own: Kubernetes' and the k3s family's usual
// ones, then private ranges less often in use for networks. A Cilium cluster, which may join a
// Cluster Mesh, keeps clear of every other cluster's ranges too, so it can go on to every /16 of
// 10.0.0.0/8.
var (
	podCandidates     = []string{"10.244.0.0/16", "10.42.0.0/16", "172.21.0.0/16", "10.201.0.0/16"}
	serviceCandidates = []string{"10.96.0.0/16", "10.43.0.0/16", "172.20.0.0/16", "10.200.0.0/16"}
	meshCandidates    = func() []string {
		const sixteens = 256
		c := make([]string, 0, sixteens)
		for i := range sixteens {
			c = append(c, fmt.Sprintf("10.%d.0.0/16", i))
		}
		return c
	}()
)

// IPv6 ranges, the same for every distribution: unique local addresses (RFC 4193) under a global ID
// of Kubehenge's own, which a site's own unique local or global addresses do not collide with. Pods
// take a /56, which gives each of 256 nodes the /64 the controller manager hands out. Services take
// a /112, the largest k3s accepts. The candidates go on through the /56s
// and /112s beneath, for a Cilium cluster that keeps clear of every other cluster's ranges.
const (
	DefaultPodsIPv6     = "fd5e:4b1c:9a20::/56"
	DefaultServicesIPv6 = "fd5e:4b1c:9a21::/112"
	// The limits of an IPv6 range's prefix length. A pod range is at most 16 bits above the nodes'
	// /64s, as the controller manager allows.
	minPodBitsIPv6     = 48
	maxPodBitsIPv6     = 63
	minServiceBitsIPv6 = 112
	maxServiceBitsIPv6 = 120
)

var (
	podCandidatesIPv6 = func() []string {
		const blocks = 256
		c := make([]string, 0, blocks)
		for i := range blocks {
			c = append(c, fmt.Sprintf("fd5e:4b1c:9a20:%x00::/56", i))
		}
		return c
	}()
	serviceCandidatesIPv6 = func() []string {
		const blocks = 256
		c := make([]string, 0, blocks)
		for i := range blocks {
			c = append(c, fmt.Sprintf("fd5e:4b1c:9a21:%x::/112", i))
		}
		return c
	}()
)

// checkRanges refuses a cluster whose pod or service ranges are no networks, are not one of each of
// its address families in their order, overlap each other, or overlap its nodes' networks: packets
// for the nodes would be routed into the cluster instead, and the cluster fails in ways that are
// hard to trace back to the ranges. An IPv6 range has to be of a size the controller manager and
// the API server take.
func checkRanges(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	subnets, err := nodeSubnets(cluster)
	if err != nil {
		return err
	}
	var problems []string
	var taken []netip.Prefix
	for _, r := range []struct {
		what   string
		blocks []string
		def    string
	}{
		{"pod", cluster.Spec.ClusterNetwork.Pods.CIDRBlocks, class.Annotations[DefaultPodsAnnotation]},
		{"service", cluster.Spec.ClusterNetwork.Services.CIDRBlocks, class.Annotations[DefaultServicesAnnotation]},
	} {
		blocks := r.blocks
		if len(blocks) == 0 && r.def != "" && len(subnets) == 1 && subnets[0].family == familyIPv4 {
			blocks = []string{r.def}
		}
		prefixes, perr := parseRanges(r.what, blocks)
		if perr != nil {
			return perr
		}
		if msg := familyOrderProblem(r.what, prefixes, subnets); msg != "" {
			return errors.New(msg)
		}
		for _, p := range prefixes {
			problems = append(problems, rangeProblems(r.what, p, subnets, taken)...)
		}
		taken = append(taken, prefixes...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s. Set spec.clusterNetwork to other ranges", strings.Join(problems, ", "))
	}
	return nil
}

// parseRanges reads a cluster's pod or service ranges, each a network without host bits.
func parseRanges(what string, blocks []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(blocks))
	for _, b := range blocks {
		p, err := netip.ParsePrefix(b)
		if err != nil || p != p.Masked() {
			return nil, fmt.Errorf("%s range: %s is no network such as 10.244.0.0/16 or %s, without host bits", what, b, DefaultPodsIPv6)
		}
		prefixes = append(prefixes, p)
	}
	return prefixes, nil
}

// rangeProblems are where a range is of a size the cluster cannot use, or overlaps the nodes'
// networks or a pod range taken before it.
func rangeProblems(what string, p netip.Prefix, subnets []nodeSubnet, taken []netip.Prefix) []string {
	problems := rangeSizeProblems(what, p)
	for _, s := range subnets {
		if p.Overlaps(s.prefix) {
			problems = append(problems, fmt.Sprintf("%s range %s overlaps the nodes' network %s", what, p, s.prefix))
		}
	}
	if i := slices.IndexFunc(taken, p.Overlaps); i >= 0 {
		problems = append(problems, fmt.Sprintf("the %s range %s overlaps the pod range %s", what, p, taken[i]))
	}
	return problems
}

// familyOrderProblem says where ranges are not one of each of the nodes' families in their order,
// which is the order the distributions take the primary family from.
func familyOrderProblem(what string, ranges []netip.Prefix, subnets []nodeSubnet) string {
	want := make([]string, 0, len(subnets))
	for _, s := range subnets {
		want = append(want, s.family)
	}
	got := make([]string, 0, len(ranges))
	for _, p := range ranges {
		got = append(got, familyOf(p.Addr()))
	}
	if len(ranges) == 0 || slices.Equal(got, want) {
		return ""
	}
	return fmt.Sprintf("the %s ranges are %s, and the cluster's families %s: give one %s range of each family, in that order, in spec.clusterNetwork",
		what, strings.Join(got, " and "), strings.Join(want, " and "), what)
}

// rangeSizeProblems are where an IPv6 range is of a size the cluster cannot use.
func rangeSizeProblems(what string, p netip.Prefix) []string {
	if p.Addr().Is4() {
		return nil
	}
	lo, hi := minPodBitsIPv6, maxPodBitsIPv6
	if what == "service" {
		lo, hi = minServiceBitsIPv6, maxServiceBitsIPv6
	}
	if p.Bits() < lo || p.Bits() > hi {
		return []string{fmt.Sprintf("the IPv6 %s range %s has a prefix length outside /%d to /%d", what, p, lo, hi)}
	}
	return nil
}

// ClusterDefaulter gives a new Kubehenge cluster the pod and service ranges it names none of: its
// distribution's own where they are clear of the nodes' network, or else the first clear
// candidates, and for a cluster that runs Cilium, which may join a Cluster Mesh later, ranges clear
// of every other cluster's too, since a peer's pod addresses must not be taken for its own pods' or
// services'. The distributions read the ranges from the Cluster, kubeadm's podSubnet among them,
// which the Tigera operator does not install Calico without.
type ClusterDefaulter struct {
	Hooks *Hooks
}

var _ admission.Defaulter[*clusterv1.Cluster] = &ClusterDefaulter{}

// Default implements admission.Defaulter.
func (d *ClusterDefaulter) Default(ctx context.Context, c *clusterv1.Cluster) error {
	n := &c.Spec.ClusterNetwork
	if !Owns(c, d.Hooks.ClassNamespace) || !c.DeletionTimestamp.IsZero() || len(n.Pods.CIDRBlocks) > 0 && len(n.Services.CIDRBlocks) > 0 {
		return nil
	}
	class, err := d.Hooks.classOf(c)(ctx)
	if err != nil || class == nil {
		return err
	}
	subnets, err := nodeSubnets(c)
	if err != nil {
		// Without a network there is nothing to keep clear of. The class's schema refuses it.
		return nil //nolint:nilerr // refused by the checks of the variables
	}
	var others *otherRanges
	if runsCiliumOf(c, class) {
		if others, err = d.otherRanges(ctx, c); err != nil {
			return err
		}
	}
	pods, services, err := pickRanges(class, subnets, n.Pods.CIDRBlocks, n.Services.CIDRBlocks, others)
	if err != nil {
		return apierrors.NewInvalid(clusterv1.GroupVersion.WithKind("Cluster").GroupKind(), c.Name, //nolint:wrapcheck // made here, not passed on
			field.ErrorList{field.Invalid(field.NewPath("spec", "clusterNetwork"), n, err.Error())})
	}
	n.Pods.CIDRBlocks, n.Services.CIDRBlocks = pods, services
	return nil
}

// runsCiliumOf tells whether a cluster runs Cilium, by its cni variable or else its class's default.
func runsCiliumOf(c *clusterv1.Cluster, class *clusterv1.ClusterClass) bool {
	return cniOf(c, class) == cniCilium
}

// otherRanges are the pod and service ranges of the other clusters, which a cluster that may join a
// Cluster Mesh keeps clear of.
type otherRanges struct {
	pods, services []netip.Prefix
}

// otherRanges are those of every Kubehenge cluster but c.
func (d *ClusterDefaulter) otherRanges(ctx context.Context, c *clusterv1.Cluster) (*otherRanges, error) {
	var list clusterv1.ClusterList
	if err := d.Hooks.Reader.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing the clusters: %w", err)
	}
	var classes clusterv1.ClusterClassList
	classReader := d.Hooks.Reader
	if d.Hooks.Classes != nil {
		classReader = d.Hooks.Classes
	}
	if err := classReader.List(ctx, &classes, client.InNamespace(d.Hooks.ClassNamespace)); err != nil {
		return nil, fmt.Errorf("listing the cluster classes: %w", err)
	}
	others := &otherRanges{}
	for i := range list.Items {
		o := &list.Items[i]
		if !Owns(o, d.Hooks.ClassNamespace) || o.Namespace == c.Namespace && o.Name == c.Name {
			continue
		}
		pods, services := ClusterRanges(o, classes.Items)
		others.pods = append(others.pods, parsePrefixes(pods)...)
		others.services = append(others.services, parsePrefixes(services)...)
	}
	return others, nil
}

func parsePrefixes(ranges []string) []netip.Prefix {
	var out []netip.Prefix
	for _, r := range ranges {
		if p, err := netip.ParsePrefix(r); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// pickRanges returns a new cluster's pod and service ranges, one of each of its families in their
// order: those asked for, or else the distribution's own IPv4 ones and Kubehenge's IPv6 ones when
// they are clear of the nodes' networks, or else the first clear candidates. With others, the ranges
// it is not asked for also keep its pods apart from the other clusters' pods and services, and its
// services from their pods.
func pickRanges(class *clusterv1.ClusterClass, subnets []nodeSubnet, pods, services []string, others *otherRanges) ([]string, []string, error) {
	// checkRanges holds those asked for to the nodes' networks and each other.
	r := &rangePicker{taken: slices.Concat(parsePrefixes(pods), parsePrefixes(services))}
	for _, s := range subnets {
		r.nodes = append(r.nodes, s.prefix)
	}
	r.taken = append(r.taken, r.nodes...)
	var podsAvoid, servicesAvoid []netip.Prefix
	if others != nil {
		podsAvoid, servicesAvoid = slices.Concat(others.pods, others.services), others.pods
	}
	outPods, outServices := pods, services
	for _, s := range subnets {
		podTries, serviceTries := podCandidatesIPv6, serviceCandidatesIPv6
		podDefault, serviceDefault := DefaultPodsIPv6, DefaultServicesIPv6
		if s.family == familyIPv4 {
			podTries, serviceTries = podCandidates, serviceCandidates
			if others != nil {
				podTries, serviceTries = append(slices.Clone(podTries), meshCandidates...), append(slices.Clone(serviceTries), meshCandidates...)
			}
			podDefault, serviceDefault = class.Annotations[DefaultPodsAnnotation], class.Annotations[DefaultServicesAnnotation]
		}
		if len(pods) == 0 {
			p, err := r.pick("pod", s.family, podDefault, podTries, podsAvoid)
			if err != nil {
				return nil, nil, err
			}
			outPods = append(outPods, p)
		}
		if len(services) == 0 {
			p, err := r.pick("service", s.family, serviceDefault, serviceTries, servicesAvoid)
			if err != nil {
				return nil, nil, err
			}
			outServices = append(outServices, p)
		}
	}
	return outPods, outServices, nil
}

// rangePicker hands out ranges that overlap neither the nodes' networks nor one handed out before.
type rangePicker struct {
	nodes []netip.Prefix
	taken []netip.Prefix
}

// pick returns the first one of def and candidates clear of those taken and of avoid.
func (r *rangePicker) pick(what, family, def string, candidates []string, avoid []netip.Prefix) (string, error) {
	for _, c := range append([]string{def}, candidates...) {
		p, err := netip.ParsePrefix(c)
		if err != nil || slices.ContainsFunc(r.taken, p.Overlaps) || slices.ContainsFunc(avoid, p.Overlaps) {
			continue
		}
		r.taken = append(r.taken, p)
		return c, nil
	}
	nodes := make([]string, 0, len(r.nodes))
	for _, n := range r.nodes {
		nodes = append(nodes, n.String())
	}
	if len(avoid) > 0 {
		return "", fmt.Errorf("every %s %s range Kubehenge tries overlaps the nodes' networks %s or another cluster's ranges; choose one", family, what, strings.Join(nodes, ", "))
	}
	return "", fmt.Errorf("every %s %s range Kubehenge tries overlaps the nodes' networks %s; choose one", family, what, strings.Join(nodes, ", "))
}
