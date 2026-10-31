package ui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// helmReleaseProxyList is the Add-on Provider for Helm's record of each add-on's release in a cluster.
var helmReleaseProxyList = schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HelmReleaseProxyList"}

// managementRead is what the management cluster holds about a cluster, read with the interface's
// own account.
type managementRead struct {
	machines        []clusterv1.Machine
	machinesErr     error
	deployments     []clusterv1.MachineDeployment
	deploymentsErr  error
	checks          []clusterv1.MachineHealthCheck
	checksErr       error
	controlPlane    *unstructured.Unstructured
	controlPlaneErr error
	releases        []unstructured.Unstructured
	releasesErr     error
	events          []corev1.Event
	eventsErr       error
}

// failures are the reads that failed, by what they read.
func (m *managementRead) failures() map[string]error {
	out := map[string]error{}
	for what, err := range map[string]error{
		"machines": m.machinesErr, "machine deployments": m.deploymentsErr, "machine health checks": m.checksErr,
		"control plane": m.controlPlaneErr, "add-on releases": m.releasesErr, "events": m.eventsErr,
	} {
		if err != nil {
			out[what] = err
		}
	}
	return out
}

// readManagement reads the cluster's Cluster API objects, add-on releases and events, each
// within healthDeadline.
func readManagement(ctx context.Context, kube client.Client, c *clusterv1.Cluster) managementRead {
	var m managementRead
	in := client.InNamespace(c.Namespace)
	ofCluster := client.MatchingLabels{clusterv1.ClusterNameLabel: c.Name}
	var machines clusterv1.MachineList
	var deployments clusterv1.MachineDeploymentList
	var checks clusterv1.MachineHealthCheckList
	var events corev1.EventList
	releases := &unstructured.UnstructuredList{}
	releases.SetGroupVersionKind(helmReleaseProxyList)
	var wg sync.WaitGroup
	read := func(into *error, f func(context.Context) error) {
		wg.Go(func() {
			rctx, cancel := context.WithTimeout(ctx, healthDeadline)
			defer cancel()
			*into = f(rctx)
		})
	}
	read(&m.machinesErr, func(ctx context.Context) error { return kube.List(ctx, &machines, in, ofCluster) })
	read(&m.deploymentsErr, func(ctx context.Context) error { return kube.List(ctx, &deployments, in, ofCluster) })
	read(&m.checksErr, func(ctx context.Context) error { return kube.List(ctx, &checks, in, ofCluster) })
	read(&m.releasesErr, func(ctx context.Context) error { return kube.List(ctx, releases, in, ofCluster) })
	read(&m.eventsErr, func(ctx context.Context) error { return kube.List(ctx, &events, in, client.Limit(healthListLimit)) })
	read(&m.controlPlaneErr, func(ctx context.Context) error {
		// Until the topology has made it, there is none to read.
		if c.Spec.ControlPlaneRef.Name == "" {
			return nil
		}
		var err error
		m.controlPlane, err = readControlPlane(ctx, kube, c)
		return err
	})
	wg.Wait()
	m.machines, m.deployments, m.checks, m.releases, m.events = machines.Items, deployments.Items, checks.Items, releases.Items, events.Items
	return m
}

// readControlPlane reads the cluster's control-plane object, of whichever provider.
func readControlPlane(ctx context.Context, kube client.Client, c *clusterv1.Cluster) (*unstructured.Unstructured, error) {
	ref := c.Spec.ControlPlaneRef
	mapping, err := kube.RESTMapper().RESTMapping(schema.GroupKind{Group: ref.APIGroup, Kind: ref.Kind})
	if err != nil {
		return nil, fmt.Errorf("the API of %s: %w", ref.Kind, err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(mapping.GroupVersionKind)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: ref.Name}, u); err != nil {
		return nil, fmt.Errorf("reading %s %s: %w", ref.Kind, ref.Name, err)
	}
	return u, nil
}

// Cluster API conditions that are normal when False; while True, something is under way.
var negativePolarity = map[string]string{
	"Paused": "paused", "Deleting": "being deleted", "RollingOut": "rolling out", "Remediating": "replacing a machine",
	"ScalingUp": "scaling up", "ScalingDown": "scaling down",
}

