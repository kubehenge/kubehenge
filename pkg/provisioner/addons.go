package provisioner

import (
	"fmt"
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
	requestsKey       = "requests"
	tolerationsKey    = "tolerations"
	memoryKey         = "memory"
	cpuKey            = "cpu"

	// The CNIs Kubehenge installs, as the cni variable names them.
	cniCilium = "cilium"
	cniCalico = "calico"
)

// Charts locates the Helm charts installed into clusters: the cloud controller manager and CSI
// plugin into every one, a CNI into those whose distribution has none of its own, and the add-ons
// a cluster switches on.
type Charts struct {
	CCM      Chart
	CSI      Chart
	Cilium   Chart
	Calico   Chart
	Headlamp Chart
	// HeadlampImage is the image tag with its digest; the chart names its image by tag only.
	HeadlampImage string
	CertManager   Chart
	Rancher       Chart
	Monitoring    Chart
	// KubeRBACProxy is the image of the proxies in front of metrics served without authentication.
	KubeRBACProxy   Image
	Kyverno         Chart
	KyvernoPolicies Chart
	// Istio is istiod's chart; its base, cni and ztunnel charts are beside it, at its version.
	Istio Chart
	// Traefik is the ingress controller of distributions that bring none.
	Traefik Chart
	// PodSecurityExempt are the namespaces Kyverno's policies leave alone, those the ClusterClasses'
	// Pod Security Admission exempts.
	PodSecurityExempt []string
}

// Addons are the add-ons a cluster switches on through its variables.
type Addons struct {
	Headlamp    Headlamp
	Rancher     Rancher
	Monitoring  Monitoring
	PodSecurity PodSecurity
	Istio       Istio
	ClusterMesh ClusterMesh
	// CIS is the cluster's cis variable, which the add-ons make room for.
	CIS bool
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
	a.Rancher, err = rancherOf(cluster)
	return a, err
}

// certManagerProxy names the cluster's cert-manager proxy, installed for Rancher and for
// monitoring, either or both.
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
// and liveness keeps its own.
func rancherValues(r Rancher) map[string]any {
	const (
		// A negative count is the chart's "up to": as many replicas as nodes, at most three.
		replicas      = -3
		probeTimeout  = 5
		probePeriod   = 10
		probeFailures = 60
	)
	return map[string]any{
		"hostname":     r.Hostname,
		"replicas":     replicas,
		"agentTLSMode": "strict",
		"ingress":      map[string]any{"tls": map[string]any{"source": "rancher"}},
		"extraEnv":     []any{map[string]any{"name": "CATTLE_SERVER_URL", "value": "https://" + r.Hostname}},
		"startupProbe": map[string]any{"timeoutSeconds": probeTimeout, "periodSeconds": probePeriod, "failureThreshold": probeFailures},
	}
}

// Headlamp is a cluster's headlamp variable.
type Headlamp struct {
	Enabled   bool   `json:"enabled"`
	Hostname  string `json:"hostname,omitempty"`
	TLSSecret string `json:"tlsSecret,omitempty"`
}

// Chart is one Helm chart.
type Chart struct {
	Repository string
	Name       string
	Version    string
}

// ParseChart reads <repository>/<name>:<version>, such as
// oci://ghcr.io/sergelogvinov/charts/proxmox-csi-plugin:0.5.12 or https://helm.cilium.io/cilium:1.20.2.
func ParseChart(s string) (Chart, error) {
	at := strings.LastIndex(s, ":")
	slash := strings.LastIndex(s, "/")
	if slash < 0 || at <= slash+1 || at == len(s)-1 {
		return Chart{}, fmt.Errorf("%q is not <repository>/<name>:<version>", s)
	}
	return Chart{Repository: s[:slash], Name: s[slash+1 : at], Version: s[at+1:]}, nil
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
}

type proxmoxConfig struct {
	Features proxmoxFeatures  `json:"features"`
	Clusters []proxmoxCluster `json:"clusters"`
}

type proxmoxFeatures struct {
	Provider       string `json:"provider"`
	ControllerVMID int    `json:"controllerVmID,omitempty"`
}

