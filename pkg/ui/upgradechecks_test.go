package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/notify"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// A cluster on a Kubernetes minor older than any offered is a warning on its Health page, and an
// automatic upgrade that waits is another.
func TestHealthSaysOutOfSupportAndASkippedUpgrade(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := &clusterv1.Cluster{Name: "web", Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{
		condition(provisioner.KubernetesOutOfSupportCondition, metav1.ConditionTrue, "Kubernetes 1.33 is older than 1.34.", since),
		condition(provisioner.AutoUpgradeSkippedCondition, metav1.ConditionTrue, "The automatic upgrade to v1.34.1 waits.", since),
	}}}
	var a attention
	watchAttention(c, &a)
	got := map[string]attentionView{}
	for _, item := range a {
		got[item.What] = item
	}
	support, skipped := got[outOfSupportWhat], got["Automatic upgrade skipped"]
	if support.Severity != severityWarning || support.Message != "Kubernetes 1.33 is older than 1.34." || !support.Since.Equal(since) {
		t.Errorf("out of support: %+v", support)
	}
	if skipped.Severity != severityWarning || skipped.Message != "The automatic upgrade to v1.34.1 waits." {
		t.Errorf("skipped upgrade: %+v", skipped)
	}
	var none attention
	watchAttention(&clusterv1.Cluster{Name: "web"}, &none)
	if len(none) != 0 {
		t.Errorf("a cluster without the conditions has %+v", none)
	}
}

// A pause of a week or more is noted, with its date. A shorter one, a pause lifted and a condition
// without the spec's pause are not.
func TestHealthNotesAClusterPausedForAWeek(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	paused := func(spec bool, since time.Time) *clusterv1.Cluster {
		return &clusterv1.Cluster{Name: "web", Spec: clusterv1.ClusterSpec{Paused: new(spec)}, Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{
			condition(clusterv1.PausedCondition, metav1.ConditionTrue, "", since),
		}}}
	}
	for _, tc := range []struct {
		name    string
		cluster *clusterv1.Cluster
		want    string
	}{
		{"six days", paused(true, now.Add(-6*24*time.Hour)), ""},
		{"seven days", paused(true, now.Add(-pausedNoteAfter)), "Paused since 2026-10-13"},
		{"months", paused(true, time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)), "Paused since 2026-03-02"},
		{"lifted", paused(false, now.Add(-30*24*time.Hour)), ""},
		{"no condition", &clusterv1.Cluster{Name: "web", Spec: clusterv1.ClusterSpec{Paused: new(true)}}, ""},
	} {
		var a attention
		pausedAttention(tc.cluster, now, &a)
		switch {
		case tc.want == "" && len(a) != 0:
			t.Errorf("%s: %+v", tc.name, a)
		case tc.want != "" && (len(a) != 1 || a[0].What != "Paused" || !strings.HasPrefix(a[0].Message, tc.want+". ") || a[0].Severity != severityWarning):
			t.Errorf("%s: %+v, want %q", tc.name, a, tc.want)
		}
	}
}

// A paused cluster's certificate warning says it is paused, as a warning rather than a renewal.
func TestHealthSaysPausedCertificatesExpire(t *testing.T) {
	t.Parallel()
	c := &clusterv1.Cluster{Name: "web", Status: clusterv1.ClusterStatus{Conditions: []metav1.Condition{{
		Type: provisioner.CertificatesCondition, Status: metav1.ConditionTrue, Reason: provisioner.CertificatesPaused,
		Message: "Certificates run out: machine web-cp-a on 2027-10-16. To renew them, resume the cluster. The machines holding them are not replaced while it is paused.",
	}}}}
	var a attention
	watchAttention(c, &a)
	if len(a) != 1 || a[0].Severity != severityWarning || a[0].What != "Certificates expire" || !strings.Contains(a[0].Message, "resume the cluster") {
		t.Errorf("%+v", a)
	}
}

// An out-of-support notice is told once it has stood, again a month later while it stands, and not in between.
func TestOutOfSupportIsToldMonthly(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	behind := notify.Notice{Key: "default/web/uid/" + outOfSupportWhat, Severity: notify.Warning, Title: "web: Kubernetes out of support"}
	once := notify.Notice{Key: "default/web/uid/other", Severity: notify.Warning, Title: "web: other"}
	found := &notifyFound{problems: []notify.Notice{behind, once}, read: map[string]bool{"": true}, listed: true}
	var st notifyState
	if got := st.step(start, found); len(got) != 0 {
		t.Fatalf("told at once: %v", got)
	}
	told := func(at time.Time) []string {
		sent := st.step(at, found)
		keys := make([]string, len(sent))
		for i := range sent {
			keys[i] = sent[i].Key
		}
		return keys
	}
	if got := told(start.Add(notifyHold)); len(got) != 2 {
		t.Fatalf("after the hold: %v", got)
	}
	if got := told(start.Add(notifyHold + 29*24*time.Hour)); len(got) != 0 {
		t.Errorf("told again after 29 days: %v", got)
	}
	if got := told(start.Add(notifyHold + 30*24*time.Hour)); !slices.Equal(got, []string{behind.Key}) {
		t.Errorf("a month on: %v, want only the out-of-support notice", got)
	}
	if got := told(start.Add(notifyHold + 31*24*time.Hour)); len(got) != 0 {
		t.Errorf("told twice in the same month: %v", got)
	}
	if got := told(start.Add(notifyHold + 60*24*time.Hour)); !slices.Equal(got, []string{behind.Key}) {
		t.Errorf("two months on: %v", got)
	}
}

