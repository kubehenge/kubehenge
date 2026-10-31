package provisioner

import (
	"errors"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/kubehenge/kubehenge/pkg/templates"
)

// Logs is a cluster's logs variable.
type Logs struct {
	Enabled bool `json:"enabled"`
	// Collector is node, Alloy on every node reading the host's logs, or api, one Alloy reading
	// the pods' logs through the Kubernetes API.
	Collector     string `json:"collector,omitempty"`
	RetentionDays int    `json:"retentionDays,omitempty"`
	SizeGiB       int    `json:"sizeGiB,omitempty"`
}

const (
	logsAddon = "logs"
	// The collectors a cluster chooses from.
	collectorNode = "node"
	collectorAPI  = "api"
	// The defaults the class's variable gives, for a variable written without them.
	defaultRetentionDays = 7
	defaultLogsSizeGiB   = 10

	// logsNamespace holds Loki and the Alloy that reads the API; logsNodeNamespace holds the Alloy
	// on every node apart from them, as only Pod Security's privileged level allows host paths.
	logsNamespace     = "logging"
	logsNodeNamespace = "logging-node"
	lokiRelease       = "loki"
	alloyRelease      = "alloy"
	alloyNodeRelease  = "alloy-node"

	// Who Alloy and Grafana are to Loki's proxy, by their client certificates.
	alloyUser   = "kubehenge:alloy"
	grafanaUser = "kubehenge:grafana"
	// lokiClientSecret holds a client's certificate, in Alloy's namespaces and in Grafana's.
	lokiClientSecret = "kubehenge-loki-client" //nolint:gosec // a Secret's name, not a credential
	lokiClientPath   = "/etc/kubehenge/loki-client"
	lokiClientVolume = "loki-client"
	lokiTLS          = "loki-tls"
	lokiStorage      = "loki-storage"
	lokiPort         = 3100
	lokiHost         = lokiRelease + "." + logsNamespace + ".svc"
	lokiURL          = "https://" + lokiHost + ":3100"

	// lokiStore keeps Loki's chunks, index and delete requests on its volume.
	lokiStore = "filesystem"
	// The ClusterRoles that let Alloy push to Loki, Grafana read it, and Alloy read the API.
	lokiPushRole = "kubehenge-loki-push"
	lokiReadRole = "kubehenge-loki-read"
	alloyRole    = "kubehenge-alloy"

	// The Alloy image's own unprivileged user, for the Alloy that needs nothing of the host.
	alloyImageUser = 473
	// alloyNodeStorage keeps how far each node's Alloy has read, so that a restart sends nothing twice.
	alloyNodeStorage = "/var/lib/alloy"
)

func (l Logs) collector() string {
	if l.Collector == "" {
		return collectorNode
	}
	return l.Collector
}

func (l Logs) retentionDays() int {
	if l.RetentionDays <= 0 {
		return defaultRetentionDays
	}
	return l.RetentionDays
}

func (l Logs) sizeGiB() int {
	if l.SizeGiB <= 0 {
		return defaultLogsSizeGiB
	}
	return l.SizeGiB
}

// logsOn reports whether the cluster gets Loki and Alloy: logs switched on beside monitoring,
// whose Grafana is where they are read.
func (a *Addons) logsOn() bool {
	return a.Logs.Enabled && a.Monitoring.Enabled
}

// checkLogs refuses logs without monitoring, whose Grafana is where they are read and whose CA
// Loki and its clients take their certificates from.
func checkLogs(cluster *clusterv1.Cluster) error {
	a, err := addonsOf(cluster)
	if err != nil || !a.Logs.Enabled || a.Monitoring.Enabled {
		return err
	}
	return errors.New("logs need monitoring. Switch monitoring on, or logs off")
}

// lokiProxy, alloyProxy and alloyNodeProxy name the cluster's logs proxies, which go again when
// logs are switched off; alloyNodeProxy goes too when the collector is the API.
func lokiProxy(cluster *clusterv1.Cluster) string      { return cluster.Name + "-" + lokiRelease }
func alloyProxy(cluster *clusterv1.Cluster) string     { return cluster.Name + "-" + alloyRelease }
func alloyNodeProxy(cluster *clusterv1.Cluster) string { return cluster.Name + "-" + alloyNodeRelease }

