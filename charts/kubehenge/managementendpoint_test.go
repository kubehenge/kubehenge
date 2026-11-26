package kubehenge_test

import (
	"strings"
	"testing"
)

const managementEndpointAnnotation = `kubehenge.dev/management-endpoint: "true"`

func TestClassesSayWhetherTheManagementClusterHasAnAddress(t *testing.T) {
	t.Parallel()
	for _, class := range []string{"k3s", "kubeadm", "rke2", "talos"} {
		file := "templates/" + class + ".yaml"
		if out := helmTemplate(t, file, "managementEndpoint=https://192.0.2.10:6443"); !strings.Contains(out, managementEndpointAnnotation) {
			t.Errorf("%s lacks %s with the address set", class, managementEndpointAnnotation)
		}
		if out := helmTemplate(t, file); strings.Contains(out, "management-endpoint") {
			t.Errorf("%s names the management cluster's address with none set", class)
		}
	}
}
