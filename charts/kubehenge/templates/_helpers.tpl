{{/*
Every ClusterClass shares the Proxmox VE side: the same variables, the same infrastructure templates and
the patches that fill them in. Cluster API's own templating uses the same braces as Helm's, so its
expressions are written here as Helm string literals.
*/}}

{{/*
Templates referenced by a ClusterClass are never edited in place (capmox refuses changes to a
ProxmoxMachineTemplate), so each name carries a hash of its spec and a changed spec is a new object.
Call with (dict "name" <base name> "spec" <rendered spec>).
*/}}
{{- define "kubehenge.hashedName" -}}
{{- printf "%s-%s" .name (.spec | sha256sum | trunc 8) -}}
{{- end -}}

{{- define "kubehenge.proxmoxClusterSpec" -}}
template:
  spec:
    # Placeholders: patches replace every value from the Cluster's variables.
    controlPlaneEndpoint:
      host: 192.0.2.1
      port: 6443
    ipv4Config:
      addresses:
        - 192.0.2.2-192.0.2.3
      prefix: 24
      gateway: 192.0.2.254
    dnsServers:
      - 192.0.2.253
{{- end -}}

{{/*
Call with "tags", the template tags to select as written by `kubehenge template create`, and
"serviceJoins" for distributions whose node joins through a systemd service that keeps retrying
(k3s, RKE2). Their cloud-init stops at the installer's first start of that service, so a join that
waits out a short outage of the control plane (another server restarting during a rolling upgrade)
leaves cloud-init in error although the service joins on its next try; capmox would fail such a
machine for good, and a control plane of one or two could then neither finish rolling nor
remediate it without losing etcd quorum. For those, capmox waits for the guest agent only, and a
node that never joins is replaced by the health check's startup timeout.
*/}}
{{- define "kubehenge.proxmoxMachineSpec" -}}
template:
  spec:
    full: true
    templateSelector:
      matchTags: {{ toJson .tags }}
      matchPolicy: exact
    {{- if .serviceJoins }}
    checks:
      skipCloudInitStatus: true
    {{- end }}
    numSockets: 1
    numCores: 2
    memoryMiB: 4096
    disks:
      bootVolume:
        disk: scsi0
        sizeGb: 30
    network:
      networkDevices:
        - name: net0
          model: virtio
          bridge: vmbr0
{{- end -}}

{{- define "kubehenge.machineSchema" -}}
type: object
default:
  cores: {{ .cores }}
  memoryMiB: {{ .memoryMiB }}
  diskGiB: {{ .diskGiB }}
properties:
  cores:
    type: integer
    minimum: {{ .minCores }}
    default: {{ .cores }}
  memoryMiB:
    type: integer
    minimum: {{ .minMemoryMiB }}
    multipleOf: 8
    default: {{ .memoryMiB }}
  diskGiB:
    type: integer
    description: Size of the boot disk. A disk only grows, so it is never below the template's.
    minimum: {{ .minDiskGiB }}
    default: {{ .diskGiB }}
{{- end -}}

{{/*
Call with (dict "controlPlaneDiskGiB" <n> "workerDiskGiB" <n> "minDiskGiB" <n> "workerMinCores" <n> "workerMinMemoryMiB" <n>).
A control plane's minimum is the first row of the k3s and RKE2 server sizing guides, 2 vCPUs and
4 GiB; kubeadm, which documents no sizes past its 2 GiB to start, follows RKE2's, whose servers run
the same components. On less, an add-on's install crowds out the API server and etcd. A worker's is
never below its distribution's documented minimum for a node.
*/}}
{{- define "kubehenge.proxmoxVariables" -}}
- name: controlPlaneEndpoint
  required: true
  schema:
    openAPIV3Schema:
      type: string
      format: ipv4
      description: >-
        Virtual IP address of the Kubernetes API. It moves between the control-plane nodes, so it is
        an unused address on the nodes' network and outside the range in network.addresses.
