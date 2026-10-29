package provisioner

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ClusterValidator answers the API server for Kubehenge's clusters as they are created and changed,
// so that what BeforeClusterCreate would refuse later is refused at once, and a change made after
// creation is held to the same: a name, version or size the interface would not offer, a node OS that cannot be built or whose CPU type no node it may
// use offers, control planes too small for the cluster's workers, nodes without
// the memory its add-ons use, logs without monitoring, backups to a target the class lacks or on
// a schedule Velero cannot read, Rancher switched on beside another, or on nodes too small for it,
// a Rancher cluster's nodes made smaller than it needs, load-balancer addresses that clash with the
// nodes' network or another cluster's, Datastore.Allocate on a storage that is not the cluster's
// alone, volume snapshots where they cannot be taken, Sys.Audit on / where no cluster may hold it,
// autoscaled pools whose bounds the autoscaler cannot work with or whose nodes would run out of
// addresses, and a Talos release that does not run the Kubernetes version or that Talos cannot
// upgrade to from the one before. A cluster that already fell short before a change is not held to it for unrelated
// edits.
type ClusterValidator struct {
	Hooks *Hooks
}

var _ admission.Validator[*clusterv1.Cluster] = &ClusterValidator{}

// ValidateCreate implements admission.Validator.
func (v *ClusterValidator) ValidateCreate(ctx context.Context, c *clusterv1.Cluster) (admission.Warnings, error) {
	return nil, v.validate(ctx, nil, c)
}

// ValidateUpdate implements admission.Validator.
func (v *ClusterValidator) ValidateUpdate(ctx context.Context, old, c *clusterv1.Cluster) (admission.Warnings, error) {
	return nil, v.validate(ctx, old, c)
}

// ValidateDelete implements admission.Validator.
func (*ClusterValidator) ValidateDelete(context.Context, *clusterv1.Cluster) (admission.Warnings, error) {
	return nil, nil
}

