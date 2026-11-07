package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ApplyAddonChangesAnnotation on a Cluster asks the controller to apply the add-on changes that
	// wait for the upgrade window, now. The controller takes it off again.
	ApplyAddonChangesAnnotation = "kubehenge.dev/apply-addon-changes"
	// AddonChangesAnnotation on a Cluster holds, as JSON, the AddonChangesStatus of the changes that
	// wait for the window.
	AddonChangesAnnotation = "kubehenge.dev/addon-changes"
	// AddonChangesPendingCondition on a Cluster says that add-on changes wait for the window.
	AddonChangesPendingCondition = "AddonChangesPending"
	// AddonChangesWaiting is the reason of AddonChangesPendingCondition.
	AddonChangesWaiting = "WaitingForWindow"

	// releaseStampAnnotation and settingsStampAnnotation on a HelmChartProxy record which release of
	// Kubehenge's controller and which settings of the cluster its spec was made for.
	releaseStampAnnotation  = "kubehenge.dev/release-stamp"
	settingsStampAnnotation = "kubehenge.dev/settings-stamp"

	// upgradesVariable is the cluster's upgrade policy, whose window the changes wait for.
	upgradesVariable = "upgrades"

	// stampLength is how many hex digits of a SHA-256 a stamp keeps.
	stampLength = 16
)

