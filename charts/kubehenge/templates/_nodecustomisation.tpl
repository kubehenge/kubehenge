{{/*
The nodeCustomisation variable: what a company adds to the nodes of a cluster. Files, boot commands,
extra kubelet and control-plane arguments, an audit policy and an audit webhook. Every distribution
takes it through class patches that come after Kubehenge's own, so Kubehenge's security settings
stay, and the webhook (pkg/provisioner/nodecustomisation.go) refuses paths and flags Kubehenge sets.
The lists are capped because a patch adds one element, so there is one patch per index.
*/}}
{{- define "kubehenge.nodeCustomisationMax" -}}10{{- end -}}

{{- define "kubehenge.nodeRolesSchema" -}}
type: array
minItems: 1
maxItems: 2
default: [controlPlane, worker]
description: The nodes it goes to, control-plane nodes, workers or both.
items:
  type: string
  enum: [controlPlane, worker]
{{- end -}}

{{- define "kubehenge.nodeArgsSchema" -}}
type: array
maxItems: {{ include "kubehenge.nodeCustomisationMax" . }}
description: {{ .description | quote }}
x-kubernetes-validations:
  - rule: self.all(a, self.exists_one(b, a.name == b.name))
    message: Each argument once.
items:
  type: object
  required: [name]
  properties:
    name:
      type: string
      minLength: 1
      maxLength: 63
      pattern: "^[a-z][a-z0-9-]*$"
    value:
      type: string
      default: ""
      maxLength: 1024
{{- end -}}

{{- define "kubehenge.nodeCustomisationVariable" -}}
{{- $max := include "kubehenge.nodeCustomisationMax" . | int }}
{{- $dns := "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" }}
- name: nodeCustomisation
  required: false
  schema:
    openAPIV3Schema:
      type: object
      default: {}
      description: >-
        What a company adds to the nodes: files, commands before the distribution starts, kubelet
        and control-plane arguments, an audit policy and an audit webhook. Kubehenge's own files and
        arguments stay and cannot be set here. Changing anything replaces every node it reaches,
        one at a time.
      properties:
        files:
          type: array
          maxItems: {{ $max }}
          description: Files written to the nodes before the distribution starts.
          x-kubernetes-validations:
            - rule: self.all(a, self.exists_one(b, a.path == b.path))
              message: Each path once.
          items:
            type: object
            required: [path]
            properties:
              path:
                type: string
                minLength: 2
                maxLength: 255
                pattern: {{ "^/[A-Za-z0-9._@%+=:,/-]+$" | quote }}
              permissions:
                type: string
                default: "0644"
                enum: ["0400", "0440", "0444", "0600", "0640", "0644", "0700", "0750", "0755"]
              content:
                type: string
                maxLength: 16384
                description: The file's text. Not for secrets.
              secret:
                type: object
                description: A Secret in the cluster's namespace whose key is the file's content.
                required: [name, key]
                properties:
                  name:
                    type: string
                    minLength: 1
                    maxLength: 253
                    pattern: {{ $dns | quote }}
                  key:
                    type: string
                    minLength: 1
                    maxLength: 253
                    pattern: "^[-._a-zA-Z0-9]+$"
              roles:
                {{- include "kubehenge.nodeRolesSchema" . | nindent 16 }}
            x-kubernetes-validations:
              - rule: has(self.content) != has(self.secret)
                message: Give the content or a secret, not both.
              - rule: {{ "!self.path.contains('//') && !self.path.endsWith('/')" | quote }}
                message: A path names a file, not a directory.
              - rule: {{ "!self.path.matches('(^|/)[.][.]?(/|$)')" | quote }}
                message: A path has no . or .. part.
        commands:
          type: array
          maxItems: {{ $max }}
          description: Commands that run, in this order, before the distribution starts.
          items:
            type: object
            required: [run]
            properties:
              run:
                type: string
                minLength: 1
                maxLength: 1024
              roles:
                {{- include "kubehenge.nodeRolesSchema" . | nindent 16 }}
        kubeletArgs:
          {{- include "kubehenge.nodeArgsSchema" (dict "description" "Extra kubelet arguments, on every node.") | nindent 10 }}
        apiServerArgs:
          {{- include "kubehenge.nodeArgsSchema" (dict "description" "Extra API server arguments.") | nindent 10 }}
        controllerManagerArgs:
          {{- include "kubehenge.nodeArgsSchema" (dict "description" "Extra controller manager arguments.") | nindent 10 }}
        schedulerArgs:
          {{- include "kubehenge.nodeArgsSchema" (dict "description" "Extra scheduler arguments.") | nindent 10 }}
        auditPolicy:
          type: object
          description: >-
            The API server's audit policy, in place of Kubehenge's. A ConfigMap in the cluster's
            namespace holds it under the key policy.yaml, or the policy is given here. Talos takes
            only the policy given here.
          properties:
            configMap:
              type: string
              minLength: 1
              maxLength: 253
              pattern: {{ $dns | quote }}
            policy:
              type: object
              x-kubernetes-preserve-unknown-fields: true
          x-kubernetes-validations:
            - rule: has(self.configMap) != has(self.policy)
              message: Give a ConfigMap or the policy, not both.
        auditWebhook:
          type: object
          required: [secret]
          description: >-
            Sends audit events to a webhook. The Secret in the cluster's namespace holds the
            webhook's kubeconfig.
          properties:
            secret:
              type: string
              minLength: 1
              maxLength: 253
              pattern: {{ $dns | quote }}
            key:
              type: string
              default: kubeconfig
              maxLength: 253
              pattern: "^[-._a-zA-Z0-9]+$"
{{- end -}}

