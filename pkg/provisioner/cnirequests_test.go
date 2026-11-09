package provisioner

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"
)

// chartClass is the chart's ClusterClass of that name, as helm renders it.
func chartClass(t *testing.T, name string) *clusterv1.ClusterClass {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "kubehenge", "../../charts/kubehenge", "--namespace", "kubehenge-system",
		"--values", "testdata/integration-values.yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	for doc := range strings.SplitSeq(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n---\n") {
		class := &clusterv1.ClusterClass{}
		if err := yaml.Unmarshal([]byte(doc), class); err != nil {
			t.Fatal(err)
		}
		if class.Kind == "ClusterClass" && class.Name == name {
			return class
		}
	}
	t.Fatalf("the chart renders no ClusterClass %s", name)
	return nil
}

// TestWorkersHaveRoomForWhatTheCNIRunsOnce holds the chart's Talos class to the pods a CNI runs
// once per cluster. The scheduler may put them all on one worker, and a worker of 1424 MiB with the
// load balancer and Rancher's agent left Calico's no room.
func TestWorkersHaveRoomForWhatTheCNIRunsOnce(t *testing.T) {
	t.Parallel()
	class := chartClass(t, "kubehenge-talos")
	for _, tc := range []struct {
		cni       string
		workerMiB int
		admitted  bool
	}{
		{"", 1424, true},
		{cniCilium, 1424, false},
		{cniCilium, 1488, true},
		{cniCalico, 1424, false},
		{cniCalico, 1680, true},
	} {
		vars := map[string]string{
			"loadBalancer":  `{"enabled":true,"addresses":["192.0.2.100-192.0.2.110"]}`,
			"workerMachine": fmt.Sprintf(`{"memoryMiB":%d}`, tc.workerMiB),
		}
		if tc.cni != "" {
			vars["cni"] = `"` + tc.cni + `"`
		}
		err := checkAddonRoom(nil, withAddons(sized("", new(int32(1))), vars), class)
		switch {
		case tc.admitted && err != nil:
			t.Errorf("CNI %q on a worker of %d MiB: refused: %v", tc.cni, tc.workerMiB, err)
		case !tc.admitted && (err == nil || !strings.Contains(err.Error(), "of requested memory")):
			t.Errorf("CNI %q on a worker of %d MiB: %v, want it short of requested memory", tc.cni, tc.workerMiB, err)
		}
	}
}
