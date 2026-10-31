package provisioner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeJobs runs the Jobs a TokenJob starts as the Job controller and the kubelet would, serving
// each request with serve after checking the pod sees what the Job was given.
type fakeJobs struct {
	kube  client.Client
	serve func(job *batchv1.Job, request []byte) (*TokenJobResult, bool)
}

func newTokenJob(t *testing.T, serve func(job *batchv1.Job, request []byte) (*TokenJobResult, bool)) (*TokenJob, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	tmpl := &corev1.PodTemplate{
		Namespace: "kubehenge-system", Name: GuestJobTemplate,
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "job", Image: "kubehenge", Args: []string{"token-job", "guest-agent"}}},
		}},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tmpl).WithStatusSubresource(&batchv1.Job{}, &corev1.Pod{}).Build()
	jobs := &fakeJobs{kube: kube, serve: serve}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go jobs.run(ctx)
	return &TokenJob{Client: kube, Reader: kube, Namespace: "kubehenge-system", Template: GuestJobTemplate, Poll: 5 * time.Millisecond}, kube
}

func (f *fakeJobs) run(ctx context.Context) {
	done := map[string]bool{}
	for ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
		var jobs batchv1.JobList
		if err := f.kube.List(ctx, &jobs); err != nil {
			continue
		}
		for i := range jobs.Items {
			job := &jobs.Items[i]
			if done[job.Name] {
				continue
			}
			done[job.Name] = true
			input := &corev1.Secret{}
			_ = f.kube.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: InputSecret(job.Labels[TokenJobLabel])}, input)
			result, completes := f.serve(job, input.Data[TokenJobRequestKey])
			pod := &corev1.Pod{Namespace: job.Namespace, Name: job.Name + "-pod", Labels: map[string]string{batchv1.JobNameLabel: job.Name}}
			if err := f.kube.Create(ctx, pod); err != nil {
				panic(err)
			}
			if result != nil {
				raw, err := json.Marshal(result)
				if err != nil {
					panic(err)
				}
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "job", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: string(raw)}}}}
				if err = f.kube.Status().Update(ctx, pod); err != nil {
					panic(err)
				}
			}
			condition := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
			if !completes {
				condition = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline"}
			}
			job.Status.Conditions = append(job.Status.Conditions, condition)
			if err := f.kube.Status().Update(ctx, job); err != nil {
				panic(err)
			}
		}
	}
}

func TestTokenJobRunsTheRequestInAJobOfItsTemplate(t *testing.T) {
	t.Parallel()
	var seen *batchv1.Job
	var asked GuestRequest
	r, kube := newTokenJob(t, func(job *batchv1.Job, request []byte) (*TokenJobResult, bool) {
		seen = job
		_ = json.Unmarshal(request, &asked)
		return &TokenJobResult{Output: "done\n"}, true
	})
	out, err := r.Run(t.Context(), GuestRequest{VMID: 101, Script: "true\n", Timeout: time.Minute}, time.Minute)
	if err != nil || out != "done\n" {
		t.Fatalf("got %q, %v", out, err)
	}
	if asked.VMID != 101 || asked.Script != "true\n" || asked.Timeout != time.Minute {
		t.Errorf("the Job was asked %+v", asked)
	}
	spec := seen.Spec.Template.Spec
	if len(spec.Containers) != 1 || strings.Join(spec.Containers[0].Args, " ") != "token-job guest-agent" ||
		seen.Spec.Template.Labels[TokenJobLabel] != GuestJobTemplate || *seen.Spec.BackoffLimit != 0 ||
		*seen.Spec.ActiveDeadlineSeconds != int64((time.Minute+tokenJobStart)/time.Second) || seen.Spec.TTLSecondsAfterFinished == nil {
		t.Errorf("the Job %+v", seen.Spec)
	}
	// Nothing of the run stays: the Job and the request.
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(seen), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("the Job stayed: %v", err)
	}
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: InputSecret(GuestJobTemplate)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("the request stayed: %v", err)
	}
}

