package ui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// The privileges on a cluster's pool, as Proxmox VE uses them for VMs: seeing it, changing it
// (create, scale, upgrade, add-ons, delete), and taking its admin kubeconfig.
const (
	privSee        = "VM.Audit"
	privChange     = "VM.Allocate"
	privKubeconfig = "Sys.Modify"
)

// createdBy records which Proxmox VE user created a cluster through the interface.
const createdBy = "kubehenge.dev/created-by"

// workerClass and workerPool name the one worker pool clusters created here get.
const (
	workerClass = "worker"
	workerPool  = "md-0"
)

// Variables a cluster may change after creation; the others describe where its VMs live.
var changeableVariables = map[string]bool{
	"headlamp": true, "monitoring": true, "logs": true, "backups": true, "rancher": true, "podSecurity": true, "istio": true, meshVariable: true, "loadBalancer": true, "upgrades": true, oidcVariable: true, allowedNodesVariable: true, "proxmoxHA": true, csiVariable: true, ccmVariable: true,
	certificatesVariable: true, gitopsVariable: true, autoscalerVariable: true,
	// A new node OS rolls every node onto its template, as a node image refresh does.
	provisioner.NodeOSVariable: true,
}

// autoUpgraded is where Kubehenge's controller records the last automatic upgrade.
const autoUpgraded = "kubehenge.dev/auto-upgraded"

func (s *Server) poolPath(namespace, name string) (string, error) {
	n, err := provisioner.NamesFor(s.WorkloadPool, namespace, name)
	if err != nil {
		return "", fmt.Errorf("cluster %s/%s: %w", namespace, name, err)
	}
	return "/pool/" + n.Pool, nil
}

func (s *Server) workloadPoolPath() string { return "/pool/" + s.WorkloadPool }

type classView struct {
	Name         string             `json:"name"`
	Distribution string             `json:"distribution"`
	Variables    []classVariable    `json:"variables"`
	Annotations  map[string]string  `json:"annotations,omitempty"`
	Versions     *regexpDescription `json:"versionFormat"`
	// NoTemplate says why no cluster of the class on the default node OS can be built yet.
	NoTemplate string `json:"noTemplate,omitempty"`
	// NodeOSes are the operating systems the class's nodes may run, but for Talos's.
	NodeOSes []nodeOSChoice `json:"nodeOSes,omitempty"`
}

// nodeOSChoice is a node OS a cluster may run, and why none can be built on it yet.
type nodeOSChoice struct {
	templates.NodeOSInfo

	NoTemplate string `json:"noTemplate,omitempty"`
}

// nodeOSChoices are the node OSes of every family, those not released yet among them, so that the
// wizard can say why they cannot be chosen.
func nodeOSChoices(c *clusterv1.ClusterClass, dist string) []nodeOSChoice {
	if dist == string(templates.Talos) {
		return nil
	}
	var out []nodeOSChoice
	for _, info := range templates.NodeOSes() {
		choice := nodeOSChoice{NodeOSInfo: info}
		if info.Unreleased == "" {
			choice.NoTemplate = missingTemplate(c, dist, "", info.NodeOS)
		}
		out = append(out, choice)
	}
	return out
}

type classVariable struct {
	Name     string                          `json:"name"`
	Required bool                            `json:"required"`
	Schema   apiextensionsv1.JSONSchemaProps `json:"schema"`
}

type regexpDescription struct {
	Pattern string `json:"pattern"`
}

