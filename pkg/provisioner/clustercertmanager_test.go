package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// clusterOwnCertManager is a cert-manager installed by the cluster's user: its definitions serving
// cert-manager.io/v1, and its controller and webhook in namespace cert-manager, Available, labelled
// as the static manifest and the chart label them.
type clusterOwnCertManager struct {
	crds                []*apiextensionsv1.CustomResourceDefinition
	controller, webhook *appsv1.Deployment
}

func newClusterOwnCertManager(annotations map[string]string) clusterOwnCertManager {
	served := func(name string) *apiextensionsv1.CustomResourceDefinition {
		c := crd(name, annotations)
		c.Spec.Versions = []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true}}
		return c
	}
	deployment := func(name, component string) *appsv1.Deployment {
		return &appsv1.Deployment{
			Namespace: certManagerAddon, Name: name, Annotations: annotations,
			Labels: map[string]string{
				"app.kubernetes.io/name": name, "app.kubernetes.io/component": component,
				"app.kubernetes.io/instance": certManagerAddon, "app.kubernetes.io/version": "v1.19.2",
			},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: name, Image: "quay.io/jetstack/" + name + ":v1.19.2", Args: []string{"--cluster-resource-namespace=$(POD_NAMESPACE)"},
			}}}}},
			Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}},
		}
	}
	controller := deployment(certManagerAddon, "controller")
	webhook := deployment("webhook", "webhook")
	webhook.Name = "cert-manager-webhook"
	return clusterOwnCertManager{
		crds:       []*apiextensionsv1.CustomResourceDefinition{served("certificates.cert-manager.io"), served("clusterissuers.cert-manager.io")},
		controller: controller, webhook: webhook,
	}
}

func (m clusterOwnCertManager) objects() []client.Object {
	out := []client.Object{}
	for _, c := range m.crds {
		out = append(out, c)
	}
	if m.controller != nil {
		out = append(out, m.controller)
	}
	if m.webhook != nil {
		out = append(out, m.webhook)
	}
	return out
}

// certManagerUserCluster is a new cluster with monitoring and logs, as the interface sends it,
// whose workload cluster holds objects.
func certManagerUserCluster(t *testing.T, objects ...client.Object) (*Reconciler, client.Client, client.Client) {
	t.Helper()
	c := certManagerUser()
	r, kube := newReconciler(t, &fakeCloud{}, c)
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(objects...).WithStatusSubresource(&appsv1.Deployment{}).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	return r, kube, workload
}

// certManagerUser is the cluster certManagerUserCluster makes.
func certManagerUser() *clusterv1.Cluster {
	c := withAddons(storageCluster(), map[string]string{"monitoring": formMonitoring, "logs": formLogs})
	c.Annotations = map[string]string{gatewayAPIAnnotation: "v1.6.3"}
	return c
}

func clusterCondition(t *testing.T, kube client.Client, c *clusterv1.Cluster, condition string) *metav1.Condition {
	t.Helper()
	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(got.Status.Conditions, condition)
}

// A cert-manager of the cluster's own that works serves Kubehenge's add-ons, however it was
// installed: Kubehenge installs none, changes nothing of it, and its users install at once.
func TestClusterCertManagerThatWorksIsUsed(t *testing.T) {
	t.Parallel()
	for name, m := range map[string]clusterOwnCertManager{
		"static manifest":      newClusterOwnCertManager(nil),
		"user's Helm release":  newClusterOwnCertManager(helmInstalled),
		"version from the tag": withoutVersionLabel(newClusterOwnCertManager(map[string]string{"operator": "other"})),
	} {
		r, kube, workload := certManagerUserCluster(t, m.objects()...)
		c := certManagerUser()
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
		if proxyExists(kube, c, certManagerProxy(c)) {
			t.Errorf("%s: Kubehenge installed a cert-manager beside the cluster's own", name)
		}
		for _, p := range []string{monitoringProxy(c), lokiProxy(c), alloyProxy(c), alloyNodeProxy(c)} {
			if !proxyExists(kube, c, p) {
				t.Errorf("%s: %s waits although the cluster's cert-manager works", name, p)
			}
		}
		for _, condition := range []string{AddonConflictCondition, AddonsWaitingCondition} {
			if cond := clusterCondition(t, kube, c, condition); cond != nil {
				t.Errorf("%s: %+v", name, cond)
			}
		}
		for _, o := range m.objects() {
			got, ok := o.DeepCopyObject().(client.Object)
			if !ok {
				t.Fatalf("%s: %T", o.GetName(), o)
			}
			if err := workload.Get(context.Background(), client.ObjectKeyFromObject(o), got); err != nil {
				t.Errorf("%s: %s: %v", name, o.GetName(), err)
			} else if got.GetResourceVersion() != "999" {
				t.Errorf("%s: %s was changed, resource version %s", name, o.GetName(), got.GetResourceVersion())
			}
		}
	}
}

