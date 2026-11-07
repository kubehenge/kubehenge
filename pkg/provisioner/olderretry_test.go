package provisioner

import (
	"context"
	"slices"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const olderLine = "kubehenge;os-almalinux-10;rke2"

func TestOlderLinesDueFollowTheHourAndTheThreeTries(t *testing.T) {
	t.Parallel()
	built := day(8)
	at := func(d time.Duration) time.Time { return built.Add(d) }
	for name, c := range map[string]struct {
		accepted, older []time.Time
		now             time.Time
		want            []string
	}{
		"before the hour is over":    {[]time.Time{day(1)}, []time.Time{built}, at(59 * time.Minute), nil},
		"an hour after the build":    {[]time.Time{day(1)}, []time.Time{built}, at(time.Hour), []string{"rke2-almalinux-10"}},
		"after the first try":        {[]time.Time{day(1)}, []time.Time{built, at(70 * time.Minute)}, at(3 * time.Hour), []string{"rke2-almalinux-10"}},
		"after the second try":       {[]time.Time{day(1)}, []time.Time{built, at(70 * time.Minute), at(140 * time.Minute)}, at(4 * time.Hour), []string{"rke2-almalinux-10"}},
		"after the third try":        {[]time.Time{day(1)}, []time.Time{built, at(70 * time.Minute), at(140 * time.Minute), at(210 * time.Minute)}, at(5 * time.Hour), nil},
		"a newer accepted build":     {[]time.Time{day(1), at(90 * time.Minute)}, []time.Time{built}, at(3 * time.Hour), nil},
		"tries before an accepted":   {[]time.Time{day(1), day(9)}, []time.Time{built, at(70 * time.Minute), at(140 * time.Minute), at(210 * time.Minute), day(10)}, day(10).Add(2 * time.Hour), []string{"rke2-almalinux-10"}},
		"given up on a stuck line":   {[]time.Time{day(1)}, []time.Time{built}, at(7 * time.Hour), nil},
		"no older build":             {[]time.Time{day(1)}, nil, at(2 * time.Hour), nil},
		"just inside the give-up":    {[]time.Time{day(1)}, []time.Time{built}, at(5 * time.Hour), []string{"rke2-almalinux-10"}},
	} {
		older := NodeImageBuilds{olderLine: c.older}
		if c.older == nil {
			older = NodeImageBuilds{}
		}
		if got := olderLinesDue(NodeImageBuilds{olderLine: c.accepted}, older, c.now); !slices.Equal(got, c.want) {
			t.Errorf("%s: due %v, want %v", name, got, c.want)
		}
	}
}

func olderSeen(older ...time.Time) *poolTemplates {
	return &poolTemplates{builds: NodeImageBuilds{olderLine: {day(1)}}, older: NodeImageBuilds{olderLine: older}}
}

func TestRetryOlderAsksOnceForALineAndNeverBesideARun(t *testing.T) {
	t.Parallel()
	requests := &corev1.ConfigMap{Namespace: "kubehenge-system", Name: TemplateRequestsName}
	kube := refreshKube(t, requests)
	p := &TemplateVersions{Client: kube, Reader: kube, ClassNamespace: "kubehenge-system", Now: func() time.Time { return day(8).Add(2 * time.Hour) }}
	ctx := context.Background()
	seen := olderSeen(day(8))
	for range 2 {
		if err := p.retryOlder(ctx, seen); err != nil {
			t.Fatal(err)
		}
	}
	got := &corev1.ConfigMap{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(requests), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 1 || got.Data[OlderRequestPrefix+"rke2-almalinux-10"] == "" {
		t.Fatalf("requests: %v", got.Data)
	}
	// The request becomes a Job that rebuilds just that line.
	job, err := requestedJob(refreshCronJob(), got.Data, day(9))
	if err != nil {
		t.Fatal(err)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	if !slices.Contains(args, "--again=rke2-almalinux-10") || !slices.Contains(args, "--missing-only") {
		t.Errorf("args %v", args)
	}
	for _, a := range args {
		if a == "--missing=rke2-almalinux-10" {
			t.Errorf("the line is asked for as missing: %v", args)
		}
	}
}

func TestRetryOlderWaitsForARunAndAnUnset(t *testing.T) {
	t.Parallel()
	now := day(8).Add(2 * time.Hour)
	for name, jobs := range map[string][]*batchv1.Job{
		"a run under way": {{Namespace: "kubehenge-system", Name: "running", Labels: map[string]string{TemplateRefreshLabel: "true"}}},
		"a run just over": {{
			Namespace: "kubehenge-system", Name: "done", Labels: map[string]string{TemplateRefreshLabel: "true"},
			CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
			Status:            batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
		}},
	} {
		requests := &corev1.ConfigMap{Namespace: "kubehenge-system", Name: TemplateRequestsName}
		objs := []client.Object{requests}
		for _, j := range jobs {
			objs = append(objs, j)
		}
		kube := refreshKube(t, objs...)
		p := &TemplateVersions{Client: kube, Reader: kube, ClassNamespace: "kubehenge-system", Now: func() time.Time { return now }}
		if err := p.retryOlder(context.Background(), olderSeen(day(8))); err != nil {
			t.Fatal(err)
		}
		got := &corev1.ConfigMap{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(requests), got); err != nil || len(got.Data) != 0 {
			t.Errorf("%s: requests %v, %v", name, got.Data, err)
		}
	}
	// Without a Reader, or without the requests ConfigMap (the refresh is off), nothing is asked.
	kube := refreshKube(t)
	for name, p := range map[string]*TemplateVersions{
		"no reader":     {Client: kube, ClassNamespace: "kubehenge-system", Now: func() time.Time { return now }},
		"no ConfigMap":  {Client: kube, Reader: kube, ClassNamespace: "kubehenge-system", Now: func() time.Time { return now }},
		"nothing older": {Client: kube, Reader: kube, ClassNamespace: "kubehenge-system", Now: func() time.Time { return now }},
	} {
		seen := olderSeen(day(8))
		if name == "nothing older" {
			seen = olderSeen()
		}
		if err := p.retryOlder(context.Background(), seen); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
