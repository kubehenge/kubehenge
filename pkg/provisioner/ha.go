package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/luthermonson/go-proxmox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// HAHook holds a control-plane Machine's deletion until its VM is out of Proxmox VE HA: Proxmox VE
// refuses to destroy a VM that is an HA resource, and capmox destroys VMs without taking them out.
// It waits for no other hook, since the control-plane providers' own hooks wait for it.
const HAHook = clusterv1.PreTerminateDeleteHookAnnotationPrefix + "/kubehenge-ha"

// LocateAnnotation names, on a ProxmoxMachine, the node its VM was last seen on. Changing it has
// capmox reconcile the machine and find a VM that HA moved: capmox destroys a VM on the node it
// recorded, and takes a VM missing from there for one already gone.
const LocateAnnotation = "kubehenge.dev/proxmox-node"

// proxmoxHAVariable lets a cluster opt out of Proxmox VE HA; unset means it takes part.
const proxmoxHAVariable = "proxmoxHA"

// haFieldOwner writes the hook: RKE2's and k3s's providers drop the managed fields of "manager".
const haFieldOwner = client.FieldOwner("kubehenge-ha")

// proxmoxMachineVersion is the capmox API the ClusterClasses create ProxmoxMachines with.
const proxmoxMachineVersion = "v1alpha2"

// HAComment marks the HA resources and rules of the cluster whose pool is given, and nothing else:
// the controller changes no HA object without it.
func HAComment(pool string) string { return haCommentPrefix + pool }

const haCommentPrefix = "Kubehenge cluster "

// The cluster's rules: keeping its control-plane VMs apart, and on its nodes.
const (
	apartRule = "apart"
	nodesRule = "nodes"
)

// haRuleID names one of the cluster's rules.
func haRuleID(pool, rule string) string {
	sum := sha256.Sum256([]byte(pool))
	return "kubehenge-" + hex.EncodeToString(sum[:8]) + "-" + rule
}

// haCluster is how one cluster takes part in HA: its objects' comment, and whether its
// control-plane VMs are HA resources.
type haCluster struct {
	comment   string
	takesPart bool
}

// HAReconciler has Proxmox VE HA keep each cluster's control-plane VMs on separate nodes, within
// the nodes the cluster may use, and restart them elsewhere when a node fails.
type HAReconciler struct {
	Client client.Client
	// Machines lists control-plane Machines; the manager's cache holds only those.
	Machines       client.Reader
	ClassNamespace string
	WorkloadPool   string
	// Proxmox reads where the VMs run and the storages, with the controller's own token.
	Proxmox *proxmox.Client
	// HA writes HA resources and rules with the token that holds Sys.Console on /. Without it the
	// reconciler only takes back the hooks it set, once capmox knows where each VM is.
	HA *proxmox.Client
}

// SetupWithManager registers the reconciler for Clusters and their control-plane Machines.
func (r *HAReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toCluster := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []ctrl.Request {
		name := o.GetLabels()[clusterv1.ClusterNameLabel]
		if name == "" {
			return nil
		}
		return []ctrl.Request{{Namespace: o.GetNamespace(), Name: name}}
	})
	err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Watches(&clusterv1.Machine{}, toCluster).
		Named("proxmox-ha").
		Complete(r)
	if err != nil {
		return fmt.Errorf("setting up the Proxmox VE HA reconciler: %w", err)
	}
	return nil
}

type haResource struct {
	SID     string `json:"sid"`
	Comment string `json:"comment"`
}

type haRuleConfig struct {
	Rule      string `json:"rule"`
	Type      string `json:"type"`
	Resources string `json:"resources"`
	Nodes     string `json:"nodes"`
}

type haStatus struct {
	ManagerStatus haManagerStatus `json:"manager_status"`
}

type haManagerStatus struct {
	// NodeStatus has every node the HA manager knows, whatever its state: Proxmox VE sizes rules
	// by them.
	NodeStatus map[string]string `json:"node_status"`
}

// haState is what Proxmox VE holds: HA resources by ID, rules by ID, the nodes HA knows, and the
// node each of the token's VMs runs on.
type haState struct {
	resources map[string]haResource
	rules     map[string]haRuleConfig
	nodes     []string
	located   map[int64]string
}

