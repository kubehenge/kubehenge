package provisioner

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the checksum S3 asks for beside an SSE-C key
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testScheme reads Secrets and Cluster API's clusters and classes.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// sourceKeysOf are the backup keys beside the cluster name, with a repository password.
func sourceKeysOf(name, repository string) *corev1.Secret {
	return &corev1.Secret{
		Namespace: "team-a", Name: BackupKeysSecretName(name),
		Data: map[string][]byte{
			BackupKeysKeyRepository: []byte(repository), BackupKeysKeySSEC: []byte(strings.Repeat("k", sseCKeyBytes)),
			BackupKeysKeyEtcd: []byte("AGE-SECRET-KEY-1"),
		},
	}
}

// namedCluster is a cluster of team-a backing up as raw says.
func namedCluster(name, raw string) *clusterv1.Cluster {
	c := backupsCluster(raw)
	c.Name = name
	return c
}

func TestCheckReadFrom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"none", `{"enabled":true,"target":"store"}`, ""},
		{"two", `{"enabled":true,"target":"store","readFrom":["shop","db"]}`, ""},
		{"itself", `{"enabled":true,"target":"store","readFrom":["web"]}`, "itself"},
		{"twice", `{"enabled":true,"target":"store","readFrom":["shop","shop"]}`, "twice"},
		{"not a name", `{"enabled":true,"target":"store","readFrom":["Shop"]}`, "not a cluster's name"},
		{"too many", `{"enabled":true,"target":"store","readFrom":["a","b","c","d","e","f","g","h","i","j","k"]}`, "at most 10"},
		{"a source's Secret", `{"enabled":true,"target":"store","existingSecret":"velero-sse-c-shop"}`, "writes Secret"},
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

func TestCheckBackupSources(t *testing.T) {
	t.Parallel()
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		sourceKeysOf("shop", "same"), namedCluster("shop", `{"enabled":true,"target":"store"}`),
		sourceKeysOf("db", "other"), namedCluster("db", `{"enabled":true,"target":"aws"}`),
		sourceKeysOf("gone", "old"),
		namedCluster("bare", `{"enabled":false}`),
		sourceKeysOf("broken", "same"), namedCluster("broken", `{"enabled":"yes"}`),
	).Build()
	ctx := context.Background()
	for _, tc := range []struct {
		name, raw, before, want string
	}{
		{"same target", `{"enabled":true,"target":"store","readFrom":["shop"]}`, "", ""},
		{"deleted since", `{"enabled":true,"target":"store","readFrom":["gone"]}`, "", ""},
		{"another target", `{"enabled":true,"target":"store","readFrom":["db"]}`, "", "does not back up to target store"},
		{"never backed up", `{"enabled":true,"target":"store","readFrom":["bare"]}`, "", "no backup keys"},
		{"a source's variable unreadable", `{"enabled":true,"target":"store","readFrom":["broken"]}`, "", "whether cluster broken backs up to target store cannot be told"},
		{"read before", `{"enabled":true,"target":"store","readFrom":["db"]}`, `{"enabled":true,"target":"store","readFrom":["db"]}`, ""},
		{"read before on another target", `{"enabled":true,"target":"store","readFrom":["db"]}`, `{"enabled":true,"target":"aws","readFrom":["db"]}`, "does not back up"},
		{"backups off", `{"enabled":false,"readFrom":["bare"]}`, "", ""},
	} {
		var old *clusterv1.Cluster
		if tc.before != "" {
			old = backupsCluster(tc.before)
		}
		err := checkBackupSources(ctx, reader, old, backupsCluster(tc.raw))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
}

