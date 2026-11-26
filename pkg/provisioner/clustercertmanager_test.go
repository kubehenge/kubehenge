package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
		"Its version cannot be read": func(m *clusterOwnCertManager) {
			delete(m.controller.Labels, "app.kubernetes.io/version")
			m.controller.Spec.Template.Spec.Containers[0].Image = "quay.io/jetstack/cert-manager-controller@sha256:0123"
		},
		"Its version cannot be read. Remove": func(m *clusterOwnCertManager) {
			m.controller.Labels["app.kubernetes.io/version"] = "latest"
		},
		"It runs in security, not in cert-manager": func(m *clusterOwnCertManager) {
			m.controller.Namespace, m.webhook.Namespace = "security", "security"
			m.controller.Spec.Template.Spec.Containers[0].Args = []string{"--cluster-resource-namespace=cert-manager"}
		},
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
		line := strings.TrimSuffix(want, ". Remove") + ". Remove it, or make it work"
		if cond == nil || cond.Reason != AddonConflictReason || !strings.Contains(cond.Message, line) ||
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

// failSwitch is the error a failing workload cluster's reads return, none while it is nil.
type failSwitch struct{ err error }

// failing is a workload cluster holding objects whose reads fail while fail holds an error.
func failing(t *testing.T, r *Reconciler, c *clusterv1.Cluster, fail *failSwitch, objects ...client.Object) {
	t.Helper()
	inner := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(objects...).WithStatusSubresource(&appsv1.Deployment{}).Build()
	workload := interceptor.NewClient(inner, interceptor.Funcs{
		Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if fail.err != nil {
				return fail.err
			}
			return w.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if fail.err != nil {
				return fail.err
			}
			return w.List(ctx, list, opts...)
		},
	})
	// A cluster forgotten as unreachable is looked up again in the management cluster, which holds no
	// kubeconfig for it.
	r.Remotes = &Remotes{Reader: r.Client, byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
}

func storedValues(t *testing.T, kube client.Client, c *clusterv1.Cluster, name string) string {
	t.Helper()
	p := newProxyObject()
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: name}, p); err != nil {
		t.Fatal(err)
	}
	return specText(p.Object["spec"], "valuesTemplate")
}

// A read of the cluster for a cert-manager of its own that fails or cannot be made holds
// Kubehenge's back with a waiting line, and is made again on the next reconcile: Kubehenge's release
// would upgrade or replace the user's. Users already installed stay as they are meanwhile.
func TestCertManagerWaitsForACompleteRead(t *testing.T) {
	t.Parallel()
	refused := errors.New("dial tcp 192.0.2.1:6443: connect: connection refused")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", errors.New("denied"))
	for name, failure := range map[string]error{"refused": refused, "forbidden": forbidden} {
		m := newClusterOwnCertManager(nil)
		r, kube := newReconciler(t, &fakeCloud{}, certManagerUser())
		c := certManagerUser()
		fail := &failSwitch{}
		failing(t, r, c, fail, m.objects()...)
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
		before := storedValues(t, kube, c, monitoringProxy(c))
		fail.err = failure
		for pass := range 2 {
			if err := reconcile(t, r, c); err != nil {
				t.Fatalf("%s, pass %d: %v", name, pass, err)
			}
			failing(t, r, c, fail, m.objects()...)
		}
		if proxyExists(kube, c, certManagerProxy(c)) {
			t.Errorf("%s: Kubehenge installed its cert-manager on a read that failed", name)
		}
		if got := storedValues(t, kube, c, monitoringProxy(c)); got != before {
			t.Errorf("%s: monitoring changed while the cluster could not be read", name)
		}
		cond := clusterCondition(t, kube, c, AddonsWaitingCondition)
		if cond == nil || cond.Reason != AddonsWaitingForCertManager || !strings.Contains(cond.Message, "wait for cert-manager, which "+certManagerUnreadWait) {
			t.Errorf("%s: waiting %+v", name, cond)
		}
		fail.err = nil
		if err := reconcile(t, r, c); err != nil {
			t.Fatal(err)
		}
		if proxyExists(kube, c, certManagerProxy(c)) || clusterCondition(t, kube, c, AddonsWaitingCondition) != nil {
			t.Errorf("%s: the cluster's cert-manager is not used once it can be read", name)
		}
	}
	// A cluster with no kubeconfig yet cannot be read either.
	r, kube := newReconciler(t, &fakeCloud{}, certManagerUser())
	c := certManagerUser()
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if proxyExists(kube, c, certManagerProxy(c)) {
		t.Error("Kubehenge installed its cert-manager in a cluster it cannot read")
	}
	if cond := clusterCondition(t, kube, c, AddonsWaitingCondition); cond == nil || !strings.Contains(cond.Message, certManagerUnreadWait) {
		t.Errorf("waiting %+v", cond)
	}
}

// A switch between the cluster's own cert-manager and Kubehenge's changes the values of the
// releases that make cert-manager objects, so that the Add-on Provider for Helm applies them again
// and the objects the removed one took with its definitions come back.
func TestCertManagerSwitchAppliesItsUsersAgain(t *testing.T) {
	t.Parallel()
	m := newClusterOwnCertManager(nil)
	r, kube, workload := certManagerUserCluster(t, m.objects()...)
	c := certManagerUser()
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	users := []string{monitoringProxy(c), lokiProxy(c), alloyProxy(c), alloyNodeProxy(c)}
	own := map[string]string{}
	for _, p := range users {
		own[p] = storedValues(t, kube, c, p)
		if !strings.Contains(own[p], certManagerServedBy+": "+servedByCluster) || strings.Contains(own[p], certManagerServedBy+": "+servedByKubehenge) {
			t.Errorf("%s does not name the cluster's cert-manager", p)
		}
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
		t.Fatal("no cert-manager of Kubehenge's once the cluster's own is removed")
	}
	for _, p := range users {
		got := storedValues(t, kube, c, p)
		if got == own[p] || !strings.Contains(got, certManagerServedBy+": "+servedByKubehenge) {
			t.Errorf("%s is not applied again for Kubehenge's cert-manager", p)
		}
	}
}

// The certificates add-on's settings that only Kubehenge's cert-manager carries are said to be not
// applied where the cluster's own cert-manager is used.
func TestCertificatesSettingsBesideTheClustersCertManager(t *testing.T) {
	t.Parallel()
	w := newRollout(t, outsideWindow)
	w.addons.Certificates = Certificates{Enabled: true, Domain: "lab.example.org", DNS01: DNS01{Provider: "cloudflare", Nameservers: []string{"1.1.1.1:53"}}}
	w.addons.viaProxy = egressEnv{url: "http://proxy.example.org:3128"}
	w.inside(newClusterOwnCertManager(nil).objects()...)
	w.apply()
	want := "certificates uses the cluster's own cert-manager, so the default issuer for tls-acme Ingresses, dns01.nameservers and the egress proxy are not applied. Set them on that cert-manager"
	if len(w.addons.conflicts) != 1 || w.addons.conflicts[0].reason != AddonConflictSettingsReason || w.addons.conflicts[0].message != want {
		t.Errorf("conflicts %+v", w.addons.conflicts)
	}
	w.addons.Certificates = Certificates{}
	w.apply()
	if len(w.addons.conflicts) != 0 {
		t.Errorf("certificates off: conflicts %+v", w.addons.conflicts)
	}
}
