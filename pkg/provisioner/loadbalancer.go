package provisioner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// metalLBNamespace is where MetalLB runs. Its speakers need the host's network.
	metalLBNamespace = "metallb-system"
	// kubeVipMetrics is where the load balancer's kube-vip serves its metrics: on loopback, as the
	// control planes' does on 2112 beside it.
	kubeVipMetrics = "127.0.0.1:2113"
	// pointToPointBits is the prefix length from which a network has no network or broadcast address.
	pointToPointBits = 31
)

// The modes a cluster's load balancer announces addresses in: ARP on the nodes' network, or BGP to
// routers.
const (
	modeL2  = "l2"
	modeBGP = "bgp"
	// maxASN is the largest 4-byte autonomous system number.
	maxASN = 1<<32 - 1
	// metalLBPoolName names the pool and its advertisement. Peers carry it as a prefix, which marks
	// the objects Kubehenge owns among any a person adds beside them.
	metalLBPoolName = "kubehenge"
	// controlPlaneLabel keeps MetalLB's sessions off control-plane nodes, where kube-vip already holds
	// one for the control-plane address from the same node address.
	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
)

// LoadBalancer is a cluster's loadBalancer variable.
type LoadBalancer struct {
	Enabled        bool     `json:"enabled"`
	Implementation string   `json:"implementation"`
	Mode           string   `json:"mode"`
	Addresses      []string `json:"addresses"`
	BGP            *BGP     `json:"bgp"`
}

// BGP is the routers a cluster announces its addresses to, and the autonomous system it does so from.
type BGP struct {
	ASN   uint32    `json:"asn"`
	Peers []BGPPeer `json:"peers"`
	// Authenticated signs every session with the password in the cluster's bgp credential.
	Authenticated bool `json:"authenticated"`
	// RouterID is the BGP router id every node gives, "" for each node's own IPv4 address.
	RouterID string `json:"routerID,omitempty"`
}

// BGPPeer is a router every node peers with.
type BGPPeer struct {
	Address string `json:"address"`
	ASN     uint32 `json:"asn"`
}

// bgp reports whether the cluster announces its addresses over BGP.
func (l LoadBalancer) bgp() bool {
	return l.Mode == modeBGP
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
		return []chartProxy{{"load-balancer", metalLB, metalLBNamespace, charts.MetalLB, metalLBValues(lb)}}, nil
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

// metalLBValues run MetalLB in layer 2 mode without frr-k8s, the BGP backend its chart deploys by
// default as a privileged DaemonSet on the host's network, and with it in BGP mode, the backend its
// chart recommends since FRR in the speaker is deprecated. Its speakers answer from every node, the
// control planes too, which kubeadm labels as excluded from load balancers: a cluster without
// workers has no other nodes to answer from in layer 2.
func metalLBValues(lb LoadBalancer) map[string]any {
	values := map[string]any{
		"speaker":    map[string]any{"ignoreExcludeLB": true, "resources": reserved(cpu10m, memory64Mi)},
		"controller": map[string]any{"resources": reserved(cpu10m, memory64Mi)},
		"frrk8s":     map[string]any{"enabled": lb.bgp()},
	}
	if lb.bgp() {
		values["frr-k8s"] = map[string]any{"frrk8s": map[string]any{
			"resources":  frrK8sRequests.controller.values(),
			"frr":        map[string]any{"resources": frrK8sRequests.frr.values()},
			"reloader":   map[string]any{"resources": frrK8sRequests.reloader.values()},
			"frrMetrics": map[string]any{"resources": frrK8sRequests.metrics.values()},
			"frrStatus":  map[string]any{"resources": frrK8sRequests.status.values()},
		}}
	}
	return values
}

// containerRequest is what a container asks the scheduler for.
type containerRequest struct{ millicores, mebibytes int }

func (r containerRequest) values() map[string]any {
	return reserved(strconv.Itoa(r.millicores)+"m", strconv.Itoa(r.mebibytes)+"Mi")
}

// frrK8sRequests are the requests of the containers of frr-k8s, which MetalLB's chart deploys in BGP
// mode and which asks for nothing by itself: its pods would get the least CPU weight a node gives,
// and miss the keepalives that hold the sessions on a busy node. The controller's request is also
// that of the status cleaner, which the chart gives the same value. The node-wide DaemonSet runs
// the controller, frr, the reloader and the two exporters on every node.
var frrK8sRequests = struct{ controller, frr, reloader, metrics, status containerRequest }{
	controller: containerRequest{10, 32}, frr: containerRequest{10, 64}, reloader: containerRequest{5, 16},
	metrics: containerRequest{5, 16}, status: containerRequest{5, 16},
}

// BGPBackendRequests is what frr-k8s adds to the load balancer's pooled add-on figures in BGP mode,
// in millicores and MiB: one node's share of the DaemonSet and the status cleaner that runs once.
func BGPBackendRequests() (int, int) {
	r := frrK8sRequests
	cpu, memory := 0, 0
	for _, c := range []containerRequest{r.controller, r.frr, r.reloader, r.metrics, r.status, r.controller} {
		cpu += c.millicores
		memory += c.mebibytes
	}
	return cpu, memory
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
		"resources": reserved(cpu10m, "32Mi"),
	}
}

