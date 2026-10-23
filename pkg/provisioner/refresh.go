package provisioner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// TemplatePool is what a refresh does in the template pool; *templates.Builder is the real one.
type TemplatePool interface {
	List(ctx context.Context) ([]*proxmox.ClusterResource, error)
	Create(ctx context.Context, t *templates.Template) (int, error)
	RemoveLeftovers(ctx context.Context, line *templates.Template, olderThan time.Duration, now time.Time) ([]int, error)
	DeleteBuild(ctx context.Context, r *proxmox.ClusterResource) error
}

// DefaultBuildsKept is how many builds of a line a refresh keeps beside those still in use: the
// newest, and the one before for a quick step back.
const DefaultBuildsKept = 2

// leftoverAge is how old a VM a build left must be before a refresh removes it: longer than a build
// takes with every step at its timeout, so that a build another run has under way is left alone.
const leftoverAge = 3 * time.Hour

// TemplateRefresh builds the template lines clusters run again, with the updates their node OS
// published since, and deletes the builds nothing can clone from any more.
type TemplateRefresh struct {
	// Kube reads the clusters and their machines in every namespace.
	Kube           client.Reader
	Pool           TemplatePool
	ClassNamespace string
	// Keep is how many builds of each line stay beside those in use.
	Keep int
	Soak time.Duration
	Now  func() time.Time
	Log  func(format string, args ...any)
	// Missing are lines to build if they have no build yet, such as a Kubernetes version a cluster
	// creator asked for. With MissingOnly, those are all a run builds.
	Missing     []templates.Request
	MissingOnly bool
	// Planned, if set, is told the lines the run builds before it starts on them.
	Planned func(lines []templates.Template)
}

// Run builds, then collects the builds no longer needed. A line that fails to build leaves the
// others to go on; every failure is in the error.
func (r *TemplateRefresh) Run(ctx context.Context) error {
	clusters, err := r.clusters(ctx)
	if err != nil {
		return err
	}
	list, err := r.Pool.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the templates: %w", err)
	}
	lines, err := r.toBuild(clusters, poolBuilds(list))
	if err != nil {
		return err
	}
	if r.Planned != nil {
		r.Planned(lines)
	}
	var errs []error
	for i := range lines {
		if err := r.build(ctx, &lines[i]); err != nil {
			r.logf("building %s failed: %v", lines[i].LineName, err)
			errs = append(errs, fmt.Errorf("building %s: %w", lines[i].LineName, err))
		}
	}
	if err := r.collect(ctx, clusters); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// refreshedCluster is what a refresh needs of a cluster: the lines its machines run and its node
// image time.
type refreshedCluster struct {
	name  string
	lines [][]string
	t     time.Time
}

func (r *TemplateRefresh) clusters(ctx context.Context) ([]refreshedCluster, error) {
	var list clusterv1.ClusterList
	if err := r.Kube.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing the clusters: %w", err)
	}
	var out []refreshedCluster
	for i := range list.Items {
		c := &list.Items[i]
		if !Owns(c, r.ClassNamespace) || !c.DeletionTimestamp.IsZero() || !RefreshesNodes(c) {
			continue
		}
		lines, err := ClusterLines(c)
		if err != nil {
			return nil, err
		}
		t, err := NodeImageTime(c)
		if err != nil {
			return nil, err
		}
		out = append(out, refreshedCluster{name: c.Namespace + "/" + c.Name, lines: lines, t: t})
	}
	return out, nil
}

// MachinesBeingCreated stands among a build's users for the machines still waiting for their VM.
const MachinesBeingCreated = "machines being created"

// BuildUsers names who clones from each build in a template listing, by the build's tags as
// templates.NormaliseTags orders them, joined with ";": the clusters whose node image time picks it
// for a line their machines run, and MachinesBeingCreated where a machine waiting for its VM selects
// it. These are the builds a refresh never deletes, and deleting one moves those clusters' nodes
// at once, outside their maintenance windows.
func BuildUsers(ctx context.Context, kube client.Reader, classNamespace string, list []*proxmox.ClusterResource, soak time.Duration) (map[string][]string, error) {
	r := &TemplateRefresh{Kube: kube, ClassNamespace: classNamespace}
	clusters, err := r.clusters(ctx)
	if err != nil {
		return nil, err
	}
	pending, err := r.pendingSelections(ctx)
	if err != nil {
		return nil, err
	}
	record := buildTimes(poolBuilds(list))
	users := map[string][]string{}
	for _, c := range clusters {
		for _, line := range c.lines {
			at, ok := record.Pick(line, c.t, soak)
			if !ok {
				continue
			}
			key := strings.Join(templates.NormaliseTags(append(slices.Clone(line), templates.BuildTag(at))), ";")
			if !slices.Contains(users[key], c.name) {
				users[key] = append(users[key], c.name)
			}
		}
	}
	for key := range pending {
		users[key] = append(users[key], MachinesBeingCreated)
	}
	return users, nil
}

