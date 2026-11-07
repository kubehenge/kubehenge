package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// fakeNodeAPI is as much of a workload cluster's API server as draining a node takes: its node,
// the pods on it and the DaemonSets that own some, eviction, which deletes a pod at once, and pod
// deletion. It records what was done.
type fakeNodeAPI struct {
	mu   sync.Mutex
	node *corev1.Node
	pods []*corev1.Pod
	done []string
	// held maps the name of a pod whose eviction a disruption budget refuses to that budget's name.
	held map[string]string
	sets []appsv1.StatefulSet
}

// refuse answers an eviction the way a disruption budget with no disruption left does.
func refuse(w http.ResponseWriter, budget string) {
	writeJSON(w, http.StatusTooManyRequests, metav1.Status{
		Kind: "Status", APIVersion: "v1", Status: metav1.StatusFailure, Reason: metav1.StatusReasonTooManyRequests, Code: http.StatusTooManyRequests,
		Message: "Cannot evict pod as it would violate the pod's disruption budget.",
		Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{
			Type: "DisruptionBudget", Message: "The disruption budget " + budget + " needs 2 healthy pods and has 2 currently",
		}}},
	})
}

func (f *fakeNodeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	succeeded := metav1.Status{Kind: "Status", APIVersion: "v1", Status: metav1.StatusSuccess}
	notFound := func() {
		writeJSON(w, http.StatusNotFound, metav1.Status{
			Kind: "Status", APIVersion: "v1", Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound,
		})
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1":
		writeJSON(w, http.StatusOK, metav1.APIResourceList{Kind: "APIResourceList", GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "nodes", Kind: "Node", Verbs: []string{"get", "patch"}},
			{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list", "delete"}},
			{Name: "pods/eviction", Kind: "Eviction", Group: "policy", Version: "v1", Namespaced: true, Verbs: []string{"create"}},
		}})
	case len(parts) == 4 && parts[2] == "nodes":
		if parts[3] != f.node.Name {
			notFound()
			return
		}
		if r.Method == http.MethodPatch && !f.cordon(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, f.node)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pods":
		list := corev1.PodList{Kind: "PodList", APIVersion: "v1"}
		for _, p := range f.pods {
			if selected(p, r.URL.Query().Get("fieldSelector")) {
				list.Items = append(list.Items, *p)
			}
		}
		writeJSON(w, http.StatusOK, list)
	case r.Method == http.MethodGet && len(parts) == 7 && parts[0] == "apis" && parts[5] == "daemonsets":
		writeJSON(w, http.StatusOK, appsv1.DaemonSet{Kind: "DaemonSet", APIVersion: "apps/v1", Namespace: parts[4], Name: parts[6]})
	case r.Method == http.MethodGet && r.URL.Path == "/apis/apps/v1/statefulsets":
		writeJSON(w, http.StatusOK, appsv1.StatefulSetList{Kind: "StatefulSetList", APIVersion: "apps/v1", Items: f.sets})
	case r.Method == http.MethodPost && len(parts) == 7 && parts[4] == "pods" && parts[6] == "eviction":
		if budget := f.held[parts[5]]; budget != "" {
			refuse(w, budget)
			return
		}
		if !f.remove(parts[3], parts[5]) {
			notFound()
			return
		}
		f.done = append(f.done, "evict "+parts[5])
		writeJSON(w, http.StatusCreated, succeeded)
	case len(parts) == 6 && parts[4] == "pods":
		i := slices.IndexFunc(f.pods, func(p *corev1.Pod) bool { return p.Namespace == parts[3] && p.Name == parts[5] })
		if i < 0 {
			notFound()
			return
		}
		if r.Method == http.MethodDelete {
			f.done = append(f.done, "delete "+parts[5])
			f.pods = slices.Delete(f.pods, i, i+1)
			writeJSON(w, http.StatusOK, succeeded)
			return
		}
		writeJSON(w, http.StatusOK, f.pods[i])
	default:
		notFound()
	}
}

// cordon applies a patch of the node's unschedulable field, which the strategic merge patch sets
// to true, or to null to clear it. It returns false when it refused the patch.
func (f *fakeNodeAPI) cordon(w http.ResponseWriter, r *http.Request) bool {
	var patch struct {
		Spec map[string]any `json:"spec"`
	}
	body, err := io.ReadAll(r.Body)
	if err == nil {
		err = json.Unmarshal(body, &patch)
	}
	value, ok := patch.Spec["unschedulable"]
	unschedulable, isBool := value.(bool)
	if err != nil || !ok || (!isBool && value != nil) {
		writeJSON(w, http.StatusUnprocessableEntity, metav1.Status{Kind: "Status", APIVersion: "v1", Status: metav1.StatusFailure, Message: string(body)})
		return false
	}
	f.node.Spec.Unschedulable = unschedulable
	f.done = append(f.done, map[bool]string{true: "cordon", false: "uncordon"}[unschedulable])
	return true
}

