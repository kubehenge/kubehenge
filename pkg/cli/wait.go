package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	defaultWaitTimeout = 10 * time.Minute
	// waitPoll is how often the operator and its providers are looked at again.
	waitPoll = 2 * time.Second
)

func newWaitCommand() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Wait for the Cluster API operator and its providers",
		Long: `Wait for the Cluster API operator and its providers.

The management cluster's providers chart runs these as Helm hooks, in the cluster, with the
credentials of their service account.`,
	}
	cmd.PersistentFlags().DurationVar(&timeout, "timeout", defaultWaitTimeout, "how long to wait before failing")

	var namespace string
	operator := &cobra.Command{
		Use:   "operator",
		Short: "Wait until the operator's webhooks admit a provider",
		Long: `Wait until the operator's webhooks admit a provider.

It tries a dry-run create of an AddonProvider named webhook-probe in --namespace until the webhooks
admit it. A dry run keeps nothing.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return inCluster(cmd.Context(), timeout, func(ctx context.Context, c client.Client) error {
				return provisioner.WaitForOperator(ctx, c, namespace, waitPoll)
			})
		},
	}
	operator.Flags().StringVar(&namespace, "namespace", "", "namespace to create the probe in")
	_ = operator.MarkFlagRequired("namespace")

	providers := &cobra.Command{
		Use:   "providers",
		Short: "Wait until the operator has applied every provider and their Deployments have rolled out",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return inCluster(cmd.Context(), timeout, func(ctx context.Context, c client.Client) error {
				return provisioner.WaitForProviders(ctx, c, waitPoll, cmd.ErrOrStderr())
			})
		},
	}
	cmd.AddCommand(operator, providers)
	return cmd
}

// inCluster runs a hook's work, for at most timeout, with a client of the cluster the command runs in.
func inCluster(ctx context.Context, timeout time.Duration, work func(context.Context, client.Client) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("the cluster to run in: %w", err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme} {
		if err = add(scheme); err != nil {
			return fmt.Errorf("the hook client's scheme: %w", err)
		}
	}
	c, err := newClient(cfg, scheme)
	if err != nil {
		return err
	}
	return work(ctx, c)
}
