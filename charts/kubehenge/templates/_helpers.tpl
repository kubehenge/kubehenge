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
Call with "tags", the tags of the template line to select, which stand in until the controller's
patch names the build each machine is cloned from (the cluster's kubehenge.dev/node-image), and
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
Call with (dict "controlPlaneDiskGiB" <n> "workerDiskGiB" <n> "minDiskGiB" <n> "workerMinCores" <n> "workerMinMemoryMiB" <n> "clusters" .Values.clusters "sysAudit" <whether clusters may hold Sys.Audit on />).
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
        volumeSnapshots:
          type: boolean
          default: false
          description: >-
            Take VolumeSnapshots of the cluster's persistent volumes, restore them and clone
            volumes. A proxy beside the plugin makes each copy with the cluster's own Proxmox VE
            token, as an unused disk of one of the cluster's holder VMs, and only of the cluster's
            own volumes. Only on storage local to each node (ZFS, LVM, LVM-thin, directory or
            btrfs), and a copy stays on its volume's node. Off, the snapshot components go, and
            the plugin reaches Proxmox VE directly again once no copy is left.
- name: ccm
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: The Proxmox VE cloud controller manager, which every cluster runs for its nodes.
      {{- /* A default at the top as well, since CAPI adds a missing variable only from one there. */}}
      {{- if .sysAudit }}
      default:
        haGroups: true
      {{- end }}
      properties:
        haGroups:
          type: boolean
          default: {{ .sysAudit }}
          description: >-
            Give the cluster's Proxmox VE token Sys.Audit on /, unpropagated, so that the cloud
            controller manager labels each node group.topology.proxmox.sinextra.dev/<group> for
            each Proxmox VE HA group, or node-affinity HA rule, its Proxmox VE node is in. The
            token may then also read the rest of the Proxmox VE cluster's own configuration
            (firewall, backup jobs, HA, corosync), but nothing of its guests, storages or nodes.
            On by default where Kubehenge's administrator lets clusters hold it, and only there.
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
256 MiB, the Alloy that reads the events and a node's Alloy. Backups are Velero's server and a
node's agent, which grow while a backup reads a volume. Argo CD is its application, ApplicationSet
and repository controllers, its server and Redis; Flux is the operator and Flux's four controllers.
*/}}
{{- define "kubehenge.addonMemory" -}}
kubehenge.dev/addon-memory: '{"headlamp":64,"monitoring":1536,"logs":512,"backups":384,"kyverno":384,"istio":384,"clusterMesh":128,"loadBalancer":128,"certManager":192,"rancher":2048,"rancherAgent":512,"argocd":512,"flux":384}'
{{- end -}}

{{/*
The memory, in MiB, each add-on adds to what each control plane uses itself, where the API server
runs as a pod of its own (kubeadm and RKE2; k3s has figures of its own): what the end-to-end tests
saw the highest working set of a control plane so far grow by as the add-on was switched on, the
more of kubeadm's and RKE2's, with a quarter on top as the node figures have, in steps of 8. RKE2's
working set dipped where the node reclaimed its caches, so it takes kubeadm's figures where those are
higher. It is mostly the API server holding the add-on's resource types and objects, and the
add-on's pods on the control planes. Flux's stayed within what the working set varies by. Rancher's
own cluster used 409 MiB more than an idle k3s control plane, and a cluster registered with it no
more than that, so Rancher's agent counts nothing here.
*/}}
{{- define "kubehenge.addonControlPlaneMemory" -}}
kubehenge.dev/addon-control-plane-memory: '{"headlamp":8,"monitoring":32,"logs":56,"backups":184,"kyverno":552,"istio":288,"certManager":48,"rancher":512,"argocd":432}'
{{- end -}}

