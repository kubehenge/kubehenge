package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func TestWindowOpen(t *testing.T) {
	t.Parallel()
	// Tuesday 13 October 2026.
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, c := range []struct {
		name string
		w    Window
		now  string
		want bool
	}{
		{"default every day 02:00 for 4 h", Window{}, "2026-10-13T03:00:00Z", true},
		{"before", Window{}, "2026-10-13T01:59:00Z", false},
		{"start is inclusive", Window{}, "2026-10-13T02:00:00Z", true},
		{"end is exclusive", Window{}, "2026-10-13T06:00:00Z", false},
		{"weekday listed", Window{Weekdays: []string{"Tue"}, Start: "22:00", DurationMinutes: 60}, "2026-10-13T22:30:00Z", true},
		{"weekday not listed", Window{Weekdays: []string{"Sat", "Sun"}}, "2026-10-13T03:00:00Z", false},
		{"month day listed", Window{MonthDays: []int{13}}, "2026-10-13T03:00:00Z", true},
		{"either list counts", Window{Weekdays: []string{"Sat"}, MonthDays: []int{13}}, "2026-10-13T03:00:00Z", true},
		{"across midnight from the day before", Window{Weekdays: []string{"Mon"}, Start: "23:00", DurationMinutes: 180}, "2026-10-13T01:00:00Z", true},
		{"across midnight, day before not listed", Window{Weekdays: []string{"Tue"}, Start: "23:00", DurationMinutes: 180}, "2026-10-13T01:00:00Z", false},
		// 02:00 in Berlin (CEST, UTC+2) is 00:00 UTC.
		{"time zone", Window{Start: "02:00", DurationMinutes: 60, TimeZone: "Europe/Berlin"}, "2026-10-13T00:30:00Z", true},
		{"time zone, UTC hour", Window{Start: "02:00", DurationMinutes: 60, TimeZone: "Europe/Berlin"}, "2026-10-13T02:30:00Z", false},
	} {
		got, err := c.w.Open(at(c.now))
		if err != nil || got != c.want {
			t.Errorf("%s: Open = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, err := (&Window{TimeZone: "Mars/Olympus"}).Open(time.Now()); err == nil {
		t.Error("an unknown time zone was accepted")
	}
}

func channels(stable int, latest ...string) Channels {
	ch := Channels{Latest: map[int]kubeVersion{}, StableMinor: stable}
	for _, s := range latest {
		v, _ := parseKubeVersion(s)
		ch.Latest[v.Minor] = v
	}
	return ch
}

func TestVersionsOfferNewClustersSupportedMinors(t *testing.T) {
	t.Parallel()
	ch := channels(36, "v1.29.15+k3s1", "v1.33.13+k3s2", "v1.34.12+k3s1", "v1.35.9+k3s1", "v1.36.5+k3s1", "v1.37.1+k3s1")
	o := ch.Versions()
	if !slices.Equal(o.Current, []string{"v1.37.1+k3s1", "v1.36.5+k3s1", "v1.35.9+k3s1", "v1.34.12+k3s1"}) ||
		!slices.Equal(o.Older, []string{"v1.33.13+k3s2", "v1.29.15+k3s1"}) || o.Stable != "v1.36.5+k3s1" {
		t.Errorf("Versions = %+v", o)
	}
	// A publisher whose stable minor lags far behind its newest still has it offered.
	o = channels(31, "v1.31.14+k3s1", "v1.35.9+k3s1", "v1.37.1+k3s1").Versions()
	if !slices.Equal(o.Current, []string{"v1.37.1+k3s1", "v1.35.9+k3s1", "v1.31.14+k3s1"}) || len(o.Older) != 0 {
		t.Errorf("Versions with a lagging stable = %+v", o)
	}
	if o = channels(36).Versions(); len(o.Current) != 0 || len(o.Older) != 0 || o.Stable != "" {
		t.Errorf("Versions of no releases = %+v", o)
	}
}

func TestTarget(t *testing.T) {
	t.Parallel()
	ch := channels(36, "v1.35.9+rke2r1", "v1.36.5+rke2r2", "v1.37.1+rke2r1")
	for _, c := range []struct{ current, mode, want string }{
		{"v1.36.5+rke2r1", "patch", "v1.36.5+rke2r2"},
		{"v1.36.2+rke2r1", "patch", "v1.36.5+rke2r2"},
		{"v1.36.5+rke2r2", "patch", ""},
		{"v1.35.4+rke2r1", "minor", "v1.36.5+rke2r2"},
		// 1.37 is not stable yet.
		{"v1.36.5+rke2r1", "minor", "v1.36.5+rke2r2"},
		{"v1.36.5+rke2r2", "minor", ""},
		{"v1.36.5+rke2r1", "off", ""},
		{"garbage", "patch", ""},
	} {
		if got := ch.Target(c.current, c.mode); got != c.want {
			t.Errorf("Target(%s, %s) = %q, want %q", c.current, c.mode, got, c.want)
		}
	}
}

func TestChannelServer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"stable","latest":"v1.36.5+k3s1"},{"id":"latest","latest":"v1.37.1+k3s1"},
			{"id":"testing","latest":"v1.18.2-rc3+k3s1"},{"id":"v1.36","latest":"v1.36.5+k3s1"},{"id":"v1.37","latest":"v1.37.1+k3s1"}]}`))
	}))
	defer srv.Close()
	s := &ChannelServer{HTTP: srv.Client(), URLs: map[string]string{"k3s": srv.URL}}
	ch, err := s.Channels(context.Background(), "k3s", templates.DefaultNodeOS)
	if err != nil {
		t.Fatal(err)
	}
	if ch.StableMinor != 36 || ch.Latest[37].Raw != "v1.37.1+k3s1" || len(ch.Latest) != 2 {
		t.Fatalf("channels = %+v", ch)
	}
	if _, err := s.Channels(context.Background(), "talos", templates.DefaultNodeOS); err == nil {
		t.Fatal("channels for an unknown distribution")
	}
}

type fixedReleases map[string]Channels

func (f fixedReleases) Channels(_ context.Context, d string, _ templates.NodeOS) (Channels, error) {
	return f[d], nil
}

func settledCluster(name, version string, vars map[string]string) *clusterv1.Cluster {
	one := int32(1)
	c := &clusterv1.Cluster{
		Namespace: "clusters", Name: name, Generation: 2,
		Spec: clusterv1.ClusterSpec{Topology: clusterv1.Topology{
			ClassRef: clusterv1.ClusterClassRef{Name: "kubehenge-rke2", Namespace: "kubehenge-system"},
			Version:  version,
		}},
		Status: clusterv1.ClusterStatus{
			ObservedGeneration: 2,
			Conditions: []metav1.Condition{
				{Type: clusterv1.AvailableCondition, Status: metav1.ConditionTrue},
				{Type: clusterv1.ClusterTopologyReconciledCondition, Status: metav1.ConditionTrue},
			},
			ControlPlane: &clusterv1.ClusterControlPlaneStatus{DesiredReplicas: &one, UpToDateReplicas: &one},
			Workers:      &clusterv1.WorkersStatus{DesiredReplicas: &one, UpToDateReplicas: &one},
		},
	}
	for k, v := range vars {
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: k, Value: apiextensionsv1.JSON{Raw: []byte(v)}})
	}
	return c
}

func TestUpgrader(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	minorPolicy := `{"mode":"minor","window":{"start":"02:00","durationMinutes":240}}`
	rolling := settledCluster("rolling", "v1.35.4+rke2r1", map[string]string{"upgrades": minorPolicy})
	two := int32(2)
	rolling.Status.ControlPlane.DesiredReplicas = &two
	closed := settledCluster("closed", "v1.35.4+rke2r1", map[string]string{"upgrades": `{"mode":"minor","window":{"weekdays":["Sun"]}}`})
	objects := []client.Object{
		settledCluster("minor", "v1.35.4+rke2r1", map[string]string{"upgrades": minorPolicy}),
		settledCluster("patch", "v1.36.2+rke2r1", map[string]string{"upgrades": `{"mode":"patch"}`}),
		settledCluster("off", "v1.35.4+rke2r1", map[string]string{"upgrades": `{"mode":"off"}`}),
		settledCluster("unset", "v1.35.4+rke2r1", nil),
		settledCluster("rancher", "v1.36.5+rke2r1", map[string]string{"upgrades": minorPolicy, "rancher": `{"install":true,"hostname":"r.example"}`}),
		rolling, closed,
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	for _, o := range objects {
		c, ok := o.(*clusterv1.Cluster)
		if !ok {
			t.Fatalf("%T is not a cluster", o)
		}
		if err := kube.Status().Update(context.Background(), c.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	}
	u := &Upgrader{
		Client:            kube,
		ClassNamespace:    "kubehenge-system",
		Releases:          fixedReleases{"rke2": channels(37, "v1.35.9+rke2r1", "v1.36.5+rke2r1", "v1.37.1+rke2r1")},
		RancherKubernetes: KubernetesMinors{Min: 34, Max: 36},
		Now:               func() time.Time { return time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC) },
	}
	u.RunOnce(context.Background())

	want := map[string]string{
		"minor":   "v1.36.5+rke2r1",
		"patch":   "v1.36.5+rke2r1",
		"off":     "v1.35.4+rke2r1",
		"unset":   "v1.35.4+rke2r1",
		"rancher": "v1.36.5+rke2r1",
		"rolling": "v1.35.4+rke2r1",
		"closed":  "v1.35.4+rke2r1",
	}
	for name, version := range want {
		c := &clusterv1.Cluster{}
		if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: name}, c); err != nil {
			t.Fatal(err)
		}
		if c.Spec.Topology.Version != version {
			t.Errorf("%s: version %s, want %s", name, c.Spec.Topology.Version, version)
		}
		if upgraded := c.Annotations[autoUpgraded] != ""; upgraded != (name == "minor" || name == "patch") {
			t.Errorf("%s: annotation %q", name, c.Annotations[autoUpgraded])
		}
	}
}

func TestKubeadmTargetsAreTemplatesTheClassSelects(t *testing.T) {
	t.Parallel()
	vms := []map[string]any{
		{"vmid": 9001, "template": 1, "tags": "build-202610010000;kubeadm;kubehenge;kubernetes-v1.36.5;os-almalinux-10"},
		// Another node OS's, which clusters on the default do not run.
		{"vmid": 9002, "template": 1, "tags": "build-202610010000;kubeadm;kubehenge;kubernetes-v1.37.1;os-rocky-10"},
		{"vmid": 9003, "template": 0, "tags": "build-202610010000;kubeadm;kubehenge;kubernetes-v1.37.2;os-almalinux-10"},
		// A VM ID reused a moment ago, which the resource view still shows as the template before.
		{"vmid": 9004, "template": 1, "tags": "build-202610010000;kubeadm;kubehenge;kubernetes-v1.38.0;os-almalinux-10"},
		// A template that is no build, which no machine selects.
		{"vmid": 9005, "template": 1, "tags": "kubeadm;kubehenge;kubernetes-v1.39.0;os-almalinux-10"},
		// A node OS Kubehenge does not know.
		{"vmid": 9006, "template": 1, "tags": "build-202610010000;kubeadm;kubehenge;kubernetes-v1.39.1;os-plan9-4"},
	}
	configs := map[string]any{}
	for _, vm := range vms {
		vm["type"], vm["node"] = "qemu", "pve1"
		configs[fmt.Sprintf("/api2/json/nodes/pve1/qemu/%d/config", vm["vmid"])] = map[string]any{"template": vm["template"]}
	}
	configs["/api2/json/nodes/pve1/qemu/9004/config"] = map[string]any{"template": 0}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any = []any{}
		if r.URL.Path == "/api2/json/cluster/resources" {
			data = vms
		} else if c, ok := configs[r.URL.Path]; ok {
			data = c
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	s := &ChannelServer{Proxmox: proxmox.NewClient(srv.URL + "/api2/json")}
	ch, err := s.Channels(context.Background(), "kubeadm", templates.DefaultNodeOS)
	if err != nil {
		t.Fatal(err)
	}
	if ch.StableMinor != 36 || len(ch.Latest) != 1 || ch.Latest[36].Raw != "v1.36.5" {
		t.Errorf("channels %+v", ch)
	}
	rocky := templates.NodeOS{Family: "rocky", Release: "10"}
	if ch, err = s.Channels(context.Background(), "kubeadm", rocky); err != nil || len(ch.Latest) != 1 || ch.Latest[37].Raw != "v1.37.1" {
		t.Errorf("Rocky Linux channels %+v, %v", ch, err)
	}

	// The patch that picks a machine's template and the upgrader name the same node OS.
	for _, o := range []templates.NodeOS{templates.DefaultNodeOS, rocky} {
		if line, err := MachineLine("kubeadm", "v1.36.5", o); err != nil || !slices.Contains(line, o.Tags()[0]) {
			t.Errorf("kubeadm machines on %s select %v, %v", o, line, err)
		}
	}
}

// A version the user sets between the upgrader's read and its patch stands.
func TestUpgraderNeverOverwritesAChangeMadeMeanwhile(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := settledCluster("minor", "v1.35.4+rke2r1", map[string]string{"upgrades": `{"mode":"minor"}`})
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).WithStatusSubresource(&clusterv1.Cluster{}).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := w.List(ctx, list, opts...); err != nil {
				return err
			}
			changed := &clusterv1.Cluster{}
			if err := w.Get(ctx, client.ObjectKeyFromObject(c), changed); err != nil {
				return err
			}
			changed.Spec.Topology.Version = "v1.35.9+rke2r1"
			return w.Update(ctx, changed)
		}}).Build()
	if err := kube.Status().Update(context.Background(), c.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	u := &Upgrader{
		Client:         kube,
		ClassNamespace: "kubehenge-system",
		Releases:       fixedReleases{"rke2": channels(37, "v1.35.9+rke2r1", "v1.36.5+rke2r1", "v1.37.1+rke2r1")},
		Now:            func() time.Time { return time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC) },
	}
	u.RunOnce(context.Background())
	got := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Topology.Version != "v1.35.9+rke2r1" || got.Annotations[autoUpgraded] != "" {
		t.Errorf("version %s, annotation %q: the user's change was overwritten", got.Spec.Topology.Version, got.Annotations[autoUpgraded])
	}
}

// upgradeRound runs one round of u, at a time inside the default window, over clusters.
func upgradeRound(t *testing.T, u *Upgrader, clusters ...*clusterv1.Cluster) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := make([]client.Object, len(clusters))
	for i, c := range clusters {
		objects[i] = c
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&clusterv1.Cluster{}).Build()
	for _, c := range clusters {
		if err := kube.Status().Update(context.Background(), c.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	}
	u.Client, u.ClassNamespace = kube, "kubehenge-system"
	if u.Now == nil {
		u.Now = func() time.Time { return time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC) }
	}
	u.RunOnce(context.Background())
	return kube
}

func clusterAfter(t *testing.T, kube client.Client, name string) *clusterv1.Cluster {
	t.Helper()
	c := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "clusters", Name: name}, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func rancherCluster(version, policy string) *clusterv1.Cluster {
	return settledCluster("rancher", version, map[string]string{"upgrades": policy, "rancher": `{"install":true,"hostname":"r.example"}`})
}

// The hook and the controller allow any version where no Rancher range is set, and 0-0 parses to
// that.
func TestUpgraderTakesAnyVersionWhereNoRancherRangeIsSet(t *testing.T) {
	t.Parallel()
	unset, err := ParseKubernetesMinors("0-0")
	if err != nil || unset != (KubernetesMinors{}) {
		t.Fatalf("0-0 parsed to %+v, %v", unset, err)
	}
	u := &Upgrader{Releases: fixedReleases{"rke2": channels(36, "v1.36.5+rke2r1")}, RancherKubernetes: unset}
	kube := upgradeRound(t, u, rancherCluster("v1.36.2+rke2r1", `{"mode":"patch"}`))
	if got := clusterAfter(t, kube, "rancher").Spec.Topology.Version; got != "v1.36.5+rke2r1" {
		t.Errorf("no Rancher range set: the cluster stays on %s, want v1.36.5+rke2r1", got)
	}
}

// A hold on the next minor leaves the cluster the patch releases of its own, and says why where
// there is nothing else to take.
func TestRancherHoldOnTheNextMinorStillTakesPatchReleases(t *testing.T) {
	t.Parallel()
	releases := fixedReleases{"rke2": channels(37, "v1.36.5+rke2r1", "v1.37.1+rke2r1")}
	for _, c := range []struct{ name, from, want, reason string }{
		{"a patch release is due", "v1.36.2+rke2r1", "v1.36.5+rke2r1", ""},
		{"the newest patch is running", "v1.36.5+rke2r1", "v1.36.5+rke2r1", UpgradeHeldRancherKubernetes},
	} {
		u := &Upgrader{Releases: releases, RancherKubernetes: KubernetesMinors{Min: 34, Max: 36}}
		got := clusterAfter(t, upgradeRound(t, u, rancherCluster(c.from, `{"mode":"minor"}`)), "rancher")
		if got.Spec.Topology.Version != c.want {
			t.Errorf("%s: Rancher runs on 1.34 to 1.36 and the cluster stays on %s, want %s", c.name, got.Spec.Topology.Version, c.want)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, AutoUpgradeSkippedCondition)
		switch {
		case c.reason == "" && cond != nil:
			t.Errorf("%s: the upgrade it took carries the note %+v", c.name, cond)
		case c.reason != "" && (cond == nil || cond.Reason != c.reason || !strings.Contains(cond.Message, "v1.37.1+rke2r1")):
			t.Errorf("%s: note %+v, want the reason %s and the version it holds back", c.name, cond, c.reason)
		}
	}
}

// Where the next minor is stable but missing from the list, as when it is unpinned, the cluster
// still takes the newest patch of its own.
func TestTargetTakesPatchReleasesWhereTheStableMinorIsNotListed(t *testing.T) {
	t.Parallel()
	ch := channels(37, "v1.35.9+rke2r1", "v1.36.5+rke2r2")
	if got := ch.Target("v1.36.2+rke2r1", "minor"); got != "v1.36.5+rke2r2" {
		t.Errorf("Target = %q, want v1.36.5+rke2r2", got)
	}
}

func twoVersions() []clusterv1.StatusVersion {
	return []clusterv1.StatusVersion{{Version: "v1.36.1"}, {Version: "v1.36.2"}}
}

// The restore, the pause, the Available condition, the mixed versions and the worker rollout each
// hold an upgrade back on their own.
func TestEachSignOfAnUnsettledClusterHoldsAnUpgradeBack(t *testing.T) {
	t.Parallel()
	one, none := int32(1), int32(0)
	policy := map[string]string{"upgrades": `{"mode":"patch"}`}
	for name, unsettle := range map[string]func(*clusterv1.Cluster){
		"restore":     func(c *clusterv1.Cluster) { c.Annotations = map[string]string{EtcdRestoreAnnotation: "{}"} },
		"pause":       func(c *clusterv1.Cluster) { c.Spec.Paused = &[]bool{true}[0] },
		"available":   func(c *clusterv1.Cluster) { c.Status.Conditions[0].Status = metav1.ConditionFalse },
		"topology":    func(c *clusterv1.Cluster) { c.Status.Conditions[1].Status = metav1.ConditionFalse },
		"generation":  func(c *clusterv1.Cluster) { c.Status.ObservedGeneration = 1 },
		"cp-versions": func(c *clusterv1.Cluster) { c.Status.ControlPlane.Versions = twoVersions() },
		"wk-versions": func(c *clusterv1.Cluster) { c.Status.Workers.Versions = twoVersions() },
		"workers": func(c *clusterv1.Cluster) {
			c.Status.Workers.DesiredReplicas, c.Status.Workers.UpToDateReplicas = &one, &none
		},
	} {
		c := settledCluster("c", "v1.36.2+rke2r1", policy)
		unsettle(c)
		u := &Upgrader{Releases: fixedReleases{"rke2": channels(36, "v1.36.5+rke2r1")}}
		if got := clusterAfter(t, upgradeRound(t, u, c), "c").Spec.Topology.Version; got != "v1.36.2+rke2r1" {
			t.Errorf("%s: upgraded to %s", name, got)
		}
	}
}

type osReleases map[string]Channels

func (f osReleases) Channels(_ context.Context, _ string, os templates.NodeOS) (Channels, error) {
	return f[os.String()], nil
}

// Clusters of one distribution on two node OSes get the versions of their own, in one round.
func TestUpgraderReadsTheChannelsOfEachNodeOS(t *testing.T) {
	t.Parallel()
	kubeadm := func(name, osVar string) *clusterv1.Cluster {
		vars := map[string]string{"upgrades": `{"mode":"patch"}`}
		if osVar != "" {
			vars[NodeOSVariable] = osVar
		}
		c := settledCluster(name, "v1.36.2", vars)
		c.Spec.Topology.ClassRef.Name = ClassPrefix + "kubeadm"
		return c
	}
	u := &Upgrader{Releases: osReleases{
		templates.DefaultNodeOS.String():                       channels(36, "v1.36.5"),
		templates.NodeOS{Family: "rocky", Release: "10"}.String(): channels(36, "v1.36.7"),
	}}
	kube := upgradeRound(t, u, kubeadm("alma", ""), kubeadm("rocky", `{"family":"rocky","release":"10"}`))
	for name, want := range map[string]string{"alma": "v1.36.5", "rocky": "v1.36.7"} {
		if got := clusterAfter(t, kube, name).Spec.Topology.Version; got != want {
			t.Errorf("%s: version %s, want %s", name, got, want)
		}
	}
}

// A cluster being deleted, and one of a class that is not Kubehenge's, are left alone.
func TestUpgraderLeavesAClusterBeingDeletedOrOfAnotherClassAlone(t *testing.T) {
	t.Parallel()
	deleting := settledCluster("deleting", "v1.36.2+rke2r1", map[string]string{"upgrades": `{"mode":"patch"}`})
	deleting.Finalizers = []string{"test"}
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)}
	foreign := settledCluster("foreign", "v1.36.2+rke2r1", map[string]string{"upgrades": `{"mode":"patch"}`})
	foreign.Spec.Topology.ClassRef.Name = "someone-elses-rke2"
	u := &Upgrader{Releases: fixedReleases{"rke2": channels(36, "v1.36.5+rke2r1"), "someone-elses-rke2": channels(36, "v1.36.5+rke2r1")}}
	kube := upgradeRound(t, u, deleting, foreign)
	for _, name := range []string{"deleting", "foreign"} {
		if got := clusterAfter(t, kube, name).Spec.Topology.Version; got != "v1.36.2+rke2r1" {
			t.Errorf("%s: upgraded to %s", name, got)
		}
	}
}
