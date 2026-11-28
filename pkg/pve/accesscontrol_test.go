package pve

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// versionsAPI answers the node list and each node's packages: versions maps a node to its
// libpve-access-control, "" to a node that lists none, and "!" to one that refuses.
func versionsAPI(t *testing.T, versions map[string]string) *Accounts {
	t.Helper()
	reply := func(w http.ResponseWriter, data any) {
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/cluster/resources", func(w http.ResponseWriter, _ *http.Request) {
		nodes := make([]map[string]any, 0, len(versions))
		for n := range versions {
			nodes = append(nodes, map[string]any{"node": n, "type": "node"})
		}
		reply(w, nodes)
	})
	mux.HandleFunc("GET /api2/json/nodes/{node}/apt/versions", func(w http.ResponseWriter, r *http.Request) {
		switch v := versions[r.PathValue("node")]; v {
		case "!":
			http.Error(w, "no Sys.Audit", http.StatusForbidden)
		case "":
			reply(w, []map[string]any{{"Package": "pve-manager", "OldVersion": "9.2.0"}})
		default:
			// The candidate is newer than what is installed, which is no node's version.
			reply(w, []map[string]any{{"Package": "libpve-access-control", "OldVersion": v, "Version": "9.9.9"}})
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAccounts(Config{URL: srv.URL, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCheckAccessControl(t *testing.T) {
	t.Parallel()
	alice := &Ticket{Username: "alice@pve", Token: "alice@pve!t=x"}
	for name, c := range map[string]struct {
		versions map[string]string
		want     string
	}{
		"all current":     {map[string]string{"a": "9.2.1", "b": "9.2.3", "c": "9.10.0"}, ""},
		"with a revision": {map[string]string{"a": "9.2.1-1"}, ""},
		"one older":       {map[string]string{"a": "9.2.3", "b": "9.2.0"}, "b has 9.2.0. Update it to 9.2.1 or newer"},
		"an older minor":  {map[string]string{"a": "9.1.9"}, "a has 9.1.9"},
		"not listed":      {map[string]string{"a": ""}, "its version on a is unreadable"},
		"refused":         {map[string]string{"a": "!"}, "its version on a is unreadable"},
	} {
		err := versionsAPI(t, c.versions).CheckAccessControl(context.Background(), alice)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (!errors.Is(err, ErrAccessControlOld) || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want too old, %q", name, err, c.want)
		}
	}
}
