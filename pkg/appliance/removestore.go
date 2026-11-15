package appliance

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// storeStopTimeout bounds waiting for the backup volume's store to stop and its volume to detach
// before a destroy.
const storeStopTimeout = 5 * time.Minute

// storeAttachment is the backup volume's attachment to a node, as the CSI plugin left it.
type storeAttachment struct {
	// Node is the Kubernetes node, which is also its VM's name.
	Node string
	// Volume is the plugin's volume handle: region/zone/storage/disk.
	Volume string
}

const volumeHandleParts = 4

// stopBackupStore stops the backup volume's store before a destroy of the module and waits until
// no pod of it is left and the volume is detached. The destroy uninstalls the CSI plugin and
// deletes its token before the namespace the store runs in, and a pod still mounting the volume
// then never terminates, which holds that namespace, and the release that made it, past its
// timeout. A management cluster out of reach, or without a store, is passed by. A volume the
// plugin does not detach in time the appliance detaches itself with the ticket.
func (a *Appliance) stopBackupStore(ctx context.Context, j *Job, env []string, t *pve.Ticket) error {
	kube, err := a.clusterOf(ctx, env)
	if err != nil || kube == nil {
		return nil //nolint:nilerr // a cluster out of reach is removed all the same
	}
	return stopStore(ctx, j, kube, storeStopTimeout, func(ctx context.Context, at storeAttachment) error {
		return a.detachVolume(ctx, t, at)
	})
}

// stopStore scales the store to zero and waits for it to go. When only the volume's attachment is
// left, detach takes the volume off the node's VM. A store that does not stop is started again,
// so that a failed removal leaves the backups running.
func stopStore(ctx context.Context, j *Job, kube kubernetes.Interface, timeout time.Duration, detach func(context.Context, storeAttachment) error) error {
	d, err := kube.AppsV1().Deployments(backupstore.Namespace).Get(ctx, backupstore.Service, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		j.Add("The management cluster does not answer, so its backup volume's store is not stopped: " + err.Error())
		return nil
	}
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return fmt.Errorf("the backup volume's store's selector: %w", err)
	}
	j.Add("Stopping the backup volume's store, so that its volume is detached before the CSI plugin goes")
	if err = scaleStore(ctx, kube, 0); err != nil {
		return fmt.Errorf("stopping the backup volume's store: %w", err)
	}
	err = waitStoreGone(ctx, kube, selector.String(), timeout, j, detach)
	if err == nil {
		return nil
	}
	start, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if serr := scaleStore(start, kube, replicas); serr != nil {
		return errors.Join(err, fmt.Errorf("starting the backup volume's store again: %w", serr))
	}
	return err
}

func scaleStore(ctx context.Context, kube kubernetes.Interface, replicas int32) error {
	scale := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
	_, err := kube.AppsV1().Deployments(backupstore.Namespace).Patch(ctx, backupstore.Service, types.MergePatchType, scale, metav1.PatchOptions{})
	return err //nolint:wrapcheck // the callers say which change failed
}

func waitStoreGone(ctx context.Context, kube kubernetes.Interface, selector string, timeout time.Duration, j *Job, detach func(context.Context, storeAttachment) error) error {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		left, at, err := storeLeft(wait, kube, selector)
		if err == nil && left == "" {
			return nil
		}
		if err == nil {
			err = errors.New(left)
		}
		select {
		case <-wait.Done():
			if at == nil {
				return fmt.Errorf("the backup volume's store did not stop, so the removal would hang deleting its namespace: %w", err)
			}
			j.Add(fmt.Sprintf("The CSI plugin did not detach the backup volume from %s, so the appliance detaches it", at.Node))
			return detach(ctx, *at)
		case <-time.After(storeReadyPoll):
		}
	}
}

