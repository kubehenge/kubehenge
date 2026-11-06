package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// TemplatesDownAnnotation lists on each ClusterClass the Kubehenge VMs of the template pool whose
// Proxmox VE node is down, as JSON. Their configuration cannot be read, so the other annotations
// leave them out, apart from the builds, which stay as they were so that no cluster's pick moves.
const TemplatesDownAnnotation = "kubehenge.dev/templates-down"

// TemplateNodeDownCondition is true on a Cluster while the template its machines would clone from
// is on a Proxmox VE node that is down. Cluster API's Proxmox VE provider clones every VM through
// the template's node, so scaling, rolling updates and replacements stay at cloning until it is
// back.
const TemplateNodeDownCondition = "TemplateNodeDown"

// TemplateNodeDownReason is the reason of TemplateNodeDownCondition.
const TemplateNodeDownReason = "NodeDown"

// NodeImageHeldCondition is true on a Cluster while the node image refresh waits because the newest
// build on offer is on a node that is down: clones of it would fail, so the cluster stays on its
// build. The refresh goes on by itself once the node is back.
const NodeImageHeldCondition = "NodeImageHeld"

// NodeImageHeldTemplateNodeDown is the reason of NodeImageHeldCondition.
const NodeImageHeldTemplateNodeDown = "TemplateNodeDown"

// without is the builds minus those whose template is on a down node.
func (b NodeImageBuilds) without(down []DownTemplate) NodeImageBuilds {
	out := NodeImageBuilds{}
	for key, list := range b {
		out[key] = slices.DeleteFunc(slices.Clone(list), func(at time.Time) bool {
			_, isDown := downBuild(down, key, at)
			return isDown
		})
	}
	return out
}

// downBuild is the template of the line's build made at, when it is on a down node.
func downBuild(down []DownTemplate, lineKey string, at time.Time) (DownTemplate, bool) {
	for _, d := range down {
		if built, has := templates.BuildOf(d.Tags); has && built.Equal(at) && templates.LineKey(d.Tags) == lineKey {
			return d, true
		}
	}
	return DownTemplate{}, false
}

// nextBuild is the build a refresh at now moves a cluster's machines of the line from (from) to
// (to). A newest build on a down node is passed over for the newest on a node that is up. When none
// is newer than the build they run, blocked is the down template that would have been the move. A
// cluster whose own build is on a down node is moved as before, since it is stuck either way.
func nextBuild(builds NodeImageBuilds, down []DownTemplate, line []string, t, now time.Time, soak time.Duration) (waiting, bool) {
	from, ok := builds.Pick(line, t, soak)
	next, nextOK := builds.Pick(line, now, soak)
	if !ok || !nextOK || next.Equal(from) {
		return waiting{}, false
	}
	key := templates.LineKey(line)
	if _, runsDown := downBuild(down, key, from); runsDown {
		return waiting{from: from, to: next}, true
	}
	if up, upOK := builds.without(down).Pick(line, now, soak); upOK && !up.Equal(from) {
		return waiting{from: from, to: up}, true
	}
	blocked, _ := downBuild(down, key, next)
	return waiting{from: from, to: next, blocked: blocked}, false
}

// refreshTime is the node image time that has the line's machines clone from the build to: now,
// unless a newer build is on offer, which only a build on a down node can be. Then it is the
// moment to was first offered.
func refreshTime(builds NodeImageBuilds, line []string, to, now time.Time, soak time.Duration) time.Time {
	if newest, ok := builds.Pick(line, now, soak); ok && newest.Equal(to) {
		return now
	}
	return to.Add(soak)
}

// markNodeImageHeld sets NodeImageHeldCondition for a blocked refresh, or takes it away. It writes
// only a change, since the upgrader looks again and again.
func (u *Upgrader) markNodeImageHeld(ctx context.Context, c *clusterv1.Cluster, blocked DownTemplate) error {
	found := meta.FindStatusCondition(c.Status.Conditions, NodeImageHeldCondition)
	var cond *metav1.Condition
	if blocked.Node != "" {
		cond = &metav1.Condition{Type: NodeImageHeldCondition, Status: metav1.ConditionTrue, Reason: NodeImageHeldTemplateNodeDown, Message: blocked.BuildMessage()}
	}
	switch {
	case cond == nil && found == nil:
		return nil
	case cond != nil && found != nil && found.Status == cond.Status && found.Reason == cond.Reason && found.Message == cond.Message:
		return nil
	}
	key := client.ObjectKeyFromObject(c)
	if _, err := markCluster(ctx, u.Client, nil, key, NodeImageHeldCondition, cond); err != nil {
		return err
	}
	// The write moved the resource version that the refresh's own patch is made against.
	if err := u.Client.Get(ctx, key, c); err != nil {
		return fmt.Errorf("reading the cluster again: %w", err)
	}
	return nil
}

// DownTemplate is a VM of the template pool on a node that is down.
type DownTemplate struct {
	// Name is empty where Proxmox VE's resource view gives none for a VM on a down node.
	Name string   `json:"name,omitempty"`
	VMID int      `json:"vmid"`
	Node string   `json:"node"`
	Tags []string `json:"tags"`
}

// BuildMessage says which build is on a down node.
func (d DownTemplate) BuildMessage() string {
	at, _ := templates.BuildOf(d.Tags)
	return fmt.Sprintf("Build %s is on node %s, which is down", templates.BuildTag(at), d.Node)
}

