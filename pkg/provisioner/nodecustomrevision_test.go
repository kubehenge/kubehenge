package provisioner

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const revisionVars = `{"files":[{"path":"/etc/ssl/ca.crt","secret":{"name":"siem","key":"ca"}}],"auditPolicy":{"configMap":"policy"}}`

type revisionHarness struct {
	t    *testing.T
	r    *Reconciler
	kube client.Client
}

func newRevisionHarness(t *testing.T, class string) *revisionHarness {
	t.Helper()
	c := cluster(variableOf(NodeCustomisationVariable, revisionVars))
	c.Spec.Topology.ClassRef.Name = ClassPrefix + class
	r, kube := newReconciler(t, &fakeCloud{}, c,
		&corev1.Secret{Namespace: "team-a", Name: "siem", Data: map[string][]byte{"ca": []byte("v1")}},
		&corev1.Secret{Namespace: "team-a", Name: "other", Data: map[string][]byte{"x": []byte("1")}},
		&corev1.ConfigMap{Namespace: "team-a", Name: "policy", Data: map[string]string{"policy.yaml": validAuditPolicy}})
	r.Reader = kube
	return &revisionHarness{t: t, r: r, kube: kube}
}

// revision runs the controller's step and reads the Cluster as it leaves it.
func (h *revisionHarness) revision() (string, *clusterv1.Cluster) {
	h.t.Helper()
	revs, c := h.revisions()
	return revs[roleControlPlane] + "/" + revs[roleWorker], c
}

func (h *revisionHarness) revisions() (map[string]string, *clusterv1.Cluster) {
	h.t.Helper()
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil {
		h.t.Fatal(err)
	}
	if err := h.r.ensureNodeCustomisation(context.Background(), c); err != nil {
		h.t.Fatal(err)
	}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil {
		h.t.Fatal(err)
	}
	rev, _, err := optionalVariable[map[string]string](c, NodeCustomisationRevisionVariable)
	if err != nil {
		h.t.Fatal(err)
	}
	return rev, c
}

func (h *revisionHarness) setSecret(name, key, value string) {
	h.t.Helper()
	s := &corev1.Secret{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, s); err != nil {
		h.t.Fatal(err)
	}
	s.Data[key] = []byte(value)
	if err := h.kube.Update(context.Background(), s); err != nil {
		h.t.Fatal(err)
	}
}

func TestChangedSecretContentReplacesTheNodes(t *testing.T) {
	t.Parallel()
	h := newRevisionHarness(t, "kubeadm")
	first, c := h.revision()
	if first == "/" || c.Annotations[nodeSourcesAnnotation] == "" {
		t.Fatalf("no revision written: %q %v", first, c.Annotations)
	}
	if again, _ := h.revision(); again != first {
		t.Errorf("the revision moved without a change: %s to %s", first, again)
	}
	h.setSecret("other", "x", "2")
	if same, _ := h.revision(); same != first {
		t.Errorf("a Secret the variable does not name moved the revision: %s to %s", first, same)
	}
	h.setSecret("siem", "ca", "v2")
	if next, _ := h.revision(); next == first || next == "/" {
		t.Errorf("a rotated Secret left the revision at %s", next)
	}
}

// A file read by workers alone moves the workers' revision and leaves the control planes' as it was,
// and an audit policy does the reverse.
func TestRevisionMovesOnlyForTheRolesThatReadTheSource(t *testing.T) {
	t.Parallel()
	h := newRevisionHarness(t, "kubeadm")
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web"}, c); err != nil {
		t.Fatal(err)
	}
	c.Spec.Topology.Variables = []clusterv1.ClusterVariable{variableOf(NodeCustomisationVariable,
		`{"files":[{"path":"/etc/ssl/ca.crt","roles":["worker"],"secret":{"name":"siem","key":"ca"}}],"auditPolicy":{"configMap":"policy"}}`)}
	if err := h.kube.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	first, _ := h.revisions()
	if first[roleControlPlane] == "" || first[roleWorker] == "" {
		t.Fatalf("revisions %v", first)
	}
	h.setSecret("siem", "ca", "v2")
	next, _ := h.revisions()
	if next[roleControlPlane] != first[roleControlPlane] {
		t.Error("a worker-only file moved the control planes' revision")
	}
	if next[roleWorker] == first[roleWorker] {
		t.Error("a worker-only file left the workers' revision")
	}
	cm := &corev1.ConfigMap{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "policy"}, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["policy.yaml"] = validAuditPolicy + "  - level: None\n"
	if err := h.kube.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	last, _ := h.revisions()
	if last[roleControlPlane] == next[roleControlPlane] || last[roleWorker] != next[roleWorker] {
		t.Errorf("an audit policy moved %v to %v", next, last)
	}
}

func TestChangedPolicyContentReplacesTheNodes(t *testing.T) {
	t.Parallel()
	h := newRevisionHarness(t, "rke2")
	first, _ := h.revision()
	cm := &corev1.ConfigMap{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "policy"}, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["policy.yaml"] = validAuditPolicy + "  - level: None\n"
	if err := h.kube.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if next, _ := h.revision(); next == first {
		t.Error("a changed audit policy left the revision as it was")
	}
}

func TestMissingReferenceIsMarkedAndKeepsTheRevision(t *testing.T) {
	t.Parallel()
	h := newRevisionHarness(t, "kubeadm")
	first, _ := h.revision()
	if err := h.kube.Delete(context.Background(), &corev1.Secret{Namespace: "team-a", Name: "siem"}); err != nil {
		t.Fatal(err)
	}
	rev, c := h.revision()
	cond := meta.FindStatusCondition(c.Status.Conditions, NodeCustomisationCondition)
	if rev != first || cond == nil || cond.Reason != NodeCustomisationMissing || cond.Status != "True" {
		t.Fatalf("revision %q (was %q), condition %v", rev, first, cond)
	}
	h.setSecretCreate()
	if _, c = h.revision(); meta.FindStatusCondition(c.Status.Conditions, NodeCustomisationCondition) != nil {
		t.Error("the condition stays after the Secret is back")
	}
}

func (h *revisionHarness) setSecretCreate() {
	h.t.Helper()
	if err := h.kube.Create(context.Background(), &corev1.Secret{Namespace: "team-a", Name: "siem", Data: map[string][]byte{"ca": []byte("v1")}}); err != nil {
		h.t.Fatal(err)
	}
}

func TestNoRevisionWithoutReferences(t *testing.T) {
	t.Parallel()
	c := cluster(variableOf(NodeCustomisationVariable, `{"files":[{"path":"/etc/x","content":"x"}]}`))
	c.Spec.Topology.ClassRef.Name = ClassPrefix + "kubeadm"
	r, kube := newReconciler(t, &fakeCloud{}, c)
	r.Reader = kube
	if err := r.ensureNodeCustomisation(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if _, had, _ := optionalVariable[map[string]string](got, NodeCustomisationRevisionVariable); had {
		t.Error("a revision for a cluster that names nothing")
	}
}
