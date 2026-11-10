package templates

import (
	"slices"
	"strings"
	"testing"
)

var (
	rocky10     = NodeOS{Family: rockyName, Release: "10"}
	rocky10FIPS = NodeOS{Family: rockyName, Release: "10", FIPS: true}
)

func TestPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  Request
		want Template
	}{
		{
			name: "talos default version",
			req:  Request{Distribution: Talos},
			want: Template{
				Name:  "kubehenge-talos-" + DefaultTalosVersion,
				Tags:  []string{"kubehenge", "talos", "talos-" + DefaultTalosVersion},
				Image: ImageRef{Kind: TalosFactory, Version: DefaultTalosVersion},
			},
		},
		{
			name: "rke2 on the default node OS",
			req:  Request{Distribution: RKE2},
			want: Template{
				Name:  "kubehenge-rke2-almalinux-10",
				Tags:  []string{"kubehenge", "os-almalinux-10", "rke2"},
				Image: ImageRef{Kind: NodeOSImage, NodeOS: DefaultNodeOS},
				Bake:  &Bake{NodeOS: DefaultNodeOS, Distribution: RKE2},
			},
		},
		{
			name: "k3s on Rocky Linux",
			req:  Request{Distribution: K3s, NodeOS: rocky10},
			want: Template{
				Name:  "kubehenge-k3s-rocky-10",
				Tags:  []string{"k3s", "kubehenge", "os-rocky-10"},
				Image: ImageRef{Kind: NodeOSImage, NodeOS: rocky10},
				Bake:  &Bake{NodeOS: rocky10, Distribution: K3s},
			},
		},
		{
			name: "rke2 on Rocky Linux in FIPS mode",
			req:  Request{Distribution: RKE2, NodeOS: rocky10FIPS},
			want: Template{
				Name:  "kubehenge-rke2-rocky-10-fips",
				Tags:  []string{"fips", "kubehenge", "os-rocky-10", "rke2"},
				Image: ImageRef{Kind: NodeOSImage, NodeOS: rocky10},
				Bake:  &Bake{NodeOS: rocky10FIPS, Distribution: RKE2},
			},
		},
		{
			name: "rke2 in FIPS mode on the default node OS",
			req:  Request{Distribution: RKE2, NodeOS: NodeOS{FIPS: true}},
			want: Template{
				Name:  "kubehenge-rke2-almalinux-10-fips",
				Tags:  []string{"fips", "kubehenge", "os-almalinux-10", "rke2"},
				Image: ImageRef{Kind: NodeOSImage, NodeOS: DefaultNodeOS},
				Bake:  &Bake{NodeOS: NodeOS{Family: almaLinuxName, Release: "10", FIPS: true}, Distribution: RKE2},
			},
		},
		{
			name: "kubeadm on Rocky Linux",
			req:  Request{Distribution: Kubeadm, NodeOS: rocky10, KubernetesVersion: "v1.37.1"},
			want: Template{
				Name:  "kubehenge-kubeadm-v1.37.1-rocky-10",
				Tags:  []string{"kubeadm", "kubehenge", "kubernetes-v1.37.1", "os-rocky-10"},
				Image: ImageRef{Kind: NodeOSImage, NodeOS: rocky10},
				Bake:  &Bake{NodeOS: rocky10, Distribution: Kubeadm, KubernetesVersion: "v1.37.1"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Plan(&tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tt.want.Name || !slices.Equal(got.Tags, tt.want.Tags) || got.Image != tt.want.Image {
				t.Errorf("Plan() = %+v, want %+v", got, tt.want)
			}
			if (got.Bake == nil) != (tt.want.Bake == nil) || (got.Bake != nil && *got.Bake != *tt.want.Bake) {
				t.Errorf("Plan().Bake = %+v, want %+v", got.Bake, tt.want.Bake)
			}
			if got.Bake != nil {
				if back, ok := RequestOf(got.Tags); !ok || back.NodeOS != got.Bake.NodeOS || back.Distribution != tt.req.Distribution ||
					back.KubernetesVersion != tt.req.KubernetesVersion {
					t.Errorf("RequestOf(%v) = %+v, %v", got.Tags, back, ok)
				}
			}
		})
	}
}

func TestPlanTagSetsAreUnique(t *testing.T) {
	t.Parallel()
	// capmox needs exactly one template per tag set, so no two requests may collide. RKE2 and k3s
	// each install their own SELinux policy, so each has its own line.
	reqs := []Request{
		{Distribution: Talos, TalosVersion: "v1.14.2"},
		{Distribution: Talos, TalosVersion: "v1.14.1"},
		{Distribution: RKE2},
		{Distribution: K3s},
		{Distribution: RKE2, NodeOS: rocky10},
		{Distribution: RKE2, NodeOS: rocky10FIPS},
		{Distribution: RKE2, NodeOS: NodeOS{Family: almaLinuxName, Release: "10", FIPS: true}},
		{Distribution: K3s, NodeOS: rocky10},
		{Distribution: Kubeadm, KubernetesVersion: "v1.37.1"},
		{Distribution: Kubeadm, KubernetesVersion: "v1.36.5"},
		{Distribution: Kubeadm, NodeOS: rocky10, KubernetesVersion: "v1.37.1"},
	}
	seen := map[string]Request{}
	for _, r := range reqs {
		tmpl, err := Plan(&r)
		if err != nil {
			t.Fatal(err)
		}
		key := strings.Join(tmpl.Tags, ";")
		if prev, ok := seen[key]; ok {
			t.Errorf("%+v and %+v share tags %s", prev, r, key)
		}
		seen[key] = r
	}
}

func TestPlanRejects(t *testing.T) {
	t.Parallel()
	for _, req := range []Request{
		{Distribution: "flatcar"},
		{Distribution: Talos, TalosVersion: "1.14.2"},
		{Distribution: Talos, TalosVersion: "v1.14"},
		{Distribution: RKE2, NodeOS: NodeOS{Family: "debian", Release: "13"}},
		{Distribution: RKE2, NodeOS: NodeOS{Family: almaLinuxName, Release: "9"}},
		// Not released yet, which no template is built of.
		{Distribution: K3s, NodeOS: NodeOS{Family: leapName, Release: "16.1"}},
		{Distribution: Kubeadm},
		{Distribution: Kubeadm, KubernetesVersion: "latest"},
	} {
		if _, err := Plan(&req); err == nil {
			t.Errorf("Plan(%+v) succeeded, want an error", req)
		}
	}
}

func TestPlanRefusesFIPSModeWhereNoneIs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		req  Request
		want string
	}{
		{Request{Distribution: K3s, NodeOS: rocky10FIPS}, "k3s has no FIPS mode. Choose RKE2 for FIPS mode"},
		{Request{Distribution: Kubeadm, NodeOS: rocky10FIPS, KubernetesVersion: "v1.37.1"}, "kubeadm has no FIPS mode. Choose RKE2 for FIPS mode"},
		{Request{Distribution: RKE2, NodeOS: NodeOS{Family: leapName, Release: "16.1", FIPS: true}}, "openSUSE Leap 16.1"},
	} {
		if _, err := Plan(&tc.req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Plan(%+v) = %v, want %q", tc.req, err, tc.want)
		}
	}
	if err := FIPSDistribution(Talos); err == nil || !strings.HasPrefix(err.Error(), "Talos has no FIPS mode") {
		t.Errorf("FIPSDistribution(Talos) = %v, want Talos refused by name", err)
	}
}

