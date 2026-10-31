package ui

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	// runsSucceededShown is how long a run that went well stays in the list, and runsFailedShown
	// one that failed, which waits for someone to look.
	runsSucceededShown = time.Hour
	runsFailedShown    = 24 * time.Hour
	// runsFresh is how long one read of a cluster's backups serves the runs of everyone.
	runsFresh = 30 * time.Second
)

// The states of a run.
const (
	runRunning = "running"
	// runStalled is a change of a cluster whose machines stopped coming (MachinesStalled).
	runStalled   = "stalled"
	runSucceeded = "succeeded"
	runFailed    = "failed"
)

// The kinds of run.
const (
	runManagement      = "management-cluster"
	runTemplate        = "template"
	runTemplateRefresh = "template-refresh"
	runApplianceUpdate = "appliance-upgrade"
	runEtcdSnapshot    = "etcd-snapshot"
	runEtcdRestore     = "etcd-restore"
	runBackup          = "backup"
	runRestore         = "restore"
)

// runView is a piece of work that runs on its own once started: a change of the management cluster,
// a template build, an upgrade of the appliance, or a cluster's snapshot, backup or restore.
type runView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Cluster   string `json:"cluster,omitempty"`
	// What names the run in a few words, and Phase says how far it has come.
	What     string     `json:"what"`
	State    string     `json:"state"`
	Phase    string     `json:"phase,omitempty"`
	By       string     `json:"by,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	// Error is the first thing that went wrong, as the run reports it.
	Error string `json:"error,omitempty"`
	// Steps are those a run takes, where it reports them.
	Steps []runStep `json:"steps,omitempty"`
	// Settings are what a change of a cluster sets.
	Settings []string `json:"settings,omitempty"`

	// What a failed run is started again with. Action is apply, remove or etcd-restore for a run
	// of the management cluster; Restore and Template are a restore's and a template build's
	// choices; Snapshot is what an etcd restore restores; Release is what an upgrade upgrades to.
	Action   string          `json:"action,omitempty"`
	Restore  *restoreRequest `json:"restore,omitempty"`
	Template json.RawMessage `json:"template,omitempty"`
	Snapshot string          `json:"snapshot,omitempty"`
	Release  string          `json:"release,omitempty"`
}

// runs answers with the work running or lately finished that the user may see: the appliance's
// and the template refresh's for those who may set up the management cluster, and the snapshots,
// backups and restores of the clusters the user sees.
func (s *Server) runs(w http.ResponseWriter, r *http.Request, u user) {
	shown, ok := s.shownClusters(w, r, u)
	if !ok {
		return
	}
	held, err := s.Accounts.Privileges(r.Context(), u.ticket, "/")
	if err != nil {
		s.proxmoxError(w, r, err)
		return
	}
	now := s.now()
	list := []runView{}
	if held.Has(setupPrivileges...) {
		list = append(list, s.applianceRuns()...)
		if kube := s.currentKube(); kube != nil {
			if state, err := s.refreshState(r.Context(), kube); err == nil {
				list = append(list, refreshRuns(state)...)
			}
		}
	}
	for _, c := range shown {
		list = append(list, etcdRuns(c)...)
	}
	ctx := r.Context()
	found := make([][]runView, len(shown))
	inParallel(len(shown), func(i int) {
		c := shown[i]
		if rec, ok := recentChange(c, now); ok {
			var machines clusterv1.MachineList
			if err := u.kube.List(ctx, &machines, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err == nil {
				found[i] = append(found[i], changeRun(c, machines.Items, rec))
			}
		}
		if c.Namespace != s.Namespace || !backupsOf(c).Enabled {
			return
		}
		key := c.Namespace + "/" + c.Name + "/" + string(c.UID)
		backups, _ := s.veleroRuns.get(ctx, key, runsFresh, s.now, func(ctx context.Context) []runView {
			return s.readVeleroRuns(ctx, u, c)
		})
		found[i] = append(found[i], backups...)
	})
	for _, f := range found {
		list = append(list, f...)
	}
	writeJSON(w, http.StatusOK, recentRuns(list, now))
}

// recentRuns keeps what runs and what finished lately, the running first, then the stalled, then
// the failed, each newest first.
func recentRuns(list []runView, now time.Time) []runView {
	kept := slices.DeleteFunc(list, func(v runView) bool {
		switch {
		case v.State == runRunning, v.State == runStalled:
			return false
		case v.State == "":
			return true
		case v.Finished == nil:
			return true
		case v.State == runFailed:
			return now.Sub(*v.Finished) > runsFailedShown
		default:
			return now.Sub(*v.Finished) > runsSucceededShown
		}
	})
	order := []string{runRunning, runStalled, runFailed, runSucceeded}
	slices.SortStableFunc(kept, func(a, b runView) int {
		if n := slices.Index(order, a.State) - slices.Index(order, b.State); n != 0 {
			return n
		}
		if n := cmpTime(b.Started, a.Started); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return kept
}

// inParallel calls f for every index below n, a few at a time.
func inParallel(n int, f func(i int)) {
	sem := make(chan struct{}, clusterReads)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			f(i)
		})
	}
	wg.Wait()
}

// applianceRuns are the appliance's last run of the module, its last template build and its last
// upgrade.
func (s *Server) applianceRuns() []runView {
	var list []runView
	if a := s.Appliance; a != nil {
		if j := a.Current(); j != nil {
			list = append(list, managementRun(j))
		}
		if j := a.TemplateJob(); j != nil {
			v := jobRun("template", runTemplate, "Build template", j)
			v.Template = j.Request
			list = append(list, v)
		}
	}
	if up := s.Upgrades; up != nil {
		if st := aboutRelease(up.Spool.Status(), up.Current); st != nil {
			if v, ok := updateRun(st); ok {
				list = append(list, v)
			}
		}
	}
	return list
}

// managementRun is the appliance's last run of the module, with which one it was.
func managementRun(j *appliance.JobView) runView {
	what := "Apply management cluster"
	switch j.Kind {
	case "remove":
		what = "Delete management cluster"
	case "etcd-restore":
		what = "Restore management cluster etcd"
	}
	v := jobRun("management-cluster", runManagement, what, j)
	v.Action = j.Kind
	var asked appliance.EtcdRestoreRequest
	if j.Kind == "etcd-restore" && json.Unmarshal(j.Request, &asked) == nil {
		v.Snapshot = asked.Snapshot
	}
	return v
}

func jobRun(id, kind, what string, j *appliance.JobView) runView {
	v := runView{ID: id, Kind: kind, What: what, State: j.State, By: j.User, Error: j.Error}
	if j.Automatic {
		v.By = ""
	}
	if !j.Started.IsZero() {
		v.Started = &j.Started
	}
	if !j.Finished.IsZero() {
		v.Finished = &j.Finished
	}
	if j.State == runRunning && len(j.Log) > 0 {
		v.Phase = j.Log[len(j.Log)-1]
	}
	return v
}

// updateRun is the updater's last upgrade of the appliance; finding nothing newer is no run.
func updateRun(st *appliance.UpdateStatus) (runView, bool) {
	at := st.Time
	v := runView{ID: "appliance-upgrade", Kind: runApplianceUpdate, What: "Upgrade appliance to " + st.To, Error: st.Error, Release: st.To}
	switch st.State {
	case "updating":
		v.State, v.Started = runRunning, &at
	case runSucceeded, runFailed:
		v.State, v.Finished = st.State, &at
	default:
		return runView{}, false
	}
	return v, true
}

// refreshRuns is the template refresh's last run, as the controller records it.
func refreshRuns(state *refreshState) []runView {
	st := state.status[provisioner.RefreshState]
	if state.schedule == nil || st == "" {
		return nil
	}
	v := runView{
		ID: "template-refresh", Kind: runTemplateRefresh, What: "Refresh templates", State: st,
		Started: parseStamp(state.status[provisioner.RefreshStarted]), Finished: parseStamp(state.status[provisioner.RefreshFinished]),
		Error: state.status[provisioner.RefreshError],
	}
	if lines := state.status[provisioner.RefreshLines]; lines != "" && st == runRunning {
		v.Phase = "Building " + lines
	}
	return []runView{v}
}

// etcdRuns are a cluster's etcd snapshot and restore, as the controller records them on it.
func etcdRuns(c *clusterv1.Cluster) []runView {
	base := runView{Namespace: c.Namespace, Cluster: c.Name}
	var list []runView
	if st, ok := provisioner.EtcdRestoreStatusOf(c); ok {
		v := base
		v.ID, v.Kind, v.What, v.By = "etcd-restore/"+clusterKey(c.Namespace, c.Name), runEtcdRestore, "Restore etcd from "+st.Snapshot, st.By
		v.Snapshot = st.Snapshot
		if st.Reset {
			v.What = "Recover the control plane on " + st.Machine
		}
		v.Started = &st.RequestedAt
		if !st.Started.IsZero() {
			v.Started = &st.Started
		}
		v.Finished = st.Finished
		switch st.Phase {
		case provisioner.RestoreSucceeded:
			v.State = runSucceeded
		case provisioner.RestoreFailed:
			v.State, v.Error = runFailed, st.Message
		case provisioner.RestorePreparing, provisioner.RestoreFetching, provisioner.RestoreStopping, provisioner.RestoreRestoring,
			provisioner.RestoreStarting, provisioner.RestoreReplacing:
			v.State, v.Phase = runRunning, string(st.Phase)
		}
		v.Steps = restoreSteps(&st, v.State)
		list = append(list, v)
	}
	snapshot := base
	snapshot.ID, snapshot.Kind, snapshot.What = "etcd-snapshot/"+clusterKey(c.Namespace, c.Name), runEtcdSnapshot, "Take etcd snapshot"
	cond := meta.FindStatusCondition(c.Status.Conditions, provisioner.EtcdSnapshotCondition)
	_, requested := c.Annotations[provisioner.EtcdSnapshotRequestAnnotation]
	switch {
	case requested:
		snapshot.State = runRunning
	case cond != nil && cond.Status == metav1.ConditionFalse:
		at := cond.LastTransitionTime.Time
		snapshot.State, snapshot.Finished, snapshot.Error = runFailed, &at, cond.Message
	default:
		last, ok := provisioner.LastEtcdSnapshot(c)
		if !ok {
			return list
		}
		snapshot.State, snapshot.Finished = runSucceeded, &last.Time
	}
	return append(list, snapshot)
}

// restoreOrder are the phases of an etcd restore, in the order it takes them.
var restoreOrder = []provisioner.RestorePhase{
	provisioner.RestorePreparing, provisioner.RestoreFetching, provisioner.RestoreStopping,
	provisioner.RestoreRestoring, provisioner.RestoreStarting, provisioner.RestoreReplacing,
}

// restoreSteps are an etcd restore's phases as steps: those before the one it is in done, and that
// one under way, or failed. Preparing, in which the restore snapshots etcd as it is, names that
// snapshot, or says why there is none.
func restoreSteps(st *provisioner.EtcdRestoreStatus, state string) []runStep {
	current := slices.Index(restoreOrder, cmp.Or(st.FailedIn, st.Phase))
	steps := make([]runStep, len(restoreOrder))
	for i, p := range restoreOrder {
		steps[i] = runStep{Name: string(p), State: stepWaiting}
		switch {
		case state == runSucceeded, i < current:
			steps[i].State = stepDone
		case i == current && state == runRunning:
			steps[i].State = stepRunning
		case i == current && state == runFailed:
			steps[i].State = stepFailed
		}
	}
	switch {
	case st.Before != "":
		steps[0].Detail = "undo snapshot " + st.Before
	case st.NoBefore != "":
		steps[0].Detail = "no undo snapshot: " + st.NoBefore
	}
	return steps
}

// readVeleroRuns are the backups and restores Velero keeps in a cluster; one it cannot read has none.
func (s *Server) readVeleroRuns(ctx context.Context, u user, c *clusterv1.Cluster) []runView {
	ctx, cancel := context.WithTimeout(ctx, veleroTimeout)
	defer cancel()
	v, _, err := s.velero(ctx, u, c, provisioner.IdentityBackups)
	if err != nil || v == nil {
		return nil
	}
	view := backupsView{}
	if err := readVelero(ctx, v, &view); err != nil {
		s.Log.InfoContext(ctx, "reading a cluster's backups for its runs", "cluster", clusterKey(c.Namespace, c.Name), "error", err)
		return nil
	}
	base := runView{Namespace: c.Namespace, Cluster: c.Name}
	var list []runView
	for i := range view.Backups {
		b := &view.Backups[i]
		// Another cluster's backup is a run of that cluster's.
		if b.From != "" {
			continue
		}
		r := base
		r.ID, r.Kind, r.What, r.By = "backup/"+clusterKey(c.Namespace, c.Name)+"/"+b.Name, runBackup, "Back up", b.By
		if b.Scheduled {
			r.What = "Scheduled backup"
		}
		r.Started, r.Finished = b.Started, b.Completed
		r.State, r.Error = veleroState(b.Phase, b.Failure, b.Errors)
		if r.State == runRunning && b.Total > 0 {
			r.Phase = fmt.Sprintf("%d of %d items", b.Items, b.Total)
		}
		list = append(list, r)
	}
	for i := range view.Restores {
		x := &view.Restores[i]
		r := base
		r.ID, r.Kind, r.What, r.By = "restore/"+clusterKey(c.Namespace, c.Name)+"/"+x.Name, runRestore, "Restore from "+x.Backup, x.By
		volumeData := x.VolumeData
		r.Restore = &restoreRequest{
			Backup: x.Backup, Namespaces: x.Namespaces, ExcludedNamespaces: x.ExcludedNamespaces,
			Resources: x.Resources, ExcludedResources: x.ExcludedResources, VolumeData: &volumeData,
		}
		r.Started, r.Finished = x.Started, x.Completed
		r.State, r.Error = veleroState(x.Phase, x.Failure, x.Errors)
		switch {
		case r.State != runRunning:
		case x.Volumes > 0:
			r.Phase = fmt.Sprintf("%d of %d volumes", x.VolumesDone, x.Volumes)
		case x.Total > 0:
			r.Phase = fmt.Sprintf("%d of %d items", x.Items, x.Total)
		}
		list = append(list, r)
	}
	return list
}

// veleroState is the state of a backup or restore in Velero's phase, with what went wrong.
func veleroState(phase, failure string, errs int64) (string, string) {
	switch phase {
	case veleroCompleted:
		return runSucceeded, ""
	case "Failed", "FailedValidation", "PartiallyFailed":
		if errs > 0 && failure == "" {
			failure = fmt.Sprintf("%d errors, see Velero's log", errs)
		}
		return runFailed, cmp.Or(failure, phase)
	case veleroDeleting:
		return "", ""
	default:
		return runRunning, ""
	}
}
