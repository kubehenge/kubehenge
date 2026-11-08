{{/*
The site settings of a cluster's nodes, for a network that reaches few hosts: registry mirrors, extra
trusted authorities and a proxy (the egressProxy variable, which the add-ons share), beside the
network variable's ntp. Every distribution and node OS takes them, and changing one replaces the
nodes, one at a time, as any node file does (the node templates change).

The webhook (pkg/provisioner/nodesite.go) holds the authorities to PEM certificates that have not
expired and a registry to one entry. A rule on a variable that is a list is refused by Cluster API, so
the schema carries none for the lists. The schemas below keep every value to characters a shell, YAML and a systemd environment file
read the same way, since the values reach the nodes' boot script.
*/}}

{{/* A PEM bundle: certificates only, base64 lines with no carriage returns. */}}
{{- define "kubehenge.pemSchema" -}}
type: string
minLength: 64
maxLength: 65536
pattern: '^(-----BEGIN CERTIFICATE-----\n([A-Za-z0-9+/=]+\n)+-----END CERTIFICATE-----\n?)+$'
{{- end -}}

{{- define "kubehenge.nodeSiteVariables" -}}
- name: registryMirrors
  required: false
  schema:
    openAPIV3Schema:
      type: array
      maxItems: 16
      description: >-
        Mirrors the nodes pull images from, per registry, tried in order before the registry itself.
        An address may end in /v2 to be used as given, as a registry's proxy project needs.
      items:
        type: object
        required: [registry, endpoints]
        properties:
          registry:
            type: string
            maxLength: 253
            pattern: '^(\*|[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:[0-9]{1,5})?)$'
            description: The registry's host name, with its port if it has one, or * for every registry.
          endpoints:
            type: array
            minItems: 1
            maxItems: 4
            items:
              type: string
              maxLength: 512
              pattern: '^https?://[A-Za-z0-9]([-A-Za-z0-9.]*[A-Za-z0-9])?(:[0-9]{1,5})?(/([A-Za-z0-9._-]+/)*v2)?$'
          caBundle:
            description: The authority that signed the mirrors' certificates, as PEM.
            {{- include "kubehenge.pemSchema" . | nindent 12 }}
- name: trustedCAs
  required: false
  schema:
    openAPIV3Schema:
      type: array
      maxItems: 8
      description: >-
        Authorities the nodes trust besides their OS's own, as PEM bundles. They go into the node
        OS's trust store, which the container runtime and the distribution read.
      items:
        {{- include "kubehenge.pemSchema" . | nindent 8 }}
{{- end -}}

{{/*
A Cluster API template that sets $noProxy, the exceptions of the proxy on a node (call with "pods" and
"services", the distribution's own ranges, for a Cluster that sets none): the cluster's pod, service
and node networks in each of its address families, its API address, its own names and the
exceptions the cluster lists.
*/}}
{{- define "kubehenge.nodeNoProxy" -}}
{{ printf "{{ $pods := list %q }}{{ $services := list %q }}" .pods .services }}
{{ `{{ with (dig "builtin" "cluster" "network" "pods" (list) .) }}{{ $pods = . }}{{ end }}
{{ with (dig "builtin" "cluster" "network" "services" (list) .) }}{{ $services = . }}{{ end }}
{{ $nodes := list }}
{{ with (dig "network" "gateway" "" .) }}{{ $nodes = append $nodes (printf "%s/%d" . (int (dig "network" "prefix" 24 $))) }}{{ end }}
{{ with (dig "network" "ipv6" "gateway" "" .) }}{{ $nodes = append $nodes (printf "%s/%d" . (int (dig "network" "ipv6" "prefix" 64 $))) }}{{ end }}
{{ $noProxy := concat (list "localhost" "127.0.0.1" "::1" ".svc" ".cluster.local" (dig "controlPlaneEndpoint" "" .)) $nodes $pods $services (dig "egressProxy" "noProxy" (list) .) | uniq | join "," }}` }}
{{- end -}}