{{/*
A patch selector for the nodes of one role (controlPlane or worker) of a class kind.
*/}}
{{- define "kubehenge.nodeSelector" -}}
selector:
  apiVersion: {{ .api }}
  kind: {{ .kind }}
  matchResources:
    {{- if eq .role "controlPlane" }}
    controlPlane: true
    {{- else }}
    machineDeploymentClass:
      names: [worker]
    {{- end }}
{{- end -}}

{{/*
The distribution's templates, by role, and where a bootstrap template keeps its files, commands and
Talos strategic patches (call with the distribution's name).
*/}}
{{- define "kubehenge.nodeTargets" -}}
{{- $targets := dict
  "kubeadm" (dict
    "controlPlane" (dict "kind" "KubeadmControlPlaneTemplate" "api" "controlplane.cluster.x-k8s.io/v1beta2" "base" "/spec/template/spec/kubeadmConfigSpec")
    "worker" (dict "kind" "KubeadmConfigTemplate" "api" "bootstrap.cluster.x-k8s.io/v1beta2" "base" "/spec/template/spec")
    "commands" "preKubeadmCommands")
  "rke2" (dict
    "controlPlane" (dict "kind" "RKE2ControlPlaneTemplate" "api" "controlplane.cluster.x-k8s.io/v1beta2" "base" "/spec/template/spec")
    "worker" (dict "kind" "RKE2ConfigTemplate" "api" "bootstrap.cluster.x-k8s.io/v1beta2" "base" "/spec/template/spec")
    "commands" "preRKE2Commands")
  "k3s" (dict
    "controlPlane" (dict "kind" "KThreesControlPlaneTemplate" "api" "controlplane.cluster.x-k8s.io/v1beta2" "base" "/spec/template/spec/kthreesConfigSpec")
    "worker" (dict "kind" "KThreesConfigTemplate" "api" "bootstrap.cluster.x-k8s.io/v1beta2" "base" "/spec/template/spec")
    "commands" "preK3sCommands")
  "talos" (dict
    "controlPlane" (dict "kind" "TalosControlPlaneTemplate" "api" "controlplane.cluster.x-k8s.io/v1beta1" "base" "/spec/template/spec/controlPlaneConfig/controlplane")
    "worker" (dict "kind" "TalosConfigTemplate" "api" "bootstrap.cluster.x-k8s.io/v1beta1" "base" "/spec/template/spec")) }}
{{- get $targets . | toJson }}
{{- end -}}

