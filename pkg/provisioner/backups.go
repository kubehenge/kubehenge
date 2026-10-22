package provisioner

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// BackupTargetsAnnotation on a ClusterClass lists, as JSON, the S3 targets an administrator set for
// backups (the chart's backupTargets), which a cluster's backups variable names one of.
const BackupTargetsAnnotation = "kubehenge.dev/backup-targets"

const (
	backupsVariable = "backups"
	// VeleroNamespace holds Velero in a workload cluster, with its keys and the interface's
	// identities.
	VeleroNamespace = "velero"
	veleroRelease   = "velero"
	// VeleroSchedule is the Schedule the chart makes of the cluster's backups variable; a backup
	// made at once copies its template.
	VeleroSchedule = "velero-scheduled"
	// veleroLocation is the one BackupStorageLocation, the cluster's prefix in its target.
	veleroLocation = "default"
	// backupCredentialPurpose is the user credential with a cluster's keys for its target.
	backupCredentialPurpose = "backup"

	// BackupKeysKeyRepository is the key of a cluster's backup keys Secret with the password of
	// Velero's Kopia repository, which encrypts the volumes' data.
	BackupKeysKeyRepository = "repository-password"
	// BackupKeysKeySSEC is the key of a cluster's backup keys Secret with the 32 bytes the store
	// encrypts every object Velero writes with (SSE-C).
	BackupKeysKeySSEC = "sse-c-key"
	// BackupKeysKeyEtcd is the key of a cluster's backup keys Secret with the age identity its etcd
	// snapshots are encrypted to.
	BackupKeysKeyEtcd = "etcd-snapshot-identity"
	// backupsInstalledAnnotation on the keys Secret records that Velero was put into the cluster,
	// so that switching backups off removes it, and nothing is looked for in clusters that never had
	// it.
	backupsInstalledAnnotation = "kubehenge.dev/backups-installed"

	// The Secrets in the velero namespace: Velero's own name for its repository password, the SSE-C
	// key the S3 plugin reads, and the store's keys as an AWS credentials file.
	veleroRepoSecret        = "velero-repo-credentials" //nolint:gosec // a Secret's name, not a credential
	veleroRepoKey           = "repository-password"
	veleroSSECSecret        = "velero-sse-c" //nolint:gosec // a Secret's name, not a credential
	veleroSSECKey           = "key"
	veleroCredentialsSecret = "cloud-credentials" //nolint:gosec // a Secret's name, not a credential
	veleroCredentialsKey    = "cloud"

	// The keys of a backup credential, as the interface stores them.
	backupAccessKeyID     = "accessKeyID"
	backupSecretAccessKey = "secretAccessKey"

	// sseCKeyBytes is the length of an SSE-C key, AES-256's.
	sseCKeyBytes = 32
	// hoursPerDay turns the retention in days into the hours Velero's TTL counts in.
	hoursPerDay = 24
	// veleroUser and veleroGroup are the cnb user Velero's image runs as, by number, so that
	// runAsNonRoot can be checked.
	veleroUser  = 1002
	veleroGroup = 1000
)

// Defaults of the backups variable, as the chart's schema has them.
const (
	defaultBackupSchedule  = "0 2 * * *"
	defaultBackupTimeZone  = "UTC"
	defaultBackupRetention = 7
	backupVolumesAll       = "all"
	backupVolumesAnnotated = "annotated"
	// defaultBackupRegion is what the AWS SDK is given for a store that does not care, as most
	// S3-compatible ones do not.
	defaultBackupRegion = "us-east-1"
)

// Backups is a cluster's backups variable.
type Backups struct {
	Enabled       bool   `json:"enabled"`
	Target        string `json:"target,omitempty"`
	Schedule      string `json:"schedule,omitempty"`
	TimeZone      string `json:"timeZone,omitempty"`
	RetentionDays int    `json:"retentionDays,omitempty"`
	Volumes       string `json:"volumes,omitempty"`
	// ExistingSecret is a Secret the cluster's owner keeps in its velero namespace, with the store's
	// keys as Velero's own credentials file, which never passes through the management cluster.
	ExistingSecret string `json:"existingSecret,omitempty"`
}