func (s *Server) classes(w http.ResponseWriter, r *http.Request, u user) {
	var list clusterv1.ClusterClassList
	if err := u.kube.List(r.Context(), &list, client.InNamespace(s.ClassNamespace)); err != nil {
		s.kubeError(w, err)
		return
	}
	views := []classView{}
	for i := range list.Items {
		c := &list.Items[i]
		if dist, ok := distribution(c.Name); ok {
			views = append(views, s.classView(c, dist))
		}
	}
	slices.SortFunc(views, func(a, b classView) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) classView(c *clusterv1.ClusterClass, dist string) classView {
	v := classView{
		Name: c.Name, Distribution: dist, Variables: []classVariable{}, Versions: &regexpDescription{Pattern: provisioner.VersionFormats[templates.Distribution(dist)].String()},
		NoTemplate: missingTemplate(c, dist, "", templates.DefaultNodeOS), NodeOSes: nodeOSChoices(c, dist),
	}
	for i := range c.Spec.Variables {
		cv := &c.Spec.Variables[i]
		var schema apiextensionsv1.JSONSchemaProps
		raw, err := json.Marshal(cv.Schema.OpenAPIV3Schema)
		if err == nil {
			err = json.Unmarshal(raw, &schema)
		}
		if err != nil {
			s.Log.Error("class variable schema", "class", c.Name, "variable", cv.Name, "error", err)
			continue
		}
		v.Variables = append(v.Variables, classVariable{Name: cv.Name, Required: cv.Required != nil && *cv.Required, Schema: schema})
	}
	for k, val := range c.Annotations {
		if strings.HasPrefix(k, "kubehenge.dev/") {
			if v.Annotations == nil {
				v.Annotations = map[string]string{}
			}
			v.Annotations[k] = val
		}
	}
	return v
}

// distribution is the distribution one of Kubehenge's classes builds, by its name.
func distribution(class string) (string, bool) {
	d, ok := strings.CutPrefix(class, provisioner.ClassPrefix)
	if !ok || provisioner.VersionFormats[templates.Distribution(d)] == nil {
		return "", false
	}
	return d, true
}

type clusterView struct {
	Namespace    string                     `json:"namespace"`
	Name         string                     `json:"name"`
	Class        string                     `json:"class"`
	Distribution string                     `json:"distribution"`
	Version      string                     `json:"version"`
	Created      time.Time                  `json:"created"`
	CreatedBy    string                     `json:"createdBy,omitempty"`
	AutoUpgraded string                     `json:"autoUpgraded,omitempty"`
	Deleting     bool                       `json:"deleting"`
	Phase        string                     `json:"phase"`
	Available    bool                       `json:"available"`
	Conditions   []conditionView            `json:"conditions"`
	Endpoint     string                     `json:"endpoint,omitempty"`
	Pods         []string                   `json:"pods,omitempty"`
	Services     []string                   `json:"services,omitempty"`
	ControlPlane replicasView               `json:"controlPlane"`
	Workers      replicasView               `json:"workers"`
	WorkerPools  []workerPoolView           `json:"workerPools"`
	Variables    map[string]json.RawMessage `json:"variables"`
	Pool         string                     `json:"pool"`
	Allowed      allowedView                `json:"allowed"`
	Machines     []machineView              `json:"machines,omitempty"`
	// Headlamp names the cluster in the shared Headlamp, which offers the interface's namespace.
	Headlamp string `json:"headlamp,omitempty"`
	// Mesh is how each peer stands that the cluster lists for Cluster Mesh, while it is on.
	Mesh []provisioner.MeshPeer `json:"mesh,omitempty"`
	// NodeImage is which template builds the nodes run and when they next move, but for Talos.
	NodeImage *nodeImageView `json:"nodeImage,omitempty"`
	// NodeOS is what the nodes run, but for Talos.
	NodeOS *templates.NodeOSInfo `json:"nodeOS,omitempty"`
	// GitOps is the deploy key and the repository host's keys the controller made for the cluster.
	GitOps *gitOpsView `json:"gitops,omitempty"`
	// Talos is how a Talos cluster's nodes stand with its Talos release.
	Talos *talosView `json:"talos,omitempty"`
	// Generation counts the changes to the cluster's spec; a change made against it carries it back
	// (generationHeader).
	Generation int64 `json:"generation"`
}

// talosView is how a Talos cluster's nodes are upgraded in place to its Talos release.
type talosView struct {
	// Version is the release the nodes are upgraded to.
	Version string `json:"version"`
	// Image is the release new machines boot first, before their upgrade.
	Image string `json:"image,omitempty"`
	// Upgrading names the node being upgraded and to which release, Upgraded the last that was,
	// Failed the one that failed and why, which holds the rest until it is tried again.
	Upgrading string `json:"upgrading,omitempty"`
	Upgraded  string `json:"upgraded,omitempty"`
	Failed    string `json:"failed,omitempty"`
}

func talosViewOf(c *clusterv1.Cluster) *talosView {
	version, _ := provisioner.TalosVersion(c)
	image, _ := provisioner.TalosImage(c)
	return &talosView{
		Version: version, Image: image,
		Upgrading: c.Annotations[provisioner.TalosUpgradingAnnotation],
		Upgraded:  c.Annotations[provisioner.TalosUpgradedAnnotation],
		Failed:    c.Annotations[provisioner.TalosUpgradeFailedAnnotation],
	}
}

type conditionView struct {
	Type    string    `json:"type"`
	Status  string    `json:"status"`
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since"`
}

type replicasView struct {
	Desired   int32 `json:"desired"`
	Ready     int32 `json:"ready"`
	Available int32 `json:"available"`
	UpToDate  int32 `json:"upToDate"`
}

type workerPoolView struct {
	Name     string `json:"name"`
	Class    string `json:"class"`
	Replicas int32  `json:"replicas"`
	// Autoscaling is set for a pool the cluster autoscaler sizes, whose replicas are then the
	// workers its MachineDeployment asks for at the moment.
	Autoscaling *poolAutoscaling `json:"autoscaling,omitempty"`
}

type allowedView struct {
	Change     bool `json:"change"`
	Kubeconfig bool `json:"kubeconfig"`
}

// A machine's role: a control plane's, or a worker's for every other one.
const (
	controlPlaneRole = "control-plane"
	workerRole       = "worker"
)

type machineView struct {
	Name string `json:"name"`
	Node string `json:"node,omitempty"`
	// ProxmoxNode is the Proxmox VE node its VM runs on, where the user may see the VM.
	ProxmoxNode string `json:"proxmoxNode,omitempty"`
	Role        string `json:"role"`
	Version     string `json:"version,omitempty"`
	// Talos is the Talos release a Talos cluster's node runs, as its kubelet reports it.
	Talos string `json:"talos,omitempty"`
	Phase string `json:"phase"`
	Ready bool   `json:"ready"`
	// Waiting is what keeps a machine that is not ready from it, as Cluster API sums it up.
	Waiting   string    `json:"waiting,omitempty"`
	Addresses []string  `json:"addresses,omitempty"`
	Created   time.Time `json:"created"`
}

// view describes a cluster to a user with privs on its pool. The interface changes clusters only in
// the namespaces it manages, so elsewhere nothing is allowed whatever the privileges.
func (s *Server) view(c *clusterv1.Cluster, pool string, privs pve.Privileges, managed managedNamespaces) clusterView {
	ours := managed.has(c.Namespace)
	dist, _ := distribution(c.Spec.Topology.ClassRef.Name)
	v := clusterView{
		Namespace:    c.Namespace,
		Name:         c.Name,
		Class:        c.Spec.Topology.ClassRef.Name,
		Distribution: dist,
		Version:      c.Spec.Topology.Version,
		Generation:   c.Generation,
		Created:      c.CreationTimestamp.Time,
		CreatedBy:    c.Annotations[createdBy],
		AutoUpgraded: c.Annotations[autoUpgraded],
		Deleting:     !c.DeletionTimestamp.IsZero(),
		Phase:        c.Status.Phase,
		Available:    meta.IsStatusConditionTrue(c.Status.Conditions, clusterv1.AvailableCondition),
		Conditions:   []conditionView{},
		WorkerPools:  []workerPoolView{},
		Variables:    map[string]json.RawMessage{},
		Pool:         pool,
		Pods:         c.Spec.ClusterNetwork.Pods.CIDRBlocks,
		Services:     c.Spec.ClusterNetwork.Services.CIDRBlocks,
		Allowed:      allowedView{Change: ours && privs.Has(privChange), Kubeconfig: ours && privs.Has(privKubeconfig)},
	}
	if s.Headlamp != nil && ours {
		v.Headlamp = headlampContext(c.Namespace, c.Name)
	}
	if dist == string(templates.Talos) {
		v.Talos = talosViewOf(c)
	}
	if e := c.Spec.ControlPlaneEndpoint; e.Host != "" {
		v.Endpoint = e.Host
		if e.Port != 0 {
			v.Endpoint += ":" + strconv.Itoa(int(e.Port))
		}
	}
	for _, cond := range c.Status.Conditions {
		v.Conditions = append(v.Conditions, conditionView{
			Type: cond.Type, Status: string(cond.Status), Reason: cond.Reason, Message: cond.Message, Since: cond.LastTransitionTime.Time,
		})
	}
	if cp := c.Status.ControlPlane; cp != nil {
		v.ControlPlane = replicas(cp.DesiredReplicas, cp.ReadyReplicas, cp.AvailableReplicas, cp.UpToDateReplicas)
	} else if r := c.Spec.Topology.ControlPlane.Replicas; r != nil {
		v.ControlPlane.Desired = *r
	}
	if ws := c.Status.Workers; ws != nil {
		v.Workers = replicas(ws.DesiredReplicas, ws.ReadyReplicas, ws.AvailableReplicas, ws.UpToDateReplicas)
	}
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		md := &c.Spec.Topology.Workers.MachineDeployments[i]
		wp := workerPoolView{Name: md.Name, Class: md.Class, Autoscaling: autoscalingOf(md.Metadata.Annotations)}
		if md.Replicas != nil {
			wp.Replicas = *md.Replicas
		}
		v.WorkerPools = append(v.WorkerPools, wp)
	}
	for _, cv := range c.Spec.Topology.Variables {
		v.Variables[cv.Name] = json.RawMessage(cv.Value.Raw)
	}
	if provisioner.RefreshesNodes(c) {
		v.NodeOS = nodeOSInfo(c)
	}
	return v
}

