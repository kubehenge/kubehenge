package provisioner

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"
)

const (
	egressVariable = "egressProxy"
	// Keys and kinds of the Flux patch and the charts' environment values.
	envKey         = "env"
	schemeHTTP     = "http"
	deploymentKind = "Deployment"
	podTemplateKey = "template"
	patchTargetKey = "target"
	// maxEgressNoProxy bounds the exceptions a cluster lists, as its variable's schema does.
	maxEgressNoProxy = 64
)

// EgressProxy is a cluster's egressProxy variable: the proxy that its add-ons reach beyond the
// cluster through. Unset, they take the management side's proxy, if it has one.
type EgressProxy struct {
	// URL is the proxy's address, without credentials, since add-on settings are plain text.
	URL string `json:"url,omitempty"`
	// HTTPSURL is the proxy for https:// addresses, where it differs from URL.
	HTTPSURL string `json:"httpsUrl,omitempty"`
	// NoProxy are hosts, domains and networks that bypass the proxy, beside the cluster's own pod
	// and service ranges, which every cluster adds.
	NoProxy []string `json:"noProxy,omitempty"`
	// Disabled has the add-ons go direct whatever the management side uses.
	Disabled bool `json:"disabled,omitempty"`
}

// Egress is the proxy the management side runs behind, which the add-ons of clusters that set none
// of their own take. A proxy that holds credentials in its address is not passed on: it would sit in
// every cluster's HelmChartProxy in plain text.
type Egress struct {
	URL     string
	NoProxy []string
}

// egressEnv is the proxy one cluster's add-ons go through. The zero value is none.
type egressEnv struct {
	url string
	// httpsURL is the proxy for https:// addresses, or "" where url serves them.
	httpsURL string
	noProxy  []string
}

func (e egressEnv) on() bool { return e.url != "" }

// https is the proxy for https:// addresses.
func (e egressEnv) https() string {
	if e.httpsURL != "" {
		return e.httpsURL
	}
	return e.url
}

func (e egressEnv) exceptions() string { return strings.Join(e.noProxy, ",") }

