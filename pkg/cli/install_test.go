package cli

import (
	"io"
	"strings"
	"testing"
)

func TestInstallRefusesBadOptions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--name"}, "flag needs an argument: --name"},
		{[]string{"--ctid", "105", "--storage"}, "flag needs an argument: --storage"},
		{[]string{"--nope"}, "unknown flag: --nope"},
		{[]string{"--ip", "192.0.2.300"}, "--ip is dhcp, an IPv4 address with or without /prefix"},
		{[]string{"--pve-entry", "--no-pve-entry"}, "--pve-entry and --no-pve-entry exclude each other"},
	}
	for _, c := range cases {
		root := NewRoot()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(append([]string{"install"}, c.args...))
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want %q", c.args, err, c.want)
		}
	}
}

// The install command's help lists the options under Install, Upgrade and Certificate.
func TestInstallHelpGroupsTheOptions(t *testing.T) {
	t.Parallel()
	root := NewRoot()
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"install", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	help := out.String()
	install, upgrade, certificate := strings.Index(help, "\nInstall:\n"), strings.Index(help, "\nUpgrade:\n"), strings.Index(help, "\nCertificate:\n")
	if install < 0 || upgrade < install || certificate < upgrade || !strings.Contains(help[upgrade:certificate], "--upgrade") ||
		!strings.Contains(help[certificate:], "--no-name") || strings.Contains(help[:upgrade], "--certificate") {
		t.Fatalf("the options are not grouped:\n%s", help)
	}
}

// The options of the interface's name and certificate are listed under Install.
func TestInstallHelpListsTheInterfaceOptionsUnderInstall(t *testing.T) {
	t.Parallel()
	root := NewRoot()
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"install", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	help := out.String()
	install, upgrade := strings.Index(help, "\nInstall:\n"), strings.Index(help, "\nUpgrade:\n")
	if install < 0 || upgrade < install {
		t.Fatalf("the options are not grouped:\n%s", help)
	}
	for _, flag := range []string{"--name ", "--cert ", "--key ", "--cert-passphrase-file "} {
		if !strings.Contains(help[install:upgrade], flag) {
			t.Errorf("%s is not under Install:\n%s", flag, help)
		}
	}
}
