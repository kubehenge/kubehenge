package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

func talosClass(releases ...string) *clusterv1.ClusterClass {
	class := rke2Class()
	class.Name = provisioner.ClassPrefix + "talos"
	class.Annotations[provisioner.TalosTemplatesAnnotation] = strings.Join(releases, ",")
	return class
}

func talosCluster(namespace, name, release string) *clusterv1.Cluster {
	c := cluster(namespace, name)
	c.Spec.Topology.ClassRef.Name = provisioner.ClassPrefix + "talos"
	c.Spec.Topology.Version = templates.TalosDefaultKubernetes
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables,
		clusterv1.ClusterVariable{Name: provisioner.TalosVersionVariable, Value: apiextensionsv1.JSON{Raw: []byte(`"` + release + `"`)}})
	return c
}

func TestTalosReleasesAreTheNewestOfEachMinorTalosChecksKnow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, talosClass("v1.14.3", "v1.13.5"))
	h.server.TalosReleases = func(context.Context) ([]string, error) {
		return []string{"v1.13.5", "v1.13.12", "v1.14.3", "v1.14.4", "v1.99.0", "garbage"}, nil
	}
	b := h.browser()
	b.signIn("viewer@pve")
	rec := b.do("GET", "/api/talos/releases", "")
	expect(t, rec, http.StatusOK)
	var v talosReleasesView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.Releases, []string{"v1.14.4", "v1.13.12"}) || !slices.Equal(v.Templates, []string{"v1.14.3", "v1.13.5"}) ||
		v.Default != templates.DefaultTalosVersion || v.Error != "" {
		t.Errorf("releases %+v", v)
	}
	for _, release := range []string{"v1.14.4", "v1.13.12", "v1.14.3", "v1.13.5"} {
		minors := v.Kubernetes[release]
		if len(minors) == 0 || !strings.HasPrefix(minors[0], "1.") {
			t.Errorf("Talos %s runs %v", release, minors)
		}
	}
	if _, ok := v.Kubernetes["v1.99.0"]; ok {
		t.Error("a release past Kubehenge's tables is offered")
	}
}

func TestMissingTalosTemplate(t *testing.T) {
	t.Parallel()
	class := func(annotations map[string]string) *clusterv1.ClusterClass {
		return &clusterv1.ClusterClass{Annotations: annotations}
	}
	recorded := provisioner.TalosTemplatesAnnotation
	for _, c := range []struct {
		name, release, want string
		class               *clusterv1.ClusterClass
	}{
		{"not looked at yet", "v1.14.4", "", class(nil)},
		{"none", "v1.14.4", "no Talos VM template yet", class(map[string]string{recorded: ""})},
		{"the release's", "v1.14.4", "", class(map[string]string{recorded: "v1.14.3,v1.14.4"})},
		{"another release's", "v1.14.4", "no Talos v1.14.4 VM template yet", class(map[string]string{recorded: "v1.14.3"})},
	} {
		if got := missingTemplate(c.class, "talos", c.release, templates.NodeOS{}); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: missingTemplate = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTalosconfigIsHandedOutLikeTheKubeconfig(t *testing.T) {
	t.Parallel()
	secret := &corev1.Secret{
		Namespace: "default", Name: provisioner.TalosconfigSecret("one"),
		Data: map[string][]byte{provisioner.TalosconfigKey: []byte("context: one\n")},
	}
	h := newHarness(t, talosClass("v1.14.4"), talosCluster("default", "one", "v1.14.4"), talosCluster("default", "two", "v1.14.4"),
		cluster("default", "three"), secret)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/talosconfig", "")
	expect(t, rec, http.StatusOK)
	if rec.Body.String() != "context: one\n" || !strings.Contains(rec.Header().Get("Content-Disposition"), `filename="one.talosconfig"`) {
		t.Fatalf("talosconfig: %v %s", rec.Header(), rec.Body)
	}
	expect(t, b.do("GET", "/api/clusters/default/two/talosconfig", ""), http.StatusForbidden)

	admin := h.browser()
	admin.signIn("admin@pve")
	expect(t, admin.do("GET", "/api/clusters/default/two/talosconfig", ""), http.StatusConflict)
	expect(t, admin.do("GET", "/api/clusters/default/three/talosconfig", ""), http.StatusNotFound)
}

func TestFailedTalosUpgradesShowAndAreTriedAgain(t *testing.T) {
	t.Parallel()
	c := talosCluster("default", "one", "v1.14.4")
	c.Annotations = map[string]string{
		provisioner.TalosImageAnnotation:         "v1.14.3",
		provisioner.TalosUpgradeFailedAnnotation: "one-cp-1: not Ready",
	}
	h := newHarness(t, talosClass("v1.14.3", "v1.14.4"), c)
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	rec := viewer.do("GET", "/api/clusters/default/one", "")
	expect(t, rec, http.StatusOK)
	var v struct {
		Talos *talosView `json:"talos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Talos == nil || v.Talos.Version != "v1.14.4" || v.Talos.Image != "v1.14.3" || v.Talos.Failed != "one-cp-1: not Ready" {
		t.Fatalf("Talos view %+v", v.Talos)
	}
	expect(t, viewer.do("DELETE", "/api/clusters/default/one/talos-upgrade-failure", ""), http.StatusForbidden)

	admin := h.browser()
	admin.signIn("admin@pve")
	expect(t, admin.do("DELETE", "/api/clusters/default/one/talos-upgrade-failure", ""), http.StatusNoContent)
	got := &clusterv1.Cluster{}
	if err := h.kube.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	if _, failed := got.Annotations[provisioner.TalosUpgradeFailedAnnotation]; failed || got.Annotations[provisioner.TalosImageAnnotation] != "v1.14.3" {
		t.Errorf("annotations after trying again %v", got.Annotations)
	}
}