// abnormal tells whether a condition is worth a look.
func abnormal(cond *conditionView) bool {
	if _, negative := negativePolarity[cond.Type]; negative {
		return cond.Status == string(metav1.ConditionTrue)
	}
	return cond.Status != string(metav1.ConditionTrue)
}

// unstructuredConditions reads an object's conditions, of Cluster API's v1beta1 or v1beta2 kind.
func unstructuredConditions(u *unstructured.Unstructured) []conditionView {
	raw, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	out := []conditionView{}
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		text := func(k string) string {
			if s, ok := m[k].(string); ok {
				return s
			}
			return ""
		}
		cv := conditionView{Type: text("type"), Status: text("status"), Reason: text("reason"), Message: text("message")}
		if t, err := time.Parse(time.RFC3339, text("lastTransitionTime")); err == nil {
			cv.Since = t
		}
		out = append(out, cv)
	}
	return out
}

func findCondition(conds []conditionView, kind string) *conditionView {
	for i := range conds {
		if conds[i].Type == kind {
			return &conds[i]
		}
	}
	return nil
}

func isFalse(conds []metav1.Condition, kind string) (*metav1.Condition, bool) {
	c := meta.FindStatusCondition(conds, kind)
	return c, c != nil && c.Status == metav1.ConditionFalse
}

func deref(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// roleWord is how a node's role reads at the start of a sentence.
func roleWord(role string) string {
	if role == controlPlaneRole {
		return "Control plane"
	}
	return "Worker"
}

func machineRole(m *clusterv1.Machine) string {
	if _, cp := m.Labels[clusterv1.MachineControlPlaneLabel]; cp {
		return controlPlaneRole
	}
	return workerRole
}

// nodesSection lists the cluster's machines with their nodes, and the nodes of no machine.
func nodesSection(m *managementRead, wl *workloadRead, now time.Time, a *attention) section[nodeView] {
	s := section[nodeView]{Items: []nodeView{}}
	if m.machinesErr != nil {
		s.Error = problem(m.machinesErr)
	}
	nodes := make(map[string]*corev1.Node, len(wl.nodes))
	for i := range wl.nodes {
		nodes[wl.nodes[i].Name] = &wl.nodes[i]
	}
	sums := podRequests(wl.pods)
	leaving := map[string]bool{}
	seen := map[string]bool{}
	for i := range m.machines {
		mc := &m.machines[i]
		v := nodeView{
			Name: cmp.Or(mc.Status.NodeRef.Name, mc.Name), Machine: mc.Name, Role: machineRole(mc),
			Pool:   cmp.Or(mc.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel], mc.Labels[clusterv1.MachineDeploymentNameLabel]),
			vmName: mc.Spec.InfrastructureRef.Name,
		}
		n := nodes[mc.Status.NodeRef.Name]
		if n != nil {
			fillNode(&v, n, sums[n.Name])
			seen[n.Name] = true
			leaving[n.Name] = !mc.DeletionTimestamp.IsZero()
		}
		machineAttention(a, mc, n != nil, now)
		s.Items = append(s.Items, v)
	}
	for i := range wl.nodes {
		n := &wl.nodes[i]
		if seen[n.Name] {
			continue
		}
		v := nodeView{Name: n.Name, Role: workerRole}
		if _, cp := n.Labels["node-role.kubernetes.io/control-plane"]; cp {
			v.Role = controlPlaneRole
		}
		fillNode(&v, n, sums[n.Name])
		s.Items = append(s.Items, v)
	}
	for i := range s.Items {
		if s.Items[i].Joined {
			s.Items[i].leaving = leaving[s.Items[i].Name]
			nodeAttention(a, &s.Items[i], now)
		}
	}
	slices.SortFunc(s.Items, func(x, y nodeView) int {
		return cmp.Or(cmp.Compare(roleOrder(x.Role), roleOrder(y.Role)), strings.Compare(x.Name, y.Name))
	})
	return s
}

func roleOrder(role string) int {
	if role == controlPlaneRole {
		return 0
	}
	return 1
}

