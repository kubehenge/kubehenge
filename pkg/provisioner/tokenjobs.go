package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/luthermonson/go-proxmox"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The PodTemplates of the Jobs that hold a Proxmox VE token the controller does not: one each for
// Proxmox VE HA and for the guest agent's commands of etcd restores.
const (
	HAJobTemplate    = "kubehenge-proxmox-ha"
	GuestJobTemplate = "kubehenge-guest-agent"
)

// TokenJobLabel names, on a token Job and its pod, the PodTemplate the Job was made from.
const TokenJobLabel = "kubehenge.dev/token-job" //nolint:gosec // a label's name, not a credential

// The token Job's input Secret holds the request and the name of the Job it is for.
const (
	TokenJobRequestKey = "request"
	TokenJobNameKey    = "job"
)

// tokenJobResultMode is the result file's, the pod's termination message, which the kubelet made.
const tokenJobResultMode = 0o600

const (
	// tokenJobStart is what a token Job may take beyond its request's own time: being scheduled,
	// starting its container and finishing.
	tokenJobStart = 5 * time.Minute
	// tokenJobTTL keeps a finished Job only until the controller has read it; it deletes the Job
	// itself, and this removes one a restart of the controller left.
	tokenJobTTL = 10 * time.Minute
	// tokenJobPoll is how often a running Job is looked at.
	tokenJobPoll = time.Second
	// The pod's termination message holds at most 4096 bytes, the result's JSON included.
	tokenJobOutput = 1500
	tokenJobError  = 2000
)

// TokenJob runs a request with a Proxmox VE token that no long-running pod holds: as a Job made from
// the PodTemplate Template, whose pod alone mounts the token, and to which the API server holds
// every Job the controller makes that mounts it. The request reaches the Job in the Secret
// <Template>-input, so that nothing it carries shows in the Job or its pod, and the result comes
// back as the pod's termination message. One request of a template runs at a time.
type TokenJob struct {
	Client    client.Client
	Reader    client.Reader
	Namespace string
	Template  string
	// Poll is how often a running Job is looked at; tokenJobPoll unless set.
	Poll time.Duration
}

// TokenJobResult is what a token Job reports: what its request printed, and how it failed.
type TokenJobResult struct {
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	NotFound bool   `json:"notFound,omitempty"`
}

// InputSecret names the Secret through which the template's Jobs get their requests.
func InputSecret(template string) string { return template + "-input" }

// Run runs request in a Job and returns what it printed. A request that failed returns its error,
// one Proxmox VE did not find wrapping proxmox.ErrNotFound; timeout bounds the request itself.
func (j *TokenJob) Run(ctx context.Context, request any, timeout time.Duration) (string, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("the request: %w", err)
	}
	tmpl := &corev1.PodTemplate{}
	if err = j.Reader.Get(ctx, client.ObjectKey{Namespace: j.Namespace, Name: j.Template}, tmpl); err != nil {
		return "", fmt.Errorf("reading PodTemplate %s: %w", j.Template, err)
	}
	name := j.Template + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err = j.putInput(ctx, name, raw); err != nil {
		return "", err
	}
	defer j.cleanUp(context.WithoutCancel(ctx), name)
	if err = j.Client.Create(ctx, TokenJobFrom(tmpl, name, timeout)); err != nil {
		return "", fmt.Errorf("starting Job %s: %w", name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+tokenJobStart+time.Minute)
	defer cancel()
	finished, err := j.wait(ctx, name)
	if err != nil {
		return "", err
	}
	result, found, err := j.result(ctx, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("token Job %s ended without a result: %s", name, jobFailure(finished))
	}
	return result.Outcome()
}

// Outcome is what the request printed, and its error, one Proxmox VE did not find wrapping
// proxmox.ErrNotFound as it did in the Job.
func (r TokenJobResult) Outcome() (string, error) {
	switch {
	case r.Error == "":
		return r.Output, nil
	case r.NotFound:
		return r.Output, fmt.Errorf("%w: %s", proxmox.ErrNotFound, r.Error)
	default:
		return r.Output, errors.New(r.Error)
	}
}

// TokenRunner runs a request where the token it needs is, as TokenJob does.
type TokenRunner interface {
	Run(ctx context.Context, request any, timeout time.Duration) (string, error)
}

var _ TokenRunner = &TokenJob{}

