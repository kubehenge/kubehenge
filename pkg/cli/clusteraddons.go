package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// addonCommands are the commands for a cluster's add-ons: applying the changes that wait for its
// maintenance window, and labelling the namespaces the add-ons install into.
func (o *clusterOptions) addonCommands() []*cobra.Command {
	apply := &cobra.Command{
		Use:   "apply-addon-changes NAME",
		Short: "Apply the add-on changes that wait for the cluster's maintenance window now",
		Long: `Apply the add-on changes that wait for the cluster's maintenance window now.

A new Kubehenge release moves the charts of a cluster's add-ons only when the cluster's window
opens. This applies them at once, as a security fix needs. "kubehenge cluster get" lists them as
addonChanges.`,
		Args: argsSay(cobra.ExactArgs(1), clusterNameArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := o.clusterPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if _, err = o.call(cmd.Context(), http.MethodPost, path+"/addon-changes", nil); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cluster %s applies its add-on changes\n", args[0])
			return nil
		},
	}
	o.namespaceFlag(apply.Flags())

	var labels []string
	var clearLabels bool
	namespaceLabels := &cobra.Command{
		Use:   "namespace-labels NAME",
		Short: "Set the labels put on the namespaces the cluster's add-ons install into",
		Long: `Set the labels put on the namespaces the cluster's add-ons install into.

The labels replace those set before. They are on the namespaces before the first pod runs, for a
cluster whose admission policy requires labels or reads Pod Security levels from them.`,
		Args: argsSay(cobra.ExactArgs(1), clusterNameArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			if clearLabels && len(labels) > 0 {
				return errors.New("give --label or --clear, not both")
			}
			set := map[string]string{}
			if !clearLabels {
				var err error
				if set, err = parseLabels(labels); err != nil {
					return err
				}
			}
			return o.change(cmd.Context(), cmd.OutOrStdout(), args[0], map[string]any{"variables": map[string]any{"namespaceLabels": set}})
		},
	}
	namespaceLabels.Flags().StringArrayVar(&labels, "label", nil, "a label as KEY=VALUE, repeated for more")
	namespaceLabels.Flags().BoolVar(&clearLabels, "clear", false, "take every label off")
	o.namespaceFlag(namespaceLabels.Flags())
	return []*cobra.Command{apply, namespaceLabels}
}

// parseLabels reads namespace-labels' --label flags, at least one.
func parseLabels(labels []string) (map[string]string, error) {
	set := map[string]string{}
	for _, l := range labels {
		key, value, ok := strings.Cut(l, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--label %q: give KEY=VALUE, such as team=platform", l)
		}
		set[key] = value
	}
	if len(set) == 0 {
		return nil, errors.New("give --label, or --clear to take them off")
	}
	return set, nil
}
