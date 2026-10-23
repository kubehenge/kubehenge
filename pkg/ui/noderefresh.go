package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// refreshView is the template refresh as the Administration page shows it: its schedule, whether a
// run was asked for, and how the last one went.
type refreshView struct {
	Weekday       string     `json:"weekday"`
	Time          string     `json:"time"`
	TimeZone      string     `json:"timeZone"`
	SoakHours     int        `json:"soakHours"`
	CreatorBuilds bool       `json:"creatorBuilds"`
	Next          *time.Time `json:"next,omitempty"`
	// Requested are the builds asked for that have not started: "refresh", or a missing line.
	Requested []string  `json:"requested"`
	Last      *lastView `json:"last,omitempty"`
}

type lastView struct {
	State    string     `json:"state"`
	Trigger  string     `json:"trigger,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	Lines    []string   `json:"lines"`
	Error    string     `json:"error,omitempty"`
	Log      []string   `json:"log"`
}

// refreshState is what the refresh's ConfigMaps hold, or nil schedule when the refresh is off.
type refreshState struct {
	schedule  *provisioner.RefreshSchedule
	status    map[string]string
	requested []string
}

// soak is how long a build waits before clusters move to it, the controller's default while the
// refresh is off.
func (r *refreshState) soak() time.Duration {
	if r.schedule == nil {
		return provisioner.DefaultNodeImageSoak
	}
	return r.schedule.Soak
}

func (s *Server) refreshState(ctx context.Context, kube client.Client) (*refreshState, error) {
	state := &refreshState{requested: []string{}}
	cm := &corev1.ConfigMap{}
	err := kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: provisioner.TemplateRefreshName}, cm)
	switch {
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		// The chart grants the read only with the refresh on.
		return state, nil
	case err != nil:
		return nil, fmt.Errorf("reading the template refresh: %w", err)
	}
	schedule, err := provisioner.ParseRefreshSchedule(cm.Data)
	if err != nil {
		return nil, fmt.Errorf("reading the template refresh's schedule: %w", err)
	}
	state.schedule, state.status = &schedule, cm.Data
	requests := &corev1.ConfigMap{}
	err = kube.Get(ctx, client.ObjectKey{Namespace: s.ClassNamespace, Name: provisioner.TemplateRequestsName}, requests)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading the template requests: %w", err)
	}
	for key := range requests.Data {
		state.requested = append(state.requested, key)
	}
	slices.Sort(state.requested)
	return state, nil
}

func (r *refreshState) view(now time.Time) *refreshView {
	if r.schedule == nil {
		return nil
	}
	v := &refreshView{
		Weekday: r.schedule.Weekday, Time: r.schedule.Time, TimeZone: r.schedule.TimeZone,
		SoakHours: int(r.schedule.Soak / time.Hour), CreatorBuilds: r.schedule.CreatorBuilds, Requested: r.requested,
	}
	if next, err := r.schedule.Next(now); err == nil {
		v.Next = &next
	}
	if state := r.status[provisioner.RefreshState]; state != "" {
		v.Last = &lastView{
			State: state, Trigger: r.status[provisioner.RefreshTrigger], Error: r.status[provisioner.RefreshError],
			Started: parseStamp(r.status[provisioner.RefreshStarted]), Finished: parseStamp(r.status[provisioner.RefreshFinished]),
			Lines: splitList(r.status[provisioner.RefreshLines], ", "), Log: splitList(r.status[provisioner.RefreshLog], "\n"),
		}
	}
	return v
}

func parseStamp(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

func splitList(s, sep string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, sep)
}

// requestBuild asks the controller for a run of the refresh: key is provisioner.RefreshRequest, or
// a missing line.
func (s *Server) requestBuild(ctx context.Context, kube client.Client, key, by string) error {
	raw, err := json.Marshal(provisioner.TemplateRequest{By: by, At: time.Now().UTC().Truncate(time.Second)})
	if err != nil {
		return fmt.Errorf("writing the request: %w", err)
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{key: string(raw)}})
	if err != nil {
		return fmt.Errorf("writing the request: %w", err)
	}
	cm := &corev1.ConfigMap{Namespace: s.ClassNamespace, Name: provisioner.TemplateRequestsName}
	if err := kube.Patch(ctx, cm, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("asking for the build: %w", err)
	}
	return nil
}

// refreshNow has the refresh build every line clusters run now, as the schedule would.
func (s *Server) refreshNow(w http.ResponseWriter, r *http.Request, u user) {
	if !s.templatesReady(w, r, u) {
		return
	}
	kube := s.currentKube()
	state, err := s.refreshState(r.Context(), kube)
	if err != nil {
		s.kubeError(w, err)
		return
	}
	if state.schedule == nil {
		writeError(w, http.StatusConflict, "the template refresh is off; switch it on in the management cluster's settings")
		return
	}
	if err := s.requestBuild(r.Context(), kube, provisioner.RefreshRequest, u.ticket.Username); err != nil {
		s.kubeError(w, err)
		return
	}
	s.Log.Info("template refresh asked for", "user", u.ticket.Username)
	state.requested = append(state.requested, provisioner.RefreshRequest)
	writeJSON(w, http.StatusAccepted, state.view(time.Now()))
}

// templateBuildsView tells the create wizard whether its user may have a missing template built,
// and which builds are under way or asked for.
type templateBuildsView struct {
	Allowed bool `json:"allowed"`
	// Requested are the request keys not started yet, such as kubeadm-v1.37.1-almalinux-10 or
	// rke2-almalinux-10.
	Requested []string `json:"requested"`
	// Building are the templates the run under way builds.
	Building []string `json:"building"`
}

func (s *Server) templateBuilds(w http.ResponseWriter, r *http.Request, u user) {
	state, err := s.refreshState(r.Context(), u.kube)
	if err != nil {
		s.kubeError(w, err)
		return
	}
	v := templateBuildsView{Requested: state.requested, Building: []string{}}
	if state.schedule != nil && state.schedule.CreatorBuilds {
		held, err := s.Accounts.Privileges(r.Context(), u.ticket, s.workloadPoolPath())
		if err != nil {
			s.proxmoxError(w, err)
			return
		}
		v.Allowed = held.Has(privChange)
	}
	if state.status[provisioner.RefreshState] == "running" {
		v.Building = splitList(state.status[provisioner.RefreshLines], ", ")
	}
	writeJSON(w, http.StatusOK, v)
}

type templateBuildRequest struct {
	// Distribution is rke2, k3s or kubeadm.
	Distribution      string `json:"distribution"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	// NodeOS is the node OS as one word, such as almalinux-10; empty means the default.
	NodeOS string `json:"nodeOS,omitempty"`
}