// logsOff names the logs proxies the cluster does not run.
func logsOff(cluster *clusterv1.Cluster, addons *Addons) []string {
	if !addons.logsOn() {
		return []string{lokiProxy(cluster), alloyProxy(cluster), alloyNodeProxy(cluster)}
	}
	if addons.Logs.collector() != collectorNode {
		return []string{alloyNodeProxy(cluster)}
	}
	return nil
}

// logsCharts are Loki, the Alloy in its namespace that reads the cluster's events, and the pods'
// logs through the API too with the api collector, and with the node collector, the Alloy on
// every node.
func logsCharts(charts *Charts, distribution templates.Distribution, logs Logs) []chartProxy {
	out := []chartProxy{
		{lokiRelease, lokiRelease, logsNamespace, charts.Loki, lokiValues(charts.KubeRBACProxy, logs)},
		{alloyRelease, alloyRelease, logsNamespace, charts.Alloy, alloyValues(charts.AlloyImage, logs.collector())},
	}
	if logs.collector() == collectorNode {
		out = append(out, chartProxy{alloyNodeRelease, alloyNodeRelease, logsNodeNamespace, charts.Alloy, alloyNodeValues(charts.AlloyImage, distribution)})
	}
	return out
}

// lokiValues run Loki as one process on a volume of its own, listening on loopback only, behind a
// kube-rbac-proxy that takes only clients it authenticates: Alloy may push and Grafana may read,
// each by its client certificate, and so may any user RBAC lets get or create those paths. Loki
// itself checks nobody, so it has one tenant. The volume is Kubehenge's rather than the
// StatefulSet's, so that it can grow, and stays when logs are switched off, as Prometheus's does.
// Loki's own Service, readiness and liveness go through the proxy, which lets the probes' two
// paths by unauthenticated.
func lokiValues(proxy Image, logs Logs) map[string]any {
	const (
		loopback = "127.0.0.1"
		grpcPort = "9095"
	)
	off := map[string]any{"enabled": false}
	probe := func(path string) map[string]any {
		return map[string]any{"httpGet": map[string]any{"path": path, "port": "http-metrics", "scheme": "HTTPS"}}
	}
	return map[string]any{
		"deploymentMode": "Monolithic",
		"loki": map[string]any{
			"auth_enabled": false,
			"server":       map[string]any{"http_listen_address": loopback, "grpc_listen_address": loopback},
			"commonConfig": map[string]any{
				"replication_factor":     1,
				"instance_addr":          loopback,
				"compactor_grpc_address": loopback + ":" + grpcPort,
				"ring":                   map[string]any{"kvstore": map[string]any{"store": "inmemory"}},
			},
			"memberlistConfig": map[string]any{"bind_addr": []any{loopback}},
			"frontend":         map[string]any{"address": loopback},
			"frontend_worker":  map[string]any{"frontend_address": loopback + ":" + grpcPort},
			"storage":          map[string]any{"type": lokiStore},
			"schemaConfig": map[string]any{"configs": []any{map[string]any{
				"from": "2024-04-01", "store": "tsdb", "object_store": lokiStore, "schema": "v13",
				"index": map[string]any{"prefix": "index_", "period": "24h"},
			}}},
			"limits_config":  map[string]any{"retention_period": strconv.Itoa(logs.retentionDays()) + "d"},
			"compactor":      map[string]any{"retention_enabled": true, "delete_request_store": lokiStore},
			"analytics":      map[string]any{"reporting_enabled": false},
			"readinessProbe": probe("/ready"),
			"livenessProbe":  probe("/loki/api/v1/status/buildinfo"),
		},
		"singleBinary": map[string]any{
			"replicas": 1,
			"persistence": map[string]any{
				"enabled":              false,
				"dataVolumeParameters": map[string]any{"persistentVolumeClaim": map[string]any{"claimName": lokiStorage}},
			},
			"podDisruptionBudget": off,
			"resources": map[string]any{
				requestsKey: map[string]any{cpuKey: "50m", memoryKey: "256Mi"},
				"limits":    map[string]any{memoryKey: "1Gi"},
			},
			"extraContainers": []any{lokiProxyContainer(proxy)},
			"extraVolumes":    []any{corev1.Volume{Name: "tls", Secret: &corev1.SecretVolumeSource{SecretName: lokiTLS}}},
		},
		// Rules come from no ConfigMaps; and the caches, the gateway, the canary and the scalable
		// targets belong to a Loki of many processes.
		"sidecar":           map[string]any{"rules": off},
		"gateway":           off,
		"chunksCache":       off,
		"resultsCache":      off,
		"memcachedExporter": off,
		"lokiCanary":        off,
		"test":              off,
		"ruler":             off,
		"backend":           map[string]any{"replicas": 0},
		"read":              map[string]any{"replicas": 0},
		"write":             map[string]any{"replicas": 0},
		"extraObjects":      lokiManifests(logs),
	}
}

