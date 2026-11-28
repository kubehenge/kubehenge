package provisioner

import (
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// MaxClusterName is the longest cluster name whose machines' host names, <name>-md-0-<two random
// parts>, stay within one DNS label with room to spare.
const MaxClusterName = 40

// MaxPoolNames is how long a cluster's name, or its node name prefix, and a worker pool's may be
// together, for the same host names <cluster>-<pool>-<two random parts>: a pool of the longest
// cluster has a name as long as md-0.
const MaxPoolNames = MaxClusterName + len("md-0")

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
	ClusterNameRule  = fmt.Sprintf("up to %d lowercase letters, digits and dashes, starting with a letter, not ending with a dash", MaxClusterName)
	ControlPlaneRule = "control planes: 1, 3, 5 or 7"
	WorkersRule      = fmt.Sprintf("workers: 0 to %d", MaxWorkers)
	PoolNameRule     = "Pool names take lowercase letters, digits and dashes, and start with a letter."
	PoolLengthRule   = fmt.Sprintf("A pool's and its cluster's names take %d characters at most together.", MaxPoolNames)
	NamePrefixRule   = fmt.Sprintf("node name prefix: up to %d lowercase letters, digits and dashes, starting with a letter, not ending with a dash", MaxClusterName)
	PrefixPoolRule   = fmt.Sprintf("A pool's name and the node name prefix take %d characters at most together.", MaxPoolNames)
)

// NodeNamePrefixVariable is what the names of a cluster's VMs and nodes start with in place of the
// cluster's name.
const NodeNamePrefixVariable = "nodeNamePrefix"

// nodeNamePrefix is the cluster's node name prefix, or "" where it sets none.
func nodeNamePrefix(c *clusterv1.Cluster) string {
	p, _, err := optionalVariable[string](c, NodeNamePrefixVariable)
	if err != nil {
		return ""
	}
	return p
}

// PoolNameProblem says what is wrong with pool as the name of a worker pool of the cluster named
// cluster, or returns "".
func PoolNameProblem(cluster, pool string) string {
	switch {
	case len(validation.IsDNS1035Label(pool)) != 0:
		return PoolNameRule
	case len(cluster)+len(pool) > MaxPoolNames:
		return PoolLengthRule
	}
	return ""
}

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
		return "Kubernetes version: use the form " + VersionExample(d)
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
		return fmt.Sprintf("Kubernetes version: from %s, go up one minor version at most, never down", from)
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
	errs = append(errs, NodeTimeoutsOf(&c.Spec.Topology.ControlPlane.Deletion).errors(topology.Child("controlPlane", "deletion"))...)
	errs = append(errs, validateNamePrefix(old, c, topology.Child("variables").Key(NodeNamePrefixVariable))...)
	return append(errs, validatePools(old, c, topology.Child("workers", "machineDeployments"))...)
}

// validateNamePrefix holds a node name prefix, as it is set or changed, to the rules of a cluster's
// name, and to the length a cluster's name may have beside each pool's: the prefix stands in for the
// cluster's name in its VMs' and nodes' names.
func validateNamePrefix(old, c *clusterv1.Cluster, path *field.Path) field.ErrorList {
	p := nodeNamePrefix(c)
	if p == "" || (old != nil && nodeNamePrefix(old) == p) {
		return nil
	}
	if !ValidClusterName(p) {
		return field.ErrorList{field.Invalid(path, p, NamePrefixRule)}
	}
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		if pool := c.Spec.Topology.Workers.MachineDeployments[i].Name; len(p)+len(pool) > MaxPoolNames {
			return field.ErrorList{field.Invalid(path, p, PrefixPoolRule)}
		}
	}
	return nil
}

// validatePools holds each worker pool's workers and name to the rules. Names are held to them as
// pools are added, as a cluster's is as it is created.
func validatePools(old, c *clusterv1.Cluster, pools *field.Path) field.ErrorList {
	var errs field.ErrorList
	named, had := map[string]bool{}, map[string]bool{}
	if old != nil {
		for i := range old.Spec.Topology.Workers.MachineDeployments {
			had[old.Spec.Topology.Workers.MachineDeployments[i].Name] = true
		}
	}
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		md := &c.Spec.Topology.Workers.MachineDeployments[i]
		if n := md.Replicas; n != nil && !ValidWorkers(*n) {
			errs = append(errs, field.Invalid(pools.Index(i).Child("replicas"), *n, WorkersRule+" in each pool"))
		}
		errs = append(errs, NodeTimeoutsOfPool(&md.Deletion).errors(pools.Index(i).Child("deletion"))...)
		problem := ""
		if !had[md.Name] {
			problem = PoolNameProblem(c.Name, md.Name)
			if p := nodeNamePrefix(c); problem == "" && p != "" && len(p)+len(md.Name) > MaxPoolNames {
				problem = PrefixPoolRule
			}
		}
		switch {
		case problem != "":
			errs = append(errs, field.Invalid(pools.Index(i).Child("name"), md.Name, problem))
		case named[md.Name]:
			errs = append(errs, field.Duplicate(pools.Index(i).Child("name"), md.Name))
		}
		named[md.Name] = true
	}
	return errs
}
