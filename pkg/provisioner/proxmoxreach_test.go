package provisioner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestProxmoxReachRecordsWhetherProxmoxVEAnswers(t *testing.T) {
	t.Parallel()
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() || r.URL.Path != "/api2/json/version" {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"version": "9.0.3"}})
	}))
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	noon := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	r := &ProxmoxReach{Reader: kube, Client: kube, Namespace: "kubehenge-system", Proxmox: proxmox.NewClient(srv.URL + "/api2/json"), Interval: time.Minute, Now: func() time.Time { return noon }}
	ctx := context.Background()
	recorded := func() Reachability {
		t.Helper()
		cm := &corev1.ConfigMap{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: "kubehenge-system", Name: ProxmoxReachName}, cm); err != nil {
			t.Fatal(err)
		}
		got, err := ParseReachability(cm)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if err := r.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recorded(); !got.OK || got.Error != "" || !got.Checked.Equal(noon) {
		t.Fatalf("answering API recorded as %+v", got)
	}

	down.Store(true)
	if err := r.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recorded(); got.OK || got.Error == "" {
		t.Fatalf("failing API recorded as %+v, want the error", got)
	}
	// A name nothing resolves is the case the check exists for.
	srv.Close()
	if err := r.publish(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recorded(); got.OK || !strings.Contains(got.Error, "/api2/json/version") {
		t.Fatalf("unreachable API recorded as %+v, want the error naming the address", got)
	}
}
