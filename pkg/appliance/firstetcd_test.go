package appliance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// firstNodes are the management nodes 10.0.0.11 and 10.0.0.12 the fake OpenTofu's talosconfig
// names, with the state of each one's etcd, Preparing unless set, and its boot, which a reboot
// leaves to the test.
type firstNodes struct {
	mu    sync.Mutex
	etcd  map[string]string
	boots map[string]int
	// starts are the starts made, "<address> bootstrap" or "<address> reboot".
	starts []string
	looks  int
}

func newFirstNodes(etcd map[string]string) *firstNodes {
	return &firstNodes{etcd: etcd, boots: map[string]int{}}
}

func (n *firstNodes) dial(_ context.Context, raw []byte, address string) (provisioner.TalosNode, error) { //nolint:ireturn // a TalosDialer
	if !strings.Contains(string(raw), "endpoints") {
		return nil, errors.New("not the admin talosconfig")
	}
	return &firstNode{nodes: n, address: address}, nil
}

func (n *firstNodes) set(address, state string, boot int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.etcd[address], n.boots[address] = state, boot
}

func (n *firstNodes) made() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.starts)
}

func (n *firstNodes) looked() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.looks
}

// firstNode is one of them. Only the calls the watch makes are there.
type firstNode struct {
	provisioner.TalosNode

	nodes   *firstNodes
	address string
}

func (n *firstNode) Etcd(ctx context.Context) (provisioner.TalosService, error) {
	// As the Talos API, an ended context reaches no node.
	if err := ctx.Err(); err != nil {
		return provisioner.TalosService{}, err
	}
	n.nodes.mu.Lock()
	defer n.nodes.mu.Unlock()
	n.nodes.looks++
	state := n.nodes.etcd[n.address]
	if state == "" {
		state = "Preparing"
	}
	s := provisioner.TalosService{State: state}
	if state == "Failed" {
		s.Event = "pull failed"
	}
	return s, nil
}

func (n *firstNode) BootID(context.Context) (string, error) {
	n.nodes.mu.Lock()
	defer n.nodes.mu.Unlock()
	return strconv.Itoa(n.nodes.boots[n.address]), nil
}

func (n *firstNode) Bootstrap(context.Context) error {
	n.nodes.mu.Lock()
	defer n.nodes.mu.Unlock()
	n.nodes.starts = append(n.nodes.starts, n.address+" bootstrap")
	return nil
}

func (n *firstNode) Reboot(context.Context) error {
	n.nodes.mu.Lock()
	defer n.nodes.mu.Unlock()
	n.nodes.starts = append(n.nodes.starts, n.address+" reboot")
	return nil
}

func (*firstNode) Close() error { return nil }

// firstTalos reaches the nodes for the watch alone.
type firstTalos struct {
	*fakeManagementTalos

	nodes *firstNodes
}

func (f firstTalos) StartFirstEtcd(ctx context.Context, admin []byte, nodes []string, starts map[string]provisioner.EtcdStart, record func() error, log func(string)) {
	m := &provisioner.ManagementTalos{Dial: f.nodes.dial}
	m.StartFirstEtcd(ctx, admin, nodes, starts, record, log)
}

