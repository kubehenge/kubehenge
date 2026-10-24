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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// rootWorkloadSecret holds, in each workload cluster that takes volume snapshots, the CSI
	// plugin's configuration with root@pam's password. It is a Secret apart from workloadSecret so
	// that the plugin's controller mounts it by name: switched on, a new pod waits until it is
	// there, and switched off, no new pod reads it, while the old configuration may still linger.
	rootWorkloadSecret = "kubehenge-proxmox-root" //nolint:gosec // a Secret's name, not a credential
	// rootPasswordKey holds the password in the management cluster's Secret.
	rootPasswordKey = "password"
	// rootUser is the one Proxmox VE user its volume copy method lets in.
	rootUser = "root@pam"

	// The two releases of the snapshot-controller chart: the CRDs and the controller, on the
	// distributions that bring none, and the CSI plugin's VolumeSnapshotClass, on every one.
	snapshotControllerRelease = "snapshot-controller"
	snapshotClassRelease      = "snapshot-class"
	// snapshotClass is the VolumeSnapshotClass, the cluster's default.
	snapshotClass = "proxmox"

	// csiCopyTimeout is the CSI plugin's sidecars' timeout with volume snapshots on. The plugin
	// waits up to four minutes for a volume copy, cut short by the call's deadline; the chart's
	// three minutes would end the call first, and its retry would find the target listed and take
	// it as complete while the copy still writes it. Above four minutes, the plugin's own wait ends
	// first and the call fails instead.
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
// through a Proxmox VE method only root@pam may call, so that the plugin signs in as root@pam.
type VolumeSnapshots struct {
	// RootSecret is the Secret in Namespace holding root@pam's password under the key password;
	// empty when Kubehenge's administrator lets no cluster take volume snapshots.
	RootSecret string
	Namespace  string
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
		return "Kubehenge's administrator has stored no root@pam password, which the CSI plugin's volume snapshots need", nil
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

// rootPassword is root@pam's password when the cluster's csi variable asks for volume snapshots
// and nothing stands in the way, else "". When something does, it says why on the Cluster, and the
// plugin keeps the cluster's own token.
func (r *Reconciler) rootPassword(ctx context.Context, cluster *clusterv1.Cluster) (string, error) {
	csi, err := csiOf(cluster)
	if err != nil || !csi.VolumeSnapshots {
		return "", err
	}
	class := &clusterv1.ClusterClass{}
	if err = r.Client.Get(ctx, client.ObjectKey{Namespace: r.ClassNamespace, Name: cluster.Spec.Topology.ClassRef.Name}, class); err != nil {
		return "", fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	refused, err := r.VolumeSnapshots.Refusal(cluster, class)
	if err != nil {
		return "", err
	}
	password := ""
	if refused == "" {
		if password, refused, err = r.VolumeSnapshots.password(ctx, r.Client); err != nil {
			return "", err
		}
	}
	if refused != "" {
		r.refuse(ctx, cluster, "VolumeSnapshots", refused)
	}
	return password, nil
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
				"resources":          map[string]any{requestsKey: map[string]any{cpuKey: cpu10m, memoryKey: "32Mi"}},
				"podSecurityContext": map[string]any{nonRootKey: true, seccompProfileKey: runtimeDefault()},
				securityKey:          restrictedContainer(map[string]any{readOnlyRootKey: true}),
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

// removeVolumeSnapshots takes volume snapshots out of a cluster that switched them off: root@pam's
// configuration from the workload cluster, as the ClusterResourceSet never deletes what it applied,
// then the VolumeSnapshotClass, and the controller once the class's release is gone, as that cannot
// be uninstalled once its kind is no longer served. The proxies go last, so that while one is
// there the configuration is looked for again. It reports whether a removal waits on another.
func (r *Reconciler) removeVolumeSnapshots(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	class, controller := snapshotClassProxy(cluster), snapshotControllerProxy(cluster)
	on, err := r.anyProxy(ctx, cluster, class, controller)
	if err != nil || !on {
		return false, err
	}
	if err = r.removeRootConfig(ctx, cluster); err != nil {
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

// removeRootConfig deletes the CSI plugin's root@pam configuration from the workload cluster.
func (r *Reconciler) removeRootConfig(ctx context.Context, cluster *clusterv1.Cluster) error {
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		// Without a kubeconfig, nothing was ever applied to the cluster.
		return err
	}
	s := &corev1.Secret{}
	s.Namespace, s.Name = metav1.NamespaceSystem, rootWorkloadSecret
	if err := client.IgnoreNotFound(workload.client.Delete(ctx, s)); err != nil {
		r.Remotes.forget(cluster)
		return fmt.Errorf("deleting the CSI plugin's root@pam configuration: %w", err)
	}
	return nil
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
