package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

func newManifestsCommand() *cobra.Command {
	var (
		providers string
		timeout   time.Duration
	)
	cmd := &cobra.Command{
		Use:   "manifests",
		Short: "Store the management cluster's provider manifests for the Cluster API operator, checked against their digests",
		Long: `Store the management cluster's provider manifests for the Cluster API operator, checked against their digests.

Each provider's metadata and components are downloaded, checked against the SHA-256 they are
named with and stored in the ConfigMap the provider's fetchConfig selects. The management cluster's
providers chart runs this as a Helm hook, in the cluster, with the credentials of its service account.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var list []provisioner.ProviderManifests
			if err := json.Unmarshal([]byte(providers), &list); err != nil {
				return fmt.Errorf("--providers: %w", err)
			}
			return inCluster(cmd.Context(), timeout, func(ctx context.Context, c client.Client) error {
				return provisioner.StoreProviderManifests(ctx, c, &http.Client{Timeout: time.Minute}, list, waitPoll)
			})
		},
	}
	cmd.Flags().StringVar(&providers, "providers", "",
		`the providers as JSON: [{"kind", "name", "namespace", "version", "metadata", "components"}], each file as <https URL>@sha256:<digest>`)
	cmd.Flags().DurationVar(&timeout, "timeout", defaultWaitTimeout, "how long to keep trying the downloads before failing")
	_ = cmd.MarkFlagRequired("providers")
	return cmd
}