{{/* The boot script of a kubeadm, RKE2 or k3s node, as the file Cluster API writes (call with the distribution and its ranges). */}}
{{- define "kubehenge.nodeSiteFile" -}}
{{ `{{ $mirrors := dig "registryMirrors" (list) . }}{{ $cas := dig "trustedCAs" (list) . }}{{ $url := dig "egressProxy" "url" "" . }}{{ $https := dig "egressProxy" "httpsUrl" $url . }}` }}
{{ include "kubehenge.nodeNoProxy" . }}
path: /etc/kubehenge/node-site.sh
owner: root:root
permissions: "0700"
content: |
{{ $.root.Files.Get "files/node-site.sh.tpl" | replace "@DISTRIBUTION@" .distribution | indent 2 }}
{{- end -}}

{{/* The enabledIf of the patches: any of the three settings is set. */}}
{{- define "kubehenge.nodeSiteEnabled" -}}
{{ `{{ if or (dig "trustedCAs" (list) .) (dig "registryMirrors" (list) .) (dig "egressProxy" "url" "" .) }}true{{ end }}` | quote }}
{{- end -}}

{{- define "kubehenge.nodeProxyEnabled" -}}
{{ `{{ if dig "egressProxy" "url" "" . }}true{{ end }}` | quote }}
{{- end -}}

{{/*
Gives a kubeadm, RKE2 or k3s node its site settings (call with "distribution", "root", "pods" and
"services"). The boot script writes the trust store, the mirrors' configuration and the proxy's
environment. The proxy's environment is read into the boot's own shell by the next command, so that the
distribution's installer and the package manager go through the proxy, and the distribution's service
reads it from its environment file.
*/}}
{{- define "kubehenge.nodeSitePatches" -}}
{{- $targets := dict
  "kubeadm" (dict "controlPlane" "KubeadmControlPlaneTemplate" "worker" "KubeadmConfigTemplate" "spec" "/spec/template/spec/kubeadmConfigSpec" "commands" "preKubeadmCommands")
  "rke2" (dict "controlPlane" "RKE2ControlPlaneTemplate" "worker" "RKE2ConfigTemplate" "spec" "/spec/template/spec" "commands" "preRKE2Commands")
  "k3s" (dict "controlPlane" "KThreesControlPlaneTemplate" "worker" "KThreesConfigTemplate" "spec" "/spec/template/spec/kthreesConfigSpec" "commands" "preK3sCommands") -}}
{{- $target := get $targets .distribution -}}
{{- $api := dict "controlPlane" "controlplane.cluster.x-k8s.io/v1beta2" "worker" "bootstrap.cluster.x-k8s.io/v1beta2" -}}
{{- $selector := dict "controlPlane" (dict "controlPlane" true) "worker" (dict "machineDeploymentClass" (dict "names" (list "worker"))) -}}
{{- $file := include "kubehenge.nodeSiteFile" . -}}
- name: {{ .distribution }}-node-site
  enabledIf: {{ include "kubehenge.nodeSiteEnabled" . }}
  definitions:
    {{- range $role := list "controlPlane" "worker" }}
    {{- $spec := ternary $target.spec "/spec/template/spec" (eq $role "controlPlane") }}
    - selector:
        apiVersion: {{ get $api $role }}
        kind: {{ get $target $role }}
        matchResources:
          {{- toYaml (get $selector $role) | nindent 10 }}
      jsonPatches:
        - op: add
          path: {{ $spec }}/files/-
          valueFrom:
            template: |
              {{- $file | nindent 14 }}
        - op: add
          path: {{ $spec }}/{{ $target.commands }}/-
          value: /etc/kubehenge/node-site.sh || exit 1
    {{- end }}
