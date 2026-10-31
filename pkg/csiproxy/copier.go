package csiproxy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// maxUnused is how many unused disks Proxmox VE lets a VM have, unused0 to unused255. A copy
	// lands on a holder while it has room for one more: detaching the copy takes an unused slot.
	maxUnused = 256
	// maxSCSI is how many SCSI disks a VM may have, scsi0 to scsi30.
	maxSCSI = 31
	// copyTimeout bounds one copy, far beyond what copying a volume on a node's own disks takes, so
	// that a task Proxmox VE never ends does not hold the cluster's other copies for good.
	copyTimeout = 24 * time.Hour
)

// errRefused is a copy that is not the cluster's to make.
var errRefused = errors.New("refused")

// copyAsk is a copy as the plugin asks for it: Source, a volume ID on Node, into Target, a name on
// the same storage, on TargetNode when that is another node.
type copyAsk struct {
	Storage    string
	Source     string
	Target     string
	Node       string
	TargetNode string
}

// job is a copy under way, which the calls asking for it wait on. The copy goes on when they give
// up, so that the plugin's next call finds it done or still going.
type job struct {
	done chan struct{}
	err  error
}

// startCopy starts the copy, or finds it started. The copy outlives ctx, the call that asks for it.
func (p *Proxy) startCopy(ctx context.Context, c *pve, ask *copyAsk) *job {
	p.jobsMu.Lock()
	defer p.jobsMu.Unlock()
	key := ask.Storage + ":" + ask.Target
	if j, ok := p.jobs[key]; ok {
		return j
	}
	if p.jobs == nil {
		p.jobs = map[string]*job{}
	}
	j := &job{done: make(chan struct{})}
	p.jobs[key] = j
	go func() {
		copyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), copyTimeout)
		defer cancel()
		j.err = p.makeCopy(copyCtx, c, ask)
		p.jobsMu.Lock()
		delete(p.jobs, key)
		p.jobsMu.Unlock()
		close(j.done)
	}()
	return j
}

// makeCopy makes the copy, one at a time for the cluster, so that no two hold one holder's
// configuration at once, and records it. A copy recorded as done is not made again.
func (p *Proxy) makeCopy(ctx context.Context, c *pve, ask *copyAsk) error {
	p.copying.Lock()
	defer p.copying.Unlock()
	rec, found, err := p.record(ctx, ask.Storage, ask.Target)
	if err != nil {
		return err
	}
	if found && rec.State == stateDone {
		return nil
	}
	if !found {
		if rec, err = p.plan(ctx, c, ask); errors.Is(err, errRefused) {
			p.refused(ctx, ask, err)
			return err
		}
	}
	if err != nil {
		p.failed(ctx, ask, err)
		return err
	}
	if err := p.copyInto(ctx, c, &rec); err != nil {
		p.failed(ctx, ask, err)
		return err
	}
	p.Log.Info("copied a volume", "volume", rec.PluginID(), "source", rec.Source, "copy", rec.Real,
		"holder", rec.Holder, "node", rec.Node)
	return nil
}

// plan checks the copy, chooses its holder and records it before anything changes.
func (p *Proxy) plan(ctx context.Context, c *pve, ask *copyAsk) (Copy, error) {
	holders, err := p.holders(ctx, c)
	if err != nil {
		return Copy{}, err
	}
	if err = p.check(ask, holders); err != nil {
		return Copy{}, err
	}
	exists, err := volumeExists(ctx, c, ask.Node, ask.Storage, ask.Storage+":"+ask.Target)
	if err != nil {
		return Copy{}, err
	}
	if exists {
		return Copy{}, fmt.Errorf("%w: %s:%s is there already", errRefused, ask.Storage, ask.Target)
	}
	h, cfg, err := holderWithRoom(ctx, c, holders, ask.Node)
	if err != nil {
		return Copy{}, err
	}
	slot := ""
	for i := range maxSCSI {
		if _, used := cfg["scsi"+strconv.Itoa(i)]; !used {
			slot = "scsi" + strconv.Itoa(i)
			break
		}
	}
	if slot == "" {
		return Copy{}, fmt.Errorf("holder VM %d has no free SCSI disk", h.VMID)
	}
	rec := Copy{
		Name: recordName(ask.Storage, ask.Target), Storage: ask.Storage, Volume: ask.Target, Source: ask.Source,
		Node: h.Node, Holder: h.VMID, Slot: slot, State: stateCopying,
		Before: slices.Sorted(maps.Values(cfg.volumes())),
	}
	if err = p.Client.Create(ctx, rec.configMap()); err != nil {
		return Copy{}, fmt.Errorf("recording the copy: %w", err)
	}
	p.Log.Info("copying a volume", "volume", rec.PluginID(), "source", rec.Source, "holder", rec.Holder, "node", rec.Node)
	return rec, nil
}

