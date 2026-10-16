package provisioner

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/go-proxmox"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeRule struct {
	Type      string
	Affinity  string
	Resources []string
	Nodes     []string
}

// fakeHA answers Proxmox VE's HA API, refusing a rule set Proxmox VE would refuse.
type fakeHA struct {
	mu        sync.Mutex
	nodes     []string
	vms       map[int64]string
	storages  []storageConfig
	resources map[string]string
	rules     map[string]*fakeRule
	haCalls   int
	// unlisted resources were added and ghosts removed by someone else after the controller read the
	// list.
	unlisted map[string]bool
	ghosts   map[string]string
}

func newFakeHA(nodes ...string) *fakeHA {
	return &fakeHA{
		nodes:     nodes,
		vms:       map[int64]string{},
		storages:  []storageConfig{{Storage: "ceph", Shared: 1}, {Storage: "local-lvm"}},
		resources: map[string]string{},
		rules:     map[string]*fakeRule{},
	}
}

func (f *fakeHA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	if strings.HasPrefix(path, "/cluster/ha/") {
		f.haCalls++
	}
	body := map[string]any{}
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	var data any
	var err error
	switch {
	case strings.HasPrefix(path, "/cluster/ha/rules"):
		data, err = f.answerRules(r.Method, path, body)
	case strings.HasPrefix(path, "/cluster/ha/"):
		data, err = f.answerHA(r.Method, path, body)
	default:
		data, err = f.answer(r.Method, path)
	}
	reply := map[string]any{"data": data}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		reply = map[string]any{"message": err.Error()}
	}
	if err := json.NewEncoder(w).Encode(reply); err != nil {
		panic(err)
	}
}

// none is the data of an answer that has none.
var none = json.RawMessage("null")

func text(body map[string]any, key string) string {
	s, ok := body[key].(string)
	if !ok {
		return ""
	}
	return s
}

func (f *fakeHA) answer(method, path string) (any, error) {
	switch {
	case method == http.MethodGet && path == "/cluster/resources":
		vms := make([]map[string]any, 0, len(f.vms))
		for id, node := range f.vms {
			vms = append(vms, map[string]any{"vmid": id, "node": node})
		}
		return vms, nil
	case method == http.MethodGet && path == "/storage":
		return f.storages, nil
	}
	return nil, fmt.Errorf("unexpected %s %s", method, path)
}

func (f *fakeHA) answerHA(method, path string, body map[string]any) (any, error) {
	switch {
	case method == http.MethodGet && path == "/cluster/ha/status/manager_status":
		status := map[string]string{}
		for _, n := range f.nodes {
			status[n] = "online"
		}
		return map[string]any{"manager_status": map[string]any{"node_status": status}}, nil
	case method == http.MethodGet && path == "/cluster/ha/resources":
		var list []map[string]string
		for sid, comment := range f.resources {
			if !f.unlisted[sid] {
				list = append(list, map[string]string{"sid": sid, "comment": comment})
			}
		}
		for sid, comment := range f.ghosts {
			list = append(list, map[string]string{"sid": sid, "comment": comment})
		}
		return list, nil
	case method == http.MethodPost && path == "/cluster/ha/resources":
		sid := text(body, "sid")
		if _, ok := f.resources[sid]; ok {
			return nil, fmt.Errorf("resource ID '%s' already defined", sid)
		}
		if state := text(body, "state"); state != "started" {
			return nil, fmt.Errorf("state %q", state)
		}
		f.resources[sid] = text(body, "comment")
		return none, nil
	case method == http.MethodDelete && strings.HasPrefix(path, "/cluster/ha/resources/"):
		return none, f.removeResource(strings.TrimPrefix(path, "/cluster/ha/resources/"))
	}
	return nil, fmt.Errorf("unexpected %s %s", method, path)
}

