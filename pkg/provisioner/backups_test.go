package provisioner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

// testTargets are the backup targets the test classes offer.
const testTargets = `[{"name":"store","endpoint":"https://s3.example.org:8333","bucket":"backups","prefix":"site/a","caCertificate":"-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n","checksums":false},` +
	`{"name":"aws","bucket":"aws-backups","region":"eu-central-2"}]`

func backupsCluster(raw string) *clusterv1.Cluster {
	return withAddons(storageCluster(), map[string]string{"backups": raw})
}

func targetClass() *clusterv1.ClusterClass {
	return &clusterv1.ClusterClass{Name: "kubehenge-rke2", Annotations: map[string]string{BackupTargetsAnnotation: testTargets}}
}

func TestCheckBackups(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"off", `{"enabled":false,"target":"gone"}`, ""},
		{"defaults", `{"enabled":true,"target":"store"}`, ""},
		{"own schedule", `{"enabled":true,"target":"aws","schedule":"30 1 * * 0","timeZone":"Europe/Zurich"}`, ""},
		{"descriptor", `{"enabled":true,"target":"aws","schedule":"@weekly"}`, ""},
		{"unknown target", `{"enabled":true,"target":"other"}`, `backup target "other"`},
		{"bad schedule", `{"enabled":true,"target":"store","schedule":"61 * * * *"}`, "schedule"},
		{"zone in the schedule", `{"enabled":true,"target":"store","schedule":"CRON_TZ=UTC 0 2 * * *"}`, "timeZone"},
		{"bad zone", `{"enabled":true,"target":"store","timeZone":"Mars/Olympus"}`, "time zone"},
		{"template in the zone", `{"enabled":true,"target":"store","timeZone":"{{ .Cluster }}"}`, "time zone"},
		{"bad volumes", `{"enabled":true,"target":"store","volumes":"some"}`, "volumes"},
		{"own Secret", `{"enabled":true,"target":"store","existingSecret":"store-keys"}`, ""},
		{"Kubehenge's Secret", `{"enabled":true,"target":"store","existingSecret":"velero-sse-c"}`, "writes Secret velero-sse-c"},
	} {
		err := checkBackups(nil, backupsCluster(tc.raw), targetClass())
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
}

// A target an administrator took away holds no cluster that already backs up to it from other
// edits, but no cluster may switch to it.
func TestCheckBackupsKeepsATargetThatWent(t *testing.T) {
	t.Parallel()
	was := backupsCluster(`{"enabled":true,"target":"old"}`)
	if err := checkBackups(was, backupsCluster(`{"enabled":true,"target":"old","retentionDays":14}`), targetClass()); err != nil {
		t.Errorf("an unrelated edit refused: %v", err)
	}
	if err := checkBackups(backupsCluster(`{"enabled":false,"target":"old"}`), was, targetClass()); err == nil {
		t.Error("switched on to a target that is gone")
	}
	if err := checkBackups(nil, was, &clusterv1.ClusterClass{}); err == nil || !strings.Contains(err.Error(), "set none") {
		t.Errorf("a class without targets: %v", err)
	}
}

func TestBackupPrefix(t *testing.T) {
	t.Parallel()
	if got := BackupPrefix("site/a", "team-a", "web"); got != "site/a/team-a.web/velero" {
		t.Errorf("under a prefix: %s", got)
	}
	if got := BackupPrefix("", "team-a", "web"); got != "team-a.web/velero" {
		t.Errorf("without one: %s", got)
	}
}