// check refuses a copy that is not of one of the cluster's own volumes, or not named as the plugin
// names the cluster's volumes, after its volume owner VM.
func (p *Proxy) check(ask *copyAsk, holders []vm) error {
	storage, name, ok := splitVolID(ask.Source)
	switch {
	case !p.Copies:
		return fmt.Errorf("%w: volume copies are switched off for this cluster", errRefused)
	case !ok || storage != ask.Storage:
		return fmt.Errorf("%w: volume %s is not on storage %s", errRefused, ask.Source, ask.Storage)
	case ask.Node == "" || strings.ContainsAny(ask.Node, "/:"):
		return fmt.Errorf("%w: %q is not a node's name", errRefused, ask.Node)
	case ask.TargetNode != "" && ask.TargetNode != ask.Node:
		// The copy is imported on the holder's node, and the storages copies are made on are each
		// node's own, so the source has to be on the same node.
		return fmt.Errorf("%w: a copy stays on its source's node %s, and cannot land on node %s", errRefused, ask.Node, ask.TargetNode)
	}
	owners := []int{p.Owner}
	for _, h := range holders {
		owners = append(owners, h.VMID)
	}
	if id := volumeVMID(name); id == 0 || !slices.Contains(owners, id) {
		return fmt.Errorf("%w: volume %s is not one of the cluster's own, named after its volume owner VM %d or one of its holder VMs", errRefused, ask.Source, p.Owner)
	}
	if volumeVMID(ask.Target) != p.Owner {
		return fmt.Errorf("%w: %s is not named after the cluster's volume owner VM %d", errRefused, ask.Target, p.Owner)
	}
	return nil
}

// copyInto imports the volume into the holder's slot, unless an earlier try did, and detaches it.
// Any volume of the holder that is not among its volumes before is the copy's: copies onto one
// holder go one at a time.
func (p *Proxy) copyInto(ctx context.Context, c *pve, rec *Copy) error {
	cfg, err := c.config(ctx, rec.Node, rec.Holder)
	if err != nil {
		return err
	}
	if len(newVolumes(cfg, rec)) == 0 {
		if err = p.importCopy(ctx, c, rec); err != nil {
			return err
		}
		if cfg, err = c.config(ctx, rec.Node, rec.Holder); err != nil {
			return err
		}
	}
	return p.keep(ctx, c, rec, cfg)
}

// importCopy imports the volume into the holder's slot, recording the task first, and waits for
// the task.
func (p *Proxy) importCopy(ctx context.Context, c *pve, rec *Copy) error {
	if rec.Task == "" {
		// A proxy that stopped after asking and before recording the task asks again: while that
		// task still holds the holder's configuration, Proxmox VE refuses, and the plugin's next
		// call finds its copy.
		params := url.Values{rec.Slot: {rec.Storage + ":0,import-from=" + rec.Source + ",format=raw"}}
		if err := c.call(ctx, http.MethodPost, vmPath(rec.Node, rec.Holder)+"/config", params, &rec.Task); err != nil {
			return p.forget(ctx, rec, fmt.Errorf("starting the copy: %w", err))
		}
		if err := p.Client.Update(ctx, rec.configMap()); err != nil {
			return fmt.Errorf("recording the copy's task: %w", err)
		}
	}
	if rec.Task == "" {
		return nil
	}
	status, err := c.taskStatus(ctx, rec.Node, rec.Task, p.Poll)
	if err != nil {
		return err
	}
	if status != "OK" {
		return p.forget(ctx, rec, fmt.Errorf("the copy's task %s ended: %s", rec.Task, status))
	}
	return nil
}

