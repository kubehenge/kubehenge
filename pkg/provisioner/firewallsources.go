package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ApplianceAddressesName is the ConfigMap in the controller's namespace that holds the
	// appliance's own addresses. The chart makes it empty, and the appliance is its one writer.
	ApplianceAddressesName = "kubehenge-appliance-addresses"
	// ApplianceAddressesKey holds the appliance's addresses, comma-separated.
	ApplianceAddressesKey = "addresses"
	// FirewallSourcesAnnotation names, on the ProxmoxCluster of a firewalled cluster, the addresses
	// beside the cluster's own that Kubehenge's capmox build keeps in each node's IPSet kubehenge:
	// the management cluster's nodes, the appliance and the nodes and pods of the clusters it is
	// linked with by Cluster Mesh. FirewallSources is its one writer.
	FirewallSourcesAnnotation = "kubehenge.dev/firewall-sources"
	// FirewallDestinationsAnnotation names, on the ProxmoxCluster of a firewalled cluster, the
	// cluster's load-balancer addresses, which capmox keeps in each node's IPSet kubehenge-lb and the
	// firewall accepts all traffic to. FirewallSources is its one writer.
	FirewallDestinationsAnnotation = "kubehenge.dev/firewall-destinations"
	// FirewallVariable switches a cluster's firewall and lists its further ports. The class
	// defaults it on.
	FirewallVariable = "firewall"
	// FirewallInactiveCondition on a firewalled Cluster says that Proxmox VE's datacenter firewall
	// is off, so the VMs' rules filter nothing. Unknown, the datacenter's options cannot be read.
	FirewallInactiveCondition = "FirewallInactive"
	// FirewallInactiveMessage is the condition's message, the same in the interface and the CLI.
	FirewallInactiveMessage = "Proxmox VE's datacenter firewall is off, so this cluster's firewall does nothing"
)

// Firewall is a cluster's firewall variable.
type Firewall struct {
	Enabled bool           `json:"enabled"`
	Ports   []FirewallPort `json:"ports,omitempty"`
}

// FirewallPort is a port or a range of ports a cluster's nodes accept from anywhere.
type FirewallPort struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	EndPort  int    `json:"endPort,omitempty"`
}

// FirewallSources keeps FirewallSourcesAnnotation and FirewallDestinationsAnnotation on the
// ProxmoxCluster of every firewalled cluster, and takes them off a cluster without a firewall.
// capmox writes a change into the nodes' IPSets, so a new address replaces no node. While the
// controller is away the IPSets keep what was last written, and new nodes get the same. It also
// keeps FirewallInactiveCondition on each firewalled cluster.
type FirewallSources struct {
	Client client.Client
	// Reader reads the appliance's ConfigMap past the cache, which holds only the classes'.
	Reader    client.Reader
	Namespace string
	// Management are the management cluster's node addresses, which its module sets.
	Management []string
	Interval   time.Duration
	// Datacenter reads whether Proxmox VE's datacenter firewall is on. Nil, no condition is kept.
	Datacenter APIReach
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*FirewallSources) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (f *FirewallSources) Start(ctx context.Context) error {
	tick := time.NewTicker(f.Interval)
	defer tick.Stop()
	for {
		if err := f.runOnce(ctx); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).WithName("firewall-sources").Error(err, "keeping the firewall sources of the clusters")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Addresses are the management side's sources: the management cluster's nodes, then the
// appliance's addresses, each once. A ConfigMap the appliance has not written adds nothing, as on a
// management cluster set up without the appliance. The admission check of a cluster's firewall asks
// for them too, so that it holds the same addresses the firewall keeps.
func (f *FirewallSources) Addresses(ctx context.Context) ([]string, error) {
	sources := slices.Clone(f.Management)
	cm := &corev1.ConfigMap{}
	err := f.Reader.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: ApplianceAddressesName}, cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading the appliance's addresses: %w", err)
	}
	for address := range strings.SplitSeq(cm.Data[ApplianceAddressesKey], ",") {
		if address = strings.TrimSpace(address); address != "" && !slices.Contains(sources, address) {
			sources = append(sources, address)
		}
	}
	return sources, nil
}

