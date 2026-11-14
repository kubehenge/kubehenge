package kubehenge_test

import (
	"strings"
	"testing"
)

const managementProxyAnnotation = `kubehenge.dev/management-proxy: "true"`

func TestClassesSayWhetherTheManagementSideHasAProxy(t *testing.T) {
	t.Parallel()
	for _, class := range []string{"k3s", "kubeadm", "rke2", "talos"} {
		file := "templates/" + class + ".yaml"
		if out := helmTemplate(t, file, "proxy.secret=kubehenge-proxy"); !strings.Contains(out, managementProxyAnnotation) {
			t.Errorf("%s lacks %s with a proxy set", class, managementProxyAnnotation)
		}
		if out := helmTemplate(t, file); strings.Contains(out, "management-proxy") {
			t.Errorf("%s names a management proxy with none set", class)
		}
	}
}