// machineAttention says what holds a machine up. Once its node is read, the node says it is not
// ready, with the kubelet's own words.
func machineAttention(a *attention, m *clusterv1.Machine, nodeRead bool, now time.Time) {
	ready := meta.FindStatusCondition(m.Status.Conditions, clusterv1.MachineReadyCondition)
	if ready != nil && ready.Status == metav1.ConditionTrue {
		return
	}
	who := roleWord(machineRole(m)) + " " + cmp.Or(m.Status.NodeRef.Name, m.Name)
	message, since := "", m.CreationTimestamp.Time
	if ready != nil {
		message, since = ready.Message, ready.LastTransitionTime.Time
	}
	if check, failed := isFalse(m.Status.Conditions, clusterv1.MachineHealthCheckSucceededCondition); failed {
		a.addMachine(m.Name, severityError, who+" failed health check", check.Message, check.LastTransitionTime.Time)
		return
	}
	switch {
	case !m.DeletionTimestamp.IsZero() || m.Status.Phase == string(clusterv1.MachinePhaseDeleting):
		a.addMachine(m.Name, severityProgress, who+" being deleted", message, since)
	case m.Status.NodeRef.Name == "":
		a.addMachine(m.Name, severityProgress, who+" being created", message, since)
	case !nodeRead && now.Sub(since) < settling:
		a.addMachine(m.Name, severityProgress, who+" joining", message, since)
	case !nodeRead:
		a.addMachine(m.Name, severityWarning, who+" not ready", message, since)
	}
}

// pressure is a node condition that reports a shortage, and how it reads.
type pressure struct {
	condition corev1.NodeConditionType
	word      string
}

var pressures = []pressure{
	{corev1.NodeMemoryPressure, "memory pressure"},
	{corev1.NodeDiskPressure, "disk pressure"},
	{corev1.NodePIDPressure, "PID pressure"},
	{corev1.NodeNetworkUnavailable, "no network"},
}

// pressureWord is how a node condition reporting a shortage reads, or false for any other.
func pressureWord(condition corev1.NodeConditionType) (string, bool) {
	i := slices.IndexFunc(pressures, func(p pressure) bool { return p.condition == condition })
	if i < 0 {
		return "", false
	}
	return pressures[i].word, true
}

// controlPlaneTaint is the taint every distribution keeps control-plane nodes apart with, which
// is normal and not worth listing.
const controlPlaneTaint = "node-role.kubernetes.io/control-plane"

func fillNode(v *nodeView, n *corev1.Node, sums podSums) {
	v.Joined = true
	v.Kubelet = n.Status.NodeInfo.KubeletVersion
	v.Unschedulable = n.Spec.Unschedulable
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			v.Ready, v.Since = cond.Status == corev1.ConditionTrue, cond.LastTransitionTime.Time
			v.Silent = cond.Status == corev1.ConditionUnknown
			if !v.Ready {
				v.Message = cmp.Or(cond.Message, cond.Reason)
			}
			continue
		}
		if _, shortage := pressureWord(cond.Type); shortage && cond.Status == corev1.ConditionTrue {
			v.Pressure = append(v.Pressure, string(cond.Type))
		}
	}
	for _, t := range n.Spec.Taints {
		// The scheduler's own taints mirror the conditions and the cordon shown beside them.
		if t.Key == controlPlaneTaint || strings.HasPrefix(t.Key, "node.kubernetes.io/") {
			continue
		}
		v.Taints = append(v.Taints, t.Key+":"+string(t.Effect))
	}
	v.CPU = &resourceUse{Requested: sums.cpu, Allocatable: n.Status.Allocatable.Cpu().MilliValue()}
	v.Memory = &resourceUse{Requested: sums.memory, Allocatable: n.Status.Allocatable.Memory().Value()}
	v.Pods = sums.pods
}

func nodeAttention(a *attention, v *nodeView, now time.Time) {
	who := roleWord(v.Role) + " " + v.Name
	if !v.Ready {
		severity := severityError
		if v.leaving || now.Sub(v.Since) < settling {
			severity = severityProgress
		}
		v.Down = severity == severityError
		a.addMachine(v.Machine, severity, who+" not ready", v.Message, v.Since)
	}
	if len(v.Pressure) > 0 {
		words := make([]string, 0, len(v.Pressure))
		for _, p := range v.Pressure {
			word, _ := pressureWord(corev1.NodeConditionType(p))
			words = append(words, word)
		}
		a.addMachine(v.Machine, severityWarning, who+": "+strings.Join(words, ", "), "", time.Time{})
	}
	if v.Unschedulable && !v.leaving {
		a.addMachine(v.Machine, severityWarning, who+" cordoned", "", time.Time{})
	}
}

