package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// trueText and falseText are how annotations, labels, Secret data and environment variables spell a
// boolean.
const trueText, falseText = "true", "false"

const (
	// TemplateRefreshName names the CronJob that builds the templates again, its ServiceAccount, and
	// the ConfigMap that holds its schedule and how its last run went.
	TemplateRefreshName = "kubehenge-template-refresh"
	// TemplateRequestsName is the ConfigMap the interface asks for builds in: a key per request,
	// RefreshRequest or a line (CloudImageLine, KubeadmLinePrefix and a version), whose value is a
	// TemplateRequest.
	TemplateRequestsName = "kubehenge-template-requests"
	// TemplateRefreshLabel marks the refresh's Jobs, whoever started them.
	TemplateRefreshLabel = "kubehenge.dev/template-refresh"

	// RefreshRequest asks for a refresh of every line now, as the schedule would.
	RefreshRequest = "refresh"
	// CloudImageLine asks for the template RKE2 and k3s share, if it has no build.
	CloudImageLine = "cloud-image"
	// KubeadmLinePrefix and a Kubernetes version ask for that version's kubeadm template, if it
	// has no build.
	KubeadmLinePrefix = "kubeadm-"
)

// The keys of TemplateRefreshName's ConfigMap. The chart writes the schedule's, the refresh its
// run's.
const (
	RefreshWeekday       = "weekday"
	RefreshTime          = "time"
	RefreshTimeZone      = "timeZone"
	RefreshSoakHours     = "soakHours"
	RefreshCreatorBuilds = "creatorBuilds"
	RefreshState         = "state"
	RefreshStarted       = "started"
	RefreshFinished      = "finished"
	RefreshTrigger       = "trigger"
	RefreshLines         = "lines"
	RefreshError         = "error"
	RefreshLog           = "log"
)

// RefreshSchedule is when the CronJob builds the templates again, as its ConfigMap records it.
type RefreshSchedule struct {
	Weekday  string
	Time     string
	TimeZone string
	Soak     time.Duration
	// CreatorBuilds lets those who may create clusters ask for a missing template.
	CreatorBuilds bool
}

// ParseRefreshSchedule reads the schedule from the refresh's ConfigMap.
func ParseRefreshSchedule(data map[string]string) (RefreshSchedule, error) {
	hours, err := strconv.Atoi(data[RefreshSoakHours])
	if err != nil || hours < 0 {
		return RefreshSchedule{}, fmt.Errorf("%s: soakHours %q is no number of hours", TemplateRefreshName, data[RefreshSoakHours])
	}
	s := RefreshSchedule{
		Weekday: data[RefreshWeekday], Time: data[RefreshTime], TimeZone: data[RefreshTimeZone],
		Soak: time.Duration(hours) * time.Hour, CreatorBuilds: data[RefreshCreatorBuilds] == trueText,
	}
	if _, err := s.Next(time.Now()); err != nil {
		return RefreshSchedule{}, err
	}
	return s, nil
}

// Next is when the next scheduled run starts after from.
func (s *RefreshSchedule) Next(from time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(cmpOr(s.TimeZone, "UTC"))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: time zone: %w", TemplateRefreshName, err)
	}
	hm, err := time.Parse("15:04", s.Time)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: time: %w", TemplateRefreshName, err)
	}
	t := from.In(loc)
	for ahead := range 8 {
		day := t.AddDate(0, 0, ahead)
		starts := time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
		if day.Weekday().String()[:3] == s.Weekday && starts.After(from) {
			return starts, nil
		}
	}
	return time.Time{}, fmt.Errorf("%s: no weekday %q", TemplateRefreshName, s.Weekday)
}

// TemplateRequest is one request for a build: who asked, and when.
type TemplateRequest struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// RequestLine is the request key of a missing line, or "" for none Kubehenge builds.
func RequestLine(req templates.Request) string {
	switch req.Distribution {
	case templates.RKE2, templates.K3s:
		return CloudImageLine
	case templates.Kubeadm:
		return KubeadmLinePrefix + req.KubernetesVersion
	case templates.Talos:
	}
	return ""
}

// ParseRequestLine is the template request a line key names.
func ParseRequestLine(key string) (templates.Request, error) {
	var req templates.Request
	switch {
	case key == CloudImageLine:
		req = templates.Request{Distribution: templates.RKE2}
	case strings.HasPrefix(key, KubeadmLinePrefix):
		req = templates.Request{Distribution: templates.Kubeadm, KubernetesVersion: strings.TrimPrefix(key, KubeadmLinePrefix)}
	default:
		return req, fmt.Errorf("no template line %q", key)
	}
	if _, err := templates.Plan(req); err != nil {
		return req, fmt.Errorf("template line %q: %w", key, err)
	}
	return req, nil
}

