package provisioner

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// renderedJob is the part of a rendered document the hook tests read.
type renderedJob struct {
	Kind string          `json:"kind"`
	Spec renderedJobSpec `json:"spec"`
}

type renderedJobSpec struct {
	Template corev1.PodTemplateSpec `json:"template"`
}

// hookPods renders the providers chart with the given values and returns the pod spec of each hook
// Job that runs a kubehenge subcommand, by container name.
func hookPods(t *testing.T, set ...string) map[string]*corev1.PodSpec {
	t.Helper()
	args := []string{
		"template", "x", "../../management-cluster/charts/cluster-api-providers",
		"--values", "testdata/cluster-api-providers-values.yaml",
		"--show-only", "templates/manifests.yaml", "--show-only", "templates/wait.yaml",
	}
	if len(set) > 0 {
		args = append(args, "--set", strings.Join(set, ","))
	}
	cmd := exec.CommandContext(t.Context(), "helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	pods := map[string]*corev1.PodSpec{}
	for doc := range strings.SplitSeq(string(out), "\n---\n") {
		var job renderedJob
		if err := yaml.Unmarshal([]byte(doc), &job); err != nil {
			t.Fatalf("%v:\n%s", err, doc)
		}
		if job.Kind == "Job" {
			pod := &job.Spec.Template.Spec
			for i := range pod.Containers {
				pods[pod.Containers[i].Name] = pod
			}
		}
	}
	if len(pods) == 0 {
		t.Fatal("the chart renders no hook Job")
	}
	return pods
}

// authorityMounted reports whether the pod mounts the authority file from the named Secret and has
// SSL_CERT_FILE name it.
func authorityMounted(pod *corev1.PodSpec, secret string) bool {
	c := pod.Containers[0]
	named := slices.ContainsFunc(c.Env, func(e corev1.EnvVar) bool {
		return e.Name == "SSL_CERT_FILE" && e.Value == "/etc/kubehenge/proxy/ca.crt"
	})
	mounted := slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == "proxy-ca" && m.MountPath == "/etc/kubehenge/proxy"
	})
	volume := slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool {
		return v.Name == "proxy-ca" && v.Secret != nil && v.Secret.SecretName == secret
	})
	return named && mounted && volume
}

func TestHookJobsTrustTheAuthoritiesWithoutAProxy(t *testing.T) {
	t.Parallel()
	for name, pod := range hookPods(t, "proxy.ca=true", "proxy.caSecret=kubehenge-proxy") {
		if !authorityMounted(pod, "kubehenge-proxy") {
			t.Errorf("%s does not trust the authority file from the Secret", name)
		}
		if len(pod.Containers[0].EnvFrom) != 0 {
			t.Errorf("%s takes proxy variables with no proxy: %+v", name, pod.Containers[0].EnvFrom)
		}
	}
}

func TestHookJobsTrustTheAuthoritiesBesideAProxy(t *testing.T) {
	t.Parallel()
	for name, pod := range hookPods(t, "proxy.ca=true", "proxy.secret=kubehenge-proxy") {
		if !authorityMounted(pod, "kubehenge-proxy") {
			t.Errorf("%s does not trust the authority file from the Secret", name)
		}
		if envFrom := pod.Containers[0].EnvFrom; len(envFrom) != 1 || envFrom[0].SecretRef == nil || envFrom[0].SecretRef.Name != "kubehenge-proxy" {
			t.Errorf("%s: envFrom = %+v, want the proxy Secret", name, envFrom)
		}
	}
}

func TestHookJobsMountNothingWithoutAuthorities(t *testing.T) {
	t.Parallel()
	for name, pod := range hookPods(t, "proxy.secret=kubehenge-proxy") {
		if len(pod.Volumes) != 0 || len(pod.Containers[0].Env) != 0 || len(pod.Containers[0].VolumeMounts) != 0 {
			t.Errorf("%s mounts or sets an authority without one: %+v", name, pod)
		}
	}
	for name, pod := range hookPods(t) {
		if len(pod.Volumes) != 0 || len(pod.Containers[0].EnvFrom) != 0 || len(pod.Containers[0].Env) != 0 {
			t.Errorf("%s gets proxy settings without a proxy: %+v", name, pod)
		}
	}
}
