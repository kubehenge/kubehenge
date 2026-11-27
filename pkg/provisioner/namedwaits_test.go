package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// drained is the events recorded so far.
func drained(recorder *events.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-recorder.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

// Every add-on whose chart waits says what for, in a line of its own.
func TestEveryChartWaitSaysWhatFor(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	kubeadm := storageCluster()
	kubeadm.Spec.Topology.ClassRef.Name = ClassPrefix + "kubeadm"
	r, kube := newReconciler(t, &fakeCloud{}, c, readyCertManager(c))
	// The cluster has no kubeconfig yet.
	r.Remotes = &Remotes{Reader: kube}
	bare := storageCluster()
	for _, tc := range []struct {
		cluster *clusterv1.Cluster
		proxy   string
		addons  Addons
		want    string
	}{
		{cluster: c, proxy: kyvernoPoliciesProxy(c), want: "kyverno-policies waits for kyverno to be deployed"},
		{cluster: c, proxy: rancherProxy(c), want: "rancher waits for the cluster's API"},
		{cluster: bare, proxy: traefikProxy(bare), want: "traefik waits for the cluster's Gateway API definitions"},
		{cluster: c, proxy: veleroProxy(c), addons: Addons{Backups: Backups{ExistingSecret: "store-keys"}}, want: "velero waits for the cluster's API"},
		{cluster: kubeadm, proxy: snapshotClassProxy(kubeadm), want: "snapshot-class waits for snapshot-controller to be deployed"},
	} {
		addons := tc.addons
		state, err := r.proxyState(context.Background(), tc.cluster, proxyNamed(tc.cluster, tc.proxy), &addons)
		if err != nil {
			t.Fatal(err)
		}
		if state != proxyWaits || len(addons.waits) != 1 || addons.waits[0].line != tc.want || addons.waits[0].reason != AddonsWaitingReason {
			t.Errorf("%s: state %v, lines %+v, want %q", tc.proxy, state, addons.waits, tc.want)
		}
	}
}

// proxyNamed is a bare HelmChartProxy of the cluster.
func proxyNamed(c *clusterv1.Cluster, name string) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	p.SetNamespace(c.Namespace)
	p.SetName(name)
	return p
}

// A missing log output credential is a waiting line, and Alloy is applied all the same.
func TestLogOutputCredentialIsAWaitingLine(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	r, kube := newReconciler(t, &fakeCloud{}, c, readyCertManager(c))
	r.Remotes = &Remotes{Reader: kube}
	addons := Addons{Logs: Logs{Outputs: allOutputs[:1]}}
	state, err := r.proxyState(context.Background(), c, proxyNamed(c, alloyProxy(c)), &addons)
	if err != nil {
		t.Fatal(err)
	}
	want := "log output " + allOutputs[0].Name + " waits for its stored credential " + allOutputs[0].CredentialPurpose()
	if state != proxyReady || len(addons.waits) != 1 || addons.waits[0].line != want || addons.waits[0].reason != AddonsWaitingCredentialMissing {
		t.Errorf("state %v, lines %+v", state, addons.waits)
	}
}