// MetalLB's kinds that announce the pool, and the group version each is served at.
const (
	metalLBGroup     = "metallb.io"
	l2Advertisement  = "L2Advertisement"
	bgpAdvertisement = "BGPAdvertisement"
	bgpPeerKind      = "BGPPeer"
	// The versions MetalLB's kinds are served at. BGPPeer alone is at the second.
	metalLBv1 = "v1beta1"
	metalLBv2 = "v1beta2"
)

// metalLBAnnouncers are the kinds of what announces the pool, with their versions. Only one mode's
// are kept.
var metalLBAnnouncers = map[string]string{l2Advertisement: metalLBv1, bgpAdvertisement: metalLBv1, bgpPeerKind: metalLBv2}

// peerName names the BGPPeer of a router.
func peerName(address string) string {
	return metalLBPoolName + "-peer-" + strings.NewReplacer(".", "-", ":", "-").Replace(address)
}

// metalLBPool is the address pool MetalLB hands out and announces, and its announcement: in layer 2
// one advertisement, in BGP an advertisement and a peer for each router. The sessions come from the
// workers only, as the control planes' kube-vip holds the control-plane address from the same node
// addresses to the same routers, which most routers refuse twice.
func metalLBPool(spans []addressSpan, lb LoadBalancer) []*unstructured.Unstructured {
	addresses := make([]any, 0, len(spans))
	for _, s := range spans {
		addresses = append(addresses, s.first.String()+"-"+s.last.String())
	}
	const name = metalLBPoolName
	objects := []*unstructured.Unstructured{metalLBObject("IPAddressPool", metalLBv1, name, map[string]any{"addresses": addresses})}
	if !lb.bgp() {
		return append(objects, metalLBObject(l2Advertisement, metalLBv1, name, map[string]any{"ipAddressPools": []any{name}}))
	}
	objects = append(objects, metalLBObject(bgpAdvertisement, metalLBv1, name, map[string]any{"ipAddressPools": []any{name}}))
	if lb.BGP == nil {
		return objects
	}
	for _, p := range lb.BGP.Peers {
		spec := map[string]any{
			"myASN": int64(lb.BGP.ASN), "peerASN": int64(p.ASN), "peerAddress": p.Address,
			"nodeSelectors": []any{map[string]any{matchExpressionsKey: []any{map[string]any{
				"key": controlPlaneLabel, "operator": "DoesNotExist",
			}}}},
		}
		if lb.BGP.Authenticated {
			spec["passwordSecret"] = map[string]any{"name": bgpPeerSecret, "namespace": metalLBNamespace}
		}
		// Without one, FRR takes the node's highest IPv4 address.
		if lb.BGP.RouterID != "" {
			spec["routerID"] = lb.BGP.RouterID
		}
		objects = append(objects, metalLBObject(bgpPeerKind, metalLBv2, peerName(p.Address), spec))
	}
	return objects
}

