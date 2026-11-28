package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeFWVM is one VM's firewall and configuration as Proxmox VE holds them.
type fakeFWVM struct {
	guest  pveGuest
	enable bool
	policy string
	rules  []pveRule
	ipsets map[string][]string
	config map[string]any
	// Each digest changes with every write to what it covers, as Proxmox VE's do.
	optionsDigest, rulesDigest int
}

// fakeVMFirewall answers the routes the VM firewall reconciler uses, in memory.
type fakeVMFirewall struct {
	mu  sync.Mutex
	vms map[int64]*fakeFWVM
	// reads counts the reads of each VM's own routes, and writes lists every write as "<method> <path>".
	reads  int
	writes []string
	// failWrite, when set, fails a write with what it returns. beforeWrite runs before the next
	// write, once.
	failWrite   func(method, path string) error
	beforeWrite func()
}

var errStaleDigest = errors.New("500 detected modified configuration - file changed by other user, try again")

var vmRoute = regexp.MustCompile(`^/nodes/([^/]+)/qemu/(\d+)(/.*)?$`)

func copyJSON(from, to any) error {
	raw, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, to)
}

func (v *fakeFWVM) renumber() {
	for i := range v.rules {
		v.rules[i].Pos = i
	}
}

func (f *fakeVMFirewall) Get(_ context.Context, path string, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path == "/cluster/resources?type=vm" {
		guests := make([]pveGuest, 0, len(f.vms))
		for _, v := range f.vms {
			guests = append(guests, v.guest)
		}
		return copyJSON(guests, out)
	}
	f.reads++
	v, rest, err := f.vm(path)
	if err != nil {
		return err
	}
	switch {
	case rest == "/firewall/options":
		enable := 0
		if v.enable {
			enable = 1
		}
		return copyJSON(map[string]any{"enable": enable, "policy_in": v.policy, "digest": "o" + strconv.Itoa(v.optionsDigest)}, out)
	case rest == "/firewall/rules":
		rules := slices.Clone(v.rules)
		for i := range rules {
			rules[i].Digest = "r" + strconv.Itoa(v.rulesDigest)
		}
		return copyJSON(rules, out)
	case strings.HasPrefix(rest, "/firewall/ipset/"):
		entries, ok := v.ipsets[strings.TrimPrefix(rest, "/firewall/ipset/")]
		if !ok {
			return errors.New("500 no such IPSet")
		}
		var list []map[string]string
		for _, e := range entries {
			list = append(list, map[string]string{"cidr": e})
		}
		return copyJSON(list, out)
	case rest == "/config":
		return copyJSON(v.config, out)
	}
	return fmt.Errorf("unexpected read of %s", path)
}

func paramOf(params any, key string) string {
	if m, ok := params.(map[string]any); ok {
		if v, found := m[key]; found {
			return fmt.Sprint(v)
		}
	}
	return ""
}

func (f *fakeVMFirewall) Put(_ context.Context, path string, params, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.write("PUT", path); err != nil {
		return err
	}
	v, rest, err := f.vm(path)
	if err != nil {
		return err
	}
	switch rest {
	case "/firewall/options":
		if d := paramOf(params, "digest"); d != "" && d != "o"+strconv.Itoa(v.optionsDigest) {
			return errStaleDigest
		}
		if e := paramOf(params, "enable"); e != "" {
			v.enable = e == "1"
		}
		if p := paramOf(params, "policy_in"); p != "" {
			v.policy = p
		}
		v.optionsDigest++
	case "/config":
		if d := paramOf(params, "digest"); d != "" && d != v.config["digest"] {
			return errStaleDigest
		}
		if pm, ok := params.(map[string]any); ok {
			for k, val := range pm {
				if k != "digest" {
					v.config[k] = val
				}
			}
		}
		v.config["digest"] = fmt.Sprint(v.config["digest"], "+")
	default:
		return fmt.Errorf("unexpected write to %s", path)
	}
	return nil
}

func (f *fakeVMFirewall) Post(_ context.Context, path string, params, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.write("POST", path); err != nil {
		return err
	}
	v, rest, err := f.vm(path)
	if err != nil {
		return err
	}
	switch {
	case rest == "/firewall/ipset":
		v.ipsets[paramOf(params, "name")] = []string{}
	case strings.HasPrefix(rest, "/firewall/ipset/"):
		name := strings.TrimPrefix(rest, "/firewall/ipset/")
		v.ipsets[name] = append(v.ipsets[name], paramOf(params, "cidr"))
	case rest == "/firewall/rules":
		if d := paramOf(params, "digest"); d != "" && d != "r"+strconv.Itoa(v.rulesDigest) {
			return errStaleDigest
		}
		if paramOf(params, "pos") != "0" || paramOf(params, "enable") != "1" {
			return fmt.Errorf("unexpected rule %v", params)
		}
		rule := pveRule{
			Type: paramOf(params, "type"), Action: paramOf(params, "action"), Source: paramOf(params, "source"), Dest: paramOf(params, "dest"),
			Proto: paramOf(params, "proto"), Dport: paramOf(params, "dport"), ICMPType: paramOf(params, "icmp-type"), Comment: paramOf(params, "comment"), Enable: true,
		}
		v.rules = slices.Insert(v.rules, 0, rule)
		v.renumber()
		v.rulesDigest++
	default:
		return fmt.Errorf("unexpected write to %s", path)
	}
	return nil
}

