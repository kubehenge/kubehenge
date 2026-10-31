package provisioner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// loadBalancerVariable gives the cluster's Services of type LoadBalancer addresses of its own.
	loadBalancerVariable = "loadBalancer"
	// The load balancers a cluster chooses from, as its loadBalancer variable names them.
	metalLB = "metallb"
	kubeVip = "kube-vip"
	// metalLBNamespace is where MetalLB runs; its speakers need the host's network.
	metalLBNamespace = "metallb-system"
	// kubeVipMetrics is where the load balancer's kube-vip serves its metrics: on loopback, as the
	// control planes' does on 2112 beside it.
	kubeVipMetrics = "127.0.0.1:2113"
	// pointToPointBits is the prefix length from which a network has no network or broadcast address.
	pointToPointBits = 31
)

// LoadBalancer is a cluster's loadBalancer variable.
type LoadBalancer struct {
	Enabled        bool     `json:"enabled"`
	Implementation string   `json:"implementation"`
	Addresses      []string `json:"addresses"`
}

// implementation is the load balancer chosen, MetalLB unless it is kube-vip.
func (l LoadBalancer) implementation() string {
	return cmp.Or(l.Implementation, metalLB)
}

func loadBalancerProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-load-balancer"
}

// kubeVipProviderProxy names the proxy of kube-vip's cloud provider, which hands out its addresses.
func kubeVipProviderProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-load-balancer-addresses"
}

// loadBalancerCharts are the charts of the cluster's load balancer: MetalLB, or kube-vip announcing
// the Services' addresses with its cloud provider handing them out.
func loadBalancerCharts(charts *Charts, lb LoadBalancer) ([]chartProxy, error) {
	spans, err := parseAddresses(lb.Addresses)
	if err != nil {
		return nil, fmt.Errorf("loadBalancer.addresses: %w", err)
	}
	if lb.implementation() == metalLB {
		return []chartProxy{{"load-balancer", metalLB, metalLBNamespace, charts.MetalLB, metalLBValues()}}, nil
	}
	ranges := make([]string, 0, len(spans))
	for _, s := range spans {
		ranges = append(ranges, s.first.String()+"-"+s.last.String())
	}
	return []chartProxy{
		{"load-balancer", kubeVip, metav1.NamespaceSystem, charts.KubeVip, withImage(kubeVipValues(), charts.KubeVipImage)},
		{"load-balancer-addresses", "kube-vip-cloud-provider", metav1.NamespaceSystem, charts.KubeVipCloudProvider, map[string]any{
			"cm": map[string]any{"data": map[string]any{"range-global": strings.Join(ranges, ",")}},
			// The chart takes its arguments as a map of flag to value.
			extraArgsKey: map[string]any{
				"leader-elect-lease-duration": leaseDuration,
				"leader-elect-renew-deadline": leaseRenew,
				"leader-elect-retry-period":   leaseRetry,
			},
		}},
	}, nil
}

// withImage pins kube-vip's chart to the image of the control planes' kube-vip, which it takes as a
// repository and a tag.
func withImage(values map[string]any, image Image) map[string]any {
	values["image"] = map[string]any{"repository": image.Registry + "/" + image.Repository, "tag": image.Tag + "@sha256:" + image.Digest}
	return values
}

// metalLBValues run MetalLB's layer 2 mode only, without frr-k8s, the BGP backend its chart
// deploys by default as a privileged DaemonSet on the host's network. Its speakers answer from
// every node, the control planes too, which kubeadm labels as excluded from load balancers: a
// cluster without workers has no other nodes to answer from.
func metalLBValues() map[string]any {
	return map[string]any{
		"speaker": map[string]any{"ignoreExcludeLB": true},
		"frrk8s":  map[string]any{"enabled": false},
	}
}

// kubeVipValues run kube-vip on every node for the Services only, each Service's address held by
// one node at a time through a lease of its own, so that the addresses spread over the nodes.
func kubeVipValues() map[string]any {
	const on, off = trueText, falseText
	return map[string]any{
		"env": map[string]any{
			"vip_arp": on, "cp_enable": off, "lb_enable": off,
			"svc_enable": on, "svc_election": on, "vip_leaderelection": off,
			"prometheus_server": kubeVipMetrics,
		},
	}
}