- name: {{ .distribution }}-node-proxy
  enabledIf: {{ include "kubehenge.nodeProxyEnabled" . }}
  definitions:
    {{- range $role := list "controlPlane" "worker" }}
    {{- $spec := ternary $target.spec "/spec/template/spec" (eq $role "controlPlane") }}
    - selector:
        apiVersion: {{ get $api $role }}
        kind: {{ get $target $role }}
        matchResources:
          {{- toYaml (get $selector $role) | nindent 10 }}
      jsonPatches:
        - op: add
          path: {{ $spec }}/{{ $target.commands }}/-
          value: set -a; . /etc/kubehenge/site/proxy.env; set +a
    {{- end }}
{{- end -}}

{{/*
Talos's site settings as the strategic patches the Talos bootstrap provider applies (call with "pods",
"services" and the "selectors" of the templates). The mirrors, their authorities and the trusted
authorities are Talos's own documents. The proxy is machine.env, which Talos's services and its
container runtime read.
*/}}
{{- define "kubehenge.talosNodeSitePatches" -}}
- name: talos-node-proxy
  enabledIf: {{ include "kubehenge.nodeProxyEnabled" . }}
  definitions:
    {{- range $sel := .selectors }}
    - selector:
        apiVersion: {{ $sel.api }}
        kind: {{ $sel.kind }}
        matchResources:
          {{- toYaml $sel.match | nindent 10 }}
      jsonPatches:
        - op: add
          path: {{ $sel.path }}
          valueFrom:
            template: |
              {{- include "kubehenge.talosProxyEnv" $ | nindent 14 }}
    {{- end }}
- name: talos-node-registries
  enabledIf: {{ `{{ if or (dig "trustedCAs" (list) .) (dig "registryMirrors" (list) .) }}true{{ end }}` | quote }}
  definitions:
    {{- range $sel := .selectors }}
    - selector:
        apiVersion: {{ $sel.api }}
        kind: {{ $sel.kind }}
        matchResources:
          {{- toYaml $sel.match | nindent 10 }}
      jsonPatches:
        - op: add
          path: {{ $sel.path }}
          valueFrom:
            template: {{ include "kubehenge.talosRegistryDocs" . | quote }}
    {{- end }}
{{- end -}}

{{- define "kubehenge.talosProxyEnv" -}}
{{ `{{ $url := dig "egressProxy" "url" "" . }}{{ $https := dig "egressProxy" "httpsUrl" $url . }}` }}
{{ include "kubehenge.nodeNoProxy" . }}
{{ `{{ dict "machine" (dict "env" (dict "HTTP_PROXY" $url "HTTPS_PROXY" $https "NO_PROXY" $noProxy)) | toJson | toJson }}` }}
{{- end -}}

{{- define "kubehenge.talosRegistryDocs" -}}
{{ `{{ $docs := list }}{{ $seen := dict }}
{{ range $m := (dig "registryMirrors" (list) .) }}{{ $eps := list }}{{ range $e := $m.endpoints }}{{ $ep := dict "url" $e }}{{ if (urlParse $e).path }}{{ $_ := set $ep "overridePath" true }}{{ end }}{{ $eps = append $eps $ep }}{{ end }}
{{ $docs = append $docs (dict "apiVersion" "v1alpha1" "kind" "RegistryMirrorConfig" "name" $m.registry "endpoints" $eps | toJson) }}
{{ $ca := dig "caBundle" "" $m }}{{ if $ca }}{{ range $e := $m.endpoints }}{{ $h := (urlParse $e).host }}{{ if not (hasKey $seen $h) }}{{ $_ := set $seen $h true }}{{ $docs = append $docs (dict "apiVersion" "v1alpha1" "kind" "RegistryTLSConfig" "name" $h "ca" $ca | toJson) }}{{ end }}{{ end }}{{ end }}{{ end }}
{{ with (dig "trustedCAs" (list) .) }}{{ $docs = append $docs (dict "apiVersion" "v1alpha1" "kind" "TrustedRootsConfig" "name" "kubehenge" "certificates" (join "\n" .) | toJson) }}{{ end }}
{{ join "\n---\n" $docs | toJson }}` }}
{{- end -}}