{{/*
The enabledIf of the item at index I of a list of nodeCustomisation (field) that goes to role, or of
any item at all when I is empty.
*/}}
{{- define "kubehenge.nodeItemEnabled" -}}
{{- if .role -}}
{{ printf "{{ with .nodeCustomisation }}{{ with .%s }}{{ if gt (len .) %d }}{{ $x := index . %d }}{{ if or (not $x.roles) (has %q $x.roles) }}true{{ end }}{{ end }}{{ end }}{{ end }}" .field .index .index .role }}
{{- else -}}
{{ printf "{{ with .nodeCustomisation }}{{ with .%s }}{{ if gt (len .) %d }}true{{ end }}{{ end }}{{ end }}" .field .index }}
{{- end -}}
{{- end -}}

{{/*
The patches that give the nodes of a distribution (kubeadm, rke2, k3s or talos) their nodeCustomisation.
*/}}
{{- define "kubehenge.nodeCustomisationPatches" -}}
{{- $dist := . }}
{{- $t := include "kubehenge.nodeTargets" $dist | fromJson }}
{{- $max := include "kubehenge.nodeCustomisationMax" . | int }}
{{- $roles := list "controlPlane" "worker" }}
{{- $anyArgs := `{{ with .nodeCustomisation }}{{ if or .kubeletArgs .apiServerArgs .controllerManagerArgs .schedulerArgs .auditWebhook }}true{{ end }}{{ end }}` }}
{{- if eq $dist "talos" }}
{{- include "kubehenge.talosNodePatches" (dict "t" $t) }}
{{- else }}
{{- range $role := $roles }}
{{- $target := get $t $role }}
{{- $selector := include "kubehenge.nodeSelector" (dict "api" $target.api "kind" $target.kind "role" $role) }}
{{- range $i := until $max }}
- name: {{ $dist }}-node-file-{{ $role }}-{{ $i }}
  enabledIf: {{ include "kubehenge.nodeItemEnabled" (dict "field" "files" "index" $i "role" $role) | quote }}
  definitions:
    - {{- $selector | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $target.base }}/files/-
          valueFrom:
            template: {{ printf `{{ $f := index .nodeCustomisation.files %d }}{{ $o := dict "path" $f.path "owner" "root:root" "permissions" ($f.permissions | default "0644") }}{{ if $f.secret }}{{ $_ := set $o "contentFrom" (dict "secret" (dict "name" $f.secret.name "key" $f.secret.key)) }}{{ else }}{{ $_ := set $o "content" ($f.content | default "") }}{{ end }}{{ toJson $o }}` $i | quote }}
- name: {{ $dist }}-node-command-{{ $role }}-{{ $i }}
  enabledIf: {{ include "kubehenge.nodeItemEnabled" (dict "field" "commands" "index" $i "role" $role) | quote }}
  definitions:
    - {{- $selector | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $target.base }}/{{ get $t "commands" }}/-
          valueFrom:
            template: {{ printf `{{ (index .nodeCustomisation.commands %d).run | toJson }}` $i | quote }}
{{- end }}
{{- end }}
{{- if eq $dist "kubeadm" }}
{{- include "kubehenge.kubeadmNodePatches" (dict "t" $t "max" $max) }}
{{- else }}
{{- include "kubehenge.agentNodePatches" (dict "dist" $dist "t" $t "anyArgs" $anyArgs) }}
{{- end }}
{{- include "kubehenge.auditPatches" (dict "dist" $dist "t" $t) }}
{{- end }}
{{- end -}}

