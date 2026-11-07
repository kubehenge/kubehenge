package kubehenge_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type secretRef struct {
	Name string `json:"name"`
}

type envFromSource struct {
	SecretRef secretRef `json:"secretRef"`
}

type podEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type volumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type container struct {
	Name         string          `json:"name"`
	EnvFrom      []envFromSource `json:"envFrom"`
	Env          []podEnvVar     `json:"env"`
	VolumeMounts []volumeMount   `json:"volumeMounts"`
}

type secretVolume struct {
	SecretName string `json:"secretName"`
}

type volume struct {
	Name   string       `json:"name"`
	Secret secretVolume `json:"secret"`
}

// podSpec is the part of a controller Deployment or a template refresh CronJob the proxy setting
// reaches.
type podSpec struct {
	Containers []container `json:"containers"`
	Volumes    []volume    `json:"volumes"`
}

type podTemplate struct {
	Spec podSpec `json:"spec"`
}

type jobSpec struct {
	Template podTemplate `json:"template"`
}

type jobTemplate struct {
	Spec jobSpec `json:"spec"`
}

type workloadSpec struct {
	Template    podTemplate `json:"template"`
	JobTemplate jobTemplate `json:"jobTemplate"`
}

type workload struct {
	Kind string       `json:"kind"`
	Spec workloadSpec `json:"spec"`
}

// proxyPods renders the chart and returns the pod specs of the controller and the template refresh
// Job, by kind.
func proxyPods(t *testing.T, set ...string) map[string]podSpec {
	t.Helper()
	args := make([]string, 0, 11+2*len(set))
	args = append(args,
		"template", "kubehenge", ".", "--namespace", "kubehenge-system",
		"--values", "../../pkg/provisioner/testdata/integration-values.yaml",
		"--show-only", "templates/controller.yaml", "--show-only", "templates/templaterefresh.yaml",
	)
	for _, s := range set {
		args = append(args, "--set", s)
	}
	cmd := exec.CommandContext(t.Context(), "helm", args...) //nolint:gosec // helm with the test's own arguments
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	pods := map[string]podSpec{}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var obj workload
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("%v:\n%s", err, doc)
		}
		switch obj.Kind {
		case "Deployment":
			pods["controller"] = obj.Spec.Template.Spec
		case "CronJob":
			pods["templaterefresh"] = obj.Spec.JobTemplate.Spec.Template.Spec
		}
	}
	for _, kind := range []string{"controller", "templaterefresh"} {
		if len(pods[kind].Containers) == 0 {
			t.Fatalf("no %s pod in the rendered chart", kind)
		}
	}
	return pods
}

func TestProxyReachesControllerAndTemplateRefresh(t *testing.T) {
	t.Parallel()
	for kind, pod := range proxyPods(t, "proxy.secret=kubehenge-proxy", "proxy.ca=true") {
		c := pod.Containers[0]
		if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != "kubehenge-proxy" {
			t.Errorf("%s: envFrom = %+v, want the proxy Secret", kind, c.EnvFrom)
		}
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		if env["SSL_CERT_FILE"] != "/etc/kubehenge/proxy/ca.crt" || env["KUBEHENGE_PROXY_CA_FILE"] != "/etc/kubehenge/proxy/ca.crt" {
			t.Errorf("%s: env = %v, want the CA file named", kind, env)
		}
		mounted := false
		for _, m := range c.VolumeMounts {
			mounted = mounted || (m.Name == "proxy-ca" && m.MountPath == "/etc/kubehenge/proxy")
		}
		hasVolume := false
		for _, v := range pod.Volumes {
			hasVolume = hasVolume || (v.Name == "proxy-ca" && v.Secret.SecretName == "kubehenge-proxy")
		}
		if !mounted || !hasVolume {
			t.Errorf("%s: CA mounted = %v, volume = %v", kind, mounted, hasVolume)
		}
	}
}

func TestProxyWithoutCAMountsNothing(t *testing.T) {
	t.Parallel()
	for kind, pod := range proxyPods(t, "proxy.secret=kubehenge-proxy") {
		c := pod.Containers[0]
		if len(c.EnvFrom) != 1 {
			t.Errorf("%s: envFrom = %+v, want the proxy Secret", kind, c.EnvFrom)
		}
		for _, e := range c.Env {
			if e.Name == "SSL_CERT_FILE" {
				t.Errorf("%s sets SSL_CERT_FILE without a CA", kind)
			}
		}
		for _, v := range pod.Volumes {
			if v.Name == "proxy-ca" {
				t.Errorf("%s mounts a CA volume without a CA", kind)
			}
		}
	}
}

func TestNoProxyLeavesThePodsAsTheyWere(t *testing.T) {
	t.Parallel()
	for kind, pod := range proxyPods(t) {
		c := pod.Containers[0]
		if len(c.EnvFrom) != 0 {
			t.Errorf("%s: envFrom = %+v, want none", kind, c.EnvFrom)
		}
		for _, e := range c.Env {
			if e.Name == "SSL_CERT_FILE" || e.Name == "KUBEHENGE_PROXY_CA_FILE" {
				t.Errorf("%s sets %s without a proxy", kind, e.Name)
			}
		}
	}
}
