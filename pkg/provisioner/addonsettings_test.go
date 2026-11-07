package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// inside gives the rollout's cluster a workload cluster holding objects.
func (w *rollout) inside(objects ...client.Object) client.Client {
	w.t.Helper()
	workload := fake.NewClientBuilder().WithScheme(newRemoteScheme()).WithObjects(objects...).Build()
	w.r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(w.cluster): {client: workload, until: time.Now().Add(time.Hour)}}}
	return workload
}

func crd(name string, annotations map[string]string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{Name: name, Annotations: annotations}
}

func newProxyObject() *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(helmChartProxyGVK)
	return p
}

func (w *rollout) exists(suffix string) bool {
	w.t.Helper()
	proxies, err := helmChartProxies(w.cluster, &w.r.Charts, "rke2", "local-lvm", CNI{}, w.addons)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, p := range proxies {
		if p.GetName() == w.cluster.Name+"-"+suffix {
			return true
		}
	}
	return false
}

func TestCertManagerInstallRefusedOverAnExistingInstall(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Certificates.Enabled = true
	w.inside(crd("certificates.cert-manager.io", nil))
	w.apply()
	if len(w.addons.conflicts) != 1 {
		t.Fatalf("conflicts %v", w.addons.conflicts)
	}
	for _, want := range []string{"cert-manager is not installed", "certificates.cert-manager.io", "useExisting.certManager"} {
		if !strings.Contains(w.addons.conflicts[0], want) {
			t.Errorf("the conflict lacks %q: %s", want, w.addons.conflicts[0])
		}
	}
	err := w.kube.Get(context.Background(), client.ObjectKey{Namespace: w.cluster.Namespace, Name: w.cluster.Name + "-cert-manager"}, newProxyObject())
	if !apierrors.IsNotFound(err) {
		t.Errorf("cert-manager was installed over the company's: %v", err)
	}
	if err := w.r.recordAddonConflicts(context.Background(), w.cluster, w.addons.conflicts); err != nil {
		t.Fatal(err)
	}
	got := &clusterv1.Cluster{}
	if err := w.kube.Get(context.Background(), client.ObjectKeyFromObject(w.cluster), got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, AddonConflictCondition)
	if cond == nil || cond.Reason != AddonConflictReason {
		t.Errorf("condition %+v", cond)
	}
}

func TestAddonsInstallWhereNothingCollides(t *testing.T) {
	t.Parallel()
	owned := map[string]string{helmReleaseNameAnnotation: "cert-manager", helmReleaseNamespaceAnnotation: "cert-manager"}
	for name, workload := range map[string][]client.Object{
		"no definitions":             nil,
		"definitions Kubehenge owns": {crd("certificates.cert-manager.io", owned)},
		"another software's":         {crd("widgets.example.com", nil)},
		"the other kind it owns":     {crd("clusterissuers.cert-manager.io", owned)},
	} {
		w := newRollout(t, outsideWindow)
		w.addons.Certificates.Enabled = true
		w.inside(workload...)
		w.apply()
		if len(w.addons.conflicts) != 0 || w.version("cert-manager") != testCharts.CertManager.Version {
			t.Errorf("%s: conflicts %v", name, w.addons.conflicts)
		}
	}
}

func TestInstalledAddonOwnsItsDefinitions(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Certificates.Enabled = true
	w.apply()
	w.inside(crd("certificates.cert-manager.io", nil))
	w.apply()
	if len(w.addons.conflicts) != 0 {
		t.Errorf("an installed cert-manager conflicts with itself: %v", w.addons.conflicts)
	}
}

func TestMonitoringAndKyvernoRefuseTheirDefinitionsToo(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Monitoring.Enabled = true
	w.addons.PodSecurity.Kyverno = "baseline"
	w.addons.UseExisting.CertManager = true
	w.inside(crd("prometheuses.monitoring.coreos.com", nil), crd("clusterpolicies.kyverno.io", nil))
	w.apply()
	if len(w.addons.conflicts) != 2 {
		t.Fatalf("conflicts %v", w.addons.conflicts)
	}
}

func TestUseExistingCertManagerInstallsNoChart(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Certificates.Enabled = true
	w.addons.Monitoring.Enabled = true
	if !w.exists("cert-manager") {
		t.Fatal("cert-manager is missing without the option")
	}
	w.addons.UseExisting.CertManager = true
	if w.exists("cert-manager") {
		t.Error("cert-manager is installed beside the company's")
	}
	w.inside(crd("certificates.cert-manager.io", nil))
	w.apply()
	if len(w.addons.conflicts) != 0 {
		t.Errorf("the existing install conflicts: %v", w.addons.conflicts)
	}
	if !w.exists(monitoringAddon) {
		t.Error("monitoring waits for a cert-manager it does not need to install")
	}
}

