package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
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

// otherRanges are the pod and service ranges of the other clusters, which a cluster that may join a
// Cluster Mesh keeps clear of: a peer's pod addresses must not be taken for its own pods' or
// services'.
type otherRanges struct {
	pods, services []netip.Prefix
}

func parseRanges(ranges []string) []netip.Prefix {
	var out []netip.Prefix
	for _, r := range ranges {
		if p, err := netip.ParsePrefix(r); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// nodesPrefix is the nodes' network, from the network variable's gateway and prefix length.
func nodesPrefix(raw json.RawMessage) (netip.Prefix, error) {
	var v struct {
		Gateway string `json:"gateway"`
		Prefix  int    `json:"prefix"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return netip.Prefix{}, fmt.Errorf("the network settings are malformed: %w", err)
	}
	gw, err := netip.ParseAddr(v.Gateway)
	if err != nil || !gw.Is4() {
		return netip.Prefix{}, fmt.Errorf("gateway: %q is no IPv4 address", v.Gateway)
	}
	p, err := gw.Prefix(v.Prefix)
	if err != nil || v.Prefix < 1 {
		return netip.Prefix{}, errors.New("prefix length: 1 to 32")
	}
	return p, nil
}

// clusterRanges returns a new cluster's pod and service ranges: those asked for, which must not
// overlap the nodes' network or each other, or else the distribution's own when they are clear of
// it, or else the first clear candidates. A range overlapping the nodes' network routes their
// packets into the cluster. With others, the cluster may join a Cluster Mesh: ranges it is not
// asked for also keep its pods apart from the other clusters' pods and services, and its services
// from their pods.
func clusterRanges(class *clusterv1.ClusterClass, nodes netip.Prefix, pods, services []string, others *otherRanges) ([]string, []string, error) {
	r := &rangePicker{nodes: nodes, taken: []netip.Prefix{nodes}}
	podTries, serviceTries := podCandidates, serviceCandidates
	var podsAvoid, servicesAvoid []netip.Prefix
	if others != nil {
		podTries, serviceTries = append(slices.Clone(podTries), meshCandidates...), append(slices.Clone(serviceTries), meshCandidates...)
		podsAvoid, servicesAvoid = slices.Concat(others.pods, others.services), others.pods
	}
	outPods, err := r.pick("pod", pods, class.Annotations[provisioner.DefaultPodsAnnotation], podTries, podsAvoid)
	if err != nil {
		return nil, nil, err
	}
	outServices, err := r.pick("service", services, class.Annotations[provisioner.DefaultServicesAnnotation], serviceTries, servicesAvoid)
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

// pick takes the ranges asked for, or else the first one of def and candidates clear of those taken
// and of avoid.
func (r *rangePicker) pick(what string, asked []string, def string, candidates []string, avoid []netip.Prefix) ([]string, error) {
	if len(asked) > 0 {
		return r.takeAsked(what, asked)
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

func (r *rangePicker) takeAsked(what string, asked []string) ([]string, error) {
	for _, a := range asked {
		p, err := netip.ParsePrefix(a)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			return nil, fmt.Errorf("%s range: %s is no IPv4 network such as 10.244.0.0/16, without host bits", what, a)
		}
		if slices.ContainsFunc(r.taken, p.Overlaps) {
			return nil, fmt.Errorf("the %s range %s overlaps the nodes' network %s or the other range", what, p, r.nodes)
		}
		r.taken = append(r.taken, p)
	}
	return asked, nil
}
