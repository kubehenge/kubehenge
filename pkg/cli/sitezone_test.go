package cli

import (
	"strings"
	"testing"
)

func TestControllerStartsWithASiteTimeZoneThatIsNoTimeZoneAndReportsIt(t *testing.T) {
	t.Parallel()
	for _, zone := range []string{"Mars/Olympus", "Europe/zurich", "Local"} {
		o := &controllerOptions{timeZone: zone}
		if err := o.parseReleaseMirror(); err != nil {
			t.Errorf("%s stopped the controller: %v", zone, err)
		}
		if !strings.Contains(o.r.SiteTimeZoneProblem, zone) {
			t.Errorf("%s: problem %q, want the zone named so that each cluster can report it", zone, o.r.SiteTimeZoneProblem)
		}
	}
	o := &controllerOptions{timeZone: "Europe/Kyiv"}
	if err := o.parseReleaseMirror(); err != nil || o.r.SiteTimeZoneProblem != "" {
		t.Errorf("a known zone: error %v, problem %q", err, o.r.SiteTimeZoneProblem)
	}
}