func veleroProxyValues(t *testing.T, raw string, target *BackupTarget) map[string]any {
	t.Helper()
	c := backupsCluster(raw)
	addons, err := addonsOf(c)
	if err != nil {
		t.Fatal(err)
	}
	addons.backupTarget = target
	charts := testCharts
	charts.PodSecurityExempt = PodSecurityExempt{Always: []string{"kube-system"}, Calico: []string{"calico-system"}, Backups: []string{VeleroNamespace}}
	proxies, err := helmChartProxies(c, &charts, "rke2", "local-lvm", CNI{}, &addons)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range proxies {
		if p.GetName() != veleroProxy(c) {
			continue
		}
		if ns := lookup[string](t, p.Object, "spec", "namespace"); ns != VeleroNamespace {
			t.Errorf("released into %s", ns)
		}
		values := map[string]any{}
		if err := yaml.Unmarshal([]byte(lookup[string](t, p.Object, "spec", "valuesTemplate")), &values); err != nil {
			t.Fatal(err)
		}
		return values
	}
	t.Fatal("no Velero proxy")
	return nil
}

func TestVeleroValues(t *testing.T) {
	t.Parallel()
	off := false
	target := BackupTarget{
		Name: "store", Endpoint: "https://s3.example.org:8333", Bucket: "backups", Prefix: "site/a",
		CACertificate: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n", Checksums: &off,
	}
	v := veleroProxyValues(t, `{"enabled":true,"target":"store","timeZone":"Europe/Zurich","retentionDays":30}`, &target)
	if tag := lookup[string](t, v, "image", "tag"); tag != "v1.18.4@sha256:c89fb5b6d1fd6afd368e0f483e6f5555fd62851a1eada8ac5c72482e674ca17b" {
		t.Errorf("Velero's image tag %s", tag)
	}
	if plugin := lookup[string](t, v, "initContainers", 0, "image"); !strings.HasPrefix(plugin, "docker.io/velero/velero-plugin-for-aws:v1.14.4@sha256:") {
		t.Errorf("the plugin %s", plugin)
	}
	location := lookup[map[string]any](t, v, "configuration", "backupStorageLocation", 0)
	if location["prefix"] != "site/a/team-a.web/velero" || location["bucket"] != "backups" || !lookup[bool](t, location, "default") {
		t.Errorf("location %v", location)
	}
	if ca, _ := base64.StdEncoding.DecodeString(lookup[string](t, location, "caCert")); string(ca) != target.CACertificate {
		t.Errorf("CA %q", ca)
	}
	config := lookup[map[string]any](t, location, "config")
	for key, want := range map[string]any{ //nolint:gosec // the names of the store's settings, not credentials
		"s3Url": target.Endpoint, "s3ForcePathStyle": "true", "region": defaultBackupRegion,
		"checksumAlgorithm": "", "customerKeyEncryptionSecret": "velero-sse-c/key",
	} {
		if config[key] != want {
			t.Errorf("config %s = %v, want %v", key, config[key], want)
		}
	}
	schedule := lookup[map[string]any](t, v, "schedules", "scheduled")
	if schedule["schedule"] != "CRON_TZ=Europe/Zurich 0 2 * * *" {
		t.Errorf("schedule %v", schedule["schedule"])
	}
	tmpl := lookup[map[string]any](t, schedule, "template")
	if tmpl["ttl"] != "720h0m0s" || !lookup[bool](t, tmpl, "defaultVolumesToFsBackup") || lookup[bool](t, tmpl, "snapshotVolumes") {
		t.Errorf("template %v", tmpl)
	}
	excluded := lookup[[]any](t, tmpl, "excludedNamespaces")
	for _, ns := range []string{"kube-system", "velero", "monitoring", "calico-system"} {
		found := false
		for _, e := range excluded {
			found = found || e == ns
		}
		if !found {
			t.Errorf("%s is backed up", ns)
		}
	}
	if !lookup[bool](t, v, "podSecurityContext", "runAsNonRoot") || lookup[bool](t, v, "containerSecurityContext", "allowPrivilegeEscalation") {
		t.Errorf("the server runs with more than it needs: %v", v["podSecurityContext"])
	}
	if lookup[string](t, v, "nodeAgent", "podSecurityContext", "seLinuxOptions", "type") != "spc_t" {
		t.Errorf("the node agent cannot find the pods' volumes on an SELinux node: %v", v["nodeAgent"])
	}
	// The node agents read the configuration the chart makes, which has their backup pods read
	// the volumes through the host.
	if lookup[string](t, v, "nodeAgent", "extraArgs", 0) != "--node-agent-configmap=velero-node-agent-config" ||
		lookup[string](t, v, "configMaps", "node-agent-config", "data", "config.json") != `{"privilegedFsBackup":true}` {
		t.Errorf("the backup pods cannot read the volumes on an SELinux node: %v %v", v["nodeAgent"], v["configMaps"])
	}
	helper := lookup[map[string]any](t, v, "configMaps", "restore-helper", "data")
	if !strings.Contains(fmt.Sprint(helper["image"]), "@sha256:") || !strings.Contains(fmt.Sprint(helper["secCtx"]), "runAsNonRoot: true") {
		t.Errorf("restore helper %v", helper)
	}
	if !lookup[bool](t, v, "credentials", "useSecret") || lookup[string](t, v, "credentials", "existingSecret") != veleroCredentialsSecret {
		t.Errorf("credentials %v", v["credentials"])
	}
}

