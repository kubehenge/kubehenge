package provisioner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

// poolServer answers /pools with one pool of the comment and guests, and the datacenter options.
func poolServer(t *testing.T, comment string, guests int, options map[string]any) *Proxmox {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/api2/json/pools":
			members := make([]map[string]any, 0, guests)
			for range guests {
				members = append(members, map[string]any{"type": "qemu", "vmid": 100, "name": "someone-elses", "node": "pve1"})
			}
			data = []map[string]any{{"comment": comment, "members": members}}
		case "/api2/json/cluster/options":
			data = options
		case "/api2/json/access/permissions":
			data = map[string]any{r.URL.Query().Get("path"): map[string]int{}}
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return &Proxmox{Client: proxmox.NewClient(srv.URL + "/api2/json")}
}

func TestAnExistingPoolWithGuestsOfOthersIsRefused(t *testing.T) {
	t.Parallel()
	pool := "kubehenge/team-a.web"
	for _, tc := range []struct {
		name, comment string
		guests        int
		refused       bool
	}{
		{"a pool someone made, with guests", "production VMs", 2, true},
		{"a pool without a comment, with guests", "", 1, true},
		{"an empty pool someone made", "mine", 0, false},
		{"a pool Kubehenge made, with guests", poolComment, 3, false},
	} {
		_, err := poolServer(t, tc.comment, tc.guests, nil).ensurePool(t.Context(), pool)
		if tc.refused && (err == nil || !strings.Contains(err.Error(), "pool "+pool+" already exists with") || !strings.Contains(err.Error(), "did not create")) {
			t.Errorf("%s: ensurePool = %v, want a refusal", tc.name, err)
		}
		if !tc.refused && err != nil {
			t.Errorf("%s: ensurePool = %v, want the pool taken", tc.name, err)
		}
	}
}

func TestANewClusterWhoseTagsTheDatacenterRefusesStopsBeforeAnyObject(t *testing.T) {
	t.Parallel()
	p := poolServer(t, poolComment, 0, map[string]any{"registered-tags": "volumes"})
	_, err := p.Ensure(t.Context(), webNames, Access{}, Credentials{}, func(Credentials) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "registered-tags lists volumes") {
		t.Errorf("Ensure = %v, want the datacenter option and the tag named", err)
	}
}