// The waiting lines go into AddonsWaiting with one Warning event when they change, none while they
// hold, and the condition goes once nothing waits. A fine cluster carries neither.
func TestAddonsWaitingSaysEveryLineAndWarnsOnChange(t *testing.T) {
	t.Parallel()
	c := storageCluster()
	r, kube := newReconciler(t, &fakeCloud{}, c)
	recorder := events.NewFakeRecorder(20)
	r.Events = recorder
	ctx := context.Background()
	record := func(waits ...addonWait) {
		t.Helper()
		current := &clusterv1.Cluster{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(c), current); err != nil {
			t.Fatal(err)
		}
		if _, err := r.recordAddonsWaiting(ctx, current, &Addons{waits: waits}, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitingEvents := func() int {
		n := 0
		for _, e := range drained(recorder) {
			if strings.Contains(e, AddonsWaitingCondition) {
				n++
			}
		}
		return n
	}

	record()
	if cond := clusterCondition(t, kube, c, AddonsWaitingCondition); cond != nil || waitingEvents() != 0 {
		t.Fatalf("a fine cluster: %+v", cond)
	}
	policies := chartWait("kyverno-policies", "kyverno to be deployed")
	credential := addonWait{addon: logsAddon, reason: AddonsWaitingCredentialMissing, line: "log output splunk waits for its stored credential log-splunk"}
	record(policies, credential)
	cond := clusterCondition(t, kube, c, AddonsWaitingCondition)
	if cond == nil || cond.Reason != AddonsWaitingReason || cond.Message != policies.line+". "+credential.line {
		t.Errorf("waiting: %+v", cond)
	}
	if n := waitingEvents(); n != 1 {
		t.Errorf("%d events for a new wait", n)
	}
	record(policies, credential)
	if n := waitingEvents(); n != 0 {
		t.Errorf("%d events while the wait holds", n)
	}
	record(credential)
	if cond := clusterCondition(t, kube, c, AddonsWaitingCondition); cond == nil || cond.Reason != AddonsWaitingCredentialMissing || cond.Message != credential.line {
		t.Errorf("one line left: %+v", cond)
	}
	if n := waitingEvents(); n != 1 {
		t.Errorf("%d events for a changed wait", n)
	}
	record()
	if cond := clusterCondition(t, kube, c, AddonsWaitingCondition); cond != nil {
		t.Errorf("nothing waits, still %+v", cond)
	}
	if n := waitingEvents(); n != 0 {
		t.Errorf("%d events once nothing waits", n)
	}
}

// cert-manager's own wait for the Gateway API is said once, in its users' line.
func TestCertManagersOwnWaitIsSaidOnce(t *testing.T) {
	t.Parallel()
	c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring})
	r, kube := newReconciler(t, &fakeCloud{}, c)
	answering(r, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	cond := clusterCondition(t, kube, c, AddonsWaitingCondition)
	if cond == nil || strings.Count(cond.Message, gatewayAPIWait) != 1 || !strings.HasPrefix(cond.Message, "monitoring wait for cert-manager") {
		t.Errorf("condition %+v", cond)
	}
}

// Without the DNS provider's credential the DNS-01 issuer is not applied, and the waiting line names
// the Secret to store.
func TestDNS01IssuerWaitsForItsCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := certificatesCluster(`{"enabled": true}`)
	certs := Certificates{Enabled: true, Domain: "lab.example.org", DNS01: DNS01{Provider: "cloudflare"}}
	r, _ := newReconciler(t, &fakeCloud{}, c)
	workload := newCertWorkload(t)
	workload.installed = true
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	if _, err := r.reconcileCertificates(ctx, c, &certs); err != nil {
		t.Fatal(err)
	}
	if workload.has("ClusterIssuer//" + ingressIssuer) {
		t.Errorf("the DNS-01 issuer applied without its credential: %v", workload.applied)
	}
	waits, err := r.certificateWaits(ctx, c, &certs)
	want := "certificates waits for the DNS-01 credential in Secret team-a/" + CredentialName(c.Name, dns01Purpose)
	if err != nil || len(waits) != 1 || waits[0].line != want || waits[0].reason != AddonsWaitingCredentialMissing {
		t.Errorf("waits %+v, %v", waits, err)
	}
}

// The Gateway API reconciler says what it waits for on the cluster, and the release once recorded.
func TestGatewayAPIReadySaysWhatItWaitsFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := initialisedCluster(true)
	_, kube := newReconciler(t, &fakeCloud{}, c)
	workload := gatewayWorkload(t)
	r := &GatewayAPIReconciler{
		Client: kube, ClassNamespace: "kubehenge-system", Bundle: testGatewayAPI(t),
		Remotes: &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}},
	}
	run := func() *metav1.Condition {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
			t.Fatal(err)
		}
		got := &clusterv1.Cluster{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(c), got); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(got.Status.Conditions, GatewayAPIReadyCondition)
	}
	if cond := run(); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != GatewayAPIWaitingReason ||
		cond.Message != "waits for RKE2's rke2-traefik-crd chart, which has not deployed" {
		t.Errorf("before the chart: %+v", cond)
	}
	if err := workload.Create(ctx, helmRevision(t, "rke2-traefik-crd", 1, "deployed", "")); err != nil {
		t.Fatal(err)
	}
	if cond := run(); cond == nil || cond.Status != metav1.ConditionFalse || cond.Message != "waits to see the CRDs it applied" {
		t.Errorf("applying: %+v", cond)
	}
	if cond := run(); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != GatewayAPIRecordedReason || cond.Message != "release v1.6.3" {
		t.Errorf("recorded: %+v", cond)
	}
}

