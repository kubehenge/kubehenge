package appliance

import (
	"strings"
	"testing"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

func TestANodeOnlineThatDoesNotAnswerIsRefusedWithItsReasonAndTheFix(t *testing.T) {
	t.Parallel()
	inv := &Inventory{Nodes: []pve.Node{
		{Node: "pve1", Online: true},
		{Node: "pve2", Fault: "HTTP 596: certificate verify failed"},
	}}
	var f Findings
	if !checkNodeExists(inv, &f, "pve1", "Choose a node that is.", "Start it.") || len(f.Errors) != 0 {
		t.Errorf("an answering node was refused: %v", f.Errors)
	}
	if checkNodeExists(inv, &f, "pve2", "Choose a node that is.", "Start it.") || len(f.Errors) != 1 {
		t.Fatalf("the node that does not answer passed: %v", f.Errors)
	}
	for _, want := range []string{"Node pve2 is online but not reachable through the cluster", "HTTP 596: certificate verify failed", "pvecm updatecerts --force", "restart pveproxy"} {
		if !strings.Contains(f.Errors[0], want) {
			t.Errorf("the refusal %q lacks %q", f.Errors[0], want)
		}
	}
}
