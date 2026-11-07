package ui

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// busyCluster is how a management cluster treats a Cluster that Cluster API and Kubehenge's
// controllers keep updating. Just before each of the first n writes to a Cluster, they update its
// status and annotations, which moves its resourceVersion on, so that write conflicts. between,
// when set, also changes what the controllers' update stores. It counts the writes made.
type busyCluster struct {
	n       int
	between func(*clusterv1.Cluster)
	writes  atomic.Int32
}

func (b *busyCluster) churn(h *harness) {
	h.t.Helper()
	kube, ok := h.kube.(client.WithWatch)
	if !ok {
		h.t.Fatal("the harness's client does not watch")
	}
	touch := func(obj client.Object) bool {
		if _, isCluster := obj.(*clusterv1.Cluster); !isCluster || int(b.writes.Add(1)) > b.n {
			return false
		}
		stored := &clusterv1.Cluster{}
		if err := kube.Get(h.t.Context(), client.ObjectKeyFromObject(obj), stored); err != nil {
			h.t.Error(err)
			return false
		}
		if stored.Annotations == nil {
			stored.Annotations = map[string]string{}
		}
		stored.Annotations["example.org/touched"] = "yes"
		stored.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Rolling"}}
		if b.between != nil {
			b.between(stored)
		}
		if err := kube.Update(h.t.Context(), stored); err != nil {
			h.t.Error(err)
		}
		return true
	}
	h.server.setKube(interceptor.NewClient(kube, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			// The fake client does not compare versions on a dry run, the API server does.
			if touch(obj) && slices.Contains(opts, client.UpdateOption(client.DryRunAll)) {
				return apierrors.NewConflict(clusterv1.GroupVersion.WithResource("clusters").GroupResource(), obj.GetName(), errors.New("the object has been modified"))
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			touch(obj)
			return c.Patch(ctx, obj, patch, opts...)
		},
	}))
}

func busy(h *harness, n int, between func(*clusterv1.Cluster)) *busyCluster {
	b := &busyCluster{n: n, between: between}
	b.churn(h)
	return b
}

func workerOf(cluster, pool, name string) *clusterv1.Machine {
	m := clusterMachine(cluster, name)
	delete(m.Labels, clusterv1.MachineControlPlaneLabel)
	m.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel] = pool
	return m
}

func storedCluster(t *testing.T, h *harness) *clusterv1.Cluster {
	t.Helper()
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func isMarkedToGo(t *testing.T, h *harness, m *clusterv1.Machine) bool {
	t.Helper()
	got := &clusterv1.Machine{}
	if err := h.kube.Get(t.Context(), client.ObjectKeyFromObject(m), got); err != nil {
		t.Fatal(err)
	}
	_, ok := got.Annotations[clusterv1.DeleteMachineAnnotation]
	return ok
}

func TestDeletingAWorkerSurvivesControllersUpdatingTheCluster(t *testing.T) {
	t.Parallel()
	worker := workerOf("one", "md-0", "one-md-0-a")
	h := newHarness(t, cluster("default", "one"), worker)
	updates := busy(h, 2, nil)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("DELETE", "/api/clusters/default/one/machines/one-md-0-a", ""), http.StatusNoContent)
	if n := *storedCluster(t, h).Spec.Topology.Workers.MachineDeployments[0].Replicas; n != 1 {
		t.Errorf("md-0 has %d workers, want 1: one lower, however often the write was made", n)
	}
	if !isMarkedToGo(t, h, worker) {
		t.Error("the worker is not marked to go first")
	}
	if n := updates.writes.Load(); n != 3 {
		t.Errorf("%d writes to the cluster, want 2 conflicts and 1 that lands", n)
	}
}

func TestDeletingAWorkerThatConflictsEveryTimeUnmarksIt(t *testing.T) {
	t.Parallel()
	worker := workerOf("one", "md-0", "one-md-0-a")
	h := newHarness(t, cluster("default", "one"), worker)
	updates := busy(h, 1000, nil)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("DELETE", "/api/clusters/default/one/machines/one-md-0-a", "")
	expect(t, rec, http.StatusConflict)
	if n := int(updates.writes.Load()); n != retry.DefaultRetry.Steps {
		t.Errorf("%d writes to the cluster, want %d attempts", n, retry.DefaultRetry.Steps)
	}
	if !strings.Contains(rec.Body.String(), "the cluster changed meanwhile") {
		t.Errorf("body %s", rec.Body)
	}
	if isMarkedToGo(t, h, worker) {
		t.Error("the worker stays marked though its pool did not shrink")
	}
	if n := *storedCluster(t, h).Spec.Topology.Workers.MachineDeployments[0].Replicas; n != 2 {
		t.Errorf("md-0 has %d workers, want the 2 it had", n)
	}
}

func TestDeletingAWorkerChecksThePoolAgainOnEachAttempt(t *testing.T) {
	t.Parallel()
	worker := workerOf("one", "md-0", "one-md-0-a")
	h := newHarness(t, cluster("default", "one"), worker)
	busy(h, 1, func(c *clusterv1.Cluster) {
		c.Spec.Topology.Workers.MachineDeployments[0].Metadata.Annotations = map[string]string{
			provisioner.AutoscalerMinAnnotation: "1", provisioner.AutoscalerMaxAnnotation: "3",
		}
	})
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("DELETE", "/api/clusters/default/one/machines/one-md-0-a", "")
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "autoscaled") {
		t.Errorf("body %s, want the pool's autoscaling named", rec.Body)
	}
	if isMarkedToGo(t, h, worker) {
		t.Error("the worker stays marked though its pool became autoscaled")
	}
	if n := *storedCluster(t, h).Spec.Topology.Workers.MachineDeployments[0].Replicas; n != 2 {
		t.Errorf("md-0 has %d workers, want the 2 it had", n)
	}
}

func TestChangeSurvivesControllersUpdatingTheCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	updates := busy(h, 2, nil)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("PATCH", "/api/clusters/default/one", `{"workerPools":{"md-0":4},"variables":{"headlamp":{"enabled":true}}}`), http.StatusOK)
	c := storedCluster(t, h)
	if n := *c.Spec.Topology.Workers.MachineDeployments[0].Replicas; n != 4 {
		t.Errorf("md-0 has %d workers, want 4", n)
	}
	if got := variableValue(c, "headlamp"); string(got) != `{"enabled":true}` || len(c.Spec.Topology.Variables) != 2 {
		t.Errorf("variables = %+v", c.Spec.Topology.Variables)
	}
	rec, ok := changeOf(c)
	if !ok || len(rec.Settings) != 2 || rec.By != "dev@pve" {
		t.Errorf("recorded change = %+v, want the two settings once", rec)
	}
	if n := updates.writes.Load(); n != 3 {
		t.Errorf("%d writes to the cluster, want 2 conflicts and 1 that lands", n)
	}
}

func TestChangeThatConflictsEveryTimeSaysTheClusterChanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	updates := busy(h, 1000, nil)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("PATCH", "/api/clusters/default/one", `{"workerPools":{"md-0":4}}`)
	expect(t, rec, http.StatusConflict)
	if n := int(updates.writes.Load()); n != retry.DefaultRetry.Steps {
		t.Errorf("%d writes to the cluster, want %d attempts", n, retry.DefaultRetry.Steps)
	}
	if !strings.Contains(rec.Body.String(), "the cluster changed meanwhile") {
		t.Errorf("body %s", rec.Body)
	}
	if n := *storedCluster(t, h).Spec.Topology.Workers.MachineDeployments[0].Replicas; n != 2 {
		t.Errorf("md-0 has %d workers, want the 2 it had", n)
	}
}

func TestPlanSurvivesControllersUpdatingTheCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cluster("default", "one"))
	busy(h, 2, nil)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("POST", "/api/clusters/default/one/plan", `{"workerPools":{"md-0":4}}`), http.StatusOK)
}

func TestChangeAgainstAnOlderGenerationIsRefusedAtOnce(t *testing.T) {
	t.Parallel()
	one := cluster("default", "one")
	one.Generation = 3
	h := newHarness(t, one)
	updates := busy(h, 2, nil)
	b := h.browser()
	b.signIn("dev@pve")
	b.generation = "2"
	rec := b.do("PATCH", "/api/clusters/default/one", `{"workerPools":{"md-0":4}}`)
	expect(t, rec, http.StatusPreconditionFailed)
	if !strings.Contains(rec.Body.String(), "the cluster changed meanwhile") {
		t.Errorf("body %s", rec.Body)
	}
	if n := updates.writes.Load(); n != 0 {
		t.Errorf("%d writes to the cluster, want none", n)
	}
}

// Another user's edit that lands while a change is retried is not undone by it.
func TestChangeRefusesAnEditThatLandsDuringARetry(t *testing.T) {
	t.Parallel()
	one := cluster("default", "one")
	one.Generation = 3
	h := newHarness(t, one)
	busy(h, 1, func(c *clusterv1.Cluster) {
		c.Generation = 4
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
			Name: "headlamp", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true}`)},
		})
	})
	b := h.browser()
	b.signIn("dev@pve")
	b.generation = "3"
	rec := b.do("PATCH", "/api/clusters/default/one", `{"variables":{"headlamp":{"enabled":false}}}`)
	expect(t, rec, http.StatusPreconditionFailed)
	if !strings.Contains(rec.Body.String(), "the cluster changed meanwhile") {
		t.Errorf("body %s", rec.Body)
	}
	if got := variableValue(storedCluster(t, h), "headlamp"); string(got) != `{"enabled":true}` {
		t.Errorf("the other user's edit was overwritten: headlamp = %s", got)
	}
}

func TestAskingForASnapshotSurvivesControllersUpdatingTheCluster(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	updates := busy(h.harness, 2, nil)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-snapshots", ""), http.StatusAccepted)
	c := h.cluster()
	if _, ok := c.Annotations[provisioner.EtcdSnapshotRequestAnnotation]; !ok {
		t.Error("the snapshot is not asked for")
	}
	if n := updates.writes.Load(); n != 3 {
		t.Errorf("%d writes to the cluster, want 2 conflicts and 1 that lands", n)
	}
}

// A restore that starts while a snapshot request is retried stops the request.
func TestAskingForASnapshotChecksTheClusterAgainOnEachAttempt(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	busy(h.harness, 1, func(c *clusterv1.Cluster) {
		c.Annotations[provisioner.EtcdRestoreAnnotation] = `{}`
	})
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("POST", "/api/clusters/default/one/etcd-snapshots", "")
	expect(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "an etcd restore is running") {
		t.Errorf("body %s", rec.Body)
	}
	if _, ok := h.cluster().Annotations[provisioner.EtcdSnapshotRequestAnnotation]; ok {
		t.Error("the snapshot is asked for during a restore")
	}
}
