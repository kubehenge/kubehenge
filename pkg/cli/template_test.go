package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func TestDeletingABuildAClusterRunsIsRefused(t *testing.T) {
	t.Parallel()
	line, err := templates.Plan(&templates.Request{Distribution: templates.RKE2})
	if err != nil {
		t.Fatal(err)
	}
	times := []time.Time{time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)}
	builds := make([]templates.Built, 0, len(times))
	for i, at := range times {
		build, err := line.Build(at)
		if err != nil {
			t.Fatal(err)
		}
		r := &proxmox.ClusterResource{VMID: uint64(9001 + i), Name: build.Name, Tags: strings.Join(build.Tags, ";")}
		builds = append(builds, templates.Built{Resource: r, At: at})
	}
	used := strings.Join(templates.NormaliseTags(templates.ParseTags(builds[1].Resource.Tags)), ";")
	checks := deleteChecks{users: func([]*proxmox.ClusterResource) (map[string][]string, error) {
		return map[string][]string{used: {"clusters/web", "clusters/api"}}, nil
	}}
	for _, tc := range []struct {
		name   string
		doomed []templates.Built
		roll   bool
		want   string
	}{
		{"a build nothing runs", builds[:1], false, ""},
		{"a build clusters run", builds[1:], false, "clusters/web, clusters/api clone from it; --roll-clusters"},
		{"a build clusters run, rolled", builds[1:], true, ""},
		{"every build, rolled", builds, true, "no build of the template would be left"},
	} {
		checks.roll = tc.roll
		refused, err := refusedBuilds(builds, tc.doomed, checks)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(refused, "\n")
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refused %q, want %q", tc.name, got, tc.want)
		}
	}
}