{{/*
The CPU, in millicores, each add-on's pods request in a cluster, the same for every distribution:
what its charts ask for at their pinned versions with the values Kubehenge gives them, counting the
pods that run once and one node's share of those on every node. The scheduler places a pod only on
a node with that much CPU not yet requested, however idle the node is. Kyverno's four controllers
ask for 100m each. Istio is istiod's 100m, and a node's CNI agent's 100m and ztunnel's 200m. Logs
are Loki's 55m with its proxy, and 10m for each Alloy; backups are Velero's server's 50m and a
node's agent's 20m. Monitoring is its metrics proxies at 5m each, a node's and a control plane's;
its other pods ask for none. The load balancer is kube-vip's cloud provider at 50m, MetalLB asking
for none. Flux is its operator and four controllers at 10m each, Headlamp 10m, Rancher its Turtles
controller. cert-manager, Argo CD, the clustermesh-apiserver, Rancher's own pods and its agent ask
for none.
*/}}
{{- define "kubehenge.addonCPU" -}}
kubehenge.dev/addon-cpu: '{"headlamp":10,"monitoring":20,"logs":75,"backups":70,"kyverno":400,"istio":400,"clusterMesh":0,"loadBalancer":50,"certManager":0,"rancher":10,"rancherAgent":0,"argocd":0,"flux":50}'
{{- end -}}

{{/*
The S3 stores an administrator set for backups, which a cluster's backups variable names one of:
the interface offers them and the controller reads them here.
*/}}
{{- define "kubehenge.backupTargets" -}}
kubehenge.dev/backup-targets: {{ .Values.backupTargets | toJson | quote }}
{{- end -}}

{{/*
Whether the controller restores etcd through the VMs' guest agents, which the interface offers
then; otherwise it shows the steps to restore by hand.
*/}}
{{- define "kubehenge.etcdRestore" -}}
kubehenge.dev/etcd-restore-automatic: {{ .Values.proxmox.etcdRestore | toString | quote }}
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
        OIDC, and Headlamp itself has no rights in the cluster, unless access is view. Reached
        through Kubehenge's interface, or at hostname when one is set.
      properties:
        enabled:
          type: boolean
          default: false
        access:
          type: string
          enum: [token, view]
          default: token
          description: >-
            token: everyone signs in with their own token or through OIDC. view: Headlamp's own
            service account is bound to the view ClusterRole and Headlamp uses it for everyone,
            so whoever reaches Headlamp sees the cluster read-only without signing in (no
            Secrets), and a token given to it is not used.
        hostname:
          type: string
          format: hostname
          description: Also serve Headlamp at this name through the cluster's ingress controller.
        tlsSecret:
          type: string
          minLength: 1
          description: >-
            Secret in the headlamp namespace holding the certificate for hostname. When unset, the
            certificates add-on's while it is on, else the ingress controller's own default.
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
            Secret in the monitoring namespace holding the certificate for hostname. When unset, the
            certificates add-on's while it is on, else the ingress controller's own default.
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
- name: backups
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Velero backing up the cluster's resources and volumes on a schedule to one of the S3
        targets an administrator set, under a prefix of the cluster's own, and restoring them. What
        it stores is encrypted with keys of the cluster's own, which Kubehenge keeps beside the
        Cluster in the <cluster>-backup-keys Secret: the objects by the store (SSE-C), the volumes'
        data by Kopia. kube-system, which holds the cluster's Proxmox VE token, and the add-ons'
        namespaces are left out. Velero reads every volume as root from the privileged velero
        namespace. Switched off, Velero goes and the backups stay in the target.
      properties:
        enabled:
          type: boolean
          default: false
        target:
          type: string
          minLength: 1
          maxLength: 63
          description: The administrator's target to back up to, by name.
        schedule:
          type: string
          minLength: 1
          maxLength: 128
          default: "0 2 * * *"
          description: When to back up, as a cron expression of five fields or a descriptor such as @weekly.
        timeZone:
          type: string
          default: UTC
          description: IANA time zone the schedule is read in, such as Europe/Berlin.
        retentionDays:
          type: integer
          minimum: 1
          maximum: 3650
          default: 7
          description: Days each backup is kept before Velero deletes it from the target.
        volumes:
          type: string
          enum: [all, annotated]
          default: all
          description: >-
            all backs up every volume of every pod but emptyDir ones; annotated only the volumes a
            pod names in its backup.velero.io/backup-volumes annotation.
        existingSecret:
          type: string
          minLength: 1
          maxLength: 253
          description: >-
            Secret in the workload cluster's velero namespace with the store's keys as Velero's
            credentials file, under the key cloud, which never passes through the management
            cluster; unset, the credential typed into the interface for the cluster, or else the
            one for every cluster.
        readFrom:
          type: array
          maxItems: 10
          items:
            type: string
            minLength: 1
            maxLength: 63
          description: >-
            Clusters of the same namespace, backing up to the same target, whose backups this one
            may restore. Velero gets a read-only location named cluster-<name> for each, with that
            cluster's SSE-C key, so whoever is cluster-admin here can read what those backups hold.
            The volumes' data comes back only when this cluster's keys were made while it read
            that cluster's backups, as it then shares that cluster's Kopia password.
      x-kubernetes-validations:
        - rule: "!self.enabled || has(self.target)"
          message: Backups need a target.
