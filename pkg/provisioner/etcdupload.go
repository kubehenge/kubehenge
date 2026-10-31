package provisioner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// RKE2's and k3s's own upload of their etcd snapshots, an opt-in beside Kubehenge's. The servers
// read the target from a Secret in the workload cluster's kube-system on every snapshot
// (etcd-s3-config-secret), which keeps the store's keys out of the bootstrap data that would
// otherwise carry them onto every server's cloud-init ISO, as the RKE2 provider's backupConfig
// does; the k3s provider has no setting for it at all, so both get the same drop-in.
const (
	// EtcdUploadSecret is the Secret in the workload cluster's kube-system the servers read the
	// target from, as the classes' drop-in names it.
	EtcdUploadSecret = "kubehenge-etcd-s3" //nolint:gosec // a Secret's name, not a credential
	// etcdUploadSecretType is the type RKE2 and k3s look for in that Secret.
	etcdUploadSecretType corev1.SecretType = "etcd.k3s.cattle.io/s3-config-secret" //nolint:gosec // a Secret's type, not a credential
	// etcdUploadInstalledAnnotation on the cluster's backup keys records that the Secret was put
	// into the cluster, so that only a cluster that had it is reached to take it away.
	etcdUploadInstalledAnnotation = "kubehenge.dev/etcd-upload-installed"
	// etcdUploadDropIn is the file the classes write into the servers' config.yaml.d to have them
	// upload, as the chart names it.
	etcdUploadDropIn  = "60-kubehenge-etcd-s3.yaml"
	defaultS3Endpoint = "s3.amazonaws.com"
	defaultS3Region   = "us-east-1"
	schemeHTTPS       = "https"
)

// uploads says whether a distribution uploads etcd snapshots of its own.
func uploads(d templates.Distribution) bool {
	return d == templates.RKE2 || d == templates.K3s
}

// EtcdUploadPrefix is the folder of a cluster's own in its target that its RKE2 or k3s servers
// upload to, beside the snapshots Kubehenge takes.
func EtcdUploadPrefix(prefix, namespace, cluster string, d templates.Distribution) string {
	return targetFolder(prefix, namespace, cluster, string(d)+"-etcd")
}

// etcdUploadData is the Secret the servers read: the target, always with its certificate verified,
// and the store's keys.
func etcdUploadData(t *BackupTarget, folder string, creds map[string][]byte) (map[string][]byte, error) {
	endpoint := defaultS3Endpoint
	if t.Endpoint != "" {
		u, err := url.Parse(t.Endpoint)
		if err != nil || u.Scheme != schemeHTTPS || u.Host == "" {
			return nil, fmt.Errorf("backup target %s: the endpoint %q is no https:// URL", t.Name, t.Endpoint)
		}
		endpoint = u.Host
	}
	data := map[string][]byte{
		"etcd-s3-endpoint":        []byte(endpoint),
		"etcd-s3-skip-ssl-verify": []byte(strconv.FormatBool(false)),
		"etcd-s3-insecure":        []byte(strconv.FormatBool(false)),
		"etcd-s3-bucket":          []byte(t.Bucket),
		"etcd-s3-folder":          []byte(folder),
		"etcd-s3-region":          []byte(cmp.Or(t.Region, defaultS3Region)),
		"etcd-s3-access-key":      []byte(strings.TrimSpace(string(creds[backupAccessKeyID]))),
		"etcd-s3-secret-key":      []byte(strings.TrimSpace(string(creds[backupSecretAccessKey]))),
	}
	if t.CACertificate != "" {
		data["etcd-s3-endpoint-ca"] = []byte(t.CACertificate)
	}
	return data, nil
}

// checkEtcdUpload holds the distribution's own upload to a cluster whose distribution has one and
// whose etcd snapshots are on, whose target and schedule it shares.
func checkEtcdUpload(c *clusterv1.Cluster, s *EtcdSnapshots) error {
	if !s.DistributionUpload {
		return nil
	}
	if !uploads(distributionOf(c)) {
		return fmt.Errorf("etcdSnapshots: distributionUpload works on RKE2 and k3s only, not %s", distributionOf(c))
	}
	if !s.Enabled {
		return errors.New("etcdSnapshots: distributionUpload needs etcd snapshots switched on")
	}
	return nil
}

