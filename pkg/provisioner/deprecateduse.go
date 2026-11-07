package provisioner

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// What the removed APIs of an upgrade's findings rest on.
const (
	// BasisRequests: the API server counts its requests per API, so an API counts as in use only
	// while its count moved in the last DeprecatedUseWindow.
	BasisRequests = "requests"
	// BasisGauge: only apiserver_requested_deprecated_apis is there, which holds every API requested
	// since the API server started and only a restart clears.
	BasisGauge = "gauge"
)

// DeprecatedUseWindow is how long after its last request an API stays in use for the checks. The
// gauge alone never lets go, so one old request would hold an upgrade that is the restart that
// clears it.
const DeprecatedUseWindow = 7 * 24 * time.Hour

const (
	// DeprecatedUseAnnotation on a Cluster keeps the controller's last reading of each deprecated
	// API's request count and when the count last moved.
	DeprecatedUseAnnotation = "kubehenge.dev/deprecated-api-use"
	// AcknowledgedAnnotation on a Cluster lists, by target minor version, the findings a person
	// accepted for the automatic upgrade to it, as {"1.37":["policy/v1beta1 podsecuritypolicies"]}.
	// A finding not listed holds the automatic upgrade again.
	AcknowledgedAnnotation = "kubehenge.dev/acknowledged-upgrade-findings"
)

// APIUse is the last reading of one API's request count.
type APIUse struct {
	Requests int64     `json:"requests"`
	Seen     time.Time `json:"seen"`
}

// APIHistory is the readings by API, as the cluster's annotation keeps them.
type APIHistory map[string]APIUse

// HistoryOf is what the controller last read of the cluster's deprecated APIs, empty when it read
// none or the annotation is not its own.
func HistoryOf(c *clusterv1.Cluster) APIHistory {
	h := APIHistory{}
	if raw := c.Annotations[DeprecatedUseAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			return APIHistory{}
		}
	}
	return h
}

// Encode is the annotation's value.
func (h APIHistory) Encode() string {
	raw, _ := json.Marshal(h) //nolint:errchkjson // strings, numbers and times always encode
	return string(raw)
}

// ApplyHistory dates each removed API's last request against the earlier readings, drops those not
// requested for DeprecatedUseWindow, and sets f.History to the readings to keep. An API met for the
// first time counts as requested now: nothing says when it was last used. Without request counts
// the findings stay as they are.
func (f *UpgradeFindings) ApplyHistory(earlier APIHistory, now time.Time) {
	f.History = earlier
	if f.Basis != BasisRequests {
		return
	}
	next := APIHistory{}
	kept := make([]RemovedAPI, 0, len(f.RemovedAPIs))
	for _, a := range f.RemovedAPIs {
		use := APIUse{Requests: a.Requests, Seen: earlier[a.API].Seen}
		if prev, known := earlier[a.API]; !known || prev.Requests != a.Requests {
			use.Seen = now
		}
		next[a.API] = use
		a.Seen = use.Seen
		if now.Sub(use.Seen) < DeprecatedUseWindow {
			kept = append(kept, a)
		}
	}
	f.RemovedAPIs, f.History = kept, next
}

// lastRequested is the sentence part that dates an API's last request.
func lastRequested(a *RemovedAPI) string {
	if a.Seen.IsZero() {
		return ""
	}
	return ", last requested " + a.Seen.UTC().Format(time.DateOnly)
}

// Key names a webhook among the findings that can be acknowledged.
func (w *RiskyWebhook) Key() string { return "webhook " + w.Kind + "/" + w.Name }

// findingKeys are the findings of an upgrade a person can accept for the automatic upgrade.
func (f *UpgradeFindings) findingKeys() []string {
	keys := make([]string, 0, len(f.RemovedAPIs)+len(f.Webhooks))
	for i := range f.RemovedAPIs {
		keys = append(keys, f.RemovedAPIs[i].API)
	}
	for i := range f.Webhooks {
		keys = append(keys, f.Webhooks[i].Key())
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

func minorKey(target string) (string, bool) {
	v, ok := parseKubeVersion(target)
	return fmt.Sprintf("1.%d", v.Minor), ok
}

func acknowledgedOf(c *clusterv1.Cluster) map[string][]string {
	acked := map[string][]string{}
	if raw := c.Annotations[AcknowledgedAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &acked); err != nil {
			return map[string][]string{}
		}
	}
	return acked
}

// Unacknowledged drops from f the findings the cluster's annotation accepts for the minor version
// of target. What it does not list, a new API in use for one, still counts.
func (f *UpgradeFindings) Unacknowledged(c *clusterv1.Cluster, target string) {
	minor, ok := minorKey(target)
	if !ok {
		return
	}
	accepted := acknowledgedOf(c)[minor]
	if len(accepted) == 0 {
		return
	}
	apis := make([]RemovedAPI, 0, len(f.RemovedAPIs))
	for _, a := range f.RemovedAPIs {
		if !slices.Contains(accepted, a.API) {
			apis = append(apis, a)
		}
	}
	hooks := make([]RiskyWebhook, 0, len(f.Webhooks))
	for i := range f.Webhooks {
		if !slices.Contains(accepted, f.Webhooks[i].Key()) {
			hooks = append(hooks, f.Webhooks[i])
		}
	}
	f.RemovedAPIs, f.Webhooks = apis, hooks
}

// Acknowledgement is the annotation's value that accepts what f found for the automatic upgrade to
// target, kept beside what the cluster already accepted for other minor versions.
func Acknowledgement(c *clusterv1.Cluster, target string, f *UpgradeFindings) (string, error) {
	minor, ok := minorKey(target)
	if !ok {
		return "", fmt.Errorf("%q is no Kubernetes version", target)
	}
	acked := maps.Clone(acknowledgedOf(c))
	acked[minor] = f.findingKeys()
	raw, err := json.Marshal(acked)
	if err != nil {
		return "", fmt.Errorf("encoding what was accepted: %w", err)
	}
	return string(raw), nil
}

// SkippedTarget is the version an AutoUpgradeSkippedCondition message says the automatic upgrade
// goes to, "" when it names none.
func SkippedTarget(message string) string {
	rest, ok := strings.CutPrefix(message, skippedPrefix)
	if !ok {
		return ""
	}
	target, _, _ := strings.Cut(rest, " waits.")
	return target
}

const skippedPrefix = "The automatic upgrade to "