// TemplateRequests starts a run of the refresh CronJob's Job for the builds asked for in
// TemplateRequestsName, one run at a time: an administrator's Build now, or a cluster creator's
// missing template. The interface never starts Jobs itself, which would let it run any pod.
type TemplateRequests struct {
	Client client.Client
	// Reader reads past any cache: Jobs and these ConfigMaps are watched by nothing else here.
	Reader    client.Reader
	Namespace string
	Interval  time.Duration
	Now       func() time.Time
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*TemplateRequests) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (t *TemplateRequests) Start(ctx context.Context) error {
	tick := time.NewTicker(t.Interval)
	defer tick.Stop()
	for {
		if err := t.runOnce(ctx); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).WithName("template-requests").Error(err, "starting a requested template build")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (t *TemplateRequests) runOnce(ctx context.Context) error {
	cm := &corev1.ConfigMap{}
	err := t.Reader.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: TemplateRequestsName}, cm)
	if apierrors.IsNotFound(err) || err == nil && len(cm.Data) == 0 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the template requests: %w", err)
	}
	running, err := t.running(ctx)
	if err != nil || running {
		return err
	}
	cron := &batchv1.CronJob{}
	err = t.Reader.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: TemplateRefreshName}, cron)
	if err != nil {
		return fmt.Errorf("reading CronJob %s: %w", TemplateRefreshName, err)
	}
	job, err := requestedJob(cron, cm.Data, t.Now())
	if err != nil {
		return err
	}
	// Taken off first, so that a Job is never started twice for one request; a request whose Job
	// cannot be created is put back.
	requests := cm.Data
	cm.Data = nil
	if err = t.Client.Update(ctx, cm); err != nil {
		return fmt.Errorf("taking the template requests: %w", err)
	}
	if err = t.Client.Create(ctx, job); err != nil {
		patch, perr := json.Marshal(map[string]any{"data": requests})
		if perr == nil {
			back := &corev1.ConfigMap{Namespace: t.Namespace, Name: TemplateRequestsName}
			perr = t.Client.Patch(ctx, back, client.RawPatch(types.MergePatchType, patch))
		}
		if perr != nil {
			ctrl.LoggerFrom(ctx).WithName("template-requests").Error(perr, "putting the template requests back")
		}
		return fmt.Errorf("starting Job %s: %w", job.Name, err)
	}
	ctrl.LoggerFrom(ctx).WithName("template-requests").Info("template build started", "job", job.Name, "requests", slices.Sorted(maps.Keys(requests)))
	return nil
}

// running tells whether a run of the refresh, scheduled or asked for, has not finished.
func (t *TemplateRequests) running(ctx context.Context) (bool, error) {
	var jobs batchv1.JobList
	if err := t.Reader.List(ctx, &jobs, client.InNamespace(t.Namespace), client.HasLabels{TemplateRefreshLabel}); err != nil {
		return false, fmt.Errorf("listing the template refresh's Jobs: %w", err)
	}
	return slices.ContainsFunc(jobs.Items, func(j batchv1.Job) bool { return !jobFinished(&j) }), nil
}

func jobFinished(j *batchv1.Job) bool {
	return slices.ContainsFunc(j.Status.Conditions, func(c batchv1.JobCondition) bool {
		return (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue
	})
}

// requestedJob is the CronJob's Job for the requests: a full refresh for RefreshRequest, else only
// the missing lines asked for, each as --missing, with --trigger saying who asked.
func requestedJob(cron *batchv1.CronJob, requests map[string]string, now time.Time) (*batchv1.Job, error) {
	tmpl := cron.Spec.JobTemplate
	spec := tmpl.Spec.DeepCopy()
	if len(spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("CronJob %s has no container", cron.Name)
	}
	var triggers, extra []string
	full := false
	for _, key := range slices.Sorted(maps.Keys(requests)) {
		var req TemplateRequest
		if err := json.Unmarshal([]byte(requests[key]), &req); err != nil {
			return nil, fmt.Errorf("template request %s: %w", key, err)
		}
		if key == RefreshRequest {
			full = true
			triggers = append(triggers, "Build now by "+req.By)
			continue
		}
		if _, err := ParseRequestLine(key); err != nil {
			return nil, fmt.Errorf("template request %s: %w", key, err)
		}
		triggers = append(triggers, key+" for "+req.By)
		extra = append(extra, "--missing="+key)
	}
	if !full {
		extra = append(extra, "--missing-only")
	}
	c := &spec.Template.Spec.Containers[0]
	c.Args = slices.DeleteFunc(c.Args, func(a string) bool { return strings.HasPrefix(a, "--trigger=") })
	c.Args = append(append(c.Args, "--trigger="+strings.Join(triggers, "; ")), extra...)
	controller := true
	job := &batchv1.Job{
		Namespace:   cron.Namespace,
		Name:        cron.Name + "-" + strconv.FormatInt(now.Unix(), 10),
		Labels:      maps.Clone(tmpl.Labels),
		Annotations: maps.Clone(tmpl.Annotations),
		// Owned by the CronJob, as its own runs are, so that they go with it.
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "CronJob", Name: cron.Name, UID: cron.UID, Controller: &controller,
		}},
		Spec: *spec,
	}
	if job.Labels == nil {
		job.Labels = map[string]string{}
	}
	job.Labels[TemplateRefreshLabel] = trueText
	return job, nil
}