type proxmoxCluster struct {
	URL         string `json:"url"`
	Insecure    bool   `json:"insecure"`
	TokenID     string `json:"token_id"`
	TokenSecret string `json:"token_secret"`
	Region      string `json:"region"`
}

// resourceSetSecret wraps the workload cluster's Secret for a ClusterResourceSet, which applies
// every key of its Secret as a manifest.
func resourceSetSecret(cluster *clusterv1.Cluster, endpoint Endpoint, c Credentials) (*corev1.Secret, error) {
	pve := proxmoxCluster{URL: endpoint.URL + "/api2/json", TokenID: c.TokenID, TokenSecret: c.TokenSecret, Region: c.Region}
	// capmox names machines proxmox://<SMBIOS UUID>, which only the capmox provider mode matches.
	ccm, err := yaml.Marshal(proxmoxConfig{Features: proxmoxFeatures{Provider: "capmox"}, Clusters: []proxmoxCluster{pve}})
	if err != nil {
		return nil, fmt.Errorf("encoding the cloud controller manager's configuration: %w", err)
	}
	csi, err := yaml.Marshal(proxmoxConfig{
		Features: proxmoxFeatures{Provider: "capmox", ControllerVMID: c.VolumeOwner},
		Clusters: []proxmoxCluster{pve},
	})
	if err != nil {
		return nil, fmt.Errorf("encoding the CSI plugin's configuration: %w", err)
	}
	inner := &corev1.Secret{
		APIVersion: "v1",
		Kind:       secretKind,
		Name:       workloadSecret,
		Namespace:  metav1.NamespaceSystem,
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"ccm.yaml": string(ccm), "csi.yaml": string(csi), proxmoxCAKey: endpoint.CA},
	}
	manifest, err := yaml.Marshal(inner)
	if err != nil {
		return nil, fmt.Errorf("encoding the workload cluster's Secret: %w", err)
	}
	return &corev1.Secret{
		Name: cluster.Name + "-proxmox", Namespace: cluster.Namespace,
		Type: addonsv1.ClusterResourceSetSecretType,
		Data: map[string][]byte{workloadSecret + ".yaml": manifest},
	}, nil
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

