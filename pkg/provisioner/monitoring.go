package provisioner

import (
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
	controllerMetrics   = "kubehenge-kube-controller-manager-metrics"
	schedulerMetrics    = "kubehenge-kube-scheduler-metrics"
	// The ports the components serve metrics on, on the loopback interface; their proxies take the
	// same port on the node's address.
	etcdMetricsPort       = 2381
	kubeProxyMetricsPort  = 10249
	controllerMetricsPort = 10257
	schedulerMetricsPort  = 10259
	coreDNSMetricsPort    = 9153

	coreDNSMetricsPolicy = "kubehenge-coredns-metrics"
	// prometheus names Prometheus in the charts' values, and its pods by their app label.
	prometheus = "prometheus"

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
	if !ok || !ok2 || !isDigest(digest) || at <= 0 || at == len(path)-1 {
		return Image{}, fmt.Errorf("%q is not <registry>/<repository>:<tag>@sha256:<digest>", s)
	}
	return Image{Registry: registry, Repository: path[:at], Tag: path[at+1:], Digest: digest}, nil
}

func (i Image) String() string {
	return i.Registry + "/" + i.Repository + ":" + i.Tag + "@sha256:" + i.Digest
}

// monitoringValues install kube-prometheus-stack so that every scrape is authenticated and its
// server verified. etcd, kube-proxy, the controller manager and the scheduler serve their metrics
// on loopback only, as the CIS benchmark asks of the last two, so each gets a kube-rbac-proxy on
// the node's address that takes only Prometheus's client certificate. On k3s every component runs
// in the one process whose kubelet already serves all their metrics, as Rancher scrapes it, so the
// chart's rules look for them in the kubelet's job. The chart's long-lived token goes only where a
// component asks for one, never over plain HTTP. With logs on, Grafana reads Loki too.
func monitoringValues(distribution templates.Distribution, proxy Image, addons *Addons) map[string]any {
	m := addons.Monitoring
	values := map[string]any{
		"kubelet":               map[string]any{"serviceMonitor": map[string]any{"tlsConfig": map[string]any{"insecureSkipVerify": false}}},
		"kubeControllerManager": proxied(controllerMetrics, controllerMetricsPort, "https", true),
		"kubeScheduler":         proxied(schedulerMetrics, schedulerMetricsPort, "https", true),
		"kubeEtcd":              proxied(etcdMetrics, etcdMetricsPort, "scheme", "https"),
		"kubeProxy":             proxied(kubeProxyMetrics, kubeProxyMetricsPort, "https", true),
		"coreDns":               map[string]any{"serviceMonitor": map[string]any{"authorization": nil}},
		"prometheus-node-exporter": map[string]any{
			"fullnameOverride":  nodeExporter,
			"namespaceOverride": nodeExporterNamespace,
			// It reads the host's processes and file systems; Helm merges this into the chart's own
			// pod settings.
			securityKey: map[string]any{seLinuxKey: hostSELinux()},
			"resources": reserved(cpu10m, "32Mi"),
			// node-exporter itself listens on loopback only, behind the proxy.
			"kubeRBACProxy": map[string]any{
				"resources":                reserved(cpu10m, "16Mi"),
				"enabled":                  true,
				"image":                    map[string]any{"registry": proxy.Registry, "repository": proxy.Repository, "tag": proxy.Tag, "sha": proxy.Digest},
				"tls":                      map[string]any{"enabled": true, "tlsClientAuth": true},
				"containerSecurityContext": restrictedContainer(map[string]any{readOnlyRootKey: true}),
			},
			"tlsSecret": map[string]any{"enabled": true, "secretName": nodeExporter + "-tls", "caItem": "ca.crt"},
			prometheus: map[string]any{"monitor": map[string]any{
				"scheme": schemeHTTPS, "tlsConfig": clientTLS(nodeExporter + "." + nodeExporterNamespace + ".svc"),
			}},
		},
		// cert-manager is there for the proxies already; it renews the webhook's certificate
		// instead of the chart's one-off Jobs.
		"prometheusOperator": map[string]any{
			"admissionWebhooks":        map[string]any{"certManager": map[string]any{"enabled": true}},
			"resources":                reserved("50m", memory64Mi),
			"prometheusConfigReloader": map[string]any{"resources": reserved(cpu10m, "16Mi")},
		},
		prometheus: map[string]any{"prometheusSpec": map[string]any{
			"storageSpec":   claim(prometheusStorage),
			"retentionSize": prometheusRetention,
			"resources":     reserved("100m", "256Mi"),
		}},
		"alertmanager": map[string]any{"alertmanagerSpec": map[string]any{
			"storage":   claim(alertmanagerStorage),
			"resources": reserved(cpu10m, "32Mi"),
		}},
		"kube-state-metrics": map[string]any{"resources": reserved(cpu10m, memory64Mi)},
		"extraManifests":     metricsManifests(distribution, proxy, addons),
	}
	switch distribution {
	case templates.K3s:
		off := map[string]any{"enabled": false}
		inKubelet := map[string]any{"jobNameOverride": "kubelet", "service": off, "serviceMonitor": off}
		values["kubeControllerManager"], values["kubeScheduler"], values["kubeProxy"] = inKubelet, inKubelet, inKubelet
	case templates.Talos:
		if !addons.talosServing {
			// Until Talos gives the controller manager and the scheduler a certificate of the
			// cluster CA, they serve one they sign themselves, which Talos keeps inside their pods
			// and offers no way to replace, so no proxy could verify them; they go unscraped, their
			// alerts with them.
			off := map[string]any{"enabled": false}
			values["kubeControllerManager"], values["kubeScheduler"] = off, off
		}
	case templates.Kubeadm, templates.RKE2:
	}
	grafana := map[string]any{
		"resources": reserved("50m", "128Mi"),
		"sidecar":   map[string]any{"resources": reserved(cpu10m, "32Mi")},
	}
	if m.Hostname != "" {
		tls, annotations := addonIngress(&addons.Certificates, "grafana", m.Hostname, m.TLSSecret)
		ingress := map[string]any{"enabled": true, "hosts": []any{m.Hostname}, "tls": []any{tls}}
		if annotations != nil {
			ingress["annotations"] = annotations
		}
		grafana[ingressKey] = ingress
		grafana["grafana.ini"] = map[string]any{"server": map[string]any{"root_url": "https://" + m.Hostname}}
	}
	if addons.logsOn() {
		grafana["additionalDataSources"] = []any{lokiDataSource()}
		grafana["extraSecretMounts"] = []any{grafanaLokiMount()}
	}
	values["grafana"] = grafana
	return values
}

