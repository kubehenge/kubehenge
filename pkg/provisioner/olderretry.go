package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// olderRetriedBy is who a retry's request names, as a user's would be named.
const olderRetriedBy = "the kernel check"

// olderGiveUp is how long after its newest older build a line is still asked for. A run that fails
// before it makes a build leaves that build as it was, so without a limit the line would be asked
// for every OlderRetryAfter for ever.
const olderGiveUp = 2 * OlderRetries * OlderRetryAfter

// retryOlder asks, in TemplateRequestsName, for another build of each line whose newest build has
// an older kernel than the build before, an hour after that build and OlderRetries times at most.
// The controller's TemplateRequests starts the run, so no refresh run waits for the hour. Past the
// last try the build stays out of the builds clusters move to, and the interface shows it as older.
func (p *TemplateVersions) retryOlder(ctx context.Context, seen *poolTemplates) error {
	if p.Reader == nil || len(seen.older) == 0 {
		return nil
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	due := olderLinesDue(seen.builds, seen.older, now)
	if len(due) == 0 {
		return nil
	}
	var jobs batchv1.JobList
	if err := p.Reader.List(ctx, &jobs, client.InNamespace(p.ClassNamespace), client.HasLabels{TemplateRefreshLabel}); err != nil {
		return fmt.Errorf("listing the template refresh's Jobs: %w", err)
	}
	if slices.ContainsFunc(jobs.Items, func(j batchv1.Job) bool {
		return !jobFinished(&j) || now.Sub(j.CreationTimestamp.Time) < OlderRetryAfter
	}) {
		return nil
	}
	cm := &corev1.ConfigMap{}
	err := p.Reader.Get(ctx, client.ObjectKey{Namespace: p.ClassNamespace, Name: TemplateRequestsName}, cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the template requests: %w", err)
	}
	raw, err := json.Marshal(TemplateRequest{By: olderRetriedBy, At: now.UTC().Truncate(time.Second)})
	if err != nil {
		return fmt.Errorf("writing the request: %w", err)
	}
	asked := map[string]string{}
	for _, line := range due {
		if _, there := cm.Data[OlderRequestPrefix+line]; !there {
			asked[OlderRequestPrefix+line] = string(raw)
		}
	}
	if len(asked) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"data": asked})
	if err != nil {
		return fmt.Errorf("writing the requests: %w", err)
	}
	back := &corev1.ConfigMap{Namespace: p.ClassNamespace, Name: TemplateRequestsName}
	if err := p.Client.Patch(ctx, back, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("asking for the builds again: %w", err)
	}
	return nil
}

// olderLinesDue are the request keys of the lines to build again at now. A line is due when its
// newest build is older, no accepted build is newer, fewer than OlderRetries older builds follow
// the last accepted one, and the newest older build is OlderRetryAfter old.
func olderLinesDue(accepted, older NodeImageBuilds, now time.Time) []string {
	var due []string
	for _, line := range older.Lines() {
		if len(older[line]) == 0 {
			continue
		}
		newest := slices.MaxFunc(older[line], time.Time.Compare)
		if now.Sub(newest) < OlderRetryAfter || now.Sub(newest) > olderGiveUp {
			continue
		}
		after := time.Time{}
		for _, at := range accepted[line] {
			after = maxTime(after, at)
		}
		if after.After(newest) {
			continue
		}
		chain := 0
		for _, at := range older[line] {
			if at.After(after) {
				chain++
			}
		}
		// The first older build is not a retry.
		if chain-1 >= OlderRetries {
			continue
		}
		req, ok := templates.RequestOf(strings.Split(line, ";"))
		if !ok {
			continue
		}
		if key := RequestLine(&req); key != "" {
			due = append(due, key)
		}
	}
	return due
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
