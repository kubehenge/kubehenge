package provisioner

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

const (
	// FirewallNotWrittenCondition on a Machine says that its VM's firewall could not be written, so
	// the VM may run without it.
	FirewallNotWrittenCondition = "FirewallNotWritten"
	// FirewallSetBack is the Normal event on a Cluster when the controller sets back what was changed
	// on a VM's firewall, and FirewallNotWrittenCondition the Warning event when it cannot write it.
	FirewallSetBack = "FirewallSetBack"

	// vmNamePrefixAnnotation on a ProxmoxMachine, from its class's template, names its VM after its
	// Machine, with this prefix in place of the cluster's name at the start. Kubehenge's capmox build
	// reads it too.
	vmNamePrefixAnnotation = "kubehenge.dev/vm-name-prefix"

	// firewallOwn starts the comment of every rule the controller writes, which tells them from an
	// administrator's.
	firewallOwn = "kubehenge:"
	// firewallIPSet and firewallLBIPSet are the VM's IPSets of the sources it accepts everything
	// from and the destinations it accepts everything to.
	firewallIPSet   = "kubehenge"
	firewallLBIPSet = "kubehenge-lb"

	// The names Proxmox VE's firewall API and a Machine's owner reference go by.
	fwAccept    = "ACCEPT"
	fwGroupType = "group"
	fwEnable    = "enable"
	fwCIDR      = "cidr"
	fwSource    = "source"
	kindMachine = "Machine"

	firewallUnsettled = 5 * time.Second
	firewallSettled   = time.Minute
	// firewallFailing is how long a write fails before the Machine says so.
	firewallFailing = time.Minute
)

// VM firewall conditions' reasons.
const (
	firewallWriteFailed = "WriteFailed"
	firewallNotMachines = "VMNotMachines"
)

var netKey = regexp.MustCompile(`^net\d+$`)

// proxmoxWriter reads Proxmox VE's API and changes it; *proxmox.Client is one.
type proxmoxWriter interface {
	proxmoxEditor
	Post(ctx context.Context, path string, params, out any) error
	Delete(ctx context.Context, path string, out any) error
}

// VMFirewall writes the Proxmox VE firewall of each cluster VM, with the controller's own token:
// the VM's IPSets, its rules, its options and the firewall flag of each interface. A ProxmoxMachine
// has the VM's ID right after the clone, so the file is written before the VM first starts. What
// an administrator changes of what the controller owns is set back. No state is kept that a restart
// would miss: every reconcile reads the VM's firewall and writes what differs.
type VMFirewall struct {
	Client client.Client
	// Reader reads the appliance's ConfigMap past the cache, through Sources.
	Reader         client.Reader
	ClassNamespace string
	// WorkloadPool is the pool the clusters' own pools nest under.
	WorkloadPool string
	Proxmox      proxmoxWriter
	// Sources are the management cluster's and the appliance's addresses.
	Sources ManagementAddresses
	Events  events.EventRecorder
	Now     func() time.Time

	mu sync.Mutex
	// settled holds the machines whose VM ran with nothing to write, so that a write after it is
	// drift. Only the event depends on it.
	settled map[types.NamespacedName]bool
	// failing is when each machine's writes started to fail, and reported the last warning told
	// for each, so that a retry that fails the same way says nothing again.
	failing  map[types.NamespacedName]time.Time
	reported map[types.NamespacedName]string
}

// firewallRule is one rule of a VM's firewall, always enabled.
type firewallRule struct {
	Type, Action, Source, Dest, Proto, Dport, ICMPType, Comment string
}

// vmFirewall is what a cluster's VMs' firewalls should hold.
type vmFirewall struct {
	Enabled bool
	Rules   []firewallRule
	// Sources fill the IPSet kubehenge, and Destinations kubehenge-lb, each as IPSet entries.
	Sources, Destinations []string
}

// servingPorts are what a firewalled VM accepts from anyone while the serving ports are open, as
// protocol and port, or ICMP type.
var servingPorts = []firewallRule{
	{Proto: schemeTCP, Dport: "6443"},
	{Proto: schemeTCP, Dport: "80"},
	{Proto: schemeTCP, Dport: "443"},
	{Proto: schemeTCP, Dport: "30000:32767"},
	{Proto: "udp", Dport: "30000:32767"},
	{Proto: "icmp", ICMPType: "echo-request"},
	{Proto: "ipv6-icmp", ICMPType: "echo-request"},
}