- name: etcdSnapshots
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Snapshots of the cluster's etcd, which holds every object of the cluster but its volumes'
        data, on a schedule to one of the S3 targets an administrator set, beside the cluster's
        Velero backups in an etcd folder of its own. Kubehenge's controller takes each snapshot
        through the cluster's API server with a client certificate it signs for itself, encrypts
        it with age to a key of the cluster's own in the <cluster>-backup-keys Secret, and stores
        it with the credential typed into the interface for the cluster, or else the one for every
        cluster; no key or credential enters the cluster. Restoring one brings back the cluster's
        objects as they were, for a cluster that lost its etcd quorum or had its objects deleted
        or damaged.
      properties:
        enabled:
          type: boolean
          default: false
        target:
          type: string
          minLength: 1
          maxLength: 63
          description: The administrator's target to store the snapshots in, by name.
        schedule:
          type: string
          minLength: 1
          maxLength: 128
          default: "0 */12 * * *"
          description: When to take a snapshot, as a cron expression of five fields or a descriptor such as @daily.
        timeZone:
          type: string
          default: UTC
          description: IANA time zone the schedule is read in, such as Europe/Berlin.
        keep:
          type: integer
          minimum: 1
          maximum: 1000
          default: 14
          description: How many snapshots to keep; the oldest beyond are deleted from the target.
        distributionUpload:
          type: boolean
          default: false
          description: >-
            RKE2 and k3s only: have the cluster's servers also take snapshots themselves on the same
            schedule, read in UTC, and upload them to the target's <distribution>-etcd folder of the
            cluster, keeping as many, as etcd holds them and not encrypted by Kubehenge. For it the
            controller puts the target and its access key into the Secret kubehenge-etcd-s3 in the
            cluster's kube-system, where whoever may read Secrets there reads the key and with it
            everything the key reaches in the target. Switching it, or changing the schedule or keep
            while it is on, replaces the control-plane machines.
      x-kubernetes-validations:
        - rule: "!self.enabled || has(self.target)"
          message: etcd snapshots need a target.
        - rule: "!has(self.distributionUpload) || !self.distributionUpload || self.enabled"
          message: The distribution's own upload takes the target, schedule and keep of etcd snapshots, which are off.