{{/*
kubeadm's arguments: one entry of its lists at a time. The kubelet's go in the init and the join
configuration of a control plane, and the web hook's file is mounted into the API server's pod.
*/}}
{{- define "kubehenge.kubeadmNodePatches" -}}
{{- $t := .t }}
{{- $cp := $t.controlPlane }}
{{- $w := $t.worker }}
{{- range $i := until (int .max) }}
{{- $entry := printf `{{ $a := index .nodeCustomisation.%s %d }}{{ dict "name" $a.name "value" ($a.value | default "") | toJson }}` "FIELD" $i }}
- name: kubeadm-node-kubelet-arg-{{ $i }}
  enabledIf: {{ include "kubehenge.nodeItemEnabled" (dict "field" "kubeletArgs" "index" $i) | quote }}
  definitions:
    - {{- include "kubehenge.nodeSelector" (dict "api" $cp.api "kind" $cp.kind "role" "controlPlane") | nindent 6 }}
      jsonPatches:
        {{- range $config := list "initConfiguration" "joinConfiguration" }}
        - op: add
          path: {{ $cp.base }}/{{ $config }}/nodeRegistration/kubeletExtraArgs/-
          valueFrom:
            template: {{ replace "FIELD" "kubeletArgs" $entry | quote }}
        {{- end }}
    - {{- include "kubehenge.nodeSelector" (dict "api" $w.api "kind" $w.kind "role" "worker") | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $w.base }}/joinConfiguration/nodeRegistration/kubeletExtraArgs/-
          valueFrom:
            template: {{ replace "FIELD" "kubeletArgs" $entry | quote }}
{{- range $field, $component := dict "apiServerArgs" "apiServer" "controllerManagerArgs" "controllerManager" "schedulerArgs" "scheduler" }}
- name: kubeadm-node-{{ $component }}-arg-{{ $i }}
  enabledIf: {{ include "kubehenge.nodeItemEnabled" (dict "field" $field "index" $i) | quote }}
  definitions:
    - {{- include "kubehenge.nodeSelector" (dict "api" $cp.api "kind" $cp.kind "role" "controlPlane") | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $cp.base }}/clusterConfiguration/{{ $component }}/extraArgs/-
          valueFrom:
            template: {{ replace "FIELD" $field $entry | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
RKE2's and k3s's arguments: a configuration drop-in that appends to the lists of the provider's own
(the + suffix), one for the control planes and one for the workers. It sorts after Kubehenge's
drop-ins, so a later scalar of it would win, which the web hook refuses for the settings Kubehenge
owns. The audit web hook's argument is part of the API server's list.
*/}}
{{- define "kubehenge.agentNodePatches" -}}
{{- $dist := .dist }}
{{- $t := .t }}
{{- $dir := printf "/etc/rancher/%s/config.yaml.d" (ternary "rke2" "k3s" (eq $dist "rke2")) }}
{{- $webhook := printf "/etc/rancher/%s/audit-webhook.yaml" (ternary "rke2" "k3s" (eq $dist "rke2")) }}
{{- $anyArgs := .anyArgs }}
{{- $build := `{{ $c := dict }}{{ with .nodeCustomisation }}{{ with .kubeletArgs }}{{ $l := list }}{{ range . }}{{ $l = append $l (printf "%s=%s" .name (.value | default "")) }}{{ end }}{{ $_ := set $c "kubelet-arg+" $l }}{{ end }}` }}
{{- $control := `{{ $l := list }}{{ with .apiServerArgs }}{{ range . }}{{ $l = append $l (printf "%s=%s" .name (.value | default "")) }}{{ end }}{{ end }}{{ if .auditWebhook }}{{ $l = append $l "audit-webhook-config-file=WEBHOOK" }}{{ end }}{{ if $l }}{{ $_ := set $c "kube-apiserver-arg+" $l }}{{ end }}{{ with .controllerManagerArgs }}{{ $l := list }}{{ range . }}{{ $l = append $l (printf "%s=%s" .name (.value | default "")) }}{{ end }}{{ $_ := set $c "kube-controller-manager-arg+" $l }}{{ end }}{{ with .schedulerArgs }}{{ $l := list }}{{ range . }}{{ $l = append $l (printf "%s=%s" .name (.value | default "")) }}{{ end }}{{ $_ := set $c "kube-scheduler-arg+" $l }}{{ end }}` }}
{{- $end := printf `{{ end }}{{ dict "path" "%s/70-kubehenge-customisation.yaml" "owner" "root:root" "permissions" "0600" "content" (toJson $c) | toJson }}` $dir }}
{{- $control = replace "WEBHOOK" $webhook $control }}
{{- range $role := list "controlPlane" "worker" }}
{{- $target := get $t $role }}
- name: {{ $dist }}-node-arguments-{{ $role }}
  enabledIf: {{ ternary $anyArgs `{{ with .nodeCustomisation }}{{ if .kubeletArgs }}true{{ end }}{{ end }}` (eq $role "controlPlane") | quote }}
  definitions:
    - {{- include "kubehenge.nodeSelector" (dict "api" $target.api "kind" $target.kind "role" $role) | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $target.base }}/files/-
          valueFrom:
            template: {{ printf "%s%s%s" $build (ternary $control "" (eq $role "controlPlane")) $end | quote }}
{{- end }}
{{- end -}}

