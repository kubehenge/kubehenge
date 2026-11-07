package provisioner

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const requestedDeprecated = `# HELP apiserver_requested_deprecated_apis Gauge of deprecated APIs that have been requested.
# TYPE apiserver_requested_deprecated_apis gauge
apiserver_requested_deprecated_apis{group="policy",removed_release="1.25",resource="podsecuritypolicies",subresource="",version="v1beta1"} 1
apiserver_requested_deprecated_apis{group="flowcontrol.apiserver.k8s.io",removed_release="1.32",resource="flowschemas",subresource="",version="v1beta3"} 1
apiserver_requested_deprecated_apis{group="",removed_release="1.33",resource="endpoints",subresource="",version="v1"} 1
apiserver_requested_deprecated_apis{group="",removed_release="1.33",resource="pods",subresource="status",version="v1"} 0
apiserver_requested_deprecated_apis{group="example.org",removed_release="1.40",resource="widgets",subresource="",version="v1alpha1"} 1
apiserver_requested_deprecated_apis{group="example.org",removed_release="",resource="gadgets",subresource="",version="v1alpha1"} 1
apiserver_request_total{code="200",group="",verb="GET"} 12
`

// An API in use is in the way when the target is the release that removes it or a later one, and
// not before. One whose gauge is 0 or that names no removal is not in the way.
func TestParseRemovedAPIsByTarget(t *testing.T) {
	t.Parallel()
	const (
		psp     = "policy/v1beta1 podsecuritypolicies"
		flow    = "flowcontrol.apiserver.k8s.io/v1beta3 flowschemas"
		core    = "v1 endpoints"
		widgets = "example.org/v1alpha1 widgets"
	)
	for _, tc := range []struct {
		target int
		want   []string
	}{
		{24, nil},
		{25, []string{psp}},
		{31, []string{psp}},
		{32, []string{psp, flow}},
		{33, []string{psp, flow, core}},
		{39, []string{psp, flow, core}},
		{40, []string{psp, flow, core, widgets}},
	} {
		got, err := ParseRemovedAPIs(strings.NewReader(requestedDeprecated), tc.target)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(got))
		for i, a := range got {
			names[i] = a.API
		}
		if !slices.Equal(names, tc.want) {
			t.Errorf("target 1.%d: %v, want %v", tc.target, names, tc.want)
		}
	}
	got, _ := ParseRemovedAPIs(strings.NewReader(requestedDeprecated), 33)
	if got[1].RemovedIn != "1.32" {
		t.Errorf("removed in %q", got[1].RemovedIn)
	}
}

func metricsOf(text string) MetricsSource {
	return func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(text)), nil }
}

func slice(namespace, service string, endpoints ...discoveryv1.Endpoint) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		Namespace: namespace, Name: service + "-1", Labels: map[string]string{discoveryv1.LabelServiceName: service},
		Endpoints: endpoints,
	}
}

func onNode(node string, ready bool) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{NodeName: new(node), Conditions: discoveryv1.EndpointConditions{Ready: new(ready)}}
}

func validating(name, namespace, service string, policy *admissionregistrationv1.FailurePolicyType) *admissionregistrationv1.ValidatingWebhookConfiguration {
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		Name: name,
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: name + ".example.org", FailurePolicy: policy,
			ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: namespace, Name: service}},
		}},
	}
}

func mutating(name, namespace, service string, policy *admissionregistrationv1.FailurePolicyType) *admissionregistrationv1.MutatingWebhookConfiguration {
	return &admissionregistrationv1.MutatingWebhookConfiguration{
		Name: name,
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name: name + ".example.org", FailurePolicy: policy,
			ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: namespace, Name: service}},
		}},
	}
}