// talosAPIPort is Talos's API, which is mTLS as the Kubernetes API is.
var talosAPIRule = firewallRule{Proto: schemeTCP, Dport: "50000"}

// desiredFirewall is what the VMs of cluster c should hold. It reads only management-cluster
// objects and calls nothing.
func desiredFirewall(c *clusterv1.Cluster, pc *unstructured.Unstructured, management []string, clusters []clusterv1.Cluster, byCluster map[types.NamespacedName]*unstructured.Unstructured) (vmFirewall, error) {
	fw, set, err := optionalVariable[Firewall](c, FirewallVariable)
	if err != nil {
		return vmFirewall{}, err
	}
	if !set || !fw.Enabled {
		return vmFirewall{}, nil
	}
	groups, _, err := optionalVariable[[]string](c, SecurityGroupsVariable)
	if err != nil {
		return vmFirewall{}, err
	}
	want := vmFirewall{Enabled: true}
	want.Rules = append(want.Rules, firewallRule{Type: "in", Action: fwAccept, Source: "+guest/" + firewallIPSet, Comment: firewallOwn + " the cluster's own traffic"})
	for _, group := range groups {
		want.Rules = append(want.Rules, firewallRule{Type: fwGroupType, Action: group, Comment: firewallOwn + " security group"})
	}
	want.Rules = append(want.Rules, firewallRule{Type: "in", Action: fwAccept, Dest: "+guest/" + firewallLBIPSet, Comment: firewallOwn + " load-balancer addresses"})
	var ports []firewallRule
	if servesPorts(c) {
		ports = slices.Clone(servingPorts)
		if isTalos(c) {
			ports = append(ports, talosAPIRule)
		}
	}
	for _, p := range fw.Ports {
		dport := strconv.Itoa(p.Port)
		if p.EndPort != 0 {
			dport += ":" + strconv.Itoa(p.EndPort)
		}
		ports = append(ports, firewallRule{Proto: p.Protocol, Dport: dport})
	}
	for i := range ports {
		ports[i].Type, ports[i].Action, ports[i].Comment = "in", fwAccept, firewallOwn+" serving port"
	}
	want.Rules = append(want.Rules, ports...)

	sources := slices.Clone(management)
	if pc != nil {
		sources = append(sources, proxmoxClusterAddresses(pc)...)
	}
	sources = append(sources, c.Spec.ClusterNetwork.Pods.CIDRBlocks...)
	sources = append(sources, meshSources(c, clusters, byCluster)...)
	if want.Sources, err = ipSetEntries(sources); err != nil {
		return vmFirewall{}, err
	}
	if servesPorts(c) {
		if want.Destinations, err = ipSetEntries(loadBalancerDestinations(c)); err != nil {
			return vmFirewall{}, err
		}
	}
	return want, nil
}

// ipSetEntries turns addresses, networks and address ranges into IPSet entries, sorted and each
// once.
func ipSetEntries(sources []string) ([]string, error) {
	entries := make([]string, 0, len(sources))
	for _, source := range sources {
		prefixes, err := pve.SourcePrefixes(strings.TrimSpace(source))
		if err != nil {
			return nil, fmt.Errorf("the source %q: %w", source, err)
		}
		for _, prefix := range prefixes {
			if prefix.IsSingleIP() {
				entries = append(entries, prefix.Addr().String())
			} else {
				entries = append(entries, prefix.String())
			}
		}
	}
	slices.Sort(entries)
	return slices.Compact(entries), nil
}

// canonicalIPSetEntry writes an address or a network in one form, so that what Proxmox VE holds
// compares with what was asked for.
func canonicalIPSetEntry(entry string) string {
	if prefix, err := netip.ParsePrefix(entry); err == nil {
		return prefix.Masked().String()
	}
	if addr, err := netip.ParseAddr(entry); err == nil {
		return netip.PrefixFrom(addr, addr.BitLen()).String()
	}
	return entry
}

