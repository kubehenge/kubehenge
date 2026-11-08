package provisioner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	"k8s.io/apimachinery/pkg/api/meta"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

const certificateRefused = "596 certificate verify failed"

// forwardingPVE is a Proxmox VE API whose nodes are listed online in the cluster and answer
// calls forwarded to them in the mode each is set to: "ok", "596" or "hang".
type forwardingPVE struct {
	mu    sync.Mutex
	mode  map[string]string
	list  []string
	reads map[string]int
	stop  chan struct{}
}

func newForwardingPVE(t *testing.T, modes map[string]string) (*forwardingPVE, *Proxmox) {
	t.Helper()
	f := &forwardingPVE{mode: modes, reads: map[string]int{}, stop: make(chan struct{})}
	for name := range modes {
		f.list = append(f.list, name)
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() { close(f.stop); srv.Close() })
	// The client the controller uses: it fails on the 596 that go-proxmox would take for success.
	hc := &http.Client{Transport: pve.CheckStatus(http.DefaultTransport)}
	return f, &Proxmox{Client: proxmox.NewClient(srv.URL+"/api2/json", proxmox.WithHTTPClient(hc))}
}

func (f *forwardingPVE) set(name, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode[name] = mode
}

func (f *forwardingPVE) statusReads(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[name]
}

func (f *forwardingPVE) serve(w http.ResponseWriter, r *http.Request) {
	var data any = []any{}
	switch path := strings.TrimPrefix(r.URL.Path, "/api2/json"); {
	case path == "/nodes":
		f.mu.Lock()
		nodes := make([]map[string]any, 0, len(f.list))
		for _, name := range f.list {
			status := "online"
			if f.mode[name] == "offline" {
				status = "offline"
			}
			nodes = append(nodes, map[string]any{"node": name, "status": status})
		}
		f.mu.Unlock()
		data = nodes
	case strings.HasPrefix(path, "/nodes/") && strings.HasSuffix(path, "/status"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/nodes/"), "/status")
		f.mu.Lock()
		f.reads[name]++
		mode := f.mode[name]
		f.mu.Unlock()
		switch mode {
		case "596":
			// pveproxy puts the reason in the status line and has no data for the call.
			bareStatus596(w)
			return
		case "hang":
			select {
			case <-f.stop:
			case <-r.Context().Done():
			}
			return
		}
		data = map[string]any{"cpuinfo": map[string]any{"flags": zen3Flags}}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		panic(err)
	}
}

// bareStatus596 serves the 596 line of Proxmox VE, whose reason text is the certificate's.
func bareStatus596(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = buf.WriteString("HTTP/1.1 " + certificateRefused + "\r\nContent-Length: 0\r\n\r\n")
	_ = buf.Flush()
}

func names(nodes []Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name
	}
	return out
}

func TestOnlineNodesTakeANodeThatAnswersForwardedCalls(t *testing.T) {
	t.Parallel()
	_, p := newForwardingPVE(t, map[string]string{"pve1": "ok", "pve2": "ok"})
	online, err := p.OnlineNodes(t.Context())
	if err != nil || strings.Join(names(online), ",") != "pve1,pve2" {
		t.Fatalf("OnlineNodes = %v, %v; want both nodes", names(online), err)
	}
	if got := p.Unreachable(); len(got) != 0 {
		t.Errorf("Unreachable = %v, want none", got)
	}
}

func TestOnlineNodesLeaveOutANodeThatAnswers596AndSayWhy(t *testing.T) {
	t.Parallel()
	_, p := newForwardingPVE(t, map[string]string{"pve1": "ok", "pve2": "596"})
	online, err := p.OnlineNodes(t.Context())
	if err != nil || strings.Join(names(online), ",") != "pve1" {
		t.Fatalf("OnlineNodes = %v, %v; want pve1 alone", names(online), err)
	}
	got := p.Unreachable()
	if len(got) != 1 || got[0].Node != "pve2" || !strings.HasPrefix(got[0].Reason, "HTTP 596") {
		t.Errorf("Unreachable = %+v, want pve2 with HTTP 596", got)
	}
}