// SplitNoProxy reads a comma-separated NO_PROXY into its entries.
func SplitNoProxy(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// hasCredentials reports whether a proxy address holds a user name or a password.
func hasCredentials(address string) bool {
	u, err := url.Parse(address)
	return err != nil || u.User != nil
}

// egressAlways are the exceptions of every cluster besides its pod and service ranges: its own
// services, which a proxy outside the cluster could not reach.
var egressAlways = []string{"localhost", loopback, loopbackIPv6, ".svc", ".cluster.local"}

// loopback and loopbackIPv6 are a node's or a pod's own address in each family.
const (
	loopback     = "127.0.0.1"
	loopbackIPv6 = "::1"
)

// resolve is the proxy a cluster's add-ons go through, and why a default is left out, or "". The
// cluster's variable wins: its own address, or none. Without one the management side's is the
// default, with the exceptions it lists.
func (e Egress) resolve(cluster *clusterv1.Cluster, v *EgressProxy) (egressEnv, string) {
	address, extra, secure := v.URL, v.NoProxy, v.HTTPSURL
	switch {
	case v.Disabled:
		return egressEnv{}, ""
	case address == "" && e.URL == "":
		return egressEnv{}, ""
	case address == "" && hasCredentials(e.URL):
		return egressEnv{}, "the management side's proxy holds credentials in its address, which add-on settings would show in plain text. Set egressProxy.url without them to use a proxy"
	case address == "":
		address, extra = e.URL, append(slices.Clone(e.NoProxy), v.NoProxy...)
	}
	exceptions := slices.Clone(cluster.Spec.ClusterNetwork.Pods.CIDRBlocks)
	exceptions = append(exceptions, cluster.Spec.ClusterNetwork.Services.CIDRBlocks...)
	exceptions = append(exceptions, egressAlways...)
	exceptions = append(exceptions, extra...)
	return egressEnv{url: address, httpsURL: secure, noProxy: unique(exceptions)}, ""
}

// unique drops repeats and keeps the order, so that the same settings always give the same values.
// It also drops a Need listed twice, as when a builder's storages are one.
func unique[T comparable](list []T) []T {
	seen := map[T]bool{}
	out := make([]T, 0, len(list))
	for _, item := range list {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

// vars are the environment variables of a pod that goes through the proxy, in both spellings:
// Go reads the upper-case ones, and curl, which git uses, the lower-case ones.
func (e egressEnv) vars() []any {
	out := make([]any, 0, 6) //nolint:mnd // three variables in two spellings
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		value := e.url
		switch name {
		case "HTTPS_PROXY":
			value = e.https()
		case "NO_PROXY":
			value = e.exceptions()
		}
		out = append(out,
			map[string]any{"name": name, valueKey: value},
			map[string]any{"name": strings.ToLower(name), valueKey: value},
		)
	}
	return out
}

// values are, by the suffix of the chart's HelmChartProxy, the settings that put the add-on behind
// the proxy. Only the add-ons that reach beyond their cluster are here: cert-manager for ACME and
// DNS providers, Velero for its S3 store, Argo CD for Git, Headlamp and Rancher for their sign-in
// and catalogues. Flux's controllers are patched in its FluxInstance (patchFlux).
func (e egressEnv) values() map[string]map[string]any {
	return map[string]map[string]any{
		certManagerAddon: {"http_proxy": e.url, "https_proxy": e.https(), "no_proxy": e.exceptions()},
		veleroRelease: {
			"configuration": map[string]any{"extraEnvVars": e.vars()},
			"nodeAgent":     map[string]any{"extraEnvVars": e.vars()},
		},
		argoCD:        {"global": map[string]any{envKey: e.vars()}},
		headlampAddon: {envKey: e.vars()},
		rancherAddon:  {"proxy": e.url, "noProxy": e.exceptions()},
	}
}

// apply lays the proxy over the values of the charts that reach beyond the cluster.
func (e egressEnv) apply(wanted []chartProxy) []chartProxy {
	if !e.on() {
		return wanted
	}
	proxied := e.values()
	out := slices.Clone(wanted)
	for i := range out {
		if v, ok := proxied[out[i].suffix]; ok {
			out[i].values = overlay(v, out[i].values)
		}
	}
	return out
}

// patchFlux puts every Flux controller of the FluxInstance behind the proxy, with a patch of the
// Deployments beside the one that sets their requests. They all name their container manager,
// which the merge goes by.
func (e egressEnv) patchFlux(instance *unstructured.Unstructured) error {
	if !e.on() {
		return nil
	}
	patch, err := yaml.Marshal(map[string]any{
		apiVersionKey: "apps/v1",
		"kind":        deploymentKind,
		"metadata":    map[string]any{"name": "all"},
		"spec": map[string]any{podTemplateKey: map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "manager", envKey: e.vars()}},
		}}},
	})
	if err != nil {
		return fmt.Errorf("writing the proxy patch of Flux: %w", err)
	}
	patches, _, err := unstructured.NestedSlice(instance.Object, "spec", "kustomize", "patches")
	if err != nil {
		return fmt.Errorf("reading the patches of the FluxInstance: %w", err)
	}
	patches = append(patches, map[string]any{patchTargetKey: map[string]any{"kind": deploymentKind}, "patch": string(patch)})
	if err := unstructured.SetNestedSlice(instance.Object, patches, "spec", "kustomize", "patches"); err != nil {
		return fmt.Errorf("setting the patches of the FluxInstance: %w", err)
	}
	return nil
}

// checkEgress holds the egressProxy variable to a proxy address without credentials, and
// exceptions that name something.
func checkEgress(v *EgressProxy) error {
	for _, a := range []struct{ name, address string }{{urlKey, v.URL}, {"httpsUrl", v.HTTPSURL}} {
		name, address := a.name, a.address
		if address == "" {
			continue
		}
		u, err := url.Parse(address)
		switch {
		case err != nil || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS) || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
			// The address is left out of the message: it may hold a password.
			return fmt.Errorf("%s is no http:// or https:// address with a host and no path", name)
		case u.User != nil:
			return fmt.Errorf("%s holds credentials, which add-on settings and node files would show in plain text. Give the address alone", name)
		case !proxyHost.MatchString(u.Host):
			return fmt.Errorf("%s has a host with characters other than letters, digits, dots, hyphens, a port or an IPv6 address in brackets", name)
		}
	}
	if v.HTTPSURL != "" && v.URL == "" {
		return errors.New("httpsUrl comes with url, for http:// addresses")
	}
	if v.Disabled && (v.URL != "" || v.HTTPSURL != "") {
		return errors.New("disabled and url exclude each other")
	}
	if len(v.NoProxy) > maxEgressNoProxy {
		return fmt.Errorf("noProxy lists %d entries, at most %d", len(v.NoProxy), maxEgressNoProxy)
	}
	for _, entry := range v.NoProxy {
		if !noProxyEntry.MatchString(entry) {
			return fmt.Errorf("noProxy entry %q is empty or has characters other than letters, digits and . _ * : / [ ] -", entry)
		}
	}
	return nil
}

// proxyHost and noProxyEntry hold the values to what the nodes' boot script and environment files
// read the same way in a shell, in YAML and in systemd. The chart's schema holds the same lines.
var (
	proxyHost    = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9.]*[A-Za-z0-9])?|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?$`)
	noProxyEntry = regexp.MustCompile(`^[A-Za-z0-9._*:/\[\]-]+$`)
)