// withDefaults fills in what the cluster left to the chart's schema; the controller reads only what
// the Cluster itself holds.
func (b *Backups) withDefaults() {
	if b.Schedule == "" {
		b.Schedule = defaultBackupSchedule
	}
	if b.TimeZone == "" {
		b.TimeZone = defaultBackupTimeZone
	}
	if b.RetentionDays == 0 {
		b.RetentionDays = defaultBackupRetention
	}
	if b.Volumes == "" {
		b.Volumes = backupVolumesAll
	}
}

// cron is the schedule as Velero reads it, in its time zone.
func (b *Backups) cron() string {
	return zonedCron(b.Schedule, b.TimeZone)
}

// zonedCron is a schedule in its time zone, as robfig/cron reads it and Velero with it.
func zonedCron(schedule, timeZone string) string {
	return "CRON_TZ=" + timeZone + " " + schedule
}

// parseSchedule reads a variable's cron schedule in its IANA time zone, which the variable names
// on its own.
func parseSchedule(what, schedule, timeZone string) (cron.Schedule, error) { //nolint:ireturn // robfig/cron parses to its Schedule interface alone
	if _, err := time.LoadLocation(timeZone); err != nil || timeZone == "Local" {
		return nil, fmt.Errorf("%s: time zone %q is not an IANA time zone", what, timeZone)
	}
	if strings.HasPrefix(schedule, "TZ=") || strings.HasPrefix(schedule, "CRON_TZ=") {
		return nil, fmt.Errorf("%s: the schedule names its time zone in timeZone, not itself", what)
	}
	s, err := cron.ParseStandard(zonedCron(schedule, timeZone))
	if err != nil {
		return nil, fmt.Errorf("%s: schedule %q: %w", what, schedule, err)
	}
	return s, nil
}

