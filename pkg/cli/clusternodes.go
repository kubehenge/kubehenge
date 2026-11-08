package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// taintEffects are the effects Kubernetes knows for a taint.
var taintEffects = []string{"NoSchedule", "PreferNoSchedule", "NoExecute"}

// parseLabel reads KEY=VALUE, the value possibly empty.
func parseLabel(s string) (map[string]string, error) {
	key, value, ok := strings.Cut(s, "=")
	if !ok || key == "" {
		return nil, fmt.Errorf("--label %q: give KEY=VALUE, such as team=a", s)
	}
	return map[string]string{"key": key, "value": value}, nil
}

// parseTaint reads KEY=VALUE:EFFECT or KEY:EFFECT.
func parseTaint(s string) (map[string]string, error) {
	head, effect, ok := strings.Cut(s, ":")
	if !ok {
		head, effect = s, ""
	}
	key, value, _ := strings.Cut(head, "=")
	if key == "" || effect == "" || !contains(taintEffects, effect) {
		return nil, fmt.Errorf("--taint %q: give KEY=VALUE:EFFECT or KEY:EFFECT, with the effect %s", s, strings.Join(taintEffects, ", "))
	}
	taint := map[string]string{"key": key, "effect": effect}
	if value != "" {
		taint["value"] = value
	}
	return taint, nil
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

// nodesValue is the labels and taints of a pool as the interface's change request holds them.
func nodesValue(labels, taints []string) (map[string]any, error) {
	ls := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		label, err := parseLabel(l)
		if err != nil {
			return nil, err
		}
		ls = append(ls, label)
	}
	ts := make([]map[string]string, 0, len(taints))
	for _, t := range taints {
		taint, err := parseTaint(t)
		if err != nil {
			return nil, err
		}
		ts = append(ts, taint)
	}
	return map[string]any{"labels": ls, "taints": ts}, nil
}

// poolRequest is the interface's change for pool set's flags.
func poolRequest(pool string, workers *int32, labels, taints []string, inherit bool) (map[string]any, error) {
	change := map[string]any{}
	switch {
	case inherit && (len(labels) > 0 || len(taints) > 0):
		return nil, errors.New("--inherit takes the cluster's labels and taints, so it cannot go with --label or --taint")
	case inherit:
		change["nodes"] = nil
	case len(labels) > 0 || len(taints) > 0:
		nodes, err := nodesValue(labels, taints)
		if err != nil {
			return nil, err
		}
		change["nodes"] = nodes
	}
	if workers != nil {
		change["replicas"] = *workers
	}
	if len(change) == 0 {
		return nil, errors.New("give --label, --taint, --inherit, --workers or a mix")
	}
	return map[string]any{"pools": map[string]any{pool: change}}, nil
}

func (o *clusterOptions) poolCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "pool", Short: "Change a cluster's worker pools"}
	var workers int32
	var labels, taints []string
	var inherit bool
	set := &cobra.Command{
		Use:   "set CLUSTER POOL",
		Short: "Set the labels and taints of a pool's nodes, or add the pool",
		Long: `Set the labels and taints that the nodes of a worker pool register with. They replace the
pool's own labels and taints, so give them all, and a node keeps them when it is replaced.

Changing them replaces the pool's nodes, one at a time. A pool the cluster lacks is added, and
needs --workers.`,
		Args: argsSay(cobra.ExactArgs(2), "give the cluster's name and the pool's"),
		RunE: func(cmd *cobra.Command, args []string) error {
			var n *int32
			if cmd.Flags().Changed("workers") {
				n = &workers
			}
			req, err := poolRequest(args[1], n, labels, taints, inherit)
			if err != nil {
				return err
			}
			return o.change(cmd.Context(), cmd.OutOrStdout(), args[0], req)
		},
	}
	set.Flags().StringArrayVar(&labels, "label", nil, "a node label as KEY=VALUE, such as team=a, repeated for more")
	set.Flags().StringArrayVar(&taints, "taint", nil, "a node taint as KEY=VALUE:EFFECT or KEY:EFFECT, such as dedicated=db:NoSchedule, repeated for more")
	set.Flags().BoolVar(&inherit, "inherit", false, "drop the pool's own labels and taints, so that it takes the cluster's")
	set.Flags().Int32Var(&workers, "workers", 0, "the pool's workers")
	o.namespaceFlag(set.Flags())
	cmd.AddCommand(set)
	return cmd
}

