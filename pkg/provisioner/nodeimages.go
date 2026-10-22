package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// NodeImageAnnotation is a cluster's node image time: its machines are cloned from the newest
	// build of their template line offered at or before it. Only the upgrader, inside the
	// cluster's window, and an upgrade move it, so Cluster API never rolls the nodes on its own
	// when a template is built. A cluster without it goes by its creation.
	NodeImageAnnotation = "kubehenge.dev/node-image"
	// NodeImageRefreshedAnnotation records the last refresh: "<build> to <build> at <time>".
	NodeImageRefreshedAnnotation = "kubehenge.dev/node-image-refreshed"
	// NodeImageHeldAnnotation is the upgrader's decision to hold a cluster whose nodes have a newer
	// build to move to until its window opens: "<build> at <time the window opens>". It is gone
	// once the move starts, or when no build waits.
	NodeImageHeldAnnotation = "kubehenge.dev/node-image-held"
	// NodeImageBuildsAnnotation lists on each ClusterClass the builds of every template line in the
	// template pool, as JSON: line key to build times, oldest first. The patch that picks a
	// machine's template reads it, rather than Proxmox VE on every call.
	NodeImageBuildsAnnotation = "kubehenge.dev/node-image-builds"
	// DefaultNodeImageSoak is how long a build waits before clusters are offered it, giving a
	// build that turns out broken time to be caught before every cluster's window takes it.
	DefaultNodeImageSoak = 24 * time.Hour
)

// NodeImageBuilds are the builds of each template line, by line key, oldest first.
type NodeImageBuilds map[string][]time.Time

// ParseNodeImageBuilds reads NodeImageBuildsAnnotation.
func ParseNodeImageBuilds(s string) (NodeImageBuilds, error) {
	raw := map[string][]string{}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("reading %s: %w", NodeImageBuildsAnnotation, err)
	}
	out := NodeImageBuilds{}
	for line, stamps := range raw {
		for _, stamp := range stamps {
			at, err := time.Parse(time.RFC3339, stamp)
			if err != nil {
				return nil, fmt.Errorf("reading %s: line %s: %w", NodeImageBuildsAnnotation, line, err)
			}
			out[line] = append(out[line], at.UTC())
		}
		slices.SortFunc(out[line], time.Time.Compare)
	}
	return out, nil
}

// Encode writes the builds as NodeImageBuildsAnnotation holds them, the same for the same builds.
func (b NodeImageBuilds) Encode() (string, error) {
	raw := map[string][]string{}
	for line, builds := range b {
		sorted := slices.SortedFunc(slices.Values(builds), time.Time.Compare)
		stamps := make([]string, len(sorted))
		for i, at := range sorted {
			stamps[i] = at.UTC().Format(time.RFC3339)
		}
		raw[line] = stamps
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", NodeImageBuildsAnnotation, err)
	}
	return string(out), nil
}

// Lines are the keys of the lines that have builds, sorted.
func (b NodeImageBuilds) Lines() []string {
	return slices.Sorted(maps.Keys(b))
}

// Pick is the build a machine of the line is cloned from at node image time t: the newest build
// offered by then, a build being offered soak after it was made. A line with none offered that
// early, such as a Kubernetes version built after t, takes its oldest build. It is false when the
// line has no build at all.
func (b NodeImageBuilds) Pick(line []string, t time.Time, soak time.Duration) (time.Time, bool) {
	builds := b[templates.LineKey(line)]
	if len(builds) == 0 {
		return time.Time{}, false
	}
	picked := builds[0]
	for _, at := range builds {
		if !at.Add(soak).After(t) {
			picked = at
		}
	}
	return picked, true
}

// Waiting is the build of the line that a refresh at now would move the cluster to, when it is
// another than the one node image time t picks.
func (b NodeImageBuilds) Waiting(line []string, t, now time.Time, soak time.Duration) (time.Time, bool) {
	at, ok := b.Pick(line, t, soak)
	next, nextOK := b.Pick(line, now, soak)
	if !ok || !nextOK || next.Equal(at) {
		return time.Time{}, false
	}
	return next, true
}

