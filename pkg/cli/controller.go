package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	"sigs.k8s.io/cluster-api/exp/runtime/server"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
	"github.com/kubehenge/kubehenge/pkg/pve"
)

const (
	// One replica runs; the lease only hands over during a rollout, which releasing it on shutdown
	// makes immediate. Renewing every 10 s rather than 2 s spares etcd a write every other second,
	// and a 40 s deadline rides out a management API server that stalls for some seconds instead of
	// restarting every controller.
	leaseDuration = 60 * time.Second
	renewDeadline = 40 * time.Second
	retryPeriod   = 10 * time.Second

	defaultUpgradeInterval = 5 * time.Minute
	defaultExtensionPort   = 9443

	// placementInterval is how soon a node that goes down is left out of the clusters' placement:
	// well inside the minutes a machine health check waits before replacing a VM it ran.
	placementInterval = 15 * time.Second
	// templateVersionsInterval is how soon a kubeadm template just built is offered in the interface.
	templateVersionsInterval = 30 * time.Second
	// haRulesInterval is how soon an HA rule made in Proxmox VE is offered to cluster creators.
	haRulesInterval = 30 * time.Second
	// templateRequestsInterval is how soon a build asked for in the interface starts.
	templateRequestsInterval = 15 * time.Second
)

// controllerOptions are the controller command's flags.
type controllerOptions struct {
	r                 provisioner.Reconciler
	settings          provisioner.Settings
	port              int
	certDir           string
	healthAddr        string
	leaderElect       bool
	ccmChart          string
	csiChart          string
	csiNewestChart    string
	ciliumChart       string
	calicoChart       string
	headlampChart     string
	certManagerChart  string
	rancherChart      string
	rancherKubernetes string
	turtlesImage      string
	turtlesCAPIImage  string
	monitoringChart   string
	kubeRBACProxy     string
	lokiChart         string
	alloyChart        string
	alloyImage        string
	veleroChart       string
	veleroImage       string
	veleroPlugin      string
	kyvernoChart      string
	kyvernoPolicies   string
	istioChart        string
	istioBaseChart    string
	istioCNIChart     string
	istioZtunnelChart string
	traefikChart      string
	metalLBChart      string
	kubeVipChart      string
	kubeVipImage      string
	kubeVipCloudChart string
	chartValues       string
	calicoImages      []string
	extensionConfig   string
	upgradeInterval   time.Duration
	classProviders    []string
	offerHARules      bool
	nodeImageSoak     time.Duration
	templateRefresh   bool
}

