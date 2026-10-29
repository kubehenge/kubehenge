package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const testCSIProxy = "ghcr.io/kubehenge/kubehenge@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var testVolumeSnapshots = VolumeSnapshots{ProxyImage: testCSIProxy}

// snapshotting asks for volume snapshots.
var snapshotting = variableOf(csiVariable, `{"volumeSnapshots":true}`)

// snapshotStoragesOf publishes the storages one of the kubehenge-system ClusterClasses takes
// volume snapshots on.
func snapshotStoragesOf(t *testing.T, kube client.Client, name, storages string) {
	t.Helper()
	class := &clusterv1.ClusterClass{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "kubehenge-system", Name: name}, class); err != nil {
		t.Fatal(err)
	}
	if class.Annotations == nil {
		class.Annotations = map[string]string{}
	}
	class.Annotations[SnapshotStoragesAnnotation] = storages
	if err := kube.Update(context.Background(), class); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeSnapshotsRefusal(t *testing.T) {
	t.Parallel()
	class := &clusterv1.ClusterClass{Name: "kubehenge-rke2", Namespace: "kubehenge-system"}
	class.Annotations = map[string]string{SnapshotStoragesAnnotation: "local,local-zfs"}
	for name, tc := range map[string]struct {
		v       VolumeSnapshots
		cluster *clusterv1.Cluster
		class   *clusterv1.ClusterClass
		want    string
	}{
		"on node-local storage": {testVolumeSnapshots, storedCluster("team-a", "web", "local-zfs", snapshotting), class, ""},
		"not asking":            {VolumeSnapshots{}, storedCluster("team-a", "web", "ceph"), class, ""},
		"asking for nothing":    {VolumeSnapshots{}, storedCluster("team-a", "web", "ceph", variableOf(csiVariable, `{"volumeSnapshots":false}`)), class, ""},
		"no proxy image":        {VolumeSnapshots{}, storedCluster("team-a", "web", "local-zfs", snapshotting), class, "copy proxy's image"},
		"shared storage":        {testVolumeSnapshots, storedCluster("team-a", "web", "ceph", snapshotting), class, "not ceph"},
		"no storages published": {testVolumeSnapshots, storedCluster("team-a", "web", "local-zfs", snapshotting), &clusterv1.ClusterClass{}, "not local-zfs"},
	} {
		got, err := tc.v.Refusal(tc.cluster, tc.class)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: refused with %q, want %q", name, got, tc.want)
		}
	}
}

func TestVolumeSnapshotsRefusedAtAdmissionAndCreation(t *testing.T) {
	t.Parallel()
	h, reader := hooksFor(t)
	snapshotStoragesOf(t, reader, "kubehenge-rke2", "local-zfs")
	validator := &ClusterValidator{Hooks: h}

	local := storedCluster("team-a", "web", "local-zfs", snapshotting)
	if _, err := validator.ValidateCreate(context.Background(), local); err == nil || !strings.Contains(err.Error(), "copy proxy's image") {
		t.Errorf("the validator let volume snapshots in without the proxy: %v", err)
	}
	created := networkCluster("192.168.1.1", 24)
	created.Spec.Topology.Variables = append(created.Spec.Topology.Variables, variableOf("storage", `"local-zfs"`), snapshotting)
	if resp := callBeforeClusterCreate(t, h, created); !strings.Contains(resp.Message, "copy proxy's image") {
		t.Errorf("BeforeClusterCreate let volume snapshots in without the proxy: %s %q", resp.Status, resp.Message)
	}

	h.VolumeSnapshots = testVolumeSnapshots
	shared := storedCluster("team-a", "web", "ceph", snapshotting)
	if _, err := validator.ValidateCreate(context.Background(), shared); err == nil || !strings.Contains(err.Error(), "not ceph") {
		t.Errorf("the validator let volume snapshots onto shared storage: %v", err)
	}
	if _, err := validator.ValidateUpdate(context.Background(), storedCluster("team-a", "web", "local-zfs"), local); err != nil {
		t.Errorf("volume snapshots refused on node-local storage: %v", err)
	}

	// Switching volume snapshots off is never refused.
	off := storedCluster("team-a", "web", "ceph", variableOf(csiVariable, `{}`))
	if _, err := validator.ValidateUpdate(context.Background(), shared, off); err != nil {
		t.Errorf("switching volume snapshots off was refused: %v", err)
	}
}

