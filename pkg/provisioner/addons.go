package provisioner

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

const (
	// workloadSecret is the Secret the cloud controller manager and the CSI plugin read their
	// configuration and the Proxmox VE CA from, in each workload cluster.
	workloadSecret = "kubehenge-proxmox" //nolint:gosec // a Secret's name, not a credential
	// proxmoxCAKey holds the Proxmox VE CA in workloadSecret.
	proxmoxCAKey = "ca.crt"
	secretKind   = "Secret"

	// The add-ons' names, which name their cluster variables, proxies and releases too.
	headlampAddon    = "headlamp"
	rancherAddon     = "rancher"
	certManagerAddon = "cert-manager"
	// rancherNamespace is the namespace Rancher's chart has to be installed in.
	rancherNamespace = "cattle-system"
	// cisVariable hardens the cluster to the CIS benchmark beyond what every cluster gets.
	cisVariable = "cis"

	// apiVersionKey is a manifest's own, in those written as maps.
	apiVersionKey = "apiVersion"
	// Keys of the charts' values and of pod specs written as maps.
	seccompProfileKey = "seccompProfile"
	ingressKey        = "ingress"
	valueKey          = "value"
	createKey         = "create"
	v1alpha1          = "v1alpha1"
	requestsKey       = "requests"
	tolerationsKey    = "tolerations"
	memoryKey         = "memory"
	cpuKey            = "cpu"
	controllerKey     = "controller"
	annotationsKey    = "annotations"
	securityKey       = "securityContext"
	nonRootKey        = "runAsNonRoot"
	readOnlyRootKey   = "readOnlyRootFilesystem"
	configKey         = "config"
	regionKey         = "region"
	defaultKey        = "default"
	installCRDsKey    = "installCRDs"
	// The least CPU and memory the small add-on pods ask for.
	cpu10m     = "10m"
	memory64Mi = "64Mi"
	// csiConfigKey is the CSI plugin's configuration in the Secrets it reads it from.
	csiConfigKey = "csi.yaml"

	// The CNIs Kubehenge installs, as the cni variable names them.
	cniCilium = "cilium"
	cniCalico = "calico"
)

// restrictedContainer is a container's security context at Pod Security's restricted level, with
// the fields given besides.
func restrictedContainer(with map[string]any) map[string]any {
	context := map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}
	maps.Copy(context, with)
	return context
}

// runtimeDefault is the seccomp profile Pod Security's restricted level asks for.
func runtimeDefault() map[string]any {
	return map[string]any{"type": string(corev1.SeccompProfileTypeRuntimeDefault)}
}

// Charts locates the Helm charts installed into clusters: the cloud controller manager and CSI
// plugin into every one, a CNI into those whose distribution has none of its own, and the add-ons
// a cluster switches on.
type Charts struct {
	CCM Chart
	// CSI is the plugin's last release that needs no Datastore.Allocate, CSINewest its newest, for
	// clusters whose user holds Datastore.Allocate on their storage.
	CSI       Chart
	CSINewest Chart
	// SnapshotController installs the volume snapshot CRDs and controller, and the plugin's
	// VolumeSnapshotClass, for clusters that take volume snapshots.
	SnapshotController Chart
	Cilium             Chart
	Calico             Chart
	// CalicoImages are the images the Tigera operator runs Calico from, all at one release.
	CalicoImages []Image
	Headlamp     Chart
	CertManager  Chart
	Rancher      Chart
	// Turtles and TurtlesClusterAPI are the images of the Turtles release Rancher installs
	// itself, and of the Cluster API controller it runs (rancherConfig).
	Turtles           Image
	TurtlesClusterAPI Image
	Monitoring        Chart
	// KubeRBACProxy is the image of the proxies in front of metrics served without authentication,
	// and of the one in front of Loki.
	KubeRBACProxy Image
	// Loki keeps the logs that Alloy collects; Alloy runs AlloyImage, which its chart takes apart.
	Loki       Chart
	Alloy      Chart
	AlloyImage Image
	// Velero is the backups' chart; its server, node agents and volume restores run VeleroImage, with
	// VeleroPlugin's S3 plugin.
	Velero          Chart
	VeleroImage     Image
	VeleroPlugin    Image
	Kyverno         Chart
	KyvernoPolicies Chart
	// Istio is istiod's chart, installed with its base, cni and ztunnel charts at the same version.
	Istio        Chart
	IstioBase    Chart
	IstioCNI     Chart
	IstioZtunnel Chart
	// Traefik is the ingress controller of distributions that bring none.
	Traefik Chart
	// The load balancers a cluster chooses from: MetalLB, or kube-vip with its cloud provider.
	// kube-vip runs the image its control planes do.
	MetalLB              Chart
	KubeVip              Chart
	KubeVipImage         Image
	KubeVipCloudProvider Chart
	// The GitOps engines a cluster chooses from: Argo CD, or the Flux Operator, which installs
	// FluxVersion, the Flux release inside its image.
	ArgoCD       Chart
	FluxOperator Chart
	FluxVersion  string
	// PodSecurityExempt are the namespaces Kyverno's policies leave alone, those the ClusterClasses'
	// Pod Security Admission exempts.
	PodSecurityExempt []string
}

