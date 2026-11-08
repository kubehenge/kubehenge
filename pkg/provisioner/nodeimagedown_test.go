package provisioner

import (
	"context"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// downBuildOf is the template of the cluster's line built at a time, on a node that is down.
func downBuildOf(t *testing.T, at time.Time) DownTemplate {
	t.Helper()
	tags := templates.NormaliseTags(append(slices.Clone(cloudLine(t)), templates.BuildTag(at)))
	return DownTemplate{VMID: 9000 + at.Day(), Node: "pve2", Tags: tags}
}

// refreshWith is a cluster whose node image time is the 9th, with the class recording builds and
// the templates down, and the upgrader to refresh it inside its window on the 20th.
func refreshWith(t *testing.T, builds []time.Time, down ...DownTemplate) (*clusterv1.Cluster, client.Client, *Upgrader) {
	t.Helper()
	c, class := refreshCluster(t, "web", map[string]string{"upgrades": `{"mode":"off","window":{"start":"02:00","durationMinutes":240}}`})
	recorded, err := NodeImageBuilds{templates.LineKey(cloudLine(t)): builds}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	class.Annotations[NodeImageBuildsAnnotation] = recorded
	setDown(t, class, down)
	scheme := runtime.NewScheme()
	if err = clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(class, c).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	if err = kube.Status().Update(context.Background(), c.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	return c, kube, &Upgrader{Client: kube, ClassNamespace: "kubehenge-system", Releases: fixedReleases{}, Soak: DefaultNodeImageSoak, Now: func() time.Time { return now }}
}

func setDown(t *testing.T, class *clusterv1.ClusterClass, down []DownTemplate) {
	t.Helper()
	encoded, err := encodeTemplatesDown(down)
	if err != nil {
		t.Fatal(err)
	}
	class.Annotations[TemplatesDownAnnotation] = encoded
}

func refreshed(t *testing.T, kube client.Client, c *clusterv1.Cluster) (string, *clusterv1.Cluster) {
	t.Helper()
	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	return got.Annotations[NodeImageAnnotation], got
}

func TestRefreshDoesNotMoveOntoABuildOnADownNodeAndGoesOnWhenItIsBack(t *testing.T) {
	t.Parallel()
	c, kube, u := refreshWith(t, []time.Time{day(1), day(8), day(15)}, downBuildOf(t, day(15)))
	ctx := context.Background()

	u.RunOnce(ctx)
	at, got := refreshed(t, kube, c)
	if at != day(9).Format(time.RFC3339) {
		t.Errorf("node image time %s; want it to stay at the 9th while the newest build is on a down node", at)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, NodeImageHeldCondition)
	if cond == nil || cond.Reason != NodeImageHeldTemplateNodeDown || cond.Message != "Build build-202610150000 is on node pve2, which is down" {
		t.Errorf("condition %+v; want NodeImageHeld naming the build and node", cond)
	}

	// The node is back: the class no longer lists the template.
	class := &clusterv1.ClusterClass{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	setDown(t, class, nil)
	if err := kube.Update(ctx, class); err != nil {
		t.Fatal(err)
	}
	u.RunOnce(ctx)
	at, got = refreshed(t, kube, c)
	if at != u.Now().Format(time.RFC3339) {
		t.Errorf("node image time %s once the node is back; want the move to now", at)
	}
	if meta.FindStatusCondition(got.Status.Conditions, NodeImageHeldCondition) != nil {
		t.Error("the hold stayed once the node was back")
	}
}

// A build newer than the cluster's but older than the newest is on a node that is up: the cluster
// moves to it, with the node image time set to when that build was first offered.
func TestRefreshMovesToTheNewestBuildOnAnUpNode(t *testing.T) {
	t.Parallel()
	c, kube, u := refreshWith(t, []time.Time{day(1), day(8), day(12), day(15)}, downBuildOf(t, day(15)))
	u.RunOnce(context.Background())
	at, got := refreshed(t, kube, c)
	if want := day(12).Add(DefaultNodeImageSoak).Format(time.RFC3339); at != want {
		t.Errorf("node image time %s; want %s, the 12th's build as offered", at, want)
	}
	if meta.FindStatusCondition(got.Status.Conditions, NodeImageHeldCondition) != nil {
		t.Error("a hold was set although the cluster could move")
	}
	line := cloudLine(t)
	all := NodeImageBuilds{templates.LineKey(line): {day(1), day(8), day(12), day(15)}}
	if picked, _ := all.Pick(line, mustTime(t, at), DefaultNodeImageSoak); !picked.Equal(day(12)) {
		t.Errorf("the node image time picks the build of %s, want the 12th", picked)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// With the cluster's own build on a down node too, it is stuck either way: the refresh goes on as
// before, and TemplateNodeDown reports it.
func TestRefreshGoesOnWhenTheClustersOwnBuildIsDownToo(t *testing.T) {
	t.Parallel()
	c, kube, u := refreshWith(t, []time.Time{day(1), day(8), day(15)}, downBuildOf(t, day(8)), downBuildOf(t, day(15)))
	u.RunOnce(context.Background())
	at, got := refreshed(t, kube, c)
	if at != u.Now().Format(time.RFC3339) {
		t.Errorf("node image time %s; want the move to now", at)
	}
	if meta.FindStatusCondition(got.Status.Conditions, NodeImageHeldCondition) != nil {
		t.Error("held although the cluster's own build is down too")
	}
}

func TestUpgradeDoesNotMoveNodeImagesOntoABuildOnADownNode(t *testing.T) {
	t.Parallel()
	c, class := refreshCluster(t, "web", nil)
	c.Spec.Topology.Version = "v1.37.1+rke2r1"
	setDown(t, class, []DownTemplate{downBuildOf(t, day(15))})
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c, class).Build()
	h := &Hooks{Reader: kube, Client: kube, ClassNamespace: "kubehenge-system", Soak: DefaultNodeImageSoak, Now: func() time.Time { return day(20) }}
	resp := &runtimehooksv1.BeforeClusterUpgradeResponse{}
	h.BeforeClusterUpgrade(context.Background(), &runtimehooksv1.BeforeClusterUpgradeRequest{Cluster: *c, ToKubernetesVersion: "v1.37.1+rke2r1"}, resp)
	at, _ := refreshed(t, kube, c)
	if resp.RetryAfterSeconds != 0 || at != day(9).Format(time.RFC3339) {
		t.Errorf("response %+v and node image time %s; want the upgrade to go on from the 9th's build", resp, at)
	}
}
