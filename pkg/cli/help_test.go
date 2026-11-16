package cli

import (
	"strings"
	"testing"
)

func TestRootHelpListsOnlyWhatAdministratorsRun(t *testing.T) {
	t.Parallel()
	out, err := run(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range internalCommands {
		if strings.Contains(out, "\n  "+name+" ") {
			t.Errorf("the help lists %q, which only Kubehenge's own pods run:\n%s", name, out)
		}
	}
	for _, name := range []string{"appliance", "cluster", "install", "release", "serve", "template"} {
		if !strings.Contains(out, "\n  "+name+" ") {
			t.Errorf("the help leaves out %q:\n%s", name, out)
		}
	}
}

func TestHiddenCommandsStillHaveHelp(t *testing.T) {
	t.Parallel()
	for _, name := range internalCommands {
		out, err := run(t, name, "--help")
		if err != nil || !strings.Contains(out, "kubehenge "+name) {
			t.Errorf("%s --help: %v\n%s", name, err, out)
		}
	}
}

func TestClusterCreateHelpGivesExamplesAndTheWizardsYAML(t *testing.T) {
	t.Parallel()
	out, err := run(t, "cluster", "create", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Examples:", "kubehenge cluster create -f web.yaml", `"Copy as YAML"`} {
		if !strings.Contains(out, want) {
			t.Errorf("cluster create --help lacks %q:\n%s", want, out)
		}
	}
}
