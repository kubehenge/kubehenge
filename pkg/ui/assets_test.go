package ui

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"strings"
	"testing"
)

// TestPrecompressedFormsAreTheFiles holds the build's compressed forms to the files they stand for:
// a browser that takes one runs it in place of the file, under the same content-hashed name it
// caches for a year. The Brotli form is made from the same bytes in the same step as the gzip one.
func TestPrecompressedFormsAreTheFiles(t *testing.T) {
	t.Parallel()
	assets := Frontend()
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		t.Fatalf("the frontend is not built: %v", err)
	}
	checked := 0
	err := fs.WalkDir(assets, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(name, ".gz") {
			return err
		}
		plain, err := fs.ReadFile(assets, strings.TrimSuffix(name, ".gz"))
		if err != nil {
			t.Errorf("%s has no file beside it: %v", name, err)
			return nil
		}
		raw, err := fs.ReadFile(assets, name)
		if err != nil {
			return err
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return nil
		}
		unpacked, err := io.ReadAll(zr)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return nil
		}
		if !bytes.Equal(unpacked, plain) {
			t.Errorf("%s does not unpack to the file beside it", name)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("the build has no compressed forms")
	}
}