// lokiProxyContainer is the kube-rbac-proxy beside Loki, on the pod's address and Loki's port.
func lokiProxyContainer(image Image) corev1.Container {
	port := strconv.Itoa(lokiPort)
	no, yes := false, true
	return corev1.Container{
		Name:  "kube-rbac-proxy",
		Image: image.String(),
		Args: []string{
			"--secure-listen-address=$(POD_IP):" + port,
			"--upstream=http://127.0.0.1:" + port + "/",
			"--ignore-paths=/ready,/loki/api/v1/status/buildinfo",
			"--client-ca-file=/etc/kube-rbac-proxy/ca.crt",
			"--tls-cert-file=/etc/kube-rbac-proxy/tls.crt",
			"--tls-private-key-file=/etc/kube-rbac-proxy/tls.key",
			"--tls-min-version=VersionTLS13",
		},
		Env: []corev1.EnvVar{{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}}},
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
	}
}

// lokiManifests are what Loki's chart does not bring: its volume, its proxy's certificate and
// rights, who may push and read, and a policy that lets only Alloy and Grafana reach it. The
// certificates come from monitoring's CA, which logs need beside them.
func lokiManifests(logs Logs) []any {
	tcp := corev1.ProtocolTCP
	user := func(name string) []rbacv1.Subject {
		return []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: name}}
	}
	from := func(namespace, name string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: namespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{appName: name}},
		}
	}
	return []any{
		&corev1.PersistentVolumeClaim{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: lokiStorage, Namespace: logsNamespace,
			// Helm leaves it when logs are switched off, and takes it up again when they are back.
			Annotations: map[string]string{"helm.sh/resource-policy": "keep"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(strconv.Itoa(logs.sizeGiB()) + "Gi"),
				}},
			},
		},
		certificate(logsNamespace, lokiTLS, metricsIssuer, serverCertificate(lokiHost)),
		&rbacv1.ClusterRoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding", Name: "kubehenge-loki-proxy",
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:auth-delegator"},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: lokiRelease, Namespace: logsNamespace}},
		},
		&rbacv1.ClusterRole{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole", Name: lokiPushRole,
			Rules: []rbacv1.PolicyRule{{NonResourceURLs: []string{"/loki/api/v1/push"}, Verbs: []string{"create"}}},
		},
		&rbacv1.ClusterRoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding", Name: lokiPushRole,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: lokiPushRole},
			Subjects: user(alloyUser),
		},
		&rbacv1.ClusterRole{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole", Name: lokiReadRole,
			Rules: []rbacv1.PolicyRule{{NonResourceURLs: []string{"/loki/*"}, Verbs: []string{"get"}}},
		},
		&rbacv1.ClusterRoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding", Name: lokiReadRole,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: lokiReadRole},
			Subjects: user(grafanaUser),
		},
		// The kubelet's probes come from the node, which a NetworkPolicy always lets through.
		&networkingv1.NetworkPolicy{
			APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy", Name: lokiRelease, Namespace: logsNamespace,
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{appName: lokiRelease, "app.kubernetes.io/component": "single-binary"}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{
						from(logsNamespace, alloyRelease), from(logsNodeNamespace, alloyRelease), from(monitoringAddon, "grafana"),
					},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: new(intstr.FromInt32(lokiPort))}},
				}},
			},
		},
	}
}

