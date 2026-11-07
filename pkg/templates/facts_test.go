package templates

import (
	"strings"
	"testing"
)

func TestCompareKernelsOrdersAsRPMDoes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		older, newer string
	}{
		{"6.12.0-124.2.1.el10_1.x86_64", "6.12.0-124.8.1.el10_1.x86_64"},
		{"6.12.0-124.8.1.el10_1.x86_64", "6.12.0-124.10.1.el10_1.x86_64"},
		{"6.12.0-124.8.1.el10_1.x86_64", "6.12.0-130.1.1.el10_1.x86_64"},
		{"6.4.0-150700.53.19-default", "6.4.0-150700.53.25-default"},
		{"6.12.0", "6.12.0-1"},
		{"6.9.9", "6.12.0"},
	} {
		if CompareKernels(tt.older, tt.newer) >= 0 || CompareKernels(tt.newer, tt.older) <= 0 {
			t.Errorf("%s should be older than %s", tt.older, tt.newer)
		}
	}
	if CompareKernels("6.12.0-124.8.1.el10_1.x86_64", "6.12.0-124.8.1.el10_1.x86_64") != 0 {
		t.Error("equal kernels compare unequal")
	}
}

func TestBuildFactsRoundTripThroughTheDescription(t *testing.T) {
	t.Parallel()
	facts := BuildFacts{Kernel: "6.12.0-124.8.1.el10_1.x86_64", Packages: strings.Repeat("c", 64)}
	got, ok := ParseBuildFacts(facts.Description(templateDescription))
	if !ok || got != facts {
		t.Errorf("read %+v back, %v", got, ok)
	}
	if _, ok := ParseBuildFacts(templateDescription); ok {
		t.Error("a description without facts gave some")
	}
}

func TestParseFactsOutputRefusesWhatIsNotAKernelAndAHash(t *testing.T) {
	t.Parallel()
	for _, out := range []string{"", "6.12.0\n", "6.12.0\nshort\n", "6.12.0\n" + strings.Repeat("a", 64) + "\nextra"} {
		if _, err := parseFactsOutput(out); err == nil {
			t.Errorf("accepted %q", out)
		}
	}
}

func TestOlderBuildsCompareWithTheNewestEarlierBuildThatIsNotOlder(t *testing.T) {
	t.Parallel()
	good, older, newer := &BuildFacts{Kernel: "6.12.0-124.8.1"}, &BuildFacts{Kernel: "6.12.0-124.2.1"}, &BuildFacts{Kernel: "6.12.0-124.10.1"}
	for name, tt := range map[string]struct {
		facts []*BuildFacts
		want  []bool
	}{
		"older":                     {[]*BuildFacts{good, older}, []bool{false, true}},
		"equal":                     {[]*BuildFacts{good, good}, []bool{false, false}},
		"newer":                     {[]*BuildFacts{good, newer}, []bool{false, false}},
		"the baseline skips olders": {[]*BuildFacts{good, older, {Kernel: "6.12.0-124.5.1"}}, []bool{false, true, true}},
		"unknown never older":       {[]*BuildFacts{nil, good, nil, older}, []bool{false, false, false, true}},
	} {
		got := OlderBuilds(tt.facts)
		for i := range tt.want {
			if got[i] != tt.want[i] {
				t.Errorf("%s: %v, want %v", name, got, tt.want)
				break
			}
		}
	}
}
