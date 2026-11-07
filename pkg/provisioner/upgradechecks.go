package provisioner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// RemovedAPI is an API in use that a Kubernetes release removes.
type RemovedAPI struct {
	// API is what kubectl api-resources lists it as: group/version resource, or version resource for
	// the core group.
	API string `json:"api"`
	// RemovedIn is the Kubernetes release that removes it, such as 1.32.
	RemovedIn string `json:"removedIn"`
	// Requests is how many requests the API server counted for it since it started, -1 when it
	// keeps no such count.
	Requests int64 `json:"requests"`
	// Seen is when the last request was seen, as far as it is known: zero when the API server's
	// record is all there is.
	Seen time.Time `json:"seen,omitzero"`
}

// RiskyWebhook is an admission webhook that rejects every request when its backend is down, with a
// backend a node drain can take away.
type RiskyWebhook struct {
	// Kind is validating or mutating.
	Kind string `json:"kind"`
	// Name is the webhook configuration.
	Name string `json:"name"`
	// Service is the namespace and name of the Service behind it.
	Service string `json:"service"`
	// Why says what is thin about the backend.
	Why string `json:"why"`
}

// UpgradeFindings are what an upgrade to a Kubernetes release would run into inside the workload
// cluster, read before it starts.
type UpgradeFindings struct {
	RemovedAPIs []RemovedAPI   `json:"removedAPIs"`
	Webhooks    []RiskyWebhook `json:"webhooks"`
	// Unchecked says what could not be read, and why.
	Unchecked []string `json:"unchecked"`
	// Basis says what the removed APIs rest on: BasisRequests, when the API server's request counts
	// tell which were used in the last days, or BasisGauge, when only its record of every use since
	// it started is there.
	Basis string `json:"basis"`
	// History is what the counts were when this was read, for the controller to keep.
	History APIHistory `json:"-"`
}

// Clean tells whether nothing was found and nothing was left unread.
func (f *UpgradeFindings) Clean() bool {
	return len(f.RemovedAPIs) == 0 && len(f.Webhooks) == 0 && len(f.Unchecked) == 0
}

// Blocks tells whether an automatic upgrade must wait: it never goes past a removed API in use, a
// thin fail-closed webhook, or a check it could not make.
func (f *UpgradeFindings) Blocks() bool { return !f.Clean() }

// Lines are the findings as sentences, one each, for the interface, the CLI and an event.
func (f *UpgradeFindings) Lines() []string {
	var out []string
	if len(f.RemovedAPIs) > 0 {
		apis := make([]string, len(f.RemovedAPIs))
		for i, a := range f.RemovedAPIs {
			apis[i] = fmt.Sprintf("%s (removed in %s%s)", a.API, a.RemovedIn, lastRequested(&a))
		}
		line := "Removed APIs still in use: " + strings.Join(apis, ", ") + "."
		if f.Basis == BasisGauge {
			line += " The API server keeps no request counts, so these are every API requested since it started."
		}
		out = append(out, line)
	}
	for _, w := range f.Webhooks {
		out = append(out, fmt.Sprintf("The %s webhook %s fails closed and its Service %s %s.", w.Kind, w.Name, w.Service, w.Why))
	}
	for _, u := range f.Unchecked {
		out = append(out, "Not checked: "+u)
	}
	return out
}

// minReadyEndpoints is how many ready endpoints keep a fail-closed webhook answering while one
// node is drained.
const minReadyEndpoints = 2

// maxMetricsBytes bounds what is read of an API server's metrics, a few MiB in practice.
const maxMetricsBytes = 256 << 20

// metricsLineMax is the longest line of the metrics read.
const metricsLineMax = 1 << 20

const (
	deprecatedAPIsMetric = "apiserver_requested_deprecated_apis{"
	requestsMetric       = "apiserver_request_total{"
)

var metricLabel = regexp.MustCompile(`(\w+)="((?:[^"\\]|\\.)*)"`)

// ownWebhookNamespaces are the namespaces of the add-ons Kubehenge installs, whose webhooks it
// sizes itself.
var ownWebhookNamespaces = []string{
	kyvernoAddon, certManagerAddon, istioNamespace, rancherNamespace, metalLBNamespace, argoCDNamespace,
	fluxNamespace, headlampAddon, monitoringAddon, VeleroNamespace, logsNamespace,
}