// The appliance's notices for a cluster out of support repeat, and carry no other repeat.
func TestClusterProblemsRepeatOutOfSupport(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	v := &healthView{Attention: []attentionView{
		{Severity: severityWarning, What: outOfSupportWhat, Message: "Kubernetes 1.33 is older than 1.34."},
		{Severity: severityWarning, What: "Paused", Message: "x"},
	}}
	c := &clusterv1.Cluster{Namespace: "default", Name: "one"}
	notices, _ := h.server.clusterProblems(c, v)
	repeats := map[string]bool{}
	for i := range notices {
		repeats[strings.TrimPrefix(notices[i].Title, "one: ")] = repeatEvery(notices[i].Key) == notifyOutOfSupportEvery
	}
	if !repeats[outOfSupportWhat] || repeats["Paused"] || len(repeats) != 2 {
		t.Errorf("repeats %v", repeats)
	}
}

const deprecatedMetrics = `# HELP apiserver_requested_deprecated_apis Gauge of deprecated APIs.
# TYPE apiserver_requested_deprecated_apis gauge
apiserver_requested_deprecated_apis{group="flowcontrol.apiserver.k8s.io",removed_release="1.32",resource="flowschemas",subresource="",version="v1beta3"} 1
apiserver_requested_deprecated_apis{group="",removed_release="1.99",resource="endpoints",subresource="",version="v1"} 1
apiserver_requested_deprecated_apis_other{group="x"} 1
`

func webhookEndpoints(namespace, service string, nodes ...string) *discoveryv1.EndpointSlice {
	slice := &discoveryv1.EndpointSlice{
		Namespace: namespace, Name: service + "-abc", Labels: map[string]string{discoveryv1.LabelServiceName: service},
	}
	for _, n := range nodes {
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{NodeName: new(n), Conditions: discoveryv1.EndpointConditions{Ready: new(true)}})
	}
	return slice
}

func failClosedWebhook(name, namespace, service string) *admissionregistrationv1.ValidatingWebhookConfiguration {
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		Name: name,
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: name + ".example.org", FailurePolicy: new(admissionregistrationv1.Fail),
			ClientConfig: admissionregistrationv1.WebhookClientConfig{Service: &admissionregistrationv1.ServiceReference{Namespace: namespace, Name: service}},
		}},
	}
}