// Newer is the oldest build of the line made after the given one, whether offered yet or not.
func (b NodeImageBuilds) Newer(line []string, than time.Time) (time.Time, bool) {
	for _, at := range b[templates.LineKey(line)] {
		if at.After(than) {
			return at, true
		}
	}
	return time.Time{}, false
}

// NodeImageTime is a cluster's node image time: the annotation, or else when it was created.
func NodeImageTime(c *clusterv1.Cluster) (time.Time, error) {
	v, ok := c.Annotations[NodeImageAnnotation]
	if !ok {
		return c.CreationTimestamp.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("cluster %s/%s: %s: %w", c.Namespace, c.Name, NodeImageAnnotation, err)
	}
	return t.UTC(), nil
}

// RefreshesNodes tells whether a cluster's nodes are cloned from template builds: every
// distribution's but Talos's, whose single template takes its updates through Talos upgrades.
func RefreshesNodes(c *clusterv1.Cluster) bool {
	return strings.TrimPrefix(c.Spec.Topology.ClassRef.Name, ClassPrefix) != string(templates.Talos)
}

// MachineLine is the template line a machine of the distribution at a Kubernetes version is
// cloned from: RKE2 and k3s share the cloud image's, kubeadm has one per version.
func MachineLine(distribution, version string) ([]string, error) {
	t, err := MachineTemplate(distribution, version)
	if err != nil {
		return nil, err
	}
	return t.Tags, nil
}

// MachineTemplate is the line MachineLine names, as planned.
func MachineTemplate(distribution, version string) (templates.Template, error) {
	req := templates.Request{Distribution: templates.Distribution(distribution)}
	switch req.Distribution {
	case templates.RKE2, templates.K3s:
		req.Distribution = templates.RKE2
	case templates.Kubeadm:
		req.KubernetesVersion = version
	case templates.Talos:
		return templates.Template{}, fmt.Errorf("%s machines boot Talos images, which no template line refreshes", distribution)
	default:
		return templates.Template{}, fmt.Errorf("no template line for %s machines", distribution)
	}
	t, err := templates.Plan(req)
	if err != nil {
		return templates.Template{}, fmt.Errorf("planning the template of %s %s machines: %w", distribution, version, err)
	}
	return t, nil
}