// metalLBPool is the address pool MetalLB hands out and announces, and its announcement in layer 2.
func metalLBPool(spans []addressSpan) []*unstructured.Unstructured {
	addresses := make([]any, 0, len(spans))
	for _, s := range spans {
		addresses = append(addresses, s.first.String()+"-"+s.last.String())
	}
	const name = "kubehenge"
	resources := []map[string]any{
		{"kind": "IPAddressPool", "spec": map[string]any{"addresses": addresses}},
		{"kind": "L2Advertisement", "spec": map[string]any{"ipAddressPools": []any{name}}},
	}
	objects := make([]*unstructured.Unstructured, 0, len(resources))
	for _, r := range resources {
		r[apiVersionKey] = "metallb.io/v1beta1"
		r["metadata"] = map[string]any{"name": name, "namespace": metalLBNamespace}
		objects = append(objects, &unstructured.Unstructured{Object: r})
	}
	return objects
}

// reconcileLoadBalancer does in the workload cluster what its load balancer needs there. It applies
// MetalLB's address pool, which MetalLB's chart has no place for and which goes with MetalLB's
// CRDs whenever MetalLB is removed. It frees Services of addresses outside the range, which
// kube-vip's cloud provider never takes back once it has recorded them, and which MetalLB would
// keep as requested. On k3s, it removes what servicelb left. It reports whether it waits for the
// cluster's API or for MetalLB to start.
func (r *Reconciler) reconcileLoadBalancer(ctx context.Context, cluster *clusterv1.Cluster, lb LoadBalancer) (bool, error) {
	if waiting, err := r.afterKubeVip(ctx, cluster, lb); err != nil || waiting {
		return waiting, err
	}
	if !lb.Enabled {
		return false, nil
	}
	spans, err := parseAddresses(lb.Addresses)
	if err != nil {
		return false, fmt.Errorf("loadBalancer.addresses: %w", err)
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return true, err
	}
	waiting := false
	if lb.implementation() == metalLB {
		if waiting, err = applyMetalLBPool(ctx, workload.client, spans); err != nil {
			r.Remotes.forget(cluster)
			return false, err
		}
	}
	if err := freeStaleAddresses(ctx, workload.client, spans); err != nil {
		r.Remotes.forget(cluster)
		return false, err
	}
	if distributionOf(cluster) == templates.K3s {
		if err := removeServiceLB(ctx, workload.client); err != nil {
			r.Remotes.forget(cluster)
			return false, err
		}
	}
	return waiting, nil
}

// applyMetalLBPool applies MetalLB's address pool, and reports whether it waits for MetalLB's CRDs
// or for its webhook, which checks the pool, to answer.
func applyMetalLBPool(ctx context.Context, workload client.Client, spans []addressSpan) (bool, error) {
	for _, o := range metalLBPool(spans) {
		err := workload.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), client.FieldOwner(fieldOwner), client.ForceOwnership)
		if meta.IsNoMatchError(err) || apierrors.IsInternalError(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("applying MetalLB's %s: %w", o.GetKind(), err)
		}
	}
	return false, nil
}

// Where kube-vip's cloud provider records a Service's address: it labels the Services it hands
// addresses to, keeps them in an annotation, and copies the first into spec.loadBalancerIP.
const (
	kubeVipImplementation = "implementation"
	kubeVipAddresses      = "kube-vip.io/loadbalancerIPs"
)

const (
	// kubeVipServicesAnnotation marks a cluster whose Services kube-vip's cloud provider may have
	// handled, until the controller has cleaned up after it.
	kubeVipServicesAnnotation = "kubehenge.dev/kube-vip-services"
	// loadBalancerCleanup is the finalizer a cloud provider's service controller holds each Service
	// of type LoadBalancer with, which only that controller lifts.
	loadBalancerCleanup = "service.kubernetes.io/load-balancer-cleanup"
	// kubeVipProvider is the Deployment of kube-vip's cloud provider in the workload cluster.
	kubeVipProvider = "kube-vip-cloud-provider"
)