// MetricsSource opens an API server's /metrics.
type MetricsSource func(ctx context.Context) (io.ReadCloser, error)

// CheckUpgrade reads what an upgrade of a workload cluster to target would run into: the
// deprecated APIs in use that target removes, and the fail-closed webhooks with a thin backend. What
// cannot be read is named in Unchecked, never taken for clean.
func CheckUpgrade(ctx context.Context, kube kubernetes.Interface, metrics MetricsSource, target string) UpgradeFindings {
	f := UpgradeFindings{RemovedAPIs: []RemovedAPI{}, Webhooks: []RiskyWebhook{}, Unchecked: []string{}}
	to, ok := parseKubeVersion(target)
	if !ok {
		f.Unchecked = append(f.Unchecked, fmt.Sprintf("%q is no Kubernetes version", target))
		return f
	}
	apis, counted, err := readRemovedAPIs(ctx, metrics, to.Minor)
	if err != nil {
		f.Unchecked = append(f.Unchecked, "the deprecated APIs in use ("+err.Error()+")")
	}
	f.RemovedAPIs = append(f.RemovedAPIs, apis...)
	f.Basis = BasisGauge
	if counted {
		f.Basis = BasisRequests
	}
	webhooks, err := readRiskyWebhooks(ctx, kube)
	if err != nil {
		f.Unchecked = append(f.Unchecked, "the admission webhooks ("+err.Error()+")")
	}
	f.Webhooks = append(f.Webhooks, webhooks...)
	return f
}

func readRemovedAPIs(ctx context.Context, metrics MetricsSource, targetMinor int) ([]RemovedAPI, bool, error) {
	if metrics == nil {
		return nil, false, errors.New("no way to read the API server's metrics")
	}
	body, err := metrics(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("reading /metrics: %w", err)
	}
	defer func() { _ = body.Close() }()
	return parseDeprecatedUse(io.LimitReader(body, maxMetricsBytes), targetMinor)
}

// ParseRemovedAPIs reads the API server's apiserver_requested_deprecated_apis gauge from its metrics
// and returns the APIs in use that a release at or below targetMinor removes, by release.
func ParseRemovedAPIs(r io.Reader, targetMinor int) ([]RemovedAPI, error) {
	apis, _, err := parseDeprecatedUse(r, targetMinor)
	return apis, err
}

// parseDeprecatedUse is ParseRemovedAPIs, with each API's request count from apiserver_request_total
// and whether the metrics hold any such counts.
func parseDeprecatedUse(r io.Reader, targetMinor int) ([]RemovedAPI, bool, error) {
	var out []RemovedAPI
	requests := map[string]int64{}
	counted := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), metricsLineMax)
	for scanner.Scan() {
		text := scanner.Text()
		if rest, ok := strings.CutPrefix(text, requestsMetric); ok {
			counted = true
			if set, value, ok := metricLine(rest); ok {
				n, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
				requests[apiName(set)] += int64(n)
			}
			continue
		}
		if api, ok := gaugedAPI(text, targetMinor); ok && !slices.ContainsFunc(out, func(o RemovedAPI) bool { return o.API == api.API && o.RemovedIn == api.RemovedIn }) {
			out = append(out, api)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("reading the metrics: %w", err)
	}
	if counted {
		for i := range out {
			out[i].Requests = requests[out[i].API]
		}
	}
	slices.SortFunc(out, func(a, b RemovedAPI) int {
		ma, _ := removedMinor(a.RemovedIn)
		mb, _ := removedMinor(b.RemovedIn)
		if ma != mb {
			return ma - mb
		}
		return strings.Compare(a.API, b.API)
	})
	return out, counted, nil
}

// gaugedAPI is the API a line of apiserver_requested_deprecated_apis names when it is set and a
// release at or below targetMinor removes it.
func gaugedAPI(text string, targetMinor int) (RemovedAPI, bool) {
	line, ok := strings.CutPrefix(text, deprecatedAPIsMetric)
	if !ok {
		return RemovedAPI{}, false
	}
	set, value, ok := metricLine(line)
	if !ok || strings.TrimSpace(value) != "1" {
		return RemovedAPI{}, false
	}
	minor, ok := removedMinor(set["removed_release"])
	if !ok || minor > targetMinor {
		return RemovedAPI{}, false
	}
	return RemovedAPI{API: apiName(set), RemovedIn: set["removed_release"], Requests: -1}, true
}

