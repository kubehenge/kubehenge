package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestSiteCARollOpensAWindowNamingNoZoneInTheSiteTimeZone(t *testing.T) {
	t.Parallel()
	c := cluster("team-a", "windowed")
	c.Status.Conditions = []metav1.Condition{{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue}}
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: "upgrades", Value: apiextensionsv1.JSON{Raw: []byte(`{"mode":"patch","window":{"weekdays":["Mon"],"start":"02:00"}}`)},
	})
	// Sunday 17:30 UTC is Monday 02:30 in Tokyo.
	now := time.Date(2026, time.November, 15, 17, 30, 0, 0, time.UTC)
	for zone, want := range map[string]siteCARoll{"Asia/Tokyo": {Inherits: 1, Now: 1}, "UTC": {Inherits: 1, Windows: 1}} {
		roll := siteCARollOf("new", zone, "kubehenge-system", []clusterv1.Cluster{*c}, nil, now)
		if roll.Now != want.Now || roll.Windows != want.Windows {
			t.Errorf("site zone %s: roll = %+v, want %d now and %d at its window", zone, roll, want.Now, want.Windows)
		}
	}
}

func TestSiteZoneOfThePreviewIsTheSavedOneWhenItIsATimeZone(t *testing.T) {
	t.Parallel()
	if got := (&Server{}).siteZone(); got != "UTC" {
		t.Errorf("without an appliance: %s, want UTC", got)
	}
	for saved, want := range map[string]string{"Asia/Tokyo": "Asia/Tokyo", "Mars/Olympus": "UTC", "": "UTC"} {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "management"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "management", "settings.json"), []byte(`{"timeZone":"`+saved+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		s := &Server{Appliance: &appliance.Appliance{Dir: dir}}
		if got := s.siteZone(); got != want {
			t.Errorf("saved %q: %s, want %s", saved, got, want)
		}
	}
}

func TestSiteCARollNamesHowManyRollNowAndHowManyWait(t *testing.T) {
	t.Parallel()
	built := func(name string) *clusterv1.Cluster {
		c := cluster("team-a", name)
		c.Status.Conditions = []metav1.Condition{{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue}}
		return c
	}
	with := func(name, variable, value string) clusterv1.Cluster {
		c := built(name)
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: variable, Value: apiextensionsv1.JSON{Raw: []byte(value)}})
		return *c
	}
	// Wednesday noon: the Monday window is closed.
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	clusters := make([]clusterv1.Cluster, 0, provisioner.SiteRollLimit+4)
	for i := range provisioner.SiteRollLimit + 2 {
		clusters = append(clusters, *built(fmt.Sprintf("web-%d", i)))
	}
	clusters = append(clusters,
		*cluster("team-a", "new"),
		with("windowed", "upgrades", `{"mode":"patch","window":{"weekdays":["Mon"],"start":"02:00","timeZone":"UTC"}}`),
		with("own", "trustedCAs", `["x"]`),
	)
	roll := siteCARollOf("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", "UTC", "kubehenge-system", clusters, nil, now)
	if roll.Inherits != provisioner.SiteRollLimit+3 || roll.Now != provisioner.SiteRollLimit || roll.Waiting != 2 || roll.Windows != 1 {
		t.Errorf("roll = %+v, want %d inheriting, %d now, 2 waiting for a place and 1 for its window", roll, provisioner.SiteRollLimit+3, provisioner.SiteRollLimit)
	}
	if roll.Opens == nil || roll.Opens.Weekday() != time.Monday {
		t.Errorf("opens = %v, want a Monday", roll.Opens)
	}
}