// What names the template: its name, or its VM ID where the name is unknown.
func (d DownTemplate) What() string {
	if d.Name != "" {
		return d.Name
	}
	return fmt.Sprintf("VM %d", d.VMID)
}

// Message says what is wrong and what to do about it.
func (d DownTemplate) Message() string {
	return fmt.Sprintf("Template %s is on node %s, which is down. Start the node, or move the template to a node that is up", d.What(), d.Node)
}

// encodeTemplatesDown writes TemplatesDownAnnotation, empty for none and the same for the same VMs.
func encodeTemplatesDown(down []DownTemplate) (string, error) {
	if len(down) == 0 {
		return "", nil
	}
	sorted := slices.SortedFunc(slices.Values(down), func(a, b DownTemplate) int { return a.VMID - b.VMID })
	out, err := json.Marshal(sorted)
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", TemplatesDownAnnotation, err)
	}
	return string(out), nil
}

// ClassTemplatesDown reads TemplatesDownAnnotation.
func ClassTemplatesDown(class *clusterv1.ClusterClass) ([]DownTemplate, error) {
	recorded := class.Annotations[TemplatesDownAnnotation]
	if recorded == "" {
		return nil, nil
	}
	var down []DownTemplate
	if err := json.Unmarshal([]byte(recorded), &down); err != nil {
		return nil, fmt.Errorf("reading %s: %w", TemplatesDownAnnotation, err)
	}
	return down, nil
}

// clusterTemplateDown is the down template a cluster's machines would be cloned from, or nil: for
// Talos the template of its release, for the others the build of each line its machines run that
// its node image time picks.
func clusterTemplateDown(c *clusterv1.Cluster, down []DownTemplate, builds NodeImageBuilds, soak time.Duration) (DownTemplate, bool, error) {
	if len(down) == 0 {
		return DownTemplate{}, false, nil
	}
	wanted, err := clonedFrom(c, builds, soak)
	if err != nil {
		return DownTemplate{}, false, err
	}
	for _, d := range down {
		tags := templates.NormaliseTags(d.Tags)
		if slices.ContainsFunc(wanted, func(w []string) bool { return slices.Equal(w, tags) }) {
			return d, true, nil
		}
	}
	return DownTemplate{}, false, nil
}

// clonedFrom are the tags, normalised, of the templates a cluster's machines would be cloned from.
func clonedFrom(c *clusterv1.Cluster, builds NodeImageBuilds, soak time.Duration) ([][]string, error) {
	if isTalos(c) {
		image, err := TalosImage(c)
		if err != nil {
			return nil, err
		}
		line, err := TalosLine(image)
		if err != nil {
			return nil, err
		}
		return [][]string{templates.NormaliseTags(line)}, nil
	}
	t, err := NodeImageTime(c)
	if err != nil {
		return nil, err
	}
	lines, err := ClusterLines(c)
	if err != nil {
		return nil, err
	}
	var wanted [][]string
	for _, line := range lines {
		if at, ok := builds.Pick(line, t, soak); ok {
			wanted = append(wanted, templates.NormaliseTags(append(slices.Clone(line), templates.BuildTag(at))))
		}
	}
	return wanted, nil
}

// checkTemplateNode refuses a new cluster whose machines would be cloned from a template on a node
// that is down: they would wait at cloning for ever.
func (h *Hooks) checkTemplateNode(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	down, err := ClassTemplatesDown(class)
	if err != nil || len(down) == 0 {
		return err
	}
	builds := NodeImageBuilds{}
	if !isTalos(cluster) {
		if builds, err = recordedBuilds(class); err != nil {
			// A class without builds recorded yet is judged by the machines' own selector later.
			return nil //nolint:nilerr // nothing to judge by yet
		}
	}
	d, found, err := clusterTemplateDown(cluster, down, builds, h.Soak)
	if err != nil || !found {
		return err
	}
	return fmt.Errorf("%s", d.Message()) //nolint:err113 // says all a person needs
}

// markTemplateNodes sets TemplateNodeDownCondition on every cluster whose template is on a down
// node, and takes it from the others, so it clears once the node is back.
func (p *TemplateVersions) markTemplateNodes(ctx context.Context, down []DownTemplate, builds NodeImageBuilds) {
	eachCluster(ctx, p.Client, p.ClassNamespace, "template-versions", func(ctx context.Context, cluster *clusterv1.Cluster) error {
		if !cluster.DeletionTimestamp.IsZero() {
			return nil
		}
		d, found, err := clusterTemplateDown(cluster, down, builds, p.Soak)
		if err != nil {
			return err
		}
		var cond *metav1.Condition
		if found {
			cond = &metav1.Condition{Type: TemplateNodeDownCondition, Status: metav1.ConditionTrue, Reason: TemplateNodeDownReason, Message: d.Message()}
		}
		_, err = markCluster(ctx, p.Client, nil, client.ObjectKeyFromObject(cluster), TemplateNodeDownCondition, cond)
		return err
	})
}

// downTemplates are the VMs of the pool on a node that is down, with the tags they carry.
func downTemplates(resources []DownTemplate) string {
	names := make([]string, len(resources))
	for i, d := range resources {
		names[i] = fmt.Sprintf("%s on %s", d.What(), d.Node)
	}
	return strings.Join(names, ", ")
}
