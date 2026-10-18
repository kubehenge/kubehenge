{{- /*
Rendered by Cluster API, not Helm, with the cluster's variables and $exempt, the namespaces the
chart exempts: the podSecurity variable's admission level becomes Pod Security Admission's default
for every namespace that labels none of its own. Below restricted, pods short of restricted are
warned about and audited; privileged alone is Kubernetes' own default, with nothing reported.
*/ -}}
{{- $level := "privileged" }}
{{- with .podSecurity }}{{ with .admission }}{{ $level = . }}{{ end }}{{ end }}
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
owner: root:root
permissions: "0600"
content: {{ dict "apiVersion" "apiserver.config.k8s.io/v1" "kind" "AdmissionConfiguration" "plugins" (list (dict "name" "PodSecurity" "configuration" $podSecurity)) | toJson | toJson }}