// podSums are the requests of the pods on one node.
type podSums struct {
	cpu, memory int64
	pods        int
}

func podRequests(pods []corev1.Pod) map[string]podSums {
	sums := map[string]podSums{}
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		s := sums[p.Spec.NodeName]
		s.pods++
		for j := range p.Spec.Containers {
			requests := p.Spec.Containers[j].Resources.Requests
			s.cpu += requests.Cpu().MilliValue()
			s.memory += requests.Memory().Value()
		}
		sums[p.Spec.NodeName] = s
	}
	return sums
}

// addonsSection lists the cluster's add-on releases as the Add-on Provider for Helm records them.
func addonsSection(c *clusterv1.Cluster, m *managementRead, a *attention) section[addonView] {
	s := section[addonView]{Items: []addonView{}}
	if m.releasesErr != nil {
		s.Error = problem(m.releasesErr)
		return s
	}
	for i := range m.releases {
		u := &m.releases[i]
		text := func(path ...string) string { v, _, _ := unstructured.NestedString(u.Object, path...); return v }
		revision, _, _ := unstructured.NestedInt64(u.Object, "status", "revision")
		version, _, _ := strings.Cut(text("spec", "version"), "@")
		v := addonView{
			Name: addonName(c, u), Release: text("spec", "releaseName"), Namespace: text("spec", "releaseNamespace"),
			Chart: text("spec", "chartName"), Version: version, Status: text("status", "status"), Revision: revision,
		}
		conds := unstructuredConditions(u)
		if ready := findCondition(conds, "Ready"); ready != nil {
			v.Ready, v.Since = ready.Status == string(metav1.ConditionTrue), ready.Since
		}
		if !v.Ready {
			for _, cond := range conds {
				if cond.Status != string(metav1.ConditionTrue) && cond.Message != "" {
					v.Message = cond.Message
					break
				}
			}
		}
		switch {
		case v.Status == "failed":
			a.add(severityError, sectionAddons, "Add-on "+v.Name+" failed", v.Message, v.Since)
		case v.Ready:
		case v.Status == "" || strings.HasPrefix(v.Status, "pending"):
			a.add(severityProgress, sectionAddons, "Add-on "+v.Name+" being installed", v.Message, v.Since)
		default:
			a.add(severityWarning, sectionAddons, "Add-on "+v.Name+" not ready", v.Message, v.Since)
		}
		s.Items = append(s.Items, v)
	}
	slices.SortFunc(s.Items, func(x, y addonView) int {
		return cmp.Or(strings.Compare(x.Name, y.Name), strings.Compare(x.Release, y.Release))
	})
	return s
}

// addonName is the add-on a release is of: its HelmChartProxy's name without the cluster's.
func addonName(c *clusterv1.Cluster, u *unstructured.Unstructured) string {
	for _, o := range u.GetOwnerReferences() {
		if o.Kind == "HelmChartProxy" {
			return strings.TrimPrefix(o.Name, c.Name+"-")
		}
	}
	release, _, _ := unstructured.NestedString(u.Object, "spec", "releaseName")
	return cmp.Or(release, u.GetName())
}

