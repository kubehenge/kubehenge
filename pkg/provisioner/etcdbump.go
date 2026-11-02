package provisioner

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

// etcdRevisionBump is how far a restored etcd's revision moves on: past any revision a client of
// the etcd from before can have seen, as etcdutl's --bump-revision in the kubeadm restore.
const etcdRevisionBump = 1_000_000_000

// etcd's own names in its database: the bucket of revisions, the bucket of its bookkeeping, and the
// key there that names the revision to compact up to when etcd starts.
var (
	etcdKeyBucket         = []byte("key")
	etcdMetaBucket        = []byte("meta")
	etcdScheduledCompact  = []byte("scheduledCompactRev")
	errEtcdSnapshotNoRevs = errors.New("the snapshot holds no revision")
)

// etcdRevisionLen is the length of a revision as etcd keys its database by: the main revision, an
// underscore, and the sub revision, both big-endian. A deletion adds a marker after it.
const etcdRevisionLen = 8 + 1 + 8

// snapshotFileMode keeps the decrypted snapshot to the controller.
const snapshotFileMode = 0o600

// bumpedEtcdSnapshot writes the snapshot read from r into a new file in dir, without the checksum
// etcd appends, and moves its revision on by etcdRevisionBump with every revision up to there
// marked compacted, as etcdutl's restore does with --bump-revision and --mark-compacted. Talos,
// RKE2 and k3s restore a snapshot as it is, so without this the restored revisions repeat ones the
// clients of the etcd from before have already seen: their watches wait for revisions they think they had,
// and a node's kube-proxy keeps the Services' endpoints from before. With it, every watch from
// before ends as compacted, and its client lists again. The file is the caller's to remove.
func bumpedEtcdSnapshot(dir string, r io.Reader) (string, error) {
	f, err := os.CreateTemp(dir, "etcd-snapshot-*.db")
	if err != nil {
		return "", fmt.Errorf("making room for the snapshot: %w", err)
	}
	path := f.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(path)
		}
	}()
	_, err = io.Copy(f, r)
	if err == nil {
		err = stripEtcdChecksum(f)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	if err = bumpEtcdRevision(path, etcdRevisionBump); err != nil {
		return "", err
	}
	done = true
	return path, nil
}

// stripEtcdChecksum checks the SHA-256 etcd appends to a snapshot and takes it off, leaving the
// database. A snapshot without one is left as it is.
func stripEtcdChecksum(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("reading the snapshot: %w", err)
	}
	// etcd's database is whole pages of at least 512 bytes, so anything over is the checksum.
	size := st.Size()
	if size%512 != sha256.Size {
		return nil
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("reading the snapshot: %w", err)
	}
	sum := sha256.New()
	if _, err = io.CopyN(sum, f, size-sha256.Size); err != nil {
		return fmt.Errorf("reading the snapshot: %w", err)
	}
	want := make([]byte, sha256.Size)
	if _, err = io.ReadFull(f, want); err != nil {
		return fmt.Errorf("reading the snapshot's checksum: %w", err)
	}
	if !bytes.Equal(sum.Sum(nil), want) {
		return errors.New("the snapshot does not match the checksum etcd gave it")
	}
	if err = f.Truncate(size - sha256.Size); err != nil {
		return fmt.Errorf("taking the checksum off the snapshot: %w", err)
	}
	return nil
}

// appendEtcdChecksum ends the database at path with its SHA-256, as etcd ends a snapshot it streams,
// and returns the SHA-256 of the whole file. RKE2's and k3s's restore refuses a snapshot without it.
func appendEtcdChecksum(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDWR, snapshotFileMode) //nolint:gosec // the controller's own file of the snapshot
	if err != nil {
		return "", fmt.Errorf("reading the snapshot: %w", err)
	}
	db, whole := sha256.New(), sha256.New()
	if _, err = io.Copy(io.MultiWriter(db, whole), f); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("reading the snapshot: %w", err)
	}
	sum := db.Sum(nil)
	whole.Write(sum)
	_, err = f.Write(sum)
	if err = errors.Join(err, f.Close()); err != nil {
		return "", fmt.Errorf("ending the snapshot with its checksum: %w", err)
	}
	return hex.EncodeToString(whole.Sum(nil)), nil
}

// bumpEtcdRevision adds an empty revision by into the etcd database at path, past its latest, and
// schedules a compaction up to it for when etcd opens the database.
func bumpEtcdRevision(path string, by uint64) error {
	db, err := bolt.Open(path, snapshotFileMode, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return fmt.Errorf("opening the snapshot as etcd's database: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		keys, meta := tx.Bucket(etcdKeyBucket), tx.Bucket(etcdMetaBucket)
		if keys == nil || meta == nil {
			return errors.New("the snapshot is no etcd database")
		}
		latest, _ := keys.Cursor().Last()
		if len(latest) < etcdRevisionLen {
			return errEtcdSnapshotNoRevs
		}
		rev := make([]byte, etcdRevisionLen)
		binary.BigEndian.PutUint64(rev, binary.BigEndian.Uint64(latest)+by)
		rev[8] = '_'
		return errors.Join(keys.Put(rev, []byte{}), meta.Put(etcdScheduledCompact, rev))
	})
	if err = errors.Join(err, db.Close()); err != nil {
		return fmt.Errorf("moving the snapshot's revision on: %w", err)
	}
	return nil
}