func TestOnlineNodesCarryProxmoxVEsReasonTextOfThe596(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/nodes":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"node": "pve2", "status": "online"}}})
		case "/api2/json/nodes/pve2/status":
			bareStatus596(w)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		}
	}))
	t.Cleanup(srv.Close)
	hc := &http.Client{Transport: pve.CheckStatus(http.DefaultTransport)}
	p := &Proxmox{Client: proxmox.NewClient(srv.URL+"/api2/json", proxmox.WithHTTPClient(hc))}
	if online, err := p.OnlineNodes(t.Context()); err != nil || len(online) != 0 {
		t.Fatalf("OnlineNodes = %v, %v; want none", online, err)
	}
	if got := p.Unreachable(); len(got) != 1 || got[0].Reason != "HTTP 596: certificate verify failed" {
		t.Errorf("Unreachable = %+v, want the reason Proxmox VE gave", got)
	}
}

func TestOnlineNodesNeverAskAnOfflineNode(t *testing.T) {
	t.Parallel()
	f, p := newForwardingPVE(t, map[string]string{"pve1": "ok", "pve2": "offline"})
	online, err := p.OnlineNodes(t.Context())
	if err != nil || strings.Join(names(online), ",") != "pve1" {
		t.Fatalf("OnlineNodes = %v, %v; want pve1 alone", names(online), err)
	}
	if got := p.Unreachable(); len(got) != 0 || f.statusReads("pve2") != 0 {
		t.Errorf("an offline node was asked (%d reads) or called unreachable (%v)", f.statusReads("pve2"), got)
	}
}

func TestOnlineNodesFollowANodeThatBreaksAndRecovers(t *testing.T) {
	t.Parallel()
	f, p := newForwardingPVE(t, map[string]string{"pve1": "ok", "pve2": "ok"})
	read := func() (string, int) {
		t.Helper()
		online, err := p.OnlineNodes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(names(online), ","), len(p.Unreachable())
	}
	if got, faults := read(); got != "pve1,pve2" || faults != 0 {
		t.Fatalf("both answer: %q, %d faults", got, faults)
	}
	// The join race: pve2 stays online in the cluster and its certificate is lost.
	f.set("pve2", "596")
	if got, faults := read(); got != "pve1" || faults != 1 {
		t.Errorf("pve2 broke after a good read: %q, %d faults; want pve1 alone and one fault", got, faults)
	}
	f.set("pve2", "ok")
	if got, faults := read(); got != "pve1,pve2" || faults != 0 {
		t.Errorf("pve2 recovered: %q, %d faults; want both and none", got, faults)
	}
}

func TestOnlineNodesAskEachOnlineNodeOncePerRead(t *testing.T) {
	t.Parallel()
	f, p := newForwardingPVE(t, map[string]string{"pve1": "ok", "pve2": "ok", "pve3": "596"})
	if _, err := p.OnlineNodes(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"pve1", "pve2", "pve3"} {
		if got := f.statusReads(n); got != 1 {
			t.Errorf("%s was asked %d times in one read, want once", n, got)
		}
	}
}