- name: network
  required: true
  schema:
    openAPIV3Schema:
      type: object
      required: [bridge, addresses, prefix, gateway, dnsServers]
      properties:
        bridge:
          type: string
          minLength: 1
          description: Proxmox VE bridge the nodes connect to.
          {{- with .clusters.bridges }}
          enum: {{ toJson . }}
          {{- end }}
        vlan:
          type: integer
          minimum: 1
          maximum: 4094
        addresses:
          type: array
          minItems: 1
          description: Addresses the nodes take theirs from, as ranges (10.0.0.10-10.0.0.20), networks in CIDR notation or single addresses.
          items:
            type: string
            minLength: 1
        prefix:
          type: integer
          minimum: 1
          maximum: 32
        gateway:
          type: string
          format: ipv4
        dnsServers:
          type: array
          minItems: 1
          items:
            type: string
            minLength: 1
- name: storage
  required: true
  schema:
    openAPIV3Schema:
      type: string
      minLength: 1
      description: Proxmox VE storage for the nodes' disks and the cluster's persistent volumes.
      {{- with .clusters.storages }}
      enum: {{ toJson . }}
      {{- end }}
- name: allowedNodes
  required: false
  schema:
    openAPIV3Schema:
      type: array
      description: Proxmox VE nodes the cluster's VMs may run on; every node when unset. New VMs go to those of them that are online.
      items:
        type: string
        minLength: 1
- name: proxmoxHA
  required: false
  schema:
    openAPIV3Schema:
      type: boolean
      default: true
      description: >-
        Whether Proxmox VE HA keeps the control-plane VMs on separate nodes and restarts them on
        another node when theirs fails. It takes effect where Kubehenge's administrator has enabled
        Proxmox VE HA and the cluster's disks are on shared storage.
- name: csi
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: The Proxmox VE CSI plugin, which every cluster runs for its persistent volumes.
      properties:
        datastoreAllocate:
          type: boolean
          default: false
          description: >-
            Run the plugin's newest release, which needs Datastore.Allocate on the cluster's
            storage, and give the cluster's Proxmox VE token Datastore.Allocate there: it may then
            delete any volume on that storage and change the storage's definition. Only where
            Kubehenge's administrator lets clusters choose it, and on a storage that neither the
            management cluster, its templates nor another cluster uses. Off, the cluster runs the
            plugin's last release that needs no more than Datastore.AllocateSpace.
- name: controlPlaneMachine
  required: false
  schema:
    openAPIV3Schema:
      {{- include "kubehenge.machineSchema" (dict "cores" 2 "minCores" 2 "memoryMiB" 4096 "minMemoryMiB" 4096 "diskGiB" .controlPlaneDiskGiB "minDiskGiB" .minDiskGiB) | nindent 6 }}
- name: workerMachine
  required: false
  schema:
    openAPIV3Schema:
      {{- include "kubehenge.machineSchema" (dict "cores" 2 "minCores" .workerMinCores "memoryMiB" 4096 "minMemoryMiB" .workerMinMemoryMiB "diskGiB" .workerDiskGiB "minDiskGiB" .minDiskGiB) | nindent 6 }}
{{- end -}}

{{/*
The memory, in MiB, each add-on's pods use in a cluster, the same for every distribution: the
end-to-end tests' working sets once every add-on has started, with room to grow. Monitoring is
Prometheus, Alertmanager, Grafana, node-exporter and the metrics proxies, at 1.5 GiB as Prometheus
grows while it fills its first two-hour block; Rancher's own pod peaks at about 1.6 GiB as it
first starts, and with a cluster registered Rancher, Fleet and their controllers hold about 1.2 GiB.
Istio uses about 100 MiB, but istiod and the ztunnel beside it ask for 384 MiB, which the node they
share has to have; the clustermesh-apiserver uses about 50 MiB. The load balancer is MetalLB's
controller and a node's speaker, or kube-vip and its cloud provider. Logs are Loki, which asks for
256 MiB, the Alloy that reads the events and a node's Alloy.
*/}}
{{- define "kubehenge.addonMemory" -}}
kubehenge.dev/addon-memory: '{"headlamp":64,"monitoring":1536,"logs":512,"kyverno":384,"istio":384,"clusterMesh":128,"loadBalancer":128,"certManager":192,"rancher":2048,"rancherAgent":512}'
{{- end -}}

