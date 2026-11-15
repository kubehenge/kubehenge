package kubehenge_test

import (
	"fmt"
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

// The Tigera operator runs Calico from the ImageSet named for the release its chart carries, so
// every image Kubehenge pins for it is at that release. Calico pushes a release's images days
// before it publishes the release and its chart.
func TestCalicoImagesMatchTheirChart(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	chart := version(fmt.Sprint(field(t, values, "addons", "calico", "version")))
	images, ok := field(t, values, "addons", "calico", "images").([]any)
	if !ok || len(images) == 0 {
		t.Fatal("addons.calico.images is not a list of images")
	}
	for _, image := range images {
		if version(fmt.Sprint(image)) != chart {
			t.Errorf("Calico's chart is %s, but its image is %v", chart, image)
		}
	}
}
