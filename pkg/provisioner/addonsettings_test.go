package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
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

func TestCertManagerInstallRefusedOverAnExistingInstall(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Certificates.Enabled = true
	w.inside(crd("certificates.cert-manager.io", nil))
	w.apply()
	conflict := w.addons.certManagerConflict
	for _, want := range []string{"cert-manager is not installed", "certificates.cert-manager.io", "Remove it"} {
		if !strings.Contains(conflict.message, want) {
			t.Errorf("the conflict lacks %q: %s", want, conflict.message)
		}
	}
	if strings.Contains(conflict.message, "useExisting") {
		t.Errorf("the conflict offers a setting that is gone: %s", conflict.message)
	}
	err := w.kube.Get(context.Background(), client.ObjectKey{Namespace: w.cluster.Namespace, Name: w.cluster.Name + "-cert-manager"}, newProxyObject())
	if !apierrors.IsNotFound(err) {
		t.Errorf("cert-manager was installed over the company's: %v", err)
	}
	if err := w.r.recordAddonConflicts(context.Background(), w.cluster, []addonConflict{conflict}); err != nil {
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

// certManagerDeployment is a cert-manager controller's Deployment in namespace, as its chart and
// its static manifest label it.
func certManagerDeployment(namespace string, annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		Namespace: namespace, Name: "cert-manager", Annotations: annotations,
		Labels: map[string]string{"app.kubernetes.io/name": "cert-manager", "app.kubernetes.io/component": "controller"},
	}
}

// A cert-manager installed without its definitions still runs a controller that would fight
// Kubehenge's: the conflict names its Deployment. Kubehenge's own is no conflict.
func TestCertManagerInstallRefusedBesideAForeignDeployment(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Certificates.Enabled = true
	w.inside(certManagerDeployment("security", nil))
	w.apply()
	if !strings.Contains(w.addons.certManagerConflict.message, "Deployment security/cert-manager") {
		t.Errorf("conflict %q", w.addons.certManagerConflict.message)
	}
	if err := w.kube.Get(context.Background(), client.ObjectKey{Namespace: w.cluster.Namespace, Name: w.cluster.Name + "-cert-manager"}, newProxyObject()); !apierrors.IsNotFound(err) {
		t.Errorf("cert-manager was installed beside the cluster's own: %v", err)
	}
	if err := w.r.recordAddonConflicts(context.Background(), w.cluster, []addonConflict{w.addons.certManagerConflict}); err != nil {
		t.Fatal(err)
	}
	got := &clusterv1.Cluster{}
	if err := w.kube.Get(context.Background(), client.ObjectKeyFromObject(w.cluster), got); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, AddonConflictCondition); cond == nil || cond.Reason != AddonConflictDeploymentReason {
		t.Errorf("a Deployment with no definitions: condition %+v, want reason %s", cond, AddonConflictDeploymentReason)
	}

	own := newRollout(t, outsideWindow)
	own.addons.Certificates.Enabled = true
	kubehengesDeployment := certManagerDeployment("cert-manager", helmInstalled)
	kubehengesDeployment.Labels[addonLabel] = certManagerAddon
	own.inside(kubehengesDeployment)
	own.apply()
	if own.addons.certManagerConflict.message != "" || own.version("cert-manager") != testCharts.CertManager.Version {
		t.Errorf("Kubehenge's own cert-manager conflicts: %q", own.addons.certManagerConflict.message)
	}
}

// kubehengesCertManager labels an object of Kubehenge's cert-manager release.
var kubehengesCertManager = map[string]string{addonLabel: certManagerAddon}

// helmInstalled annotates an object as Helm does for release cert-manager in namespace cert-manager,
// the name cert-manager's own install command gives it.
var helmInstalled = map[string]string{helmReleaseNameAnnotation: "cert-manager", helmReleaseNamespaceAnnotation: "cert-manager"}