func (f *fakeVMFirewall) Delete(_ context.Context, path string, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.write("DELETE", path); err != nil {
		return err
	}
	u, err := url.Parse(path)
	if err != nil {
		return err
	}
	v, rest, err := f.vm(u.EscapedPath())
	if err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(rest, "/firewall/rules/"):
		pos, _ := strconv.Atoi(strings.TrimPrefix(rest, "/firewall/rules/"))
		if d := u.Query().Get("digest"); d != "" && d != "r"+strconv.Itoa(v.rulesDigest) {
			return errStaleDigest
		}
		if pos >= len(v.rules) {
			return errors.New("500 no rule there")
		}
		v.rules = slices.Delete(v.rules, pos, pos+1)
		v.renumber()
		v.rulesDigest++
	case strings.HasPrefix(rest, "/firewall/ipset/"):
		name, entry, nested := strings.Cut(strings.TrimPrefix(rest, "/firewall/ipset/"), "/")
		if !nested {
			delete(v.ipsets, name)
			break
		}
		entry, _ = url.PathUnescape(entry)
		v.ipsets[name] = slices.DeleteFunc(v.ipsets[name], func(e string) bool { return e == entry })
	default:
		return fmt.Errorf("unexpected write to %s", path)
	}
	return nil
}

func (f *fakeVMFirewall) vm(path string) (*fakeFWVM, string, error) {
	m := vmRoute.FindStringSubmatch(path)
	if m == nil {
		return nil, "", fmt.Errorf("unexpected route %s", path)
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	v := f.vms[id]
	if v == nil {
		return nil, "", fmt.Errorf("500 Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", m[1], id)
	}
	return v, m[3], nil
}

// write checks a write, and counts it.
func (f *fakeVMFirewall) write(method, path string) error {
	if f.beforeWrite != nil {
		hook := f.beforeWrite
		f.beforeWrite = nil
		hook()
	}
	if f.failWrite != nil {
		if err := f.failWrite(method, path); err != nil {
			return err
		}
	}
	f.writes = append(f.writes, method+" "+path)
	return nil
}

func (f *fakeVMFirewall) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// sum is a rule in a few words, in the order Proxmox VE lists them.
func sum(p *pveRule) string {
	parts := []string{p.Type, p.Action}
	for _, kv := range [][2]string{{"from:", p.Source}, {"to:", p.Dest}, {"", p.Proto}, {"", p.Dport}, {"", p.ICMPType}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+kv[1])
		}
	}
	return strings.Join(parts, " ")
}

func sums(rules []pveRule) []string {
	out := make([]string, 0, len(rules))
	for i := range rules {
		out = append(out, sum(&rules[i]))
	}
	return out
}

// servingSums are the rules a firewalled kubeadm VM holds with the serving ports open and no group.
var servingSums = []string{
	"in ACCEPT from:+guest/kubehenge",
	"in ACCEPT to:+guest/kubehenge-lb",
	"in ACCEPT tcp 6443", "in ACCEPT tcp 80", "in ACCEPT tcp 443",
	"in ACCEPT tcp 30000:32767", "in ACCEPT udp 30000:32767",
	"in ACCEPT icmp echo-request", "in ACCEPT ipv6-icmp echo-request",
}

type fixedManagement []string

func (m fixedManagement) Addresses(context.Context) ([]string, error) { return m, nil }

// firewallFixture is one Kubehenge cluster, team-a/web, with a control-plane machine whose VM is VM 100.
type firewallFixture struct {
	t      *testing.T
	kube   client.Client
	pve    *fakeVMFirewall
	r      *VMFirewall
	events *events.FakeRecorder
	now    time.Time
	pool   string
}

