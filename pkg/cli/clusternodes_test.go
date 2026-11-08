package cli

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPoolSetSendsLabelsAndTaints(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

	for _, args := range [][]string{
		{"pool", "set", "web", "md-0", "--label", "team=a", "--label", "tier=", "--taint", "dedicated=db:NoSchedule", "--taint", "spot:PreferNoSchedule"},
		{"pool", "set", "web", "md-1", "--inherit"},
		{"pool", "set", "web", "md-2", "--label", "team=b", "--workers", "2"},
	} {
		if out, err := runCluster(t, srv.URL, token, args...); err != nil || out != "cluster web changed\n" {
			t.Fatalf("%v = %q, %v", args, out, err)
		}
	}
	fake.mu.Lock()
	sent := fake.sent
	fake.mu.Unlock()
	want := []string{
		`PATCH /api/clusters/clusters/web {"pools":{"md-0":{"nodes":{"labels":[{"key":"team","value":"a"},{"key":"tier","value":""}],"taints":[{"effect":"NoSchedule","key":"dedicated","value":"db"},{"effect":"PreferNoSchedule","key":"spot"}]}}}}`,
		`PATCH /api/clusters/clusters/web {"pools":{"md-1":{"nodes":null}}}`,
		`PATCH /api/clusters/clusters/web {"pools":{"md-2":{"nodes":{"labels":[{"key":"team","value":"b"}],"taints":[]},"replicas":2}}}`,
	}
	if strings.Join(sent, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent\n%s\nwant\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

func TestPoolSetRefusesWhatTheServerWould(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(&fakeClusterAPI{})
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"pool", "set", "web", "md-0"}, "give --label, --taint, --inherit, --workers or a mix"},
		{[]string{"pool", "set", "web", "md-0", "--label", "team"}, `--label "team": give KEY=VALUE`},
		{[]string{"pool", "set", "web", "md-0", "--label", "=a"}, `--label "=a": give KEY=VALUE`},
		{[]string{"pool", "set", "web", "md-0", "--taint", "dedicated=db"}, `--taint "dedicated=db": give KEY=VALUE:EFFECT`},
		{[]string{"pool", "set", "web", "md-0", "--taint", "dedicated=db:Evict"}, "with the effect NoSchedule, PreferNoSchedule, NoExecute"},
		{[]string{"pool", "set", "web", "md-0", "--taint", "=db:NoSchedule"}, `--taint "=db:NoSchedule"`},
		{[]string{"pool", "set", "web", "md-0", "--inherit", "--label", "a=b"}, "--inherit takes the cluster's labels and taints"},
	} {
		if _, err := runCluster(t, srv.URL, token, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
}

func TestClusterUpdateSwitchesCISOn(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	const token = "dev@pve!ci=secret" //nolint:gosec // the fake interface's own

	if _, err := runCluster(t, srv.URL, token, "update", "web"); err == nil || !strings.Contains(err.Error(), "give --cis") {
		t.Errorf("update without a setting: %v", err)
	}
	if out, err := runCluster(t, srv.URL, token, "update", "web", "--cis"); err != nil || out != "cluster web changed\n" {
		t.Fatalf("update --cis = %q, %v", out, err)
	}
	fake.mu.Lock()
	sent := fake.sent
	fake.mu.Unlock()
	var body struct {
		Variables map[string]bool `json:"variables"`
	}
	_, raw, _ := strings.Cut(strings.Join(sent, "\n"), "PATCH /api/clusters/clusters/web ")
	if err := json.Unmarshal([]byte(raw), &body); err != nil || !body.Variables["cis"] {
		t.Errorf("sent %q", sent)
	}
}
