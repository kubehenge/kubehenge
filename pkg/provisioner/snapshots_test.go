package provisioner

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

var testVolumeSnapshots = VolumeSnapshots{RootSecret: "kubehenge-root-pam", Namespace: "kubehenge-system"} //nolint:gosec // a Secret's name, not a credential

// snapshotting asks for volume snapshots.
var snapshotting = variableOf(csiVariable, `{"volumeSnapshots":true}`)

// rootPasswordSecret holds root@pam's password as the appliance stores it.
func rootPasswordSecret(password string) *corev1.Secret {
	return &corev1.Secret{
		Namespace: "kubehenge-system", Name: "kubehenge-root-pam",
		Data: map[string][]byte{"password": []byte(password)},
	}
}

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
		"no root password":      {VolumeSnapshots{}, storedCluster("team-a", "web", "local-zfs", snapshotting), class, "stored no root@pam password"},
		"shared storage":        {testVolumeSnapshots, storedCluster("team-a", "web", "ceph", snapshotting), class, "which storage ceph is not"},
		"no storages published": {testVolumeSnapshots, storedCluster("team-a", "web", "local-zfs", snapshotting), &clusterv1.ClusterClass{}, "which storage local-zfs is not"},
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
	h.VolumeSnapshots = testVolumeSnapshots
	snapshotStoragesOf(t, reader, "kubehenge-rke2", "local-zfs")
	validator := &ClusterValidator{Hooks: h}

	shared := storedCluster("team-a", "web", "ceph", snapshotting)
	if _, err := validator.ValidateCreate(context.Background(), shared); err == nil || !strings.Contains(err.Error(), "which storage ceph is not") {
		t.Errorf("the validator let volume snapshots onto shared storage: %v", err)
	}
	local := storedCluster("team-a", "web", "local-zfs", snapshotting)
	if _, err := validator.ValidateCreate(context.Background(), local); err == nil || !strings.Contains(err.Error(), "no root@pam password is stored") {
		t.Errorf("the validator let volume snapshots in without root@pam's password: %v", err)
	}
	created := networkCluster("192.168.1.1", 24)
	created.Spec.Topology.Variables = append(created.Spec.Topology.Variables, variableOf("storage", `"local-zfs"`), snapshotting)
	if resp := callBeforeClusterCreate(h, created); !strings.Contains(resp.Message, "no root@pam password is stored") {
		t.Errorf("BeforeClusterCreate let volume snapshots in without root@pam's password: %s %q", resp.Status, resp.Message)
	}

	if err := reader.Create(context.Background(), rootPasswordSecret("")); err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateCreate(context.Background(), local); err == nil || !strings.Contains(err.Error(), "holds no root@pam password") {
		t.Errorf("the validator took an empty password: %v", err)
	}
	stored := rootPasswordSecret("s3cret\n")
	if err := reader.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateUpdate(context.Background(), storedCluster("team-a", "web", "local-zfs"), local); err != nil {
		t.Errorf("volume snapshots refused on node-local storage with the password stored: %v", err)
	}

	// Switching volume snapshots off is never refused.
	off := storedCluster("team-a", "web", "ceph", variableOf(csiVariable, `{}`))
	if _, err := validator.ValidateUpdate(context.Background(), shared, off); err != nil {
		t.Errorf("switching volume snapshots off was refused: %v", err)
	}
}

