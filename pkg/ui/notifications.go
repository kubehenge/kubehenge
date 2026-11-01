package ui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/notify"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	// notifyEvery is how often the appliance looks for what to tell.
	notifyEvery = time.Minute
	// notifyHold is how long a problem stands before it is told, so that what a cluster passes
	// through (a node joining, a controller rolling, a pod restarting once) is not.
	notifyHold = 5 * time.Minute
	// notifyRunsKept is how long a failed run told is remembered, a day past the runs page keeping
	// it.
	notifyRunsKept = runsFailedShown + 24*time.Hour
	// notifyBatch is the most notices one pass sends one by one. More, as when the management
	// cluster stops answering and every cluster with it, go as one.
	notifyBatch = 5

	notifyStateFile             = "notifications.json"
	notifyStateMode fs.FileMode = 0o600
)

// notifyState is what has been told, kept in the appliance's folder so that a restart tells
// nothing twice.
type notifyState struct {
	// Since is when the appliance first looked with a target to tell. A run that failed before
	// is not told.
	Since    time.Time                 `json:"since"`
	Problems map[string]*notifyProblem `json:"problems"`
	// Runs are the failed runs told, by key, with when they were.
	Runs map[string]time.Time `json:"runs"`
	// Sent is each target's last sending.
	Sent map[string]notifySent `json:"sent"`
}

type notifyProblem struct {
	notify.Notice

	// Seen is when the problem began, or was first seen.
	Seen time.Time `json:"seen"`
	Told bool      `json:"told"`
}

type notifySent struct {
	At    time.Time `json:"at"`
	Title string    `json:"title,omitempty"`
	Error string    `json:"error,omitempty"`
}

// notifyFound is what one look found.
type notifyFound struct {
	problems []notify.Notice
	runs     []notify.Notice
	// read tells, for each cluster and for the management cluster (""), whether all of it was
	// read. A problem of a cluster read only in part stays as it was.
	read map[string]bool
	// listed tells that the clusters were listed, so that a problem of one not among them, since
	// deleted, goes without a word.
	listed bool
}

// notifier holds the state between looks. Its zero value is ready to use.
type notifier struct {
	mu      sync.Mutex
	state   notifyState
	loaded  bool
	targets []notify.Target
}

// notifyTargetsAt reads the targets' credentials in the interface's first namespace.
func notifyTargetsAt(ctx context.Context, kube client.Client, namespace string) ([]notify.Target, []error) {
	return notify.Targets(func(purpose string) (map[string][]byte, bool, error) {
		var secret corev1.Secret
		err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: provisioner.CredentialName("", purpose)}, &secret)
		switch {
		case apierrors.IsNotFound(err):
			return nil, false, nil
		case err != nil:
			return nil, false, fmt.Errorf("reading its credential: %w", err)
		}
		return secret.Data, true, nil
	}, nil)
}

// notifyLoop looks every notifyEvery until ctx ends.
func (s *Server) notifyLoop(ctx context.Context) {
	for {
		s.notifyOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(notifyEvery):
		}
	}
}

// notifyOnce looks once and tells what it found.
func (s *Server) notifyOnce(ctx context.Context) {
	n := &s.notifier
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.loaded {
		n.state = s.loadNotifyState()
		n.loaded = true
	}
	kube := s.currentKube()
	found := notifyFound{read: map[string]bool{}}
	var namespaces managedNamespaces
	if kube != nil {
		var err error
		if namespaces, err = s.namespaces(ctx, kube); err == nil && len(namespaces) > 0 {
			// The targets last read stay while the management cluster does not answer, which is
			// when they are needed most.
			targets, errs := notifyTargetsAt(ctx, kube, namespaces[0])
			for _, err := range errs {
				s.Log.Warn("a notification target is not usable", "error", err)
			}
			if len(errs) == 0 || len(targets) > 0 {
				n.targets = targets
			}
		}
	}
	if len(n.targets) == 0 {
		n.state.Since = time.Time{}
		return
	}
	s.findManagement(ctx, kube, namespaces, &found)
	if kube != nil && namespaces != nil {
		s.findClusters(ctx, kube, namespaces, &found)
	}
	found.runs = append(found.runs, s.failedRuns(s.applianceRuns(), "")...)
	tell := n.state.step(s.now(), &found)
	s.send(ctx, n, tell)
	s.saveNotifyState(&n.state)
}