// release lets the budget holding a pod go, as the replicas it waited for become ready.
func (f *fakeNodeAPI) release(pod string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.held, pod)
}

func (f *fakeNodeAPI) did() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.done)
}

func (f *fakeNodeAPI) left() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.pods))
	for _, p := range f.pods {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return names
}

func (f *fakeNodeAPI) unschedulable() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.node.Spec.Unschedulable
}

func (f *fakeNodeAPI) remove(namespace, name string) bool {
	for i, p := range f.pods {
		if p.Namespace == namespace && p.Name == name {
			f.pods = slices.Delete(f.pods, i, i+1)
			return true
		}
	}
	return false
}

// selected reads a pod list's field selector, spec.nodeName and status.phase as the drain uses them.
func selected(p *corev1.Pod, selector string) bool {
	for term := range strings.SplitSeq(selector, ",") {
		field, value, _ := strings.Cut(term, "=")
		switch {
		case field == "spec.nodeName" && p.Spec.NodeName != value,
			field == "status.phase" && string(p.Status.Phase) != value:
			return false
		}
	}
	return true
}

// nodePod is a pod on node web-w-1, owned by a controller of the kind given, if any.
func nodePod(name, kind string, phase corev1.PodPhase) *corev1.Pod {
	p := &corev1.Pod{
		Kind: "Pod", APIVersion: "v1",
		Namespace: "apps", Name: name, UID: types.UID("uid-" + name),
		Spec:   corev1.PodSpec{NodeName: "web-w-1", Containers: []corev1.Container{{Name: "app", Image: "app"}}},
		Status: corev1.PodStatus{Phase: phase},
	}
	if kind != "" {
		yes := true
		apiVersion := "apps/v1"
		if kind == "Job" {
			apiVersion = "batch/v1"
		}
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name + "-owner", UID: "owner", Controller: &yes}}
	}
	return p
}

// drainRig is a RemoteDrainer reaching cluster web's API server, which runs node web-w-1.
func drainRig(t *testing.T, pods ...*corev1.Pod) (*RemoteDrainer, *fakeNodeAPI, *clusterv1.Cluster) {
	t.Helper()
	api := &fakeNodeAPI{node: &corev1.Node{Kind: "Node", APIVersion: "v1", Name: "web-w-1"}, pods: pods}
	config := serveKubeAPI(t, api)
	c := cluster()
	return &RemoteDrainer{Remotes: &Remotes{byCluster: map[string]remote{
		remoteKey(c): {config: config, until: time.Now().Add(time.Hour)},
	}}}, api, c
}