{{/*
The audit policy and webhook of the control planes (kubeadm, RKE2 and k3s). The policy is a second
file at the path of Kubehenge's own, written after it. A ConfigMap's policy is read from the Secret
of the cluster's name that Kubehenge's controller keeps in step with it, since a bootstrap template
reads Secrets only.
*/}}
{{- define "kubehenge.auditPatches" -}}
{{- $dist := .dist }}
{{- $cp := .t.controlPlane }}
{{- if ne $dist "talos" }}
{{- $dir := ternary "/etc/kubernetes" (printf "/etc/rancher/%s" $dist) (eq $dist "kubeadm") }}
- name: {{ $dist }}-node-audit-policy
  enabledIf: {{ `{{ with .nodeCustomisation }}{{ if .auditPolicy }}true{{ end }}{{ end }}` | quote }}
  definitions:
    - {{- include "kubehenge.nodeSelector" (dict "api" $cp.api "kind" $cp.kind "role" "controlPlane") | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $cp.base }}/files/-
          valueFrom:
            template: {{ printf `{{ $o := dict "path" "%s/audit-policy.yaml" "owner" "root:root" "permissions" "0600" }}{{ if .nodeCustomisation.auditPolicy.configMap }}{{ $_ := set $o "contentFrom" (dict "secret" (dict "name" (printf "%%s-audit-policy" .builtin.cluster.name) "key" "audit-policy.yaml")) }}{{ else }}{{ $_ := set $o "content" (toJson .nodeCustomisation.auditPolicy.policy) }}{{ end }}{{ toJson $o }}` $dir | quote }}
- name: {{ $dist }}-node-audit-webhook
  enabledIf: {{ `{{ with .nodeCustomisation }}{{ if .auditWebhook }}true{{ end }}{{ end }}` | quote }}
  definitions:
    - {{- include "kubehenge.nodeSelector" (dict "api" $cp.api "kind" $cp.kind "role" "controlPlane") | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $cp.base }}/files/-
          valueFrom:
            template: {{ printf `{{ dict "path" "%s/audit-webhook.yaml" "owner" "root:root" "permissions" "0600" "contentFrom" (dict "secret" (dict "name" .nodeCustomisation.auditWebhook.secret "key" (.nodeCustomisation.auditWebhook.key | default "kubeconfig"))) | toJson }}` $dir | quote }}
        {{- if eq $dist "kubeadm" }}
        - op: add
          path: {{ $cp.base }}/clusterConfiguration/apiServer/extraArgs/-
          value:
            name: audit-webhook-config-file
            value: {{ $dir }}/audit-webhook.yaml
        - op: add
          path: {{ $cp.base }}/clusterConfiguration/apiServer/extraVolumes/-
          value:
            name: audit-webhook
            hostPath: {{ $dir }}/audit-webhook.yaml
            mountPath: {{ $dir }}/audit-webhook.yaml
            readOnly: true
            pathType: File
        {{- end }}
{{- end }}
{{- end -}}

