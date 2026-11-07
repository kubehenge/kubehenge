package provisioner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	kernelGood  = "6.12.0-124.8.1.el10_1.x86_64"
	kernelOlder = "6.12.0-124.2.1.el10_1.x86_64"
	kernelNewer = "6.12.0-124.10.1.el10_1.x86_64"
	rke2Line    = "kubehenge-rke2-almalinux-10"
)

// olderRefresh is a refresh of the RKE2 line with one good build, whose next builds install kernels.
func olderRefresh(t *testing.T, kernels ...string) (*TemplateRefresh, *fakePool, *[]string) {
	t.Helper()
	pool := &fakePool{kernels: map[string][]string{rke2Line: kernels}}
	pool.add(t, &templates.Request{Distribution: templates.RKE2}, day(1))
	pool.record(pool.list[0].VMID, kernelGood)
	var log []string
	r := &TemplateRefresh{
		Kube: refreshKube(t), Pool: pool, ClassNamespace: "kubehenge-system", Keep: DefaultBuildsKept, Soak: DefaultNodeImageSoak,
		Now: func() time.Time { return day(8) },
		Log: func(format string, _ ...any) { log = append(log, format) },
	}
	return r, pool, &log
}

func TestTemplateRefreshNeverWaitsForABuildWithAnOlderKernel(t *testing.T) {
	t.Parallel()
	r, pool, log := olderRefresh(t, kernelOlder, kernelNewer)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One build, kept as the newest of the line, with the controller asked for the next.
	if len(pool.built) != 1 || len(pool.deleted) != 0 || len(pool.list) != 2 {
		t.Errorf("built %v, deleted %v, left %d templates", pool.built, pool.deleted, len(pool.list))
	}
	if !slices.ContainsFunc(*log, func(l string) bool { return strings.Contains(l, "has an older kernel") }) {
		t.Errorf("the log says nothing of the older build: %v", *log)
	}
}

func TestTemplateRefreshBuildsALineAgainWhenAsked(t *testing.T) {
	t.Parallel()
	again := []templates.Request{{Distribution: templates.RKE2}}
	for name, missingOnly := range map[string]bool{"a full run": false, "only the lines asked for": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, pool, _ := olderRefresh(t, kernelNewer)
			r.MissingOnly, r.Again = missingOnly, again
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(pool.built) != 1 {
				t.Errorf("built %v, want the one line once", pool.built)
			}
		})
	}
	t.Run("not asked", func(t *testing.T) {
		t.Parallel()
		r, pool, _ := olderRefresh(t, kernelNewer)
		r.MissingOnly = true
		if err := r.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(pool.built) != 0 {
			t.Errorf("built %v, want none", pool.built)
		}
	})
}

func TestTemplateRefreshMovesOnFromAnEqualOrNewerKernel(t *testing.T) {
	t.Parallel()
	for name, kernel := range map[string]string{"equal": kernelGood, "newer": kernelNewer} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, pool, log := olderRefresh(t, kernel)
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(pool.built) != 1 || slices.ContainsFunc(*log, func(l string) bool { return strings.Contains(l, "has an older kernel") }) {
				t.Errorf("built %v, log %v", pool.built, *log)
			}
		})
	}
}

func TestOlderBuildsAreLeftOutOfWhatClustersMoveTo(t *testing.T) {
	t.Parallel()
	var p poolTemplates
	p.builds, p.older, p.facts = NodeImageBuilds{}, NodeImageBuilds{}, map[string]map[time.Time]templates.BuildFacts{}
	p.kubeadm = map[templates.NodeOS][]kubeVersion{}
	p.nodeOSes = make(map[templates.Distribution][]templates.NodeOS)
	line, err := templates.Plan(&templates.Request{Distribution: templates.RKE2})
	if err != nil {
		t.Fatal(err)
	}
	for i, kernel := range []string{kernelGood, kernelOlder, kernelNewer, kernelOlder} {
		build, buildErr := line.Build(day(1 + 7*i))
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		p.add(build.Tags, &templates.BuildFacts{Kernel: kernel})
	}
	// A build from before builds recorded their kernel never counts as older.
	legacy, err := line.Build(day(1).Add(-24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p.add(legacy.Tags, nil)
	p.sort()
	key := templates.LineKey(line.Tags)
	if got := len(p.builds[key]); got != 3 {
		t.Errorf("clusters may move to %d builds, want the legacy one, the first and the third", got)
	}
	if got := p.older[key]; !slices.Equal(got, []time.Time{day(8), day(22)}) {
		t.Errorf("older builds: %v", got)
	}
}

func TestBuildUsersNeverNameAnOlderBuild(t *testing.T) {
	t.Parallel()
	pool := &fakePool{}
	cloud := templates.Request{Distribution: templates.RKE2}
	for i, kernel := range []string{kernelGood, kernelOlder} {
		pool.add(t, &cloud, day(1+7*i))
		pool.record(pool.list[i].VMID, kernel)
	}
	web, _ := refreshCluster(t, "web", nil)
	web.Annotations[NodeImageAnnotation] = day(20).Format(time.RFC3339)
	facts, _ := pool.Facts(context.Background())
	users, err := BuildUsers(context.Background(), refreshKube(t, web), "kubehenge-system", pool.list, facts, DefaultNodeImageSoak)
	if err != nil {
		t.Fatal(err)
	}
	for key, who := range users {
		if strings.Contains(key, templates.BuildTag(day(8))) {
			t.Errorf("%v clone from the build with the older kernel", who)
		}
	}
	if len(users) != 1 {
		t.Errorf("users: %v", users)
	}
}

func TestOlderBuildStaysOnlyWhileItIsTheNewest(t *testing.T) {
	t.Parallel()
	res := func(id uint64, at time.Time) templates.Built {
		return templates.Built{Resource: &proxmox.ClusterResource{VMID: id, Tags: "kubehenge;rke2;almalinux-10;" + templates.BuildTag(at)}, At: at}
	}
	key := "almalinux-10;kubehenge;rke2"
	builds := map[string][]templates.Built{key: {res(4, day(22)), res(3, day(15)), res(2, day(8)), res(1, day(1))}}
	// Build 4 has an older kernel than 3: it stays as the newest, and 3 and 2 fill the two places.
	got := unneededBuilds(builds, map[uint64]bool{4: true}, nil, nil, 2, DefaultNodeImageSoak)
	if len(got) != 1 || got[0].Resource.VMID != 1 {
		t.Errorf("deleted %v, want only the oldest", got)
	}
	// A newer build exists beside it: the older one no longer stays.
	builds[key] = append([]templates.Built{res(5, day(29))}, builds[key]...)
	got = unneededBuilds(builds, map[uint64]bool{4: true}, nil, nil, 2, DefaultNodeImageSoak)
	ids := make([]uint64, 0, len(got))
	for _, b := range got {
		ids = append(ids, b.Resource.VMID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []uint64{1, 2, 4}) {
		t.Errorf("deleted builds %v, want 1, 2 and 4", ids)
	}
}