func (f *fakeHA) answerRules(method, path string, body map[string]any) (any, error) {
	switch {
	case method == http.MethodGet && path == "/cluster/ha/rules":
		list := make([]map[string]string, 0, len(f.rules))
		for id, rule := range f.rules {
			list = append(list, map[string]string{"rule": id, "type": rule.Type, "resources": strings.Join(rule.Resources, ","), "nodes": strings.Join(rule.Nodes, ",")})
		}
		return list, nil
	case method == http.MethodPost && path == "/cluster/ha/rules":
		id := text(body, "rule")
		if _, ok := f.rules[id]; ok {
			return nil, fmt.Errorf("rule '%s' already defined", id)
		}
		if strict, ok := body["strict"].(float64); text(body, "type") == "node-affinity" && (!ok || strict != 1) {
			return nil, fmt.Errorf("node rule %s is not strict", id)
		}
		rule := &fakeRule{Type: text(body, "type"), Affinity: text(body, "affinity"), Resources: splitList(text(body, "resources")), Nodes: splitList(text(body, "nodes"))}
		return none, f.write(id, rule)
	case method == http.MethodPut && strings.HasPrefix(path, "/cluster/ha/rules/"):
		id := strings.TrimPrefix(path, "/cluster/ha/rules/")
		old, ok := f.rules[id]
		if !ok || text(body, "type") != old.Type {
			return nil, fmt.Errorf("rule %s: type %q", id, text(body, "type"))
		}
		rule := *old
		rule.Resources = splitList(text(body, "resources"))
		if nodes := text(body, "nodes"); nodes != "" {
			rule.Nodes = splitList(nodes)
		}
		return none, f.write(id, &rule)
	case method == http.MethodDelete && strings.HasPrefix(path, "/cluster/ha/rules/"):
		delete(f.rules, strings.TrimPrefix(path, "/cluster/ha/rules/"))
		return none, nil
	}
	return nil, fmt.Errorf("unexpected %s %s", method, path)
}

// removeResource purges the resource from the rules, as Proxmox VE does by default.
func (f *fakeHA) removeResource(sid string) error {
	delete(f.ghosts, sid)
	if _, ok := f.resources[sid]; !ok {
		return fmt.Errorf("service '%s' does not exist", sid)
	}
	delete(f.resources, sid)
	for id, rule := range f.rules {
		rule.Resources = slices.DeleteFunc(rule.Resources, func(r string) bool { return r == sid })
		if len(rule.Resources) == 0 {
			delete(f.rules, id)
		}
	}
	return nil
}

// write keeps the rule if Proxmox VE's checks pass with it.
func (f *fakeHA) write(id string, rule *fakeRule) error {
	old, had := f.rules[id]
	f.rules[id] = rule
	if err := f.feasible(); err != nil {
		if had {
			f.rules[id] = old
		} else {
			delete(f.rules, id)
		}
		return fmt.Errorf("rule '%s' is invalid: %w", id, err)
	}
	return nil
}

func (f *fakeHA) feasible() error {
	onNodes := map[string][]string{}
	for _, rule := range f.rules {
		for _, sid := range rule.Resources {
			if _, ok := f.resources[sid]; !ok {
				return fmt.Errorf("cannot use unmanaged resource(s) %s", sid)
			}
			if rule.Type == "node-affinity" {
				if _, twice := onNodes[sid]; twice {
					return fmt.Errorf("%s is in two node affinity rules", sid)
				}
				onNodes[sid] = rule.Nodes
			}
		}
	}
	for _, rule := range f.rules {
		if rule.Type != "resource-affinity" {
			continue
		}
		if len(rule.Resources) < 2 {
			return errors.New("rule is ineffective as there are less than two resources")
		}
		if rule.Affinity != "negative" {
			return fmt.Errorf("affinity %q", rule.Affinity)
		}
		if len(rule.Resources) > len(f.nodes) {
			return errors.New("rule defines more resources than available nodes")
		}
		located, allowed := 0, map[string]bool{}
		for _, sid := range rule.Resources {
			if nodes, ok := onNodes[sid]; ok {
				located++
				for _, n := range nodes {
					allowed[n] = true
				}
			}
		}
		if len(allowed) < located {
			return errors.New("resources are restricted to less nodes than needed to keep them separate")
		}
	}
	return nil
}

func (f *fakeHA) rule(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rules[id]; ok {
		return slices.Sorted(slices.Values(r.Resources))
	}
	return nil
}

func (f *fakeHA) haResources() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.resources)
}

// webPool is the Proxmox VE pool of the cluster team-a/web.
const webPool = "kubehenge/team-a.web"

var proxmoxMachineGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: proxmoxMachineVersion, Kind: "ProxmoxMachine"}

// controlPlane is a control-plane Machine of web and its ProxmoxMachine, whose VM capmox recorded
// on node.
func controlPlane(name string, vmid int64, node string) (*clusterv1.Machine, *unstructured.Unstructured) {
	m := &clusterv1.Machine{
		Name: name, Namespace: "team-a",
		Labels:     map[string]string{clusterv1.ClusterNameLabel: "web", clusterv1.MachineControlPlaneLabel: ""},
		Finalizers: []string{clusterv1.MachineFinalizer},
		Spec: clusterv1.MachineSpec{
			ClusterName:       "web",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: proxmoxMachineGVK.Group, Kind: proxmoxMachineGVK.Kind, Name: name},
		},
		Status: clusterv1.MachineStatus{NodeRef: clusterv1.MachineNodeReference{Name: name}},
	}
	pm := &unstructured.Unstructured{Object: map[string]any{
		"spec":   map[string]any{"virtualMachineID": vmid},
		"status": map[string]any{"proxmoxNode": node},
	}}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	pm.SetNamespace("team-a")
	pm.SetName(name)
	return m, pm
}

func variableOf(name, raw string) clusterv1.ClusterVariable {
	return clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(raw)}}
}

// haFixture is web on shared storage with three control planes joined, on pve1 to pve3.
type haFixture struct {
	t    *testing.T
	pve  *fakeHA
	r    *HAReconciler
	kube client.Client
}

// newHAFixture has HA know the nodes given; the reconciler holds the HA token.
func newHAFixture(t *testing.T, c *clusterv1.Cluster, nodes ...string) *haFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(proxmoxMachineGVK, &unstructured.Unstructured{})
	pve := newFakeHA(nodes...)
	const controlPlanes = 3
	objs := make([]client.Object, 0, 2+2*controlPlanes)
	objs = append(objs, c)
	for i, n := range []string{"pve1", "pve2", "pve3"} {
		m, pm := controlPlane(fmt.Sprintf("web-cp-%d", i), int64(101+i), n)
		pve.vms[int64(101+i)] = n
		objs = append(objs, m, pm)
	}
	worker := &clusterv1.Machine{Name: "web-md-0", Namespace: "team-a", Labels: map[string]string{clusterv1.ClusterNameLabel: "web"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, worker)...).Build()
	srv := httptest.NewServer(pve)
	t.Cleanup(srv.Close)
	r := &HAReconciler{
		Client: kube, Machines: kube, ClassNamespace: "kubehenge-system", WorkloadPool: "kubehenge",
		Proxmox: proxmox.NewClient(srv.URL + "/api2/json"),
		HA:      proxmox.NewClient(srv.URL + "/api2/json"),
	}
	return &haFixture{t: t, pve: pve, r: r, kube: kube}
}