// Draining a node cordons it and evicts every pod but the DaemonSets', those no controller owns
// and those with emptyDir data among them, as kubectl drain --force does. Pods elsewhere stay.
func TestRemoteDrainerCordonsAndEvictsAllButDaemonSetPods(t *testing.T) {
	t.Parallel()
	scratch := nodePod("cache", "ReplicaSet", corev1.PodRunning)
	scratch.Spec.Volumes = []corev1.Volume{{Name: "scratch", EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	elsewhere := nodePod("elsewhere", "ReplicaSet", corev1.PodRunning)
	elsewhere.Spec.NodeName = "web-w-2"
	d, api, c := drainRig(t,
		nodePod("web", "ReplicaSet", corev1.PodRunning), nodePod("bare", "", corev1.PodRunning), scratch,
		nodePod("cilium", "DaemonSet", corev1.PodRunning), elsewhere,
	)
	if err := d.Drain(t.Context(), c, "web-w-1", DrainOptions{}); err != nil {
		t.Fatal(err)
	}
	if !api.unschedulable() {
		t.Error("the node was not cordoned")
	}
	did := api.did()
	if len(did) == 0 || did[0] != "cordon" {
		t.Errorf("did %v; the cordon comes first", did)
	}
	evicted := slices.DeleteFunc(slices.Clone(did), func(s string) bool { return !strings.HasPrefix(s, "evict ") })
	slices.Sort(evicted)
	if want := []string{"evict bare", "evict cache", "evict web"}; !slices.Equal(evicted, want) {
		t.Errorf("evicted %v, want %v", evicted, want)
	}
	if left := api.left(); !slices.Equal(left, []string{"cilium", "elsewhere"}) {
		t.Errorf("left %v", left)
	}
}

// Letting the node back uncordons it and clears the DaemonSets' pods its shutdown rejected.
func TestRemoteDrainerUncordonsAndClearsRejectedDaemonPods(t *testing.T) {
	t.Parallel()
	d, api, c := drainRig(t,
		nodePod("cilium-rejected", "DaemonSet", corev1.PodFailed), nodePod("cilium", "DaemonSet", corev1.PodRunning),
		nodePod("job", "Job", corev1.PodFailed),
	)
	api.node.Spec.Unschedulable = true
	if err := d.Uncordon(t.Context(), c, "web-w-1"); err != nil {
		t.Fatal(err)
	}
	if api.unschedulable() {
		t.Error("the node stayed cordoned")
	}
	if did := api.did(); !slices.Equal(did, []string{"uncordon", "delete cilium-rejected"}) {
		t.Errorf("did %v", did)
	}
}

// A node the cluster does not know fails, and the cluster's client is dropped, as its kubeconfig
// may be stale. A cluster without a kubeconfig fails before anything is done.
func TestRemoteDrainerFailsWithoutTheNodeOrTheKubeconfig(t *testing.T) {
	t.Parallel()
	d, api, c := drainRig(t)
	if err := d.Drain(t.Context(), c, "web-w-9", DrainOptions{}); err == nil || !strings.Contains(err.Error(), "reading node web-w-9") {
		t.Errorf("draining an unknown node: %v", err)
	}
	d.Remotes.mu.Lock()
	_, kept := d.Remotes.byCluster[remoteKey(c)]
	d.Remotes.mu.Unlock()
	if kept {
		t.Error("kept the cluster's client after it failed")
	}
	if did := api.did(); len(did) != 0 {
		t.Errorf("did %v for an unknown node", did)
	}

	d, api, c = drainRig(t)
	if err := d.Uncordon(t.Context(), c, "web-w-9"); err == nil || !strings.Contains(err.Error(), "reading node web-w-9") {
		t.Errorf("uncordoning an unknown node: %v", err)
	}

	_, kube := newReconciler(t, &fakeCloud{})
	none := &RemoteDrainer{Remotes: &Remotes{Reader: kube}}
	drain := func(ctx context.Context, c *clusterv1.Cluster, node string) error {
		return none.Drain(ctx, c, node, DrainOptions{})
	}
	for name, call := range map[string]func(context.Context, *clusterv1.Cluster, string) error{"drain": drain, "uncordon": none.Uncordon} {
		if err := call(t.Context(), c, "web-w-1"); err == nil || !strings.Contains(err.Error(), "no kubeconfig yet") {
			t.Errorf("%s without a kubeconfig: %v", name, err)
		}
	}
	if did := api.did(); len(did) != 0 {
		t.Errorf("did %v", did)
	}
}

// The helper keeps the drain's own settings, and none of its own timeout: the caller's bounds the drain.
func TestDrainHelperSettings(t *testing.T) {
	t.Parallel()
	d, _, c := drainRig(t)
	h, err := d.helper(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if h.Timeout != 0 || !h.Force || !h.IgnoreAllDaemonSets || !h.DeleteEmptyDirData || h.GracePeriodSeconds != -1 {
		t.Errorf("drain helper %+v", h)
	}
}

// heldRig is a node with a database pod a disruption budget holds, the retry short enough for a test.
func heldRig(t *testing.T) (*RemoteDrainer, *fakeNodeAPI, *clusterv1.Cluster) {
	t.Helper()
	d, api, c := drainRig(t, nodePod("db-0", "StatefulSet", corev1.PodRunning))
	api.held = map[string]string{"db-0": "db-pdb"}
	d.RetryDelay = time.Millisecond
	return d, api, c
}

// A drain a disruption budget holds asks again until the budget lets the pod go, and says each round
// which pod is held and by which budget, in the words Cluster API uses for a Machine.
func TestRemoteDrainerWaitsOnABudgetAndNamesIt(t *testing.T) {
	t.Parallel()
	d, api, c := heldRig(t)
	var reports []string
	var refusals []bool
	err := d.Drain(t.Context(), c, "web-w-1", DrainOptions{Timeout: time.Minute, Blocked: func(held string, refused bool) {
		reports, refusals = append(reports, held), append(refusals, refused)
		if len(reports) == 3 {
			api.release("db-0")
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Three rounds the budget refuses, then one where the eviction goes through and the pod is not gone yet.
	if len(reports) != 4 || !slices.Equal(refusals, []bool{true, true, true, false}) {
		t.Fatalf("reports %q, refusals %v", reports, refusals)
	}
	if want := "Pod apps/db-0: deletionTimestamp set, but still not removed from the Node"; reports[3] != want {
		t.Errorf("last report %q, want %q", reports[3], want)
	}
	want := "Pod apps/db-0: cannot evict pod as it would violate the pod's disruption budget. The disruption budget db-pdb needs 2 healthy pods and has 2 currently"
	if reports[0] != want {
		t.Errorf("reported %q, want %q", reports[0], want)
	}
	if left := api.left(); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// When the timeout runs out with a pod still held, the drain returns ErrDrainTimeout and leaves the
// pod where it is, which is what a Machine's deletion does at its nodeDrainTimeoutSeconds.
func TestRemoteDrainerStopsAtItsTimeoutWithThePodLeft(t *testing.T) {
	t.Parallel()
	d, api, c := heldRig(t)
	rounds := 0
	err := d.Drain(t.Context(), c, "web-w-1", DrainOptions{Timeout: 50 * time.Millisecond, Blocked: func(string, bool) { rounds++ }})
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("drain: %v", err)
	}
	if rounds == 0 {
		t.Error("the held pod was never reported")
	}
	if left := api.left(); !slices.Equal(left, []string{"db-0"}) {
		t.Errorf("left %v", left)
	}
}

// A timeout of zero never ends the wait.
func TestRemoteDrainerWithoutATimeoutWaitsForEver(t *testing.T) {
	t.Parallel()
	d, api, c := heldRig(t)
	rounds := 0
	err := d.Drain(t.Context(), c, "web-w-1", DrainOptions{Blocked: func(string, bool) {
		rounds++
		if rounds == 40 {
			api.release("db-0")
		}
	}})
	if err != nil || rounds < 40 {
		t.Fatalf("drain: %v after %d rounds", err, rounds)
	}
}

// A drain cancelled from outside returns that, not ErrDrainTimeout.
func TestRemoteDrainerCancelledIsNotATimeout(t *testing.T) {
	t.Parallel()
	d, _, c := heldRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	err := d.Drain(ctx, c, "web-w-1", DrainOptions{Timeout: time.Minute, Blocked: func(string, bool) { cancel() }})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("drain: %v", err)
	}
}

// A StatefulSet is short when fewer replicas are ready than it wants, one replica when it says none.
// It holds the drain only with a ready pod on the node: its broken pod, or its pods on other nodes,
// lose it nothing more.
func TestRemoteDrainerNamesTheStatefulSetsShortOfReplicas(t *testing.T) {
	t.Parallel()
	three, none := int32(3), int32(0)
	set := func(name string, want *int32, ready int32) appsv1.StatefulSet {
		return appsv1.StatefulSet{
			Namespace: "apps", Name: name,
			Spec: appsv1.StatefulSetSpec{Replicas: want}, Status: appsv1.StatefulSetStatus{ReadyReplicas: ready},
		}
	}
	pod := func(set, node string, ready corev1.ConditionStatus) *corev1.Pod {
		p := nodePod(set+"-0", "StatefulSet", corev1.PodRunning)
		p.OwnerReferences[0].Name = set
		p.Spec.NodeName = node
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}
		return p
	}
	on, ready := "web-w-1", corev1.ConditionTrue
	d, api, c := drainRig(t,
		pod("db", on, ready), pod("ok", on, ready), pod("default", on, ready), pod("fine", on, ready),
		pod("broken", on, corev1.ConditionFalse), pod("elsewhere", "web-w-2", ready),
	)
	api.sets = []appsv1.StatefulSet{
		set("db", &three, 2), set("ok", &three, 3), set("off", &none, 0), set("default", nil, 0), set("fine", nil, 1),
		set("broken", &three, 2), set("elsewhere", &three, 1), set("idle", &three, 0),
	}
	got, err := d.ShortStatefulSets(t.Context(), c, on)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"StatefulSet apps/db has 2 of 3 replicas ready", "StatefulSet apps/default has 0 of 1 replicas ready"}
	if !slices.Equal(got, want) {
		t.Errorf("short %q, want %q", got, want)
	}
	if got, err := d.ShortStatefulSets(t.Context(), c, "web-w-3"); err != nil || got != nil {
		t.Errorf("an empty node: short %q, %v", got, err)
	}
}

// Started by the manager, the upgrader looks at once, and returns only after the upgrades it began
// have finished, once the manager stops it.
func TestTalosUpgraderStartLooksAtOnceAndWaitsForItsUpgrades(t *testing.T) {
	t.Parallel()
	r := newTalosRig(t, talosCluster("web", "v1.14.4"), talosMachine("web-cp-0", "192.0.2.10", "v1.14.3"))
	r.u.Interval = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.u.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.talos.mu.Lock()
	installed, rebooted := r.talos.installed["192.0.2.10"], slices.Clone(r.talos.rebooted)
	r.talos.mu.Unlock()
	if installed == "" || !slices.Equal(rebooted, []string{"192.0.2.10"}) {
		t.Errorf("installed %q, rebooted %v by the time Start returned", installed, rebooted)
	}
	if r.u.isBusy(talosCluster("web", "v1.14.4")) {
		t.Error("the cluster is still marked busy after its upgrade finished")
	}
}