func newFWFixture(t *testing.T, mods ...func(*clusterv1.Cluster)) *firewallFixture {
	t.Helper()
	c := cluster()
	c.Spec.Topology.ClassRef.Name = "kubehenge-kubeadm"
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.244.0.0/16"}
	addVariable(c, FirewallVariable, "{\"enabled\": true}")
	for _, mod := range mods {
		mod(c)
	}
	names, err := NamesFor(testPool, c.Namespace, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	machine := &clusterv1.Machine{Name: "web-cp-x7k2p", Namespace: c.Namespace}
	pm := proxmoxMachineObject()
	pm.SetName(machine.Name)
	pm.SetNamespace(c.Namespace)
	pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: c.Name})
	pm.SetAnnotations(map[string]string{vmNamePrefixAnnotation: c.Name})
	pm.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: machine.Name, UID: "m1"}})
	if err := unstructured.SetNestedField(pm.Object, int64(100), "spec", "virtualMachineID"); err != nil {
		t.Fatal(err)
	}
	pc := proxmoxClusterObject()
	pc.SetName("web-x1")
	pc.SetNamespace(c.Namespace)
	pc.SetLabels(map[string]string{clusterv1.ClusterNameLabel: c.Name})
	if err := unstructured.SetNestedField(pc.Object, "10.0.0.5", "spec", "controlPlaneEndpoint", "host"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedStringSlice(pc.Object, []string{"10.0.0.10-10.0.0.13"}, "spec", "ipv4Config", "addresses"); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(c, machine, pm, pc).WithStatusSubresource(&clusterv1.Machine{}).Build()
	fx := &firewallFixture{t: t, kube: kube, events: events.NewFakeRecorder(50), now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), pool: names.Pool}
	fx.pve = &fakeVMFirewall{vms: map[int64]*fakeFWVM{100: {
		guest:  pveGuest{VMID: 100, Name: machine.Name, Pool: names.Pool, Node: "pve1", Type: qemuType, Status: "stopped"},
		ipsets: map[string][]string{},
		config: map[string]any{
			"name": machine.Name, "memory": 2048.0, "digest": "c0",
			"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=30", "net1": "virtio=BC:24:11:00:00:02,bridge=vmbr1,firewall=0",
		},
	}}}
	fx.r = fx.newReconciler()
	return fx
}

// newReconciler is a reconciler with no memory, as after a restart.
func (fx *firewallFixture) newReconciler() *VMFirewall {
	return &VMFirewall{
		Client: fx.kube, Reader: fx.kube, ClassNamespace: "kubehenge-system", WorkloadPool: testPool, Proxmox: fx.pve,
		Sources: fixedManagement{"192.0.2.11"}, Events: fx.events, Now: func() time.Time { return fx.now },
	}
}

func proxmoxMachineObject() *unstructured.Unstructured {
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(proxmoxMachineGVK)
	return pm
}

func (fx *firewallFixture) vm() *fakeFWVM { return fx.pve.vms[100] }

func (fx *firewallFixture) run() (ctrl.Result, error) {
	fx.t.Helper()
	var req ctrl.Request
	req.Namespace, req.Name = "team-a", "web-cp-x7k2p"
	return fx.r.Reconcile(fx.t.Context(), req)
}

func (fx *firewallFixture) mustRun() ctrl.Result {
	fx.t.Helper()
	res, err := fx.run()
	if err != nil {
		fx.t.Fatal(err)
	}
	return res
}

func (fx *firewallFixture) condition() *metav1.Condition {
	fx.t.Helper()
	m := &clusterv1.Machine{}
	if err := fx.kube.Get(fx.t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web-cp-x7k2p"}, m); err != nil {
		fx.t.Fatal(err)
	}
	return meta.FindStatusCondition(m.Status.Conditions, FirewallNotWrittenCondition)
}

func (fx *firewallFixture) recorded() []string {
	var out []string
	for len(fx.events.Events) > 0 {
		out = append(out, <-fx.events.Events)
	}
	return out
}

