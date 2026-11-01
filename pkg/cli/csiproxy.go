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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/csiproxy"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

// csiProxyPoll is how often a copy's task is looked at: a copy takes seconds at the least.
const csiProxyPoll = 2 * time.Second

// csiProxyOptions are the csi-proxy command's flags.
type csiProxyOptions struct {
	listen  string
	owner   int
	pool    string
	holders string
	copies  bool
}

func newCSIProxyCommand(conn *pve.Config) *cobra.Command {
	var o csiProxyOptions
	cmd := &cobra.Command{
		Use:   "csi-proxy",
		Short: "Proxy a workload cluster's CSI plugin to the Proxmox VE API, for volume snapshots",
		Long: `Proxy a workload cluster's Proxmox VE CSI plugin to the Proxmox VE API. It runs in the workload
cluster beside the plugin's controller, with the plugin's service account, in clusters that take
volume snapshots.

Every call goes on to the API with the cluster's own token, which the call carries. Proxmox VE lets
only root@pam copy a volume, so the proxy makes the copy with that token instead. It imports the
volume into a new disk of one of the cluster's holder VMs on the volume's node, then detaches it as
an unused disk. It records the name the plugin knows the copy by in the namespace ` + csiproxy.Namespace + `.
Every later call and answer is translated between that name and the real one. A copy is deleted
through its holder. POD_NAME and POD_NAMESPACE name the pod its events are about.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCSIProxy(cmd.Context(), conn, &o)
		},
	}
	f := cmd.Flags()
	proxmoxFlags(f, conn)
	f.StringVar(&o.listen, "listen", "127.0.0.1:8006", "address the CSI plugin reaches the proxy on. Keep it on loopback, since calls carry the cluster's token over plain HTTP")
	f.IntVar(&o.owner, "volume-owner", 0, "VMID of the cluster's volume owner VM, which the plugin names the cluster's volumes after")
	f.StringVar(&o.pool, "pool", "", "the cluster's Proxmox VE pool, where its holder VMs are")
	f.StringVar(&o.holders, "holders", "", "name of the cluster's holder VMs, which keep the copies as unused disks")
	f.BoolVar(&o.copies, "copies", true, "make the copies the plugin asks for. Off, the proxy only finds copies made before")
	return cmd
}

func runCSIProxy(ctx context.Context, conn *pve.Config, o *csiProxyOptions) error {
	if o.owner <= 0 || o.pool == "" || o.holders == "" {
		return errors.New("--volume-owner, --pool and --holders are needed")
	}
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
	p := &csiproxy.Proxy{
		Upstream: upstream, Transport: transport, Client: c, Log: log, Poll: csiProxyPoll,
		Owner: o.owner, Pool: o.pool, Holders: o.holders, Copies: o.copies,
		Pod: types.NamespacedName{Namespace: os.Getenv("POD_NAMESPACE"), Name: os.Getenv("POD_NAME")},
	}
	srv := &http.Server{
		Addr:              o.listen,
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
	log.Info("serving", "address", o.listen, "proxmox", upstream.String(), "volumeOwner", o.owner, "pool", o.pool, "copies", o.copies)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}