func TestResourceSetSecretProxiesTheCSIPlugin(t *testing.T) {
	t.Parallel()
	creds := Credentials{TokenID: "kubehenge.team-a.web@pve!cluster", TokenSecret: "s3cret", VolumeOwner: 4242, Region: "lab"} //nolint:gosec // test data
	s, err := resourceSetSecret(cluster(), Endpoint{URL: "https://pve.example.org:8006", CA: "PEM"}, creds, &Addons{VolumeSnapshots: true, copyProxy: copyProxy{on: true, copies: true}})
	if err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(s.Data)); !slices.Equal(keys, []string{"kubehenge-proxmox-proxied.yaml", "kubehenge-proxmox.yaml", "kubehenge-volume-copies.yaml"}) {
		t.Fatalf("the resource set's manifests %v", keys)
	}
	var proxied corev1.Secret
	if err := yaml.UnmarshalStrict(s.Data["kubehenge-proxmox-proxied.yaml"], &proxied); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(proxied.StringData)); proxied.Namespace != "kube-system" || proxied.Name != "kubehenge-proxmox-proxied" || !slices.Equal(keys, []string{"csi.yaml"}) {
		t.Errorf("proxied secret %s/%s keys %v", proxied.Namespace, proxied.Name, keys)
	}
	var cfg proxmoxConfig
	if err := yaml.UnmarshalStrict([]byte(proxied.StringData["csi.yaml"]), &cfg); err != nil {
		t.Fatal(err)
	}
	want := proxmoxCluster{URL: "http://127.0.0.1:8006/api2/json", TokenID: creds.TokenID, TokenSecret: creds.TokenSecret, Region: "lab"}
	if cfg.Features.ControllerVMID != 4242 || len(cfg.Clusters) != 1 || cfg.Clusters[0] != want {
		t.Errorf("proxied configuration %+v", cfg)
	}

	docs := strings.Split(strings.TrimPrefix(string(s.Data["kubehenge-volume-copies.yaml"]), "---\n"), "---\n")
	if len(docs) != 5 {
		t.Fatalf("the proxy's manifests are %d objects, want the namespace and two Roles with their bindings:\n%s", len(docs), s.Data["kubehenge-volume-copies.yaml"])
	}
	var ns corev1.Namespace
	var records, eventsRole rbacv1.Role
	var recordsBinding, eventsBinding rbacv1.RoleBinding
	for i, into := range []any{&ns, &records, &recordsBinding, &eventsRole, &eventsBinding} {
		if err := yaml.UnmarshalStrict([]byte(docs[i]), into); err != nil {
			t.Fatal(err)
		}
	}
	if ns.Kind != "Namespace" || ns.Name != "kubehenge-volume-copies" {
		t.Errorf("namespace %s %s", ns.Kind, ns.Name)
	}
	subject := rbacv1.Subject{Kind: "ServiceAccount", Namespace: "kube-system", Name: "proxmox-csi-plugin-controller"}
	for _, tc := range []struct {
		role    rbacv1.Role
		binding rbacv1.RoleBinding
		at      string
		rule    rbacv1.PolicyRule
	}{
		{records, recordsBinding, "kubehenge-volume-copies", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "create", "update", "delete"}}},
		{eventsRole, eventsBinding, "kube-system", rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create"}}},
	} {
		if tc.role.Kind != "Role" || tc.role.Namespace != tc.at || len(tc.role.Rules) != 1 || !reflect.DeepEqual(tc.role.Rules[0], tc.rule) {
			t.Errorf("the proxy's Role in %s %+v", tc.at, tc.role)
		}
		if tc.binding.Kind != "RoleBinding" || tc.binding.Namespace != tc.at || tc.binding.RoleRef.Name != tc.role.Name ||
			len(tc.binding.Subjects) != 1 || tc.binding.Subjects[0] != subject {
			t.Errorf("the proxy's RoleBinding in %s %+v", tc.at, tc.binding)
		}
	}
	for key, manifest := range s.Data {
		if strings.Contains(string(manifest), "root@pam") || strings.Contains(string(manifest), "password") {
			t.Errorf("%s names root@pam or a password:\n%s", key, manifest)
		}
	}
}

