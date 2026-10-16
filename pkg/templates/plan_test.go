package templates

import (
	"slices"
	"strings"
	"testing"
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
			name: "rke2 shares the cloud image template",
			req:  Request{Distribution: RKE2},
			want: Template{
				Name:  "kubehenge-" + DefaultOS,
				Tags:  []string{"cloud-image", "kubehenge", DefaultOS},
				Image: ImageRef{Kind: UbuntuCloud, Version: DefaultOS},
				Bake:  &Bake{},
			},
		},
		{
			name: "kubeadm on an older Ubuntu",
			req:  Request{Distribution: Kubeadm, OS: "ubuntu-24.04", KubernetesVersion: "v1.37.1"},
			want: Template{
				Name:  "kubehenge-kubeadm-v1.37.1-ubuntu-24.04",
				Tags:  []string{"kubeadm", "kubehenge", "kubernetes-v1.37.1", "ubuntu-24.04"},
				Image: ImageRef{Kind: UbuntuCloud, Version: "ubuntu-24.04"},
				Bake:  &Bake{KubernetesVersion: "v1.37.1"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Plan(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tt.want.Name || !slices.Equal(got.Tags, tt.want.Tags) || got.Image != tt.want.Image {
				t.Errorf("Plan() = %+v, want %+v", got, tt.want)
			}
			if (got.Bake == nil) != (tt.want.Bake == nil) || (got.Bake != nil && *got.Bake != *tt.want.Bake) {
				t.Errorf("Plan().Bake = %+v, want %+v", got.Bake, tt.want.Bake)
			}
		})
	}
}

func TestPlanRKE2AndK3sShareATemplate(t *testing.T) {
	t.Parallel()
	rke2, err := Plan(Request{Distribution: RKE2, OS: "ubuntu-24.04"})
	if err != nil {
		t.Fatal(err)
	}
	k3s, err := Plan(Request{Distribution: K3s, OS: "ubuntu-24.04"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rke2.Tags, k3s.Tags) || rke2.Name != k3s.Name {
		t.Errorf("RKE2 %v and k3s %v should share one template", rke2.Tags, k3s.Tags)
	}
}

func TestPlanTagSetsAreUnique(t *testing.T) {
	t.Parallel()
	// capmox needs exactly one template per tag set, so no two requests may collide.
	reqs := []Request{
		{Distribution: Talos, TalosVersion: "v1.14.2"},
		{Distribution: Talos, TalosVersion: "v1.14.1"},
		{Distribution: RKE2, OS: "ubuntu-26.04"},
		{Distribution: RKE2, OS: "ubuntu-24.04"},
		{Distribution: Kubeadm, OS: "ubuntu-26.04", KubernetesVersion: "v1.37.1"},
		{Distribution: Kubeadm, OS: "ubuntu-26.04", KubernetesVersion: "v1.36.5"},
		{Distribution: Kubeadm, OS: "ubuntu-24.04", KubernetesVersion: "v1.37.1"},
	}
	seen := map[string]Request{}
	for _, r := range reqs {
		tmpl, err := Plan(r)
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
		{Distribution: RKE2, OS: "debian-13"},
		{Distribution: Kubeadm},
		{Distribution: Kubeadm, KubernetesVersion: "latest"},
	} {
		if _, err := Plan(req); err == nil {
			t.Errorf("Plan(%+v) succeeded, want an error", req)
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
