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
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
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
	Enabled bool `json:"enabled"`
	// ServingPorts, unless false, opens the API, ingress, the load-balancer addresses, the
	// NodePorts and ICMP echo to anyone, after the security groups.
	ServingPorts *bool          `json:"servingPorts,omitempty"`
	Ports        []FirewallPort `json:"ports,omitempty"`
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
	// API is the management cluster's API as workload clusters reach it, such as
	// https://192.0.2.10:6443, and Bridge and VLAN the network the management cluster's nodes are
	// on. Without a bridge, no cluster is held off the management side's addresses (Held).
	API    string
	Bridge string
	VLAN   int
	// Nodes names the management nodes by address, for the refusal of an address one of them holds.
	Nodes    map[string]string
	Interval time.Duration
	// Datacenter reads whether Proxmox VE's datacenter firewall is on. Nil, no condition is kept.
	Datacenter APIReach
	// unreached counts the runs in a row whose read of the datacenter firewall got no answer.
	unreached int
	// Events, when set, records a warning on the cluster once its firewall does nothing or its
	// state cannot be read.
	Events events.EventRecorder
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
	own, err := f.applianceAddresses(ctx)
	if err != nil {
		return nil, err
	}
	for _, address := range own {
		if !slices.Contains(sources, address) {
			sources = append(sources, address)
		}
	}
	return sources, nil
}

// applianceAddresses are the addresses the appliance recorded for itself, none while it has not.
func (f *FirewallSources) applianceAddresses(ctx context.Context) ([]string, error) {
	cm := &corev1.ConfigMap{}
	err := f.Reader.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: ApplianceAddressesName}, cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading the appliance's addresses: %w", err)
	}
	var own []string
	for address := range strings.SplitSeq(cm.Data[ApplianceAddressesKey], ",") {
		if address = strings.TrimSpace(address); address != "" {
			own = append(own, address)
		}
	}
	return own, nil
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
		// A cluster without a firewall has nothing the datacenter firewall's state could say.
		seen := datacenterUnasked
		if firewalled(c) {
			seen = datacenter
		}
		if err := f.annotate(ctx, pc, firewallAnnotations(c, sources, clusters.Items, byCluster)); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s/%s: %w", c.Namespace, c.Name, err))
		}
		if err := f.markInactive(ctx, c, seen); err != nil {
			errs = append(errs, fmt.Errorf("cluster %s/%s: %w", c.Namespace, c.Name, err))
		}
	}
	return errors.Join(errs...)
}

