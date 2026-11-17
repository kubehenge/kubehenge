package appliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// exportFolder holds the exports of Kubehenge's objects beside the etcd snapshots.
const exportFolder = "management/exports"

// ManagementExport is what each backup keeps of Kubehenge besides etcd: the appliance's settings and
// Kubehenge's own objects in the management cluster, readable without etcd.
type ManagementExport struct {
	Taken time.Time `json:"taken"`
	// Snapshot is the etcd snapshot taken with it.
	Snapshot string    `json:"snapshot"`
	Settings *Settings `json:"settings,omitempty"`
	// Objects is a YAML stream: each cluster's spec, its Secrets' values sealed with the recovery
	// key, and the ClusterClasses the clusters use.
	Objects string `json:"objects"`
}

// ExportRecord is the last export taken.
type ExportRecord struct {
	Name  string    `json:"name"`
	Time  time.Time `json:"time"`
	Bytes int64     `json:"bytes"`
}

func exportKey(snapshot string) string {
	return path.Join(exportFolder, strings.TrimSuffix(snapshot, provisioner.EtcdSnapshotSuffix)+".json.age")
}

// exportToVolume exports Kubehenge's objects, encrypted like the snapshots, onto the volume, where
// the newest volumeBackupKeep stay. It returns the encrypted export for the appliance's copy, also
// when the volume could not take it.
func (a *Appliance) exportToVolume(ctx context.Context, keys *volumeBackupKeys, snapshot string) (*ExportRecord, []byte, error) {
	if a.ExportObjects == nil {
		return nil, nil, errors.New("this appliance cannot read Kubehenge's objects to export them")
	}
	objects, err := a.ExportObjects(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("exporting Kubehenge's objects: %w", err)
	}
	to, err := age.ParseX25519Recipient(keys.Recipient)
	if err != nil {
		return nil, nil, fmt.Errorf("the snapshots' key: %w", err)
	}
	now := a.now()
	plain, err := json.Marshal(ManagementExport{Taken: now, Snapshot: snapshot, Settings: a.SavedSettings(), Objects: string(objects)})
	if err != nil {
		return nil, nil, fmt.Errorf("encoding the export: %w", err)
	}
	var sealed bytes.Buffer
	w, err := age.Encrypt(&sealed, to)
	if err == nil {
		_, err = w.Write(plain)
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("encrypting the export: %w", err)
	}
	key := exportKey(snapshot)
	store, err := a.openVolume(ctx)
	if err != nil {
		return nil, sealed.Bytes(), err
	}
	if _, err := store.Put(ctx, key, bytes.NewReader(sealed.Bytes()), nil); err != nil {
		return nil, sealed.Bytes(), fmt.Errorf("storing the export on the backup volume: %w", err)
	}
	if err := pruneFolder(ctx, store, exportFolder, volumeBackupKeep); err != nil {
		return nil, sealed.Bytes(), fmt.Errorf("deleting older exports on the backup volume: %w", err)
	}
	return &ExportRecord{Name: path.Base(key), Time: now, Bytes: int64(sealed.Len())}, sealed.Bytes(), nil
}

// newestFirst lists the objects under folder, newest first. Their names start with the time.
func newestFirst(ctx context.Context, store provisioner.ObjectStore, folder string) ([]string, error) {
	list, err := store.List(ctx, folder+"/")
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", folder, err)
	}
	keys := make([]string, 0, len(list))
	for _, o := range list {
		keys = append(keys, o.Key)
	}
	slices.Sort(keys)
	slices.Reverse(keys)
	return keys, nil
}

func pruneFolder(ctx context.Context, store provisioner.ObjectStore, folder string, keep int) error {
	keys, err := newestFirst(ctx, store, folder)
	if err != nil || len(keys) <= keep {
		return err
	}
	for _, k := range keys[keep:] {
		if err := store.Remove(ctx, k); err != nil {
			return fmt.Errorf("deleting %s: %w", k, err)
		}
	}
	return nil
}

// openExport decrypts an export with identity.
func openExport(r io.Reader, identity age.Identity) (*ManagementExport, error) {
	plain, err := age.Decrypt(r, identity)
	if err != nil {
		return nil, fmt.Errorf("opening the export: %w", err)
	}
	var e ManagementExport
	if err := json.NewDecoder(plain).Decode(&e); err != nil {
		return nil, fmt.Errorf("reading the export: %w", err)
	}
	return &e, nil
}
