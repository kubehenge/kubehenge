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

const (
	// storeStopTimeout bounds waiting for the backup volume's store to stop and its volume to
	// detach before a destroy.
	storeStopTimeout = 5 * time.Minute
	// storeDetachTimeout bounds what follows that wait: the last look at the store, the appliance's
	// own detach and starting the store again.
	storeDetachTimeout = 2 * time.Minute
)

// storeAttachment is the backup volume's attachment to a node, as the CSI plugin left it.
type storeAttachment struct {
	// Node is the Kubernetes node.
	Node string
	// SystemUUID is the node's SMBIOS UUID, which its VM's smbios1 holds.
	SystemUUID string
	// Volume is the plugin's volume handle: region/zone/storage/disk.
	Volume string
}

// volumeHandleParts are a handle's region, zone, storage and disk. A disk on file storage holds a
// slash itself (9000/vm-9000-disk-0.raw), so the handle splits into at most four, as the plugin
// splits it.
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
// with the replicas it had, so that a failed removal leaves the backups as they were.
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
	start, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeDetachTimeout)
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

// waitStoreGone polls until nothing of the store is left. Once timeout passes it looks once more,
// with a context of its own, since the deadline may have cut off a look in flight, and detaches a
// volume that is all that is left.
func waitStoreGone(ctx context.Context, kube kubernetes.Interface, selector string, timeout time.Duration, j *Job, detach func(context.Context, storeAttachment) error) error {
	if storeGone(ctx, kube, selector, timeout) {
		return nil
	}
	last, cancel := context.WithTimeout(ctx, storeDetachTimeout)
	defer cancel()
	return detachLeft(last, kube, selector, j, detach)
}

// storeGone polls until nothing of the store is left, or reports false once timeout passes.
func storeGone(ctx context.Context, kube kubernetes.Interface, selector string, timeout time.Duration) bool {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if left, _, err := storeLeft(wait, kube, selector); err == nil && left == "" {
			return true
		}
		select {
		case <-wait.Done():
			return false
		case <-time.After(storeReadyPoll):
		}
	}
}

func detachLeft(ctx context.Context, kube kubernetes.Interface, selector string, j *Job, detach func(context.Context, storeAttachment) error) error {
	left, at, err := storeLeft(ctx, kube, selector)
	switch {
	case err == nil && left == "":
		return nil
	case err == nil && at == nil:
		err = errors.New(left)
	case err == nil:
		j.Add(fmt.Sprintf("The CSI plugin did not detach the backup volume from %s, so the appliance detaches it", at.Node))
		return detach(ctx, *at)
	}
	return fmt.Errorf("the backup volume's store did not stop, so the removal would hang deleting its namespace: %w", err)
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
		node, err := kube.CoreV1().Nodes().Get(ctx, spec.NodeName, metav1.GetOptions{})
		if err != nil || node.Status.NodeInfo.SystemUUID == "" {
			return left + ", whose VM cannot be told without the node's SMBIOS UUID", nil, nil //nolint:nilerr // reported as what is left
		}
		return left, &storeAttachment{Node: spec.NodeName, SystemUUID: node.Status.NodeInfo.SystemUUID, Volume: volume.Spec.CSI.VolumeHandle}, nil
	}
	return "", nil, nil
}

// detachVolume takes the volume off the node's VM as the CSI plugin does. The plugin, in the
// management cluster's capmox mode, knows the VM by its SMBIOS UUID alone, since the nodes carry
// no provider ID, so the VM is the one whose smbios1 holds the node's UUID, whatever its name.
// An unlink of a disk another VM owns only removes it from the configuration (qemu-server), so the
// backups stay.
func (a *Appliance) detachVolume(ctx context.Context, t *pve.Ticket, at storeAttachment) error {
	parts := strings.SplitN(at.Volume, "/", volumeHandleParts)
	if len(parts) != volumeHandleParts || parts[2] == "" || parts[3] == "" {
		return fmt.Errorf("the backup volume's handle %q is not region/zone/storage/disk", at.Volume)
	}
	disk := parts[2] + ":" + parts[3]
	c, err := pve.ConnectSession(pve.Config{URL: a.ProxmoxURL, CAFile: a.ProxmoxCAFile, Insecure: a.ProxmoxInsecure}, t)
	if err != nil {
		return fmt.Errorf("detaching %s from node %s's VM: %w", disk, at.Node, err)
	}
	vm, err := nodeVM(ctx, c, at)
	if err != nil {
		return fmt.Errorf("detaching %s from node %s's VM: %w", disk, at.Node, err)
	}
	if err := unlinkVolume(ctx, c, vm, disk); err != nil {
		return fmt.Errorf("detaching %s from VM %d (%s): %w", disk, vm.VMID, vm.Name, err)
	}
	return nil
}