func TestOnlineNodesGiveUpOnANodeThatHangs(t *testing.T) {
	t.Parallel()
	_, p := newForwardingPVE(t, map[string]string{"pve1": "ok", "pve2": "hang"})
	p.ProbeTimeout = 100 * time.Millisecond
	start := time.Now()
	online, err := p.OnlineNodes(t.Context())
	if err != nil || strings.Join(names(online), ",") != "pve1" {
		t.Fatalf("OnlineNodes = %v, %v; want pve1 alone", names(online), err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the read took %v, want it bounded by the probe's timeout", took)
	}
	if got := p.Unreachable(); len(got) != 1 || got[0].Node != "pve2" || got[0].Reason != "no answer in time" {
		t.Errorf("Unreachable = %+v, want pve2 with no answer in time", got)
	}
}

func TestOnlineNodesReuseAnAnswerWithinTheProbeTTL(t *testing.T) {
	t.Parallel()
	f, p := newForwardingPVE(t, map[string]string{"pve1": "ok", "pve2": "596"})
	now := time.Now()
	p.ProbeTTL, p.Now = 5*time.Second, func() time.Time { return now }
	for range 3 {
		if _, err := p.OnlineNodes(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if f.statusReads("pve1") != 1 || f.statusReads("pve2") != 1 {
		t.Errorf("three reads inside the TTL asked pve1 %d and pve2 %d times, want once each", f.statusReads("pve1"), f.statusReads("pve2"))
	}
	f.set("pve2", "ok")
	now = now.Add(6 * time.Second)
	online, err := p.OnlineNodes(t.Context())
	if err != nil || strings.Join(names(online), ",") != "pve1,pve2" {
		t.Errorf("after the TTL: %v, %v; want pve2 back", names(online), err)
	}
}

func TestOwnerNodeSkipsAnOnlineNodeThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	f, p := newForwardingPVE(t, map[string]string{"pve1": "596", "pve2": "ok"})
	got, err := p.ownerNode(t.Context())
	if err != nil || got != "pve2" {
		t.Fatalf("ownerNode = %q, %v; want pve2, since pve1 does not answer", got, err)
	}
	f.set("pve1", "ok")
	if got, err = p.ownerNode(t.Context()); err != nil || got != "pve1" {
		t.Fatalf("ownerNode = %q, %v; want pve1 once it answers", got, err)
	}
	f.set("pve1", "596")
	f.set("pve2", "596")
	if got, err = p.ownerNode(t.Context()); err == nil || !strings.Contains(err.Error(), "pve1 (HTTP 596") {
		t.Errorf("ownerNode with none answering = %q, %v; want the refusal naming both nodes", got, err)
	}
}

func TestPlacerRefusesAndPicksNothingWhenEveryNodeIsUnreachable(t *testing.T) {
	t.Parallel()
	_, p := newForwardingPVE(t, map[string]string{"pve1": "596", "pve2": "596"})
	web := cluster()
	_, c := newReconciler(t, nil, web)
	placer := &Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: p}
	placer.placeAll(t.Context())
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[OnlineNodesAnnotation]; v != "" {
		t.Errorf("placement records %q, want no node", v)
	}
	blocked := got.Annotations[PlacementBlockedAnnotation]
	if !strings.Contains(blocked, "not reachable through the cluster") || !strings.Contains(blocked, "pve1 (HTTP 596") || !strings.Contains(blocked, "pve2 (HTTP 596") {
		t.Errorf("placement-blocked = %q, want the refusal naming both nodes with their 596", blocked)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, NodesSkippedCondition)
	if cond == nil || !strings.Contains(cond.Message, "pve1 (HTTP 596") {
		t.Errorf("%s = %+v, want both nodes named", NodesSkippedCondition, cond)
	}
}

func TestPlacerSkipsAnUnreachableNodeLikeAnOfflineOne(t *testing.T) {
	t.Parallel()
	_, p := newForwardingPVE(t, map[string]string{"pve1": "ok", "pve2": "596", "pve3": "offline"})
	web := cluster()
	_, c := newReconciler(t, nil, web)
	placer := &Placer{Client: c, ClassNamespace: "kubehenge-system", Nodes: p}
	placer.placeAll(t.Context())
	got := &clusterv1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(web), got); err != nil {
		t.Fatal(err)
	}
	if v := got.Annotations[OnlineNodesAnnotation]; v != "pve1" {
		t.Errorf("placement records %q, want pve1 alone", v)
	}
	if v, set := got.Annotations[PlacementBlockedAnnotation]; set {
		t.Errorf("placement is blocked (%q) although pve1 can take a VM", v)
	}
}
