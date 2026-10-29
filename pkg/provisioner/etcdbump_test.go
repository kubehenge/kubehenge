package provisioner

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// etcdTestRev is a revision of a key in etcd's database, deleted or not.
type etcdTestRev struct {
	main, sub uint64
	deleted   bool
}

// key is the revision as etcd keys its database by.
func (r etcdTestRev) key() []byte {
	b := make([]byte, etcdRevisionLen, etcdRevisionLen+1)
	binary.BigEndian.PutUint64(b, r.main)
	b[8] = '_'
	binary.BigEndian.PutUint64(b[9:], r.sub)
	if r.deleted {
		b = append(b, 't')
	}
	return b
}

// etcdTestRevisions are the revisions etcdTestSnapshot's database holds, a deletion last.
var etcdTestRevisions = []etcdTestRev{{1, 0, false}, {2, 0, false}, {2, 1, false}, {7, 0, true}}

// etcdTestDB is a database with etcd's buckets and revs in it.
func etcdTestDB(t *testing.T, revs []etcdTestRev) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	db, err := bolt.Open(path, snapshotFileMode, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		keys, kerr := tx.CreateBucket(etcdKeyBucket)
		_, merr := tx.CreateBucket(etcdMetaBucket)
		if kerr != nil || merr != nil {
			return errors.Join(kerr, merr)
		}
		for _, r := range revs {
			if perr := keys.Put(r.key(), []byte("value")); perr != nil {
				return perr
			}
		}
		return nil
	})
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // the test's own temporary file
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// etcdTestSnapshot is a snapshot as etcd streams one: a database of etcdTestRevisions, then its
// SHA-256.
func etcdTestSnapshot(t *testing.T) []byte {
	t.Helper()
	data := etcdTestDB(t, etcdTestRevisions)
	sum := sha256.Sum256(data)
	return append(data, sum[:]...)
}

// checkBumped finds the database data with every revision of etcdTestRevisions kept, one empty
// revision etcdRevisionBump past the latest, and a compaction scheduled up to it.
func checkBumped(t *testing.T, data []byte) {
	t.Helper()
	if len(data)%512 == sha256.Size {
		t.Error("the snapshot still carries etcd's checksum")
	}
	path := filepath.Join(t.TempDir(), "db")
	if err := os.WriteFile(path, data, snapshotFileMode); err != nil { //nolint:gosec // the test's own temporary file
		t.Fatal(err)
	}
	db, err := bolt.Open(path, snapshotFileMode, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("not a database: %v", err)
	}
	defer func() { _ = db.Close() }()
	want := etcdTestRev{main: 7 + etcdRevisionBump}.key()
	err = db.View(func(tx *bolt.Tx) error {
		keys := tx.Bucket(etcdKeyBucket)
		for _, r := range etcdTestRevisions {
			if v := keys.Get(r.key()); string(v) != "value" {
				t.Errorf("revision %d_%d is %q", r.main, r.sub, v)
			}
		}
		k, v := keys.Cursor().Last()
		if !bytes.Equal(k, want) || len(v) != 0 {
			t.Errorf("latest revision %x = %q, want %x empty", k, v, want)
		}
		if got := tx.Bucket(etcdMetaBucket).Get(etcdScheduledCompact); !bytes.Equal(got, want) {
			t.Errorf("compaction scheduled up to %x, want %x", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBumpedEtcdSnapshotMovesTheRevisionOnAndCompacts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"as etcd streams it", etcdTestSnapshot(t)},
		{"without a checksum", etcdTestDB(t, etcdTestRevisions)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path, err := bumpedEtcdSnapshot(t.TempDir(), bytes.NewReader(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path) //nolint:gosec // the file the test had made
			if err != nil {
				t.Fatal(err)
			}
			checkBumped(t, data)
		})
	}
}

func TestBumpedEtcdSnapshotRefuses(t *testing.T) {
	t.Parallel()
	tampered := etcdTestSnapshot(t)
	tampered[len(tampered)-1] ^= 1
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"a checksum that does not match", tampered, "does not match the checksum"},
		{"no database", append(bytes.Repeat([]byte("x"), 4096), make([]byte, sha256.Size)...), "does not match the checksum"},
		{"no database without a checksum", bytes.Repeat([]byte("x"), 4096), "as etcd's database"},
		{"no revision", etcdTestDB(t, nil), "no revision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if _, err := bumpedEtcdSnapshot(dir, bytes.NewReader(tc.data)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if left, _ := os.ReadDir(dir); len(left) != 0 {
				t.Errorf("left %v behind", left)
			}
		})
	}
}