func newControllerCommand(conn *pve.Config) *cobra.Command {
	o := &controllerOptions{}
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run Kubehenge's controller in the management cluster",
		Long: `Run Kubehenge's controller in the management cluster.

It gives each cluster built from a Kubehenge ClusterClass its own Proxmox VE pool, and a user and
API token scoped to that pool for the cluster's cloud controller manager and CSI plugin, before
Cluster API creates any of its VMs (as the BeforeClusterCreate hook of a Runtime Extension), and
removes them once the cluster's VMs are gone. It installs the add-ons each cluster switches on, and
registers every cluster with the Rancher one of them installs. A distribution's Cluster API
providers run only while a cluster uses it.

The API token is read from KUBEHENGE_PROXMOX_TOKEN. With a second one in KUBEHENGE_PROXMOX_HA_TOKEN,
holding Sys.Audit and Sys.Console on /, Proxmox VE HA keeps each cluster's control-plane VMs on
separate nodes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.Context(), conn)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.r.WorkloadPool, "workload-pool", "kubehenge", "Proxmox VE pool the clusters' own pools nest under")
	f.StringVar(&o.r.ClassNamespace, "class-namespace", os.Getenv("POD_NAMESPACE"), "namespace of Kubehenge's ClusterClasses (env POD_NAMESPACE)")
	f.StringVar(&o.settings.Group, "cluster-group", "kubehenge-clusters", "Proxmox VE group the clusters' users join")
	f.StringVar(&o.settings.Role, "cluster-role", "kubehenge-cluster", "Proxmox VE role each cluster's user gets on its own pool")
	f.StringVar(&o.settings.Node, "volume-owner-node", "", "Proxmox VE node for the VMs that own clusters' volumes; default the first online node")
	f.StringVar(&o.settings.Region, "region", "", "region the clusters see Proxmox VE as; default the Proxmox VE cluster's name")
	o.addonFlags(f)
	f.StringSliceVar(&o.r.Charts.PodSecurityExempt, "pod-security-exempt", nil, "namespaces Kyverno's pod security policies leave alone, those the ClusterClasses' Pod Security Admission exempts (comma-separated)")
	f.StringArrayVar(&o.classProviders, "class-providers", nil, "namespaces of the Cluster API providers only one ClusterClass's clusters use, as <class>=<namespace>,<namespace>; they run only while a cluster does (repeatable)")
	f.BoolVar(&o.offerHARules, "offer-ha-rules", false, "offer Proxmox VE's HA node-affinity rules, their names and nodes only, to those who may create a cluster but not read the rules, through the ConfigMap "+provisioner.HARulesName)
	f.DurationVar(&o.upgradeInterval, "upgrade-interval", defaultUpgradeInterval, "how often clusters are checked for an automatic upgrade due in their window")
	f.DurationVar(&o.nodeImageSoak, "node-image-soak", provisioner.DefaultNodeImageSoak, "how old a template build is before clusters move to it; the template refresh's --soak")
	f.BoolVar(&o.templateRefresh, "template-refresh", false, "start the template refresh CronJob's Job for the builds asked for in the interface")
	f.IntVar(&o.port, "extension-port", defaultExtensionPort, "port of the Runtime Extension server")
	f.StringVar(&o.extensionConfig, "extension-config", "kubehenge", "ExtensionConfig through which Cluster API calls this controller's hooks")
	f.StringVar(&o.certDir, "extension-cert-dir", "/etc/kubehenge/tls", "directory with tls.crt and tls.key for the Runtime Extension server")
	f.StringVar(&o.healthAddr, "health-address", ":8081", "address of the liveness and readiness probes")
	f.BoolVar(&o.leaderElect, "leader-elect", true, "elect one active controller among replicas")
	return cmd
}

// addonFlags names the charts and images Kubehenge installs into clusters, and the values beneath
// them. Every chart and image is required, so that a controller installs only what its release pins.
func (o *controllerOptions) addonFlags(f *pflag.FlagSet) {
	f.StringVar(&o.ccmChart, "ccm-chart", "", "cloud controller manager chart, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.csiChart, "csi-chart", "", "CSI plugin chart, as <repository>/<name>:<version>@sha256:<digest>")
	o.grantFlags(f)
	f.StringVar(&o.ciliumChart, "cilium-chart", "", "Cilium chart, for distributions without a CNI of their own, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.calicoChart, "calico-chart", "", "Calico's operator chart, for distributions without a CNI of their own, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.headlampChart, "headlamp-chart", "", "Headlamp chart, for clusters that switch it on, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringSliceVar(&o.calicoImages, "calico-images", nil, "the images the Tigera operator runs Calico from, all at its release, as <registry>/<repository>:<tag>@sha256:<digest> (comma-separated)")
	f.StringVar(&o.chartValues, "chart-values", "{}", "values beneath those Kubehenge gives each chart, naming its images by digest, as a JSON object by chart flag without -chart, such as {\"cert-manager\":{...}}")
	f.StringVar(&o.certManagerChart, "cert-manager-chart", "", "cert-manager chart, installed with Rancher and with monitoring, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.rancherChart, "rancher-chart", "", "Rancher chart, for the cluster that installs it, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.rancherKubernetes, "rancher-kubernetes", "", "Kubernetes minor versions the Rancher chart runs on, as <min>-<max>, such as 34-36")
	f.StringVar(&o.turtlesImage, "rancher-turtles-image", "", "the image of the Turtles release Rancher installs, as <registry>/<repository>:<tag>@sha256:<digest>")
	f.StringVar(&o.turtlesCAPIImage, "rancher-cluster-api-image", "", "the Cluster API controller image that Turtles runs, as <registry>/<repository>:<tag>@sha256:<digest>")
	f.StringVar(&o.monitoringChart, "monitoring-chart", "", "kube-prometheus-stack chart, for clusters that switch monitoring on, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.kubeRBACProxy, "kube-rbac-proxy-image", "", "kube-rbac-proxy image, in front of the metrics monitoring scrapes and of Loki, as <registry>/<repository>:<tag>@sha256:<digest>")
	f.StringVar(&o.lokiChart, "loki-chart", "", "Loki chart, for clusters that switch logs on, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.alloyChart, "alloy-chart", "", "Grafana Alloy chart, which collects the logs of clusters that switch logs on, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.alloyImage, "alloy-image", "", "Grafana Alloy image, as <registry>/<repository>:<tag>@sha256:<digest>")
	f.StringVar(&o.veleroChart, "velero-chart", "", "Velero chart, for clusters that switch backups on, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.veleroImage, "velero-image", "", "Velero image, as <registry>/<repository>:<tag>@sha256:<digest>")
	f.StringVar(&o.veleroPlugin, "velero-plugin-image", "", "image of Velero's plugin for S3, as <registry>/<repository>:<tag>@sha256:<digest>")
	f.StringVar(&o.kyvernoChart, "kyverno-chart", "", "Kyverno chart, for clusters that have Kyverno enforce pod security, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.kyvernoPolicies, "kyverno-policies-chart", "", "Kyverno's Pod Security Standards policies chart, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.istioChart, "istio-chart", "", "Istio's istiod chart, for clusters that switch Istio on, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.istioBaseChart, "istio-base-chart", "", "Istio's base chart, with its CRDs, at istiod's version, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.istioCNIChart, "istio-cni-chart", "", "Istio's cni chart at istiod's version, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.istioZtunnelChart, "istio-ztunnel-chart", "", "Istio's ztunnel chart at istiod's version, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.traefikChart, "traefik-chart", "", "Traefik chart, the ingress controller of distributions without one of their own, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.metalLBChart, "metallb-chart", "", "MetalLB chart, for clusters whose load balancer is MetalLB, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.kubeVipChart, "kube-vip-chart", "", "kube-vip chart, for clusters whose load balancer is kube-vip, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.kubeVipImage, "kube-vip-image", "", "kube-vip image, as <registry>/<repository>:<tag>@sha256:<digest>")
	f.StringVar(&o.kubeVipCloudChart, "kube-vip-cloud-provider-chart", "", "kube-vip's cloud provider chart, which hands kube-vip's Services their addresses, as <repository>/<name>:<version>@sha256:<digest>")
	for _, flag := range []string{"ccm-chart", "csi-chart", "csi-newest-chart", "cilium-chart", "calico-chart", "calico-images", "headlamp-chart", "cert-manager-chart", "rancher-chart", "rancher-kubernetes", "rancher-turtles-image", "rancher-cluster-api-image", "monitoring-chart", "kube-rbac-proxy-image", "loki-chart", "alloy-chart", "alloy-image", "velero-chart", "velero-image", "velero-plugin-image", "kyverno-chart", "kyverno-policies-chart", "istio-chart", "istio-base-chart", "istio-cni-chart", "istio-ztunnel-chart", "traefik-chart", "metallb-chart", "kube-vip-chart", "kube-vip-image", "kube-vip-cloud-provider-chart"} {
		_ = cobra.MarkFlagRequired(f, flag)
	}
}

// grantFlags are those that let clusters' tokens hold more than their pools: Datastore.Allocate
// for the CSI plugin's newest release, Sys.Audit on / for the cloud controller manager's HA group
// labels.
func (o *controllerOptions) grantFlags(f *pflag.FlagSet) {
	f.StringVar(&o.settings.SysAuditGroup, "sys-audit-group", "", "Proxmox VE group holding Sys.Audit on / that the user of each cluster asking for the cloud controller manager's HA group labels joins; empty to let no cluster join it")
	f.StringVar(&o.csiNewestChart, "csi-newest-chart", "", "the CSI plugin's newest release, for clusters whose token holds Datastore.Allocate on their storage, as <repository>/<name>:<version>@sha256:<digest>")
	f.StringVar(&o.settings.DatastoreRole, "datastore-allocate-role", "", "Proxmox VE role, holding Datastore.Allocate, each cluster that asks for it gets on its storage; empty to let no cluster hold it")
	f.StringSliceVar(&o.r.DatastoreAllocate.Management, "management-storages", nil, "storages the management cluster uses, on which no cluster may hold Datastore.Allocate (comma-separated)")
}

func (o *controllerOptions) run(ctx context.Context, conn *pve.Config) error {
	if conn.CAFile == "" {
		return errors.New("--proxmox-ca is required: workload clusters are given the CA to verify the Proxmox VE API with")
	}
	if err := provisioner.ValidWorkloadPool(o.r.WorkloadPool); err != nil {
		return fmt.Errorf("--workload-pool: %w", err)
	}
	ca, err := os.ReadFile(conn.CAFile)
	if err != nil {
		return fmt.Errorf("--proxmox-ca: %w", err)
	}
	c, err := pve.Connect(*conn)
	if err != nil {
		return fmt.Errorf("connecting to Proxmox VE: %w", err)
	}
	if err = o.parseCharts(); err != nil {
		return err
	}
	rancherMinors, err := provisioner.ParseKubernetesMinors(o.rancherKubernetes)
	if err != nil {
		return fmt.Errorf("--rancher-kubernetes: %w", err)
	}
	providers, err := provisioner.ParseClassProviders(o.classProviders)
	if err != nil {
		return fmt.Errorf("--class-providers: %w", err)
	}
	distributions := &provisioner.Distributions{ClassNamespace: o.r.ClassNamespace, Providers: providers}
	cloud := &provisioner.Proxmox{Client: c, Settings: o.settings}
	o.completeReconciler(cloud, provisioner.Endpoint{URL: strings.TrimSuffix(conn.URL, "/"), CA: string(ca), Insecure: conn.Insecure}, rancherMinors)

	ctrl.SetLogger(zap.New())
	scheme, err := controllerScheme()
	if err != nil {
		return err
	}
	extensions, err := o.newExtensionServer()
	if err != nil {
		return err
	}
	mgr, err := o.newManager(scheme, extensions, distributions)
	if err != nil {
		return err
	}
	remotes := &provisioner.Remotes{Reader: mgr.GetClient()}
	registrar := &provisioner.Registrar{Client: mgr.GetClient(), ClassNamespace: o.r.ClassNamespace, Remotes: remotes}
	if err = o.setUpControllers(mgr, registrar, distributions); err != nil {
		return err
	}
	if err = o.addReclaimer(mgr, conn, cloud, remotes); err != nil {
		return err
	}
	hooks := &provisioner.Hooks{
		Reader:            mgr.GetAPIReader(),
		ClassNamespace:    o.r.ClassNamespace,
		Registrar:         registrar,
		RancherKubernetes: rancherMinors,
		Nodes:             cloud,
		DatastoreAllocate: o.r.DatastoreAllocate,
		SysAuditGroup:     o.r.SysAuditGroup,
		Releases: &provisioner.ReleaseChecksums{
			Reader:    mgr.GetAPIReader(),
			Client:    mgr.GetClient(),
			Namespace: o.r.ClassNamespace,
			HTTP:      &http.Client{Timeout: time.Minute},
			GitHub:    "https://github.com",
			Raw:       "https://raw.githubusercontent.com",
		},
		Client: mgr.GetClient(),
		Soak:   o.nodeImageSoak,
		Now:    time.Now,
	}
	if err = o.addPlacement(mgr, extensions, scheme, hooks, conn, cloud); err != nil {
		return err
	}
	if err = o.addUpgrader(mgr, c, rancherMinors); err != nil {
		return err
	}
	if err = o.addProbes(mgr); err != nil {
		return err
	}
	if err = mgr.Start(ctx); err != nil {
		return fmt.Errorf("the controller manager: %w", err)
	}
	return nil
}

// completeReconciler gives the reconciler what only the parsed flags and the connection make: the
// grants clusters may ask for are the Proxmox VE settings', so both read the same flags.
func (o *controllerOptions) completeReconciler(cloud *provisioner.Proxmox, endpoint provisioner.Endpoint, rancherMinors provisioner.KubernetesMinors) {
	o.r.Cloud = cloud
	o.r.DatastoreAllocate.Role = o.settings.DatastoreRole
	o.r.SysAuditGroup = o.settings.SysAuditGroup
	o.r.Endpoint = endpoint
	o.r.RancherKubernetes = rancherMinors
}

// newExtensionServer is the Runtime Extension server Cluster API calls the hooks through.
func (o *controllerOptions) newExtensionServer() (*server.Server, error) {
	catalog := runtimecatalog.New()
	if err := runtimehooksv1.AddToCatalog(catalog); err != nil {
		return nil, fmt.Errorf("the runtime hooks' catalog: %w", err)
	}
	extensions, err := server.New(server.Options{Port: o.port, CertDir: o.certDir, Catalog: catalog})
	if err != nil {
		return nil, fmt.Errorf("the Runtime Extension server: %w", err)
	}
	return extensions, nil
}

// addReclaimer adds what deletes the volumes of clusters' released persistent volumes, signed in
// with each cluster's own token.
func (o *controllerOptions) addReclaimer(mgr manager.Manager, conn *pve.Config, cloud *provisioner.Proxmox, remotes *provisioner.Remotes) error {
	reclaimer := &provisioner.Reclaimer{
		Client:         mgr.GetClient(),
		ClassNamespace: o.r.ClassNamespace,
		Connect: func(token string) (*proxmox.Client, error) {
			return pve.Connect(pve.Config{URL: conn.URL, Token: token, CAFile: conn.CAFile, Insecure: conn.Insecure})
		},
		Mover:   cloud,
		Remotes: remotes,
		// The CSI plugin retries a released volume's deletion until this has deleted it.
		Interval: time.Minute,
	}
	if err := mgr.Add(reclaimer); err != nil {
		return fmt.Errorf("the reclaimer: %w", err)
	}
	return nil
}

// parseCharts reads every chart flag, with the chart's values, into the reconciler's charts.
func (o *controllerOptions) parseCharts() error {
	var values map[string]map[string]any
	if err := json.Unmarshal([]byte(o.chartValues), &values); err != nil {
		return fmt.Errorf("--chart-values: %w", err)
	}
	for _, c := range []struct {
		flag  string
		value string
		chart *provisioner.Chart
	}{
		{"--ccm-chart", o.ccmChart, &o.r.Charts.CCM},
		{"--csi-chart", o.csiChart, &o.r.Charts.CSI},
		{"--csi-newest-chart", o.csiNewestChart, &o.r.Charts.CSINewest},
		{"--cilium-chart", o.ciliumChart, &o.r.Charts.Cilium},
		{"--calico-chart", o.calicoChart, &o.r.Charts.Calico},
		{"--headlamp-chart", o.headlampChart, &o.r.Charts.Headlamp},
		{"--cert-manager-chart", o.certManagerChart, &o.r.Charts.CertManager},
		{"--rancher-chart", o.rancherChart, &o.r.Charts.Rancher},
		{"--monitoring-chart", o.monitoringChart, &o.r.Charts.Monitoring},
		{"--loki-chart", o.lokiChart, &o.r.Charts.Loki},
		{"--alloy-chart", o.alloyChart, &o.r.Charts.Alloy},
		{"--velero-chart", o.veleroChart, &o.r.Charts.Velero},
		{"--kyverno-chart", o.kyvernoChart, &o.r.Charts.Kyverno},
		{"--kyverno-policies-chart", o.kyvernoPolicies, &o.r.Charts.KyvernoPolicies},
		{"--istio-chart", o.istioChart, &o.r.Charts.Istio},
		{"--istio-base-chart", o.istioBaseChart, &o.r.Charts.IstioBase},
		{"--istio-cni-chart", o.istioCNIChart, &o.r.Charts.IstioCNI},
		{"--istio-ztunnel-chart", o.istioZtunnelChart, &o.r.Charts.IstioZtunnel},
		{"--traefik-chart", o.traefikChart, &o.r.Charts.Traefik},
		{"--metallb-chart", o.metalLBChart, &o.r.Charts.MetalLB},
		{"--kube-vip-chart", o.kubeVipChart, &o.r.Charts.KubeVip},
		{"--kube-vip-cloud-provider-chart", o.kubeVipCloudChart, &o.r.Charts.KubeVipCloudProvider},
	} {
		var err error
		if *c.chart, err = provisioner.ParseChart(c.value); err != nil {
			return fmt.Errorf("%s: %w", c.flag, err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(c.flag, "--"), "-chart")
		c.chart.Values = values[name]
		delete(values, name)
	}
	if len(values) > 0 {
		return fmt.Errorf("--chart-values: no chart is called %s", strings.Join(slices.Sorted(maps.Keys(values)), ", "))
	}
	for _, i := range []struct {
		flag  string
		value string
		image *provisioner.Image
	}{
		{"--kube-rbac-proxy-image", o.kubeRBACProxy, &o.r.Charts.KubeRBACProxy},
		{"--alloy-image", o.alloyImage, &o.r.Charts.AlloyImage},
		{"--velero-image", o.veleroImage, &o.r.Charts.VeleroImage},
		{"--velero-plugin-image", o.veleroPlugin, &o.r.Charts.VeleroPlugin},
		{"--kube-vip-image", o.kubeVipImage, &o.r.Charts.KubeVipImage},
		{"--rancher-turtles-image", o.turtlesImage, &o.r.Charts.Turtles},
		{"--rancher-cluster-api-image", o.turtlesCAPIImage, &o.r.Charts.TurtlesClusterAPI},
	} {
		var err error
		if *i.image, err = provisioner.ParseImage(i.value); err != nil {
			return fmt.Errorf("%s: %w", i.flag, err)
		}
	}
	var err error
	if o.r.Charts.CalicoImages, err = provisioner.ParseCalicoImages(o.calicoImages); err != nil {
		return fmt.Errorf("--calico-images: %w", err)
	}
	return nil
}

func controllerScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, clusterv1.AddToScheme, addonsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("the controller's scheme: %w", err)
		}
	}
	return scheme, nil
}

func (o *controllerOptions) newManager(scheme *runtime.Scheme, extensions *server.Server, distributions *provisioner.Distributions) (manager.Manager, error) {
	lease, renew, retry := leaseDuration, renewDeadline, retryPeriod
	ofClass, err := labels.NewRequirement(provisioner.ClassLabel, selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("the ClusterClass label's selector: %w", err)
	}
	controlPlane, err := labels.NewRequirement(clusterv1.MachineControlPlaneLabel, selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("the control-plane label's selector: %w", err)
	}
	cached := map[client.Object]cache.ByObject{
		&clusterv1.ClusterClass{}: {Namespaces: map[string]cache.Config{o.r.ClassNamespace: {}}},
		// Watched for Proxmox VE HA, which takes part only for control-plane VMs.
		&clusterv1.Machine{}: {Label: labels.NewSelector().Add(*controlPlane)},
		&corev1.ConfigMap{}: {
			Namespaces: map[string]cache.Config{o.r.ClassNamespace: {}},
			Label:      labels.NewSelector().Add(*ofClass),
		},
	}
	if len(distributions.Providers) > 0 {
		deployments := map[string]cache.Config{}
		for _, ns := range distributions.Namespaces() {
			deployments[ns] = cache.Config{}
		}
		cached[&appsv1.Deployment{}] = cache.ByObject{Namespaces: deployments}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                o.leaderElect,
		LeaderElectionID:              "kubehenge-controller",
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 &lease,
		RenewDeadline:                 &renew,
		RetryPeriod:                   &retry,
		HealthProbeBindAddress:        o.healthAddr,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		WebhookServer:                 extensions,
		Cache: cache.Options{
			// Nothing here reads who last wrote which field.
			DefaultTransform: cache.TransformStripManagedFields(),
			ByObject:         cached,
		},
		// Secrets are read directly: caching them would mean watching every Secret in the
		// management cluster. Machines too: the cache holds only control-plane ones, and every
		// Machine is counted while a cluster is deleted.
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}, &clusterv1.Machine{}}}},
	})
	if err != nil {
		return nil, fmt.Errorf("the controller manager: %w", err)
	}
	return mgr, nil
}

// setUpControllers adds the reconciler, the registrar, what taints control-plane nodes, what
// approves kubeadm kubelets' serving certificates, what keeps the interface's identities in the
// clusters and, when some distribution's providers run only while used, the controller that
// starts and stops them.
func (o *controllerOptions) setUpControllers(mgr manager.Manager, registrar *provisioner.Registrar, distributions *provisioner.Distributions) error {
	o.r.Client = mgr.GetClient()
	o.r.Registrar = registrar
	o.r.Remotes = registrar.Remotes
	o.r.Events = mgr.GetEventRecorder("kubehenge")
	if err := o.r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("the cluster reconciler: %w", err)
	}
	if err := registrar.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("the Rancher registrar: %w", err)
	}
	tainter := &provisioner.TaintReconciler{Client: mgr.GetClient(), ClassNamespace: o.r.ClassNamespace, Remotes: registrar.Remotes}
	if err := tainter.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("the control-plane taints: %w", err)
	}
	approver := &provisioner.ServingCertApprover{Client: mgr.GetClient(), ClassNamespace: o.r.ClassNamespace, Remotes: registrar.Remotes}
	if err := approver.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("the kubelet serving certificates: %w", err)
	}
	identities := &provisioner.WorkloadIdentities{Client: mgr.GetClient(), ClassNamespace: o.r.ClassNamespace, Remotes: registrar.Remotes}
	if err := identities.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("the workload identities: %w", err)
	}
	bundle, err := provisioner.LoadGatewayAPI()
	if err != nil {
		return fmt.Errorf("the Gateway API: %w", err)
	}
	gatewayAPI := &provisioner.GatewayAPIReconciler{Client: mgr.GetClient(), ClassNamespace: o.r.ClassNamespace, Remotes: registrar.Remotes, Bundle: bundle}
	if err = gatewayAPI.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("the Gateway API: %w", err)
	}
	if len(distributions.Providers) == 0 {
		return nil
	}
	distributions.Client = mgr.GetClient()
	if err = distributions.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("the distributions' providers: %w", err)
	}
	return nil
}

// addPlacement serves the hooks, among them the patch naming the nodes capmox may use, and adds the
// Placer that keeps the online nodes those come from recorded on every cluster, and what keeps
// control-plane VMs apart with Proxmox VE HA.
func (o *controllerOptions) addPlacement(mgr manager.Manager, extensions *server.Server, scheme *runtime.Scheme, hooks *provisioner.Hooks, conn *pve.Config, cloud *provisioner.Proxmox) error {
	if err := addHooks(extensions, scheme, hooks); err != nil {
		return err
	}
	placer := &provisioner.Placer{Client: mgr.GetClient(), ClassNamespace: o.r.ClassNamespace, Nodes: cloud, Interval: placementInterval}
	if err := mgr.Add(placer); err != nil {
		return fmt.Errorf("the placer: %w", err)
	}
	rules := &provisioner.HARules{
		Reader:    mgr.GetAPIReader(),
		Client:    mgr.GetClient(),
		Namespace: o.r.ClassNamespace,
		Proxmox:   cloud.Client,
		Offer:     o.offerHARules,
		Interval:  haRulesInterval,
	}
	if err := mgr.Add(rules); err != nil {
		return fmt.Errorf("the HA rules offered: %w", err)
	}
	return o.addHA(mgr, conn, cloud.Client)
}

// addUpgrader adds what upgrades clusters automatically in their windows, and what records the
// Kubernetes versions kubeadm templates exist for, which both the upgrader and the interface go by.
func (o *controllerOptions) addUpgrader(mgr manager.Manager, c *proxmox.Client, rancherMinors provisioner.KubernetesMinors) error {
	versions := &provisioner.TemplateVersions{Client: mgr.GetClient(), ClassNamespace: o.r.ClassNamespace, Proxmox: c, Interval: templateVersionsInterval}
	if err := mgr.Add(versions); err != nil {
		return fmt.Errorf("the template versions: %w", err)
	}
	upgrader := &provisioner.Upgrader{
		Client:         mgr.GetClient(),
		ClassNamespace: o.r.ClassNamespace,
		Releases: &provisioner.ChannelServer{
			HTTP:    &http.Client{Timeout: time.Minute},
			URLs:    provisioner.DefaultChannelURLs,
			Proxmox: c,
		},
		RancherKubernetes: rancherMinors,
		Interval:          o.upgradeInterval,
		Soak:              o.nodeImageSoak,
		Now:               time.Now,
	}
	if err := mgr.Add(upgrader); err != nil {
		return fmt.Errorf("the upgrader: %w", err)
	}
	if !o.templateRefresh {
		return nil
	}
	requests := &provisioner.TemplateRequests{
		Client:    mgr.GetClient(),
		Reader:    mgr.GetAPIReader(),
		Namespace: o.r.ClassNamespace,
		Interval:  templateRequestsInterval,
		Now:       time.Now,
	}
	if err := mgr.Add(requests); err != nil {
		return fmt.Errorf("the template requests: %w", err)
	}
	return nil
}

// addHA adds the reconciler that has Proxmox VE HA keep clusters' control-plane VMs apart, signed
// in with KUBEHENGE_PROXMOX_HA_TOKEN. Without that token it takes back the hooks it set before.
func (o *controllerOptions) addHA(mgr manager.Manager, conn *pve.Config, c *proxmox.Client) error {
	r := &provisioner.HAReconciler{
		Client:         mgr.GetClient(),
		Machines:       mgr.GetCache(),
		ClassNamespace: o.r.ClassNamespace,
		WorkloadPool:   o.r.WorkloadPool,
		Proxmox:        c,
	}
	if token := os.Getenv("KUBEHENGE_PROXMOX_HA_TOKEN"); token != "" {
		ha, err := pve.Connect(pve.Config{URL: conn.URL, Token: token, CAFile: conn.CAFile, Insecure: conn.Insecure})
		if err != nil {
			return fmt.Errorf("connecting to Proxmox VE for HA: %w", err)
		}
		r.HA = ha
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("the Proxmox VE HA reconciler: %w", err)
	}
	return nil
}

// addHooks serves the Runtime Extension's hooks and the Cluster validation.
func addHooks(extensions *server.Server, scheme *runtime.Scheme, hooks *provisioner.Hooks) error {
	handlers := []server.ExtensionHandler{
		{Hook: runtimehooksv1.BeforeClusterCreate, Name: "before-cluster-create", HandlerFunc: hooks.BeforeClusterCreate},
		{Hook: runtimehooksv1.BeforeClusterUpgrade, Name: "before-cluster-upgrade", HandlerFunc: hooks.BeforeClusterUpgrade},
		{Hook: runtimehooksv1.BeforeClusterDelete, Name: "before-cluster-delete", HandlerFunc: hooks.BeforeClusterDelete},
		{Hook: runtimehooksv1.GeneratePatches, Name: "generate-patches", HandlerFunc: hooks.GeneratePatches},
	}
	for i := range handlers {
		if err := extensions.AddExtensionHandler(handlers[i]); err != nil {
			return fmt.Errorf("hook %s: %w", handlers[i].Name, err)
		}
	}
	extensions.Register("/validate-cluster", admission.WithValidator(scheme, &provisioner.ClusterValidator{Hooks: hooks}))
	return nil
}

// addProbes adds the liveness and readiness checks, and the announcer that tells Cluster API the
// hooks are served.
func (o *controllerOptions) addProbes(mgr manager.Manager) error {
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("the liveness check: %w", err)
	}
	// The manager runs its webhook server, here the Runtime Extension server, only once something
	// has asked for it.
	serving := mgr.GetWebhookServer().StartedChecker()
	if err := mgr.AddReadyzCheck("extensions", serving); err != nil {
		return fmt.Errorf("the readiness check: %w", err)
	}
	announcer := &provisioner.Announcer{
		Client:          mgr.GetClient(),
		ExtensionConfig: o.extensionConfig,
		Serving:         func() error { return serving(nil) },
		Version:         os.Getenv("POD_NAME"),
	}
	if err := mgr.Add(announcer); err != nil {
		return fmt.Errorf("the announcer: %w", err)
	}
	return nil
}