// A cluster that registers with Rancher says what registration waits for, and that it is
// registered once Rancher has it ready.
func TestRancherRegisteredSaysWhatRegistrationWaitsFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	web := availableNow(clusterWithRancher("one", false))
	f := newRancherFixture(t, web, nil)
	f.mustReconcile(t, web)
	if cond := clusterCondition(t, f.kube, web, RancherRegisteredCondition); cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Reason != RancherWaitingReason || cond.Message != rancherAgentWait {
		t.Errorf("created in Rancher: %+v", cond)
	}
	rc := &unstructured.Unstructured{}
	rc.SetGroupVersionKind(provisioningClusterGVK)
	if err := f.ranchers.Get(ctx, f.registeredKey, rc); err != nil {
		t.Fatal(err)
	}
	rc.Object["status"] = map[string]any{"ready": true}
	if err := f.ranchers.Update(ctx, rc); err != nil {
		t.Fatal(err)
	}
	f.mustReconcile(t, web)
	if cond := clusterCondition(t, f.kube, web, RancherRegisteredCondition); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != RancherRegisteredReason {
		t.Errorf("registered: %+v", cond)
	}
}

// A serving certificate request left pending for an address that is not its node's is said once.
func TestPendingServingCertificateIsWarnedOnce(t *testing.T) {
	t.Parallel()
	c := cluster()
	recorder := events.NewFakeRecorder(10)
	a := &ServingCertApprover{Events: recorder}
	held := []servingHold{{uid: "one", node: "web-control-plane-abc", why: "address 192.0.2.1 is not the Machine's"}}
	a.warnHeld(c, held)
	got := drained(recorder)
	if len(got) != 1 || !strings.Contains(got[0], "Warning") || !strings.Contains(got[0], "web-control-plane-abc") || !strings.Contains(got[0], "192.0.2.1") {
		t.Errorf("events %v", got)
	}
	a.warnHeld(c, held)
	if got := drained(recorder); len(got) != 0 {
		t.Errorf("said again: %v", got)
	}
	a.warnHeld(c, nil)
	a.warnHeld(c, []servingHold{held[0], {uid: "two", node: "web-md-0-def", why: "name evil.example.org is not the Machine's"}})
	if got := drained(recorder); len(got) != 2 {
		t.Errorf("after the first went and came back with another: %v", got)
	}
}

// BeforeClusterCreate says what the controller last said of the cluster, and the fixed text only
// where nothing else says why.
func TestBeforeClusterCreateSaysWhyItHolds(t *testing.T) {
	t.Parallel()
	c := cluster()
	if got := heldBecause(c); got != "waiting for the cluster's Proxmox VE pool, user and token" {
		t.Errorf("nothing said: %q", got)
	}
	c.Status.Conditions = []metav1.Condition{{Type: AddonsWaitingCondition, Status: metav1.ConditionTrue, Reason: AddonsWaitingReason, Message: "kyverno-policies waits for kyverno to be deployed"}}
	if got := heldBecause(c); got != "kyverno-policies waits for kyverno to be deployed" {
		t.Errorf("add-ons waiting: %q", got)
	}
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{Type: KubehengeReadyCondition, Status: metav1.ConditionFalse, Reason: "Failed", Message: "reading the cluster's ClusterClass: gone"})
	if got := heldBecause(c); got != "reading the cluster's ClusterClass: gone" {
		t.Errorf("not reconciled: %q", got)
	}
	hooks, _ := hooksFor(t)
	n := networkCluster("192.0.2.1", 24)
	n.Status.Conditions = c.Status.Conditions
	if resp := callBeforeClusterCreate(t, hooks, n); resp.RetryAfterSeconds == 0 || resp.Message != heldBecause(n) {
		t.Errorf("hook: %+v", resp)
	}
}
