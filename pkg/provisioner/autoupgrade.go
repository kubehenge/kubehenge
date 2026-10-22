package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// UpgradePolicy is a cluster's upgrades variable.
type UpgradePolicy struct {
	// Mode is off, patch or minor.
	Mode   string `json:"mode"`
	Window Window `json:"window"`
	// NodeImages has the nodes move to new template builds inside the window, whatever Mode says;
	// unset means on.
	NodeImages *bool `json:"nodeImages,omitempty"`
}

// Window is when automatic upgrades may start.
type Window struct {
	Weekdays        []string `json:"weekdays,omitempty"`
	MonthDays       []int    `json:"monthDays,omitempty"`
	Start           string   `json:"start,omitempty"`
	DurationMinutes int      `json:"durationMinutes,omitempty"`
	TimeZone        string   `json:"timeZone,omitempty"`
}

// defaultWindowMinutes is how long a window without a duration stays open.
const defaultWindowMinutes = 240

// Open tells whether now lies in the window. A window may run past midnight, so the one that
// opened the day before counts too.
func (w *Window) Open(now time.Time) (bool, error) {
	loc, err := time.LoadLocation(cmpOr(w.TimeZone, "UTC"))
	if err != nil {
		return false, fmt.Errorf("window time zone: %w", err)
	}
	start := cmpOr(w.Start, "02:00")
	hm, err := time.Parse("15:04", start)
	if err != nil {
		return false, fmt.Errorf("window start %q: %w", start, err)
	}
	minutes := w.DurationMinutes
	if minutes == 0 {
		minutes = defaultWindowMinutes
	}
	t := now.In(loc)
	for _, back := range []int{0, 1} {
		day := t.AddDate(0, 0, -back)
		if !w.onDay(day) {
			continue
		}
		opens := time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
		if !t.Before(opens) && t.Before(opens.Add(time.Duration(minutes)*time.Minute)) {
			return true, nil
		}
	}
	return false, nil
}

