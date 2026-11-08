package cli

import (
	"fmt"
	"net/http"
	"os/user"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// pinFetchTimeout bounds the read of one release's checksum file and install script.
const pinFetchTimeout = time.Minute

// defaultClassNamespace is where the chart puts Kubehenge's ClusterClasses and the pins.
const defaultClassNamespace = "kubehenge-system"

func pinStoreFor(namespace string) (*provisioner.PinStore, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("the management cluster's API (KUBECONFIG): %w", err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme} {
		if err = add(scheme); err != nil {
			return nil, fmt.Errorf("the management cluster's scheme: %w", err)
		}
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("the management cluster's API: %w", err)
	}
	return &provisioner.PinStore{Reader: kube, Writer: kube, Namespace: namespace}, nil
}

func newReleasePinCommand() *cobra.Command {
	var namespace string
	var yes bool
	cmd := &cobra.Command{
		Use:   "pin <version>",
		Short: "Let clusters run an RKE2 or k3s release this Kubehenge does not carry",
		Long: `Let clusters run an RKE2 or k3s release this Kubehenge does not carry, such as v1.36.6+rke2r1.

It reads the release's checksum file and install script and prints their SHA-256. Nodes will
install the release only if both still match. With --yes the pin is saved to the ConfigMap
` + provisioner.ReleasePinsName + ` in --namespace, with who added it. It needs the management
cluster's API (KUBECONFIG) and a right to change that ConfigMap.`,
		Args: argsSay(cobra.ExactArgs(1), "give the release, such as v1.36.6+rke2r1"),
		RunE: func(cmd *cobra.Command, args []string) error {
			version := args[0]
			checks := &provisioner.ReleaseChecksums{
				HTTP: &http.Client{Timeout: pinFetchTimeout}, GitHub: pinsSources.github, Raw: pinsSources.raw,
			}
			pin, err := checks.Digests(cmd.Context(), version)
			if err != nil {
				return fmt.Errorf("pinning %s: %w", version, err)
			}
			out := cmd.OutOrStdout()
			if _, err = fmt.Fprintf(out, "%s\n  checksum file  %s\n  install script %s\n", version, pin.File, pin.Script); err != nil {
				return fmt.Errorf("printing: %w", err)
			}
			if !yes {
				_, err = fmt.Fprintln(out, "Not saved. Run it again with --yes to pin these.")
				return wrapPrint(err)
			}
			store, err := pinStoreFor(namespace)
			if err != nil {
				return err
			}
			if err = store.Add(cmd.Context(), version, pin, whoAmI(), time.Now()); err != nil {
				return fmt.Errorf("pinning %s: %w", version, err)
			}
			_, err = fmt.Fprintln(out, "pinned", version)
			return wrapPrint(err)
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", defaultClassNamespace, "namespace of Kubehenge's ClusterClasses")
	cmd.Flags().BoolVar(&yes, "yes", false, "save the pin after showing the digests")
	return cmd
}

func newReleaseUnpinCommand() *cobra.Command {
	var namespace string
	cmd := &cobra.Command{
		Use:   "unpin <version>",
		Short: "Withdraw a release pin an administrator added",
		Long: `Withdraw a release pin added with kubehenge release pin. It is refused while a cluster runs the
release, and for a release this Kubehenge carries.`,
		Args: argsSay(cobra.ExactArgs(1), "give the release, such as v1.36.6+rke2r1"),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := pinStoreFor(namespace)
			if err != nil {
				return err
			}
			if err = store.Remove(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("unpinning %s: %w", args[0], err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "unpinned", args[0])
			return wrapPrint(err)
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", defaultClassNamespace, "namespace of Kubehenge's ClusterClasses")
	return cmd
}

func wrapPrint(err error) error {
	if err != nil {
		return fmt.Errorf("printing: %w", err)
	}
	return nil
}

// whoAmI names the operating system user running the command, for the pin's record.
func whoAmI() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}