{{/*
Talos: each setting is a strategic patch of the machine configuration, since the Talos bootstrap
provider takes no files, commands or Secrets of its own. The policy replaces Kubehenge's, which
the first patch removes.
*/}}
{{- define "kubehenge.talosNodePatches" -}}
{{- $cp := .t.controlPlane }}
{{- $w := .t.worker }}
{{- $perm := `{{ $perm := dict "0400" 256 "0440" 288 "0444" 292 "0600" 384 "0640" 416 "0644" 420 "0700" 448 "0750" 488 "0755" 493 }}` }}
{{- range $role := list "controlPlane" "worker" }}
{{- $target := get $.t $role }}
{{- $any := printf `{{ $any := false }}{{ with .nodeCustomisation }}{{ range .files }}{{ if or (not .roles) (has %q .roles) }}{{ $any = true }}{{ end }}{{ end }}{{ end }}{{ if $any }}true{{ end }}` $role }}
- name: talos-node-files-{{ $role }}
  enabledIf: {{ $any | quote }}
  definitions:
    - {{- include "kubehenge.nodeSelector" (dict "api" $target.api "kind" $target.kind "role" $role) | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $target.base }}/strategicPatches/-
          valueFrom:
            template: {{ printf `%s{{ $fs := list }}{{ range .nodeCustomisation.files }}{{ if or (not .roles) (has %q .roles) }}{{ $fs = append $fs (dict "path" .path "op" "create" "permissions" (get $perm (.permissions | default "0644")) "content" (.content | default "")) }}{{ end }}{{ end }}{{ dict "machine" (dict "files" $fs) | toJson | toJson }}` $perm $role | quote }}
- name: talos-node-kubelet-args-{{ $role }}
  enabledIf: {{ `{{ with .nodeCustomisation }}{{ if .kubeletArgs }}true{{ end }}{{ end }}` | quote }}
  definitions:
    - {{- include "kubehenge.nodeSelector" (dict "api" $target.api "kind" $target.kind "role" $role) | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $target.base }}/strategicPatches/-
          valueFrom:
            template: {{ `{{ $m := dict }}{{ range .nodeCustomisation.kubeletArgs }}{{ $_ := set $m .name (.value | default "") }}{{ end }}{{ dict "apiVersion" "v1alpha1" "kind" "KubeletConfig" "extraArgs" $m | toJson | toJson }}` | quote }}
{{- end }}
{{- range $field, $kind := dict "apiServerArgs" "KubeAPIServerConfig" "controllerManagerArgs" "KubeControllerManagerConfig" "schedulerArgs" "KubeSchedulerConfig" }}
- name: talos-node-{{ $field }}
  enabledIf: {{ printf `{{ with .nodeCustomisation }}{{ if .%s }}true{{ end }}{{ end }}` $field | quote }}
  definitions:
    - {{- include "kubehenge.nodeSelector" (dict "api" $cp.api "kind" $cp.kind "role" "controlPlane") | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $cp.base }}/strategicPatches/-
          valueFrom:
            template: {{ printf `{{ $m := dict }}{{ range .nodeCustomisation.%s }}{{ $_ := set $m .name (.value | default "") }}{{ end }}{{ dict "apiVersion" "v1alpha1" "kind" "%s" "extraArgs" $m | toJson | toJson }}` $field $kind | quote }}
{{- end }}
- name: talos-node-audit-policy
  enabledIf: {{ `{{ with .nodeCustomisation }}{{ with .auditPolicy }}{{ if .policy }}true{{ end }}{{ end }}{{ end }}` | quote }}
  definitions:
    - {{- include "kubehenge.nodeSelector" (dict "api" $cp.api "kind" $cp.kind "role" "controlPlane") | nindent 6 }}
      jsonPatches:
        - op: add
          path: {{ $cp.base }}/strategicPatches/-
          value: {{ dict "apiVersion" "v1alpha1" "kind" "KubeAuditPolicyConfig" "$patch" "delete" | toJson | quote }}
        - op: add
          path: {{ $cp.base }}/strategicPatches/-
          valueFrom:
            template: {{ `{{ dict "apiVersion" "v1alpha1" "kind" "KubeAuditPolicyConfig" "configuration" .nodeCustomisation.auditPolicy.policy | toJson | toJson }}` | quote }}
{{- end -}}