- name: podSecurity
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        The Pod Security Standard the cluster's pods are held to, by Kubernetes' Pod Security
        Admission, by Kyverno, or by both. The namespaces of the components that need the host
        (kube-system, Calico's, node-exporter's, Alloy's node collectors', Velero's, Istio's node
        agents', MetalLB's, Rancher's and its agents') are exempt from Pod Security Admission in
        every cluster, and from Kyverno where the cluster runs them.
      properties:
        admission:
          type: string
          enum: [privileged, baseline, restricted]
          default: baseline
          description: >-
            The level Pod Security Admission enforces in every namespace that labels none of its
            own. Pods short of restricted are warned about and audited at every level. Changing it
            replaces the control-plane machines.
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
- name: certificates
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Certificates from cert-manager for the cluster's Ingresses, routes and Gateways. With DNS-01
        or the cluster's CA, a certificate for domain and every name one label beneath it is the one
        Traefik serves wherever no other is asked for, and Traefik's Gateway listens for HTTPS with
        it. An Ingress annotated cert-manager.io/cluster-issuer: kubehenge-ingress, or
        kubernetes.io/tls-acme: "true", gets its own certificate from the same issuer. Installs
        cert-manager too.
      properties:
        enabled:
          type: boolean
          default: false
        issuer:
          type: string
          enum: [acme-dns01, acme-http01, ca]
          default: acme-dns01
          description: >-
            Where certificates come from: an ACME server such as Let's Encrypt, proving the names
            through the DNS provider's API (acme-dns01) or over HTTP on port 80 (acme-http01, which
            gives no default certificate), or a CA of the cluster's own (ca), whose certificate the
            interface hands out for clients to trust.
        domain:
          type: string
          format: hostname
          maxLength: 253
          description: >-
            The domain of the default certificate, which covers it and *.domain. The cluster's CA
            signs names in it alone, and a new domain makes a new CA.
        acme:
          type: object
          properties:
            email:
              type: string
              format: email
              description: The ACME account's contact, told of certificates about to expire.
            server:
              type: string
              pattern: "^https://"
              description: The ACME server's directory; Let's Encrypt's when unset.
            staging:
              type: boolean
              default: false
              description: Let's Encrypt's staging directory, whose certificates no browser trusts.
            caBundle:
              type: string
              minLength: 1
              description: >-
                PEM of the CA that signed the ACME server's own certificate, for a server whose
                certificate the system's CAs do not vouch for.
          x-kubernetes-validations:
            - rule: "!has(self.server) || !self.staging"
              message: Staging is Let's Encrypt's own directory; it cannot go with another server.
        dns01:
          type: object
          properties:
            provider:
              type: string
              enum: [cloudflare, route53, digitalocean, azuredns, clouddns, rfc2136, acmedns, akamai]
              description: >-
                The DNS provider whose API sets the challenge's record. Its credential is the one
                stored for the cluster in the interface, unless existingSecret or sharedCredential
                says otherwise: Cloudflare's or DigitalOcean's API token, Route 53's secret access
                key, the Azure service principal's client secret, the Google service account's JSON
                key, the TSIG secret, or the ACME-DNS account's JSON.
            existingSecret:
              type: string
              minLength: 1
              maxLength: 253
              pattern: "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$"
              description: >-
                A Secret in the workload cluster's cert-manager namespace holding the credential
                under the key token; Akamai's under clientToken, clientSecret and accessToken.
            sharedCredential:
              type: boolean
              default: false
              description: >-
                Take the DNS-01 credential an administrator stored for every cluster rather than
                one of the cluster's own.
            nameservers:
              type: array
              maxItems: 8
              description: >-
                The DNS servers (host:port) cert-manager checks the challenge's record with, for a
                domain whose answers inside the cluster differ from the public ones.
              items:
                type: string
                pattern: "^[^\\s]+:[0-9]{1,5}$"
            route53:
              type: object
              properties:
                region:
                  type: string
                  minLength: 1
                accessKeyID:
                  type: string
                  minLength: 1
                hostedZoneID:
                  type: string
                  minLength: 1
            azureDNS:
              type: object
              properties:
                subscriptionID:
                  type: string
                  minLength: 1
                resourceGroupName:
                  type: string
                  minLength: 1
                tenantID:
                  type: string
                  minLength: 1
                clientID:
                  type: string
                  minLength: 1
                hostedZoneName:
                  type: string
                  minLength: 1
                environment:
                  type: string
                  enum: [AzurePublicCloud, AzureChinaCloud, AzureUSGovernmentCloud]
            cloudDNS:
              type: object
              properties:
                project:
                  type: string
                  minLength: 1
                hostedZoneName:
                  type: string
                  minLength: 1
            rfc2136:
              type: object
              properties:
                nameserver:
                  type: string
                  pattern: "^[^\\s]+:[0-9]{1,5}$"
                  description: The DNS server (host:port) that takes the dynamic update.
                tsigKeyName:
                  type: string
                  minLength: 1
                tsigAlgorithm:
                  type: string
                  enum: [HMACMD5, HMACSHA1, HMACSHA256, HMACSHA512]
                  default: HMACSHA256
            acmeDNS:
              type: object
              properties:
                host:
                  type: string
                  pattern: "^https://"
            akamai:
              type: object
              properties:
                serviceConsumerDomain:
                  type: string
                  minLength: 1
          x-kubernetes-validations:
            - rule: "!has(self.existingSecret) || !self.sharedCredential"
              message: The credential comes from an existing Secret or from the one shared by every cluster, not both.
            - rule: "!has(self.provider) || self.provider != 'akamai' || has(self.existingSecret)"
              message: Akamai's three secrets come only from an existing Secret.
            - rule: "!has(self.provider) || self.provider != 'akamai' || (has(self.akamai) && has(self.akamai.serviceConsumerDomain))"
              message: Akamai needs its service consumer domain.
            - rule: "!has(self.provider) || self.provider != 'route53' || (has(self.route53) && has(self.route53.region) && has(self.route53.accessKeyID))"
              message: Route 53 needs its region and access key ID.
            - rule: "!has(self.provider) || self.provider != 'azuredns' || (has(self.azureDNS) && has(self.azureDNS.subscriptionID) && has(self.azureDNS.resourceGroupName) && has(self.azureDNS.tenantID) && has(self.azureDNS.clientID))"
              message: Azure DNS needs its subscription, resource group, tenant and client IDs.
            - rule: "!has(self.provider) || self.provider != 'clouddns' || (has(self.cloudDNS) && has(self.cloudDNS.project))"
              message: Google Cloud DNS needs its project.
            - rule: "!has(self.provider) || self.provider != 'rfc2136' || (has(self.rfc2136) && has(self.rfc2136.nameserver) && has(self.rfc2136.tsigKeyName))"
              message: RFC 2136 needs its nameserver and TSIG key name.
            - rule: "!has(self.provider) || self.provider != 'acmedns' || (has(self.acmeDNS) && has(self.acmeDNS.host))"
              message: ACME-DNS needs its host.
      x-kubernetes-validations:
        - rule: "!self.enabled || self.issuer == 'acme-http01' || has(self.domain)"
          message: DNS-01 and the cluster's CA need a domain.
        - rule: "!self.enabled || self.issuer != 'acme-dns01' || (has(self.dns01) && has(self.dns01.provider))"
          message: DNS-01 needs a DNS provider.
