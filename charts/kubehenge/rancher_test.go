package kubehenge_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// version is the version an image reference's tag names, without its digest.
func version(image string) string {
	image, _, _ = strings.Cut(image, "@")
	return image[strings.LastIndex(image, ":")+1:]
}

// The charts Rancher installs itself run the images Kubehenge pins for them: a chart version
// such as 110.0.4+up0.27.4 runs v0.27.4. Rancher reads those charts from its own image only.
func TestRancherChartsMatchTheirImages(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	str := func(path ...string) string {
		return fmt.Sprint(field(t, values, append([]string{"addons", "rancher"}, path...)...))
	}
	if str("values", "useBundledSystemChart") != "true" {
		t.Error("Rancher fetches the charts it installs from Git, not from its image")
	}
	var extraEnv []envVar
	if err := yaml.Unmarshal([]byte(asJSON(t, field(t, values, "addons", "rancher", "values", "extraEnv"))), &extraEnv); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range extraEnv {
		env[e.Name] = e.Value
	}
	for setting, images := range map[string][]string{
		"CATTLE_RANCHER_TURTLES_VERSION": {str("turtles", "image")},
		"CATTLE_RANCHER_WEBHOOK_VERSION": {str("values", "webhook", "image", "tag")},
		"CATTLE_FLEET_VERSION":           {str("values", "fleet", "image", "tag"), str("values", "fleet", "agentImage", "tag")},
	} {
		chart, ok := env[setting]
		_, upstream, found := strings.Cut(chart, "+up")
		if !ok || !found {
			t.Errorf("%s = %q, want a chart version such as 110.0.4+up0.27.4", setting, chart)
			continue
		}
		for _, image := range images {
			if version(image) != "v"+upstream {
				t.Errorf("%s names chart %s, but its image is %q", setting, chart, image)
			}
		}
	}
	shell := version(env["CATTLE_SHELL_IMAGE"])
	for _, hook := range []string{"preUpgrade", "postDelete"} {
		if tag := str("values", hook, "image", "tag"); version(tag) != shell {
			t.Errorf("Rancher's %s hook runs shell %q, its shell image is %s", hook, tag, env["CATTLE_SHELL_IMAGE"])
		}
	}
}
