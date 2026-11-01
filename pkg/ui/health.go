package ui

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// How the health page reads a cluster.
const (
	// healthFresh is how long one read of a cluster's health serves everyone looking at it.
	healthFresh = 10 * time.Second
	// fleetHealthFresh is how long one serves the fleet's summary, which only needs the worst of
	// each cluster and reads them all at once.
	fleetHealthFresh = 30 * time.Second
	// healthDeadline bounds each source's reads, so one that does not answer holds up no other.
	healthDeadline = 5 * time.Second
	// healthListLimit bounds a list from a workload cluster, such as the pods of a very large one.
	healthListLimit = 5000
	// eventWindow is how far back the events and past failures shown go.
	eventWindow = time.Hour
	// settling is how long a node, pod or volume claim may take to come up before it is a problem.
	settling = 2 * time.Minute
	// eventsShown bounds the events listed, newest first.
	eventsShown = 200
	// podsNeedingAttention bounds the pods in the needs-attention list. The pods' own section has
	// them all.
	podsNeedingAttention = 20
)

// How bad something on the page is.
const (
	severityError    = "error"
	severityWarning  = "warning"
	severityProgress = "progress"
)

// severities are in order, the worst first.
var severities = []string{severityError, severityWarning, severityProgress}

// severityRank is a severity's place among severities, 0 for the worst.
func severityRank(severity string) int { return slices.Index(severities, severity) }

// The verdict at the top of the page.
const (
	verdictHealthy     = "healthy"
	verdictChanging    = "changing"
	verdictDegraded    = "degraded"
	verdictUnreachable = "unreachable"
)

// The page's sections, which each item needing attention names as where its details are.
const (
	sectionNodes        = "nodes"
	sectionPods         = "pods"
	sectionVolumes      = "volumes"
	sectionAddons       = "addons"
	sectionClusterAPI   = "cluster-api"
	sectionControlPlane = "control-plane"
)

// section is one part of the health page from one source: what was read, or why it was not.
type section[T any] struct {
	Items []T    `json:"items"`
	Error string `json:"error,omitempty"`
	// Truncated says the list stopped at healthListLimit.
	Truncated bool `json:"truncated,omitempty"`
}

// healthView is the health page. Of what runs in the cluster it carries names, reasons, messages
// and counts, never a spec: a pod's environment and commands stay in the backend.
type healthView struct {
	Read      time.Time       `json:"read"`
	Verdict   string          `json:"verdict"`
	Attention []attentionView `json:"attention"`
	// Workload says why nothing from inside the cluster shows, when nothing does.
	Workload     string                   `json:"workload,omitempty"`
	Nodes        section[nodeView]        `json:"nodes"`
	Pods         section[podView]         `json:"pods"`
	Volumes      section[volumeView]      `json:"volumes"`
	Addons       section[addonView]       `json:"addons"`
	Pools        section[poolView]        `json:"pools"`
	ControlPlane controlPlaneView         `json:"controlPlane"`
	HealthChecks section[healthCheckView] `json:"healthChecks"`
	Events       section[eventView]       `json:"events"`
	// VMs says why the nodes show no figures from Proxmox VE, when they show none.
	VMs string `json:"vms,omitempty"`
	// Logs tells whether the user may read the pods' logs on the page.
	Logs bool `json:"logs"`
}

type attentionView struct {
	Severity string `json:"severity"`
	// Section is the part of the page with the details.
	Section string    `json:"section"`
	What    string    `json:"what"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since,omitzero"`
	// Pod names the pod an item is about, for its links.
	Pod *podRef `json:"pod,omitempty"`
	// Machine names the Cluster API machine an item is about, which its own view acts on.
	Machine string `json:"machine,omitempty"`
}

type podRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type nodeView struct {
	// Name is the node's, or its machine's until it has joined.
	Name    string `json:"name"`
	Machine string `json:"machine,omitempty"`
	Role    string `json:"role"`
	Pool    string `json:"pool,omitempty"`
	// Joined tells whether the node was read in the cluster. The fields up to Pods need it.
	Joined bool `json:"joined"`
	Ready  bool `json:"ready"`
	// Down tells a node has been not ready for longer than starting or rebooting takes, while its
	// machine is not being deleted: a fault, not work under way.
	Down bool `json:"down,omitempty"`
	// Silent tells its kubelet stopped reporting, rather than reporting the node unwell.
	Silent        bool         `json:"silent,omitempty"`
	Since         time.Time    `json:"since,omitzero"`
	Message       string       `json:"message,omitempty"`
	Pressure      []string     `json:"pressure,omitempty"`
	Unschedulable bool         `json:"unschedulable,omitempty"`
	Taints        []string     `json:"taints,omitempty"`
	Kubelet       string       `json:"kubelet,omitempty"`
	CPU           *resourceUse `json:"cpu,omitempty"`
	Memory        *resourceUse `json:"memory,omitempty"`
	Pods          int          `json:"pods"`
	// VM is the node's VM as the user sees it in Proxmox VE.
	VM *vmView `json:"vm,omitempty"`

	vmName string
	// leaving tells its machine is being deleted, so its going down is expected.
	leaving bool
}

// resourceUse is what the pods on a node ask for of what it offers them: millicores of CPU, bytes
// of memory.
type resourceUse struct {
	Requested   int64 `json:"requested"`
	Allocatable int64 `json:"allocatable"`
}

type vmView struct {
	ID     int     `json:"id"`
	Node   string  `json:"node"`
	Status string  `json:"status"`
	CPU    float64 `json:"cpu"`
	Cores  float64 `json:"cores"`
	Memory int64   `json:"memory"`
	MaxMem int64   `json:"maxMemory"`
	Uptime int64   `json:"uptime"`
	// Load is its CPU load of the last hour, a point a minute. Null where it did not run.
	Load []*float64 `json:"load,omitempty"`
}

type podView struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Node      string    `json:"node,omitempty"`
	Severity  string    `json:"severity"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message,omitempty"`
	Restarts  int32     `json:"restarts"`
	Since     time.Time `json:"since,omitzero"`
	// Containers are the pod's containers that have run and so have logs, the unwell one first.
	Containers []string `json:"containers"`
}

type volumeView struct {
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	StorageClass string    `json:"storageClass,omitempty"`
	Since        time.Time `json:"since"`
}

type addonView struct {
	Name      string    `json:"name"`
	Release   string    `json:"release"`
	Namespace string    `json:"namespace,omitempty"`
	Chart     string    `json:"chart"`
	Version   string    `json:"version,omitempty"`
	Status    string    `json:"status"`
	Revision  int64     `json:"revision,omitempty"`
	Ready     bool      `json:"ready"`
	Message   string    `json:"message,omitempty"`
	Since     time.Time `json:"since,omitzero"`
}

type poolView struct {
	Name       string `json:"name"`
	Desired    int32  `json:"desired"`
	Ready      int32  `json:"ready"`
	Available  int32  `json:"available"`
	UpToDate   int32  `json:"upToDate"`
	RollingOut bool   `json:"rollingOut"`
	Message    string `json:"message,omitempty"`
	// Autoscaling is set for a pool the cluster autoscaler sizes.
	Autoscaling *poolAutoscaling `json:"autoscaling,omitempty"`
}

type controlPlaneView struct {
	Kind       string          `json:"kind,omitempty"`
	Conditions []conditionView `json:"conditions"`
	Error      string          `json:"error,omitempty"`
}

type healthCheckView struct {
	Name     string `json:"name"`
	Expected int32  `json:"expected"`
	Healthy  int32  `json:"healthy"`
	// Allowed tells whether the check may replace machines now. It stops when too many fail.
	Allowed     bool     `json:"allowed"`
	Message     string   `json:"message,omitempty"`
	Remediating []string `json:"remediating,omitempty"`
}

