package provisioner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/csiproxy"
	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// proxiedWorkloadSecret holds, in each workload cluster that takes volume snapshots, the CSI
	// plugin's configuration that reaches Proxmox VE through the proxy beside it, with the cluster's
	// own token. It is a Secret apart from workloadSecret so that the plugin's controller mounts it
	// by name: switched on, a new pod waits until it is there, and switched off, no new pod reads
	// it, while the old configuration may still linger.
	proxiedWorkloadSecret = "kubehenge-proxmox-proxied" //nolint:gosec // a Secret's name, not a credential
	// rootPasswordKey holds the password in the management cluster's Secret.
	rootPasswordKey = "password"
	// rootUser is the one Proxmox VE user its volume copy method lets in.
	rootUser = "root@pam"

	// csiProxyAddress is where the CSI plugin's controller reaches the proxy beside it: on the
	// pod's own loopback, as the plugin's calls carry the cluster's token over plain HTTP.
	csiProxyAddress = "127.0.0.1:8006"
	// csiProxyCA is where the proxy reads the Proxmox VE CA.
	csiProxyCA = "/etc/kubehenge/proxmox/ca.crt"
	// csiProxyName names the proxy's container, and its Role and RoleBinding in csiproxy.Namespace.
	csiProxyName = "kubehenge-csi-proxy"
	// csiControllerAccount is the CSI plugin's controller's ServiceAccount in kube-system, as its
	// chart names it for the release proxmox-csi-plugin.
	csiControllerAccount = "proxmox-csi-plugin-controller"
	// kindNamespace is a Namespace's kind in a manifest.
	kindNamespace = "Namespace"

	// The two releases of the snapshot-controller chart: the CRDs and the controller, on the
	// distributions that bring none, and the CSI plugin's VolumeSnapshotClass, on every one.
	snapshotControllerRelease = "snapshot-controller"
	snapshotClassRelease      = "snapshot-class"
	// snapshotClass is the VolumeSnapshotClass, the cluster's default.
	snapshotClass = "proxmox"

	// csiCopyTimeout is the CSI plugin's sidecars' timeout with volume snapshots on, as long as the
	// plugin's own calls to Proxmox VE may take, so that a copy the proxy waits on is not cut short
	// by the chart's three minutes. A copy that takes longer runs on, and the sidecar's next try
	// waits on the same copy.
	csiCopyTimeout = "5m"
)

// SnapshotStoragesAnnotation lists on every ClusterClass, comma-separated, the storages the CSI
// plugin takes volume snapshots on: those local to each node, of a type whose volumes Proxmox VE
// copies.
const SnapshotStoragesAnnotation = "kubehenge.dev/snapshot-storages"

// snapshotStorageTypes are the Proxmox VE storage types whose volumes the CSI plugin copies for a
// snapshot, a restore or a clone.
var snapshotStorageTypes = []string{"zfspool", "lvm", "lvmthin", "dir", "btrfs"}

// VolumeSnapshots lets clusters take volume snapshots through the CSI plugin, which copies volumes
// through a Proxmox VE method only root@pam may call. root@pam's password stays in the management
// cluster: the plugin keeps the cluster's token, and the controller makes the copies it asks for
// through the proxy beside it (VolumeCopier).
type VolumeSnapshots struct {
	// RootSecret is the Secret in Namespace holding root@pam's password under the key password;
	// empty when Kubehenge's administrator lets no cluster take volume snapshots.
	RootSecret string
	Namespace  string
}

// copyProxy is what the proxy beside the CSI plugin's controller runs: its image, and the Proxmox
// VE API it passes the plugin's calls on to.
type copyProxy struct {
	image    string
	endpoint Endpoint
}

// ParseCSIProxyImage checks that the proxy's image is named by its digest, as
// <repository>@sha256:<digest>, the form the controller's own image is given in.
func ParseCSIProxyImage(s string) (string, error) {
	name, digest, ok := strings.Cut(s, "@sha256:")
	if !ok || name == "" || !strings.Contains(name, "/") || !isDigest(digest) {
		return "", fmt.Errorf("%q is not <repository>@sha256:<digest>", s)
	}
	return s, nil
}