// proxyOf reads one of web's HelmChartProxies, its chart version and values.
func proxyOf(t *testing.T, kube client.Client, name string) (string, map[string]any, bool) {
	t.Helper()
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, hcp); err != nil {
		return "", nil, false
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(lookup[string](t, hcp.Object, "spec", "valuesTemplate")), &values); err != nil {
		t.Fatal(err)
	}
	return lookup[string](t, hcp.Object, "spec", "version"), values, true
}

// setProxyReady marks one of web's HelmChartProxies Ready for its current generation.
func setProxyReady(t *testing.T, kube client.Client, name string) {
	t.Helper()
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, hcp); err != nil {
		t.Fatal(err)
	}
	hcp.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "True", "observedGeneration": hcp.GetGeneration()},
	}}
	if err := kube.Update(context.Background(), hcp); err != nil {
		t.Fatal(err)
	}
}

func setVariable(t *testing.T, kube client.Client, c *clusterv1.Cluster, v clusterv1.ClusterVariable) *clusterv1.Cluster {
	t.Helper()
	current := &clusterv1.Cluster{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(c), current); err != nil {
		t.Fatal(err)
	}
	vars := current.Spec.Topology.Variables
	i := slices.IndexFunc(vars, func(cv clusterv1.ClusterVariable) bool { return cv.Name == v.Name })
	if i < 0 {
		vars = append(vars, v)
	} else {
		vars[i] = v
	}
	current.Spec.Topology.Variables = vars
	if err := kube.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	return current
}

func TestReconcileSwitchesVolumeSnapshotsOnAndOff(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "local-zfs", snapshotting)
	web.Spec.Topology.ClassRef.Name = "kubehenge-k3s"
	web.Annotations = map[string]string{OnlineNodesAnnotation: "pve1,pve2"}
	cloud := &fakeCloud{}
	r, kube := newReconciler(t, cloud, web)
	r.VolumeSnapshots = testVolumeSnapshots
	snapshotStoragesOf(t, kube, "kubehenge-k3s", "local,local-zfs")
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(web): {client: workload, until: time.Now().Add(time.Hour)}}}
	ctx := context.Background()

	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cloud.holderNodes, []string{"pve1", "pve2"}) {
		t.Errorf("holder VMs asked on %v, want every node the cluster's volumes may be on", cloud.holderNodes)
	}
	wrapper := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "web-proxmox"}, wrapper); err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapper.Data["kubehenge-proxmox-proxied.yaml"]; !ok {
		t.Errorf("the resource set carries no proxied configuration: %v", slices.Sorted(maps.Keys(wrapper.Data)))
	}
	checkProxiedCSI(t, kube, true)
	_, controller, ok := proxyOf(t, kube, "web-snapshot-controller")
	if !ok || !lookup[bool](t, controller, "installCRDs") || !lookup[bool](t, controller, "keepCRDs") {
		t.Errorf("no snapshot controller with its CRDs: %v", controller)
	}
	if _, _, early := proxyOf(t, kube, "web-snapshot-class"); early {
		t.Error("the VolumeSnapshotClass did not wait for the CRDs")
	}

	setProxyReady(t, kube, "web-snapshot-controller")
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	_, class, ok := proxyOf(t, kube, "web-snapshot-class")
	if !ok || lookup[bool](t, class, "installCRDs") || lookup[bool](t, class, "controller", "enabled") ||
		lookup[string](t, class, "volumeSnapshotClasses", 0, "driver") != csiDriver ||
		lookup[string](t, class, "volumeSnapshotClasses", 0, "deletionPolicy") != "Delete" {
		t.Errorf("the VolumeSnapshotClass: %v", class)
	}

	// What the resource set applied, and a copy a restored volume still uses.
	applied := []client.Object{
		&corev1.Secret{Namespace: "kube-system", Name: "kubehenge-proxmox-proxied"},
		&rbacv1.Role{Namespace: "kube-system", Name: "kubehenge-csi-proxy"},
		&rbacv1.RoleBinding{Namespace: "kube-system", Name: "kubehenge-csi-proxy"},
		&corev1.Namespace{Name: "kubehenge-volume-copies"},
	}
	record := &corev1.ConfigMap{Namespace: "kubehenge-volume-copies", Name: "copy-1", Labels: map[string]string{"kubehenge.dev/volume-copy": ""}}
	for _, o := range append(slices.Clone(applied), record) {
		if err := workload.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	setVariable(t, kube, web, variableOf(csiVariable, `{}`))
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(wrapper), wrapper); err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapper.Data["kubehenge-proxmox-proxied.yaml"]; !ok {
		t.Errorf("the proxy went while a copy is left, whose name only it knows: %v", slices.Sorted(maps.Keys(wrapper.Data)))
	}
	checkProxiedCSI(t, kube, false)
	if cloud.emptied != 0 {
		t.Error("holder VMs removed while a copy is left")
	}

	if err := workload.Delete(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(wrapper), wrapper); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(wrapper.Data)); !slices.Equal(keys, []string{"kubehenge-proxmox.yaml"}) {
		t.Errorf("the resource set still carries the proxy's manifests: %v", keys)
	}
	for _, o := range applied {
		if err := workload.Get(ctx, client.ObjectKeyFromObject(o), o); err == nil {
			t.Errorf("the workload cluster still holds %T %s", o, o.GetName())
		}
	}
	if cloud.emptied != 1 {
		t.Errorf("empty holder VMs removed %d times, want once", cloud.emptied)
	}
	checkDirectCSI(t, kube)
	for _, name := range []string{"web-snapshot-class", "web-snapshot-controller"} {
		if _, _, ok := proxyOf(t, kube, name); ok {
			t.Errorf("%s is still there", name)
		}
	}
}

