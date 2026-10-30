package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubehenge/kubehenge/pkg/diagnose"
)

// diagnoseTimeout bounds the reading of a bundle, the providers' logs being the most of it.
const diagnoseTimeout = 5 * time.Minute

// bundleMode keeps a bundle to its writer: its logs may hold what the redaction misses.
const bundleMode = 0o600

func newDiagnoseCommand() *cobra.Command {
	var output, kubeconfig, classNamespace string
	cmd := &cobra.Command{
		Use:   "diagnose <namespace>/<cluster>",
		Short: "Write a cluster's support bundle",
		Long: `Write a cluster's support bundle, a .tar.gz of what helps find why it is stuck.

It holds the Cluster and every object of Cluster API and its providers that belongs to it, the boot
logs the controller read for its machines, their events, and the lines of Kubehenge's controller's,
capmox's and the Add-on Provider for Helm's recent logs that name them; run on Kubehenge's
appliance, the lines of the interface's journal that name them too. It holds no Secret, and
passwords and tokens in what it holds are replaced by ` + "[REDACTED]" + `. It reads the management
cluster through --kubeconfig, or KUBECONFIG.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, name, ok := strings.Cut(args[0], "/")
			if !ok || namespace == "" || name == "" {
				return errors.New("name the cluster as <namespace>/<cluster>")
			}
			cfg, err := managementConfig(kubeconfig)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), diagnoseTimeout)
			defer cancel()
			if output == "" {
				output = diagnose.Name(namespace, name, time.Now())
			}
			err = writeBundle(ctx, cfg, classNamespace, namespace, name, output)
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintln(cmd.OutOrStdout(), output); err != nil {
				return fmt.Errorf("naming the bundle: %w", err)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&output, "output", "o", "", "file to write; default <namespace>-<cluster>-<UTC time>.tar.gz")
	f.StringVar(&kubeconfig, "kubeconfig", "", "the management cluster's kubeconfig; default KUBECONFIG, or the cluster the command runs in")
	f.StringVar(&classNamespace, "class-namespace", "kubehenge-system", "namespace of Kubehenge's ClusterClasses and controller")
	return cmd
}

func managementConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("--kubeconfig: %w", err)
		}
		return cfg, nil
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("the management cluster: %w", err)
	}
	return cfg, nil
}

func newClientset(cfg *rest.Config) (kubernetes.Interface, error) { //nolint:ireturn // the bundle takes client-go's interface, which its tests fill with client-go's fake
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("a client of the management cluster: %w", err)
	}
	return cs, nil
}

// writeBundle writes the bundle to a new file, which goes again when the bundle fails.
func writeBundle(ctx context.Context, cfg *rest.Config, classNamespace, namespace, name, output string) error {
	scheme, err := interfaceScheme()
	if err != nil {
		return err
	}
	kube, err := newClient(cfg, scheme)
	if err != nil {
		return err
	}
	cs, err := newClientset(cfg)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, bundleMode) //nolint:gosec // the file the user names to write the bundle to
	if err != nil {
		return fmt.Errorf("--output: %w", err)
	}
	src := &diagnose.Source{Kube: kube, Clientset: cs, ControllerNamespace: classNamespace, Journal: diagnose.ApplianceJournal}
	err = errors.Join(diagnose.Write(ctx, f, src, namespace, name), f.Close())
	if err != nil {
		_ = os.Remove(output)
		return err
	}
	return nil
}