// step folds what a look found into the state and returns what to tell.
func (st *notifyState) step(now time.Time, found *notifyFound) []notify.Notice {
	if st.Since.IsZero() {
		st.Since = now
	}
	if st.Problems == nil {
		st.Problems = map[string]*notifyProblem{}
	}
	if st.Runs == nil {
		st.Runs = map[string]time.Time{}
	}
	tell := st.stepProblems(now, found.problems)
	tell = append(tell, st.stepResolved(now, found)...)
	tell = append(tell, st.stepRuns(found.runs)...)
	maps.DeleteFunc(st.Runs, func(_ string, at time.Time) bool { return now.Sub(at) > notifyRunsKept })
	return tell
}

// stepProblems records the problems found and tells those that have stood notifyHold.
func (st *notifyState) stepProblems(now time.Time, found []notify.Notice) []notify.Notice {
	var tell []notify.Notice
	for i := range found {
		f := &found[i]
		p := st.Problems[f.Key]
		if p == nil {
			p = &notifyProblem{Seen: now}
			if !f.At.IsZero() && f.At.Before(now) {
				p.Seen = f.At
			}
			st.Problems[f.Key] = p
		}
		told := p.Told
		p.Notice = *f
		if !told && now.Sub(p.Seen) >= notifyHold {
			p.Told = true
			n := p.Notice
			n.At = now
			tell = append(tell, n)
		}
	}
	return tell
}

// stepResolved drops the problems no longer found where they were looked for, and tells those
// that were told.
func (st *notifyState) stepResolved(now time.Time, found *notifyFound) []notify.Notice {
	present := map[string]bool{}
	for i := range found.problems {
		present[found.problems[i].Key] = true
	}
	var tell []notify.Notice
	for _, key := range slices.Sorted(maps.Keys(st.Problems)) {
		p := st.Problems[key]
		if present[key] {
			continue
		}
		read, known := found.read[p.Cluster]
		if (known && !read) || (!known && !found.listed) {
			continue
		}
		delete(st.Problems, key)
		if known && p.Told {
			n := p.Notice
			n.Severity, n.Title, n.At = notify.Resolved, "Resolved: "+p.Title, now
			tell = append(tell, n)
		}
	}
	return tell
}

// stepRuns tells each failed run once, unless it failed before the first look.
func (st *notifyState) stepRuns(found []notify.Notice) []notify.Notice {
	var tell []notify.Notice
	for i := range found {
		r := &found[i]
		if _, told := st.Runs[r.Key]; told {
			continue
		}
		st.Runs[r.Key] = r.At
		if !r.At.Before(st.Since) {
			tell = append(tell, *r)
		}
	}
	return tell
}

// untell forgets that a notice was told, so that the next look tells it again.
func (st *notifyState) untell(n *notify.Notice) {
	if p := st.Problems[n.Key]; p != nil && n.Severity != notify.Resolved {
		p.Told = false
	}
	delete(st.Runs, n.Key)
}

