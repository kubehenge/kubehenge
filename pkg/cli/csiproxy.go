package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/csiproxy"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// csiProxyPoll is how often a waiting copy's request is read: a copy takes seconds at the least,
// and the controller looks for new requests every few.
const csiProxyPoll = 2 * time.Second

func newCSIProxyCommand(conn *pve.Config) *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "csi-proxy",
		Short: "Stand between a workload cluster's CSI plugin and the Proxmox VE API, for volume snapshots",
		Long: `Stand between a workload cluster's Proxmox VE CSI plugin and the Proxmox VE API, beside the plugin's
controller in clusters that take volume snapshots.

Every call goes on to the API with the cluster's own token, which it carries. A volume copy, which
Proxmox VE lets only root@pam make, becomes a ConfigMap in the namespace ` + csiproxy.Namespace + `
instead, which Kubehenge's controller in the management cluster carries out as root@pam once it has
checked that both volumes are the cluster's own; the call returns when the copy is done. Runs in
the workload cluster, with its service account.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCSIProxy(cmd.Context(), conn, listen)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8006", "address the CSI plugin reaches the proxy on; keep it on loopback, as the calls carry the cluster's token over plain HTTP")
	return cmd
}

func runCSIProxy(ctx context.Context, conn *pve.Config, listen string) error {
	upstream, transport, err := conn.Upstream()
	if err != nil {
		return fmt.Errorf("reaching Proxmox VE: %w", err)
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("the workload cluster's API: %w", err)
	}
	c, err := client.New(config, client.Options{})
	if err != nil {
		return fmt.Errorf("the workload cluster's API: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	p := &csiproxy.Proxy{Upstream: upstream, Transport: transport, Client: c, Poll: csiProxyPoll, Log: log}
	srv := &http.Server{
		Addr:              listen,
		Handler:           p.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer done()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("serving", "address", listen, "proxmox", upstream.String())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}
