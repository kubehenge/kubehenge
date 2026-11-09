package ui

import (
	"os"
	"strings"
	"testing"
)

// A token holding only the privileges the install guide once listed met a refusal at each of these
// in the lab, one per run, and a sign-in with them is the only way a token user reaches the setup.
func TestSetupPrivilegesHoldWhatARunNeeds(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"Pool.Audit", "VM.Audit", "VM.PowerMgmt", "VM.Config.CPU", "VM.Config.Cloudinit", "VM.Config.Disk",
		"VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options", "Datastore.Allocate",
		"SDN.Audit", "SDN.Use", "Sys.AccessNetwork",
	} {
		found := false
		for _, have := range setupPrivileges {
			found = found || have == want
		}
		if !found {
			t.Errorf("setupPrivileges lacks %s, which a setup as a token user is refused without", want)
		}
	}
}

func TestInstallGuideListsEverySetupPrivilege(t *testing.T) {
	t.Parallel()
	guide, err := os.ReadFile("../../docs/install.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, priv := range setupPrivileges {
		if !strings.Contains(string(guide), "`"+priv+"`") {
			t.Errorf("docs/install.md does not list %s among the privileges a setup needs", priv)
		}
	}
}