func (fx *firewallFixture) setCluster(mod func(*clusterv1.Cluster)) {
	fx.t.Helper()
	c := &clusterv1.Cluster{}
	if err := fx.kube.Get(fx.t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil {
		fx.t.Fatal(err)
	}
	mod(c)
	if err := fx.kube.Update(fx.t.Context(), c); err != nil {
		fx.t.Fatal(err)
	}
}

// A VM cloned and stopped, with the firewall flag on none of its interfaces, gets all of its
// firewall at its first reconcile, before it ever starts: both IPSets, the rules in order, the
// options and the flag of each interface with the rest of the interface kept. A second reconcile
// reads the five things and writes none.
func TestVMFirewallIsWrittenBeforeTheVMServes(t *testing.T) {
	t.Parallel()
	fx := newFWFixture(t)
	if res := fx.mustRun(); res.RequeueAfter != firewallUnsettled {
		t.Errorf("requeue after %v, want %v while the VM is not running", res.RequeueAfter, firewallUnsettled)
	}
	vm := fx.vm()
	if got := sums(vm.rules); !slices.Equal(got, servingSums) {
		t.Errorf("rules %q, want %q", got, servingSums)
	}
	for i := range vm.rules {
		if p := &vm.rules[i]; !bool(p.Enable) || !strings.HasPrefix(p.Comment, "kubehenge: ") {
			t.Errorf("rule %q is enabled %v with the comment %q", sum(p), p.Enable, p.Comment)
		}
	}
	if !vm.enable || vm.policy != "DROP" {
		t.Errorf("firewall enabled %v, inbound policy %q, want on and DROP", vm.enable, vm.policy)
	}
	sources := slices.Clone(vm.ipsets["kubehenge"])
	slices.Sort(sources)
	want, err := ipSetEntries([]string{"192.0.2.11", "10.0.0.5", "10.0.0.10-10.0.0.13", "10.244.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sources, want) {
		t.Errorf("IPSet kubehenge holds %v, want %v", sources, want)
	}
	if lb, ok := vm.ipsets["kubehenge-lb"]; !ok || len(lb) != 0 {
		t.Errorf("IPSet kubehenge-lb is %v (%v), want it made and empty without a load balancer", lb, ok)
	}
	if got := vm.config["net0"]; got != "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=30,firewall=1" {
		t.Errorf("net0 = %v", got)
	}
	if got := vm.config["net1"]; got != "virtio=BC:24:11:00:00:02,bridge=vmbr1,firewall=1" {
		t.Errorf("net1 = %v, want firewall=0 replaced", got)
	}
	if vm.config["memory"] != 2048.0 {
		t.Errorf("the rest of the configuration changed: %v", vm.config)
	}
	if got := fx.recorded(); len(got) != 0 {
		t.Errorf("events %q for a first write", got)
	}

	writes, reads := len(fx.pve.written()), fx.pve.reads
	fx.mustRun()
	if got := len(fx.pve.written()); got != writes {
		t.Errorf("a second reconcile wrote %v", fx.pve.written()[writes:])
	}
	if got := fx.pve.reads - reads; got != 5 {
		t.Errorf("a second reconcile made %d reads of the VM, want its options, rules, two IPSets and configuration", got)
	}
}

func TestVMFirewallRulesFollowTheClusterSettings(t *testing.T) {
	t.Parallel()
	talos := func(c *clusterv1.Cluster) { c.Spec.Topology.ClassRef.Name = "kubehenge-talos" }
	variable := func(name, raw string) func(*clusterv1.Cluster) {
		return func(c *clusterv1.Cluster) {
			c.Spec.Topology.Variables = slices.DeleteFunc(c.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool { return v.Name == name })
			c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(raw)}})
		}
	}
	for name, tc := range map[string]struct {
		mods []func(*clusterv1.Cluster)
		want []string
	}{
		"default":           {want: servingSums},
		"Talos":             {mods: []func(*clusterv1.Cluster){talos}, want: slices.Concat(servingSums, []string{"in ACCEPT tcp 50000"})},
		"groups in order":   {mods: []func(*clusterv1.Cluster){variable("securityGroups", "[\"k8s\",\"corp\"]")}, want: slices.Concat(servingSums[:1], []string{"group k8s", "group corp"}, servingSums[1:])},
		"serving ports off": {mods: []func(*clusterv1.Cluster){talos, variable("firewall", "{\"enabled\": true, \"servingPorts\": false}")}, want: servingSums[:1]},
		"more ports": {
			mods: []func(*clusterv1.Cluster){variable("firewall", "{\"enabled\": true, \"servingPorts\": false, \"ports\": [{\"protocol\": \"udp\", \"port\": 51820}, {\"protocol\": \"tcp\", \"port\": 8000, \"endPort\": 8100}]}")},
			want: []string{servingSums[0], servingSums[1], "in ACCEPT udp 51820", "in ACCEPT tcp 8000:8100"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx := newFWFixture(t, tc.mods...)
			fx.mustRun()
			want := tc.want
			if len(want) == 1 {
				want = append(slices.Clone(want), servingSums[1])
			}
			if got := sums(fx.vm().rules); !slices.Equal(got, want) {
				t.Errorf("rules %q, want %q", got, want)
			}
		})
	}
}

// The load-balancer addresses go to the destinations' IPSet while the serving ports are open, and
// a Cluster Mesh peer's addresses to the sources', but only one that lists the cluster back.
func TestVMFirewallIPSetsCarryTheLoadBalancerAndTheMeshPeers(t *testing.T) {
	t.Parallel()
	mesh := func(name string, id int, endpoint, pods, peers string) (*clusterv1.Cluster, *unstructured.Unstructured) {
		c := meshCluster(name, id, endpoint, pods, "{\"enabled\": true, \"peers\": "+peers+"}")
		addVariable(c, FirewallVariable, "{\"enabled\": true}")
		addVariable(c, loadBalancerVariable, "{\"enabled\": true, \"addresses\": [\"10.0."+strconv.Itoa(id)+".30-10.0."+strconv.Itoa(id)+".31\"]}")
		pc := proxmoxClusterObject()
		pc.SetName(name + "-x1")
		pc.SetNamespace(c.Namespace)
		pc.SetLabels(map[string]string{clusterv1.ClusterNameLabel: name})
		pc.Object["spec"] = map[string]any{
			"controlPlaneEndpoint": map[string]any{"host": endpoint},
			"ipv4Config":           map[string]any{"addresses": []any{"10.0." + strconv.Itoa(id) + ".10-10.0." + strconv.Itoa(id) + ".20"}},
		}
		return c, pc
	}
	a, aPC := mesh("a", 1, "10.0.1.5", "10.244.0.0/16", "[\"b\", \"c\"]")
	b, bPC := mesh("b", 2, "10.0.2.5", "10.245.0.0/16", "[\"a\"]")
	c, cPC := mesh("c", 3, "10.0.3.5", "10.246.0.0/16", "[]")
	clusters := []clusterv1.Cluster{*a, *b, *c}
	byCluster := map[types.NamespacedName]*unstructured.Unstructured{}
	for i, pc := range []*unstructured.Unstructured{aPC, bPC, cPC} {
		byCluster[client.ObjectKeyFromObject(&clusters[i])] = pc
	}
	got, err := desiredFirewall(a, aPC, []string{"192.0.2.11"}, clusters, byCluster)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"192.0.2.11", "10.0.1.5", "10.244.0.0/16", "10.0.2.5", "10.245.0.0/16", "10.0.2.10/31"} {
		if !slices.Contains(got.Sources, in) {
			t.Errorf("sources %v lack %s", got.Sources, in)
		}
	}
	for _, out := range []string{"10.0.3.5", "10.246.0.0/16"} {
		if slices.Contains(got.Sources, out) {
			t.Errorf("sources %v hold %s of a cluster that does not list a back", got.Sources, out)
		}
	}
	if want := []string{"10.0.1.30/31"}; !slices.Equal(got.Destinations, want) {
		t.Errorf("destinations %v, want %v", got.Destinations, want)
	}
	closed := a.DeepCopy()
	addVariable(closed, FirewallVariable, "{\"enabled\": true, \"servingPorts\": false}")
	closed.Spec.Topology.Variables = slices.DeleteFunc(closed.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool {
		return v.Name == FirewallVariable && string(v.Value.Raw) == "{\"enabled\": true}"
	})
	if got, err := desiredFirewall(closed, aPC, nil, clusters, byCluster); err != nil || len(got.Destinations) != 0 {
		t.Errorf("destinations %v (%v) with the serving ports closed, want none", got.Destinations, err)
	}
}