func metalLBObject(kind, version, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"kind": kind, apiVersionKey: metalLBGroup + "/" + version,
		"metadata": map[string]any{"name": name, "namespace": metalLBNamespace},
		"spec":     spec,
	}}
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
		if waiting, err = r.applyMetalLB(ctx, cluster, workload.client, spans, lb); err != nil {
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

// applyMetalLB keeps the password of MetalLB's sessions in the cluster and applies its pool. It
// holds the peers back, which name the password, until the credential has one to give.
func (r *Reconciler) applyMetalLB(ctx context.Context, cluster *clusterv1.Cluster, workload client.Client, spans []addressSpan, lb LoadBalancer) (bool, error) {
	p, err := r.bgpPassword(ctx, cluster, lb)
	if err != nil || p.state == bgpWaiting {
		return p.state == bgpWaiting, err
	}
	var waiting bool
	if p.state == bgpSigned {
		waiting, err = applyBGPPeerSecret(ctx, workload, p.password)
	} else {
		err = deleteBGPPeerSecret(ctx, workload)
	}
	if err != nil || waiting {
		return waiting, err
	}
	return applyMetalLBPool(ctx, workload, spans, lb)
}

// applyMetalLBPool applies MetalLB's address pool and what announces it, takes away what announced
// it in the other mode or to routers since removed, and reports whether it waits for MetalLB's CRDs
// or for its webhook, which checks the pool, to answer.
func applyMetalLBPool(ctx context.Context, workload client.Client, spans []addressSpan, lb LoadBalancer) (bool, error) {
	objects := metalLBPool(spans, lb)
	for _, o := range objects {
		err := workload.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), client.FieldOwner(fieldOwner), client.ForceOwnership)
		if meta.IsNoMatchError(err) || apierrors.IsInternalError(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("applying MetalLB's %s: %w", o.GetKind(), err)
		}
	}
	return false, pruneMetalLB(ctx, workload, objects)
}

// pruneMetalLB deletes the advertisements and peers Kubehenge made that the cluster's mode no
// longer has, and so stops the announcements before the new ones can be told from the old.
func pruneMetalLB(ctx context.Context, workload client.Client, keep []*unstructured.Unstructured) error {
	for kind, version := range metalLBAnnouncers {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: metalLBGroup, Version: version, Kind: kind + "List"})
		if err := workload.List(ctx, list, client.InNamespace(metalLBNamespace)); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("listing MetalLB's %ss: %w", kind, err)
		}
		for i := range list.Items {
			o := &list.Items[i]
			if !strings.HasPrefix(o.GetName(), metalLBPoolName) || slices.ContainsFunc(keep, func(k *unstructured.Unstructured) bool {
				return k.GetKind() == kind && k.GetName() == o.GetName()
			}) {
				continue
			}
			if err := workload.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting MetalLB's %s %s: %w", kind, o.GetName(), err)
			}
		}
	}
	return nil
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

// addressSpan is a run of addresses of one family, first to last.
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

// parseSpan reads a range, a network or a single address, of either family. A range's two ends
// are of one family.
func parseSpan(e string) (addressSpan, error) {
	if from, to, ok := strings.Cut(e, "-"); ok {
		first, err1 := netip.ParseAddr(strings.TrimSpace(from))
		last, err2 := netip.ParseAddr(strings.TrimSpace(to))
		if err1 != nil || err2 != nil || first.Is4() != last.Is4() || last.Less(first) || first.Zone() != "" || last.Zone() != "" {
			return addressSpan{}, fmt.Errorf("%q is not a range of addresses of one family, first to last", e)
		}
		return addressSpan{first, last}, nil
	}
	if strings.Contains(e, "/") {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return addressSpan{}, fmt.Errorf("%q is not a network", e)
		}
		p = p.Masked()
		return addressSpan{p.Addr(), lastAddress(p)}, nil
	}
	a, err := netip.ParseAddr(e)
	if err != nil || a.Zone() != "" {
		return addressSpan{}, fmt.Errorf("%q is not an address", e)
	}
	return addressSpan{a, a}, nil
}

// clusterAddresses are the addresses a cluster takes on its nodes' networks.
type clusterAddresses struct {
	// segment is the network's bridge and VLAN. Clusters on different ones share no addresses.
	segment string
	// routed is set when the cluster announces its addresses over BGP, so that the control-plane
	// endpoint and the load-balancer addresses may lie outside the nodes' network.
	routed bool
	// subnets are the nodes' networks of each of the cluster's families, the primary first.
	subnets  []nodeSubnet
	machines []addressSpan
	endpoint addressSpan
	services []addressSpan
}

