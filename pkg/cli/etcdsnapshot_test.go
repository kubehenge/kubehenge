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

	"github.com/kubehenge/kubehenge/pkg/provisioner"
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

// receive runs `etcd-snapshot receive` on stdin.
func receive(t *testing.T, stdin []byte, args ...string) (string, error) {
	t.Helper()
	root := NewRoot()
	var out bytes.Buffer
	root.SetIn(bytes.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"etcd-snapshot", "receive"}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// The receiver writes what the controller streams, refuses a stream cut short, and refuses before
// reading when the disk is too small.
func TestEtcdSnapshotReceive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := bytes.Repeat([]byte("sealed snapshot"), 1000)
	var stream bytes.Buffer
	if err := provisioner.SendEtcdSnapshot(&stream, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "received.enc")
	if got, err := receive(t, stream.Bytes(), "--out", out, "--need", "1"); err != nil || !strings.Contains(got, "received 15000 bytes") {
		t.Fatalf("receive: %v\n%s", err, got)
	}
	if written, err := os.ReadFile(out); err != nil || !bytes.Equal(written, body) { //nolint:gosec // the test's own temporary file
		t.Errorf("wrote %d bytes: %v", len(written), err)
	}
	cut := filepath.Join(dir, "cut.enc")
	if _, err := receive(t, stream.Bytes()[:stream.Len()-10], "--out", cut); err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Errorf("a stream cut short: %v", err)
	}
	if _, err := os.Stat(cut); !os.IsNotExist(err) {
		t.Errorf("left a cut file behind: %v", err)
	}
	if _, err := receive(t, stream.Bytes(), "--out", filepath.Join(dir, "big.enc"), "--need", "9000000000000000000"); err == nil || !strings.Contains(err.Error(), "free at "+dir) {
		t.Errorf("a disk too small: %v", err)
	}
}
