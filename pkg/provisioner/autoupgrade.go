package provisioner

import (
	"bytes"
	"cmp"
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
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// UpgradePolicy is a cluster's upgrades variable.
type UpgradePolicy struct {
	// Mode is off, patch or minor.
	Mode   string `json:"mode"`
	Window Window `json:"window"`
	// NodeImages has the nodes move to new template builds inside the window, whatever Mode says.
	// Unset means on.
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
	loc, err := time.LoadLocation(cmp.Or(w.TimeZone, "UTC"))
	if err != nil {
		return false, fmt.Errorf("window time zone: %w", err)
	}
	start := cmp.Or(w.Start, "02:00")
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
// release. The channels list every minor ever released, years past their last fix.
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
		if ch.offeredNew(m, minors[0]) {
			o.Current = append(o.Current, ch.Latest[m].Raw)
		} else {
			o.Older = append(o.Older, ch.Latest[m].Raw)
		}
	}
	o.Stable = ch.Latest[ch.StableMinor].Raw
	return o
}

// OldestOffered is the oldest minor version a new cluster may start on, false when the channels
// list none.
func (ch Channels) OldestOffered() (int, bool) {
	if len(ch.Latest) == 0 {
		return 0, false
	}
	minors := slices.Sorted(maps.Keys(ch.Latest))
	newest := minors[len(minors)-1]
	i := slices.IndexFunc(minors, func(m int) bool { return ch.offeredNew(m, newest) })
	return minors[i], true
}

// The modes of automatic upgrades, as a cluster's upgrades variable names them.
const (
	upgradePatch = "patch"
	upgradeMinor = "minor"
)

// Target is the version an automatic upgrade moves to from current, or "" for none: in patch
// mode the newest release of the same minor. In minor mode the newest of the next minor once that
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

// offeredNew tells whether a new cluster may start on minor, given the newest minor there is.
func (ch Channels) offeredNew(minor, newest int) bool {
	return minor > newest-supportedMinors || minor >= ch.StableMinor
}

// Releases tells which Kubernetes versions a distribution offers on a node OS.
type Releases interface {
	Channels(ctx context.Context, distribution string, os templates.NodeOS) (Channels, error)
}

// ChannelServer reads RKE2's and k3s's release channels (update.rke2.io, update.k3s.io), whatever
// the node OS. It reads kubeadm's from the templates Kubehenge has built for the node OS, as a
// kubeadm node needs one for its version. It reads Talos's from Kubernetes's own, for the minor versions the Talos
// release Kubehenge makes templates of runs. Talos is its own OS, so the node OS plays no part there.
type ChannelServer struct {
	HTTP    *http.Client
	URLs    map[string]string
	Proxmox *proxmox.Client
	// Pins, when set, have RKE2's and k3s's channels name the newest release of each minor version
	// that they pin, and leave out a minor version of which they pin none: a node installs no other.
	Pins ReleasePins
	// Store, when set, adds the pins administrators added to Pins.
	Store *PinStore
}

// DefaultChannelURLs are the publishers' own. Talos's is where Kubernetes names the newest release
// of each minor version, in stable-1.<minor>.txt.
var DefaultChannelURLs = map[string]string{
	"rke2":  "https://update.rke2.io/v1-release/channels",
	"k3s":   "https://update.k3s.io/v1-release/channels",
	"talos": KubernetesReleaseMarkers,
}