- name: gitops
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        GitOps: Argo CD or Flux keeps the cluster in step with a directory of a Git repository,
        applying what it holds and removing what goes from it. Switching it off, or to the other
        engine, removes the engine and leaves what it deployed running.
      properties:
        enabled:
          type: boolean
          default: false
        engine:
          type: string
          enum: [argocd, flux]
          default: argocd
          description: >-
            Argo CD, with a web interface that signs in through the cluster's first OIDC provider,
            or Flux, installed by the Flux Operator, whose web interface is optional.
        repository:
          type: object
          required: [url]
          properties:
            url:
              type: string
              maxLength: 2048
              pattern: "^(https|ssh)://[^\\s]+$"
              description: >-
                The repository, as https://host/path or ssh://user@host[:port]/path; ssh:// is
                read with a deploy key or a Secret of your own.
            revision:
              type: string
              maxLength: 255
              pattern: "^[A-Za-z0-9._/-]+$"
              default: main
              description: A branch, or a full ref such as refs/tags/v1.
            path:
              type: string
              maxLength: 1024
              pattern: "^[A-Za-z0-9._/-]+$"
              default: "."
              description: >-
                The directory synced, with plain manifests or a kustomization; the repository's
                root by default.
          x-kubernetes-validations:
            - rule: "!self.url.matches('^https://[^/]*@')"
              message: A token goes into the cluster's gitops credential, not into the URL.
            - rule: "!has(self.path) || !self.path.matches('(^|/)[.][.](/|$)')"
              message: The path stays inside the repository.
        auth:
          type: string
          enum: [none, deployKey, token, existingSecret]
          default: none
          description: >-
            How the engine reads the repository: none for a public one over HTTPS; deployKey, a key
            pair Kubehenge makes, whose public half you add to the repository as a read-only
            deploy key; token, a user name and token over HTTPS, stored as the cluster's gitops
            credential; or existingSecret, a Secret of your own in the engine's namespace.
        existingSecret:
          type: string
          minLength: 1
          maxLength: 253
          description: >-
            With auth existingSecret: for Flux, a Secret in flux-system as a GitRepository's
            secretRef takes it (identity and known_hosts, or username and password); for Argo CD,
            a repository Secret in argocd labelled argocd.argoproj.io/secret-type=repository for
            this URL.
        knownHosts:
          type: string
          maxLength: 16384
          description: >-
            The SSH host keys of the repository's server, as known_hosts lines (ssh-keygen -F or
            the forge's published keys), at least one for its host. Required for a repository
            reached over ssh://, but Flux's with credentials from a Secret of your own, which
            carries them: Kubehenge never reads them from the network.
        hostname:
          type: string
          format: hostname
          description: Serve the engine's web interface at this name through the cluster's ingress controller.
        tlsSecret:
          type: string
          minLength: 1
          description: >-
            Secret in the engine's namespace holding the certificate for hostname. When unset, the
            certificates add-on's while it is on, else the ingress controller's own default.
        admins:
          type: array
          maxItems: 64
          description: >-
            Groups of the cluster's first OIDC provider that administer Argo CD; nobody else has
            any rights in it. Flux's web interface acts as the signed-in user, with the cluster's
            own RBAC.
          items:
            type: string
            minLength: 1
            maxLength: 256
            pattern: "^[^,\\s]+$"
        signIn:
          type: object
          description: >-
            How the web interface signs in through the cluster's first OIDC provider: as that
            provider's client unless clientID names another, without a client secret, using PKCE,
            unless one is given. Argo CD's local admin stays disabled.
          properties:
            clientID:
              type: string
              minLength: 1
              maxLength: 256
            clientSecret:
              type: string
              enum: [none, stored, existingSecret]
              default: none
              description: >-
                none for a public client; stored, the cluster's oidc-client credential; or
                existingSecret, a Secret of your own in the engine's namespace with the key
                clientSecret, labelled app.kubernetes.io/part-of=argocd for Argo CD.
            existingSecret:
              type: string
              minLength: 1
              maxLength: 253
          x-kubernetes-validations:
            - rule: "!has(self.clientSecret) || self.clientSecret != 'existingSecret' || has(self.existingSecret)"
              message: A client secret from a Secret of your own needs the Secret's name.
        ui:
          type: boolean
          default: false
          description: >-
            Flux's web interface, at hostname: it shows every Flux object and the workloads they
            deployed, and lets those signed in reconcile, suspend and resume them, with the rights
            the cluster's RBAC gives them. It signs in with a client secret.
      x-kubernetes-validations:
        - rule: "!self.enabled || has(self.repository)"
          message: GitOps needs a repository.
        - rule: "!has(self.repository) || !has(self.auth) || self.auth != 'deployKey' || self.repository.url.startsWith('ssh://')"
          message: A deploy key reads the repository over ssh://.
        - rule: "!self.enabled || !has(self.repository) || !self.repository.url.startsWith('ssh://') || (has(self.knownHosts) && size(self.knownHosts) > 0) || (has(self.auth) && self.auth == 'existingSecret' && has(self.engine) && self.engine == 'flux')"
          message: A repository reached over ssh:// needs its host's keys in knownHosts.
        - rule: "!has(self.repository) || !has(self.auth) || !(self.auth in ['none', 'token']) || self.repository.url.startsWith('https://')"
          message: Without a key, the repository is read over https://.
        - rule: "!has(self.auth) || self.auth != 'existingSecret' || has(self.existingSecret)"
          message: Credentials from a Secret of your own need the Secret's name.
        - rule: "!has(self.ui) || !self.ui || (has(self.engine) && self.engine == 'flux' && has(self.hostname))"
          message: The web interface is Flux's, and needs a hostname.
        - rule: "!has(self.ui) || !self.ui || (has(self.signIn) && has(self.signIn.clientSecret) && self.signIn.clientSecret != 'none')"
          message: Flux's web interface signs in with a client secret.
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
{{ "{{- $talos := false }}" }}
{{ .root.Files.Get "files/authentication-config.tpl" }}
{{- end -}}