// BackupTarget is one of the S3 stores in BackupTargetsAnnotation.
type BackupTarget struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket"`
	Region   string `json:"region,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	// CACertificate is the PEM CA of the store's certificate, when the system's do not cover it.
	CACertificate string `json:"caCertificate,omitempty"`
	// Checksums false keeps the AWS SDK from sending checksums, which some stores refuse.
	Checksums *bool `json:"checksums,omitempty"`
}

// BackupTargets reads a class's targets.
func BackupTargets(class *clusterv1.ClusterClass) ([]BackupTarget, error) {
	raw, ok := class.Annotations[BackupTargetsAnnotation]
	if !ok || raw == "" {
		return nil, nil
	}
	var targets []BackupTarget
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		return nil, fmt.Errorf("class %s, annotation %s: %w", class.Name, BackupTargetsAnnotation, err)
	}
	return targets, nil
}

// BackupPrefix is where a cluster's backups go in its target: under the target's prefix, a folder
// of the cluster's own, named for its namespace and name as its Proxmox VE pool is.
func BackupPrefix(prefix, namespace, cluster string) string {
	return targetFolder(prefix, namespace, cluster, "velero")
}

// targetFolder is a folder of the cluster's own in a target, for its Velero backups or its etcd
// snapshots.
func targetFolder(prefix, namespace, cluster, folder string) string {
	own := namespace + "." + cluster + "/" + folder
	if prefix == "" {
		return own
	}
	return prefix + "/" + own
}

// checkBackups holds a cluster's backups variable to what Velero takes: a target of the class, a
// schedule cron reads and a time zone that exists. A target that is gone stays allowed for a
// cluster that already names it, so that an administrator taking one away holds no unrelated edit
// back; the controller then neither installs nor removes Velero.
func checkBackups(old, c *clusterv1.Cluster, class *clusterv1.ClusterClass) error {
	b, _, err := optionalVariable[Backups](c, backupsVariable)
	if err != nil || !b.Enabled {
		return err
	}
	b.withDefaults()
	if err = b.valid(); err != nil {
		return err
	}
	if old != nil {
		was, _, werr := optionalVariable[Backups](old, backupsVariable)
		if werr == nil && was.Enabled && was.Target == b.Target {
			return nil
		}
	}
	return targetOffered(class, b.Target, "backups")
}

// targetOffered tells whether the class offers the target a cluster's backups or etcd snapshots
// name.
func targetOffered(class *clusterv1.ClusterClass, name, what string) error {
	targets, err := BackupTargets(class)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(targets, func(t BackupTarget) bool { return t.Name == name }) {
		if len(targets) == 0 {
			return fmt.Errorf("%s need a target, and an administrator has set none: %q is not one", what, name)
		}
		return fmt.Errorf("backup target %q is not one an administrator set", name)
	}
	return nil
}

// valid checks the schedule and time zone, which reach Velero inside its chart's values.
func (b *Backups) valid() error {
	if _, err := parseSchedule(backupsVariable, b.Schedule, b.TimeZone); err != nil {
		return err
	}
	if b.Volumes != backupVolumesAll && b.Volumes != backupVolumesAnnotated {
		return fmt.Errorf("backups: volumes is all or annotated, not %q", b.Volumes)
	}
	if slices.Contains([]string{veleroRepoSecret, veleroSSECSecret, veleroCredentialsSecret}, b.ExistingSecret) {
		return fmt.Errorf("backups: Kubehenge writes Secret %s itself; existingSecret names another", b.ExistingSecret)
	}
	return nil
}

// veleroProxy names the cluster's Velero proxy.
func veleroProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-" + veleroRelease
}

// BackupKeysSecretName is the Secret beside a Cluster with its backups' keys. The Cluster does not
// own it, so that a cluster made again under the same name, backing up to the same target, reads
// the backups of the one before; deleting it leaves those backups unreadable for good.
func BackupKeysSecretName(cluster string) string { return cluster + "-backup-keys" }

// backupsExcluded are the namespaces left out of every backup: Kubernetes' own, kube-system above
// all, which holds the cluster's Proxmox VE token, Velero's, and those of the add-ons, which
// Kubehenge installs again itself and whose restored copies would fight the releases that own them.
func backupsExcluded(exempt []string) []any {
	names := []string{
		metav1.NamespaceSystem, metav1.NamespacePublic, corev1.NamespaceNodeLease, VeleroNamespace,
		headlampAddon, monitoringAddon, logsNamespace, logsNodeNamespace, certManagerAddon, kyvernoAddon,
		"istio-system", rancherNamespace,
	}
	for _, ns := range exempt {
		if !slices.Contains(names, ns) {
			names = append(names, ns)
		}
	}
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

// backupsExcludedResources are what no restore should bring back: what each node and the API server
// keep of themselves, and the CNIs' state of addresses and identities, which the running ones own.
var backupsExcludedResources = []any{
	"nodes", "events", "events.events.k8s.io", "csinodes.storage.k8s.io", "volumeattachments.storage.k8s.io",
	"certificatesigningrequests.certificates.k8s.io", "leases.coordination.k8s.io",
	"ciliumendpoints.cilium.io", "ciliumendpointslices.cilium.io", "ciliumidentities.cilium.io", "ciliumnodes.cilium.io",
	"ipamblocks.crd.projectcalico.org", "blockaffinities.crd.projectcalico.org", "ipamhandles.crd.projectcalico.org",
}

// veleroPolicy is the resource policy every backup follows: emptyDir volumes hold nothing a pod
// started again would miss.
const veleroPolicy = `version: v1
volumePolicies:
  - conditions:
      volumeTypes: [emptyDir]
    action:
      type: skip