// Addons are the add-ons a cluster switches on through its variables.
type Addons struct {
	Headlamp     Headlamp
	Rancher      Rancher
	Monitoring   Monitoring
	Logs         Logs
	Backups      Backups
	PodSecurity  PodSecurity
	Istio        Istio
	ClusterMesh  ClusterMesh
	LoadBalancer LoadBalancer
	Certificates Certificates
	GitOps       GitOps
	// CIS is the cluster's cis variable, which the add-ons make room for.
	CIS bool
	// CSINewest runs the CSI plugin's newest release, once the cluster's user holds
	// Datastore.Allocate on its storage, as its csi variable asks.
	CSINewest bool
	// VolumeSnapshots has the CSI plugin take volume snapshots signed in as root@pam, once nothing
	// stands in the way of what the cluster's csi variable asks.
	VolumeSnapshots bool

	// backupTarget is the target the backups name, found in the cluster's class; nil while it is
	// not, when Velero is neither installed nor removed.
	backupTarget *BackupTarget
	// gitOpsFound is what the controller found for GitOps before writing the charts' values.
	gitOpsFound gitOpsState
}

// addonsOf reads a cluster's add-on variables.
func addonsOf(cluster *clusterv1.Cluster) (Addons, error) {
	var a Addons
	var err error
	if a.Headlamp, _, err = optionalVariable[Headlamp](cluster, headlampAddon); err != nil {
		return a, err
	}
	if a.Monitoring, _, err = optionalVariable[Monitoring](cluster, monitoringAddon); err != nil {
		return a, err
	}
	if a.Logs, _, err = optionalVariable[Logs](cluster, logsAddon); err != nil {
		return a, err
	}
	if a.Backups, _, err = optionalVariable[Backups](cluster, backupsVariable); err != nil {
		return a, err
	}
	a.Backups.withDefaults()
	if a.PodSecurity, _, err = optionalVariable[PodSecurity](cluster, podSecurityVariable); err != nil {
		return a, err
	}
	if a.CIS, _, err = optionalVariable[bool](cluster, cisVariable); err != nil {
		return a, err
	}
	if a.Istio, _, err = optionalVariable[Istio](cluster, istioAddon); err != nil {
		return a, err
	}
	if a.ClusterMesh, _, err = optionalVariable[ClusterMesh](cluster, clusterMeshVariable); err != nil {
		return a, err
	}
	if a.LoadBalancer, _, err = optionalVariable[LoadBalancer](cluster, loadBalancerVariable); err != nil {
		return a, err
	}
	if a.Certificates, err = CertificatesOf(cluster); err != nil {
		return a, err
	}
	if a.GitOps, _, err = optionalVariable[GitOps](cluster, gitopsVariable); err != nil {
		return a, err
	}
	a.Rancher, err = rancherOf(cluster)
	return a, err
}

// certManager reports whether the cluster runs cert-manager, which Rancher, monitoring and the
// certificates add-on each need.
func (a *Addons) certManager() bool {
	return a.Rancher.Install || a.Monitoring.Enabled || a.Certificates.on()
}

// certManagerProxy names the cluster's cert-manager proxy, installed for whichever add-ons need it.
func certManagerProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-" + certManagerAddon
}

// rancherOf reads a cluster's rancher variable, the zero value if it has none.
func rancherOf(cluster *clusterv1.Cluster) (Rancher, error) {
	r, _, err := optionalVariable[Rancher](cluster, rancherAddon)
	return r, err
}

// rancherProxy names the proxy of Rancher's own cluster, which goes when it is switched off.
func rancherProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-" + rancherAddon
}

// rancherValues: Rancher makes its own CA through cert-manager and the agents trust only that CA,
// pinned by its checksum in the manifest each cluster gets. Its server URL is fixed to hostname,
// without which it makes no registration manifest. Up to three replicas, as many as there are
// nodes; the admin's first password is generated into the bootstrap-secret Secret, never put into
// values. Rancher's chart gives a start two minutes, which its first start on one small node can
// overrun while it registers its CRDs; every kill begins that again, so the window is ten minutes,
// and liveness keeps its own. The server URL joins the chart's own extraEnv, which a list in these
// values would replace.
func rancherValues(r Rancher, base map[string]any) map[string]any {
	const (
		// A negative count is the chart's "up to": as many replicas as nodes, at most three.
		replicas      = -3
		probeTimeout  = 5
		probePeriod   = 10
		probeFailures = 60
	)
	var env []any
	if e, ok := base["extraEnv"].([]any); ok {
		env = slices.Clone(e)
	}
	return map[string]any{
		"hostname":     r.Hostname,
		"replicas":     replicas,
		"agentTLSMode": "strict",
		ingressKey:     map[string]any{"tls": map[string]any{"source": "rancher"}},
		"extraEnv":     append(env, map[string]any{"name": "CATTLE_SERVER_URL", valueKey: "https://" + r.Hostname}),
		"startupProbe": map[string]any{"timeoutSeconds": probeTimeout, "periodSeconds": probePeriod, "failureThreshold": probeFailures},
	}
}

// Headlamp is a cluster's headlamp variable.
type Headlamp struct {
	Enabled   bool   `json:"enabled"`
	Hostname  string `json:"hostname,omitempty"`
	TLSSecret string `json:"tlsSecret,omitempty"`
	// Access is token, the default, for everyone's own token or OIDC sign-in, or headlampViewAccess.
	Access string `json:"access,omitempty"`
}

// headlampViewAccess has Headlamp serve everyone who reaches it through its own account, bound to
// Kubernetes' view role.
const headlampViewAccess = "view"

// Chart is one Helm chart, pinned by its digest: an OCI chart's manifest digest, or the SHA-256 of
// the archive a repository's index lists.
type Chart struct {
	Repository string
	Name       string
	Version    string
	Digest     string
	// Values lie beneath those Kubehenge gives the chart: the images it runs, each by tag and digest.
	Values map[string]any
}

