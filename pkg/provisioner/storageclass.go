package provisioner

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// defaultStorageClass is the name of the StorageClass the CSI plugin's chart creates in every
// cluster.
const defaultStorageClass = "proxmox"

// driveParameters are the drive options of the default StorageClass. discard lets the storage take
// back the blocks a pod frees: without it a thin pool only ever grows. backup stays at the plugin's
// own default of off.
func driveParameters() map[string]any {
	return map[string]any{"discard": "on", "ssd": "true"}
}

// defaultStorageClassParameters are the parameters of the default StorageClass that Kubehenge sets.
func defaultStorageClassParameters(storage string) map[string]string {
	return map[string]string{"storage": storage, "discard": "on", "ssd": "true"}
}

// retireStaleStorageClass deletes the cluster's default StorageClass when its parameters are not
// the ones Kubehenge sets, because Kubernetes refuses a change to them and the CSI plugin's release
// would never upgrade. The release creates it again with the right ones. Volumes keep working, as
// they name the class only as a string. It reports whether it deleted the class.
func (r *Reconciler) retireStaleStorageClass(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	storage, set, err := optionalVariable[string](cluster, "storage")
	if err != nil || !set {
		return false, err
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		// Without a kubeconfig the cluster has no StorageClass to retire.
		return false, err
	}
	class := &storagev1.StorageClass{}
	if err = workload.client.Get(ctx, client.ObjectKey{Name: defaultStorageClass}, class); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		// The next resync looks again: a cluster whose API does not answer has more to wait on than this.
		ctrl.LoggerFrom(ctx).Info("cannot read the cluster's default StorageClass", "error", err.Error())
		return false, nil
	}
	if !storageClassStale(class, storage) {
		return false, nil
	}
	if err = workload.client.Delete(ctx, class, client.Preconditions{UID: &class.UID}); err != nil && !apierrors.IsNotFound(err) {
		r.Remotes.forget(cluster)
		return false, fmt.Errorf("deleting the default StorageClass %s to create it again: %w", defaultStorageClass, err)
	}
	ctrl.LoggerFrom(ctx).Info("deleted the default StorageClass so that the CSI plugin creates it with the current parameters")
	if r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeNormal, "StorageClassRecreated", "RecreateStorageClass",
			"the StorageClass %s has other parameters than the cluster's storage asks for and is created again", defaultStorageClass)
	}
	return true, nil
}

// storageClassStale reports whether any parameter Kubehenge sets differs on the class.
func storageClassStale(class *storagev1.StorageClass, storage string) bool {
	for k, want := range defaultStorageClassParameters(storage) {
		if class.Parameters[k] != want {
			return true
		}
	}
	return false
}
