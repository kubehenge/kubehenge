package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

func attentionWhats(v *healthView) []string {
	whats := make([]string, 0, len(v.Attention))
	for i := range v.Attention {
		whats = append(whats, v.Attention[i].What)
	}
	return whats
}

func TestHealthWarnsOfProxmoxNodesWhoseClockIsOff(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	h.accounts.nodeSkew = map[string]time.Duration{"pve1": clockSkewLimit, "pve2": -3 * time.Minute}

	admin := h.browser()
	admin.signIn("admin@pve")
	v, _ := readHealthView(t, admin)
	whats := attentionWhats(&v)
	if !slices.Contains(whats, "Proxmox VE node pve2 clock is off by 3m0s") {
		t.Errorf("a node three minutes behind: %q", whats)
	}
	if slices.ContainsFunc(whats, func(w string) bool { return strings.Contains(w, "node pve1 clock") }) {
		t.Errorf("a node exactly at the limit is warned of: %q", whats)
	}

	// Proxmox VE shows a node's time to users with Sys.Audit on it alone. The page then says nothing
	// of clocks, and no error.
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	v, _ = readHealthView(t, viewer)
	if slices.ContainsFunc(attentionWhats(&v), func(w string) bool { return strings.Contains(w, "Proxmox VE node") }) {
		t.Errorf("a user without Sys.Audit on the node sees its clock: %q", attentionWhats(&v))
	}
}

func TestHealthWarnsOfAProxmoxNodeAheadAndKeepsTheVerdictDegraded(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	h.accounts.nodeSkew = map[string]time.Duration{"pve1": 31 * time.Second}
	admin := h.browser()
	admin.signIn("admin@pve")
	v, _ := readHealthView(t, admin)
	whats := attentionWhats(&v)
	if !slices.Contains(whats, "Proxmox VE node pve1 clock is off by 31s") {
		t.Fatalf("a node 31 seconds ahead: %q", whats)
	}
	i := slices.IndexFunc(v.Attention, func(a attentionView) bool { return strings.HasPrefix(a.What, "Proxmox VE node") })
	if v.Attention[i].Severity != severityWarning || !strings.Contains(v.Attention[i].Message, "ahead of") {
		t.Errorf("row = %+v", v.Attention[i])
	}
}

func TestAMachineOnAnOfflineNodeDegradesItsCluster(t *testing.T) {
	t.Parallel()
	two := cluster("default", "two")
	two.UID = "uid-two"
	h, _ := healthHarness(t, two)
	const offline = "Proxmox VE node pve2 is offline"
	read := func() (healthView, fleetHealthView) {
		t.Helper()
		b := h.browser()
		b.signIn("admin@pve")
		rec := b.do("GET", "/api/clusters/default/two/health", "")
		expect(t, rec, http.StatusOK)
		var page healthView
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		rec = b.do("GET", "/api/fleet/health", "")
		expect(t, rec, http.StatusOK)
		var fleet []fleetHealthView
		if err := json.Unmarshal(rec.Body.Bytes(), &fleet); err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(fleet, func(f fleetHealthView) bool { return f.Name == "two" })
		if i < 0 {
			t.Fatalf("fleet %+v lacks cluster two", fleet)
		}
		return page, fleet[i]
	}
	listsOffline := func(a []attentionView) bool {
		return slices.ContainsFunc(a, func(a attentionView) bool { return a.What == offline && a.Severity == severityError })
	}

	page, summary := read()
	if page.Verdict == verdictDegraded || summary.Verdict == verdictDegraded || listsOffline(page.Attention) || listsOffline(summary.Attention) {
		t.Fatalf("with every node online: page %s %q, fleet %s %+v", page.Verdict, attentionWhats(&page), summary.Verdict, summary.Attention)
	}

	// pve2 runs a worker of the cluster and is down, which Cluster API notices only later.
	h.accounts.mu.Lock()
	h.accounts.nodes = []pve.Node{{Node: "pve1", Online: true, MaxMemory: 64 << 30}, {Node: "pve2"}}
	h.accounts.mu.Unlock()
	page, summary = read()
	if page.Verdict != verdictDegraded || !listsOffline(page.Attention) {
		t.Errorf("page: %s %q, want degraded with %q", page.Verdict, attentionWhats(&page), offline)
	}
	if summary.Verdict != verdictDegraded || !listsOffline(summary.Attention) {
		t.Errorf("fleet: %s %+v, want degraded with %q", summary.Verdict, summary.Attention, offline)
	}
}

