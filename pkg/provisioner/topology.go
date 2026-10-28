package provisioner

import (
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// MaxClusterName is the longest cluster name whose machines' host names, <name>-md-0-<three
// random parts>, stay within one DNS label.
const MaxClusterName = 40

// MaxWorkers is the most workers a pool of fixed size may have, as many as an autoscaled one may
// grow to.
const MaxWorkers = MaxAutoscaledWorkers

// VersionFormats is each distribution's form of a Kubernetes version.
var VersionFormats = map[templates.Distribution]*regexp.Regexp{
	templates.Kubeadm: regexp.MustCompile(`^v1\.\d+\.\d+$`),
	templates.RKE2:    regexp.MustCompile(`^v1\.\d+\.\d+\+rke2r\d+$`),
	templates.K3s:     regexp.MustCompile(`^v1\.\d+\.\d+\+k3s\d+$`),
	templates.Talos:   regexp.MustCompile(`^v1\.\d+\.\d+$`),
}

// VersionExample is a version in the distribution's form, for messages.
func VersionExample(d templates.Distribution) string {
	switch d {
	case templates.RKE2:
		return "v1.36.5+rke2r1"
	case templates.K3s:
		return "v1.36.5+k3s1"
	case templates.Kubeadm, templates.Talos:
	}
	return "v1.36.5"
}

// Messages for what the topology rules refuse, shared with the interface so that both say the same.
var (
	ClusterNameRule  = fmt.Sprintf("lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit, at most %d characters", MaxClusterName)
	ControlPlaneRule = "control-plane nodes: 1, 3, 5 or 7, an odd number so etcd keeps a majority"
	WorkersRule      = fmt.Sprintf("workers: 0 to %d", MaxWorkers)
)

// ValidClusterName reports whether name may name a cluster.
func ValidClusterName(name string) bool {
	return len(validation.IsDNS1035Label(name)) == 0 && len(name) <= MaxClusterName
}

// ValidControlPlane reports whether a cluster may run n control planes: an odd number, so that etcd
// keeps a majority when one is lost, up to seven.
func ValidControlPlane(n int32) bool { return n >= 1 && n <= 7 && n%2 == 1 }

// ValidWorkers reports whether a pool of fixed size may run n workers.
func ValidWorkers(n int32) bool { return n >= 0 && n <= MaxWorkers }

// VersionRule says what is wrong with version as a version of distribution d, or returns "".
func VersionRule(d templates.Distribution, version string) string {
	if f := VersionFormats[d]; f != nil && !f.MatchString(version) {
		return fmt.Sprintf("Kubernetes version: %s versions look like %s", d, VersionExample(d))
	}
	return ""
}

// UpgradeRule says what is wrong with moving a cluster from one version to another, or returns "".
// Kubernetes supports upgrading one minor version at a time and never downgrading.
func UpgradeRule(from, to string) string {
	was, ok := parseKubeVersion(from)
	if !ok {
		return ""
	}
	now, ok := parseKubeVersion(to)
	if !ok {
		return ""
	}
	if now.Minor > was.Minor+1 || now.less(was) {
		return fmt.Sprintf("Kubernetes version: from %s, one minor version up at most, and never back", from)
	}
	return ""
}

// validateTopology holds a cluster's name, version and sizes to what the interface offers, so that
// one applied with kubectl or by GitOps gets no further than one made in the interface.
func validateTopology(old, c *clusterv1.Cluster) field.ErrorList {
	var errs field.ErrorList
	topology := field.NewPath("spec", "topology")
	if old == nil && !ValidClusterName(c.Name) {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), c.Name, ClusterNameRule))
	}
	if old == nil || old.Spec.Topology.Version != c.Spec.Topology.Version {
		if msg := VersionRule(distributionOf(c), c.Spec.Topology.Version); msg != "" {
			errs = append(errs, field.Invalid(topology.Child("version"), c.Spec.Topology.Version, msg))
		} else if old != nil {
			if msg := UpgradeRule(old.Spec.Topology.Version, c.Spec.Topology.Version); msg != "" {
				errs = append(errs, field.Invalid(topology.Child("version"), c.Spec.Topology.Version, msg))
			}
		}
	}
	if n := c.Spec.Topology.ControlPlane.Replicas; n != nil && !ValidControlPlane(*n) {
		errs = append(errs, field.Invalid(topology.Child("controlPlane", "replicas"), *n, ControlPlaneRule))
	}
	pools := topology.Child("workers", "machineDeployments")
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		if n := c.Spec.Topology.Workers.MachineDeployments[i].Replicas; n != nil && !ValidWorkers(*n) {
			errs = append(errs, field.Invalid(pools.Index(i).Child("replicas"), *n, WorkersRule+" in each pool"))
		}
	}
	return errs
}
