//go:build integration

package provisioner

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The chart tells the interface, in the ConfigMap it reads its namespaces from, that the shared
// Headlamp offers its actions, and says nothing of them otherwise.
func TestIntegrationChartHeadlampActions(t *testing.T) {
	t.Parallel()
	actions := func(values ...string) (string, bool) {
		t.Helper()
		for _, o := range renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, append([]string{"testdata/integration-values.yaml"}, values...)...) {
			if o.GetKind() == "ConfigMap" && o.GetName() == "kubehenge-interface" {
				v, found, err := unstructured.NestedString(o.Object, "data", "headlampActions")
				if err != nil {
					t.Fatal(err)
				}
				return v, found
			}
		}
		t.Fatal("the chart makes no ConfigMap kubehenge-interface")
		return "", false
	}
	if v, found := actions(); found {
		t.Errorf("by default the ConfigMap says %q of the actions, want nothing", v)
	}
	if v, found := actions("testdata/headlamp-actions-values.yaml"); !found || v != "true" {
		t.Errorf("with headlampActions the ConfigMap says %q (%v), want true", v, found)
	}
}
