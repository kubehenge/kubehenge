package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// activityEntry is one thing a user did through the interface, as it answers it.
type activityEntry struct {
	At      time.Time         `json:"at"`
	User    string            `json:"user"`
	What    string            `json:"what"`
	Cluster string            `json:"cluster,omitempty"`
	Failed  bool              `json:"failed,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

type activityAnswer struct {
	Entries []activityEntry `json:"entries"`
	Error   string          `json:"error,omitempty"`
}

const activityDefaultDays = 7

func (o *applianceOptions) activityCommand() *cobra.Command {
	var days int
	var cluster string
	cmd := &cobra.Command{
		Use:   "activity",
		Short: "Show who did what through the interface, newest first",
		Long: `Show who did what through the interface, newest first: sign-ins, refused sign-ins and every
change. The whole appliance's needs Sys.Audit on /. One cluster's, with --cluster, needs only to see
the cluster. --json adds the management cluster's events of the last day.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := "/api/activity"
			if cluster != "" {
				ns, name, ok := strings.Cut(cluster, "/")
				if !ok || ns == "" || name == "" {
					return errors.New("--cluster takes namespace/name")
				}
				path = "/api/clusters/" + url.PathEscape(ns) + "/" + url.PathEscape(name) + "/activity"
			}
			path += "?days=" + strconv.Itoa(days)
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				var v activityAnswer
				if err := o.get(ctx, cmd, s, path, &v); err != nil {
					return err
				}
				return printActivity(cmd, &v)
			})
		},
	}
	cmd.Flags().IntVar(&days, "days", activityDefaultDays, "how many days back, up to 90")
	cmd.Flags().StringVar(&cluster, "cluster", "", "only what was done to this cluster, as namespace/name")
	o.signInFlags(cmd)
	o.jsonFlag(cmd)
	return cmd
}

func printActivity(cmd *cobra.Command, v *activityAnswer) error {
	if v.Error != "" {
		return errors.New(v.Error)
	}
	if len(v.Entries) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "Nothing done")
		return err //nolint:wrapcheck // a failed write to the terminal is reported as it is
	}
	t := table(cmd)
	for _, e := range v.Entries {
		var details []string
		for _, k := range slices.Sorted(maps.Keys(e.Details)) {
			if k != "requestId" && k != "auditId" {
				details = append(details, k+" "+e.Details[k])
			}
		}
		what := e.What
		if e.Failed {
			what += " (failed)"
		}
		row(t, e.At.UTC().Format(time.RFC3339), e.User, what, e.Cluster, strings.Join(details, ", "))
	}
	return flush(t)
}
