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
	// Facts are what the builds recorded of themselves, by VM ID.
	Facts(ctx context.Context) (map[uint64]templates.BuildFacts, error)
	Create(ctx context.Context, t *templates.Template) (int, error)
	RemoveLeftovers(ctx context.Context, line *templates.Template, olderThan time.Duration, now time.Time) ([]int, error)
	DeleteBuild(ctx context.Context, r *proxmox.ClusterResource) error
}

// DefaultBuildsKept is how many builds of a line a refresh keeps beside those still in use: the
// newest, and the one before for a quick step back.
const DefaultBuildsKept = 2

const (
	// OlderRetries is how many times a line whose newest build has a kernel older than the previous
	// build's is built again, in case its mirror was one sync behind. The controller starts each as
	// a run of its own (TemplateVersions), so no run sleeps for it.
	OlderRetries = 3
	// OlderRetryAfter is the wait before each of those builds.
	OlderRetryAfter = time.Hour
)

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
	// Retry builds a line again after it fails. Its Log is the run's.
	Retry templates.Retry
	// Patches reads the newest release of a Kubernetes minor version. Set, a run also builds the
	// kubeadm templates of those releases (patchRequests). Nil switches that off.
	Patches func(ctx context.Context, minor int) (string, bool, error)
	// Again are lines to build once more though they have builds, because the newest has an older
	// kernel than the build before. With MissingOnly, those are built too.
	Again []templates.Request
}

// madeBuild is a build a run made, and whether its kernel is older than the previous build's.
type madeBuild struct {
	line  *templates.Template
	vmid  int
	older bool
}

// Run builds, then collects the builds no longer needed. A line that fails to build leaves the
// others to go on. Every failure is in the error.
func (r *TemplateRefresh) Run(ctx context.Context) error {
	clusters, err := r.clusters(ctx)
	if err != nil {
		return err
	}
	talos, err := r.talosReleases(ctx)
	if err != nil {
		return err
	}
	list, err := r.Pool.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the templates: %w", err)
	}
	patches, errs := r.patchRequests(ctx, clusters)
	lines, err := r.toBuild(clusters, talos, patches, poolBuilds(list), poolLines(list))
	if err != nil {
		return err
	}
	if r.Planned != nil {
		r.Planned(lines)
	}
	for i := range lines {
		made, err := r.build(ctx, &lines[i])
		if err != nil {
			r.logf("building %s failed: %v", lines[i].LineName, err)
			errs = append(errs, fmt.Errorf("building %s: %w", lines[i].LineName, err))
		} else if made.older {
			r.logf("%s (VM %d) has an older kernel than the build before. No cluster moves to it, and the controller asks for another build in %s (up to %d times)", made.line.LineName, made.vmid, OlderRetryAfter, OlderRetries)
		}
	}
	if err := r.collect(ctx, clusters, talos); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// olderOf tells whether the build made as VM vmid has a kernel older than the previous build of its
// line.
func (r *TemplateRefresh) olderOf(ctx context.Context, vmid int) (bool, error) {
	list, err := r.Pool.List(ctx)
	if err != nil {
		return false, fmt.Errorf("listing the templates: %w", err)
	}
	facts, err := r.Pool.Facts(ctx)
	if err != nil {
		return false, fmt.Errorf("reading what the builds installed: %w", err)
	}
	return olderVMs(poolBuilds(list), facts)[uint64(vmid)], nil //nolint:gosec // a VM ID Proxmox VE handed out is positive
}