func (w *Window) onDay(d time.Time) bool {
	if len(w.Weekdays) == 0 && len(w.MonthDays) == 0 {
		return true
	}
	return slices.Contains(w.Weekdays, d.Weekday().String()[:3]) || slices.Contains(w.MonthDays, d.Day())
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// kubeVersion is a distribution's Kubernetes version: v1.36.5, v1.36.5+rke2r1 or v1.36.5+k3s1.
type kubeVersion struct {
	Minor, Patch, Build int
	Raw                 string
}

var kubeVersionFormat = regexp.MustCompile(`^v1\.(\d+)\.(\d+)(?:\+(?:rke2r|k3s)(\d+))?$`)

func parseKubeVersion(s string) (kubeVersion, bool) {
	m := kubeVersionFormat.FindStringSubmatch(s)
	if m == nil {
		return kubeVersion{}, false
	}
	minor, _ := strconv.Atoi(m[1])
	patch, _ := strconv.Atoi(m[2])
	build, _ := strconv.Atoi(m[3])
	return kubeVersion{Minor: minor, Patch: patch, Build: build, Raw: s}, true
}

func (a kubeVersion) less(b kubeVersion) bool {
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	if a.Patch != b.Patch {
		return a.Patch < b.Patch
	}
	return a.Build < b.Build
}

// Channels are a distribution's releases: the newest of each minor version, and the minor its
// publisher calls stable.
type Channels struct {
	Latest      map[int]kubeVersion
	StableMinor int
}

// supportedMinors is how many minor versions, counted down from the newest, a new cluster may
// start on. Kubernetes patches its three newest and keeps the one before for a few weeks after a
// release; the channels list every minor ever released, years past their last fix.
const supportedMinors = 4

// Offer is the newest release of each minor version, newest first.
type Offer struct {
	// Current are those a new cluster may start on.
	Current []string
	// Older are those of the minors before, still the next step of a cluster already on one.
	Older []string
	// Stable is the release of the minor the publisher calls stable, always among Current; ""
	// when it lists none.
	Stable string
}

// Versions are what the channels offer.
func (ch Channels) Versions() Offer {
	minors := slices.Sorted(maps.Keys(ch.Latest))
	slices.Reverse(minors)
	o := Offer{Current: []string{}, Older: []string{}}
	if len(minors) == 0 {
		return o
	}
	for _, m := range minors {
		if m > minors[0]-supportedMinors || m >= ch.StableMinor {
			o.Current = append(o.Current, ch.Latest[m].Raw)
		} else {
			o.Older = append(o.Older, ch.Latest[m].Raw)
		}
	}
	o.Stable = ch.Latest[ch.StableMinor].Raw
	return o
}

// The modes of automatic upgrades, as a cluster's upgrades variable names them.
const (
	upgradePatch = "patch"
	upgradeMinor = "minor"
)

// Target is the version an automatic upgrade moves to from current, or "" for none: in patch
// mode the newest release of the same minor; in minor mode the newest of the next minor once that
// is stable, else as in patch mode.
func (ch Channels) Target(current, mode string) string {
	cur, ok := parseKubeVersion(current)
	if !ok || (mode != upgradePatch && mode != upgradeMinor) {
		return ""
	}
	if mode == upgradeMinor && cur.Minor+1 <= ch.StableMinor {
		if next, ok := ch.Latest[cur.Minor+1]; ok {
			return next.Raw
		}
	}
	if same, ok := ch.Latest[cur.Minor]; ok && cur.less(same) {
		return same.Raw
	}
	return ""
}

// Releases tells which Kubernetes versions a distribution offers.
type Releases interface {
	Channels(ctx context.Context, distribution string) (Channels, error)
}

// ChannelServer reads RKE2's and k3s's release channels (update.rke2.io, update.k3s.io), and
// kubeadm's from the templates Kubehenge has built: a kubeadm node needs one for its version.
type ChannelServer struct {
	HTTP    *http.Client
	URLs    map[string]string
	Proxmox *proxmox.Client
}

// DefaultChannelURLs are the publishers' own.
var DefaultChannelURLs = map[string]string{
	"rke2": "https://update.rke2.io/v1-release/channels",
	"k3s":  "https://update.k3s.io/v1-release/channels",
}

// channelList is a release channel server's answer.
type channelList struct {
	Data []channel `json:"data"`
}

type channel struct {
	ID     string `json:"id"`
	Latest string `json:"latest"`
}

// maxChannelList bounds what is read of a channel server's answer, a few KiB in practice.
const maxChannelList = 4 << 20

// Channels implements Releases.
func (s *ChannelServer) Channels(ctx context.Context, distribution string) (Channels, error) {
	if distribution == "kubeadm" {
		return s.templateChannels(ctx)
	}
	url, ok := s.URLs[distribution]
	if !ok {
		return Channels{}, fmt.Errorf("no release channels for %s", distribution)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return Channels{}, fmt.Errorf("reading %s: %w", url, err)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return Channels{}, fmt.Errorf("reading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Channels{}, fmt.Errorf("%s: %s", url, resp.Status)
	}
	var body channelList
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxChannelList)).Decode(&body); err != nil {
		return Channels{}, fmt.Errorf("%s: %w", url, err)
	}
	ch := Channels{Latest: map[int]kubeVersion{}}
	for _, c := range body.Data {
		v, ok := parseKubeVersion(c.Latest)
		if !ok {
			continue
		}
		switch {
		case c.ID == "stable":
			ch.StableMinor = v.Minor
		case strings.HasPrefix(c.ID, "v1."):
			ch.Latest[v.Minor] = v
		}
	}
	if ch.StableMinor == 0 {
		return Channels{}, fmt.Errorf("%s names no stable channel", url)
	}
	return ch, nil
}

var kubeadmTemplateVersion = regexp.MustCompile(`^kubernetes-(v1\.\d+\.\d+)$`)

func (s *ChannelServer) templateChannels(ctx context.Context) (Channels, error) {
	seen, err := listPoolTemplates(ctx, s.Proxmox)
	if err != nil {
		return Channels{}, err
	}
	ch := Channels{Latest: map[int]kubeVersion{}}
	for _, v := range seen.kubeadm {
		if old, seen := ch.Latest[v.Minor]; !seen || old.less(v) {
			ch.Latest[v.Minor] = v
		}
		ch.StableMinor = max(ch.StableMinor, v.Minor)
	}
	return ch, nil
}