// Reconcile implements reconcile.Reconciler.
func (r *HAReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading cluster %s: %w", req.NamespacedName, err)
	}
	if !Owns(cluster, r.ClassNamespace) {
		return ctrl.Result{}, nil
	}
	names, err := NamesFor(r.WorkloadPool, cluster.Namespace, cluster.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	machines := &clusterv1.MachineList{}
	err = r.Machines.List(ctx, machines, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing the cluster's control-plane machines: %w", err)
	}
	s, err := r.read(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	takesPart, allowed, err := r.placement(ctx, cluster, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	members, waiting, err := r.reconcileMachines(ctx, machines.Items, s, haCluster{comment: HAComment(names.Pool), takesPart: takesPart})
	if err != nil {
		return ctrl.Result{}, err
	}
	if r.HA != nil {
		if err := r.reconcileRules(ctx, names.Pool, s, members, allowed); err != nil {
			return ctrl.Result{}, err
		}
		// Proxmox VE lists the nodes once its HA manager has started, which the first HA resource
		// does; the rule keeping VMs apart can be sized only then.
		waiting = waiting || len(members) > 1 && len(s.nodes) == 0
	}
	if waiting {
		return ctrl.Result{RequeueAfter: waitInterval}, nil
	}
	return ctrl.Result{RequeueAfter: resync}, nil
}

// read gathers what the reconcile decides on. Without the HA token it reads only where VMs run.
func (r *HAReconciler) read(ctx context.Context) (*haState, error) {
	s := &haState{resources: map[string]haResource{}, rules: map[string]haRuleConfig{}, located: map[int64]string{}}
	var vms []struct {
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
	}
	if err := r.Proxmox.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return nil, fmt.Errorf("listing the VMs: %w", err)
	}
	for _, vm := range vms {
		s.located[vm.VMID] = vm.Node
	}
	if r.HA == nil {
		return s, nil
	}
	var resources []haResource
	if err := r.HA.Get(ctx, "/cluster/ha/resources", &resources); err != nil {
		return nil, fmt.Errorf("listing the HA resources: %w", err)
	}
	for _, res := range resources {
		s.resources[res.SID] = res
	}
	var rules []haRuleConfig
	if err := r.HA.Get(ctx, "/cluster/ha/rules", &rules); err != nil {
		return nil, fmt.Errorf("listing the HA rules: %w", err)
	}
	for _, rule := range rules {
		s.rules[rule.Rule] = rule
	}
	var status haStatus
	if err := r.HA.Get(ctx, "/cluster/ha/status/manager_status", &status); err != nil {
		return nil, fmt.Errorf("reading the HA manager's status: %w", err)
	}
	for n := range status.ManagerStatus.NodeStatus {
		s.nodes = append(s.nodes, n)
	}
	slices.Sort(s.nodes)
	return s, nil
}

// placement tells whether the cluster's control-plane VMs take part in HA, and the nodes HA may
// move them to when that is fewer than all: those the cluster may use that have its storage.
func (r *HAReconciler) placement(ctx context.Context, cluster *clusterv1.Cluster, s *haState) (bool, []string, error) {
	if r.HA == nil || !cluster.DeletionTimestamp.IsZero() {
		return false, nil, nil
	}
	opted, set, err := optionalVariable[bool](cluster, proxmoxHAVariable)
	if err != nil || set && !opted {
		return false, nil, err
	}
	name, err := variable[string](cluster, "storage")
	if err != nil {
		return false, nil, err
	}
	list, err := storages(ctx, r.Proxmox)
	if err != nil {
		return false, nil, err
	}
	i := slices.IndexFunc(list, func(c storageConfig) bool { return c.Storage == name })
	if i < 0 {
		return false, nil, fmt.Errorf("storage %s is not visible to the token", name)
	}
	// HA moves and restarts a VM only where its disks are reachable from the other nodes.
	if list[i].Shared == 0 {
		return false, nil, nil
	}
	chosen, _, err := optionalVariable[[]string](cluster, allowedNodesVariable)
	if err != nil {
		return false, nil, err
	}
	return true, haNodes(s.nodes, chosen, list[i].Nodes), nil
}

// haNodes are the nodes HA may run the cluster's VMs on, or nil for every node it knows.
func haNodes(known, chosen []string, storageNodes string) []string {
	var limited []string
	if storageNodes != "" {
		limited = strings.Split(storageNodes, ",")
	}
	nodes := slices.DeleteFunc(slices.Clone(known), func(n string) bool {
		return len(chosen) > 0 && !slices.Contains(chosen, n) || limited != nil && !slices.Contains(limited, n)
	})
	if len(nodes) == 0 || len(nodes) == len(known) {
		return nil
	}
	return nodes
}

// reconcileMachines makes each control-plane VM that has joined the cluster an HA resource while
// the cluster takes part, and takes the others out. It returns the HA resources of the cluster,
// and whether a hook waits for capmox to find a VM that HA moved.
func (r *HAReconciler) reconcileMachines(ctx context.Context, machines []clusterv1.Machine, s *haState, hc haCluster) ([]string, bool, error) {
	var members []string
	waiting := false
	seen := map[string]bool{}
	for i := range machines {
		m := &machines[i]
		got, err := r.reconcileMachine(ctx, m, s, hc)
		if err != nil {
			return nil, false, fmt.Errorf("machine %s: %w", m.Name, err)
		}
		seen[got.sid] = true
		if got.member {
			members = append(members, got.sid)
		}
		waiting = waiting || got.waiting
	}
	// A VM whose machine is gone leaves its HA resource behind only if the controller missed it.
	for sid, res := range s.resources {
		if res.Comment == hc.comment && !seen[sid] {
			if err := r.removeResource(ctx, sid); err != nil {
				return nil, false, err
			}
			s.forget(sid)
		}
	}
	slices.Sort(members)
	return members, waiting, nil
}

// machineHA is where one control-plane machine stands: its VM's HA resource, whether that is one
// of the cluster's, and whether the machine's hook waits for capmox to find its VM.
type machineHA struct {
	sid     string
	member  bool
	waiting bool
}

func (r *HAReconciler) reconcileMachine(ctx context.Context, m *clusterv1.Machine, s *haState, hc haCluster) (machineHA, error) {
	pm, vmid, err := r.proxmoxMachine(ctx, m)
	if err != nil {
		return machineHA{}, err
	}
	got := machineHA{sid: "vm:" + strconv.FormatInt(vmid, 10)}
	moved := false
	if pm != nil {
		if moved, err = r.locate(ctx, pm, s.located[vmid]); err != nil {
			return got, err
		}
	}
	if hc.takesPart && m.DeletionTimestamp.IsZero() && vmid > 0 && m.Status.NodeRef.IsDefined() {
		// The hook first, so that no deletion can reach an HA resource without it.
		if err := r.patchHook(ctx, m, `"kubehenge"`); err != nil {
			return got, err
		}
		if _, ok := s.resources[got.sid]; !ok {
			if err := r.addResource(ctx, got.sid, hc.comment); err != nil {
				return got, err
			}
			s.resources[got.sid] = haResource{SID: got.sid, Comment: hc.comment}
		}
		got.member = true
		return got, nil
	}
	if res, ok := s.resources[got.sid]; ok && vmid > 0 && res.Comment == hc.comment {
		if err := r.removeResource(ctx, got.sid); err != nil {
			return got, err
		}
		s.forget(got.sid)
	}
	if moved {
		got.waiting = m.Annotations[HAHook] != ""
		return got, nil
	}
	return got, r.patchHook(ctx, m, "null")
}

// proxmoxMachine reads the machine's ProxmoxMachine and its VMID, or nil and 0 before it exists.
func (r *HAReconciler) proxmoxMachine(ctx context.Context, m *clusterv1.Machine) (*unstructured.Unstructured, int64, error) {
	return proxmoxMachineOf(ctx, r.Client, m)
}

// proxmoxMachineOf reads a machine's ProxmoxMachine and its VMID, or nil and 0 before it exists.
func proxmoxMachineOf(ctx context.Context, c client.Reader, m *clusterv1.Machine) (*unstructured.Unstructured, int64, error) {
	ref := m.Spec.InfrastructureRef
	if ref.Name == "" {
		return nil, 0, nil
	}
	pm := &unstructured.Unstructured{}
	pm.SetAPIVersion(ref.APIGroup + "/" + proxmoxMachineVersion)
	pm.SetKind(ref.Kind)
	err := c.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: ref.Name}, pm)
	if apierrors.IsNotFound(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s %s: %w", ref.Kind, ref.Name, err)
	}
	vmid, _, err := unstructured.NestedInt64(pm.Object, "spec", "virtualMachineID")
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", ref.Kind, ref.Name, err)
	}
	return pm, vmid, nil
}

