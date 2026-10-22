package provisioner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

func TestEtcdUploadData(t *testing.T) {
	t.Parallel()
	creds := map[string][]byte{backupAccessKeyID: []byte(" AKID\n"), backupSecretAccessKey: []byte("secret\n")}
	own := &BackupTarget{
		Name: "store", Endpoint: "https://s3.example.org:8333", Bucket: "backups", Prefix: "site/a",
		CACertificate: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n",
	}
	data, err := etcdUploadData(own, "site/a/team-a.web/rke2-etcd", creds)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"etcd-s3-endpoint": "s3.example.org:8333", "etcd-s3-skip-ssl-verify": "false", "etcd-s3-insecure": "false",
		"etcd-s3-bucket": "backups", "etcd-s3-folder": "site/a/team-a.web/rke2-etcd", "etcd-s3-region": "us-east-1",
		"etcd-s3-access-key": "AKID", "etcd-s3-secret-key": "secret", "etcd-s3-endpoint-ca": own.CACertificate,
	} {
		if got := string(data[k]); got != want {
			t.Errorf("%s is %q, not %q", k, got, want)
		}
	}
	data, err = etcdUploadData(&BackupTarget{Name: "aws", Bucket: "b", Region: "eu-central-2"}, "f", creds)
	if err != nil || string(data["etcd-s3-endpoint"]) != "s3.amazonaws.com" || string(data["etcd-s3-region"]) != "eu-central-2" {
		t.Errorf("AWS: %q %q, %v", data["etcd-s3-endpoint"], data["etcd-s3-region"], err)
	}
	if _, ok := data["etcd-s3-endpoint-ca"]; ok {
		t.Error("a CA for a store the system's CAs cover")
	}
	if _, err := etcdUploadData(&BackupTarget{Name: "plain", Endpoint: "http://s3.example.org", Bucket: "b"}, "f", creds); err == nil {
		t.Error("uploads over plain HTTP")
	}
	if got := EtcdUploadPrefix("", "team-a", "web", templates.K3s); got != "team-a.web/k3s-etcd" {
		t.Errorf("k3s uploads to %s", got)
	}
}

func TestCheckEtcdUpload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, class, raw, want string
	}{
		{"RKE2", "kubehenge-rke2", `{"enabled":true,"target":"store","distributionUpload":true}`, ""},
		{"k3s", "kubehenge-k3s", `{"enabled":true,"target":"store","distributionUpload":true}`, ""},
		{"kubeadm", "kubehenge-kubeadm", `{"enabled":true,"target":"store","distributionUpload":true}`, "kubeadm has not"},
		{"kubeadm without", "kubehenge-kubeadm", `{"enabled":true,"target":"store"}`, ""},
		{"snapshots off", "kubehenge-rke2", `{"enabled":false,"target":"store","distributionUpload":true}`, "which are off"},
	} {
		c := etcdCluster(tc.raw)
		c.Spec.Topology.ClassRef.Name = tc.class
		err := checkEtcdSnapshots(nil, c, targetClass())
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}
}

type uploadSetup struct {
	r              *Reconciler
	mgmt, workload client.Client
	c              *clusterv1.Cluster
}

// uploadFixture is an RKE2 cluster with etcd snapshots to the store and the distribution's own
// upload, the credential for every cluster, and a workload cluster in memory.
func uploadFixture(t *testing.T) uploadSetup {
	t.Helper()
	c := withAddons(storageCluster(), map[string]string{
		etcdSnapshotsVariable: `{"enabled":true,"target":"store","distributionUpload":true}`,
	})
	credential := &corev1.Secret{
		Namespace: c.Namespace, Name: CredentialName("", backupCredentialPurpose),
		Data: map[string][]byte{backupAccessKeyID: []byte("AKID"), backupSecretAccessKey: []byte("secret")},
	}
	r, mgmt := newReconciler(t, &fakeCloud{}, c, credential)
	workload := fake.NewClientBuilder().WithScheme(remoteScheme).Build()
	r.Remotes = &Remotes{byCluster: map[string]remote{remoteKey(c): {client: workload, until: time.Now().Add(time.Hour)}}}
	return uploadSetup{r: r, mgmt: mgmt, workload: workload, c: c}
}

