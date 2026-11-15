package ui

import (
	"fmt"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestSiteCARollNamesHowManyRollNowAndHowManyWait(t *testing.T) {
	t.Parallel()
	with := func(name, variable, value string) clusterv1.Cluster {
		c := cluster("team-a", name)
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: variable, Value: apiextensionsv1.JSON{Raw: []byte(value)}})
		return *c
	}
	// Wednesday noon: the Monday window is closed.
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	var clusters []clusterv1.Cluster
	for i := range provisioner.SiteRollLimit + 2 {
		clusters = append(clusters, *cluster("team-a", fmt.Sprintf("web-%d", i)))
	}
	clusters = append(clusters,
		with("windowed", "upgrades", `{"mode":"patch","window":{"weekdays":["Mon"],"start":"02:00","timeZone":"UTC"}}`),
		with("own", "trustedCAs", `["x"]`),
	)
	roll, err := siteCARollOf("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", clusters, now)
	if err != nil {
		t.Fatal(err)
	}
	if roll.Inherits != provisioner.SiteRollLimit+3 || roll.Now != provisioner.SiteRollLimit || roll.Waiting != 2 || roll.Windows != 1 {
		t.Errorf("roll = %+v, want %d inheriting, %d now, 2 waiting for a place and 1 for its window", roll, provisioner.SiteRollLimit+3, provisioner.SiteRollLimit)
	}
	if roll.Opens == nil || roll.Opens.Weekday() != time.Monday {
		t.Errorf("opens = %v, want a Monday", roll.Opens)
	}
}