// A VM replaced under a machine, with a new VM ID, gets the firewall again, and a reconcile that
// starts with no memory finishes what a failed one began.
func TestVMFirewallSurvivesReplacementRestartAndRetry(t *testing.T) {
	t.Parallel()
	fx := newFWFixture(t)
	fx.mustRun()
	old := fx.vm()
	// The machine is replaced: a new clone with its own ID.
	pm := proxmoxMachineObject()
	if err := fx.kube.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web-cp-x7k2p"}, pm); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(pm.Object, int64(101), "spec", "virtualMachineID"); err != nil {
		t.Fatal(err)
	}
	if err := fx.kube.Update(t.Context(), pm); err != nil {
		t.Fatal(err)
	}
	fresh := &fakeFWVM{guest: old.guest, ipsets: map[string][]string{}, config: map[string]any{"net0": "virtio=BC:24:11:00:00:09,bridge=vmbr0", "digest": "c0"}}
	fresh.guest.VMID = 101
	delete(fx.pve.vms, 100)
	fx.pve.vms[101] = fresh
	fx.mustRun()
	if got := sums(fresh.rules); !slices.Equal(got, servingSums) || !fresh.enable || fresh.config["net0"] != "virtio=BC:24:11:00:00:09,bridge=vmbr0,firewall=1" {
		t.Errorf("the replacement VM holds rules %q, enabled %v, net0 %v", got, fresh.enable, fresh.config["net0"])
	}

	// A write that fails part way, then a restart: a new reconciler reaches the same end state.
	again := newFWFixture(t)
	n := 0
	again.pve.failWrite = func(string, string) error {
		n++
		if n == 6 {
			return errors.New("500 try again")
		}
		return nil
	}
	if _, err := again.run(); err == nil {
		t.Fatal("the failed write was not reported")
	}
	again.r = again.newReconciler()
	again.mustRun()
	again.mustRun()
	if got := sums(again.vm().rules); !slices.Equal(got, servingSums) {
		t.Errorf("rules after a retry %q, want %q", got, servingSums)
	}
	if !again.vm().enable || again.vm().config["net0"] == "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=30" {
		t.Error("the options or the interfaces were not written after the retry")
	}
}

