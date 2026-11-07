package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/kubernetes/fake"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

const (
	pspGauge   = `apiserver_requested_deprecated_apis{group="policy",removed_release="1.36",resource="podsecuritypolicies",subresource="",version="v1beta1"} 1` + "\n"
	pspCounter = `apiserver_request_total{code="200",component="apiserver",group="policy",resource="podsecuritypolicies",scope="cluster",subresource="",verb="LIST",version="v1beta1"} 5` + "\n"
	oldAPI     = "policy/v1beta1 podsecuritypolicies"
)

// countingChecker reads the metrics it is given as the health identity does, with the cluster's
// stored history.
type countingChecker struct {
	metrics string
	now     func() time.Time
}

func (k *countingChecker) Check(ctx context.Context, c *clusterv1.Cluster, target string) UpgradeFindings {
	f := CheckUpgrade(ctx, fake.NewClientset(), metricsOf(k.metrics), target)
	f.ApplyHistory(HistoryOf(c), k.now())
	return f
}

// An API counts as in use while its request count moved in the last 7 days. A request counted
// since the last look, a first look and a lack of request counts all count as use.
func TestRemovedAPIsCountOnlyWhileRequested(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		metrics string
		history APIHistory
		active  bool
		basis   string
	}{
		{"requested a day ago", pspGauge + pspCounter, APIHistory{oldAPI: {Requests: 5, Seen: now.Add(-24 * time.Hour)}}, true, BasisRequests},
		{"last requested eight days ago", pspGauge + pspCounter, APIHistory{oldAPI: {Requests: 5, Seen: now.Add(-8 * 24 * time.Hour)}}, false, BasisRequests},
		{"requested again since", pspGauge + pspCounter, APIHistory{oldAPI: {Requests: 3, Seen: now.Add(-8 * 24 * time.Hour)}}, true, BasisRequests},
		{"met for the first time", pspGauge + pspCounter, APIHistory{}, true, BasisRequests},
		{"no request counts", pspGauge, APIHistory{oldAPI: {Requests: 5, Seen: now.Add(-8 * 24 * time.Hour)}}, true, BasisGauge},
	} {
		f := CheckUpgrade(context.Background(), fake.NewClientset(), metricsOf(tc.metrics), "v1.36.5")
		f.ApplyHistory(tc.history, now)
		if got := len(f.RemovedAPIs) == 1; got != tc.active || f.Basis != tc.basis {
			t.Errorf("%s: %+v, basis %q; want active %v, basis %q", tc.name, f.RemovedAPIs, f.Basis, tc.active, tc.basis)
		}
	}
}

func upgradeAfter(t *testing.T, metrics string, annotations map[string]string) (*clusterv1.Cluster, string) {
	t.Helper()
	c := settledCluster("web", "v1.35.4+rke2r1", map[string]string{"upgrades": `{"mode":"minor"}`})
	c.Annotations = annotations
	u, kube, _ := upgraderFixture(t, c)
	u.Checker = &countingChecker{metrics: metrics, now: u.Now}
	u.RunOnce(context.Background())
	got := getCluster(t, kube, "web")
	cond := meta.FindStatusCondition(got.Status.Conditions, AutoUpgradeSkippedCondition)
	if cond == nil {
		return got, ""
	}
	return got, cond.Message
}

func historyAnnotation(requests int64, ago time.Duration) map[string]string {
	seen := time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC).Add(-ago)
	return map[string]string{DeprecatedUseAnnotation: APIHistory{oldAPI: {Requests: requests, Seen: seen}}.Encode()}
}

