package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHARulesOfferedByNameAndNodes(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	rules := []map[string]any{
		{"rule": "fast", "type": "node-affinity", "nodes": "pve2:2,pve1", "strict": 1, "comment": "for the databases", "resources": "vm:100"},
		{"rule": "not-pve2", "type": "node-affinity", "nodes": "pve2", "affinity": "negative", "resources": "vm:101"},
		{"rule": "off", "type": "node-affinity", "nodes": "pve1", "disable": 1, "resources": "vm:102"},
		{"rule": "gone", "type": "node-affinity", "nodes": "pve9", "resources": "vm:103"},
		{"rule": "kubehenge-0123456789abcdef-nodes", "type": "node-affinity", "nodes": "pve1", "comment": HAComment("kubehenge/clusters.web"), "resources": "vm:104"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api2/json/cluster/ha/rules":
			if r.URL.Query().Get("type") != "node-affinity" {
				http.Error(w, "only node-affinity rules are asked for", http.StatusBadRequest)
				return
			}
			mu.Lock()
			data = rules
			mu.Unlock()
		case "/api2/json/nodes":
			data = []map[string]any{{"node": "pve2", "status": "online"}, {"node": "pve1", "status": "offline"}}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	h := &HARules{Reader: kube, Client: kube, Namespace: "kubehenge-system", Proxmox: proxmox.NewClient(srv.URL + "/api2/json"), Offer: true}
	ctx := context.Background()
	offered := func() string {
		t.Helper()
		cm := &corev1.ConfigMap{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: "kubehenge-system", Name: HARulesName}, cm); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseOfferedRules(cm); err != nil {
			t.Fatal(err)
		}
		return cm.Data[HARulesKey]
	}

	// Names and the nodes each allows only; neither disabled rules, nor those allowing no node, nor
	// those the controller keeps for clusters.
	if err := h.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if got := offered(); got != `[{"rule":"fast","nodes":["pve1","pve2"]},{"rule":"not-pve2","nodes":["pve1"]}]` {
		t.Fatalf("offered %s", got)
	}

	mu.Lock()
	rules = rules[1:2]
	mu.Unlock()
	if err := h.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if got := offered(); got != `[{"rule":"not-pve2","nodes":["pve1"]}]` {
		t.Fatalf("after a rule was removed, offered %s", got)
	}

	// Switched off, the controller withdraws the offer, and finds nothing to withdraw next time.
	h.Offer = false
	for range 2 {
		if err := h.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	err := kube.Get(ctx, client.ObjectKey{Namespace: "kubehenge-system", Name: HARulesName}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("still offered after switching off: %v", err)
	}
}

func TestNoHARulesOfferedWhenProxmoxVERefuses(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Permission check failed (/, Sys.Audit)", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	h := &HARules{Reader: kube, Client: kube, Namespace: "kubehenge-system", Proxmox: proxmox.NewClient(srv.URL + "/api2/json"), Offer: true}
	if err := h.publish(context.Background()); err == nil {
		t.Fatal("published although the rules could not be read")
	}
	err := kube.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: HARulesName}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("offered rules nobody read: %v", err)
	}
}