// nodeOSInfo describes the node OS a cluster's nodes run, or nil when its variable cannot be read.
func nodeOSInfo(c *clusterv1.Cluster) *templates.NodeOSInfo {
	o, err := provisioner.ClusterNodeOS(c)
	if err != nil {
		return nil
	}
	for _, info := range templates.NodeOSes() {
		if info.NodeOS == o {
			return &info
		}
	}
	return &templates.NodeOSInfo{NodeOS: o, Title: o.String()}
}

func replicas(desired, ready, available, upToDate *int32) replicasView {
	get := func(p *int32) int32 {
		if p == nil {
			return 0
		}
		return *p
	}
	return replicasView{Desired: get(desired), Ready: get(ready), Available: get(available), UpToDate: get(upToDate)}
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request, u user) {
	var list clusterv1.ClusterList
	if err := u.kube.List(r.Context(), &list); err != nil {
		s.kubeError(w, err)
		return
	}
	// One question for every cluster at once; a cluster's pool is asked about on its own only where
	// the answer cannot tell (inheritedHere).
	all, err := s.Accounts.AllPrivileges(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	views, err := s.visibleClusters(r.Context(), u, list.Items, all)
	if err != nil {
		s.proxmoxError(w, err)
		return
	}
	// The peers' standing is an addition to the clusters, which show without it.
	if err := s.addMesh(r.Context(), u, list.Items, views, visibleKeys(views)); err != nil {
		s.Log.Error("reading the clusters' Cluster Mesh peers", "error", err)
	}
	slices.SortFunc(views, func(a, b clusterView) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	writeJSON(w, http.StatusOK, views)
}

// privilegeAsks is how many pools' privileges are asked about at once.
const privilegeAsks = 8

type clusterResult struct {
	view clusterView
	ok   bool
	err  error
}

// visibleClusters describes the clusters the user may see, by the privileges in all or, where
// those cannot tell, asking about each pool.
func (s *Server) visibleClusters(ctx context.Context, u user, clusters []clusterv1.Cluster, all map[string]pve.Privileges) ([]clusterView, error) {
	results := make([]clusterResult, len(clusters))
	sem := make(chan struct{}, privilegeAsks)
	var wg sync.WaitGroup
	for i := range clusters {
		c := &clusters[i]
		if !provisioner.Owns(c, s.ClassNamespace) {
			continue
		}
		pool, err := s.poolPath(c.Namespace, c.Name)
		if err != nil {
			continue
		}
		if privs, listed := all[pool]; listed || !inheritedHere(all, pool) {
			if privs.Has(privSee) {
				results[i] = clusterResult{view: s.view(c, pool, privs, u.namespaces), ok: true}
			}
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.askedView(ctx, u, c, pool)
		})
	}
	wg.Wait()
	views := []clusterView{}
	for i := range results {
		if results[i].err != nil {
			return nil, results[i].err
		}
		if results[i].ok {
			views = append(views, results[i].view)
		}
	}
	return views, nil
}

func (s *Server) askedView(ctx context.Context, u user, c *clusterv1.Cluster, pool string) clusterResult {
	privs, err := s.Accounts.Privileges(ctx, u.ticket, pool)
	if err != nil {
		return clusterResult{err: err}
	}
	if !privs.Has(privSee) {
		return clusterResult{}
	}
	return clusterResult{view: s.view(c, pool, privs, u.namespaces), ok: true}
}

// inheritedHere tells whether path, missing from all, may still hold privileges inherited from
// above. Proxmox VE lists every path with an ACL entry where the user holds anything, so a missing
// path holds nothing, unless it has no ACL entry of its own (a pool the controller has not yet
// given its cluster's user) and the user holds something on a path above it.
func inheritedHere(all map[string]pve.Privileges, path string) bool {
	for p := path; p != "/"; {
		p = parentPath(p)
		if len(all[p]) > 0 {
			return true
		}
	}
	return false
}

func parentPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}

// poolAccess is a cluster's pool and the user's privileges on it.
type poolAccess struct {
	path string
	held pve.Privileges
}

// cluster finds the request's cluster and checks the user's privileges on its pool. A cluster the
// user may not see is answered as not found, so its existence is not given away.
func (s *Server) cluster(w http.ResponseWriter, r *http.Request, u user, privs ...string) (*clusterv1.Cluster, poolAccess, bool) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	pool, err := s.poolPath(ns, name)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such cluster")
		return nil, poolAccess{}, false
	}
	held, err := s.Accounts.Privileges(r.Context(), u.ticket, pool)
	if err != nil {
		s.proxmoxError(w, err)
		return nil, poolAccess{}, false
	}
	c := &clusterv1.Cluster{}
	if !held.Has(privSee) {
		writeError(w, http.StatusNotFound, "no such cluster")
		return nil, poolAccess{}, false
	}
	if err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, c); err != nil || !provisioner.Owns(c, s.ClassNamespace) {
		if err != nil && !apierrors.IsNotFound(err) {
			s.kubeError(w, err)
			return nil, poolAccess{}, false
		}
		writeError(w, http.StatusNotFound, "no such cluster")
		return nil, poolAccess{}, false
	}
	for _, p := range privs {
		if !held[p] {
			writeError(w, http.StatusForbidden, fmt.Sprintf("%s needs %s on %s", u.ticket.Username, p, pool))
			return nil, poolAccess{}, false
		}
	}
	if len(privs) > 0 && !u.namespaces.has(c.Namespace) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("the interface manages the clusters in %s; this one is managed from elsewhere", namespacesPhrase(u.namespaces)))
		return nil, poolAccess{}, false
	}
	return c, poolAccess{path: pool, held: held}, true
}

