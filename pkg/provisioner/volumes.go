package provisioner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// csiDriver is the Proxmox VE CSI plugin's driver name.
const csiDriver = "csi.proxmox.sinextra.dev"

// Proxmox VE numbers a VM's unused disks unused0 to unused255.
const maxUnused = 256

// Reclaimer deletes the volumes of workload clusters' released persistent volumes and deleted
// volume snapshots. The CSI plugin deletes a volume through the storage, which needs
// Datastore.Allocate on all of it; a cluster's token has rights on its own volume owner VM only,
// through which a volume can be deleted too. Once the volume is gone, the plugin's next attempt
// finds nothing to delete and the volume or snapshot is released.
type Reclaimer struct {
	Client         client.Client
	ClassNamespace string
	// Connect signs in to Proxmox VE with a cluster's token, user@realm!name=secret.
	Connect func(token string) (*proxmox.Client, error)
	// Mover moves volume owner VMs to the node of a volume on node-local storage.
	Mover Mover
	// Remotes reaches the workload clusters.
	Remotes  *Remotes
	Interval time.Duration
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*Reclaimer) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (r *Reclaimer) Start(ctx context.Context) error {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.reclaimAll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *Reclaimer) reclaimAll(ctx context.Context) {
	log := ctrl.LoggerFrom(ctx).WithName("volume-reclaimer")
	clusters := &clusterv1.ClusterList{}
	// Only read: the cache's own objects spare a copy of every cluster each round.
	if err := r.Client.List(ctx, clusters, client.UnsafeDisableDeepCopy); err != nil {
		log.Error(err, "listing clusters")
		return
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if !Owns(c, r.ClassNamespace) || !c.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.reclaim(ctx, c); err != nil && ctx.Err() == nil {
			log.Error(err, "reclaiming released volumes", "cluster", client.ObjectKeyFromObject(c))
		}
	}
}

func (r *Reclaimer) reclaim(ctx context.Context, cluster *clusterv1.Cluster) error {
	stored := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, stored)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the cluster's credentials: %w", err)
	}
	if !isReady(stored) {
		return nil
	}
	creds, err := credentialsFrom(stored)
	if err != nil {
		return err
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return err
	}
	if err := r.reclaimIn(ctx, cluster, creds, workload.client); err != nil {
		r.Remotes.forget(cluster)
		return err
	}
	return nil
}

// reclaimIn deletes the volumes of the released persistent volumes, and of the volume snapshots
// being deleted, in the workload cluster.
func (r *Reclaimer) reclaimIn(ctx context.Context, cluster *clusterv1.Cluster, creds Credentials, workload client.Client) error {
	volumes, err := releasedVolumes(ctx, workload)
	if err != nil {
		return err
	}
	snapshots, err := deletedSnapshots(ctx, workload)
	if err != nil {
		return err
	}
	var owner *ownerVM
	var errs []error
	reclaim := func(what, name, handle string) error {
		v, err := parseVolumeHandle(handle)
		if err != nil {
			return fmt.Errorf("%s %s: %w", what, name, err)
		}
		// Only the cluster's own volumes; the owner VM's ACL would refuse others anyway.
		if v.owner != creds.VolumeOwner {
			return nil
		}
		if owner == nil {
			pve, err := r.Connect(creds.TokenID + "=" + creds.TokenSecret)
			if err != nil {
				return err
			}
			owner = &ownerVM{pve: pve, mover: r.Mover, vmid: creds.VolumeOwner, created: creds.VolumeOwnerNode}
		}
		if err := owner.deleteVolume(ctx, v); err != nil {
			return fmt.Errorf("%s %s: %w", what, name, err)
		}
		ctrl.LoggerFrom(ctx).Info("deleted the volume of a released "+what, "cluster", client.ObjectKeyFromObject(cluster), "name", name, "volume", v.volid)
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(volumes)) {
		errs = append(errs, reclaim("persistent volume", name, volumes[name]))
	}
	for _, name := range slices.Sorted(maps.Keys(snapshots)) {
		errs = append(errs, reclaim("volume snapshot", name, snapshots[name]))
	}
	return errors.Join(errs...)
}

// releasedVolumes are the volume handles, by persistent volume, of the CSI plugin's released
// persistent volumes that no node has attached.
func releasedVolumes(ctx context.Context, workload client.Client) (map[string]string, error) {
	pvs := &corev1.PersistentVolumeList{}
	if err := workload.List(ctx, pvs); err != nil {
		return nil, fmt.Errorf("listing persistent volumes: %w", err)
	}
	// Kubernetes' own CSI deletion waits until no VolumeAttachment names the volume: a node that
	// went away may still have it attached, and deleting it then would break the VM it is on.
	attachments := &storagev1.VolumeAttachmentList{}
	if err := workload.List(ctx, attachments); err != nil {
		return nil, fmt.Errorf("listing volume attachments: %w", err)
	}
	attached := map[string]bool{}
	for i := range attachments.Items {
		if name := attachments.Items[i].Spec.Source.PersistentVolumeName; name != nil {
			attached[*name] = true
		}
	}
	handles := map[string]string{}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if released(pv) && !attached[pv.Name] {
			handles[pv.Name] = pv.Spec.CSI.VolumeHandle
		}
	}
	return handles, nil
}

