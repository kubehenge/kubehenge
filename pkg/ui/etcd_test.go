package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func etcdStatused(t *testing.T, h *etcdHarness, status provisioner.EtcdStatus) {
	t.Helper()
	c := h.cluster()
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[provisioner.EtcdStatusAnnotation] = string(raw)
	if err := h.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
}

func TestDefragEtcdAsksTheController(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-defrag", ""), http.StatusAccepted)
	raw := h.cluster().Annotations[provisioner.EtcdDefragRequestAnnotation]
	var req provisioner.EtcdDefragRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil || req.By != "dev@pve" {
		t.Errorf("request %q: %v", raw, err)
	}
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("POST", "/api/clusters/default/one/etcd-defrag", ""), http.StatusForbidden)
}

func TestClusterShowsItsEtcdSizesAndWarning(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	etcdStatused(t, h, provisioner.EtcdStatus{
		At: time.Now().UTC(), QuotaBytes: 2 << 30,
		Members: []provisioner.EtcdMember{{Name: "etcd-cp-0", Leader: true, DBSize: 3 << 29, DBSizeInUse: 1 << 29}},
	})
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	var v clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Etcd == nil || v.Etcd.QuotaBytes != 2<<30 || len(v.Etcd.Members) != 1 || v.Etcd.Members[0].DBSizeInUse != 1<<29 || v.Etcd.Members[0].QuotaPercent != 75 ||
		!strings.Contains(v.Etcd.Warning, "etcd member etcd-cp-0 is at 75% of its quota") {
		t.Fatalf("etcd %+v", v.Etcd)
	}
}

func TestClusterWithoutAnEtcdReadShowsNone(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one", "")
	var v clusterView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Etcd != nil {
		t.Fatalf("etcd %+v: %v", v.Etcd, err)
	}
}

func TestHealthWarnsOfAFillingEtcdDatabase(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	for _, c := range []struct {
		name   string
		member provisioner.EtcdMember
		warns  string
	}{
		{"small", provisioner.EtcdMember{Name: "cp-0", DBSize: 100 << 20}, ""},
		{"above 60 percent", provisioner.EtcdMember{Name: "cp-0", DBSize: 7 << 29}, "etcd member cp-0 is at 88% of its quota"},
		{"alarm", provisioner.EtcdMember{Name: "cp-0", DBSize: 1 << 30, Alarms: []string{"NOSPACE"}}, "etcd member cp-0 refuses writes"},
	} {
		etcdStatused(t, h, provisioner.EtcdStatus{At: time.Now().UTC(), QuotaBytes: 4 << 30, Members: []provisioner.EtcdMember{c.member}})
		var a attention
		etcdAttention(h.cluster(), &a)
		got := a.sorted()
		if c.warns == "" {
			if len(got) != 0 {
				t.Errorf("%s: %+v", c.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Severity != severityWarning || got[0].Section != sectionControlPlane || !strings.Contains(got[0].Message, c.warns) {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

func TestEtcdQuotaChangesAfterCreation(t *testing.T) {
	t.Parallel()
	if !changeableVariables[etcdVariable] {
		t.Error("the etcd quota cannot be raised on a cluster")
	}
}
