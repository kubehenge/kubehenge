// Package talos_test validates the machine configurations the management cluster module sends to
// its nodes, with the validation Talos itself runs when a node receives one.
package talos_test

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
)

// plansEnv names the output of `tofu test -json -verbose -test-directory=tests/talos`, whose plans
// carry the configuration the Talos provider generated for each node (talos.tftest.hcl).
const plansEnv = "KUBEHENGE_TALOS_PLANS"

// metal is the mode of a node installed from the disk image, which is how the module's nodes run.
type metal struct{}

func (metal) String() string        { return "metal" }
func (metal) RequiresInstall() bool { return true }
func (metal) InContainer() bool     { return false }

type machineState struct {
	MachineConfiguration string `json:"machine_configuration"` //nolint:tagliatelle // OpenTofu's plan format
}

type machineChange struct {
	Actions []string     `json:"actions"`
	After   machineState `json:"after"`
}

type resourceChange struct {
	Address string        `json:"address"`
	Change  machineChange `json:"change"`
}

type testPlan struct {
	ResourceChanges []resourceChange `json:"resource_changes"` //nolint:tagliatelle // OpenTofu's plan format
}

type event struct {
	Type     string   `json:"type"`
	TestRun  string   `json:"@testrun"`  //nolint:tagliatelle // OpenTofu's log format
	TestPlan testPlan `json:"test_plan"` //nolint:tagliatelle // OpenTofu's log format
}

// nodeChanges returns the first node's planned change of each planned run.
func nodeChanges(t *testing.T, path string) map[string]machineChange {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the path is the make target's own output file
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]machineChange{}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var e event
		if err = json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		if e.Type != "test_plan" {
			continue
		}
		for _, rc := range e.TestPlan.ResourceChanges {
			if rc.Address == "module.talos_node_0.talos_machine.this" {
				out[e.TestRun] = rc.Change
			}
		}
	}
	return out
}

// clientValidator is the client-side validation of Talos's config container, which Provider does
// not export since Validate was deprecated.
type clientValidator interface {
	ValidateAsClient(mode validation.RuntimeMode, opt ...validation.Option) ([]string, error)
}

func TestNodeConfigurationsAreValid(t *testing.T) {
	t.Parallel()
	path := os.Getenv(plansEnv)
	if path == "" {
		t.Skipf("%s names the output of tofu test, which make unit-tofu produces", plansEnv)
	}
	changes := nodeChanges(t, path)
	for _, run := range []string{"ipv4", "dual", "ipv6_first", "ipv6_only"} {
		t.Run(run, func(t *testing.T) {
			t.Parallel()
			yaml := changes[run].After.MachineConfiguration
			if yaml == "" {
				t.Fatalf("the plan of run %q has no machine configuration", run)
			}
			cfg, err := configloader.NewFromBytes([]byte(yaml))
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			v, ok := cfg.(clientValidator)
			if !ok {
				t.Fatalf("%T cannot validate as a client", cfg)
			}
			warnings, err := v.ValidateAsClient(metal{})
			if err != nil {
				t.Fatalf("Talos refuses the configuration: %v", err)
			}
			if len(warnings) > 0 {
				t.Errorf("Talos warns about the configuration: %s", strings.Join(warnings, "; "))
			}
		})
	}
}

// A VM made again boots Talos unconfigured, so its node is sent the configuration again. A plan
// that keeps the VM sends nothing.
func TestNodeIsConfiguredAgainOnAVMMadeAgain(t *testing.T) {
	t.Parallel()
	path := os.Getenv(plansEnv)
	if path == "" {
		t.Skipf("%s names the output of tofu test, which make unit-tofu produces", plansEnv)
	}
	changes := nodeChanges(t, path)
	if got := changes["vm_made_again"].Actions; !slices.Contains(got, "create") {
		t.Errorf("a node whose VM is made again plans %v, not its configuration again", got)
	}
	if got := changes["ipv4"].Actions; !slices.Equal(got, []string{"no-op"}) {
		t.Errorf("a node whose VM stays plans %v, not nothing", got)
	}
}