// vmNameOf is the name capmox gives the VM of a ProxmoxMachine: its Machine's name with the
// prefix for the cluster's name, or its own name where its template sets no prefix.
func vmNameOf(pm *unstructured.Unstructured, clusterName string) string {
	prefix, ok := pm.GetAnnotations()[vmNamePrefixAnnotation]
	machine := ""
	for _, ref := range pm.GetOwnerReferences() {
		if ref.Kind == kindMachine {
			machine = ref.Name
		}
	}
	if !ok || machine == "" {
		return pm.GetName()
	}
	if prefix == "" {
		return machine
	}
	if rest, found := strings.CutPrefix(machine, clusterName+"-"); found && len(prefix)+1+len(rest) <= maxVMName {
		return prefix + "-" + rest
	}
	return machine
}

// SetupWithManager registers the reconciler for ProxmoxMachines, and for a change of their Cluster
// or of a Cluster Mesh peer.
func (r *VMFirewall) SetupWithManager(mgr ctrl.Manager) error {
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(schema.GroupVersionKind{Group: infrastructureGroup, Version: proxmoxMachineVersion, Kind: "ProxmoxMachine"})
	err := ctrl.NewControllerManagedBy(mgr).
		For(pm, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
			predicate.Funcs{UpdateFunc: vmIDOrStatusChanged},
		))).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.machinesOfCluster), builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Named("vm-firewall").
		Complete(r)
	if err != nil {
		return fmt.Errorf("setting up the VM firewall reconciler: %w", err)
	}
	return nil
}

// vmIDOrStatusChanged lets through a ProxmoxMachine whose VM ID or status changed.
func vmIDOrStatusChanged(e event.UpdateEvent) bool {
	was, okOld := e.ObjectOld.(*unstructured.Unstructured)
	now, okNew := e.ObjectNew.(*unstructured.Unstructured)
	if !okOld || !okNew {
		return true
	}
	return !reflect.DeepEqual(was.Object["spec"], now.Object["spec"]) || !reflect.DeepEqual(was.Object["status"], now.Object["status"])
}

// Reconcile implements reconcile.Reconciler.
func (r *VMFirewall) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster, pm, err := r.machineAndCluster(ctx, req.NamespacedName)
	if cluster == nil || err != nil {
		return ctrl.Result{}, err
	}
	vmid, _, err := unstructured.NestedInt64(pm.Object, "spec", "virtualMachineID")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ProxmoxMachine %s: %w", req.NamespacedName, err)
	}
	if vmid == 0 {
		return ctrl.Result{RequeueAfter: firewallUnsettled}, nil
	}
	guest, result, err := r.machinesVM(ctx, cluster, pm, vmid)
	if guest == nil || err != nil {
		return result, err
	}
	want, err := r.desired(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, r.failed(ctx, cluster, pm, firewallWriteFailed, err)
	}
	changed, err := r.sync(ctx, guest, &want)
	if err != nil {
		if isGone(err) {
			if again, gerr := r.exists(ctx, vmid); gerr == nil && !again {
				return ctrl.Result{}, nil
			}
		}
		return ctrl.Result{}, r.failed(ctx, cluster, pm, firewallWriteFailed, err)
	}
	r.succeeded(ctx, cluster, pm, guest, changed)
	if len(changed) == 0 && guest.Status == guestRunning {
		return ctrl.Result{RequeueAfter: firewallSettled}, nil
	}
	return ctrl.Result{RequeueAfter: firewallUnsettled}, nil
}

// machineAndCluster reads a ProxmoxMachine and its Cluster. The cluster is nil where there is
// nothing to do: the machine or the cluster is gone or going, or the cluster is not Kubehenge's.
func (r *VMFirewall) machineAndCluster(ctx context.Context, key types.NamespacedName) (*clusterv1.Cluster, *unstructured.Unstructured, error) {
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(schema.GroupVersionKind{Group: infrastructureGroup, Version: proxmoxMachineVersion, Kind: "ProxmoxMachine"})
	if err := r.Client.Get(ctx, key, pm); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(key)
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading ProxmoxMachine %s: %w", key, err)
	}
	clusterName := pm.GetLabels()[clusterv1.ClusterNameLabel]
	if !pm.GetDeletionTimestamp().IsZero() || clusterName == "" {
		return nil, nil, nil
	}
	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: pm.GetNamespace(), Name: clusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading Cluster %s: %w", clusterName, err)
	}
	if !Owns(cluster, r.ClassNamespace) || !cluster.DeletionTimestamp.IsZero() || RestoreInProgress(cluster) {
		return nil, nil, nil
	}
	return cluster, pm, nil
}