// checkProxiedCSI checks that web's CSI plugin is its newest release reaching Proxmox VE through
// the proxy beside it, which makes copies, with the snapshot sidecar, or only finds those made
// before, and that its values name no root@pam.
func checkProxiedCSI(t *testing.T, kube client.Client, copies bool) { //nolint:revive // flag-parameter: copies is the state the check expects, not a mode
	t.Helper()
	version, csi, _ := proxyOf(t, kube, "web-proxmox-csi")
	if !strings.HasPrefix(version, testCharts.CSINewest.Version+"@") {
		t.Errorf("the CSI plugin's chart is %s, want the newest release", version)
	}
	_, snapshotter := lookup[map[string]any](t, csi, "controller", "snapshotter")["enabled"]
	if lookup[string](t, csi, "existingConfigSecret") != "kubehenge-proxmox-proxied" || lookup[string](t, csi, "timeout") != "5m" ||
		snapshotter != copies {
		t.Errorf("the CSI plugin's values do not reach Proxmox VE through the proxy: %v", csi)
	}
	names, err := NamesFor("kubehenge", "team-a", "web")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"csi-proxy", "--listen=127.0.0.1:8006", "--proxmox-url=https://pve.example.org:8006", "--proxmox-ca=/etc/kubehenge/proxmox/ca.crt",
		"--proxmox-insecure=false", "--volume-owner=4242", "--pool=" + names.Pool, "--holders=" + names.VolumeHolders(),
		"--copies=" + strconv.FormatBool(copies),
	}
	given := lookup[[]any](t, csi, "initContainers", 0, "args")
	args := make([]string, 0, len(given))
	for _, a := range given {
		args = append(args, fmt.Sprint(a))
	}
	if lookup[string](t, csi, "initContainers", 0, "image") != testCSIProxy ||
		lookup[string](t, csi, "initContainers", 0, "restartPolicy") != "Always" || !slices.Equal(args, want) ||
		lookup[string](t, csi, "initContainers", 0, "env", 0, "valueFrom", "fieldRef", "fieldPath") != "metadata.name" ||
		lookup[string](t, csi, "initContainers", 0, "env", 1, "valueFrom", "fieldRef", "fieldPath") != "metadata.namespace" {
		t.Errorf("the proxy beside the CSI plugin: %v", csi["initContainers"])
	}
	if raw, err := json.Marshal(csi); err != nil || strings.Contains(string(raw), "root@pam") || strings.Contains(string(raw), "password") {
		t.Errorf("the CSI plugin's values name root@pam or a password: %s %v", raw, err)
	}
}

