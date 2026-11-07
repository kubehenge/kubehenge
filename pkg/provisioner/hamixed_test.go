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
)

// upgradingPVE puts the node versions and Proxmox VE's refusal of rules before the HA fake.
type upgradingPVE struct {
	inner    *fakeHA
	mu       sync.Mutex
	versions map[string]string
	// refuse is the message a rule write is refused with, empty to accept it.
	refuse string
	writes int
}

func (u *upgradingPVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	u.mu.Lock()
	versions, refuse := u.versions, u.refuse
	if strings.HasPrefix(path, "/cluster/ha/rules") && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		u.writes++
	}
	u.mu.Unlock()
	reply := func(code int, body any) {
		if code == http.StatusInternalServerError {
			// Proxmox VE sends an error's message as the status line's reason.
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				panic(err)
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 500 " + refuse + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			_ = buf.Flush()
			return
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
	switch {
	case path == "/nodes":
		list := []map[string]string{}
		for n := range versions {
			list = append(list, map[string]string{"node": n, "status": "online"})
		}
		reply(http.StatusOK, map[string]any{"data": list})
	case strings.HasSuffix(path, "/version"):
		reply(http.StatusOK, map[string]any{"data": map[string]string{"version": versions[strings.Split(path, "/")[2]]}})
	case refuse != "" && strings.HasPrefix(path, "/cluster/ha/rules") && (r.Method == http.MethodPost || r.Method == http.MethodPut):
		reply(http.StatusInternalServerError, map[string]any{"message": refuse})
	default:
		u.inner.ServeHTTP(w, r)
	}
}

// upgradeFixture is the HA fixture behind upgradingPVE.
func upgradeFixture(t *testing.T, versions map[string]string) (*haFixture, *upgradingPVE) {
	t.Helper()
	f := newHAFixture(t, shared(), "pve1", "pve2", "pve3")
	u := &upgradingPVE{inner: f.pve, versions: versions}
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)
	f.r.Proxmox = proxmox.NewClient(srv.URL + "/api2/json")
	f.r.HA = &HAJobs{Jobs: &inProcessJob{t: t, serve: ServeHARequest, token: proxmox.NewClient(srv.URL + "/api2/json")}}
	return f, u
}

func (f *haFixture) pending() *clusterv1ConditionView {
	f.t.Helper()
	got := &clusterv1.Cluster{}
	if err := f.kube.Get(f.t.Context(), client.ObjectKey{Namespace: "team-a", Name: "web"}, got); err != nil {
		f.t.Fatal(err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, HARulesPendingCondition)
	if c == nil {
		return nil
	}
	return &clusterv1ConditionView{Status: string(c.Status), Reason: c.Reason, Message: c.Message}
}

type clusterv1ConditionView struct{ Status, Reason, Message string }

var allNine = map[string]string{"pve1": "9.0.3", "pve2": "9.0.3", "pve3": "9.0.3"}

func TestHARuleCreateRefusedBeforeTheGroupsMigrate(t *testing.T) {
	t.Parallel()
	f, u := upgradeFixture(t, allNine)
	u.refuse = "cannot create ha rule: ha groups have not been migrated yet"
	res := f.reconcile()
	cond := f.pending()
	if cond == nil || cond.Status != "True" || cond.Reason != haGroupsNotMigrated {
		t.Fatalf("condition %+v, want HARulesPending true with %s", cond, haGroupsNotMigrated)
	}
	if res.RequeueAfter != haRulesRetry {
		t.Errorf("requeue after %v, want %v", res.RequeueAfter, haRulesRetry)
	}
	if len(f.pve.haResources()) != 3 {
		t.Error("the HA resources are not added although the rule is refused")
	}
	u.mu.Lock()
	u.refuse = ""
	u.mu.Unlock()
	f.reconcile()
	if f.pending() != nil || f.pve.rule(haRuleID(webPool, apartRule)) == nil {
		t.Errorf("once the groups migrate the condition is %+v and the rule %v", f.pending(), f.pve.rule(haRuleID(webPool, apartRule)))
	}
}

func TestHARuleUpdateRefusedBeforeTheGroupsMigrate(t *testing.T) {
	t.Parallel()
	f, u := upgradeFixture(t, allNine)
	f.reconcile()
	apart := haRuleID(webPool, apartRule)
	f.pve.mu.Lock()
	f.pve.rules[apart].Resources = []string{"vm:101", "vm:102"}
	f.pve.mu.Unlock()
	u.mu.Lock()
	u.refuse = "cannot update ha rule: ha groups have not been migrated yet"
	u.mu.Unlock()
	f.reconcile()
	if cond := f.pending(); cond == nil || cond.Reason != haGroupsNotMigrated {
		t.Errorf("condition %+v, want %s", cond, haGroupsNotMigrated)
	}
}

func TestHAWritesNoRuleWhileTheNodesRunDifferentVersions(t *testing.T) {
	t.Parallel()
	f, u := upgradeFixture(t, map[string]string{"pve1": "8.4.1", "pve2": "9.0.3", "pve3": "9.0.3"})
	now := time.Unix(1_000, 0)
	f.r.versions.now = func() time.Time { return now }
	res := f.reconcile()
	cond := f.pending()
	if cond == nil || cond.Reason != haMixedVersions {
		t.Fatalf("condition %+v, want %s", cond, haMixedVersions)
	}
	if u.writes != 0 || f.pve.rule(haRuleID(webPool, apartRule)) != nil {
		t.Errorf("%d rule writes while versions are mixed", u.writes)
	}
	if res.RequeueAfter != haRulesRetry {
		t.Errorf("requeue after %v, want %v", res.RequeueAfter, haRulesRetry)
	}
	u.mu.Lock()
	u.versions = allNine
	u.mu.Unlock()
	now = now.Add(2 * versionFresh)
	f.reconcile()
	if f.pending() != nil {
		t.Errorf("condition %+v stays once every node runs 9", f.pending())
	}
	if f.pve.rule(haRuleID(webPool, apartRule)) == nil {
		t.Error("no rule written once every node runs 9")
	}
}

func TestHAOffersNoRulesWhileVersionsAreMixed(t *testing.T) {
	t.Parallel()
	f, u := upgradeFixture(t, map[string]string{"pve1": "8.4.1", "pve2": "9.0.3"})
	h := &HARules{Proxmox: f.r.Proxmox}
	if h.rulesOnDisk(t.Context()) {
		t.Error("rules are offered while versions are mixed")
	}
	u.mu.Lock()
	u.versions = allNine
	u.mu.Unlock()
	h = &HARules{Proxmox: f.r.Proxmox}
	if !h.rulesOnDisk(t.Context()) {
		t.Error("no rules offered with one version and no groups")
	}
}
