package ui

import (
	"log/slog"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

func TestClassesGiveTheInterfaceTheSiteTimeZoneTheControllerRunsSchedulesIn(t *testing.T) {
	t.Parallel()
	s := &Server{Log: slog.New(slog.DiscardHandler)}
	for zone, want := range map[string]string{"Asia/Tokyo": "Asia/Tokyo", "Mars/Olympus": "", "Local": ""} {
		class := &clusterv1.ClusterClass{}
		class.Name = "kubehenge-k3s"
		class.Annotations = map[string]string{siteTimeZoneAnnotation: zone}
		// An unknown zone leaves schedules in UTC, so the interface must not start or show them in it.
		if got := s.classView(t.Context(), class, "k3s").Annotations[siteTimeZoneAnnotation]; got != want {
			t.Errorf("site zone %s: the class shows %q, want %q", zone, got, want)
		}
	}
}