// requestTemplateBuild has a missing template built for one who may create clusters. The build
// runs in the management cluster with the builder's token, from images whose addresses and
// checksums Kubehenge fixes; the request names only the distribution, version and node OS.
func (s *Server) requestTemplateBuild(w http.ResponseWriter, r *http.Request, u user) {
	var req templateBuildRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !slices.Contains([]string{string(templates.RKE2), string(templates.K3s), string(templates.Kubeadm)}, req.Distribution) {
		writeError(w, http.StatusUnprocessableEntity, "distribution: RKE2, k3s or kubeadm")
		return
	}
	if req.Distribution != string(templates.Kubeadm) && req.KubernetesVersion != "" {
		writeError(w, http.StatusUnprocessableEntity, "Kubernetes version: only kubeadm templates are built per version")
		return
	}
	plan := templates.Request{Distribution: templates.Distribution(req.Distribution), KubernetesVersion: req.KubernetesVersion, NodeOS: templates.DefaultNodeOS}
	if req.NodeOS != "" {
		o, err := templates.ParseNodeOS(req.NodeOS)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "node OS: "+err.Error())
			return
		}
		plan.NodeOS = o
	}
	if _, err := templates.Plan(&plan); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	key := provisioner.RequestLine(&plan)
	state, err := s.refreshState(r.Context(), u.kube)
	if err != nil {
		s.kubeError(w, err)
		return
	}
	if state.schedule == nil || !state.schedule.CreatorBuilds {
		writeError(w, http.StatusForbidden, "an administrator builds the VM templates here; ask for this one on the Administration page")
		return
	}
	if !s.require(w, r, u, s.workloadPoolPath(), privChange) {
		return
	}
	if err := s.requestBuild(r.Context(), u.kube, key, u.ticket.Username); err != nil {
		s.kubeError(w, err)
		return
	}
	s.Log.Info("template build asked for", "user", u.ticket.Username, "template", key)
	w.WriteHeader(http.StatusAccepted)
}

// nodeImageView is which template builds a cluster's nodes are cloned from, and when they move to
// a newer one.
type nodeImageView struct {
	// Refreshes says the nodes move to new builds inside the window.
	Refreshes bool `json:"refreshes"`
	// Since is the cluster's node image time: its nodes run the newest builds offered by then.
	Since  time.Time        `json:"since"`
	Builds []nodeBuildView  `json:"builds"`
	Last   string           `json:"last,omitempty"`
	Next   *nodeRefreshView `json:"next,omitempty"`
}

type nodeBuildView struct {
	Template string    `json:"template"`
	Build    string    `json:"build"`
	Built    time.Time `json:"built"`
}