func readUpgradeChecks(t *testing.T, b *browser, version string) upgradeChecksView {
	t.Helper()
	rec := b.do("GET", "/api/clusters/default/one/upgrade-checks?version="+version, "")
	expect(t, rec, http.StatusOK)
	var v upgradeChecksView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The upgrade dialog reads the cluster as the health identity: the APIs in use that the target
// removes, and the fail-closed webhooks whose backend is thin.
func TestUpgradeChecksNameRemovedAPIsAndThinWebhooks(t *testing.T) {
	t.Parallel()
	h, workloads := healthHarness(t)
	workloads.metrics = deprecatedMetrics
	ctx := context.Background()
	if _, err := workloads.kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(ctx, failClosedWebhook("policy-guard", "guard", "policy"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := workloads.kube.DiscoveryV1().EndpointSlices("guard").Create(ctx, webhookEndpoints("guard", "policy", "node-a"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("dev@pve")

	v := readUpgradeChecks(t, b, "v1.33.1")
	if len(v.RemovedAPIs) != 1 || v.RemovedAPIs[0].API != "flowcontrol.apiserver.k8s.io/v1beta3 flowschemas" || v.RemovedAPIs[0].RemovedIn != "1.32" {
		t.Errorf("removed APIs %+v", v.RemovedAPIs)
	}
	if len(v.Webhooks) != 1 || v.Webhooks[0].Name != "policy-guard" || v.Webhooks[0].Why != "has 1 ready endpoint" || len(v.Unchecked) != 0 {
		t.Errorf("webhooks %+v, unchecked %v", v.Webhooks, v.Unchecked)
	}
	if len(v.Lines) != 2 || !strings.HasPrefix(v.Lines[0], "Removed APIs still in use: flowcontrol.apiserver.k8s.io/v1beta3 flowschemas (removed in 1.32)") {
		t.Errorf("lines %q", v.Lines)
	}
	if !slices.Equal(workloads.connected(), []string{"health-token"}) {
		t.Errorf("read with %v, not the health identity", workloads.connected())
	}

	// An API removed after the target is not in the way.
	if v = readUpgradeChecks(t, b, "v1.31.9"); len(v.RemovedAPIs) != 0 {
		t.Errorf("removed APIs for 1.31: %+v", v.RemovedAPIs)
	}
}

// Upgrade anyway stores the findings listed now on the cluster, by name, for the minor version
// asked about. Findings that could not be read are not accepted.
func TestAcceptUpgradeStoresTheListedFindings(t *testing.T) {
	t.Parallel()
	h, workloads := healthHarness(t)
	workloads.metrics = deprecatedMetrics
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("POST", "/api/clusters/default/one/upgrade-checks/accept?version=v1.33.1", "")
	expect(t, rec, http.StatusOK)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	if got := c.Annotations[provisioner.AcknowledgedAnnotation]; got != `{"1.33":["flowcontrol.apiserver.k8s.io/v1beta3 flowschemas"]}` {
		t.Errorf("annotation %q", got)
	}

	workloads.metricsErr = errors.New("403 Forbidden")
	rec = b.do("POST", "/api/clusters/default/one/upgrade-checks/accept?version=v1.34.1", "")
	expect(t, rec, http.StatusConflict)
}

func TestUpgradeChecksSayWhatCouldNotBeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(h *harness, w *fakeWorkloads)
		want   string
	}{
		{"no metrics", func(_ *harness, w *fakeWorkloads) { w.metricsErr = errors.New("403 Forbidden") }, "the deprecated APIs in use"},
		{"its token ran out", func(h *harness, _ *fakeWorkloads) { h.clock = h.clock.Add(2 * time.Hour) }, "Access to the cluster expired"},
		{"no token yet", func(h *harness, _ *fakeWorkloads) {
			if err := h.kube.Delete(context.Background(), &corev1.Secret{Namespace: "default", Name: provisioner.IdentitySecretName("one", provisioner.IdentityHealth)}); err != nil {
				t.Fatal(err)
			}
		}, notSetUp},
		{"outside the interface's namespace", func(h *harness, _ *fakeWorkloads) { h.server.Namespace = "clusters" }, "the interface reads inside clusters in namespace clusters only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, workloads := healthHarness(t)
			tc.change(h, workloads)
			b := h.browser()
			b.signIn("dev@pve")
			v := readUpgradeChecks(t, b, "v1.33.1")
			if len(v.Unchecked) == 0 || !strings.Contains(v.Unchecked[0], tc.want) || len(v.Lines) == 0 {
				t.Errorf("unchecked %q, lines %q; want %q", v.Unchecked, v.Lines, tc.want)
			}
		})
	}
}

// The demo's clusters have a removed API in use and a thin fail-closed webhook, so its upgrade
// dialog shows what the checks find.
func TestDemoUpgradeChecksFindSomething(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	rec := b.do("GET", "/api/clusters/clusters/web/upgrade-checks?version=v1.37.1", "")
	expect(t, rec, http.StatusOK)
	var v upgradeChecksView
	decodeDemo(t, rec, &v)
	if len(v.RemovedAPIs) != 1 || len(v.Webhooks) != 1 || len(v.Unchecked) != 0 {
		t.Errorf("findings %+v", v)
	}
	rec = b.do("GET", "/api/clusters/clusters/web/upgrade-checks?version=v1.36.9", "")
	expect(t, rec, http.StatusOK)
	decodeDemo(t, rec, &v)
	if len(v.RemovedAPIs) != 0 || len(v.Webhooks) != 1 {
		t.Errorf("findings for a release before the removal: %+v", v)
	}
}

// The demo fleet has a cluster out of support and paused, and one whose automatic upgrade waits for
// a removed API, so its Health pages show these as they are read from the clusters.
func TestDemoHealthShowsVersionAndPauseNotes(t *testing.T) {
	t.Parallel()
	_, b := newDemo(t)
	b.signIn()
	whats := func(cluster string) []string {
		var v healthView
		decodeDemo(t, b.do("GET", "/api/clusters/clusters/"+cluster+"/health", ""), &v)
		out := make([]string, len(v.Attention))
		for i := range v.Attention {
			out[i] = v.Attention[i].What
		}
		return out
	}
	legacy := whats("legacy")
	for _, want := range []string{outOfSupportWhat, "Paused", "Certificates expire"} {
		if !slices.Contains(legacy, want) {
			t.Errorf("legacy: %q lacks %q", legacy, want)
		}
	}
	if got := whats("staging"); !slices.Contains(got, "Automatic upgrade skipped") {
		t.Errorf("staging: %q", got)
	}
}

func TestUpgradeChecksNeedToSeeTheCluster(t *testing.T) {
	t.Parallel()
	h, _ := healthHarness(t)
	h.accounts.passwords["nobody@pve"] = "secret"
	b := h.browser()
	b.signIn("nobody@pve")
	expect(t, b.do("GET", "/api/clusters/default/one/upgrade-checks?version=v1.33.1", ""), http.StatusNotFound)
}