// TokenJobFrom is the Job name of the PodTemplate, whose pod runs exactly as the PodTemplate says:
// the API server lets no other Job have the token.
func TokenJobFrom(tmpl *corev1.PodTemplate, name string, timeout time.Duration) *batchv1.Job {
	labels := maps.Clone(tmpl.Template.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[TokenJobLabel] = tmpl.Name
	deadline := int64((timeout + tokenJobStart) / time.Second)
	job := &batchv1.Job{
		Namespace: tmpl.Namespace,
		Name:      name,
		Labels:    map[string]string{TokenJobLabel: tmpl.Name},
		Spec: batchv1.JobSpec{
			// A request is not run twice: a second try could repeat what the first did.
			BackoffLimit:            new(int32(0)),
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: new(int32(tokenJobTTL / time.Second)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: tmpl.Template.ObjectMeta,
				Spec:       *tmpl.Template.Spec.DeepCopy(),
			},
		},
	}
	job.Spec.Template.Labels = labels
	return job
}

// putInput leaves the request for the Job, replacing what an earlier one left.
func (j *TokenJob) putInput(ctx context.Context, job string, request []byte) error {
	data := map[string][]byte{TokenJobRequestKey: request, TokenJobNameKey: []byte(job)}
	s := &corev1.Secret{Namespace: j.Namespace, Name: InputSecret(j.Template), Type: corev1.SecretTypeOpaque, Data: data}
	err := j.Client.Create(ctx, s)
	if !apierrors.IsAlreadyExists(err) {
		if err != nil {
			return fmt.Errorf("writing Secret %s: %w", s.Name, err)
		}
		return nil
	}
	if err = j.Reader.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil {
		return fmt.Errorf("reading Secret %s: %w", s.Name, err)
	}
	s.Data = data
	if err = j.Client.Update(ctx, s); err != nil {
		return fmt.Errorf("writing Secret %s: %w", s.Name, err)
	}
	return nil
}

// cleanUp removes the Job, its pod and the request.
func (j *TokenJob) cleanUp(ctx context.Context, name string) {
	job := &batchv1.Job{Namespace: j.Namespace, Name: name}
	_ = client.IgnoreNotFound(j.Client.Delete(ctx, job, client.PropagationPolicy("Background")))
	input := &corev1.Secret{Namespace: j.Namespace, Name: InputSecret(j.Template)}
	_ = client.IgnoreNotFound(j.Client.Delete(ctx, input))
}

// wait returns the Job once it has finished.
func (j *TokenJob) wait(ctx context.Context, name string) (*batchv1.Job, error) {
	poll := j.Poll
	if poll == 0 {
		poll = tokenJobPoll
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		job := &batchv1.Job{}
		if err := j.Reader.Get(ctx, client.ObjectKey{Namespace: j.Namespace, Name: name}, job); err != nil {
			return nil, fmt.Errorf("reading Job %s: %w", name, err)
		}
		if jobFinished(job) {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for Job %s: %w", name, ctx.Err())
		case <-t.C:
		}
	}
}

// result reads the result the Job's pod left as its termination message.
func (j *TokenJob) result(ctx context.Context, name string) (TokenJobResult, bool, error) {
	var pods corev1.PodList
	err := j.Reader.List(ctx, &pods, client.InNamespace(j.Namespace), client.MatchingLabels{batchv1.JobNameLabel: name})
	if err != nil {
		return TokenJobResult{}, false, fmt.Errorf("listing the pods of Job %s: %w", name, err)
	}
	for i := range pods.Items {
		for j := range pods.Items[i].Status.ContainerStatuses {
			done := pods.Items[i].Status.ContainerStatuses[j].State.Terminated
			if done == nil || done.Message == "" {
				continue
			}
			var r TokenJobResult
			if err := json.Unmarshal([]byte(done.Message), &r); err != nil {
				return TokenJobResult{}, false, fmt.Errorf("the result of Job %s: %w", name, err)
			}
			return r, true, nil
		}
	}
	return TokenJobResult{}, false, nil
}

func jobFailure(job *batchv1.Job) string {
	i := slices.IndexFunc(job.Status.Conditions, func(c batchv1.JobCondition) bool {
		return c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue
	})
	if i < 0 {
		return "it completed"
	}
	return job.Status.Conditions[i].Reason + ": " + job.Status.Conditions[i].Message
}

// ServeTokenJob runs the request a TokenJob left in dir for the Job named job, and writes the
// result to resultPath, the pod's termination message. A request for another Job is refused: the
// controller may have moved on from this one.
func ServeTokenJob(ctx context.Context, dir, job, resultPath string, run func(context.Context, []byte) (string, error)) error {
	var out string
	err := errors.New("this pod runs no Job: the downward API names none")
	if job != "" {
		out, err = runTokenJobRequest(ctx, dir, job, run)
	}
	r := TokenJobResult{Output: tail(out, tokenJobOutput)}
	if err != nil {
		r.Error, r.NotFound = tail(err.Error(), tokenJobError), proxmox.IsNotFound(err)
	}
	raw, merr := json.Marshal(r)
	if merr != nil {
		return fmt.Errorf("the result: %w", merr)
	}
	if werr := os.WriteFile(resultPath, raw, tokenJobResultMode); werr != nil {
		return fmt.Errorf("writing the result: %w", werr)
	}
	return nil
}

func runTokenJobRequest(ctx context.Context, dir, job string, run func(context.Context, []byte) (string, error)) (string, error) {
	for _, f := range []string{TokenJobNameKey, TokenJobRequestKey} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return "", fmt.Errorf("no request: %w", err)
		}
	}
	forJob, err := os.ReadFile(filepath.Join(dir, TokenJobNameKey)) //nolint:gosec // the pod's own Secret volume
	if err != nil {
		return "", fmt.Errorf("reading the request: %w", err)
	}
	if string(forJob) != job {
		return "", fmt.Errorf("the request is for Job %s, not %s", forJob, job)
	}
	request, err := os.ReadFile(filepath.Join(dir, TokenJobRequestKey)) //nolint:gosec // the pod's own Secret volume
	if err != nil {
		return "", fmt.Errorf("reading the request: %w", err)
	}
	return run(ctx, request)
}