// addExtras adds what the cluster's page shows beside the cluster itself: its Cluster Mesh peers,
// its node images and its GitOps keys. Each is an addition, so the cluster shows without the ones
// that cannot be read.
func (s *Server) addExtras(r *http.Request, u user, c *clusterv1.Cluster, v *clusterView) {
	if err := s.addClusterMesh(r.Context(), u, v); err != nil {
		s.Log.Error("reading a cluster's Cluster Mesh peers", "cluster", c.Namespace+"/"+c.Name, "error", err)
	}
	// Talos clusters have no node images, their template being their release's image.
	if provisioner.RefreshesNodes(c) {
		image, err := s.nodeImage(r.Context(), u.kube, c, s.now())
		if err != nil {
			s.Log.Warn("reading a cluster's node images", "cluster", c.Namespace+"/"+c.Name, "error", err)
		}
		v.NodeImage = image
	}
	if err := addGitOps(r.Context(), u, v); err != nil {
		s.Log.Error("reading a cluster's GitOps keys", "cluster", c.Namespace+"/"+c.Name, "error", err)
	}
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request, u user) {
	c, access, ok := s.cluster(w, r, u)
	if !ok {
		return
	}
	v := s.view(c, access.path, access.held, u.namespaces)
	s.addExtras(r, u, c, &v)
	if err := countAutoscaled(r.Context(), u.kube, c, &v); err != nil {
		s.kubeError(w, err)
		return
	}
	var machines clusterv1.MachineList
	if err := u.kube.List(r.Context(), &machines, client.InNamespace(c.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}); err != nil {
		s.kubeError(w, err)
		return
	}
	hosts := s.vmNodes(r, u, c, access.path)
	v.Machines = []machineView{}
	for i := range machines.Items {
		m := &machines.Items[i]
		mv := machineView{
			Name:        m.Name,
			Node:        m.Status.NodeRef.Name,
			ProxmoxNode: hosts[m.Spec.InfrastructureRef.Name],
			Role:        workerRole,
			Version:     m.Spec.Version,
			Phase:       m.Status.Phase,
			Ready:       meta.IsStatusConditionTrue(m.Status.Conditions, clusterv1.MachineReadyCondition),
			Created:     m.CreationTimestamp.Time,
		}
		if v.Talos != nil {
			mv.Talos, _ = provisioner.MachineTalos(m)
		}
		if _, cp := m.Labels[clusterv1.MachineControlPlaneLabel]; cp {
			mv.Role = controlPlaneRole
		}
		if ready := meta.FindStatusCondition(m.Status.Conditions, clusterv1.MachineReadyCondition); ready != nil && !mv.Ready {
			mv.Waiting = ready.Message
		}
		for _, a := range m.Status.Addresses {
			if a.Type == clusterv1.MachineInternalIP || a.Type == clusterv1.MachineExternalIP {
				mv.Addresses = append(mv.Addresses, a.Address)
			}
		}
		v.Machines = append(v.Machines, mv)
	}
	slices.SortFunc(v.Machines, func(a, b machineView) int {
		switch a.Role {
		case b.Role:
			return strings.Compare(a.Name, b.Name)
		case controlPlaneRole:
			return -1
		default:
			return 1
		}
	})
	writeJSON(w, http.StatusOK, v)
}

// vmNodes maps the names of the cluster's VMs, which the Proxmox VE provider names after their
// machines' infrastructure, to the nodes they run on now. The machines show without their nodes
// when Proxmox VE does not tell: they are the page's subject, the nodes an addition.
func (s *Server) vmNodes(r *http.Request, u user, c *clusterv1.Cluster, poolPath string) map[string]string {
	members, err := s.Accounts.PoolMembers(r.Context(), u.ticket, strings.TrimPrefix(poolPath, "/pool/"))
	if err != nil {
		if !errors.Is(err, pve.ErrForbidden) {
			s.Log.Error("listing a cluster's VMs", "cluster", c.Namespace+"/"+c.Name, "error", err)
		}
		return nil
	}
	nodes := make(map[string]string, len(members))
	for i := range members {
		nodes[members[i].Name] = members[i].Node
	}
	return nodes
}

type createRequest struct {
	// Namespace is one of those the interface manages; unset, its first.
	Namespace            string `json:"namespace,omitempty"`
	Name                 string `json:"name"`
	Class                string `json:"class"`
	Version              string `json:"version"`
	ControlPlaneReplicas int32  `json:"controlPlaneReplicas"`
	WorkerReplicas       int32  `json:"workerReplicas"`
	// WorkerAutoscaling has the cluster autoscaler size the worker pool in place of WorkerReplicas.
	WorkerAutoscaling *poolAutoscaling           `json:"workerAutoscaling,omitempty"`
	Variables         map[string]json.RawMessage `json:"variables"`
	// Pods and Services are the cluster's own ranges; unset, ones clear of the nodes' network are
	// chosen.
	Pods     []string `json:"pods,omitempty"`
	Services []string `json:"services,omitempty"`
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request, u user) {
	var req createRequest
	if !readJSON(w, r, &req) {
		return
	}
	if msg := req.invalid(); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if req.Namespace == "" {
		req.Namespace = u.namespaces[0]
	} else if !u.namespaces.has(req.Namespace) {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("namespace: the interface creates clusters in %s only", namespacesPhrase(u.namespaces)))
		return
	}
	class, ok := s.createClass(w, r, u, &req)
	if !ok {
		return
	}
	pods, services, ok := s.newRanges(w, r, u, &req, class)
	if !ok {
		return
	}
	pool, err := s.poolPath(req.Namespace, req.Name)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "name: "+err.Error())
		return
	}
	if !s.mayCreate(w, r, u, class, &req) {
		return
	}
	c := s.newCluster(u, &req, pods, services)
	if err = u.kube.Create(r.Context(), c); err != nil {
		s.kubeError(w, err)
		return
	}
	s.Log.Info("created cluster", "user", u.ticket.Username, "cluster", req.Namespace+"/"+req.Name, "class", req.Class, "version", req.Version)
	privs, err := s.Accounts.Privileges(r.Context(), u.ticket, pool)
	if err != nil {
		privs = pve.Privileges{}
	}
	writeJSON(w, http.StatusCreated, s.view(c, pool, privs, u.namespaces))
}

