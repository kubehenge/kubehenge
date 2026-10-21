package provisioner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

//go:embed release.sh
var releaseScript string

const releaseScriptPath = "/usr/local/sbin/kubehenge-release"

// ReleaseChecksumsName is the ConfigMap the checksums are kept in, one key per release.
const ReleaseChecksumsName = "kubehenge-release-checksums"

// ReleaseChecksums records, for each RKE2 and k3s release a cluster uses, the SHA-256 of the
// release's checksum file and of its install script as first read, and has every node of that
// release check its files against them. Neither project signs its releases or images, and a tag or
// an asset can be replaced; without this a node would run whatever its tag names on the day it
// boots.
type ReleaseChecksums struct {
	// Reader should read past any cache, so that a release recorded a moment ago is found.
	Reader    client.Reader
	Client    client.Client
	Namespace string
	HTTP      *http.Client
	// GitHub and Raw are https://github.com and https://raw.githubusercontent.com, unless a test
	// serves its own.
	GitHub, Raw string
}

// releaseTemplate is where a bootstrap template of RKE2 or k3s keeps its node's configuration.
type releaseTemplate struct {
	release      *distributionRelease
	spec         string
	commands     string
	controlPlane bool
}

const templateSpec = "/spec/template/spec"

type distributionRelease struct {
	distribution templates.Distribution
	repository   string
	version      *regexp.Regexp
	// files are what every node installs.
	files []string
}

// k3sBinary is the release's k3s itself.
const k3sBinary = "k3s"

var (
	rke2Release = &distributionRelease{templates.RKE2, "rancher/rke2", regexp.MustCompile(`^v1\.\d+\.\d+\+rke2r\d+$`), []string{"rke2.linux-amd64.tar.gz", "rke2-images-core.linux-amd64.tar.zst"}}
	k3sRelease  = &distributionRelease{templates.K3s, "k3s-io/k3s", regexp.MustCompile(`^v1\.\d+\.\d+\+k3s\d+$`), []string{k3sBinary, "k3s-airgap-images-amd64.tar.zst"}}
)

var releaseTemplates = map[string]releaseTemplate{
	"RKE2ControlPlaneTemplate":    {release: rke2Release, spec: templateSpec, commands: "preRKE2Commands", controlPlane: true},
	"RKE2ConfigTemplate":          {release: rke2Release, spec: templateSpec, commands: "preRKE2Commands"},
	"KThreesControlPlaneTemplate": {release: k3sRelease, spec: templateSpec + "/kthreesConfigSpec", commands: "preK3sCommands", controlPlane: true},
	"KThreesConfigTemplate":       {release: k3sRelease, spec: templateSpec, commands: "preK3sCommands"},
}

// rke2CNIs are the CNIs RKE2 brings, each with its own images. A release is recorded only if it
// lists all of them, so that a cluster on another CNI finds the release it shares complete.
var rke2CNIs = []string{"canal", "calico", "cilium"}

func rke2CNIImages(cni string) string { return "rke2-images-" + cni + ".linux-amd64.tar.zst" }

const (
	maxChecksumFile  = 1 << 16
	maxInstallScript = 1 << 20
	// recordAttempts bounds the rereads while other hooks record releases at the same moment.
	recordAttempts = 5
)

// patch has an RKE2 or k3s node install its release from checked files, through its bootstrap
// provider's air-gapped install; it is nil for any other template.
func (r *ReleaseChecksums) patch(ctx context.Context, item *runtimehooksv1.GeneratePatchesRequestItem, variables []runtimehooksv1.Variable) ([]byte, error) {
	var object metav1.TypeMeta
	if len(item.Object.Raw) > 0 {
		if err := json.Unmarshal(item.Object.Raw, &object); err != nil {
			return nil, fmt.Errorf("reading template %s: %w", item.UID, err)
		}
	}
	tmpl, ok := releaseTemplates[object.Kind]
	if !ok {
		return nil, nil
	}
	version, err := builtinVersion(item.Variables, tmpl.controlPlane)
	if err != nil {
		return nil, err
	}
	release := tmpl.release
	if !release.version.MatchString(version) {
		return nil, fmt.Errorf("%s is no %s release", version, release.distribution)
	}
	files := release.files
	if release == rke2Release {
		cni, cniErr := stringVariable(variables, "cni", "canal")
		if cniErr != nil {
			return nil, cniErr
		}
		if !slices.Contains(rke2CNIs, cni) {
			return nil, fmt.Errorf("RKE2 brings no CNI %q", cni)
		}
		files = append(slices.Clone(files), rke2CNIImages(cni))
	}
	sums, err := r.checksums(ctx, release, version)
	if err != nil {
		return nil, err
	}
	command := strings.Join(append([]string{releaseScriptPath, string(release.distribution), version, sums.file, sums.script}, files...), " ")
	patch, err := json.Marshal([]map[string]any{
		addOp(tmpl.spec+"/files/-", map[string]any{
			"path": releaseScriptPath, "owner": "root:root", "permissions": "0700", "content": releaseScript,
		}),
		addOp(tmpl.spec+"/"+tmpl.commands+"/-", command),
		addOp(tmpl.spec+"/agentConfig/airGapped", true),
	})
	if err != nil {
		return nil, fmt.Errorf("patching template %s: %w", item.UID, err)
	}
	return patch, nil
}