// nodeRefreshView is the coming refresh: the build it takes, or none while it is not built yet,
// and when it can start at the earliest.
type nodeRefreshView struct {
	Build string    `json:"build,omitempty"`
	Due   time.Time `json:"due"`
}

// nodeImage describes the node images of a cluster whose nodes are refreshed.
func (s *Server) nodeImage(ctx context.Context, kube client.Client, c *clusterv1.Cluster, now time.Time) (*nodeImageView, error) {
	builds, err := provisioner.ClassBuilds(ctx, kube, c)
	if err != nil {
		return nil, fmt.Errorf("reading the builds offered to the cluster: %w", err)
	}
	since, err := provisioner.NodeImageTime(c)
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's node image time: %w", err)
	}
	state, err := s.refreshState(ctx, kube)
	if err != nil {
		return nil, err
	}
	policy, _, err := clusterUpgradePolicy(c)
	if err != nil {
		return nil, err
	}
	dist, _ := distribution(c.Spec.Topology.ClassRef.Name)
	nodeOS, err := provisioner.ClusterNodeOS(c)
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's node OS: %w", err)
	}
	v := &nodeImageView{Refreshes: policy.RefreshesNodeImages(), Since: since, Builds: []nodeBuildView{}, Last: c.Annotations[provisioner.NodeImageRefreshedAnnotation]}
	seen := map[string]bool{}
	var due *nodeRefreshView
	soak := state.soak()
	for _, version := range statusVersions(c) {
		t, err := provisioner.MachineTemplate(dist, version, nodeOS)
		if err != nil || seen[t.LineName] {
			continue
		}
		seen[t.LineName] = true
		at, ok := builds.Pick(t.Tags, since, soak)
		if !ok {
			continue
		}
		v.Builds = append(v.Builds, nodeBuildView{Template: t.LineName, Build: templates.BuildTag(at), Built: at})
		if !v.Refreshes {
			continue
		}
		if next := nextRefresh(builds, state, &policy.Window, t.Tags, at, now, soak); next != nil && (due == nil || next.Due.Before(due.Due)) {
			due = next
		}
	}
	v.Next = due
	if held, ok := heldRefresh(c.Annotations[provisioner.NodeImageHeldAnnotation]); ok && v.Refreshes {
		// The upgrader's own decision, which outranks working the move out again here.
		v.Next = held
	}
	return v, nil
}

// heldRefresh reads provisioner.NodeImageHeldAnnotation: "<build> at <time the window opens>".
func heldRefresh(s string) (*nodeRefreshView, bool) {
	build, at, ok := strings.Cut(s, " at ")
	if !ok {
		return nil, false
	}
	due, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return nil, false
	}
	return &nodeRefreshView{Build: build, Due: due}, true
}

// statusVersions are a cluster's Kubernetes version and those its status says parts of it run.
func statusVersions(c *clusterv1.Cluster) []string {
	versions := []string{c.Spec.Topology.Version}
	if cp := c.Status.ControlPlane; cp != nil {
		for _, ver := range cp.Versions {
			versions = append(versions, ver.Version)
		}
	}
	if ws := c.Status.Workers; ws != nil {
		for _, ver := range ws.Versions {
			versions = append(versions, ver.Version)
		}
	}
	return versions
}

// nextRefresh is when the nodes of one line move on: to a newer build, inside the first window
// after it is offered, or else to the one the next scheduled run makes.
func nextRefresh(builds provisioner.NodeImageBuilds, state *refreshState, window *provisioner.Window, line []string, current, now time.Time, soak time.Duration) *nodeRefreshView {
	var offered time.Time
	build := ""
	if newer, ok := builds.Newer(line, current); ok {
		offered, build = newer.Add(soak), templates.BuildTag(newer)
	} else if state.schedule != nil {
		run, err := state.schedule.Next(now)
		if err != nil {
			return nil
		}
		offered = run.Add(soak)
	} else {
		return nil
	}
	due, err := window.Next(maxTime(offered, now))
	if err != nil {
		return nil
	}
	return &nodeRefreshView{Build: build, Due: due}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// clusterUpgradePolicy reads the cluster's upgrades variable; unset, it takes the class's defaults.
func clusterUpgradePolicy(c *clusterv1.Cluster) (provisioner.UpgradePolicy, bool, error) {
	var p provisioner.UpgradePolicy
	for _, cv := range c.Spec.Topology.Variables {
		if cv.Name == "upgrades" {
			if err := json.Unmarshal(cv.Value.Raw, &p); err != nil {
				return p, true, fmt.Errorf("cluster %s/%s: variable upgrades: %w", c.Namespace, c.Name, err)
			}
			return p, true, nil
		}
	}
	return p, false, nil
}