func TestDistributionTitleIsTheDistributionAsItsProjectWritesIt(t *testing.T) {
	t.Parallel()
	for d, want := range map[Distribution]string{Talos: "Talos", RKE2: "RKE2", K3s: "k3s", Kubeadm: "kubeadm"} {
		if got := d.Title(); got != want {
			t.Errorf("%s.Title() = %q, want %q", d, got, want)
		}
	}
}

func TestRequestOfRefusesWhatNoLineIs(t *testing.T) {
	t.Parallel()
	for _, tags := range [][]string{
		{"kubehenge", "rke2"},
		{"kubehenge", "os-almalinux-10"},
		{"kubeadm", "kubehenge", "os-almalinux-10"},
		{"kubehenge", "kubernetes-v1.37.1", "os-almalinux-10", "rke2"},
		{"kubehenge", "os-almalinux", "rke2"},
	} {
		if req, ok := RequestOf(tags); ok {
			t.Errorf("RequestOf(%v) = %+v", tags, req)
		}
	}
}

func TestParseTags(t *testing.T) {
	t.Parallel()
	got := ParseTags("talos;Kubehenge, talos-v1.14.2;;")
	want := []string{"kubehenge", "talos", "talos-v1.14.2"}
	if !slices.Equal(got, want) {
		t.Errorf("ParseTags() = %v, want %v", got, want)
	}
}
