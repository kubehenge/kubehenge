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
		Short: "Stand between a workload cluster's CSI plugin and the Proxmox VE API, for volume snapshots",
		Long: `Stand between a workload cluster's Proxmox VE CSI plugin and the Proxmox VE API, beside the plugin's
controller in clusters that take volume snapshots.

Every call goes on to the API with the cluster's own token, which it carries. A volume copy, which
Proxmox VE lets only root@pam make, the proxy makes with that token instead: it imports the volume
into a new disk of one of the cluster's holder VMs on the volume's node, detaches it as an unused
disk of the holder, and records in the namespace ` + csiproxy.Namespace + ` which name the plugin
knows the copy by; it turns that name into the copy's real one in every later call, and back in
every answer. A copy is deleted through its holder. Runs in the workload cluster, with the plugin's
service account; POD_NAME and POD_NAMESPACE name the pod its events are about.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCSIProxy(cmd.Context(), conn, &o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", "127.0.0.1:8006", "address the CSI plugin reaches the proxy on; keep it on loopback, as the calls carry the cluster's token over plain HTTP")
	f.IntVar(&o.owner, "volume-owner", 0, "VMID of the cluster's volume owner VM, which the plugin names the cluster's volumes after")
	f.StringVar(&o.pool, "pool", "", "the cluster's Proxmox VE pool, where its holder VMs are")
	f.StringVar(&o.holders, "holders", "", "the name of the cluster's holder VMs, whose unused disks the copies are")
	f.BoolVar(&o.copies, "copies", true, "make the copies the plugin asks for; off, only the copies made before are found")
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