// builtinVersion is the Kubernetes version the template's machines run: during an upgrade the
// control plane's and the workers' differ.
func builtinVersion(variables []runtimehooksv1.Variable, controlPlane bool) (string, error) {
	for _, v := range variables {
		if v.Name != runtimehooksv1.BuiltinsName {
			continue
		}
		var b runtimehooksv1.Builtins
		if err := json.Unmarshal(v.Value.Raw, &b); err != nil {
			return "", fmt.Errorf("variable %s: %w", runtimehooksv1.BuiltinsName, err)
		}
		switch {
		case controlPlane && b.ControlPlane != nil && b.ControlPlane.Version != "":
			return b.ControlPlane.Version, nil
		case !controlPlane && b.MachineDeployment != nil && b.MachineDeployment.Version != "":
			return b.MachineDeployment.Version, nil
		}
	}
	return "", errors.New("the template's variables carry no Kubernetes version")
}

func stringVariable(variables []runtimehooksv1.Variable, name, fallback string) (string, error) {
	for _, v := range variables {
		if v.Name != name {
			continue
		}
		var s string
		if err := json.Unmarshal(v.Value.Raw, &s); err != nil {
			return "", fmt.Errorf("variable %s: %w", name, err)
		}
		return s, nil
	}
	return fallback, nil
}

type releaseSums struct{ file, script string }

// checksums are the release's recorded SHA-256s, read from the release and recorded the first
// time; a record never changes after, and the first of several hooks recording at once wins.
func (r *ReleaseChecksums) checksums(ctx context.Context, release *distributionRelease, version string) (releaseSums, error) {
	// A ConfigMap key takes no "+".
	key := strings.ReplaceAll(version, "+", "_")
	cm, err := r.record(ctx)
	if err != nil {
		return releaseSums{}, err
	}
	if v, ok := cm.Data[key]; ok {
		return parseReleaseSums(key, v)
	}
	sums, err := r.read(ctx, release, version)
	if err != nil {
		return releaseSums{}, err
	}
	for range recordAttempts {
		if v, ok := cm.Data[key]; ok {
			return parseReleaseSums(key, v)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[key] = sums.file + " " + sums.script
		if cm.ResourceVersion == "" {
			err = r.Client.Create(ctx, cm)
		} else {
			err = r.Client.Update(ctx, cm)
		}
		if err == nil {
			return sums, nil
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return releaseSums{}, fmt.Errorf("recording %s: %w", version, err)
		}
		if cm, err = r.record(ctx); err != nil {
			return releaseSums{}, err
		}
	}
	return releaseSums{}, fmt.Errorf("recording %s: the record kept changing", version)
}

// record is the ConfigMap the checksums are kept in, empty and not yet created before the first.
func (r *ReleaseChecksums) record(ctx context.Context) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	err := r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: ReleaseChecksumsName}, cm)
	if apierrors.IsNotFound(err) {
		return &corev1.ConfigMap{Namespace: r.Namespace, Name: ReleaseChecksumsName}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the release checksums: %w", err)
	}
	return cm, nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func parseReleaseSums(key, v string) (releaseSums, error) {
	file, script, ok := strings.Cut(v, " ")
	if !ok || !sha256Hex.MatchString(file) || !sha256Hex.MatchString(script) {
		return releaseSums{}, fmt.Errorf("the release checksums' %s is not two SHA-256s: %q", key, v)
	}
	return releaseSums{file: file, script: script}, nil
}

// read hashes the release's checksum file and install script as they are now, and checks the
// file lists everything a node of the release may install.
func (r *ReleaseChecksums) read(ctx context.Context, release *distributionRelease, version string) (releaseSums, error) {
	file, err := r.fetch(ctx, r.GitHub+"/"+release.repository+"/releases/download/"+version+"/sha256sum-amd64.txt", maxChecksumFile)
	if err != nil {
		return releaseSums{}, err
	}
	listed := map[string]bool{}
	lines := bufio.NewScanner(bytes.NewReader(file))
	for lines.Scan() {
		if sum, name, ok := strings.Cut(lines.Text(), "  "); ok && sha256Hex.MatchString(sum) {
			listed[name] = true
		}
	}
	want := release.files
	if release == rke2Release {
		want = slices.Clone(want)
		for _, cni := range rke2CNIs {
			want = append(want, rke2CNIImages(cni))
		}
	}
	for _, f := range want {
		if !listed[f] {
			return releaseSums{}, fmt.Errorf("the checksums of %s list no %s", version, f)
		}
	}
	script, err := r.fetch(ctx, r.Raw+"/"+release.repository+"/"+version+"/install.sh", maxInstallScript)
	if err != nil {
		return releaseSums{}, err
	}
	return releaseSums{file: hexSHA256(file), script: hexSHA256(script)}, nil
}

func (r *ReleaseChecksums) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return body, nil
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
