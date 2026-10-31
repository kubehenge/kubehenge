package provisioner

import (
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// checkRequestsCPU fails for each path into values whose resources ask for no CPU. A container
// without a request gets the least CPU weight a node gives, and on a node every add-on starting at
// once keeps busy, its liveness probe times out and the kubelet restarts it.
func checkRequestsCPU(t *testing.T, what string, values map[string]any, paths ...[]any) {
	t.Helper()
	for _, path := range paths {
		if lookup[string](t, values, append(slices.Clone(path), "resources", "requests", "cpu")...) == "" {
			t.Errorf("%s: %v requests no CPU", what, path)
		}
	}
}

func TestCertManagerRequestsWhatItRuns(t *testing.T) {
	t.Parallel()
	checkRequestsCPU(t, "cert-manager", certManagerValues(&Certificates{}), []any{}, []any{"webhook"}, []any{"cainjector"})
}

func TestRancherRequestsWhatItRuns(t *testing.T) {
	t.Parallel()
	checkRequestsCPU(t, "Rancher", rancherValues(Rancher{Hostname: "rancher.example.org"}, nil), []any{}, []any{"webhook"}, []any{"fleet"})
}

func TestCSINodePluginRequestsCPU(t *testing.T) {
	t.Parallel()
	_, csi := proxmoxValues("local-lvm", &Addons{})
	checkRequestsCPU(t, "the CSI plugin", csi, []any{"node", "plugin"})
}

func TestCiliumRequestsWhatItRuns(t *testing.T) {
	t.Parallel()
	values, err := ciliumValues(cluster(), 1, &Addons{ClusterMesh: ClusterMesh{Enabled: true}}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	checkRequestsCPU(t, "Cilium", values, []any{}, []any{"envoy"}, []any{"operator"},
		[]any{"clustermesh", "apiserver"}, []any{"clustermesh", "apiserver", "etcd"}, []any{"clustermesh", "apiserver", "kvstoremesh"})
}

func TestKubeVipRequestsCPU(t *testing.T) {
	t.Parallel()
	checkRequestsCPU(t, "kube-vip", kubeVipValues(), []any{})
}

func TestArgoCDRequestsWhatItRuns(t *testing.T) {
	t.Parallel()
	for _, hostname := range []string{"", "argo.example.org"} {
		values, err := argoCDValues(&Certificates{}, &GitOps{Hostname: hostname}, &gitOpsState{})
		if err != nil {
			t.Fatal(err)
		}
		checkRequestsCPU(t, "Argo CD at "+hostname, values,
			[]any{"server"}, []any{controllerKey}, []any{"repoServer"}, []any{"applicationSet"}, []any{"redis"})
	}
}

// checkCalicoRequests fails for each of the operator's pods in spec, by its key there, whose
// container is not named as the operator takes it or asks for no CPU.
func checkCalicoRequests(t *testing.T, what string, spec map[string]any, containers map[string]string) {
	t.Helper()
	for key, name := range containers {
		container := lookup[map[string]any](t, spec, key, "spec", "template", "spec", "containers", 0)
		if container["name"] != name {
			t.Errorf("%s: %s overrides container %v, not %s", what, key, container["name"], name)
		}
		checkRequestsCPU(t, what+" "+key, container, []any{})
	}
}

var calicoInstallationContainers = map[string]string{
	"calicoNodeDaemonSet":             "calico-node",
	"typhaDeployment":                 "calico-typha",
	"calicoKubeControllersDeployment": "calico-kube-controllers",
}

func TestCalicoRequestsWhatItRuns(t *testing.T) {
	t.Parallel()
	_, values := proxySpecs(t, templates.Kubeadm, CNI{Name: cniCalico, Pods: "10.244.0.0/16"}, &Addons{})
	checkRequestsCPU(t, "the Tigera operator", values["web-cni"], []any{})

	s, err := calicoResources(cluster(), CNI{Name: cniCalico, Pods: "10.244.0.0/16"}, testCharts.CalicoImages)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for doc := range strings.SplitSeq(string(s.Data["calico.yaml"]), "---\n") {
		var d map[string]any
		if decodeErr := yaml.Unmarshal([]byte(doc), &d); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		switch d["kind"] {
		case "Installation":
			found["Installation"] = true
			checkCalicoRequests(t, "Calico", lookup[map[string]any](t, d, "spec"), calicoInstallationContainers)
		case "APIServer":
			found["APIServer"] = true
			checkCalicoRequests(t, "Calico's API server", lookup[map[string]any](t, d, "spec"), map[string]string{
				"apiServerDeployment": "calico-apiserver", "calicoWebhooksDeployment": "calico-webhooks",
			})
		}
	}
	if !found["Installation"] || !found["APIServer"] {
		t.Errorf("Calico's resources %v", found)
	}

	s, err = rke2CalicoConfig(cluster())
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(s.Data[rke2CalicoChart+".yaml"], &config); err != nil {
		t.Fatal(err)
	}
	var rke2 map[string]any
	if err := yaml.Unmarshal([]byte(lookup[string](t, config, "spec", "valuesContent")), &rke2); err != nil {
		t.Fatal(err)
	}
	checkRequestsCPU(t, "RKE2's Tigera operator", rke2, []any{})
	checkCalicoRequests(t, "RKE2's Calico", lookup[map[string]any](t, rke2, "installation"), calicoInstallationContainers)
}