// clusterVM is a VM as /cluster/resources lists it.
type clusterVM struct {
	VMID int    `json:"vmid"`
	Name string `json:"name"`
	Node string `json:"node"`
	Type string `json:"type"`
}

func (v clusterVM) path() string {
	return fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(v.Node), v.VMID)
}

// nodeVM finds the qemu VM whose SMBIOS UUID is the node's.
func nodeVM(ctx context.Context, c *proxmox.Client, at storeAttachment) (clusterVM, error) {
	var vms []clusterVM
	if err := c.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return clusterVM{}, fmt.Errorf("listing the VMs: %w", err)
	}
	for _, v := range vms {
		if v.Type != "qemu" {
			continue
		}
		var config struct {
			SMBIOS1 string `json:"smbios1"`
		}
		if err := c.Get(ctx, v.path()+"/config", &config); err != nil {
			return clusterVM{}, fmt.Errorf("reading VM %d's configuration: %w", v.VMID, err)
		}
		if strings.EqualFold(smbiosUUID(config.SMBIOS1), at.SystemUUID) {
			return v, nil
		}
	}
	return clusterVM{}, fmt.Errorf("no VM has SMBIOS UUID %s", at.SystemUUID)
}

// smbiosUUID reads the uuid of a VM's smbios1 setting.
func smbiosUUID(smbios1 string) string {
	for option := range strings.SplitSeq(smbios1, ",") {
		if uuid, ok := strings.CutPrefix(option, "uuid="); ok {
			return uuid
		}
	}
	return ""
}

// unlinkVolume unlinks disk from vm. A protected VM's protection is lifted for the unlink alone.
// On every path after the lift is sent, its answer lost included, the configuration is read again
// and the protection set again where it is off without the CSI plugin's marker. A marked VM is the
// plugin's, which sets it again itself.
func unlinkVolume(ctx context.Context, c *proxmox.Client, vm clusterVM, disk string) (err error) {
	var config map[string]any
	if err = c.Get(ctx, vm.path()+"/config", &config); err != nil {
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
		defer func() {
			again, cancel := context.WithTimeout(context.WithoutCancel(ctx), protectAgainTimeout)
			defer cancel()
			if perr := protectUnlessMarked(again, c, vm); perr != nil {
				err = errors.Join(err, fmt.Errorf("VM %d is left without protection, set it with qm set %d --protection 1: %w", vm.VMID, vm.VMID, perr))
			}
		}()
		lift := map[string]any{protectionOption: int(unprotected), "digest": config["digest"]}
		if err = c.Put(ctx, vm.path()+"/config", lift, nil); err != nil {
			return fmt.Errorf("lifting the protection: %w", err)
		}
	}
	if err = c.Put(ctx, vm.path()+"/unlink", map[string]any{"idlist": drive}, nil); err != nil {
		return fmt.Errorf("unlinking %s: %w", drive, err)
	}
	return nil
}

// protectUnlessMarked sets vm's protection again where its configuration has it off and carries
// no CSI marker.
func protectUnlessMarked(ctx context.Context, c *proxmox.Client, vm clusterVM) error {
	var config struct {
		Protection  int    `json:"protection"`
		Description string `json:"description"`
	}
	if err := c.Get(ctx, vm.path()+"/config", &config); err != nil {
		return fmt.Errorf("reading the configuration: %w", err)
	}
	if config.Protection == int(protected) || strings.Contains(config.Description, csiLiftedMarker) {
		return nil
	}
	if err := c.Put(ctx, vm.path()+"/config", map[string]any{protectionOption: int(protected)}, nil); err != nil {
		return fmt.Errorf("setting protection=1: %w", err)
	}
	return nil
}

// driveKey says whether a configuration key names a SCSI drive, the only bus the CSI plugin
// attaches volumes on.
func driveKey(key string) bool {
	n, ok := strings.CutPrefix(key, "scsi")
	return ok && n != "" && strings.Trim(n, "0123456789") == ""
}