// snapshotContentList is the external snapshotter's list of VolumeSnapshotContents.
var snapshotContentList = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContentList"}

// deletedSnapshots are the snapshot handles, by VolumeSnapshotContent, of the CSI plugin's snapshots
// being deleted with their volume. The plugin deletes a snapshot's volume through the storage too,
// and the snapshot is named after the owner VM as the cluster's volumes are, so it is deleted as
// they are; the plugin's next attempt then finds nothing left to delete.
func deletedSnapshots(ctx context.Context, workload client.Client) (map[string]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(snapshotContentList)
	if err := workload.List(ctx, list); err != nil {
		// A cluster that never took a snapshot may have no such kind.
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("listing volume snapshot contents: %w", err)
	}
	handles := map[string]string{}
	for i := range list.Items {
		c := &list.Items[i]
		driver, _, _ := unstructured.NestedString(c.Object, "spec", "driver")
		policy, _, _ := unstructured.NestedString(c.Object, "spec", "deletionPolicy")
		handle, _, _ := unstructured.NestedString(c.Object, "status", "snapshotHandle")
		if c.GetDeletionTimestamp() != nil && driver == csiDriver && policy == "Delete" && handle != "" {
			handles[c.GetName()] = handle
		}
	}
	return handles, nil
}

// released reports whether a persistent volume of the Proxmox VE CSI plugin waits for its volume
// to be deleted.
func released(pv *corev1.PersistentVolume) bool {
	return pv.Spec.CSI != nil && pv.Spec.CSI.Driver == csiDriver &&
		pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete &&
		(pv.Status.Phase == corev1.VolumeReleased || pv.Status.Phase == corev1.VolumeFailed)
}

type volume struct {
	// node is where the volume is, which matters only on storage local to each node.
	node    string
	storage string
	// volid is Proxmox VE's volume ID, storage:volume name.
	volid string
	owner int
}

// parseVolumeHandle reads the CSI plugin's volume handle, region/zone/storage/volume name, where
// the zone is the Proxmox VE node and the volume name is the storage's own, such as
// 102/vm-102-pvc-<uid>.raw on a directory storage or vm-102-pvc-<uid> on LVM.
func parseVolumeHandle(handle string) (volume, error) {
	const fields = 4
	parts := strings.SplitN(handle, "/", fields)
	if len(parts) != fields || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return volume{}, fmt.Errorf("volume handle %q is not region/zone/storage/volume", handle)
	}
	name := parts[3][strings.LastIndex(parts[3], "/")+1:]
	rest, ok := strings.CutPrefix(name, "vm-")
	if !ok {
		return volume{}, fmt.Errorf("volume %q is not named after its owner VM", parts[3])
	}
	id, _, _ := strings.Cut(rest, "-")
	owner, err := strconv.Atoi(id)
	if err != nil {
		return volume{}, fmt.Errorf("volume %q is not named after its owner VM", parts[3])
	}
	return volume{node: parts[1], storage: parts[2], volid: parts[2] + ":" + parts[3], owner: owner}, nil
}

// Mover moves a volume owner VM between nodes, which a cluster's own token may not.
type Mover interface {
	MoveVolumeOwner(ctx context.Context, vmid int, from, to string) error
}

// ownerVM is a cluster's volume owner VM, reached with the cluster's token while it runs and with
// the controller's when it is removed.
type ownerVM struct {
	pve   *proxmox.Client
	mover Mover
	vmid  int
	// created is the node the VM was created on.
	created string
	// discardUnused deletes unused disks before a move instead of refusing it, for when every
	// volume of the VM goes.
	discardUnused bool
}

// deleteVolume deletes one of the VM's volumes. Proxmox VE deletes an unused disk on the node its
// VM is on, so for a volume on storage local to another node the VM moves there first.
func (o *ownerVM) deleteVolume(ctx context.Context, v volume) error {
	at, err := locate(ctx, o.pve, o.vmid, o.created)
	if err != nil {
		return err
	}
	if at != v.node {
		local, err := nodeLocal(ctx, o.pve, v.storage)
		if err != nil {
			return err
		}
		if local {
			if err := o.moveTo(ctx, at, v.node); err != nil {
				return err
			}
			at = v.node
		}
	}
	return deleteThroughOwner(ctx, o.pve, at, o.vmid, v.volid)
}

