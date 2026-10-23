package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// fakePool is a template pool in memory: each template a resource whose tags say its line and build.
type fakePool struct {
	list     []*proxmox.ClusterResource
	built    []string
	deleted  []string
	leftover []string
	fail     map[string]bool
}

func (p *fakePool) List(context.Context) ([]*proxmox.ClusterResource, error) {
	return slices.Clone(p.list), nil
}

func (p *fakePool) Create(_ context.Context, t *templates.Template) (int, error) {
	if p.fail[t.LineName] {
		return 0, errors.New("the mirror is down")
	}
	p.built = append(p.built, t.Name)
	id := 9100 + len(p.list)
	p.list = append(p.list, &proxmox.ClusterResource{VMID: uint64(id), Name: t.Name, Tags: strings.Join(t.Tags, ";")})
	return id, nil
}

func (p *fakePool) RemoveLeftovers(_ context.Context, line *templates.Template, _ time.Duration, _ time.Time) ([]int, error) {
	p.leftover = append(p.leftover, line.LineName)
	return nil, nil
}

func (p *fakePool) DeleteBuild(_ context.Context, r *proxmox.ClusterResource) error {
	p.deleted = append(p.deleted, r.Name)
	p.list = slices.DeleteFunc(p.list, func(x *proxmox.ClusterResource) bool { return x == r })
	return nil
}

func (p *fakePool) add(t *testing.T, req *templates.Request, at time.Time) {
	t.Helper()
	line, err := templates.Plan(req)
	if err != nil {
		t.Fatal(err)
	}
	build, err := line.Build(at)
	if err != nil {
		t.Fatal(err)
	}
	p.list = append(p.list, &proxmox.ClusterResource{VMID: uint64(9000 + len(p.list)), Name: build.Name, Tags: strings.Join(build.Tags, ";")})
}