// talosClockProbe answers for the addresses a test names.
type talosClockProbe struct {
	mu      sync.Mutex
	synced  map[string]bool
	failing map[string]bool
	asked   []string
}

func (p *talosClockProbe) probe(_ context.Context, raw []byte, address string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, address+" "+string(raw))
	if p.failing[address] {
		return false, errors.New("the node does not answer")
	}
	return p.synced[address], nil
}

func TestHealthSaysWhichTalosNodesHaveNotSyncedTheirClocks(t *testing.T) {
	t.Parallel()
	h, workloads := healthHarness(t)
	ctx := context.Background()
	c := cluster("default", "one")
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.Topology.ClassRef.Name = "kubehenge-talos"
	if err := h.kube.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{Namespace: "default", Name: provisioner.TalosconfigSecret("one"), Data: map[string][]byte{provisioner.TalosconfigKey: []byte("the talosconfig")}}
	if err := h.kube.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	for name, address := range map[string]string{"one-cp-a": "192.0.2.11", "one-md-0-a": "192.0.2.12"} {
		n, err := workloads.kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}
		if _, err := workloads.kube.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	probe := &talosClockProbe{synced: map[string]bool{"192.0.2.11": false, "192.0.2.12": true}, failing: map[string]bool{}}
	h.server.TalosClock = probe.probe

	b := h.browser()
	b.signIn("viewer@pve")
	v, _ := readHealthView(t, b)
	whats := attentionWhats(&v)
	if !slices.Contains(whats, "Control plane one-cp-a: clock not synced") {
		t.Errorf("a node whose time sync has not succeeded: %q", whats)
	}
	if slices.ContainsFunc(whats, func(w string) bool { return strings.Contains(w, "Worker one-md-0-a: clock not synced") }) {
		t.Errorf("a synced node is reported: %q", whats)
	}
	if !slices.Contains(probe.asked, "192.0.2.11 the talosconfig") {
		t.Errorf("the nodes were asked as %q", probe.asked)
	}
}

func TestHealthSaysNothingOfAClockTalosDoesNotAnswerFor(t *testing.T) {
	t.Parallel()
	h, workloads := healthHarness(t)
	ctx := context.Background()
	c := cluster("default", "one")
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.Topology.ClassRef.Name = "kubehenge-talos"
	if err := h.kube.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	n, err := workloads.kube.CoreV1().Nodes().Get(ctx, "one-cp-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.11"}}
	if _, err := workloads.kube.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	probe := &talosClockProbe{failing: map[string]bool{"192.0.2.11": true}}
	h.server.TalosClock = probe.probe
	b := h.browser()
	b.signIn("viewer@pve")

	// Without a talosconfig nobody is asked.
	v, _ := readHealthView(t, b)
	if len(probe.asked) != 0 || slices.ContainsFunc(attentionWhats(&v), func(w string) bool { return strings.Contains(w, "clock not synced") }) {
		t.Errorf("asked %q, attention %q", probe.asked, attentionWhats(&v))
	}

	// A node that does not answer says nothing.
	secret := &corev1.Secret{Namespace: "default", Name: provisioner.TalosconfigSecret("one"), Data: map[string][]byte{provisioner.TalosconfigKey: []byte("x")}}
	if err := h.kube.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	h.clock = h.clock.Add(healthFresh + time.Second)
	v, _ = readHealthView(t, b)
	if len(probe.asked) == 0 || slices.ContainsFunc(attentionWhats(&v), func(w string) bool { return strings.Contains(w, "clock not synced") }) {
		t.Errorf("asked %q, attention %q", probe.asked, attentionWhats(&v))
	}
}