// AddonChange is a change to an add-on's HelmChartProxy that waits for the window: its chart and the
// version it runs and will run. From equals To where only the values change.
type AddonChange struct {
	Addon string `json:"addon"`
	Chart string `json:"chart"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// AddonChangesStatus is what AddonChangesAnnotation holds.
type AddonChangesStatus struct {
	// Opens is when the cluster's window next opens.
	Opens   time.Time     `json:"opens"`
	Changes []AddonChange `json:"changes"`
}

// rolloutGate decides which changes of an add-on's proxy wait. A new release of the controller
// changes the charts it pins, and the new pins reach a cluster whose upgrade window is closed
// only once it opens. A change the cluster's owner makes in its settings, a new cluster and an
// add-on just switched on apply at once.
type rolloutGate struct {
	// hold is set while the cluster's window is closed and nobody asked to apply the changes now.
	hold  bool
	opens time.Time
	// release stamps the controller's build and pins, settings the cluster's variables but its
	// upgrade policy.
	release, settings string
}

// rolloutGate reads how the cluster's changes are to be applied. A cluster has a window when its
// upgrades variable has the controller do something inside one.
func (r *Reconciler) rolloutGate(cluster *clusterv1.Cluster) (rolloutGate, error) {
	g := rolloutGate{release: r.releaseStamp(), settings: settingsStamp(cluster)}
	policy, set, err := optionalVariable[UpgradePolicy](cluster, upgradesVariable)
	if err != nil {
		return g, err
	}
	_, asked := cluster.Annotations[ApplyAddonChangesAnnotation]
	windowed := policy.Mode == upgradePatch || policy.Mode == upgradeMinor || policy.RefreshesNodeImages()
	if !set || !windowed || asked {
		return g, nil
	}
	now := r.now()
	open, err := policy.Window.Open(now)
	if err != nil || open {
		return g, err
	}
	g.hold = true
	g.opens, err = policy.Window.Next(now)
	return g, err
}

// releaseStamp tells releases of the controller apart: its build, and the charts and namespaces it
// pins.
func (r *Reconciler) releaseStamp() string {
	parts := []string{fmt.Sprintf("%#v", &r.Charts)}
	if info, ok := debug.ReadBuildInfo(); ok {
		parts = append(parts, info.Main.Version)
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				parts = append(parts, s.Value)
			}
		}
	}
	return stamp(parts...)
}

// settingsStamp changes with any variable of the cluster but its upgrade policy.
func settingsStamp(cluster *clusterv1.Cluster) string {
	parts := make([]string, 0, len(cluster.Spec.Topology.Variables))
	for _, v := range cluster.Spec.Topology.Variables {
		if v.Name != upgradesVariable {
			parts = append(parts, v.Name+"="+string(v.Value.Raw))
		}
	}
	slices.Sort(parts)
	return stamp(parts...)
}

func stamp(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:stampLength]
}

// applyProxy applies one HelmChartProxy and reports the change it holds back, if any. A proxy is
// held back only when the new spec comes from a new release and none of the cluster's settings
// moved with it.
func (r *Reconciler) applyProxy(ctx context.Context, cluster *clusterv1.Cluster, p *unstructured.Unstructured, g *rolloutGate) (*AddonChange, error) {
	desired := p.Object["spec"]
	var held *AddonChange
	mutate := func(got *unstructured.Unstructured) {
		if got.GetResourceVersion() != "" {
			if change, wait := g.holds(got, desired, cluster.Name); wait {
				held = &change
				return
			}
		}
		got.Object["spec"] = desired
		annotations := got.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[releaseStampAnnotation], annotations[settingsStampAnnotation] = g.release, g.settings
		got.SetAnnotations(annotations)
	}
	err := applyOwned(ctx, r.Client, cluster, p, mutate)
	return held, err
}

// holds reports whether the stored proxy keeps its spec for now, and the change it holds back.
func (g *rolloutGate) holds(stored *unstructured.Unstructured, desired any, cluster string) (AddonChange, bool) {
	if !g.hold || covers(stored.Object["spec"], desired) {
		return AddonChange{}, false
	}
	a := stored.GetAnnotations()
	if a[releaseStampAnnotation] == "" || a[releaseStampAnnotation] == g.release || a[settingsStampAnnotation] != g.settings {
		return AddonChange{}, false
	}
	return AddonChange{
		Addon: strings.TrimPrefix(stored.GetName(), cluster+"-"),
		Chart: specText(desired, "chartName"),
		From:  chartVersion(specText(stored.Object["spec"], "version")),
		To:    chartVersion(specText(desired, "version")),
	}, true
}

func specText(spec any, key string) string {
	m, ok := spec.(map[string]any)
	if !ok {
		return ""
	}
	if s, isText := m[key].(string); isText {
		return s
	}
	return ""
}

// chartVersion is a proxy's version without the digest that pins it.
func chartVersion(pinned string) string {
	v, _, _ := strings.Cut(pinned, "@")
	return v
}

// covers reports whether have holds everything want does. A server may add defaults to what it
// stores, which are no change.
func covers(have, want any) bool {
	wm, ok := want.(map[string]any)
	if !ok {
		return reflect.DeepEqual(have, want)
	}
	hm, ok := have.(map[string]any)
	if !ok {
		return false
	}
	for k, w := range wm {
		if h, found := hm[k]; !found || !covers(h, w) {
			return false
		}
	}
	return true
}

// recordAddonChanges records on the cluster the changes that wait for its window, and takes the
// request to apply them now off once it has been answered.
func (r *Reconciler) recordAddonChanges(ctx context.Context, cluster *clusterv1.Cluster, addons *Addons) error {
	changes, g := addons.pending, addons.rollout
	value := any(nil)
	if len(changes) > 0 {
		raw, err := json.Marshal(AddonChangesStatus{Opens: g.opens.UTC(), Changes: changes})
		if err != nil {
			return fmt.Errorf("recording the add-on changes that wait: %w", err)
		}
		value = string(raw)
	}
	_, asked := cluster.Annotations[ApplyAddonChangesAnnotation]
	if current, has := cluster.Annotations[AddonChangesAnnotation]; has != (value != nil) || (has && current != value) || asked {
		body := map[string]any{AddonChangesAnnotation: value}
		if asked {
			body[ApplyAddonChangesAnnotation] = nil
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{annotationsKey: body}})
		if err != nil {
			return fmt.Errorf("recording the add-on changes that wait: %w", err)
		}
		target := &clusterv1.Cluster{}
		target.Namespace, target.Name = cluster.Namespace, cluster.Name
		if err := r.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return fmt.Errorf("recording the add-on changes that wait: %w", err)
		}
	}
	return r.markAddonChanges(ctx, cluster, changes, g.opens)
}

// markAddonChanges sets the cluster's AddonChangesPending condition, or takes it off where nothing
// waits. It writes only a change.
func (r *Reconciler) markAddonChanges(ctx context.Context, cluster *clusterv1.Cluster, changes []AddonChange, opens time.Time) error {
	found := meta.FindStatusCondition(cluster.Status.Conditions, AddonChangesPendingCondition)
	if len(changes) == 0 {
		if found == nil {
			return nil
		}
		_, err := markCluster(ctx, r.Client, r.Reader, client.ObjectKeyFromObject(cluster), AddonChangesPendingCondition, nil)
		return err
	}
	cond := metav1.Condition{
		Type: AddonChangesPendingCondition, Status: metav1.ConditionTrue, Reason: AddonChangesWaiting,
		Message: changesText(changes) + " wait for the upgrade window, which opens " + opens.UTC().Format(time.RFC3339),
	}
	if found != nil && found.Status == cond.Status && found.Reason == cond.Reason && found.Message == shorten(cond.Message, watchMessageMax) {
		return nil
	}
	_, err := markCluster(ctx, r.Client, r.Reader, client.ObjectKeyFromObject(cluster), AddonChangesPendingCondition, &cond)
	return err
}

// changesText names the changes: "cert-manager v1.19.1 to v1.20.0, kyverno values".
func changesText(changes []AddonChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.From == c.To {
			parts = append(parts, c.Addon+" values")
			continue
		}
		parts = append(parts, c.Addon+" "+c.From+" to "+c.To)
	}
	return strings.Join(parts, ", ")
}

// untilWindow is how long the controller waits before it looks at a cluster whose add-on changes
// wait, if sooner than resync.
func (a *Addons) untilWindow(now time.Time) (time.Duration, bool) {
	if len(a.pending) == 0 || a.rollout.opens.IsZero() {
		return 0, false
	}
	until := max(a.rollout.opens.Sub(now), 0) + time.Second
	return until, until < resync
}