// firewallAnnotations are the firewall annotations a cluster's ProxmoxCluster should hold, each
// empty where it has none: the sources of a firewalled cluster, and the load-balancer addresses
// where its serving ports are open.
func firewallAnnotations(c *clusterv1.Cluster, sources []string, clusters []clusterv1.Cluster, byCluster map[types.NamespacedName]*unstructured.Unstructured) map[string]string {
	want := map[string]string{FirewallSourcesAnnotation: "", FirewallDestinationsAnnotation: ""}
	if !firewalled(c) {
		return want
	}
	want[FirewallSourcesAnnotation] = strings.Join(append(slices.Clone(sources), meshSources(c, clusters, byCluster)...), ",")
	if servesPorts(c) {
		want[FirewallDestinationsAnnotation] = strings.Join(loadBalancerDestinations(c), ",")
	}
	return want
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

// servesPorts tells whether a cluster's firewall opens its serving ports to anyone, as its class's
// template does: when the firewall variable is on and its serving ports are not switched off. A
// cluster made before the variable has its groups alone.
func servesPorts(c *clusterv1.Cluster) bool {
	fw, set, err := optionalVariable[Firewall](c, FirewallVariable)
	return err == nil && set && fw.Enabled && (fw.ServingPorts == nil || *fw.ServingPorts)
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
	zones, _, _ := unstructured.NestedSlice(pc.Object, "spec", "zoneConfigs")
	for _, zone := range zones {
		if z, ok := zone.(map[string]any); ok {
			pools = append(pools, map[string]any{"spec": z})
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
	// datacenterUnasked is a run that read nothing, as no cluster is firewalled.
	datacenterUnasked datacenterState = iota
	datacenterOn
	datacenterOff
	// datacenterRefused is Proxmox VE refusing the read for want of Sys.Audit on /.
	datacenterRefused
	// datacenterTokenRefused is Proxmox VE refusing the controller's token itself.
	datacenterTokenRefused
	// datacenterHeld is a read that got no answer, too few runs in a row to say so: each cluster
	// keeps what it holds, so a flaky read neither flips the condition nor records events.
	datacenterHeld
	// datacenterUnreached is unreachedRuns or more runs in a row without an answer.
	datacenterUnreached
)

// unreachedRuns is how many runs in a row get no answer before a cluster says its firewall's state
// is unknown.
const unreachedRuns = 3

// datacenter reads the datacenter firewall's state once a run, and only where a cluster is
// firewalled.
func (f *FirewallSources) datacenter(ctx context.Context, clusters []clusterv1.Cluster) datacenterState {
	if f.Datacenter == nil || !slices.ContainsFunc(clusters, func(c clusterv1.Cluster) bool { return firewalled(&c) }) {
		return datacenterUnasked
	}
	reach, err := f.Datacenter.APIReach(ctx)
	if err == nil || refused(err) {
		f.unreached = 0
	}
	switch {
	case pve.Unauthenticated(err):
		return datacenterTokenRefused
	case refused(err):
		return datacenterRefused
	case err != nil:
		f.unreached++
		if f.unreached < unreachedRuns {
			return datacenterHeld
		}
		return datacenterUnreached
	case reach.Enabled:
		return datacenterOn
	default:
		return datacenterOff
	}
}

// inactiveCondition is FirewallInactiveCondition for a firewalled cluster in the datacenter state
// given, nil where it has none. Each cause of an unknown state has a reason of its own, so a new
// cause records an event.
func inactiveCondition(datacenter datacenterState) *metav1.Condition {
	switch datacenter {
	case datacenterOff:
		return &metav1.Condition{Status: metav1.ConditionTrue, Reason: "DatacenterFirewallOff", Message: FirewallInactiveMessage}
	case datacenterRefused:
		return &metav1.Condition{
			Status: metav1.ConditionUnknown, Reason: "SysAuditMissing",
			Message: "Kubehenge cannot read whether Proxmox VE's datacenter firewall is on, which needs Sys.Audit on /",
		}
	case datacenterTokenRefused:
		return &metav1.Condition{
			Status: metav1.ConditionUnknown, Reason: "TokenRefused",
			Message: "Proxmox VE refused Kubehenge's API token, which may have expired, been revoked or be wrong. " +
				"Make a new token, or apply the management cluster again",
		}
	case datacenterUnreached:
		return &metav1.Condition{
			Status: metav1.ConditionUnknown, Reason: "ProxmoxUnreachable",
			Message: "Kubehenge cannot reach Proxmox VE to read whether its datacenter firewall is on",
		}
	case datacenterUnasked, datacenterOn, datacenterHeld:
		return nil
	}
	return nil
}

// markInactive keeps FirewallInactiveCondition on a cluster at what is known of the datacenter
// firewall for it, and takes it off once there is nothing to say, as for a cluster without a
// firewall or once the datacenter's is on. It is a warning only: the cluster is never held for it.
func (f *FirewallSources) markInactive(ctx context.Context, c *clusterv1.Cluster, datacenter datacenterState) error {
	if f.Datacenter == nil || datacenter == datacenterHeld {
		return nil
	}
	cond := inactiveCondition(datacenter)
	have := meta.FindStatusCondition(c.Status.Conditions, FirewallInactiveCondition)
	if (cond == nil && have == nil) || (cond != nil && have != nil && have.Status == cond.Status && have.Reason == cond.Reason && have.Message == cond.Message) {
		return nil
	}
	before, err := markCluster(ctx, f.Client, f.Reader, client.ObjectKeyFromObject(c), FirewallInactiveCondition, cond)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("marking the datacenter firewall: %w", err)
	}
	if err == nil && f.Events != nil && newlyMarked(before, cond) {
		f.Events.Eventf(c, nil, corev1.EventTypeWarning, FirewallInactiveCondition, "WatchFirewall", "%s", cond.Message)
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