// KubernetesReleaseMarkers is where Kubernetes names the newest release of each minor version, in
// stable-1.<minor>.txt.
const KubernetesReleaseMarkers = "https://dl.k8s.io/release"

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
func (s *ChannelServer) Channels(ctx context.Context, distribution string, os templates.NodeOS) (Channels, error) {
	if distribution == "kubeadm" {
		return s.templateChannels(ctx, os)
	}
	url, ok := s.URLs[distribution]
	if !ok {
		return Channels{}, fmt.Errorf("no release channels for %s", distribution)
	}
	if distribution == string(templates.Talos) {
		return s.kubernetesChannels(ctx, url)
	}
	raw, found, err := s.get(ctx, url)
	if err != nil {
		return Channels{}, err
	}
	if !found {
		return Channels{}, fmt.Errorf("%s: not found", url)
	}
	var body channelList
	if err := json.Unmarshal(raw, &body); err != nil {
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
	if err := s.keepPinned(ctx, distribution, &ch); err != nil {
		return Channels{}, err
	}
	return ch, nil
}

// NewestPatch is the newest release of Kubernetes minor version 1.<minor> that Kubernetes names in
// base/stable-1.<minor>.txt, or false for a minor it has not released yet.
func (s *ChannelServer) NewestPatch(ctx context.Context, base string, minor int) (string, bool, error) {
	v, found, err := s.markedRelease(ctx, base, minor)
	if err != nil {
		return "", false, fmt.Errorf("the newest Kubernetes 1.%d release: %w", minor, err)
	}
	return v.Raw, found, nil
}

// keepPinned leaves in ch the newest pinned release of each minor version, the built-in pins and
// those an administrator added, and drops a minor version with none.
func (s *ChannelServer) keepPinned(ctx context.Context, distribution string, ch *Channels) error {
	if s.Pins == nil {
		return nil
	}
	pins := s.Pins
	if s.Store != nil {
		added, err := s.Store.Added(ctx)
		if err != nil {
			return err
		}
		pins = maps.Clone(s.Pins)
		maps.Copy(pins, added)
	}
	release := map[string]*distributionRelease{string(templates.RKE2): rke2Release, string(templates.K3s): k3sRelease}[distribution]
	for m := range ch.Latest {
		if v, ok := pins.newest(release, m); ok {
			ch.Latest[m] = v
		} else {
			delete(ch.Latest, m)
		}
	}
	return nil
}

// get reads a channel server's answer, and whether there is one at url.
func (s *ChannelServer) get(ctx context.Context, url string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", url, err)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("%s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChannelList))
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", url, err)
	}
	return body, true, nil
}

// kubernetesChannels are the newest Kubernetes release of each minor version the Talos release
// Kubehenge makes templates of runs, as Kubernetes names them. Stable is the minor of the release
// Talos itself installs by default.
func (s *ChannelServer) kubernetesChannels(ctx context.Context, base string) (Channels, error) {
	minors, err := templates.TalosKubernetesMinors(templates.DefaultTalosVersion)
	if err != nil {
		return Channels{}, fmt.Errorf("the Kubernetes channels of Talos: %w", err)
	}
	stable, ok := parseKubeVersion(templates.TalosDefaultKubernetes)
	if !ok {
		return Channels{}, fmt.Errorf("talos's default kubernetes %q is no version", templates.TalosDefaultKubernetes)
	}
	ch := Channels{Latest: map[int]kubeVersion{}, StableMinor: stable.Minor}
	for _, m := range minors {
		v, found, err := s.markedRelease(ctx, base, m)
		if err != nil {
			return Channels{}, err
		}
		// A minor Talos runs that Kubernetes has not released yet is left out.
		if found {
			ch.Latest[m] = v
		}
	}
	if _, ok := ch.Latest[ch.StableMinor]; !ok {
		return Channels{}, fmt.Errorf("%s names no Kubernetes 1.%d release", base, ch.StableMinor)
	}
	return ch, nil
}

// markedRelease is the release base/stable-1.<minor>.txt names.
func (s *ChannelServer) markedRelease(ctx context.Context, base string, minor int) (kubeVersion, bool, error) {
	url := fmt.Sprintf("%s/stable-1.%d.txt", base, minor)
	raw, found, err := s.get(ctx, url)
	if err != nil || !found {
		return kubeVersion{}, false, err
	}
	v, ok := parseKubeVersion(strings.TrimSpace(string(raw)))
	if !ok || v.Minor != minor || v.Build != 0 {
		return kubeVersion{}, false, fmt.Errorf("%s names no Kubernetes 1.%d release: %q", url, minor, raw)
	}
	return v, true, nil
}

var kubeadmTemplateVersion = regexp.MustCompile(`^kubernetes-(v1\.\d+\.\d+)$`)

func (s *ChannelServer) templateChannels(ctx context.Context, os templates.NodeOS) (Channels, error) {
	seen, err := listPoolTemplates(ctx, s.Proxmox)
	if err != nil {
		return Channels{}, err
	}
	ch := Channels{Latest: map[int]kubeVersion{}}
	for _, v := range seen.kubeadm[os] {
		if old, seen := ch.Latest[v.Minor]; !seen || old.less(v) {
			ch.Latest[v.Minor] = v
		}
		ch.StableMinor = max(ch.StableMinor, v.Minor)
	}
	return ch, nil
}

