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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

//go:embed release.sh
var releaseScript string

const releaseScriptPath = "/usr/local/sbin/kubehenge-release"

// ReleaseChecksums has every node of an RKE2 or k3s release check its files against the release's
// pins, the SHA-256 of its checksum file and of its install script. Without them a node would run
// whatever its tag names on the day it boots.
type ReleaseChecksums struct {
	// Pins are the releases a node may install; DefaultReleasePins in the controller.
	Pins ReleasePins
	// HTTP, GitHub and Raw read a release's checksum file and install script when it is pinned:
	// https://github.com and https://raw.githubusercontent.com, unless a test serves its own.
	HTTP        *http.Client
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
)

// patch has an RKE2 or k3s node install its release from checked files, through its bootstrap
// provider's air-gapped install; it is nil for any other template.
func (r *ReleaseChecksums) patch(item *runtimehooksv1.GeneratePatchesRequestItem, cluster *clusterv1.Cluster) ([]byte, error) {
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
		cni, set, cniErr := optionalVariable[string](cluster, "cni")
		if cniErr != nil {
			return nil, cniErr
		}
		if !set {
			cni = "canal"
		}
		if !slices.Contains(rke2CNIs, cni) {
			return nil, fmt.Errorf("RKE2 brings no CNI %q", cni)
		}
		files = append(slices.Clone(files), rke2CNIImages(cni))
	}
	sums, ok := r.Pins[version]
	if !ok {
		return nil, unpinnedRelease(release, version)
	}
	nodeOS, err := ClusterNodeOS(cluster)
	if err != nil {
		return nil, err
	}
	family, _, err := templates.ReleaseOf(nodeOS)
	if err != nil {
		return nil, err //nolint:wrapcheck // names the node OS
	}
	command := strings.Join(append([]string{releaseScriptPath, string(release.distribution), version, sums.File, sums.Script}, files...), " ")
	ops := []map[string]any{
		addOp(tmpl.spec+"/files/-", map[string]any{
			"path": releaseScriptPath, "owner": "root:root", "permissions": "0700", "content": releaseScript,
		}),
		addOp(tmpl.spec+"/"+tmpl.commands+"/-", command),
		addOp(tmpl.spec+"/agentConfig/airGapped", true),
	}
	if family.MAC() == templates.SELinux {
		// Neither bootstrap provider has a field for it, and both distributions read every file in
		// config.yaml.d. The node's template carries the distribution's policy package.
		ops = append(ops, addOp(tmpl.spec+"/files/-", map[string]any{
			"path":  "/etc/rancher/" + string(release.distribution) + "/config.yaml.d/50-kubehenge-selinux.yaml",
			"owner": "root:root", "permissions": "0600", "content": "selinux: true\n",
		}))
	}
	patch, err := json.Marshal(ops)
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

// unpinnedRelease is the error for a release this Kubehenge does not pin.
func unpinnedRelease(release *distributionRelease, version string) error {
	return fmt.Errorf("this Kubehenge release pins no %s release %s: its nodes install only the releases it carries the checksums of", release.distribution, version)
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

type releaseSums struct{ File, Script string }

// read hashes the release's checksum file and install script as they are now, for pinning them,
// and checks the file lists everything a node of the release may install.
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
			return releaseSums{}, fmt.Errorf("%w: the checksums of %s list no %s", errUnlisted, version, f)
		}
	}
	script, err := r.fetch(ctx, r.Raw+"/"+release.repository+"/"+version+"/install.sh", maxInstallScript)
	if err != nil {
		return releaseSums{}, err
	}
	return releaseSums{File: hexSHA256(file), Script: hexSHA256(script)}, nil
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
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// GitHub publishes a release before its files, and keeps some that never got any.
		return nil, fmt.Errorf("%w: %s: %s", errUnlisted, url, resp.Status)
	default:
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