// storeLeft names what of the stopped store is still there: a pod, or the volume's attachment,
// which it also returns. Empty means nothing.
func storeLeft(ctx context.Context, kube kubernetes.Interface, selector string) (string, *storeAttachment, error) {
	pods, err := kube.CoreV1().Pods(backupstore.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", nil, fmt.Errorf("the backup volume's store's pods: %w", err)
	}
	if len(pods.Items) > 0 {
		return "pod " + pods.Items[0].Name + " is still there", nil, nil
	}
	claim, err := kube.CoreV1().PersistentVolumeClaims(backupstore.Namespace).Get(ctx, backupstore.Claim, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || (err == nil && claim.Spec.VolumeName == "") {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("the backup volume's claim: %w", err)
	}
	attachments, err := kube.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("the volumes' attachments: %w", err)
	}
	for i := range attachments.Items {
		spec := &attachments.Items[i].Spec
		if pv := spec.Source.PersistentVolumeName; pv == nil || *pv != claim.Spec.VolumeName {
			continue
		}
		left := "the volume is still attached to node " + spec.NodeName
		volume, err := kube.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
		if err != nil || volume.Spec.CSI == nil {
			return left, nil, nil //nolint:nilerr // without its handle the volume cannot be detached here, and the attachment is reported instead
		}
		return left, &storeAttachment{Node: spec.NodeName, Volume: volume.Spec.CSI.VolumeHandle}, nil
	}
	return "", nil, nil
}

// detachVolume takes the volume off the VM named after the node, as the CSI plugin does: it lifts
// the VM's protection for the unlink alone, without the plugin's marker, so that the plugin leaves
// the VM to this run. A volume no VM of that name holds is detached already. An unlink of a disk
// another VM owns only removes it from the configuration (qemu-server), so the backups stay.
func (a *Appliance) detachVolume(ctx context.Context, t *pve.Ticket, at storeAttachment) error {
	parts := strings.Split(at.Volume, "/")
	if len(parts) != volumeHandleParts {
		return fmt.Errorf("the backup volume's handle %q is not region/zone/storage/disk", at.Volume)
	}
	disk := parts[2] + ":" + parts[3]
	c, err := pve.ConnectSession(pve.Config{URL: a.ProxmoxURL, CAFile: a.ProxmoxCAFile, Insecure: a.ProxmoxInsecure}, t)
	if err != nil {
		return fmt.Errorf("detaching %s from VM %s: %w", disk, at.Node, err)
	}
	var vms []struct {
		VMID int    `json:"vmid"`
		Name string `json:"name"`
		Node string `json:"node"`
		Type string `json:"type"`
	}
	if err := c.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return fmt.Errorf("detaching %s from VM %s: listing the VMs: %w", disk, at.Node, err)
	}
	for _, v := range vms {
		if v.Type != "qemu" || v.Name != at.Node {
			continue
		}
		if err := unlinkVolume(ctx, c, v.Node, v.VMID, disk); err != nil {
			return fmt.Errorf("detaching %s from VM %d (%s): %w", disk, v.VMID, v.Name, err)
		}
		return nil
	}
	return nil
}

// unlinkVolume unlinks disk from VM id on node, lifting its protection for the unlink and setting
// it again after.
func unlinkVolume(ctx context.Context, c *proxmox.Client, node string, id int, disk string) (err error) {
	vm := fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(node), id)
	var config map[string]any
	if err = c.Get(ctx, vm+"/config", &config); err != nil {
		return fmt.Errorf("reading the configuration: %w", err)
	}
	drive := ""
	for key, value := range config {
		if s, ok := value.(string); ok && strings.Split(s, ",")[0] == disk && driveKey(key) {
			drive = key
		}
	}
	if drive == "" {
		return nil
	}
	if p, ok := config[protectionOption].(float64); ok && p == float64(protected) {
		lift := map[string]any{protectionOption: int(unprotected), "digest": config["digest"]}
		if err = c.Put(ctx, vm+"/config", lift, nil); err != nil {
			return fmt.Errorf("lifting the protection: %w", err)
		}
		defer func() {
			again, cancel := context.WithTimeout(context.WithoutCancel(ctx), protectAgainTimeout)
			defer cancel()
			if perr := c.Put(again, vm+"/config", map[string]any{protectionOption: int(protected)}, nil); perr != nil {
				err = errors.Join(err, fmt.Errorf("the VM is left without protection, set it with qm set %d --protection 1: %w", id, perr))
			}
		}()
	}
	if err = c.Put(ctx, vm+"/unlink", map[string]any{"idlist": drive}, nil); err != nil {
		return fmt.Errorf("unlinking %s: %w", drive, err)
	}
	return nil
}

// driveKey says whether a configuration key names a disk drive.
func driveKey(key string) bool {
	for _, bus := range []string{"scsi", "virtio", "sata", "ide"} {
		if n, ok := strings.CutPrefix(key, bus); ok && n != "" && strings.Trim(n, "0123456789") == "" {
			return true
		}
	}
	return false
}
