package provisioner

import (
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// Monitoring is a cluster's monitoring variable.
type Monitoring struct {
	Enabled   bool   `json:"enabled"`
	Hostname  string `json:"hostname,omitempty"`
	TLSSecret string `json:"tlsSecret,omitempty"`
}

const (
	monitoringAddon   = "monitoring"
	monitoringRelease = "kube-prometheus-stack"
	// nodeExporterNamespace holds node-exporter apart from the rest: it reads the host's namespaces
	// and file system, which only Pod Security's privileged level allows.
	nodeExporterNamespace = "node-exporter"
	nodeExporter          = "node-exporter"

	// metricsUser is who Prometheus is to the metrics proxies, by its client certificate. A bearer
	// token would do too, but whoever receives one can replay it.
	metricsUser = "kubehenge:prometheus"
	// metricsIssuer signs the metrics proxies' serving certificates and Prometheus's client
	// certificates, and nothing else, so the proxies trust no other client.
	metricsIssuer       = "kubehenge-metrics"
	metricsClientSecret = "kubehenge-metrics-client" //nolint:gosec // a Secret's name, not a credential
	metricsProxyTLS     = "kubehenge-metrics-proxy-tls"
	metricsProxyAccount = "kubehenge-metrics-proxy"
	etcdMetrics         = "kubehenge-etcd-metrics"
	kubeProxyMetrics    = "kubehenge-kube-proxy-metrics"
	// The ports etcd and kube-proxy serve metrics on, on the loopback interface; their proxies
	// take the same port on the node's address.
	etcdMetricsPort      = 2381
	kubeProxyMetricsPort = 10249

	// The kube-rbac-proxy image's own user.
	proxyUser = 65532
	// What Prometheus keeps: the chart's ten days, within a volume of prometheusStorage.
	prometheusStorage   = "20Gi"
	prometheusRetention = "18GB"
	alertmanagerStorage = "1Gi"

	// appName is the label the proxies' pods are selected by.
	appName           = "app.kubernetes.io/name"
	certManagerAPI    = "cert-manager.io/v1"
	clusterIssuerKind = "ClusterIssuer"
	// keyBits is the size of every metrics key, ECDSA's P-256.
	keyBits = 256
)

// monitoringProxy names the cluster's kube-prometheus-stack proxy, which goes again when
// monitoring is switched off.
func monitoringProxy(cluster *clusterv1.Cluster) string {
	return cluster.Name + "-" + monitoringAddon
}

// Image is a container image named by its tag and pinned by its digest.
type Image struct {
	Registry, Repository, Tag, Digest string
}

// ParseImage reads <registry>/<repository>:<tag>@sha256:<digest>.
func ParseImage(s string) (Image, error) {
	name, digest, ok := strings.Cut(s, "@sha256:")
	registry, path, ok2 := strings.Cut(name, "/")
	at := strings.LastIndex(path, ":")
	if !ok || !ok2 || digest == "" || at <= 0 || at == len(path)-1 {
		return Image{}, fmt.Errorf("%q is not <registry>/<repository>:<tag>@sha256:<digest>", s)
	}
	return Image{Registry: registry, Repository: path[:at], Tag: path[at+1:], Digest: digest}, nil
}

func (i Image) String() string {
	return i.Registry + "/" + i.Repository + ":" + i.Tag + "@sha256:" + i.Digest
}

// monitoringValues install kube-prometheus-stack so that every scrape is authenticated and its
// server verified. The controller manager and the scheduler serve on the node's address behind
// their own authentication; etcd and kube-proxy serve plain HTTP on loopback, so each gets a
// kube-rbac-proxy on the node's address that takes only Prometheus's client certificate. On k3s
// every component runs in the one process whose kubelet already serves all their metrics, as
// Rancher scrapes it, so the chart's rules look for them in the kubelet's job. The chart's
// long-lived token goes only where a component asks for one, never over plain HTTP.
func monitoringValues(distribution templates.Distribution, proxy Image, m Monitoring) map[string]any {
	verified := map[string]any{"insecureSkipVerify": false}
	if distribution == templates.RKE2 {
		// RKE2's serving certificates for the two name only the loopback addresses, under the
		// server CA the cluster's root CA ConfigMap holds.
		verified["serverName"] = "localhost"
	}
	secure := map[string]any{"serviceMonitor": map[string]any{"https": true, "tlsConfig": verified}}
	values := map[string]any{
		"kubelet":               map[string]any{"serviceMonitor": map[string]any{"tlsConfig": map[string]any{"insecureSkipVerify": false}}},
		"kubeControllerManager": secure,
		"kubeScheduler":         secure,
		"kubeEtcd":              proxied(etcdMetrics, etcdMetricsPort, "scheme", "https"),
		"kubeProxy":             proxied(kubeProxyMetrics, kubeProxyMetricsPort, "https", true),
		"coreDns":               map[string]any{"serviceMonitor": map[string]any{"authorization": nil}},
		"prometheus-node-exporter": map[string]any{
			"fullnameOverride":  nodeExporter,
			"namespaceOverride": nodeExporterNamespace,
			// node-exporter itself listens on loopback only, behind the proxy.
			"kubeRBACProxy": map[string]any{
				"enabled":                  true,
				"image":                    map[string]any{"registry": proxy.Registry, "repository": proxy.Repository, "tag": proxy.Tag, "sha": proxy.Digest},
				"tls":                      map[string]any{"enabled": true, "tlsClientAuth": true},
				"containerSecurityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}}},
			},
			"tlsSecret": map[string]any{"enabled": true, "secretName": nodeExporter + "-tls", "caItem": "ca.crt"},
			"prometheus": map[string]any{"monitor": map[string]any{
				"scheme": "https", "tlsConfig": clientTLS(nodeExporter + "." + nodeExporterNamespace + ".svc"),
			}},
		},
		// cert-manager is there for the proxies already; it renews the webhook's certificate
		// instead of the chart's one-off Jobs.
		"prometheusOperator": map[string]any{"admissionWebhooks": map[string]any{"certManager": map[string]any{"enabled": true}}},
		"prometheus": map[string]any{"prometheusSpec": map[string]any{
			"storageSpec":   claim(prometheusStorage),
			"retentionSize": prometheusRetention,
		}},
		"alertmanager":   map[string]any{"alertmanagerSpec": map[string]any{"storage": claim(alertmanagerStorage)}},
		"extraManifests": metricsManifests(distribution, proxy),
	}
	if distribution == templates.K3s {
		off := map[string]any{"enabled": false}
		inKubelet := map[string]any{"jobNameOverride": "kubelet", "service": off, "serviceMonitor": off}
		values["kubeControllerManager"], values["kubeScheduler"], values["kubeProxy"] = inKubelet, inKubelet, inKubelet
	}
	if m.Hostname != "" {
		values["grafana"] = map[string]any{
			"ingress":     map[string]any{"enabled": true, "hosts": []any{m.Hostname}, "tls": []any{ingressTLS(m.Hostname, m.TLSSecret)}},
			"grafana.ini": map[string]any{"server": map[string]any{"root_url": "https://" + m.Hostname}},
		}
	}
	return values
}

// proxied scrapes a component through its metrics proxy, with the chart's own way of saying
// HTTPS for it, and without the chart's token.
func proxied(proxy string, port int, httpsKey string, https any) map[string]any {
	return map[string]any{
		"service": map[string]any{"selector": map[string]any{appName: proxy}, "port": port, "targetPort": port},
		"serviceMonitor": map[string]any{
			httpsKey: https, "authorization": nil, "tlsConfig": clientTLS(proxy + "." + metav1.NamespaceSystem + ".svc"),
		},
	}
}

// ingressTLS is an Ingress's TLS for hostname, with the certificate in secret, or the ingress
// controller's own default without one.
func ingressTLS(hostname, secret string) map[string]any {
	tls := map[string]any{"hosts": []any{hostname}}
	if secret != "" {
		tls["secretName"] = secret
	}
	return tls
}

// clientTLS is how Prometheus reaches a metrics proxy: with its client certificate, trusting only
// the metrics CA, for the name the proxy's certificate carries.
func clientTLS(serverName string) map[string]any {
	key := func(k string) map[string]any { return map[string]any{"name": metricsClientSecret, "key": k} }
	return map[string]any{
		"insecureSkipVerify": false,
		"serverName":         serverName,
		"ca":                 map[string]any{"configMap": nil, "secret": key("ca.crt")},
		"cert":               map[string]any{"secret": key("tls.crt")},
		"keySecret":          key("tls.key"),
	}
}

func claim(size string) map[string]any {
	return map[string]any{"volumeClaimTemplate": map[string]any{"spec": map[string]any{
		"accessModes": []any{string(corev1.ReadWriteOnce)},
		"resources":   map[string]any{"requests": map[string]any{"storage": size}},
	}}}
}

// metricsManifests are what the chart does not bring: the metrics CA and certificates, the
// proxies and their rights, and node-exporter's privileged namespace. They are part of the
// release, so they go when monitoring does; until cert-manager serves, the release fails and the
// Add-on Provider for Helm tries it again.
func metricsManifests(distribution templates.Distribution, proxy Image) []any {
	server := func(names ...string) map[string]any {
		return map[string]any{"dnsNames": names, "usages": []any{"server auth", "digital signature"}}
	}
	client := func() map[string]any {
		return map[string]any{"commonName": metricsUser, "usages": []any{"client auth", "digital signature"}}
	}
	privileged := map[string]string{}
	for _, mode := range []string{"enforce", "audit", "warn"} {
		privileged["pod-security.kubernetes.io/"+mode] = "privileged"
	}
	user := []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: metricsUser}}
	out := []any{
		&corev1.Namespace{APIVersion: "v1", Kind: "Namespace", Name: nodeExporterNamespace, Labels: privileged},
		issuer(metricsIssuer+"-root", map[string]any{"selfSigned": map[string]any{}}),
		// Long-lived: cert-manager renews a CA with a new key, which every certificate it signed
		// would have to follow at once.
		certificate(certManagerAddon, metricsIssuer+"-ca", metricsIssuer+"-root", map[string]any{
			"isCA": true, "commonName": metricsIssuer, "duration": "87600h",
		}),
		issuer(metricsIssuer, map[string]any{"ca": map[string]any{"secretName": metricsIssuer + "-ca"}}),
		certificate(metav1.NamespaceSystem, metricsProxyTLS, metricsIssuer,
			server(etcdMetrics+"."+metav1.NamespaceSystem+".svc", kubeProxyMetrics+"."+metav1.NamespaceSystem+".svc")),
		certificate(nodeExporterNamespace, nodeExporter+"-tls", metricsIssuer, server(nodeExporter+"."+nodeExporterNamespace+".svc")),
		certificate(monitoringAddon, metricsClientSecret, metricsIssuer, client()),
		certificate(nodeExporterNamespace, metricsClientSecret, metricsIssuer, client()),
		&rbacv1.ClusterRole{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole", Name: metricsIssuer,
			Rules: []rbacv1.PolicyRule{{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}}},
		},
		&rbacv1.ClusterRoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding", Name: metricsIssuer,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: metricsIssuer},
			Subjects: user,
		},
		// node-exporter's chart has its proxy ask whether the client may get its Service's
		// node-exporter subresource.
		&rbacv1.Role{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role", Name: metricsIssuer, Namespace: nodeExporterNamespace,
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"services/" + nodeExporter}, ResourceNames: []string{nodeExporter}, Verbs: []string{"get"},
			}},
		},
		&rbacv1.RoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding", Name: metricsIssuer, Namespace: nodeExporterNamespace,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: metricsIssuer},
			Subjects: user,
		},
		&corev1.ServiceAccount{APIVersion: "v1", Kind: "ServiceAccount", Name: metricsProxyAccount, Namespace: metav1.NamespaceSystem},
		// The proxies ask the API server who a client is and what it may do.
		&rbacv1.ClusterRoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding", Name: metricsProxyAccount,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:auth-delegator"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: metricsProxyAccount, Namespace: metav1.NamespaceSystem}},
		},
		metricsProxy(etcdMetrics, etcdMetricsPort, &controlPlaneAffinity, proxy),
	}
	// k3s's kube-proxy is in the kubelet's metrics.
	if distribution != templates.K3s {
		out = append(out, metricsProxy(kubeProxyMetrics, kubeProxyMetricsPort, nil, proxy))
	}
	return out
}