// machinesVM finds the VM of a ProxmoxMachine in Proxmox VE's list. It returns no VM where the
// reconcile ends, with the result. A VM that another pool or name holds now is never written.
func (r *VMFirewall) machinesVM(ctx context.Context, cluster *clusterv1.Cluster, pm *unstructured.Unstructured, vmid int64) (*pveGuest, ctrl.Result, error) {
	names, err := NamesFor(r.WorkloadPool, cluster.Namespace, cluster.Name)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	var guests []pveGuest
	if lerr := r.Proxmox.Get(ctx, "/cluster/resources?type=vm", &guests); lerr != nil {
		return nil, ctrl.Result{}, r.failed(ctx, cluster, pm, firewallWriteFailed, fmt.Errorf("listing the VMs: %w", lerr))
	}
	i := slices.IndexFunc(guests, func(g pveGuest) bool { return g.VMID == vmid && g.Type == qemuType })
	if i < 0 {
		return nil, ctrl.Result{}, nil
	}
	guest := &guests[i]
	if name := vmNameOf(pm, cluster.Name); guest.Pool != names.Pool || guest.Name != name {
		merr := fmt.Errorf("VM %d is named %q in pool %q, not the machine's %q in pool %q", vmid, guest.Name, guest.Pool, name, names.Pool)
		r.mark(ctx, cluster, pm, firewallNotMachines, merr)
		return nil, ctrl.Result{RequeueAfter: firewallSettled}, nil
	}
	if guest.Lock != "" {
		return nil, ctrl.Result{RequeueAfter: firewallUnsettled}, nil
	}
	return guest, ctrl.Result{}, nil
}

// machinesOfCluster maps a Cluster to the ProxmoxMachines of itself and of every cluster of its
// namespace that Cluster Mesh links it with, as the mesh's sources are in their IPSets.
func (r *VMFirewall) machinesOfCluster(ctx context.Context, o client.Object) []ctrl.Request {
	changed, ok := o.(*clusterv1.Cluster)
	if !ok {
		return nil
	}
	names := []string{changed.Name}
	list := &clusterv1.ClusterList{}
	if err := r.Client.List(ctx, list, client.InNamespace(changed.Namespace)); err == nil {
		for i := range list.Items {
			c := &list.Items[i]
			if c.Name == changed.Name {
				continue
			}
			mine, _, _ := optionalVariable[ClusterMesh](changed, clusterMeshVariable)
			theirs, _, _ := optionalVariable[ClusterMesh](c, clusterMeshVariable)
			if slices.Contains(theirs.Peers, changed.Name) || slices.Contains(mine.Peers, c.Name) {
				names = append(names, c.Name)
			}
		}
	}
	var requests []ctrl.Request
	for _, name := range names {
		pms := proxmoxMachineList()
		if err := r.Client.List(ctx, pms, client.InNamespace(changed.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: name}); err != nil {
			continue
		}
		for i := range pms.Items {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pms.Items[i])})
		}
	}
	return requests
}

// desired reads the objects the cluster's VM firewall comes from.
func (r *VMFirewall) desired(ctx context.Context, c *clusterv1.Cluster) (vmFirewall, error) {
	var management []string
	if r.Sources != nil {
		var err error
		if management, err = r.Sources.Addresses(ctx); err != nil {
			return vmFirewall{}, fmt.Errorf("reading the management addresses: %w", err)
		}
	}
	clusters := &clusterv1.ClusterList{}
	if err := r.Client.List(ctx, clusters); err != nil {
		return vmFirewall{}, fmt.Errorf("listing the clusters: %w", err)
	}
	pcs := &unstructured.UnstructuredList{}
	pcs.SetGroupVersionKind(proxmoxClusterListGVK)
	if err := r.Client.List(ctx, pcs); err != nil {
		return vmFirewall{}, fmt.Errorf("listing the ProxmoxClusters: %w", err)
	}
	byCluster := make(map[types.NamespacedName]*unstructured.Unstructured, len(pcs.Items))
	for i := range pcs.Items {
		if name := pcs.Items[i].GetLabels()[clusterv1.ClusterNameLabel]; name != "" {
			byCluster[types.NamespacedName{Namespace: pcs.Items[i].GetNamespace(), Name: name}] = &pcs.Items[i]
		}
	}
	return desiredFirewall(c, byCluster[client.ObjectKeyFromObject(c)], management, clusters.Items, byCluster)
}

