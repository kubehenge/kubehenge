package provisioner

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The annotations on the read-only location a cluster's Velero gets for the backups of each
// cluster it reads, which tell the interface and kubectl whose backups the location holds.
const (
	// BackupSourceAnnotation names the cluster whose backups the location holds.
	BackupSourceAnnotation = "kubehenge.dev/backups-of"
	// VolumeDataAnnotation is "true" where Velero can read the volumes' data of the location's
	// backups: the two clusters share one Kopia password.
	VolumeDataAnnotation = "kubehenge.dev/volume-data"
)

const (
	// maxBackupSources bounds the clusters one reads the backups of.
	maxBackupSources = 10
	// sourceLocationPrefix begins the name of the location of another cluster's backups.
	sourceLocationPrefix = "cluster-"
	// sourceSSECPrefix begins the name of the Secret with another cluster's SSE-C key.
	sourceSSECPrefix = veleroSSECSecret + "-"
)

// SourceLocation is the location in a cluster's Velero with the backups of the cluster source.
func SourceLocation(source string) string { return sourceLocationPrefix + source }

// backupSource is a cluster whose backups another reads.
type backupSource struct {
	name string
	sseC []byte
	// volumeData tells whether the reading cluster's Velero can read the volumes' data too.
	volumeData bool
}

// checkReadFrom holds a cluster's readFrom to the names of other clusters, each named once.
func (b *Backups) checkReadFrom(cluster string) error {
	if len(b.ReadFrom) > maxBackupSources {
		return fmt.Errorf("backups: readFrom names at most %d clusters", maxBackupSources)
	}
	seen := map[string]bool{}
	for _, s := range b.ReadFrom {
		switch {
		case len(validation.IsDNS1123Label(s)) > 0:
			return fmt.Errorf("backups: readFrom: %q is not a cluster's name", s)
		case s == cluster:
			return errors.New("backups: readFrom names the cluster itself, whose backups it reads anyway")
		case seen[s]:
			return fmt.Errorf("backups: readFrom names %s twice", s)
		}
		seen[s] = true
	}
	return nil
}

// sourceKeys reads the backup keys of a cluster another reads the backups of, with why it may not
// when it may not: the keys must be beside the cluster, and the source, while it exists, must back
// up to the same target, where its backups are. A source deleted since keeps its backups and keys.
func sourceKeys(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster, target, source string) (*corev1.Secret, string, error) {
	keys := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: BackupKeysSecretName(source)}, keys)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Sprintf("cluster %s has no backup keys beside it, so it has no backups to read", source), nil
	case err != nil:
		return nil, "", fmt.Errorf("reading the backup keys of %s: %w", source, err)
	case len(keys.Data[BackupKeysKeySSEC]) != sseCKeyBytes || len(keys.Data[BackupKeysKeyRepository]) == 0:
		return nil, fmt.Sprintf("the Secret %s holds no backup keys", BackupKeysSecretName(source)), nil
	}
	other := &clusterv1.Cluster{}
	err = c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: source}, other)
	switch {
	case apierrors.IsNotFound(err):
		return keys, "", nil
	case err != nil:
		return nil, "", fmt.Errorf("reading cluster %s: %w", source, err)
	}
	// A source whose variable cannot be read is refused like one backing up elsewhere: it is that
	// cluster's to fix, and whether its backups are where this one's are cannot be told.
	b, _, err := optionalVariable[Backups](other, backupsVariable)
	if err != nil {
		return nil, fmt.Sprintf("whether cluster %s backs up to target %s cannot be told: %v", source, target, err), nil
	}
	if b.Target != target {
		return nil, fmt.Sprintf("cluster %s does not back up to target %s", source, target), nil
	}
	return keys, "", nil
}

// checkBackupSources refuses the clusters a cluster's backups newly read that it may not read. Those
// it read before stay allowed, so that a source changed since holds no unrelated edit back.
func checkBackupSources(ctx context.Context, c client.Reader, old, cluster *clusterv1.Cluster) error {
	b, _, err := optionalVariable[Backups](cluster, backupsVariable)
	if err != nil || !b.Enabled {
		return err
	}
	var before []string
	if old != nil {
		if was, _, werr := optionalVariable[Backups](old, backupsVariable); werr == nil && was.Target == b.Target {
			before = was.ReadFrom
		}
	}
	for _, s := range b.ReadFrom {
		if slices.Contains(before, s) || len(validation.IsDNS1123Label(s)) > 0 {
			continue
		}
		_, why, err := sourceKeys(ctx, c, cluster, b.Target, s)
		if err != nil {
			return err
		}
		if why != "" {
			return errors.New("backups: readFrom: " + why)
		}
	}
	return nil
}