// lokiClient is the client certificate a namespace's Alloy or Grafana shows Loki's proxy.
func lokiClient(namespace, user string) map[string]any {
	return certificate(namespace, lokiClientSecret, metricsIssuer, clientCertificate(user))
}

// lokiDataSource is Loki in monitoring's Grafana, reached with Grafana's client certificate,
// which grafanaLokiMount puts where the data source reads it.
func lokiDataSource() map[string]any {
	file := func(name string) string { return "$__file{" + lokiClientPath + "/" + name + "}" }
	return map[string]any{
		"name": "Loki", "type": "loki", "uid": lokiRelease, "access": "proxy", "url": lokiURL,
		"jsonData":       map[string]any{"tlsAuth": true, "tlsAuthWithCACert": true, "serverName": lokiHost},
		"secureJsonData": map[string]any{"tlsCACert": file("ca.crt"), "tlsClientCert": file("tls.crt"), "tlsClientKey": file("tls.key")},
	}
}

func grafanaLokiMount() map[string]any {
	return map[string]any{"name": lokiClientVolume, "secretName": lokiClientSecret, "mountPath": lokiClientPath, "readOnly": true}
}

// alloyWrite is where every Alloy sends what it collects.
const alloyWrite = `loki.write "loki" {
  endpoint {
    url = "` + lokiURL + `/loki/api/v1/push"
    tls_config {
      ca_file     = "` + lokiClientPath + `/ca.crt"
      cert_file   = "` + lokiClientPath + `/tls.crt"
      key_file    = "` + lokiClientPath + `/tls.key"
      min_version = "TLS13"
    }
  }
}
`

// alloyEvents reads the cluster's events, which outlive no hour in the API.
const alloyEvents = `loki.source.kubernetes_events "events" {
  job_name   = "events"
  forward_to = [loki.write.loki.receiver]
}
`

// alloyAPIPods reads every container's log through the API, labelled as alloyNodePods labels
// them. Discovery gives a container once for each port, and the tailer takes each container once.
const alloyAPIPods = `discovery.kubernetes "pods" {
  role = "pod"
}

discovery.relabel "pods" {
  targets = discovery.kubernetes.pods.targets
  rule {
    source_labels = ["__meta_kubernetes_namespace"]
    target_label  = "namespace"
  }
  rule {
    source_labels = ["__meta_kubernetes_pod_name"]
    target_label  = "pod"
  }
  rule {
    source_labels = ["__meta_kubernetes_pod_container_name"]
    target_label  = "container"
  }
  rule {
    source_labels = ["__meta_kubernetes_pod_node_name"]
    target_label  = "node"
  }
  rule {
    target_label = "job"
    replacement  = "pods"
  }
}

loki.source.kubernetes "pods" {
  targets    = discovery.relabel.pods.output
  forward_to = [loki.write.loki.receiver]
}
`

