package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/pve"
)

// HARulesName is the ConfigMap, beside the ClusterClasses, through which the controller offers
// Proxmox VE's HA node-affinity rules to those who may create a cluster but not read the rules,
// which takes Sys.Audit on /. It exists only while an administrator has the rules offered.
const HARulesName = "kubehenge-ha-rules"

// HARulesKey holds the offered rules in HARulesName, as JSON.
const HARulesKey = "rules"

// OfferedRule is all of an HA rule that is offered: its name and the nodes it keeps resources to.
// Its comment, its resources and how strict it is stay with those who may read the rules.
type OfferedRule struct {
	Rule  string   `json:"rule"`
	Nodes []string `json:"nodes"`
}

// HARules keeps HARulesName current while Offer is set, and removes it while not. The controller's
// own token reads the rules: it holds Sys.Audit on / already, so offering them asks nothing new of
// Proxmox VE, and the HA token, which could, would bring Sys.Console to a read.
type HARules struct {
	// Reader reads past the cache, which holds only the classes' ConfigMaps.
	Reader    client.Reader
	Client    client.Client
	Namespace string
	Proxmox   *proxmox.Client
	Offer     bool
	// Interval is how often the rules are read: a rule added, changed or removed in Proxmox VE is
	// offered so within about that long.
	Interval time.Duration
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (*HARules) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (h *HARules) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("ha-rules")
	if !h.Offer {
		if err := h.withdraw(ctx); err != nil {
			log.Error(err, "withdrawing the HA rules offered to cluster creators")
		}
		return nil
	}
	t := time.NewTicker(h.Interval)
	defer t.Stop()
	for {
		if err := h.publish(ctx); err != nil && ctx.Err() == nil {
			log.Error(err, "offering the HA rules to cluster creators")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (h *HARules) withdraw(ctx context.Context) error {
	cm := &corev1.ConfigMap{}
	cm.Namespace, cm.Name = h.Namespace, HARulesName
	if err := h.Client.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing %s: %w", HARulesName, err)
	}
	return nil
}

func (h *HARules) publish(ctx context.Context) error {
	rules, err := h.read(ctx)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return fmt.Errorf("encoding the HA rules: %w", err)
	}
	cm := &corev1.ConfigMap{}
	err = h.Reader.Get(ctx, client.ObjectKey{Namespace: h.Namespace, Name: HARulesName}, cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = &corev1.ConfigMap{Data: map[string]string{HARulesKey: string(raw)}}
		cm.Namespace, cm.Name = h.Namespace, HARulesName
		err = h.Client.Create(ctx, cm)
	case err != nil:
	case cm.Data[HARulesKey] == string(raw):
		return nil
	default:
		cm.Data = map[string]string{HARulesKey: string(raw)}
		err = h.Client.Update(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("recording the HA rules in %s: %w", HARulesName, err)
	}
	return nil
}

// read lists the enabled node-affinity rules with the nodes each allows, leaving out those that
// allow none and those the controller keeps for clusters itself, whose nodes are their clusters'
// own business.
func (h *HARules) read(ctx context.Context) ([]OfferedRule, error) {
	var entries []pve.HARuleEntry
	if err := h.Proxmox.Get(ctx, pve.NodeAffinityPath, &entries); err != nil {
		return nil, fmt.Errorf("listing the HA rules: %w", err)
	}
	var listed []struct {
		Node string `json:"node"`
	}
	if err := h.Proxmox.Get(ctx, "/nodes", &listed); err != nil {
		return nil, fmt.Errorf("listing the nodes: %w", err)
	}
	nodes := make([]string, 0, len(listed))
	for _, n := range listed {
		nodes = append(nodes, n.Node)
	}
	slices.Sort(nodes)
	offered := []OfferedRule{}
	for _, rule := range pve.EnabledRules(entries) {
		if strings.HasPrefix(rule.Comment, haCommentPrefix) {
			continue
		}
		if allowed := rule.Allowed(nodes); len(allowed) > 0 {
			offered = append(offered, OfferedRule{Rule: rule.Rule, Nodes: allowed})
		}
	}
	slices.SortFunc(offered, func(a, b OfferedRule) int { return strings.Compare(a.Rule, b.Rule) })
	return offered, nil
}

// ParseOfferedRules reads the rules HARulesName offers.
func ParseOfferedRules(cm *corev1.ConfigMap) ([]OfferedRule, error) {
	var rules []OfferedRule
	if err := json.Unmarshal([]byte(cm.Data[HARulesKey]), &rules); err != nil {
		return nil, fmt.Errorf("reading %s: %w", HARulesName, err)
	}
	return rules, nil
}