{{/*
The API server's admission configuration as a bootstrap file at path, a template Cluster API
renders with the cluster's variables, the chart's exempt namespaces written into it. Call with
(dict "root" $ "path" …).
*/}}
{{- define "kubehenge.admissionFile" -}}
path: {{ .path }}
{{ include "kubehenge.exempt" .root.Values.podSecurityExempt }}
{{ "{{- $talos := false }}" }}
{{ .root.Files.Get "files/admission.tpl" }}
{{- end -}}

{{/*
The line in front of files/admission.tpl that hands it every namespace of podSecurityExempt as
$exempt, whatever the cluster runs: the exemptions are part of a file on the control-plane nodes,
and ones that followed the add-ons would replace those nodes whenever an add-on is switched.
*/}}
{{- define "kubehenge.exempt" -}}
{{- $all := list }}
{{- range $by, $namespaces := . }}{{ $all = concat $all $namespaces }}{{ end }}
{{- printf "{{- $exempt := list %s }}" (include "kubehenge.quotedList" (uniq $all)) }}
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
The Go memory limit of a control plane's API server, as a Cluster API template over the cluster's
variables (.share is the percentage of the control plane's memory). Go collects its heap only once
it has grown to twice what the last collection left, so the API server, holding about 900 MiB, sat
at 2.2 to 2.4 GiB, and as every add-on was removed at once a 4096 MiB control plane ran out of
memory and stalled without an OOM kill. Under the limit it collects sooner instead, as it nears it.
*/}}
{{- define "kubehenge.apiServerMemoryLimit" -}}
{{ printf "{{ div (mul .controlPlaneMachine.memoryMiB %d) 100 }}MiB" .share }}
{{- end -}}

