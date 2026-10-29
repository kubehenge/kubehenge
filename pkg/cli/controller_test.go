package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

const (
	chartDigest = "@sha256:0805a5273118c436c9772bb9566d87ec558d8b25cc2ce5dc32918ecdd36f2c7c"
	testToken   = "kubehenge@pve!test=00000000-0000-0000-0000-000000000000" //nolint:gosec // test data
)

// validControllerOptions are the controller's flags as the chart passes them, every chart and image
// pinned, with a connection to a Proxmox VE at url whose CA is caFile.
func validControllerOptions(url, caFile string) (*controllerOptions, *pve.Config) {
	o := &controllerOptions{
		kubeRBACProxy: "ghcr.io/example/proxy:v1" + chartDigest, kubeVipImage: "ghcr.io/example/vip:v1" + chartDigest,
		turtlesImage: "docker.io/example/turtles:v1" + chartDigest, turtlesCAPIImage: "registry.example.test/example/capi:v1" + chartDigest,
		alloyImage:  "docker.io/example/alloy:v1" + chartDigest,
		veleroImage: "docker.io/example/velero:v1" + chartDigest, veleroPlugin: "docker.io/example/plugin:v1" + chartDigest,
		calicoImages:      []string{"quay.io/calico/node:v3.33.0" + chartDigest},
		autoscalerImages:  []string{"registry.k8s.io/autoscaling/cluster-autoscaler:v1.36.1" + chartDigest},
		chartValues:       "{}",
		podSecurityExempt: "{}",
		rancherKubernetes: "34-37",
	}
	for _, f := range []*string{
		&o.ccmChart, &o.csiChart, &o.csiNewestChart, &o.snapshotChart, &o.ciliumChart, &o.calicoChart, &o.headlampChart, &o.certManagerChart, &o.rancherChart,
		&o.monitoringChart, &o.lokiChart, &o.alloyChart, &o.veleroChart, &o.kyvernoChart, &o.kyvernoPolicies, &o.istioChart,
		&o.istioBaseChart, &o.istioCNIChart, &o.istioZtunnelChart, &o.traefikChart, &o.metalLBChart, &o.kubeVipChart, &o.kubeVipCloudChart,
		&o.argoCDChart, &o.fluxOperatorChart,
	} {
		*f = "https://charts.example.test/chart:1.0.0" + chartDigest
	}
	o.r.WorkloadPool = "kubehenge"
	o.r.ClassNamespace = "kubehenge-system"
	o.autoscaler.Namespace = "kubehenge-autoscaler"
	return o, &pve.Config{URL: url, CAFile: caFile, Token: testToken}
}

func TestStrictJSONRefusesWhatTheFlagCannotHold(t *testing.T) {
	t.Parallel()
	var exempt provisioner.PodSecurityExempt
	if err := strictJSON(`{"always":["kube-system"],"calico":["calico-system"]}`, &exempt); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(exempt.Always, []string{"kube-system"}) || !slices.Equal(exempt.Calico, []string{"calico-system"}) {
		t.Errorf("decoded %+v", exempt)
	}
	for value, want := range map[string]string{
		// A misspelt key would leave its namespaces held to the cluster's level without a word.
		`{"alwys":["kube-system"]}`: `unknown field "alwys"`,
		// So would the part after a brace closed too early.
		`{"always":["kube-system"]},"calico":["calico-system"]}`: "after the first JSON value",
		`["kube-system"]`: "cannot unmarshal array",
		``:                "EOF",
	} {
		var into provisioner.PodSecurityExempt
		if err := strictJSON(value, &into); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("strictJSON(%q) gave %v, %+v; want %q", value, err, into, want)
		}
	}
}