// alloyNodePods reads the node's container logs from the files the kubelet writes, those of pods
// gone again within seconds too; the path names the namespace, the pod and the container.
const alloyNodePods = `local.file_match "pods" {
  path_targets = [{"__path__" = "/var/log/pods/*/*/*.log", "job" = "pods", "node" = sys.env("K8S_NODE_NAME")}]
  sync_period  = "5s"
}

discovery.relabel "pods" {
  targets = local.file_match.pods.targets
  rule {
    source_labels = ["__path__"]
    regex         = "/var/log/pods/([^_]+)_([^_]+)_[^/]+/([^/]+)/.+"
    replacement   = "$1"
    target_label  = "namespace"
  }
  rule {
    source_labels = ["__path__"]
    regex         = "/var/log/pods/([^_]+)_([^_]+)_[^/]+/([^/]+)/.+"
    replacement   = "$2"
    target_label  = "pod"
  }
  rule {
    source_labels = ["__path__"]
    regex         = "/var/log/pods/([^_]+)_([^_]+)_[^/]+/([^/]+)/.+"
    replacement   = "$3"
    target_label  = "container"
  }
}

loki.source.file "pods" {
  targets    = discovery.relabel.pods.output
  forward_to = [loki.process.pods.receiver]
}

loki.process "pods" {
  stage.cri {}
  stage.label_drop {
    values = ["filename"]
  }
  forward_to = [loki.write.loki.receiver]
}

local.file_match "audit" {
  path_targets = [{"__path__" = "AUDIT_LOG", "job" = "audit", "node" = sys.env("K8S_NODE_NAME")}]
}

// From its end: the log goes back a month, which would arrive as of now.
loki.source.file "audit" {
  targets       = local.file_match.audit.targets
  tail_from_end = true
  forward_to    = [loki.process.audit.receiver]
}

loki.process "audit" {
  stage.json {
    expressions = {"ts" = "stageTimestamp"}
  }
  stage.timestamp {
    source = "ts"
    format = "RFC3339Nano"
  }
  stage.label_drop {
    values = ["filename"]
  }
  forward_to = [loki.write.loki.receiver]
}
`

// talosLogsPort is where the node's Alloy takes the logs of Talos's services and kernel, on the
// node's own link-local address the Talos ClusterClass names as its log destination
// (kubehenge.talosCommonPatches).
const talosLogsPort = 5170

// The keys of a port in charts' values and Gateway listeners.
const (
	hostPortKey = "hostPort"
	protocolKey = "protocol"
)

// alloyTalos takes in what a Talos node's services and kernel log, which Talos keeps in memory
// alone and sends as JSON lines to its log destination, and labels it as alloyJournal labels the
// journal: the service as the unit, and the kernel's as kernel. otelcol.receiver.tcplog is the one
// component of Alloy that reads lines off TCP; the OTLP log it makes carries the line as its body.
const alloyTalos = `otelcol.receiver.tcplog "talos" {
  listen_address = "0.0.0.0:TALOS_LOGS_PORT"
  output {
    logs = [otelcol.exporter.loki.talos.input]
  }
}

otelcol.exporter.loki "talos" {
  forward_to = [loki.process.talos.receiver]
}

loki.process "talos" {
  stage.json {
    expressions = {"event" = "body"}
  }
  stage.json {
    source      = "event"
    expressions = {"msg" = "msg", "unit" = "\"talos-service\"", "facility" = "facility", "level" = "\"talos-level\"", "time" = "\"talos-time\""}
  }
  stage.template {
    source   = "unit"
    template = "{{ if .unit }}{{ .unit }}{{ else if .facility }}kernel{{ end }}"
  }
  stage.timestamp {
    source = "time"
    format = "RFC3339Nano"
  }
  stage.label_drop {
    values = ["exporter"]
  }
  stage.labels {
    values = {"unit" = "", "level" = ""}
  }
  stage.static_labels {
    values = {"job" = "talos", "node" = sys.env("K8S_NODE_NAME")}
  }
  stage.output {
    source = "msg"
  }
  forward_to = [loki.write.loki.receiver]
}
`

// alloyJournal reads the node's journal, which every distribution but Talos keeps (alloyTalos).
const alloyJournal = `loki.relabel "journal" {
  forward_to = []
  rule {
    source_labels = ["__journal__systemd_unit"]
    target_label  = "unit"
  }
}

loki.source.journal "journal" {
  path          = "/var/log/journal"
  labels        = {"job" = "journal", "node" = sys.env("K8S_NODE_NAME")}
  relabel_rules = loki.relabel.journal.rules
  forward_to    = [loki.write.loki.receiver]
}
`

// auditLog is the directory the distribution's API server writes its audit log into, on the
// control planes, and the log's name there.
func auditLog(distribution templates.Distribution) (string, string) {
	const name = "audit.log"
	switch distribution {
	case templates.RKE2:
		return "/var/lib/rancher/rke2/server/logs", name
	case templates.K3s:
		return "/var/lib/rancher/k3s/server/logs", name
	case templates.Talos:
		return "/var/log/audit/kube", "kube-apiserver.log"
	case templates.Kubeadm:
	}
	return "/var/log/kubernetes/audit", name
}