// poolTemplates is what the token sees of Kubehenge's templates, the controller's being those of
// the template pool. The ClusterClasses clone from builds of templates of a node OS only. One
// without a build tag, or of an operating system no family names, is a source no machine of theirs
// could be built from.
type poolTemplates struct {
	// kubeadm are the Kubernetes versions of each node OS's kubeadm templates, newest first.
	kubeadm map[templates.NodeOS][]kubeVersion
	// nodeOSes are the node OSes RKE2's and k3s's templates are there for, by distribution.
	nodeOSes map[templates.Distribution][]templates.NodeOS
	// storages are those any of Kubehenge's templates have disks on, whatever their OS.
	storages []string
	// builds are the builds of each line that clusters may move to.
	builds NodeImageBuilds
	// older are the builds whose kernel is older than the previous build's, which clusters never
	// move to.
	older NodeImageBuilds
	// facts are what each build recorded, by line and build time, until sort sets the older ones
	// aside.
	facts map[string]map[time.Time]templates.BuildFacts
	// talos are the Talos releases of the Talos templates, newest first.
	talos []string
	// down are the VMs of the pool on a node that is down.
	down []DownTemplate
}

// talosVersionOf is the Talos release a Talos template's tags name: exactly a release's tags, as
// the Talos ClusterClass's machines select them.
func talosVersionOf(tags []string) (string, bool) {
	for _, tag := range tags {
		v, ok := strings.CutPrefix(tag, "talos-")
		if !ok {
			continue
		}
		if line, err := TalosLine(v); err == nil && slices.Equal(line, templates.NormaliseTags(tags)) {
			return v, true
		}
	}
	return "", false
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
	seen := poolTemplates{
		storages: inv.Storages, builds: NodeImageBuilds{}, older: NodeImageBuilds{},
		facts:   map[string]map[time.Time]templates.BuildFacts{},
		kubeadm: map[templates.NodeOS][]kubeVersion{}, nodeOSes: make(map[templates.Distribution][]templates.NodeOS),
	}
	for _, r := range inv.Templates {
		var facts *templates.BuildFacts
		if f, ok := inv.Facts[r.VMID]; ok {
			facts = &f
		}
		seen.add(templates.ParseTags(r.Tags), facts)
	}
	for _, r := range inv.Down {
		seen.addDown(r)
	}
	seen.sort()
	return seen, nil
}

// add records what the template of these tags offers: a Talos release, which has no builds, or a
// build of a node OS line, with what it recorded of itself, if anything.
func (p *poolTemplates) add(tags []string, facts *templates.BuildFacts) {
	if v, ok := talosVersionOf(tags); ok && !slices.Contains(p.talos, v) {
		p.talos = append(p.talos, v)
	}
	at, built := templates.BuildOf(tags)
	os, known := templates.NodeOSOf(tags)
	if _, _, err := templates.ReleaseOf(os); !built || !known || err != nil {
		return
	}
	line := templates.LineKey(tags)
	p.builds[line] = append(p.builds[line], at)
	if facts != nil {
		if p.facts[line] == nil {
			p.facts[line] = map[time.Time]templates.BuildFacts{}
		}
		p.facts[line][at.UTC()] = *facts
	}
	for _, d := range []templates.Distribution{templates.RKE2, templates.K3s} {
		if slices.Contains(tags, string(d)) && !slices.Contains(p.nodeOSes[d], os) {
			p.nodeOSes[d] = append(p.nodeOSes[d], os)
		}
	}
	if v, ok := kubeadmVersionOf(tags); ok && !slices.Contains(p.kubeadm[os], v) {
		p.kubeadm[os] = append(p.kubeadm[os], v)
	}
}

