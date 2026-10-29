package provisioner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// sharedKubernetes is a Kubernetes release both Talos releases run.
func sharedKubernetes(t *testing.T, a, b string) string {
	t.Helper()
	minorsA, err := templates.TalosKubernetesMinors(a)
	if err != nil {
		t.Fatal(err)
	}
	minorsB, err := templates.TalosKubernetesMinors(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range minorsA {
		if slices.Contains(minorsB, m) {
			return fmt.Sprintf("v1.%d.0", m)
		}
	}
	t.Fatalf("Talos %s and %s run no Kubernetes minor in common", a, b)
	return ""
}

func TestCheckTalosHoldsTheReleaseToTalosRules(t *testing.T) {
	t.Parallel()
	with := func(talos, kubernetes string) *clusterv1.Cluster {
		c := talosCluster("web", talos)
		c.Spec.Topology.Version = kubernetes
		return c
	}
	both := sharedKubernetes(t, "v1.13.5", "v1.14.4")
	for _, tc := range []struct {
		name    string
		old, c  *clusterv1.Cluster
		refused string
	}{
		{"another distribution", nil, settledCluster("web", "v1.20.0+rke2r1", nil), ""},
		{"a release and the Kubernetes it runs", nil, with("v1.14.4", templates.TalosDefaultKubernetes), ""},
		{"a Kubernetes the release does not run", nil, with("v1.14.4", "v1.20.0"), "does not run"},
		{"no release", nil, settledCluster("web", "v1.36.5", nil), ""},
		{"the next minor, on a Kubernetes both run", with("v1.13.5", both), with("v1.14.4", both), ""},
		{"two minors at once", with("v1.12.6", templates.TalosDefaultKubernetes), with("v1.14.4", templates.TalosDefaultKubernetes), "to the newest 1.13 release first"},
		{"back", with("v1.14.4", both), with("v1.13.5", both), "cannot downgrade"},
		{"the same release, the Kubernetes changed", with("v1.14.4", both), with("v1.14.4", templates.TalosDefaultKubernetes), ""},
	} {
		if tc.name == "no release" {
			tc.c.Spec.Topology.ClassRef.Name = ClassPrefix + "talos"
			tc.refused = TalosVersionVariable
		}
		err := checkTalos(tc.old, tc.c)
		if (err == nil) != (tc.refused == "") || err != nil && !strings.Contains(err.Error(), tc.refused) {
			t.Errorf("%s: %v, want it refused for %q", tc.name, err, tc.refused)
		}
	}
}

func TestTalosSnapshotsComeFromAControlPlaneThatAnswers(t *testing.T) {
	t.Parallel()
	c := talosCluster("web", "v1.14.4")
	noAddress := talosMachine("web-cp-0", "", "v1.14.4")
	noAddress.Status.Addresses = nil
	r := newTalosRig(t, c, noAddress,
		talosMachine("web-cp-1", "10.0.0.11", "v1.14.4"),
		talosMachine("web-cp-2", "10.0.0.12", "v1.14.4"),
		talosMachine("web-worker-0", "10.0.0.21", "v1.14.4"))
	kube := r.kube
	var dialled []string
	d := &EtcdDialer{Reader: kube, Talos: func(ctx context.Context, raw []byte, address string) (TalosNode, error) {
		dialled = append(dialled, address)
		if address == "10.0.0.11" {
			return nil, errors.New("connection refused")
		}
		return r.talos.dial(ctx, raw, address)
	}}
	s, err := d.Snapshot(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(s)
	if err != nil || string(body) != "snapshot" || s.Version != "3.6.5" || s.Revision != 0 || s.Close() != nil {
		t.Errorf("snapshot %q, %v, version %s, revision %d", body, err, s.Version, s.Revision)
	}
	if !slices.Equal(dialled, []string{"10.0.0.11", "10.0.0.12"}) {
		t.Errorf("dialled %v, want the control planes with an address in turn", dialled)
	}
	if _, err := (&EtcdDialer{Reader: kube}).Snapshot(context.Background(), c); err == nil {
		t.Error("a snapshot without a way to reach Talos")
	}
}

func talosClass(releases ...string) *clusterv1.ClusterClass {
	return &clusterv1.ClusterClass{
		Namespace: "kubehenge-system", Name: ClassPrefix + "talos",
		Annotations: map[string]string{TalosTemplatesAnnotation: strings.Join(releases, ",")},
	}
}

func talosHooks(t *testing.T, objects ...client.Object) (*Hooks, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &Hooks{Reader: kube, Client: kube, ClassNamespace: "kubehenge-system"}, kube
}

func talosImageOf(t *testing.T, kube client.Client, c *clusterv1.Cluster) string {
	t.Helper()
	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	return got.Annotations[TalosImageAnnotation]
}

func TestNewTalosClustersWaitForTheirReleasesTemplate(t *testing.T) {
	t.Parallel()
	c := talosCluster("web", "v1.14.4")
	h, kube := talosHooks(t, c)
	// Cluster API's patch at creation clears what its copy of the cluster lacks, so the cluster
	// waits until the copy it is given carries the record.
	waiting, retry, err := h.talosImageReady(context.Background(), c, talosClass("v1.14.3", "v1.14.4"))
	if err != nil || !strings.Contains(waiting, "recording") || retry != 1 {
		t.Errorf("before Cluster API has read the record: %q, %d, %v", waiting, retry, err)
	}
	if got := talosImageOf(t, kube, c); got != "v1.14.4" {
		t.Errorf("the machines boot %q, want v1.14.4", got)
	}
	if err = kube.Get(context.Background(), client.ObjectKeyFromObject(c), c); err != nil {
		t.Fatal(err)
	}
	waiting, retry, err = h.talosImageReady(context.Background(), c, talosClass("v1.14.3"))
	if err != nil || !strings.Contains(waiting, "Talos v1.14.4 template") || retry != 10 {
		t.Errorf("without its template: %q, %d, %v", waiting, retry, err)
	}
	waiting, _, err = h.talosImageReady(context.Background(), c, talosClass("v1.14.3", "v1.14.4"))
	if err != nil || waiting != "" {
		t.Errorf("with its template: %q, %v", waiting, err)
	}
}

func TestUpgradesBootTheNewReleaseOnceItsTemplateIsThere(t *testing.T) {
	t.Parallel()
	c := talosCluster("web", "v1.14.4")
	c.Annotations[TalosImageAnnotation] = "v1.14.3"
	class := talosClass("v1.14.3")
	h, kube := talosHooks(t, c, class)
	if moved, err := h.newestTalosImage(context.Background(), c); err != nil || moved || talosImageOf(t, kube, c) != "v1.14.3" {
		t.Errorf("moved to a release with no template: %v, %v", moved, err)
	}
	class.Annotations[TalosTemplatesAnnotation] = "v1.14.3,v1.14.4"
	if err := kube.Update(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	if moved, err := h.newestTalosImage(context.Background(), c); err != nil || !moved || talosImageOf(t, kube, c) != "v1.14.4" {
		t.Errorf("not moved to the cluster's release: %v, %v", moved, err)
	}
}