func issuer(name string, spec map[string]any) map[string]any {
	return map[string]any{"apiVersion": certManagerAPI, "kind": clusterIssuerKind, "metadata": map[string]any{"name": name}, "spec": spec}
}

// certificate is a cert-manager Certificate of a key of its own, in a Secret of its own name.
func certificate(namespace, name, issuer string, spec map[string]any) map[string]any {
	spec["secretName"] = name
	spec["privateKey"] = map[string]any{"algorithm": "ECDSA", "size": keyBits}
	spec["issuerRef"] = map[string]any{"kind": clusterIssuerKind, "name": issuer}
	return map[string]any{
		"apiVersion": certManagerAPI, "kind": "Certificate",
		"metadata": map[string]any{"name": name, "namespace": namespace}, "spec": spec,
	}
}

// controlPlaneAffinity keeps a pod to the control-plane nodes.
var controlPlaneAffinity = corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
	RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: controlPlaneRole, Operator: corev1.NodeSelectorOpExists}},
	}}},
}}

// metricsProxy is a kube-rbac-proxy on every node affinity allows, in front of a component's
// metrics on loopback. It listens on the node's address and the component's own port, so that the
// component serving there itself would keep it from starting rather than sit beside it
// unauthenticated. It opens no other port: probes would need one of their own on the host.
func metricsProxy(name string, port int32, affinity *corev1.Affinity, image Image) *appsv1.DaemonSet {
	p := strconv.Itoa(int(port))
	labels := map[string]string{appName: name}
	user, no, yes := int64(proxyUser), false, true
	return &appsv1.DaemonSet{
		APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "DaemonSet",
		Name: name, Namespace: metav1.NamespaceSystem, Labels: labels,
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{Labels: labels, Spec: corev1.PodSpec{
				ServiceAccountName: metricsProxyAccount,
				HostNetwork:        true,
				PriorityClassName:  "system-node-critical",
				Affinity:           affinity,
				Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: &yes, RunAsUser: &user, RunAsGroup: &user,
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:  "kube-rbac-proxy",
					Image: image.String(),
					Args: []string{
						"--secure-listen-address=$(HOST_IP):" + p,
						"--upstream=http://127.0.0.1:" + p + "/",
						"--allow-paths=/metrics",
						"--client-ca-file=/etc/kube-rbac-proxy/ca.crt",
						"--tls-cert-file=/etc/kube-rbac-proxy/tls.crt",
						"--tls-private-key-file=/etc/kube-rbac-proxy/tls.key",
						"--tls-min-version=VersionTLS13",
					},
					Env:   []corev1.EnvVar{{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}}},
					Ports: []corev1.ContainerPort{{Name: "https-metrics", ContainerPort: port}},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &no,
						ReadOnlyRootFilesystem:   &yes,
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "tls", MountPath: "/etc/kube-rbac-proxy", ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{{Name: "tls", Secret: &corev1.SecretVolumeSource{SecretName: metricsProxyTLS}}},
			}},
		},
	}
}