// A VM that is gone ends the reconcile with no error and no write, whether it was gone from the
// start or went between the reads and a write. A machine with no VM ID yet and a VM that Proxmox
// VE still locks are looked at again soon.
func TestVMFirewallLeavesAbsentVMsAlone(t *testing.T) {
	t.Parallel()
	absent := newFWFixture(t)
	delete(absent.pve.vms, 100)
	if res, err := absent.run(); err != nil || res.RequeueAfter != 0 || len(absent.pve.written()) != 0 {
		t.Errorf("absent VM: %+v, %v, writes %v", res, err, absent.pve.written())
	}

	vanishing := newFWFixture(t)
	vanishing.pve.beforeWrite = func() { delete(vanishing.pve.vms, 100) }
	if res, err := vanishing.run(); err != nil || res.RequeueAfter != 0 || vanishing.condition() != nil {
		t.Errorf("vanishing VM: %+v, %v, condition %v", res, err, vanishing.condition())
	}

	locked := newFWFixture(t)
	locked.vm().guest.Lock = "clone"
	if res, err := locked.run(); err != nil || res.RequeueAfter != firewallUnsettled || len(locked.pve.written()) != 0 || locked.pve.reads != 0 {
		t.Errorf("locked VM: %+v, %v, writes %v, reads %d", res, err, locked.pve.written(), locked.pve.reads)
	}

	noID := newFWFixture(t)
	pm := proxmoxMachineObject()
	if err := noID.kube.Get(t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web-cp-x7k2p"}, pm); err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(pm.Object, "spec", "virtualMachineID")
	if err := noID.kube.Update(t.Context(), pm); err != nil {
		t.Fatal(err)
	}
	if res, err := noID.run(); err != nil || res.RequeueAfter != firewallUnsettled || len(noID.pve.written()) != 0 {
		t.Errorf("no VM ID: %+v, %v, writes %v", res, err, noID.pve.written())
	}
}

// A VM ID that another pool or a VM of another name holds now is never written. The machine says
// so at once, with one warning for the cause, and the condition goes when the VM is the machine's.
func TestVMFirewallNeverWritesAnotherVM(t *testing.T) {
	t.Parallel()
	for name, mod := range map[string]func(*fakeFWVM, *firewallFixture){
		"another pool": func(v *fakeFWVM, _ *firewallFixture) { v.guest.Pool = "elsewhere" },
		"another name": func(v *fakeFWVM, _ *firewallFixture) { v.guest.Name = "someone-elses" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx := newFWFixture(t)
			mine := fx.vm().guest
			mod(fx.vm(), fx)
			for range 2 {
				if _, err := fx.run(); err != nil {
					t.Fatal(err)
				}
			}
			if w := fx.pve.written(); len(w) != 0 {
				t.Errorf("wrote to another VM: %v", w)
			}
			if got := fx.condition(); got == nil || got.Status != metav1.ConditionTrue || got.Reason != "VMNotMachines" {
				t.Errorf("condition %+v, want True for VMNotMachines at once", got)
			}
			if got := fx.recorded(); len(got) != 1 || !strings.HasPrefix(got[0], "Warning "+FirewallNotWrittenCondition) {
				t.Errorf("events %q, want one warning", got)
			}
			fx.vm().guest = mine
			fx.mustRun()
			if fx.condition() != nil {
				t.Error("the condition stays once the VM is the machine's")
			}
		})
	}
}

// The controller's block goes above an administrator's rules, which keep their order below it, and
// its own rules that are not wanted any more go.
func TestVMFirewallKeepsOtherPeoplesRulesBelowItsBlock(t *testing.T) {
	t.Parallel()
	fx := newFWFixture(t)
	fx.vm().rules = []pveRule{
		{Type: "in", Action: "DROP", Source: "203.0.113.9", Comment: "mine", Enable: true},
		{Type: "in", Action: "ACCEPT", Proto: "tcp", Dport: "9999", Comment: "kubehenge: serving port", Enable: true},
		{Type: "in", Action: "ACCEPT", Proto: "tcp", Dport: "2222", Comment: "ssh", Enable: true},
	}
	fx.vm().renumber()
	fx.mustRun()
	want := slices.Concat(servingSums, []string{"in DROP from:203.0.113.9", "in ACCEPT tcp 2222"})
	if got := sums(fx.vm().rules); !slices.Equal(got, want) {
		t.Errorf("rules %q, want %q", got, want)
	}
	if got := fx.vm().rules[len(servingSums)].Comment; got != "mine" {
		t.Errorf("the administrator's comment became %q", got)
	}
	n := len(fx.pve.written())
	fx.mustRun()
	if len(fx.pve.written()) != n {
		t.Errorf("a second reconcile wrote %v", fx.pve.written()[n:])
	}
}

// What an administrator changes of the controller's firewall on a VM that was settled is set back,
// each time with a FirewallSetBack event on the Cluster. The first write on a VM is no drift, and
// capmox rewriting an interface without the flag is set back too.
func TestVMFirewallSetsBackDrift(t *testing.T) {
	t.Parallel()
	fx := newFWFixture(t)
	fx.vm().guest.Status = "running"
	if res := fx.mustRun(); res.RequeueAfter != firewallUnsettled {
		t.Errorf("requeue after %v after a write, want %v", res.RequeueAfter, firewallUnsettled)
	}
	if res := fx.mustRun(); res.RequeueAfter != firewallSettled {
		t.Errorf("requeue after %v once settled, want %v", res.RequeueAfter, firewallSettled)
	}
	if got := fx.recorded(); len(got) != 0 {
		t.Fatalf("events %q before any drift", got)
	}
	for name, drift := range map[string]func(*fakeFWVM){
		"a rule deleted":   func(v *fakeFWVM) { v.rules = v.rules[:4]; v.rulesDigest++ },
		"the firewall off": func(v *fakeFWVM) { v.enable = false },
		"policy changed":   func(v *fakeFWVM) { v.policy = "ACCEPT" },
		"an entry removed": func(v *fakeFWVM) { v.ipsets["kubehenge"] = v.ipsets["kubehenge"][1:] },
		"an IPSet deleted": func(v *fakeFWVM) { delete(v.ipsets, "kubehenge-lb") },
		"a flag rewritten": func(v *fakeFWVM) { v.config["net0"] = "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=30" },
		"a stray rule": func(v *fakeFWVM) {
			v.rules = slices.Insert(v.rules, 0, pveRule{Type: "in", Action: "ACCEPT", Source: "198.51.100.1", Comment: "kubehenge: old", Enable: true})
			v.renumber()
		},
	} {
		drift(fx.vm())
		fx.mustRun()
		if got := fx.recorded(); len(got) != 1 || !strings.HasPrefix(got[0], "Normal "+FirewallSetBack) {
			t.Errorf("%s: events %q, want one %s", name, got, FirewallSetBack)
		}
		if got := sums(fx.vm().rules); !slices.Equal(got, servingSums) || !fx.vm().enable || fx.vm().policy != "DROP" || !strings.HasSuffix(fmt.Sprint(fx.vm().config["net0"]), "firewall=1") || len(fx.vm().ipsets) != 2 {
			t.Errorf("%s: not set back: rules %q, enabled %v, policy %s, net0 %v", name, got, fx.vm().enable, fx.vm().policy, fx.vm().config["net0"])
		}
		fx.mustRun()
	}
}

