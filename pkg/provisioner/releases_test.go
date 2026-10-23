package provisioner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeReleases serves releases as GitHub does, its files' content changeable under a test.
type fakeReleases struct {
	mu    sync.Mutex
	files map[string]string
}

func (f *fakeReleases) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write([]byte(content))
}

func (f *fakeReleases) set(path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = content
}

func listing(names ...string) string {
	var b strings.Builder
	for i, n := range names {
		fmt.Fprintf(&b, "%064x  %s\n", i, n)
	}
	return b.String()
}

var (
	rke2Listing = listing("rke2.linux-amd64.tar.gz", "rke2-images-core.linux-amd64.tar.zst", "rke2-images-canal.linux-amd64.tar.zst",
		"rke2-images-calico.linux-amd64.tar.zst", "rke2-images-cilium.linux-amd64.tar.zst", "rke2-images.linux-amd64.tar.zst")
	k3sListing = listing("k3s", "k3s-airgap-images-amd64.tar.zst", "k3s-images.txt")
)

func newReleaseChecksums(t *testing.T) (*ReleaseChecksums, *fakeReleases, client.Client) {
	t.Helper()
	f := &fakeReleases{files: map[string]string{
		"/rancher/rke2/releases/download/v1.37.1+rke2r1/sha256sum-amd64.txt": rke2Listing,
		"/rancher/rke2/v1.37.1+rke2r1/install.sh":                            "#!/bin/sh\n# rke2 1.37.1\n",
		"/rancher/rke2/releases/download/v1.36.5+rke2r1/sha256sum-amd64.txt": rke2Listing,
		"/rancher/rke2/v1.36.5+rke2r1/install.sh":                            "#!/bin/sh\n# rke2 1.36.5\n",
		"/k3s-io/k3s/releases/download/v1.37.1+k3s1/sha256sum-amd64.txt":     k3sListing,
		"/k3s-io/k3s/v1.37.1+k3s1/install.sh":                                "#!/bin/sh\n# k3s\n",
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := fake.NewClientBuilder().Build()
	return &ReleaseChecksums{Reader: c, Client: c, Namespace: "kubehenge", HTTP: srv.Client(), GitHub: srv.URL, Raw: srv.URL}, f, c
}

// releaseItem is a template of the kind given, with the control plane's and the workers' versions.
func releaseItem(t *testing.T, kind, controlPlane, workers string) *runtimehooksv1.GeneratePatchesRequestItem {
	t.Helper()
	builtins, err := json.Marshal(runtimehooksv1.Builtins{
		ControlPlane:      &runtimehooksv1.ControlPlaneBuiltins{Version: controlPlane},
		MachineDeployment: &runtimehooksv1.MachineDeploymentBuiltins{Version: workers},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &runtimehooksv1.GeneratePatchesRequestItem{
		UID:       "template",
		Object:    runtime.RawExtension{Raw: []byte(`{"apiVersion":"x.cluster.x-k8s.io/v1beta2","kind":"` + kind + `"}`)},
		Variables: []runtimehooksv1.Variable{{Name: runtimehooksv1.BuiltinsName, Value: apiextensionsv1.JSON{Raw: builtins}}},
	}
}

func cniVariable(cni string) []runtimehooksv1.Variable {
	return []runtimehooksv1.Variable{{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"` + cni + `"`)}}}
}

type releaseOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func releaseOps(t *testing.T, patch []byte) map[string]any {
	t.Helper()
	var ops []releaseOp
	if err := json.Unmarshal(patch, &ops); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]any{}
	for _, op := range ops {
		if op.Op != "add" {
			t.Errorf("%s: op %s", op.Path, op.Op)
		}
		// Of the files appended, the first is the release's script; the rest are listed beside it.
		if _, seen := byPath[op.Path]; seen && strings.HasSuffix(op.Path, "/files/-") {
			more, ok := byPath[op.Path+"+"].([]any)
			if !ok {
				t.Fatalf("%s: the files after the first are %v", op.Path, byPath[op.Path+"+"])
			}
			byPath[op.Path+"+"] = append(more, op.Value)
			continue
		}
		byPath[op.Path] = op.Value
		if strings.HasSuffix(op.Path, "/files/-") {
			byPath[op.Path+"+"] = []any{}
		}
	}
	return byPath
}

func TestReleasePatchesInstallTheRecordedFiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind, version, spec, commands string
		variables                           []runtimehooksv1.Variable
		files                               []string
	}{
		{
			"an RKE2 control plane on its default CNI", "RKE2ControlPlaneTemplate", "v1.37.1+rke2r1", "/spec/template/spec", "preRKE2Commands", nil,
			[]string{"rke2.linux-amd64.tar.gz", "rke2-images-core.linux-amd64.tar.zst", "rke2-images-canal.linux-amd64.tar.zst"},
		},
		{
			"an RKE2 worker, on the workers' version", "RKE2ConfigTemplate", "v1.36.5+rke2r1", "/spec/template/spec", "preRKE2Commands", cniVariable("cilium"),
			[]string{"rke2.linux-amd64.tar.gz", "rke2-images-core.linux-amd64.tar.zst", "rke2-images-cilium.linux-amd64.tar.zst"},
		},
		{
			"a k3s control plane", "KThreesControlPlaneTemplate", "v1.37.1+k3s1", "/spec/template/spec/kthreesConfigSpec", "preK3sCommands", cniVariable("cilium"),
			[]string{"k3s", "k3s-airgap-images-amd64.tar.zst"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, f, c := newReleaseChecksums(t)
			controlPlane, workers := tc.version, "v1.0.0"
			if !strings.HasSuffix(tc.kind, "ControlPlaneTemplate") {
				controlPlane, workers = "v1.0.0", tc.version
			}
			patch, err := r.patch(t.Context(), releaseItem(t, tc.kind, controlPlane, workers), tc.variables)
			if err != nil {
				t.Fatal(err)
			}
			ops := releaseOps(t, patch)
			release := releaseTemplates[tc.kind].release
			sums := hexSHA256([]byte(f.files["/"+release.repository+"/releases/download/"+tc.version+"/sha256sum-amd64.txt"]))
			script := hexSHA256([]byte(f.files["/"+release.repository+"/"+tc.version+"/install.sh"]))
			want := strings.Join(append([]string{releaseScriptPath, string(release.distribution), tc.version, sums, script}, tc.files...), " ")
			if got := ops[tc.spec+"/"+tc.commands+"/-"]; got != want {
				t.Errorf("command %v, want %s", got, want)
			}
			if file, ok := ops[tc.spec+"/files/-"].(map[string]any); !ok || file["path"] != releaseScriptPath || file["content"] != releaseScript || file["permissions"] != "0700" {
				t.Errorf("script file %v", ops[tc.spec+"/files/-"])
			}
			// The default node OS enforces SELinux, which the distribution is told to work with.
			dropIn := "/etc/rancher/" + string(release.distribution) + "/config.yaml.d/50-kubehenge-selinux.yaml"
			more, ok := ops[tc.spec+"/files/-+"].([]any)
			var first map[string]any
			if ok && len(more) == 1 {
				first, ok = more[0].(map[string]any)
			}
			if !ok || len(more) != 1 || first["path"] != dropIn || first["content"] != "selinux: true\n" || first["permissions"] != "0600" {
				t.Errorf("further files %v, want only %s", more, dropIn)
			}
			if airGapped, ok := ops[tc.spec+"/agentConfig/airGapped"].(bool); !ok || !airGapped {
				t.Errorf("airGapped %v, want true", ops[tc.spec+"/agentConfig/airGapped"])
			}
			cm := &corev1.ConfigMap{}
			if err := c.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge", Name: ReleaseChecksumsName}, cm); err != nil {
				t.Fatal(err)
			}
			if got := cm.Data[strings.ReplaceAll(tc.version, "+", "_")]; got != sums+" "+script {
				t.Errorf("recorded %q, want %q", got, sums+" "+script)
			}
		})
	}
}

func TestReleaseRecordNeverChanges(t *testing.T) {
	t.Parallel()
	r, f, _ := newReleaseChecksums(t)
	item := releaseItem(t, "RKE2ControlPlaneTemplate", "v1.37.1+rke2r1", "v1.36.5+rke2r1")
	first, err := r.patch(t.Context(), item, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.set("/rancher/rke2/v1.37.1+rke2r1/install.sh", "#!/bin/sh\n# replaced under the tag\n")
	f.set("/rancher/rke2/releases/download/v1.37.1+rke2r1/sha256sum-amd64.txt", rke2Listing+"# replaced\n")
	again, err := r.patch(t.Context(), item, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, first) {
		t.Errorf("the patch followed the replaced release:\n%s\nwant\n%s", again, first)
	}
	// Recording a second release keeps the first.
	if _, err = r.patch(t.Context(), releaseItem(t, "RKE2ConfigTemplate", "v1.37.1+rke2r1", "v1.36.5+rke2r1"), nil); err != nil {
		t.Fatal(err)
	}
	if again, err = r.patch(t.Context(), item, nil); err != nil || !bytes.Equal(again, first) {
		t.Errorf("after recording another release: %v\n%s\nwant\n%s", err, again, first)
	}
}

func TestReleasePatchesRefuse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		item      func(t *testing.T) *runtimehooksv1.GeneratePatchesRequestItem
		variables []runtimehooksv1.Variable
		breakIt   func(f *fakeReleases)
		want      string
	}{
		{
			name: "a version that is no release of the distribution",
			item: func(t *testing.T) *runtimehooksv1.GeneratePatchesRequestItem {
				t.Helper()
				return releaseItem(t, "RKE2ControlPlaneTemplate", "v1.37.1+k3s1", "")
			},
			want: "no rke2 release",
		},
		{
			name: "a version that would reach the node's shell",
			item: func(t *testing.T) *runtimehooksv1.GeneratePatchesRequestItem {
				t.Helper()
				return releaseItem(t, "KThreesConfigTemplate", "", "v1.37.1+k3s1; reboot")
			},
			want: "no k3s release",
		},
		{
			name: "a CNI RKE2 does not bring",
			item: func(t *testing.T) *runtimehooksv1.GeneratePatchesRequestItem {
				t.Helper()
				return releaseItem(t, "RKE2ConfigTemplate", "", "v1.37.1+rke2r1")
			},
			variables: cniVariable("flannel"),
			want:      `no CNI "flannel"`,
		},
		{
			name: "a template without its version",
			item: func(t *testing.T) *runtimehooksv1.GeneratePatchesRequestItem {
				t.Helper()
				return releaseItem(t, "RKE2ConfigTemplate", "v1.37.1+rke2r1", "")
			},
			want: "no Kubernetes version",
		},
		{
			name: "a release whose checksums miss a CNI's images",
			item: func(t *testing.T) *runtimehooksv1.GeneratePatchesRequestItem {
				t.Helper()
				return releaseItem(t, "RKE2ControlPlaneTemplate", "v1.37.1+rke2r1", "")
			},
			breakIt: func(f *fakeReleases) {
				f.set("/rancher/rke2/releases/download/v1.37.1+rke2r1/sha256sum-amd64.txt", listing("rke2.linux-amd64.tar.gz", "rke2-images-core.linux-amd64.tar.zst", "rke2-images-canal.linux-amd64.tar.zst"))
			},
			want: "list no rke2-images-calico.linux-amd64.tar.zst",
		},
		{
			name: "a release without its install script",
			item: func(t *testing.T) *runtimehooksv1.GeneratePatchesRequestItem {
				t.Helper()
				return releaseItem(t, "KThreesControlPlaneTemplate", "v1.37.1+k3s1", "")
			},
			breakIt: func(f *fakeReleases) { delete(f.files, "/k3s-io/k3s/v1.37.1+k3s1/install.sh") },
			want:    "404",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, f, c := newReleaseChecksums(t)
			if tc.breakIt != nil {
				tc.breakIt(f)
			}
			_, err := r.patch(t.Context(), tc.item(t), tc.variables)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
			cm := &corev1.ConfigMap{}
			if err := c.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge", Name: ReleaseChecksumsName}, cm); err == nil {
				t.Errorf("recorded %v", cm.Data)
			}
		})
	}
}

func TestReleasePatchesLeaveOtherTemplates(t *testing.T) {
	t.Parallel()
	r, _, _ := newReleaseChecksums(t)
	for _, kind := range []string{"KubeadmControlPlaneTemplate", "KubeadmConfigTemplate", "ProxmoxMachineTemplate", ""} {
		item := releaseItem(t, kind, "v1.37.1", "v1.37.1")
		if kind == "" {
			item.Object.Raw = nil
		}
		if patch, err := r.patch(t.Context(), item, nil); patch != nil || err != nil {
			t.Errorf("%q: %s, %v", kind, patch, err)
		}
	}
}

func TestGeneratePatchesFailOnARelease(t *testing.T) {
	t.Parallel()
	r, _, _ := newReleaseChecksums(t)
	h := &Hooks{Reader: r.Reader, Releases: r}
	req := &runtimehooksv1.GeneratePatchesRequest{Items: []runtimehooksv1.GeneratePatchesRequestItem{
		*releaseItem(t, "RKE2ControlPlaneTemplate", "v1.37.1+rke2r1", ""),
		*releaseItem(t, "RKE2ConfigTemplate", "", "v1.35.0+rke2r1"),
	}}
	resp := &runtimehooksv1.GeneratePatchesResponse{}
	h.GeneratePatches(t.Context(), req, resp)
	if resp.Status != runtimehooksv1.ResponseStatusFailure || !strings.Contains(resp.Message, "v1.35.0+rke2r1") {
		t.Errorf("status %s, message %q, want a failure naming the missing release", resp.Status, resp.Message)
	}
	req.Items = req.Items[:1]
	resp = &runtimehooksv1.GeneratePatchesResponse{}
	h.GeneratePatches(t.Context(), req, resp)
	if resp.Status != runtimehooksv1.ResponseStatusSuccess || len(resp.Items) != 1 || !slices.ContainsFunc(resp.Items, func(i runtimehooksv1.GeneratePatchesResponseItem) bool {
		return i.UID == "template" && i.PatchType == runtimehooksv1.JSONPatchType
	}) {
		t.Errorf("status %s, items %v", resp.Status, resp.Items)
	}
}