// A webhook that fails closed is at risk when fewer than two endpoints are ready, or when all of
// them run on one node. Two ready endpoints on two nodes are fine, and so are the webhooks that
// fail open, Kubehenge's own add-ons' and those that call a URL.
func TestCheckUpgradeFindsThinFailClosedWebhooks(t *testing.T) {
	t.Parallel()
	fail, ignore := new(admissionregistrationv1.Fail), new(admissionregistrationv1.Ignore)
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
		want    []RiskyWebhook
	}{
		{"one endpoint", []runtime.Object{
			validating("guard", "apps", "guard", fail), slice("apps", "guard", onNode("a", true)),
		}, []RiskyWebhook{{Kind: "validating", Name: "guard", Service: "apps/guard", Why: "has 1 ready endpoint"}}},
		{
			"no endpoint",
			[]runtime.Object{validating("guard", "apps", "guard", fail)},
			[]RiskyWebhook{{Kind: "validating", Name: "guard", Service: "apps/guard", Why: "has 0 ready endpoints"}},
		},
		{"one of two ready", []runtime.Object{
			validating("guard", "apps", "guard", fail), slice("apps", "guard", onNode("a", true), onNode("b", false)),
		}, []RiskyWebhook{{Kind: "validating", Name: "guard", Service: "apps/guard", Why: "has 1 ready endpoint"}}},
		{"two on one node", []runtime.Object{
			validating("guard", "apps", "guard", fail), slice("apps", "guard", onNode("a", true), onNode("a", true)),
		}, []RiskyWebhook{{Kind: "validating", Name: "guard", Service: "apps/guard", Why: "has all 2 endpoints on one node"}}},
		{"two on two nodes", []runtime.Object{
			validating("guard", "apps", "guard", fail), slice("apps", "guard", onNode("a", true), onNode("b", true)),
		}, nil},
		{"the default policy fails closed", []runtime.Object{
			validating("guard", "apps", "guard", nil), slice("apps", "guard", onNode("a", true)),
		}, []RiskyWebhook{{Kind: "validating", Name: "guard", Service: "apps/guard", Why: "has 1 ready endpoint"}}},
		{"a mutating webhook", []runtime.Object{
			mutating("inject", "apps", "inject", fail), slice("apps", "inject", onNode("a", true)),
		}, []RiskyWebhook{{Kind: "mutating", Name: "inject", Service: "apps/inject", Why: "has 1 ready endpoint"}}},
		{"fails open", []runtime.Object{
			validating("guard", "apps", "guard", ignore), slice("apps", "guard", onNode("a", true)),
		}, nil},
		{"Kubehenge's own", []runtime.Object{
			validating("kyverno-resource-validating-webhook-cfg", "kyverno", "kyverno-svc", fail), slice("kyverno", "kyverno-svc", onNode("a", true)),
			mutating("cert-manager-webhook", "cert-manager", "cert-manager-webhook", fail),
		}, nil},
		{"a URL", []runtime.Object{&admissionregistrationv1.ValidatingWebhookConfiguration{
			Name: "external",
			Webhooks: []admissionregistrationv1.ValidatingWebhook{{
				Name: "external.example.org", ClientConfig: admissionregistrationv1.WebhookClientConfig{URL: new("https://hooks.example.org/validate")},
			}},
		}}, nil},
		{"another Service's endpoints", []runtime.Object{
			validating("guard", "apps", "guard", fail), slice("apps", "other", onNode("a", true), onNode("b", true)),
		}, []RiskyWebhook{{Kind: "validating", Name: "guard", Service: "apps/guard", Why: "has 0 ready endpoints"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CheckUpgrade(context.Background(), fake.NewClientset(tc.objects...), metricsOf(""), "v1.33.1")
			if !slices.Equal(got.Webhooks, tc.want) {
				t.Errorf("webhooks %+v, want %+v", got.Webhooks, tc.want)
			}
			if len(got.Unchecked) != 0 || len(got.RemovedAPIs) != 0 {
				t.Errorf("unchecked %v, removed %v", got.Unchecked, got.RemovedAPIs)
			}
			if got.Blocks() != (len(tc.want) > 0) {
				t.Errorf("Blocks = %v for %d findings", got.Blocks(), len(tc.want))
			}
		})
	}
}

// What cannot be read is named, never taken for clean, and the other check still runs.
func TestCheckUpgradeNamesWhatItCouldNotRead(t *testing.T) {
	t.Parallel()
	kube := fake.NewClientset(validating("guard", "apps", "guard", new(admissionregistrationv1.Fail)), slice("apps", "guard", onNode("a", true)))
	denied := func(context.Context) (io.ReadCloser, error) { return nil, errors.New("403 Forbidden") }

	got := CheckUpgrade(context.Background(), kube, denied, "v1.33.1")
	if len(got.Unchecked) != 1 || !strings.Contains(got.Unchecked[0], "the deprecated APIs in use") || !strings.Contains(got.Unchecked[0], "403 Forbidden") {
		t.Errorf("unchecked %q", got.Unchecked)
	}
	if len(got.Webhooks) != 1 || !got.Blocks() || got.Clean() {
		t.Errorf("webhooks %+v, blocks %v", got.Webhooks, got.Blocks())
	}
	if got = CheckUpgrade(context.Background(), kube, nil, "v1.33.1"); len(got.Unchecked) != 1 {
		t.Errorf("without a way to read metrics: %q", got.Unchecked)
	}
	if got = CheckUpgrade(context.Background(), kube, metricsOf(""), "latest"); len(got.Unchecked) != 1 || !got.Blocks() {
		t.Errorf("for a target that is no version: %q", got.Unchecked)
	}
	if got = CheckUpgrade(context.Background(), fake.NewClientset(), metricsOf(requestedDeprecated), "v1.24.0"); !got.Clean() || got.Blocks() {
		t.Errorf("nothing removed by 1.24 is %+v", got)
	}
}