// exists tells whether Proxmox VE still lists a VM.
func (r *VMFirewall) exists(ctx context.Context, vmid int64) (bool, error) {
	var guests []pveGuest
	if err := r.Proxmox.Get(ctx, "/cluster/resources?type=vm", &guests); err != nil {
		return false, fmt.Errorf("listing the VMs: %w", err)
	}
	return slices.ContainsFunc(guests, func(g pveGuest) bool { return g.VMID == vmid }), nil
}

// isGone tells whether Proxmox VE's answer says the VM or its file is no longer there.
func isGone(err error) bool {
	text := err.Error()
	return strings.Contains(text, "does not exist") || strings.Contains(text, "No such") || strings.Contains(text, "404")
}

type pveRule struct {
	Pos      int     `json:"pos"`
	Type     string  `json:"type"`
	Action   string  `json:"action"`
	Source   string  `json:"source"`
	Dest     string  `json:"dest"`
	Proto    string  `json:"proto"`
	Dport    string  `json:"dport"`
	ICMPType string  `json:"icmp-type"`
	Comment  string  `json:"comment"`
	Enable   pveFlag `json:"enable"`
	Digest   string  `json:"digest"`
}

func (p *pveRule) matches(w *firewallRule) bool {
	return bool(p.Enable) && p.Type == w.Type && p.Action == w.Action && p.Source == w.Source && p.Dest == w.Dest &&
		p.Proto == w.Proto && p.Dport == w.Dport && p.ICMPType == w.ICMPType && p.Comment == w.Comment
}

type pveOptions struct {
	Enable   pveFlag `json:"enable"`
	PolicyIn string  `json:"policy_in"`
	Digest   string  `json:"digest"`
}

type pveIPSetEntry struct {
	CIDR string `json:"cidr"`
}

// sync writes the VM's firewall to want and says what it set. It reads the five things the
// firewall is made of and writes only what differs. Each write carries the digest of what it read
// where Proxmox VE takes one, so that an edit in between is refused and read again next time. The
// order keeps every state in between from cutting the cluster's own traffic: IPSets, then rules,
// then the options that switch the firewall on, then the interfaces.
func (r *VMFirewall) sync(ctx context.Context, g *pveGuest, want *vmFirewall) ([]string, error) {
	base := fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(g.Node), g.VMID)
	var changed []string
	var options pveOptions
	if err := r.Proxmox.Get(ctx, base+"/firewall/options", &options); err != nil {
		return nil, fmt.Errorf("reading the firewall options of VM %d: %w", g.VMID, err)
	}
	if !want.Enabled {
		return r.syncOff(ctx, base, g.VMID, &options)
	}
	for _, set := range []struct {
		name    string
		entries []string
	}{{firewallIPSet, want.Sources}, {firewallLBIPSet, want.Destinations}} {
		did, err := r.syncIPSet(ctx, base, set.name, set.entries)
		if err != nil {
			return changed, fmt.Errorf("IPSet %s of VM %d: %w", set.name, g.VMID, err)
		}
		if did {
			changed = append(changed, "the IPSet "+set.name)
		}
	}
	did, err := r.syncRules(ctx, base, want.Rules)
	if err != nil {
		return changed, fmt.Errorf("the firewall rules of VM %d: %w", g.VMID, err)
	}
	if did {
		changed = append(changed, "the rules")
	}
	if !bool(options.Enable) || options.PolicyIn != "DROP" {
		params := map[string]any{fwEnable: 1, "policy_in": "DROP"}
		if options.Digest != "" {
			params["digest"] = options.Digest
		}
		if err = r.Proxmox.Put(ctx, base+"/firewall/options", params, nil); err != nil {
			return changed, fmt.Errorf("writing the firewall options of VM %d: %w", g.VMID, err)
		}
		changed = append(changed, "the options")
	}
	did, err = r.syncInterfaces(ctx, base, g.VMID)
	if err != nil {
		return changed, err
	}
	if did {
		changed = append(changed, "the interfaces' firewall flag")
	}
	return changed, nil
}

