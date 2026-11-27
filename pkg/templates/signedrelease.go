package templates

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// KubernetesRegistry is where kubeadm pulls a release's images from. Kubernetes' release process
// signs each with cosign, which stores the signature beside it as the tag sha256-<digest>.sig.
const KubernetesRegistry = "https://registry.k8s.io"

// Signatures change only when a release is signed late, so an answer is kept for hours. The map is
// emptied once it holds signatureCacheSize releases, far more than are ever asked for.
const (
	signatureCacheFor  = 6 * time.Hour
	signatureCacheSize = 256
	// nearestSearch bounds how many patch releases either side the nearest signed one is looked for.
	nearestSearch = 16
)

// ReleaseSignatures tells whether a Kubernetes release's images carry its release signature, as
// images.sh demands of a kubeadm template, so that a release it would refuse is refused before any
// build starts.
type ReleaseSignatures struct {
	// Registry is KubernetesRegistry outside tests.
	Registry string
	HTTP     *http.Client

	mu   sync.Mutex
	seen map[string]signatureAnswer
}

type releaseSignature int

const (
	releaseSigned releaseSignature = iota
	releaseUnsigned
	releaseMissing
	// releaseUnknown is a release the registry did not answer for.
	releaseUnknown
)

type signatureAnswer struct {
	state releaseSignature
	at    time.Time
}

// Check refuses a release whose images carry no signature, naming the nearest signed release of its
// minor version, and a release that does not exist. A registry that does not answer leaves the
// release to the build's own check, and note says so.
func (r *ReleaseSignatures) Check(ctx context.Context, version string) (string, error) {
	state, unanswered := r.state(ctx, version)
	switch state {
	case releaseSigned:
		return "", nil
	case releaseUnknown:
		return "release signature unchecked: " + unanswered, nil
	case releaseMissing:
		return "", fmt.Errorf("%s is no Kubernetes release", version)
	case releaseUnsigned:
	}
	if near := r.nearest(ctx, version); near != "" {
		return "", fmt.Errorf("%s has no signature of Kubernetes' release. Use %s", version, near)
	}
	return "", fmt.Errorf("%s has no signature of Kubernetes' release", version)
}

// nearest is the signed release of version's minor version closest to it, the newer one first, or
// "" when none is found.
func (r *ReleaseSignatures) nearest(ctx context.Context, version string) string {
	var minor, patch int
	if _, err := fmt.Sscanf(version, "v1.%d.%d", &minor, &patch); err != nil {
		return ""
	}
	newer := true
	for d := 1; d <= nearestSearch && (newer || patch >= d); d++ {
		for _, p := range [...]int{patch + d, patch - d} {
			if p < 0 || p > patch && !newer {
				continue
			}
			v := fmt.Sprintf("v1.%d.%d", minor, p)
			switch state, _ := r.state(ctx, v); {
			case state == releaseUnknown:
				return ""
			case state == releaseSigned:
				return v
			case state == releaseMissing && p > patch:
				newer = false
			}
		}
	}
	return ""
}

// state reads whether version's images are signed, or keeps the answer read within
// signatureCacheFor. A registry that does not answer is not remembered: the release is
// releaseUnknown, and the second value says why.
func (r *ReleaseSignatures) state(ctx context.Context, version string) (releaseSignature, string) {
	r.mu.Lock()
	cached, ok := r.seen[version]
	r.mu.Unlock()
	if ok && time.Since(cached.at) < signatureCacheFor {
		return cached.state, ""
	}
	state := releaseSigned
	// A release's images named after it, those images.sh refuses unsigned. The first one missing
	// means there is no such release.
	for i, image := range [...]string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-proxy"} {
		digest, found, err := r.head(ctx, image+"/manifests/"+version)
		if err != nil {
			return releaseUnknown, err.Error()
		}
		if !found {
			state = releaseUnsigned
			if i == 0 {
				state = releaseMissing
			}
			break
		}
		hex, ok := strings.CutPrefix(digest, "sha256:")
		if !ok {
			return releaseUnknown, image + " " + version + " has no sha256 digest"
		}
		if _, signed, err := r.head(ctx, image+"/manifests/sha256-"+hex+".sig"); err != nil {
			return releaseUnknown, err.Error()
		} else if !signed {
			state = releaseUnsigned
			break
		}
	}
	r.mu.Lock()
	if r.seen == nil || len(r.seen) >= signatureCacheSize {
		r.seen = map[string]signatureAnswer{}
	}
	r.seen[version] = signatureAnswer{state: state, at: time.Now()}
	r.mu.Unlock()
	return state, ""
}

// manifestTypes are those a release's tag and its signature's are, a list of images and an image.
const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// head asks the registry for a manifest, and gives its digest and whether there is one.
func (r *ReleaseSignatures) head(ctx context.Context, path string) (string, bool, error) {
	url := r.Registry + "/v2/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, http.NoBody)
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", url, err)
	}
	req.Header.Set("Accept", manifestTypes)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", url, err)
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Header.Get("Docker-Content-Digest"), true, nil
	case http.StatusNotFound:
		return "", false, nil
	}
	return "", false, fmt.Errorf("%s: %s", url, resp.Status)
}