func TestUseExistingReadsTheVariable(t *testing.T) {
	t.Parallel()
	c := cluster(variableJSON("useExisting", `{"certManager":true}`), variableJSON("namespaceLabels", `{"team":"platform"}`))
	addons, err := addonsOf(c)
	if err != nil || !addons.UseExisting.CertManager || addons.NamespaceLabels["team"] != "platform" {
		t.Errorf("%+v, %v", addons, err)
	}
}

func TestNamespaceLabelsReachTheNamespacesBeforeTheirPods(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.NamespaceLabels = map[string]string{"team": "platform", "pod-security.kubernetes.io/enforce": "privileged"}
	existing := &corev1.Namespace{Name: "headlamp", Labels: map[string]string{"kept": "yes"}}
	workload := w.inside(existing)
	w.apply()

	read := func(name string) *corev1.Namespace {
		ns := &corev1.Namespace{}
		if err := workload.Get(context.Background(), client.ObjectKey{Name: name}, ns); err != nil {
			t.Fatalf("namespace %s: %v", name, err)
		}
		return ns
	}
	for _, name := range []string{"headlamp"} {
		ns := read(name)
		if ns.Labels["team"] != "platform" || ns.Labels["pod-security.kubernetes.io/enforce"] != "privileged" {
			t.Errorf("%s labels %v", name, ns.Labels)
		}
	}
	if read("headlamp").Labels["kept"] != "yes" {
		t.Error("a label of the company's was taken off")
	}
	if w.version("headlamp") == "" {
		t.Error("headlamp is not installed")
	}
	if err := workload.Get(context.Background(), client.ObjectKey{Name: metav1.NamespaceSystem}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Errorf("kube-system was created or labelled by the add-ons: %v", err)
	}

	w.addons.NamespaceLabels = map[string]string{"team": "data"}
	w.apply()
	ns := read("headlamp")
	if ns.Labels["team"] != "data" || ns.Labels["pod-security.kubernetes.io/enforce"] != "" || ns.Labels["kept"] != "yes" {
		t.Errorf("after the change %v", ns.Labels)
	}

	w.addons.NamespaceLabels = nil
	w.apply()
	if ns = read("headlamp"); ns.Labels["team"] != "" || ns.Labels["kept"] != "yes" {
		t.Errorf("after the labels went %v", ns.Labels)
	}
}

func TestNamespaceLabelsWaitForTheClusterAndCreateNamespacesLabelled(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.NamespaceLabels = map[string]string{"team": "platform"}
	// No kubeconfig yet: the add-on waits for the labels rather than create its namespace bare.
	proxies, err := helmChartProxies(w.cluster, &w.r.Charts, "rke2", "local-lvm", CNI{}, &Addons{Headlamp: Headlamp{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := w.r.applyProxies(context.Background(), w.cluster, proxies, w.addons)
	if err != nil || !waiting {
		t.Fatalf("waiting %v, %v", waiting, err)
	}
	if err := w.kube.Get(context.Background(), client.ObjectKey{Namespace: w.cluster.Namespace, Name: "web-headlamp"}, newProxyObject()); err == nil {
		t.Error("headlamp was installed before its namespace carried the labels")
	}

	workload := w.inside()
	w.apply()
	ns := &corev1.Namespace{}
	if err := workload.Get(context.Background(), client.ObjectKey{Name: "headlamp"}, ns); err != nil || ns.Labels["team"] != "platform" {
		t.Errorf("namespace %+v, %v", ns, err)
	}
}

func TestNamespaceLabelsAreHeldToLabelSyntax(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		labels string
		want   string
	}{
		"good":            {`{"team":"platform","example.com/tier":"a-b_c.d","empty":""}`, ""},
		"key":             {`{"-bad":"x"}`, "the label's key"},
		"value":           {`{"team":"has space"}`, "the label's value"},
		"long value":      {`{"team":"` + strings.Repeat("a", 64) + `"}`, "the label's value"},
		"Kubernetes' own": {`{"kubernetes.io/metadata.name":"x"}`, "sets kubernetes.io/metadata.name itself"},
	} {
		errs := validateAddonSettings(nil, cluster(variableJSON("namespaceLabels", c.labels)))
		switch {
		case c.want == "" && len(errs) != 0:
			t.Errorf("%s: %v", name, errs)
		case c.want != "" && !strings.Contains(errs.ToAggregate().Error(), c.want):
			t.Errorf("%s: %v", name, errs)
		}
	}
}

func TestUseExistingCannotChangeAfterCreation(t *testing.T) {
	t.Parallel()
	on := cluster(variableJSON("useExisting", `{"certManager":true}`))
	off := cluster(variableJSON("useExisting", `{"certManager":false}`))
	if errs := validateAddonSettings(nil, on); len(errs) != 0 {
		t.Errorf("creation: %v", errs)
	}
	if errs := validateAddonSettings(off, on); len(errs) != 1 {
		t.Errorf("switching it on: %v", errs)
	}
	if errs := validateAddonSettings(on, on); len(errs) != 0 {
		t.Errorf("an unrelated edit: %v", errs)
	}
}