// olderVMs are the VM IDs of the builds whose kernel is older than the newest earlier build of
// their line that is not itself older.
func olderVMs(builds map[string][]templates.Built, facts map[uint64]templates.BuildFacts) map[uint64]bool {
	out := map[uint64]bool{}
	for _, newestFirst := range builds {
		oldestFirst := slices.Clone(newestFirst)
		slices.Reverse(oldestFirst)
		recorded := make([]*templates.BuildFacts, len(oldestFirst))
		for i, b := range oldestFirst {
			if f, ok := facts[b.Resource.VMID]; ok {
				recorded[i] = &f
			}
		}
		for i, isOlder := range templates.OlderBuilds(recorded) {
			if isOlder {
				out[oldestFirst[i].Resource.VMID] = true
			}
		}
	}
	return out
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
		if !ownedAndLive(c, r.ClassNamespace) || !RefreshesNodes(c) {
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
// at once, outside their upgrade windows.
func BuildUsers(ctx context.Context, kube client.Reader, classNamespace string, list []*proxmox.ClusterResource, facts map[uint64]templates.BuildFacts, soak time.Duration) (map[string][]string, error) {
	r := &TemplateRefresh{Kube: kube, ClassNamespace: classNamespace}
	clusters, err := r.clusters(ctx)
	if err != nil {
		return nil, err
	}
	pending, err := r.pendingSelections(ctx)
	if err != nil {
		return nil, err
	}
	builds := poolBuilds(list)
	record := buildTimes(builds, olderVMs(builds, facts))
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

// poolLines are the line keys of the templates in a listing that are not builds of a line: those
// made once, as a Talos release's is.
func poolLines(list []*proxmox.ClusterResource) map[string]bool {
	out := map[string]bool{}
	for _, res := range list {
		tags := templates.ParseTags(res.Tags)
		if _, built := templates.BuildOf(tags); !built {
			out[templates.LineKey(tags)] = true
		}
	}
	return out
}

// talosReleases are the Talos releases the Talos clusters' machines boot or are upgraded to.
func (r *TemplateRefresh) talosReleases(ctx context.Context) ([]string, error) {
	var list clusterv1.ClusterList
	if err := r.Kube.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing the clusters: %w", err)
	}
	var out []string
	for i := range list.Items {
		c := &list.Items[i]
		if !ownedAndLive(c, r.ClassNamespace) || !isTalos(c) {
			continue
		}
		for _, release := range []func(*clusterv1.Cluster) (string, error){TalosImage, TalosVersion} {
			v, err := release(c)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

// toBuild are the lines this run builds, refreshedRequests's and the Talos releases', then each
// missing line asked for that has no build. A Talos release's template is made once, so it is built
// only while the pool has none: for a release a Talos cluster boots or is upgraded to, or one asked
// for.
func (r *TemplateRefresh) toBuild(clusters []refreshedCluster, talos []string, patches []templates.Request, builds map[string][]templates.Built, made map[string]bool) ([]templates.Template, error) {
	var reqs []templates.Request
	if !r.MissingOnly {
		reqs = refreshedRequests(clusters, builds)
		for _, v := range talos {
			reqs = append(reqs, templates.Request{Distribution: templates.Talos, TalosVersion: v})
		}
	}
	var out []templates.Template
	seen := map[string]bool{}
	add := func(req templates.Request, onlyMissing bool) error {
		t, err := templates.Plan(&req)
		if err != nil {
			return fmt.Errorf("planning the %s template: %w", req.Distribution, err)
		}
		key := templates.LineKey(t.Tags)
		if seen[key] || !t.Refreshed() && made[key] || onlyMissing && len(builds[key]) > 0 {
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
	for _, req := range slices.Concat(r.Missing, patches) {
		if err := add(req, true); err != nil {
			return nil, err
		}
	}
	for _, req := range r.Again {
		if err := add(req, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// patchRequests are the kubeadm templates of the newest patch of each Kubernetes minor version a
// kubeadm cluster runs, on its node OS, and of the next minor version, which Kubernetes names in its
// release markers. A kubeadm cluster is offered only versions with a template, so without these no
// patch release ever reaches one. They are built only while they have no build: a cluster that
// upgrades to one makes it a line the refresh builds again. A marker that cannot be read is
// reported, and the other minor versions go on.
func (r *TemplateRefresh) patchRequests(ctx context.Context, clusters []refreshedCluster) ([]templates.Request, []error) {
	if r.Patches == nil || r.MissingOnly {
		return nil, nil
	}
	var reqs []templates.Request
	var errs []error
	markers := patchMarkers{read: r.Patches, newest: map[int]string{}, failed: map[int]bool{}}
	for _, c := range clusters {
		for _, line := range c.lines {
			running, ok := kubeadmVersionOf(line)
			nodeOS, known := templates.NodeOSOf(line)
			if _, _, err := templates.Available(nodeOS); !ok || !known || err != nil {
				continue
			}
			for _, minor := range []int{running.Minor, running.Minor + 1} {
				v, err := markers.of(ctx, minor)
				if err != nil {
					r.logf("%v", err)
					errs = append(errs, err)
				}
				reqs = addPatchRequest(reqs, nodeOS, v)
			}
		}
	}
	return reqs, errs
}

// addPatchRequest adds the kubeadm template of release v on nodeOS to reqs, unless v is empty or the
// template is there.
func addPatchRequest(reqs []templates.Request, nodeOS templates.NodeOS, v string) []templates.Request {
	req := templates.Request{Distribution: templates.Kubeadm, NodeOS: nodeOS, KubernetesVersion: v}
	if v == "" || slices.Contains(reqs, req) {
		return reqs
	}
	return append(reqs, req)
}

// patchMarkers reads each minor version's newest release once, and a marker that failed not again.
type patchMarkers struct {
	read   func(ctx context.Context, minor int) (string, bool, error)
	newest map[int]string
	failed map[int]bool
}

// of is the newest release of minor, or "" when Kubernetes has none or the marker could not be
// read. The error is reported on the first failed read only.
func (m *patchMarkers) of(ctx context.Context, minor int) (string, error) {
	if v, read := m.newest[minor]; read {
		return v, nil
	}
	if m.failed[minor] {
		return "", nil
	}
	v, found, err := m.read(ctx, minor)
	if err != nil {
		m.failed[minor] = true
		return "", err
	}
	if !found {
		v = ""
	}
	m.newest[minor] = v
	return v, nil
}

// refreshedRequests are the lines every run builds: RKE2's on the default node OS, so that a first
// cluster finds its template, every line a cluster's machines run, and every RKE2 and k3s line the
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
func (r *TemplateRefresh) build(ctx context.Context, line *templates.Template) (madeBuild, error) {
	now := r.Now()
	retry := r.Retry
	retry.Log = r.logf
	if !line.Refreshed() {
		r.logf("making %s", line.Name)
		vmid, err := retry.Do(ctx, line.Name, func(ctx context.Context) (int, error) { return r.Pool.Create(ctx, line) })
		if err != nil {
			return madeBuild{}, fmt.Errorf("making %s: %w", line.Name, err)
		}
		r.logf("template %s is VM %d", line.Name, vmid)
		return madeBuild{line: line, vmid: vmid}, nil
	}
	if _, err := r.Pool.RemoveLeftovers(ctx, line, leftoverAge, now); err != nil {
		return madeBuild{}, fmt.Errorf("removing what earlier builds of %s left: %w", line.LineName, err)
	}
	build, err := line.Build(now)
	if err != nil {
		return madeBuild{}, fmt.Errorf("naming the build of %s: %w", line.LineName, err)
	}
	r.logf("building %s", build.Name)
	vmid, err := retry.Do(ctx, build.Name, func(ctx context.Context) (int, error) { return r.Pool.Create(ctx, &build) })
	if err != nil {
		return madeBuild{}, fmt.Errorf("building %s: %w", build.Name, err)
	}
	r.logf("template %s is VM %d", build.Name, vmid)
	older, err := r.olderOf(ctx, vmid)
	if err != nil {
		return madeBuild{}, err
	}
	return madeBuild{line: line, vmid: vmid, older: older}, nil
}

// collect deletes the builds nothing needs: of each line, all but the newest Keep and those still
// in use. In use is the build a cluster's node image time picks for a line its machines run, and
// one a machine still waiting for its VM selects. A kubeadm line no cluster runs or can upgrade to
// keeps only its newest. The last build of a line is never deleted here. Talos templates go as
// unneededTalos says.
func (r *TemplateRefresh) collect(ctx context.Context, clusters []refreshedCluster, talos []string) error {
	list, err := r.Pool.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the templates: %w", err)
	}
	pending, err := r.pendingSelections(ctx)
	if err != nil {
		return err
	}
	facts, err := r.Pool.Facts(ctx)
	if err != nil {
		return fmt.Errorf("reading what the builds installed: %w", err)
	}
	builds := poolBuilds(list)
	var errs []error
	for _, b := range unneededBuilds(builds, olderVMs(builds, facts), clusters, pending, max(r.Keep, 1), r.Soak) {
		r.logf("deleting %s (VM %d), which nothing clones from", b.Resource.Name, b.Resource.VMID)
		if err := r.Pool.DeleteBuild(ctx, b.Resource); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", b.Resource.Name, err))
		}
	}
	for _, res := range unneededTalos(list, talos, pending, r.Missing, max(r.Keep, 1)) {
		r.logf("deleting %s (VM %d), which nothing clones from", res.Name, res.VMID)
		if err := r.Pool.DeleteBuild(ctx, res); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", res.Name, err))
		}
	}
	return errors.Join(errs...)
}

// unneededTalos are the Talos templates collect deletes. A Talos release's template is made once,
// so each release stands for a build of its minor's line: of each minor, all but the newest keep
// releases go, unless a cluster's machines boot it or are upgraded to it, a machine still waiting
// for its VM selects it, or the run was asked to make it.
func unneededTalos(list []*proxmox.ClusterResource, inUse []string, pending map[string]bool, asked []templates.Request, keep int) []*proxmox.ClusterResource {
	kept := map[string]bool{}
	for _, v := range inUse {
		kept[v] = true
	}
	for _, req := range asked {
		if req.Distribution == templates.Talos {
			kept[req.TalosVersion] = true
		}
	}
	type release struct {
		res     *proxmox.ClusterResource
		version [3]int
		raw     string
	}
	byMinor := map[int][]release{}
	for _, res := range list {
		tags := templates.ParseTags(res.Tags)
		raw, ok := talosTemplateRelease(tags)
		if !ok {
			continue
		}
		var v [3]int
		if _, err := fmt.Sscanf(raw, "v%d.%d.%d", &v[0], &v[1], &v[2]); err != nil {
			continue
		}
		byMinor[v[1]] = append(byMinor[v[1]], release{res: res, version: v, raw: raw})
	}
	var out []*proxmox.ClusterResource
	for _, minor := range slices.Sorted(maps.Keys(byMinor)) {
		releases := byMinor[minor]
		slices.SortFunc(releases, func(x, y release) int { return slices.Compare(y.version[:], x.version[:]) })
		for i, r := range releases {
			if i < keep || kept[r.raw] || pending[strings.Join(templates.ParseTags(r.res.Tags), ";")] {
				continue
			}
			out = append(out, r.res)
		}
	}
	return out
}

// talosTemplateRelease is the Talos release a template listing's tags are of, if they are a Talos
// template's.
func talosTemplateRelease(tags []string) (string, bool) {
	if !slices.Contains(tags, string(templates.Talos)) {
		return "", false
	}
	if _, built := templates.BuildOf(tags); built {
		return "", false
	}
	for _, tag := range tags {
		if v, ok := strings.CutPrefix(tag, string(templates.Talos)+"-"); ok {
			return v, true
		}
	}
	return "", false
}

// unneededBuilds are the builds collect deletes. A build with an older kernel than the one before
// (older, by VM ID) takes none of a line's places for kept builds. It stays only while it is newer
// than every other build, which is when the interface shows it as older.
func unneededBuilds(builds map[string][]templates.Built, older map[uint64]bool, clusters []refreshedCluster, pending map[string]bool, keep int, soak time.Duration) []templates.Built {
	record := buildTimes(builds, older)
	u := usageOf(record, clusters, soak)
	var out []templates.Built
	for _, key := range record.Lines() {
		kept := keep
		if !u.run[key] && !reachable(key, u.kubeadm) {
			kept = 1
		}
		accepted := 0
		for _, b := range builds[key] {
			full := strings.Join(templates.ParseTags(b.Resource.Tags), ";")
			isOlder := older[b.Resource.VMID]
			if !isOlder {
				accepted++
			}
			stays := accepted <= kept && (!isOlder || accepted == 0)
			if stays || u.used[key+";"+templates.BuildTag(b.At)] || pending[full] {
				continue
			}
			out = append(out, b)
		}
	}
	return out
}

// buildTimes are the times of the pool's builds that clusters may move to, oldest first, as the
// ClusterClasses record them.
func buildTimes(builds map[string][]templates.Built, older map[uint64]bool) NodeImageBuilds {
	record := NodeImageBuilds{}
	for key, list := range builds {
		for _, b := range list {
			if !older[b.Resource.VMID] {
				record[key] = append(record[key], b.At)
			}
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
	list := proxmoxMachineList()
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