// ParseChart reads <repository>/<name>:<version>@sha256:<digest>, such as
// oci://ghcr.io/sergelogvinov/charts/proxmox-csi-plugin:0.5.12@sha256:bbd9… or
// https://helm.cilium.io/cilium:1.20.3@sha256:0805….
func ParseChart(s string) (Chart, error) {
	name, digest, ok := strings.Cut(s, "@sha256:")
	at := strings.LastIndex(name, ":")
	slash := strings.LastIndex(name, "/")
	if !ok || !isDigest(digest) || slash < 0 || at <= slash+1 || at == len(name)-1 {
		return Chart{}, fmt.Errorf("%q is not <repository>/<name>:<version>@sha256:<digest>", s)
	}
	return Chart{Repository: name[:slash], Name: name[slash+1 : at], Version: name[at+1:], Digest: digest}, nil
}

// pinnedVersion is the version a HelmChartProxy takes, which Kubehenge's build of the Add-on
// Provider for Helm pulls the chart by or checks the chart against.
func (c Chart) pinnedVersion() string {
	return c.Version + "@sha256:" + c.Digest
}

// isDigest reports whether s is a SHA-256 digest in lower-case hex.
func isDigest(s string) bool {
	const sha256Hex = 64
	if len(s) != sha256Hex {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// CNIAnnotation on a ClusterClass lists, comma-separated, the values of its cni variable that name
// a CNI Kubehenge installs rather than the distribution.
const CNIAnnotation = "kubehenge.dev/cni"

// CNIForwardingAnnotation "true" on a ClusterClass lets pods forward packets, for distributions
// whose own components route inside pods, such as k3s's servicelb.
const CNIForwardingAnnotation = "kubehenge.dev/cni-container-forwarding"

// CNI is the pod network Kubehenge installs into a cluster; the zero value installs none.
type CNI struct {
	// Name is cilium or calico.
	Name string
	// Pods is the pod range for a Cluster that names none, its distribution's own default.
	Pods string
	// ContainerForwarding is CNIForwardingAnnotation.
	ContainerForwarding bool
	// ClusterID is Cilium's cluster id, CiliumClusterIDAnnotation.
	ClusterID int
}

// Endpoint is how workload clusters reach the Proxmox VE API.
type Endpoint struct {
	// URL is the API's address, such as https://pve.example.org:8006.
	URL string
	// CA is the PEM certificate that signed the API's certificate.
	CA string
	// Insecure has the cloud controller manager and the CSI plugin skip verifying the API's
	// certificate, as the controller does.
	Insecure bool
}

type proxmoxConfig struct {
	Features proxmoxFeatures  `json:"features"`
	Clusters []proxmoxCluster `json:"clusters"`
}

type proxmoxFeatures struct {
	Provider       string `json:"provider"`
	ControllerVMID int    `json:"controllerVmID,omitempty"`
}

// proxmoxCluster signs in with a token, or with a user's password; the plugins take exactly one.
type proxmoxCluster struct {
	URL         string `json:"url"`
	Insecure    bool   `json:"insecure"`
	TokenID     string `json:"token_id,omitempty"`
	TokenSecret string `json:"token_secret,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	Region      string `json:"region"`
}

// resourceSetSecret wraps the workload cluster's Secrets for a ClusterResourceSet, which applies
// every key of its Secret as a manifest: workloadSecret, and with rootPassword rootWorkloadSecret,
// whose CSI plugin configuration signs in as root@pam.
func resourceSetSecret(cluster *clusterv1.Cluster, endpoint Endpoint, c Credentials, rootPassword string) (*corev1.Secret, error) {
	pve := proxmoxCluster{URL: endpoint.URL + "/api2/json", Insecure: endpoint.Insecure, TokenID: c.TokenID, TokenSecret: c.TokenSecret, Region: c.Region}
	// capmox names machines proxmox://<SMBIOS UUID>, which only the capmox provider mode matches.
	ccm, err := yaml.Marshal(proxmoxConfig{Features: proxmoxFeatures{Provider: "capmox"}, Clusters: []proxmoxCluster{pve}})
	if err != nil {
		return nil, fmt.Errorf("encoding the cloud controller manager's configuration: %w", err)
	}
	csi, err := csiConfig(c, &pve)
	if err != nil {
		return nil, err
	}
	manifest, err := workloadManifest(workloadSecret, map[string]string{"ccm.yaml": string(ccm), csiConfigKey: csi, proxmoxCAKey: endpoint.CA})
	if err != nil {
		return nil, err
	}
	data := map[string][]byte{workloadSecret + ".yaml": manifest}
	if rootPassword != "" {
		root, err := csiConfig(c, &proxmoxCluster{URL: pve.URL, Insecure: pve.Insecure, Username: rootUser, Password: rootPassword, Region: c.Region})
		if err != nil {
			return nil, err
		}
		if data[rootWorkloadSecret+".yaml"], err = workloadManifest(rootWorkloadSecret, map[string]string{csiConfigKey: root}); err != nil {
			return nil, err
		}
	}
	return &corev1.Secret{
		Name: cluster.Name + "-proxmox", Namespace: cluster.Namespace,
		Type: addonsv1.ClusterResourceSetSecretType,
		Data: data,
	}, nil
}

// csiConfig is the CSI plugin's configuration, signing in as pve says.
func csiConfig(c Credentials, pve *proxmoxCluster) (string, error) {
	csi, err := yaml.Marshal(proxmoxConfig{
		Features: proxmoxFeatures{Provider: "capmox", ControllerVMID: c.VolumeOwner},
		Clusters: []proxmoxCluster{*pve},
	})
	if err != nil {
		return "", fmt.Errorf("encoding the CSI plugin's configuration: %w", err)
	}
	return string(csi), nil
}

// workloadManifest is a kube-system Secret of the workload cluster, as a manifest.
func workloadManifest(name string, data map[string]string) ([]byte, error) {
	manifest, err := yaml.Marshal(&corev1.Secret{
		APIVersion: "v1",
		Kind:       secretKind,
		Name:       name,
		Namespace:  metav1.NamespaceSystem,
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding the workload cluster's Secret %s: %w", name, err)
	}
	return manifest, nil
}

// podRange is the CNI's pod range: the Cluster's own, or else the one the distribution's own
// components assume, kube-proxy's cluster CIDR among them.
func podRange(cluster *clusterv1.Cluster, cni CNI) (string, error) {
	if blocks := cluster.Spec.ClusterNetwork.Pods.CIDRBlocks; len(blocks) > 0 {
		return blocks[0], nil
	}
	if cni.Pods == "" {
		return "", fmt.Errorf("cluster %s/%s: no pod range for the CNI", cluster.Namespace, cluster.Name)
	}
	return cni.Pods, nil
}

func calicoResourcesName(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-calico"
}

// ParseCalicoImages reads the images the Tigera operator runs Calico from, which have to be of one
// release, the one its ImageSet is named for.
func ParseCalicoImages(refs []string) ([]Image, error) {
	images := make([]Image, 0, len(refs))
	for _, ref := range refs {
		image, err := ParseImage(ref)
		if err != nil {
			return nil, err
		}
		if len(images) > 0 && image.Tag != images[0].Tag {
			return nil, fmt.Errorf("%s is not at %s, the release of %s", ref, images[0].Tag, images[0])
		}
		images = append(images, image)
	}
	if len(images) == 0 {
		return nil, errors.New("no images")
	}
	return images, nil
}

// calicoImageSet pins the images the operator runs Calico from to their digests. The operator
// takes the ImageSet named for its own release; it names the images by repository, which is the
// operator's image path and the image's name.
func calicoImageSet(images []Image) map[string]any {
	pinned := make([]any, 0, len(images))
	for _, i := range images {
		pinned = append(pinned, map[string]any{"image": i.Repository, "digest": "sha256:" + i.Digest})
	}
	return map[string]any{
		"kind":     "ImageSet",
		"metadata": map[string]any{"name": "calico-" + images[0].Tag},
		"spec":     map[string]any{"images": pinned},
	}
}

// calicoResources wraps, for the ClusterResourceSet, the resources the Tigera operator installs
// Calico from, as its chart's defaults have them, and the ImageSet that pins its images, which
// comes first so that the operator never runs one by tag. The chart would render them before the
// operator has created their CRDs, which a Helm release cannot wait for; a ClusterResourceSet
// retries until they exist. Nor can Helm install the CRDs first: Calico's CRD chart is larger than
// the 1 MiB Secret a Helm release is kept in.
func calicoResources(cluster *clusterv1.Cluster, cni CNI, images []Image) (*corev1.Secret, error) {
	pods, err := podRange(cluster, cni)
	if err != nil {
		return nil, err
	}
	network := map[string]any{"ipPools": []any{map[string]any{"cidr": pods, "encapsulation": "VXLANCrossSubnet"}}}
	if cni.ContainerForwarding {
		network["containerIPForwarding"] = "Enabled"
	}
	resources := []map[string]any{
		calicoImageSet(images),
		{"kind": "Installation", "spec": map[string]any{
			"cni":           map[string]any{"type": "Calico"},
			"calicoNetwork": network,
			// Calico's CSI driver, for its own volume types, which Kubehenge does not use.
			"kubeletVolumePluginPath": "None",
		}},
		{"kind": "APIServer", "spec": map[string]any{}},
		{"kind": "Goldmane", "spec": map[string]any{}},
		{"kind": "Whisker", "spec": map[string]any{}},
	}
	var manifest []byte
	for _, r := range resources {
		r[apiVersionKey] = "operator.tigera.io/v1"
		if _, named := r["metadata"]; !named {
			r["metadata"] = map[string]any{"name": "default"}
		}
		doc, err := yaml.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("encoding Calico's %s: %w", r["kind"], err)
		}
		manifest = append(append(manifest, "---\n"...), doc...)
	}
	return &corev1.Secret{
		Name: calicoResourcesName(cluster), Namespace: cluster.Namespace,
		Type: addonsv1.ClusterResourceSetSecretType,
		Data: map[string][]byte{"calico.yaml": manifest},
	}, nil
}

// The leader election of every controller Kubehenge installs into a cluster that exits once it
// misses its lease's renewal: the Cilium operator, the cloud controller managers and the CSI
// plugin's sidecars. Their defaults give up within 10 s, which a node short of CPU, or an API
// server waiting on etcd, makes them miss; these are the control planes' controller managers' and
// the management cluster's controllers' leases.
const (
	leaseDuration = "60s"
	leaseRenew    = "40s"
	leaseRetry    = "10s"
)

// leaseArgs are those leases as flags of the given prefix: leader-elect for Kubernetes' own
// components and those built on them, leader-election for Cilium and the CSI sidecars.
func leaseArgs(prefix string) []any {
	return []any{
		"--" + prefix + "-lease-duration=" + leaseDuration,
		"--" + prefix + "-renew-deadline=" + leaseRenew,
		"--" + prefix + "-retry-period=" + leaseRetry,
	}
}

// ciliumValues are what Kubehenge sets in Cilium's chart, its own or the distribution's. The
// cluster's name and id are Cilium's from the start: a cluster that joins a mesh later would
// otherwise have to restart every workload. operator holds the operator's own values.
func ciliumValues(cluster *clusterv1.Cluster, id int, addons *Addons, operator map[string]any) (map[string]any, error) {
	if len(cluster.Name) > MaxCiliumClusterName {
		return nil, fmt.Errorf("cluster %s/%s: Cilium takes cluster names of at most %d characters", cluster.Namespace, cluster.Name, MaxCiliumClusterName)
	}
	// The agents and the operator read their configuration only when they start: an agent that
	// started with cni.exclusive goes on taking istio-cni's plugin out after Istio is switched on.
	operator["rollOutPods"] = true
	// As arguments rather than in Cilium's ConfigMap, a change to them restarts the operator only.
	operator["extraArgs"] = leaseArgs("leader-election")
	// Without kube-proxy replacement, Cilium serves host ports, Traefik's on every node, only
	// through the portmap plugin chained behind it, as RKE2 runs its own Cilium.
	cni := map[string]any{"chainingMode": "portmap"}
	values := map[string]any{
		"cluster":           map[string]any{"name": cluster.Name, "id": id},
		"rollOutCiliumPods": true,
		"operator":          operator,
		"cni":               cni,
	}
	if addons.ClusterMesh.Enabled {
		values["clustermesh"] = clusterMeshValues()
	}
	if addons.Istio.Enabled {
		// Cilium would otherwise take istio-cni's plugin out of the node's CNI configuration.
		cni["exclusive"] = false
	}
	return values, nil
}

func rke2CiliumName(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-rke2-cilium"
}

// rke2CiliumConfig wraps, for the ClusterResourceSet, the HelmChartConfig through which RKE2's
// helm-controller merges Kubehenge's values into RKE2's own Cilium chart. A change reaches the
// running cluster, where a file on the nodes would reach only those created after it.
func rke2CiliumConfig(cluster *clusterv1.Cluster, id int, addons *Addons) (*corev1.Secret, error) {
	values, err := ciliumValues(cluster, id, addons, map[string]any{})
	if err != nil {
		return nil, err
	}
	content, err := yaml.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encoding Cilium's values: %w", err)
	}
	manifest, err := yaml.Marshal(map[string]any{
		apiVersionKey: "helm.cattle.io/v1",
		"kind":        "HelmChartConfig",
		"metadata":    map[string]any{"name": "rke2-cilium", "namespace": metav1.NamespaceSystem},
		"spec":        map[string]any{"valuesContent": string(content)},
	})
	if err != nil {
		return nil, fmt.Errorf("encoding RKE2's Cilium configuration: %w", err)
	}
	return &corev1.Secret{
		Name: rke2CiliumName(cluster), Namespace: cluster.Namespace,
		Type: addonsv1.ClusterResourceSetSecretType,
		Data: map[string][]byte{"rke2-cilium.yaml": manifest},
	}, nil
}

// clusterResourceSet applies the cluster's Proxmox VE Secret and the other Secrets given, such as
// Calico's resources when Kubehenge installs Calico; each is retried on its own until it applies.
func clusterResourceSet(cluster *clusterv1.Cluster, secrets []*corev1.Secret) *addonsv1.ClusterResourceSet {
	resources := make([]addonsv1.ResourceRef, 0, len(secrets))
	for _, s := range secrets {
		resources = append(resources, addonsv1.ResourceRef{Kind: secretKind, Name: s.Name})
	}
	return &addonsv1.ClusterResourceSet{
		Name: cluster.Name + "-proxmox", Namespace: cluster.Namespace,
		Spec: addonsv1.ClusterResourceSetSpec{
			ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{clusterv1.ClusterNameLabel: cluster.Name}},
			Resources:       resources,
			// Reapplied when a Secret changes, such as when a lost token is replaced.
			Strategy: string(addonsv1.ClusterResourceSetStrategyReconcile),
		},
	}
}

// headlampValues keep Headlamp without rights of its own unless the cluster asks for view access:
// every user signs in with their own token or through OIDC. Its service account token stays
// mounted because Headlamp reads the API server's address and CA through it; the account is bound
// to no role. With view access, the account is bound to the view ClusterRole and Headlamp sends
// every request with its token, the user's own dropped, so whoever reaches Headlamp reads the
// cluster without signing in and nobody changes anything through it. Plugins are those in the
// image, nothing fetched at run time.
func headlampValues(h Headlamp, certs *Certificates) map[string]any {
	// The image's own user and group.
	const user, group = 100, 101
	binding := map[string]any{createKey: false}
	config := map[string]any{"oidc": map[string]any{"secret": map[string]any{createKey: false}}}
	if h.Access == headlampViewAccess {
		binding = map[string]any{createKey: true, "clusterRoleName": "view"}
		config["unsafeUseServiceAccountToken"] = true
	}
	values := map[string]any{
		"clusterRoleBinding": binding,
		configKey:            config,
		"pluginsManager":     map[string]any{"enabled": false},
		// The chart's own default leaves out what Pod Security's restricted level requires.
		securityKey: restrictedContainer(map[string]any{
			nonRootKey:        true,
			"runAsUser":       user,
			"runAsGroup":      group,
			"privileged":      false,
			readOnlyRootKey:   true,
			seccompProfileKey: runtimeDefault(),
		}),
		"resources": map[string]any{
			requestsKey: map[string]any{cpuKey: cpu10m, memoryKey: memory64Mi},
			"limits":    map[string]any{memoryKey: "256Mi"},
		},
	}
	if h.Hostname != "" {
		tls, annotations := addonIngress(certs, headlampAddon, h.Hostname, h.TLSSecret)
		ingress := map[string]any{
			"enabled": true,
			"hosts":   []any{map[string]any{"host": h.Hostname, "paths": []any{map[string]any{"path": "/", "type": "Prefix"}}}},
			"tls":     []any{tls},
		}
		if annotations != nil {
			ingress["annotations"] = annotations
		}
		values[ingressKey] = ingress
	}
	return values
}

// traefikProxy names the proxy of the ingress controller Kubehenge installs where the distribution
// brings none.
func traefikProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-traefik"
}

// traefikValues run Traefik as RKE2 runs its own: on every node, on the node's ports 80 and 443,
// so that a host name pointing at any node works without a load balancer. It runs on the control
// planes too, so that the cluster's API address serves it as well. Its namespace is kube-system,
// which Pod Security exempts, since host ports are beyond the baseline level.
//
// Its Gateway API provider is on, as in the Traefik RKE2 and k3s bring: the chart creates the
// GatewayClass traefik and the Gateway traefik-gateway beside Traefik, whose HTTP listener takes
// routes from every namespace, as Traefik takes Ingresses from every namespace. The Gateway reports
// the cluster's API address, where Traefik answers too. With a default certificate, the Gateway
// listens for HTTPS as well, with the values the RKE2 and k3s classes give their Traefik
// (kubehenge.traefikGatewayValues).
func traefikValues(endpoint string, certs *Certificates) map[string]any {
	const http, https = 80, 443
	gatewayProvider := map[string]any{"enabled": true}
	if endpoint != "" {
		gatewayProvider["statusAddress"] = map[string]any{"ip": endpoint}
	}
	return overlay(map[string]any{
		"deployment": map[string]any{"kind": "DaemonSet"},
		// A host port is held by the pod on it, so a node's new pod starts only once the old one is gone.
		"updateStrategy": map[string]any{"rollingUpdate": map[string]any{"maxUnavailable": 1, "maxSurge": 0}},
		"ports": map[string]any{
			"web":       map[string]any{"hostPort": http},
			"websecure": map[string]any{"hostPort": https},
		},
		"service": map[string]any{"spec": map[string]any{"type": "ClusterIP"}},
		"providers": map[string]any{
			// The Service's address is one only pods reach, so Ingresses are not told it.
			"kubernetesIngress": map[string]any{"publishedService": map[string]any{"enabled": false}},
			"kubernetesGateway": gatewayProvider,
		},
		"gateway": map[string]any{"listeners": map[string]any{webListener: map[string]any{"namespacePolicy": map[string]any{"from": "All"}}}},
		// Routes whose TLS options conflict fail rather than fall back to the defaults, which would
		// drop a host's client-certificate check (GHSA-g55h-rg46-x9c5).
		"core":         map[string]any{"strictTLSOptions": true},
		tolerationsKey: []any{toleration(controlPlaneTaint.Key, controlPlaneTaint.Effect)},
		"resources":    map[string]any{requestsKey: map[string]any{cpuKey: cpu10m, memoryKey: memory64Mi}},
	}, traefikCertificateValues(certs))
}

var helmChartProxyGVK = schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: v1alpha1, Kind: "HelmChartProxy"}

// headlampProxy names the cluster's Headlamp proxy, which goes again when Headlamp is switched off.
func headlampProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-headlamp"
}

// toleration tolerates a taint of any value.
func toleration(key string, effect corev1.TaintEffect) map[string]any {
	return map[string]any{"key": key, "operator": string(corev1.TolerationOpExists), "effect": string(effect)}
}

// proxmoxValues are the values of the two charts that talk to Proxmox VE: the cloud controller
// manager's and the CSI plugin's. With volume snapshots, the plugin signs in as root@pam, runs its
// snapshot sidecar, and its sidecars wait past the plugin's own wait for a volume copy.
func proxmoxValues(storage string, addons *Addons) (map[string]any, map[string]any) {
	// The charts have no CA setting; this replaces the image's trust store with the Proxmox VE CA,
	// the only server either talks to besides the in-cluster API, which uses its own CA.
	ca := map[string]any{
		"extraVolumes": []any{map[string]any{
			"name":   "proxmox-ca",
			"secret": map[string]any{"secretName": workloadSecret, "items": []any{map[string]any{"key": proxmoxCAKey, "path": proxmoxCAKey}}},
		}},
		"extraVolumeMounts": []any{map[string]any{
			"name": "proxmox-ca", "mountPath": "/etc/ssl/certs/ca-certificates.crt", "subPath": proxmoxCAKey, "readOnly": true,
		}},
	}
	// The CCM and the CSI plugin's controller are what mount the cluster's Proxmox VE token (the
	// CSI node plugin needs none); kept to the control-plane nodes, no worker ever holds it.
	controlPlane := map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
		"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{map[string]any{
			"key": controlPlaneRole, "operator": string(corev1.NodeSelectorOpExists),
		}}}},
	}}}
	controlPlaneTolerations := func(more ...any) []any {
		return append([]any{
			toleration(controlPlaneTaint.Key, controlPlaneTaint.Effect),
			toleration("node-role.kubernetes.io/etcd", corev1.TaintEffectNoExecute),
		}, more...)
	}
	ccm := map[string]any{ //nolint:gosec // names a Secret
		"existingConfigSecret":    workloadSecret,
		"existingConfigSecretKey": "ccm.yaml",
		// On the host's network: nodes are initialised before the CNI runs, and a CNI that waits
		// for initialised nodes (RKE2's Calico schedules Typha only there) would otherwise leave a
		// CCM on the pod network unable to reach Proxmox VE, and the cluster stuck.
		"useDaemonSet": true,
		"affinity":     controlPlane,
		tolerationsKey: controlPlaneTolerations(toleration("node.cloudprovider.kubernetes.io/uninitialized", corev1.TaintEffectNoSchedule)),
		"extraArgs":    leaseArgs("leader-elect"),
	}
	sidecar := func(args ...any) map[string]any {
		return map[string]any{"args": append(args, leaseArgs("leader-election")...)}
	}
	snapshotter := sidecar()
	config := workloadSecret
	csi := map[string]any{
		// A sidecar's args replace the chart's own, which for the attacher and the provisioner are
		// the file system they default to.
		controllerKey: map[string]any{
			"attacher":    sidecar("--default-fstype=ext4"),
			"provisioner": sidecar("--default-fstype=ext4"),
			"resizer":     sidecar(),
			"snapshotter": snapshotter,
		},
		"affinity":                controlPlane,
		tolerationsKey:            controlPlaneTolerations(),
		"existingConfigSecretKey": csiConfigKey,
		"storageClass": []any{map[string]any{
			"name":          "proxmox",
			"storage":       storage,
			"reclaimPolicy": string(corev1.PersistentVolumeReclaimDelete),
			"fstype":        "ext4",
			annotationsKey:  map[string]any{"storageclass.kubernetes.io/is-default-class": strconv.FormatBool(true)},
		}},
	}
	if addons.VolumeSnapshots {
		snapshotter["enabled"] = true
		config = rootWorkloadSecret
		csi["timeout"] = csiCopyTimeout
	}
	csi["existingConfigSecret"] = config
	for k, v := range ca {
		ccm[k], csi[k] = v, v
	}
	return ccm, csi
}

// proxmoxCharts are the charts that talk to Proxmox VE: the cloud controller manager, the CSI
// plugin, and with volume snapshots the snapshot controller and the plugin's VolumeSnapshotClass.
func proxmoxCharts(charts *Charts, distribution templates.Distribution, storage string, addons *Addons) []chartProxy {
	ccm, csi := proxmoxValues(storage, addons)
	csiChart := charts.CSI
	// root@pam needs none of the privileges the newest release is held back for.
	if addons.CSINewest || addons.VolumeSnapshots {
		csiChart = charts.CSINewest
	}
	wanted := []chartProxy{
		{"proxmox-ccm", "proxmox-cloud-controller-manager", metav1.NamespaceSystem, charts.CCM, ccm},
		{"proxmox-csi", "proxmox-csi-plugin", metav1.NamespaceSystem, csiChart, csi},
	}
	if addons.VolumeSnapshots {
		wanted = append(wanted, snapshotCharts(charts, distribution)...)
	}
	return wanted
}

// chartProxy is a chart a cluster gets, by the suffix of its HelmChartProxy's name.
type chartProxy struct {
	suffix, release, namespace string
	chart                      Chart
	values                     map[string]any
}

// cniChart is the chart of the CNI named by cni, if one is.
func cniChart(cluster *clusterv1.Cluster, charts *Charts, cni CNI, addons *Addons) ([]chartProxy, error) {
	switch cni.Name {
	case "":
		return nil, nil
	case cniCilium:
		pods, err := podRange(cluster, cni)
		if err != nil {
			return nil, err
		}
		values, err := ciliumValues(cluster, cni.ClusterID, addons, map[string]any{"replicas": 1})
		if err != nil {
			return nil, err
		}
		values["ipam"] = map[string]any{"mode": "cluster-pool", "operator": map[string]any{"clusterPoolIPv4PodCIDRList": []any{pods}}}
		return []chartProxy{{"cni", cniCilium, metav1.NamespaceSystem, charts.Cilium, values}}, nil
	case cniCalico:
		// Only the operator: it creates Calico's CRDs itself, and calicoResources brings the
		// resources of them that the chart would otherwise render before they exist.
		off := map[string]any{"enabled": false}
		return []chartProxy{{"cni", cniCalico, "tigera-operator", charts.Calico, map[string]any{
			"manageCRDs": true, "installation": off, "apiServer": off, "goldmane": off, "whisker": off,
		}}}, nil
	}
	return nil, fmt.Errorf("cluster %s/%s: no chart for CNI %q", cluster.Namespace, cluster.Name, cni.Name)
}

// helmChartProxies installs the cloud controller manager and the CSI plugin into the cluster, the
// CNI named by cni unless none is, the add-ons switched on, the load balancer, the GitOps engine,
// and on kubeadm, which brings no ingress controller, Traefik. The Add-on Provider for Helm
// selects only Clusters in a proxy's own namespace, so each cluster gets its own proxies beside it.
func helmChartProxies(cluster *clusterv1.Cluster, charts *Charts, distribution templates.Distribution, storage string, cni CNI, addons *Addons) ([]*unstructured.Unstructured, error) {
	wanted := proxmoxCharts(charts, distribution, storage, addons)
	cniCharts, err := cniChart(cluster, charts, cni, addons)
	if err != nil {
		return nil, err
	}
	wanted = append(wanted, cniCharts...)
	if addons.Headlamp.Enabled {
		wanted = append(wanted, chartProxy{headlampAddon, headlampAddon, headlampAddon, charts.Headlamp, headlampValues(addons.Headlamp, &addons.Certificates)})
	}
	if addons.certManager() {
		wanted = append(wanted, chartProxy{certManagerAddon, certManagerAddon, certManagerAddon, charts.CertManager, certManagerValues(&addons.Certificates)})
	}
	if addons.Rancher.Install {
		if addons.Rancher.Hostname == "" {
			return nil, fmt.Errorf("cluster %s/%s: Rancher needs a hostname", cluster.Namespace, cluster.Name)
		}
		wanted = append(wanted, chartProxy{rancherAddon, rancherAddon, rancherNamespace, charts.Rancher, rancherValues(addons.Rancher, charts.Rancher.Values)})
	}
	if addons.Monitoring.Enabled {
		wanted = append(wanted, chartProxy{
			monitoringAddon, monitoringRelease, monitoringAddon, charts.Monitoring,
			monitoringValues(distribution, charts.KubeRBACProxy, addons),
		})
	}
	if addons.logsOn() {
		wanted = append(wanted, logsCharts(charts, distribution, addons.Logs)...)
	}
	if addons.Backups.Enabled && addons.backupTarget != nil {
		wanted = append(wanted, chartProxy{veleroRelease, veleroRelease, VeleroNamespace, charts.Velero, veleroValues(charts, &addons.Backups, addons.backupTarget, cluster)})
	}
	if addons.PodSecurity.kyvernoOn() {
		wanted = append(wanted,
			chartProxy{kyvernoAddon, kyvernoAddon, kyvernoAddon, charts.Kyverno, kyvernoValues()},
			chartProxy{kyvernoPolicies, kyvernoPolicies, kyvernoAddon, charts.KyvernoPolicies, kyvernoPolicyValues(addons.PodSecurity.Kyverno, charts.PodSecurityExempt)},
		)
	}
	if addons.Istio.Enabled {
		wanted = append(wanted, istioCharts(charts, distribution, cni)...)
	}
	serving, err := servingCharts(cluster, charts, distribution, addons)
	if err != nil {
		return nil, err
	}
	return chartProxyObjects(cluster, append(wanted, serving...))
}

// servingCharts are those that bring traffic into the cluster and what it runs from Git: on
// kubeadm, which brings no ingress controller, Traefik; the load balancer; the GitOps engine.
func servingCharts(cluster *clusterv1.Cluster, charts *Charts, distribution templates.Distribution, addons *Addons) ([]chartProxy, error) {
	var wanted []chartProxy
	if distribution == templates.Kubeadm {
		endpoint, _, err := optionalVariable[string](cluster, "controlPlaneEndpoint")
		if err != nil {
			return nil, err
		}
		wanted = append(wanted, chartProxy{"traefik", "traefik", metav1.NamespaceSystem, charts.Traefik, traefikValues(endpoint, &addons.Certificates)})
	}
	if addons.LoadBalancer.Enabled {
		lb, err := loadBalancerCharts(charts, addons.LoadBalancer)
		if err != nil {
			return nil, fmt.Errorf("cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
		}
		wanted = append(wanted, lb...)
	}
	if addons.GitOps.Enabled {
		g, err := gitOpsCharts(charts, &addons.Certificates, &addons.GitOps, &addons.gitOpsFound)
		if err != nil {
			return nil, fmt.Errorf("cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
		}
		wanted = append(wanted, g...)
	}
	return wanted, nil
}

// chartProxyObjects makes the cluster's HelmChartProxies of the charts it wants, in their order,
// each selecting the cluster alone, with its chart's values beneath its own.
func chartProxyObjects(cluster *clusterv1.Cluster, wanted []chartProxy) ([]*unstructured.Unstructured, error) {
	proxies := make([]*unstructured.Unstructured, 0, len(wanted))
	for i := range wanted {
		p := &wanted[i]
		values, err := yaml.Marshal(overlay(p.chart.Values, p.values))
		if err != nil {
			return nil, fmt.Errorf("encoding the values of %s: %w", p.release, err)
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(helmChartProxyGVK)
		u.SetName(cluster.Name + "-" + p.suffix)
		u.SetNamespace(cluster.Namespace)
		u.Object["spec"] = map[string]any{
			"clusterSelector": map[string]any{"matchLabels": map[string]any{clusterv1.ClusterNameLabel: cluster.Name}},
			"repoURL":         p.chart.Repository,
			"chartName":       p.chart.Name,
			"version":         p.chart.pinnedVersion(),
			"releaseName":     p.release,
			"namespace":       p.namespace,
			"options":         map[string]any{"install": map[string]any{"createNamespace": true}},
			// Values are plain text in HelmReleaseProxy objects; they hold no secret, only where it is.
			// The provider renders them as a Go template, which none of them is: what a cluster's
			// owner typed in, a repository's URL or a group's name, comes through as it is.
			"valuesTemplate": strings.ReplaceAll(string(values), "{{", `{{"{{"}}`),
		}
		proxies = append(proxies, u)
	}
	return proxies, nil
}

// overlay lays values over base into a new map, leaving both as they are: maps merge key by key,
// and anything else in values replaces what base has.
func overlay(base, values map[string]any) map[string]any {
	merged := maps.Clone(values)
	if merged == nil {
		merged = map[string]any{}
	}
	for k, b := range base {
		v, ok := merged[k]
		if !ok {
			merged[k] = b
			continue
		}
		bm, bIsMap := b.(map[string]any)
		vm, vIsMap := v.(map[string]any)
		if bIsMap && vIsMap {
			merged[k] = overlay(bm, vm)
		}
	}
	return merged
}

func credentialsSecretName(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-proxmox-token"
}

// credentialsSecret keeps what Ensure returned, so that later reconciles neither replace the token
// nor create a second volume owner.
func credentialsSecret(cluster *clusterv1.Cluster, c Credentials, ready bool) *corev1.Secret {
	return &corev1.Secret{
		Name: credentialsSecretName(cluster), Namespace: cluster.Namespace,
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tokenID":         []byte(c.TokenID),
			"tokenSecret":     []byte(c.TokenSecret),
			"volumeOwner":     []byte(strconv.Itoa(c.VolumeOwner)),
			"volumeOwnerNode": []byte(c.VolumeOwnerNode),
			regionKey:         []byte(c.Region),
			"ready":           []byte(strconv.FormatBool(ready)),
		},
	}
}

func credentialsFrom(s *corev1.Secret) (Credentials, error) {
	owner, err := strconv.Atoi(string(s.Data["volumeOwner"]))
	if err != nil {
		return Credentials{}, fmt.Errorf("secret %s/%s: volumeOwner: %w", s.Namespace, s.Name, err)
	}
	return Credentials{
		TokenID:         string(s.Data["tokenID"]),
		TokenSecret:     string(s.Data["tokenSecret"]),
		VolumeOwner:     owner,
		VolumeOwnerNode: string(s.Data["volumeOwnerNode"]),
		Region:          string(s.Data[regionKey]),
	}, nil
}

// isReady reports whether a credentials Secret marks the cluster's objects as all in place.
func isReady(s *corev1.Secret) bool {
	return string(s.Data["ready"]) == trueText
}