// syncOff takes Kubehenge's firewall off a VM that holds it, and leaves any other alone.
func (r *VMFirewall) syncOff(ctx context.Context, base string, vmid int64, options *pveOptions) ([]string, error) {
	var rules []pveRule
	if err := r.Proxmox.Get(ctx, base+"/firewall/rules", &rules); err != nil {
		return nil, fmt.Errorf("reading the firewall rules of VM %d: %w", vmid, err)
	}
	if !slices.ContainsFunc(rules, func(p pveRule) bool { return strings.HasPrefix(p.Comment, firewallOwn) }) {
		return nil, nil
	}
	if err := r.deleteOwn(ctx, base, 0); err != nil {
		return nil, fmt.Errorf("the firewall rules of VM %d: %w", vmid, err)
	}
	for _, name := range []string{firewallIPSet, firewallLBIPSet} {
		path := base + "/firewall/ipset/" + name + "?force=1"
		if err := r.Proxmox.Delete(ctx, path, nil); err != nil && !isGone(err) {
			return nil, fmt.Errorf("deleting IPSet %s of VM %d: %w", name, vmid, err)
		}
	}
	if bool(options.Enable) {
		params := map[string]any{fwEnable: 0}
		if options.Digest != "" {
			params["digest"] = options.Digest
		}
		if err := r.Proxmox.Put(ctx, base+"/firewall/options", params, nil); err != nil {
			return nil, fmt.Errorf("switching the firewall of VM %d off: %w", vmid, err)
		}
	}
	return []string{"the firewall, which is off"}, nil
}

// syncIPSet makes the IPSet hold exactly the entries given, creating it where it is missing. New
// entries go in before old ones go.
func (r *VMFirewall) syncIPSet(ctx context.Context, base, name string, entries []string) (bool, error) {
	var present []pveIPSetEntry
	path := base + "/firewall/ipset/" + name
	err := r.Proxmox.Get(ctx, path, &present)
	missing := err != nil
	changed := false
	if missing {
		if err := r.Proxmox.Post(ctx, base+"/firewall/ipset", map[string]any{"name": name, "comment": firewallIPSet}, nil); err != nil {
			return false, fmt.Errorf("creating it: %w", err)
		}
		changed = true
	}
	have := make(map[string]string, len(present))
	for _, e := range present {
		have[canonicalIPSetEntry(e.CIDR)] = e.CIDR
	}
	wanted := make(map[string]bool, len(entries))
	for _, entry := range entries {
		key := canonicalIPSetEntry(entry)
		wanted[key] = true
		if _, ok := have[key]; ok {
			continue
		}
		if err := r.Proxmox.Post(ctx, path, map[string]any{fwCIDR: entry}, nil); err != nil {
			return changed, fmt.Errorf("adding %s: %w", entry, err)
		}
		changed = true
	}
	for key, cidr := range have {
		if wanted[key] {
			continue
		}
		// The entry is a path segment, and a network's slash would split it.
		if err := r.Proxmox.Delete(ctx, path+"/"+url.PathEscape(cidr), nil); err != nil {
			return changed, fmt.Errorf("removing %s: %w", cidr, err)
		}
		changed = true
	}
	return changed, nil
}