type eventView struct {
	// Source is the cluster the event happened in: workload, or the management cluster.
	Source  string    `json:"source"`
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Object  string    `json:"object"`
	Count   int32     `json:"count"`
	Last    time.Time `json:"last"`
	// Action is what the reporter was doing, as Kubehenge's controller names its runs.
	Action string `json:"action,omitempty"`
}

// The events' sources.
const (
	sourceWorkload   = "workload"
	sourceManagement = "management"
)

// attention collects what needs a look.
type attention []attentionView

func (a *attention) add(severity, where, what, message string, since time.Time) {
	*a = append(*a, attentionView{Severity: severity, Section: where, What: what, Message: message, Since: since})
}

// addMachine adds an item about one machine.
func (a *attention) addMachine(machine, severity, what, message string, since time.Time) {
	*a = append(*a, attentionView{Severity: severity, Section: sectionNodes, What: what, Message: message, Since: since, Machine: machine})
}

// sorted puts the worst first, then what has stood longest.
func (a *attention) sorted() []attentionView {
	out := slices.Clone(*a)
	if out == nil {
		out = []attentionView{}
	}
	slices.SortStableFunc(out, func(x, y attentionView) int {
		return cmp.Or(cmp.Compare(severityRank(x.Severity), severityRank(y.Severity)), x.Since.Compare(y.Since), strings.Compare(x.What, y.What))
	})
	return out
}

// verdict sums the page up in a word.
func verdict(c *clusterv1.Cluster, unreachable bool, items []attentionView) string {
	worst := len(severities)
	for i := range items {
		worst = min(worst, severityRank(items[i].Severity))
	}
	switch {
	case !c.DeletionTimestamp.IsZero():
		return verdictChanging
	case unreachable:
		return verdictUnreachable
	case worst <= severityRank(severityWarning):
		return verdictDegraded
	case worst == severityRank(severityProgress):
		return verdictChanging
	default:
		return verdictHealthy
	}
}

// problem is what a failed read says, without the URL it was made to and Go's wrapping.
func problem(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "no answer within " + healthDeadline.String()
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return status.Status().Message
	}
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err.Error()
	}
	return err.Error()
}

func (s *Server) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// clusterHealth answers the health page. Seeing it needs what seeing the cluster does: VM.Audit on
// its pool. The VMs' figures are read with the user's own ticket, so Proxmox VE decides who sees
// them. What Kubehenge reads with its own identities is shared by every viewer.
func (s *Server) clusterHealth(w http.ResponseWriter, r *http.Request, u user) {
	c, access, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	key := c.Namespace + "/" + c.Name + "/" + string(c.UID)
	shared, err := s.health.get(r.Context(), key, healthFresh, s.now, func(ctx context.Context) healthView {
		return s.readHealth(ctx, u.kube, c, u.namespaces)
	})
	if err != nil {
		return
	}
	v := shared
	v.Nodes.Items = slices.Clone(shared.Nodes.Items)
	v.VMs = s.addVMs(r.Context(), u, c, access.path, v.Nodes.Items)
	v.Logs = u.namespaces.has(c.Namespace) && access.held.Has(privKubeconfig) && v.Workload == ""
	writeJSON(w, http.StatusOK, v)
}

// readHealth reads every source of the page at once, each within its own deadline.
func (s *Server) readHealth(ctx context.Context, kube client.Client, c *clusterv1.Cluster, managed managedNamespaces) healthView {
	var m managementRead
	var wl workloadRead
	var wg sync.WaitGroup
	wg.Go(func() { m = readManagement(ctx, kube, c) })
	wg.Go(func() { wl = s.readWorkload(ctx, kube, c, managed) })
	wg.Wait()
	for what, err := range m.failures() {
		s.Log.ErrorContext(ctx, "reading a cluster's health", "cluster", c.Namespace+"/"+c.Name, "what", what, "error", err)
	}
	return assemble(c, &m, &wl, s.now())
}