// addDown records a VM on a down node. Its build stays among the builds, so that the build a
// cluster's node image time picks does not move to another while the node is down. What the
// template offers a new cluster is left out, since it cannot be cloned from.
func (p *poolTemplates) addDown(r *proxmox.ClusterResource) {
	tags := templates.NormaliseTags(templates.ParseTags(r.Tags))
	vmid, _ := templateVMID(r)
	p.down = append(p.down, DownTemplate{Name: r.Name, VMID: vmid, Node: r.Node, Tags: tags})
	at, built := templates.BuildOf(tags)
	os, known := templates.NodeOSOf(tags)
	if _, _, err := templates.ReleaseOf(os); !built || !known || err != nil {
		return
	}
	line := templates.LineKey(tags)
	p.builds[line] = append(p.builds[line], at)
}

// templateVMID reads a VM ID from the cluster's resource list, where it is unsigned.
func templateVMID(r *proxmox.ClusterResource) (int, error) {
	const largest = 999999999 // Proxmox VE's own upper bound
	if r.VMID > largest {
		return 0, fmt.Errorf("VM ID %d is outside Proxmox VE's range", r.VMID)
	}
	return int(r.VMID), nil
}

// sort puts the versions and releases newest first, the node OSes by name and the builds oldest
// first.
func (p *poolTemplates) sort() {
	for os := range p.kubeadm {
		slices.SortFunc(p.kubeadm[os], func(a, b kubeVersion) int {
			switch {
			case b.less(a):
				return -1
			case a.less(b):
				return 1
			}
			return 0
		})
	}
	for d := range p.nodeOSes {
		slices.SortFunc(p.nodeOSes[d], func(a, b templates.NodeOS) int { return strings.Compare(a.String(), b.String()) })
	}
	slices.SortFunc(p.talos, newerFirst)
	for line, builds := range p.builds {
		slices.SortFunc(builds, time.Time.Compare)
		recorded := make([]*templates.BuildFacts, len(builds))
		for i, at := range builds {
			if f, ok := p.facts[line][at.UTC()]; ok {
				recorded[i] = &f
			}
		}
		older := templates.OlderBuilds(recorded)
		accepted := builds[:0:0]
		for i, at := range builds {
			if older[i] {
				p.older[line] = append(p.older[line], at)
			} else {
				accepted = append(accepted, at)
			}
		}
		p.builds[line] = accepted
	}
}

// newerFirst orders v1.x.y releases newest first.
func newerFirst(a, b string) int {
	va, _ := parseKubeVersion(a)
	vb, _ := parseKubeVersion(b)
	switch {
	case vb.less(va):
		return -1
	case va.less(vb):
		return 1
	}
	return 0
}

// autoUpgraded records the last automatic upgrade on the Cluster.
const autoUpgraded = "kubehenge.dev/auto-upgraded"

