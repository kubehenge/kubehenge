package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// versionFlag is the flag and the API field that carry the Kubernetes version.
const versionFlag = "version"

// upgradeChecks is what the interface found an upgrade would run into inside the cluster.
type upgradeChecks struct {
	Lines []string `json:"lines"`
}

func (o *clusterOptions) upgradeCommand() *cobra.Command {
	var version string
	var yes bool
	cmd := &cobra.Command{
		Use:   "upgrade NAME --version VERSION",
		Short: "Upgrade a cluster's Kubernetes, after listing the removed APIs and fail-closed webhooks it meets",
		Long: `Upgrade a cluster's Kubernetes to the version given, one minor version at a time.

Before it asks, it prints what the upgrade would run into inside the cluster: the APIs in use that
the version removes, and the admission webhooks that reject every request when their backend is
down and whose Service has fewer than two ready endpoints, or all of them on one node. --yes goes
past the list without asking.`,
		Args: argsSay(cobra.ExactArgs(1), clusterNameArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			in, out := cmd.InOrStdin(), cmd.OutOrStdout()
			return o.upgrade(cmd.Context(), out, args[0], version, func() bool { return yes || confirmed(in, out) })
		},
	}
	cmd.Flags().StringVar(&version, versionFlag, "", "the Kubernetes version to upgrade to")
	cmd.Flags().BoolVar(&yes, "yes", false, "upgrade without asking when the checks list something")
	_ = cmd.MarkFlagRequired(versionFlag)
	o.namespaceFlag(cmd.Flags())
	return cmd
}

// upgrade lists what the upgrade meets and, when it lists something, goes on only if goOn says so.
func (o *clusterOptions) upgrade(ctx context.Context, out io.Writer, name, version string, goOn func() bool) error {
	path, err := o.clusterPath(ctx, name)
	if err != nil {
		return err
	}
	body, err := o.call(ctx, http.MethodGet, path+"/upgrade-checks?version="+url.QueryEscape(version), nil)
	if err != nil {
		return err
	}
	var found upgradeChecks
	if err := json.Unmarshal(body, &found); err != nil {
		return fmt.Errorf("reading the upgrade checks: %w", err)
	}
	if len(found.Lines) > 0 {
		_, _ = fmt.Fprintf(out, "Upgrading %s to %s would run into:\n", name, version)
		for _, line := range found.Lines {
			_, _ = fmt.Fprintf(out, "  - %s\n", line)
		}
		if !goOn() {
			return errors.New("upgrade cancelled: add --yes to go past the list")
		}
	}
	if _, err := o.call(ctx, http.MethodPatch, path, map[string]any{versionFlag: version}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "cluster %s upgrading to %s\n", name, version)
	return nil
}

// confirmed asks whether to go on and reads the answer, no for anything but y or yes.
func confirmed(in io.Reader, out io.Writer) bool {
	_, _ = fmt.Fprint(out, "Upgrade anyway? [y/N] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}