// invalid says what is wrong with a create request on its own, before anything is looked up, or
// returns "".
func (req *createRequest) invalid() string {
	if !provisioner.ValidClusterName(req.Name) {
		return "name: " + provisioner.ClusterNameRule
	}
	dist, ok := distribution(req.Class)
	if !ok {
		return "distribution: not one of Kubehenge's cluster classes"
	}
	if msg := provisioner.VersionRule(templates.Distribution(dist), req.Version); msg != "" {
		return msg
	}
	if !provisioner.ValidControlPlane(req.ControlPlaneReplicas) {
		return provisioner.ControlPlaneRule
	}
	if !provisioner.ValidWorkers(req.WorkerReplicas) {
		return provisioner.WorkersRule
	}
	if a := req.WorkerAutoscaling; a != nil && !a.valid() {
		return autoscalingCounts
	}
	return ""
}

// createClass fetches the class a create request names and checks that it defines every variable
// the request sets, or answers the request.
func (s *Server) createClass(w http.ResponseWriter, r *http.Request, u user, req *createRequest) (*clusterv1.ClusterClass, bool) {
	class := &clusterv1.ClusterClass{}
	if err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: s.ClassNamespace, Name: req.Class}, class); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusUnprocessableEntity, "distribution: its cluster class is missing from the management cluster")
			return nil, false
		}
		s.kubeError(w, err)
		return nil, false
	}
	defined := map[string]bool{}
	for i := range class.Spec.Variables {
		defined[class.Spec.Variables[i].Name] = true
	}
	for name := range req.Variables {
		if !defined[name] {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("the cluster class %s has no variable %s", req.Class, name))
			return nil, false
		}
	}
	// A cluster made without its template would wait for ever for its first VM.
	dist, _ := distribution(req.Class)
	version := req.Version
	var nodeOS templates.NodeOS
	if dist == string(templates.Talos) {
		// A Talos template is a Talos release's, whatever Kubernetes version runs on it, and Talos
		// is its own OS: the class has no nodeOS variable to set.
		if err := json.Unmarshal(req.Variables[provisioner.TalosVersionVariable], &version); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "Talos version: choose a Talos release")
			return nil, false
		}
	} else {
		var err error
		if nodeOS, err = requestNodeOS(req.Variables[provisioner.NodeOSVariable]); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return nil, false
		}
	}
	if missing := missingTemplate(class, dist, version, nodeOS); missing != "" {
		writeError(w, http.StatusConflict, missing)
		return nil, false
	}
	return class, true
}

// requestNodeOS is the node OS a request's nodeOS variable names, the default where it names none,
// once Kubehenge builds templates of it.
func requestNodeOS(raw json.RawMessage) (templates.NodeOS, error) {
	o := templates.DefaultNodeOS
	if raw != nil {
		if err := json.Unmarshal(raw, &o); err != nil {
			return o, fmt.Errorf("node OS: %w", err)
		}
	}
	if _, _, err := templates.Available(o); err != nil {
		return o, fmt.Errorf("node OS: %w", err)
	}
	return o, nil
}

// mayCreate makes the checks Proxmox VE makes of a user creating a VM in the workload pool, on that
// storage and that bridge, since the interface creates the VMs with its own account, and those of
// the add-ons that need more; it answers the request when one fails.
func (s *Server) mayCreate(w http.ResponseWriter, r *http.Request, u user, class *clusterv1.ClusterClass, req *createRequest) bool {
	if !s.require(w, r, u, s.workloadPoolPath(), privChange) ||
		!s.mayUseStorage(w, r, u, class, req.Variables["storage"]) ||
		!s.mayUseNetwork(w, r, u, class, req.Variables["network"]) ||
		!s.mayPlace(w, r, u, req.Variables[allowedNodesVariable], nil) ||
		!s.mayPlaceAutoscaler(w, r, u, req.Variables[autoscalerVariable], nil) {
		return false
	}
	if installsRancher(req.Variables["rancher"]) && !s.allowRancher(w, r, u, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}) {
		return false
	}
	if allocatesDatastore(req.Variables[csiVariable]) && !s.allowDatastoreAllocate(w, r, u, req.Variables["storage"]) {
		return false
	}
	if takesSnapshots(req.Variables[csiVariable]) && !s.require(w, r, u, "/", privPermissionsModify) {
		return false
	}
	if !s.allowGitOps(w, r, u, s.workloadPoolPath(), req.Variables[gitopsVariable], nil) {
		return false
	}
	raw, ok := req.Variables[oidcVariable]
	certs := req.Variables[certificatesVariable]
	if !ok && !sharesDNS01(certs) {
		return true
	}
	held, err := s.Accounts.Privileges(r.Context(), u.ticket, s.workloadPoolPath())
	if err != nil {
		s.proxmoxError(w, err)
		return false
	}
	if !checkSharedDNS01(w, u, s.workloadPoolPath(), held, certs, nil) {
		return false
	}
	return !ok || checkOIDC(w, u, class, s.workloadPoolPath(), held, raw, nil)
}

func (s *Server) mayUseStorage(w http.ResponseWriter, r *http.Request, u user, class *clusterv1.ClusterClass, raw json.RawMessage) bool {
	storage, ok := stringVariable(raw)
	if !ok {
		return true
	}
	if allowed := classChoices(class, "storage"); allowed != nil && !slices.Contains(allowed, storage) {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("storage: clusters may use %s only", strings.Join(allowed, ", ")))
		return false
	}
	return s.require(w, r, u, storagePath(storage), privStorage)
}

func (s *Server) mayUseNetwork(w http.ResponseWriter, r *http.Request, u user, class *clusterv1.ClusterClass, raw json.RawMessage) bool {
	bridge, vlan, ok := networkVariable(raw)
	if !ok {
		return true
	}
	if allowed := classChoices(class, "network", "bridge"); allowed != nil && !slices.Contains(allowed, bridge) {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("bridge: clusters may attach to %s only", strings.Join(allowed, ", ")))
		return false
	}
	zone, err := s.Accounts.Zone(r.Context(), u.ticket, bridge)
	if err != nil {
		s.proxmoxError(w, err)
		return false
	}
	return s.require(w, r, u, bridgePath(zone, bridge, vlan), privBridge)
}

// allowedNodesVariable keeps a cluster's VMs to some of the Proxmox VE nodes.
const allowedNodesVariable = "allowedNodes"

