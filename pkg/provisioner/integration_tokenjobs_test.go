//go:build integration

package provisioner

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestIntegrationTokenJobsHoldTheTokens holds the controller to the chart's token Jobs: it starts
// them as their PodTemplates say, with the defaults the API server gives both, and no Job that gets
// a token any other way.
func TestIntegrationTokenJobsHoldTheTokens(t *testing.T) { //nolint:paralleltest // starts an API server and ends every child process of the binary after it
	cfg, _ := startAPIConfig(t, "testdata/proxmox-ha-values.yaml", "testdata/etcd-restore-values.yaml")
	ctx := context.Background()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + classNamespace + ":kubehenge-controller",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + classNamespace, "system:authenticated"},
	}
	controller, err := client.New(impersonated, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	for _, template := range []string{HAJobTemplate, GuestJobTemplate} {
		tmpl := &corev1.PodTemplate{}
		if err = controller.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: template}, tmpl); err != nil {
			t.Fatalf("the controller cannot read PodTemplate %s: %v", template, err)
		}
		input := &corev1.Secret{Namespace: classNamespace, Name: InputSecret(template), Data: map[string][]byte{TokenJobRequestKey: []byte("{}")}}
		if err = controller.Create(ctx, input); err != nil {
			t.Fatalf("the controller cannot leave a request: %v", err)
		}
		// The policy takes a moment to reach the API server's admission after the chart is applied:
		// until it refuses a Job with another command, none of this tells anything.
		for n, deadline := 0, time.Now().Add(time.Minute); ; n++ {
			probe := TokenJobFrom(tmpl, template+"-probe-"+strconv.Itoa(n), time.Minute)
			probe.Spec.Template.Spec.Containers[0].Args = []string{"controller"}
			if err = controller.Create(ctx, probe); err != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the policy never refused a Job of %s with another command", template)
			}
			time.Sleep(200 * time.Millisecond)
		}
		job := TokenJobFrom(tmpl, template+"-a", time.Minute)
		if err = controller.Create(ctx, job); err != nil {
			t.Fatalf("the controller cannot start a Job of %s: %v", template, err)
		}
		var pods corev1.PodList
		if err = controller.List(ctx, &pods, client.InNamespace(classNamespace), client.MatchingLabels{batchv1.JobNameLabel: job.Name}); err != nil {
			t.Errorf("the controller cannot read its Job's result: %v", err)
		}
		if err = controller.Delete(ctx, &batchv1.Job{Namespace: classNamespace, Name: job.Name}); err != nil {
			t.Errorf("the controller cannot remove its Job: %v", err)
		}

		token := tmpl.Annotations["kubehenge.dev/token-secret"]
		for i, tc := range []struct {
			name   string
			change func(spec *corev1.PodSpec)
		}{
			{"another command", func(spec *corev1.PodSpec) { spec.Containers[0].Args = []string{"controller"} }},
			{"another image", func(spec *corev1.PodSpec) {
				spec.Containers[0].Image = "registry.example.org/shell@sha256:" + strings.Repeat("0", 64)
			}},
			{"a second container", func(spec *corev1.PodSpec) {
				spec.Containers = append(spec.Containers, *spec.Containers[0].DeepCopy())
				spec.Containers[1].Name = "second"
			}},
			{"the token in a volume", func(spec *corev1.PodSpec) {
				spec.Containers[0].Env = nil
				spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "token", Secret: &corev1.SecretVolumeSource{SecretName: token}})
			}},
			{"the token in a projected volume", func(spec *corev1.PodSpec) {
				spec.Containers[0].Env = nil
				spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "token", Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{Name: token}}},
				}})
			}},
			{"the token's whole Secret in the environment", func(spec *corev1.PodSpec) {
				spec.Containers[0].Env = nil
				spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{Name: token}}}
			}},
			{"an init container with the token", func(spec *corev1.PodSpec) {
				spec.InitContainers = []corev1.Container{*spec.Containers[0].DeepCopy()}
				spec.InitContainers[0].Name = "first"
				spec.InitContainers[0].Args = []string{"controller"}
				spec.Containers[0].Env = nil
			}},
		} {
			changed := TokenJobFrom(tmpl, template+"-changed-"+strconv.Itoa(i), time.Minute)
			tc.change(&changed.Spec.Template.Spec)
			err = controller.Create(ctx, changed)
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "only a Job made exactly from PodTemplate "+template) {
				t.Errorf("%s: a Job with %s: %v, want refused", template, tc.name, err)
			}
		}
	}

	// A Job that gets no token is no concern of the policy's: the template refresh's runs are such.
	plain := &batchv1.Job{Namespace: classNamespace, Name: "plain", Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers:    []corev1.Container{{Name: "c", Image: "registry.example.org/c@sha256:" + strings.Repeat("0", 64)}},
	}}}}
	if err = controller.Create(ctx, plain); err != nil {
		t.Errorf("a Job without a token: %v", err)
	}
}
