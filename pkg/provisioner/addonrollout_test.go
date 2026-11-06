package provisioner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const closedWindowPolicy = `{"mode":"patch","window":{"start":"02:00","durationMinutes":240,"timeZone":"UTC"}}`

var (
	// Tuesday 13 October 2026: 03:00 lies in the window of closedWindowPolicy, 12:00 outside it.
	insideWindow  = time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)
	outsideWindow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
)

func variableJSON(name, raw string) clusterv1.ClusterVariable {
	return clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: []byte(raw)}}
}

// rollout is a reconciler over a fake management cluster, with one cluster in it.
type rollout struct {
	t       *testing.T
	r       *Reconciler
	kube    client.Client
	cluster *clusterv1.Cluster
	addons  *Addons
	now     time.Time
}

func newRollout(t *testing.T, now time.Time, variables ...clusterv1.ClusterVariable) *rollout {
	t.Helper()
	c := cluster(variables...)
	// Traefik and cert-manager wait for the Gateway API, which the cluster has.
	c.Annotations = map[string]string{gatewayAPIAnnotation: "true"}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(c).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	w := &rollout{t: t, kube: kube, cluster: c, now: now, addons: &Addons{Headlamp: Headlamp{Enabled: true}}}
	w.r = &Reconciler{Client: kube, Charts: testCharts, Now: func() time.Time { return w.now }}
	return w
}

// apply is one reconcile of the cluster's proxies. It returns what it held back.
func (w *rollout) apply() []AddonChange {
	w.t.Helper()
	if err := w.kube.Get(context.Background(), client.ObjectKeyFromObject(w.cluster), w.cluster); err != nil {
		w.t.Fatal(err)
	}
	proxies, err := helmChartProxies(w.cluster, &w.r.Charts, "rke2", "local-lvm", CNI{}, w.addons)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err = w.r.applyProxies(context.Background(), w.cluster, proxies, w.addons); err != nil {
		w.t.Fatal(err)
	}
	return w.addons.pending
}

func (w *rollout) version(suffix string) string {
	w.t.Helper()
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	if err := w.kube.Get(context.Background(), client.ObjectKey{Namespace: w.cluster.Namespace, Name: w.cluster.Name + "-" + suffix}, p); err != nil {
		w.t.Fatal(err)
	}
	version, _, _ := strings.Cut(specText(p.Object["spec"], "version"), "@")
	return version
}

// newRelease is a controller release that pins another Headlamp chart.
func (w *rollout) newRelease() {
	w.r.Charts.Headlamp.Version = "9.9.9"
}

func TestAddonChangeWaitsForTheClosedWindow(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow, variableJSON("upgrades", closedWindowPolicy))
	was := testCharts.Headlamp.Version
	if held := w.apply(); len(held) != 0 || w.version("headlamp") != was {
		t.Fatalf("a new cluster holds %v at %s", held, w.version("headlamp"))
	}
	w.newRelease()
	held := w.apply()
	want := []AddonChange{{Addon: "headlamp", Chart: testCharts.Headlamp.Name, From: was, To: "9.9.9"}}
	if diff := cmp.Diff(want, held); diff != "" {
		t.Errorf("held changes (-want +got):\n%s", diff)
	}
	if got := w.version("headlamp"); got != was {
		t.Errorf("headlamp moved to %s inside a closed window", got)
	}
	w.apply()
	if got := w.version("headlamp"); got != was {
		t.Errorf("headlamp moved to %s on the next look", got)
	}
}

func TestAddonChangeAppliesWhenTheWindowOpens(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow, variableJSON("upgrades", closedWindowPolicy))
	w.apply()
	w.newRelease()
	w.apply()
	w.now = insideWindow
	if held := w.apply(); len(held) != 0 || w.version("headlamp") != "9.9.9" {
		t.Errorf("inside the window: held %v, headlamp at %s", held, w.version("headlamp"))
	}
}

func TestAddonChangeAppliesAtOnceWithoutAWindow(t *testing.T) {
	t.Parallel()
	for name, variables := range map[string][]clusterv1.ClusterVariable{
		"no upgrades variable":            nil,
		"upgrades off and no node images": {variableJSON("upgrades", `{"mode":"off","nodeImages":false}`)},
	} {
		w := newRollout(t, outsideWindow, variables...)
		w.apply()
		w.newRelease()
		if held := w.apply(); len(held) != 0 || w.version("headlamp") != "9.9.9" {
			t.Errorf("%s: held %v, headlamp at %s", name, held, w.version("headlamp"))
		}
	}
}