// assemble makes the page of what was read.
func assemble(c *clusterv1.Cluster, m *managementRead, wl *workloadRead, now time.Time) healthView {
	var a attention
	v := healthView{
		Read:         now,
		Workload:     wl.why(),
		Nodes:        nodesSection(m, wl, now, &a),
		Pods:         podsSection(wl, now, &a),
		Volumes:      volumesSection(wl, now, &a),
		Addons:       addonsSection(c, m, &a),
		Pools:        poolsSection(m, &a),
		ControlPlane: controlPlaneSection(c, m, &a),
		HealthChecks: healthChecksSection(c, m, &a),
		Events:       eventsSection(c, m, wl, now),
	}
	readinessAttention(wl, &a)
	if wl.velero {
		a = append(a, backupsAttention(wl.veleroSchedule, wl.veleroBackups, now)...)
	}
	watchAttention(c, &a)
	v.Attention = a.sorted()
	v.Verdict = verdict(c, wl.unreachable(), v.Attention)
	return v
}

// vmLoadFresh is how long a VM's load is kept: Proxmox VE adds a point a minute.
const vmLoadFresh = time.Minute

// vmLoadAsks is how many VMs' loads are asked for at once.
const vmLoadAsks = 8

type vmLoadCache struct {
	mu   sync.Mutex
	byVM map[string]vmLoad
}

type vmLoad struct {
	points []*float64
	until  time.Time
}

// addVMs gives the nodes their VMs' figures, as far as Proxmox VE shows them to the user, and says
// why none show when they do not.
func (s *Server) addVMs(ctx context.Context, u user, c *clusterv1.Cluster, poolPath string, nodes []nodeView) string {
	members, err := s.Accounts.PoolMembers(ctx, u.ticket, strings.TrimPrefix(poolPath, "/pool/"))
	if errors.Is(err, pve.ErrForbidden) {
		return "Proxmox VE does not show you this cluster's VMs."
	}
	if err != nil {
		s.Log.ErrorContext(ctx, "listing a cluster's VMs", "cluster", c.Namespace+"/"+c.Name, "error", err)
		return "Proxmox VE did not answer."
	}
	byName := make(map[string]*pve.Member, len(members))
	for i := range members {
		byName[members[i].Name] = &members[i]
	}
	sem := make(chan struct{}, vmLoadAsks)
	var wg sync.WaitGroup
	for i := range nodes {
		m, ok := byName[nodes[i].vmName]
		if !ok || nodes[i].vmName == "" {
			continue
		}
		vm := &vmView{ID: m.VMID, Node: m.Node, Status: m.Status, CPU: m.CPU, Cores: m.Cores, Memory: m.Memory, MaxMem: m.MaxMem, Uptime: m.Uptime}
		nodes[i].VM = vm
		if m.Status != "running" {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			vm.Load = s.vmLoad(ctx, u, m)
		})
	}
	wg.Wait()
	return ""
}

// vmLoad is a VM's CPU load of the last hour as the user may read it, kept a minute per user.
func (s *Server) vmLoad(ctx context.Context, u user, m *pve.Member) []*float64 {
	key := u.ticket.Username + "|" + m.Node + "|" + strconv.Itoa(m.VMID)
	now := s.now()
	lc := &s.vmLoads
	lc.mu.Lock()
	kept, ok := lc.byVM[key]
	lc.mu.Unlock()
	if ok && now.Before(kept.until) {
		return kept.points
	}
	rctx, cancel := context.WithTimeout(ctx, healthDeadline)
	defer cancel()
	points, err := s.Accounts.VMLoad(rctx, u.ticket, m.Node, m.VMID)
	if err != nil {
		if !errors.Is(err, pve.ErrForbidden) {
			s.Log.WarnContext(ctx, "reading a VM's load", "vm", m.VMID, "error", err)
		}
		return nil
	}
	load := make([]*float64, len(points))
	for i, p := range points {
		load[i] = p.CPU
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.byVM == nil {
		lc.byVM = map[string]vmLoad{}
	}
	for k, old := range lc.byVM {
		if now.After(old.until) {
			delete(lc.byVM, k)
		}
	}
	lc.byVM[key] = vmLoad{points: load, until: now.Add(vmLoadFresh)}
	return load
}