func TestResourceSetSecretSignsTheCSIPluginInAsRoot(t *testing.T) {
	t.Parallel()
	creds := Credentials{TokenID: "kubehenge.team-a.web@pve!cluster", TokenSecret: "s3cret", VolumeOwner: 4242, Region: "lab"} //nolint:gosec // test data
	s, err := resourceSetSecret(cluster(), Endpoint{URL: "https://pve.example.org:8006", CA: "PEM"}, creds, "r00t")
	if err != nil {
		t.Fatal(err)
	}
	var token, root corev1.Secret
	if err := yaml.UnmarshalStrict(s.Data["kubehenge-proxmox.yaml"], &token); err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(s.Data["kubehenge-proxmox-root.yaml"], &root); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(root.StringData)); root.Namespace != "kube-system" || root.Name != "kubehenge-proxmox-root" || !slices.Equal(keys, []string{"csi.yaml"}) {
		t.Errorf("root secret %s/%s keys %v", root.Namespace, root.Name, keys)
	}
	var cfg proxmoxConfig
	if err := yaml.UnmarshalStrict([]byte(root.StringData["csi.yaml"]), &cfg); err != nil {
		t.Fatal(err)
	}
	want := proxmoxCluster{URL: "https://pve.example.org:8006/api2/json", Username: "root@pam", Password: "r00t", Region: "lab"}
	if cfg.Features.ControllerVMID != 4242 || len(cfg.Clusters) != 1 || cfg.Clusters[0] != want {
		t.Errorf("root configuration %+v", cfg)
	}
	// The plugin refuses a configuration with both a token and a password.
	if strings.Contains(root.StringData["csi.yaml"], "token") {
		t.Errorf("the root configuration names a token:\n%s", root.StringData["csi.yaml"])
	}
	for key, config := range token.StringData {
		if strings.Contains(config, "r00t") || strings.Contains(config, "root@pam") {
			t.Errorf("%s of the token's Secret holds root@pam's password", key)
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
	r, kube := newReconciler(t, &fakeCloud{}, web, rootPasswordSecret("r00t\r\n"))
	r.VolumeSnapshots = testVolumeSnapshots
	snapshotStoragesOf(t, kube, "kubehenge-k3s", "local,local-zfs")
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(web): {client: workload, until: time.Now().Add(time.Hour)}}}
	ctx := context.Background()

	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	wrapper := &corev1.Secret{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "web-proxmox"}, wrapper); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wrapper.Data["kubehenge-proxmox-root.yaml"]), "password: r00t\n") {
		t.Errorf("the resource set carries no root configuration, or the password with its line end:\n%s", wrapper.Data["kubehenge-proxmox-root.yaml"])
	}
	version, csi, _ := proxyOf(t, kube, "web-proxmox-csi")
	if !strings.HasPrefix(version, testCharts.CSINewest.Version+"@") {
		t.Errorf("the CSI plugin's chart is %s, want the newest release", version)
	}
	if lookup[string](t, csi, "existingConfigSecret") != "kubehenge-proxmox-root" || lookup[string](t, csi, "timeout") != "5m" ||
		!lookup[bool](t, csi, "controller", "snapshotter", "enabled") {
		t.Errorf("the CSI plugin's values do not take snapshots as root@pam: %v", csi)
	}
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

	root := &corev1.Secret{}
	root.Namespace, root.Name = "kube-system", "kubehenge-proxmox-root"
	if err := workload.Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	setVariable(t, kube, web, variableOf(csiVariable, `{}`))
	if err := reconcile(t, r, web); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(wrapper), wrapper); err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapper.Data["kubehenge-proxmox-root.yaml"]; ok {
		t.Error("the resource set still carries the root configuration")
	}
	if err := workload.Get(ctx, client.ObjectKeyFromObject(root), root); err == nil {
		t.Error("the workload cluster still holds the root configuration")
	}
	version, csi, _ = proxyOf(t, kube, "web-proxmox-csi")
	_, snapshots := lookup[map[string]any](t, csi, "controller", "snapshotter")["enabled"]
	if !strings.HasPrefix(version, testCharts.CSI.Version+"@") || lookup[string](t, csi, "existingConfigSecret") != "kubehenge-proxmox" || snapshots {
		t.Errorf("the CSI plugin did not go back to its token: %s %v", version, csi)
	}
	if _, has := csi["timeout"]; has {
		t.Errorf("the CSI plugin keeps the longer timeout: %v", csi["timeout"])
	}
	for _, name := range []string{"web-snapshot-class", "web-snapshot-controller"} {
		if _, _, ok := proxyOf(t, kube, name); ok {
			t.Errorf("%s is still there", name)
		}
	}
}

func TestReconcileTakesSnapshotsWithRKE2sOwnController(t *testing.T) {
	t.Parallel()
	web := storedCluster("team-a", "web", "local-zfs", snapshotting)
	r, kube := newReconciler(t, &fakeCloud{}, web, rootPasswordSecret("r00t"))
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
	r.VolumeSnapshots = testVolumeSnapshots
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
		if !strings.Contains(e, "VolumeSnapshotsRefused") || !strings.Contains(e, "no root@pam password is stored") {
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
