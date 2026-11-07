package cli

import (
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func newChartsCommand() *cobra.Command {
	var mirrors []string
	cmd := &cobra.Command{
		Use:   "charts VALUES",
		Short: "List the Helm charts Kubehenge installs into clusters, to mirror them",
		Long: `List the Helm charts Kubehenge installs into clusters, each as
<repository>/<name>:<version>@sha256:<digest>, one per line.

VALUES is the values file of Kubehenge's chart, or - for standard input, such as the output of
"helm show values oci://ghcr.io/kubehenge/charts/kubehenge". A site without internet access copies
each chart to a mirror under the same digest and gives the controller the mirror (--chart-mirror).
With --mirror, each line also shows the address the chart is then fetched from.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return listCharts(cmd.OutOrStdout(), cmd.InOrStdin(), args[0], mirrors)
		},
	}
	cmd.Flags().StringArrayVar(&mirrors, "mirror", nil, "a mirror of a chart repository, as <host>=<address>, to show where each chart then comes from (repeatable)")
	return cmd
}

func listCharts(out io.Writer, in io.Reader, file string, pairs []string) error {
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(in)
	} else {
		raw, err = os.ReadFile(file) //nolint:gosec // a file the user names
	}
	if err != nil {
		return fmt.Errorf("reading the values: %w", err)
	}
	found, err := provisioner.ChartsInValues(raw)
	if err != nil {
		return fmt.Errorf("charts: %w", err)
	}
	mirrors, err := provisioner.ParseChartMirrors(pairs)
	if err != nil {
		return fmt.Errorf("--mirror: %w", err)
	}
	charts := slices.Collect(maps.Values(found))
	if err = mirrors.Check(charts); err != nil {
		return fmt.Errorf("--mirror: %w", err)
	}
	lines := map[string]string{}
	for i := range charts {
		c := &charts[i]
		line := c.String()
		if mirror := mirrors.Repository(c.Repository); mirror != c.Repository {
			line += "\t" + mirror + "/" + c.Name
		}
		lines[c.String()] = line
	}
	for _, key := range slices.Sorted(maps.Keys(lines)) {
		if _, err = fmt.Fprintln(out, lines[key]); err != nil {
			return fmt.Errorf("writing the list: %w", err)
		}
	}
	return nil
}