// send tells every target, as one notice when there are more than notifyBatch. A notice no
// target took is told again on the next look.
func (s *Server) send(ctx context.Context, n *notifier, tell []notify.Notice) {
	if len(tell) == 0 {
		return
	}
	batches := [][]notify.Notice{}
	if len(tell) > notifyBatch {
		batches = append(batches, tell)
	} else {
		for _, t := range tell {
			batches = append(batches, []notify.Notice{t})
		}
	}
	if n.state.Sent == nil {
		n.state.Sent = map[string]notifySent{}
	}
	for _, batch := range batches {
		msg := batch[0]
		if len(batch) > 1 {
			msg = oneNotice(batch, s.now())
		}
		taken := false
		for _, t := range n.targets {
			err := t.Send(ctx, &msg)
			sent := notifySent{At: s.now(), Title: msg.Title}
			if err != nil {
				sent.Error = err.Error()
				s.Log.Warn("sending a notification failed", "target", t.Kind(), "title", msg.Title, "error", err)
			} else {
				taken = true
			}
			n.state.Sent[t.Kind()] = sent
		}
		if !taken {
			for i := range batch {
				n.state.untell(&batch[i])
			}
		}
	}
}

// oneNotice is one notice for many, the worst first.
func oneNotice(tell []notify.Notice, now time.Time) notify.Notice {
	worstFirst := []string{notify.Error, notify.Warning, notify.Info, notify.Resolved}
	rank := func(n notify.Notice) int { return slices.Index(worstFirst, n.Severity) }
	sorted := slices.Clone(tell)
	slices.SortStableFunc(sorted, func(a, b notify.Notice) int { return cmp.Compare(rank(a), rank(b)) })
	lines := make([]string, len(sorted))
	for i, t := range sorted {
		lines[i] = t.Title
	}
	return notify.Notice{
		Key: "batch", Severity: sorted[0].Severity, Title: fmt.Sprintf("%d things need a look", len(sorted)),
		Text: strings.Join(lines, "\n"), At: now,
	}
}

// findManagement adds the management cluster's problems: not answering, a node not ready, or a
// controller with fewer replicas ready than wanted.
func (s *Server) findManagement(ctx context.Context, kube client.Client, namespaces managedNamespaces, found *notifyFound) {
	link := s.notifyLink("/administration")
	if kube == nil {
		return
	}
	if namespaces == nil {
		found.read[""] = true
		found.problems = append(found.problems, notify.Notice{
			Key: "management/answers", Severity: notify.Error, Title: "Management cluster not answering", Link: link,
		})
		return
	}
	ctx, cancel := context.WithTimeout(ctx, managementHealthTimeout)
	defer cancel()
	v, err := readManagementHealth(ctx, kube)
	if err != nil {
		found.read[""] = true
		found.problems = append(found.problems, notify.Notice{
			Key: "management/answers", Severity: notify.Error, Title: "Management cluster not answering", Text: err.Error(), Link: link,
		})
		return
	}
	found.read[""] = true
	for _, node := range v.Nodes {
		if !node.Ready {
			found.problems = append(found.problems, notify.Notice{
				Key: "management/node/" + node.Name, Severity: notify.Error, Title: "Management node " + node.Name + " not ready",
				Text: node.Message, Link: link,
			})
		}
	}
	for _, c := range v.Controllers {
		if c.Ready < c.Wanted {
			found.problems = append(found.problems, notify.Notice{
				Key: "management/controller/" + c.Namespace + "/" + c.Name, Severity: notify.Warning,
				Title: c.Name + " not ready", Text: fmt.Sprintf("%d of %d replicas ready in %s.", c.Ready, c.Wanted, c.Namespace), Link: link,
			})
		}
	}
}

