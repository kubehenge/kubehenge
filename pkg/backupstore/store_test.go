package backupstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func newStore(t *testing.T) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewServer((&Server{Dir: dir, Token: "secret"}).Handler())
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, Token: "secret", HTTP: srv.Client()}, dir
}

func TestBackupStoreKeepsObjects(t *testing.T) {
	ctx := t.Context()
	c, _ := newStore(t)
	n, err := c.Put(ctx, "management/etcd/a.db", strings.NewReader("hello"), map[string]string{"sha256": "x"})
	if err != nil || n != 5 {
		t.Fatalf("put: %d, %v", n, err)
	}
	if _, err := c.Put(ctx, "management/export/b.json", strings.NewReader("{}"), nil); err != nil {
		t.Fatal(err)
	}
	list, err := c.List(ctx, "management/etcd/")
	if err != nil || len(list) != 1 || list[0].Key != "management/etcd/a.db" || list[0].Size != 5 {
		t.Fatalf("list under the prefix: %+v, %v", list, err)
	}
	st, err := c.Stat(ctx, "management/etcd/a.db")
	if err != nil || st.Size != 5 || st.Metadata["sha256"] != "x" {
		t.Fatalf("stat: %+v, %v", st, err)
	}
	r, err := c.Get(ctx, "management/etcd/a.db")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r)
	_ = r.Close()
	if string(body) != "hello" {
		t.Fatalf("got %q back", body)
	}
	if err := c.Remove(ctx, "management/etcd/a.db"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "management/etcd/a.db"); err != nil {
		t.Fatalf("removing a removed object: %v", err)
	}
	if _, err := c.Get(ctx, "management/etcd/a.db"); !errors.Is(err, provisioner.ErrNoObject) {
		t.Fatalf("a removed object reads as %v, want ErrNoObject", err)
	}
}

func TestBackupStoreRefusesWithoutTheToken(t *testing.T) {
	c, _ := newStore(t)
	c.Token = "wrong"
	_, err := c.Put(t.Context(), "a", strings.NewReader("x"), nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a wrong token stored an object: %v", err)
	}
	resp, err := c.HTTP.Get(c.Base + "/list")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a listing without the token: %s", resp.Status)
	}
}

func TestBackupStoreRefusesKeysOutsideIt(t *testing.T) {
	for _, key := range []string{"", "../x", "a/../../x", "a//b", "/a", "a/.put-1", `a\b`} {
		if ValidKey(key) {
			t.Errorf("%q passes as a key", key)
		}
	}
	c, _ := newStore(t)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, c.Base+"/objects/a/%2e%2e/%2e%2e/x", strings.NewReader("x"))
	req.Header.Set(TokenHeader, c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an escaping key: %s", resp.Status)
	}
}

// The etcd snapshot code stores, lists and prunes on the volume as on a bucket.
func TestBackupStoreHoldsEtcdSnapshots(t *testing.T) {
	ctx := t.Context()
	c, _ := newStore(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		taken := &provisioner.EtcdSnapshot{
			Name:              provisioner.EtcdSnapshotName(time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC), "v1.37.2"),
			KubernetesVersion: "v1.37.2", Distribution: "talos",
		}
		// etcd sends the database, then its SHA-256.
		db := []byte("etcd database")
		sum := sha256.Sum256(db)
		if err := provisioner.StoreEtcdSnapshot(ctx, c, "management/etcd", taken, bytes.NewReader(append(db, sum[:]...)), id.Recipient()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := provisioner.PruneEtcdSnapshots(ctx, c, "management/etcd", 2); err != nil {
		t.Fatal(err)
	}
	list, err := provisioner.ListEtcdSnapshots(ctx, c, "management/etcd")
	if err != nil || len(list) != 2 {
		t.Fatalf("after pruning to 2: %+v, %v", list, err)
	}
}
