package provisioner

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ManifestsLabel names the provider a ConfigMap of manifests is for, as its kind and name in
	// lower case; with the operator's version label, it is what the provider's fetchConfig selects.
	ManifestsLabel = "kubehenge.dev/provider-manifests"
	// manifestsVersionLabel is the operator's: it reads the provider version a ConfigMap holds from it.
	manifestsVersionLabel = "provider.cluster.x-k8s.io/version"
	// Cluster API's core components, the largest, are about a megabyte.
	maxManifest = 16 << 20
)

// errDigest is a download that arrived whole and does not match its digest, which trying again
// cannot change.
var errDigest = errors.New("does not match its digest")

// builtManifests are the manifests of Kubehenge's provider builds that no release publishes, made
// from the same source as their images (management-cluster/versions.tf), by provider and file.
//
//go:embed all:providers
var builtManifests embed.FS

// builtScheme names one of builtManifests, by its path under providers/, in place of a download.
const builtScheme = "kubehenge:"

// ProviderManifests names the files the Cluster API operator installs a provider from, each as
// <https URL>@sha256:<digest>, or kubehenge:<provider>/<file>@sha256:<digest> for one this binary
// carries.
type ProviderManifests struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Version    string `json:"version"`
	Metadata   string `json:"metadata"`
	Components string `json:"components"`
}

// ManifestsSelector is the value of ManifestsLabel for a provider.
func ManifestsSelector(kind, name string) string {
	return strings.ToLower(kind) + "-" + name
}

// StoreProviderManifests downloads each provider's files, checks them against their digests and
// stores them in the ConfigMap the provider's fetchConfig selects, then removes the ConfigMaps of
// the provider's other versions. The operator would otherwise fetch them itself, from a release
// address it checks nothing against. A download that fails is tried again every interval until ctx
// ends; a file that does not match its digest fails at once.
func StoreProviderManifests(ctx context.Context, c client.Client, hc *http.Client, providers []ProviderManifests, interval time.Duration) error {
	for i := range providers {
		p := &providers[i]
		metadata, err := downloadManifest(ctx, hc, p.Metadata, interval)
		if err != nil {
			return fmt.Errorf("%s %s: %w", p.Kind, p.Name, err)
		}
		components, err := downloadManifest(ctx, hc, p.Components, interval)
		if err != nil {
			return fmt.Errorf("%s %s: %w", p.Kind, p.Name, err)
		}
		cm, err := manifestsConfigMap(p, metadata, components)
		if err != nil {
			return fmt.Errorf("%s %s: %w", p.Kind, p.Name, err)
		}
		if err = c.Apply(ctx, client.ApplyConfigurationFromUnstructured(cm), client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("storing the manifests of %s %s: %w", p.Kind, p.Name, err)
		}
		others, err := otherVersions(p)
		if err != nil {
			return err
		}
		if err = c.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(p.Namespace), others); err != nil {
			return fmt.Errorf("removing the older manifests of %s %s: %w", p.Kind, p.Name, err)
		}
	}
	return nil
}

// manifestsConfigMap is the ConfigMap the operator reads a provider from. The components go in
// gzipped, as the operator itself stores them: Cluster API's core alone is past a ConfigMap's limit.
func manifestsConfigMap(p *ProviderManifests, metadata, components []byte) (*unstructured.Unstructured, error) {
	var gz bytes.Buffer
	w, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("compressing the components: %w", err)
	}
	if _, err = w.Write(components); err != nil {
		return nil, fmt.Errorf("compressing the components: %w", err)
	}
	if err = w.Close(); err != nil {
		return nil, fmt.Errorf("compressing the components: %w", err)
	}
	cm := &unstructured.Unstructured{Object: map[string]any{
		"data":       map[string]any{"metadata": string(metadata)},
		"binaryData": map[string]any{"components": base64.StdEncoding.EncodeToString(gz.Bytes())},
	}}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	cm.SetNamespace(p.Namespace)
	cm.SetName(strings.ToLower(p.Kind) + "-" + p.Name + "-" + p.Version)
	cm.SetLabels(map[string]string{ManifestsLabel: ManifestsSelector(p.Kind, p.Name), manifestsVersionLabel: p.Version})
	cm.SetAnnotations(map[string]string{"provider.cluster.x-k8s.io/compressed": strconv.FormatBool(true)})
	return cm, nil
}

// otherVersions selects a provider's ConfigMaps of every version but its own.
func otherVersions(p *ProviderManifests) (client.MatchingLabelsSelector, error) {
	provider, err := labels.NewRequirement(ManifestsLabel, selection.Equals, []string{ManifestsSelector(p.Kind, p.Name)})
	if err != nil {
		return client.MatchingLabelsSelector{}, fmt.Errorf("%s %s: %w", p.Kind, p.Name, err)
	}
	version, err := labels.NewRequirement(manifestsVersionLabel, selection.NotEquals, []string{p.Version})
	if err != nil {
		return client.MatchingLabelsSelector{}, fmt.Errorf("%s %s: %w", p.Kind, p.Name, err)
	}
	return client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(*provider, *version)}, nil
}

// downloadManifest fetches a file named as <https URL>@sha256:<digest> and checks it, trying again
// every interval while the download fails. One this binary carries is read from it, and checked all
// the same.
func downloadManifest(ctx context.Context, hc *http.Client, ref string, interval time.Duration) ([]byte, error) {
	url, digest, ok := strings.Cut(ref, "@sha256:")
	name, built := strings.CutPrefix(url, builtScheme)
	if !ok || !built && !strings.HasPrefix(url, "https://") || !isDigest(digest) {
		return nil, fmt.Errorf("%q is not <https URL>@sha256:<digest>", ref)
	}
	matches := func(body []byte) ([]byte, error) {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != digest {
			return nil, fmt.Errorf("%s %w", url, errDigest)
		}
		return body, nil
	}
	if built {
		body, err := fs.ReadFile(builtManifests, path.Join("providers", name))
		if err != nil {
			return nil, fmt.Errorf("%s is not among the manifests this binary carries: %w", url, err)
		}
		return matches(body)
	}
	for {
		body, err := getManifest(ctx, hc, url)
		if err == nil {
			return matches(body)
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(interval):
		}
	}
}

func getManifest(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("GET %s: %w", url, err)
	case len(body) > maxManifest:
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, maxManifest)
	}
	return body, nil
}
