//go:build integration

package provisioner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestIntegrationChartNetworkPolicies holds the chart to its NetworkPolicies: nothing reaches a pod
// of Kubehenge's but the controller's webhook port, and a token Job reaches only DNS and Proxmox
// VE's API by its port.
func TestIntegrationChartNetworkPolicies(t *testing.T) {
	t.Parallel()
	for url, port := range map[string]int{
		"https://pve.example.org:8006": 8006,
		"https://pve.example.org":      443,
		"https://[fd00::1]:8443":       8443,
	} {
		values := filepath.Join(t.TempDir(), "values.yaml")
		raw := "proxmox:\n  url: " + url + "\n  ha: true\n  etcdRestore: true\n"
		if err := os.WriteFile(values, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		policies, pods := chartPolicies(t, renderChart(t, "kubehenge", "../../charts/kubehenge", classNamespace, "testdata/integration-values.yaml", values))
		if len(pods[classNamespace]) < 3 {
			t.Fatalf("%s: found the pods of %d workloads, want the controller's, the template builds' and the token Jobs'", url, len(pods[classNamespace]))
		}
		for _, ns := range []string{classNamespace, "kubehenge-autoscaler"} {
			if !slices.ContainsFunc(policies[ns], func(p networkingv1.NetworkPolicy) bool {
				return len(p.Spec.PodSelector.MatchLabels) == 0 && len(p.Spec.PodSelector.MatchExpressions) == 0 &&
					slices.Contains(p.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) && len(p.Spec.Ingress) == 0
			}) {
				t.Errorf("%s: no policy in %s keeps every pod from being reached", url, ns)
			}
		}
		for _, l := range pods[classNamespace] {
			ingress, egress, restricted := podRules(t, policies[classNamespace], l)
			switch l["app.kubernetes.io/name"] {
			case "kubehenge-controller":
				if len(ingress) != 1 || len(ingress[0].From) != 0 || !onlyPorts(ingress[0].Ports, corev1.ProtocolTCP, 9443) {
					t.Errorf("%s: the controller may be reached by %+v, want only its webhook port", url, ingress)
				}
			case "kubehenge-token-job":
				dns := len(egress) == 2 && len(egress[0].To) == 1 && egress[0].To[0].PodSelector != nil &&
					egress[0].To[0].PodSelector.MatchLabels["k8s-app"] == "kube-dns" &&
					egress[0].To[0].NamespaceSelector != nil &&
					egress[0].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "kube-system" &&
					len(egress[0].Ports) == 2
				pve := len(egress) == 2 && len(egress[1].To) == 0 && onlyPorts(egress[1].Ports, corev1.ProtocolTCP, port)
				if len(ingress) != 0 || !restricted || !dns || !pve {
					t.Errorf("%s: a token Job may be reached by %+v and reach %+v, want nothing in and only DNS and port %d out", url, ingress, egress, port)
				}
			default:
				if len(ingress) != 0 {
					t.Errorf("%s: %v may be reached by %+v", url, l, ingress)
				}
			}
		}
	}
}

// chartPolicies parts out the chart's NetworkPolicies and the labels of the pods its workloads
// make, by namespace.
func chartPolicies(t *testing.T, objs []*unstructured.Unstructured) (map[string][]networkingv1.NetworkPolicy, map[string][]map[string]string) {
	t.Helper()
	policies := map[string][]networkingv1.NetworkPolicy{}
	pods := map[string][]map[string]string{}
	podLabels := map[string][]string{
		"Deployment":  {"spec", "template", "metadata", "labels"},
		"PodTemplate": {"template", "metadata", "labels"},
		"CronJob":     {"spec", "jobTemplate", "spec", "template", "metadata", "labels"},
	}
	for _, o := range objs {
		if path, ok := podLabels[o.GetKind()]; ok {
			pods[o.GetNamespace()] = append(pods[o.GetNamespace()], mapAt(o.Object, path...))
		}
		if o.GetKind() != "NetworkPolicy" {
			continue
		}
		var p networkingv1.NetworkPolicy
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &p); err != nil {
			t.Fatal(err)
		}
		policies[p.Namespace] = append(policies[p.Namespace], p)
	}
	return policies, pods
}

// podRules are the ingress and egress rules of the policies that select a pod of these labels, and
// whether any restricts its egress.
func podRules(t *testing.T, policies []networkingv1.NetworkPolicy, l map[string]string) ([]networkingv1.NetworkPolicyIngressRule, []networkingv1.NetworkPolicyEgressRule, bool) {
	t.Helper()
	var ingress []networkingv1.NetworkPolicyIngressRule
	var egress []networkingv1.NetworkPolicyEgressRule
	restricted := false
	for i := range policies {
		sel, err := metav1.LabelSelectorAsSelector(&policies[i].Spec.PodSelector)
		if err != nil {
			t.Fatal(err)
		}
		if !sel.Matches(labels.Set(l)) {
			continue
		}
		ingress = append(ingress, policies[i].Spec.Ingress...)
		if slices.Contains(policies[i].Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
			restricted = true
			egress = append(egress, policies[i].Spec.Egress...)
		}
	}
	return ingress, egress, restricted
}

func onlyPorts(ports []networkingv1.NetworkPolicyPort, protocol corev1.Protocol, port int) bool {
	return len(ports) == 1 && ports[0].Protocol != nil && *ports[0].Protocol == protocol &&
		ports[0].Port != nil && *ports[0].Port == intstr.FromInt32(int32(port)) && ports[0].EndPort == nil //nolint:gosec // a port number
}

// mapAt is the string map at path in an object, or nil.
func mapAt(obj map[string]any, path ...string) map[string]string {
	var cur any = obj
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
