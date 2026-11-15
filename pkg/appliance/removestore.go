package appliance

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
)

// storeStopTimeout bounds waiting for the backup volume's store to stop and its volume to detach
// before a destroy.
const storeStopTimeout = 5 * time.Minute

// stopBackupStore stops the backup volume's store before a destroy of the module and waits until
// no pod of it is left and the volume is detached. The destroy uninstalls the CSI plugin and
// deletes its token before the namespace the store runs in, and a pod still mounting the volume
// then never terminates, which holds that namespace, and the release that made it, past its
// timeout. A management cluster out of reach, or without a store, is passed by.
func (a *Appliance) stopBackupStore(ctx context.Context, j *Job, env []string) error {
	kube, err := a.clusterOf(ctx, env)
	if err != nil || kube == nil {
		return nil //nolint:nilerr // a cluster out of reach is removed all the same
	}
	return stopStore(ctx, j, kube, storeStopTimeout)
}

func stopStore(ctx context.Context, j *Job, kube kubernetes.Interface, timeout time.Duration) error {
	d, err := kube.AppsV1().Deployments(backupstore.Namespace).Get(ctx, backupstore.Service, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the backup volume's store: %w", err)
	}
	j.Add("Stopping the backup volume's store, so that its volume is detached before the CSI plugin goes")
	stop := []byte(`{"spec":{"replicas":0}}`)
	if _, err = kube.AppsV1().Deployments(backupstore.Namespace).Patch(ctx, backupstore.Service, types.MergePatchType, stop, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("stopping the backup volume's store: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return fmt.Errorf("the backup volume's store's selector: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		left, err := storeLeft(ctx, kube, selector.String())
		if err == nil && left == "" {
			return nil
		}
		if err == nil {
			err = errors.New(left)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the backup volume's store did not stop, so the removal would hang deleting its namespace: %w", err)
		case <-time.After(storeReadyPoll):
		}
	}
}

// storeLeft names what of the stopped store is still there: a pod, or the volume's attachment.
// Empty means nothing.
func storeLeft(ctx context.Context, kube kubernetes.Interface, selector string) (string, error) {
	pods, err := kube.CoreV1().Pods(backupstore.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("the backup volume's store's pods: %w", err)
	}
	if len(pods.Items) > 0 {
		return "pod " + pods.Items[0].Name + " is still there", nil
	}
	claim, err := kube.CoreV1().PersistentVolumeClaims(backupstore.Namespace).Get(ctx, backupstore.Claim, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || (err == nil && claim.Spec.VolumeName == "") {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("the backup volume's claim: %w", err)
	}
	attachments, err := kube.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("the volumes' attachments: %w", err)
	}
	for i := range attachments.Items {
		spec := &attachments.Items[i].Spec
		if pv := spec.Source.PersistentVolumeName; pv != nil && *pv == claim.Spec.VolumeName {
			return "the volume is still attached to node " + spec.NodeName, nil
		}
	}
	return "", nil
}