// The automatic upgrade skips for an API requested in the last week and goes ahead for one not
// requested since, which is how a stale gauge no longer holds an upgrade that would clear it.
func TestAutomaticUpgradeSkipsOnlyForRecentRequests(t *testing.T) {
	t.Parallel()
	got, note := upgradeAfter(t, pspGauge+pspCounter, historyAnnotation(5, 8*24*time.Hour))
	if got.Spec.Topology.Version != "v1.36.5+rke2r1" || note != "" {
		t.Errorf("a use eight days old: version %s, note %q", got.Spec.Topology.Version, note)
	}
	got, note = upgradeAfter(t, pspGauge+pspCounter, historyAnnotation(5, 24*time.Hour))
	if got.Spec.Topology.Version != "v1.35.4+rke2r1" || !strings.Contains(note, oldAPI) || !strings.Contains(note, "last requested 2026-10-12") {
		t.Errorf("a recent use: version %s, note %q", got.Spec.Topology.Version, note)
	}
	got, note = upgradeAfter(t, pspGauge+pspCounter, historyAnnotation(2, 8*24*time.Hour))
	if got.Spec.Topology.Version != "v1.35.4+rke2r1" || note == "" {
		t.Errorf("a request counted since: version %s, note %q", got.Spec.Topology.Version, note)
	}
}

// Without request counts the gauge decides, and the note says it holds every use since the API
// server started.
func TestAutomaticUpgradeFallsBackToTheGauge(t *testing.T) {
	t.Parallel()
	got, note := upgradeAfter(t, pspGauge, historyAnnotation(5, 8*24*time.Hour))
	if got.Spec.Topology.Version != "v1.35.4+rke2r1" || !strings.Contains(note, "keeps no request counts") {
		t.Errorf("version %s, note %q", got.Spec.Topology.Version, note)
	}
}

// What a person accepted for a minor version lets the automatic upgrade to it go ahead, and an API
// in use that was not on the list holds it again.
func TestAcknowledgedFindingsEndTheSkip(t *testing.T) {
	t.Parallel()
	c := settledCluster("web", "v1.35.4+rke2r1", nil)
	f := CheckUpgrade(context.Background(), fake.NewClientset(), metricsOf(pspGauge+pspCounter), "v1.36.5")
	value, err := Acknowledgement(c, "v1.36.5+rke2r1", &f)
	if err != nil || value != `{"1.36":["`+oldAPI+`"]}` {
		t.Fatalf("%q, %v", value, err)
	}
	annotations := historyAnnotation(5, 24*time.Hour)
	annotations[AcknowledgedAnnotation] = value
	got, note := upgradeAfter(t, pspGauge+pspCounter, annotations)
	if got.Spec.Topology.Version != "v1.36.5+rke2r1" || note != "" {
		t.Errorf("accepted: version %s, note %q", got.Spec.Topology.Version, note)
	}

	other := `apiserver_requested_deprecated_apis{group="flowcontrol.apiserver.k8s.io",removed_release="1.32",resource="flowschemas",subresource="",version="v1beta3"} 1` + "\n"
	got, note = upgradeAfter(t, pspGauge+pspCounter+other, annotations)
	if got.Spec.Topology.Version != "v1.35.4+rke2r1" || !strings.Contains(note, "flowschemas") || strings.Contains(note, "podsecuritypolicies") {
		t.Errorf("a new API: version %s, note %q", got.Spec.Topology.Version, note)
	}

	annotations[AcknowledgedAnnotation] = `{"1.37":["` + oldAPI + `"]}`
	if got, _ = upgradeAfter(t, pspGauge+pspCounter, annotations); got.Spec.Topology.Version != "v1.35.4+rke2r1" {
		t.Errorf("accepted for another minor: version %s", got.Spec.Topology.Version)
	}
}

// The controller keeps the readings on the cluster, so that the next look can tell what moved.
func TestControllerKeepsTheRequestCounts(t *testing.T) {
	t.Parallel()
	got, _ := upgradeAfter(t, pspGauge+pspCounter, nil)
	h := HistoryOf(got)
	if h[oldAPI].Requests != 5 || h[oldAPI].Seen.IsZero() {
		t.Errorf("history %v from %q", h, got.Annotations[DeprecatedUseAnnotation])
	}
}