{{/*
Keeps an RKE2 or k3s server (.distribution) down once its etcd member is removed. The control-plane
providers remove the member before the machine's VM goes, and the server, still running, leaves a
tombstone; started again, it wipes its database and joins as a new member, which the VM's deletion
then takes away as a voter: beside a single new server, etcd loses its quorum. Cluster API never
adds a removed machine back, so nothing needs it to rejoin. A unit condition would not hold it:
systemd tests conditions only on a start it is asked for, not when Restart= starts the unit again,
so the check runs before every start instead.
*/}}
{{- define "kubehenge.removedServerFile" -}}
- path: /etc/systemd/system/{{ ternary "rke2-server" "k3s" (eq .distribution "rke2") }}.service.d/10-kubehenge-removed.conf
  owner: root:root
  permissions: "0644"
  content: |
    [Service]
    ExecStartPre=/bin/sh -c '! test -e /var/lib/rancher/{{ .distribution }}/server/db/etcd/tombstone'
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

{{/*
The Gateway values of the Traefik RKE2 and k3s bring: its Gateway's HTTP listener takes routes
from every namespace.
*/}}
{{- define "kubehenge.traefikGatewayValues" -}}
gateway:
  listeners:
    web:
      namespacePolicy:
        from: All
{{- end -}}

{{/*
The Secret in kube-system whose values.yaml Kubehenge's controller keeps in step with the
cluster's certificates, merged over the HelmChartConfig's own values: the default certificate and
the Gateway's HTTPS listener. helm-controller upgrades Traefik when it changes, and installs it
only once the Secret is there.
*/}}
{{- define "kubehenge.traefikValuesSecrets" -}}
valuesSecrets:
  - name: kubehenge-traefik
    keys: [values.yaml]
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

{{- define "kubehenge.etcdUploadEnabled" -}}
{{ "{{ with .etcdSnapshots }}{{ if and .enabled .distributionUpload }}true{{ end }}{{ end }}" | quote }}
{{- end -}}

{{/*
The drop-in that has RKE2's or k3s's servers upload their own etcd snapshots, with the target read
from the Secret Kubehenge's controller keeps in kube-system: given any other etcd-s3 setting, the
servers ignore that Secret, and the store's keys would have to be written here, into the
bootstrap data. The retention, an etcd-snapshot setting, counts each server's local snapshots and
the uploads of all servers together.
*/}}
{{- define "kubehenge.etcdUploadFile" -}}
path: {{ .path }}
owner: root:root
permissions: "0600"
content: |
  etcd-s3: true
  etcd-s3-config-secret: kubehenge-etcd-s3
  etcd-snapshot-schedule-cron: {{ "{{ .etcdSnapshots.schedule | quote }}" }}
  etcd-snapshot-retention: {{ "{{ .etcdSnapshots.keep }}" }}
{{- end -}}