`

// veleroValues install Velero with its plugin for S3 and its node agents, Kopia backing volumes up
// file by file, and one backup location: the cluster's prefix in its target, the store encrypting
// every object with the cluster's SSE-C key, which the plugin reads from the velero-sse-c Secret.
// The Secrets themselves are put into the cluster beforehand (backupsWait), never into values. The
// server runs without root or capabilities; the node agents read every pod's volumes as root. The
// schedule's backups outlive it, so that switching backups off leaves them in the target.
func veleroValues(charts *Charts, b *Backups, t *BackupTarget, cluster *clusterv1.Cluster) map[string]any {
	config := map[string]any{
		regionKey:                     t.Region,
		"customerKeyEncryptionSecret": veleroSSECSecret + "/" + veleroSSECKey,
	}
	if t.Region == "" {
		config[regionKey] = defaultBackupRegion
	}
	if t.Endpoint != "" {
		config["s3Url"] = t.Endpoint
		config["s3ForcePathStyle"] = strconv.FormatBool(true)
	}
	if t.Checksums != nil && !*t.Checksums {
		config["checksumAlgorithm"] = ""
	}
	location := map[string]any{
		"name": veleroLocation, "provider": "aws", "bucket": t.Bucket, defaultKey: true,
		"prefix": BackupPrefix(t.Prefix, cluster.Namespace, cluster.Name), configKey: config,
	}
	if t.CACertificate != "" {
		location["caCert"] = base64.StdEncoding.EncodeToString([]byte(t.CACertificate))
	}
	all := b.Volumes == backupVolumesAll
	policyName := veleroRelease + "-resource-policy"
	tolerations := []any{toleration(controlPlaneTaint.Key, controlPlaneTaint.Effect)}
	return map[string]any{
		"image": map[string]any{
			"repository": charts.VeleroImage.Registry + "/" + charts.VeleroImage.Repository,
			"tag":        charts.VeleroImage.Tag + "@sha256:" + charts.VeleroImage.Digest,
		},
		"initContainers": []any{map[string]any{
			"name": "velero-plugin-for-aws", "image": charts.VeleroPlugin.String(), "imagePullPolicy": "IfNotPresent",
			"volumeMounts": []any{map[string]any{"mountPath": "/target", "name": "plugins"}},
			securityKey:    restrictedContainer(nil),
		}},
		"podSecurityContext": map[string]any{
			nonRootKey: true, "runAsUser": veleroUser, "runAsGroup": veleroGroup, "fsGroup": veleroGroup,
			seccompProfileKey: runtimeDefault(),
		},
		"containerSecurityContext": restrictedContainer(nil),
		"resources": map[string]any{
			requestsKey: map[string]any{cpuKey: "50m", memoryKey: "128Mi"},
			"limits":    map[string]any{memoryKey: "1Gi"},
		},
		tolerationsKey: tolerations,
		// Its metrics are served without authentication.
		"metrics":          map[string]any{"enabled": false},
		"snapshotsEnabled": false,
		"deployNodeAgent":  true,
		"nodeAgent": map[string]any{
			"resources":    map[string]any{requestsKey: map[string]any{cpuKey: "20m", memoryKey: memory64Mi}},
			tolerationsKey: tolerations,
		},
		"credentials": map[string]any{"useSecret": true, "existingSecret": cmp.Or(b.ExistingSecret, veleroCredentialsSecret)},
		"configuration": map[string]any{
			"uploaderType":             "kopia",
			"defaultVolumesToFsBackup": all,
			"backupStorageLocation":    []any{location},
		},
		"schedules": map[string]any{
			strings.TrimPrefix(VeleroSchedule, veleroRelease+"-"): map[string]any{
				"schedule": b.cron(),
				"template": map[string]any{
					"ttl":                      fmt.Sprintf("%dh0m0s", b.RetentionDays*hoursPerDay),
					"storageLocation":          veleroLocation,
					"excludedNamespaces":       backupsExcluded(charts.PodSecurityExempt),
					"excludedResources":        backupsExcludedResources,
					"defaultVolumesToFsBackup": all,
					"snapshotVolumes":          false,
					"resourcePolicy":           map[string]any{"kind": "configmap", "name": policyName},
				},
			},
		},
		"configMaps": map[string]any{
			strings.TrimPrefix(policyName, veleroRelease+"-"): map[string]any{"data": map[string]any{"policy.yaml": veleroPolicy}},
			// The init container that holds a restored pod until its volumes are back, pinned and
			// fit for a namespace held to Pod Security's restricted level.
			"restore-helper": map[string]any{
				"labels": map[string]any{"velero.io/plugin-config": "", "velero.io/pod-volume-restore": "RestoreItemAction"},
				"data": map[string]any{
					"image": charts.VeleroImage.String(), "cpuRequest": cpu10m, "memRequest": "16Mi", "memLimit": memory64Mi,
					"secCtx": fmt.Sprintf("runAsNonRoot: true\nrunAsUser: %d\nrunAsGroup: %d\nallowPrivilegeEscalation: false\n"+
						"capabilities:\n  drop: [ALL]\nseccompProfile:\n  type: RuntimeDefault\n", veleroUser, veleroGroup),
				},
			},
		},
	}
}

// backupTarget finds the target a cluster's backups name in its class, or false when the class has
// none of that name.
func (r *Reconciler) backupTarget(ctx context.Context, cluster *clusterv1.Cluster, name string) (BackupTarget, bool, error) {
	return classTarget(ctx, r.Client, r.ClassNamespace, cluster, name)
}

// classTarget finds a target by name in the cluster's class, or false when the class has none of
// that name.
func classTarget(ctx context.Context, c client.Reader, classNamespace string, cluster *clusterv1.Cluster, name string) (BackupTarget, bool, error) {
	class := &clusterv1.ClusterClass{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: classNamespace, Name: cluster.Spec.Topology.ClassRef.Name}, class); err != nil {
		return BackupTarget{}, false, fmt.Errorf("reading the cluster's ClusterClass: %w", err)
	}
	targets, err := BackupTargets(class)
	if err != nil {
		return BackupTarget{}, false, err
	}
	i := slices.IndexFunc(targets, func(t BackupTarget) bool { return t.Name == name })
	if i < 0 {
		return BackupTarget{}, false, nil
	}
	return targets[i], true, nil
}

// holdBackups reports on the cluster why its backups are neither installed nor removed.
func (r *Reconciler) holdBackups(ctx context.Context, cluster *clusterv1.Cluster, reason, message string) {
	ctrl.LoggerFrom(ctx).Info("holding backups: " + message)
	if r.Events != nil {
		r.Events.Eventf(cluster, nil, corev1.EventTypeWarning, reason, "InstallBackups", "%s", message)
	}
}

// backupKeys reads the cluster's backup keys, making them first when create is set and there are
// none.
func (r *Reconciler) backupKeys(ctx context.Context, cluster *clusterv1.Cluster, create bool) (*corev1.Secret, error) {
	return clusterKeys(ctx, r.Client, cluster, create)
}

// clusterKeys reads the cluster's backup keys, making them first when create is set and there are
// none: a random Kopia password and SSE-C key for Velero, and the age identity etcd snapshots are
// encrypted to, made once and never changed, since what was stored with them can be read with
// nothing else.
func clusterKeys(ctx context.Context, c client.Client, cluster *clusterv1.Cluster, create bool) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: BackupKeysSecretName(cluster.Name)}, s)
	switch {
	case err == nil:
		if len(s.Data[BackupKeysKeyRepository]) == 0 || len(s.Data[BackupKeysKeySSEC]) != sseCKeyBytes || len(s.Data[BackupKeysKeyEtcd]) == 0 {
			return nil, fmt.Errorf("the Secret %s holds no backup keys of the cluster", BackupKeysSecretName(cluster.Name))
		}
		return s, nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("reading the cluster's backup keys: %w", err)
	case !create:
		return nil, nil //nolint:nilnil // none yet is no error
	}
	password := make([]byte, sseCKeyBytes)
	key := make([]byte, sseCKeyBytes)
	if _, err = rand.Read(password); err != nil {
		return nil, fmt.Errorf("making a repository password: %w", err)
	}
	if _, err = rand.Read(key); err != nil {
		return nil, fmt.Errorf("making an SSE-C key: %w", err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("making an etcd snapshot key: %w", err)
	}
	s = &corev1.Secret{
		Namespace: cluster.Namespace, Name: BackupKeysSecretName(cluster.Name),
		Labels: map[string]string{clusterv1.ClusterNameLabel: cluster.Name},
		Type:   corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			BackupKeysKeyRepository: []byte(base64.RawURLEncoding.EncodeToString(password)),
			BackupKeysKeySSEC:       key,
			BackupKeysKeyEtcd:       []byte(identity.String()),
		},
	}
	if err := c.Create(ctx, s); err != nil {
		return nil, fmt.Errorf("storing the cluster's backup keys: %w", err)
	}
	return s, nil
}

// BackupCredentialNames are the credentials beside a cluster that may hold its store's keys, in the
// order they are looked for: the one typed into the interface for it, then the one for every
// cluster.
func BackupCredentialNames(cluster string) []string {
	return []string{CredentialName(cluster, backupCredentialPurpose), CredentialName("", backupCredentialPurpose)}
}

// backupCredential finds the store's keys for the cluster among its credentials. The message says
// what is missing, or is empty.
func (r *Reconciler) backupCredential(ctx context.Context, cluster *clusterv1.Cluster) (map[string][]byte, string, error) {
	return backupCredential(ctx, r.Client, cluster)
}

// backupCredential finds the store's keys for the cluster among its credentials in the management
// cluster. The message says what is missing, or is empty.
func backupCredential(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) (map[string][]byte, string, error) {
	for _, name := range BackupCredentialNames(cluster.Name) {
		s := &corev1.Secret{}
		err := c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, s)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("reading the backup credential %s: %w", name, err)
		}
		if len(s.Data[backupAccessKeyID]) == 0 || len(s.Data[backupSecretAccessKey]) == 0 {
			return nil, fmt.Sprintf("Secret %s holds no %s and %s", name, backupAccessKeyID, backupSecretAccessKey), nil
		}
		return s.Data, "", nil
	}
	return nil, noBackupCredential, nil
}

// noBackupCredential is what backupCredential says when the cluster has no credential at all.
const noBackupCredential = "no credential for the backup target: type one in for the cluster in the interface, or have an administrator store one for every cluster" //nolint:gosec // a message, not a credential

// backupsWait puts what Velero needs into the workload cluster before its chart: the namespace, the
// repository password, the SSE-C key and, unless the cluster's owner keeps them in a Secret of
// their own there, the store's keys. Velero would make a repository password of its own, the same
// for every installation, were it to start without one. It reports whether the chart has to wait,
// for the cluster's API or for a credential.
func (r *Reconciler) backupsWait(ctx context.Context, cluster *clusterv1.Cluster, b *Backups) (bool, error) {
	keys, err := r.backupKeys(ctx, cluster, true)
	if err != nil {
		return false, err
	}
	var creds map[string][]byte
	if b.ExistingSecret == "" {
		var missing string
		creds, missing, err = r.backupCredential(ctx, cluster)
		if err != nil {
			return false, err
		}
		if missing == noBackupCredential {
			// Velero alone can also take a Secret of the owner's own in the workload cluster.
			missing += ", or name a Secret of the cluster's velero namespace in existingSecret"
		}
		if missing != "" {
			r.holdBackups(ctx, cluster, "BackupCredentialMissing", missing)
			return true, nil
		}
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return true, err
	}
	if keys.Annotations[backupsInstalledAnnotation] != strconv.FormatBool(true) {
		patch := client.MergeFrom(keys.DeepCopy())
		metav1.SetMetaDataAnnotation(&keys.ObjectMeta, backupsInstalledAnnotation, strconv.FormatBool(true))
		if err := r.Client.Patch(ctx, keys, patch); err != nil {
			return false, fmt.Errorf("marking the backup keys: %w", err)
		}
	}
	secrets := map[string]map[string][]byte{
		veleroRepoSecret: {veleroRepoKey: keys.Data[BackupKeysKeyRepository]},
		veleroSSECSecret: {veleroSSECKey: keys.Data[BackupKeysKeySSEC]},
	}
	if creds != nil {
		secrets[veleroCredentialsSecret] = map[string][]byte{veleroCredentialsKey: fmt.Appendf(nil,
			"[default]\naws_access_key_id=%s\naws_secret_access_key=%s\n",
			strings.TrimSpace(string(creds[backupAccessKeyID])), strings.TrimSpace(string(creds[backupSecretAccessKey])))}
	}
	if err := putVeleroSecrets(ctx, workload.client, secrets); err != nil {
		r.Remotes.forget(cluster)
		return false, err
	}
	if creds == nil {
		// Keys put there before the owner named a Secret of their own would outlive their use.
		stale := &corev1.Secret{Namespace: VeleroNamespace, Name: veleroCredentialsSecret}
		if err := workload.client.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			r.Remotes.forget(cluster)
			return false, fmt.Errorf("deleting Secret %s/%s: %w", VeleroNamespace, veleroCredentialsSecret, err)
		}
	}
	return false, nil
}

// putVeleroSecrets creates the velero namespace and puts the Secrets into it as given.
func putVeleroSecrets(ctx context.Context, workload client.Client, secrets map[string]map[string][]byte) error {
	ns := &corev1.Namespace{Name: VeleroNamespace}
	if err := workload.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the velero namespace: %w", err)
	}
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		data := secrets[name]
		s := &corev1.Secret{Namespace: VeleroNamespace, Name: name}
		_, err := controllerutil.CreateOrUpdate(ctx, workload, s, func() error {
			s.Type = corev1.SecretTypeOpaque
			s.Data = data
			return nil
		})
		if err != nil {
			return fmt.Errorf("applying Secret %s/%s: %w", VeleroNamespace, name, err)
		}
	}
	return nil
}

// removeBackups takes Velero out of a cluster that switched backups off: its release, then what the
// release leaves, Velero's resources and their definitions and the velero namespace with its keys.
// The backups stay in the target, and the keys beside the Cluster. It reports whether it waits.
func (r *Reconciler) removeBackups(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	keys, err := r.backupKeys(ctx, cluster, false)
	if err != nil || keys == nil || keys.Annotations[backupsInstalledAnnotation] != strconv.FormatBool(true) {
		return false, err
	}
	if err = r.removeProxies(ctx, cluster, veleroProxy(cluster)); err != nil {
		return false, err
	}
	if there, perr := r.anyProxy(ctx, cluster, veleroProxy(cluster)); perr != nil || there {
		return there, perr
	}
	workload, found, err := r.Remotes.get(ctx, cluster)
	if err != nil || !found {
		return true, err
	}
	gone, err := removeVelero(ctx, workload.client)
	if err != nil {
		r.Remotes.forget(cluster)
		return false, err
	}
	if !gone {
		return true, nil
	}
	patch := client.MergeFrom(keys.DeepCopy())
	delete(keys.Annotations, backupsInstalledAnnotation)
	if err := r.Client.Patch(ctx, keys, patch); err != nil {
		return false, fmt.Errorf("unmarking the backup keys: %w", err)
	}
	return false, nil
}

// veleroAPIGroup is the API group of Velero's resources.
const veleroAPIGroup = "velero.io"

// removeVelero deletes Velero's resource definitions, with their resources once their finalizers
// are taken off, which nothing would take off with Velero gone, and the velero namespace. It
// reports whether all of it is gone.
func removeVelero(ctx context.Context, workload client.Client) (bool, error) {
	crds := &unstructured.UnstructuredList{}
	crds.SetGroupVersionKind(crdGVK.GroupVersion().WithKind(crdGVK.Kind + "List"))
	if err := workload.List(ctx, crds); err != nil {
		return false, fmt.Errorf("listing the resource definitions: %w", err)
	}
	left := false
	for i := range crds.Items {
		crd := &crds.Items[i]
		group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
		if group != veleroAPIGroup {
			continue
		}
		left = true
		if err := unfinalize(ctx, workload, crd); err != nil {
			return false, err
		}
		if err := client.IgnoreNotFound(workload.Delete(ctx, crd)); err != nil {
			return false, fmt.Errorf("deleting %s: %w", crd.GetName(), err)
		}
	}
	ns := &corev1.Namespace{}
	err := workload.Get(ctx, client.ObjectKey{Name: VeleroNamespace}, ns)
	switch {
	case apierrors.IsNotFound(err):
		return !left, nil
	case err != nil:
		return false, fmt.Errorf("reading the velero namespace: %w", err)
	}
	if ns.DeletionTimestamp.IsZero() {
		if err := client.IgnoreNotFound(workload.Delete(ctx, ns)); err != nil {
			return false, fmt.Errorf("deleting the velero namespace: %w", err)
		}
	}
	return false, nil
}

// unfinalize takes the finalizers off every resource of a definition.
func unfinalize(ctx context.Context, workload client.Client, crd *unstructured.Unstructured) error {
	kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	version := ""
	for _, v := range versions {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if stored, isBool := m["storage"].(bool); isBool && stored {
			if name, isText := m["name"].(string); isText {
				version = name
			}
		}
	}
	if kind == "" || version == "" {
		return nil
	}
	gvk := schema.GroupVersionKind{Group: veleroAPIGroup, Version: version, Kind: kind}
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(kind + "List"))
	if err := workload.List(ctx, list); err != nil {
		return fmt.Errorf("listing %s: %w", kind, err)
	}
	unfinalized := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))
	for i := range list.Items {
		o := &list.Items[i]
		if len(o.Finalizers) == 0 {
			continue
		}
		o.SetGroupVersionKind(gvk)
		if err := client.IgnoreNotFound(workload.Patch(ctx, o, unfinalized)); err != nil {
			return fmt.Errorf("taking the finalizers off %s %s: %w", kind, client.ObjectKeyFromObject(o), err)
		}
	}
	return nil
}
