package appliance

import (
	"cmp"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// clusterAddress is the module's resource whose create bootstraps the management cluster and waits
// for its etcd.
const clusterAddress = moduleAddress + "talos_cluster.this"

// nodeModuleAddress starts the address of each node's Talos configuration and OS version.
const nodeModuleAddress = moduleAddress + "module.talos_node_"

// firstEtcdPoll is how often the watch of the first etcd start looks at the nodes.
const firstEtcdPoll = 20 * time.Second

// etcdStartsPath keeps the watch's starts by node: the count across the applies of one run, and
// each node's boot and whether its etcd ran across runs, so that no run reboots a node twice in
// one boot or bootstraps beside etcd data.
func (a *Appliance) etcdStartsPath() string { return filepath.Join(a.workdir(), "etcd-starts.json") }

// freshEtcdStarts gives each node a new count of starts for a run, which the user starts once the
// cause of the last run's give-up is fixed. A service restart ends a run, so the count is the run's
// own.
func (a *Appliance) freshEtcdStarts() error {
	saved, err := readJSONFile[map[string]provisioner.EtcdStart](a.etcdStartsPath())
	if err != nil {
		return nil //nolint:nilerr // the watch starts from no count then too
	}
	for address, s := range *saved {
		(*saved)[address] = provisioner.EtcdStart{Boot: s.Boot, Ran: s.Ran}
	}
	return writeJSONFile(a.etcdStartsPath(), *saved)
}

// createsCluster reports whether a plan, as `tofu show -json` prints it, bootstraps the management
// cluster: its first apply, or the next after one that failed before the cluster was up. Every
// other apply has the cluster in its state already. A plan that changes a node in any other way
// than creating it is left out, since such an apply reboots or replaces the node itself.
func createsCluster(shown []byte) bool {
	var plan struct {
		ResourceChanges []plannedChange `json:"resource_changes"`
	}
	_ = json.Unmarshal(shown, &plan) // plannedNodeVMs refused a plan that does not read
	creates := false
	for _, rc := range plan.ResourceChanges {
		switch {
		case rc.Address == clusterAddress:
			creates = slices.Contains(rc.Change.Actions, "create")
		case !strings.HasPrefix(rc.Address, nodeVMAddress+"[") && !strings.HasPrefix(rc.Address, nodeModuleAddress):
		case slices.ContainsFunc(rc.Change.Actions, func(action string) bool { return action != "create" && action != "no-op" }):
			return false
		}
	}
	return creates
}

// pulledResource is a resource in a state as `tofu state pull` prints it, with what the module's
// client configuration holds.
type pulledResource struct {
	Module    string           `json:"module"`
	Mode      string           `json:"mode"`
	Type      string           `json:"type"`
	Name      string           `json:"name"`
	Instances []pulledInstance `json:"instances"`
}

type pulledInstance struct {
	Attributes clientConfiguration `json:"attributes"`
}

type clientConfiguration struct {
	TalosConfig string `json:"talos_config"`
}

// stateTalosconfig is the module's talosconfig in a state as `tofu state pull` prints it, or nil.
func stateTalosconfig(pulled []byte) []byte {
	var state struct {
		Resources []pulledResource `json:"resources"`
	}
	if json.Unmarshal(pulled, &state) != nil {
		return nil
	}
	for _, r := range state.Resources {
		if r.Module+"."+r.Mode+"."+r.Type+"."+r.Name == talosconfigAddress && len(r.Instances) == 1 {
			return []byte(r.Instances[0].Attributes.TalosConfig)
		}
	}
	return nil
}

// talosconfigAddress is the module's client configuration, which holds its talosconfig.
const talosconfigAddress = moduleAddress + "data.talos_client_configuration.this"

// shownModule is a module of a plan's prior state, as `tofu show -json` prints it.
type shownModule struct {
	Resources    []shownResource `json:"resources"`
	ChildModules []shownModule   `json:"child_modules"`
}

type shownResource struct {
	Address string              `json:"address"`
	Values  clientConfiguration `json:"values"`
}

type shownValues struct {
	RootModule shownModule `json:"root_module"`
}

type shownState struct {
	Values shownValues `json:"values"`
}

// planTalosconfig is the module's talosconfig in the prior state of a plan, as `tofu show -json`
// prints it, or nil where the plan read none.
func planTalosconfig(shown []byte) []byte {
	var plan struct {
		PriorState shownState `json:"prior_state"`
	}
	if json.Unmarshal(shown, &plan) != nil {
		return nil
	}
	modules := []shownModule{plan.PriorState.Values.RootModule}
	for len(modules) > 0 {
		m := modules[0]
		modules = append(modules[1:], m.ChildModules...)
		for _, r := range m.Resources {
			if r.Address == talosconfigAddress && r.Values.TalosConfig != "" {
				return []byte(r.Values.TalosConfig)
			}
		}
	}
	return nil
}

// watchFirstEtcd starts etcd again on the management nodes Talos left it Failed on, while the apply
// that first starts it runs. Talos does not start a Failed etcd again by itself, and the apply would
// wait for it until its time ran out. The func it returns ends the watch and waits until it has stopped.
func (a *Appliance) watchFirstEtcd(ctx context.Context, j *Job, env []string, shown []byte) func() {
	if a.Talos == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.firstEtcd(ctx, j, env, shown)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (a *Appliance) firstEtcd(ctx context.Context, j *Job, env []string, shown []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(cmp.Or(a.etcdPoll, firstEtcdPoll)):
		}
		// The plan holds the client configuration when OpenTofu read it while planning, which it does
		// once the cluster's secrets exist and stay. Otherwise the read waits for the apply, and the
		// state holds the last apply's until OpenTofu writes this one's, so every look reads it again.
		// The apply's outputs reach the state only once it ends.
		admin := planTalosconfig(shown)
		if admin == nil {
			pulled, err := a.read(ctx, env, "state", "pull")
			if err != nil {
				continue
			}
			admin = stateTalosconfig(pulled)
		}
		nodes, err := provisioner.TalosEndpoints(admin)
		if err != nil {
			continue
		}
		starts := map[string]provisioner.EtcdStart{}
		if saved, err := readJSONFile[map[string]provisioner.EtcdStart](a.etcdStartsPath()); err == nil {
			starts = *saved
		}
		a.Talos.StartFirstEtcd(ctx, admin, nodes, starts, func() error { return writeJSONFile(a.etcdStartsPath(), starts) }, j.Add)
	}
}
