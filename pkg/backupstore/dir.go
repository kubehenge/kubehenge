package backupstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// Dir keeps objects as files under Root: the object under objects/ and its metadata beside it under
// metadata/. The store on the backup volume and the appliance's own copy both use it.
type Dir struct {
	Root string
}

var _ provisioner.ObjectStore = Dir{}

// ValidKey tells whether key names an object: slash-separated names, none empty, and none starting
// with a dot, which the store's own files do.
func ValidKey(key string) bool {
	if key == "" || len(key) > 1024 || strings.ContainsAny(key, "\\\x00") {
		return false
	}
	return !slices.ContainsFunc(strings.Split(key, "/"), func(part string) bool { return part == "" || strings.HasPrefix(part, ".") })
}

func (d Dir) objectPath(key string) string {
	return filepath.Join(d.Root, "objects", filepath.FromSlash(key))
}

func (d Dir) metaPath(key string) string {
	return filepath.Join(d.Root, "metadata", filepath.FromSlash(key)+".json")
}

func checkKey(key string) error {
	if !ValidKey(key) {
		return fmt.Errorf("%q is no object's key: use slash-separated names that do not start with a dot", key)
	}
	return nil
}

// Put stores r under key, whole or not at all.
func (d Dir) Put(ctx context.Context, key string, r io.Reader, metadata map[string]string) (int64, error) {
	if err := checkKey(key); err != nil {
		return 0, err
	}
	size, err := writeAtomic(ctx, d.objectPath(key), r)
	if err != nil {
		return 0, fmt.Errorf("storing %s: %w", key, err)
	}
	raw, err := json.Marshal(metadata)
	if err == nil {
		_, err = writeAtomic(ctx, d.metaPath(key), strings.NewReader(string(raw)))
	}
	if err != nil {
		return 0, fmt.Errorf("storing %s's metadata: %w", key, err)
	}
	return size, nil
}

// writeAtomic writes r to a hidden file beside name and renames it there once complete, so that a
// reader never sees half an object.
func writeAtomic(ctx context.Context, name string, r io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return 0, err //nolint:wrapcheck // the caller names the object
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".put-*")
	if err != nil {
		return 0, err //nolint:wrapcheck // the caller names the object
	}
	size, err := io.Copy(f, &ctxReader{ctx: ctx, r: r})
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), name)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return 0, err
	}
	return size, nil
}

// ctxReader stops a copy once its context ends.
type ctxReader struct {
	ctx context.Context //nolint:containedctx // bounds one copy
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err //nolint:wrapcheck // the context's own
	}
	return c.r.Read(p) //nolint:wrapcheck // io.Reader's own errors, io.EOF among them
}

func (d Dir) open(key string) (*os.File, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	f, err := os.Open(d.objectPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", key, provisioner.ErrNoObject)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}
	return f, nil
}

func (d Dir) metadata(key string) map[string]string {
	raw, err := os.ReadFile(d.metaPath(key))
	if err != nil {
		return nil
	}
	var meta map[string]string
	_ = json.Unmarshal(raw, &meta)
	return meta
}

// Stat reads one object's size, time and metadata.
func (d Dir) Stat(_ context.Context, key string) (provisioner.StoredObject, error) {
	f, err := d.open(key)
	if err != nil {
		return provisioner.StoredObject{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return provisioner.StoredObject{}, fmt.Errorf("reading %s: %w", key, err)
	}
	return provisioner.StoredObject{Key: key, Size: info.Size(), Modified: info.ModTime().UTC(), Metadata: d.metadata(key)}, nil
}

// Get streams an object.
func (d Dir) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return d.open(key)
}

// Remove deletes an object. One that is gone already is no error.
func (d Dir) Remove(_ context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	for _, name := range []string{d.objectPath(key), d.metaPath(key)} {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("deleting %s: %w", key, err)
		}
	}
	return nil
}

// List lists the objects under prefix.
func (d Dir) List(ctx context.Context, prefix string) ([]provisioner.StoredObject, error) {
	root := filepath.Join(d.Root, "objects")
	out := []provisioner.StoredObject{}
	err := filepath.WalkDir(root, func(name string, e fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && name == root {
			return fs.SkipAll
		}
		if err != nil || e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // the context's own
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err //nolint:wrapcheck // reported below
		}
		key := path.Clean(filepath.ToSlash(rel))
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err //nolint:wrapcheck // reported below
		}
		out = append(out, provisioner.StoredObject{Key: key, Size: info.Size(), Modified: info.ModTime().UTC()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", prefix, err)
	}
	return out, nil
}

// PresignGet is refused: a directory hands out no URLs.
func (Dir) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", ErrNoPresign
}

// Size is the bytes the objects under Root take.
func (d Dir) Size(ctx context.Context) (int64, error) {
	list, err := d.List(ctx, "")
	var n int64
	for _, o := range list {
		n += o.Size
	}
	return n, err
}