func addressesOf(cluster *clusterv1.Cluster) (clusterAddresses, error) {
	var a clusterAddresses
	network, err := variable[nodeNetwork](cluster, networkVariable)
	if err != nil {
		return a, err
	}
	a.segment = fmt.Sprintf("%s/%d", network.Bridge, network.VLAN)
	if a.subnets, err = network.subnets(); err != nil {
		return a, err
	}
	for _, s := range a.subnets {
		machines, perr := parseAddresses(network.of(s.family).Addrs)
		if perr != nil {
			return a, unreadable{fmt.Errorf("%s.addresses: %w", familyPath(s.family), perr)}
		}
		a.machines = append(a.machines, machines...)
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
	a.routed = lb.bgp()
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
		// Cluster API refuses missing variables itself. Until the cluster has them, it holds no
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
			// A cluster whose variables don't read was refused or is being fixed. It holds no addresses yet.
			continue
		}
		problems = append(problems, own.clashes(o.Namespace+"/"+o.Name, &theirs)...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, ", "))
	}
	return nil
}

// problems are where the cluster's addresses clash with its own networks.
func (a *clusterAddresses) problems() []string {
	var problems []string
	for _, m := range a.machines {
		if n, ok := subnetOf(a.subnets, m.first); ok && (!n.prefix.Contains(m.first) || !n.prefix.Contains(m.last)) {
			problems = append(problems, fmt.Sprintf("node addresses %s are outside the network %s", m, n.prefix))
		}
		if m.overlaps(a.endpoint) {
			problems = append(problems, fmt.Sprintf("control-plane endpoint %s is inside node addresses %s", a.endpoint, m))
		}
	}
	primary := a.subnets[0]
	switch {
	case familyOf(a.endpoint.first) != primary.family:
		problems = append(problems, fmt.Sprintf("control-plane endpoint %s is %s, and the cluster's first family is %s. Give it a %s address",
			a.endpoint, familyOf(a.endpoint.first), primary.family, primary.family))
	case !a.routed && !primary.prefix.Contains(a.endpoint.first):
		problems = append(problems, fmt.Sprintf("control-plane endpoint %s is outside the network %s", a.endpoint, primary.prefix))
	case a.endpoint.first == primary.gateway:
		problems = append(problems, fmt.Sprintf("control-plane endpoint %s is the gateway", a.endpoint))
	}
	return append(problems, a.serviceProblems()...)
}

// serviceProblems are where the load-balancer addresses clash with the cluster's networks or with
// each other.
func (a *clusterAddresses) serviceProblems() []string {
	var problems []string
	for i, s := range a.services {
		n, ok := subnetOf(a.subnets, s.first)
		if !ok {
			problems = append(problems, fmt.Sprintf("load-balancer addresses %s are %s, which the cluster's nodes do not have", s, familyOf(s.first)))
			continue
		}
		if !a.routed && (!n.prefix.Contains(s.first) || !n.prefix.Contains(s.last)) {
			problems = append(problems, fmt.Sprintf("load-balancer addresses %s are outside the network %s", s, n.prefix))
		}
		problems = append(problems, a.taken(s, n)...)
		for _, t := range a.services[i+1:] {
			if s.overlaps(t) {
				problems = append(problems, fmt.Sprintf("load-balancer addresses %s and %s overlap", s, t))
			}
		}
	}
	return problems
}

