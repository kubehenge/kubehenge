package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerRefusesABadReleaseMirror(t *testing.T) {
	t.Parallel()
	o := &controllerOptions{}
	o.releaseMirror.URL = "https://mirror.example.org/"
	if err := o.parseReleaseMirror(); err == nil || !strings.Contains(err.Error(), "--release-mirror") {
		t.Errorf("a trailing slash gave %v", err)
	}
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	o = &controllerOptions{releaseMirrorCA: ca}
	o.releaseMirror.URL = "https://mirror.example.org"
	if err := o.parseReleaseMirror(); err == nil || !strings.Contains(err.Error(), "holds no PEM certificate") {
		t.Errorf("an authority without a certificate gave %v", err)
	}
	o = &controllerOptions{releaseMirrorCA: filepath.Join(t.TempDir(), "missing")}
	if err := o.parseReleaseMirror(); err == nil || !strings.Contains(err.Error(), "--release-mirror-ca") {
		t.Errorf("a missing authority file gave %v", err)
	}
	o = &controllerOptions{}
	if err := o.parseReleaseMirror(); err != nil {
		t.Errorf("no mirror gave %v", err)
	}
}

func TestReleasePinReadsTheMirror(t *testing.T) {
	t.Parallel()
	if _, err := pinChecks("mirror.example.org", ""); err == nil || !strings.Contains(err.Error(), "--mirror") {
		t.Errorf("an address without a scheme gave %v", err)
	}
	checks, err := pinChecks("https://mirror.example.org/github", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := checks.Mirror().URL; got != "https://mirror.example.org/github" {
		t.Errorf("the checks read %q", got)
	}
	plain, err := pinChecks("", "")
	if err != nil || plain.Mirror().Enabled() {
		t.Errorf("no mirror gave %+v, %v", plain.Mirror(), err)
	}
}