func TestVeleroValuesForAWSAndAnnotatedVolumes(t *testing.T) {
	t.Parallel()
	v := veleroProxyValues(t, `{"enabled":true,"target":"aws","volumes":"annotated"}`, &BackupTarget{Name: "aws", Bucket: "aws-backups", Region: "eu-central-2"})
	config := lookup[map[string]any](t, v, "configuration", "backupStorageLocation", 0, "config")
	if _, ok := config["s3Url"]; ok || config["region"] != "eu-central-2" {
		t.Errorf("config %v", config)
	}
	if _, ok := config["checksumAlgorithm"]; ok {
		t.Error("checksums switched off for a store that takes them")
	}
	own := veleroProxyValues(t, `{"enabled":true,"target":"aws","existingSecret":"store-keys"}`, &BackupTarget{Name: "aws", Bucket: "aws-backups"})
	if got := lookup[string](t, own, "credentials", "existingSecret"); got != "store-keys" {
		t.Errorf("Velero reads the store's keys from %s, not the owner's Secret", got)
	}
	if lookup[bool](t, v, "configuration", "defaultVolumesToFsBackup") {
		t.Error("every volume backed up though only annotated ones should be")
	}
	if ttl := lookup[string](t, v, "schedules", "scheduled", "template", "ttl"); ttl != "168h0m0s" {
		t.Errorf("default retention %s", ttl)
	}
}

// storeKeys is a backup credential as the interface stores it.
func storeKeys(name, cluster string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{Namespace: "team-a", Name: name, Labels: map[string]string{CredentialLabel: "backup"}}
	if cluster != "" {
		s.Labels[CredentialClusterLabel] = cluster
	}
	s.Data = map[string][]byte{}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

var backupKeysData = map[string]string{"accessKeyID": "AKID", "secretAccessKey": "secret"}

func TestBackupCredential(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		raw     string
		objs    []client.Object
		key     string
		missing string
	}{
		{"the cluster's own", `{"enabled":true}`, []client.Object{
			storeKeys("web.backup", "web", map[string]string{"accessKeyID": "OWN", "secretAccessKey": "s"}),
			storeKeys("kubehenge-backup", "", backupKeysData),
		}, "OWN", ""},
		{"the appliance's", `{"enabled":true}`, []client.Object{storeKeys("kubehenge-backup", "", backupKeysData)}, "AKID", ""},
		{"none", `{"enabled":true}`, nil, "", "no credential"},
		{"another cluster's", `{"enabled":true}`, []client.Object{storeKeys("db.backup", "db", backupKeysData)}, "", "no credential"},
		{"without keys", `{"enabled":true}`, []client.Object{storeKeys("web.backup", "web", map[string]string{"token": "x"})}, "", "holds no"},
	} {
		r, _ := newReconciler(t, &fakeCloud{}, tc.objs...)
		c := backupsCluster(tc.raw)
		data, missing, err := backupCredential(context.Background(), r.Client, c)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(data[backupAccessKeyID]) != tc.key || (tc.missing == "") != (missing == "") || !strings.Contains(missing, tc.missing) {
			t.Errorf("%s: key %q, missing %q", tc.name, data[backupAccessKeyID], missing)
		}
	}
}