// poolsSection lists the cluster's worker pools, as their MachineDeployments stand.
func poolsSection(m *managementRead, a *attention) section[poolView] {
	s := section[poolView]{Items: []poolView{}}
	if m.deploymentsErr != nil {
		s.Error = problem(m.deploymentsErr)
		return s
	}
	for i := range m.deployments {
		md := &m.deployments[i]
		v := poolView{
			Name:    cmp.Or(md.Labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel], md.Name),
			Desired: deref(md.Spec.Replicas), Ready: deref(md.Status.ReadyReplicas),
			Available: deref(md.Status.AvailableReplicas), UpToDate: deref(md.Status.UpToDateReplicas),
			Autoscaling: autoscalingOf(md.Annotations),
		}
		conds := md.Status.Conditions
		rolling := meta.FindStatusCondition(conds, clusterv1.MachineDeploymentRollingOutCondition)
		v.RollingOut = rolling != nil && rolling.Status == metav1.ConditionTrue || v.UpToDate < v.Desired
		scaling := meta.FindStatusCondition(conds, clusterv1.MachineDeploymentScalingUpCondition)
		if scaling == nil || scaling.Status != metav1.ConditionTrue {
			scaling = meta.FindStatusCondition(conds, clusterv1.MachineDeploymentScalingDownCondition)
		}
		who := "Pool " + v.Name
		switch available, notAvailable := isFalse(conds, clusterv1.MachineDeploymentAvailableCondition); {
		case v.RollingOut:
			if rolling != nil {
				v.Message = rolling.Message
			}
			a.add(severityProgress, sectionClusterAPI, fmt.Sprintf("%s rolling out: %d of %d up to date", who, v.UpToDate, v.Desired), v.Message, time.Time{})
		case scaling != nil && scaling.Status == metav1.ConditionTrue:
			v.Message = scaling.Message
			a.add(severityProgress, sectionClusterAPI, fmt.Sprintf("%s scaling to %d", who, v.Desired), v.Message, scaling.LastTransitionTime.Time)
		case notAvailable:
			v.Message = available.Message
			a.add(severityWarning, sectionClusterAPI, who+" not available", v.Message, available.LastTransitionTime.Time)
		}
		s.Items = append(s.Items, v)
	}
	slices.SortFunc(s.Items, func(x, y poolView) int { return strings.Compare(x.Name, y.Name) })
	return s
}

// Control-plane conditions that sum up the machines, whose own entries say more.
var machineSummaries = map[string]bool{"Ready": true, "MachinesReady": true, "MachinesUpToDate": true, "Paused": true, "Deleting": true}

func controlPlaneSection(c *clusterv1.Cluster, m *managementRead, a *attention) controlPlaneView {
	v := controlPlaneView{Kind: c.Spec.ControlPlaneRef.Kind, Conditions: []conditionView{}}
	if m.controlPlaneErr != nil {
		v.Error = problem(m.controlPlaneErr)
		return v
	}
	if m.controlPlane == nil {
		return v
	}
	v.Conditions = unstructuredConditions(m.controlPlane)
	for i := range v.Conditions {
		cond := &v.Conditions[i]
		if machineSummaries[cond.Type] || !abnormal(cond) {
			continue
		}
		if doing, ok := negativePolarity[cond.Type]; ok {
			a.add(severityProgress, sectionControlPlane, "Control plane "+doing, cond.Message, cond.Since)
			continue
		}
		a.add(severityWarning, sectionControlPlane, "Control plane: "+cond.Type+" is "+cond.Status, cond.Message, cond.Since)
	}
	return v
}

func healthChecksSection(c *clusterv1.Cluster, m *managementRead, a *attention) section[healthCheckView] {
	s := section[healthCheckView]{Items: []healthCheckView{}}
	if m.checksErr != nil {
		s.Error = problem(m.checksErr)
		return s
	}
	remediating := map[string]bool{}
	for i := range m.machines {
		mc := &m.machines[i]
		_, failed := isFalse(mc.Status.Conditions, clusterv1.MachineHealthCheckSucceededCondition)
		_, replacing := isFalse(mc.Status.Conditions, clusterv1.MachineOwnerRemediatedCondition)
		remediating[mc.Name] = failed || replacing
	}
	for i := range m.checks {
		mhc := &m.checks[i]
		v := healthCheckView{
			Name: strings.TrimPrefix(mhc.Name, c.Name+"-"), Expected: deref(mhc.Status.ExpectedMachines),
			Healthy: deref(mhc.Status.CurrentHealthy), Allowed: true,
		}
		if allowed, refused := isFalse(mhc.Status.Conditions, clusterv1.MachineHealthCheckRemediationAllowedCondition); refused {
			v.Allowed, v.Message = false, allowed.Message
			a.add(severityError, sectionClusterAPI, "Health check "+v.Name+" stopped replacing machines", v.Message, allowed.LastTransitionTime.Time)
		}
		for _, t := range mhc.Status.Targets {
			if remediating[t] {
				v.Remediating = append(v.Remediating, t)
			}
		}
		s.Items = append(s.Items, v)
	}
	slices.SortFunc(s.Items, func(x, y healthCheckView) int { return strings.Compare(x.Name, y.Name) })
	return s
}