func TestAddonChangeAppliesNowOnRequest(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow, variableJSON("upgrades", closedWindowPolicy))
	w.apply()
	w.newRelease()
	w.apply()
	if err := w.r.recordAddonChanges(context.Background(), w.cluster, w.addons); err != nil {
		t.Fatal(err)
	}
	waits := &clusterv1.Cluster{}
	if err := w.kube.Get(context.Background(), client.ObjectKeyFromObject(w.cluster), waits); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(waits.Annotations[AddonChangesAnnotation], `"to":"9.9.9"`) {
		t.Fatalf("the waiting change is not recorded: %v", waits.Annotations)
	}
	cond := meta.FindStatusCondition(waits.Status.Conditions, AddonChangesPendingCondition)
	if cond == nil || cond.Reason != AddonChangesWaiting || !strings.Contains(cond.Message, "headlamp") {
		t.Fatalf("condition %+v", cond)
	}

	waits.Annotations[ApplyAddonChangesAnnotation] = "now"
	if err := w.kube.Update(context.Background(), waits); err != nil {
		t.Fatal(err)
	}
	if held := w.apply(); len(held) != 0 || w.version("headlamp") != "9.9.9" {
		t.Fatalf("on request: held %v, headlamp at %s", held, w.version("headlamp"))
	}
	if err := w.r.recordAddonChanges(context.Background(), w.cluster, w.addons); err != nil {
		t.Fatal(err)
	}
	done := &clusterv1.Cluster{}
	if err := w.kube.Get(context.Background(), client.ObjectKeyFromObject(w.cluster), done); err != nil {
		t.Fatal(err)
	}
	if _, has := done.Annotations[AddonChangesAnnotation]; has {
		t.Errorf("the change still waits: %v", done.Annotations)
	}
	if _, has := done.Annotations[ApplyAddonChangesAnnotation]; has {
		t.Errorf("the request stays: %v", done.Annotations)
	}
	if meta.FindStatusCondition(done.Status.Conditions, AddonChangesPendingCondition) != nil {
		t.Error("the condition stays once nothing waits")
	}
}

func TestAddonChangeFromTheClustersOwnSettingsAppliesAtOnce(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow, variableJSON("upgrades", closedWindowPolicy))
	w.apply()
	w.newRelease()
	w.cluster.Spec.Topology.Variables = append(w.cluster.Spec.Topology.Variables, variableJSON("cis", "true"))
	if err := w.kube.Update(context.Background(), w.cluster); err != nil {
		t.Fatal(err)
	}
	if held := w.apply(); len(held) != 0 || w.version("headlamp") != "9.9.9" {
		t.Errorf("held %v, headlamp at %s", held, w.version("headlamp"))
	}
}

func TestAddonSwitchedOnInsideAClosedWindowInstallsAtOnce(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow, variableJSON("upgrades", closedWindowPolicy))
	w.apply()
	w.addons.Monitoring.Enabled = true
	if held := w.apply(); len(held) != 0 {
		t.Fatalf("held %v", held)
	}
	if got := w.version(monitoringAddon); got != testCharts.Monitoring.Version {
		t.Errorf("monitoring at %q", got)
	}
}

func TestAddonChangeStaysWhileTheNextWindowIsAway(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow, variableJSON("upgrades", closedWindowPolicy))
	w.apply()
	w.newRelease()
	w.apply()
	until, sooner := w.addons.untilWindow(w.now)
	if want := 14*time.Hour + time.Second; sooner || until != want {
		t.Errorf("until %v, want %v", until, want)
	}
	w.now = time.Date(2026, 10, 14, 1, 59, 0, 0, time.UTC)
	if until, sooner = w.addons.untilWindow(w.now); !sooner || until != time.Minute+time.Second {
		t.Errorf("until %v, sooner %v", until, sooner)
	}
}

func TestCoversIgnoresWhatAServerAdds(t *testing.T) {
	t.Parallel()
	stored := map[string]any{"version": "1", "options": map[string]any{"install": map[string]any{"createNamespace": true, "wait": false}}, "extra": "x"}
	same := map[string]any{"version": "1", "options": map[string]any{"install": map[string]any{"createNamespace": true}}}
	other := map[string]any{"version": "2", "options": map[string]any{"install": map[string]any{"createNamespace": true}}}
	if !covers(stored, same) || covers(stored, other) {
		t.Error("covers reads a server's defaults as a change, or a new version as none")
	}
	raw, err := json.Marshal(AddonChangesStatus{Changes: []AddonChange{{Addon: "a", Chart: "c", From: "1", To: "1"}}})
	if err != nil || !strings.Contains(string(raw), `"from":"1"`) {
		t.Errorf("%s, %v", raw, err)
	}
}
