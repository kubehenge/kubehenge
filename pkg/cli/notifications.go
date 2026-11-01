package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// notificationSent is a notification target's last sending, as the interface answers it.
type notificationSent struct {
	Kind  string    `json:"kind"`
	At    time.Time `json:"at,omitzero"`
	Title string    `json:"title,omitempty"`
	Error string    `json:"error,omitempty"`
}

// notificationTargets names the targets by their credential's purpose.
var notificationTargets = map[string]string{"notify-webhook": "Webhook", "notify-ntfy": "ntfy", "notify-email": "Email"}

func (o *applianceOptions) notificationsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notifications",
		Short: "Show each notification target's last sending",
		Long: `Show each notification target's last sending. The targets are set on the Administration page, or
as the Secrets kubehenge-notify-webhook, kubehenge-notify-ntfy and kubehenge-notify-email.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				var v []notificationSent
				if err := o.get(ctx, cmd, s, "/api/notifications", &v); err != nil {
					return err
				}
				return printSent(cmd, v)
			})
		},
	}
	o.signInFlags(cmd)
	o.jsonFlag(cmd)
	cmd.AddCommand(o.testNotificationsCommand())
	return cmd
}

func (o *applianceOptions) testNotificationsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Send a test notification to every target and show how each took it",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				raw, err := s.call(ctx, http.MethodPost, "/api/notifications/test", nil, nil)
				if err != nil {
					return err
				}
				if o.json {
					return printJSON(cmd, raw)
				}
				var v []notificationSent
				if err := json.Unmarshal(raw, &v); err != nil {
					return fmt.Errorf("the interface's answer: %w", err)
				}
				if err := printSent(cmd, v); err != nil {
					return err
				}
				for _, sent := range v {
					if sent.Error != "" {
						return fmt.Errorf("%s did not take the test", notificationTargets[sent.Kind])
					}
				}
				return nil
			})
		},
	}
	o.signInFlags(cmd)
	o.jsonFlag(cmd)
	return cmd
}

func printSent(cmd *cobra.Command, list []notificationSent) error {
	t := table(cmd)
	for _, sent := range list {
		name := notificationTargets[sent.Kind]
		switch {
		case sent.At.IsZero():
			row(t, name, "nothing sent")
		case sent.Error != "":
			row(t, name, fmt.Sprintf("failed %s: %s (%s)", sent.At.UTC().Format(time.RFC3339), sent.Title, sent.Error))
		default:
			row(t, name, fmt.Sprintf("sent %s: %s", sent.At.UTC().Format(time.RFC3339), sent.Title))
		}
	}
	return flush(t)
}