// clusterObjects are the names of the management cluster's objects of a cluster, which its events
// there are about: the Cluster, its machines with their VMs and bootstrap configurations, pools,
// health checks, control plane and add-on releases.
func clusterObjects(c *clusterv1.Cluster, m *managementRead) (map[string]bool, []string) {
	prefixes := make([]string, 0, len(m.deployments))
	names := map[string]bool{c.Name: true, c.Spec.ControlPlaneRef.Name: true, c.Spec.InfrastructureRef.Name: true}
	for i := range m.machines {
		mc := &m.machines[i]
		names[mc.Name], names[mc.Spec.InfrastructureRef.Name], names[mc.Spec.Bootstrap.ConfigRef.Name] = true, true, true
	}
	for i := range m.deployments {
		names[m.deployments[i].Name] = true
		// A pool's MachineSets are named after it.
		prefixes = append(prefixes, m.deployments[i].Name+"-")
	}
	for i := range m.checks {
		names[m.checks[i].Name] = true
	}
	for i := range m.releases {
		names[m.releases[i].GetName()] = true
		for _, o := range m.releases[i].GetOwnerReferences() {
			names[o.Name] = true
		}
	}
	delete(names, "")
	return names, prefixes
}

// clusterObject tells whether an event is about one of the cluster's objects in the management
// cluster.
func clusterObject(c *clusterv1.Cluster, m *managementRead) func(corev1.ObjectReference) bool {
	names, prefixes := clusterObjects(c, m)
	return func(o corev1.ObjectReference) bool {
		return names[o.Name] || slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(o.Name, p) })
	}
}

// eventMerge counts an event repeated, by its source, type, object, reason and message, once.
type eventMerge map[string]*eventView

// add counts in an event that last happened at from or later.
func (em eventMerge) add(source string, e *corev1.Event, from time.Time) {
	last := eventTime(e)
	if last.Before(from) {
		return
	}
	o := e.InvolvedObject
	object := o.Kind + " " + o.Name
	if o.Namespace != "" && source == sourceWorkload {
		object = o.Kind + " " + o.Namespace + "/" + o.Name
	}
	count := max(e.Count, 1)
	if e.Series != nil {
		count = max(e.Series.Count, count)
	}
	key := strings.Join([]string{source, e.Type, object, e.Reason, e.Message}, "\x00")
	if kept, ok := em[key]; ok {
		kept.Count += count
		if last.After(kept.Last) {
			kept.Last = last
		}
		return
	}
	em[key] = &eventView{Source: source, Type: e.Type, Reason: e.Reason, Message: e.Message, Object: object, Count: count, Last: last, Action: e.Action}
}

// eventsSection merges the events of the last hour: the management cluster's about the cluster's
// objects, and the workload cluster's warnings, repeats counted once.
func eventsSection(c *clusterv1.Cluster, m *managementRead, wl *workloadRead, now time.Time) section[eventView] {
	s := section[eventView]{Items: []eventView{}}
	var problems []string
	if m.eventsErr != nil {
		problems = append(problems, "management cluster: "+problem(m.eventsErr))
	}
	if wl.eventsErr != nil && !wl.unreachable() {
		problems = append(problems, "cluster: "+problem(wl.eventsErr))
	}
	s.Error = strings.Join(problems, ", ")
	ours := clusterObject(c, m)
	from := now.Add(-eventWindow)
	merged := eventMerge{}
	for i := range m.events {
		if ours(m.events[i].InvolvedObject) {
			merged.add(sourceManagement, &m.events[i], from)
		}
	}
	for i := range wl.events {
		if wl.events[i].Type == corev1.EventTypeWarning {
			merged.add(sourceWorkload, &wl.events[i], from)
		}
	}
	for _, e := range merged {
		s.Items = append(s.Items, *e)
	}
	slices.SortFunc(s.Items, func(x, y eventView) int { return cmp.Or(y.Last.Compare(x.Last), strings.Compare(x.Object, y.Object)) })
	if len(s.Items) > eventsShown {
		s.Items, s.Truncated = s.Items[:eventsShown], true
	}
	return s
}

// eventTime is when an event last happened, by whichever of its times its reporter set.
func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}
