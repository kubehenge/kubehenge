package ui

import (
	"context"
	"crypto/sha256"
	_ "embed" // the demo's sample fleet
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

// demoFleetJSON is the sample fleet: its clusters, add-ons, settings and activity.
//
//go:embed demo-fleet.json
var demoFleetJSON []byte

const (
	demoPool           = "kubehenge"
	demoClassNamespace = "kubehenge-system"
	demoNamespace      = provisioner.InterfaceNamespace
	demoGiB            = 1 << 30
	demoControlPlane   = "controlplane.cluster.x-k8s.io"
	statusKey          = "status"
	phaseKey           = "phase"
)

var (
	demoHelmReleaseProxy = schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HelmReleaseProxy"}
	demoControlPlanes    = map[templates.Distribution]schema.GroupVersionKind{
		templates.Kubeadm: {Group: demoControlPlane, Version: clusterv1.GroupVersion.Version, Kind: "KubeadmControlPlane"},
		templates.RKE2:    {Group: demoControlPlane, Version: clusterv1.GroupVersion.Version, Kind: "RKE2ControlPlane"},
		templates.K3s:     {Group: demoControlPlane, Version: clusterv1.GroupVersion.Version, Kind: "KThreesControlPlane"},
		templates.Talos:   {Group: demoControlPlane, Version: "v1beta1", Kind: "TalosControlPlane"},
	}
	demoVeleroKinds = map[schema.GroupVersionResource]string{
		veleroBackups: "BackupList", veleroRestores: "RestoreList", veleroSchedule: "ScheduleList",
		veleroLocation: "BackupStorageLocationList", veleroVolumes: "PodVolumeRestoreList",
		veleroDeletions: "DeleteBackupRequestList", veleroDownloads: "DownloadRequestList",
	}
)

// demoData is demo-fleet.json.
type demoData struct {
	ProxmoxNodes     []string                   `json:"proxmoxNodes"`
	VMStatus         string                     `json:"vmStatus"`
	Settings         map[string]json.RawMessage `json:"settings"`
	NodeOS           json.RawMessage            `json:"nodeOS"`
	AddOns           map[string]demoAddOnData   `json:"addOns"`
	BackupTargets    json.RawMessage            `json:"backupTargets"`
	Clusters         []demoCluster              `json:"clusters"`
	KubeadmTemplates []string                   `json:"kubeadmTemplates"`
	// Management are the appliance's saved settings, as its settings.json holds them.
	Management     json.RawMessage    `json:"management"`
	TemplateBuilds []demoTemplateLine `json:"templateBuilds"`
	ReleasePins    []demoPin          `json:"releasePins"`
	Activity       []demoActivityLine `json:"activity"`
}

// demoTemplateLine is a template line of the demo's template pool.
type demoTemplateLine struct {
	Distribution      templates.Distribution `json:"distribution"`
	KubernetesVersion string                 `json:"kubernetesVersion"`
	// Builds are the line's builds. A line without any is a single template, as Talos has.
	Builds []demoBuild `json:"builds"`
}

type demoBuild struct {
	AgoDays int `json:"agoDays"`
	// Older says its kernel is older than the previous build's.
	Older bool `json:"older"`
}

// demoPin is a release an administrator pinned.
type demoPin struct {
	Version string `json:"version"`
	By      string `json:"by"`
	AgoDays int    `json:"agoDays"`
}

// demoAddOnData is an add-on: the chart it installs and the cluster variable that switches it on.
type demoAddOnData struct {
	Chart     string          `json:"chart"`
	Namespace string          `json:"namespace"`
	Version   string          `json:"version"`
	Value     json.RawMessage `json:"value"`
}

// demoCluster is one cluster of the sample fleet.
type demoCluster struct {
	Name          string                 `json:"name"`
	Distribution  templates.Distribution `json:"distribution"`
	Version       string                 `json:"version"`
	ControlPlanes int32                  `json:"controlPlanes"`
	Workers       int32                  `json:"workers"`
	// Subnet is the third octet of the cluster's addresses, 10.20.<subnet>.0/24.
	Subnet  int      `json:"subnet"`
	AgeDays int      `json:"ageDays"`
	AddOns  []string `json:"addOns"`
	// Crashing names a workload whose pod restarts again and again, with an alert firing for it.
	Crashing string `json:"crashing"`
	// Upgrade is set on a cluster whose upgrade is under way.
	Upgrade *demoUpgrade `json:"upgrade"`
	// Paused is a cluster whose reconciling is paused.
	Paused bool `json:"paused"`
	// Conditions are what Kubehenge's controller has noted on the cluster.
	Conditions []demoNote `json:"conditions"`
}

// demoNote is a condition on a demo cluster that holds, set AgoHours before the demo's now.
type demoNote struct {
	Type     string `json:"type"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	AgoHours int    `json:"agoHours"`
}

type demoUpgrade struct {
	From   string          `json:"from"`
	Change []plannedChange `json:"change"`
}

type demoActivityLine struct {
	AgoMinutes int               `json:"agoMinutes"`
	User       string            `json:"user"`
	What       string            `json:"what"`
	Cluster    string            `json:"cluster"`
	Details    map[string]string `json:"details"`
}

// demoFleet is everything the demo serves: the management cluster's objects, each workload
// cluster's own API, the Proxmox VE guests and the activity.
type demoFleet struct {
	data      demoData
	now       time.Time
	objects   []client.Object
	workloads map[string]*Workload
	// members are each cluster pool's guests, by pool.
	members  map[string][]pve.Member
	activity []activityEntry
	// templates are the VM templates of the template pool. builds and older are their builds as the
	// ClusterClasses record them.
	templates     []pve.Member
	builds, older provisioner.NodeImageBuilds
}

func newDemoFleet(now time.Time) (*demoFleet, error) {
	f := &demoFleet{now: now, workloads: map[string]*Workload{}, members: map[string][]pve.Member{}}
	if err := json.Unmarshal(demoFleetJSON, &f.data); err != nil {
		return nil, fmt.Errorf("the demo's fleet: %w", err)
	}
	if err := f.templateLines(); err != nil {
		return nil, err
	}
	classes, err := f.classes()
	if err != nil {
		return nil, err
	}
	f.objects = append(f.objects, classes...)
	f.objects = append(f.objects, f.settingsObjects()...)
	vmid := 101
	for i := range f.data.Clusters {
		if err := f.add(&f.data.Clusters[i], &vmid); err != nil {
			return nil, err
		}
	}
	for _, l := range f.data.Activity {
		e := activityEntry{At: now.Add(-time.Duration(l.AgoMinutes) * time.Minute), User: l.User, What: l.What, Details: l.Details}
		if l.Cluster != "" {
			e.Cluster = clusterKey(demoNamespace, l.Cluster)
		}
		f.activity = append(f.activity, e)
	}
	return f, nil
}

// classes are the chart's ClusterClasses, with the templates, snapshot storages and backup
// targets the controller would publish on them.
func (f *demoFleet) classes() ([]client.Object, error) {
	var classes []clusterv1.ClusterClass
	if err := json.Unmarshal(demoClassesJSON, &classes); err != nil {
		return nil, fmt.Errorf("the demo's ClusterClasses: %w", err)
	}
	nodeOS := templates.DefaultNodeOS.String()
	kubeadm, err := json.Marshal(map[string][]string{nodeOS: f.data.KubeadmTemplates})
	if err != nil {
		return nil, fmt.Errorf("the demo's kubeadm templates: %w", err)
	}
	var storage string
	if err = json.Unmarshal(f.data.Settings["storage"], &storage); err != nil {
		return nil, fmt.Errorf("the demo's storage: %w", err)
	}
	builds, err := f.builds.Encode()
	if err != nil {
		return nil, fmt.Errorf("the demo's template builds: %w", err)
	}
	older, err := f.older.Encode()
	if err != nil {
		return nil, fmt.Errorf("the demo's older template builds: %w", err)
	}
	objects := make([]client.Object, 0, len(classes))
	for i := range classes {
		c := &classes[i]
		switch templates.Distribution(strings.TrimPrefix(c.Name, provisioner.ClassPrefix)) {
		case templates.Kubeadm:
			c.Annotations[provisioner.KubeadmVersionsAnnotation] = string(kubeadm)
			c.Annotations[provisioner.NodeImageBuildsAnnotation], c.Annotations[provisioner.NodeImageOlderAnnotation] = builds, older
		case templates.Talos:
			c.Annotations[provisioner.TalosTemplatesAnnotation] = templates.DefaultTalosVersion
		case templates.RKE2, templates.K3s:
			c.Annotations[provisioner.NodeOSTemplatesAnnotation] = nodeOS
			c.Annotations[provisioner.NodeImageBuildsAnnotation], c.Annotations[provisioner.NodeImageOlderAnnotation] = builds, older
		}
		c.Annotations[provisioner.SnapshotStoragesAnnotation] = storage
		c.Annotations[provisioner.BackupTargetsAnnotation] = string(f.data.BackupTargets)
		objects = append(objects, c)
	}
	return objects, nil
}

// demoTemplateVMID is the first guest ID of the demo's templates.
const demoTemplateVMID = 9000

// templateLines makes the templates of the template pool and the builds the ClusterClasses record.
func (f *demoFleet) templateLines() error {
	f.builds, f.older = provisioner.NodeImageBuilds{}, provisioner.NodeImageBuilds{}
	vmid := demoTemplateVMID
	add := func(t *templates.Template) {
		f.templates = append(f.templates, pve.Member{
			VMID: vmid, Node: f.data.ProxmoxNodes[vmid%len(f.data.ProxmoxNodes)], Type: "qemu", Name: t.Name, Template: 1, Tags: strings.Join(t.Tags, ";"),
		})
		vmid++
	}
	for _, l := range f.data.TemplateBuilds {
		line, err := templates.Plan(&templates.Request{Distribution: l.Distribution, KubernetesVersion: l.KubernetesVersion})
		if err != nil {
			return fmt.Errorf("the demo's template line %s: %w", l.Distribution, err)
		}
		if len(l.Builds) == 0 {
			add(&line)
			continue
		}
		for _, b := range l.Builds {
			at := f.now.AddDate(0, 0, -b.AgoDays).Truncate(time.Minute)
			build, err := line.Build(at)
			if err != nil {
				return fmt.Errorf("the demo's template line %s: %w", l.Distribution, err)
			}
			add(&build)
			recorded := f.builds
			if b.Older {
				recorded = f.older
			}
			key := templates.LineKey(build.Tags)
			recorded[key] = append(recorded[key], at)
		}
	}
	return nil
}

// settingsObjects are the ConfigMaps of the management cluster that hold the weekly template build
// and the release pins administrators added.
func (f *demoFleet) settingsObjects() []client.Object {
	now := f.now.UTC()
	day := now.Truncate(24 * time.Hour)
	// The weekly build runs on Sundays at 01:00 UTC. The last one finished soon after it started.
	started := day.AddDate(0, 0, -int(day.Weekday())).Add(time.Hour)
	if started.After(now) {
		started = started.AddDate(0, 0, -7)
	}
	refresh := &corev1.ConfigMap{Data: map[string]string{
		provisioner.RefreshWeekday: "Sun", provisioner.RefreshTime: "01:00", provisioner.RefreshTimeZone: time.UTC.String(),
		provisioner.RefreshSoakHours: "24", provisioner.RefreshCreatorBuilds: "false",
		provisioner.RefreshState: runSucceeded, provisioner.RefreshTrigger: "schedule",
		provisioner.RefreshStarted:  started.Format(time.RFC3339),
		provisioner.RefreshFinished: started.Add(41 * time.Minute).Format(time.RFC3339),
		provisioner.RefreshLines:    "rke2-almalinux-10, k3s-almalinux-10, kubeadm-v1.37.1-almalinux-10, kubeadm-v1.36.5-almalinux-10",
	}}
	refresh.Namespace, refresh.Name = demoClassNamespace, provisioner.TemplateRefreshName
	var pins strings.Builder
	for _, p := range f.data.ReleasePins {
		file, script := demoDigest(p.Version, "checksums"), demoDigest(p.Version, "install.sh")
		at := now.AddDate(0, 0, -p.AgoDays).Truncate(time.Minute)
		fmt.Fprintf(&pins, "%s %s %s # added by %s at %s\n", p.Version, file, script, p.By, at.Format(time.RFC3339))
	}
	pinned := &corev1.ConfigMap{Data: map[string]string{"pins": pins.String()}}
	pinned.Namespace, pinned.Name = demoClassNamespace, provisioner.ReleasePinsName
	return []client.Object{refresh, pinned}
}

// demoDigest is a stand-in SHA-256 for a release's file.
func demoDigest(version, file string) string {
	sum := sha256.Sum256([]byte(version + " " + file))
	return hex.EncodeToString(sum[:])
}

// add puts one cluster into the fleet: its Cluster API objects, its VMs and its own API.
func (f *demoFleet) add(d *demoCluster, vmid *int) error {
	created := f.now.AddDate(0, 0, -d.AgeDays)
	c := f.cluster(d, created)
	names, err := provisioner.NamesFor(demoPool, c.Namespace, c.Name)
	if err != nil {
		return fmt.Errorf("the demo's cluster %s: %w", d.Name, err)
	}
	f.objects = append(f.objects, c, demoControlPlaneObject(d, created), f.workerPool(d, created))
	var machines []*clusterv1.Machine
	for i := range d.ControlPlanes {
		machines = append(machines, f.machine(d, controlPlaneRole, int(i), created))
	}
	for i := range d.Workers {
		machines = append(machines, f.machine(d, workerPool, int(i), created))
	}
	server := fmt.Sprintf("https://10.20.%d.10:6443", d.Subnet)
	for _, id := range []provisioner.Identity{provisioner.IdentityHealth, provisioner.IdentityLogs, provisioner.IdentityAlerts, provisioner.IdentityBackups} {
		f.objects = append(f.objects, f.identity(d.Name, id, server))
	}
	for _, a := range d.AddOns {
		f.objects = append(f.objects, demoAddOn(d, a, f.data.AddOns[a], created))
	}
	nodes := f.data.ProxmoxNodes
	for i, m := range machines {
		f.objects = append(f.objects, m)
		mem := int64(8) * demoGiB
		if !isControlPlane(m) {
			mem = 16 * demoGiB
		}
		f.members[names.Pool] = append(f.members[names.Pool], pve.Member{
			VMID: *vmid, Node: nodes[i%len(nodes)], Type: "qemu", Name: m.Spec.InfrastructureRef.Name,
			Status: f.data.VMStatus, CPU: 0.08 + float64((*vmid*7)%23)/100, Cores: 4, Memory: mem * int64(40+(*vmid*13)%35) / 100, MaxMem: mem,
			Uptime: int64(f.now.Sub(m.CreationTimestamp.Time).Seconds()),
		})
		*vmid++
	}
	f.workloads[server] = f.workload(d, machines)
	return nil
}

func isControlPlane(m *clusterv1.Machine) bool {
	_, ok := m.Labels[clusterv1.MachineControlPlaneLabel]
	return ok
}

func (f *demoFleet) cluster(d *demoCluster, created time.Time) *clusterv1.Cluster {
	cp, workers := d.ControlPlanes, d.Workers
	pods, services := "10.42.0.0/16", "10.43.0.0/16"
	if d.Distribution == templates.Kubeadm || d.Distribution == templates.Talos {
		pods, services = "10.244.0.0/16", "10.96.0.0/12"
	}
	vars := map[string]json.RawMessage{
		"controlPlaneEndpoint": json.RawMessage(fmt.Sprintf(`"10.20.%d.10"`, d.Subnet)),
		"network": json.RawMessage(fmt.Sprintf(
			`{"bridge":"vmbr0","addresses":["10.20.%[1]d.11-10.20.%[1]d.60"],"prefix":24,"gateway":"10.20.%[1]d.1","dnsServers":["10.20.0.53"]}`, d.Subnet)),
	}
	maps.Copy(vars, f.data.Settings)
	if d.Distribution == templates.Talos {
		vars[provisioner.TalosVersionVariable] = json.RawMessage(`"` + templates.DefaultTalosVersion + `"`)
	} else {
		vars[provisioner.NodeOSVariable] = f.data.NodeOS
	}
	for _, a := range d.AddOns {
		vars[a] = f.data.AddOns[a].Value
	}
	c := &clusterv1.Cluster{
		Namespace: demoNamespace, Name: d.Name, UID: types.UID("demo-" + d.Name), Generation: 4,
		CreationTimestamp: metav1.NewTime(created), Annotations: map[string]string{createdBy: demoUser},
		Spec: clusterv1.ClusterSpec{
			ClusterNetwork:       clusterv1.ClusterNetwork{Pods: clusterv1.NetworkRanges{CIDRBlocks: []string{pods}}, Services: clusterv1.NetworkRanges{CIDRBlocks: []string{services}}},
			ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: fmt.Sprintf("10.20.%d.10", d.Subnet), Port: 6443},
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: demoControlPlanes[d.Distribution].Group, Kind: demoControlPlanes[d.Distribution].Kind, Name: d.Name + "-control-plane",
			},
			Topology: clusterv1.Topology{
				ClassRef:     clusterv1.ClusterClassRef{Name: provisioner.ClassPrefix + string(d.Distribution), Namespace: demoClassNamespace},
				Version:      d.Version,
				ControlPlane: clusterv1.ControlPlaneTopology{Replicas: &cp},
				Workers:      clusterv1.WorkersTopology{MachineDeployments: []clusterv1.MachineDeploymentTopology{{Class: workerClass, Name: workerPool, Replicas: &workers}}},
			},
		},
	}
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{Name: name, Value: apiextensionsv1.JSON{Raw: vars[name]}})
	}
	since := metav1.NewTime(created.Add(9 * time.Minute))
	upToDateCP, upToDateWorkers := cp, workers
	reconciled := metav1.Condition{
		Type: clusterv1.ClusterTopologyReconciledCondition, Status: metav1.ConditionTrue, Reason: clusterv1.ClusterTopologyReconcileSucceededReason, LastTransitionTime: since,
	}
	if u := d.Upgrade; u != nil {
		started := f.now.Add(-14 * time.Minute)
		upToDateWorkers = workers - 1
		reconciled = metav1.Condition{
			Type: clusterv1.ClusterTopologyReconciledCondition, Status: metav1.ConditionFalse, Reason: clusterv1.ClusterTopologyReconciledClusterUpgradingReason,
			Message: "Cluster is upgrading to " + d.Version, LastTransitionTime: metav1.NewTime(started),
		}
		raw, _ := json.Marshal(changeRecord{By: demoUser, At: started, Settings: u.Change}) //nolint:errchkjson // strings and a time
		c.Annotations[changeAnnotation] = string(raw)
		c.Generation++
	}
	c.Status = clusterv1.ClusterStatus{
		Phase:              string(clusterv1.ClusterPhaseProvisioned),
		ObservedGeneration: c.Generation,
		Initialization:     clusterv1.ClusterInitializationStatus{InfrastructureProvisioned: new(true), ControlPlaneInitialized: new(true)},
		Conditions: []metav1.Condition{
			{Type: clusterv1.AvailableCondition, Status: metav1.ConditionTrue, Reason: clusterv1.AvailableReason, LastTransitionTime: since},
			{Type: clusterv1.ClusterControlPlaneInitializedCondition, Status: metav1.ConditionTrue, Reason: clusterv1.ClusterControlPlaneInitializedReason, LastTransitionTime: since},
			{Type: provisioner.KubehengeReadyCondition, Status: metav1.ConditionTrue, Reason: clusterv1.ReadyReason, LastTransitionTime: since},
			reconciled,
		},
		ControlPlane: &clusterv1.ClusterControlPlaneStatus{
			DesiredReplicas: &cp, Replicas: &cp, ReadyReplicas: &cp, AvailableReplicas: &cp, UpToDateReplicas: &upToDateCP,
		},
		Workers: &clusterv1.WorkersStatus{
			DesiredReplicas: &workers, Replicas: &workers, ReadyReplicas: &workers, AvailableReplicas: &workers, UpToDateReplicas: &upToDateWorkers,
		},
	}
	if d.Paused {
		c.Spec.Paused = new(true)
	}
	for _, n := range d.Conditions {
		c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
			Type: n.Type, Status: metav1.ConditionTrue, Reason: n.Reason, Message: n.Message,
			LastTransitionTime: metav1.NewTime(f.now.Add(-time.Duration(n.AgoHours) * time.Hour)),
		})
	}
	return c
}

// demoCondition is a condition that holds, as an unstructured object carries it.
func demoCondition(kind, reason string, at time.Time) map[string]any {
	return map[string]any{"type": kind, statusKey: string(metav1.ConditionTrue), "reason": reason, "lastTransitionTime": at.Format(time.RFC3339)}
}

func demoControlPlaneObject(d *demoCluster, created time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(demoControlPlanes[d.Distribution])
	u.SetNamespace(demoNamespace)
	u.SetName(d.Name + "-control-plane")
	u.SetLabels(map[string]string{clusterv1.ClusterNameLabel: d.Name})
	at := created.Add(9 * time.Minute)
	u.Object[statusKey] = map[string]any{"conditions": []any{
		demoCondition(clusterv1.AvailableCondition, clusterv1.AvailableReason, at),
		demoCondition(clusterv1.ReadyCondition, clusterv1.ReadyReason, at),
	}}
	return u
}

func (f *demoFleet) workerPool(d *demoCluster, created time.Time) *clusterv1.MachineDeployment {
	replicas := d.Workers
	upToDate := replicas
	conditions := []metav1.Condition{{
		Type: clusterv1.AvailableCondition, Status: metav1.ConditionTrue, Reason: clusterv1.MachineDeploymentAvailableReason, LastTransitionTime: metav1.NewTime(created),
	}}
	if d.Upgrade != nil {
		upToDate--
		conditions = append(conditions, metav1.Condition{
			Type: clusterv1.MachineDeploymentRollingOutCondition, Status: metav1.ConditionTrue, Reason: clusterv1.MachineDeploymentRollingOutReason,
			Message: fmt.Sprintf("Rolling out %d of %d workers", replicas-upToDate, replicas), LastTransitionTime: metav1.NewTime(f.now.Add(-6 * time.Minute)),
		})
	}
	return &clusterv1.MachineDeployment{
		Namespace: demoNamespace, Name: d.Name + "-" + workerPool, CreationTimestamp: metav1.NewTime(created),
		Labels: map[string]string{clusterv1.ClusterNameLabel: d.Name, clusterv1.ClusterTopologyMachineDeploymentNameLabel: workerPool},
		Spec:   clusterv1.MachineDeploymentSpec{ClusterName: d.Name, Replicas: &replicas},
		Status: clusterv1.MachineDeploymentStatus{
			Replicas: &replicas, ReadyReplicas: &replicas, AvailableReplicas: &replicas, UpToDateReplicas: &upToDate, Conditions: conditions,
		},
	}
}

// machine is a cluster's control-plane node or worker, by role: controlPlaneRole or the worker
// pool's name. In an upgrade under way the control plane and the first worker are new, the others
// still on the old version.
func (f *demoFleet) machine(d *demoCluster, role string, i int, created time.Time) *clusterv1.Machine {
	labels := map[string]string{clusterv1.ClusterNameLabel: d.Name}
	host := 21 + i
	controlPlane := role == controlPlaneRole
	if controlPlane {
		host = 11 + i
		labels[clusterv1.MachineControlPlaneLabel] = ""
	} else {
		labels[clusterv1.ClusterTopologyMachineDeploymentNameLabel] = role
	}
	name := fmt.Sprintf("%s-%s-%s", d.Name, role, demoSuffix(d.Name, role, i))
	version, born := d.Version, created.Add(time.Duration(3+i)*time.Minute)
	if d.Upgrade != nil {
		switch {
		case controlPlane:
			born = f.now.Add(-11 * time.Minute)
		case i == 0:
			born = f.now.Add(-5 * time.Minute)
		default:
			version = d.Upgrade.From
		}
	}
	return &clusterv1.Machine{
		Namespace: demoNamespace, Name: name, Labels: labels, CreationTimestamp: metav1.NewTime(born),
		Spec: clusterv1.MachineSpec{
			ClusterName: d.Name, Version: version,
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "ProxmoxMachine", Name: name},
		},
		Status: clusterv1.MachineStatus{
			Phase:     string(clusterv1.MachinePhaseRunning),
			NodeRef:   clusterv1.MachineNodeReference{Name: name},
			Addresses: clusterv1.MachineAddresses{{Type: clusterv1.MachineInternalIP, Address: fmt.Sprintf("10.20.%d.%d", d.Subnet, host)}},
			Conditions: []metav1.Condition{{
				Type: clusterv1.MachineReadyCondition, Status: metav1.ConditionTrue, Reason: clusterv1.MachineReadyReason, LastTransitionTime: metav1.NewTime(born.Add(4 * time.Minute)),
			}},
		},
	}
}

// demoSuffix stands in for the random suffix Cluster API gives a machine's name, the same on every
// run: FNV-1a over the parts, spelt in the letters Kubernetes uses for such suffixes.
func demoSuffix(parts ...any) string {
	const (
		letters     = "bcdfghjklmnpqrstvwxz2456789"
		offsetBasis = 2166136261
		prime       = 16777619
		length      = 5
	)
	h := uint32(offsetBasis)
	for _, b := range []byte(fmt.Sprint(parts...)) {
		h = (h ^ uint32(b)) * prime
	}
	out := make([]byte, length)
	for i := range out {
		out[i] = letters[h%uint32(len(letters))]
		h = h/uint32(len(letters)) + prime
	}
	return string(out)
}

func (f *demoFleet) identity(cluster string, id provisioner.Identity, server string) *corev1.Secret {
	return &corev1.Secret{
		Namespace: demoNamespace, Name: provisioner.IdentitySecretName(cluster, id),
		Annotations: map[string]string{provisioner.IdentityExpiresAnnotation: f.now.Add(24 * time.Hour).Format(time.RFC3339)},
		Data:        map[string][]byte{provisioner.IdentityServerKey: []byte(server), provisioner.IdentityCAKey: []byte("demo"), provisioner.IdentityTokenKey: []byte("demo")},
	}
}

// demoAddOn is an add-on's release in the cluster, as the Add-on Provider for Helm keeps it.
func demoAddOn(d *demoCluster, name string, a demoAddOnData, created time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(demoHelmReleaseProxy)
	u.SetNamespace(demoNamespace)
	u.SetName(fmt.Sprintf("%s-%s-%s", d.Name, name, demoSuffix(d.Name, name)))
	u.SetLabels(map[string]string{clusterv1.ClusterNameLabel: d.Name})
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "addons.cluster.x-k8s.io/v1alpha1", Kind: "HelmChartProxy", Name: d.Name + "-" + name, UID: types.UID("demo")}})
	u.Object["spec"] = map[string]any{"releaseName": name, "releaseNamespace": a.Namespace, "chartName": a.Chart, "version": a.Version}
	u.Object[statusKey] = map[string]any{
		statusKey: "deployed", "revision": int64(3),
		"conditions": []any{demoCondition(clusterv1.ReadyCondition, clusterv1.ReadyReason, created.Add(12*time.Minute))},
	}
	return u
}

// workload is a cluster's own API, as its identities see it.
func (f *demoFleet) workload(d *demoCluster, machines []*clusterv1.Machine) *Workload {
	objects := make([]runtime.Object, 0, 4*len(machines))
	for i, m := range machines {
		objects = append(objects, demoNode(m))
		for j, app := range []string{"api", "frontend", "cache"} {
			if isControlPlane(m) && j > 0 {
				break
			}
			ns := "apps"
			if isControlPlane(m) {
				ns, app = "kube-system", "metrics-server"
			}
			name := fmt.Sprintf("%s-%s", app, demoSuffix(d.Name, m.Name, j))
			objects = append(objects, demoPod(ns, name, m.Name, m.CreationTimestamp.Time, "500m", fmt.Sprintf("%dMi", 512*(1+(i+j)%3))))
		}
	}
	alerts := "[]"
	if d.Crashing != "" {
		objects, alerts = f.crashing(d, machines[len(machines)-1].Name, objects)
	}
	objects = append(objects, demoThinWebhook()...)
	return &Workload{
		Kube:    kubefake.NewClientset(objects...),
		Metrics: demoDeprecatedAPIs,
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), demoVeleroKinds, f.velero(d)...),
		Readyz: func(context.Context) (string, error) {
			return "[+]ping ok\n[+]log ok\n[+]etcd ok\n[+]etcd-readiness ok\n[+]informer-sync ok\nreadyz check passed\n", nil
		},
		Alerts: func(context.Context) ([]byte, error) { return []byte(alerts), nil },
	}
}

// crashing adds a pod that restarts again and again on a worker, its warning, and the alert
// monitoring fires for it.
func (f *demoFleet) crashing(d *demoCluster, worker string, objects []runtime.Object) ([]runtime.Object, string) {
	const namespace, restarts = "billing", 14
	pod := demoPod(namespace, d.Crashing+"-"+demoSuffix(d.Crashing), worker, f.now.Add(-50*time.Minute), "250m", "256Mi")
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(f.now.Add(-48 * time.Minute))}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: pod.Spec.Containers[0].Name, RestartCount: restarts,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}},
	}}
	objects = append(objects, pod, &corev1.Event{
		Namespace: namespace, Name: pod.Name + ".backoff",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: namespace, Name: pod.Name},
		Type:           corev1.EventTypeWarning, Reason: "BackOff", Message: "Back-off restarting failed container " + pod.Spec.Containers[0].Name, Count: restarts,
		LastTimestamp: metav1.NewTime(f.now.Add(-3 * time.Minute)),
	})
	raw, _ := json.Marshal([]map[string]any{{ //nolint:errchkjson // strings only
		"labels":      map[string]string{"alertname": "KubePodCrashLooping", "severity": "warning", "namespace": namespace},
		"annotations": map[string]string{"summary": "Pod " + namespace + "/" + pod.Name + " is crash looping."},
		"startsAt":    f.now.Add(-35 * time.Minute).Format(time.RFC3339),
	}})
	return objects, string(raw)
}

func demoNode(m *clusterv1.Machine) *corev1.Node {
	labels := map[string]string{corev1.LabelHostname: m.Name}
	mem := "15Gi"
	if isControlPlane(m) {
		labels["node-role.kubernetes.io/control-plane"] = ""
		mem = "7Gi"
	}
	since := metav1.NewTime(m.CreationTimestamp.Add(3 * time.Minute))
	return &corev1.Node{
		Name: m.Status.NodeRef.Name, Labels: labels, CreationTimestamp: since,
		Status: corev1.NodeStatus{
			NodeInfo:    corev1.NodeSystemInfo{KubeletVersion: m.Spec.Version},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse(mem)},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastTransitionTime: since},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, LastTransitionTime: since},
				{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, LastTransitionTime: since},
				{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse, LastTransitionTime: since},
			},
		},
	}
}

func demoPod(namespace, name, node string, started time.Time, cpu, memory string) *corev1.Pod {
	return &corev1.Pod{
		Namespace: namespace, Name: name, CreationTimestamp: metav1.NewTime(started),
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory),
			}},
		}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			StartTime:  &metav1.Time{Time: started},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(started)}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}},
			}},
		},
	}
}

// velero is what Velero holds in a cluster that backs up: a week of nightly backups and one
// taken by hand.
func (f *demoFleet) velero(d *demoCluster) []runtime.Object {
	if !slices.Contains(d.AddOns, "backups") {
		return nil
	}
	obj := func(kind, name string, labels map[string]string, body map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: body}
		u.SetAPIVersion("velero.io/v1")
		u.SetKind(kind)
		u.SetNamespace(provisioner.VeleroNamespace)
		u.SetName(name)
		u.SetLabels(labels)
		return u
	}
	nightly := time.Date(f.now.Year(), f.now.Month(), f.now.Day(), 3, 0, 0, 0, time.UTC)
	if nightly.After(f.now) {
		nightly = nightly.AddDate(0, 0, -1)
	}
	objects := []runtime.Object{
		obj("BackupStorageLocation", "default", nil, map[string]any{statusKey: map[string]any{phaseKey: "Available"}}),
		obj("Schedule", provisioner.VeleroSchedule, nil, map[string]any{
			"spec":    map[string]any{"schedule": "CRON_TZ=Europe/London 0 3 * * *"},
			statusKey: map[string]any{"lastBackup": nightly.Format(time.RFC3339), phaseKey: "Enabled"},
		}),
	}
	backup := func(name string, labels map[string]string, start time.Time, items int64) *unstructured.Unstructured {
		return obj("Backup", name, labels, map[string]any{
			"spec": map[string]any{"storageLocation": "default"},
			statusKey: map[string]any{
				phaseKey: "Completed", "startTimestamp": start.Format(time.RFC3339), "completionTimestamp": start.Add(3*time.Minute + 20*time.Second).Format(time.RFC3339),
				"expiration": start.AddDate(0, 0, 7).Format(time.RFC3339),
				"progress":   map[string]any{"itemsBackedUp": items, "totalItems": items},
			},
		})
	}
	for day := range 7 {
		at := nightly.AddDate(0, 0, -day)
		objects = append(objects, backup(provisioner.VeleroSchedule+"-"+at.Format("20060102150405"),
			map[string]string{"velero.io/schedule-name": provisioner.VeleroSchedule}, at, int64(1184+day*3)))
	}
	manual := backup("before-ingress-change", nil, f.now.Add(-2*24*time.Hour-5*time.Hour), 1179)
	manual.SetAnnotations(map[string]string{"kubehenge.dev/requested-by": demoUser})
	return append(objects, manual)
}

// connect hands out the workload cluster a server address belongs to. There is no other.
func (f *demoFleet) connect(server string, _ []byte, _ string) (*Workload, error) {
	w, ok := f.workloads[server]
	if !ok {
		return nil, fmt.Errorf("the demo has no cluster at %s", server)
	}
	return w, nil
}

// demoAccounts is Proxmox VE as the demo's administrator sees it: three nodes, their storage and
// each cluster's VMs. Anyone signing in is that administrator.
type demoAccounts struct {
	fleet *demoFleet
}

var demoPrivileges = pve.Privileges{
	"VM.Audit": true, "VM.Allocate": true, "VM.Console": true, "VM.PowerMgmt": true, "Sys.Modify": true, "Sys.Audit": true,
	"Datastore.AllocateSpace": true, "Datastore.AllocateTemplate": true, "SDN.Use": true, "Permissions.Modify": true,
	"User.Modify": true, "Group.Allocate": true, "Realm.AllocateUser": true, "Pool.Allocate": true,
}

// demoTicket is issued now, not at the demo's time: sessions end by the real clock.
func demoTicket(username string) *pve.Ticket {
	return &pve.Ticket{Username: username, Ticket: demoName, CSRF: demoName, Issued: time.Now()}
}

func (*demoAccounts) SignIn(_ context.Context, username, _ string) (*pve.Ticket, *pve.Challenge, error) {
	return demoTicket(username), nil, nil
}

func (*demoAccounts) TokenUser(context.Context, string) (*pve.Ticket, error) {
	return nil, pve.ErrAuthentication
}

func (*demoAccounts) Answer(context.Context, *pve.Challenge, string, string) (*pve.Ticket, error) {
	return nil, pve.ErrAuthentication
}

func (*demoAccounts) Renew(_ context.Context, t *pve.Ticket) (*pve.Ticket, error) {
	return demoTicket(t.Username), nil
}

func (*demoAccounts) Realms(context.Context) ([]pve.Realm, error) {
	return []pve.Realm{{Realm: "pve", Type: "pve", Comment: "Proxmox VE authentication server", Default: true}}, nil
}

func (*demoAccounts) OpenIDAuthURL(context.Context, string, string) (string, error) {
	return "", errDemo
}

func (*demoAccounts) OpenIDLogin(context.Context, string, string, string) (*pve.Ticket, error) {
	return nil, errDemo
}

func (*demoAccounts) Privileges(context.Context, *pve.Ticket, string) (pve.Privileges, error) {
	return demoPrivileges, nil
}

func (*demoAccounts) AllPrivileges(context.Context, *pve.Ticket) (map[string]pve.Privileges, error) {
	return map[string]pve.Privileges{"/": demoPrivileges}, nil
}

func (a *demoAccounts) Storages(context.Context, *pve.Ticket) ([]pve.Storage, error) {
	nodes := a.fleet.data.ProxmoxNodes
	list := make([]pve.Storage, 0, 2*len(nodes))
	for i, node := range nodes {
		list = append(list,
			pve.Storage{Storage: "local-lvm", Node: node, Content: []string{"images", "rootdir"}, Avail: int64(610+40*i) * demoGiB, Total: 900 * demoGiB},
			pve.Storage{Storage: "local", Node: node, Content: []string{"iso", "import", "vztmpl"}, Avail: 80 * demoGiB, Total: 100 * demoGiB},
		)
	}
	return list, nil
}

func (a *demoAccounts) Nodes(context.Context, *pve.Ticket) ([]pve.Node, error) {
	nodes := a.fleet.data.ProxmoxNodes
	list := make([]pve.Node, 0, len(nodes))
	for i, node := range nodes {
		list = append(list, pve.Node{Node: node, Online: true, Memory: int64(148+22*i) * demoGiB, MaxMemory: 384 * demoGiB})
	}
	return list, nil
}

func (*demoAccounts) Bridges(_ context.Context, _ *pve.Ticket, node string) ([]pve.Bridge, error) {
	return []pve.Bridge{{Bridge: "vmbr0", Node: node, CIDR: "10.20.0.2/16", Gateway: "10.20.0.1"}}, nil
}

func (*demoAccounts) DNS(context.Context, *pve.Ticket, string) ([]string, error) {
	return []string{"10.20.0.53"}, nil
}

func (*demoAccounts) AccessNames(context.Context, *pve.Ticket) (pve.Names, error) {
	return pve.Names{}, nil
}

func (*demoAccounts) NodeAddresses(context.Context, *pve.Ticket) (map[string]string, error) {
	return map[string]string{}, nil
}

func (*demoAccounts) NodeAffinityRules(context.Context, *pve.Ticket) ([]pve.HARule, error) {
	return nil, nil
}

func (*demoAccounts) Network(context.Context, *pve.Ticket, string) (pve.Network, error) {
	return pve.Network{Zone: pve.LocalZone}, nil
}

func (a *demoAccounts) PoolMembers(_ context.Context, _ *pve.Ticket, pool string) ([]pve.Member, error) {
	return a.fleet.members[pool], nil
}

// VMLoad is an hour of a VM's load, a little different for each VM.
func (a *demoAccounts) VMLoad(_ context.Context, _ *pve.Ticket, _ string, vmid int) ([]pve.LoadPoint, error) {
	const minutes = 60
	points := make([]pve.LoadPoint, 0, minutes)
	startSec := a.fleet.now.Add(-time.Hour).Unix()
	for i := range minutes {
		load := 0.1 + float64((vmid*31+i*17)%40)/200
		points = append(points, pve.LoadPoint{Time: startSec + int64(i*60), CPU: &load})
	}
	return points, nil
}

func (a *demoAccounts) Templates(context.Context, *pve.Ticket, string) ([]pve.Member, error) {
	return a.fleet.templates, nil
}

func (*demoAccounts) BackupCovers(context.Context, *pve.Ticket, int) (bool, error) { return true, nil }

func (*demoAccounts) DeleteVM(context.Context, *pve.Ticket, string, int) error { return errDemo }

func (*demoAccounts) RebootVM(context.Context, *pve.Ticket, string, int) error { return errDemo }

func (*demoAccounts) ReleaseHA(context.Context, *pve.Ticket, string) error { return errDemo }

func (*demoAccounts) CreateAdministratorToken(context.Context, *pve.Ticket, string, string, string) (string, error) {
	return "", errDemo
}

func (*demoAccounts) DeleteUser(context.Context, *pve.Ticket, string) error { return errDemo }
