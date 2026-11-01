package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
	"github.com/kubehenge/kubehenge/pkg/release"
	"github.com/kubehenge/kubehenge/pkg/templates"
	"github.com/kubehenge/kubehenge/pkg/ui"
)

const (
	defaultSessionMaxAge = 8 * time.Hour
	// releaseChannelTimeout bounds a read of RKE2's and k3s's release channels, which a wizard
	// waits on.
	releaseChannelTimeout = 30 * time.Second
	// interfaceRequestTimeout bounds the interface's own requests. Headlamp's run on.
	interfaceRequestTimeout = 5 * time.Minute
	// shutdownTimeout is how long requests under way may take to finish on shutdown.
	shutdownTimeout = 10 * time.Second

	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	maxHeaderBytes    = 64 << 10
	defaultHTTPSPort  = 443
)

// serveOptions are the serve command's flags.
type serveOptions struct {
	s            ui.Server
	a            appliance.Appliance
	updateSpool  string
	releaseIndex string
	listen       string
	certFile     string
	keyFile      string
	logs         logOptions

	headlampListen     string
	headlampUpstream   string
	headlampKubeconfig string
}

func newServeCommand(conn *pve.Config) *cobra.Command {
	o := &serveOptions{}
	s, a := &o.s, &o.a
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve Kubehenge's web interface",
		Long: `Serve Kubehenge's web interface.

Users sign in with their Proxmox VE accounts, through any realm. Their privileges on each cluster's
pool decide what they may do. VM.Audit shows a cluster. VM.Allocate creates one (on the workload
pool), scales, upgrades, changes add-ons and deletes. Sys.Modify downloads its admin kubeconfig.
The interface acts in the management cluster with its own kubeconfig.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.Context(), conn)
		},
	}
	f := cmd.Flags()
	proxmoxFlags(f, conn)
	f.StringVar(&o.listen, "listen", ":8443", "address to serve on")
	f.StringVar(&o.certFile, "tls-cert", "", "PEM file with the interface's certificate and any intermediates")
	f.StringVar(&o.keyFile, "tls-key", "", "PEM file with the certificate's private key")
	f.StringVar(&s.WorkloadPool, "workload-pool", "kubehenge", "Proxmox VE pool the clusters' own pools nest under, as the controller's --workload-pool")
	f.StringVar(&s.ClassNamespace, "class-namespace", "kubehenge-system", "namespace of Kubehenge's ClusterClasses")
	f.StringVar(&s.Namespace, "namespace", provisioner.InterfaceNamespace,
		"namespace clusters are created in, the chart's interface.namespace. A list in the chart's ConfigMap kubehenge-interface in --class-namespace (interface.extraNamespaces) takes its place. The interface changes only the clusters in these namespaces")
	f.DurationVar(&s.SessionMaxAge, "session-max-age", defaultSessionMaxAge, "how long a sign-in lasts, however active the user")
	f.StringVar(&s.PublicURL, "public-url", os.Getenv("KUBEHENGE_PUBLIC_URL"),
		"address users reach the interface at, such as https://kubehenge.example. OpenID sign-in needs it, for identity providers to send users back (env KUBEHENGE_PUBLIC_URL)")
	f.StringVar(&a.Dir, "appliance-dir", "", "run as Kubehenge's appliance, keeping the management cluster's state in this directory and setting it up from the interface. "+
		"Without it, the management cluster is the one in KUBECONFIG")
	f.StringVar(&a.KeyDir, "key-dir", "", "with --appliance-dir, a directory on a volume the appliance's backups leave out. It holds the key that seals what automatic upgrades of the management cluster keep. Empty, those upgrades cannot be switched on")
	f.StringVar(&o.headlampListen, "headlamp-listen", "", "address to serve the shared Headlamp on, behind the interface's sign-in, or empty for none")
	f.StringVar(&o.headlampUpstream, "headlamp-upstream", "", "the Headlamp server, such as http://127.0.0.1:4466")
	f.StringVar(&o.headlampKubeconfig, "headlamp-kubeconfig", "", "file the Headlamp server reads its clusters from, written by the interface")
	f.IntVar(&s.ApplianceVMID, "appliance-vmid", applianceVMIDDefault(), "the appliance's guest ID in Proxmox VE, for telling administrators whether a backup job includes it (env KUBEHENGE_APPLIANCE_VMID)")
	f.StringVar(&a.Module, "module", "/usr/share/kubehenge/management-cluster", "the management-cluster module the appliance applies")
	f.StringVar(&a.Tofu, "tofu", "/usr/local/bin/tofu", "the OpenTofu binary the appliance runs")
	f.StringVar(&o.updateSpool, "update-spool", "/var/lib/kubehenge-update", "the appliance updater's spool, where upgrades are requested")
	f.StringVar(&o.releaseIndex, "release-index", releaseIndexDefault(), "URL of the signed index of Kubehenge's releases (env KUBEHENGE_RELEASE_INDEX)")
	o.logs.addFlags(f, "log in development mode, naming where in the code each line was logged. Lines stay in JSON")
	return cmd
}

func (o *serveOptions) run(ctx context.Context, conn *pve.Config) error {
	s := &o.s
	if o.certFile == "" || o.keyFile == "" {
		return errors.New("--tls-cert and --tls-key are required: the interface carries Proxmox VE passwords and is served over HTTPS only")
	}
	if err := provisioner.ValidWorkloadPool(s.WorkloadPool); err != nil {
		return fmt.Errorf("--workload-pool: %w", err)
	}
	pair, err := tls.LoadX509KeyPair(o.certFile, o.keyFile)
	if err != nil {
		return fmt.Errorf("--tls-cert and --tls-key: %w", err)
	}
	s.CertificateExpires = pair.Leaf.NotAfter
	accounts, err := pve.NewAccounts(*conn)
	if err != nil {
		return fmt.Errorf("the Proxmox VE API: %w", err)
	}
	scheme, err := interfaceScheme()
	if err != nil {
		return err
	}
	s.NewKube = func(kubeconfig []byte) (client.Client, error) {
		cfg, cfgErr := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
		if cfgErr != nil {
			return nil, fmt.Errorf("parsing the kubeconfig: %w", cfgErr)
		}
		return newClient(cfg, scheme)
	}
	s.Accounts, s.Assets, s.ProxmoxURL = accounts, ui.Frontend(), conn.URL
	s.Releases = &provisioner.ChannelServer{
		HTTP: &http.Client{Timeout: releaseChannelTimeout}, URLs: provisioner.DefaultChannelURLs, Pins: provisioner.DefaultReleasePins(),
	}
	s.TalosReleases = templates.DefaultSources().TalosReleases
	if s.Log, err = o.logs.slogLogger(os.Stderr); err != nil {
		return err
	}

	if o.a.Dir == "" {
		cfg, cfgErr := ctrl.GetConfig()
		if cfgErr != nil {
			return fmt.Errorf("the management cluster: %w", cfgErr)
		}
		if s.Kube, err = newClient(cfg, scheme); err != nil {
			return err
		}
		s.NewClientset = func() (kubernetes.Interface, error) { return newClientset(cfg) }
	} else if err = o.setUpAppliance(conn); err != nil {
		return err
	}
	if err = o.setUpHeadlamp(); err != nil {
		return err
	}
	return o.serve(ctx)
}

func interfaceScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("the interface's scheme: %w", err)
		}
	}
	return scheme, nil
}

func newClient(cfg *rest.Config, scheme *runtime.Scheme) (client.Client, error) {
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("a client of the management cluster: %w", err)
	}
	return c, nil
}

// setUpAppliance makes the interface the appliance's, which sets up the management cluster, builds
// templates and upgrades itself.
func (o *serveOptions) setUpAppliance(conn *pve.Config) error {
	s, a := &o.s, &o.a
	if conn.CAFile == "" {
		return errors.New("--proxmox-ca is required in the appliance: the management cluster verifies the Proxmox VE API with it")
	}
	ca, err := os.ReadFile(conn.CAFile)
	if err != nil {
		return fmt.Errorf("--proxmox-ca: %w", err)
	}
	a.ProxmoxURL, a.ProxmoxCA, a.ProxmoxInsecure, a.Log, a.Release = conn.URL, string(ca), conn.Insecure, s.Log, release.Version
	s.ProxmoxInsecure = conn.Insecure
	a.InterfaceKubeconfig = appliance.InterfaceKubeconfig
	a.Talos = &provisioner.ManagementTalos{}
	a.LoadLast()
	if a.Ready() {
		kubeconfig, readErr := os.ReadFile(a.KubeconfigPath())
		if readErr != nil {
			return fmt.Errorf("the management cluster's kubeconfig: %w", readErr)
		}
		if s.Kube, err = s.NewKube(kubeconfig); err != nil {
			return fmt.Errorf("the management cluster the appliance set up: %w", err)
		}
	}
	s.Appliance = a
	// Read when asked: the appliance writes the kubeconfig once it has set the management cluster up.
	s.NewClientset = func() (kubernetes.Interface, error) {
		kubeconfig, readErr := os.ReadFile(a.KubeconfigPath())
		if readErr != nil {
			return nil, fmt.Errorf("the management cluster's kubeconfig: %w", readErr)
		}
		cfg, cfgErr := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
		if cfgErr != nil {
			return nil, fmt.Errorf("parsing the kubeconfig: %w", cfgErr)
		}
		return newClientset(cfg)
	}
	pveConfig := *conn
	s.MakeTemplate = func(ctx context.Context, t *pve.Ticket, b templates.Builder, tpl *templates.Template) (int, error) {
		session, connErr := pve.ConnectSession(pveConfig, t)
		if connErr != nil {
			return 0, fmt.Errorf("connecting to Proxmox VE as %s: %w", t.Username, connErr)
		}
		b.Client, b.Sources = session, templates.DefaultSources()
		return b.Create(ctx, tpl)
	}
	s.Upgrades = &ui.Upgrades{
		Spool:        appliance.Spool(o.updateSpool),
		Index:        o.releaseIndex,
		HTTP:         &http.Client{Timeout: time.Minute},
		SettingsPath: filepath.Join(a.Dir, "upgrades.json"),
		Now:          time.Now,
		Current:      release.Version,
		Keys:         release.PublicKeys(),
	}
	return nil
}

// setUpHeadlamp puts the shared Headlamp behind the interface's sign-in, when asked for.
func (o *serveOptions) setUpHeadlamp() error {
	if o.headlampListen == "" {
		return nil
	}
	if o.headlampUpstream == "" || o.headlampKubeconfig == "" {
		return errors.New("--headlamp-listen needs --headlamp-upstream and --headlamp-kubeconfig")
	}
	up, err := url.Parse(o.headlampUpstream)
	if err != nil || up.Scheme != "http" || up.Host == "" {
		return errors.New("--headlamp-upstream is an http:// address on the loopback interface")
	}
	port, err := listenPort(o.headlampListen)
	if err != nil {
		return fmt.Errorf("--headlamp-listen: %w", err)
	}
	interfacePort, err := listenPort(o.listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	o.s.Headlamp = &ui.Headlamp{Upstream: up, Kubeconfig: o.headlampKubeconfig, Port: port, InterfacePort: interfacePort}
	return nil
}

// serve runs the interface and, with it, Headlamp's port until ctx ends or one of them stops.
func (o *serveOptions) serve(ctx context.Context) error {
	s := &o.s
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { s.Run(ctx) })
	if o.a.Dir != "" {
		wg.Go(func() { o.a.RenewInterfaceToken(ctx) })
	}
	servers := []*http.Server{newServer(o.listen, s.Handler(), interfaceRequestTimeout)}
	if s.Headlamp != nil {
		servers = append(servers, newServer(o.headlampListen, s.HeadlampHandler(), 0))
	}
	wg.Go(func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer done()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdown)
		}
	})
	errs := make(chan error, len(servers))
	for _, srv := range servers {
		s.Log.Info("serving", "address", srv.Addr)
		go func() { errs <- srv.ListenAndServeTLS(o.certFile, o.keyFile) }()
	}
	// The first to stop takes the others down with it. The process ends when serve returns, so it
	// waits for the servers' shutdown, which lets the requests under way finish, and for the loops
	// that write the appliance's files.
	err := <-errs
	cancel()
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serving: %w", err)
}

// newServer serves h over HTTPS. A timeout of 0 lets requests run on: a watch or a terminal through
// Headlamp lasts as long as the user keeps it open.
func newServer(addr string, h http.Handler, timeout time.Duration) *http.Server {
	readTimeout := time.Duration(0)
	if timeout != 0 {
		readTimeout = time.Minute
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      timeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// listenPort is the port of a listen address, 443 when it names none.
func listenPort(addr string) (int, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("listen address %q: %w", addr, err)
	}
	if port == "" {
		return defaultHTTPSPort, nil
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0, fmt.Errorf("listen address %q: %w", addr, err)
	}
	return n, nil
}

func applianceVMIDDefault() int {
	id, _ := strconv.Atoi(os.Getenv("KUBEHENGE_APPLIANCE_VMID"))
	return id
}