// Upgrader starts the automatic upgrades clusters ask for, one step at a time, inside their
// windows. Cluster API carries each upgrade out. The BeforeClusterUpgrade hook still applies.
type Upgrader struct {
	Client         client.Client
	ClassNamespace string
	Releases       Releases
	// RancherKubernetes holds back the cluster that runs Rancher, as the hook would.
	RancherKubernetes KubernetesMinors
	// Soak is how long a template build waits before clusters are offered it.
	Soak time.Duration
	// TalosImages lists the Talos releases Talos clusters move to the newest patch of.
	TalosImages TalosImages
	// Checker reads each cluster for removed APIs in use and fail-closed webhooks before an upgrade
	// starts. Unset, no check is made.
	Checker UpgradeChecker
	// Events records a skipped upgrade and a cluster that fell out of support. Unset, none are.
	Events   events.EventRecorder
	Interval time.Duration
	Now      func() time.Time
	// Informers, if set, start a round as soon as a cluster's upgrades change, a cluster settles or
	// a ClusterClass is offered other builds, rather than at the next Interval.
	Informers cache.Informers

	// supportAt is when the clusters' minors were last compared with what Kubehenge offers.
	supportAt time.Time
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: one replica upgrades.
func (*Upgrader) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (u *Upgrader) Start(ctx context.Context) error {
	wake := make(chan struct{}, 1)
	if u.Informers != nil {
		if err := u.watch(ctx, wake); err != nil {
			return err
		}
	}
	t := time.NewTicker(u.Interval)
	defer t.Stop()
	for {
		u.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-wake:
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
	compare := u.supportDue()
	for i := range list.Items {
		c := &list.Items[i]
		if compare {
			u.markSupport(ctx, c, channels)
		}
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
		// The target was chosen from what was read. A version the user set since, or a cluster that
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
		u.clearSkipped(ctx, c)
	}
	if compare {
		// After the loop, since a condition written first would make the upgrade's patch conflict.
		for i := range list.Items {
			u.recordChecks(ctx, &list.Items[i], channels)
		}
		u.supportAt = u.Now()
	}
}

// watch sends on wake when a round could find something to do. A full channel already holds a
// round to come, so changes in a burst start one.
func (u *Upgrader) watch(ctx context.Context, wake chan<- struct{}) error {
	nudge := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	for _, w := range []struct {
		obj     client.Object
		changed func(before, after client.Object) bool
	}{
		{&clusterv1.Cluster{}, func(before, after client.Object) bool {
			b, ok := before.(*clusterv1.Cluster)
			a, ok2 := after.(*clusterv1.Cluster)
			return ok && ok2 && upgradeInputChanged(b, a)
		}},
		{&clusterv1.ClusterClass{}, func(before, after client.Object) bool {
			return before.GetAnnotations()[NodeImageBuildsAnnotation] != after.GetAnnotations()[NodeImageBuildsAnnotation]
		}},
	} {
		informer, err := u.Informers.GetInformer(ctx, w.obj)
		if err != nil {
			return fmt.Errorf("watching %T: %w", w.obj, err)
		}
		changed := w.changed
		_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			UpdateFunc: func(oldObj, newObj any) {
				before, ok := oldObj.(client.Object)
				after, ok2 := newObj.(client.Object)
				if ok && ok2 && changed(before, after) {
					nudge()
				}
			},
		})
		if err != nil {
			return fmt.Errorf("watching %T: %w", w.obj, err)
		}
	}
	return nil
}

// upgradeInputChanged tells whether a round could decide otherwise for the cluster now: its
// upgrades variable changed, or it settled. The upgrader's own annotations change neither.
func upgradeInputChanged(before, after *clusterv1.Cluster) bool {
	return !settled(before) && settled(after) ||
		!bytes.Equal(variableValue(before, "upgrades"), variableValue(after, "upgrades"))
}

func variableValue(c *clusterv1.Cluster, name string) []byte {
	for _, v := range c.Spec.Topology.Variables {
		if v.Name == name {
			return v.Value.Raw
		}
	}
	return nil
}

// next returns the version a cluster should move to now, or "".
func (u *Upgrader) next(ctx context.Context, c *clusterv1.Cluster, channels map[string]Channels) (string, error) {
	if !Owns(c, u.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
		return "", nil
	}
	policy, set, err := optionalVariable[UpgradePolicy](c, "upgrades")
	if err != nil || !set || (policy.Mode != upgradePatch && policy.Mode != upgradeMinor) {
		u.clearSkipped(ctx, c)
		return "", err
	}
	if !settled(c) {
		return "", nil
	}
	open, err := policy.Window.Open(u.Now())
	if err != nil || !open {
		return "", err
	}
	ch, err := u.channelsOf(ctx, c, channels)
	if err != nil {
		return "", err
	}
	to := ch.Target(c.Spec.Topology.Version, policy.Mode)
	if to == "" {
		u.clearSkipped(ctx, c)
		return "", nil
	}
	if r, err := rancherOf(c); err == nil && r.Install && !u.RancherKubernetes.Allows(to) {
		return "", nil
	}
	if isTalos(c) {
		// The Talos release moves first, by hand: a minor version it does not run waits for it.
		talos, err := TalosVersion(c)
		if err != nil || templates.TalosRunsKubernetes(talos, to) != nil {
			return "", err
		}
		if _, upgrading := c.Annotations[TalosUpgradingAnnotation]; upgrading {
			return "", nil
		}
	}
	if !u.vetUpgrade(ctx, c, to) {
		return "", nil
	}
	return to, nil
}