{{/*
Add-ons Kubehenge's controller installs into a cluster, the same for every distribution.
*/}}
{{- define "kubehenge.addonVariables" -}}
- name: headlamp
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Headlamp, a web interface for the cluster. Users sign in with their own token or through
        OIDC; Headlamp itself has no rights in the cluster. Reached through Kubehenge's interface, or
        at hostname when one is set.
      properties:
        enabled:
          type: boolean
          default: false
        hostname:
          type: string
          format: hostname
          description: Also serve Headlamp at this name through the cluster's ingress controller.
        tlsSecret:
          type: string
          minLength: 1
          description: >-
            Secret in the headlamp namespace holding the certificate for hostname; the ingress
            controller's own default certificate when unset.
- name: monitoring
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Prometheus, Alertmanager and Grafana with the kube-prometheus-stack dashboards and alerts,
        scraping every node and control-plane component over verified, authenticated TLS. Installs
        cert-manager too. Grafana's admin password is in the kube-prometheus-stack-grafana Secret
        in the monitoring namespace.
      properties:
        enabled:
          type: boolean
          default: false
        hostname:
          type: string
          format: hostname
          description: Serve Grafana at this name through the cluster's ingress controller.
        tlsSecret:
          type: string
          minLength: 1
          description: >-
            Secret in the monitoring namespace holding the certificate for hostname; the ingress
            controller's own default certificate when unset.
- name: logs
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Loki, keeping the cluster's logs and its events on a volume of their own, read in
        monitoring's Grafana, which they need, and Grafana Alloy collecting them. Only Alloy may
        push to Loki and only Grafana read it, each by its client certificate, besides users
        whose RBAC allows them the Loki paths. Switched off, the volume stays.
      properties:
        enabled:
          type: boolean
          default: false
        collector:
          type: string
          enum: [node, api]
          default: node
          description: >-
            node runs Alloy on every node, reading the files the kubelet writes, the node's journal
            and on the control planes the API server's audit log, so it catches pods gone again
            within seconds; it reads host paths as root without capabilities, from the privileged
            logging-node namespace. api runs one Alloy without anything of the host, reading the
            pods' logs through the Kubernetes API: lighter, but without journals or audit log, and
            it can miss pods that live only seconds.
        retentionDays:
          type: integer
          minimum: 1
          maximum: 366
          default: 7
          description: Days Loki keeps each line.
        sizeGiB:
          type: integer
          minimum: 1
          maximum: 1024
          default: 10
          description: >-
            Loki's volume, in GiB, which has to hold the retention's worth of logs: Loki takes no
            lines while it is full. It only grows.
          x-kubernetes-validations:
            - rule: self >= oldSelf
              message: Loki's volume only grows.