// Refusal says why the cluster may not take volume snapshots as its csi variable asks: no cluster
// may, or its storage is not one the plugin takes them on (class's SnapshotStoragesAnnotation). It
// returns "" when nothing stands in the way, or when the cluster does not ask.
func (v VolumeSnapshots) Refusal(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) (string, error) {
	csi, err := csiOf(cluster)
	if err != nil || !csi.VolumeSnapshots {
		return "", err
	}
	if v.RootSecret == "" {
		return "Kubehenge's administrator has stored no root@pam password, with which Kubehenge copies the volumes volume snapshots take", nil
	}
	storage, err := variable[string](cluster, "storage")
	if err != nil {
		return "", err
	}
	if class == nil || !slices.Contains(splitList(class.Annotations[SnapshotStoragesAnnotation]), storage) {
		return fmt.Sprintf("the CSI plugin takes volume snapshots only on storage local to each node (ZFS, LVM, LVM-thin, directory or btrfs), which storage %s is not", storage), nil
	}
	return "", nil
}

// password reads root@pam's password, or says why there is none.
func (v VolumeSnapshots) password(ctx context.Context, c client.Reader) (string, string, error) {
	s := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: v.Namespace, Name: v.RootSecret}, s)
	if apierrors.IsNotFound(err) {
		return "", fmt.Sprintf("no root@pam password is stored in Secret %s/%s", v.Namespace, v.RootSecret), nil
	}
	if err != nil {
		return "", "", fmt.Errorf("reading root@pam's password: %w", err)
	}
	// A password typed into kubectl or a file ends with the line.
	password := strings.TrimRight(string(s.Data[rootPasswordKey]), "\r\n")
	if password == "" {
		return "", fmt.Sprintf("Secret %s/%s holds no root@pam password under %s", v.Namespace, v.RootSecret, rootPasswordKey), nil
	}
	return password, "", nil
}

// refusalNow says why the cluster may not take volume snapshots now, also when root@pam's
// password, with which its copies are made, is not stored; "" when nothing stands in the way, or
// when the cluster does not ask.
func (v VolumeSnapshots) refusalNow(ctx context.Context, c client.Reader, classNamespace string, cluster *clusterv1.Cluster) (string, error) {
	csi, err := csiOf(cluster)
	if err != nil || !csi.VolumeSnapshots {
		return "", err
	}
	class := &clusterv1.ClusterClass{}
	if err = c.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: cluster.Spec.Topology.ClassRef.Name}, class); err != nil {
		return "", fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	refused, err := v.Refusal(cluster, class)
	if err != nil || refused != "" {
		return refused, err
	}
	_, refused, err = v.password(ctx, c)
	return refused, err
}

// volumeSnapshotsOn reports whether the cluster's csi variable asks for volume snapshots and nothing
// stands in the way. When something does, it says why on the Cluster, and the plugin goes without.
func (r *Reconciler) volumeSnapshotsOn(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	csi, err := csiOf(cluster)
	if err != nil || !csi.VolumeSnapshots {
		return false, err
	}
	refused, err := r.VolumeSnapshots.refusalNow(ctx, r.Client, r.ClassNamespace, cluster)
	if err != nil {
		return false, err
	}
	if refused != "" {
		r.refuse(ctx, cluster, "VolumeSnapshots", refused)
	}
	return refused == "", nil
}

// checkVolumeSnapshots refuses volume snapshots for a cluster that cannot take them.
func (h *Hooks) checkVolumeSnapshots(ctx context.Context, cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	refused, err := h.VolumeSnapshots.Refusal(cluster, class)
	if err != nil {
		return err
	}
	if refused == "" {
		csi, err := csiOf(cluster)
		if err != nil || !csi.VolumeSnapshots {
			return err
		}
		if _, refused, err = h.VolumeSnapshots.password(ctx, h.Reader); err != nil {
			return err
		}
	}
	if refused != "" {
		return errors.New(refused)
	}
	return nil
}

