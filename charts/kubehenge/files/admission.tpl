{{- /*
Rendered by Cluster API, not Helm, with the cluster's variables and $exempt, the namespaces the
chart exempts: the API server's admission configuration, as a bootstrap file or, with $talos, as
one Talos document per plugin in a single strategic patch.

PodSecurity: the podSecurity variable's admission level becomes Pod Security Admission's default
for every namespace that labels none of its own, and restricted on a cluster hardened to the CIS
benchmark, as the RKE2 and k3s hardening guides have it. Below restricted, pods short of
restricted are warned about and audited; privileged alone is Kubernetes' own default, with nothing
reported.

EventRateLimit: a namespace, or the cluster as a whole, that writes events faster than this has the
rest refused, so that no workload can flood etcd with them. The limits sit well above what a large
rollout writes.
*/ -}}
{{- $level := "privileged" }}
{{- with .podSecurity }}{{ with .admission }}{{ $level = . }}{{ end }}{{ end }}
{{- if .cis }}{{ $level = "restricted" }}{{ end }}
{{- $defaults := dict "enforce" $level "enforce-version" "latest" }}
{{- if ne $level "privileged" }}
{{- $_ := set $defaults "audit" "restricted" }}
{{- $_ = set $defaults "audit-version" "latest" }}
{{- $_ = set $defaults "warn" "restricted" }}
{{- $_ = set $defaults "warn-version" "latest" }}
{{- end }}
{{- $podSecurity := dict
  "apiVersion" "pod-security.admission.config.k8s.io/v1" "kind" "PodSecurityConfiguration"
  "defaults" $defaults "exemptions" (dict "namespaces" $exempt) }}
{{- $eventRateLimit := dict
  "apiVersion" "eventratelimit.admission.k8s.io/v1alpha1" "kind" "Configuration"
  "limits" (list
    (dict "type" "Server" "qps" 500 "burst" 2000)
    (dict "type" "Namespace" "qps" 50 "burst" 500 "cacheSize" 4096)) }}
{{- $plugins := list
  (dict "name" "PodSecurity" "configuration" $podSecurity)
  (dict "name" "EventRateLimit" "configuration" $eventRateLimit) }}
{{- if $talos }}
{{- $docs := list }}
{{- range $plugins }}
{{- $docs = append $docs (dict "apiVersion" "v1alpha1" "kind" "KubeAdmissionControlConfig" "name" .name "configuration" .configuration | toJson) }}
{{- end }}
{{ join "\n---\n" $docs | toJson }}
{{- else }}
owner: root:root
permissions: "0600"
content: {{ dict "apiVersion" "apiserver.config.k8s.io/v1" "kind" "AdmissionConfiguration" "plugins" $plugins | toJson | toJson }}
{{- end }}