func withoutVersionLabel(m clusterOwnCertManager) clusterOwnCertManager {
	delete(m.controller.Labels, "app.kubernetes.io/version")
	m.controller.Spec.Template.Spec.Containers[0].Image = "registry.example.org/cert-manager-controller:v1.18.0@sha256:0123"
	return m
}

// A cert-manager of the cluster's own that does not work is a conflict that says what is wrong,
// and its users wait with it. Nothing is installed over it.
func TestClusterCertManagerThatDoesNotWorkIsAConflict(t *testing.T) {
	t.Parallel()
	for want, broken := range map[string]func(*clusterOwnCertManager){
		"It has no controller Deployment, as after an uninstall": func(m *clusterOwnCertManager) { m.controller, m.webhook = nil, nil },
		"Its webhook cert-manager/cert-manager-webhook is not Available": func(m *clusterOwnCertManager) {
			m.webhook.Status.Conditions[0].Status = corev1.ConditionFalse
		},
		"Its controller cert-manager/cert-manager is not Available": func(m *clusterOwnCertManager) { m.controller.Status.Conditions = nil },
		"It has no webhook Deployment in cert-manager":              func(m *clusterOwnCertManager) { m.webhook = nil },
		"It is v1.14.5, older than v1.15.0": func(m *clusterOwnCertManager) {
			m.controller.Labels["app.kubernetes.io/version"] = "v1.14.5"
		},
		"It is v1.14.0, older than v1.15.0": func(m *clusterOwnCertManager) {
			delete(m.controller.Labels, "app.kubernetes.io/version")
			m.controller.Spec.Template.Spec.Containers[0].Image = "registry.example.org:5000/cert-manager-controller:v1.14.0@sha256:0123"
		},
		"It does not serve clusterissuers.cert-manager.io at v1": func(m *clusterOwnCertManager) { m.crds[1].Spec.Versions[0].Served = false },
		"It keeps the cluster issuers' Secrets in security, not in cert-manager": func(m *clusterOwnCertManager) {
			m.controller.Spec.Template.Spec.Containers[0].Args = []string{"--cluster-resource-namespace=security"}
		},
	} {
		m := newClusterOwnCertManager(nil)
		broken(&m)
		r, kube, _ := certManagerUserCluster(t, m.objects()...)
		c := certManagerUser()
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{certManagerProxy(c), monitoringProxy(c), lokiProxy(c)} {
			if proxyExists(kube, c, p) {
				t.Errorf("%s: %s installed", want, p)
			}
		}
		cond := clusterCondition(t, kube, c, AddonConflictCondition)
		if cond == nil || cond.Reason != AddonConflictReason || !strings.Contains(cond.Message, want+". Remove it, or make it work") ||
			!strings.Contains(cond.Message, "monitoring, loki, alloy and alloy-node wait") {
			t.Errorf("%s: conflict %+v", want, cond)
		}
	}
}

// A cert-manager of the cluster's own that becomes ready clears its conflict on the next look, and
// one its user removes is replaced by Kubehenge's.
func TestClusterCertManagerConflictClearsAndKubehengesFollowsItsRemoval(t *testing.T) {
	t.Parallel()
	m := newClusterOwnCertManager(nil)
	m.webhook.Status.Conditions = nil
	r, kube, workload := certManagerUserCluster(t, m.objects()...)
	c := certManagerUser()
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if clusterCondition(t, kube, c, AddonConflictCondition) == nil {
		t.Fatal("a webhook that is not Available gives no conflict")
	}
	m.webhook.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
	if err := workload.Status().Update(context.Background(), m.webhook); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if cond := clusterCondition(t, kube, c, AddonConflictCondition); cond != nil {
		t.Errorf("the conflict stays once the webhook is Available: %+v", cond)
	}
	if proxyExists(kube, c, certManagerProxy(c)) || !proxyExists(kube, c, monitoringProxy(c)) {
		t.Error("the cluster's cert-manager works and is not used")
	}
	for _, o := range m.objects() {
		if err := workload.Delete(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if !proxyExists(kube, c, certManagerProxy(c)) {
		t.Error("no cert-manager of Kubehenge's once the cluster's own is removed")
	}
}