// keep detaches the copy from its slot, deletes any other volume an earlier try left, and records
// the copy as done.
func (p *Proxy) keep(ctx context.Context, c *pve, rec *Copy, cfg vmConfig) error {
	found := newVolumes(cfg, rec)
	if len(found) == 0 {
		return p.forget(ctx, rec, errors.New("the copy's task ended without a new volume"))
	}
	kept := found[0]
	if v, ok := cfg.volumes()[rec.Slot]; ok && slices.Contains(found, v) {
		kept = v
	}
	for _, v := range found {
		// A disk turns into an unused one when deleted from the VM that owns it.
		if key := cfg.keyOf(v); !strings.HasPrefix(key, "unused") {
			if err := c.call(ctx, http.MethodPut, vmPath(rec.Node, rec.Holder)+"/config", url.Values{"delete": {key}}, nil); err != nil {
				return fmt.Errorf("detaching %s from holder VM %d: %w", v, rec.Holder, err)
			}
		}
	}
	cfg, err := c.config(ctx, rec.Node, rec.Holder)
	if err != nil {
		return err
	}
	for _, v := range newVolumes(cfg, rec) {
		if v == kept {
			continue
		}
		if err := deleteUnused(ctx, c, rec.Node, rec.Holder, cfg, v); err != nil {
			return err
		}
		p.Log.Warn("deleted a volume an earlier try at the copy left", "volume", rec.PluginID(), "left", v, "holder", rec.Holder)
	}
	if key := cfg.keyOf(kept); !strings.HasPrefix(key, "unused") {
		return fmt.Errorf("the copy %s is no unused disk of holder VM %d", kept, rec.Holder)
	}
	rec.Real, rec.State = kept, stateDone
	if err := p.Client.Update(ctx, rec.configMap()); err != nil {
		return fmt.Errorf("recording the copy: %w", err)
	}
	return nil
}

// newVolumes are the volumes of the holder that it did not have before the copy, sorted.
func newVolumes(cfg vmConfig, rec *Copy) []string {
	var found []string
	for _, v := range cfg.volumes() {
		if !slices.Contains(rec.Before, v) && !slices.Contains(found, v) {
			found = append(found, v)
		}
	}
	slices.Sort(found)
	return found
}

// forget deletes the record of a copy that did not happen, so that the next call starts it afresh.
func (p *Proxy) forget(ctx context.Context, rec *Copy, why error) error {
	if err := client.IgnoreNotFound(p.Client.Delete(ctx, rec.configMap())); err != nil {
		return errors.Join(why, fmt.Errorf("deleting the copy's record: %w", err))
	}
	return why
}

// deleteUnused deletes an unused disk of a VM, which deletes its volume.
func deleteUnused(ctx context.Context, c *pve, node string, vmid int, cfg vmConfig, volid string) error {
	key := cfg.keyOf(volid)
	if key == "" {
		return nil
	}
	if err := c.call(ctx, http.MethodPut, vmPath(node, vmid)+"/config", url.Values{"delete": {key}}, nil); err != nil {
		return fmt.Errorf("deleting %s of holder VM %d: %w", volid, vmid, err)
	}
	if strings.HasPrefix(key, "unused") {
		return nil
	}
	cfg, err := c.config(ctx, node, vmid)
	if err != nil {
		return err
	}
	return deleteUnused(ctx, c, node, vmid, cfg, volid)
}

// deleteCopy deletes a copy's volume, through the holder it is an unused disk of, and then its
// record.
func (p *Proxy) deleteCopy(ctx context.Context, c *pve, rec *Copy) error {
	p.copying.Lock()
	defer p.copying.Unlock()
	cfg, err := c.config(ctx, rec.Node, rec.Holder)
	if err != nil {
		return err
	}
	if err := deleteUnused(ctx, c, rec.Node, rec.Holder, cfg, rec.Real); err != nil {
		return err
	}
	if err := client.IgnoreNotFound(p.Client.Delete(ctx, rec.configMap())); err != nil {
		return fmt.Errorf("deleting the copy's record: %w", err)
	}
	p.Log.Info("deleted a volume copy", "volume", rec.PluginID(), "copy", rec.Real, "holder", rec.Holder, "node", rec.Node)
	return nil
}