// ClusterLines are the template lines a cluster's machines run: its version's, and during an
// upgrade the version each part still runs.
func ClusterLines(c *clusterv1.Cluster) ([][]string, error) {
	dist := strings.TrimPrefix(c.Spec.Topology.ClassRef.Name, ClassPrefix)
	versions := []string{c.Spec.Topology.Version}
	if cp := c.Status.ControlPlane; cp != nil {
		for _, v := range cp.Versions {
			versions = append(versions, v.Version)
		}
	}
	if w := c.Status.Workers; w != nil {
		for _, v := range w.Versions {
			versions = append(versions, v.Version)
		}
	}
	var lines [][]string
	seen := map[string]bool{}
	for _, v := range versions {
		line, err := MachineLine(dist, v)
		if err != nil {
			return nil, err
		}
		if key := templates.LineKey(line); !seen[key] {
			seen[key] = true
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// proxmoxMachineTemplate is the kind of capmox's machine templates, which select what to clone.
const proxmoxMachineTemplate = "ProxmoxMachineTemplate"

// kindOf is the kind of a template a patch request carries, or "".
func kindOf(item *runtimehooksv1.GeneratePatchesRequestItem) string {
	var object metav1.TypeMeta
	if len(item.Object.Raw) == 0 || json.Unmarshal(item.Object.Raw, &object) != nil {
		return ""
	}
	return object.Kind
}

// nodeImages picks, for one patch request, the template build each of a cluster's machine
// templates selects. It reads the cluster and its class once for all the request's templates.
type nodeImages struct {
	hooks     *Hooks
	variables []runtimehooksv1.Variable

	loaded       bool
	refreshed    bool
	distribution string
	t            time.Time
	builds       NodeImageBuilds
}

func (n *nodeImages) load(ctx context.Context) error {
	if n.loaded {
		return nil
	}
	var key client.ObjectKey
	for _, v := range n.variables {
		if v.Name != runtimehooksv1.BuiltinsName {
			continue
		}
		var b runtimehooksv1.Builtins
		if err := json.Unmarshal(v.Value.Raw, &b); err != nil {
			return fmt.Errorf("variable %s: %w", runtimehooksv1.BuiltinsName, err)
		}
		if b.Cluster != nil {
			key = client.ObjectKey{Namespace: b.Cluster.Namespace, Name: b.Cluster.Name}
		}
	}
	if key.Name == "" {
		return errors.New("the patch request names no cluster")
	}
	c := &clusterv1.Cluster{}
	if err := n.hooks.Reader.Get(ctx, key, c); err != nil {
		return fmt.Errorf("reading cluster %s: %w", key, err)
	}
	var err error
	n.distribution = strings.TrimPrefix(c.Spec.Topology.ClassRef.Name, ClassPrefix)
	n.refreshed = RefreshesNodes(c)
	n.loaded = true
	if !n.refreshed {
		return nil
	}
	if n.t, err = NodeImageTime(c); err != nil {
		return err
	}
	n.builds, err = ClassBuilds(ctx, n.hooks.Reader, c)
	return err
}

// patch has a machine template select the build of its line that the cluster's node image time
// picks, which stays the same until that time moves.
func (n *nodeImages) patch(ctx context.Context, item *runtimehooksv1.GeneratePatchesRequestItem) ([]byte, error) {
	if err := n.load(ctx); err != nil || !n.refreshed {
		return nil, err
	}
	version, err := builtinVersion(item.Variables, item.HolderReference.Kind != "MachineDeployment")
	if err != nil {
		return nil, err
	}
	line, err := MachineLine(n.distribution, version)
	if err != nil {
		return nil, err
	}
	at, ok := n.builds.Pick(line, n.t, n.hooks.Soak)
	if !ok {
		return nil, fmt.Errorf("the template pool holds no template %s; build it on the Administration page or with kubehenge template create", strings.Join(line, ";"))
	}
	tags := templates.NormaliseTags(append(slices.Clone(line), templates.BuildTag(at)))
	patch, err := json.Marshal([]map[string]any{
		{"op": "replace", "path": templateSpec + "/templateSelector/matchTags", "value": tags},
	})
	if err != nil {
		return nil, fmt.Errorf("patching the template selector: %w", err)
	}
	return patch, nil
}

// RefreshesNodeImages tells whether the policy has the cluster's nodes move to new template builds
// in its window, which it does unless switched off.
func (p *UpgradePolicy) RefreshesNodeImages() bool {
	return p.NodeImages == nil || *p.NodeImages
}

// Next is when the window next lets an upgrade or refresh start, from on: from itself while open.
func (w *Window) Next(from time.Time) (time.Time, error) {
	open, err := w.Open(from)
	if err != nil || open {
		return from, err
	}
	loc, err := time.LoadLocation(cmpOr(w.TimeZone, "UTC"))
	if err != nil {
		return time.Time{}, fmt.Errorf("window time zone: %w", err)
	}
	hm, err := time.Parse("15:04", cmpOr(w.Start, "02:00"))
	if err != nil {
		return time.Time{}, fmt.Errorf("window start: %w", err)
	}
	t := from.In(loc)
	// Every month has a 31st within two of them, and every weekday comes within one.
	const horizon = 62
	for ahead := range horizon {
		day := t.AddDate(0, 0, ahead)
		opens := time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
		if w.onDay(day) && opens.After(from) {
			return opens, nil
		}
	}
	return time.Time{}, fmt.Errorf("the window does not open within %d days", horizon)
}
