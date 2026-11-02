package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// uiStore is a bucket in memory.
type uiStore struct {
	mu      sync.Mutex
	objects map[string]provisioner.StoredObject
	data    map[string][]byte
}

func (s *uiStore) Put(_ context.Context, key string, r io.Reader, metadata map[string]string) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = provisioner.StoredObject{Key: key, Size: int64(len(b)), Metadata: metadata}
	s.data[key] = b
	return int64(len(b)), nil
}

func (s *uiStore) SetMetadata(_ context.Context, key string, metadata map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.objects[key]
	o.Metadata = metadata
	s.objects[key] = o
	return nil
}

func (s *uiStore) List(_ context.Context, prefix string) ([]provisioner.StoredObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []provisioner.StoredObject
	for k, o := range s.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *uiStore) Stat(_ context.Context, key string) (provisioner.StoredObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return o, provisioner.ErrNoObject
	}
	return o, nil
}

func (s *uiStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data[key]
	if !ok {
		return nil, provisioner.ErrNoObject
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (*uiStore) Remove(context.Context, string) error { return nil }

func (*uiStore) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

const uiSnapshotFolder = "default.one/etcd"

type etcdHarness struct {
	*harness

	store *uiStore
	plain []byte
	name  string
}

// etcdSnapshotting is cluster one with etcd snapshots to the class's target, its credential and
// keys beside it, its control plane on two machines, and one snapshot in the target.
func etcdSnapshotting(t *testing.T) *etcdHarness {
	t.Helper()
	c := cluster("default", "one")
	c.Spec.Topology.Variables = append(c.Spec.Topology.Variables, clusterv1.ClusterVariable{
		Name: "etcdSnapshots", Value: apiextensionsv1.JSON{Raw: []byte(`{"enabled":true,"target":"store","keep":5}`)},
	})
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	controlPlane := func(name string) *clusterv1.Machine {
		return &clusterv1.Machine{
			Namespace: "default", Name: name,
			Labels: map[string]string{clusterv1.ClusterNameLabel: "one", clusterv1.MachineControlPlaneLabel: ""},
			Status: clusterv1.MachineStatus{NodeRef: clusterv1.MachineNodeReference{Name: name}},
		}
	}
	objs := []client.Object{
		c,
		&corev1.Secret{Namespace: "default", Name: provisioner.CredentialName("one", "backup"), Data: map[string][]byte{
			"accessKeyID": []byte("AKID"), "secretAccessKey": []byte("secret"),
		}},
		&corev1.Secret{Namespace: "default", Name: provisioner.BackupKeysSecretName("one"), Data: map[string][]byte{
			provisioner.BackupKeysKeyRepository: []byte("password"), provisioner.BackupKeysKeySSEC: bytes.Repeat([]byte{1}, 32),
			provisioner.BackupKeysKeyEtcd: []byte(identity.String()),
		}},
		controlPlane("one-cp-a"),
		controlPlane("one-cp-b"),
	}
	h := &etcdHarness{harness: newHarness(t, objs...), store: &uiStore{objects: map[string]provisioner.StoredObject{}, data: map[string][]byte{}}}
	h.setClass(map[string]string{provisioner.BackupTargetsAnnotation: `[{"name":"store","endpoint":"https://s3.example.org","bucket":"backups"}]`})
	h.server.OpenStore = func(target *provisioner.BackupTarget, credential map[string][]byte) (provisioner.ObjectStore, error) {
		if target.Name != "store" || string(credential["accessKeyID"]) != "AKID" {
			return nil, fmt.Errorf("opened %s with %q", target.Name, credential["accessKeyID"])
		}
		return h.store, nil
	}
	h.plain = []byte("an etcd database")
	h.name = h.put(identity, "20261013T060000Z-v1.36.5-rke2r1.db.age", "v1.36.5+rke2r1", h.plain)
	h.put(identity, "20261012T060000Z-v1.36.5-rke2r1.db.age", "v1.36.5+rke2r1", []byte("older"))
	h.put(identity, "20261011T060000Z-v1.35.2-rke2r1.db.age", "v1.35.2+rke2r1", []byte("oldest"))
	return h
}

func (h *etcdHarness) setClass(annotations map[string]string) {
	h.t.Helper()
	class := &clusterv1.ClusterClass{}
	if err := h.kube.Get(h.t.Context(), client.ObjectKey{Namespace: "kubehenge-system", Name: "kubehenge-rke2"}, class); err != nil {
		h.t.Fatal(err)
	}
	if class.Annotations == nil {
		class.Annotations = map[string]string{}
	}
	maps.Copy(class.Annotations, annotations)
	if err := h.kube.Update(h.t.Context(), class); err != nil {
		h.t.Fatal(err)
	}
}

func (h *etcdHarness) put(identity *age.X25519Identity, name, version string, plain []byte) string {
	h.t.Helper()
	var sealed bytes.Buffer
	w, err := age.Encrypt(&sealed, identity.Recipient())
	if err != nil {
		h.t.Fatal(err)
	}
	_, _ = w.Write(plain)
	if err = w.Close(); err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.Sum256(plain)
	_, err = h.store.Put(h.t.Context(), uiSnapshotFolder+"/"+name, &sealed, map[string]string{
		provisioner.SnapshotMetaKubernetes: version, provisioner.SnapshotMetaDistribution: "rke2",
		provisioner.SnapshotMetaSHA256: hex.EncodeToString(sum[:]), provisioner.SnapshotMetaSize: strconv.Itoa(len(plain)),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return name
}

func (h *etcdHarness) cluster() *clusterv1.Cluster {
	h.t.Helper()
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(h.t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		h.t.Fatal(err)
	}
	return c
}

func etcdViewOf(t *testing.T, body []byte) etcdSnapshotsView {
	t.Helper()
	var v etcdSnapshotsView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

func TestEtcdSnapshotsView(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/etcd-snapshots", "")
	expect(t, rec, http.StatusOK)
	v := etcdViewOf(t, rec.Body.Bytes())
	if !v.Enabled || v.Target != "store" || v.Keep != 5 || v.Schedule != "0 */12 * * *" || !v.CanSnapshot || !v.CanRestore || v.Automatic ||
		v.DistributionUpload || v.Message != "" {
		t.Fatalf("view %+v", v)
	}
	names := make([]string, 0, len(v.Snapshots))
	for _, s := range v.Snapshots {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{h.name, "20261012T060000Z-v1.36.5-rke2r1.db.age", "20261011T060000Z-v1.35.2-rke2r1.db.age"}) {
		t.Errorf("snapshots %v", names)
	}
	if v.Snapshots[0].Size != int64(len(h.plain)) || v.Snapshots[0].Distribution != "rke2" {
		t.Errorf("snapshot %+v", v.Snapshots[0])
	}

	viewer := h.browser()
	viewer.signIn("viewer@pve")
	rec = viewer.do("GET", "/api/clusters/default/one/etcd-snapshots", "")
	expect(t, rec, http.StatusOK)
	if v := etcdViewOf(t, rec.Body.Bytes()); v.CanSnapshot || v.CanRestore || len(v.Snapshots) != 3 {
		t.Errorf("a viewer's view %+v", v)
	}
	for _, req := range [][2]string{
		{"POST", "/api/clusters/default/one/etcd-snapshots"},
		{"GET", "/api/clusters/default/one/etcd-snapshots/" + h.name},
		{"GET", "/api/clusters/default/one/etcd-snapshots/" + h.name + "/restore-plan"},
		{"POST", "/api/clusters/default/one/etcd-restores"},
	} {
		expect(t, viewer.do(req[0], req[1], `{"snapshot":"`+h.name+`"}`), http.StatusForbidden)
	}
}

func TestEtcdSnapshotsViewWithTheDistributionsUpload(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	c := &clusterv1.Cluster{}
	if err := h.kube.Get(h.t.Context(), client.ObjectKey{Namespace: "default", Name: "one"}, c); err != nil {
		t.Fatal(err)
	}
	for i := range c.Spec.Topology.Variables {
		if c.Spec.Topology.Variables[i].Name == "etcdSnapshots" {
			c.Spec.Topology.Variables[i].Value.Raw = []byte(`{"enabled":true,"target":"store","distributionUpload":true}`)
		}
	}
	if err := h.kube.Update(h.t.Context(), c); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/etcd-snapshots", "")
	expect(t, rec, http.StatusOK)
	if v := etcdViewOf(t, rec.Body.Bytes()); !v.DistributionUpload {
		t.Errorf("view %+v", v)
	}
}

func TestEtcdSnapshotsViewWithoutATarget(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	if err := h.kube.Delete(h.t.Context(), &corev1.Secret{Namespace: "default", Name: provisioner.CredentialName("one", "backup")}); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/etcd-snapshots", "")
	expect(t, rec, http.StatusOK)
	if v := etcdViewOf(t, rec.Body.Bytes()); len(v.Snapshots) != 0 || !strings.Contains(v.Message, "credential") {
		t.Errorf("view %+v", v)
	}
}

func TestTakeEtcdSnapshot(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	b := h.browser()
	b.signIn("dev@pve")
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-snapshots", ""), http.StatusAccepted)
	raw := h.cluster().Annotations[provisioner.EtcdSnapshotRequestAnnotation]
	var req provisioner.EtcdSnapshotRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil || req.By != "dev@pve" {
		t.Errorf("request %q: %v", raw, err)
	}
	rec := b.do("GET", "/api/clusters/default/one/etcd-snapshots", "")
	if v := etcdViewOf(t, rec.Body.Bytes()); !v.Requested {
		t.Error("the view does not show the request")
	}
}

func TestDownloadEtcdSnapshot(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/etcd-snapshots/"+h.name, "")
	expect(t, rec, http.StatusOK)
	if !bytes.Equal(rec.Body.Bytes(), h.plain) {
		t.Errorf("downloaded %q", rec.Body)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="one-20261013T060000Z-v1.36.5-rke2r1.db"` {
		t.Errorf("named %s", got)
	}
	expect(t, b.do("GET", "/api/clusters/default/one/etcd-snapshots/notes.txt", ""), http.StatusNotFound)
	expect(t, b.do("GET", "/api/clusters/default/one/etcd-snapshots/20260101T000000Z-v1.db.age", ""), http.StatusNotFound)
}

func TestEtcdRestorePlan(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	b := h.browser()
	b.signIn("dev@pve")
	rec := b.do("GET", "/api/clusters/default/one/etcd-snapshots/"+h.name+"/restore-plan", "")
	expect(t, rec, http.StatusOK)
	var plan provisioner.EtcdRestorePlan
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(h.plain)
	if len(plan.Steps) != 9 || !strings.Contains(plan.Steps[2].Command, hex.EncodeToString(sum[:])) || !slices.Equal(plan.Replaced, []string{"one-cp-b"}) {
		t.Errorf("plan %+v", plan)
	}
	// The cluster's API server may not read the objects of another Kubernetes minor.
	expect(t, b.do("GET", "/api/clusters/default/one/etcd-snapshots/20261011T060000Z-v1.35.2-rke2r1.db.age/restore-plan", ""), http.StatusConflict)
}

func TestRestoreEtcd(t *testing.T) {
	t.Parallel()
	h := etcdSnapshotting(t)
	b := h.browser()
	b.signIn("dev@pve")
	body := `{"snapshot":"` + h.name + `","confirm":"one"}`
	// Without the opt-in, a restore is done by hand.
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-restores", body), http.StatusConflict)

	h.setClass(map[string]string{provisioner.AutomaticRestoreAnnotation: "true"})
	rec := b.do("GET", "/api/clusters/default/one/etcd-snapshots", "")
	if v := etcdViewOf(t, rec.Body.Bytes()); !v.Automatic {
		t.Error("the view does not say the controller restores")
	}
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-restores", `{"snapshot":"20261011T060000Z-v1.35.2-rke2r1.db.age","confirm":"one"}`), http.StatusConflict)
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-restores", `{"snapshot":"20260101T000000Z-v1.db.age","confirm":"one"}`), http.StatusNotFound)
	// A restore names the cluster it is for, as the dialog has its user type it.
	for _, unconfirmed := range []string{`{"snapshot":"` + h.name + `"}`, `{"snapshot":"` + h.name + `","confirm":"two"}`} {
		expect(t, b.do("POST", "/api/clusters/default/one/etcd-restores", unconfirmed), http.StatusUnprocessableEntity)
	}
	if _, ok, _ := provisioner.EtcdRestoreRequestOf(h.cluster()); ok {
		t.Fatal("an unconfirmed restore was asked for")
	}
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-restores", body), http.StatusAccepted)
	req, ok, err := provisioner.EtcdRestoreRequestOf(h.cluster())
	if !ok || err != nil || req.Snapshot != h.name || req.Target != "store" || req.By != "dev@pve" {
		t.Errorf("request %+v, %v", req, err)
	}
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-restores", body), http.StatusConflict)
	expect(t, b.do("POST", "/api/clusters/default/one/etcd-snapshots", ""), http.StatusConflict)
}
