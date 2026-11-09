package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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
	// jobStopTimeout is how long an interrupted run has to write its state on shutdown. It stays
	// under kubehenge.service's TimeoutStopSec, which stays under the 180 seconds Proxmox VE gives
	// a container to shut down for a migration.
	jobStopTimeout = 150 * time.Second

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
	demo         bool
	demoTime     string
	// listenChanged tells whether --listen was given, which the demo's own default yields to.
	listenChanged bool

	trustedProxies []string

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
			o.listenChanged = cmd.Flags().Changed("listen")
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
	f.StringSliceVar(&o.trustedProxies, "trusted-proxy", trustedProxiesDefault(),
		"address or CIDR range of a reverse proxy in front of the interface, repeatable. From one, the sign-in limits count the client in X-Forwarded-For, "+
			"so one person's failures no longer lock out everyone behind the proxy. Without it forwarding headers are ignored (env KUBEHENGE_TRUSTED_PROXIES, comma-separated)")
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
	f.BoolVar(&o.demo, "demo", false, "serve the interface with sample clusters and no Proxmox VE or management cluster behind it. "+
		"Changes are refused. Without --tls-cert it serves plain HTTP, on a loopback address only")
	f.StringVar(&o.demoTime, "demo-time", "", "with --demo, stop the demo's clock at this RFC 3339 time, for screenshots that come out the same every run")
	o.logs.addFlags(f, "log in development mode, naming where in the code each line was logged. Lines stay in JSON")
	return cmd
}

// demoListen is where the demo serves unless --listen says otherwise: plain HTTP stays on the
// machine it runs on.
const demoListen = "127.0.0.1:8443"

// trustedProxiesDefault is the list KUBEHENGE_TRUSTED_PROXIES holds, split at commas and spaces.
func trustedProxiesDefault() []string {
	return strings.FieldsFunc(os.Getenv("KUBEHENGE_TRUSTED_PROXIES"), func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}

func (o *serveOptions) parseTrustedProxies() error {
	proxies, err := ui.ParseTrustedProxies(o.trustedProxies)
	if err != nil {
		return fmt.Errorf("--trusted-proxy: %w", err)
	}
	o.s.TrustedProxies = proxies
	return nil
}

func (o *serveOptions) run(ctx context.Context, conn *pve.Config) error {
	s := &o.s
	if err := o.parseTrustedProxies(); err != nil {
		return err
	}
	if o.demo {
		return o.runDemo(ctx)
	}
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
	if o.a.Dir != "" {
		knowTheAppliance(s, pair.Leaf, o.a.Dir)
	}
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
		HTTP: savedProxyClient(o.a.Dir, conn.URL, releaseChannelTimeout), URLs: provisioner.DefaultChannelURLs, Pins: provisioner.DefaultReleasePins(),
		Store: &provisioner.PinStore{Reader: s.ManagementReader(), Namespace: s.ClassNamespace},
	}
	s.PinChecks = &provisioner.ReleaseChecksums{
		HTTP: savedProxyClient(o.a.Dir, conn.URL, pinFetchTimeout), GitHub: pinsSources.github, Raw: pinsSources.raw,
		Mirror: o.savedReleaseMirror,
	}
	sources := templates.DefaultSources()
	sources.HTTP = savedProxyClient(o.a.Dir, conn.URL, sources.HTTP.Timeout)
	s.TalosReleases = sources.TalosReleases
	if s.Log, err = o.logs.slogLogger(os.Stderr); err != nil {
		return err
	}
	// Without a logger, controller-runtime drops its own messages and says so once.
	logger, err := o.logs.zapLogger()
	if err != nil {
		return err
	}
	setControllerLogger(logger)

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

// runDemo serves the interface over sample clusters held in memory. Nothing it does reaches a
// Proxmox VE host or a cluster: every client behind the interface is the demo's own.
func (o *serveOptions) runDemo(ctx context.Context) error {
	if (o.certFile == "") != (o.keyFile == "") {
		return errors.New("--tls-cert and --tls-key go together")
	}
	if o.certFile == "" {
		if !o.listenChanged {
			o.listen = demoListen
		}
		if !loopback(o.listen) {
			return fmt.Errorf("--listen %s: without --tls-cert the demo serves on a loopback address only, such as %s", o.listen, demoListen)
		}
	}
	if o.headlampListen != "" || o.a.Dir != "" {
		return errors.New("--demo takes no --headlamp-listen or --appliance-dir")
	}
	var err error
	if o.s.Log, err = o.logs.slogLogger(os.Stderr); err != nil {
		return err
	}
	clock := time.Now
	if o.demoTime != "" {
		at, parseErr := time.Parse(time.RFC3339, o.demoTime)
		if parseErr != nil {
			return fmt.Errorf("--demo-time: %w", parseErr)
		}
		clock = func() time.Time { return at }
	}
	o.s.Assets = ui.Frontend()
	cleanup, err := ui.SetUpDemo(&o.s, clock)
	if err != nil {
		return fmt.Errorf("--demo: %w", err)
	}
	defer cleanup()
	return o.serve(ctx)
}

// loopback tells whether a listen address names a loopback address or localhost.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	if s.Accounts != nil {
		a.NextID = s.Accounts.NextID
	}
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
		b.Client, b.Sources, b.Proxy = session, templates.DefaultSources(), savedGuestProxy(a)
		b.Sources.HTTP = savedProxyClient(a.Dir, pveConfig.URL, b.Sources.HTTP.Timeout)
		return b.Create(ctx, tpl)
	}
	s.Upgrades = &ui.Upgrades{
		Spool:        appliance.Spool(o.updateSpool),
		Index:        o.releaseIndex,
		HTTP:         savedProxyClient(a.Dir, pveConfig.URL, time.Minute),
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
		wg.Go(func() { o.a.KeepAddresses(ctx) })
		wg.Go(func() {
			<-ctx.Done()
			stopping, done := context.WithTimeout(context.WithoutCancel(ctx), jobStopTimeout)
			defer done()
			if err := o.a.Stop(stopping); err != nil {
				s.Log.Error("stopping with a run under way", "error", err)
			}
		})
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
		if o.certFile == "" {
			go func() { errs <- srv.ListenAndServe() }()
		} else {
			go func() { errs <- srv.ListenAndServeTLS(o.certFile, o.keyFile) }()
		}
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

// knowTheAppliance tells the interface the appliance's MAC and IP addresses and those its
// certificate is for, by which it finds its guest in Proxmox VE and notices an address it lost. It
// also reads the use of the disk holding the appliance's state in dir.
func knowTheAppliance(s *ui.Server, cert *x509.Certificate, dir string) {
	s.DiskUse = func() (uint64, uint64, error) { return appliance.DiskUse(dir) }
	s.ApplianceMACs, s.OwnAddresses = ownMACs(), ownAddresses
	s.CertificateNames = cert.DNSNames
	for _, ip := range cert.IPAddresses {
		if addr, ok := netip.AddrFromSlice(ip); ok {
			s.CertificateIPs = append(s.CertificateIPs, addr.Unmap())
		}
	}
}

// ownAddresses are the addresses of this machine others may reach it at: neither loopback nor
// link-local.
func ownAddresses() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var own []netip.Addr
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().IsGlobalUnicast() {
			own = append(own, p.Addr().Unmap())
		}
	}
	return own
}

// ownMACs are the MAC addresses of this machine's network interfaces, by which the appliance finds
// its own guest in Proxmox VE.
func ownMACs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var macs []string
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback == 0 && len(i.HardwareAddr) > 0 {
			macs = append(macs, i.HardwareAddr.String())
		}
	}
	return macs
}
