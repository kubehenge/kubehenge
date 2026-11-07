package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A paused cluster's certificates that run out are reported with their dates and the reason Paused,
// and no machine is replaced. Once the cluster is resumed the watch renews them.
func TestPausedClusterReportsCertificatesAndReplacesNothing(t *testing.T) {
	t.Parallel()
	paused := certCluster("rke2", 3)
	paused.Spec.Paused = new(true)
	f := newCertFixture(t, paused,
		[]client.Object{certControlPlane("web-cp-a"), certControlPlane("web-cp-b"), certControlPlane("web-cp-c"), certWorker("web-md-0-a")},
		report("web-cp-a", 45, time.Minute), report("web-md-0-a", 50, time.Minute), report("web-cp-c", 400, time.Minute))

	cond := f.look()
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != CertificatesPaused {
		t.Fatalf("condition %+v", cond)
	}
	if head := CertificatesHead(cond.Message); head != "Certificates run out: machine web-cp-a on 2027-10-16, machine web-md-0-a on 2027-10-21." {
		t.Errorf("head %q", head)
	}
	if !strings.Contains(cond.Message, "resume the cluster") || !strings.Contains(cond.Message, "paused") {
		t.Errorf("message %q", cond.Message)
	}
	if _, asked := f.machine("web-cp-a").Annotations[clusterv1.RemediateMachineAnnotation]; asked {
		t.Error("a machine of a paused cluster is asked to be replaced")
	}
	c := &clusterv1.Cluster{}
	if err := f.kube.Get(context.Background(), webKey, c); err != nil {
		t.Fatal(err)
	}
	if after := c.Spec.Topology.Workers.MachineDeployments[0].Rollout.After; !after.IsZero() {
		t.Errorf("the workers roll after %v", after)
	}
	if got := f.drained(); len(got) != 1 || !strings.HasPrefix(got[0], "Warning CertificatesExpiring Certificates run out") {
		t.Errorf("events %q", got)
	}
	f.look()
	if len(f.drained()) != 0 {
		t.Error("a second look repeats the event")
	}

	// Resumed, the next look replaces them.
	c.Spec.Paused = nil
	if err := f.kube.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if cond = f.look(); cond == nil || cond.Reason != CertificatesRenewing {
		t.Fatalf("after resuming: %+v", cond)
	}
	if _, asked := f.machine("web-cp-a").Annotations[clusterv1.RemediateMachineAnnotation]; !asked {
		t.Error("web-cp-a is not replaced after resuming")
	}
}

// A paused cluster whose certificates are far from running out has no condition.
func TestPausedClusterWithTimeToSpareHasNoCertificateCondition(t *testing.T) {
	t.Parallel()
	paused := certCluster("rke2", 1)
	paused.Spec.Paused = new(true)
	f := newCertFixture(t, paused, []client.Object{certControlPlane("web-cp-a")}, report("web-cp-a", 300, time.Minute))
	if cond := f.look(); cond != nil {
		t.Errorf("condition %+v", cond)
	}
}

// The health identity reads the API server's metrics, the webhook configurations and the endpoint
// slices behind them, and nothing it could change.
func TestHealthIdentityReadsWhatUpgradeChecksNeed(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	c := initialised(storageCluster())
	f := newIdentities(t, c, &clock)
	reconcileIdentities(t, f.w, c)

	var metrics, webhooks, endpoints bool
	for _, r := range clusterRights(t, f.workload, "kubehenge-health") {
		reads := len(r.Verbs) > 0 && !slices.ContainsFunc(r.Verbs, func(v string) bool { return v != "get" && v != "list" })
		metrics = metrics || reads && slices.Contains(r.NonResourceURLs, "/metrics")
		webhooks = webhooks || reads && slices.Contains(r.Resources, "validatingwebhookconfigurations") && slices.Contains(r.Resources, "mutatingwebhookconfigurations")
		endpoints = endpoints || reads && slices.Contains(r.Resources, "endpointslices")
	}
	if !metrics || !webhooks || !endpoints {
		t.Errorf("metrics %v, webhook configurations %v, endpoint slices %v", metrics, webhooks, endpoints)
	}
}

// Without a usable token the checker says so and finds nothing, which holds an automatic upgrade.
func TestIdentityCheckerSaysWhyItCannotRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	cluster := &clusterv1.Cluster{Namespace: "clusters", Name: "web"}
	secret := func(expires time.Time, data map[string][]byte) *corev1.Secret {
		s := &corev1.Secret{Namespace: "clusters", Name: IdentitySecretName("web", IdentityHealth), Data: data}
		if !expires.IsZero() {
			s.Annotations = map[string]string{IdentityExpiresAnnotation: expires.Format(time.RFC3339)}
		}
		return s
	}
	full := map[string][]byte{IdentityServerKey: []byte("https://10.0.0.20:6443"), IdentityTokenKey: []byte("t")}
	for _, tc := range []struct {
		name   string
		secret *corev1.Secret
		want   string
	}{
		{"no Secret", nil, "not set up yet"},
		{"no token", secret(now.Add(time.Hour), map[string][]byte{IdentityServerKey: []byte("https://10.0.0.20:6443")}), "not set up yet"},
		{"not written by the controller", secret(time.Time{}, full), "not set up yet"},
		{"expired", secret(now.Add(-time.Minute), full), "has expired"},
	} {
		builder := ctrlfake.NewClientBuilder()
		if tc.secret != nil {
			builder = builder.WithObjects(tc.secret)
		}
		checker := &IdentityChecker{Reader: builder.Build(), Now: func() time.Time { return now }}
		got := checker.Check(context.Background(), cluster, "v1.33.1")
		if len(got.Unchecked) != 1 || !strings.Contains(got.Unchecked[0], tc.want) || !got.Blocks() {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
}
