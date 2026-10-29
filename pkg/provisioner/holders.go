package provisioner

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	// holderFull is how many copies a holder VM holds before the controller adds another on its
	// node. The proxy fills a holder up to Proxmox VE's 256 unused disks, so the rest is the room the
	// cluster's copies have until the controller next looks.
	holderFull = maxUnused / 2
	// holderListed is how long Proxmox VE may take to list a new VM in its pool, during which the
	// controller counts a holder it created without seeing it.
	holderListed = 2 * time.Minute
	// holderTags mark a holder VM in Proxmox VE's interface.
	holderTags = "kubehenge;volume-copies"
)

// holderVM is a cluster's holder VM.
type holderVM struct {
	vmid    int
	node    string
	created time.Time
}

// EnsureVolumeHolders gives the cluster a holder VM with room for copies on each of nodes: a VM in
// its pool, never started and without disks of its own, whose unused disks the CSI plugin's proxy
// makes the copies of the cluster's volumes. Proxmox VE names a copy after the VM it is a disk of,
// so the cluster's token reaches it, and Proxmox VE lets a VM have 256 unused disks. Holders are
// never moved, as moving a VM copies its unused disks along, and never removed before the cluster.
func (p *Proxmox) EnsureVolumeHolders(ctx context.Context, n Names, nodes []string) error {
	holders, err := p.volumeHolders(ctx, n)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		room, err := p.holderRoomOn(ctx, holders, node)
		if err != nil {
			return err
		}
		if !room {
			if err = p.createVolumeHolder(ctx, n, node); err != nil {
				return err
			}
		}
	}
	return nil
}

// holderRoomOn reports whether a holder on node has room for more copies: one it created lately
// has.
func (p *Proxmox) holderRoomOn(ctx context.Context, holders []holderVM, node string) (bool, error) {
	for _, h := range holders {
		if h.node != node {
			continue
		}
		if !h.created.IsZero() {
			return true, nil
		}
		var config map[string]any
		if err := p.Client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), h.vmid), &config); err != nil {
			return false, fmt.Errorf("reading holder VM %d: %w", h.vmid, err)
		}
		if unusedDisks(config) < holderFull {
			return true, nil
		}
	}
	return false, nil
}

// unusedDisks counts a VM configuration's unused disks.
func unusedDisks(config map[string]any) int {
	count := 0
	for k := range config {
		if rest, ok := strings.CutPrefix(k, "unused"); ok && rest != "" && strings.Trim(rest, "0123456789") == "" {
			count++
		}
	}
	return count
}