func TestControllerSchemeHoldsEveryKindTheControllersUse(t *testing.T) {
	t.Parallel()
	scheme, err := controllerScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []runtime.Object{
		&clusterv1.Cluster{}, &clusterv1.ClusterClass{}, &clusterv1.Machine{}, &clusterv1.MachineDeployment{}, &clusterv1.MachineHealthCheck{},
		&addonsv1.ClusterResourceSet{},
		&corev1.Secret{}, &corev1.ConfigMap{}, &corev1.ServiceAccount{}, &corev1.Namespace{}, &corev1.Node{}, &corev1.PersistentVolume{},
		&corev1.Pod{}, &corev1.Service{}, &corev1.Event{},
		&appsv1.Deployment{}, &appsv1.DaemonSet{}, &batchv1.Job{}, &rbacv1.Role{}, &rbacv1.RoleBinding{}, &rbacv1.ClusterRole{},
		&certificatesv1.CertificateSigningRequest{}, &coordinationv1.Lease{}, &eventsv1.Event{},
	} {
		if kinds, _, err := scheme.ObjectKinds(o); err != nil || len(kinds) == 0 {
			t.Errorf("the controller's scheme does not know %T: %v", o, err)
		}
	}
}

func TestCompleteReconcilerGrantsWhatTheProxmoxSettingsDo(t *testing.T) {
	t.Parallel()
	o, _ := validControllerOptions("https://pve.example.test:8006", "")
	o.settings.DatastoreRole = "KubehengeDatastore"
	o.settings.SysAuditGroup = "kubehenge-audit"
	cloud := &provisioner.Proxmox{Settings: o.settings}
	endpoint := provisioner.Endpoint{URL: "https://pve.example.test:8006", CA: "ca"}
	minors := provisioner.KubernetesMinors{Min: 34, Max: 37}

	o.completeReconciler(cloud, endpoint, minors)
	r := &o.r
	if r.Cloud != cloud || r.Endpoint != endpoint || r.RancherKubernetes != minors {
		t.Errorf("the reconciler has cloud %p, endpoint %+v, Rancher %+v", r.Cloud, r.Endpoint, r.RancherKubernetes)
	}
	if r.DatastoreAllocate.Role != "KubehengeDatastore" || r.SysAuditGroup != "kubehenge-audit" || r.VolumeSnapshots.Namespace != "kubehenge-system" {
		t.Errorf("the reconciler grants Datastore.Allocate as %q, Sys.Audit through %q, and reads root's Secret in %q",
			r.DatastoreAllocate.Role, r.SysAuditGroup, r.VolumeSnapshots.Namespace)
	}
}

// The controller checks every flag before it connects to the management cluster, so that a
// mistake in the chart's values stops it with the flag's name rather than a crash loop of its
// controllers.
func TestControllerRefusesAFlagBeforeItStarts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	caFile, _ := writeCertificate(t, dir)
	for _, c := range []struct {
		name   string
		change func(*controllerOptions, *pve.Config)
		want   string
	}{
		{"no CA", func(_ *controllerOptions, conn *pve.Config) { conn.CAFile = "" }, "--proxmox-ca is required"},
		{"a pool too deep", func(o *controllerOptions, _ *pve.Config) { o.r.WorkloadPool = "a/b/c/d/e/f/g/h" }, "--workload-pool"},
		{"a CA file missing", func(_ *controllerOptions, conn *pve.Config) { conn.CAFile = filepath.Join(dir, "missing.pem") }, "--proxmox-ca:"},
		{"a token without its secret", func(_ *controllerOptions, conn *pve.Config) { conn.Token = "kubehenge@pve!controller" }, "connecting to Proxmox VE"},
		{"a chart without its digest", func(o *controllerOptions, _ *pve.Config) { o.ccmChart = "https://charts.example.test/ccm:1.0.0" }, "--ccm-chart"},
		{"a misspelt exemption", func(o *controllerOptions, _ *pve.Config) { o.podSecurityExempt = `{"alwys":[]}` }, "--pod-security-exempt"},
		{"an exemption cut short", func(o *controllerOptions, _ *pve.Config) {
			o.podSecurityExempt = `{"always":["kube-system"]},"calico":["calico-system"]}`
		}, "--pod-security-exempt"},
		{"Rancher's Kubernetes", func(o *controllerOptions, _ *pve.Config) { o.rancherKubernetes = "37-34" }, "--rancher-kubernetes"},
		{"class providers", func(o *controllerOptions, _ *pve.Config) { o.classProviders = []string{"kubehenge-k3s"} }, "--class-providers"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o, conn := validControllerOptions("https://pve.example.test:8006", caFile)
			c.change(o, conn)
			if err := o.run(t.Context(), conn); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("run gave %v; want %q", err, c.want)
			}
		})
	}
}