// A cert-manager installed the usual Helm way has the release name and namespace of Kubehenge's.
// It is still the cluster's own: one conflict line, nothing installed over it and nothing deleted.
func TestCertManagerInstalledByHandUnderTheSameNameIsForeign(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Certificates.Enabled = true
	deployment := certManagerDeployment("cert-manager", helmInstalled)
	certificates, issuers := crd("certificates.cert-manager.io", helmInstalled), crd("clusterissuers.cert-manager.io", helmInstalled)
	workload := w.inside(deployment, certificates, issuers)
	w.apply()
	if !strings.Contains(w.addons.certManagerConflict.message, "certificates.cert-manager.io, which no Kubehenge release owns") ||
		w.addons.certManagerConflict.reason != AddonConflictReason {
		t.Errorf("conflict %+v", w.addons.certManagerConflict)
	}
	if err := w.kube.Get(context.Background(), client.ObjectKey{Namespace: w.cluster.Namespace, Name: w.cluster.Name + "-cert-manager"}, newProxyObject()); !apierrors.IsNotFound(err) {
		t.Errorf("cert-manager was installed over the cluster's own release: %v", err)
	}
	for _, o := range []client.Object{deployment, certificates, issuers} {
		if err := workload.Get(context.Background(), client.ObjectKeyFromObject(o), o); err != nil {
			t.Errorf("%s: %v", o.GetName(), err)
		}
	}
}

// Kubehenge's cert-manager release labels its objects, which is how a later look tells it apart.
func TestKubehengesCertManagerLabelsItsObjects(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Certificates.Enabled = true
	w.apply()
	p := newProxyObject()
	if err := w.kube.Get(context.Background(), client.ObjectKey{Namespace: w.cluster.Namespace, Name: w.cluster.Name + "-cert-manager"}, p); err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(specText(p.Object["spec"], "valuesTemplate")), &values); err != nil {
		t.Fatal(err)
	}
	if labels := lookup[map[string]any](t, values, "global", "commonLabels"); labels[addonLabel] != certManagerAddon {
		t.Errorf("cert-manager's common labels %v lack %s", labels, addonLabel)
	}
}

func TestAddonsInstallWhereNothingCollides(t *testing.T) {
	t.Parallel()
	owned := func(name string) *apiextensionsv1.CustomResourceDefinition {
		o := crd(name, helmInstalled)
		o.Labels = kubehengesCertManager
		return o
	}
	for name, workload := range map[string][]client.Object{
		"no definitions":             nil,
		"definitions Kubehenge owns": {owned("certificates.cert-manager.io")},
		"another software's":         {crd("widgets.example.com", nil)},
		"the other kind it owns":     {owned("clusterissuers.cert-manager.io")},
	} {
		w := newRollout(t, outsideWindow)
		w.addons.Certificates.Enabled = true
		w.inside(workload...)
		w.apply()
		if w.addons.certManagerConflict.message != "" || w.version("cert-manager") != testCharts.CertManager.Version {
			t.Errorf("%s: conflict %q", name, w.addons.certManagerConflict.message)
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
	if w.addons.certManagerConflict.message != "" {
		t.Errorf("an installed cert-manager conflicts with itself: %s", w.addons.certManagerConflict.message)
	}
}

func TestMonitoringAndKyvernoRefuseTheirDefinitionsToo(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Monitoring.Enabled = true
	w.addons.PodSecurity.Kyverno = "baseline"
	w.inside(crd("prometheuses.monitoring.coreos.com", nil), crd("clusterpolicies.kyverno.io", nil))
	w.apply()
	if len(w.addons.conflicts) != 2 {
		t.Fatalf("conflicts %v", w.addons.conflicts)
	}
}

func TestNamespaceLabelsAreRead(t *testing.T) {
	t.Parallel()
	addons, err := addonsOf(cluster(variableJSON("namespaceLabels", `{"team":"platform"}`)))
	if err != nil || addons.NamespaceLabels["team"] != "platform" {
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
	w.r.Remotes = nil
	proxies, err := helmChartProxies(w.cluster, &w.r.Charts, "rke2", "local-lvm", CNI{}, &Addons{Headlamp: Headlamp{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := w.r.applyProxies(context.Background(), w.cluster, proxies, w.addons)
	if err != nil || !waiting {
		t.Fatalf("waiting %v, %v", waiting, err)
	}
	if want := "the add-ons in headlamp wait for the cluster's API to label their namespaces"; len(w.addons.waits) != 1 || w.addons.waits[0].line != want {
		t.Errorf("waiting lines %+v, want %q", w.addons.waits, want)
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
		errs := validateAddonSettings(cluster(variableJSON("namespaceLabels", c.labels)))
		switch {
		case c.want == "" && len(errs) != 0:
			t.Errorf("%s: %v", name, errs)
		case c.want != "" && !strings.Contains(errs.ToAggregate().Error(), c.want):
			t.Errorf("%s: %v", name, errs)
		}
	}
}