// snapshotCharts are the cluster's releases of the snapshot-controller chart: the CRDs and the
// controller where the distribution brings none (RKE2 runs its own, and two would contend for the
// same objects), and the plugin's VolumeSnapshotClass. The CRDs stay when the controller goes:
// deleting them would take every VolumeSnapshot left with them, or hang on finalizers that no
// controller clears any more.
func snapshotCharts(charts *Charts, distribution templates.Distribution) []chartProxy {
	var proxies []chartProxy
	if distribution != templates.RKE2 {
		proxies = append(proxies, chartProxy{snapshotControllerRelease, snapshotControllerRelease, metav1.NamespaceSystem, charts.SnapshotController, map[string]any{
			installCRDsKey: true,
			"keepCRDs":     true,
			controllerKey: map[string]any{
				// Beside the CSI plugin's controller, so that a cluster without workers takes
				// snapshots too.
				tolerationsKey: []any{
					toleration(controlPlaneTaint.Key, controlPlaneTaint.Effect),
					toleration("node-role.kubernetes.io/etcd", corev1.TaintEffectNoExecute),
				},
				"resources":    map[string]any{requestsKey: map[string]any{cpuKey: cpu10m, memoryKey: "32Mi"}},
				podSecurityKey: map[string]any{nonRootKey: true, seccompProfileKey: runtimeDefault()},
				securityKey:    restrictedContainer(map[string]any{readOnlyRootKey: true}),
			},
		}})
	}
	return append(proxies, chartProxy{snapshotClassRelease, snapshotClassRelease, metav1.NamespaceSystem, charts.SnapshotController, map[string]any{
		installCRDsKey: false,
		controllerKey:  map[string]any{"enabled": false},
		"volumeSnapshotClasses": []any{map[string]any{
			"name":           snapshotClass,
			annotationsKey:   map[string]any{"snapshot.storage.kubernetes.io/is-default-class": strconv.FormatBool(true)},
			"driver":         csiDriver,
			"deletionPolicy": "Delete",
		}},
	}})
}

func snapshotControllerProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-" + snapshotControllerRelease
}

func snapshotClassProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-" + snapshotClassRelease
}

// snapshotClassWaits holds the VolumeSnapshotClass back until the snapshot controller's release,
// with the CRDs, is deployed: Helm cannot install a release of kinds the API server does not serve
// yet. RKE2's are there from the start.
func (r *Reconciler) snapshotClassWaits(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	if distributionOf(cluster) == templates.RKE2 {
		return false, nil
	}
	ready, err := r.proxyReady(ctx, cluster, snapshotControllerProxy(cluster))
	return !ready, err
}

// removeVolumeSnapshots takes volume snapshots out of a cluster that switched them off: the
// proxied configuration and the namespace of the copy requests from the workload cluster, as the
// ClusterResourceSet never deletes what it applied, then the VolumeSnapshotClass, and the
// controller once the class's release is gone, as that cannot be uninstalled once its kind is no
// longer served. The HelmChartProxies go last, so that while one is there the rest is looked for
// again. It reports whether a removal waits on another.
func (r *Reconciler) removeVolumeSnapshots(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	class, controller := snapshotClassProxy(cluster), snapshotControllerProxy(cluster)
	on, err := r.anyProxy(ctx, cluster, class, controller)
	if err != nil || !on {
		return false, err
	}
	if err = r.removeCopyProxy(ctx, cluster); err != nil {
		return false, err
	}
	if err = r.removeProxies(ctx, cluster, class); err != nil {
		return false, err
	}
	waiting, err := r.anyProxy(ctx, cluster, class)
	if err != nil || waiting {
		return waiting, err
	}
	return false, r.removeProxies(ctx, cluster, controller)
}

// removeCopyProxy deletes the CSI plugin's proxied configuration from the workload cluster, and the
// namespace of its copy requests with the proxy's rights there.
func (r *Reconciler) removeCopyProxy(ctx context.Context, cluster *clusterv1.Cluster) error {
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		// Without a kubeconfig, nothing was ever applied to the cluster.
		return err
	}
	for _, o := range []client.Object{
		&corev1.Secret{Namespace: metav1.NamespaceSystem, Name: proxiedWorkloadSecret},
		&corev1.Namespace{Name: csiproxy.Namespace},
	} {
		if err := client.IgnoreNotFound(workload.client.Delete(ctx, o)); err != nil {
			r.Remotes.forget(cluster)
			return fmt.Errorf("deleting the CSI plugin's proxy from the cluster: %w", err)
		}
	}
	return nil
}

