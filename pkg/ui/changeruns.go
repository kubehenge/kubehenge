package ui

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// changeAnnotation records on a cluster the last change made through the interface: who made it,
// when, and what each of its settings does to the machines, which its run follows.
const changeAnnotation = "kubehenge.dev/change"

// runChange is a change of a cluster's settings rolling out.
const runChange = "change"

// How far a step of a run has come.
const (
	stepDone    = "done"
	stepRunning = "running"
	stepWaiting = "waiting"
	stepFailed  = "failed"
)

// changesShown is how long after it was made a change still counts as a run.
const changesShown = 24 * time.Hour

// runStep is one step of a run, in the order the run takes them.
type runStep struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type changeRecord struct {
	By       string          `json:"by"`
	At       time.Time       `json:"at"`
	Settings []plannedChange `json:"settings"`
}

// recordChange notes a change on the cluster it is made on, in the same update.
func recordChange(c *clusterv1.Cluster, by string, at time.Time, settings []plannedChange) {
	raw, err := json.Marshal(changeRecord{By: by, At: at.UTC().Truncate(time.Second), Settings: settings})
	if err != nil {
		return
	}
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[changeAnnotation] = string(raw)
}

func changeOf(c *clusterv1.Cluster) (changeRecord, bool) {
	var rec changeRecord
	raw, ok := c.Annotations[changeAnnotation]
	if !ok || json.Unmarshal([]byte(raw), &rec) != nil || rec.At.IsZero() {
		return changeRecord{}, false
	}
	return rec, true
}

// recentChange is a cluster's last change, while it still counts as a run.
func recentChange(c *clusterv1.Cluster, now time.Time) (changeRecord, bool) {
	rec, ok := changeOf(c)
	return rec, ok && now.Sub(rec.At) <= changesShown
}

func (rec *changeRecord) does(effects ...string) bool {
	return slices.ContainsFunc(rec.Settings, func(p plannedChange) bool { return slices.Contains(effects, p.Effect) })
}

// replaced counts the machines of a role a change has to replace: those made since it, ready, of
// those the cluster asks for, and those left from before it.
type replaced struct {
	fresh, old, want int
}

func (r replaced) step(name string) runStep {
	st := runStep{Name: name, State: stepRunning, Detail: fmt.Sprintf("%d of %d", min(r.fresh, r.want), r.want)}
	if r.fresh >= r.want && r.old == 0 {
		st.State = stepDone
	}
	return st
}

// changeRun is a cluster's last change as a run: accepted, then the machines it replaces or adds,
// then the cluster settling on it.
func changeRun(c *clusterv1.Cluster, machines []clusterv1.Machine, rec changeRecord) runView {
	v := runView{
		ID: "change/" + clusterKey(c.Namespace, c.Name), Kind: runChange, Namespace: c.Namespace, Cluster: c.Name,
		What: "Change the cluster", By: rec.By, Started: &rec.At,
	}
	for _, p := range rec.Settings {
		v.Settings = append(v.Settings, p.Setting)
	}
	controlPlane, workers := rolesSince(c, machines, rec.At)
	last := rec.At
	steps := []runStep{{Name: "Accepted by the API server", State: stepDone}}
	if rec.does(effectControlPlane, effectAll) {
		steps = append(steps, controlPlane.step("Replace the control-plane machines"))
	}
	if rec.does(effectWorkers, effectAll) {
		steps = append(steps, workers.step("Replace the workers"))
	}
	if rec.does(effectScales) {
		steps = append(steps, scaled(c, machines))
	}
	for i := range machines {
		if t := machines[i].CreationTimestamp.Time; t.After(last) {
			last = t
		}
	}
	settle, at := settleStep(c, steps)
	if at.After(last) {
		last = at
	}
	steps = append(steps, settle)
	v.Steps, v.State, v.Phase = steps, runRunning, phaseOf(steps)
	if settle.State == stepDone {
		v.State, v.Phase, v.Finished = runSucceeded, "", &last
	}
	return v
}