// metricLine splits a metrics line, past its name, into its labels and its value.
func metricLine(rest string) (map[string]string, string, bool) {
	labels, value, ok := strings.Cut(rest, "}")
	if !ok {
		return nil, "", false
	}
	set := map[string]string{}
	for _, m := range metricLabel.FindAllStringSubmatch(labels, -1) {
		set[m[1]] = m[2]
	}
	return set, value, true
}

// removedMinor is the minor version of a removed_release label such as 1.32, false for none.
func removedMinor(release string) (int, bool) {
	rest, ok := strings.CutPrefix(release, "1.")
	if !ok {
		return 0, false
	}
	minor, err := strconv.Atoi(rest)
	return minor, err == nil
}

func apiName(labels map[string]string) string {
	version := labels["version"]
	if g := labels["group"]; g != "" {
		version = g + "/" + version
	}
	name := version + " " + labels["resource"]
	if sub := labels["subresource"]; sub != "" {
		name += "/" + sub
	}
	return name
}

// webhookRef is one webhook of a configuration, as far as an upgrade cares.
type webhookRef struct {
	kind, config string
	service      *admissionregistrationv1.ServiceReference
	failClosed   bool
}

func readRiskyWebhooks(ctx context.Context, kube kubernetes.Interface) ([]RiskyWebhook, error) {
	var refs []webhookRef
	validating, err := kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing validating webhooks: %w", err)
	}
	for i := range validating.Items {
		for j := range validating.Items[i].Webhooks {
			w := &validating.Items[i].Webhooks[j]
			refs = append(refs, webhookRef{"validating", validating.Items[i].Name, w.ClientConfig.Service, closed(w.FailurePolicy)})
		}
	}
	mutating, err := kube.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing mutating webhooks: %w", err)
	}
	for i := range mutating.Items {
		for j := range mutating.Items[i].Webhooks {
			w := &mutating.Items[i].Webhooks[j]
			refs = append(refs, webhookRef{"mutating", mutating.Items[i].Name, w.ClientConfig.Service, closed(w.FailurePolicy)})
		}
	}
	var out []RiskyWebhook
	var errs []error
	for _, ref := range refs {
		if !ref.failClosed || ref.service == nil || slices.Contains(ownWebhookNamespaces, ref.service.Namespace) {
			continue
		}
		why, err := thinBackend(ctx, kube, ref.service)
		errs = append(errs, err)
		risky := RiskyWebhook{Kind: ref.kind, Name: ref.config, Service: ref.service.Namespace + "/" + ref.service.Name, Why: why}
		if why != "" && !slices.Contains(out, risky) {
			out = append(out, risky)
		}
	}
	return out, errors.Join(errs...)
}

// closed tells whether a webhook rejects requests when it cannot answer: Fail is the API's default.
func closed(p *admissionregistrationv1.FailurePolicyType) bool {
	return p == nil || *p == admissionregistrationv1.Fail
}

// thinBackend says what is thin about a Service's endpoints, "" when two or more are ready on
// two or more nodes.
func thinBackend(ctx context.Context, kube kubernetes.Interface, svc *admissionregistrationv1.ServiceReference) (string, error) {
	parts, err := kube.DiscoveryV1().EndpointSlices(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + svc.Name})
	if err != nil {
		return "", fmt.Errorf("listing the endpoints of %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	ready := 0
	nodes := map[string]bool{}
	for i := range parts.Items {
		for _, e := range parts.Items[i].Endpoints {
			// An endpoint that does not say is ready.
			if e.Conditions.Ready != nil && !*e.Conditions.Ready {
				continue
			}
			ready++
			if e.NodeName != nil {
				nodes[*e.NodeName] = true
			}
		}
	}
	switch {
	case ready < minReadyEndpoints:
		return fmt.Sprintf("has %d ready %s", ready, plural(ready, "endpoint", "endpoints")), nil
	case len(nodes) == 1:
		return fmt.Sprintf("has all %d endpoints on one node", ready), nil
	}
	return "", nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
