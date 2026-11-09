package kubehenge_test

import (
	"encoding/json"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// TestKubeadmControlPlanesRegisterWithTheTaintKubeadmLeavesOff checks that a kubeadm control plane
// takes its taint from the kubelet rather than from kubeadm. Unset taints are kubeadm's default
// taint, which the control-plane provider then requires on the node, so a control plane would stay
// not ready while the controller has the taint off, and long after it is back.
func TestKubeadmControlPlanesRegisterWithTheTaintKubeadmLeavesOff(t *testing.T) {
	t.Parallel()
	class := renderClass(t, "kubeadm.yaml")
	cpTaint := corev1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}
	// registration reads a node registration's taints as Cluster API's KubeadmConfig does: nil
	// when unset, an empty list when set to none.
	type nodeRegistration struct {
		Taints *[]corev1.Taint `json:"taints,omitempty"`
	}
	registration := func(doc map[string]any, path ...string) nodeRegistration {
		t.Helper()
		raw, err := json.Marshal(field(t, doc, path...))
		if err != nil {
			t.Fatal(err)
		}
		var r nodeRegistration
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	// registersWithTaint tells whether the files hold a kubelet patch that registers the node with
	// the control-plane taint.
	registersWithTaint := func(files any) bool {
		t.Helper()
		list, ok := files.([]any)
		if !ok {
			t.Fatalf("files %v", files)
		}
		for _, f := range list {
			var file struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(asJSON(t, f)), &file); err != nil {
				t.Fatal(err)
			}
			var kubelet struct {
				RegisterWithTaints []corev1.Taint `json:"registerWithTaints"`
			}
			if err := yaml.Unmarshal([]byte(file.Content), &kubelet); err != nil {
				continue
			}
			if slices.ContainsFunc(kubelet.RegisterWithTaints, func(x corev1.Taint) bool { return cpTaint.MatchTaint(&x) }) {
				if file.Path != "/etc/kubernetes/patches/kubeletconfiguration0+merge.yaml" {
					t.Errorf("the taint's kubelet patch is at %s, not among kubeadm's kubelet patches", file.Path)
				}
				return true
			}
		}
		return false
	}

	cp := class.patched(t, "KubeadmControlPlaneTemplate", map[string]any{})
	spec := []string{"spec", "template", "spec", "kubeadmConfigSpec"}
	for _, config := range []string{"initConfiguration", "joinConfiguration"} {
		r := registration(cp, append(slices.Clone(spec), config, "nodeRegistration")...)
		if r.Taints == nil || len(*r.Taints) != 0 {
			t.Errorf("%s: control-plane taints %v, not an empty list that leaves kubeadm's default off", config, r.Taints)
		}
	}
	if !registersWithTaint(field(t, cp, append(slices.Clone(spec), "files")...)) {
		t.Error("a control plane's kubelet does not register it with the control-plane taint")
	}

	worker := class.patched(t, "KubeadmConfigTemplate", map[string]any{})
	if r := registration(worker, "spec", "template", "spec", "joinConfiguration", "nodeRegistration"); r.Taints != nil {
		t.Errorf("worker taints %v without any asked for", *r.Taints)
	}
	if registersWithTaint(field(t, worker, "spec", "template", "spec", "files")) {
		t.Error("a worker's kubelet registers it with the control-plane taint")
	}
}
