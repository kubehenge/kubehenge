package appliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// nodeVMAddress is the management node VMs' resource, as the root config's state names it.
const nodeVMAddress = moduleAddress + "proxmox_virtual_environment_vm.node"

// planFile is the plan each apply runs, in the working directory and encrypted as the state is.
const planFile = "kubehenge.tfplan"

// protectAgainTimeout bounds setting protection again after a failed apply, which runs on after
// the job's own context has ended.
const protectAgainTimeout = time.Minute

// managementVM is a management node VM as Proxmox VE knows it.
type managementVM struct {
	ID   int    `json:"vm_id"`
	Name string `json:"name"`
}

// protection is the value of a VM's protection flag in Proxmox VE's configuration.
type protection int

const (
	unprotected protection = 0
	protected   protection = 1
)

// plannedChange is one resource's change in a plan, as `tofu show -json` prints it.
type plannedChange struct {
	Address string       `json:"address"`
	Change  changeAction `json:"change"`
}

type changeAction struct {
	Actions []string      `json:"actions"`
	Before  *managementVM `json:"before"`
}

// applyAttempts bounds the runs applyPlanned makes while each fails on etcd timing out a request.
// Storage that holds etcd's syncs for seconds fails a request now and then. The next run carries on
// from the state the last one wrote.
const applyAttempts = 3

// etcdStallPause is the wait before planning again after etcd timed out a request.
const etcdStallPause = 30 * time.Second

// etcdStalls are etcd's errors for a request it could not commit in time, as the API server passes
// them on. Anything else fails the run at once.
var etcdStalls = []string{
	"etcdserver: request timed out",
	"etcdserver: leader changed",
	"etcdserver: no leader",
	"etcdserver: too many requests",
}

// etcdStalled reports whether err names one of etcd's timing errors.
func etcdStalled(err error) bool {
	return slices.ContainsFunc(etcdStalls, func(s string) bool { return strings.Contains(err.Error(), s) })
}

// applyPlanned runs applyOnce, and again up to applyAttempts in all while a run fails on etcd timing
// out a request.
func (a *Appliance) applyPlanned(ctx context.Context, j *Job, env []string, t *pve.Ticket, args ...string) error {
	pause := etcdStallPause
	if a.stallPause != 0 {
		pause = a.stallPause
	}
	for attempt := 1; ; attempt++ {
		err := a.applyOnce(ctx, j, env, t, args...)
		if err == nil || attempt == applyAttempts || !etcdStalled(err) {
			return err
		}
		j.Add(fmt.Sprintf("etcd timed out a request, as slow storage makes it do. Running again in %s, attempt %d of %d", pause, attempt+1, applyAttempts))
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause):
		}
	}
}

// applyOnce plans with args, lifts the protection of exactly the node VMs that plan replaces or
// destroys, and applies that same plan. An in-place change lifts nothing. A failed apply sets
// protection again on each lifted VM that is still there, so that no run leaves one unprotected.
// A run cut off before that leaves the flag off on a VM the state holds, and the next plan sets it.
func (a *Appliance) applyOnce(ctx context.Context, j *Job, env []string, t *pve.Ticket, args ...string) error {
	defer func() { _ = os.Remove(filepath.Join(a.workdir(), planFile)) }()
	plan := append([]string{"plan", "-input=false", "-json", "-out=" + planFile}, args...)
	if err := a.tofu(ctx, j, env, jsonMessage, plan...); err != nil {
		return err
	}
	shown, err := a.read(ctx, env, "show", "-json", planFile)
	if err != nil {
		return err
	}
	deleted, err := plannedDeletes(shown)
	if err != nil {
		return err
	}
	var lifted []managementVM
	for _, vm := range deleted {
		j.Add(fmt.Sprintf("Lifting the protection of VM %d (%s), which this run replaces or removes", vm.ID, vm.Name))
		if err := a.setProtection(ctx, t, vm, unprotected); err != nil {
			return errors.Join(fmt.Errorf("lifting the protection of VM %d: %w", vm.ID, err), a.protectAgain(ctx, j, t, lifted))
		}
		lifted = append(lifted, vm)
	}
	if err := a.tofu(ctx, j, env, jsonMessage, "apply", "-auto-approve", "-input=false", "-json", planFile); err != nil {
		return errors.Join(err, a.protectAgain(ctx, j, t, lifted))
	}
	return nil
}

// plannedDeletes lists the node VMs a plan, as `tofu show -json` prints it, deletes: those it
// replaces and those it destroys.
func plannedDeletes(shown []byte) ([]managementVM, error) {
	var plan struct {
		ResourceChanges []plannedChange `json:"resource_changes"`
	}
	if err := json.Unmarshal(shown, &plan); err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	var out []managementVM
	for _, rc := range plan.ResourceChanges {
		if !strings.HasPrefix(rc.Address, nodeVMAddress+"[") || !slices.Contains(rc.Change.Actions, "delete") || rc.Change.Before == nil {
			continue
		}
		out = append(out, *rc.Change.Before)
	}
	return out, nil
}

// protectAgain sets protection on the lifted VMs after a failed apply. One that the apply removed
// is passed by.
func (a *Appliance) protectAgain(ctx context.Context, j *Job, t *pve.Ticket, lifted []managementVM) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), protectAgainTimeout)
	defer cancel()
	var errs []error
	for _, vm := range lifted {
		j.Add(fmt.Sprintf("Setting the protection of VM %d (%s) again", vm.ID, vm.Name))
		if err := a.setProtection(ctx, t, vm, protected); err != nil {
			errs = append(errs, fmt.Errorf("VM %d (%s) is left without protection, set it with qm set %d --protection 1: %w", vm.ID, vm.Name, vm.ID, err))
		}
	}
	return errors.Join(errs...)
}

// setProtection sets or lifts a management VM's protection with the ticket, on whichever node it
// runs on now, as HA may have moved it. A VM that is gone, or whose ID another VM has taken since,
// is passed by.
func (a *Appliance) setProtection(ctx context.Context, t *pve.Ticket, vm managementVM, value protection) error {
	if a.protect != nil {
		return a.protect(ctx, t, vm, value)
	}
	c, err := pve.ConnectSession(pve.Config{URL: a.ProxmoxURL, CAFile: a.ProxmoxCAFile, Insecure: a.ProxmoxInsecure}, t)
	if err != nil {
		return err //nolint:wrapcheck // the caller names the VM
	}
	var vms []struct {
		VMID int    `json:"vmid"`
		Name string `json:"name"`
		Node string `json:"node"`
		Type string `json:"type"`
	}
	if err := c.Get(ctx, "/cluster/resources?type=vm", &vms); err != nil {
		return fmt.Errorf("listing the VMs: %w", err)
	}
	for _, v := range vms {
		if v.VMID != vm.ID || v.Type != "qemu" || v.Name != vm.Name {
			continue
		}
		path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(v.Node), v.VMID)
		if err := c.Put(ctx, path, map[string]any{"protection": int(value)}, nil); err != nil {
			return fmt.Errorf("setting protection=%d: %w", value, err)
		}
		return nil
	}
	return nil
}
