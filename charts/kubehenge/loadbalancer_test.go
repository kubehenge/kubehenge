package kubehenge_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

type fieldRef struct {
	FieldPath string `json:"fieldPath"`
}

type envSource struct {
	FieldRef fieldRef `json:"fieldRef"`
}

type podEnvVar struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	ValueFrom envSource `json:"valueFrom"`
}

type staticPod struct {
	Spec podSpec `json:"spec"`
}

// kubeVipEnv renders a distribution's ClusterClass with Helm, finds the patch that writes the
// control planes' kube-vip static pod, renders it as Cluster API does for these variables and
// returns the pod's environment by name.
func kubeVipEnv(t *testing.T, file string, variables map[string]any) map[string]podEnvVar {
	t.Helper()
	return podEnv(t, kubeVipFile(t, file, variables))
}

// kubeVipFile is the bootstrap file the class writes kube-vip's static pod with, as Cluster API
// renders it for these variables.
func kubeVipFile(t *testing.T, file string, variables map[string]any) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system", //nolint:gosec // helm with the chart's own file names
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/"+file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var head renderedDocument
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatal(err)
		}
		for _, p := range head.Spec.Patches {
			for _, d := range p.Definitions {
				for _, j := range d.JSONPatches {
					if j.ValueFrom == nil || !strings.Contains(j.ValueFrom.Template, "name: kube-vip") {
						continue
					}
					return capiTemplate(t, j.ValueFrom.Template, variables)
				}
			}
		}
	}
	t.Fatalf("%s: no patch writes kube-vip", file)
	return ""
}

func podEnv(t *testing.T, rendered string) map[string]podEnvVar {
	t.Helper()
	var file struct {
		Content string `json:"content"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &file); err != nil {
		t.Fatalf("%v:\n%s", err, rendered)
	}
	var pod staticPod
	if err := yaml.Unmarshal([]byte(file.Content), &pod); err != nil {
		t.Fatalf("%v:\n%s", err, file.Content)
	}
	env := map[string]podEnvVar{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	return env
}

// The pinned MetalLB chart asks for nothing for frr-k8s, so Kubehenge sets the requests and the
// class's pooled figures for the load balancer grow by their sum in BGP mode.
func TestClassFiguresCountFRRK8sRequests(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml", "--show-only", "templates/kubeadm.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	cpu, memory := provisioner.BGPBackendRequests()
	for annotation, want := range map[string]int{"kubehenge.dev/addon-cpu": cpu, "kubehenge.dev/addon-memory": memory} {
		var figures map[string]int
		for line := range strings.SplitSeq(string(out), "\n") {
			if raw, found := strings.CutPrefix(strings.TrimSpace(line), annotation+": '"); found {
				if err := json.Unmarshal([]byte(strings.TrimSuffix(raw, "'")), &figures); err != nil {
					t.Fatal(err)
				}
			}
		}
		if figures["loadBalancerBGP"] != want {
			t.Errorf("%s: loadBalancerBGP = %d, want the %d that frr-k8s requests", annotation, figures["loadBalancerBGP"], want)
		}
	}
}

type secretKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type contentSource struct {
	Secret secretKey `json:"secret"`
}

type kubeVipBootstrapFile struct {
	Path        string        `json:"path"`
	Content     string        `json:"content"`
	ContentFrom contentSource `json:"contentFrom"`
}

// A password cannot go into a ClusterClass, so with sessions that carry one the class writes the
// file from the Secret Kubehenge's controller makes, and with none it writes the pod inline.
func TestSignedBGPSessionsTakeTheirKubeVipFromASecret(t *testing.T) {
	t.Parallel()
	peers := []any{map[string]any{"address": "10.0.0.1", "asn": float64(65000)}}
	variables := func(authenticated any) map[string]any {
		bgp := map[string]any{"asn": float64(65001), "peers": peers}
		if authenticated != nil {
			bgp["authenticated"] = authenticated
		}
		return map[string]any{
			"controlPlaneEndpoint": "10.0.0.5",
			"builtin":              map[string]any{"cluster": map[string]any{"name": "web"}},
			"loadBalancer":         map[string]any{"enabled": true, "mode": "bgp", "bgp": bgp},
		}
	}
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			var signed kubeVipBootstrapFile
			rendered := kubeVipFile(t, file, variables(true))
			if err := yaml.Unmarshal([]byte(rendered), &signed); err != nil {
				t.Fatalf("%v:\n%s", err, rendered)
			}
			if signed.Content != "" || signed.ContentFrom.Secret.Name != "web-kube-vip" || signed.ContentFrom.Secret.Key != "kube-vip.yaml" ||
				!strings.HasSuffix(signed.Path, "/kube-vip.yaml") {
				t.Errorf("signed: %+v", signed)
			}
			for name, v := range map[string]map[string]any{"unsigned": variables(false), "unset": variables(nil)} {
				var inline kubeVipBootstrapFile
				rendered := kubeVipFile(t, file, v)
				if err := yaml.Unmarshal([]byte(rendered), &inline); err != nil {
					t.Fatalf("%s: %v:\n%s", name, err, rendered)
				}
				if inline.Content == "" || inline.ContentFrom.Secret.Name != "" || inline.Path != signed.Path {
					t.Errorf("%s: %+v", name, inline)
				}
			}
		})
	}
}

func TestControlPlaneAddressIsAnnouncedOverBGPOnEveryDistribution(t *testing.T) {
	t.Parallel()
	bgp := map[string]any{
		"controlPlaneEndpoint": "10.0.0.5",
		"loadBalancer": map[string]any{
			"enabled": true, "mode": "bgp",
			"bgp": map[string]any{"asn": float64(4_200_000_001), "peers": []any{
				map[string]any{"address": "10.0.0.1", "asn": float64(65000)},
				map[string]any{"address": "10.0.0.2", "asn": float64(65001)},
			}},
		},
	}
	l2 := map[string]any{"controlPlaneEndpoint": "10.0.0.5", "loadBalancer": map[string]any{"enabled": true, "mode": "l2"}}
	unset := map[string]any{"controlPlaneEndpoint": "10.0.0.5"}
	for _, file := range []string{"kubeadm.yaml", "rke2.yaml", "k3s.yaml"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			env := kubeVipEnv(t, file, bgp)
			for name, want := range map[string]string{
				"vip_arp": "false", "bgp_enable": "true", "bgp_as": "4200000001",
				"bgp_peers": "10.0.0.1:65000::false,10.0.0.2:65001::false", "address": "10.0.0.5",
			} {
				if env[name].Value != want {
					t.Errorf("bgp: %s = %q, want %q", name, env[name].Value, want)
				}
			}
			if env["bgp_routerid"].ValueFrom.FieldRef.FieldPath != "status.hostIP" {
				t.Errorf("bgp: router id = %+v, want the node's address", env["bgp_routerid"])
			}
			for name, variables := range map[string]map[string]any{"layer 2": l2, "unset": unset} {
				env := kubeVipEnv(t, file, variables)
				if env["vip_arp"].Value != "true" {
					t.Errorf("%s: vip_arp = %q, want true", name, env["vip_arp"].Value)
				}
				for key := range env {
					if strings.HasPrefix(key, "bgp_") {
						t.Errorf("%s: kube-vip got %s", name, key)
					}
				}
			}
		})
	}
}
