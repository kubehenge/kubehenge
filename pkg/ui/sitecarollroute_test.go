package ui

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func TestSiteCARollRouteChecksTheAuthorityAndNeedsTheSetupPrivileges(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	// No management cluster is up yet, so nothing rolls and the route still answers.
	if rec := b.do("POST", "/api/setup/site-ca-roll", `{"siteCA":""}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"inherits":0`) {
		t.Errorf("before the first setup: %d %s", rec.Code, rec.Body.String())
	}
	expect(t, b.do("POST", "/api/setup/site-ca-roll", `{"siteCA":"not a certificate"}`), http.StatusUnprocessableEntity)
	if rec := b.do("POST", "/api/setup/site-ca-roll", `{"siteCA":""}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"inherits"`) {
		t.Errorf("an empty site CA answered %d %s", rec.Code, rec.Body.String())
	}
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("POST", "/api/setup/site-ca-roll", `{"siteCA":""}`), http.StatusForbidden)
}

func TestSiteCARollRouteCountsClustersStillTakingUpTheLastSiteCA(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	ctx := t.Context()
	built := []metav1.Condition{{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue}}
	note := fmt.Sprintf(`{"at":%q,"generations":{"MachineDeployment/%%s-md-0":1}}`, time.Now().UTC().Format(time.RFC3339))
	for i := range provisioner.SiteRollLimit {
		name := fmt.Sprintf("web-%d", i)
		c := cluster("team-a", name)
		c.Status.Conditions = built
		c.Annotations = map[string]string{provisioner.SiteRollAnnotation: fmt.Sprintf(note, name)}
		md := &clusterv1.MachineDeployment{}
		md.Namespace, md.Name, md.Generation = "team-a", name+"-md-0", 1
		md.Labels = map[string]string{clusterv1.ClusterNameLabel: name}
		md.Status.ObservedGeneration = 1
		for _, o := range []client.Object{c, md} {
			if err := h.kube.Create(ctx, o); err != nil {
				t.Fatal(err)
			}
		}
	}
	extra := cluster("team-a", "z-extra")
	extra.Status.Conditions = built
	extra.Spec.Topology.Variables = append(extra.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: provisioner.SiteCAsVariable, Value: apiextensionsv1.JSON{Raw: []byte(`["x"]`)}})
	if err := h.kube.Create(ctx, extra); err != nil {
		t.Fatal(err)
	}
	rec := b.do("POST", "/api/setup/site-ca-roll", `{"siteCA":""}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"now":0`) || !strings.Contains(rec.Body.String(), `"waiting":1`) {
		t.Errorf("answered %d %s, want the clusters still taking up the last site CA to hold every place", rec.Code, rec.Body.String())
	}
}

func TestTimeZoneRouteAnswersAsTheServersTimeZoneDatabaseDoes(t *testing.T) {
	t.Parallel()
	h, b := preflightHarness(t)
	for zone, want := range map[string]int{
		"Europe/Kyiv": http.StatusOK, "Etc/GMT+1": http.StatusOK, "Factory": http.StatusOK,
		"Europe/zurich": http.StatusUnprocessableEntity, "America/new_york": http.StatusUnprocessableEntity,
		"Mars/Olympus": http.StatusUnprocessableEntity, "Local": http.StatusUnprocessableEntity,
	} {
		expect(t, b.do("POST", "/api/setup/time-zone", `{"timeZone":"`+zone+`"}`), want)
	}
	// The refusal names the zone, then the fix, as the interface shows it under the field.
	if rec := b.do("POST", "/api/setup/time-zone", `{"timeZone":"Europe/zurich"}`); !strings.Contains(rec.Body.String(), `"Unknown time zone \"Europe/zurich\". Use an IANA name, such as Europe/Zurich."`) {
		t.Errorf("refused with %s", rec.Body.String())
	}
	viewer := h.browser()
	viewer.signIn("viewer@pve")
	expect(t, viewer.do("POST", "/api/setup/time-zone", `{"timeZone":"UTC"}`), http.StatusForbidden)
}