// poolTemplates is what the token sees of Kubehenge's templates, the controller's being those of
// the template pool. The ClusterClasses clone from builds of templates of the default OS only; one
// made from another cloud image, or without a build tag, is a source no machine of theirs could be
// built from.
type poolTemplates struct {
	// kubeadm are the Kubernetes versions of the kubeadm templates, newest first.
	kubeadm []kubeVersion
	// cloudImage tells whether the template RKE2's and k3s's machines are cloned from is there.
	cloudImage bool
	// storages are those any of Kubehenge's templates have disks on, whatever their OS.
	storages []string
	// builds are the builds of each line.
	builds NodeImageBuilds
}

// kubeadmVersionOf is the Kubernetes version a kubeadm template's tags name.
func kubeadmVersionOf(tags []string) (kubeVersion, bool) {
	if !slices.Contains(tags, "kubeadm") {
		return kubeVersion{}, false
	}
	for _, tag := range tags {
		if m := kubeadmTemplateVersion.FindStringSubmatch(tag); m != nil {
			return parseKubeVersion(m[1])
		}
	}
	return kubeVersion{}, false
}

// listPoolTemplates reads the templates through the builder, which confirms each from its VM's
// configuration: an upgrade started from a moment's stale listing would ask for a template that is
// gone.
func listPoolTemplates(ctx context.Context, c *proxmox.Client) (poolTemplates, error) {
	inv, err := (&templates.Builder{Client: c}).Inventory(ctx)
	if err != nil {
		return poolTemplates{}, fmt.Errorf("listing the Proxmox VE templates: %w", err)
	}
	seen := poolTemplates{storages: inv.Storages, builds: NodeImageBuilds{}}
	for _, r := range inv.Templates {
		tags := templates.ParseTags(r.Tags)
		at, built := templates.BuildOf(tags)
		if !slices.Contains(tags, templates.DefaultOS) || !built {
			continue
		}
		line := templates.LineKey(tags)
		seen.builds[line] = append(seen.builds[line], at)
		if slices.Contains(tags, "cloud-image") {
			seen.cloudImage = true
		}
		if v, ok := kubeadmVersionOf(tags); ok && !slices.Contains(seen.kubeadm, v) {
			seen.kubeadm = append(seen.kubeadm, v)
		}
	}
	slices.SortFunc(seen.kubeadm, func(a, b kubeVersion) int {
		switch {
		case b.less(a):
			return -1
		case a.less(b):
			return 1
		}
		return 0
	})
	for line := range seen.builds {
		slices.SortFunc(seen.builds[line], time.Time.Compare)
	}
	return seen, nil
}

// autoUpgraded records the last automatic upgrade on the Cluster.
const autoUpgraded = "kubehenge.dev/auto-upgraded"