// poolBuilds groups the builds in a template listing by line key, newest first.
func poolBuilds(list []*proxmox.ClusterResource) map[string][]templates.Built {
	out := map[string][]templates.Built{}
	for _, res := range list {
		tags := templates.ParseTags(res.Tags)
		if at, built := templates.BuildOf(tags); built {
			key := templates.LineKey(tags)
			out[key] = append(out[key], templates.Built{Resource: res, At: at})
		}
	}
	for key := range out {
		slices.SortFunc(out[key], func(x, y templates.Built) int { return y.At.Compare(x.At) })
	}
	return out
}

// toBuild are the lines this run builds, refreshedRequests's, then each missing line asked for
// that has no build.
func (r *TemplateRefresh) toBuild(clusters []refreshedCluster, builds map[string][]templates.Built) ([]templates.Template, error) {
	var reqs []templates.Request
	if !r.MissingOnly {
		reqs = refreshedRequests(clusters, builds)
	}
	var out []templates.Template
	seen := map[string]bool{}
	add := func(req templates.Request, onlyMissing bool) error {
		t, err := templates.Plan(&req)
		if err != nil {
			return fmt.Errorf("planning the %s template: %w", req.Distribution, err)
		}
		key := templates.LineKey(t.Tags)
		if seen[key] || onlyMissing && len(builds[key]) > 0 {
			return nil
		}
		seen[key] = true
		out = append(out, t)
		return nil
	}
	for _, req := range reqs {
		if err := add(req, false); err != nil {
			return nil, err
		}
	}
	for _, req := range r.Missing {
		if err := add(req, true); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// refreshedRequests are the lines every run builds: RKE2's on the default node OS, so that a first
// cluster finds its template; every line a cluster's machines run; and every RKE2 and k3s line the
// pool holds, which someone asked for and no Kubernetes version outdates. A line of a node OS that
// can no longer be built is left as it is.
func refreshedRequests(clusters []refreshedCluster, builds map[string][]templates.Built) []templates.Request {
	reqs := []templates.Request{{Distribution: templates.RKE2, NodeOS: templates.DefaultNodeOS}}
	add := func(line []string, kubeadm bool) {
		req, ok := templates.RequestOf(line)
		if !ok || req.Distribution == templates.Kubeadm && !kubeadm {
			return
		}
		if _, _, err := templates.Available(req.NodeOS); err == nil && !slices.Contains(reqs, req) {
			reqs = append(reqs, req)
		}
	}
	for _, c := range clusters {
		for _, line := range c.lines {
			add(line, true)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(builds)) {
		add(strings.Split(key, ";"), false)
	}
	return reqs
}

// build makes a new build of a line, first removing what an earlier run that did not finish left.
func (r *TemplateRefresh) build(ctx context.Context, line *templates.Template) error {
	now := r.Now()
	if _, err := r.Pool.RemoveLeftovers(ctx, line, leftoverAge, now); err != nil {
		return fmt.Errorf("removing what earlier builds of %s left: %w", line.LineName, err)
	}
	build, err := line.Build(now)
	if err != nil {
		return fmt.Errorf("naming the build of %s: %w", line.LineName, err)
	}
	r.logf("building %s", build.Name)
	vmid, err := r.Pool.Create(ctx, &build)
	if err != nil {
		return fmt.Errorf("building %s: %w", build.Name, err)
	}
	r.logf("template %s is VM %d", build.Name, vmid)
	return nil
}

// collect deletes the builds nothing needs: of each line, all but the newest Keep and those still
// in use. In use is the build a cluster's node image time picks for a line its machines run, and
// one a machine still waiting for its VM selects. A kubeadm line no cluster runs or can upgrade to
// keeps only its newest; the last build of a line is never deleted here.
func (r *TemplateRefresh) collect(ctx context.Context, clusters []refreshedCluster) error {
	list, err := r.Pool.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the templates: %w", err)
	}
	pending, err := r.pendingSelections(ctx)
	if err != nil {
		return err
	}
	builds := poolBuilds(list)
	var errs []error
	for _, b := range unneededBuilds(builds, clusters, pending, max(r.Keep, 1), r.Soak) {
		r.logf("deleting %s (VM %d), which nothing clones from", b.Resource.Name, b.Resource.VMID)
		if err := r.Pool.DeleteBuild(ctx, b.Resource); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", b.Resource.Name, err))
		}
	}
	return errors.Join(errs...)
}

// unneededBuilds are the builds collect deletes.
func unneededBuilds(builds map[string][]templates.Built, clusters []refreshedCluster, pending map[string]bool, keep int, soak time.Duration) []templates.Built {
	record := buildTimes(builds)
	u := usageOf(record, clusters, soak)
	var out []templates.Built
	for _, key := range record.Lines() {
		kept := keep
		if !u.run[key] && !reachable(key, u.kubeadm) {
			kept = 1
		}
		for i, b := range builds[key] {
			full := strings.Join(templates.ParseTags(b.Resource.Tags), ";")
			if i < kept || u.used[key+";"+templates.BuildTag(b.At)] || pending[full] {
				continue
			}
			out = append(out, b)
		}
	}
	return out
}

// buildTimes are the times of the pool's builds, oldest first, as the ClusterClasses record them.
func buildTimes(builds map[string][]templates.Built) NodeImageBuilds {
	record := NodeImageBuilds{}
	for key, list := range builds {
		for _, b := range list {
			record[key] = append(record[key], b.At)
		}
		slices.SortFunc(record[key], time.Time.Compare)
	}
	return record
}

// buildUsage is what the clusters run: the lines (run), each line's build a cluster picks (used,
// by line and build tag), and the Kubernetes versions of kubeadm lines by node OS.
type buildUsage struct {
	run, used map[string]bool
	kubeadm   map[templates.NodeOS][]kubeVersion
}

func usageOf(record NodeImageBuilds, clusters []refreshedCluster, soak time.Duration) buildUsage {
	u := buildUsage{run: map[string]bool{}, used: map[string]bool{}, kubeadm: map[templates.NodeOS][]kubeVersion{}}
	for _, c := range clusters {
		for _, line := range c.lines {
			key := templates.LineKey(line)
			u.run[key] = true
			if at, ok := record.Pick(line, c.t, soak); ok {
				u.used[key+";"+templates.BuildTag(at)] = true
			}
			if v, ok := kubeadmVersionOf(line); ok {
				os, _ := templates.NodeOSOf(line)
				u.kubeadm[os] = append(u.kubeadm[os], v)
			}
		}
	}
	return u
}

// reachable tells whether a line is one clusters may still need: any but a kubeadm one, or a
// kubeadm version above one a cluster on its node OS runs, which an upgrade could take.
func reachable(key string, running map[templates.NodeOS][]kubeVersion) bool {
	tags := strings.Split(key, ";")
	v, ok := kubeadmVersionOf(tags)
	if !ok {
		return true
	}
	os, _ := templates.NodeOSOf(tags)
	return slices.ContainsFunc(running[os], func(r kubeVersion) bool { return r.less(v) })
}

// pendingSelections are the tag sets the ProxmoxMachines still waiting for their VM select, which
// they clone from any moment.
func (r *TemplateRefresh) pendingSelections(ctx context.Context) (map[string]bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("infrastructure.cluster.x-k8s.io/" + proxmoxMachineVersion)
	list.SetKind("ProxmoxMachineList")
	if err := r.Kube.List(ctx, list); err != nil {
		return nil, fmt.Errorf("listing the ProxmoxMachines: %w", err)
	}
	out := map[string]bool{}
	for _, pm := range list.Items {
		provisioned, _, _ := unstructured.NestedBool(pm.Object, "status", "initialization", "provisioned")
		if provisioned {
			continue
		}
		tags, _, _ := unstructured.NestedStringSlice(pm.Object, "spec", "templateSelector", "matchTags")
		if len(tags) > 0 {
			out[strings.Join(templates.NormaliseTags(tags), ";")] = true
		}
	}
	return out, nil
}

func (r *TemplateRefresh) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}