// mayPlace checks the nodes a cluster's VMs are kept to: none twice, and each one the user sees or
// one of kept, those the cluster already has, which the user may keep without seeing.
func (s *Server) mayPlace(w http.ResponseWriter, r *http.Request, u user, raw, kept json.RawMessage) bool {
	if raw == nil {
		return true
	}
	var old []string
	// Without a list of nodes there are none to keep.
	_ = json.Unmarshal(kept, &old)
	var nodes []string
	if err := json.Unmarshal(raw, &nodes); err != nil || len(nodes) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "allowed nodes: at least one Proxmox VE node, or none set for every node")
		return false
	}
	visible, err := s.Accounts.Nodes(r.Context(), u.ticket)
	if err != nil {
		s.proxmoxError(w, err)
		return false
	}
	for i, node := range nodes {
		if slices.Contains(nodes[:i], node) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("allowed nodes: %s is listed twice", node))
			return false
		}
		if !slices.Contains(old, node) && !slices.ContainsFunc(visible, func(n pve.Node) bool { return n.Node == node }) {
			writeError(w, http.StatusUnprocessableEntity, "allowed nodes: Proxmox VE shows you no node "+node)
			return false
		}
	}
	return true
}

func (s *Server) newCluster(u user, req *createRequest, pods, services []string) *clusterv1.Cluster {
	c := &clusterv1.Cluster{
		Namespace:   req.Namespace,
		Name:        req.Name,
		Annotations: map[string]string{createdBy: u.ticket.Username},
		Spec: clusterv1.ClusterSpec{ClusterNetwork: clusterv1.ClusterNetwork{
			Pods:     clusterv1.NetworkRanges{CIDRBlocks: pods},
			Services: clusterv1.NetworkRanges{CIDRBlocks: services},
		}, Topology: clusterv1.Topology{
			ClassRef:     clusterv1.ClusterClassRef{Name: req.Class, Namespace: s.ClassNamespace},
			Version:      req.Version,
			ControlPlane: clusterv1.ControlPlaneTopology{Replicas: &req.ControlPlaneReplicas},
			Workers: clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{
				{Class: workerClass, Name: workerPool, Replicas: &req.WorkerReplicas},
			}},
		}},
	}
	if req.WorkerAutoscaling != nil {
		setAutoscaling(&c.Spec.Topology.Workers.MachineDeployments[0], req.WorkerAutoscaling, 0)
	}
	for _, name := range slices.Sorted(maps.Keys(req.Variables)) {
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
			Name: name, Value: apiextensionsv1.JSON{Raw: req.Variables[name]},
		})
	}
	return c
}

// allowRancher checks the right to install Rancher into the cluster key, and that no other cluster
// runs it. Rancher gets the admin kubeconfig of every cluster that registers with it, so installing
// it needs what taking all of their kubeconfigs would: Sys.Modify on the workload pool.
func (s *Server) allowRancher(w http.ResponseWriter, r *http.Request, u user, key client.ObjectKey) bool {
	if !s.require(w, r, u, s.workloadPoolPath(), privKubeconfig) {
		return false
	}
	var list clusterv1.ClusterList
	if err := u.kube.List(r.Context(), &list); err != nil {
		s.kubeError(w, err)
		return false
	}
	if rc := provisioner.RancherCluster(list.Items, s.ClassNamespace); rc != nil && client.ObjectKeyFromObject(rc) != key {
		writeError(w, http.StatusConflict, fmt.Sprintf("Rancher: cluster %s/%s already runs it, and every cluster registers with that one", rc.Namespace, rc.Name))
		return false
	}
	return true
}

func stringVariable(raw json.RawMessage) (string, bool) {
	var v string
	if raw == nil || json.Unmarshal(raw, &v) != nil || v == "" {
		return "", false
	}
	return v, true
}

// networkVariable reads the bridge and the VLAN ("" for none) of a network variable.
func networkVariable(raw json.RawMessage) (string, string, bool) {
	var v struct {
		Bridge string `json:"bridge"`
		VLAN   *int   `json:"vlan"`
	}
	if raw == nil || json.Unmarshal(raw, &v) != nil || v.Bridge == "" {
		return "", "", false
	}
	vlan := ""
	if v.VLAN != nil {
		vlan = strconv.Itoa(*v.VLAN)
	}
	return v.Bridge, vlan, true
}

// csiVariable configures a cluster's CSI plugin; its datastoreAllocate gives the cluster's token
// Datastore.Allocate on the cluster's storage, and its volumeSnapshots signs the plugin in as
// root@pam.
const csiVariable = "csi"

// ccmVariable configures a cluster's cloud controller manager; its haGroups gives the cluster's
// token Sys.Audit on / for the HA group labels. That reads the Proxmox VE cluster's configuration
// and nothing of its guests, so it takes no right of the user's own: Kubehenge's administrator
// decides whether clusters may hold it.
const ccmVariable = "ccm"

// csiOptions are the csi variable's switches; a value that is not one reads as all off.
type csiOptions struct {
	DatastoreAllocate bool `json:"datastoreAllocate"`
	VolumeSnapshots   bool `json:"volumeSnapshots"`
}

func csiOf(raw json.RawMessage) csiOptions {
	var v csiOptions
	if raw == nil || json.Unmarshal(raw, &v) != nil {
		return csiOptions{}
	}
	return v
}

func allocatesDatastore(raw json.RawMessage) bool { return csiOf(raw).DatastoreAllocate }

func takesSnapshots(raw json.RawMessage) bool { return csiOf(raw).VolumeSnapshots }

// allowDatastoreAllocate checks the right to give a cluster's token Datastore.Allocate on its
// storage: the user's own Datastore.Allocate there. Whether the storage is the cluster's alone is
// the controller's admission check, which refuses the cluster otherwise.
func (s *Server) allowDatastoreAllocate(w http.ResponseWriter, r *http.Request, u user, storage json.RawMessage) bool {
	name, ok := stringVariable(storage)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "CSI plugin: Datastore.Allocate needs the cluster's storage chosen")
		return false
	}
	return s.require(w, r, u, storagePath(name), privDatastoreAllocate)
}

func installsRancher(raw json.RawMessage) bool {
	var v struct {
		Install bool `json:"install"`
	}
	return raw != nil && json.Unmarshal(raw, &v) == nil && v.Install
}

type changeRequest struct {
	Version              *string `json:"version"`
	ControlPlaneReplicas *int32  `json:"controlPlaneReplicas"`
	// WorkerPools sets the replicas of worker pools by name.
	WorkerPools map[string]int32 `json:"workerPools"`
	// Autoscaling switches the cluster autoscaler on for worker pools by name, or off with null.
	Autoscaling map[string]*poolAutoscaling `json:"autoscaling"`
	Variables   map[string]json.RawMessage  `json:"variables"`
}

// changed is a change read and checked, applied to the cluster in memory, with the cluster as it was.
type changed struct {
	req    changeRequest
	before *clusterv1.Cluster
	c      *clusterv1.Cluster
	access poolAccess
}

