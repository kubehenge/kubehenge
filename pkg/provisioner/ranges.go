package provisioner

import (
	"fmt"
	"net/netip"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// Annotations on a ClusterClass naming its distribution's pod and service ranges, used for a Cluster
// that sets none.
const (
	DefaultPodsAnnotation     = "kubehenge.dev/default-pods"
	DefaultServicesAnnotation = "kubehenge.dev/default-services"
)

type nodeNetwork struct {
	Gateway string   `json:"gateway"`
	Prefix  int      `json:"prefix"`
	Addrs   []string `json:"addresses"`
}

// checkRanges refuses a cluster whose pod or service range overlaps its nodes' network: packets for
// the nodes would be routed into the cluster instead, and the cluster fails in ways that are hard to
// trace back to the ranges.
func checkRanges(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	network, err := variable[nodeNetwork](cluster, "network")
	if err != nil {
		return err
	}
	gateway, err := netip.ParseAddr(network.Gateway)
	if err != nil {
		return fmt.Errorf("network.gateway: %w", err)
	}
	nodes, err := gateway.Prefix(network.Prefix)
	if err != nil {
		return fmt.Errorf("network.prefix: %w", err)
	}
	var problems []string
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
			if err != nil {
				return fmt.Errorf("%s range %q: %w", r.what, b, err)
			}
			if p.Overlaps(nodes) {
				problems = append(problems, fmt.Sprintf("the %s range %s overlaps the nodes' network %s", r.what, p, nodes))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s; set spec.clusterNetwork to ranges the network does not use", strings.Join(problems, "; "))
	}
	return nil
}