// reserved requests a container its share of a node. The chart requests nothing, and its pods then
// share the least CPU weight a node gives: on a node every add-on starting at once kept busy, the
// operator went unscheduled for over a minute, and its liveness probe restarted it.
func reserved(cpu, memory string) map[string]any {
	return map[string]any{requestsKey: map[string]any{cpuKey: cpu, memoryKey: memory}}
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
		"resources":   map[string]any{requestsKey: map[string]any{"storage": size}},
	}}}
}

// serverCertificate is a certificate for a server's names, from the metrics CA.
func serverCertificate(names ...string) map[string]any {
	return map[string]any{"dnsNames": names, "usages": []any{"server auth", "digital signature"}}
}

// clientCertificate is a certificate a client shows a kube-rbac-proxy, which names it user.
func clientCertificate(user string) map[string]any {
	return map[string]any{"commonName": user, "usages": []any{"client auth", "digital signature"}}
}

// metricsManifests are what the chart does not bring: the metrics CA and certificates, the
// proxies and their rights, and node-exporter's privileged namespace. They are part of the
// release, so they go when monitoring does; until cert-manager serves, the release fails and the
// Add-on Provider for Helm tries it again.
func metricsManifests(distribution templates.Distribution, proxy Image, addons *Addons) []any {
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
		certificate(metav1.NamespaceSystem, metricsProxyTLS, metricsIssuer, serverCertificate(
			etcdMetrics+"."+metav1.NamespaceSystem+".svc", kubeProxyMetrics+"."+metav1.NamespaceSystem+".svc",
			controllerMetrics+"."+metav1.NamespaceSystem+".svc", schedulerMetrics+"."+metav1.NamespaceSystem+".svc",
		)),
		certificate(nodeExporterNamespace, nodeExporter+"-tls", metricsIssuer, serverCertificate(nodeExporter+"."+nodeExporterNamespace+".svc")),
		certificate(monitoringAddon, metricsClientSecret, metricsIssuer, clientCertificate(metricsUser)),
		certificate(nodeExporterNamespace, metricsClientSecret, metricsIssuer, clientCertificate(metricsUser)),
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
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: roleKind, Name: metricsIssuer, Namespace: nodeExporterNamespace,
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"services/" + nodeExporter}, ResourceNames: []string{nodeExporter}, Verbs: []string{"get"},
			}},
		},
		&rbacv1.RoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding", Name: metricsIssuer, Namespace: nodeExporterNamespace,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: roleKind, Name: metricsIssuer},
			Subjects: user,
		},
		&corev1.ServiceAccount{APIVersion: "v1", Kind: "ServiceAccount", Name: metricsProxyAccount, Namespace: metav1.NamespaceSystem},
		// The proxies ask the API server who a client is and what it may do.
		&rbacv1.ClusterRoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding", Name: metricsProxyAccount,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:auth-delegator"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: metricsProxyAccount, Namespace: metav1.NamespaceSystem}},
		},
		metricsProxy(etcdMetrics, etcdMetricsPort, "http", &controlPlaneAffinity, proxy),
	}
	if addons.logsOn() {
		out = append(out, lokiClient(monitoringAddon, grafanaUser))
	}
	// k3s's kube-proxy, controller manager and scheduler are in the kubelet's metrics.
	switch distribution {
	case templates.K3s:
	case templates.Talos:
		out = append(out, metricsProxy(kubeProxyMetrics, kubeProxyMetricsPort, "http", nil, proxy))
		if addons.talosServing {
			out = append(out,
				metricsProxy(controllerMetrics, controllerMetricsPort, "https", &controlPlaneAffinity, proxy),
				metricsProxy(schedulerMetrics, schedulerMetricsPort, "https", &controlPlaneAffinity, proxy),
			)
		}
	case templates.Kubeadm, templates.RKE2:
		out = append(out,
			metricsProxy(kubeProxyMetrics, kubeProxyMetricsPort, "http", nil, proxy),
			metricsProxy(controllerMetrics, controllerMetricsPort, "https", &controlPlaneAffinity, proxy),
			metricsProxy(schedulerMetrics, schedulerMetricsPort, "https", &controlPlaneAffinity, proxy),
		)
	}
	// RKE2's CIS profile lets only kube-system's own pods reach the pods there, CoreDNS's metrics
	// among them. Only there: where no policy isolates CoreDNS, this one would, to its metrics.
	if distribution == templates.RKE2 && addons.CIS {
		tcp := corev1.ProtocolTCP
		out = append(out, &networkingv1.NetworkPolicy{
			APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy", Name: coreDNSMetricsPolicy, Namespace: metav1.NamespaceSystem,
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: monitoringAddon}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{appName: prometheus}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: new(intstr.FromInt32(coreDNSMetricsPort))}},
				}},
			},
		})
	}
	return out
}

