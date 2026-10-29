// Package csiproxy stands between a workload cluster's Proxmox VE CSI plugin and the Proxmox VE API
// in the clusters that take volume snapshots. The plugin copies a volume for a snapshot, a restore
// or a clone through a Proxmox VE method only root@pam may call. The proxy makes the copy itself
// with the cluster's own token instead: it imports the volume into a new disk of one of the
// cluster's holder VMs, detaches it, and records under which name the plugin knows it. Every other
// call goes on to Proxmox VE with the same token, with the plugin's names of the copies turned into
// their real volume IDs on the way there and back.
package csiproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Namespace holds the copies' records, as ConfigMaps, in each workload cluster that takes volume
	// snapshots. Only the CSI plugin's controller, and with it the proxy, may write there.
	Namespace = "kubehenge-volume-copies"
	// CopyLabel marks a ConfigMap in Namespace as a copy's record.
	CopyLabel = "kubehenge.dev/volume-copy"

	// stateCopying is a copy asked for and not finished: its record is written before Proxmox VE
	// is asked, so that a proxy that restarts finds the copy again rather than make a second one.
	stateCopying = "copying"
	// stateDone is a copy in place under its real volume ID.
	stateDone = "done"

	keyStorage = "storage"
	keyVolume  = "volume"
	keySource  = "source"
	keyNode    = "node"
	keyHolder  = "holder"
	keySlot    = "slot"
	keyBefore  = "before"
	keyTask    = "task"
	keyState   = "state"
	keyCopy    = "copy"
)

// Copy is the record of a volume copy in the workload cluster.
type Copy struct {
	// Name is the ConfigMap's, derived from Storage and Volume (recordName).
	Name string
	// Storage holds both volumes.
	Storage string
	// Volume is the copy's name as the CSI plugin asked for it, without the storage: vm-<owner>-…,
	// below <owner>/ with an extension on file-based storages.
	Volume string
	// Source is the real volume ID copied, storage:name.
	Source string
	// Node is the node of the holder VM, and with it of the copy.
	Node string
	// Holder is the VMID of the holder VM the copy is an unused disk of.
	Holder int
	// Slot is the holder's disk the copy is imported into before it is detached.
	Slot string
	// Before are the volume IDs the holder had before the copy: any new one is the copy's.
	Before []string
	// Task is Proxmox VE's task importing the volume, once it started.
	Task string
	// State is stateCopying or stateDone.
	State string
	// Real is the copy's real volume ID once done: storage:vm-<holder>-disk-<n>, below <holder>/
	// with an extension on file-based storages.
	Real string
}

// PluginID is the copy's volume ID as the plugin knows it.
func (c *Copy) PluginID() string {
	return c.Storage + ":" + c.Volume
}

// recordName is the record's name for a copy, the same on every try, so that a retried call finds
// the record of the first.
func recordName(storage, volume string) string {
	sum := sha256.Sum256([]byte(storage + ":" + volume))
	return "copy-" + hex.EncodeToString(sum[:])[:32]
}

// parseCopy reads a copy's record.
func parseCopy(cm *corev1.ConfigMap) (Copy, error) {
	d := cm.Data
	c := Copy{
		Name: cm.Name, Storage: d[keyStorage], Volume: d[keyVolume], Source: d[keySource], Node: d[keyNode],
		Slot: d[keySlot], Task: d[keyTask], State: d[keyState], Real: d[keyCopy],
	}
	if d[keyBefore] != "" {
		c.Before = strings.Split(d[keyBefore], "\n")
	}
	holder, err := strconv.Atoi(d[keyHolder])
	if err != nil {
		return c, fmt.Errorf("record %s: holder %q: %w", cm.Name, d[keyHolder], err)
	}
	c.Holder = holder
	switch {
	case c.Storage == "" || c.Volume == "" || c.Node == "":
		return c, fmt.Errorf("record %s names no storage, volume or node", cm.Name)
	case c.State != stateCopying && c.State != stateDone:
		return c, fmt.Errorf("record %s is in no known state", cm.Name)
	case c.State == stateDone && c.Real == "":
		return c, fmt.Errorf("record %s is done without a copy", cm.Name)
	}
	return c, nil
}

// configMap is the record as a ConfigMap.
func (c *Copy) configMap() *corev1.ConfigMap {
	data := map[string]string{
		keyStorage: c.Storage, keyVolume: c.Volume, keySource: c.Source, keyNode: c.Node,
		keyHolder: strconv.Itoa(c.Holder), keySlot: c.Slot, keyState: c.State,
	}
	for k, v := range map[string]string{keyBefore: strings.Join(c.Before, "\n"), keyTask: c.Task, keyCopy: c.Real} {
		if v != "" {
			data[k] = v
		}
	}
	return &corev1.ConfigMap{
		Namespace: Namespace, Name: c.Name,
		Labels: map[string]string{CopyLabel: ""},
		Data:   data,
	}
}

// Copies reads the records of the copies in place, by the volume ID the plugin knows each by.
// Kubehenge's controller reads them too, to leave the copies' volumes to the proxy.
func Copies(ctx context.Context, c client.Reader) (map[string]*Copy, error) {
	list := &corev1.ConfigMapList{}
	if err := c.List(ctx, list, client.InNamespace(Namespace), client.HasLabels{CopyLabel}); err != nil {
		return nil, fmt.Errorf("listing the volume copies: %w", err)
	}
	copies := map[string]*Copy{}
	for i := range list.Items {
		cp, err := parseCopy(&list.Items[i])
		if err == nil && cp.State == stateDone {
			copies[cp.PluginID()] = &cp
		}
	}
	return copies, nil
}

// volumeName is a volume's name on a storage as Proxmox VE and the CSI plugin name it:
// vm-<vmid>-<rest>, below <vmid>/ and with an extension on file-based storages.
var volumeName = regexp.MustCompile(`^(?:(\d+)/)?vm-(\d+)-[A-Za-z0-9_.-]+$`)

// volumeVMID is the VM a volume's name says it belongs to, or 0 when the name says none, or two.
func volumeVMID(name string) int {
	m := volumeName.FindStringSubmatch(name)
	if len(m) != 3 || (m[1] != "" && m[1] != m[2]) {
		return 0
	}
	id, err := strconv.Atoi(m[2])
	if err != nil {
		return 0
	}
	return id
}

// splitVolID splits storage:name.
func splitVolID(volid string) (string, string, bool) {
	return strings.Cut(volid, ":")
}