// taken are where load-balancer addresses hold one the cluster's network of their family has for
// something else.
func (a *clusterAddresses) taken(s addressSpan, n nodeSubnet) []string {
	var problems []string
	if s.overlaps(addressSpan{n.gateway, n.gateway}) {
		problems = append(problems, fmt.Sprintf("load-balancer addresses %s include the gateway %s", s, n.gateway))
	}
	for _, reserved := range reservedAddresses(n.prefix) {
		if !s.overlaps(addressSpan{reserved, reserved}) {
			continue
		}
		what := "the network or broadcast address"
		if n.family == familyIPv6 {
			what = "the network's subnet-router anycast address"
		}
		problems = append(problems, fmt.Sprintf("load-balancer addresses %s include %s %s", s, what, reserved))
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
	for _, name := range []string{networkVariable, "controlPlaneEndpoint", loadBalancerVariable} {
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

// checkBGP holds a cluster that announces its addresses over BGP to what that can do. Its
// control-plane address is announced by kube-vip as a static pod, which Talos has no place for: it
// holds its virtual IP with its own layer 2 election and has no BGP option. MetalLB peers from the
// workers only, so a cluster of control planes alone would have no session for its Services.
func checkBGP(c *clusterv1.Cluster) error {
	lb, _, err := optionalVariable[LoadBalancer](c, loadBalancerVariable)
	if err != nil || !lb.bgp() {
		return err
	}
	var problems []string
	if isTalos(c) {
		problems = append(problems, "BGP is not available on Talos clusters: Talos holds the control-plane address with its own layer 2 election and has no BGP option")
	}
	if lb.Enabled && lb.implementation() == kubeVip {
		problems = append(problems, "BGP needs MetalLB: kube-vip's load balancer answers by ARP only")
	}
	if lb.Enabled && len(c.Spec.Topology.Workers.MachineDeployments) == 0 {
		problems = append(problems, "BGP load balancing needs worker nodes: MetalLB peers from workers only, as kube-vip already holds the control-plane address from the control planes' addresses to the same routers")
	}
	problems = append(problems, bgpSettingProblems(c, &lb)...)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, ", "))
	}
	return nil
}

// bgpSettingProblems are where the autonomous systems and routers a cluster's BGP names are no use.
func bgpSettingProblems(c *clusterv1.Cluster, lb *LoadBalancer) []string {
	if lb.BGP == nil {
		return []string{"BGP needs bgp settings: the cluster's ASN and at least one router"}
	}
	var problems []string
	if lb.BGP.ASN == 0 {
		problems = append(problems, fmt.Sprintf("the cluster's ASN 0 is outside 1 to %d", uint32(maxASN)))
	}
	if len(lb.BGP.Peers) == 0 {
		problems = append(problems, "BGP needs at least one router to peer with")
	}
	endpoint, _ := variable[string](c, "controlPlaneEndpoint")
	pool, _ := parseAddresses(lb.Addresses)
	families := clusterFamilies(c)
	if id := lb.BGP.RouterID; id != "" {
		if a, err := netip.ParseAddr(id); err != nil || !a.Is4() || a.IsUnspecified() {
			problems = append(problems, fmt.Sprintf("router id %q is not a dotted 32-bit id other than 0.0.0.0, such as 10.0.0.1", id))
		}
	} else if !slices.Contains(families, familyIPv4) {
		problems = append(problems, "BGP on an IPv6-only cluster needs bgp.routerID: a router id is a dotted 32-bit id, and the nodes have no IPv4 address to take theirs from. Set one, such as 10.0.0.1")
	}
	seen := map[netip.Addr]bool{}
	for _, p := range lb.BGP.Peers {
		a, err := netip.ParseAddr(strings.TrimSpace(p.Address))
		if err != nil || a.Zone() != "" {
			problems = append(problems, fmt.Sprintf("router %q is not an IP address", p.Address))
			continue
		}
		if !slices.Contains(families, familyOf(a)) {
			problems = append(problems, fmt.Sprintf("router %s is %s, which the cluster's nodes do not have", a, familyOf(a)))
		}
		announced := func(a netip.Addr) bool { return endpoint == a.String() || (lb.Enabled && inSpans(pool, a.String())) }
		problems = append(problems, peerProblems(a, p.ASN, seen, announced)...)
		seen[a] = true
	}
	return problems
}

// peerProblems are where a router's AS number is unusable, or its address is listed before or is
// one the cluster announces, which a router cannot also be reached at.
func peerProblems(a netip.Addr, asn uint32, seen map[netip.Addr]bool, announced func(netip.Addr) bool) []string {
	var problems []string
	if asn == 0 {
		problems = append(problems, fmt.Sprintf("router %s: ASN 0 is outside 1 to %d", a, uint32(maxASN)))
	}
	if seen[a] {
		problems = append(problems, fmt.Sprintf("router %s is listed twice", a))
	}
	if announced(a) {
		problems = append(problems, fmt.Sprintf("router %s is an address the cluster announces", a))
	}
	return problems
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
