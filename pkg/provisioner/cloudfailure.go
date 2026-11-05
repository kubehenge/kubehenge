package provisioner

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// CloudFailedReason is the reason of KubehengeReadyCondition, and of the Warning Event beside it,
// when the cluster's Proxmox VE pool, user, token or volume owner could not be made.
const CloudFailedReason = "ProxmoxObjectsFailed"

const cloudErrorPrefix = "creating the cluster's Proxmox VE objects: "

// cloudError is a failure making a cluster's Proxmox VE objects.
type cloudError struct{ err error }

func (e *cloudError) Error() string { return cloudErrorPrefix + e.err.Error() }
func (e *cloudError) Unwrap() error { return e.err }

// failureReason is the reason a failed reconcile is recorded under.
func failureReason(err error) string {
	if _, ok := errors.AsType[*cloudError](err); ok {
		return CloudFailedReason
	}
	return ReconcileFailedReason
}

// cloudFailure is why the cluster's Proxmox VE objects could not be made, from the condition the
// reconciler last recorded, or "" when the last reconcile did not fail on them.
func cloudFailure(c *clusterv1.Cluster) string {
	cond := meta.FindStatusCondition(c.Status.Conditions, KubehengeReadyCondition)
	if cond == nil || cond.Reason != CloudFailedReason {
		return ""
	}
	return strings.TrimPrefix(cond.Message, cloudErrorPrefix)
}

// checkStorageListed refuses a storage outside the choices the class offers. Those are the module's
// workload_datastores: the clusters' tokens hold rights on those storages alone, so a VM or volume
// on another fails with a permission error.
func checkStorageListed(c *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	storage, _, err := optionalVariable[string](c, "storage")
	if err != nil || storage == "" {
		return err
	}
	allowed := StorageChoices(class)
	if allowed == nil || slices.Contains(allowed, storage) {
		return nil
	}
	return errors.New(StorageNotListed(storage, allowed))
}

// StorageNotListed is the refusal for a storage the management cluster gives its clusters no
// rights on.
func StorageNotListed(storage string, allowed []string) string {
	return fmt.Sprintf("storage %s is not in workload_datastores (%s), so the clusters' tokens have no rights there. "+
		"Add it to workload_datastores of the management cluster's module and apply it", storage, strings.Join(allowed, ", "))
}

// StorageChoices are the storages the class's storage variable offers, or nil when it offers any.
func StorageChoices(class *clusterv1.ClusterClass) []string {
	for i := range class.Spec.Variables {
		v := &class.Spec.Variables[i]
		if v.Name != "storage" || len(v.Schema.OpenAPIV3Schema.Enum) == 0 {
			continue
		}
		var out []string
		for _, e := range v.Schema.OpenAPIV3Schema.Enum {
			var s string
			if json.Unmarshal(e.Raw, &s) == nil {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
