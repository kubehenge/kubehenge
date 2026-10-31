package provisioner

import (
	"context"
	"encoding/json"
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

// Candidate ranges, tried after the distribution's own: Kubernetes' and the k3s family's usual
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

type nodeNetwork struct {
	Bridge  string   `json:"bridge"`
	VLAN    int      `json:"vlan"`
	Gateway string   `json:"gateway"`
	Prefix  int      `json:"prefix"`
	Addrs   []string `json:"addresses"`
}

// nodesPrefix is the nodes' network, from the network variable's gateway and prefix length.
func nodesPrefix(cluster *clusterv1.Cluster) (netip.Prefix, error) {
	network, err := variable[nodeNetwork](cluster, "network")
	if err != nil {
		return netip.Prefix{}, err
	}
	gateway, err := netip.ParseAddr(network.Gateway)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("network.gateway: %w", err)
	}
	nodes, err := gateway.Prefix(network.Prefix)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("network.prefix: %w", err)
	}
	return nodes, nil
}

// checkRanges refuses a cluster whose pod or service ranges are no IPv4 networks, overlap each
// other, or overlap its nodes' network: packets for the nodes would be routed into the cluster
// instead, and the cluster fails in ways that are hard to trace back to the ranges.
func checkRanges(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	nodes, err := nodesPrefix(cluster)
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
		if len(blocks) == 0 && r.def != "" {
			blocks = []string{r.def}
		}
		for _, b := range blocks {
			p, err := netip.ParsePrefix(b)
			if err != nil || !p.Addr().Is4() || p != p.Masked() {
				return fmt.Errorf("%s range: %s is no IPv4 network such as 10.244.0.0/16, without host bits", r.what, b)
			}
			if p.Overlaps(nodes) {
				problems = append(problems, fmt.Sprintf("%s range %s overlaps the nodes' network %s", r.what, p, nodes))
			}
			if i := slices.IndexFunc(taken, p.Overlaps); i >= 0 {
				problems = append(problems, fmt.Sprintf("the %s range %s overlaps the pod range %s", r.what, p, taken[i]))
			}
		}
		for _, b := range blocks {
			taken = append(taken, netip.MustParsePrefix(b))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s. Set spec.clusterNetwork to other ranges", strings.Join(problems, ", "))
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
	class := &clusterv1.ClusterClass{}
	if err := d.Hooks.Reader.Get(ctx, client.ObjectKey{Namespace: d.Hooks.ClassNamespace, Name: c.Spec.Topology.ClassRef.Name}, class); err != nil {
		if apierrors.IsNotFound(err) {
			// Cluster API refuses a cluster of a class that does not exist.
			return nil
		}
		return fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	nodes, err := nodesPrefix(c)
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
	pods, services, err := pickRanges(class, nodes, n.Pods.CIDRBlocks, n.Services.CIDRBlocks, others)
	if err != nil {
		return apierrors.NewInvalid(clusterv1.GroupVersion.WithKind("Cluster").GroupKind(), c.Name, //nolint:wrapcheck // made here, not passed on
			field.ErrorList{field.Invalid(field.NewPath("spec", "clusterNetwork"), n, err.Error())})
	}
	n.Pods.CIDRBlocks, n.Services.CIDRBlocks = pods, services
	return nil
}

// runsCiliumOf tells whether a cluster runs Cilium, by its cni variable or else its class's default.
func runsCiliumOf(c *clusterv1.Cluster, class *clusterv1.ClusterClass) bool {
	cni, set, err := optionalVariable[string](c, "cni")
	if err != nil {
		return false
	}
	if !set {
		i := slices.IndexFunc(class.Spec.Variables, func(v clusterv1.ClusterClassVariable) bool { return v.Name == "cni" })
		if i < 0 || class.Spec.Variables[i].Schema.OpenAPIV3Schema.Default == nil ||
			json.Unmarshal(class.Spec.Variables[i].Schema.OpenAPIV3Schema.Default.Raw, &cni) != nil {
			return false
		}
	}
	return cni == cniCilium
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
	if err := d.Hooks.Reader.List(ctx, &classes, client.InNamespace(d.Hooks.ClassNamespace)); err != nil {
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

// pickRanges returns a new cluster's pod and service ranges: those asked for, or else the
// distribution's own when they are clear of the nodes' network, or else the first clear candidates.
// With others, the ranges it is not asked for also keep its pods apart from the other clusters'
// pods and services, and its services from their pods.
func pickRanges(class *clusterv1.ClusterClass, nodes netip.Prefix, pods, services []string, others *otherRanges) ([]string, []string, error) {
	// checkRanges holds those asked for to the nodes' network and each other.
	r := &rangePicker{nodes: nodes, taken: slices.Concat([]netip.Prefix{nodes}, parsePrefixes(pods), parsePrefixes(services))}
	podTries, serviceTries := podCandidates, serviceCandidates
	var podsAvoid, servicesAvoid []netip.Prefix
	if others != nil {
		podTries, serviceTries = append(slices.Clone(podTries), meshCandidates...), append(slices.Clone(serviceTries), meshCandidates...)
		podsAvoid, servicesAvoid = slices.Concat(others.pods, others.services), others.pods
	}
	outPods, err := r.pick("pod", pods, class.Annotations[DefaultPodsAnnotation], podTries, podsAvoid)
	if err != nil {
		return nil, nil, err
	}
	outServices, err := r.pick("service", services, class.Annotations[DefaultServicesAnnotation], serviceTries, servicesAvoid)
	if err != nil {
		return nil, nil, err
	}
	return outPods, outServices, nil
}

// rangePicker hands out ranges that overlap neither the nodes' network nor one handed out before.
type rangePicker struct {
	nodes netip.Prefix
	taken []netip.Prefix
}

// pick returns the ranges asked for, or else the first one of def and candidates clear of those
// taken and of avoid.
func (r *rangePicker) pick(what string, asked []string, def string, candidates []string, avoid []netip.Prefix) ([]string, error) {
	if len(asked) > 0 {
		return asked, nil
	}
	for _, c := range append([]string{def}, candidates...) {
		p, err := netip.ParsePrefix(c)
		if err != nil || slices.ContainsFunc(r.taken, p.Overlaps) || slices.ContainsFunc(avoid, p.Overlaps) {
			continue
		}
		r.taken = append(r.taken, p)
		return []string{c}, nil
	}
	if len(avoid) > 0 {
		return nil, fmt.Errorf("every %s range Kubehenge tries overlaps the nodes' network %s or another cluster's ranges; choose one", what, r.nodes)
	}
	return nil, fmt.Errorf("every %s range Kubehenge tries overlaps the nodes' network %s; choose one", what, r.nodes)
}
