package provisioner

import (
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// tolerates is whether a pod with these tolerations may run on a node with the taint, as the
// scheduler and the taint manager read them.
func tolerates(t *testing.T, tolerations any, key string, effect corev1.TaintEffect) bool {
	t.Helper()
	encoded, err := yaml.Marshal(tolerations)
	if err != nil {
		t.Fatal(err)
	}
	var list []corev1.Toleration
	if err := yaml.Unmarshal(encoded, &list); err != nil {
		t.Fatal(err)
	}
	taint := &corev1.Taint{Key: key, Effect: effect}
	return slices.ContainsFunc(list, func(tl corev1.Toleration) bool { return tl.ToleratesTaint(logr.Discard(), taint, false) })
}

// checkCalicoTolerations holds the tolerations of a Calico component that has to run before the
// network does to what a drain needs: everywhere a node is not ready yet, never on a cordoned one.
func checkCalicoTolerations(t *testing.T, what string, tolerations any) {
	t.Helper()
	if tolerates(t, tolerations, corev1.TaintNodeUnschedulable, corev1.TaintEffectNoSchedule) {
		t.Errorf("%s is scheduled onto a node being drained: %v", what, tolerations)
	}
	for _, taint := range []corev1.Taint{
		controlPlaneTaint,
		{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoSchedule},
		{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoExecute},
		{Key: corev1.TaintNodeNetworkUnavailable, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.cloudprovider.kubernetes.io/uninitialized", Effect: corev1.TaintEffectNoSchedule},
	} {
		if !tolerates(t, tolerations, taint.Key, taint.Effect) {
			t.Errorf("%s does not start on a node with %s:%s, before the network does", what, taint.Key, taint.Effect)
		}
	}
}

func TestCalicoKeepsOffNodesBeingDrained(t *testing.T) {
	t.Parallel()
	for _, d := range []templates.Distribution{templates.Kubeadm, templates.K3s} {
		_, values := proxySpecs(t, d, CNI{Name: cniCalico, Pods: "10.244.0.0/16"}, &Addons{})
		checkCalicoTolerations(t, string(d)+"'s Tigera operator", values["web-cni"][tolerationsKey])
	}
	checkCalicoTolerations(t, "the Talos Tigera operator", talosProxies(t, CNI{Name: cniCalico, Pods: "10.244.0.0/16"}, &Addons{})["web-cni"][tolerationsKey])

	s, err := calicoResources(cluster(), CNI{Name: cniCalico, Pods: "10.244.0.0/16"}, testCharts.CalicoImages)
	if err != nil {
		t.Fatal(err)
	}
	for doc := range strings.SplitSeq(string(s.Data["calico.yaml"]), "---\n") {
		var d map[string]any
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatal(err)
		}
		if d["kind"] == "Installation" {
			checkCalicoTolerations(t, "Typha", lookup[[]any](t, d, "spec", "typhaDeployment", "spec", "template", "spec", tolerationsKey))
		}
	}
}

func TestRKE2sOwnCalicoKeepsOffNodesBeingDrained(t *testing.T) {
	t.Parallel()
	secretsOf := func(class, cni string) []*corev1.Secret {
		c := cluster()
		c.Spec.Topology.ClassRef.Name = ClassPrefix + class
		if cni != "" {
			c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: "cni", Value: apiextensionsv1.JSON{Raw: []byte(`"` + cni + `"`)}})
		}
		secrets, err := resourceSetSecrets(c, &corev1.Secret{Name: "proxmox"}, CNI{}, &Addons{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return secrets
	}
	named := func(secrets []*corev1.Secret, name string) *corev1.Secret {
		if i := slices.IndexFunc(secrets, func(s *corev1.Secret) bool { return s.Name == name }); i >= 0 {
			return secrets[i]
		}
		return nil
	}
	s := named(secretsOf("rke2", cniCalico), "web-"+rke2CalicoChart)
	if s == nil {
		t.Fatal("RKE2's Calico is left with its chart's tolerations")
	}
	var config map[string]any
	if err := yaml.Unmarshal(s.Data[rke2CalicoChart+".yaml"], &config); err != nil {
		t.Fatal(err)
	}
	if config["kind"] != "HelmChartConfig" || lookup[string](t, config, "metadata", "name") != rke2CalicoChart || lookup[string](t, config, "metadata", "namespace") != "kube-system" {
		t.Fatalf("configures %v", config["metadata"])
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(lookup[string](t, config, "spec", "valuesContent")), &values); err != nil {
		t.Fatal(err)
	}
	checkCalicoTolerations(t, "RKE2's Tigera operator", values[tolerationsKey])
	checkCalicoTolerations(t, "RKE2's Typha", lookup[[]any](t, values, "installation", "typhaDeployment", "spec", "template", "spec", tolerationsKey))

	for _, other := range []struct{ class, cni string }{{"rke2", ""}, {"rke2", "canal"}, {"rke2", cniCilium}, {"k3s", cniCalico}, {"kubeadm", cniCalico}} {
		if named(secretsOf(other.class, other.cni), "web-"+rke2CalicoChart) != nil {
			t.Errorf("%s with CNI %q configures RKE2's Calico chart", other.class, other.cni)
		}
	}
}