func uploadSecret(t *testing.T, workload client.Client) (*corev1.Secret, bool) {
	t.Helper()
	s := &corev1.Secret{}
	err := workload.Get(context.Background(), client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: EtcdUploadSecret}, s)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return s, true
}

func uploadMarked(t *testing.T, mgmt client.Client, c *clusterv1.Cluster) bool {
	t.Helper()
	keys := &corev1.Secret{}
	if err := mgmt.Get(context.Background(), client.ObjectKey{Namespace: c.Namespace, Name: BackupKeysSecretName(c.Name)}, keys); err != nil {
		t.Fatal(err)
	}
	return keys.Annotations[etcdUploadInstalledAnnotation] == "true"
}

// The servers get the target with its certificate verified and the store's keys, and lose them
// again once the cluster no longer uploads.
func TestReconcileEtcdUpload(t *testing.T) {
	t.Parallel()
	f := uploadFixture(t)
	r, mgmt, workload, c := f.r, f.mgmt, f.workload, f.c
	ctx := context.Background()
	if wait, err := r.reconcileEtcdUpload(ctx, c); err != nil || wait {
		t.Fatalf("waits %v: %v", wait, err)
	}
	s, ok := uploadSecret(t, workload)
	if !ok {
		t.Fatal("no Secret for the servers")
	}
	if s.Type != etcdUploadSecretType || string(s.Data["etcd-s3-folder"]) != "site/a/team-a.web/rke2-etcd" ||
		string(s.Data["etcd-s3-skip-ssl-verify"]) != "false" || string(s.Data["etcd-s3-access-key"]) != "AKID" {
		t.Errorf("Secret %s: %q", s.Type, s.Data)
	}
	if !uploadMarked(t, mgmt, c) {
		t.Error("the keys do not record the Secret put into the cluster")
	}

	off := withAddons(storageCluster(), map[string]string{etcdSnapshotsVariable: `{"enabled":true,"target":"store"}`})
	if wait, err := r.reconcileEtcdUpload(ctx, off); err != nil || wait {
		t.Fatalf("off: waits %v: %v", wait, err)
	}
	if _, ok := uploadSecret(t, workload); ok {
		t.Error("the Secret outlives the upload")
	}
	if uploadMarked(t, mgmt, c) {
		t.Error("the keys still record the Secret")
	}
	// A cluster that never uploaded is not reached at all.
	r.Remotes = nil
	if wait, err := r.reconcileEtcdUpload(ctx, off); err != nil || wait {
		t.Errorf("never on: waits %v: %v", wait, err)
	}
}

