// Package provisioner gives each workload cluster its own corner of Proxmox VE before Cluster API
// creates any of its VMs: a pool for the VMs, a user and token scoped to that pool for the
// cluster's cloud controller manager and CSI plugin, and a VM that owns the cluster's volumes. It
// delivers the token to the cluster, and removes it all once the cluster's VMs are gone.
package provisioner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Proxmox VE's limit on the part of a user ID before the realm.
	maxUserName = 64
	// A VM name is a DNS name; one label keeps it valid.
	maxVMName = 63
)

// Characters Proxmox VE allows in pool and user names, other than the separators added here.
var poolSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Names are the Proxmox VE objects of one workload cluster.
type Names struct {
	// Pool holds the cluster's VMs, nested under the workload pool. The ClusterClasses place the
	// VMs in the same pool, so the two must agree: <workload pool>/<namespace>.<name>.
	Pool string
	// User signs in only through Token, carries the cluster's ACLs and belongs to the clusters'
	// group, which holds the storage and node rights all clusters share.
	User string
	// Token is the token's ID without the user, as Proxmox VE's API addresses it.
	Token string
	// VolumeOwner names the VM that owns the cluster's volumes.
	VolumeOwner string
}

// VolumeHolders names the VMs that hold the copies of the cluster's volumes, made for its volume
// snapshots, restores and clones, as unused disks. It carries the owner's whole cluster identity.
func (n Names) VolumeHolders() string {
	return strings.TrimSuffix(n.VolumeOwner, volumeOwnerSuffix) + "-copies"
}

// Proxmox VE nests pools three deep; each cluster's pool takes the last level.
const maxWorkloadPoolDepth = 2

// ValidWorkloadPool tells whether pool can hold the clusters' pools: a pool name, nested at most
// one level.
func ValidWorkloadPool(pool string) error {
	segments := strings.Split(pool, "/")
	if len(segments) > maxWorkloadPoolDepth {
		return fmt.Errorf("workload pool %q is nested %d deep; Proxmox VE leaves room for the clusters' pools under at most %d", pool, len(segments), maxWorkloadPoolDepth)
	}
	for _, s := range segments {
		if !poolSegment.MatchString(s) {
			return fmt.Errorf("workload pool %q is not a Proxmox VE pool name", pool)
		}
	}
	return nil
}

// InterfaceNamespace is the namespace the interface creates clusters in unless it is given more,
// the chart's interface.namespace.
const InterfaceNamespace = "clusters"

// clusterNamespace is a namespace's name, a DNS label.
var clusterNamespace = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidExtraNamespace tells whether namespace may hold the interface's clusters beside
// InterfaceNamespace, as the chart's interface.extraNamespaces checks it: a namespace name, and
// none of Kubernetes', the Cluster API providers', cert-manager's or Kubehenge's own, whose Secrets
// the interface would otherwise read.
func ValidExtraNamespace(namespace string) error {
	switch {
	case !clusterNamespace.MatchString(namespace):
		return fmt.Errorf("%q is not a namespace name: lowercase letters, digits and dashes, at most 63", namespace)
	case namespace == InterfaceNamespace:
		return fmt.Errorf("%s is the interface's own namespace already", namespace)
	case namespace == metav1.NamespaceDefault || namespace == "cert-manager" || strings.HasPrefix(namespace, "kube-") || strings.HasSuffix(namespace, "-system"):
		return fmt.Errorf("%s holds Kubernetes', the providers' or Kubehenge's own objects", namespace)
	}
	return nil
}

// NamesFor returns the names for the cluster namespace/name. A namespace holds no dot, so
// <namespace>.<name> is unique where <namespace>-<name> would not be.
func NamesFor(workloadPool, namespace, name string) (Names, error) {
	if err := ValidWorkloadPool(workloadPool); err != nil {
		return Names{}, err
	}
	if !poolSegment.MatchString(namespace) || !poolSegment.MatchString(name) {
		return Names{}, fmt.Errorf("cluster %s/%s: name holds characters a Proxmox VE pool cannot", namespace, name)
	}
	id := namespace + "." + name
	sum := sha256.Sum256([]byte(id))
	digest := hex.EncodeToString(sum[:])
	// A user name holds no "/", which a nested workload pool does. "~" stands in for it: Proxmox VE
	// allows it in a user name but not in a pool's, so the workload pools a/b and a-b, of two
	// management clusters on one Proxmox VE, never give their clusters the same users.
	prefix := strings.ReplaceAll(workloadPool, "/", "~")
	user := prefix + "." + id
	if len(user) > maxUserName {
		user = prefix + "." + digest[:16]
	}
	if len(user) > maxUserName || !userName.MatchString(user) {
		return Names{}, fmt.Errorf("workload pool %q cannot name the clusters' Proxmox VE users", workloadPool)
	}
	// The owner is recognised by its name, so the name carries the whole cluster identity: two
	// clusters of one name in different namespaces must never take each other's owner, whose
	// removal deletes every volume it owns.
	suffix := "-" + digest[:8] + volumeOwnerSuffix
	return Names{
		Pool:        workloadPool + "/" + id,
		User:        user + "@pve",
		Token:       "cluster",
		VolumeOwner: name[:min(len(name), maxVMName-len(suffix))] + suffix,
	}, nil
}

// volumeOwnerSuffix ends the name of the VM that owns a cluster's volumes.
const volumeOwnerSuffix = "-volumes"

// Characters Proxmox VE allows in the user part of a user ID.
var userName = regexp.MustCompile(`^[^\s:/]+$`)

// TokenID is the full token ID, user@realm!token.
func (n Names) TokenID() string {
	return n.User + "!" + n.Token
}