func TestTokenJobReportsHowTheRequestFailed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		result    *TokenJobResult
		completes bool
		want      string
		notFound  bool
	}{
		{"an error", &TokenJobResult{Output: "half", Error: "VM 101: exited 1: no etcd"}, true, "VM 101: exited 1: no etcd", false},
		{"something Proxmox VE did not find", &TokenJobResult{Error: "no such resource 'vm:101'", NotFound: true}, true, "no such resource", true},
		{"a Job that failed without a result", nil, false, "ended without a result: DeadlineExceeded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := newTokenJob(t, func(*batchv1.Job, []byte) (*TokenJobResult, bool) { return tc.result, tc.completes })
			_, err := r.Run(t.Context(), GuestRequest{VMID: 101, Script: "true\n", Timeout: time.Minute}, time.Minute)
			if err == nil || !strings.Contains(err.Error(), tc.want) || proxmox.IsNotFound(err) != tc.notFound {
				t.Errorf("got %v, want %q, not found %t", err, tc.want, tc.notFound)
			}
		})
	}
}

func TestTokenJobReplacesARequestLeftBehind(t *testing.T) {
	t.Parallel()
	r, kube := newTokenJob(t, func(*batchv1.Job, []byte) (*TokenJobResult, bool) { return &TokenJobResult{}, true })
	stale := &corev1.Secret{Namespace: "kubehenge-system", Name: InputSecret(GuestJobTemplate), Data: map[string][]byte{TokenJobNameKey: []byte("old")}}
	if err := kube.Create(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(t.Context(), GuestRequest{VMID: 101, Script: "true\n", Timeout: time.Minute}, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestServeTokenJobAnswersOnlyItsOwnRequest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, content := range map[string]string{TokenJobRequestKey: `{"x":1}`, TokenJobNameKey: "job-a"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result := filepath.Join(dir, "result")
	ran := 0
	run := func(_ context.Context, request []byte) (string, error) {
		ran++
		if string(request) != `{"x":1}` {
			t.Errorf("request %s", request)
		}
		return strings.Repeat("x", 5000), proxmox.ErrNotFound
	}
	read := func() TokenJobResult {
		t.Helper()
		raw, err := os.ReadFile(result) //nolint:gosec // the test's own file
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 4096 {
			t.Errorf("a result of %d bytes, more than a termination message holds", len(raw))
		}
		var r TokenJobResult
		if err = json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, job := range []string{"job-b", ""} {
		if err := ServeTokenJob(t.Context(), dir, job, result, run); err != nil {
			t.Fatal(err)
		}
		if r := read(); r.Error == "" || ran != 0 {
			t.Errorf("Job %q ran another's request: %+v", job, r)
		}
	}
	if err := ServeTokenJob(t.Context(), dir, "job-a", result, run); err != nil {
		t.Fatal(err)
	}
	if r := read(); ran != 1 || !r.NotFound || !strings.HasSuffix(r.Output, "xxx") || len(r.Output) > tokenJobOutput+3 {
		t.Errorf("result %+v", r)
	}
	if err := ServeTokenJob(t.Context(), t.TempDir(), "job-a", result, run); err != nil {
		t.Fatal(err)
	}
	if r := read(); !strings.Contains(r.Error, "no request") {
		t.Errorf("without a request: %+v", r)
	}
}

func TestServeGuestRequestRefusesWhatNoRestoreAsks(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"vmid":0,"script":"true","timeout":60000000000}`, `{"vmid":101,"script":"true","timeout":0}`, `{"vmid":101,"script":"true","timeout":3600000000000}`, `not json`} {
		if _, err := ServeGuestRequest(t.Context(), proxmox.NewClient("http://127.0.0.1:1"), []byte(raw)); err == nil || !strings.Contains(err.Error(), "the request") {
			t.Errorf("%s: %v", raw, err)
		}
	}
}
