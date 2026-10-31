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

	// csiProxyAddress is where the CSI plugin's controller reaches the proxy beside it: on the
	// pod's own loopback, as the plugin's calls carry the cluster's token over plain HTTP.
	csiProxyAddress = "127.0.0.1:8006"
	// csiProxyCA is where the proxy reads the Proxmox VE CA.
	csiProxyCA = "/etc/kubehenge/proxmox/ca.crt"
	// csiProxyName names the proxy's container, and its Roles and RoleBindings in csiproxy.Namespace,
	// for the copies' records, and in kube-system, for its events.
	csiProxyName = "kubehenge-csi-proxy"
	// verbUpdate is the RBAC verb that lets a subject change an object.
	verbUpdate = "update"
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

// VolumeSnapshots lets clusters take volume snapshots through the CSI plugin. The plugin copies
// volumes through a Proxmox VE method only root@pam may call, so a proxy beside it, Kubehenge's own
// csi-proxy (package csiproxy), makes the copies with the cluster's own token instead.
type VolumeSnapshots struct {
	// ProxyImage is the proxy's image, <repository>@sha256:<digest>; empty, no cluster may take
	// volume snapshots.
	ProxyImage string
}

// copyProxy is the proxy beside the CSI plugin's controller: whether it runs, whether it makes
// copies or only finds those made before, and what it runs with.
type copyProxy struct {
	on, copies bool
	image      string
	endpoint   Endpoint
	// owner is the cluster's volume owner VM. pool and holders are its pool and holder VMs' name.
	owner         int
	pool, holders string
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

// Refusal says why the cluster may not take volume snapshots as its csi variable asks: the
// controller has no image of the proxy, or the cluster's storage is not one the plugin takes them
// on (class's SnapshotStoragesAnnotation). It returns "" when nothing stands in the way, or when the
// cluster does not ask.
func (v VolumeSnapshots) Refusal(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) (string, error) {
	csi, err := csiOf(cluster)
	if err != nil || !csi.VolumeSnapshots {
		return "", err
	}
	if v.ProxyImage == "" {
		return "volume snapshots need the copy proxy's image, which the controller was not given", nil
	}
	storage, err := variable[string](cluster, "storage")
	if err != nil {
		return "", err
	}
	if class == nil || !slices.Contains(splitList(class.Annotations[SnapshotStoragesAnnotation]), storage) {
		return "volume snapshots need local storage (ZFS, LVM, LVM-thin, directory or btrfs), not " + storage, nil
	}
	return "", nil
}

// volumeSnapshotsOn reports whether the cluster's csi variable asks for volume snapshots and nothing
// stands in the way. When something does, it says why on the Cluster, and the plugin goes without.
func (r *Reconciler) volumeSnapshotsOn(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	csi, err := csiOf(cluster)
	if err != nil || !csi.VolumeSnapshots {
		return false, err
	}
	class := &clusterv1.ClusterClass{}
	if err = r.Client.Get(ctx, client.ObjectKey{Namespace: r.ClassNamespace, Name: cluster.Spec.Topology.ClassRef.Name}, class); err != nil {
		return false, fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	refused, err := r.VolumeSnapshots.Refusal(cluster, class)
	if err != nil {
		return false, err
	}
	if refused != "" {
		r.refuse(ctx, cluster, "VolumeSnapshots", refused)
	}
	return refused == "", nil
}

// checkVolumeSnapshots refuses volume snapshots for a cluster that cannot take them.
func (h *Hooks) checkVolumeSnapshots(cluster *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	refused, err := h.VolumeSnapshots.Refusal(cluster, class)
	if err != nil {
		return err
	}
	if refused != "" {
		return errors.New(refused)
	}
	return nil
}

// copyProxyFor is the proxy the cluster's CSI plugin reaches Proxmox VE through. It runs while the
// cluster takes volume snapshots, and gets holder VMs on the nodes its volumes may be on. Once
// they are switched off, it stays, making no more copies, while any copy made before is left, whose
// name only it turns into the real one. When it is no longer needed, it is taken out of the
// workload cluster before the ClusterResourceSet drops it, so that a failure is tried again.
func (r *Reconciler) copyProxyFor(ctx context.Context, cluster *clusterv1.Cluster, creds Credentials, addons *Addons) (copyProxy, error) {
	names, err := NamesFor(r.WorkloadPool, cluster.Namespace, cluster.Name)
	if err != nil {
		return copyProxy{}, err
	}
	proxy := copyProxy{
		on: addons.VolumeSnapshots, copies: addons.VolumeSnapshots, image: r.VolumeSnapshots.ProxyImage, endpoint: r.Endpoint,
		owner: creds.VolumeOwner, pool: names.Pool, holders: names.VolumeHolders(),
	}
	if addons.VolumeSnapshots {
		var nodes []string
		if nodes, err = holderNodes(cluster); err != nil {
			return proxy, err
		}
		if err = r.Cloud.EnsureVolumeHolders(ctx, names, nodes); err != nil {
			return proxy, fmt.Errorf("creating the cluster's holder VMs: %w", err)
		}
		return proxy, nil
	}
	was, err := r.copyProxyApplied(ctx, cluster)
	if err != nil || !was {
		return proxy, err
	}
	if proxy.on, err = r.copiesRemain(ctx, cluster); err != nil || proxy.on {
		return proxy, err
	}
	// Before the proxy, whose removal ends these steps for good.
	if err = r.Cloud.RemoveEmptyVolumeHolders(ctx, names); err != nil {
		return proxy, fmt.Errorf("removing the cluster's empty holder VMs: %w", err)
	}
	return proxy, r.removeCopyProxy(ctx, cluster)
}

// holderNodes are the nodes the cluster's volumes may be on, which hold its copies: those its VMs
// may be placed on.
func holderNodes(cluster *clusterv1.Cluster) ([]string, error) {
	chosen, _, err := optionalVariable[[]string](cluster, allowedNodesVariable)
	if err != nil {
		return nil, err
	}
	var online []string
	if value := cluster.Annotations[OnlineNodesAnnotation]; value != "" {
		online = strings.Split(value, ",")
	}
	return placement(chosen, online), nil
}

// copyProxyApplied reports whether the cluster's ClusterResourceSet applies the proxy.
func (r *Reconciler) copyProxyApplied(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	s := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name + "-proxmox"}, s)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the cluster's ClusterResourceSet Secret: %w", err)
	}
	_, applied := s.Data[proxiedWorkloadSecret+".yaml"]
	return applied, nil
}