// alloyCommon are the values of both Alloys: the image by digest alone, since the chart puts its
// tag into a label; the configuration in a ConfigMap whose checksum restarts Alloy when it changes,
// so without a reloader beside it; its interface on loopback only; no Service, CRDs or reporting.
// The chart's own rules read far more than either Alloy does, Secrets among them. The second map
// is the values' alloy section, which each Alloy completes.
func alloyCommon(image Image, config, namespace string, objects ...any) (map[string]any, map[string]any) {
	off := map[string]any{"enabled": false}
	alloy := map[string]any{
		// The chart renders the content as a Helm template, which would empty Alloy's own templates.
		"configMap":            map[string]any{"content": strings.ReplaceAll(config, "{{", `{{"{{"}}`)},
		"listenAddr":           "127.0.0.1",
		"enableHttpServerPort": false,
		"enableReporting":      false,
	}
	values := map[string]any{
		"image":          map[string]any{"registry": image.Registry, "repository": image.Repository, "digest": "sha256:" + image.Digest},
		"crds":           map[string]any{"create": false},
		"configReloader": off,
		"service":        off,
		"rbac":           map[string]any{"create": false},
		"alloy":          alloy,
		"extraObjects":   append([]any{lokiClient(namespace, alloyUser)}, objects...),
	}
	return values, alloy
}

// talosLogsPolicy lets no one reach the node Alloys on Talos, whose host port takes the node's logs
// from any address: a NetworkPolicy always lets the pod's own node through, which is where Talos
// sends from, so another node or pod cannot pass lines off as the node's.
func talosLogsPolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy", Name: alloyNodeRelease, Namespace: logsNodeNamespace,
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{appName: alloyRelease}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
}

// clientVolume mounts a namespace's Loki client certificate.
func clientVolume() (corev1.Volume, corev1.VolumeMount) {
	return corev1.Volume{Name: lokiClientVolume, Secret: &corev1.SecretVolumeSource{SecretName: lokiClientSecret}},
		corev1.VolumeMount{Name: lokiClientVolume, MountPath: lokiClientPath, ReadOnly: true}
}

// alloyValues run the Alloy in Loki's namespace, unprivileged and without anything of the host:
// one replica that reads the cluster's events, and with the api collector every pod's log too,
// with only the rights to read those.
func alloyValues(image Image, collector string) map[string]any {
	config := alloyEvents + "\n" + alloyWrite
	rules := []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"get", "list", "watch"}}}
	if collector == collectorAPI {
		config = alloyAPIPods + "\n" + config
		rules = append(rules,
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		)
	}
	values, alloy := alloyCommon(image, config, logsNamespace,
		&rbacv1.ClusterRole{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole", Name: alloyRole, Rules: rules},
		&rbacv1.ClusterRoleBinding{
			APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding", Name: alloyRole,
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: alloyRole},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: alloyRelease, Namespace: logsNamespace}},
		},
	)
	user, no, yes := int64(alloyImageUser), false, true
	client, clientMount := clientVolume()
	alloy["securityContext"] = corev1.SecurityContext{
		RunAsNonRoot: &yes, RunAsUser: &user, RunAsGroup: &user,
		AllowPrivilegeEscalation: &no,
		ReadOnlyRootFilesystem:   &yes,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	alloy["resources"] = map[string]any{
		requestsKey: map[string]any{cpuKey: "10m", memoryKey: "64Mi"},
		"limits":    map[string]any{memoryKey: "256Mi"},
	}
	alloy["mounts"] = map[string]any{"extra": []any{corev1.VolumeMount{Name: "storage", MountPath: "/tmp/alloy"}, clientMount}}
	values[controllerKey] = map[string]any{
		"type": "deployment", "replicas": 1,
		"volumes": map[string]any{"extra": []any{
			corev1.Volume{Name: "storage", EmptyDir: &corev1.EmptyDirVolumeSource{}}, client,
		}},
	}
	return values
}

