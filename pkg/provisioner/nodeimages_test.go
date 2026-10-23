package provisioner

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func day(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }

func cloudLine(t *testing.T) []string {
	t.Helper()
	line, err := MachineLine("rke2", "v1.36.5+rke2r1", templates.DefaultNodeOS)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestPickTakesTheNewestBuildOfferedByTheNodeImageTime(t *testing.T) {
	t.Parallel()
	line := cloudLine(t)
	builds := NodeImageBuilds{templates.LineKey(line): {day(1), day(8), day(15)}}
	for _, c := range []struct {
		at   time.Time
		want time.Time
	}{
		// Before any build is offered: the oldest, such as for a cluster made before the first.
		{day(1), day(1)},
		{day(9), day(8)},
		{day(9).Add(-time.Second), day(1)},
		{day(20), day(15)},
	} {
		if got, ok := builds.Pick(line, c.at, DefaultNodeImageSoak); !ok || !got.Equal(c.want) {
			t.Errorf("Pick at %s = %s, want %s", c.at, got, c.want)
		}
	}
	if _, ok := builds.Pick([]string{"kubehenge", "other"}, day(20), DefaultNodeImageSoak); ok {
		t.Error("a line without builds picks one")
	}
	if next, ok := builds.Waiting(line, day(9), day(20), DefaultNodeImageSoak); !ok || !next.Equal(day(15)) {
		t.Errorf("Waiting = %s, %v", next, ok)
	}
	if _, ok := builds.Waiting(line, day(9), day(15), DefaultNodeImageSoak); ok {
		t.Error("a build not a day old is waiting")
	}
	if newer, ok := builds.Newer(line, day(8)); !ok || !newer.Equal(day(15)) {
		t.Errorf("Newer = %s, %v", newer, ok)
	}
	raw, err := builds.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseNodeImageBuilds(raw)
	if err != nil || !slices.EqualFunc(back[templates.LineKey(line)], builds[templates.LineKey(line)], time.Time.Equal) {
		t.Errorf("round trip %s: %v, %v", raw, back, err)
	}
}

func TestWindowNext(t *testing.T) {
	t.Parallel()
	w := &Window{Weekdays: []string{"Sat"}, Start: "02:00", DurationMinutes: 120, TimeZone: "Europe/Zurich"}
	// Tuesday 13 October 2026; Zurich is two hours ahead of UTC then.
	got, err := w.Next(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	if err != nil || !got.Equal(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Next = %s, %v", got, err)
	}
	inside := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	if got, err := w.Next(inside); err != nil || !got.Equal(inside) {
		t.Errorf("Next inside the window = %s, %v", got, err)
	}
	monthly := &Window{MonthDays: []int{31}}
	if got, err := monthly.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil || !got.Equal(time.Date(2026, 12, 31, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("Next of the 31st = %s, %v", got, err)
	}
}

func TestRefreshScheduleNext(t *testing.T) {
	t.Parallel()
	s, err := ParseRefreshSchedule(map[string]string{RefreshWeekday: "Sun", RefreshTime: "01:00", RefreshTimeZone: "UTC", RefreshSoakHours: "24", RefreshCreatorBuilds: "true"})
	if err != nil || s.Soak != 24*time.Hour || !s.CreatorBuilds {
		t.Fatalf("schedule %+v, %v", s, err)
	}
	sunday := time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
	for from, want := range map[time.Time]time.Time{
		time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC): sunday,
		sunday: sunday.AddDate(0, 0, 7),
	} {
		if got, err := s.Next(from); err != nil || !got.Equal(want) {
			t.Errorf("Next(%s) = %s, %v; want %s", from, got, err, want)
		}
	}
	if _, err := ParseRefreshSchedule(map[string]string{RefreshWeekday: time.Sunday.String(), RefreshTime: "01:00", RefreshSoakHours: "24"}); err == nil {
		t.Error("a weekday spelled out was taken")
	}
}

// refreshCluster is a settled RKE2 cluster whose node image time is the 9th, with the builds of the
// 1st, 8th and 15th recorded on its class.
func refreshCluster(t *testing.T, name string, vars map[string]string) (*clusterv1.Cluster, *clusterv1.ClusterClass) {
	t.Helper()
	c := settledCluster(name, "v1.36.5+rke2r1", vars)
	c.Annotations = map[string]string{NodeImageAnnotation: day(9).Format(time.RFC3339)}
	builds, err := NodeImageBuilds{templates.LineKey(cloudLine(t)): {day(1), day(8), day(15)}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	class := &clusterv1.ClusterClass{}
	class.Namespace, class.Name = "kubehenge-system", "kubehenge-rke2"
	class.Annotations = map[string]string{NodeImageBuildsAnnotation: builds}
	return c, class
}

func TestUpgraderRefreshesNodeImagesInTheWindow(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	window := `"window":{"start":"02:00","durationMinutes":240}`
	on, class := refreshCluster(t, "on", map[string]string{"upgrades": `{"mode":"off",` + window + `}`})
	unset, _ := refreshCluster(t, "unset", nil)
	off, _ := refreshCluster(t, "off", map[string]string{"upgrades": `{"mode":"off","nodeImages":false,` + window + `}`})
	closed, _ := refreshCluster(t, "closed", map[string]string{"upgrades": `{"mode":"off","window":{"weekdays":["Sun"]}}`})
	rolling, _ := refreshCluster(t, "rolling", map[string]string{"upgrades": `{"mode":"off",` + window + `}`})
	two := int32(2)
	rolling.Status.ControlPlane.DesiredReplicas = &two
	// Held from the last time the upgrader looked, before the window opened.
	on.Annotations[NodeImageHeldAnnotation] = "build-202610150000 at 2026-10-20T02:00:00Z"
	off.Annotations[NodeImageHeldAnnotation] = "build-202610150000 at 2026-10-20T02:00:00Z"
	objects := []client.Object{class, on, unset, off, closed, rolling}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	for _, o := range []*clusterv1.Cluster{on, unset, off, closed, rolling} {
		if err := kube.Status().Update(context.Background(), o.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	}
	// Tuesday the 20th at 03:00, inside the window.
	now := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	u := &Upgrader{Client: kube, ClassNamespace: "kubehenge-system", Releases: fixedReleases{}, Soak: DefaultNodeImageSoak, Now: func() time.Time { return now }}
	u.RunOnce(context.Background())
	for name, moved := range map[string]bool{"on": true, "unset": true, "off": false, "closed": false, "rolling": false} {
		c := &clusterv1.Cluster{}
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: name}, c); err != nil {
			t.Fatal(err)
		}
		at := c.Annotations[NodeImageAnnotation]
		if got := at == now.Format(time.RFC3339); got != moved {
			t.Errorf("%s: node image time %s", name, at)
		}
		if moved && c.Annotations[NodeImageRefreshedAnnotation] != "build-202610080000 to build-202610150000 at 2026-10-20T03:00:00Z" {
			t.Errorf("%s: refreshed %q", name, c.Annotations[NodeImageRefreshedAnnotation])
		}
		// Only the closed window holds a cluster; one that moves, or does not refresh, is held by
		// nothing. The window has no duration of its own: Sunday the 25th from 02:00.
		want := map[string]string{"closed": "build-202610150000 at 2026-10-25T02:00:00Z"}[name]
		if held, ok := c.Annotations[NodeImageHeldAnnotation]; held != want || ok != (want != "") {
			t.Errorf("%s: held %q", name, held)
		}
	}
}

func TestUpgradeTakesTheNewestNodeImages(t *testing.T) {
	t.Parallel()
	c, class := refreshCluster(t, "web", nil)
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c, class).Build()
	now := day(20)
	h := &Hooks{Reader: kube, Client: kube, ClassNamespace: "kubehenge-system", Soak: DefaultNodeImageSoak, Now: func() time.Time { return now }}
	req := &runtimehooksv1.BeforeClusterUpgradeRequest{Cluster: *c, ToKubernetesVersion: "v1.37.1+rke2r1"}
	resp := &runtimehooksv1.BeforeClusterUpgradeResponse{}
	h.BeforeClusterUpgrade(context.Background(), req, resp)
	if resp.RetryAfterSeconds == 0 || resp.Status != runtimehooksv1.ResponseStatusSuccess {
		t.Fatalf("response %+v", resp)
	}
	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[NodeImageAnnotation] != now.Format(time.RFC3339) {
		t.Fatalf("node image time %q", got.Annotations[NodeImageAnnotation])
	}
	// Once moved, the upgrade goes ahead.
	req.Cluster = *got
	resp = &runtimehooksv1.BeforeClusterUpgradeResponse{}
	h.BeforeClusterUpgrade(context.Background(), req, resp)
	if resp.RetryAfterSeconds != 0 {
		t.Fatalf("held again: %+v", resp)
	}
}

func TestGeneratePatchesSelectTheBuildOfTheNodeImageTime(t *testing.T) {
	t.Parallel()
	c, class := refreshCluster(t, "web", nil)
	talos, _ := refreshCluster(t, "metal", nil)
	talos.Spec.Topology.ClassRef.Name = "kubehenge-talos"
	talos.Spec.Topology.Variables = append(talos.Spec.Topology.Variables,
		clusterv1.ClusterVariable{Name: TalosVersionVariable, Value: apiextensionsv1.JSON{Raw: []byte(`"v1.14.4"`)}})
	talos.Annotations[TalosImageAnnotation] = "v1.14.3"
	rocky, _ := refreshCluster(t, "rocky", map[string]string{NodeOSVariable: `{"family":"rocky","release":"10"}`})
	rockyLine, err := MachineLine("rke2", "v1.36.5+rke2r1", templates.NodeOS{Family: "rocky", Release: "10"})
	if err != nil {
		t.Fatal(err)
	}
	builds, err := NodeImageBuilds{templates.LineKey(cloudLine(t)): {day(1), day(8), day(15)}, templates.LineKey(rockyLine): {day(2)}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	class.Annotations[NodeImageBuildsAnnotation] = builds
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c, talos, rocky, class).Build()
	h := &Hooks{Reader: kube, ClassNamespace: "kubehenge-system", Soak: DefaultNodeImageSoak}
	request := func(name string) *runtimehooksv1.GeneratePatchesResponse {
		builtins := func(v any) apiextensionsv1.JSON {
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			return apiextensionsv1.JSON{Raw: raw}
		}
		item := runtimehooksv1.GeneratePatchesRequestItem{
			UID:             "machines",
			HolderReference: runtimehooksv1.HolderReference{Kind: "MachineDeployment", Namespace: "clusters", Name: name + "-md-0"},
			Object:          runtime.RawExtension{Raw: []byte(`{"apiVersion":"infrastructure.cluster.x-k8s.io/v1alpha2","kind":"ProxmoxMachineTemplate"}`)},
			Variables: []runtimehooksv1.Variable{{Name: runtimehooksv1.BuiltinsName, Value: builtins(map[string]any{
				"machineDeployment": map[string]any{"version": "v1.36.5+rke2r1"},
			})}},
		}
		req := &runtimehooksv1.GeneratePatchesRequest{
			Variables: []runtimehooksv1.Variable{{Name: runtimehooksv1.BuiltinsName, Value: builtins(map[string]any{
				"cluster": map[string]any{"name": name, "namespace": "clusters"},
			})}},
			Items: []runtimehooksv1.GeneratePatchesRequestItem{item},
		}
		resp := &runtimehooksv1.GeneratePatchesResponse{}
		h.GeneratePatches(context.Background(), req, resp)
		if resp.Status != runtimehooksv1.ResponseStatusSuccess {
			t.Fatalf("%s: %s", name, resp.Message)
		}
		return resp
	}
	resp := request("web")
	if len(resp.Items) != 1 || !strings.Contains(string(resp.Items[0].Patch), `"build-202610080000"`) ||
		!strings.Contains(string(resp.Items[0].Patch), "/spec/template/spec/templateSelector/matchTags") {
		t.Fatalf("items %+v", resp.Items)
	}
	// Talos machines boot the release the cluster records, whatever talosVersion says: they are
	// upgraded in place, without a rollout.
	resp = request("metal")
	if len(resp.Items) != 1 || !strings.Contains(string(resp.Items[0].Patch), `"talos-v1.14.3"`) ||
		strings.Contains(string(resp.Items[0].Patch), "build-") {
		t.Errorf("Talos machines: %+v", resp.Items)
	}
	// The request carries none of the cluster's own variables; its node OS is the Cluster's.
	if resp := request("rocky"); len(resp.Items) != 1 || !strings.Contains(string(resp.Items[0].Patch), `"os-rocky-10"`) ||
		!strings.Contains(string(resp.Items[0].Patch), `"build-202610020000"`) {
		t.Errorf("a cluster on Rocky Linux: %+v", resp.Items)
	}
}