// Channels offer a new cluster the newest minors, and OldestOffered is the oldest of those.
func TestOldestOffered(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ch   Channels
		want int
		ok   bool
	}{
		{"four newest", channels(36, "v1.29.1+k3s1", "v1.33.1+k3s1", "v1.34.1+k3s1", "v1.35.1+k3s1", "v1.36.1+k3s1", "v1.37.1+k3s1"), 34, true},
		{"a lagging stable keeps its minor", channels(31, "v1.31.14+k3s1", "v1.35.9+k3s1", "v1.37.1+k3s1"), 31, true},
		{"none", channels(36), 0, false},
	} {
		if got, ok := tc.ch.OldestOffered(); got != tc.want || ok != tc.ok {
			t.Errorf("%s: %d, %v; want %d, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

type fakeChecker struct {
	found UpgradeFindings
	asked []string
}

func (f *fakeChecker) Check(_ context.Context, _ *clusterv1.Cluster, target string) UpgradeFindings {
	f.asked = append(f.asked, target)
	return f.found
}

func upgraderFixture(t *testing.T, c *clusterv1.Cluster) (*Upgrader, client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(c).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	if err := kube.Status().Update(context.Background(), c.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	recorder := events.NewFakeRecorder(20)
	return &Upgrader{
		Client:         kube,
		ClassNamespace: "kubehenge-system",
		Releases:       fixedReleases{"rke2": channels(37, "v1.34.9+rke2r1", "v1.35.9+rke2r1", "v1.36.5+rke2r1", "v1.37.1+rke2r1")},
		Events:         recorder,
		Now:            func() time.Time { return time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC) },
	}, kube, recorder
}

func getCluster(t *testing.T, kube client.Client, name string) *clusterv1.Cluster {
	t.Helper()
	c := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: name}, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func recorded(r *events.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-r.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

// An automatic upgrade waits for a removed API in use, a thin fail-closed webhook or a check it
// could not make, says so on the cluster and in an event, and goes ahead once nothing is in the way.
func TestAutomaticUpgradeWaitsForWhatTheChecksFind(t *testing.T) {
	t.Parallel()
	c := settledCluster("web", "v1.35.4+rke2r1", map[string]string{"upgrades": `{"mode":"minor"}`})
	u, kube, recorder := upgraderFixture(t, c)
	checker := &fakeChecker{}
	u.Checker = checker

	for _, tc := range []struct {
		name   string
		found  UpgradeFindings
		reason string
		text   string
	}{
		{"a removed API", UpgradeFindings{RemovedAPIs: []RemovedAPI{{API: "policy/v1beta1 podsecuritypolicies", RemovedIn: "1.36"}}}, AutoUpgradeSkippedRemovedAPIs, "policy/v1beta1 podsecuritypolicies (removed in 1.36)"},
		{"a webhook", UpgradeFindings{Webhooks: []RiskyWebhook{{Kind: "validating", Name: "guard", Service: "apps/guard", Why: "has 1 ready endpoint"}}}, AutoUpgradeSkippedWebhooks, "apps/guard has 1 ready endpoint"},
		{"no way to check", UpgradeFindings{Unchecked: []string{"the deprecated APIs in use (403 Forbidden)"}}, AutoUpgradeSkippedUnchecked, "403 Forbidden"},
	} {
		checker.found = tc.found
		u.RunOnce(context.Background())
		got := getCluster(t, kube, "web")
		cond := meta.FindStatusCondition(got.Status.Conditions, AutoUpgradeSkippedCondition)
		if got.Spec.Topology.Version != "v1.35.4+rke2r1" || got.Annotations[autoUpgraded] != "" {
			t.Fatalf("%s: upgraded to %s", tc.name, got.Spec.Topology.Version)
		}
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tc.reason || !strings.Contains(cond.Message, tc.text) || !strings.Contains(cond.Message, "v1.36.5+rke2r1") {
			t.Fatalf("%s: condition %+v", tc.name, cond)
		}
		if ev := recorded(recorder); !hasEvent(ev, "Warning AutoUpgradeSkipped") {
			t.Errorf("%s: events %q", tc.name, ev)
		}
		// The same findings again record no second event.
		u.RunOnce(context.Background())
		if ev := recorded(recorder); hasEvent(ev, "AutoUpgradeSkipped") {
			t.Errorf("%s: events %q on a repeat", tc.name, ev)
		}
	}
	if !slices.Contains(checker.asked, "v1.36.5+rke2r1") {
		t.Errorf("checked %v", checker.asked)
	}

	checker.found = UpgradeFindings{}
	u.RunOnce(context.Background())
	got := getCluster(t, kube, "web")
	if got.Spec.Topology.Version != "v1.36.5+rke2r1" || meta.FindStatusCondition(got.Status.Conditions, AutoUpgradeSkippedCondition) != nil {
		t.Errorf("version %s, condition %+v", got.Spec.Topology.Version, meta.FindStatusCondition(got.Status.Conditions, AutoUpgradeSkippedCondition))
	}
}

func hasEvent(all []string, prefix string) bool {
	return slices.ContainsFunc(all, func(e string) bool { return strings.Contains(e, prefix) })
}

// A cluster whose automatic upgrades are switched off loses the note of a skip at once.
func TestSkippedNoteGoesWhenUpgradesAreOff(t *testing.T) {
	t.Parallel()
	c := settledCluster("web", "v1.35.4+rke2r1", map[string]string{"upgrades": `{"mode":"off"}`})
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{Type: AutoUpgradeSkippedCondition, Status: metav1.ConditionTrue, Reason: AutoUpgradeSkippedRemovedAPIs})
	u, kube, _ := upgraderFixture(t, c)
	u.Checker = &fakeChecker{}
	u.RunOnce(context.Background())
	if cond := meta.FindStatusCondition(getCluster(t, kube, "web").Status.Conditions, AutoUpgradeSkippedCondition); cond != nil {
		t.Errorf("condition %+v stays", cond)
	}
}

// A manual upgrade is not held back: the Upgrader only decides the automatic ones, so a cluster
// that is not due is not read at all.
func TestChecksAreNotMadeForAClusterNotDue(t *testing.T) {
	t.Parallel()
	c := settledCluster("web", "v1.37.1+rke2r1", map[string]string{"upgrades": `{"mode":"minor"}`})
	u, _, _ := upgraderFixture(t, c)
	checker := &fakeChecker{found: UpgradeFindings{Unchecked: []string{"x"}}}
	u.Checker = checker
	u.RunOnce(context.Background())
	if len(checker.asked) != 0 {
		t.Errorf("checked %v on a cluster with nothing to upgrade to", checker.asked)
	}
}

// A cluster on a minor older than the oldest offered for new clusters has the condition and an
// event, it is compared again once an hour, and the condition goes when it is not behind.
func TestClusterOnAnUnsupportedMinor(t *testing.T) {
	t.Parallel()
	c := settledCluster("web", "v1.33.4+rke2r1", map[string]string{"upgrades": `{"mode":"off"}`})
	supported := settledCluster("fresh", "v1.34.2+rke2r1", nil)
	u, kube, recorder := upgraderFixture(t, c)
	if err := kube.Create(context.Background(), supported); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)
	u.Now = func() time.Time { return now }

	u.RunOnce(context.Background())
	got := getCluster(t, kube, "web")
	cond := meta.FindStatusCondition(got.Status.Conditions, KubernetesOutOfSupportCondition)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != KubernetesBehind ||
		!strings.Contains(cond.Message, "Kubernetes 1.33 is older than 1.34") {
		t.Fatalf("condition %+v", cond)
	}
	if meta.FindStatusCondition(getCluster(t, kube, "fresh").Status.Conditions, KubernetesOutOfSupportCondition) != nil {
		t.Error("a cluster on the oldest offered minor is marked")
	}
	if ev := recorded(recorder); !hasEvent(ev, "Warning KubernetesOutOfSupport") {
		t.Errorf("events %q", ev)
	}

	// Within the hour nothing is compared again.
	got.Spec.Topology.Version = "v1.34.9+rke2r1"
	if err := kube.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	u.RunOnce(context.Background())
	if meta.FindStatusCondition(getCluster(t, kube, "web").Status.Conditions, KubernetesOutOfSupportCondition) == nil {
		t.Error("compared again within the hour")
	}
	now = now.Add(supportEvery)
	u.RunOnce(context.Background())
	if meta.FindStatusCondition(getCluster(t, kube, "web").Status.Conditions, KubernetesOutOfSupportCondition) != nil {
		t.Error("the condition stays on a supported minor")
	}
	if ev := recorded(recorder); !hasEvent(ev, "Normal KubernetesSupported") {
		t.Errorf("events %q", ev)
	}
}