- name: podSecurity
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        The Pod Security Standard the cluster's pods are held to, by Kubernetes' Pod Security
        Admission, by Kyverno, or by both. The namespaces of the components that need the host
        (kube-system, the CNI's, node-exporter's, Alloy's node collectors', Istio's node agents',
        Rancher's) are exempt from both.
      properties:
        admission:
          type: string
          enum: [privileged, baseline, restricted]
          default: privileged
          description: >-
            The level Pod Security Admission enforces in every namespace that labels none of its
            own. Below restricted, pods short of restricted are warned about and audited. Changing
            it replaces the control-plane machines.
        kyverno:
          type: string
          enum: ["off", baseline, restricted]
          default: "off"
          description: >-
            The level Kyverno's Pod Security Standards policies enforce, in every namespace and
            whatever its labels say, with a policy report of what falls short; off leaves Kyverno
            out of the cluster.
- name: istio
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Istio in ambient mode: a namespace labelled istio.io/dataplane-mode=ambient has its pods'
        traffic carried over mutual TLS between them, with their workload identities, and no
        sidecars. Switching it off first takes every namespace and pod out of the mesh.
      properties:
        enabled:
          type: boolean
          default: false
- name: clusterMesh
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Cilium Cluster Mesh, for a cluster that runs Cilium: its pods reach those of the clusters it
        is linked with, by address and through global services, and Cilium's network policies can
        select them. Two clusters of one namespace are linked while both are on and list each
        other, with pod ranges apart; each then reads the other's pod names, labels and identities.
      properties:
        enabled:
          type: boolean
          default: false
        peers:
          type: array
          maxItems: 254
          description: The clusters of this namespace to link with.
          items:
            type: string
            maxLength: 32
            pattern: "^[a-z]([-a-z0-9]*[a-z0-9])?$"
- name: loadBalancer
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Addresses for the cluster's Services of type LoadBalancer, from a range on the nodes'
        network. One node at a time answers for each address, and another takes over when it fails.
        Without it, kubeadm and RKE2 give such Services no address, and k3s's servicelb answers for
        them on every node's own address.
      properties:
        enabled:
          type: boolean
          default: false
        implementation:
          type: string
          enum: [metallb, kube-vip]
          default: metallb
          description: >-
            MetalLB, or kube-vip, which also holds the cluster's API address. Switching replaces the
            one with the other; Services keep their addresses where the new one can, and take new
            ones from the range otherwise.
        addresses:
          type: array
          minItems: 1
          description: >-
            Addresses the Services take theirs from, as ranges (10.0.0.30-10.0.0.40), networks in
            CIDR notation or single addresses: unused addresses on the nodes' network, apart from
            network.addresses, the control-plane endpoint, the gateway, the network's own and
            broadcast addresses and other clusters' addresses there. Services outside a changed
            range take new addresses from it.
          items:
            type: string
            minLength: 1
      x-kubernetes-validations:
        - rule: "!self.enabled || has(self.addresses)"
          message: A load balancer needs addresses.
- name: rancher
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Rancher. One cluster installs it, and Kubehenge registers every other cluster with it unless
        that cluster sets register to false; a cluster leaves Rancher before it is deleted.
      properties:
        install:
          type: boolean
          default: false
          description: >-
            Install Rancher into this cluster. Kubernetes stays within the versions the installed
            Rancher release runs on.
        hostname:
          type: string
          format: hostname
          description: >-
            Name Rancher is reached at, by people and by every registered cluster's agent; it
            resolves to this cluster's ingress controller, such as its control-plane virtual IP.
        register:
          type: boolean
          default: true
          description: Register this cluster with the Rancher Kubehenge installed.
      x-kubernetes-validations:
        - rule: "!self.install || has(self.hostname)"
          message: Rancher needs a hostname.
{{- end -}}

{{/*
Keeps a cluster on the CNI it was created with: switching a running cluster's pod network leaves
the old one's interfaces, routes and pod addresses behind on every node.
*/}}
{{- define "kubehenge.cniFixed" -}}
x-kubernetes-validations:
  - rule: self == oldSelf
    message: the CNI is chosen when the cluster is created and cannot be changed
{{- end -}}

{{/*
Identity providers a cluster's API server trusts, the same for every distribution. Call with the
root context: the default is the chart's clusterOIDC.
*/}}
{{- define "kubehenge.oidcVariable" -}}
- name: oidc
  required: false
  schema:
    openAPIV3Schema:
      type: array
      maxItems: 8
      description: >-
        OpenID Connect providers whose ID tokens the API server accepts, besides certificates and
        ServiceAccount tokens. A token's audience has to be the provider's clientID. Headlamp and
        kubectl sign in with the first one; tokens keep working beside it.
      {{- with .Values.clusterOIDC }}
      default:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      items:
        type: object
        required: [issuerURL, clientID]
        properties:
          issuerURL:
            type: string
            pattern: "^https://"
            maxLength: 2048
            description: The provider's issuer, exactly as its tokens name it.
          clientID:
            type: string
            minLength: 1
            maxLength: 256
            description: The client sign-in tools use; tokens must be issued to it.
          usernameClaim:
            type: string
            default: email
          usernamePrefix:
            type: string
            maxLength: 64
            default: "oidc:"
            description: Put before every user name, so none can pass for a user of another kind.
            {{- include "kubehenge.notSystem" . | nindent 12 }}
          groupsClaim:
            type: string
            default: groups
          groupsPrefix:
            type: string
            maxLength: 64
            default: "oidc:"
            {{- include "kubehenge.notSystem" . | nindent 12 }}
          scopes:
            type: array
            items:
              type: string
            default: [email, profile]
            description: Scopes sign-in tools ask for besides openid; the API server does not read them.
          certificateAuthority:
            type: string
            description: PEM CA of the provider's certificate, when the system's do not cover it.
{{- end -}}

{{/*
Kubernetes keeps system: for its own users and groups, such as system:masters. On the strings
themselves: Cluster API cannot check a rule on a list variable that also has a default.
*/}}
{{- define "kubehenge.notSystem" -}}
x-kubernetes-validations:
  - rule: "!self.startsWith('system:')"
    message: "Kubernetes keeps system: for its own users and groups."
{{- end -}}

{{/*
The structured authentication configuration as a bootstrap file at path: a template Cluster API
renders with the cluster's variables. With anonymous, requests without credentials reach the few
paths kubeadm needs them to; without, the API server is to be given anonymous-auth=false
(files/authentication-config.tpl). Call with (dict "root" $ "path" … "anonymous" true|false).
*/}}
{{- define "kubehenge.oidcFile" -}}
path: {{ .path }}
{{ printf "{{- $anonymous := %t }}" .anonymous }}
{{ .root.Files.Get "files/authentication-config.tpl" }}
{{- end -}}

{{/*
The API server's admission configuration as a bootstrap file at path, a template Cluster API
renders with the cluster's variables, the chart's exempt namespaces written into it. Call with
(dict "root" $ "path" …).
*/}}
{{- define "kubehenge.admissionFile" -}}
path: {{ .path }}
{{ printf "{{- $exempt := list %s }}" (include "kubehenge.quotedList" .root.Values.podSecurityExempt) }}
{{ .root.Files.Get "files/admission.tpl" }}
{{- end -}}

{{/*
The kernel settings every node runs with. The kubelet checks them and refuses to start when they
differ (protect-kernel-defaults), rather than change the host behind its back: the four the CIS
hardening guides set, and the two the kubelet itself would. A node whose kernel panics or oopses
restarts after ten seconds instead of running on half broken; its machine health check and
Proxmox VE HA take it from there. Applied before the distribution starts with sysctl --system.
*/}}
{{- define "kubehenge.sysctlFile" -}}
- path: /etc/sysctl.d/90-kubehenge.conf
  owner: root:root
  permissions: "0644"
  content: |
    vm.panic_on_oom=0
    vm.overcommit_memory=1
    kernel.panic=10
    kernel.panic_on_oops=1
    kernel.keys.root_maxkeys=1000000
    kernel.keys.root_maxbytes=25000000
{{- end -}}

{{/*
The API server's audit policy as a bootstrap file at path, the same for every distribution.
*/}}
{{- define "kubehenge.auditPolicyFile" -}}
- path: {{ .path }}
  owner: root:root
  permissions: "0600"
  content: |
    {{- .root.Files.Get "files/audit-policy.yaml" | nindent 4 }}
{{- end -}}

{{/*
The TLS 1.2 cipher suites the kubelets and kubeadm's API server offer: forward secret, with
authenticated encryption only. TLS 1.3 chooses its own.
*/}}
{{- define "kubehenge.tlsCipherSuites" -}}
TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
{{- end -}}

{{- define "kubehenge.quotedList" -}}
{{- $quoted := list }}
{{- range . }}{{ $quoted = append $quoted (quote .) }}{{ end }}
{{- join " " $quoted }}
{{- end -}}

{{- define "kubehenge.oidcEnabled" -}}
{{ "{{ if .oidc }}true{{ end }}" | quote }}
{{- end -}}

{{/*
Hardening to the CIS Kubernetes Benchmark beyond the baseline every cluster gets, where it costs
workloads something: restricted pods, the runtime's seccomp profile for every container, and
streaming sessions that end when idle. Each class adds its distribution's own on top.
*/}}
{{- define "kubehenge.cisVariable" -}}
- name: cis
  required: false
  schema:
    openAPIV3Schema:
      type: boolean
      default: false
      description: >-
        Harden the cluster to the CIS Kubernetes Benchmark beyond what every cluster gets: Pod
        Security Admission enforces restricted outside the exempt namespaces, every container
        runs under the runtime's default seccomp profile unless it asks for another, and exec,
        attach and port-forward sessions end after five idle minutes. RKE2 also runs its own CIS
        profile, under which pods in kube-system, kube-public and default take connections only
        from their own namespace, DNS and a few of RKE2's own services aside. Fixed once the
        cluster exists.
      x-kubernetes-validations:
        - rule: self == oldSelf
          message: CIS hardening is chosen when the cluster is created and cannot be changed
{{- end -}}

{{- define "kubehenge.cisEnabled" -}}
{{ "{{ if .cis }}true{{ end }}" | quote }}
{{- end -}}

{{/*
The kubelet settings cis adds, as kubelet flags.
*/}}
{{- define "kubehenge.cisKubeletArgs" -}}
- seccomp-default=true
- streaming-connection-idle-timeout=5m
{{- end -}}

{{/*
Automatic Kubernetes upgrades, which Kubehenge's controller starts; no patch reads it.
*/}}
{{- define "kubehenge.upgradeVariables" -}}
- name: upgrades
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Upgrade Kubernetes automatically, inside a maintenance window. Kubehenge's controller takes
        the newest release the mode allows, one minor version at a time and never past the
        distribution's stable channel, and starts only when the cluster is healthy and no rollout
        is running. A started rollout finishes even if the window ends first.
      properties:
        mode:
          type: string
          enum: ["off", patch, minor]
          default: "off"
          description: patch stays on the cluster's minor version; minor also moves to the next one.
        window:
          type: object
          properties:
            weekdays:
              type: array
              description: Days of the week the window opens on; with monthDays, a day listed in either counts. Every day when both are empty.
              items:
                type: string
                enum: [Mon, Tue, Wed, Thu, Fri, Sat, Sun]
            monthDays:
              type: array
              items:
                type: integer
                minimum: 1
                maximum: 31
            start:
              type: string
              pattern: "^([01][0-9]|2[0-3]):[0-5][0-9]$"
              default: "02:00"
              description: When the window opens, as HH:MM.
            durationMinutes:
              type: integer
              minimum: 30
              maximum: 1440
              default: 240
            timeZone:
              type: string
              default: UTC
              description: IANA time zone of start, such as Europe/Berlin.
{{- end -}}

{{/*
Call with the root context.
*/}}
{{- define "kubehenge.proxmoxPatches" -}}
- name: proxmox-cluster
  definitions:
    - selector:
        apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
        kind: ProxmoxClusterTemplate
        matchResources:
          infrastructureCluster: true
      jsonPatches:
        - op: replace
          path: /spec/template/spec/controlPlaneEndpoint/host
          valueFrom:
            variable: controlPlaneEndpoint
        - op: replace
          path: /spec/template/spec/ipv4Config/addresses
          valueFrom:
            variable: network.addresses
        - op: replace
          path: /spec/template/spec/ipv4Config/prefix
          valueFrom:
            variable: network.prefix
        - op: replace
          path: /spec/template/spec/ipv4Config/gateway
          valueFrom:
            variable: network.gateway
        - op: replace
          path: /spec/template/spec/dnsServers
          valueFrom:
            variable: network.dnsServers
# The controller names the nodes capmox may use, the allowed ones that are online: capmox clones
# onto the template's node when given none, and places no VM while a node it is given is down. On
# RKE2 and k3s it also has every node install its release from files checked against the checksums
# recorded when a cluster first used the release, as neither project signs them.
- name: controller
  external:
    generatePatchesExtension: generate-patches.kubehenge
- name: proxmox-machines
  definitions:
    - selector:
        apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
        kind: ProxmoxMachineTemplate
        matchResources:
          controlPlane: true
          machineDeploymentClass:
            names: [worker]
      jsonPatches:
        - op: add
          path: /spec/template/spec/pool
          valueFrom:
            template: {{ printf "%s/{{ .builtin.cluster.namespace }}.{{ .builtin.cluster.name }}" .Values.workloadPool | quote }}
        - op: replace
          path: /spec/template/spec/network
          valueFrom:
            template: |
              networkDevices:
                - name: net0
                  model: virtio
                  bridge: {{ "{{ .network.bridge | quote }}" }}
                  {{- "{{ if .network.vlan }}" | nindent 18 }}
                  vlan: {{ "{{ .network.vlan }}" }}
                  {{- "{{ end }}" | nindent 18 }}
- name: proxmox-storage
  definitions:
    - selector:
        apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
        kind: ProxmoxMachineTemplate
        matchResources:
          controlPlane: true
          machineDeploymentClass:
            names: [worker]
      jsonPatches:
        - op: add
          path: /spec/template/spec/storage
          valueFrom:
            variable: storage
{{- range $role, $selector := dict "controlPlane" (dict "controlPlane" true) "worker" (dict "machineDeploymentClass" (dict "names" (list "worker"))) }}
- name: proxmox-{{ $role | kebabcase }}-size
  definitions:
    - selector:
        apiVersion: infrastructure.cluster.x-k8s.io/v1alpha2
        kind: ProxmoxMachineTemplate
        matchResources:
          {{- toYaml $selector | nindent 10 }}
      jsonPatches:
        - op: replace
          path: /spec/template/spec/numCores
          valueFrom:
            variable: {{ $role }}Machine.cores
        - op: replace
          path: /spec/template/spec/memoryMiB
          valueFrom:
            variable: {{ $role }}Machine.memoryMiB
        - op: replace
          path: /spec/template/spec/disks/bootVolume/sizeGb
          valueFrom:
            variable: {{ $role }}Machine.diskGiB
{{- end }}
{{- end -}}

{{/*
Machine health checks: a node that stays not ready is replaced. Called with "workers" or
"controlPlane". A share of unhealthy machines as the limit would never let a pool of one or two
workers be repaired; workers are instead replaced one at a time, which also keeps a fault that
takes many nodes down from replacing them all at once. The control-plane providers guard their own
remediation by etcd quorum.
*/}}
{{- define "kubehenge.healthCheck" -}}
checks:
  nodeStartupTimeoutSeconds: 1200
  unhealthyNodeConditions:
    - type: Ready
      status: Unknown
      timeoutSeconds: 300
    - type: Ready
      status: "False"
      timeoutSeconds: 300
{{- if eq . "workers" }}
remediation:
  maxInFlight: 1
{{- end }}
{{- end -}}

{{/*
k3s and RKE2 servers register with kubeadm's control-plane taint, so a new server takes no
workloads before the controller has looked at it; the controller takes the taint off while a
cluster has no workers.
*/}}
{{- define "kubehenge.controlPlaneTaints" -}}
nodeTaints:
  - node-role.kubernetes.io/control-plane:NoSchedule
{{- end -}}

{{/*
A ClusterClass's control-plane and bootstrap templates, which its distribution's providers validate.
Those run only while a cluster uses the class, so the controller applies the templates from this
ConfigMap once they do, rather than Helm here. Call with (dict "class" <ClusterClass name>
"namespace" <its namespace> "templates" (list (dict "apiVersion" … "kind" … "name" … "spec" <rendered
spec>) …)).
*/}}
{{- define "kubehenge.classTemplates" -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .class }}
  namespace: {{ .namespace }}
  labels:
    kubehenge.dev/class: {{ .class }}
data:
  templates.yaml: |
    {{- range $i, $t := .templates }}
    {{- if $i }}
    ---
    {{- end }}
    apiVersion: {{ $t.apiVersion }}
    kind: {{ $t.kind }}
    metadata:
      name: {{ $t.name }}
      namespace: {{ $.namespace }}
    spec:
      {{- $t.spec | nindent 6 }}
    {{- end }}
{{- end -}}

{{/*
The leader election of a control plane's controller manager and scheduler, as flag=value lines:
those of the management cluster's (management-cluster/cluster-api.tf). With their defaults (15 s,
renewed within 10 s) an etcd without a leader for that long, as a VM stalled on its Proxmox VE host
can leave it, has them exit. k3s runs both in its own process, so that takes the node's etcd member
down with them; with one control plane being replaced, etcd has two voting members, each needing
the other, and the exit on one node ends the other's quorum too, until both are back.
*/}}
{{- define "kubehenge.patientLeaseArgs" -}}
- leader-elect-lease-duration=60s
- leader-elect-renew-deadline=40s
- leader-elect-retry-period=10s
{{- end -}}

{{/*
kube-vip as a static pod on each control-plane node, holding the control-plane endpoint by ARP for
whichever node leads. Returned as the template of a JSON patch value that adds the bootstrap file.
Call with (dict "root" . "path" <static pod manifest file> "kubeconfig" <admin kubeconfig on the node>).
*/}}
{{- define "kubehenge.kubeVipFile" -}}
path: {{ .path }}
owner: root:root
permissions: "0600"
content: |
  apiVersion: v1
  kind: Pod
  metadata:
    name: kube-vip
    namespace: kube-system
  spec:
    hostNetwork: true
    hostAliases:
      - ip: 127.0.0.1
        hostnames: [kubernetes]
    containers:
      - name: kube-vip
        image: {{ .root.Values.kubeVip }}
        imagePullPolicy: IfNotPresent
        args: [manager]
        env:
          - name: address
            value: {{ "{{ .controlPlaneEndpoint | quote }}" }}
          - name: port
            value: "6443"
          - name: vip_arp
            value: "true"
          - name: cp_enable
            value: "true"
          - name: cp_namespace
            value: kube-system
          - name: svc_enable
            value: "false"
          # kube-vip serves its metrics without authentication, on every address by default.
          - name: prometheus_server
            value: 127.0.0.1:2112
          - name: vip_leaderelection
            value: "true"
          # kube-vip gives up on each API request after 10 s; a renew deadline of 10 s would let one
          # request hung on a stalled etcd drop the address. 25 s leaves room for a second attempt,
          # at the cost of up to 30 s before another node takes over from a dead one.
          - name: vip_leaseduration
            value: "30"
          - name: vip_renewdeadline
            value: "25"
          - name: vip_retryperiod
            value: "3"
          - name: vip_nodename
            valueFrom:
              fieldRef:
                fieldPath: spec.nodeName
        securityContext:
          capabilities:
            add: [NET_ADMIN, NET_RAW]
            drop: [ALL]
        volumeMounts:
          - name: kubeconfig
            mountPath: /etc/kubernetes/admin.conf
            readOnly: true
    volumes:
      - name: kubeconfig
        hostPath:
          path: {{ .kubeconfig }}
          type: File
{{- end -}}