// updateFlags are the settings update changes. A nil or empty one is left as the cluster has it.
type updateFlags struct {
	cis            *bool
	grace          *int
	labels, taints []string
	custom         *nodeCustomFlags
}

// updateRequest is the interface's change for update's flags: each sets its own variable only.
func updateRequest(f updateFlags) (map[string]any, error) {
	vars := map[string]any{}
	if f.cis != nil {
		if !*f.cis {
			return nil, errors.New("--cis=false: CIS hardening cannot be switched off once on")
		}
		vars["cis"] = true
	}
	if f.grace != nil {
		if err := checkGrace(*f.grace); err != nil {
			return nil, err
		}
		vars[provisioner.HostOutageGraceVariable] = *f.grace
	}
	if len(f.labels) > 0 || len(f.taints) > 0 {
		nodes, err := nodesValue(f.labels, f.taints)
		if err != nil {
			return nil, err
		}
		vars["workerNodes"] = nodes
	}
	if f.custom != nil && f.custom.any() {
		value, err := f.custom.value()
		if err != nil {
			return nil, err
		}
		vars[nodeCustomisationVariable] = value
	}
	if len(vars) == 0 {
		return nil, errors.New("give --cis, --" + graceFlag + ", --label, --taint, a node customisation flag such as --node-file or --kubelet-arg, or a mix")
	}
	return map[string]any{variablesKey: vars}, nil
}

func (o *clusterOptions) updateCommand() *cobra.Command {
	var cis bool
	var grace int
	var labels, taints []string
	custom := &nodeCustomFlags{}
	update := &cobra.Command{
		Use:   "update NAME [--cis] [--" + graceFlag + " N] [--label KEY=VALUE] [--taint KEY=VALUE:EFFECT] [--node-file ...]",
		Short: "Change a cluster's CIS hardening, host outage wait, worker labels and taints, or node customisation",
		Long: `Change the settings given, and leave the others as the cluster has them.

--cis switches CIS hardening on for a cluster created without it. It enforces the restricted Pod
Security level, the runtime's seccomp profile and, on RKE2, its CIS profile, and it replaces the
control plane and the workers, one node at a time. It cannot be switched off again.

--` + graceFlag + ` sets how long a machine on a host that is offline or in maintenance
waits before it is replaced.

--label and --taint set the labels and taints every worker registers with, unless its pool has its
own. They replace the cluster's labels and taints, so give them all. The workers are replaced, one
node at a time.

The --node-*, --kubelet-arg, --apiserver-arg, --controller-manager-arg, --scheduler-arg and --audit-*
flags set the cluster variable nodeCustomisation. They replace the whole variable, so give every
file, command and flag the cluster should keep. The nodes it reaches are replaced, one at a time.`,
		Args: argsSay(cobra.ExactArgs(1), clusterNameArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			f := updateFlags{labels: labels, taints: taints, custom: custom}
			if cmd.Flags().Changed("cis") {
				f.cis = &cis
			}
			if cmd.Flags().Changed(graceFlag) {
				f.grace = &grace
			}
			req, err := updateRequest(f)
			if err != nil {
				return err
			}
			return o.change(cmd.Context(), cmd.OutOrStdout(), args[0], req)
		},
	}
	update.Flags().BoolVar(&cis, "cis", false, "switch CIS hardening on, which replaces every node")
	update.Flags().IntVar(&grace, graceFlag, 0, graceHelp())
	update.Flags().StringArrayVar(&labels, "label", nil, "a worker label as KEY=VALUE, such as team=a, repeated for more")
	update.Flags().StringArrayVar(&taints, "taint", nil, "a worker taint as KEY=VALUE:EFFECT or KEY:EFFECT, such as dedicated=db:NoSchedule, repeated for more")
	custom.register(update.Flags())
	o.namespaceFlag(update.Flags())
	return update
}