// moveTo moves the VM only while it has no unused disks, which migration would copy to the new
// node. One is left only when a deletion fails half-way, and is finished when its persistent
// volume comes round again.
func (o *ownerVM) moveTo(ctx context.Context, from, to string) error {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", from, o.vmid)
	config := map[string]any{}
	if err := o.pve.Get(ctx, path, &config); err != nil {
		return fmt.Errorf("reading owner VM %d: %w", o.vmid, err)
	}
	var unused []string
	for k := range config {
		if strings.HasPrefix(k, "unused") {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 && !o.discardUnused {
		return fmt.Errorf("owner VM %d has unused disks (%s), so it cannot move to node %s", o.vmid, strings.Join(unused, ", "), to)
	}
	if len(unused) > 0 {
		if err := o.pve.Put(ctx, path, map[string]any{"delete": strings.Join(unused, ",")}, nil); err != nil {
			return fmt.Errorf("deleting the unused disks of owner VM %d: %w", o.vmid, err)
		}
	}
	if o.mover == nil {
		return fmt.Errorf("owner VM %d is on node %s, not %s, and nothing may move it", o.vmid, from, to)
	}
	if err := o.mover.MoveVolumeOwner(ctx, o.vmid, from, to); err != nil {
		return fmt.Errorf("moving owner VM %d from node %s to %s: %w", o.vmid, from, to, err)
	}
	return nil
}

// locate finds the node a VM is on. Proxmox VE lists a new VM only seconds after creating it, so
// one not listed yet is taken to be on the node it was created on.
func locate(ctx context.Context, pve *proxmox.Client, vmid int, created string) (string, error) {
	vm, listed, err := listedVM(ctx, pve, vmid)
	if err != nil || !listed {
		return created, err
	}
	return vm.Node, nil
}

type resourceVM struct {
	VMID int    `json:"vmid"`
	Node string `json:"node"`
	Pool string `json:"pool"`
}

// listedVM looks a VM up in Proxmox VE's resource list, and reports whether it is there.
func listedVM(ctx context.Context, pve *proxmox.Client, vmid int) (resourceVM, bool, error) {
	var vms []resourceVM
	if err := pve.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return resourceVM{}, false, fmt.Errorf("listing VMs: %w", err)
	}
	for _, vm := range vms {
		if vm.VMID == vmid {
			return vm, true, nil
		}
	}
	return resourceVM{}, false, nil
}

type storageConfig struct {
	Storage string `json:"storage"`
	Content string `json:"content"`
	// Nodes limits the storage to some nodes; empty means all.
	Nodes  string `json:"nodes"`
	Shared int    `json:"shared"`
}

// storages lists the storages the token may use; Proxmox VE marks network storages shared.
func storages(ctx context.Context, pve *proxmox.Client) ([]storageConfig, error) {
	var list []storageConfig
	if err := pve.Get(ctx, "/storage", &list); err != nil {
		return nil, fmt.Errorf("listing storages: %w", err)
	}
	return list, nil
}

// nodeLocal reports whether each node has a storage of its own under this name.
func nodeLocal(ctx context.Context, pve *proxmox.Client, storage string) (bool, error) {
	list, err := storages(ctx, pve)
	if err != nil {
		return false, err
	}
	for _, s := range list {
		if s.Storage == storage {
			return s.Shared == 0, nil
		}
	}
	return false, fmt.Errorf("storage %s is not visible to the token", storage)
}

// deleteThroughOwner deletes a volume by making it an unused disk of the VM that owns it and
// removing that disk, which needs only VM.Config.Disk on the VM.
func deleteThroughOwner(ctx context.Context, pve *proxmox.Client, node string, vmid int, volid string) error {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid)
	config := map[string]any{}
	if err := pve.Get(ctx, path, &config); err != nil {
		return fmt.Errorf("reading owner VM %d: %w", vmid, err)
	}
	key := ""
	for i := range maxUnused {
		k := "unused" + strconv.Itoa(i)
		v, used := config[k]
		if used && v == volid {
			key = k
			break
		}
		if !used && key == "" {
			key = k
		}
	}
	if key == "" {
		return fmt.Errorf("owner VM %d has no free unused disk slot", vmid)
	}
	if config[key] != volid {
		err := pve.Put(ctx, path, map[string]any{key: volid}, nil)
		if isMissing(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("attaching %s to owner VM %d as %s: %w", volid, vmid, key, err)
		}
	}
	if err := pve.Put(ctx, path, map[string]any{"delete": key}, nil); err != nil {
		return fmt.Errorf("deleting %s through owner VM %d: %w", volid, vmid, err)
	}
	return nil
}
