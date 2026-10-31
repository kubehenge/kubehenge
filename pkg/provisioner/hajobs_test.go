package provisioner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthermonson/go-proxmox"
)

// The HA token is a shell on every node: its Job writes Kubehenge's own HA resources and rules and
// nothing else, whatever the controller asks.
func TestServeHARequestWritesOnlyKubehengesHA(t *testing.T) {
	t.Parallel()
	ours, theirs := HAComment(webPool), "someone else's"
	apart := haRuleID(webPool, apartRule)
	for _, tc := range []struct {
		name string
		req  HARequest
		want string
	}{
		{"a resource of Kubehenge's", HARequest{http.MethodPost, "/cluster/ha/resources", map[string]any{"sid": "vm:103", "state": "started", "comment": ours}}, ""},
		{"a resource without Kubehenge's comment", HARequest{http.MethodPost, "/cluster/ha/resources", map[string]any{"sid": "vm:103", "state": "started", "comment": theirs}}, "not one of Kubehenge's"},
		{"a container", HARequest{http.MethodPost, "/cluster/ha/resources", map[string]any{"sid": "ct:103", "state": "started", "comment": ours}}, "not one of Kubehenge's"},
		{"a resource with more settings", HARequest{http.MethodPost, "/cluster/ha/resources", map[string]any{"sid": "vm:103", "state": "started", "comment": ours, "group": "x"}}, "not one of Kubehenge's"},
		{"removing Kubehenge's resource", HARequest{Method: http.MethodDelete, Path: "/cluster/ha/resources/vm:101"}, ""},
		{"removing someone else's resource", HARequest{Method: http.MethodDelete, Path: "/cluster/ha/resources/vm:900"}, "not one of Kubehenge's"},
		{"removing a resource that is gone", HARequest{Method: http.MethodDelete, Path: "/cluster/ha/resources/vm:104"}, "no such resource"},
		{"a rule of Kubehenge's", HARequest{http.MethodPost, "/cluster/ha/rules", map[string]any{
			"rule": haRuleID(webPool, nodesRule), "comment": ours, "type": "node-affinity", "resources": "vm:101", "nodes": "pve1,pve2", "strict": 1,
		}}, ""},
		{"a rule over someone else's VM", HARequest{http.MethodPost, "/cluster/ha/rules", map[string]any{
			"rule": haRuleID(webPool, nodesRule), "comment": ours, "type": "node-affinity", "resources": "vm:101,vm:900", "nodes": "pve1", "strict": 1,
		}}, "resource vm:900"},
		{"a rule of another name", HARequest{http.MethodPost, "/cluster/ha/rules", map[string]any{
			"rule": "fast", "comment": ours, "type": "node-affinity", "resources": "vm:101", "nodes": "pve1", "strict": 1,
		}}, "not one of Kubehenge's"},
		{"changing Kubehenge's rule", HARequest{http.MethodPut, "/cluster/ha/rules/" + apart, map[string]any{"type": "resource-affinity", "resources": "vm:101,vm:102"}}, ""},
		{"changing someone else's rule", HARequest{http.MethodPut, "/cluster/ha/rules/" + haRuleID("other", apartRule), map[string]any{"type": "resource-affinity", "resources": "vm:101"}}, "not one of Kubehenge's"},
		{"changing a rule's comment", HARequest{http.MethodPut, "/cluster/ha/rules/" + apart, map[string]any{"type": "resource-affinity", "resources": "vm:101", "comment": theirs}}, "not one of Kubehenge's"},
		{"removing Kubehenge's rule", HARequest{Method: http.MethodDelete, Path: "/cluster/ha/rules/" + apart}, ""},
		{"a shell on a node", HARequest{http.MethodPost, "/nodes/pve1/termproxy", map[string]any{}}, "not one of Kubehenge's"},
		{"reading something", HARequest{Method: http.MethodGet, Path: "/cluster/ha/resources"}, "not one of Kubehenge's"},
		{"the HA manager's settings", HARequest{http.MethodPut, "/cluster/options", map[string]any{"ha": "shutdown_policy=migrate"}}, "not one of Kubehenge's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pve := newFakeHA("pve1", "pve2", "pve3")
			pve.resources["vm:101"], pve.resources["vm:102"], pve.resources["vm:900"] = ours, ours, theirs
			pve.rules[apart] = &fakeRule{Type: "resource-affinity", Affinity: "negative", Comment: ours, Resources: []string{"vm:101", "vm:102"}}
			pve.rules[haRuleID("other", apartRule)] = &fakeRule{Type: "resource-affinity", Affinity: "negative", Comment: theirs, Resources: []string{"vm:900", "vm:101"}}
			srv := httptest.NewServer(pve)
			t.Cleanup(srv.Close)
			raw, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ServeHARequest(t.Context(), proxmox.NewClient(srv.URL+"/api2/json"), raw)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}