func (f *FirewallSources) runOnce(ctx context.Context) error {
	sources, err := f.Addresses(ctx)
	if err != nil {
		return err
	}
	clusters := &clusterv1.ClusterList{}
	if err := f.Client.List(ctx, clusters); err != nil {
		return fmt.Errorf("listing the clusters: %w", err)
	}
	proxmoxClusters := &unstructured.UnstructuredList{}
	proxmoxClusters.SetGroupVersionKind(proxmoxClusterListGVK)
	if err := f.Client.List(ctx, proxmoxClusters); err != nil {
		return fmt.Errorf("listing the ProxmoxClusters: %w", err)
	}
	byCluster := make(map[types.NamespacedName]*unstructured.Unstructured, len(proxmoxClusters.Items))
	for i := range proxmoxClusters.Items {
		pc := &proxmoxClusters.Items[i]
		if name := pc.GetLabels()[clusterv1.ClusterNameLabel]; name != "" {
			byCluster[types.NamespacedName{Namespace: pc.GetNamespace(), Name: name}] = pc
		}
	}
	datacenter := f.datacenter(ctx, clusters.Items)
	var errs []error
	for i := range clusters.Items {
		c := &clusters.Items[i]
		pc := byCluster[types.NamespacedName{Namespace: c.Namespace, Name: c.Name}]
		if pc == nil || !Owns(c, f.Namespace) || !c.DeletionTimestamp.IsZero() {
			continue
		}
		want := map[string]string{FirewallSourcesAnnotation: "", FirewallDestinationsAnnotation: ""}
		on := firewalled(c)
		if on {
			want[FirewallSourcesAnnotation] = strings.Join(append(slices.Clone(sources), meshSources(c, clusters.Items, byCluster)...), ",")
			want[FirewallDestinationsAnnotation] = strings.Join(loadBalancerDestinations(c), ",")
		}
		if err := f.annotate(ctx, pc, want); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s/%s: %w", c.Namespace, c.Name, err))
		}
		if err := f.markInactive(ctx, c, on, datacenter); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s/%s: %w", c.Namespace, c.Name, err))
		}
	}
	return errors.Join(errs...)
}

// firewalled tells whether a cluster's nodes have a firewall: the firewall variable on, or security
// groups named on a cluster without the variable.
func firewalled(c *clusterv1.Cluster) bool {
	if fw, set, err := optionalVariable[Firewall](c, FirewallVariable); err == nil && set {
		return fw.Enabled
	}
	groups, set, err := optionalVariable[[]string](c, SecurityGroupsVariable)
	return err == nil && set && len(groups) > 0
}

// loadBalancerDestinations are the addresses a cluster's load balancer hands out, none without one.
func loadBalancerDestinations(c *clusterv1.Cluster) []string {
	lb, set, err := optionalVariable[LoadBalancer](c, loadBalancerVariable)
	if err != nil || !set || !lb.Enabled {
		return nil
	}
	return lb.Addresses
}

// meshSources are the control-plane endpoints, node addresses and pod ranges of the clusters a
// cluster is linked with by Cluster Mesh, whose nodes reach its nodes and pods directly or through
// Cilium's tunnels. Only linked peers count, so listing a cluster opens nothing on its own.
func meshSources(c *clusterv1.Cluster, clusters []clusterv1.Cluster, byCluster map[types.NamespacedName]*unstructured.Unstructured) []string {
	var sources []string
	for _, peer := range MeshPeers(c, clusters, nil) {
		if peer.Refused != "" {
			continue
		}
		key := types.NamespacedName{Namespace: c.Namespace, Name: peer.Name}
		i := slices.IndexFunc(clusters, func(o clusterv1.Cluster) bool { return o.Namespace == key.Namespace && o.Name == key.Name })
		if i >= 0 {
			sources = append(sources, clusters[i].Spec.ClusterNetwork.Pods.CIDRBlocks...)
		}
		if pc := byCluster[key]; pc != nil {
			sources = append(sources, proxmoxClusterAddresses(pc)...)
		}
	}
	slices.Sort(sources)
	return slices.Compact(sources)
}