{{/*
The kubelet settings cis adds, as kubelet flags.
*/}}
{{- define "kubehenge.cisKubeletArgs" -}}
- seccomp-default=true
- streaming-connection-idle-timeout=5m
{{- end -}}

{{/*
The operating system of an RKE2, k3s or kubeadm cluster's nodes. files/node-oses.json lists the node
OSes Kubehenge builds templates of; a unit test holds it to the families in pkg/templates.
*/}}
{{- define "kubehenge.nodeOSVariable" -}}
{{- $oses := .Files.Get "files/node-oses.json" | fromJson }}
- name: nodeOS
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Operating system of the nodes, a family and one of its releases, as Kubehenge builds its VM
        templates: the family's own signed packages and only those a node needs, SELinux enforcing
        with the distribution's policy, and for AlmaLinux and Rocky Linux an image-mode system whose
        /usr is read-only. Changing it replaces every node with a clone of the new node OS's
        template, one at a time as in an upgrade; that template has to be built first.
      required: [family, release]
      default: {{ toJson $oses.default }}
      properties:
        family:
          type: string
          enum: {{ toJson $oses.families }}
        release:
          type: string
          minLength: 1
      x-kubernetes-validations:
        - rule: "(self.family + '-' + self.release) in {{ toJson $oses.available | replace "\"" "'" }}"
          message: "a release of the family Kubehenge builds templates of: {{ join ", " $oses.available }}"
{{- end -}}

{{/*
Automatic Kubernetes upgrades and node image refreshes, which Kubehenge's controller starts; no patch
reads it.
*/}}
{{- define "kubehenge.upgradeVariables" -}}
- name: upgrades
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        Upgrade Kubernetes automatically, and move the nodes to new builds of their VM template,
        inside a maintenance window. Kubehenge's controller takes the newest release the mode
        allows, one minor version at a time and never past the distribution's stable channel, and
        starts only when the cluster is healthy and no rollout is running. A started rollout
        finishes even if the window ends first.
      properties:
        mode:
          type: string
          enum: ["off", patch, minor]
          default: "off"
          description: patch stays on the cluster's minor version; minor also moves to the next one.
        nodeImages:
          type: boolean
          default: true
          description: >-
            Replace the nodes inside the window with clones of the newest build of their VM
            template, which carries their node OS's updates, once that build is a day old, whatever
            mode says. Nodes are replaced one at a time, as in an upgrade. Talos nodes take their
            updates by upgrading Talos instead.
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
The cluster autoscaler of a cluster with an autoscaled pool, which Kubehenge's controller runs; no
patch reads it. A pool is autoscaled by Cluster API's own annotations on it.
*/}}
{{- define "kubehenge.autoscalerVariable" -}}
- name: autoscaler
  required: false
  schema:
    openAPIV3Schema:
      type: object
      description: >-
        How the cluster autoscaler scales the cluster's autoscaled pools: those whose topology entry
        carries cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size and -max-size and no
        replicas. Kubehenge's controller runs it while the cluster has such a pool.
      properties:
        placement:
          type: string
          enum: [management, workload]
          default: management
          description: >-
            Where the autoscaler runs. management keeps it in the management cluster, and the
            cluster holds no credential of the management cluster. workload runs it on the
            cluster's control planes, sparing the management cluster its memory, and puts a
            management-cluster token inside the cluster that can read every cluster's machines in
            its namespace; the admission policy still lets it scale only this cluster's pools.
        expander:
          type: string
          enum: [least-waste, most-pods, random]
          default: least-waste
          description: Which pool grows when several could take the waiting pods.
        scaleDownDelayAfterAddMinutes:
          type: integer
          minimum: 1
          maximum: 1440
          default: 10
          description: How long after a scale-up the autoscaler waits before it removes any node.
        scaleDownUnneededMinutes:
          type: integer
          minimum: 1
          maximum: 1440
          default: 10
          description: How long a node stays unneeded before the autoscaler removes it.
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
    # It reads the node's own admin kubeconfig, whose SELinux type no confined container may read,
    # as the control plane's static pods beside it do. Categories mean nothing to an unconfined
    # type, and on k3s the runtime's change into new ones is refused under no_new_privs.
    securityContext:
      seLinuxOptions:
        type: spc_t
        level: s0
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