// calicoResources wraps, for the ClusterResourceSet, the resources the Tigera operator installs
// Calico from, as its chart's defaults have them. The chart would render them before the operator
// has created their CRDs, which a Helm release cannot wait for; a ClusterResourceSet retries until
// they exist. Nor can Helm install the CRDs first: Calico's CRD chart is larger than the 1 MiB
// Secret a Helm release is kept in.
func calicoResources(cluster *clusterv1.Cluster, cni CNI) (*corev1.Secret, error) {
	pods, err := podRange(cluster, cni)
	if err != nil {
		return nil, err
	}
	network := map[string]any{"ipPools": []any{map[string]any{"cidr": pods, "encapsulation": "VXLANCrossSubnet"}}}
	if cni.ContainerForwarding {
		network["containerIPForwarding"] = "Enabled"
	}
	resources := []map[string]any{
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
		r["metadata"] = map[string]any{"name": "default"}
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
	values := map[string]any{
		"cluster":           map[string]any{"name": cluster.Name, "id": id},
		"rollOutCiliumPods": true,
		"operator":          operator,
	}
	if addons.ClusterMesh.Enabled {
		values["clustermesh"] = clusterMeshValues()
	}
	if addons.Istio.Enabled {
		// Cilium would otherwise take istio-cni's plugin out of the node's CNI configuration.
		values["cni"] = map[string]any{"exclusive": false}
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

// headlampValues keep Headlamp without rights of its own: every user signs in with their own token
// or through OIDC. Its service account token stays mounted only because Headlamp reads the API
// server's address and CA through it; the account is bound to no role. Plugins are those in the
// image, nothing fetched at run time.
func headlampValues(image string, h Headlamp) map[string]any {
	// The image's own user and group.
	const user, group = 100, 101
	values := map[string]any{
		"image":              map[string]any{"tag": image},
		"clusterRoleBinding": map[string]any{"create": false},
		"config":             map[string]any{"oidc": map[string]any{"secret": map[string]any{"create": false}}},
		"pluginsManager":     map[string]any{"enabled": false},
		// The chart's own default leaves out what Pod Security's restricted level requires.
		"securityContext": map[string]any{
			"runAsNonRoot":             true,
			"runAsUser":                user,
			"runAsGroup":               group,
			"privileged":               false,
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   true,
			"capabilities":             map[string]any{"drop": []any{"ALL"}},
			seccompProfileKey:          map[string]any{"type": "RuntimeDefault"},
		},
		"resources": map[string]any{
			requestsKey: map[string]any{cpuKey: "10m", memoryKey: "64Mi"},
			"limits":    map[string]any{memoryKey: "256Mi"},
		},
	}
	if h.Hostname != "" {
		values["ingress"] = map[string]any{
			"enabled": true,
			"hosts":   []any{map[string]any{"host": h.Hostname, "paths": []any{map[string]any{"path": "/", "type": "Prefix"}}}},
			"tls":     []any{ingressTLS(h.Hostname, h.TLSSecret)},
		}
	}
	return values
}

// traefikValues run Traefik as RKE2 runs its own: on every node, on the node's ports 80 and 443,
// so that a host name pointing at any node works without a load balancer. It runs on the control
// planes too, so that the cluster's API address serves it as well. Its namespace is kube-system,
// which Pod Security exempts, since host ports are beyond the baseline level.
func traefikValues() map[string]any {
	const http, https = 80, 443
	return map[string]any{
		"deployment": map[string]any{"kind": "DaemonSet"},
		// A host port is held by the pod on it, so a node's new pod starts only once the old one is gone.
		"updateStrategy": map[string]any{"rollingUpdate": map[string]any{"maxUnavailable": 1, "maxSurge": 0}},
		"ports": map[string]any{
			"web":       map[string]any{"hostPort": http},
			"websecure": map[string]any{"hostPort": https},
		},
		"service": map[string]any{"type": "ClusterIP"},
		// The Service's address is one only pods reach, so Ingresses are not told it.
		"providers": map[string]any{"kubernetesIngress": map[string]any{"publishedService": map[string]any{"enabled": false}}},
		// Routes whose TLS options conflict fail rather than fall back to the defaults, which would
		// drop a host's client-certificate check (GHSA-g55h-rg46-x9c5).
		"core":         map[string]any{"strictTLSOptions": true},
		tolerationsKey: []any{toleration(controlPlaneTaint.Key, controlPlaneTaint.Effect)},
		"resources":    map[string]any{requestsKey: map[string]any{cpuKey: "10m", memoryKey: "64Mi"}},
	}
}

var helmChartProxyGVK = schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HelmChartProxy"}

// headlampProxy names the cluster's Headlamp proxy, which goes again when Headlamp is switched off.
func headlampProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-headlamp"
}

// toleration tolerates a taint of any value.
func toleration(key string, effect corev1.TaintEffect) map[string]any {
	return map[string]any{"key": key, "operator": string(corev1.TolerationOpExists), "effect": string(effect)}
}

// proxmoxValues are the values of the two charts that talk to Proxmox VE: the cloud controller
// manager's and the CSI plugin's.
func proxmoxValues(storage string) (map[string]any, map[string]any) {
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
	}
	csi := map[string]any{
		"affinity":                controlPlane,
		tolerationsKey:            controlPlaneTolerations(),
		"existingConfigSecret":    workloadSecret,
		"existingConfigSecretKey": "csi.yaml",
		"storageClass": []any{map[string]any{
			"name":          "proxmox",
			"storage":       storage,
			"reclaimPolicy": string(corev1.PersistentVolumeReclaimDelete),
			"fstype":        "ext4",
			"annotations":   map[string]any{"storageclass.kubernetes.io/is-default-class": strconv.FormatBool(true)},
		}},
	}
	for k, v := range ca {
		ccm[k], csi[k] = v, v
	}
	return ccm, csi
}

// chartProxy is a chart a cluster gets, by the suffix of its HelmChartProxy's name.
type chartProxy struct {
	suffix, release, namespace string
	chart                      Chart
	values                     map[string]any
}

// helmChartProxies installs the cloud controller manager and the CSI plugin into the cluster, the
// CNI named by cni unless none is, the add-ons switched on, and on kubeadm, which brings no
// ingress controller, Traefik. The Add-on Provider for Helm
// selects only Clusters in a proxy's own namespace, so each cluster gets its own proxies beside it.
func helmChartProxies(cluster *clusterv1.Cluster, charts *Charts, distribution templates.Distribution, storage string, cni CNI, addons *Addons) ([]*unstructured.Unstructured, error) {
	ccm, csi := proxmoxValues(storage)
	wanted := []chartProxy{
		{"proxmox-ccm", "proxmox-cloud-controller-manager", metav1.NamespaceSystem, charts.CCM, ccm},
		{"proxmox-csi", "proxmox-csi-plugin", metav1.NamespaceSystem, charts.CSI, csi},
	}
	switch cni.Name {
	case "":
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
		wanted = append(wanted, chartProxy{"cni", cniCilium, metav1.NamespaceSystem, charts.Cilium, values})
	case cniCalico:
		// Only the operator: it creates Calico's CRDs itself, and calicoResources brings the
		// resources of them that the chart would otherwise render before they exist.
		off := map[string]any{"enabled": false}
		wanted = append(wanted, chartProxy{"cni", cniCalico, "tigera-operator", charts.Calico, map[string]any{
			"manageCRDs": true, "installation": off, "apiServer": off, "goldmane": off, "whisker": off,
		}})
	default:
		return nil, fmt.Errorf("cluster %s/%s: no chart for CNI %q", cluster.Namespace, cluster.Name, cni.Name)
	}
	if addons.Headlamp.Enabled {
		wanted = append(wanted, chartProxy{headlampAddon, headlampAddon, headlampAddon, charts.Headlamp, headlampValues(charts.HeadlampImage, addons.Headlamp)})
	}
	if addons.Rancher.Install || addons.Monitoring.Enabled {
		// A Secret goes with its Certificate, so that nothing an add-on asked for outlives it.
		wanted = append(wanted, chartProxy{certManagerAddon, certManagerAddon, certManagerAddon, charts.CertManager, map[string]any{
			"crds": map[string]any{"enabled": true}, "enableCertificateOwnerRef": true,
		}})
	}
	if addons.Rancher.Install {
		if addons.Rancher.Hostname == "" {
			return nil, fmt.Errorf("cluster %s/%s: Rancher needs a hostname", cluster.Namespace, cluster.Name)
		}
		wanted = append(wanted, chartProxy{rancherAddon, rancherAddon, rancherNamespace, charts.Rancher, rancherValues(addons.Rancher)})
	}
	if addons.Monitoring.Enabled {
		wanted = append(wanted, chartProxy{
			monitoringAddon, monitoringRelease, monitoringAddon, charts.Monitoring,
			monitoringValues(distribution, charts.KubeRBACProxy, addons),
		})
	}
	if addons.PodSecurity.kyvernoOn() {
		wanted = append(wanted,
			chartProxy{kyvernoAddon, kyvernoAddon, kyvernoAddon, charts.Kyverno, kyvernoValues()},
			chartProxy{kyvernoPolicies, kyvernoPolicies, kyvernoAddon, charts.KyvernoPolicies, kyvernoPolicyValues(addons.PodSecurity.Kyverno, charts.PodSecurityExempt)},
		)
	}
	if addons.Istio.Enabled {
		wanted = append(wanted, istioCharts(charts.Istio, distribution, cni)...)
	}
	if distribution == templates.Kubeadm {
		wanted = append(wanted, chartProxy{"traefik", "traefik", metav1.NamespaceSystem, charts.Traefik, traefikValues()})
	}

	var proxies []*unstructured.Unstructured
	for _, p := range wanted {
		values, err := yaml.Marshal(p.values)
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
			"version":         p.chart.Version,
			"releaseName":     p.release,
			"namespace":       p.namespace,
			"options":         map[string]any{"install": map[string]any{"createNamespace": true}},
			// Values are plain text in HelmReleaseProxy objects; they hold no secret, only where it is.
			"valuesTemplate": string(values),
		}
		proxies = append(proxies, u)
	}
	return proxies, nil
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
			"region":          []byte(c.Region),
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
		Region:          string(s.Data["region"]),
	}, nil
}

// isReady reports whether a credentials Secret marks the cluster's objects as all in place.
func isReady(s *corev1.Secret) bool {
	return string(s.Data["ready"]) == "true"
}