func TestVeleroValuesReadOtherClusters(t *testing.T) {
	t.Parallel()
	target := BackupTarget{Name: "store", Endpoint: "https://s3.example.org:8333", Bucket: "backups", Prefix: "site/a"}
	c := backupsCluster(`{"enabled":true,"target":"store","readFrom":["shop","db"]}`)
	b, _, _ := optionalVariable[Backups](c, backupsVariable)
	sources := []backupSource{{name: "shop", volumeData: true}, {name: "db"}}
	v := veleroValues(&testCharts, &b, &target, c, sources)
	locations := lookup[[]any](t, v, "configuration", "backupStorageLocation")
	if len(locations) != 3 {
		t.Fatalf("locations %v", locations)
	}
	own := lookup[map[string]any](t, locations, 0)
	if own["name"] != "default" || own["accessMode"] != nil || lookup[string](t, own, "config", "customerKeyEncryptionSecret") != "velero-sse-c/key" {
		t.Errorf("the cluster's own location %v", own)
	}
	shop := lookup[map[string]any](t, locations, 1)
	if shop["name"] != "cluster-shop" || shop["accessMode"] != "ReadOnly" || shop["prefix"] != "site/a/team-a.shop/velero" || shop[defaultKey] != nil {
		t.Errorf("shop's location %v", shop)
	}
	if lookup[string](t, shop, "config", "customerKeyEncryptionSecret") != "velero-sse-c-shop/key" || lookup[string](t, shop, "config", "s3Url") != target.Endpoint {
		t.Errorf("shop's location's config %v", shop["config"])
	}
	if a := lookup[map[string]any](t, shop, "annotations"); a[BackupSourceAnnotation] != "shop" || a[VolumeDataAnnotation] != "true" {
		t.Errorf("shop's annotations %v", a)
	}
	if a := lookup[map[string]any](t, locations, 2, "annotations"); a[VolumeDataAnnotation] != "false" {
		t.Errorf("db's annotations %v", a)
	}
}

// A cluster whose keys are made while it reads another's backups takes that one's Kopia password,
// so that its Velero reads the volumes' data too; one it may not read is left out, with an event.
func TestBackupSourcesShareTheRepositoryFromTheStart(t *testing.T) {
	t.Parallel()
	c := backupsCluster(`{"enabled":true,"target":"store","readFrom":["gone","shop","db"]}`)
	r, kube := newReconciler(t, &fakeCloud{}, c,
		sourceKeysOf("shop", "shop-password"), namedCluster("shop", `{"enabled":true,"target":"store"}`),
		sourceKeysOf("db", "db-password"), namedCluster("db", `{"enabled":true,"target":"store"}`),
		storeKeys("web.backup", "web", backupKeysData))
	recorder := events.NewFakeRecorder(10)
	r.Events = recorder
	ctx := context.Background()
	keys, err := r.backupKeys(ctx, c, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(keys.Data[BackupKeysKeyRepository]) != "shop-password" || string(keys.Data[BackupKeysKeySSEC]) == strings.Repeat("k", sseCKeyBytes) {
		t.Errorf("keys %q", keys.Data)
	}
	b, _, _ := optionalVariable[Backups](c, backupsVariable)
	sources, err := r.backupSources(ctx, c, &b)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].name != "shop" || !sources[0].volumeData || sources[1].name != "db" || sources[1].volumeData {
		t.Errorf("sources %+v", sources)
	}
	select {
	case e := <-recorder.Events:
		if !strings.Contains(e, "BackupSourceRefused") || !strings.Contains(e, "gone") {
			t.Errorf("event %s", e)
		}
	default:
		t.Error("no event for the source that has no keys")
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "shop-backup-keys"}, &corev1.Secret{}); err != nil {
		t.Errorf("the source's keys went: %v", err)
	}
}

func TestBackupsWaitPutsAndTakesTheSourcesKeys(t *testing.T) {
	t.Parallel()
	c := backupsCluster(`{"enabled":true,"target":"store","readFrom":["shop"]}`)
	r, _ := newReconciler(t, &fakeCloud{}, c, storeKeys("web.backup", "web", backupKeysData))
	backup := func(name, location string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"storageLocation": location}}}
		o.SetGroupVersionKind(veleroBackupGVK)
		o.SetNamespace(VeleroNamespace)
		o.SetName(name)
		return o
	}
	// The fake adds Velero's kinds, which it holds and lists, to its scheme: a shared one races
	// other tests.
	workload := fake.NewClientBuilder().WithScheme(newRemoteScheme()).WithObjects(
		&corev1.Secret{Namespace: VeleroNamespace, Name: "velero-sse-c-old"},
		&corev1.Secret{Namespace: VeleroNamespace, Name: "owners-keys"},
		backup("own", "default"), backup("of-shop", "cluster-shop"), backup("of-old", "cluster-old"),
	).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	b, _, _ := optionalVariable[Backups](c, backupsVariable)
	key := []byte(strings.Repeat("s", sseCKeyBytes))
	wait, err := r.backupsWait(context.Background(), c, &b, []backupSource{{name: "shop", sseC: key}})
	if err != nil || wait {
		t.Fatalf("waits %v: %v", wait, err)
	}
	ctx := context.Background()
	shop := &corev1.Secret{}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: "velero-sse-c-shop"}, shop); err != nil || !bytes.Equal(shop.Data[veleroSSECKey], key) {
		t.Errorf("shop's SSE-C key %q: %v", shop.Data, err)
	}
	for name, kept := range map[string]bool{"velero-sse-c-old": false, "owners-keys": true, veleroSSECSecret: true} {
		err := workload.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: name}, &corev1.Secret{})
		if kept != (err == nil) {
			t.Errorf("Secret %s kept %v: %v", name, !kept, err)
		}
	}
	for name, kept := range map[string]bool{"own": true, "of-shop": true, "of-old": false} {
		err := workload.Get(ctx, client.ObjectKey{Namespace: VeleroNamespace, Name: name}, backup(name, ""))
		if kept != (err == nil) || (!kept && !apierrors.IsNotFound(err)) {
			t.Errorf("backup %s kept %v: %v", name, !kept, err)
		}
	}
}

