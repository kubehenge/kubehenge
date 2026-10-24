package templates

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestNodeOSRegistry(t *testing.T) {
	t.Parallel()
	if _, r, err := Available(DefaultNodeOS); err != nil || r.Mode != ImageMode {
		t.Fatalf("the default node OS: %+v, %v", r, err)
	}
	if !slices.Equal(AvailableNodeOSes(), []string{"almalinux-10", "rocky-10"}) {
		t.Errorf("available %v", AvailableNodeOSes())
	}
	names := map[string]bool{}
	for _, f := range Families {
		if strings.Contains(f.Name(), "-") || names[f.Name()] {
			t.Errorf("family %q: a family's name is one word of its own", f.Name())
		}
		names[f.Name()] = true
		if f.MAC() != SELinux {
			t.Errorf("%s enforces %q", f.Title(), f.MAC())
		}
		for _, r := range f.Releases() {
			if r.Mode != ImageMode && r.Mode != Transactional {
				t.Errorf("%s %s builds as %q", f.Title(), r.Name, r.Mode)
			}
			if (r.Unreleased == "") == (r.Build == "") {
				t.Errorf("%s %s: a released release names its build, and only it", f.Title(), r.Name)
			}
		}
	}
	for _, o := range NodeOSes() {
		back, err := ParseNodeOS(o.String())
		if err != nil || back != o.NodeOS || !strings.HasPrefix(o.Tag(), "os-") {
			t.Errorf("%s reads back as %+v, %v", o, back, err)
		}
		if tagged, ok := NodeOSOf([]string{"kubehenge", o.Tag()}); !ok || tagged != o.NodeOS {
			t.Errorf("%s from its tag: %+v", o, tagged)
		}
	}
}

// The ClusterClasses validate the nodeOS variable against the chart's own list, which has to say
// what the families here do.
func TestTheChartListsTheNodeOSes(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../charts/kubehenge/files/node-oses.json")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Default   NodeOS   `json:"default"`
		Families  []string `json:"families"`
		Available []string `json:"available"`
	}
	if err := json.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Default != DefaultNodeOS || !slices.Equal(chart.Available, AvailableNodeOSes()) {
		t.Errorf("the chart offers %s by default of %v, want %s of %v", chart.Default, chart.Available, DefaultNodeOS, AvailableNodeOSes())
	}
	var families []string
	for _, o := range AvailableNodeOSes() {
		family, _, _ := strings.Cut(o, "-")
		if !slices.Contains(families, family) {
			families = append(families, family)
		}
	}
	if !slices.Equal(chart.Families, families) {
		t.Errorf("the chart's families %v, want %v", chart.Families, families)
	}
}

func TestLeapWaitsForItsRelease(t *testing.T) {
	t.Parallel()
	o := NodeOS{Family: leapName, Release: "16.1"}
	if _, _, err := ReleaseOf(o); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Available(o); err == nil || !strings.Contains(err.Error(), "openSUSE Leap 16.1") {
		t.Errorf("Available = %v", err)
	}
	i := slices.IndexFunc(NodeOSes(), func(n NodeOSInfo) bool { return n.NodeOS == o })
	if i < 0 || NodeOSes()[i].Unreleased == "" || NodeOSes()[i].Default {
		t.Errorf("listed as %+v", NodeOSes())
	}
}

func TestParseNodeOSRefuses(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "almalinux", "-10", "almalinux-"} {
		if o, err := ParseNodeOS(s); err == nil {
			t.Errorf("ParseNodeOS(%q) = %+v", s, o)
		}
	}
	for _, o := range []NodeOS{{Family: "debian", Release: "13"}, {Family: almaLinuxName, Release: "9"}} {
		if _, _, err := ReleaseOf(o); err == nil {
			t.Errorf("ReleaseOf(%+v) found one", o)
		}
	}
	if _, ok := NodeOSOf([]string{"kubehenge", "rke2"}); ok {
		t.Error("tags without a node OS name one")
	}
}

func TestCPULevels(t *testing.T) {
	t.Parallel()
	const (
		v1 = "fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36 clflush mmx fxsr sse sse2 syscall nx lm"
		v2 = v1 + " pni ssse3 cx16 sse4_1 sse4_2 popcnt lahf_lm"
		v3 = v2 + " avx f16c abm fma bmi1 avx2 bmi2 movbe xsave"
		v4 = v3 + " avx512f avx512dq avx512cd avx512bw avx512vl"
	)
	for flags, want := range map[string]int{"": 1, v1: 1, v2: 2, v3: 3, v4: 4, v1 + " avx512f avx512dq avx512cd avx512bw avx512vl": 1} {
		if got := CPULevelOf(flags); got != want {
			t.Errorf("CPULevelOf(%.30q...) = %d, want %d", flags, got, want)
		}
	}
	for cpu, want := range map[string]int{"x86-64-v2-AES": 2, "x86-64-v3": 3, "x86-64-v4": 4, "host": 1, "kvm64": 1, "x86-64-v30": 1} {
		if got := cpuTypeLevel(cpu); got != want {
			t.Errorf("cpuTypeLevel(%s) = %d, want %d", cpu, got, want)
		}
	}
	if CPULevel(DefaultNodeOS) != 2 || CPULevel(rocky10) != 3 || CPULevel(NodeOS{Family: "plan9"}) != 1 {
		t.Errorf("levels %d %d", CPULevel(DefaultNodeOS), CPULevel(rocky10))
	}
}