// A write refused for a minute gives the Machine FirewallNotWritten and the Cluster one warning for
// the cause, however often it is tried. The condition goes with the first write that works.
func TestVMFirewallSaysWhenItCannotWrite(t *testing.T) {
	t.Parallel()
	fx := newFWFixture(t)
	fx.pve.failWrite = func(string, string) error {
		return errors.New("403 Permission check failed (/vms/100, VM.Config.Network)")
	}
	if _, err := fx.run(); err == nil || fx.condition() != nil {
		t.Fatalf("first failure: err %v, condition %v, want an error and no condition yet", err, fx.condition())
	}
	fx.now = fx.now.Add(30 * time.Second)
	_, _ = fx.run()
	if fx.condition() != nil {
		t.Error("the condition is set before the writes failed for a minute")
	}
	fx.now = fx.now.Add(40 * time.Second)
	if _, err := fx.run(); err == nil {
		t.Fatal("a refused write was not reported")
	}
	if got := fx.condition(); got == nil || got.Status != metav1.ConditionTrue || got.Reason != "WriteFailed" || !strings.Contains(got.Message, "VM.Config.Network") {
		t.Fatalf("condition %+v, want True for WriteFailed naming the right", got)
	}
	_, _ = fx.run()
	if got := fx.recorded(); len(got) != 1 || !strings.HasPrefix(got[0], "Warning "+FirewallNotWrittenCondition) {
		t.Errorf("events %q, want one warning for the cause", got)
	}
	fx.pve.failWrite = nil
	fx.mustRun()
	if fx.condition() != nil {
		t.Error("the condition stays after a write that works")
	}
}

// A write whose digest another editor made stale is refused by Proxmox VE and tried again from a
// fresh read, without a failure the Machine would carry.
func TestVMFirewallRetriesAfterAnEditInBetween(t *testing.T) {
	t.Parallel()
	fx := newFWFixture(t)
	fx.pve.beforeWrite = func() {
		vm := fx.vm()
		vm.optionsDigest++
		vm.rulesDigest++
		vm.config["digest"] = "other"
	}
	if _, err := fx.run(); err == nil {
		t.Fatal("a refused write was not reported")
	}
	fx.mustRun()
	fx.mustRun()
	if got := sums(fx.vm().rules); !slices.Equal(got, servingSums) || !fx.vm().enable || !strings.HasSuffix(fmt.Sprint(fx.vm().config["net1"]), "firewall=1") {
		t.Errorf("rules %q, enabled %v, net1 %v after the retry", got, fx.vm().enable, fx.vm().config["net1"])
	}
	if fx.condition() != nil {
		t.Error("a single refused write left a condition")
	}
}

// A security group added to a cluster reaches its running nodes in place, between the cluster's own
// traffic and the serving ports. A new address of the appliance reaches the IPSet, and an old one goes.
func TestVMFirewallFollowsTheClusterInPlace(t *testing.T) {
	t.Parallel()
	fx := newFWFixture(t)
	fx.vm().guest.Status = "running"
	fx.mustRun()
	fx.recorded()
	fx.setCluster(func(c *clusterv1.Cluster) { addVariable(c, SecurityGroupsVariable, "[\"k8s\"]") })
	n := len(fx.pve.written())
	fx.mustRun()
	want := slices.Concat(servingSums[:1], []string{"group k8s"}, servingSums[1:])
	if got := sums(fx.vm().rules); !slices.Equal(got, want) {
		t.Errorf("rules %q, want %q", got, want)
	}
	for _, w := range fx.pve.written()[n:] {
		if !strings.Contains(w, "/firewall/rules") {
			t.Errorf("a security group change wrote %s", w)
		}
	}
	fx.r.Sources = fixedManagement{"192.0.2.12"}
	fx.mustRun()
	if got := fx.vm().ipsets["kubehenge"]; !slices.Contains(got, "192.0.2.12") || slices.Contains(got, "192.0.2.11") {
		t.Errorf("IPSet kubehenge holds %v, want the new address only", got)
	}
	// Entries Proxmox VE shows in another form are the same entries.
	fx.vm().ipsets["kubehenge"] = slices.Concat(fx.vm().ipsets["kubehenge"], nil)
	for i, e := range fx.vm().ipsets["kubehenge"] {
		if !strings.Contains(e, "/") && !strings.Contains(e, ":") {
			fx.vm().ipsets["kubehenge"][i] = e + "/32"
		}
	}
	n = len(fx.pve.written())
	fx.mustRun()
	if got := fx.pve.written()[n:]; len(got) != 0 {
		t.Errorf("an address as /32 was rewritten: %v", got)
	}
}

