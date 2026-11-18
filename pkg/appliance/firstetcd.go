package appliance

import (
	"cmp"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// clusterAddress is the module's resource whose create bootstraps the management cluster and waits
// for its etcd.
const clusterAddress = moduleAddress + "talos_cluster.this"

// firstEtcdPoll is how often the watch of the first etcd start looks at the nodes.
const firstEtcdPoll = 20 * time.Second

// etcdStartsPath keeps the watch's starts by node, so that a service restart mid-apply neither
// loses the count nor starts etcd twice in one boot.
func (a *Appliance) etcdStartsPath() string { return filepath.Join(a.workdir(), "etcd-starts.json") }

// createsCluster reports whether a plan, as `tofu show -json` prints it, bootstraps the management
// cluster: its first apply, or the next after one that failed before the cluster was up. Every
// other apply has the cluster in its state already.
func createsCluster(shown []byte) bool {
	var plan struct {
		ResourceChanges []plannedChange `json:"resource_changes"`
	}
	_ = json.Unmarshal(shown, &plan) // plannedNodeVMs refused a plan that does not read
	return slices.ContainsFunc(plan.ResourceChanges, func(rc plannedChange) bool {
		return rc.Address == clusterAddress && slices.Contains(rc.Change.Actions, "create")
	})
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
		// The apply puts the talosconfig in the state right after the cluster's secrets, minutes
		// before any node runs etcd.
		if admin == nil {
			out, err := a.output(ctx, env, talosconfigName)
			if err != nil {
				continue
			}
			if nodes, err = provisioner.TalosEndpoints([]byte(out)); err != nil {
				continue
			}
			admin = []byte(out)
		}
		starts := map[string]provisioner.EtcdStart{}
		if saved, err := readJSONFile[map[string]provisioner.EtcdStart](a.etcdStartsPath()); err == nil {
			starts = *saved
		}
		a.Talos.StartFirstEtcd(ctx, admin, nodes, starts, func() error { return writeJSONFile(a.etcdStartsPath(), starts) }, j.Add)
	}
}