// prepareChange reads a change and applies it to the cluster in memory, or answers the request with
// what is wrong with it.
func (s *Server) prepareChange(w http.ResponseWriter, r *http.Request, u user) (*changed, bool) {
	var req changeRequest
	if !readJSON(w, r, &req) {
		return nil, false
	}
	c, access, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return nil, false
	}
	if !c.DeletionTimestamp.IsZero() {
		writeError(w, http.StatusConflict, "the cluster is being deleted")
		return nil, false
	}
	if !unchangedSince(w, r, c) {
		return nil, false
	}
	before := c.DeepCopy()
	// Autoscaling goes first, so that a change switching a pool's off may also fix its workers.
	msg, err := applyAutoscaling(r.Context(), u.kube, c, req.Autoscaling)
	if err != nil {
		s.kubeError(w, err)
		return nil, false
	}
	if msg == "" {
		msg = applyTopology(c, &req)
	}
	if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return nil, false
	}
	if !s.applyVariables(w, r, u, c, access, req.Variables) {
		return nil, false
	}
	return &changed{req: req, before: before, c: c, access: access}, true
}

// generationHeader carries the generation of the cluster a change was made against, in the
// canonical form Go reads header names in.
const generationHeader = "X-Kubehenge-Generation"

// clusterChanged answers a change made against a cluster someone has changed since.
const clusterChanged = "the cluster changed meanwhile; reload and try again"

// unchangedSince tells whether the cluster's spec is still the one the change was made against, as
// the request's generation says, and answers it when not. A change sets whole values, of a
// variable say, from what its user was shown, so without this the second of two users editing
// at once would undo the first's change unseen. The generation stands for the spec alone, which
// Cluster API's status updates leave as it is; the update itself is then made against the version
// just read, so nothing slips in between. A request without the header is checked against nothing.
func unchangedSince(w http.ResponseWriter, r *http.Request, c *clusterv1.Cluster) bool {
	raw := r.Header.Get(generationHeader)
	if raw == "" {
		return true
	}
	generation, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, generationHeader+" is a number")
		return false
	}
	if generation != c.Generation {
		writeError(w, http.StatusPreconditionFailed, clusterChanged)
		return false
	}
	return true
}

func (s *Server) changeCluster(w http.ResponseWriter, r *http.Request, u user) {
	ch, ok := s.prepareChange(w, r, u)
	if !ok {
		return
	}
	c, access := ch.c, ch.access
	changes, err := plannedOf(r.Context(), u, ch)
	if err != nil {
		s.kubeError(w, err)
		return
	}
	if len(changes) > 0 {
		recordChange(c, u.ticket.Username, s.now(), changes)
	}
	if err := u.kube.Update(r.Context(), c); err != nil {
		s.kubeError(w, err)
		return
	}
	s.Log.Info("changed cluster", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name)
	writeJSON(w, http.StatusOK, s.view(c, access.path, access.held, u.namespaces))
}

// applyTopology sets the version and the replicas a change asks for, or says what is wrong with
// them.
func applyTopology(c *clusterv1.Cluster, req *changeRequest) string {
	if req.Version != nil {
		if msg := checkUpgrade(c, *req.Version); msg != "" {
			return msg
		}
		c.Spec.Topology.Version = *req.Version
	}
	if req.ControlPlaneReplicas != nil {
		if !provisioner.ValidControlPlane(*req.ControlPlaneReplicas) {
			return provisioner.ControlPlaneRule
		}
		c.Spec.Topology.ControlPlane.Replicas = req.ControlPlaneReplicas
	}
	for name, n := range req.WorkerPools {
		if !provisioner.ValidWorkers(n) {
			return provisioner.WorkersRule + " in each pool"
		}
		if isAutoscaled(c, name) {
			return "pool " + name + ": the cluster autoscaler sizes it; switch autoscaling off to fix its workers"
		}
		if !setWorkerReplicas(c, name, n) {
			return "the cluster has no worker pool " + name
		}
	}
	return ""
}

// checkUpgrade says what is wrong with moving the cluster to version, or returns "".
func checkUpgrade(c *clusterv1.Cluster, version string) string {
	dist, _ := distribution(c.Spec.Topology.ClassRef.Name)
	if msg := provisioner.VersionRule(templates.Distribution(dist), version); msg != "" {
		return msg
	}
	return provisioner.UpgradeRule(c.Spec.Topology.Version, version)
}

// setWorkerReplicas sets the replicas of the worker pool name, telling whether the cluster has it.
func setWorkerReplicas(c *clusterv1.Cluster, name string, n int32) bool {
	found := false
	for i := range c.Spec.Topology.Workers.MachineDeployments {
		if md := &c.Spec.Topology.Workers.MachineDeployments[i]; md.Name == name {
			md.Replicas, found = &n, true
		}
	}
	return found
}

// applyVariables sets the variables a change asks for, checking the rights each needs; it answers
// the request when one fails.
func (s *Server) applyVariables(w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster, access poolAccess, vars map[string]json.RawMessage) bool {
	for name, value := range vars {
		if !changeableVariables[name] {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("the variable %s is fixed when the cluster is created", name))
			return false
		}
		if !s.allowVariable(w, r, u, c, access, name, value) {
			return false
		}
		setVariable(c, name, value)
	}
	return true
}

// allowVariable checks the rights a change of the cluster's variable name to value needs: those of
// what it switches on or changes, never of what it keeps. It answers the request when they fail.
func (s *Server) allowVariable(w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster, access poolAccess, name string, value json.RawMessage) bool {
	old := variableValue(c, name)
	switch name {
	case "rancher":
		return !installsRancher(value) || installsRancher(old) || s.allowRancher(w, r, u, client.ObjectKeyFromObject(c))
	case csiVariable:
		return s.allowCSI(w, r, u, c, value, old)
	case oidcVariable:
		return sameJSON(old, value) || s.allowOIDC(w, r, u, c, access, value, old)
	case gitopsVariable:
		return s.allowGitOps(w, r, u, access.path, value, old)
	case allowedNodesVariable:
		return string(value) == jsonNull || sameJSON(old, value) || s.mayPlace(w, r, u, value, old)
	case certificatesVariable:
		return checkSharedDNS01(w, u, access.path, access.held, value, old)
	case autoscalerVariable:
		return s.mayPlaceAutoscaler(w, r, u, value, old)
	case provisioner.NodeOSVariable:
		return sameJSON(old, value) || s.allowNodeOS(w, r, u, c, value)
	}
	return true
}

