package appliance

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	upgraderUser = "kubehenge-upgrader@pve"
	upgraderAPI  = upgraderUser + "!apply=0f1e2d3c"
)

// The parser follows the module's own versions.tf, so a change of its layout shows here.
func TestModuleVersionsOfTheReleasedModule(t *testing.T) {
	t.Parallel()
	a := &Appliance{Module: filepath.Join("..", "..", "management-cluster")}
	got := a.ModuleVersions()
	for _, key := range []string{"talos", "kubernetes", "cluster_api", "capmox", "rke2", "k3s", "ipam_in_cluster", "helm_addon", "talos_bootstrap", "talos_control_plane", "cert_manager", "capi_operator", "kubehenge"} {
		if v := got[key]; v == "" || strings.ContainsAny(v, "@\" ") {
			t.Errorf("version of %s = %q", key, v)
		}
	}
	if len(got) != 13 {
		t.Errorf("versions = %v", got)
	}
}

func writeModuleVersions(t *testing.T, a *Appliance, talos string) {
	t.Helper()
	tf := "locals {\n  tested_versions = {\n    # renovate: datasource=github-releases depName=siderolabs/talos\n    talos = \"" + talos + "\"\n    kubernetes = \"v1.37.1\"\n  }\n\n" +
		"  cert_manager = {\n    chart = \"v1.21.2@sha256:0123\"\n    images = {\n      controller = \"v1.21.2@sha256:4567\"\n    }\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(a.Module, "versions.tf"), []byte(tf), 0o600); err != nil {
		t.Fatal(err)
	}
	charts := filepath.Join(filepath.Dir(a.Module), "charts", "kubehenge")
	if err := os.MkdirAll(charts, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(charts, "Chart.yaml"), []byte("apiVersion: v2\nname: kubehenge\nversion: 0.14.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRecordsWhatItRan(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	a.Release = "0.2.0"
	writeModuleVersions(t, a, "v1.14.4")
	if a.LastApplied() != nil {
		t.Fatal("applied before any apply")
	}
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "succeeded" || j.Automatic {
		t.Fatalf("job = %+v", j)
	}
	got := a.LastApplied()
	want := map[string]string{"talos": "v1.14.4", "kubernetes": "v1.37.1", "cert_manager": "v1.21.2", "kubehenge": "0.14.0"}
	if got == nil || got.Release != "0.2.0" || len(got.Versions) != len(want) {
		t.Fatalf("applied = %+v", got)
	}
	for k, v := range want {
		if got.Versions[k] != v {
			t.Errorf("applied %s = %q, want %q", k, got.Versions[k], v)
		}
	}
}

func TestFailedApplyRecordsNothing(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	a.Release = "0.2.0"
	if err := a.Apply(settings("fail"), "correct horse battery staple", ticket(), func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "failed" {
		t.Fatalf("job = %+v", j)
	}
	if got := a.LastApplied(); got != nil {
		t.Fatalf("a failed apply recorded %+v", got)
	}
}

func TestCheckPassphrase(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	if err := a.CheckPassphrase(context.Background(), "correct horse battery staple"); err == nil {
		t.Fatal("a passphrase checked before there is a state")
	}
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	if err := os.WriteFile(filepath.Join(a.Dir, "management", "terraform.tfstate"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckPassphrase(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckPassphrase(context.Background(), "the wrong passphrase entirely"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

// tofuEnvs are the environments tofu ran with since the log was last cleared.
func tofuEnvs(t *testing.T, a *Appliance) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(a.Dir, "fake-tofu.log"))
	if err != nil {
		t.Fatal(err)
	}
	var envs []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		var rec struct {
			Env []string `json:"env"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		envs = append(envs, strings.Join(rec.Env, "\n"))
	}
	return envs
}

// Kept, forgotten, and refused while forgotten.
func TestUnattendedKeptAndForgotten(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if a.UnattendedOn() != nil {
		t.Fatal("on before anyone chose it")
	}
	if err := a.ApplyUnattended(func(context.Context, []byte) error { return nil }); !errors.Is(err, ErrNotUnattended) {
		t.Fatalf("an automatic apply without the opt-in: %v", err)
	}
	if a.Current() != nil {
		t.Fatal("a job started without the opt-in")
	}
	if err := a.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderUser + "=0f1e2d3c"}); err == nil {
		t.Fatal("kept a token without its name")
	}
	if err := a.KeepUnattended(Unattended{Passphrase: "short", Token: upgraderAPI}); err == nil {
		t.Fatal("kept a passphrase too short to be one")
	}
	since := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if err := a.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderAPI, By: "root@pam", Since: since}); err != nil {
		t.Fatal(err)
	}
	kept, err := os.Stat(filepath.Join(a.Dir, "unattended.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && kept.Mode().Perm() != 0o600 {
		t.Fatalf("kept with mode %v", kept.Mode())
	}
	if got := a.UnattendedOn(); got == nil || got.By != "root@pam" || !got.Since.Equal(since) || got.User != upgraderUser {
		t.Fatalf("UnattendedOn = %+v", got)
	}
	raw, err := os.ReadFile(filepath.Join(a.Dir, "unattended.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "correct horse") || strings.Contains(string(raw), "0f1e2d3c") {
		t.Fatalf("kept the passphrase or the token in the open: %s", raw)
	}
	if u, err := a.unattended(); err != nil || u.Passphrase != "correct horse battery staple" || u.Token != upgraderAPI {
		t.Fatalf("opened %+v, %v", u, err)
	}

	if err := a.ForgetUnattended(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(a.Dir, "unattended.json"), filepath.Join(a.KeyDir, "unattended.key")} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s kept after switching off: %v", p, err)
		}
	}
	if a.UnattendedOn() != nil {
		t.Fatal("on after switching off")
	}
	if err := a.ApplyUnattended(func(context.Context, []byte) error { return nil }); !errors.Is(err, ErrNotUnattended) {
		t.Fatalf("an automatic apply after switching off: %v", err)
	}
	if err := a.ForgetUnattended(); err != nil {
		t.Fatalf("switching off twice: %v", err)
	}
}

// A backup holds the appliance's directory without the key volume: restored, what automatic
// upgrades keep shows as on, and a run fails saying why instead of applying.
func TestUnattendedWithoutItsKey(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if err := a.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderAPI}); err != nil {
		t.Fatal(err)
	}
	restored := &Appliance{Dir: a.Dir, KeyDir: t.TempDir(), Module: a.Module, Tofu: a.Tofu, Log: a.Log}
	if restored.UnattendedOn() == nil {
		t.Fatal("off once the key is gone")
	}
	if err := restored.ApplyUnattended(func(context.Context, []byte) error { return nil }); !errors.Is(err, ErrUnattendedKey) {
		t.Fatalf("an automatic apply without the key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(restored.KeyDir, "unattended.key"), make([]byte, sealKeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.unattended(); !errors.Is(err, ErrUnattendedKey) {
		t.Fatalf("opened with another key: %v", err)
	}
	restored.KeyDir = ""
	if err := restored.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderAPI}); err == nil {
		t.Fatal("kept with nowhere for the key")
	}
}

func TestUnattendedApply(t *testing.T) {
	t.Parallel()
	a, ready := newAppliance(t)
	a.Release = "0.1.0"
	if err := a.Apply(settings("pve1"), "correct horse battery staple", ticket(), func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, a)
	<-ready
	// The appliance upgraded itself since.
	a.Release = "0.2.0"
	if err := a.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderAPI}); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(a.Dir, "fake-tofu.log")); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyUnattended(func(_ context.Context, k []byte) error { ready <- k; return nil }); err != nil {
		t.Fatal(err)
	}
	j := waitFor(t, a)
	if j.State != "succeeded" || !j.Automatic || j.User != upgraderUser {
		t.Fatalf("job = %+v", j)
	}
	if !strings.Contains(strings.Join(j.Log, "\n"), "from what 0.1.0 applied to what 0.2.0 carries") {
		t.Fatalf("log = %q", j.Log)
	}
	if got := a.LastApplied(); got == nil || got.Release != "0.2.0" {
		t.Fatalf("applied = %+v", got)
	}
	if got := <-ready; !strings.HasPrefix(string(got), "interface for admin-kubeconfig") {
		t.Fatalf("ready with %q", got)
	}
	for _, env := range tofuEnvs(t, a) {
		if !strings.Contains(env, "PROXMOX_VE_API_TOKEN="+upgraderAPI) || strings.Contains(env, "PROXMOX_VE_AUTH_TICKET") {
			t.Errorf("an automatic run's tofu env:\n%s", env)
		}
	}
	restarted := &Appliance{Dir: a.Dir}
	restarted.LoadLast()
	if j := restarted.Current(); j == nil || !j.Automatic {
		t.Fatalf("after a restart: %+v", j)
	}
}

// An automatic run that cannot go ahead says why in its record, which the interface shows.
func TestUnattendedApplyWithoutAManagementCluster(t *testing.T) {
	t.Parallel()
	a, _ := newAppliance(t)
	if err := a.KeepUnattended(Unattended{Passphrase: "correct horse battery staple", Token: upgraderAPI}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyUnattended(func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j := waitFor(t, a); j.State != "failed" || !j.Automatic || !strings.Contains(j.Error, "no management cluster") {
		t.Fatalf("job = %+v", j)
	}
}