// record reads a copy's record.
func (p *Proxy) record(ctx context.Context, storage, volume string) (Copy, bool, error) {
	cm := &corev1.ConfigMap{}
	err := p.Client.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: recordName(storage, volume)}, cm)
	if apierrors.IsNotFound(err) {
		return Copy{}, false, nil
	}
	if err != nil {
		return Copy{}, false, fmt.Errorf("reading the copy's record: %w", err)
	}
	rec, err := parseCopy(cm)
	if err != nil {
		return Copy{}, false, err
	}
	if rec.Storage != storage || rec.Volume != volume {
		return Copy{}, false, fmt.Errorf("record %s is of another copy", cm.Name)
	}
	return rec, true, nil
}

// holders are the cluster's holder VMs: those the token sees by the holders' name, which carries
// the cluster's identity. Proxmox VE names a VM's pool only to Pool.Audit on it, which the
// cluster's token lacks, so the token seeing the VM at all, through its role on its own pool,
// stands for it. A pool named is checked.
func (p *Proxy) holders(ctx context.Context, c *pve) ([]vm, error) {
	all, err := c.vms(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the holder VMs: %w", err)
	}
	var holders []vm
	for _, v := range all {
		if v.Name == p.Holders && (v.Pool == "" || v.Pool == p.Pool) && v.VMID != p.Owner {
			holders = append(holders, v)
		}
	}
	slices.SortFunc(holders, func(a, b vm) int { return a.VMID - b.VMID })
	return holders, nil
}

// holderWithRoom is the first holder on the node with room for one more copy.
func holderWithRoom(ctx context.Context, c *pve, holders []vm, node string) (vm, vmConfig, error) {
	for _, h := range holders {
		if h.Node != node {
			continue
		}
		cfg, err := c.config(ctx, h.Node, h.VMID)
		if err != nil {
			return vm{}, nil, err
		}
		if cfg.unused() < maxUnused-1 {
			return h, cfg, nil
		}
	}
	return vm{}, nil, fmt.Errorf("no holder VM on node %s has room for another copy yet; Kubehenge's controller adds one", node)
}

// contentEntry is a volume as a storage's content lists it.
type contentEntry struct {
	VolID string `json:"volid"`
}

// volumeExists reports whether a node's storage has the volume.
func volumeExists(ctx context.Context, c *pve, node, storage, volid string) (bool, error) {
	var content []contentEntry
	path := "/nodes/" + url.PathEscape(node) + "/storage/" + url.PathEscape(storage) + "/content"
	if err := c.call(ctx, http.MethodGet, path, nil, &content); err != nil {
		return false, err
	}
	return slices.Contains(content, contentEntry{VolID: volid}), nil
}

// refused logs a copy refused and records it as an event. Failed does the same for one that went wrong.
func (p *Proxy) refused(ctx context.Context, ask *copyAsk, err error) {
	p.Log.Warn("refused a volume copy", "volume", ask.Storage+":"+ask.Target, "source", ask.Source, "node", ask.Node, "reason", err.Error())
	p.event(ctx, "VolumeCopyFailed", fmt.Sprintf("Copying %s into %s:%s: %v", ask.Source, ask.Storage, ask.Target, err))
}

func (p *Proxy) failed(ctx context.Context, ask *copyAsk, err error) {
	p.Log.Error("a volume copy failed", "volume", ask.Storage+":"+ask.Target, "source", ask.Source, "node", ask.Node, "error", err.Error())
	p.event(ctx, "VolumeCopyFailed", fmt.Sprintf("Copying %s into %s:%s failed: %v", ask.Source, ask.Storage, ask.Target, err))
}

// event records a warning on the proxy's pod. If that fails, the log has it already.
func (p *Proxy) event(ctx context.Context, reason, message string) {
	if p.Pod.Name == "" {
		return
	}
	now := metav1.Now()
	e := &corev1.Event{
		Namespace: p.Pod.Namespace, GenerateName: p.Pod.Name + ".",
		InvolvedObject: corev1.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: p.Pod.Namespace, Name: p.Pod.Name},
		Reason:         reason, Message: message, Type: corev1.EventTypeWarning,
		Source:         corev1.EventSource{Component: "kubehenge-csi-proxy"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if err := p.Client.Create(ctx, e); err != nil {
		p.Log.Warn("recording an event", "reason", reason, "error", err.Error())
	}
}