// controlPlaneMachine is a control-plane Machine of the cluster with the bootstrap data given, none
// for an empty one.
func controlPlaneMachine(t *testing.T, mgmt client.Client, c *clusterv1.Cluster, name, bootstrap string) *clusterv1.Machine {
	t.Helper()
	ctx := context.Background()
	m := &clusterv1.Machine{
		Namespace: c.Namespace, Name: name,
		Labels: map[string]string{clusterv1.ClusterNameLabel: c.Name, clusterv1.MachineControlPlaneLabel: ""},
	}
	if bootstrap != "" {
		m.Spec.Bootstrap.DataSecretName = &name
		data := &corev1.Secret{Namespace: c.Namespace, Name: name, Data: map[string][]byte{"value": []byte(bootstrap)}}
		if err := mgmt.Create(ctx, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgmt.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Switched off, the upload's Secret stays while any server may still run with the drop-in, since
// the roll that takes it away replaces the servers one at a time and each reads the Secret on
// every scheduled snapshot.
func TestEtcdUploadOutlivesTheServersThatUpload(t *testing.T) {
	t.Parallel()
	f := uploadFixture(t)
	r, mgmt, workload, c := f.r, f.mgmt, f.workload, f.c
	ctx := context.Background()
	if _, err := r.reconcileEtcdUpload(ctx, c); err != nil {
		t.Fatal(err)
	}
	const (
		with    = "#cloud-config\nwrite_files:\n- path: /etc/rancher/rke2/config.yaml.d/" + etcdUploadDropIn + "\n"
		without = "#cloud-config\nwrite_files:\n- path: /etc/rancher/rke2/config.yaml.d/50-kubehenge.yaml\n"
	)
	old := controlPlaneMachine(t, mgmt, c, "web-cp-old", with)
	controlPlaneMachine(t, mgmt, c, "web-cp-new", without)
	off := withAddons(storageCluster(), map[string]string{etcdSnapshotsVariable: `{"enabled":true,"target":"store"}`})
	if wait, err := r.reconcileEtcdUpload(ctx, off); err != nil || !wait {
		t.Fatalf("an old server left: waits %v: %v", wait, err)
	}
	if _, ok := uploadSecret(t, workload); !ok || !uploadMarked(t, mgmt, c) {
		t.Fatal("the Secret went while a server still uploads")
	}

	if err := mgmt.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	pending := controlPlaneMachine(t, mgmt, c, "web-cp-pending", "")
	if wait, err := r.reconcileEtcdUpload(ctx, off); err != nil || !wait {
		t.Fatalf("a server without bootstrap data yet: waits %v: %v", wait, err)
	}
	if _, ok := uploadSecret(t, workload); !ok {
		t.Fatal("the Secret went before a new server's bootstrap data showed it does not upload")
	}

	if err := mgmt.Delete(ctx, pending); err != nil {
		t.Fatal(err)
	}
	// A machine whose bootstrap data has gone has no VM left either.
	controlPlaneMachine(t, mgmt, c, "web-cp-gone", "x")
	if err := mgmt.Delete(ctx, &corev1.Secret{Namespace: c.Namespace, Name: "web-cp-gone"}); err != nil {
		t.Fatal(err)
	}
	if wait, err := r.reconcileEtcdUpload(ctx, off); err != nil || wait {
		t.Fatalf("every server replaced: waits %v: %v", wait, err)
	}
	if _, ok := uploadSecret(t, workload); ok {
		t.Error("the Secret outlives the servers that read it")
	}
	if uploadMarked(t, mgmt, c) {
		t.Error("the keys still record the Secret")
	}
}

// The drop-in the controller looks for in the servers' bootstrap data is the one the classes write.
func TestEtcdUploadDropInIsTheClasses(t *testing.T) {
	t.Parallel()
	for _, class := range []string{"rke2", "k3s"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "kubehenge", "templates", class+".yaml")) //nolint:gosec // the repository's own chart
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "config.yaml.d/"+etcdUploadDropIn) {
			t.Errorf("the %s class writes no %s", class, etcdUploadDropIn)
		}
	}
}

// Without the store's keys, or on a distribution without an upload of its own, nothing goes into
// the cluster.
func TestReconcileEtcdUploadHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := uploadFixture(t)
	r, workload, c := f.r, f.workload, f.c
	if err := r.Client.Delete(ctx, &corev1.Secret{Namespace: c.Namespace, Name: CredentialName("", backupCredentialPurpose)}); err != nil {
		t.Fatal(err)
	}
	if wait, err := r.reconcileEtcdUpload(ctx, c); err != nil || wait {
		t.Fatalf("waits %v: %v", wait, err)
	}
	if _, ok := uploadSecret(t, workload); ok {
		t.Error("a Secret without the store's keys")
	}

	f = uploadFixture(t)
	r, workload, c = f.r, f.workload, f.c
	c.Spec.Topology.ClassRef.Name = "kubehenge-kubeadm"
	if wait, err := r.reconcileEtcdUpload(ctx, c); err != nil || wait {
		t.Fatalf("kubeadm: waits %v: %v", wait, err)
	}
	if _, ok := uploadSecret(t, workload); ok {
		t.Error("a Secret for kubeadm")
	}

	// Until the cluster's API answers, it waits.
	f = uploadFixture(t)
	r, c = f.r, f.c
	r.Remotes = &Remotes{Reader: r.Client, byCluster: map[string]remote{}}
	if wait, err := r.reconcileEtcdUpload(ctx, c); err != nil || !wait {
		t.Errorf("no kubeconfig yet: waits %v: %v", wait, err)
	}
}