// volumeHolders are the cluster's holder VMs: those of its pool by the holders' name, with those
// created too lately to be listed yet.
func (p *Proxmox) volumeHolders(ctx context.Context, n Names) ([]holderVM, error) {
	members, _, err := p.poolMembers(ctx, n.Pool)
	if err != nil {
		return nil, err
	}
	var holders []holderVM
	for _, m := range members {
		if m.Type == qemuType && m.Name == n.VolumeHolders() {
			holders = append(holders, holderVM{vmid: m.VMID, node: m.Node})
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	recent := p.newHolders[n.Pool][:0]
	for _, h := range p.newHolders[n.Pool] {
		listed := slices.ContainsFunc(holders, func(l holderVM) bool { return l.vmid == h.vmid })
		if !listed && time.Since(h.created) < holderListed {
			recent = append(recent, h)
			holders = append(holders, h)
		}
	}
	if p.newHolders != nil {
		p.newHolders[n.Pool] = recent
	}
	slices.SortFunc(holders, func(a, b holderVM) int { return a.vmid - b.vmid })
	return holders, nil
}

// createVolumeHolder creates a holder VM on node.
func (p *Proxmox) createVolumeHolder(ctx context.Context, n Names, node string) error {
	for attempt := 1; ; attempt++ {
		var next string
		if err := p.Client.Get(ctx, "/cluster/nextid", &next); err != nil {
			return fmt.Errorf("choosing a VMID: %w", err)
		}
		var vmid int
		if _, err := fmt.Sscan(next, &vmid); err != nil {
			return fmt.Errorf("choosing a VMID: %q: %w", next, err)
		}
		// As the owner: Proxmox VE checks every other setting against the VM's own path while
		// creating it, before its pool's rights reach it, so the rest comes after.
		var upid string
		err := p.Client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu", url.PathEscape(node)), map[string]any{vmidParam: vmid, "name": n.VolumeHolders(), "pool": n.Pool}, &upid)
		switch {
		case isExisting(err) && attempt < volumeOwnerAttempts:
			continue
		case err != nil:
			return fmt.Errorf("creating a holder VM on node %s: %w", node, err)
		}
		p.mu.Lock()
		if p.newHolders == nil {
			p.newHolders = map[string][]holderVM{}
		}
		p.newHolders[n.Pool] = append(p.newHolders[n.Pool], holderVM{vmid: vmid, node: node, created: time.Now()})
		p.mu.Unlock()
		if err := p.waitTask(ctx, node, upid, createTaskTimeout); err != nil {
			return fmt.Errorf("creating holder VM %d: %w", vmid, err)
		}
		config := map[string]any{
			"tags": holderTags,
			"description": "Holds copies of the persistent volumes of Kubernetes cluster " + n.Pool +
				", made for its volume snapshots, restores and clones, as unused disks. Never started; deleting it deletes them.",
		}
		if err := p.Client.Put(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), config, nil); err != nil {
			return fmt.Errorf("describing holder VM %d: %w", vmid, err)
		}
		return nil
	}
}

// RemoveEmptyVolumeHolders destroys the cluster's holder VMs that hold no disk, once the cluster no
// longer takes volume snapshots and no copy is left. A holder that still has a disk stays until the
// cluster goes: no record says whose it is, and a persistent volume may still use it.
func (p *Proxmox) RemoveEmptyVolumeHolders(ctx context.Context, n Names) error {
	holders, err := p.volumeHolders(ctx, n)
	if err != nil {
		return err
	}
	empty := holders[:0]
	for _, h := range holders {
		var config map[string]any
		err := p.Client.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(h.node), h.vmid), &config)
		if isMissing(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading holder VM %d: %w", h.vmid, err)
		}
		if !holdsDisks(config) {
			empty = append(empty, h)
		}
	}
	return p.destroyVolumeHolders(ctx, empty)
}

// holdsDisks reports whether a VM configuration names any disk, attached or unused.
func holdsDisks(config map[string]any) bool {
	for k := range config {
		rest := strings.TrimLeft(k, "abcdefghijklmnopqrstuvwxyz")
		if rest == "" || strings.Trim(rest, "0123456789") != "" {
			continue
		}
		switch k[:len(k)-len(rest)] {
		case "scsi", "virtio", "sata", "ide", "unused", "efidisk", "tpmstate":
			return true
		}
	}
	return false
}

// removeVolumeHolders destroys the cluster's holder VMs with the copies they hold, as the cluster
// goes.
func (p *Proxmox) removeVolumeHolders(ctx context.Context, n Names) error {
	holders, err := p.volumeHolders(ctx, n)
	if err != nil {
		return err
	}
	return p.destroyVolumeHolders(ctx, holders)
}

// destroyVolumeHolders destroys holder VMs with their disks, each on its own node, where its disks
// are.
func (p *Proxmox) destroyVolumeHolders(ctx context.Context, holders []holderVM) error {
	for _, h := range holders {
		var upid string
		params := url.Values{"destroy-unreferenced-disks": {"1"}, "purge": {"1"}}
		err := p.Client.Delete(ctx, fmt.Sprintf("/nodes/%s/qemu/%d?%s", url.PathEscape(h.node), h.vmid, params.Encode()), &upid)
		if isMissing(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("deleting holder VM %d on node %s: %w", h.vmid, h.node, err)
		}
		if err := p.waitTask(ctx, h.node, upid, deleteTaskTimeout); err != nil {
			return fmt.Errorf("deleting holder VM %d: %w", h.vmid, err)
		}
	}
	return nil
}