// findClusters adds every Kubehenge cluster's problems, as its health page shows them, and its
// failed runs.
func (s *Server) findClusters(ctx context.Context, kube client.Client, namespaces managedNamespaces, found *notifyFound) {
	var list clusterv1.ClusterList
	if err := kube.List(ctx, &list); err != nil {
		return
	}
	found.listed = true
	var clusters []*clusterv1.Cluster
	for i := range list.Items {
		if c := &list.Items[i]; provisioner.Owns(c, s.ClassNamespace) {
			clusters = append(clusters, c)
		}
	}
	var runs []runView
	if state, err := s.refreshState(ctx, kube); err == nil {
		runs = append(runs, refreshRuns(state)...)
	}
	found.runs = append(found.runs, s.failedRuns(runs, "")...)
	type result struct {
		problems []notify.Notice
		runs     []notify.Notice
		read     bool
	}
	results := make([]result, len(clusters))
	inParallel(len(clusters), func(i int) {
		c := clusters[i]
		key := c.Namespace + "/" + c.Name + "/" + string(c.UID)
		v, err := s.health.get(ctx, key, fleetHealthFresh, s.now, func(ctx context.Context) healthView {
			return s.readHealth(ctx, kube, c, namespaces)
		})
		r := &results[i]
		if err == nil {
			r.problems, r.read = s.clusterProblems(c, &v)
		}
		cr := etcdRuns(c)
		if c.Namespace == s.Namespace && backupsOf(c).Enabled {
			u := user{kube: kube, namespaces: namespaces}
			backups, _ := s.veleroRuns.get(ctx, key, runsFresh, s.now, func(ctx context.Context) []runView {
				return s.readVeleroRuns(ctx, u, c)
			})
			cr = append(cr, backups...)
		}
		r.runs = s.failedRuns(cr, s.clusterName(c))
	})
	for i, c := range clusters {
		found.read[clusterKey(c.Namespace, c.Name)] = results[i].read
		found.problems = append(found.problems, results[i].problems...)
		found.runs = append(found.runs, results[i].runs...)
	}
}

// clusterProblems are the errors and warnings of a cluster's health page, and those of
// Kubehenge's controller that the page shows elsewhere. Whether the page read all of the cluster
// comes with them.
func (s *Server) clusterProblems(c *clusterv1.Cluster, v *healthView) ([]notify.Notice, bool) {
	if !c.DeletionTimestamp.IsZero() && trueCondition(c, provisioner.DeletionStalledCondition) == nil {
		return nil, true
	}
	name, key := s.clusterName(c), clusterKey(c.Namespace, c.Name)
	link := s.notifyLink("/clusters/" + key + "/health")
	var out []notify.Notice
	add := func(id, severity, what, text string, since time.Time) {
		out = append(out, notify.Notice{
			Key: key + "/" + string(c.UID) + "/" + id, Severity: severity, Title: name + ": " + what, Text: text,
			Cluster: key, Link: link, At: since,
		})
	}
	for _, a := range v.Attention {
		if a.Severity != severityError && a.Severity != severityWarning {
			continue
		}
		id := a.What
		if a.Machine != "" {
			id += "/" + a.Machine
		}
		if a.Pod != nil {
			id += "/" + a.Pod.Namespace + "/" + a.Pod.Name
		}
		add(id, a.Severity, a.What, a.Message, a.Since)
	}
	if cond := meta.FindStatusCondition(c.Status.Conditions, provisioner.KubehengeReadyCondition); cond != nil && cond.Status == metav1.ConditionFalse {
		add("KubehengeReady", notify.Error, "Kubehenge cannot apply it", cond.Message, cond.LastTransitionTime.Time)
	}
	if cond := trueCondition(c, provisioner.MachinesStalledCondition); cond != nil {
		add("MachinesStalled", notify.Warning, "Machines stalled", cond.Message, cond.LastTransitionTime.Time)
	}
	unreachable := v.Verdict == verdictUnreachable
	if unreachable {
		add("unreachable", notify.Error, "Unreachable", v.Workload, time.Time{})
	}
	return out, !unreachable
}

// failedRuns are the failed runs among list, each keyed by its run and when it ended.
func (s *Server) failedRuns(list []runView, cluster string) []notify.Notice {
	var out []notify.Notice
	for i := range list {
		r := &list[i]
		if r.State != runFailed {
			continue
		}
		at := s.now()
		if r.Finished != nil {
			at = *r.Finished
		} else if r.Started != nil {
			at = *r.Started
		}
		n := notify.Notice{
			Key: "run/" + r.ID + "/" + at.UTC().Format(time.RFC3339), Severity: notify.Error, Title: r.What + " failed",
			Text: r.Error, Link: s.notifyLink("/runs/" + r.ID), At: at,
		}
		if cluster != "" {
			n.Title, n.Cluster = cluster+": "+n.Title, clusterKey(r.Namespace, r.Cluster)
		}
		out = append(out, n)
	}
	return out
}

