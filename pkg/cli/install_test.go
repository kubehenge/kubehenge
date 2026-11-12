package cli

import (
	"io"
	"strings"
	"testing"
)

func TestInstallRefusesBadOptions(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--name"}, "flag needs an argument: --name"},
		{[]string{"--ctid", "105", "--storage"}, "flag needs an argument: --storage"},
		{[]string{"--nope"}, "unknown flag: --nope"},
		{[]string{"--ip", "192.0.2.5"}, "--ip is dhcp or an IPv4 address with its prefix length"},
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