// The firewall off takes the controller's rules and IPSets and the options off a VM that has them,
// and leaves a VM it never wrote, with the administrator's own firewall, untouched.
func TestVMFirewallOffRemovesOnlyItsOwn(t *testing.T) {
	t.Parallel()
	fx := newFWFixture(t)
	fx.vm().guest.Status = "running"
	fx.vm().rules = []pveRule{{Type: "in", Action: "DROP", Source: "203.0.113.9", Comment: "mine", Enable: true}}
	fx.vm().renumber()
	fx.mustRun()
	fx.setCluster(func(c *clusterv1.Cluster) {
		c.Spec.Topology.Variables = slices.DeleteFunc(c.Spec.Topology.Variables, func(v clusterv1.ClusterVariable) bool { return v.Name == FirewallVariable })
		addVariable(c, FirewallVariable, "{\"enabled\": false}")
	})
	fx.mustRun()
	vm := fx.vm()
	if got := sums(vm.rules); !slices.Equal(got, []string{"in DROP from:203.0.113.9"}) || vm.enable || len(vm.ipsets) != 0 {
		t.Errorf("rules %q, enabled %v, IPSets %v, want only the administrator's rule", got, vm.enable, vm.ipsets)
	}
	n := len(fx.pve.written())
	fx.mustRun()
	if got := fx.pve.written()[n:]; len(got) != 0 {
		t.Errorf("a second reconcile with the firewall off wrote %v", got)
	}

	never := newFWFixture(t, func(c *clusterv1.Cluster) {
		c.Spec.Topology.Variables = nil
		addVariable(c, FirewallVariable, "{\"enabled\": false}")
	})
	never.vm().enable = true
	never.vm().rules = []pveRule{{Type: "in", Action: "DROP", Source: "203.0.113.9", Comment: "mine", Enable: true}}
	never.vm().renumber()
	never.mustRun()
	if w := never.pve.written(); len(w) != 0 || !never.vm().enable {
		t.Errorf("a VM Kubehenge never wrote was changed: %v, enabled %v", w, never.vm().enable)
	}
}

// A change of a Cluster, or of a Cluster Mesh peer, queues the machines whose IPSets it changes.
func TestVMFirewallQueuesMachinesOfTheClusterAndItsPeers(t *testing.T) {
	t.Parallel()
	a := meshCluster("a", 1, "10.0.1.5", "10.244.0.0/16", "{\"enabled\": true, \"peers\": [\"b\"]}")
	b := meshCluster("b", 2, "10.0.2.5", "10.245.0.0/16", "{\"enabled\": true, \"peers\": [\"a\"]}")
	c := meshCluster("c", 3, "10.0.3.5", "10.246.0.0/16", "")
	objects := make([]client.Object, 0, 6)
	objects = append(objects, a, b, c)
	for _, name := range []string{"a", "b", "c"} {
		pm := proxmoxMachineObject()
		pm.SetName(name + "-cp")
		pm.SetNamespace("team-a")
		pm.SetLabels(map[string]string{clusterv1.ClusterNameLabel: name})
		objects = append(objects, pm)
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	r := &VMFirewall{Client: kube}
	reqs := r.machinesOfCluster(t.Context(), a)
	got := make([]string, 0, len(reqs))
	for _, req := range reqs {
		got = append(got, req.Name)
	}
	slices.Sort(got)
	if want := []string{"a-cp", "b-cp"}; !slices.Equal(got, want) {
		t.Errorf("a change of a queues %v, want %v", got, want)
	}
}

func TestVMNameFollowsCapmox(t *testing.T) {
	t.Parallel()
	machine := func(name, prefix string, annotated bool) *unstructured.Unstructured {
		pm := proxmoxMachineObject()
		pm.SetName("template-x")
		if annotated {
			pm.SetAnnotations(map[string]string{vmNamePrefixAnnotation: prefix})
		}
		if name != "" {
			pm.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Machine", Name: name}})
		}
		return pm
	}
	long := strings.Repeat("p", 60)
	for name, tc := range map[string]struct {
		pm   *unstructured.Unstructured
		want string
	}{
		"the cluster's name as the prefix": {machine("web-cp-x", "web", true), "web-cp-x"},
		"a prefix in its place":            {machine("web-cp-x", "dev", true), "dev-cp-x"},
		"an empty prefix":                  {machine("web-cp-x", "", true), "web-cp-x"},
		"a prefix too long for the name":   {machine("web-cp-x", long, true), "web-cp-x"},
		"no annotation":                    {machine("web-cp-x", "", false), "template-x"},
		"no Machine":                       {machine("", "web", true), "template-x"},
		"a Machine not of the cluster":     {machine("other-cp-x", "dev", true), "other-cp-x"},
	} {
		if got := vmNameOf(tc.pm, "web"); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