// reconcileEtcdUpload puts the Secret the servers of an RKE2 or k3s cluster read their target from
// into the cluster while they upload snapshots of their own, and takes it out again once they no
// longer do. It reports whether it waits on the cluster's API.
func (r *Reconciler) reconcileEtcdUpload(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	d := distributionOf(cluster)
	if !uploads(d) {
		return false, nil
	}
	s, err := EtcdSnapshotsOf(cluster)
	if err != nil {
		return false, err
	}
	keys, err := r.backupKeys(ctx, cluster, s.Enabled && s.DistributionUpload)
	if err != nil || keys == nil {
		return false, err
	}
	installed := keys.Annotations[etcdUploadInstalledAnnotation] == strconv.FormatBool(true)
	if !s.Enabled || !s.DistributionUpload {
		if !installed {
			return false, nil
		}
		return r.removeEtcdUpload(ctx, cluster, keys)
	}
	target, found, err := r.backupTarget(ctx, cluster, s.Target)
	if err != nil {
		return false, err
	}
	if !found {
		r.holdEtcdUpload(ctx, cluster, fmt.Sprintf("backup target %q does not exist", s.Target))
		return false, nil
	}
	creds, missing, err := r.clusterBackupCredential(ctx, cluster)
	if err != nil {
		return false, err
	}
	if missing != "" {
		r.holdEtcdUpload(ctx, cluster, missing)
		return false, nil
	}
	data, err := etcdUploadData(&target, EtcdUploadPrefix(target.Prefix, cluster.Namespace, cluster.Name, d), creds)
	if err != nil {
		r.holdEtcdUpload(ctx, cluster, err.Error())
		return false, nil
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return true, err
	}
	if !installed {
		// Marked first, so that a Secret put there is always taken away again.
		err = r.patchKeys(ctx, keys, func(k *corev1.Secret) {
			metav1.SetMetaDataAnnotation(&k.ObjectMeta, etcdUploadInstalledAnnotation, strconv.FormatBool(true))
		})
		if err != nil {
			return false, err
		}
	}
	secret := &corev1.Secret{Namespace: metav1.NamespaceSystem, Name: EtcdUploadSecret}
	_, err = controllerutil.CreateOrUpdate(ctx, workload.client, secret, func() error {
		secret.Type, secret.Data = etcdUploadSecretType, data
		return nil
	})
	if err != nil {
		r.Remotes.forget(cluster)
		return false, fmt.Errorf("applying Secret %s/%s: %w", metav1.NamespaceSystem, EtcdUploadSecret, err)
	}
	return false, nil
}

// removeEtcdUpload takes the servers' Secret out of a cluster that no longer uploads, once no server
// still runs with the drop-in: switching the upload off only rolls the control planes, and a server
// not yet replaced reads the Secret on each scheduled snapshot, which fails without it. It reports
// whether it waits, on those servers or on the cluster's API.
func (r *Reconciler) removeEtcdUpload(ctx context.Context, cluster *clusterv1.Cluster, keys *corev1.Secret) (bool, error) {
	if uploading, err := r.serversUpload(ctx, cluster); err != nil || uploading {
		return uploading, err
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return true, err
	}
	secret := &corev1.Secret{Namespace: metav1.NamespaceSystem, Name: EtcdUploadSecret}
	if err := workload.client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		r.Remotes.forget(cluster)
		return false, fmt.Errorf("deleting Secret %s/%s: %w", metav1.NamespaceSystem, EtcdUploadSecret, err)
	}
	return false, r.patchKeys(ctx, keys, func(k *corev1.Secret) { delete(k.Annotations, etcdUploadInstalledAnnotation) })
}

// serversUpload reports whether any of a cluster's control-plane machines, those being deleted
// among them, may still run with the drop-in: one whose bootstrap data writes it, or one without
// bootstrap data yet, which may still get it from the template it was made from.
func (r *Reconciler) serversUpload(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	var machines clusterv1.MachineList
	err := r.Client.List(ctx, &machines, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}, client.HasLabels{clusterv1.MachineControlPlaneLabel})
	if err != nil {
		return false, fmt.Errorf("listing the control-plane machines: %w", err)
	}
	for i := range machines.Items {
		m := &machines.Items[i]
		name := m.Spec.Bootstrap.DataSecretName
		if name == nil || *name == "" {
			return true, nil
		}
		data := &corev1.Secret{}
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: *name}, data)
		switch {
		// Cluster API deletes the bootstrap data only after the machine's VM.
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return false, fmt.Errorf("reading machine %s's bootstrap data: %w", m.Name, err)
		case strings.Contains(string(data.Data["value"]), etcdUploadDropIn):
			return true, nil
		}
	}
	return false, nil
}

func (r *Reconciler) patchKeys(ctx context.Context, keys *corev1.Secret, change func(*corev1.Secret)) error {
	patch := client.MergeFrom(keys.DeepCopy())
	change(keys)
	if err := r.Client.Patch(ctx, keys, patch); err != nil {
		return fmt.Errorf("marking the backup keys: %w", err)
	}
	return nil
}

// holdEtcdUpload reports on the cluster why its servers are given no target to upload to.
func (r *Reconciler) holdEtcdUpload(ctx context.Context, cluster *clusterv1.Cluster, message string) {
	ctrl.LoggerFrom(ctx).Info("holding the distribution's etcd upload: " + message)
	if r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, "EtcdUploadHeld", "UploadEtcdSnapshots", "%s", message)
	}
}