// sourceRepository is the Kopia password a cluster's new keys take: that of the first cluster it
// reads the backups of that has keys, or nil. Keys are never changed once made, so a variable that
// cannot be read makes none rather than a password that might be the wrong one for good.
func sourceRepository(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) ([]byte, error) {
	b, _, err := optionalVariable[Backups](cluster, backupsVariable)
	if err != nil {
		return nil, err
	}
	for _, s := range b.ReadFrom {
		keys, why, err := sourceKeys(ctx, c, cluster, b.Target, s)
		if err != nil {
			return nil, err
		}
		if why == "" {
			return keys.Data[BackupKeysKeyRepository], nil
		}
	}
	return nil, nil
}

// backupSources are the clusters whose backups the cluster reads, of those it names, with their
// SSE-C keys. One it may not read is left out, with an event saying why.
func (r *Reconciler) backupSources(ctx context.Context, cluster *clusterv1.Cluster, b *Backups) ([]backupSource, error) {
	if len(b.ReadFrom) == 0 {
		return nil, nil
	}
	own, err := r.backupKeys(ctx, cluster, false)
	if err != nil {
		return nil, err
	}
	var out []backupSource
	for _, name := range b.ReadFrom {
		keys, why, err := sourceKeys(ctx, r.Client, cluster, b.Target, name)
		if err != nil {
			return nil, err
		}
		if why != "" {
			r.holdBackups(ctx, cluster, "BackupSourceRefused", "not reading the backups of "+name+": "+why)
			continue
		}
		shared := own != nil && subtle.ConstantTimeCompare(own.Data[BackupKeysKeyRepository], keys.Data[BackupKeysKeyRepository]) == 1
		out = append(out, backupSource{name: name, sseC: keys.Data[BackupKeysKeySSEC], volumeData: shared})
	}
	return out, nil
}

// veleroBackupGVK is Velero's Backup.
var veleroBackupGVK = schema.GroupVersionKind{Group: veleroAPIGroup, Version: "v1", Kind: "Backup"}

// removeOldSources takes out of the cluster what is left of the clusters it no longer reads the
// backups of: their SSE-C keys, and the Backups Velero listed from their locations, which it keeps
// once the location is gone. Deleting a Backup, unlike asking Velero to, leaves what is in the
// target as it is.
func removeOldSources(ctx context.Context, workload client.Client, sources []backupSource) error {
	wanted := map[string]bool{}
	for _, s := range sources {
		wanted[s.name] = true
	}
	secrets := &corev1.SecretList{}
	if err := workload.List(ctx, secrets, client.InNamespace(VeleroNamespace)); err != nil {
		return fmt.Errorf("listing the Secrets in %s: %w", VeleroNamespace, err)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if name, ok := strings.CutPrefix(s.Name, sourceSSECPrefix); ok && !wanted[name] {
			if err := client.IgnoreNotFound(workload.Delete(ctx, s)); err != nil {
				return fmt.Errorf("deleting Secret %s/%s: %w", VeleroNamespace, s.Name, err)
			}
		}
	}
	backups := &unstructured.UnstructuredList{}
	backups.SetGroupVersionKind(veleroBackupGVK.GroupVersion().WithKind(veleroBackupGVK.Kind + "List"))
	err := workload.List(ctx, backups, client.InNamespace(VeleroNamespace))
	if meta.IsNoMatchError(err) {
		// Velero's chart has not put its resource definitions in yet.
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing Velero's backups: %w", err)
	}
	for i := range backups.Items {
		o := &backups.Items[i]
		location, _, _ := unstructured.NestedString(o.Object, "spec", "storageLocation")
		if name, ok := strings.CutPrefix(location, sourceLocationPrefix); ok && !wanted[name] {
			if err := client.IgnoreNotFound(workload.Delete(ctx, o)); err != nil {
				return fmt.Errorf("deleting backup %s: %w", o.GetName(), err)
			}
		}
	}
	return nil
}