// syncRules puts the wanted rules at the top, in order, and takes the controller's other rules off.
// The block goes in from its last rule, each at the top, so the rules in force are always the new
// block, or the new block above the old rules. Anyone else's rules keep their order below it. Each
// step reads the rules again, so a restart or a refused write only repeats it.
func (r *VMFirewall) syncRules(ctx context.Context, base string, want []firewallRule) (bool, error) {
	changed := false
	for range 2*len(want) + 64 {
		var rules []pveRule
		if err := r.Proxmox.Get(ctx, base+"/firewall/rules", &rules); err != nil {
			return changed, fmt.Errorf("reading them: %w", err)
		}
		slices.SortStableFunc(rules, func(a, b pveRule) int { return a.Pos - b.Pos })
		if k := blockAtTop(rules, want); k < len(want) {
			digest := ""
			if len(rules) > 0 {
				digest = rules[0].Digest
			}
			rule := want[len(want)-k-1]
			if err := r.Proxmox.Post(ctx, base+"/firewall/rules", ruleParams(&rule, digest), nil); err != nil {
				return changed, fmt.Errorf("adding %s: %w", describeRule(&rule), err)
			}
			changed = true
			continue
		}
		if !slices.ContainsFunc(rules[len(want):], func(p pveRule) bool { return strings.HasPrefix(p.Comment, firewallOwn) }) {
			return changed, nil
		}
		// The last first, so the positions of the rest hold.
		if err := r.deleteOwn(ctx, base, len(want)); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, errors.New("it keeps changing")
}

// blockAtTop says how many of the block's last rules already sit at the top, in order.
func blockAtTop(rules []pveRule, want []firewallRule) int {
	for n := len(want); n > 0; n-- {
		if len(rules) >= n && rulesAre(rules[:n], want[len(want)-n:]) {
			return n
		}
	}
	return 0
}

// ruleParams are the form fields that add a rule at the top.
func ruleParams(rule *firewallRule, digest string) map[string]any {
	params := map[string]any{"type": rule.Type, "action": rule.Action, fwEnable: 1, "comment": rule.Comment, "pos": 0}
	for key, v := range map[string]string{fwSource: rule.Source, "dest": rule.Dest, "proto": rule.Proto, "dport": rule.Dport, "icmp-type": rule.ICMPType} {
		if v != "" {
			params[key] = v
		}
	}
	if digest != "" {
		params["digest"] = digest
	}
	return params
}

// deleteOwn deletes the controller's rules from position from on, the last first.
func (r *VMFirewall) deleteOwn(ctx context.Context, base string, from int) error {
	for {
		var rules []pveRule
		if err := r.Proxmox.Get(ctx, base+"/firewall/rules", &rules); err != nil {
			return fmt.Errorf("reading them: %w", err)
		}
		pos := -1
		for i := range rules {
			if p := &rules[i]; p.Pos >= from && strings.HasPrefix(p.Comment, firewallOwn) && p.Pos > pos {
				pos = p.Pos
			}
		}
		if pos < 0 {
			return nil
		}
		path := fmt.Sprintf("%s/firewall/rules/%d", base, pos)
		if rules[0].Digest != "" {
			path += "?digest=" + url.QueryEscape(rules[0].Digest)
		}
		if err := r.Proxmox.Delete(ctx, path, nil); err != nil {
			return fmt.Errorf("removing the rule at position %d: %w", pos, err)
		}
	}
}

func rulesAre(have []pveRule, want []firewallRule) bool {
	for i := range want {
		if !have[i].matches(&want[i]) {
			return false
		}
	}
	return true
}

func describeRule(r *firewallRule) string {
	switch {
	case r.Type == fwGroupType:
		return "security group " + r.Action
	case r.Source != "":
		return "the rule accepting the cluster's own traffic"
	case r.Dest != "":
		return "the rule accepting the load-balancer addresses"
	case r.ICMPType != "":
		return "the rule accepting " + r.Proto + " " + r.ICMPType
	}
	return "the rule accepting " + r.Proto + "/" + r.Dport
}

// syncInterfaces sets the firewall flag on each interface of the VM that lacks it, keeping the
// rest of the interface as Proxmox VE holds it. Proxmox VE filters an interface only when the VM's
// firewall and the interface's flag are both on.
func (r *VMFirewall) syncInterfaces(ctx context.Context, base string, vmid int64) (bool, error) {
	var config map[string]any
	if err := r.Proxmox.Get(ctx, base+"/config", &config); err != nil {
		return false, fmt.Errorf("reading the configuration of VM %d: %w", vmid, err)
	}
	params := map[string]any{}
	for key, v := range config {
		value, ok := v.(string)
		if !ok || !netKey.MatchString(key) {
			continue
		}
		var parts []string
		for part := range strings.SplitSeq(value, ",") {
			if !strings.HasPrefix(part, "firewall=") {
				parts = append(parts, part)
			}
		}
		if flagged := strings.Join(append(slices.Clone(parts), "firewall=1"), ","); !strings.Contains(","+value+",", ",firewall=1,") {
			params[key] = flagged
		}
	}
	if len(params) == 0 {
		return false, nil
	}
	if digest, ok := config["digest"].(string); ok && digest != "" {
		params["digest"] = digest
	}
	if err := r.Proxmox.Put(ctx, base+"/config", params, nil); err != nil {
		return false, fmt.Errorf("setting the firewall flag of VM %d's interfaces: %w", vmid, err)
	}
	return true, nil
}

func (r *VMFirewall) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *VMFirewall) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.settled, key)
	delete(r.failing, key)
	delete(r.reported, key)
}