// channelsOf reads the release channels of a cluster's distribution on its node OS, once a round
// for all the clusters that share them. Talos is its own OS, so its channels are read without one.
func (u *Upgrader) channelsOf(ctx context.Context, c *clusterv1.Cluster, channels map[string]Channels) (Channels, error) {
	dist := strings.TrimPrefix(c.Spec.Topology.ClassRef.Name, ClassPrefix)
	var os templates.NodeOS
	where := dist
	if !isTalos(c) {
		var err error
		if os, err = ClusterNodeOS(c); err != nil {
			return Channels{}, err
		}
		where = dist + " on " + os.String()
	}
	if ch, ok := channels[where]; ok {
		return ch, nil
	}
	ch, err := u.Releases.Channels(ctx, dist, os)
	if err != nil {
		return Channels{}, fmt.Errorf("reading the releases of %s: %w", where, err)
	}
	channels[where] = ch
	return ch, nil
}

// refresh moves a cluster's node image time to now, which has Cluster API replace its machines with
// clones of the newest builds of their template lines, when the cluster is settled inside its
// window and a build it does not run yet has been offered. Outside the window it records that it
// holds the cluster, and until when.
func (u *Upgrader) refresh(ctx context.Context, c *clusterv1.Cluster) error {
	if !Owns(c, u.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
		return nil
	}
	if isTalos(c) {
		return u.talosPatch(ctx, c)
	}
	if !RefreshesNodes(c) {
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
	if err = u.markNodeImageHeld(ctx, c, w.blocked); err != nil {
		return err
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
	c.Annotations[NodeImageAnnotation] = w.at.Format(time.RFC3339)
	c.Annotations[NodeImageRefreshedAnnotation] = fmt.Sprintf("%s to %s at %s", templates.BuildTag(from), templates.BuildTag(to), now.Format(time.RFC3339))
	delete(c.Annotations, NodeImageHeldAnnotation)
	if err := u.Client.Patch(ctx, c, base); err != nil {
		return fmt.Errorf("moving the node image time: %w", err)
	}
	ctrl.LoggerFrom(ctx).WithName("upgrader").Info("node image refresh started", "cluster", client.ObjectKeyFromObject(c), "from", templates.BuildTag(from), "to", templates.BuildTag(to))
	return nil
}

// talosPatch moves a Talos cluster's talosVersion to the newest patch release of its minor version,
// as node image refreshes bring the other distributions' nodes their updates: when the cluster is
// settled inside its window, nodeImages is on, and no node upgrade is under way or failed. The
// TalosUpgrader then upgrades the nodes in place.
func (u *Upgrader) talosPatch(ctx context.Context, c *clusterv1.Cluster) error {
	policy, _, err := optionalVariable[UpgradePolicy](c, "upgrades")
	if err != nil || !policy.RefreshesNodeImages() || u.TalosImages == nil {
		return err
	}
	_, upgrading := c.Annotations[TalosUpgradingAnnotation]
	_, failed := c.Annotations[TalosUpgradeFailedAnnotation]
	if upgrading || failed || !settled(c) {
		return nil
	}
	open, err := policy.Window.Open(u.Now())
	if err != nil || !open {
		return err
	}
	current, err := TalosVersion(c)
	if err != nil {
		return err
	}
	releases, err := u.TalosImages.TalosReleases(ctx)
	if err != nil {
		return fmt.Errorf("patching Talos of %s/%s: %w", c.Namespace, c.Name, err)
	}
	to := newestTalosPatch(current, releases)
	if to == current {
		return nil
	}
	if err = templates.TalosRunsKubernetes(to, c.Spec.Topology.Version); err != nil {
		// A patch release that drops the cluster's Kubernetes waits for the person to move both.
		return nil //nolint:nilerr // not a failure of the upgrader
	}
	base := client.MergeFromWithOptions(c.DeepCopy(), client.MergeFromWithOptimisticLock{})
	value, err := json.Marshal(to)
	if err != nil {
		return fmt.Errorf("the Talos release: %w", err)
	}
	for i := range c.Spec.Topology.Variables {
		if c.Spec.Topology.Variables[i].Name == TalosVersionVariable {
			c.Spec.Topology.Variables[i].Value.Raw = value
		}
	}
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[autoUpgraded] = fmt.Sprintf("Talos %s to %s at %s", current, to, u.Now().UTC().Format(time.RFC3339))
	if err := u.Client.Patch(ctx, c, base); err != nil {
		return fmt.Errorf("starting an automatic Talos upgrade: %w", err)
	}
	ctrl.LoggerFrom(ctx).WithName("upgrader").Info("automatic Talos upgrade started", "cluster", client.ObjectKeyFromObject(c), "from", current, "to", to)
	return nil
}

// newestTalosPatch is the newest of releases of current's minor version, or current.
func newestTalosPatch(current string, releases []string) string {
	minor, err := templates.TalosMinor(current)
	if err != nil {
		return current
	}
	newest := current
	for _, r := range releases {
		if m, err := templates.TalosMinor(r); err == nil && m == minor && newerFirst(r, newest) < 0 {
			newest = r
		}
	}
	return newest
}

// waiting is the build a cluster's nodes run of a line (from) and the newer one offered that they
// would move to (to); zero times when none waits.
type waiting struct {
	from, to time.Time
	// at is the node image time that has the machines clone from to.
	at time.Time
	// blocked is the template on a down node that the move would have gone to, when no move is
	// possible for that reason alone.
	blocked DownTemplate
}

// waitingBuild is the build that waits for a cluster's nodes. A ClusterClass the controller has
// not recorded builds on yet offers none.
func (u *Upgrader) waitingBuild(ctx context.Context, c *clusterv1.Cluster, now time.Time) (waiting, error) {
	builds, down, err := classBuildsAndDown(ctx, u.Client, c)
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
	var held waiting
	for _, line := range lines {
		move, moves := nextBuild(builds, down, line, t, now, u.Soak)
		switch {
		case moves:
			move.at = refreshTime(builds, line, move.to, now, u.Soak)
			return move, nil
		case move.blocked.Node != "" && held.blocked.Node == "":
			held.blocked = move.blocked
		}
	}
	return held, nil
}

// hold records on the cluster which build it is held from and until when, or with "" that it is
// held from none. It writes only a change.
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
	builds, _, err := classBuildsAndDown(ctx, r, c)
	return builds, err
}

// classBuildsAndDown reads the builds recorded on a cluster's ClusterClass and the templates it
// records as being on a down node.
func classBuildsAndDown(ctx context.Context, r client.Reader, c *clusterv1.Cluster) (NodeImageBuilds, []DownTemplate, error) {
	ref := c.Spec.Topology.ClassRef
	ns := cmp.Or(ref.Namespace, c.Namespace)
	class := &clusterv1.ClusterClass{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, class); err != nil {
		return nil, nil, fmt.Errorf("reading ClusterClass %s/%s: %w", ns, ref.Name, err)
	}
	builds, err := recordedBuilds(class)
	if err != nil {
		return nil, nil, err
	}
	down, err := ClassTemplatesDown(class)
	return builds, down, err
}

// recordedBuilds reads the builds recorded on a ClusterClass.
func recordedBuilds(class *clusterv1.ClusterClass) (NodeImageBuilds, error) {
	recorded, ok := class.Annotations[NodeImageBuildsAnnotation]
	if !ok {
		return nil, fmt.Errorf("ClusterClass %s/%s: %w", class.Namespace, class.Name, errNoBuildsRecorded)
	}
	return ParseNodeImageBuilds(recorded)
}

// OlderBuilds reads the builds with an older kernel than the previous build's, as the controller
// records them on the RKE2 ClusterClass in namespace. They are the same on every class that has any.
// None is recorded before the controller has looked at the template pool.
func OlderBuilds(ctx context.Context, r client.Reader, namespace string) (NodeImageBuilds, error) {
	class := &clusterv1.ClusterClass{}
	name := ClassPrefix + string(templates.RKE2)
	if err := client.IgnoreNotFound(r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, class)); err != nil {
		return nil, fmt.Errorf("reading ClusterClass %s/%s: %w", namespace, name, err)
	}
	recorded, ok := class.Annotations[NodeImageOlderAnnotation]
	if !ok {
		return NodeImageBuilds{}, nil
	}
	return ParseNodeImageBuilds(recorded)
}

// settled tells whether a cluster is available with nothing rolling out, so an upgrade starts
// from a known good state and never stacks on another, nor on a restore.
func settled(c *clusterv1.Cluster) bool {
	if RestoreInProgress(c) || ptr.Deref(c.Spec.Paused, false) ||
		!meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.AvailableCondition) ||
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
