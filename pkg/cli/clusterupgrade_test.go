package cli

import (
	"bytes"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func runUpgrade(t *testing.T, fake *fakeClusterAPI, stdin string, args ...string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	cmd := newClusterCommand(&pve.Config{Token: "dev@pve!ci=secret"}) //nolint:gosec // the fake interface's own
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"--url", srv.URL, "upgrade", "web"}, args...))
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func (f *fakeClusterAPI) patches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		if strings.HasPrefix(s, "PATCH ") {
			out = append(out, s)
		}
	}
	return out
}

// accept-upgrade posts the version, prints what was accepted and upgrades nothing.
func TestClusterAcceptUpgrade(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	cmd := newClusterCommand(&pve.Config{Token: "dev@pve!ci=secret"}) //nolint:gosec // the fake interface's own
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--url", srv.URL, "accept-upgrade", "web", "--version", "v1.35.1"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "podsecuritypolicies") || !strings.Contains(out.String(), "accepted for the automatic upgrade of web to v1.35.1") {
		t.Errorf("out %q", out.String())
	}
	if len(fake.patches()) != 0 {
		t.Errorf("sent %v", fake.patches())
	}
}

// With nothing in the way the upgrade goes ahead without a question.
func TestClusterUpgradeWithNothingInTheWay(t *testing.T) {
	t.Parallel()
	fake := &fakeClusterAPI{}
	out, err := runUpgrade(t, fake, "", "--version", "v1.34.9")
	if err != nil || strings.Contains(out, "Upgrade anyway") || !strings.Contains(out, "cluster web upgrading to v1.34.9") {
		t.Errorf("out %q, err %v", out, err)
	}
	if got := fake.patches(); len(got) != 1 || got[0] != `PATCH /api/clusters/clusters/web {"version":"v1.34.9"}` {
		t.Errorf("sent %v", got)
	}
}

// The removed APIs and webhooks print before the question, and only a yes upgrades.
func TestClusterUpgradeListsWhatItMeetsBeforeAsking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stdin string
		args        []string
		upgrades    bool
	}{
		{"yes", "y\n", nil, true},
		{"no", "n\n", nil, false},
		{"no answer", "", nil, false},
		{"--yes", "", []string{"--yes"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeClusterAPI{}
			out, err := runUpgrade(t, fake, tc.stdin, append([]string{"--version", "v1.35.1"}, tc.args...)...)
			list := "Upgrading web to v1.35.1 would run into:\n" +
				"  - Removed APIs still in use: policy/v1beta1 podsecuritypolicies (removed in 1.35).\n" +
				"  - The validating webhook guard fails closed and its Service apps/guard has 1 ready endpoint.\n"
			if !strings.HasPrefix(out, list) {
				t.Errorf("out %q does not open with the list", out)
			}
			if sent := len(fake.patches()) == 1; sent != tc.upgrades || (err == nil) != tc.upgrades {
				t.Errorf("upgraded %v, err %v, want upgrade %v", sent, err, tc.upgrades)
			}
			if strings.Contains(out, "Upgrade anyway") == (len(tc.args) > 0) {
				t.Errorf("asked: %v with %v", strings.Contains(out, "Upgrade anyway"), tc.args)
			}
		})
	}
}

func TestClusterUpgradeNeedsAVersion(t *testing.T) {
	t.Parallel()
	if _, err := runUpgrade(t, &fakeClusterAPI{}, ""); err == nil {
		t.Error("an upgrade without --version was accepted")
	}
}