// Upgrader starts the automatic upgrades clusters ask for, one step at a time, inside their
// windows. Cluster API carries each upgrade out; the BeforeClusterUpgrade hook still applies.
type Upgrader struct {
	Client         client.Client
	ClassNamespace string
	Releases       Releases
	// RancherKubernetes holds back the cluster that runs Rancher, as the hook would.
	RancherKubernetes KubernetesMinors
	// Soak is how long a template build waits before clusters are offered it.
	Soak     time.Duration
	Interval time.Duration
	Now      func() time.Time
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: one replica upgrades.
func (*Upgrader) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (u *Upgrader) Start(ctx context.Context) error {
	t := time.NewTicker(u.Interval)
	defer t.Stop()
	for {
		u.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// RunOnce looks at every cluster once.
func (u *Upgrader) RunOnce(ctx context.Context) {
	log := ctrl.LoggerFrom(ctx).WithName("upgrader")
	var list clusterv1.ClusterList
	if err := u.Client.List(ctx, &list); err != nil {
		log.Error(err, "listing clusters")
		return
	}
	channels := map[string]Channels{}
	for i := range list.Items {
		c := &list.Items[i]
		to, err := u.next(ctx, c, channels)
		if err != nil {
			log.Error(err, "automatic upgrade", "cluster", client.ObjectKeyFromObject(c))
			continue
		}
		if to == "" {
			// An upgrade takes the newest node images itself, so a refresh is only looked for
			// when none starts.
			if err := u.refresh(ctx, c); err != nil {
				log.Error(err, "node image refresh", "cluster", client.ObjectKeyFromObject(c))
			}
			continue
		}
		from := c.Spec.Topology.Version
		// The target was chosen from what was read; a version the user set since, or a cluster that
		// stopped being settled, makes the patch fail on the resource version and is looked at anew.
		base := client.MergeFromWithOptions(c.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations[autoUpgraded] = fmt.Sprintf("%s to %s at %s", from, to, u.Now().UTC().Format(time.RFC3339))
		c.Spec.Topology.Version = to
		if err := u.Client.Patch(ctx, c, base); err != nil {
			log.Error(err, "starting an automatic upgrade", "cluster", client.ObjectKeyFromObject(c))
			continue
		}
		log.Info("automatic upgrade started", "cluster", client.ObjectKeyFromObject(c), "from", from, "to", to)
	}
}

// next returns the version a cluster should move to now, or "".
func (u *Upgrader) next(ctx context.Context, c *clusterv1.Cluster, channels map[string]Channels) (string, error) {
	if !Owns(c, u.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
		return "", nil
	}
	policy, set, err := optionalVariable[UpgradePolicy](c, "upgrades")
	if err != nil || !set || (policy.Mode != upgradePatch && policy.Mode != upgradeMinor) {
		return "", err
	}
	if !settled(c) {
		return "", nil
	}
	open, err := policy.Window.Open(u.Now())
	if err != nil || !open {
		return "", err
	}
	dist := strings.TrimPrefix(c.Spec.Topology.ClassRef.Name, ClassPrefix)
	ch, ok := channels[dist]
	if !ok {
		if ch, err = u.Releases.Channels(ctx, dist); err != nil {
			return "", fmt.Errorf("reading the releases of %s: %w", dist, err)
		}
		channels[dist] = ch
	}
	to := ch.Target(c.Spec.Topology.Version, policy.Mode)
	if to == "" {
		return "", nil
	}
	if r, err := rancherOf(c); err == nil && r.Install && !u.RancherKubernetes.Allows(to) {
		return "", nil
	}
	return to, nil
}

// refresh moves a cluster's node image time to now, which has Cluster API replace its machines with
// clones of the newest builds of their template lines, when the cluster is settled inside its
// window and a build it does not run yet has been offered. Outside the window it records that it
// holds the cluster, and until when.
func (u *Upgrader) refresh(ctx context.Context, c *clusterv1.Cluster) error {
	if !Owns(c, u.ClassNamespace) || !c.DeletionTimestamp.IsZero() || !RefreshesNodes(c) {
		return nil
	}
	policy, _, err := optionalVariable[UpgradePolicy](c, "upgrades")
	if err != nil {
		return err
	}
	now := u.Now().UTC().Truncate(time.Second)
	var w waiting
	if policy.RefreshesNodeImages() {
		if w, err = u.waitingBuild(ctx, c, now); err != nil {
			return err
		}
	}
	from, to := w.from, w.to
	if to.IsZero() {
		return u.hold(ctx, c, "")
	}
	open, err := policy.Window.Open(now)
	if err != nil {
		return err
	}
	if !open {
		opens, err := policy.Window.Next(now)
		if err != nil {
			return err
		}
		return u.hold(ctx, c, fmt.Sprintf("%s at %s", templates.BuildTag(to), opens.UTC().Format(time.RFC3339)))
	}
	if !settled(c) {
		return nil
	}
	base := client.MergeFromWithOptions(c.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[NodeImageAnnotation] = now.Format(time.RFC3339)
	c.Annotations[NodeImageRefreshedAnnotation] = fmt.Sprintf("%s to %s at %s", templates.BuildTag(from), templates.BuildTag(to), now.Format(time.RFC3339))
	delete(c.Annotations, NodeImageHeldAnnotation)
	if err := u.Client.Patch(ctx, c, base); err != nil {
		return fmt.Errorf("moving the node image time: %w", err)
	}
	ctrl.LoggerFrom(ctx).WithName("upgrader").Info("node image refresh started", "cluster", client.ObjectKeyFromObject(c), "from", templates.BuildTag(from), "to", templates.BuildTag(to))
	return nil
}

// waiting is the build a cluster's nodes run of a line (from) and the newer one offered that they
// would move to (to); zero times when none waits.
type waiting struct {
	from, to time.Time
}

// waitingBuild is the build that waits for a cluster's nodes. A ClusterClass the controller has
// not recorded builds on yet offers none.
func (u *Upgrader) waitingBuild(ctx context.Context, c *clusterv1.Cluster, now time.Time) (waiting, error) {
	builds, err := ClassBuilds(ctx, u.Client, c)
	if errors.Is(err, errNoBuildsRecorded) {
		return waiting{}, nil
	}
	if err != nil {
		return waiting{}, err
	}
	t, err := NodeImageTime(c)
	if err != nil {
		return waiting{}, err
	}
	lines, err := ClusterLines(c)
	if err != nil {
		return waiting{}, err
	}
	for _, line := range lines {
		if next, ok := builds.Waiting(line, t, now, u.Soak); ok {
			from, _ := builds.Pick(line, t, u.Soak)
			return waiting{from: from, to: next}, nil
		}
	}
	return waiting{}, nil
}

// hold records on the cluster which build it is held from and until when, or with "" that it is
// held from none; it writes only a change.
func (u *Upgrader) hold(ctx context.Context, c *clusterv1.Cluster, held string) error {
	if c.Annotations[NodeImageHeldAnnotation] == held {
		return nil
	}
	value := any(nil)
	if held != "" {
		value = held
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{annotationsKey: map[string]any{NodeImageHeldAnnotation: value}}})
	if err != nil {
		return fmt.Errorf("recording the node image hold: %w", err)
	}
	target := &clusterv1.Cluster{}
	target.Namespace, target.Name = c.Namespace, c.Name
	if err := u.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("recording the node image hold: %w", err)
	}
	return nil
}

// errNoBuildsRecorded is a ClusterClass the controller has not recorded the template builds on yet.
var errNoBuildsRecorded = errors.New("no template builds recorded yet")

// ClassBuilds reads the builds of the template lines recorded on a cluster's ClusterClass.
func ClassBuilds(ctx context.Context, r client.Reader, c *clusterv1.Cluster) (NodeImageBuilds, error) {
	ref := c.Spec.Topology.ClassRef
	ns := cmpOr(ref.Namespace, c.Namespace)
	class := &clusterv1.ClusterClass{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, class); err != nil {
		return nil, fmt.Errorf("reading ClusterClass %s/%s: %w", ns, ref.Name, err)
	}
	recorded, ok := class.Annotations[NodeImageBuildsAnnotation]
	if !ok {
		return nil, fmt.Errorf("ClusterClass %s/%s: %w", ns, ref.Name, errNoBuildsRecorded)
	}
	return ParseNodeImageBuilds(recorded)
}

// settled tells whether a cluster is available with nothing rolling out, so an upgrade starts
// from a known good state and never stacks on another.
func settled(c *clusterv1.Cluster) bool {
	if !meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.AvailableCondition) ||
		!meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.ClusterTopologyReconciledCondition) ||
		c.Status.ObservedGeneration != c.Generation {
		return false
	}
	cp, w := c.Status.ControlPlane, c.Status.Workers
	if cp == nil || cp.DesiredReplicas == nil || cp.UpToDateReplicas == nil || *cp.UpToDateReplicas != *cp.DesiredReplicas {
		return false
	}
	if w != nil && w.DesiredReplicas != nil && (w.UpToDateReplicas == nil || *w.UpToDateReplicas != *w.DesiredReplicas) {
		return false
	}
	return len(cp.Versions) <= 1 && (w == nil || len(w.Versions) <= 1)
}