func TestVeleroDownload(t *testing.T) {
	t.Parallel()
	key := strings.Repeat("k", sseCKeyBytes)
	sum := md5.Sum([]byte(key)) //nolint:gosec // as above
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Key") != base64.StdEncoding.EncodeToString([]byte(key)) ||
			r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Key-Md5") != base64.StdEncoding.EncodeToString(sum[:]) ||
			r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Algorithm") != "AES256" {
			http.Error(w, "no key", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/backups/gone" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "results")
	}))
	t.Cleanup(store.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: store.Certificate().Raw}))
	class := &clusterv1.ClusterClass{
		Name: "kubehenge-rke2", Namespace: "kubehenge-system",
		Annotations: map[string]string{BackupTargetsAnnotation: `[{"name":"store","endpoint":"` + store.URL + `","bucket":"backups","caCertificate":` + jsonString(t, ca) + `}]`},
	}
	c := backupsCluster(`{"enabled":true,"target":"store"}`)
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(class, sourceKeysOf("shop", "p")).Build()
	ts := &ClusterTargets{Client: reader, ClassNamespace: "kubehenge-system"}
	ctx := context.Background()
	body, err := ts.VeleroDownload(ctx, c, "shop", store.URL+"/backups/results?X-Amz-Signature=x")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "results" {
		t.Errorf("read %q", got)
	}
	if _, err := ts.VeleroDownload(ctx, c, "shop", store.URL+"/backups/gone"); !errors.Is(err, ErrNoObject) {
		t.Errorf("a file that is not there: %v", err)
	}
	if _, err := ts.VeleroDownload(ctx, c, "shop", "https://elsewhere.example:8333/backups/results"); err == nil || !strings.Contains(err.Error(), "does not lead") {
		t.Errorf("the key sent elsewhere: %v", err)
	}
	if _, err := ts.VeleroDownload(ctx, c, "web", store.URL+"/backups/results"); err == nil {
		t.Error("read without the cluster's keys")
	}
}

func TestTargetServes(t *testing.T) {
	t.Parallel()
	own := BackupTarget{Endpoint: "https://s3.example.org:8333"}
	aws := BackupTarget{}
	for link, want := range map[string]bool{
		"https://s3.example.org:8333/b/k":                true,
		"https://S3.example.org:8333/b/k":                true,
		"http://s3.example.org:8333/b/k":                 false,
		"https://s3.example.org/b/k":                     false,
		"https://u:p@s3.example.org:8333/b/k":            false,
		"https://s3.example.org.evil:8333/b/k":           false,
		"https://backups.s3.amazonaws.com/k":             false,
		"https://bucket.s3.eu-central-2.amazonaws.com/k": false,
	} {
		if got := own.serves(link); got != want {
			t.Errorf("%s served by its own endpoint: %v", link, got)
		}
	}
	for link, want := range map[string]bool{
		"https://bucket.s3.eu-central-2.amazonaws.com/k": true,
		"https://s3.amazonaws.com/bucket/k":              true,
		"https://amazonaws.com.evil/k":                   false,
	} {
		if got := aws.serves(link); got != want {
			t.Errorf("%s served by AWS: %v", link, got)
		}
	}
}