// afterKubeVip marks the cluster while kube-vip's cloud provider runs in it, and once it no longer
// does, lifts the provider's finalizer from the Services it handled: nothing else would, and they
// could never be deleted, nor their namespaces. It reports whether it waits for the provider to go
// or for the cluster's API.
func (r *Reconciler) afterKubeVip(ctx context.Context, cluster *clusterv1.Cluster, lb LoadBalancer) (bool, error) {
	running := lb.Enabled && lb.implementation() == kubeVip
	_, marked := cluster.Annotations[kubeVipServicesAnnotation]
	if running == marked {
		return false, nil
	}
	if !running {
		waiting, err := r.releaseKubeVipServices(ctx, cluster)
		if err != nil || waiting {
			return waiting, err
		}
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	if running {
		if cluster.Annotations == nil {
			cluster.Annotations = map[string]string{}
		}
		cluster.Annotations[kubeVipServicesAnnotation] = trueText
	} else {
		delete(cluster.Annotations, kubeVipServicesAnnotation)
	}
	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return false, fmt.Errorf("marking whether kube-vip handles the cluster's Services: %w", err)
	}
	return false, nil
}

// releaseKubeVipServices lifts kube-vip's cloud provider's finalizer from the Services it handled,
// once the provider is gone, and reports whether it waits for that.
func (r *Reconciler) releaseKubeVipServices(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		// Without a kubeconfig, the cluster never had Services to release.
		return false, err
	}
	provider := &metav1.PartialObjectMetadata{}
	provider.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
	err = workload.client.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: kubeVipProvider}, provider)
	if err == nil {
		return true, nil
	}
	if !apierrors.IsNotFound(err) {
		r.Remotes.forget(cluster)
		return false, fmt.Errorf("looking for kube-vip's cloud provider: %w", err)
	}
	services := &corev1.ServiceList{}
	if err := workload.client.List(ctx, services, client.MatchingLabels{kubeVipImplementation: kubeVip}); err != nil {
		r.Remotes.forget(cluster)
		return false, fmt.Errorf("listing the Services kube-vip gave addresses: %w", err)
	}
	for i := range services.Items {
		s := &services.Items[i]
		patch := client.MergeFromWithOptions(s.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if !controllerutil.RemoveFinalizer(s, loadBalancerCleanup) {
			continue
		}
		if err := workload.client.Patch(ctx, s, patch); err != nil && !apierrors.IsNotFound(err) {
			r.Remotes.forget(cluster)
			return false, fmt.Errorf("releasing Service %s/%s: %w", s.Namespace, s.Name, err)
		}
	}
	return false, nil
}