// allowNodeOS checks that a cluster can move its nodes to the node OS value: a template of it at
// the cluster's version must be there, or the rollout would wait for ever for its first VM.
func (s *Server) allowNodeOS(w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster, value json.RawMessage) bool {
	dist, _ := distribution(c.Spec.Topology.ClassRef.Name)
	if dist == string(templates.Talos) {
		writeError(w, http.StatusUnprocessableEntity, "node OS: Talos nodes run Talos")
		return false
	}
	nodeOS, err := requestNodeOS(value)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return false
	}
	class := &clusterv1.ClusterClass{}
	if err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: s.ClassNamespace, Name: c.Spec.Topology.ClassRef.Name}, class); err != nil {
		s.kubeError(w, err)
		return false
	}
	if missing := missingTemplate(class, dist, c.Spec.Topology.Version, nodeOS); missing != "" {
		writeError(w, http.StatusConflict, missing)
		return false
	}
	return true
}

// allowCSI checks the rights a change of the cluster's csi variable from old to value needs: those
// of what it switches on, never of what it keeps or switches off.
func (s *Server) allowCSI(w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster, value, old json.RawMessage) bool {
	if allocatesDatastore(value) && !allocatesDatastore(old) && !s.allowDatastoreAllocate(w, r, u, variableValue(c, "storage")) {
		return false
	}
	return !takesSnapshots(value) || takesSnapshots(old) || s.require(w, r, u, "/", privPermissionsModify)
}

func variableValue(c *clusterv1.Cluster, name string) json.RawMessage {
	var value json.RawMessage
	for i := range c.Spec.Topology.Variables {
		if cv := &c.Spec.Topology.Variables[i]; cv.Name == name {
			value = cv.Value.Raw
		}
	}
	return value
}

// allowOIDC checks a change of the cluster's oidc variable from old to value against its class.
func (s *Server) allowOIDC(w http.ResponseWriter, r *http.Request, u user, c *clusterv1.Cluster, access poolAccess, value, old json.RawMessage) bool {
	class, ref := &clusterv1.ClusterClass{}, c.Spec.Topology.ClassRef
	if ref.Namespace == "" {
		ref.Namespace = c.Namespace
	}
	if err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, class); err != nil {
		s.kubeError(w, err)
		return false
	}
	return checkOIDC(w, u, class, access.path, access.held, value, old)
}

// setVariable replaces a topology variable, or removes it for a JSON null.
func setVariable(c *clusterv1.Cluster, name string, value json.RawMessage) {
	vars := c.Spec.Topology.Variables[:0:0]
	for _, cv := range c.Spec.Topology.Variables {
		if cv.Name != name {
			vars = append(vars, cv)
		}
	}
	if string(value) != jsonNull {
		vars = append(vars, clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: value}})
	}
	c.Spec.Topology.Variables = vars
}

// confirmRequest names the cluster a request that cannot be undone is for, as its user typed it.
type confirmRequest struct {
	Confirm string `json:"confirm"`
}

// confirmed tells whether a request that cannot be undone names the cluster it is for, and answers
// it when not. The interface has its user type the name; asking for it here as well holds a script,
// or a page that reached the session's CSRF token, to the same step.
func confirmed(w http.ResponseWriter, c *clusterv1.Cluster, typed string) bool {
	if typed != c.Name {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("confirm with the cluster's name, %q", c.Name))
		return false
	}
	return true
}

func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return
	}
	var req confirmRequest
	if !readJSON(w, r, &req) || !confirmed(w, c, req.Confirm) {
		return
	}
	if err := u.kube.Delete(r.Context(), c, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		s.kubeError(w, err)
		return
	}
	s.Log.Info("deleted cluster", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) kubeconfig(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	secret := &corev1.Secret{}
	if err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: c.Name + "-kubeconfig"}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusConflict, "the cluster has no kubeconfig yet")
			return
		}
		s.kubeError(w, err)
		return
	}
	s.Log.Info("handed out kubeconfig", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name)
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", c.Name+".kubeconfig"))
	_, _ = w.Write(secret.Data["value"])
}

// talosconfig hands out a Talos cluster's talosconfig, which signs in to its nodes' Talos API as
// os:admin, to those its kubeconfig goes to: either is the whole cluster.
func (s *Server) talosconfig(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privKubeconfig)
	if !ok {
		return
	}
	if dist, _ := distribution(c.Spec.Topology.ClassRef.Name); dist != string(templates.Talos) {
		writeError(w, http.StatusNotFound, "only a Talos cluster has a talosconfig")
		return
	}
	secret := &corev1.Secret{}
	if err := u.kube.Get(r.Context(), client.ObjectKey{Namespace: c.Namespace, Name: provisioner.TalosconfigSecret(c.Name)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusConflict, "the cluster has no talosconfig yet")
			return
		}
		s.kubeError(w, err)
		return
	}
	s.Log.Info("handed out talosconfig", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name)
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", c.Name+".talosconfig"))
	_, _ = w.Write(secret.Data[provisioner.TalosconfigKey])
}

// retryTalosUpgrade takes back the failure that holds a Talos cluster's node upgrades, so that the
// controller tries again.
func (s *Server) retryTalosUpgrade(w http.ResponseWriter, r *http.Request, u user) {
	c, _, ok := s.cluster(w, r, u, privChange)
	if !ok {
		return
	}
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:null}}}`, provisioner.TalosUpgradeFailedAnnotation)
	if err := u.kube.Patch(r.Context(), c, client.RawPatch(types.MergePatchType, patch)); err != nil {
		s.kubeError(w, err)
		return
	}
	s.Log.Info("Talos upgrade tried again", "user", u.ticket.Username, "cluster", c.Namespace+"/"+c.Name)
	w.WriteHeader(http.StatusNoContent)
}

// kubeError answers with what the management cluster refused. Validation messages are the user's
// to read; anything else is logged and kept vague.
func (s *Server) kubeError(w http.ResponseWriter, err error) {
	var status apierrors.APIStatus
	switch {
	case apierrors.IsNotFound(err):
		writeError(w, http.StatusNotFound, "not found")
	case apierrors.IsAlreadyExists(err):
		writeError(w, http.StatusConflict, "a cluster of that name exists")
	case apierrors.IsConflict(err):
		writeError(w, http.StatusConflict, clusterChanged)
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		if errors.As(err, &status) {
			writeError(w, http.StatusUnprocessableEntity, status.Status().Message)
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		s.Log.Error("management cluster request failed", "error", err)
		writeError(w, http.StatusBadGateway, "the management cluster did not answer")
	}
}