// copyProxyManifests are, for the ClusterResourceSet, the CSI plugin's configuration that reaches
// Proxmox VE through the proxy, and the namespace of its copy requests, where the plugin's
// controller, and with it the proxy, may write requests and read them back.
func copyProxyManifests(c Credentials) (map[string][]byte, error) {
	csi, err := csiConfig(c, &proxmoxCluster{URL: "http://" + csiProxyAddress + "/api2/json", TokenID: c.TokenID, TokenSecret: c.TokenSecret, Region: c.Region})
	if err != nil {
		return nil, err
	}
	config, err := workloadManifest(proxiedWorkloadSecret, map[string]string{csiConfigKey: csi})
	if err != nil {
		return nil, err
	}
	var requests []byte
	for _, o := range []any{
		&corev1.Namespace{APIVersion: "v1", Kind: kindNamespace, Name: csiproxy.Namespace},
		&rbacv1.Role{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role", Namespace: csiproxy.Namespace, Name: csiProxyName,
			Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "create", "delete"}}},
		},
		&rbacv1.RoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding", Namespace: csiproxy.Namespace, Name: csiProxyName,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: csiProxyName},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: metav1.NamespaceSystem, Name: csiControllerAccount}},
		},
	} {
		manifest, err := yaml.Marshal(o)
		if err != nil {
			return nil, fmt.Errorf("encoding the copy requests' namespace: %w", err)
		}
		requests = append(append(requests, "---\n"...), manifest...)
	}
	return map[string][]byte{proxiedWorkloadSecret + ".yaml": config, csiproxy.Namespace + ".yaml": requests}, nil
}

// csiProxyContainer is the proxy beside the CSI plugin's controller, as a native sidecar, which
// starts before the plugin and runs as long as it does.
func csiProxyContainer(image string, endpoint Endpoint) map[string]any {
	return map[string]any{
		"name":          csiProxyName,
		"image":         image,
		"restartPolicy": string(corev1.ContainerRestartPolicyAlways),
		"args": []any{
			"csi-proxy", "--listen=" + csiProxyAddress, "--proxmox-url=" + endpoint.URL, "--proxmox-ca=" + csiProxyCA,
			"--proxmox-insecure=" + strconv.FormatBool(endpoint.Insecure),
		},
		"resources":    map[string]any{requestsKey: map[string]any{cpuKey: cpu10m, memoryKey: "32Mi"}},
		securityKey:    restrictedContainer(map[string]any{readOnlyRootKey: true}),
		"volumeMounts": []any{map[string]any{"name": proxmoxCAVolume, "mountPath": csiProxyCA, "subPath": proxmoxCAKey, "readOnly": true}},
	}
}

// storageResource is a storage as Proxmox VE's resource list shows it, once per node.
type storageResource struct {
	Type       string `json:"type"`
	Storage    string `json:"storage"`
	PluginType string `json:"plugintype"`
	Shared     int    `json:"shared"`
}

// snapshotStorages are the storages the token sees that the CSI plugin takes volume snapshots on,
// sorted. A storage of the same name that is shared, or of another type, on any node rules it out.
func snapshotStorages(ctx context.Context, c *proxmox.Client) ([]string, error) {
	var resources []storageResource
	if err := c.Get(ctx, "/cluster/resources?type=storage", &resources); err != nil {
		return nil, fmt.Errorf("listing storages: %w", err)
	}
	var out, ruledOut []string
	for _, s := range resources {
		if s.Type != "storage" || slices.Contains(ruledOut, s.Storage) {
			continue
		}
		if s.Shared != 0 || !slices.Contains(snapshotStorageTypes, s.PluginType) {
			ruledOut = append(ruledOut, s.Storage)
			out = slices.DeleteFunc(out, func(name string) bool { return name == s.Storage })
			continue
		}
		if !slices.Contains(out, s.Storage) {
			out = append(out, s.Storage)
		}
	}
	slices.Sort(out)
	return out, nil
}
