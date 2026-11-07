package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
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

func (o *clusterOptions) createCommand() *cobra.Command {
	var file string
	custom := &nodeCustomFlags{}
	create := &cobra.Command{
		Use:   "create -f FILE",
		Short: "Create a cluster from the interface's create request, in JSON or YAML",
		Long: `Create a cluster from the interface's create request, in JSON or YAML.

The request is what the interface's wizard sends: name, class, version, controlPlaneReplicas,
workerReplicas, variables, and namespace where the interface has more than one. It is checked as
the wizard's request is.

The --node-*, --kubelet-arg, --apiserver-arg, --controller-manager-arg, --scheduler-arg and --audit-*
flags set the cluster variable nodeCustomisation. They add to the lists in the file, and replace its
audit policy and audit webhook.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.create(cmd.Context(), cmd.OutOrStdout(), file, custom)
		},
	}
	custom.register(create.Flags())
	create.Flags().StringVarP(&file, "filename", "f", "", "the create request, or - for standard input")
	_ = create.MarkFlagRequired("filename")
	return create
}

func (o *clusterOptions) updateCommand() *cobra.Command {
	var cis bool
	custom := &nodeCustomFlags{}
	update := &cobra.Command{
		Use:   "update NAME (--cis | --node-file ... | --node-command ... | --kubelet-arg ...)",
		Short: "Switch on a cluster's CIS hardening or set its node customisation, which replaces every node",
		Long: `Switch on CIS hardening on a cluster that was created without it. It enforces the restricted Pod
Security level, the runtime's seccomp profile and, on RKE2, its CIS profile. It cannot be switched
off again.

The --node-*, --kubelet-arg, --apiserver-arg, --controller-manager-arg, --scheduler-arg and --audit-*
flags set the cluster variable nodeCustomisation. They replace the whole variable, so give every
file, command and flag the cluster should keep.

Either change replaces the control plane and the workers, one node at a time.`,
		Args: argsSay(cobra.ExactArgs(1), clusterNameArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			variables := map[string]any{}
			if cis {
				variables["cis"] = true
			}
			if custom.any() {
				value, err := custom.value()
				if err != nil {
					return err
				}
				variables[nodeCustomisationVariable] = value
			}
			if len(variables) == 0 {
				return errors.New("give --cis or a node customisation flag, such as --node-file or --kubelet-arg")
			}
			return o.change(cmd.Context(), cmd.OutOrStdout(), args[0], map[string]any{"variables": variables})
		},
	}
	update.Flags().BoolVar(&cis, "cis", false, "switch CIS hardening on")
	custom.register(update.Flags())
	o.namespaceFlag(update.Flags())
	return update
}