// clusterName is how a notice names a cluster: its name, with its namespace where that is not the
// interface's own.
func (s *Server) clusterName(c *clusterv1.Cluster) string {
	if c.Namespace == s.Namespace {
		return c.Name
	}
	return clusterKey(c.Namespace, c.Name)
}

func (s *Server) notifyLink(path string) string {
	if s.PublicURL == "" {
		return ""
	}
	return strings.TrimSuffix(s.PublicURL, "/") + path
}

func (s *Server) notifyStatePath() string {
	if s.Appliance == nil || s.Appliance.Dir == "" {
		return ""
	}
	return filepath.Join(s.Appliance.Dir, notifyStateFile)
}

func (s *Server) loadNotifyState() notifyState {
	var st notifyState
	path := s.notifyStatePath()
	if path == "" {
		return st
	}
	b, err := os.ReadFile(path) //nolint:gosec // the appliance's own folder
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.Log.Warn("reading what was told", "error", err)
		}
		return st
	}
	if err := json.Unmarshal(b, &st); err != nil {
		s.Log.Warn("reading what was told", "error", err)
		return notifyState{}
	}
	return st
}

// saveNotifyState writes the state whole or not at all.
func (s *Server) saveNotifyState(st *notifyState) {
	path := s.notifyStatePath()
	if path == "" {
		return
	}
	b, err := json.Marshal(st)
	if err == nil {
		tmp := path + ".new"
		if err = os.WriteFile(tmp, b, notifyStateMode); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		s.Log.Warn("keeping what was told", "error", err)
	}
}

// notificationView is a target's last sending, for the Administration page.
type notificationView struct {
	Kind  string     `json:"kind"`
	At    *time.Time `json:"at,omitempty"`
	Title string     `json:"title,omitempty"`
	Error string     `json:"error,omitempty"`
}

// notifications answers with each target's last sending.
func (s *Server) notifications(w http.ResponseWriter, r *http.Request, u user) {
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	n := &s.notifier
	n.mu.Lock()
	sent := maps.Clone(n.state.Sent)
	n.mu.Unlock()
	out := make([]notificationView, 0, len(notify.Purposes))
	for _, kind := range notify.Purposes {
		v := notificationView{Kind: kind}
		if last, ok := sent[kind]; ok {
			v.At, v.Title, v.Error = &last.At, last.Title, last.Error
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// testNotifications sends a notice to every target stored, and answers with how each went.
func (s *Server) testNotifications(w http.ResponseWriter, r *http.Request, u user) {
	if !s.require(w, r, u, "/", setupPrivileges...) {
		return
	}
	targets, errs := notifyTargetsAt(r.Context(), u.kube, u.namespaces[0])
	out := []notificationView{}
	for _, err := range errs {
		kind, _, _ := strings.Cut(err.Error(), ":")
		out = append(out, notificationView{Kind: kind, Error: err.Error()})
	}
	if len(targets) == 0 && len(errs) == 0 {
		writeError(w, http.StatusConflict, "no notification target is set")
		return
	}
	now := s.now()
	msg := notify.Notice{Key: "test", Severity: notify.Info, Title: "Kubehenge test", Text: "Sent by " + u.ticket.Username + ".", Link: s.notifyLink("/administration/notifications"), At: now}
	for _, t := range targets {
		v := notificationView{Kind: t.Kind(), At: &now, Title: msg.Title}
		if err := t.Send(r.Context(), &msg); err != nil {
			v.Error = err.Error()
		}
		out = append(out, v)
	}
	s.Log.InfoContext(r.Context(), "sent a test notification", "user", u.ticket.Username)
	writeJSON(w, http.StatusOK, out)
}