// copiesRemain reports whether the workload cluster has a record of a copy, made or under way.
func (r *Reconciler) copiesRemain(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return false, err
	}
	list := &corev1.ConfigMapList{}
	if err := workload.client.List(ctx, list, client.InNamespace(csiproxy.Namespace), client.HasLabels{csiproxy.CopyLabel}, client.Limit(1)); err != nil {
		r.Remotes.forget(cluster)
		return false, fmt.Errorf("listing the cluster's volume copies: %w", err)
	}
	return len(list.Items) > 0, nil
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
// VolumeSnapshotClass, and the controller once the class's release is gone, as that cannot be
// uninstalled once its kind is no longer served. The proxy goes once no copy is left
// (copyProxyFor). It reports whether a removal waits on another.
func (r *Reconciler) removeVolumeSnapshots(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	class, controller := snapshotClassProxy(cluster), snapshotControllerProxy(cluster)
	on, err := r.anyProxy(ctx, cluster, class, controller)
	if err != nil || !on {
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

// removeCopyProxy deletes from the workload cluster what the ClusterResourceSet applied for the
// proxy, which it never deletes itself: the CSI plugin's proxied configuration, the proxy's rights
// for its events, and the namespace of the copies' records with its rights there.
func (r *Reconciler) removeCopyProxy(ctx context.Context, cluster *clusterv1.Cluster) error {
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		// Without a kubeconfig, nothing was ever applied to the cluster.
		return err
	}
	for _, o := range []client.Object{
		&corev1.Secret{Namespace: metav1.NamespaceSystem, Name: proxiedWorkloadSecret},
		&rbacv1.RoleBinding{Namespace: metav1.NamespaceSystem, Name: csiProxyName},
		&rbacv1.Role{Namespace: metav1.NamespaceSystem, Name: csiProxyName},
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
// Proxmox VE through the proxy, the namespace of the copies' records (which only the plugin's
// controller, and with it the proxy, may write), and its right to record events in kube-system.
func copyProxyManifests(c Credentials) (map[string][]byte, error) {
	csi, err := csiConfig(c, &proxmoxCluster{URL: "http://" + csiProxyAddress + "/api2/json", TokenID: c.TokenID, TokenSecret: c.TokenSecret, Region: c.Region})
	if err != nil {
		return nil, err
	}
	config, err := workloadManifest(proxiedWorkloadSecret, map[string]string{csiConfigKey: csi})
	if err != nil {
		return nil, err
	}
	role := func(namespace string, rule rbacv1.PolicyRule) []any {
		return []any{
			&rbacv1.Role{
				APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role", Namespace: namespace, Name: csiProxyName,
				Rules: []rbacv1.PolicyRule{rule},
			},
			&rbacv1.RoleBinding{
				APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding", Namespace: namespace, Name: csiProxyName,
				RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: csiProxyName},
				Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: metav1.NamespaceSystem, Name: csiControllerAccount}},
			},
		}
	}
	objects := append([]any{&corev1.Namespace{APIVersion: "v1", Kind: kindNamespace, Name: csiproxy.Namespace}},
		role(csiproxy.Namespace, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "create", verbUpdate, "delete"}})...)
	objects = append(objects, role(metav1.NamespaceSystem, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create"}})...)
	var records []byte
	for _, o := range objects {
		manifest, err := yaml.Marshal(o)
		if err != nil {
			return nil, fmt.Errorf("encoding the proxy's rights: %w", err)
		}
		records = append(append(records, "---\n"...), manifest...)
	}
	return map[string][]byte{proxiedWorkloadSecret + ".yaml": config, csiproxy.Namespace + ".yaml": records}, nil
}

// csiProxyContainer is the proxy beside the CSI plugin's controller, as a native sidecar, which
// starts before the plugin and runs as long as it does.
func csiProxyContainer(proxy *copyProxy) map[string]any {
	fieldEnv := func(name, path string) map[string]any {
		return map[string]any{"name": name, "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": path}}}
	}
	return map[string]any{
		"name":          csiProxyName,
		"image":         proxy.image,
		"restartPolicy": string(corev1.ContainerRestartPolicyAlways),
		"args": []any{
			"csi-proxy", "--listen=" + csiProxyAddress, "--proxmox-url=" + proxy.endpoint.URL, "--proxmox-ca=" + csiProxyCA,
			"--proxmox-insecure=" + strconv.FormatBool(proxy.endpoint.Insecure),
			"--volume-owner=" + strconv.Itoa(proxy.owner), "--pool=" + proxy.pool, "--holders=" + proxy.holders,
			"--copies=" + strconv.FormatBool(proxy.copies),
		},
		"env":          []any{fieldEnv("POD_NAME", "metadata.name"), fieldEnv("POD_NAMESPACE", "metadata.namespace")},
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