// firstApply starts an apply of a whose plan holds changes, held until released. For word "fail"
// it fails then, and for "stall" it fails on etcd timing out a request, which the run waits to
// apply again after. a's watch reaches the nodes.
func firstApply(t *testing.T, a *Appliance, nodes *firstNodes, word string, changes ...change) {
	t.Helper()
	a.Talos = firstTalos{fakeManagementTalos: &fakeManagementTalos{}, nodes: nodes}
	a.etcdPoll = time.Millisecond
	writePlan(t, a, changes...)
	s := settings("pve1")
	s.ControllerImage = "held"
	switch word {
	case "fail":
		s.SearchDomains = []string{"fail"}
	case "stall":
		s.SearchDomains = []string{"stall"}
		a.stallPause = time.Hour
		if err := os.WriteFile(filepath.Join(a.Dir, "stalls"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Apply(s, passphrase, ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func releaseApply(t *testing.T, a *Appliance) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.Dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !done(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never " + what)
		}
	}
}

// stays fails t if the nodes are looked at again within a while.
func stays(t *testing.T, nodes *firstNodes, what string) {
	t.Helper()
	before := nodes.looked()
	time.Sleep(300 * time.Millisecond)
	if after := nodes.looked(); after != before {
		t.Errorf("%s: the nodes were looked at %d more times", what, after-before)
	}
}

func readStarts(t *testing.T, a *Appliance) map[string]provisioner.EtcdStart {
	t.Helper()
	saved, err := readJSONFile[map[string]provisioner.EtcdStart](a.etcdStartsPath())
	if err != nil {
		t.Fatal(err)
	}
	return *saved
}

var createsTheCluster = change{address: clusterAddress, actions: []string{"create"}}

// The apply that creates the management cluster bootstraps its first node again when Talos left
// etcd there Failed, and says so in the job's log with Talos's reason.
func TestFirstApplyStartsAFailedEtcdAgain(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	nodes := newFirstNodes(map[string]string{"10.0.0.11": "Failed"})
	firstApply(t, a, nodes, "held", createsTheCluster)
	eventually(t, "bootstrapped", func() bool { return slices.Equal(nodes.made(), []string{"10.0.0.11 bootstrap"}) })
	nodes.set("10.0.0.11", "Running", 0)
	releaseApply(t, a)
	j := waitFor(t, a)
	if j.State != StateSucceeded || !slices.Contains(j.Log, "Talos left etcd on 10.0.0.11 Failed: pull failed. Bootstrapped it again.") {
		t.Errorf("job %+v", j)
	}
	if got := readStarts(t, a)["10.0.0.11"]; got.Repairs != 1 {
		t.Errorf("recorded %+v", got)
	}
}

// Only the apply whose plan creates the cluster watches etcd. An upgrade, the apply after a
// restore or of a recreated cluster's VMs, and one that replaces, updates or reboots a node leave
// the nodes alone.
func TestOnlyTheApplyThatCreatesTheClusterWatchesEtcd(t *testing.T) {
	t.Parallel()
	talosMachine := nodeModuleAddress + "1.talos_machine.this"
	for name, changes := range map[string][]change{
		"upgrade":                   {{address: clusterAddress, actions: []string{"update"}}},
		"recreate":                  {{address: nodeVMAddress + "[0]", actions: []string{"create"}}},
		"replacing a node":          {createsTheCluster, {address: nodeVMAddress + "[1]", actions: []string{"delete", "create"}, id: 101}},
		"rebooting a node in place": {createsTheCluster, {address: talosMachine, actions: []string{"update"}}},
		"replacing a node's Talos":  {createsTheCluster, {address: talosMachine, actions: []string{"create", "delete"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, _ := newAppliance(t)
			recordProtections(a, nil)
			nodes := newFirstNodes(map[string]string{"10.0.0.11": "Failed"})
			firstApply(t, a, nodes, "held", changes...)
			eventually(t, "applied", func() bool { return lastTofuCommand(a) == "apply" })
			time.Sleep(300 * time.Millisecond)
			releaseApply(t, a)
			waitFor(t, a)
			if n := nodes.looked(); n != 0 || len(nodes.made()) != 0 {
				t.Errorf("looked %d times, started %v", n, nodes.made())
			}
		})
	}
}

// The watch ends with the apply, whether it succeeds or fails, while the run goes on, and at once
// when the job is stopped, while OpenTofu still finishes.
func TestFirstEtcdWatchEndsWithTheApply(t *testing.T) {
	t.Parallel()
	for _, word := range []string{"held", "stall", "stopped"} {
		t.Run(word, func(t *testing.T) {
			t.Parallel()
			a, _ := newAppliance(t)
			nodes := newFirstNodes(map[string]string{})
			firstApply(t, a, nodes, word, createsTheCluster)
			eventually(t, "looked at the nodes", func() bool { return nodes.looked() > 0 })
			stop := func() {
				a.mu.Lock()
				a.job.stop(ErrServiceStopped)
				a.mu.Unlock()
			}
			if word == "stopped" {
				stop()
				stays(t, nodes, "once stopped")
			}
			releaseApply(t, a)
			if word == "stall" {
				eventually(t, "failed", func() bool {
					return slices.ContainsFunc(a.Current().Log, func(l string) bool { return strings.Contains(l, "Running again in") })
				})
				stays(t, nodes, "while the run waits to apply again")
				stop()
			}
			j := waitFor(t, a)
			if want := map[string]string{"held": StateSucceeded, "stall": StateFailed, "stopped": StateFailed}[word]; j.State != want {
				t.Errorf("job %s, want %s: %s", j.State, want, j.Error)
			}
			stays(t, nodes, "after the apply")
		})
	}
}

// A restart of the service mid-apply keeps the count: the next apply starts no node again in the
// boot it was started from, and counts on from there.
func TestFirstEtcdStartsSurviveARestart(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	nodes := newFirstNodes(map[string]string{"10.0.0.12": "Failed"})
	firstApply(t, a, nodes, "fail", createsTheCluster)
	eventually(t, "rebooted", func() bool { return len(nodes.made()) == 1 })
	releaseApply(t, a)
	waitFor(t, a)
	if err := os.Remove(filepath.Join(a.Dir, "release")); err != nil {
		t.Fatal(err)
	}

	b, _ := newAppliance(t)
	b.Dir, b.KeyDir = a.Dir, a.KeyDir
	firstApply(t, b, nodes, "held", createsTheCluster)
	before := nodes.looked()
	eventually(t, "looked again", func() bool { return nodes.looked() > before+8 })
	if got := nodes.made(); !slices.Equal(got, []string{"10.0.0.12 reboot"}) {
		t.Errorf("started %v in the same boot", got)
	}
	nodes.set("10.0.0.12", "Failed", 1)
	eventually(t, "rebooted in the next boot", func() bool { return len(nodes.made()) == 2 })
	releaseApply(t, b)
	waitFor(t, b)
	if got := readStarts(t, b)["10.0.0.12"]; got != (provisioner.EtcdStart{Boot: "1", Repairs: 2}) {
		t.Errorf("recorded %+v", got)
	}
}

// A removal forgets the starts, so that the next setup's cluster starts from none.
func TestRemovalForgetsTheEtcdStarts(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(a.etcdStartsPath(), map[string]provisioner.EtcdStart{"10.0.0.11": {Ran: true}}); err != nil {
		t.Fatal(err)
	}
	if err := a.forgetRemoved(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.etcdStartsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the starts are still there: %v", err)
	}
}