func (v *ClusterValidator) validate(ctx context.Context, old, c *clusterv1.Cluster) error {
	if !Owns(c, v.Hooks.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
		return nil
	}
	r, err := rancherOf(c)
	if err != nil {
		return err
	}
	class := &clusterv1.ClusterClass{}
	if err := v.Hooks.Reader.Get(ctx, client.ObjectKey{Namespace: v.Hooks.ClassNamespace, Name: c.Spec.Topology.ClassRef.Name}, class); err != nil {
		if apierrors.IsNotFound(err) {
			// Cluster API refuses a cluster of a class that does not exist.
			return nil
		}
		return fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	var errs field.ErrorList
	topology := field.NewPath("spec", "topology")
	errs = append(errs, validateTopology(old, c)...)
	errs = append(errs, v.validateNodeOS(ctx, old, c)...)
	if err := checkControlPlaneSize(old, c, class); err != nil {
		errs = append(errs, field.Forbidden(topology, err.Error()))
	}
	if err := checkAddonRoom(old, c, class); err != nil {
		errs = append(errs, field.Forbidden(topology, err.Error()))
	}
	if r.Install {
		errs = append(errs, v.validateRancher(ctx, old, c, class)...)
	}
	errs = append(errs, validateClusterMesh(old, c)...)
	if err := checkLogs(c); err != nil {
		errs = append(errs, field.Forbidden(topology.Child("variables").Key(logsAddon), err.Error()))
	}
	if err := checkBackups(old, c, class); err != nil {
		errs = append(errs, field.Invalid(topology.Child("variables").Key(backupsVariable), backupsVariable, err.Error()))
	}
	errs = append(errs, validateCertificates(c)...)
	errs = append(errs, validateGitOps(c)...)
	errs = append(errs, validateAutoscaling(c)...)
	if err := checkEtcdSnapshots(old, c, class); err != nil {
		errs = append(errs, field.Invalid(topology.Child("variables").Key(etcdSnapshotsVariable), etcdSnapshotsVariable, err.Error()))
	}
	if err := checkNodeAddresses(old, c); err != nil {
		errs = append(errs, field.Forbidden(topology.Child("variables").Key("network"), err.Error()))
	}
	if err := checkTalos(old, c); err != nil {
		errs = append(errs, field.Forbidden(topology.Child("variables").Key(TalosVersionVariable), err.Error()))
	}
	if addressesChanged(old, c) {
		if err := v.Hooks.checkAddresses(ctx, c); err != nil {
			errs = append(errs, field.Forbidden(topology.Child("variables").Key("network"), err.Error()))
		}
	}
	errs = append(errs, v.validateGrants(ctx, old, c, class)...)
	if len(errs) == 0 {
		return nil
	}
	// Invalid, not the plain denial a validator's error becomes, so that the interface shows the
	// reason as it does the API server's own validation.
	return apierrors.NewInvalid(clusterv1.GroupVersion.WithKind("Cluster").GroupKind(), c.Name, errs) //nolint:wrapcheck // made here, not passed on
}

// validateNodeOS checks the node OS when it changes, and when the nodes the cluster may use do, which
// may leave none whose CPU runs it.
func (v *ClusterValidator) validateNodeOS(ctx context.Context, old, c *clusterv1.Cluster) field.ErrorList {
	if old != nil && !variableChanged(old, c, NodeOSVariable) && !variableChanged(old, c, allowedNodesVariable) {
		return nil
	}
	if err := v.Hooks.checkNodeOS(ctx, c); err != nil {
		return field.ErrorList{field.Forbidden(field.NewPath("spec", "topology", "variables").Key(NodeOSVariable), err.Error())}
	}
	return nil
}

// validateGrants holds the privileges a cluster asks for beyond its pool to what the management
// cluster allows, when the variables that ask for them change.
func (v *ClusterValidator) validateGrants(ctx context.Context, old, c *clusterv1.Cluster, class *clusterv1.ClusterClass) field.ErrorList {
	var errs field.ErrorList
	variables := field.NewPath("spec", "topology", "variables")
	if old == nil || variableChanged(old, c, csiVariable) || variableChanged(old, c, "storage") {
		if err := v.Hooks.checkDatastoreAllocate(ctx, c, class); err != nil {
			errs = append(errs, field.Forbidden(variables.Key(csiVariable), err.Error()))
		}
		if err := v.Hooks.checkVolumeSnapshots(ctx, c, class); err != nil {
			errs = append(errs, field.Forbidden(variables.Key(csiVariable), err.Error()))
		}
	}
	if old == nil || variableChanged(old, c, ccmVariable) {
		if err := v.Hooks.checkSysAudit(c); err != nil {
			errs = append(errs, field.Forbidden(variables.Key(ccmVariable), err.Error()))
		}
	}
	return errs
}

// validateRancher holds a cluster that installs Rancher to what Rancher needs.
func (v *ClusterValidator) validateRancher(ctx context.Context, old, c *clusterv1.Cluster, class *clusterv1.ClusterClass) field.ErrorList {
	switchedOn := old == nil
	if old != nil {
		was, err := rancherOf(old)
		switchedOn = err == nil && !was.Install
	}
	var errs field.ErrorList
	topology := field.NewPath("spec", "topology")
	if switchedOn {
		if err := v.Hooks.checkRancher(ctx, c, c.Spec.Topology.Version); err != nil {
			errs = append(errs, field.Forbidden(topology.Child("variables"), err.Error()))
		}
	}
	if err := checkRancherNodes(c, class); err != nil && (switchedOn || checkRancherNodes(old, class) == nil) {
		errs = append(errs, field.Forbidden(topology, err.Error()))
	}
	return errs
}

// validateClusterMesh holds a cluster to what Cilium takes: a name of at most 32 characters for a
// new cluster that runs it, Cluster Mesh only beside it, peers other than the cluster itself, and a
// Cilium cluster id that stays once given.
func validateClusterMesh(old, c *clusterv1.Cluster) field.ErrorList {
	var errs field.ErrorList
	cni, _, err := optionalVariable[string](c, "cni")
	if err != nil {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "topology", "variables"), "cni", err.Error())}
	}
	if old == nil && cni == cniCilium && len(c.Name) > MaxCiliumClusterName {
		errs = append(errs, field.TooLong(field.NewPath("metadata", "name"), c.Name, MaxCiliumClusterName))
	}
	mesh, _, err := optionalVariable[ClusterMesh](c, clusterMeshVariable)
	variables := field.NewPath("spec", "topology", "variables").Key(clusterMeshVariable)
	switch {
	case err != nil:
		errs = append(errs, field.Invalid(variables, clusterMeshVariable, err.Error()))
	case mesh.Enabled && cni != cniCilium:
		errs = append(errs, field.Forbidden(variables, "Cluster Mesh links clusters that run Cilium; this one runs "+cmp.Or(cni, "its distribution's own CNI")))
	case slices.Contains(mesh.Peers, c.Name):
		errs = append(errs, field.Invalid(variables.Child("peers"), c.Name, "a cluster cannot list itself"))
	}
	if old != nil {
		if was, ok := old.Annotations[CiliumClusterIDAnnotation]; ok && c.Annotations[CiliumClusterIDAnnotation] != was {
			errs = append(errs, field.Forbidden(field.NewPath("metadata", "annotations").Key(CiliumClusterIDAnnotation),
				"the Cilium cluster id is fixed once given: changing it would restart every workload and could clash with a peer's"))
		}
	}
	return errs
}

// validateGitOps holds a cluster's GitOps to the host keys of its repository, which a repository
// reached over SSH has to give, and to the identity provider its web interface signs in with:
// Flux's, and Argo CD's administrators, need the cluster's first OIDC provider.
func validateGitOps(c *clusterv1.Cluster) field.ErrorList {
	path := field.NewPath("spec", "topology", "variables").Key(gitopsVariable)
	g, _, err := optionalVariable[GitOps](c, gitopsVariable)
	if err != nil {
		return field.ErrorList{field.Invalid(path, nil, err.Error())}
	}
	if g.Enabled && g.sshURL() && g.KnownHosts != "" {
		if err := checkKnownHosts(g.KnownHosts, g.Repository.URL); err != nil {
			return field.ErrorList{field.Invalid(path.Child("knownHosts"), nil, err.Error())}
		}
	}
	if g.Enabled && g.knownHostsNeeded() && g.KnownHosts == "" {
		return field.ErrorList{field.Required(path.Child("knownHosts"),
			"a repository reached over SSH needs its host's keys as known_hosts lines; Kubehenge does not read them from the network")}
	}
	if !g.Enabled || (!g.UI && len(g.Admins) == 0) {
		return nil
	}
	provider, err := signInProvider(c)
	if err != nil {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "topology", "variables").Key("oidc"), nil, err.Error())}
	}
	if provider != nil {
		return nil
	}
	if g.UI {
		return field.ErrorList{field.Forbidden(path.Child("ui"), "Flux's web interface signs in through the cluster's first OIDC provider, and the cluster has none")}
	}
	return field.ErrorList{field.Forbidden(path.Child("admins"), "Argo CD's administrators sign in through the cluster's first OIDC provider, and the cluster has none")}
}
