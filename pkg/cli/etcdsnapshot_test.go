package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestEtcdSnapshotDecrypt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	db := []byte("an etcd database")
	trailer := sha256.Sum256(db)
	plain := slices.Concat(db, trailer[:])
	var sealed bytes.Buffer
	w, err := age.Encrypt(&sealed, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(plain)
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	in, key, out := filepath.Join(dir, "s.db.age"), filepath.Join(dir, "identity"), filepath.Join(dir, "s.db")
	for name, data := range map[string][]byte{in: sealed.Bytes(), key: []byte(identity.String() + "\n")} {
		if err = os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := run(t, "etcd-snapshot", "decrypt", "--identity", key, in, out)
	whole := sha256.Sum256(plain)
	if err != nil || !strings.Contains(got, hex.EncodeToString(whole[:])) {
		t.Fatalf("decrypt: %v\n%s", err, got)
	}
	written, err := os.ReadFile(out) //nolint:gosec // the test's own temporary file
	if err != nil || !bytes.Equal(written, plain) {
		t.Errorf("wrote %q: %v", written, err)
	}
	// What is there already stays.
	if _, err = run(t, "etcd-snapshot", "decrypt", "--identity", key, in, out); err == nil {
		t.Error("overwrote a file")
	}

	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte(other.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := filepath.Join(dir, "refused.db")
	if _, err := run(t, "etcd-snapshot", "decrypt", "--identity", key, in, refused); err == nil {
		t.Error("decrypted with another cluster's key")
	}
	if _, err := os.Stat(refused); !os.IsNotExist(err) {
		t.Errorf("left a file behind: %v", err)
	}
}
