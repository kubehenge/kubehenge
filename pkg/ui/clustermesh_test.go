package ui

import (
	"encoding/json"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// meshed is a cluster of the default namespace that runs Cilium with Cluster Mesh on. It lists peers.
func meshed(name string, id int, pods string, peers ...string) *clusterv1.Cluster {
	c := cluster("default", name)
	c.Annotations = map[string]string{provisioner.CiliumClusterIDAnnotation: strconv.Itoa(id)}
	c.Spec.ControlPlaneEndpoint.Host = "10.0.0." + strconv.Itoa(10+id)
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{pods}
	mesh := []byte(`{"enabled":true,"peers":["` + strings.Join(peers, `","`) + `"]}`)
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables,
		clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"cilium"`)}},
		clusterv1.ClusterVariable{Name: meshVariable, Value: apiextensionsv1.JSON{Raw: mesh}},
	)
	return c
}

func TestClustersTellHowTheirMeshPeersStand(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		meshed("one", 1, "10.1.0.0/16", "two", "three", "four"),
		meshed("two", 2, "10.2.0.0/16", "one"),
		meshed("three", 3, "10.1.0.0/16", "one"),
	)
	peers := func(b *browser, path string) map[string]string {
		t.Helper()
		rec := b.do("GET", path, "")
		expect(t, rec, http.StatusOK)
		var views []clusterView
		if strings.HasSuffix(path, "/one") {
			views = make([]clusterView, 1)
			if err := json.Unmarshal(rec.Body.Bytes(), &views[0]); err != nil {
				t.Fatal(err)
			}
		} else if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
			t.Fatal(err)
		}
		for _, v := range views {
			if v.Name == "one" {
				got := map[string]string{}
				for _, p := range v.Mesh {
					got[p.Name] = p.Refused
				}
				return got
			}
		}
		t.Fatalf("%s does not show one", path)
		return nil
	}
	a := h.browser()
	a.signIn("admin@pve")
	want := map[string]string{"two": "", "three": "the pod ranges 10.1.0.0/16 and 10.1.0.0/16 overlap", "four": hiddenPeer}
	for _, path := range []string{"/api/clusters", "/api/clusters/default/one"} {
		if got := peers(a, path); !maps.Equal(got, want) {
			t.Errorf("admin, %s: peers %v, want %v", path, got, want)
		}
	}
	// The viewer sees one alone: of the others, only that two linked back.
	v := h.browser()
	v.signIn("viewer@pve")
	hidden := map[string]string{"two": "", "three": hiddenPeer, "four": hiddenPeer}
	for _, path := range []string{"/api/clusters", "/api/clusters/default/one"} {
		if got := peers(v, path); !maps.Equal(got, hidden) {
			t.Errorf("viewer, %s: peers %v, want %v", path, got, hidden)
		}
	}
}

func TestCreateHoldsCiliumClustersToNamesCiliumTakes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	class := &clusterv1.ClusterClass{}
	if err := h.kube.Get(t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	class.Spec.Variables = append(class.Spec.Variables, clusterv1.ClusterClassVariable{
		Name: "cni", Schema: clusterv1.VariableSchema{OpenAPIV3Schema: clusterv1.JSONSchemaProps{Type: "string", Default: &apiextensionsv1.JSON{Raw: []byte(`"canal"`)}}},
	})
	if err := h.kube.Update(t.Context(), class); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("admin@pve")
	long := strings.Replace(strings.Replace(createBody, `"web"`, `"`+strings.Repeat("a", provisioner.MaxCiliumClusterName+1)+`"`, 1),
		`"storage":"local-lvm"`, `"storage":"local-lvm","cni":"cilium"`, 1)
	rec := b.do("POST", "/api/clusters", long)
	expect(t, rec, http.StatusUnprocessableEntity)
	if !strings.Contains(rec.Body.String(), "at most 32 characters with Cilium") {
		t.Errorf("a long name for a Cilium cluster: %s", rec.Body)
	}
}