func issuer(name string, spec map[string]any) map[string]any {
	return map[string]any{apiVersionKey: certManagerAPI, "kind": clusterIssuerKind, "metadata": map[string]any{"name": name}, "spec": spec}
}

// certificate is a cert-manager Certificate of a key of its own, in a Secret of its own name.
func certificate(namespace, name, issuer string, spec map[string]any) map[string]any {
	spec["secretName"] = name
	spec["privateKey"] = map[string]any{"algorithm": "ECDSA", "size": keyBits}
	spec["issuerRef"] = map[string]any{"kind": clusterIssuerKind, "name": issuer}
	return map[string]any{
		apiVersionKey: certManagerAPI, "kind": certificateKind,
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
// unauthenticated. It declares no port: the scheduler books a host network port for the whole node,
// so it would find the component's static pod, whose probes declare the same port on loopback, in
// the way. It opens no other port either: probes would need one of their own on the host. A
// component whose scheme is https is verified against the cluster CA, under the name its serving
// certificate carries on every distribution.
func metricsProxy(name string, port int32, scheme string, affinity *corev1.Affinity, image Image) *appsv1.DaemonSet {
	p := strconv.Itoa(int(port))
	labels := map[string]string{appName: name}
	upstream := []string{"--upstream=http://127.0.0.1:" + p + "/"}
	mounts := []corev1.VolumeMount{{Name: "tls", MountPath: "/etc/kube-rbac-proxy", ReadOnly: true}}
	volumes := []corev1.Volume{{Name: "tls", Secret: &corev1.SecretVolumeSource{SecretName: metricsProxyTLS}}}
	if scheme == schemeHTTPS {
		upstream = []string{"--upstream=https://localhost:" + p + "/", "--upstream-ca-file=/etc/kube-rbac-proxy-upstream/ca.crt"}
		mounts = append(mounts, corev1.VolumeMount{Name: "upstream", MountPath: "/etc/kube-rbac-proxy-upstream", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "upstream", ConfigMap: &corev1.ConfigMapVolumeSource{Name: "kube-root-ca.crt"}})
	}
	return &appsv1.DaemonSet{
		APIVersion: appsv1.SchemeGroupVersion.String(), Kind: kindDaemonSet,
		Name: name, Namespace: metav1.NamespaceSystem, Labels: labels,
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{Labels: labels, Spec: corev1.PodSpec{
				ServiceAccountName: metricsProxyAccount,
				HostNetwork:        true,
				PriorityClassName:  "system-node-critical",
				Affinity:           affinity,
				Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				SecurityContext:    restrictedPod(proxyUser),
				Containers: []corev1.Container{{
					Name:  "kube-rbac-proxy",
					Image: image.String(),
					Args: append([]string{
						"--secure-listen-address=$(HOST_IP):" + p,
						"--allow-paths=/metrics",
						"--client-ca-file=/etc/kube-rbac-proxy/ca.crt",
						"--tls-cert-file=/etc/kube-rbac-proxy/tls.crt",
						"--tls-private-key-file=/etc/kube-rbac-proxy/tls.key",
						"--tls-min-version=VersionTLS13",
					}, upstream...),
					Env:             []corev1.EnvVar{{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}}},
					SecurityContext: restrictedSecurity(),
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
					},
					VolumeMounts: mounts,
				}},
				Volumes: volumes,
			}},
		},
	}
}