// locate has capmox look for the VM again when it runs elsewhere than capmox recorded, and tells
// whether it does.
func (r *HAReconciler) locate(ctx context.Context, pm *unstructured.Unstructured, node string) (bool, error) {
	recorded, _, _ := unstructured.NestedString(pm.Object, "status", "proxmoxNode")
	if node == "" || node == recorded {
		return false, nil
	}
	if pm.GetAnnotations()[LocateAnnotation] == node {
		return true, nil
	}
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, LocateAnnotation, node)
	if err := r.Client.Patch(ctx, pm, client.RawPatch(types.MergePatchType, patch), haFieldOwner); err != nil {
		return true, fmt.Errorf("marking %s with its VM's node: %w", pm.GetName(), err)
	}
	return true, nil
}

// patchHook sets the hook to value, JSON for its annotation's value or null to remove it.
func (r *HAReconciler) patchHook(ctx context.Context, m *clusterv1.Machine, value string) error {
	if (m.Annotations[HAHook] != "") == (value != "null") {
		return nil
	}
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%s}}}`, HAHook, value)
	target := &clusterv1.Machine{}
	target.Namespace, target.Name = m.Namespace, m.Name
	if err := r.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch), haFieldOwner); err != nil {
		return fmt.Errorf("setting the Proxmox VE HA hook: %w", err)
	}
	return nil
}

func (r *HAReconciler) addResource(ctx context.Context, sid, comment string) error {
	err := r.HA.Post(ctx, "/cluster/ha/resources", map[string]any{"sid": sid, "state": "started", "comment": comment}, nil)
	if err != nil && !strings.Contains(err.Error(), "already defined") {
		return fmt.Errorf("making %s an HA resource: %w", sid, err)
	}
	return nil
}

// removeResource takes a VM out of HA and out of every rule naming it; the VM keeps running.
func (r *HAReconciler) removeResource(ctx context.Context, sid string) error {
	err := r.HA.Delete(ctx, "/cluster/ha/resources/"+url.PathEscape(sid), nil)
	if err != nil && !isMissing(err) {
		return fmt.Errorf("taking %s out of HA: %w", sid, err)
	}
	return nil
}

// reconcileRules keeps the cluster's HA resources on separate nodes and, when it may use fewer
// than all, on its own. Proxmox VE refuses a rule it cannot keep, so the rule keeping VMs apart
// names no more than there are nodes for: during a rolling update the newest stays out of it
// until an old one is gone.
func (r *HAReconciler) reconcileRules(ctx context.Context, pool string, s *haState, members, allowed []string) error {
	apartID, nodesID := haRuleID(pool, apartRule), haRuleID(pool, nodesRule)
	limit := len(s.nodes)
	var onNodes *haRuleConfig
	if allowed != nil && len(members) > 0 {
		onNodes = &haRuleConfig{Rule: nodesID, Type: "node-affinity", Resources: strings.Join(members, ","), Nodes: strings.Join(allowed, ",")}
		limit = min(limit, len(allowed))
	}
	current := s.rules[apartID]
	var apart *haRuleConfig
	if chosen := apartResources(members, splitList(current.Resources), limit); len(chosen) > 1 {
		apart = &haRuleConfig{Rule: apartID, Type: "resource-affinity", Resources: strings.Join(chosen, ",")}
	}
	// Proxmox VE checks the rules together: fewer VMs to keep apart go in before fewer nodes, more
	// nodes before more VMs.
	steps := []func() error{
		func() error { return r.applyRule(ctx, pool, s, nodesID, onNodes) },
		func() error { return r.applyRule(ctx, pool, s, apartID, apart) },
	}
	if apart == nil || len(splitList(apart.Resources)) < len(splitList(current.Resources)) {
		slices.Reverse(steps)
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// apartResources picks at most limit of the members, those the rule already names first.
func apartResources(members, current []string, limit int) []string {
	chosen := slices.DeleteFunc(slices.Clone(current), func(sid string) bool { return !slices.Contains(members, sid) })
	for _, sid := range members {
		if !slices.Contains(chosen, sid) {
			chosen = append(chosen, sid)
		}
	}
	chosen = chosen[:min(len(chosen), limit)]
	slices.Sort(chosen)
	return chosen
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	list := strings.Split(s, ",")
	slices.Sort(list)
	return list
}

// applyRule writes the rule as wanted, or removes it when want is nil.
func (r *HAReconciler) applyRule(ctx context.Context, pool string, s *haState, id string, want *haRuleConfig) error {
	have, exists := s.rules[id]
	path := "/cluster/ha/rules/" + id
	switch {
	case want == nil && !exists:
		return nil
	case want == nil:
		if err := r.HA.Delete(ctx, path, nil); err != nil && !isMissing(err) {
			return fmt.Errorf("removing HA rule %s: %w", id, err)
		}
		delete(s.rules, id)
		return nil
	case exists && slices.Equal(splitList(have.Resources), splitList(want.Resources)) && slices.Equal(splitList(have.Nodes), splitList(want.Nodes)):
		return nil
	}
	params := map[string]any{"type": want.Type, "resources": want.Resources}
	if want.Nodes != "" {
		params["nodes"] = want.Nodes
	}
	var err error
	if exists {
		err = r.HA.Put(ctx, path, params, nil)
	} else {
		params["rule"], params["comment"] = id, HAComment(pool)
		if want.Type == "node-affinity" {
			params["strict"] = 1
		} else {
			params["affinity"] = "negative"
		}
		err = r.HA.Post(ctx, "/cluster/ha/rules", params, nil)
	}
	if err != nil {
		return fmt.Errorf("writing HA rule %s: %w", id, err)
	}
	s.rules[id] = *want
	return nil
}

// forget drops a removed HA resource from the rules, and a rule left without resources, as Proxmox
// VE does.
func (s *haState) forget(sid string) {
	for id, rule := range s.rules {
		resources := slices.DeleteFunc(splitList(rule.Resources), func(r string) bool { return r == sid })
		if len(resources) == 0 {
			delete(s.rules, id)
			continue
		}
		rule.Resources = strings.Join(resources, ",")
		s.rules[id] = rule
	}
}
