package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// One login serves the appliance commands and the cluster commands alike, and is kept readable by
// its owner alone.
func TestOneLoginServesEveryCommand(t *testing.T) {
	f := newFakeInterface(t)
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "appliance", "upgrades"); err != nil || !strings.Contains(out, "Installed     0.1.0") {
		t.Errorf("appliance upgrades: %v\n%s", err, out)
	}
	if out, err := run(t, "cluster", "list", "-o", "json"); err != nil || !strings.Contains(out, `"clusters"`) {
		t.Errorf("cluster list: %v\n%s", err, out)
	}
	path, err := loginPath(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the login file is readable by others: %v", info.Mode())
	}
}

// A login ends with kubehenge logout, which also ends the session at the interface.
func TestLogoutEndsTheSessionAndForgetsTheLogin(t *testing.T) {
	f := newFakeInterface(t)
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "logout"); err != nil {
		t.Fatal(err)
	}
	if f.signOuts != 1 {
		t.Errorf("signed out %d times", f.signOuts)
	}
	if _, err := run(t, "appliance", "upgrades"); err == nil || err.Error() != notLoggedIn {
		t.Errorf("after logout: %v", err)
	}
}

// A session the interface has ended says to log in again, not that the user is refused.
func TestAnEndedLoginSaysToLogInAgain(t *testing.T) {
	f := newFakeInterface(t)
	if err := f.login("admin@pve", fakePassword); err != nil {
		t.Fatal(err)
	}
	path, err := loginPath(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"value":"session"`, `"value":"expired"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "cluster", "list"); err == nil || !strings.Contains(err.Error(), "run kubehenge login <url> again") {
		t.Errorf("an ended session: %v", err)
	}
}

// The interface's own certificate given as --ca is trusted for the name it carries, so that the
// appliance's shell can log in by its loopback address.
func TestLoginTrustsTheCertificateItWasGivenByItsName(t *testing.T) {
	f := newFakeInterface(t)
	t.Setenv(loginFileEnv, filepath.Join(t.TempDir(), "login.json"))
	address := strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1)
	if out, err := run(t, "login", address, "--ca", f.certFile(), "--user", "admin@pve", "--password-file", writeTemp(t, fakePassword)); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
}

// A Proxmox VE API token is kept as it is, and every command sends it.
func TestLoginKeepsAToken(t *testing.T) {
	f := newFakeInterface(t)
	f.token = "dev@pve!ci=secret"
	t.Setenv(loginFileEnv, filepath.Join(t.TempDir(), "login.json"))
	if _, err := run(t, "login", f.srv.URL, "--ca", f.certFile()); err == nil {
		t.Error("a login with neither a user nor a token")
	}
	if out, err := run(t, "login", f.srv.URL, "--ca", f.certFile(), "--token-file", writeTemp(t, "dev@pve!ci=secret")); err != nil || !strings.Contains(out, "Logged in to") {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if out, err := run(t, "cluster", "list", "-o", "json"); err != nil || !strings.Contains(out, `"clusters"`) {
		t.Errorf("cluster list: %v\n%s", err, out)
	}
	if _, err := run(t, "login", f.srv.URL, "--ca", f.certFile(), "--token-file", writeTemp(t, "dev@pve!ci=guess")); err == nil {
		t.Error("a refused token was kept")
	}
}
