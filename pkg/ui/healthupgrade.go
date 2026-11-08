package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// notReachableDocs is the troubleshooting entry for a node that is online and does not answer.
const notReachableDocs = "https://kubehenge.dev/troubleshooting#a-node-is-online-but-not-reachable"

// upgradeAttention adds what the controller found of a Proxmox VE upgrade under way, which it
// records on the Cluster: nodes on different major versions, VMs with different interface sizes
// and HA rules that wait.
func upgradeAttention(c *clusterv1.Cluster, a *attention) {
	if groups := provisioner.ParseVersionNote(c.Annotations[provisioner.ProxmoxVersionsAnnotation]); len(groups) > 1 {
		majors := make([]string, len(groups))
		parts := make([]string, len(groups))
		for i, g := range groups {
			majors[i] = strconv.Itoa(g.Major)
			parts[i] = fmt.Sprintf("Proxmox VE %d: %s", g.Major, strings.Join(g.Nodes, ", "))
		}
		what := "Proxmox VE " + strings.Join(majors[:len(majors)-1], ", ") + " and " + majors[len(majors)-1] + " nodes in one cluster"
		a.add(severityWarning, sectionNodes, what,
			strings.Join(parts, ". ")+". New VMs prefer the oldest version.", time.Time{})
	}
	if sizes := provisioner.ParseNodeMTUs(c.Annotations[provisioner.NodeMTUsAnnotation]); len(sizes) > 1 {
		parts := make([]string, len(sizes))
		for i, s := range sizes {
			parts[i] = fmt.Sprintf("%s: %d", s.Node, s.MTU)
		}
		a.add(severityWarning, sectionNodes, "VMs differ in interface MTU",
			strings.Join(parts, ", ")+". Set the network's MTU so every node gets the same.", time.Time{})
	}
	if cond := trueCondition(c, provisioner.NodesSkippedCondition); cond != nil {
		*a = append(*a, attentionView{
			Severity: severityWarning, Section: sectionNodes, What: "Proxmox VE node not reachable",
			Message: cond.Message + ". On the node, then restart pveproxy:",
			Since:   cond.LastTransitionTime.Time, Docs: notReachableDocs, Command: "pvecm updatecerts --force",
		})
	}
	if cond := trueCondition(c, provisioner.HARulesPendingCondition); cond != nil {
		a.add(severityWarning, sectionNodes, "HA rules wait for the Proxmox VE upgrade", cond.Message, cond.LastTransitionTime.Time)
	}
}