// settleStep waits for the steps before it, then for the cluster's topology to take the change in
// and the cluster to be available, and says when it became so.
func settleStep(c *clusterv1.Cluster, before []runStep) (runStep, time.Time) {
	settle := runStep{Name: "Settle", State: stepWaiting}
	if slices.ContainsFunc(before, func(s runStep) bool { return s.State != stepDone }) {
		return settle, time.Time{}
	}
	reconciled := meta.FindStatusCondition(c.Status.Conditions, clusterv1.ClusterTopologyReconciledCondition)
	available := meta.FindStatusCondition(c.Status.Conditions, clusterv1.ClusterAvailableCondition)
	settle.State = stepRunning
	switch {
	case c.Status.ObservedGeneration < c.Generation || reconciled == nil || reconciled.Status != metav1.ConditionTrue:
		if reconciled != nil {
			settle.Detail = reconciled.Message
		}
	case available == nil || available.Status != metav1.ConditionTrue:
		if available != nil {
			settle.Detail = available.Message
		}
	default:
		settle.State = stepDone
		return settle, available.LastTransitionTime.Time
	}
	return settle, time.Time{}
}

// phaseOf says how far a run has come: the step it is on, with how far that has.
func phaseOf(steps []runStep) string {
	for _, s := range steps {
		if s.State != stepRunning {
			continue
		}
		if s.Detail == "" {
			return s.Name
		}
		return s.Name + ": " + s.Detail
	}
	return ""
}

// rolesSince counts the control-plane machines and the workers made since a change, ready, and
// those still there from before it.
func rolesSince(c *clusterv1.Cluster, machines []clusterv1.Machine, at time.Time) (replaced, replaced) {
	var controlPlane, workers replaced
	for i := range machines {
		m := &machines[i]
		if !m.DeletionTimestamp.IsZero() && m.CreationTimestamp.After(at) {
			continue
		}
		role := &workers
		if _, cp := m.Labels[clusterv1.MachineControlPlaneLabel]; cp {
			role = &controlPlane
		}
		switch {
		case m.CreationTimestamp.Time.Before(at):
			role.old++
		case meta.IsStatusConditionTrue(m.Status.Conditions, clusterv1.MachineReadyCondition):
			role.fresh++
		}
	}
	controlPlane.want, workers.want = wanted(c, machines)
	return controlPlane, workers
}

// wanted are the control-plane machines and the workers a cluster asks for; an autoscaled pool
// asks for those it has.
func wanted(c *clusterv1.Cluster, machines []clusterv1.Machine) (int, int) {
	has := map[string]int{}
	controlPlane := 0
	for i := range machines {
		m := &machines[i]
		if !m.DeletionTimestamp.IsZero() {
			continue
		}
		if _, cp := m.Labels[clusterv1.MachineControlPlaneLabel]; cp {
			controlPlane++
			continue
		}
		has[m.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel]]++
	}
	if n := c.Spec.Topology.ControlPlane.Replicas; n != nil {
		controlPlane = int(*n)
	}
	workers := 0
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		md := &c.Spec.Topology.Workers.MachineDeployments[i]
		if md.Replicas != nil {
			workers += int(*md.Replicas)
		} else {
			workers += has[md.Name]
		}
	}
	return controlPlane, workers
}

// scaled is the step of a change that adds or removes machines: done once the cluster has the
// machines it asks for, each ready.
func scaled(c *clusterv1.Cluster, machines []clusterv1.Machine) runStep {
	controlPlane, workers := wanted(c, machines)
	ready, all := 0, 0
	for i := range machines {
		m := &machines[i]
		all++
		if m.DeletionTimestamp.IsZero() && meta.IsStatusConditionTrue(m.Status.Conditions, clusterv1.MachineReadyCondition) {
			ready++
		}
	}
	want := controlPlane + workers
	st := runStep{Name: "Add or remove machines", State: stepRunning, Detail: fmt.Sprintf("%d of %d ready", min(ready, want), want)}
	if ready == want && all == want {
		st.State = stepDone
	}
	return st
}