// The keys that go into the cluster are only its own: the appliance's reach every cluster's backups.
func TestClusterBackupCredentialIsOnlyTheClusters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		key     string
		missing string
	}{
		{"the cluster's own", []client.Object{
			storeKeys("web.backup", "web", map[string]string{"accessKeyID": "OWN", "secretAccessKey": "s"}),
			storeKeys("kubehenge-backup", "", backupKeysData),
		}, "OWN", ""},
		{"the appliance's alone", []client.Object{storeKeys("kubehenge-backup", "", backupKeysData)}, "", "never stands in"},
		{"another cluster's", []client.Object{storeKeys("db.backup", "db", backupKeysData)}, "", "of the cluster's own"},
		{"without keys", []client.Object{storeKeys("web.backup", "web", map[string]string{"token": "x"})}, "", "holds no"},
	} {
		r, _ := newReconciler(t, &fakeCloud{}, tc.objs...)
		data, missing, err := r.clusterBackupCredential(context.Background(), backupsCluster(`{"enabled":true}`))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(data[backupAccessKeyID]) != tc.key || (tc.missing == "") != (missing == "") || !strings.Contains(missing, tc.missing) {
			t.Errorf("%s: key %q, missing %q", tc.name, data[backupAccessKeyID], missing)
		}
	}
}

// The keys are made once, kept beside the Cluster without it owning them, and Velero's chart waits
// until its Secrets are in the cluster.
func TestReconcileMakesBackupKeysOnceAndHoldsVeleroBack(t *testing.T) {
	t.Parallel()
	c := backupsCluster(`{"enabled":true,"target":"store"}`)
	r, kube := newReconciler(t, &fakeCloud{}, c, storeKeys("web.backup", "web", backupKeysData))
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	keys := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-backup-keys"}, keys); err != nil {
		t.Fatal(err)
	}
	if len(keys.Data[BackupKeysKeySSEC]) != 32 || len(keys.Data[BackupKeysKeyRepository]) < 32 || len(keys.OwnerReferences) != 0 {
		t.Errorf("keys %d and %d bytes, owners %v", len(keys.Data[BackupKeysKeySSEC]), len(keys.Data[BackupKeysKeyRepository]), keys.OwnerReferences)
	}
	first := string(keys.Data[BackupKeysKeySSEC])
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(keys), keys); err != nil || string(keys.Data[BackupKeysKeySSEC]) != first {
		t.Errorf("the SSE-C key changed: %v", err)
	}
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(helmChartProxyGVK)
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-velero"}, hcp); !apierrors.IsNotFound(err) {
		t.Errorf("Velero's chart applied before its Secrets are in the cluster: %v", err)
	}
}

func TestReconcileHoldsBackupsToATargetThatWent(t *testing.T) {
	t.Parallel()
	c := backupsCluster(`{"enabled":true,"target":"gone"}`)
	r, kube := newReconciler(t, &fakeCloud{}, c)
	if err := reconcile(t, r, c); err != nil {
		t.Fatal(err)
	}
	keys := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "web-backup-keys"}, keys); !apierrors.IsNotFound(err) {
		t.Errorf("keys made for a target that is not there: %v", err)
	}
}