// proxmoxClusterAddresses are a ProxmoxCluster's control-plane endpoint, where it is an address,
// and its node address pools of every family and zone.
func proxmoxClusterAddresses(pc *unstructured.Unstructured) []string {
	var addrs []string
	if host, _, _ := unstructured.NestedString(pc.Object, "spec", "controlPlaneEndpoint", "host"); host != "" {
		if _, err := netip.ParseAddr(host); err == nil {
			addrs = append(addrs, host)
		}
	}
	pools := []map[string]any{pc.Object}
	if spec, ok := pc.Object["spec"].(map[string]any); ok {
		zones, _ := spec["zoneConfigs"].([]any)
		for _, zone := range zones {
			if z, ok := zone.(map[string]any); ok {
				pools = append(pools, map[string]any{"spec": z})
			}
		}
	}
	for _, pool := range pools {
		for _, family := range []string{"ipv4Config", "ipv6Config"} {
			list, _, _ := unstructured.NestedStringSlice(pool, "spec", family, "addresses")
			addrs = append(addrs, list...)
		}
	}
	return addrs
}

// datacenterState is what is known of Proxmox VE's datacenter firewall.
type datacenterState int

const (
	datacenterUnread datacenterState = iota
	datacenterOn
	datacenterOff
)

// datacenter reads the datacenter firewall's state once a run, and only where a cluster is
// firewalled.
func (f *FirewallSources) datacenter(ctx context.Context, clusters []clusterv1.Cluster) datacenterState {
	if f.Datacenter == nil || !slices.ContainsFunc(clusters, func(c clusterv1.Cluster) bool { return firewalled(&c) }) {
		return datacenterUnread
	}
	reach, ok := f.Datacenter.APIReach(ctx)
	switch {
	case !ok:
		return datacenterUnread
	case reach.Enabled:
		return datacenterOn
	default:
		return datacenterOff
	}
}

// markInactive keeps FirewallInactiveCondition on a firewalled cluster at what is known of the
// datacenter firewall, and takes it off a cluster without a firewall or once the datacenter's is on.
// It is a warning only: the cluster is never held for it.
func (f *FirewallSources) markInactive(ctx context.Context, c *clusterv1.Cluster, on bool, datacenter datacenterState) error {
	if f.Datacenter == nil {
		return nil
	}
	var cond *metav1.Condition
	switch {
	case !on || datacenter == datacenterOn:
	case datacenter == datacenterOff:
		cond = &metav1.Condition{Status: metav1.ConditionTrue, Reason: "DatacenterFirewallOff", Message: FirewallInactiveMessage}
	default:
		cond = &metav1.Condition{
			Status: metav1.ConditionUnknown, Reason: "DatacenterFirewallUnread",
			Message: "Kubehenge cannot read whether Proxmox VE's datacenter firewall is on, which needs Sys.Audit on /",
		}
	}
	have := meta.FindStatusCondition(c.Status.Conditions, FirewallInactiveCondition)
	if (cond == nil && have == nil) || (cond != nil && have != nil && have.Status == cond.Status && have.Reason == cond.Reason) {
		return nil
	}
	if _, err := markCluster(ctx, f.Client, f.Reader, client.ObjectKeyFromObject(c), FirewallInactiveCondition, cond); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("marking the datacenter firewall: %w", err)
	}
	return nil
}

// annotate sets each of the ProxmoxCluster's annotations in want where it differs, and takes one off
// where want holds it empty. capmox reads an empty annotation and none alike.
func (f *FirewallSources) annotate(ctx context.Context, pc *unstructured.Unstructured, want map[string]string) error {
	changes := map[string]any{}
	for key, value := range want {
		held, has := pc.GetAnnotations()[key]
		switch {
		case value == "" && has:
			changes[key] = nil
		case value != "" && held != value:
			changes[key] = value
		}
	}
	if len(changes) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{annotationsKey: changes}})
	if err != nil {
		return fmt.Errorf("the firewall sources: %w", err)
	}
	if err := f.Client.Patch(ctx, pc, client.RawPatch(types.MergePatchType, patch)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("annotating ProxmoxCluster %s with the firewall sources: %w", pc.GetName(), err)
	}
	return nil
}
