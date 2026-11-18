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

// etcdStartsPath keeps the watch's starts by node, so that a service restart mid-apply neither
// loses the count nor starts etcd twice in one boot.
func (a *Appliance) etcdStartsPath() string { return filepath.Join(a.workdir(), "etcd-starts.json") }

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
		if r.Module+"."+r.Mode+"."+r.Type+"."+r.Name == moduleAddress+"data.talos_client_configuration.this" && len(r.Instances) == 1 {
			return []byte(r.Instances[0].Attributes.TalosConfig)
		}
	}
	return nil
}

// watchFirstEtcd starts etcd again on the management nodes Talos left it Failed on, while the apply
// that first starts it runs. Talos does not start a Failed etcd again by itself, and the apply would
// wait for it until its time ran out. The func it returns ends the watch and waits until it has stopped.
func (a *Appliance) watchFirstEtcd(ctx context.Context, j *Job, env []string) func() {
	if a.Talos == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.firstEtcd(ctx, j, env)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (a *Appliance) firstEtcd(ctx context.Context, j *Job, env []string) {
	var admin []byte
	var nodes []string
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(cmp.Or(a.etcdPoll, firstEtcdPoll)):
		}
		// The apply's outputs reach the state only once it ends. The client configuration they are
		// made of is there as soon as OpenTofu first writes the state, minutes before any node
		// runs etcd.
		if admin == nil {
			pulled, err := a.read(ctx, env, "state", "pull")
			if err != nil {
				continue
			}
			out := stateTalosconfig(pulled)
			if nodes, err = provisioner.TalosEndpoints(out); err != nil {
				continue
			}
			admin = out
		}
		starts := map[string]provisioner.EtcdStart{}
		if saved, err := readJSONFile[map[string]provisioner.EtcdStart](a.etcdStartsPath()); err == nil {
			starts = *saved
		}
		a.Talos.StartFirstEtcd(ctx, admin, nodes, starts, func() error { return writeJSONFile(a.etcdStartsPath(), starts) }, j.Add)
	}
}