// succeeded records a pass that wrote nothing, or what it wrote, and takes the condition off.
func (r *VMFirewall) succeeded(ctx context.Context, c *clusterv1.Cluster, pm *unstructured.Unstructured, g *pveGuest, changed []string) {
	key := client.ObjectKeyFromObject(pm)
	r.mu.Lock()
	was := r.settled[key]
	if r.settled == nil {
		r.settled = map[types.NamespacedName]bool{}
	}
	r.settled[key] = len(changed) == 0 && g.Status == "running"
	delete(r.failing, key)
	delete(r.reported, key)
	r.mu.Unlock()
	r.condition(ctx, c, pm, nil)
	if was && len(changed) > 0 && r.Events != nil {
		r.Events.Eventf(c, nil, corev1.EventTypeNormal, FirewallSetBack, "KeepFirewall",
			"The firewall of VM %d (%s) was changed, and the controller set back %s.", g.VMID, g.Name, strings.Join(changed, ", "))
	}
}

// failed records a failure and returns it. After firewallFailing of it, the Machine says so.
func (r *VMFirewall) failed(ctx context.Context, c *clusterv1.Cluster, pm *unstructured.Unstructured, reason string, err error) error {
	key := client.ObjectKeyFromObject(pm)
	r.mu.Lock()
	if r.failing == nil {
		r.failing = map[types.NamespacedName]time.Time{}
	}
	since, ok := r.failing[key]
	if !ok {
		since = r.now()
		r.failing[key] = since
	}
	delete(r.settled, key)
	r.mu.Unlock()
	if r.now().Sub(since) >= firewallFailing {
		r.mark(ctx, c, pm, reason, err)
	}
	return err
}

// mark says on the Machine that the VM's firewall is not written, and on the Cluster once for each
// cause.
func (r *VMFirewall) mark(ctx context.Context, c *clusterv1.Cluster, pm *unstructured.Unstructured, reason string, cause error) {
	key := client.ObjectKeyFromObject(pm)
	message := shorten("The firewall of this machine's VM is not written: "+cause.Error(), watchEventMax)
	r.condition(ctx, c, pm, &metav1.Condition{Status: metav1.ConditionTrue, Reason: reason, Message: message})
	r.mu.Lock()
	if r.reported == nil {
		r.reported = map[types.NamespacedName]string{}
	}
	told := r.reported[key] == reason+message
	r.reported[key] = reason + message
	r.mu.Unlock()
	if !told && r.Events != nil {
		r.Events.Eventf(c, nil, corev1.EventTypeWarning, FirewallNotWrittenCondition, "KeepFirewall", "Machine %s: %s", pm.GetName(), message)
	}
}

// condition sets FirewallNotWrittenCondition on the ProxmoxMachine's Machine, or takes it off when
// cond is nil.
func (r *VMFirewall) condition(ctx context.Context, c *clusterv1.Cluster, pm *unstructured.Unstructured, cond *metav1.Condition) {
	machine := ""
	for _, ref := range pm.GetOwnerReferences() {
		if ref.Kind == "Machine" {
			machine = ref.Name
		}
	}
	if machine == "" {
		return
	}
	m := &clusterv1.Machine{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: pm.GetNamespace(), Name: machine}, m); err != nil {
		return
	}
	before := m.DeepCopy()
	if cond == nil {
		meta.RemoveStatusCondition(&m.Status.Conditions, FirewallNotWrittenCondition)
	} else {
		set := *cond
		set.Type = FirewallNotWrittenCondition
		meta.SetStatusCondition(&m.Status.Conditions, set)
	}
	if equalConditions(before.Status.Conditions, m.Status.Conditions) {
		return
	}
	if err := r.Client.Status().Patch(ctx, m, client.MergeFrom(before)); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "recording the VM firewall's state on the machine", "cluster", c.Name)
	}
}