// alloyNodeValues run Alloy on every node, the control planes too, reading the node's container
// logs, its journal or on Talos what Talos sends it and, on the control planes, the API server's audit log. It runs as root to read
// those, which root owns, but with no capability but Talos's one, so it reads nothing else root's
// alone, and mounts each read-only; it writes only where it keeps how far it has read. It needs nothing of the
// API, so it gets no token. On workers, which have no audit log, its directory is an empty one.
func alloyNodeValues(image Image, distribution templates.Distribution) map[string]any {
	audit, auditFile := auditLog(distribution)
	config := strings.Replace(alloyNodePods, "AUDIT_LOG", audit+"/"+auditFile, 1)
	journal := distribution != templates.Talos
	var objects []any
	if journal {
		config += "\n" + alloyJournal
	} else {
		config += "\n" + strings.Replace(alloyTalos, "TALOS_LOGS_PORT", strconv.Itoa(talosLogsPort), 1)
		objects = append(objects, talosLogsPolicy())
	}
	values, alloy := alloyCommon(image, config+"\n"+alloyWrite, logsNodeNamespace, objects...)
	if !journal {
		// Talos dials the port on an address of its node's own, which the CNI's portmap plugin
		// carries to the pod on that node. otelcol.receiver.tcplog is still experimental in Alloy.
		alloy["stabilityLevel"] = "experimental"
		alloy["extraPorts"] = []any{map[string]any{
			"name": "talos-logs", "port": talosLogsPort, "targetPort": talosLogsPort, hostPortKey: talosLogsPort, protocolKey: "TCP",
		}}
	}
	root, no, yes := int64(0), false, true
	client, clientMount := clientVolume()
	host := func(name, path string, kind corev1.HostPathType) corev1.Volume {
		return corev1.Volume{Name: name, HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &kind}}
	}
	alloy["storagePath"] = alloyNodeStorage
	capabilities := &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	if distribution == templates.Talos {
		// Talos keeps the audit log in a directory only the API server's own user may enter.
		// Reading past that is all the capability grants, and only in what is mounted here.
		capabilities.Add = []corev1.Capability{"DAC_READ_SEARCH"}
	}
	alloy["securityContext"] = corev1.SecurityContext{
		RunAsNonRoot: &no, RunAsUser: &root, RunAsGroup: &root,
		AllowPrivilegeEscalation: &no,
		ReadOnlyRootFilesystem:   &yes,
		Capabilities:             capabilities,
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		SELinuxOptions:           hostSELinux(),
	}
	alloy["resources"] = map[string]any{
		requestsKey: map[string]any{cpuKey: "10m", memoryKey: "128Mi"},
		"limits":    map[string]any{memoryKey: "512Mi"},
	}
	mounts := []any{
		corev1.VolumeMount{Name: "pods", MountPath: "/var/log/pods", ReadOnly: true},
		corev1.VolumeMount{Name: "audit", MountPath: audit, ReadOnly: true},
		corev1.VolumeMount{Name: "storage", MountPath: alloyNodeStorage},
		clientMount,
	}
	volumes := []any{
		host("pods", "/var/log/pods", corev1.HostPathDirectory),
		host("audit", audit, corev1.HostPathDirectoryOrCreate),
		host("storage", alloyNodeStorage, corev1.HostPathDirectoryOrCreate),
		client,
	}
	if journal {
		mounts = append(mounts, corev1.VolumeMount{Name: "journal", MountPath: "/var/log/journal", ReadOnly: true})
		volumes = append(volumes, host("journal", "/var/log/journal", corev1.HostPathDirectory))
	}
	alloy["mounts"] = map[string]any{"extra": mounts}
	values[controllerKey] = map[string]any{
		"type":         "daemonset",
		tolerationsKey: []any{corev1.Toleration{Operator: corev1.TolerationOpExists}},
		"volumes":      map[string]any{"extra": volumes},
	}
	values["serviceAccount"] = map[string]any{"automountServiceAccountToken": false}
	return values
}
