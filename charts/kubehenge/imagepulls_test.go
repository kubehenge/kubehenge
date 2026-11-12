package kubehenge_test

import (
	"testing"

	"sigs.k8s.io/yaml"
)

// pullSettings are the kubelet settings that keep one stalled image pull from holding every later
// pull of the node behind it.
type pullSettings struct {
	SerializeImagePulls   *bool  `json:"serializeImagePulls"`
	MaxParallelImagePulls *int32 `json:"maxParallelImagePulls"`
}

func (p pullSettings) parallel() bool {
	return p.SerializeImagePulls != nil && !*p.SerializeImagePulls && p.MaxParallelImagePulls != nil && *p.MaxParallelImagePulls == 3
}

// fileContent returns the content of the file at path among a template's files.
func fileContent(t *testing.T, files any, path string) (string, bool) {
	t.Helper()
	list, ok := files.([]any)
	if !ok {
		t.Fatalf("files %v", files)
	}
	for _, f := range list {
		file := asMap(t, f)
		if file["path"] == path {
			content, _ := file["content"].(string)
			return content, true
		}
	}
	return "", false
}

func TestKubeletsOfKubeadmRKE2AndK3sPullImagesInParallel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file, kind, path string
		spec             []string
	}{
		{"kubeadm.yaml", "KubeadmControlPlaneTemplate", "/etc/kubernetes/patches/kubeletconfiguration+merge.yaml", []string{"spec", "template", "spec", "kubeadmConfigSpec", "files"}},
		{"kubeadm.yaml", "KubeadmConfigTemplate", "/etc/kubernetes/patches/kubeletconfiguration+merge.yaml", []string{"spec", "template", "spec", "files"}},
		{"rke2.yaml", "RKE2ControlPlaneTemplate", "/var/lib/rancher/rke2/agent/etc/kubelet.conf.d/50-kubehenge-image-pulls.conf", []string{"spec", "template", "spec", "files"}},
		{"rke2.yaml", "RKE2ConfigTemplate", "/var/lib/rancher/rke2/agent/etc/kubelet.conf.d/50-kubehenge-image-pulls.conf", []string{"spec", "template", "spec", "files"}},
		{"k3s.yaml", "KThreesControlPlaneTemplate", "/var/lib/rancher/k3s/agent/etc/kubelet.conf.d/50-kubehenge-image-pulls.conf", []string{"spec", "template", "spec", "kthreesConfigSpec", "files"}},
		{"k3s.yaml", "KThreesConfigTemplate", "/var/lib/rancher/k3s/agent/etc/kubelet.conf.d/50-kubehenge-image-pulls.conf", []string{"spec", "template", "spec", "files"}},
	}
	for _, c := range cases {
		t.Run(c.file+" "+c.kind, func(t *testing.T) {
			t.Parallel()
			class := renderClass(t, c.file)
			content, ok := fileContent(t, field(t, class.patched(t, c.kind, map[string]any{}), c.spec...), c.path)
			if !ok {
				t.Fatalf("no file %s", c.path)
			}
			var got pullSettings
			if err := yaml.Unmarshal([]byte(content), &got); err != nil {
				t.Fatal(err)
			}
			if !got.parallel() {
				t.Errorf("%s holds %q, want serializeImagePulls false and maxParallelImagePulls 3", c.path, content)
			}
		})
	}
}