// checkDirectCSI checks that web's CSI plugin is its held release again, reaching Proxmox VE
// directly with the cluster's token, without the snapshot sidecar, the proxy or the longer timeout.
func checkDirectCSI(t *testing.T, kube client.Client) {
	t.Helper()
	version, csi, _ := proxyOf(t, kube, "web-proxmox-csi")
	_, snapshots := lookup[map[string]any](t, csi, "controller", "snapshotter")["enabled"]
	if !strings.HasPrefix(version, testCharts.CSI.Version+"@") || lookup[string](t, csi, "existingConfigSecret") != "kubehenge-proxmox" || snapshots {
		t.Errorf("the CSI plugin did not go back to its token: %s %v", version, csi)
	}
	for _, key := range []string{"timeout", "initContainers"} {
		if _, has := csi[key]; has {
			t.Errorf("the CSI plugin keeps its %s: %v", key, csi[key])
		}
	}
}

func TestReconcileTakesSnapshotsWithRKE2sOwnController(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "local-zfs", snapshotting)
	r, kube := newReconciler(t, &fakeCloud{}, web)
	r.VolumeSnapshots = testVolumeSnapshots
	snapshotStoragesOf(t, kube, "kubehenge-rke2", "local-zfs")
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := proxyOf(t, kube, "web-snapshot-controller"); ok {
		t.Error("a second snapshot controller beside RKE2's")
	}
	if _, _, ok := proxyOf(t, kube, "web-snapshot-class"); !ok {
		t.Error("no VolumeSnapshotClass")
	}
}

func TestReconcileKeepsTheTokenWhenSnapshotsAreRefused(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "local-zfs", snapshotting)
	r, kube := newReconciler(t, &fakeCloud{}, web)
	snapshotStoragesOf(t, kube, "kubehenge-rke2", "local-zfs")
	recorder := events.NewFakeRecorder(10)
	r.Events = recorder
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	wrapper := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-proxmox"}, wrapper); err != nil {
		t.Fatal(err)
	}
	if len(wrapper.Data) != 1 {
		t.Errorf("the resource set has %d Secrets, want the token's alone", len(wrapper.Data))
	}
	if _, csi, _ := proxyOf(t, kube, "web-proxmox-csi"); lookup[string](t, csi, "existingConfigSecret") != "kubehenge-proxmox" {
		t.Errorf("the CSI plugin reads %v", csi["existingConfigSecret"])
	}
	select {
	case e := <-recorder.Events:
		if !strings.Contains(e, "VolumeSnapshotsRefused") || !strings.Contains(e, "copy proxy's image") {
			t.Errorf("event %q", e)
		}
	default:
		t.Error("no event says why")
	}
}

func TestSnapshotChartsLeaveRKE2sControllerAlone(t *testing.T) {
	t.Parallel()
	for distribution, want := range map[templates.Distribution][]string{
		templates.Kubeadm: {"snapshot-controller", "snapshot-class"},
		templates.K3s:     {"snapshot-controller", "snapshot-class"},
		templates.Talos:   {"snapshot-controller", "snapshot-class"},
		templates.RKE2:    {"snapshot-class"},
	} {
		var got []string
		for _, p := range snapshotCharts(&testCharts, distribution) {
			got = append(got, p.release)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", distribution, got, want)
		}
	}
}

func TestSnapshotStorages(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/resources" || r.URL.Query().Get("type") != "storage" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"type": "storage", "storage": "local-zfs", "node": "pve1", "plugintype": "zfspool", "shared": 0},
			map[string]any{"type": "storage", "storage": "local-zfs", "node": "pve2", "plugintype": "zfspool", "shared": 0},
			map[string]any{"type": "storage", "storage": "local", "node": "pve1", "plugintype": "dir", "shared": 0},
			map[string]any{"type": "storage", "storage": "thin", "node": "pve1", "plugintype": "lvmthin", "shared": 0},
			map[string]any{"type": "storage", "storage": "ceph", "node": "pve1", "plugintype": "rbd", "shared": 1},
			map[string]any{"type": "storage", "storage": "backup", "node": "pve1", "plugintype": "pbs", "shared": 1},
			map[string]any{"type": "storage", "storage": "smb", "node": "pve1", "plugintype": "cifs", "shared": 1},
			// A directory one node shares with the rest is not local to each.
			map[string]any{"type": "storage", "storage": "nfs-dir", "node": "pve1", "plugintype": "dir", "shared": 0},
			map[string]any{"type": "storage", "storage": "nfs-dir", "node": "pve2", "plugintype": "dir", "shared": 1},
		}})
	}))
	t.Cleanup(srv.Close)
	got, err := snapshotStorages(context.Background(), proxmox.NewClient(srv.URL+"/api2/json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"local", "local-zfs", "thin"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
