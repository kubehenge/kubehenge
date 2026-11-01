package provisioner

import (
	"context"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func rangesString(c *clusterv1.Cluster) string {
	return strings.Join(c.Spec.ClusterNetwork.Pods.CIDRBlocks, ",") + " " + strings.Join(c.Spec.ClusterNetwork.Services.CIDRBlocks, ",")
}

func withCNI(c *clusterv1.Cluster, cni string) *clusterv1.Cluster {
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"` + cni + `"`)}})
	return c
}

func TestDefaulterChoosesRangesClearOfTheNodes(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	d := &ClusterDefaulter{Hooks: hooks}
	defaulted := func(c *clusterv1.Cluster) string {
		t.Helper()
		if err := d.Default(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		return rangesString(c)
	}
	if got := defaulted(networkCluster("10.0.0.1", 24)); got != "10.42.0.0/16 10.43.0.0/16" {
		t.Errorf("the distribution's own ranges: %s", got)
	}
	// Nodes in 10.42.0.0/15 take both of the distribution's ranges.
	if got := defaulted(networkCluster("10.42.0.1", 15)); got != "10.244.0.0/16 10.96.0.0/16" {
		t.Errorf("ranges clear of the nodes: %s", got)
	}
	asked := networkCluster("10.0.0.1", 24)
	asked.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"172.30.0.0/16"}
	asked.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"172.31.0.0/16"}
	if got := defaulted(asked); got != "172.30.0.0/16 172.31.0.0/16" {
		t.Errorf("the ranges asked for: %s", got)
	}
	// The one asked for is kept clear of before the other is chosen.
	half := networkCluster("10.0.0.1", 24)
	half.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"10.42.0.0/16"}
	if got := defaulted(half); got != "10.244.0.0/16 10.42.0.0/16" {
		t.Errorf("a pod range beside the service range asked for: %s", got)
	}
	// 10.0.0.0/8 holds the distribution's ranges and every candidate but 172.21.0.0/16 and 172.20.0.0/16.
	if got := defaulted(networkCluster("10.0.0.1", 8)); got != "172.21.0.0/16 172.20.0.0/16" {
		t.Errorf("nodes in 10.0.0.0/8: %s", got)
	}
	full := networkCluster("10.0.0.1", 0)
	if err := d.Default(context.Background(), full); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "every pod range Kubehenge tries overlaps the nodes' network") {
		t.Errorf("no candidate clear of the nodes: %v", err)
	}

	for name, c := range map[string]*clusterv1.Cluster{
		"of another class namespace": func() *clusterv1.Cluster {
			c := networkCluster("10.0.0.1", 24)
			c.Spec.Topology.ClassRef.Namespace = "elsewhere"
			return c
		}(),
		"without a network": cluster(),
		"being deleted": func() *clusterv1.Cluster {
			c := networkCluster("10.0.0.1", 24)
			c.DeletionTimestamp = new(metav1.Now())
			return c
		}(),
	} {
		if got := defaulted(c); got != " " {
			t.Errorf("a cluster %s got ranges %s", name, got)
		}
	}
}

func TestDefaulterKeepsCiliumClustersApart(t *testing.T) {
	t.Parallel()
	two := networkCluster("10.0.0.1", 24)
	two.Name = "two"
	two.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.244.0.0/16"}
	two.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"10.96.0.0/16"}
	one := networkCluster("10.0.0.1", 24)
	one.Name, one.Namespace = "one", "default"
	foreign := networkCluster("10.0.0.1", 24)
	foreign.Name, foreign.Spec.Topology.ClassRef.Name = "foreign", "someone-elses"
	foreign.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"172.21.0.0/16"}
	hooks, kube := hooksFor(t, one, two, foreign)
	d := &ClusterDefaulter{Hooks: hooks}
	create := func(name, cni string) *clusterv1.Cluster {
		t.Helper()
		c := networkCluster("10.0.0.1", 24)
		c.Name = name
		if cni != "" {
			withCNI(c, cni)
		}
		if err := d.Default(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if err := kube.Create(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	// one has RKE2's own ranges, two Kubernetes' usual ones. Neither runs Cilium, and foreign is no
	// cluster of Kubehenge's.
	if got := rangesString(create("canal", "")); got != "10.42.0.0/16 10.43.0.0/16" {
		t.Errorf("a cluster without Cilium got %s, want RKE2's own ranges", got)
	}
	if got := rangesString(create("first", cniCilium)); got != "172.21.0.0/16 10.43.0.0/16" {
		t.Errorf("the first Cilium cluster got %s, want pods clear of every cluster's pods and services", got)
	}
	if got := rangesString(create("second", cniCilium)); got != "10.201.0.0/16 10.43.0.0/16" {
		t.Errorf("the second Cilium cluster got %s, want pods clear of the first's too", got)
	}
	// A cluster is not kept clear of itself, as when the API server asks again.
	again := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "second"}, again); err != nil {
		t.Fatal(err)
	}
	again.Spec.ClusterNetwork = clusterv1.ClusterNetwork{}
	if err := d.Default(context.Background(), again); err != nil || rangesString(again) != "10.201.0.0/16 10.43.0.0/16" {
		t.Errorf("the second Cilium cluster asked about again: %s, %v", rangesString(again), err)
	}

	// A class that runs Cilium by default.
	class := &clusterv1.ClusterClass{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	class.Spec.Variables = append(class.Spec.Variables, clusterv1.ClusterClassVariable{
		Name: "cni", Schema: clusterv1.VariableSchema{OpenAPIV3Schema: clusterv1.JSONSchemaProps{Type: "string", Default: &apiextensionsv1.JSON{Raw: []byte(`"cilium"`)}}},
	})
	if err := kube.Update(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	if got := rangesString(create("by-default", "")); got == "10.42.0.0/16 10.43.0.0/16" {
		t.Errorf("a cluster of a class that runs Cilium by default got the ranges of the others: %s", got)
	}
}

func TestCheckRangesRefusesRangesThatAreNoNetworksOrOverlap(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	class := &clusterv1.ClusterClass{}
	if err := hooks.Reader.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		pods, services []string
		refusal        string
	}{
		"over the nodes":  {[]string{"10.0.0.0/16"}, nil, "pod range 10.0.0.0/16 overlaps the nodes' network 10.0.0.0/24"},
		"over each other": {[]string{"172.30.0.0/16"}, []string{"172.30.128.0/17"}, "the service range 172.30.128.0/17 overlaps the pod range 172.30.0.0/16"},
		"default over":    {[]string{"10.43.0.0/17"}, nil, "the service range 10.43.0.0/16 overlaps the pod range 10.43.0.0/17"},
		"not a network":   {nil, []string{"172.31.0.1/16"}, "service range: 172.31.0.1/16 is no IPv4 network"},
		"not IPv4":        {[]string{"fd00::/64"}, nil, "pod range: fd00::/64 is no IPv4 network"},
		"no range":        {[]string{"everywhere"}, nil, "pod range: everywhere is no IPv4 network"},
	} {
		c := networkCluster("10.0.0.1", 24)
		c.Spec.ClusterNetwork.Pods.CIDRBlocks, c.Spec.ClusterNetwork.Services.CIDRBlocks = tc.pods, tc.services
		if err := checkRanges(c, class); err == nil || !strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("%s: %v", name, err)
		}
	}
	c := networkCluster("10.0.0.1", 24)
	c.Spec.ClusterNetwork.Pods.CIDRBlocks, c.Spec.ClusterNetwork.Services.CIDRBlocks = []string{"172.30.0.0/16"}, []string{"172.31.0.0/16"}
	if err := checkRanges(c, class); err != nil {
		t.Errorf("ranges apart: %v", err)
	}
}

func TestValidatorChecksTheRangesOfNewClusters(t *testing.T) {
	t.Parallel()
	hooks, _ := hooksFor(t)
	v := &ClusterValidator{Hooks: hooks}
	c := networkCluster("10.0.0.1", 24)
	c.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{"10.0.0.0/16"}
	if _, err := v.ValidateCreate(context.Background(), c); !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "overlaps the nodes' network") {
		t.Errorf("a new cluster's pods over its nodes: %v", err)
	}
	// A running cluster's ranges are what they are. Changing them is Cluster API's to refuse.
	if _, err := v.ValidateUpdate(context.Background(), c, c); err != nil && strings.Contains(err.Error(), "overlaps the nodes' network") {
		t.Errorf("a running cluster held to its ranges again: %v", err)
	}
}