// With a Secret of the owner's own, no credential is needed beside the Cluster, and the keys put
// into the cluster before are taken away.
func TestBackupsWaitWithTheOwnersSecret(t *testing.T) {
	t.Parallel()
	c := backupsCluster(`{"enabled":true,"target":"store","existingSecret":"store-keys"}`)
	r, _ := newReconciler(t, &fakeCloud{}, c)
	// The fake adds Velero's kinds, which it lists, to its scheme: a shared one races other tests.
	workload := fake.NewClientBuilder().WithScheme(newRemoteScheme()).WithObjects(
		&corev1.Secret{Namespace: VeleroNamespace, Name: veleroCredentialsSecret},
		&corev1.Secret{Namespace: VeleroNamespace, Name: "store-keys"},
	).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	b, _, _ := optionalVariable[Backups](c, backupsVariable)
	wait, err := r.backupsWait(context.Background(), c, &b, nil)
	if err != nil || wait {
		t.Fatalf("waits %v: %v", wait, err)
	}
	ctx := context.Background()
	if err := workload.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: veleroSSECSecret}, &corev1.Secret{}); err != nil {
		t.Errorf("no SSE-C key: %v", err)
	}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: veleroCredentialsSecret}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("the keys put there before stay: %v", err)
	}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: "store-keys"}, &corev1.Secret{}); err != nil {
		t.Errorf("the owner's Secret went: %v", err)
	}
}

func TestPutVeleroSecrets(t *testing.T) {
	t.Parallel()
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).WithObjects(
		&corev1.Secret{Namespace: VeleroNamespace, Name: veleroRepoSecret, Data: map[string][]byte{veleroRepoKey: []byte("static-passw0rd")}},
	).Build()
	err := putVeleroSecrets(context.Background(), workload, map[string]map[string][]byte{
		veleroRepoSecret: {veleroRepoKey: []byte("ours")},
		veleroSSECSecret: {veleroSSECKey: []byte(strings.Repeat("k", 32))},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &corev1.Secret{}
	if err := workload.Get(context.Background(), client.ObjectKey{Namespace: VeleroNamespace, Name: veleroRepoSecret}, s); err != nil || string(s.Data[veleroRepoKey]) != "ours" {
		t.Errorf("repository password %q, %v", s.Data[veleroRepoKey], err)
	}
	if err := workload.Get(context.Background(), client.ObjectKey{Name: VeleroNamespace}, &corev1.Namespace{}); err != nil {
		t.Errorf("no velero namespace: %v", err)
	}
}

func TestRemoveVelero(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	restoreGVK := schema.GroupVersionKind{Group: veleroAPIGroup, Version: "v1", Kind: "Restore"}
	for _, gvk := range []schema.GroupVersionKind{crdGVK, restoreGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	crd := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"group": veleroAPIGroup, "names": map[string]any{"kind": "Restore"},
		"versions": []any{map[string]any{"name": "v1", "storage": true}},
	}}}
	crd.SetGroupVersionKind(crdGVK)
	crd.SetName("restores.velero.io")
	other := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"group": "example.org"}}}
	other.SetGroupVersionKind(crdGVK)
	other.SetName("things.example.org")
	restore := &unstructured.Unstructured{}
	restore.SetGroupVersionKind(restoreGVK)
	restore.SetNamespace(VeleroNamespace)
	restore.SetName("r")
	restore.SetFinalizers([]string{"restores.velero.io/external-resources-finalizer"})
	workload := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		crd, other, restore, &corev1.Namespace{Name: VeleroNamespace},
	).WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if list.GetObjectKind().GroupVersionKind().Kind == crdGVK.Kind+"List" {
			if _, full := list.(*unstructured.UnstructuredList); full {
				return errors.New("every resource definition listed in full")
			}
		}
		return c.List(ctx, list, opts...)
	}}).Build()
	gone, err := removeVelero(context.Background(), workload)
	if err != nil || gone {
		t.Fatalf("gone %v at once, %v", gone, err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(restoreGVK)
	if err = workload.Get(context.Background(), client.ObjectKeyFromObject(restore), got); err == nil && len(got.GetFinalizers()) > 0 {
		t.Error("the restore keeps its finalizer")
	}
	if err = workload.Get(context.Background(), client.ObjectKeyFromObject(other), other); err != nil {
		t.Errorf("another group's definition went: %v", err)
	}
	if gone, err = removeVelero(context.Background(), workload); err != nil || !gone {
		t.Errorf("not gone the second time: %v", err)
	}
}