func refreshKube(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clusterv1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	gv := schema.GroupVersion{Group: "infrastructure.cluster.x-k8s.io", Version: proxmoxMachineVersion}
	scheme.AddKnownTypeWithName(gv.WithKind("ProxmoxMachine"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gv.WithKind("ProxmoxMachineList"), &unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func kubeadmCluster(name, version string, at time.Time) *clusterv1.Cluster {
	c := settledCluster(name, version, nil)
	c.Spec.Topology.ClassRef.Name = "kubehenge-kubeadm"
	c.Annotations = map[string]string{NodeImageAnnotation: at.Format(time.RFC3339)}
	return c
}

func TestTemplateRefreshBuildsWhatClustersRunAndCollectsTheRest(t *testing.T) {
	t.Parallel()
	pool := &fakePool{}
	cloud := templates.Request{Distribution: templates.RKE2}
	v136 := templates.Request{Distribution: templates.Kubeadm, KubernetesVersion: "v1.36.5"}
	v135 := templates.Request{Distribution: templates.Kubeadm, KubernetesVersion: "v1.35.9"}
	v137 := templates.Request{Distribution: templates.Kubeadm, KubernetesVersion: "v1.37.1"}
	for _, at := range []time.Time{day(1), day(8), day(15)} {
		pool.add(t, &cloud, at)
		pool.add(t, &v136, at)
	}
	// No cluster runs v1.35.9 and none can upgrade to it; v1.37.1 is a step up from v1.36.5.
	pool.add(t, &v135, day(1))
	pool.add(t, &v135, day(8))
	pool.add(t, &v137, day(1))
	pool.add(t, &v137, day(8))
	// A line of another node OS no cluster runs yet, kept fresh for the clusters made of it.
	pool.add(t, &templates.Request{Distribution: templates.K3s, NodeOS: templates.NodeOS{Family: "rocky", Release: "10"}}, day(1))
	web, _ := refreshCluster(t, "web", nil)
	web.Annotations[NodeImageAnnotation] = day(3).Format(time.RFC3339)
	kube := refreshKube(t, web, kubeadmCluster("api", "v1.36.5", day(20)))
	now := day(22)
	r := &TemplateRefresh{
		Kube: kube, Pool: pool, ClassNamespace: "kubehenge-system", Keep: DefaultBuildsKept, Soak: DefaultNodeImageSoak,
		Now: func() time.Time { return now }, Missing: []templates.Request{v137, {Distribution: templates.Kubeadm, KubernetesVersion: "v1.38.0"}},
	}
	var planned []string
	r.Planned = func(lines []templates.Template) {
		for _, l := range lines {
			planned = append(planned, l.LineName)
		}
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(planned, []string{
		"kubehenge-rke2-almalinux-10", "kubehenge-kubeadm-v1.36.5-almalinux-10", "kubehenge-k3s-rocky-10", "kubehenge-kubeadm-v1.38.0-almalinux-10",
	}) {
		t.Errorf("planned %v", planned)
	}
	if len(pool.built) != 4 || !slices.Equal(pool.leftover, planned) {
		t.Errorf("built %v after clearing %v", pool.built, pool.leftover)
	}
	// Of RKE2's: web runs the build of the 1st, and the two newest stay. kubeadm v1.36.5's: the two
	// newest, the 22nd's and the 15th's, which api runs. v1.35.9 keeps its newest.
	slices.Sort(pool.deleted)
	want := []string{
		"kubehenge-kubeadm-v1.35.9-almalinux-10-build-202610010000", "kubehenge-kubeadm-v1.36.5-almalinux-10-build-202610010000",
		"kubehenge-kubeadm-v1.36.5-almalinux-10-build-202610080000", "kubehenge-rke2-almalinux-10-build-202610080000",
	}
	if !slices.Equal(pool.deleted, want) {
		t.Errorf("deleted %v, want %v", pool.deleted, want)
	}
}

func TestBuildUsersNameTheClustersOfEachBuild(t *testing.T) {
	t.Parallel()
	pool := &fakePool{}
	cloud := templates.Request{Distribution: templates.RKE2}
	v136 := templates.Request{Distribution: templates.Kubeadm, KubernetesVersion: "v1.36.5"}
	for _, at := range []time.Time{day(1), day(8), day(15)} {
		pool.add(t, &cloud, at)
		pool.add(t, &v136, at)
	}
	web, _ := refreshCluster(t, "web", nil)
	web.Annotations[NodeImageAnnotation] = day(3).Format(time.RFC3339)
	api := kubeadmCluster("api", "v1.36.5", day(20))
	users, err := BuildUsers(context.Background(), refreshKube(t, web, api), "kubehenge-system", pool.list, DefaultNodeImageSoak)
	if err != nil {
		t.Fatal(err)
	}
	of := func(req templates.Request, at time.Time) []string {
		line, err := templates.Plan(&req)
		if err != nil {
			t.Fatal(err)
		}
		build, err := line.Build(at)
		if err != nil {
			t.Fatal(err)
		}
		return users[strings.Join(templates.NormaliseTags(build.Tags), ";")]
	}
	if got := of(cloud, day(1)); !slices.Equal(got, []string{web.Namespace + "/web"}) {
		t.Errorf("the cloud image's first build: %v", got)
	}
	if got := of(v136, day(15)); !slices.Equal(got, []string{api.Namespace + "/api"}) {
		t.Errorf("v1.36.5's newest build: %v", got)
	}
	if got := of(cloud, day(15)); len(got) != 0 {
		t.Errorf("the cloud image's newest build, which nothing runs: %v", got)
	}
}

func TestTemplateRefreshKeepsWhatAMachineBeingMadeSelects(t *testing.T) {
	t.Parallel()
	pool := &fakePool{}
	cloud := templates.Request{Distribution: templates.RKE2}
	for _, at := range []time.Time{day(1), day(8), day(15)} {
		pool.add(t, &cloud, at)
	}
	line, err := templates.Plan(&cloud)
	if err != nil {
		t.Fatal(err)
	}
	old, err := line.Build(day(1))
	if err != nil {
		t.Fatal(err)
	}
	pending := &unstructured.Unstructured{}
	pending.SetAPIVersion("infrastructure.cluster.x-k8s.io/" + proxmoxMachineVersion)
	pending.SetKind("ProxmoxMachine")
	pending.SetNamespace("clusters")
	pending.SetName("web-abc")
	if err := unstructured.SetNestedStringSlice(pending.Object, old.Tags, "spec", "templateSelector", "matchTags"); err != nil {
		t.Fatal(err)
	}
	done := pending.DeepCopy()
	done.SetName("web-done")
	if err := unstructured.SetNestedField(done.Object, true, "status", "initialization", "provisioned"); err != nil {
		t.Fatal(err)
	}
	r := &TemplateRefresh{
		Kube: refreshKube(t, pending, done), Pool: pool, ClassNamespace: "kubehenge-system", Keep: DefaultBuildsKept,
		Soak: DefaultNodeImageSoak, Now: func() time.Time { return day(22) }, MissingOnly: true,
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pool.built) != 0 || len(pool.deleted) != 0 {
		t.Errorf("built %v, deleted %v", pool.built, pool.deleted)
	}
}

// Of each Talos minor the two newest releases' templates stay, and any older one a cluster boots
// or is upgraded to, a machine being made selects, or the run was asked to make.
func TestTemplateRefreshCollectsTalosTemplatesByMinor(t *testing.T) {
	t.Parallel()
	pool := &fakePool{}
	talos := func(v string) templates.Template {
		line, err := templates.Plan(&templates.Request{Distribution: templates.Talos, TalosVersion: v})
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	for _, v := range []string{"v1.12.1", "v1.13.8", "v1.13.9", "v1.13.10", "v1.13.11", "v1.13.12", "v1.14.0", "v1.14.2", "v1.14.3", "v1.14.4"} {
		line := talos(v)
		pool.list = append(pool.list, &proxmox.ClusterResource{VMID: uint64(9000 + len(pool.list)), Name: line.Name, Tags: strings.Join(line.Tags, ";")})
	}
	web := talosCluster("web", "v1.14.2")
	web.Annotations[TalosImageAnnotation] = "v1.13.10"
	pending := &unstructured.Unstructured{}
	pending.SetAPIVersion("infrastructure.cluster.x-k8s.io/" + proxmoxMachineVersion)
	pending.SetKind("ProxmoxMachine")
	pending.SetNamespace("clusters")
	pending.SetName("api-abc")
	if err := unstructured.SetNestedStringSlice(pending.Object, talos("v1.13.8").Tags, "spec", "templateSelector", "matchTags"); err != nil {
		t.Fatal(err)
	}
	r := &TemplateRefresh{
		Kube: refreshKube(t, web, pending), Pool: pool, ClassNamespace: "kubehenge-system", Keep: DefaultBuildsKept,
		Soak: DefaultNodeImageSoak, Now: func() time.Time { return day(22) }, MissingOnly: true,
		Missing: []templates.Request{{Distribution: templates.Talos, TalosVersion: "v1.14.1"}},
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pool.built, []string{"kubehenge-talos-v1.14.1"}) {
		t.Errorf("built %v", pool.built)
	}
	slices.Sort(pool.deleted)
	if want := []string{"kubehenge-talos-v1.13.9", "kubehenge-talos-v1.14.0"}; !slices.Equal(pool.deleted, want) {
		t.Errorf("deleted %v, want %v", pool.deleted, want)
	}
}

func TestTemplateRefreshGoesOnPastAFailedLine(t *testing.T) {
	t.Parallel()
	pool := &fakePool{fail: map[string]bool{"kubehenge-rke2-almalinux-10": true}}
	kube := refreshKube(t, kubeadmCluster("api", "v1.36.5", day(20)))
	r := &TemplateRefresh{Kube: kube, Pool: pool, ClassNamespace: "kubehenge-system", Keep: 2, Now: func() time.Time { return day(22) }}
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kubehenge-rke2-almalinux-10") || len(pool.built) != 1 {
		t.Errorf("error %v, built %v", err, pool.built)
	}
}

func refreshCronJob() *batchv1.CronJob {
	cron := &batchv1.CronJob{Namespace: "kubehenge-system", Name: TemplateRefreshName, UID: "cron-uid"}
	cron.Spec.JobTemplate.Labels = map[string]string{TemplateRefreshLabel: "true"}
	cron.Spec.JobTemplate.Spec.Template.Spec.Containers = []corev1.Container{{Name: "refresh", Args: []string{"template", "refresh", "--node=pve1", "--trigger=schedule"}}}
	return cron
}

func request(t *testing.T, by string) string {
	t.Helper()
	raw, err := json.Marshal(TemplateRequest{By: by, At: day(20)})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRequestedJob(t *testing.T) {
	t.Parallel()
	job, err := requestedJob(refreshCronJob(), map[string]string{
		"kubeadm-v1.38.0-almalinux-10": request(t, "dev@pve"), "rke2-rocky-10": request(t, "ops@pve"),
	}, day(20))
	if err != nil {
		t.Fatal(err)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	want := []string{
		"template", "refresh", "--node=pve1", "--trigger=kubeadm-v1.38.0-almalinux-10 for dev@pve; rke2-rocky-10 for ops@pve",
		"--missing=kubeadm-v1.38.0-almalinux-10", "--missing=rke2-rocky-10", "--missing-only",
	}
	if !slices.Equal(args, want) || job.Labels[TemplateRefreshLabel] != "true" || job.OwnerReferences[0].UID != "cron-uid" {
		t.Errorf("job %+v with %v", job.ObjectMeta, args)
	}
	job, err = requestedJob(refreshCronJob(), map[string]string{RefreshRequest: request(t, "admin@pve"), "kubeadm-v1.38.0-almalinux-10": request(t, "dev@pve")}, day(20))
	if err != nil {
		t.Fatal(err)
	}
	if args := job.Spec.Template.Spec.Containers[0].Args; slices.Contains(args, "--missing-only") || !slices.Contains(args, "--missing=kubeadm-v1.38.0-almalinux-10") {
		t.Errorf("a full run: %v", args)
	}
	job, err = requestedJob(refreshCronJob(), map[string]string{"talos-v1.14.4": request(t, "dev@pve")}, day(20))
	if err != nil {
		t.Fatal(err)
	}
	if args := job.Spec.Template.Spec.Containers[0].Args; !slices.Contains(args, "--missing=talos-v1.14.4") {
		t.Errorf("a Talos release: %v", args)
	}
	for name, requests := range map[string]map[string]string{
		"no such line":         {"flatcar-v4230.2.1": request(t, "dev@pve")},
		"no version":           {"kubeadm-1.38-almalinux-10": request(t, "dev@pve")},
		"no node OS":           {"kubeadm-v1.38.0": request(t, "dev@pve")},
		"an unknown node OS":   {"rke2-plan9-4": request(t, "dev@pve")},
		"an unreleased one":    {"rke2-leap-16.1": request(t, "dev@pve")},
		"not written as named": {"rke2-almalinux-10.0": request(t, "dev@pve")},
		"no Talos release":     {"talos-1.14": request(t, "dev@pve")},
		"a Talos node OS":      {"talos-v1.14.4-almalinux-10": request(t, "dev@pve")},
		"no request in it":     {"rke2-almalinux-10": "yes"},
	} {
		if _, err := requestedJob(refreshCronJob(), requests, day(20)); err == nil {
			t.Errorf("%s: a Job made", name)
		}
	}
}

func TestTemplateRequestsStartOneRunAtATime(t *testing.T) {
	t.Parallel()
	requests := &corev1.ConfigMap{Namespace: "kubehenge-system", Name: TemplateRequestsName}
	requests.Data = map[string]string{"rke2-almalinux-10": request(t, "dev@pve")}
	running := &batchv1.Job{Namespace: "kubehenge-system", Name: "scheduled", Labels: map[string]string{TemplateRefreshLabel: "true"}}
	kube := refreshKube(t, requests, refreshCronJob(), running)
	r := &TemplateRequests{Client: kube, Reader: kube, Namespace: "kubehenge-system", Now: func() time.Time { return day(20) }}
	ctx := context.Background()
	if err := r.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := kube.List(ctx, jobs); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("a run started beside one under way: %v, %v", jobs.Items, err)
	}
	running.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := kube.Status().Update(ctx, running); err != nil {
		t.Fatal(err)
	}
	if err := r.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := kube.List(ctx, jobs); err != nil || len(jobs.Items) != 2 {
		t.Fatalf("jobs %v, %v", jobs.Items, err)
	}
	left := &corev1.ConfigMap{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(requests), left); err != nil || len(left.Data) != 0 {
		t.Fatalf("requests left %v, %v", left.Data, err)
	}
	// Nothing asked for, nothing started.
	if err := r.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := kube.List(ctx, jobs); err != nil || len(jobs.Items) != 2 {
		t.Fatalf("jobs %v, %v", jobs.Items, err)
	}
}