// freeStaleAddresses takes from Services what kube-vip's cloud provider recorded for them where
// that lies outside the cluster's load-balancer addresses, so that the load balancer gives them
// new ones from the range. An address outside it is no longer held against other clusters.
func freeStaleAddresses(ctx context.Context, workload client.Client, spans []addressSpan) error {
	services := &corev1.ServiceList{}
	if err := workload.List(ctx, services, client.MatchingLabels{kubeVipImplementation: kubeVip}); err != nil {
		return fmt.Errorf("listing the Services kube-vip gave addresses: %w", err)
	}
	free := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"`+kubeVipImplementation+`":null},`+
		`"annotations":{"`+kubeVipAddresses+`":null}},"spec":{"loadBalancerIP":null}}`))
	for i := range services.Items {
		s := &services.Items[i]
		if inSpans(spans, s.Spec.LoadBalancerIP) && allInSpans(spans, s.Annotations[kubeVipAddresses]) {
			continue
		}
		if err := workload.Patch(ctx, s, free); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("freeing Service %s/%s's address: %w", s.Namespace, s.Name, err)
		}
	}
	return nil
}

// inSpans reports whether an address, if there is one, lies in one of the spans.
func inSpans(spans []addressSpan, address string) bool {
	if address == "" {
		return true
	}
	a, err := netip.ParseAddr(strings.TrimSpace(address))
	return err == nil && slices.ContainsFunc(spans, func(s addressSpan) bool { return s.overlaps(addressSpan{a, a}) })
}

// allInSpans reports whether every address of a comma-separated list lies in one of the spans.
func allInSpans(spans []addressSpan, addresses string) bool {
	for a := range strings.SplitSeq(addresses, ",") {
		if !inSpans(spans, a) {
			return false
		}
	}
	return true
}

// addressSpan is a run of IPv4 addresses, first to last.
type addressSpan struct{ first, last netip.Addr }

func (s addressSpan) String() string {
	if s.first == s.last {
		return s.first.String()
	}
	return s.first.String() + "-" + s.last.String()
}

func (s addressSpan) overlaps(o addressSpan) bool {
	return s.first.Compare(o.last) <= 0 && o.first.Compare(s.last) <= 0
}

// parseAddresses reads addresses as the network and loadBalancer variables write them: ranges
// (10.0.0.10-10.0.0.20), networks in CIDR notation and single addresses.
func parseAddresses(entries []string) ([]addressSpan, error) {
	spans := make([]addressSpan, 0, len(entries))
	for _, e := range entries {
		s, err := parseSpan(strings.TrimSpace(e))
		if err != nil {
			return nil, err
		}
		spans = append(spans, s)
	}
	return spans, nil
}

func parseSpan(e string) (addressSpan, error) {
	if from, to, ok := strings.Cut(e, "-"); ok {
		first, err1 := netip.ParseAddr(strings.TrimSpace(from))
		last, err2 := netip.ParseAddr(strings.TrimSpace(to))
		if err1 != nil || err2 != nil || !first.Is4() || !last.Is4() || last.Less(first) {
			return addressSpan{}, fmt.Errorf("%q is not a range of IPv4 addresses, first to last", e)
		}
		return addressSpan{first, last}, nil
	}
	if strings.Contains(e, "/") {
		p, err := netip.ParsePrefix(e)
		if err != nil || !p.Addr().Is4() {
			return addressSpan{}, fmt.Errorf("%q is not an IPv4 network", e)
		}
		p = p.Masked()
		return addressSpan{p.Addr(), lastAddress(p)}, nil
	}
	a, err := netip.ParseAddr(e)
	if err != nil || !a.Is4() {
		return addressSpan{}, fmt.Errorf("%q is not an IPv4 address", e)
	}
	return addressSpan{a, a}, nil
}

// lastAddress is a network's last address.
func lastAddress(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) | (uint32(1)<<(p.Addr().BitLen()-p.Bits()) - 1)
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// clusterAddresses are the addresses a cluster takes on its nodes' network.
type clusterAddresses struct {
	// segment is the network's bridge and VLAN; clusters on different ones share no addresses.
	segment  string
	nodes    netip.Prefix
	gateway  netip.Addr
	machines []addressSpan
	endpoint addressSpan
	services []addressSpan
}

func addressesOf(cluster *clusterv1.Cluster) (clusterAddresses, error) {
	var a clusterAddresses
	network, err := variable[nodeNetwork](cluster, "network")
	if err != nil {
		return a, err
	}
	a.segment = fmt.Sprintf("%s/%d", network.Bridge, network.VLAN)
	if a.gateway, err = netip.ParseAddr(network.Gateway); err != nil {
		return a, fmt.Errorf("network.gateway: %w", err)
	}
	if a.nodes, err = a.gateway.Prefix(network.Prefix); err != nil {
		return a, fmt.Errorf("network.prefix: %w", err)
	}
	if a.machines, err = parseAddresses(network.Addrs); err != nil {
		return a, unreadable{fmt.Errorf("network.addresses: %w", err)}
	}
	endpoint, err := variable[string](cluster, "controlPlaneEndpoint")
	if err != nil {
		return a, err
	}
	if a.endpoint, err = parseSpan(endpoint); err != nil {
		return a, fmt.Errorf("controlPlaneEndpoint: %w", err)
	}
	lb, _, err := optionalVariable[LoadBalancer](cluster, loadBalancerVariable)
	if err != nil {
		return a, err
	}
	if lb.Enabled {
		if a.services, err = parseAddresses(lb.Addresses); err != nil {
			return a, unreadable{fmt.Errorf("loadBalancer.addresses: %w", err)}
		}
	}
	return a, nil
}

// unreadable is an address list entry that is no address, range or network, which the variables'
// schema lets through as any string.
type unreadable struct{ error }

// checkAddresses refuses node addresses or a control-plane endpoint off the nodes' network, an
// endpoint the nodes may take themselves, and load-balancer addresses off the network or on the
// gateway, a node, an endpoint or another Service: two hosts answering for one address take each
// other's traffic. Other clusters count where they share the network's bridge and VLAN.
func checkAddresses(cluster *clusterv1.Cluster, others []clusterv1.Cluster) error {
	own, err := addressesOf(cluster)
	if err != nil {
		lb, _, lbErr := optionalVariable[LoadBalancer](cluster, loadBalancerVariable)
		if lbErr != nil || lb.Enabled || errors.As(err, new(unreadable)) {
			return err
		}
		// Cluster API refuses missing variables itself; until the cluster has them, it holds no
		// addresses to compare with other clusters.
		return nil
	}
	problems := own.problems()
	for i := range others {
		o := &others[i]
		if o.Namespace == cluster.Namespace && o.Name == cluster.Name {
			continue
		}
		theirs, err := addressesOf(o)
		if err != nil || theirs.segment != own.segment {
			// A cluster whose variables don't read was refused or is being fixed; it holds no addresses yet.
			continue
		}
		problems = append(problems, own.clashes(o.Namespace+"/"+o.Name, &theirs)...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, ", "))
	}
	return nil
}

// problems are where the cluster's addresses clash with its own network.
func (a *clusterAddresses) problems() []string {
	var problems []string
	for _, m := range a.machines {
		if !a.nodes.Contains(m.first) || !a.nodes.Contains(m.last) {
			problems = append(problems, fmt.Sprintf("node addresses %s are outside the network %s", m, a.nodes))
		}
		if m.overlaps(a.endpoint) {
			problems = append(problems, fmt.Sprintf("control-plane endpoint %s is inside node addresses %s", a.endpoint, m))
		}
	}
	if !a.nodes.Contains(a.endpoint.first) {
		problems = append(problems, fmt.Sprintf("control-plane endpoint %s is outside the network %s", a.endpoint, a.nodes))
	}
	if a.endpoint.first == a.gateway {
		problems = append(problems, fmt.Sprintf("control-plane endpoint %s is the gateway", a.endpoint))
	}
	for i, s := range a.services {
		if !a.nodes.Contains(s.first) || !a.nodes.Contains(s.last) {
			problems = append(problems, fmt.Sprintf("load-balancer addresses %s are outside the network %s", s, a.nodes))
		}
		problems = append(problems, a.taken(s)...)
		for _, t := range a.services[i+1:] {
			if s.overlaps(t) {
				problems = append(problems, fmt.Sprintf("load-balancer addresses %s and %s overlap", s, t))
			}
		}
	}
	return problems
}

// taken are where load-balancer addresses hold one the cluster's network has for something else.
func (a *clusterAddresses) taken(s addressSpan) []string {
	var problems []string
	if s.overlaps(addressSpan{a.gateway, a.gateway}) {
		problems = append(problems, fmt.Sprintf("load-balancer addresses %s include the gateway %s", s, a.gateway))
	}
	// Hosts on the network take its first and last address for the network and its broadcast,
	// so a Service given one would never be answered; a /31 or /32 has neither.
	if a.nodes.Bits() < pointToPointBits {
		for _, reserved := range []netip.Addr{a.nodes.Masked().Addr(), lastAddress(a.nodes)} {
			if s.overlaps(addressSpan{reserved, reserved}) {
				problems = append(problems, fmt.Sprintf("load-balancer addresses %s include the network or broadcast address %s", s, reserved))
			}
		}
	}
	if s.overlaps(a.endpoint) {
		problems = append(problems, fmt.Sprintf("load-balancer addresses %s include the control-plane endpoint %s", s, a.endpoint))
	}
	for _, m := range a.machines {
		if s.overlaps(m) {
			problems = append(problems, fmt.Sprintf("load-balancer addresses %s overlap node addresses %s", s, m))
		}
	}
	return problems
}

// clashes are where the cluster's addresses and those of the cluster named take each other's.
func (a *clusterAddresses) clashes(name string, theirs *clusterAddresses) []string {
	var problems []string
	taken := slices.Concat([]addressSpan{theirs.endpoint}, theirs.machines, theirs.services)
	for _, s := range a.services {
		for _, t := range taken {
			if s.overlaps(t) {
				problems = append(problems, fmt.Sprintf("load-balancer addresses %s overlap %s of cluster %s", s, t, name))
			}
		}
	}
	mine := slices.Concat([]addressSpan{a.endpoint}, a.machines)
	for _, m := range mine {
		for _, t := range slices.Concat([]addressSpan{theirs.endpoint}, theirs.machines) {
			if m.overlaps(t) {
				problems = append(problems, fmt.Sprintf("addresses %s overlap %s of cluster %s", m, t, name))
			}
		}
	}
	for _, s := range theirs.services {
		for _, m := range mine {
			if s.overlaps(m) {
				problems = append(problems, fmt.Sprintf("addresses %s overlap load-balancer addresses %s of cluster %s", m, s, name))
			}
		}
	}
	return problems
}

// AddressSpans reads addresses as the network and loadBalancer variables write them, each a first
// and last address.
func AddressSpans(entries []string) ([][2]netip.Addr, error) {
	spans, err := parseAddresses(entries)
	if err != nil {
		return nil, err
	}
	out := make([][2]netip.Addr, 0, len(spans))
	for _, s := range spans {
		out = append(out, [2]netip.Addr{s.first, s.last})
	}
	return out, nil
}

// TakenAddresses are the addresses the clusters hold on a bridge and VLAN, each a first and last
// address: their nodes', their control-plane endpoints and their load-balancer addresses.
func TakenAddresses(clusters []clusterv1.Cluster, bridge string, vlan int) [][2]netip.Addr {
	segment := fmt.Sprintf("%s/%d", bridge, vlan)
	var taken [][2]netip.Addr
	for i := range clusters {
		a, err := addressesOf(&clusters[i])
		if err != nil || a.segment != segment {
			continue
		}
		for _, s := range slices.Concat([]addressSpan{a.endpoint}, a.machines, a.services) {
			taken = append(taken, [2]netip.Addr{s.first, s.last})
		}
	}
	return taken
}

// addressesChanged reports whether an update changes what the cluster holds on its nodes' network.
func addressesChanged(old, c *clusterv1.Cluster) bool {
	if old == nil {
		return true
	}
	for _, name := range []string{"network", "controlPlaneEndpoint", loadBalancerVariable} {
		was, _, err1 := optionalVariable[json.RawMessage](old, name)
		is, _, err2 := optionalVariable[json.RawMessage](c, name)
		if err1 != nil || err2 != nil || !bytes.Equal(was, is) {
			return true
		}
	}
	return false
}

// checkAddresses holds the cluster's addresses to checkAddresses against every other cluster.
func (h *Hooks) checkAddresses(ctx context.Context, cluster *clusterv1.Cluster) error {
	clusters := &clusterv1.ClusterList{}
	if err := h.Reader.List(ctx, clusters); err != nil {
		return fmt.Errorf("listing the clusters: %w", err)
	}
	return checkAddresses(cluster, clusters.Items)
}

// k3sServiceLB is the label k3s's servicelb puts on the DaemonSets that answer for Services.
const k3sServiceLB = "svccontroller.k3s.cattle.io/svcname"

// removeServiceLB deletes what k3s's servicelb left of the Services it answered for before the
// cluster's own load balancer took over: k3s cleans up after servicelb only where it still runs
// its cloud provider, which Kubehenge's clusters don't. A server still running servicelb while the
// control planes are replaced makes them again, so this goes on until none does.
func removeServiceLB(ctx context.Context, workload client.Client) error {
	daemonSets := &metav1.PartialObjectMetadata{}
	daemonSets.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("DaemonSet"))
	if err := workload.DeleteAllOf(ctx, daemonSets, client.InNamespace(metav1.NamespaceSystem), client.HasLabels{k3sServiceLB}); err != nil {
		return fmt.Errorf("removing servicelb's DaemonSets: %w", err)
	}
	return nil
}