func (f *haFixture) reconcile() ctrl.Result {
	f.t.Helper()
	res, err := f.r.Reconcile(f.t.Context(), ctrl.Request{Namespace: "team-a", Name: "web"})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *haFixture) machine(name string) *clusterv1.Machine {
	f.t.Helper()
	m := &clusterv1.Machine{}
	if err := f.kube.Get(f.t.Context(), client.ObjectKey{Namespace: "team-a", Name: name}, m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

// hooked lists the Machines carrying the HA hook.
func (f *haFixture) hooked() []string {
	f.t.Helper()
	list := &clusterv1.MachineList{}
	if err := f.kube.List(f.t.Context(), list); err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for i := range list.Items {
		if list.Items[i].Annotations[HAHook] != "" {
			names = append(names, list.Items[i].Name)
		}
	}
	return names
}

// add adds a control plane that has joined the cluster.
func (f *haFixture) add(name string, vmid int64, node string) {
	f.t.Helper()
	m, pm := controlPlane(name, vmid, node)
	f.create(m, pm, vmid, node)
}

// addJoining adds a control plane whose node has not joined the cluster yet.
func (f *haFixture) addJoining(name string, vmid int64, node string) {
	f.t.Helper()
	m, pm := controlPlane(name, vmid, node)
	m.Status.NodeRef = clusterv1.MachineNodeReference{}
	f.create(m, pm, vmid, node)
}

func (f *haFixture) create(m *clusterv1.Machine, pm *unstructured.Unstructured, vmid int64, node string) {
	f.t.Helper()
	for _, o := range []client.Object{m, pm} {
		if err := f.kube.Create(f.t.Context(), o); err != nil {
			f.t.Fatal(err)
		}
	}
	f.pve.mu.Lock()
	f.pve.vms[vmid] = node
	f.pve.mu.Unlock()
}

func (f *haFixture) remove(name string) {
	f.t.Helper()
	if err := f.kube.Delete(f.t.Context(), f.machine(name)); err != nil {
		f.t.Fatal(err)
	}
}

func shared() *clusterv1.Cluster { return cluster(variableOf("storage", `"ceph"`)) }

func TestHAKeepsTheControlPlanesApart(t *testing.T) {
	t.Parallel()
	f := newHAFixture(t, shared(), "pve1", "pve2", "pve3")
	apart, nodes := haRuleID(webPool, apartRule), haRuleID(webPool, nodesRule)
	f.reconcile()
	if got := f.hooked(); !slices.Equal(got, []string{"web-cp-0", "web-cp-1", "web-cp-2"}) {
		t.Errorf("hooked %v", got)
	}
	if got := f.machine("web-cp-0").Annotations[HAHook]; got != "kubehenge" {
		t.Errorf("hook %q", got)
	}
	resources := f.pve.haResources()
	if len(resources) != 3 || resources["vm:101"] != "Kubehenge cluster kubehenge/team-a.web" {
		t.Errorf("HA resources %v", resources)
	}
	if got := f.pve.rule(apart); !slices.Equal(got, []string{"vm:101", "vm:102", "vm:103"}) {
		t.Errorf("apart %v", got)
	}
	if f.pve.rule(nodes) != nil {
		t.Error("kept to some nodes although the cluster may use every one")
	}

	// A rolling update: the new machine is an HA resource, but the rule keeps the three it names
	// until an old one is gone, as three nodes cannot keep four apart, even when the new VMID is
	// lower than theirs.
	f.add("web-cp-3", 100, "pve1")
	f.addJoining("web-cp-4", 105, "pve2")
	f.reconcile()
	if _, ok := f.pve.haResources()["vm:100"]; !ok {
		t.Error("the new machine is no HA resource")
	}
	if _, ok := f.pve.haResources()["vm:105"]; ok {
		t.Error("a machine that has not joined is an HA resource")
	}
	if got := f.pve.rule(apart); !slices.Equal(got, []string{"vm:101", "vm:102", "vm:103"}) {
		t.Errorf("during the update, apart %v", got)
	}
	f.remove("web-cp-0")
	f.reconcile()
	if _, ok := f.pve.haResources()["vm:101"]; ok {
		t.Error("the deleted machine's VM is still an HA resource")
	}
	if slices.Contains(f.hooked(), "web-cp-0") {
		t.Error("the deleted machine is still held")
	}
	if got := f.pve.rule(apart); !slices.Equal(got, []string{"vm:100", "vm:102", "vm:103"}) {
		t.Errorf("after the update, apart %v", got)
	}
}

func TestHALetsGoOfADeletedCluster(t *testing.T) {
	t.Parallel()
	f := newHAFixture(t, shared(), "pve1", "pve2", "pve3")
	f.reconcile()
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	c.Finalizers = []string{clusterv1.ClusterFinalizer}
	if err := f.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	// Its machines are not deleted yet, but they leave HA at once.
	f.reconcile()
	if got := f.pve.haResources(); len(got) != 0 {
		t.Errorf("HA resources %v", got)
	}
	if got := f.pve.rule(haRuleID(webPool, apartRule)); got != nil {
		t.Errorf("apart %v", got)
	}
	if got := f.hooked(); len(got) != 0 {
		t.Errorf("hooked %v", got)
	}
}

// Someone else added an HA resource, or removed one, after the controller read the list.
func TestHAToleratesChangesMadeMeanwhile(t *testing.T) {
	t.Parallel()
	f := newHAFixture(t, shared(), "pve1", "pve2", "pve3")
	ours := HAComment(webPool)
	f.pve.resources["vm:101"] = ours
	f.pve.unlisted = map[string]bool{"vm:101": true}
	f.pve.ghosts = map[string]string{"vm:900": ours}
	f.reconcile()
	if got := f.pve.haResources(); len(got) != 3 {
		t.Errorf("HA resources %v", got)
	}
	if got := f.pve.rule(haRuleID(webPool, apartRule)); len(got) != 3 {
		t.Errorf("apart %v", got)
	}
}

func TestHAKeepsToTheClustersNodes(t *testing.T) {
	t.Parallel()
	c := cluster(variableOf("storage", `"ceph"`), variableOf("allowedNodes", `["pve1","pve2","pve9"]`))
	f := newHAFixture(t, c, "pve1", "pve2", "pve3")
	apart, nodes := haRuleID(webPool, apartRule), haRuleID(webPool, nodesRule)
	f.reconcile()
	if got := f.pve.rule(nodes); !slices.Equal(got, []string{"vm:101", "vm:102", "vm:103"}) {
		t.Errorf("on nodes %v", got)
	}
	f.pve.mu.Lock()
	kept := slices.Clone(f.pve.rules[nodes].Nodes)
	f.pve.mu.Unlock()
	if !slices.Equal(kept, []string{"pve1", "pve2"}) {
		t.Errorf("kept to %v", kept)
	}
	if got := f.pve.rule(apart); len(got) != 2 {
		t.Errorf("two nodes keep two apart, not %v", got)
	}

	keepTo := func(nodes string) {
		t.Helper()
		c := &clusterv1.Cluster{}
		if err := f.kube.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil {
			t.Fatal(err)
		}
		c.Spec.Topology.Variables = []clusterv1.ClusterVariable{variableOf("storage", `"ceph"`)}
		if nodes != "" {
			c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, variableOf("allowedNodes", nodes))
		}
		if err := f.kube.Update(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		f.reconcile()
	}

	// Every node allowed again: the node rule goes, and the rule keeping them apart takes all three.
	keepTo("")
	if f.pve.rule(nodes) != nil {
		t.Error("still kept to some nodes")
	}
	if got := f.pve.rule(apart); len(got) != 3 {
		t.Errorf("apart %v", got)
	}

	// Fewer nodes: Proxmox VE takes the node rule only once the apart rule names no more VMs than it
	// leaves nodes, and on one node there is nothing to keep apart.
	keepTo(`["pve1","pve2"]`)
	if got := f.pve.rule(apart); len(got) != 2 {
		t.Errorf("on two nodes, apart %v", got)
	}
	if got := f.pve.rule(nodes); len(got) != 3 {
		t.Errorf("on two nodes, on nodes %v", got)
	}
	keepTo(`["pve1"]`)
	if got := f.pve.rule(apart); got != nil {
		t.Errorf("on one node, apart %v", got)
	}
	if got := f.pve.rule(nodes); len(got) != 3 {
		t.Errorf("on one node, on nodes %v", got)
	}

	// All three replaced at once on that one node: Proxmox VE drops the node rule with the last of
	// its VMs, and the new one gets a rule of its own.
	for _, name := range []string{"web-cp-0", "web-cp-1", "web-cp-2"} {
		f.remove(name)
	}
	f.add("web-cp-3", 104, "pve1")
	f.reconcile()
	if got := f.pve.rule(nodes); !slices.Equal(got, []string{"vm:104"}) {
		t.Errorf("after the replacement, on nodes %v", got)
	}
}

func TestHALeavesOutClustersThatDoNotTakePart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cluster *clusterv1.Cluster
	}{
		{"opted out", cluster(variableOf("storage", `"ceph"`), variableOf("proxmoxHA", "false"))},
		{"on local storage", cluster(variableOf("storage", `"local-lvm"`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newHAFixture(t, tc.cluster, "pve1", "pve2", "pve3")
			apart := haRuleID(webPool, apartRule)
			// What an earlier reconcile left while the cluster took part.
			f.pve.resources["vm:101"] = HAComment("kubehenge/team-a.web")
			f.pve.resources["vm:102"] = HAComment("kubehenge/team-a.web")
			f.pve.resources["vm:900"] = "someone else's"
			f.pve.rules[apart] = &fakeRule{Type: "resource-affinity", Affinity: "negative", Resources: []string{"vm:101", "vm:102"}}
			m := f.machine("web-cp-0")
			m.Annotations = map[string]string{HAHook: "kubehenge"}
			if err := f.kube.Update(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			f.reconcile()
			if got := f.pve.haResources(); len(got) != 1 || got["vm:900"] == "" {
				t.Errorf("HA resources %v", got)
			}
			if f.pve.rule(apart) != nil {
				t.Error("the rule stayed")
			}
			if got := f.hooked(); len(got) != 0 {
				t.Errorf("hooked %v", got)
			}
		})
	}
}

func TestHAWaitsForCapmoxToFindAMovedVM(t *testing.T) {
	t.Parallel()
	f := newHAFixture(t, shared(), "pve1", "pve2", "pve3")
	f.reconcile()
	// HA moved web-cp-0's VM; capmox still records pve1 and would look for it there.
	f.pve.mu.Lock()
	f.pve.vms[101] = "pve3"
	f.pve.mu.Unlock()
	f.remove("web-cp-0")
	if res := f.reconcile(); res.RequeueAfter != waitInterval {
		t.Errorf("requeued after %v", res.RequeueAfter)
	}
	if _, ok := f.pve.haResources()["vm:101"]; ok {
		t.Error("still an HA resource")
	}
	if !slices.Contains(f.hooked(), "web-cp-0") {
		t.Fatal("let go before capmox found the VM")
	}
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	if err := f.kube.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web-cp-0"}, pm); err != nil {
		t.Fatal(err)
	}
	if got := pm.GetAnnotations()[LocateAnnotation]; got != "pve3" {
		t.Errorf("marked with %q", got)
	}
	// capmox found it.
	if err := unstructured.SetNestedField(pm.Object, "pve3", "status", "proxmoxNode"); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Update(t.Context(), pm); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if slices.Contains(f.hooked(), "web-cp-0") {
		t.Error("still held once capmox knows where the VM is")
	}
}

func TestHAWaitsForTheHAManagerToSizeTheRule(t *testing.T) {
	t.Parallel()
	f := newHAFixture(t, shared())
	if res := f.reconcile(); res.RequeueAfter != waitInterval {
		t.Errorf("requeued after %v", res.RequeueAfter)
	}
	if len(f.pve.haResources()) != 3 {
		t.Errorf("HA resources %v", f.pve.haResources())
	}
	f.pve.mu.Lock()
	f.pve.nodes = []string{"pve1", "pve2"}
	f.pve.mu.Unlock()
	f.reconcile()
	apart := haRuleID(webPool, apartRule)
	if got := f.pve.rule(apart); len(got) != 2 {
		t.Errorf("apart %v", got)
	}
}

func TestHAWithoutItsTokenTakesBackTheHooks(t *testing.T) {
	t.Parallel()
	f := newHAFixture(t, shared(), "pve1", "pve2", "pve3")
	f.r.HA = nil
	for _, name := range []string{"web-cp-0", "web-cp-1"} {
		m := f.machine(name)
		m.Annotations = map[string]string{HAHook: "kubehenge"}
		if err := f.kube.Update(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	f.pve.vms[102] = "pve3"
	f.reconcile()
	if got := f.hooked(); !slices.Equal(got, []string{"web-cp-1"}) {
		t.Errorf("hooked %v: web-cp-1 waits for capmox to find its VM, web-cp-0 for nothing", got)
	}
	if f.pve.haCalls != 0 {
		t.Errorf("%d HA calls without the HA token", f.pve.haCalls)
	}
}

func TestHANodes(t *testing.T) {
	t.Parallel()
	known := []string{"pve1", "pve2", "pve3"}
	for _, tc := range []struct {
		name    string
		chosen  []string
		storage string
		want    []string
	}{
		{"every node", nil, "", nil},
		{"the chosen", []string{"pve3", "pve1"}, "", []string{"pve1", "pve3"}},
		{"those with the storage", nil, "pve2,pve3,pve7", []string{"pve2", "pve3"}},
		{"both", []string{"pve1", "pve2"}, "pve2,pve3", []string{"pve2"}},
		{"none known", []string{"pve8"}, "", nil},
	} {
		if got := haNodes(known, tc.chosen, tc.storage); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
