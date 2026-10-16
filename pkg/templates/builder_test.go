package templates

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

const testPool = "kubehenge-templates"

// clusterVMs serves the Proxmox VE cluster's VM list, the only API the template lookups read.
func clusterVMs(t *testing.T, vms ...map[string]any) *proxmox.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch strings.TrimPrefix(r.URL.Path, "/api2/json") {
		case "/cluster/status":
			data = []any{}
		case "/cluster/resources":
			data = vms
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return proxmox.NewClient(srv.URL + "/api2/json")
}

func template(vmid int, name, tags, pool string) map[string]any {
	vm := map[string]any{"vmid": vmid, "node": "pve1", "name": name, "tags": tags, "template": 1, "type": "qemu"}
	if pool != "" {
		vm["pool"] = pool
	}
	return vm
}

func TestFindLooksOnlyInTheBuildersPool(t *testing.T) {
	t.Parallel()
	tmpl, err := Plan(Request{Distribution: RKE2})
	if err != nil {
		t.Fatal(err)
	}
	tags := strings.Join(tmpl.Tags, ";")
	elsewhere := template(101, tmpl.Name, tags, "")
	inPool := template(201, tmpl.Name, tags, testPool)
	other := template(202, "kubehenge-talos", "kubehenge;talos;talos-v1.14.4", testPool)
	ctx := t.Context()

	b := &Builder{Client: clusterVMs(t, elsewhere, other), Pool: testPool}
	if _, err = b.Find(ctx, &tmpl); !errors.Is(err, ErrNoTemplate) {
		t.Errorf("with the template only outside the pool, Find gave %v, want ErrNoTemplate", err)
	}
	if list, listErr := b.List(ctx); listErr != nil || len(list) != 1 || list[0].VMID != 202 {
		t.Errorf("List gave %v, %v; want the pool's template 202 alone", list, listErr)
	}

	b = &Builder{Client: clusterVMs(t, elsewhere, inPool, other), Pool: testPool}
	found, err := b.Find(ctx, &tmpl)
	if err != nil || found.VMID != 201 {
		t.Errorf("Find gave %v, %v; want the pool's 201", found, err)
	}

	b = &Builder{Client: clusterVMs(t, elsewhere, inPool, other)}
	if _, err := b.Find(ctx, &tmpl); err == nil || !strings.Contains(err.Error(), "2 templates carry") {
		t.Errorf("without a pool, Find gave %v, want both templates refused as ambiguous", err)
	}
	if list, err := b.List(ctx); err != nil || len(list) != 3 {
		t.Errorf("without a pool, List gave %d templates, %v; want all 3", len(list), err)
	}
}
