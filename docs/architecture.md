# Architecture

Kubehenge assembles maintained upstream pieces into one tested whole for Proxmox VE. It writes as little of its own as it can. What it writes is the cluster templates, the add-on catalogue, the image pipeline, the first-run bootstrap and the interface.

## Why Cluster API

Cluster API is the Kubernetes project's own way to describe clusters as Kubernetes objects and have controllers create and keep them in that state. VMware's vSphere Kubernetes Service and OpenStack's Magnum both run on it. It gives Kubehenge, for free:

- **Several distributions over one infrastructure layer.** The distribution is a swappable bootstrap and control-plane provider over the same Proxmox VE layer.
- **Day-two operations.** Rolling upgrades with nodes replaced one at a time, scaling by changing a number, health checks that replace dead nodes, clean deletion.
- **GitOps.** Clusters are YAML, so they work from Git with any GitOps tool.

A Terraform/OpenTofu-only design would create clusters once and leave upgrades and node replacement to be rebuilt for every distribution.

## Components

```text
Kubehenge interface (the appliance, an LXC container on Proxmox VE)
        │ acts with a ServiceAccount of its own
        ▼
management cluster (small, Talos, on Proxmox VE VMs; runs Kubehenge and nothing else)
 ├─ Cluster API core, through the Cluster API Operator
 ├─ capmox                       infrastructure: clones Proxmox VE VM templates, sets CPU, memory
 │                               and disks, assigns addresses from IP pools
 ├─ bootstrap + control plane    kubeadm (Cluster API core) | RKE2 (Rancher) | k3s (k3s project) |
 │                               Talos (Sidero Labs)
 ├─ IP address management        in-cluster IP pools for node addresses
 ├─ Add-on Provider for Helm     installs each cluster's add-ons as Helm charts
 ├─ cert-manager                 certificates for the providers' webhooks
 ├─ Kubehenge ClusterClasses     one tested template per distribution
 ├─ Kubehenge controller         each cluster's Proxmox VE objects, add-ons, Rancher registration,
 │                               automatic upgrades, autoscalers
 └─ cluster autoscalers          one per cluster with an autoscaled pool, unless it runs in the
                                 cluster
        │
        ▼ creates and manages
workload clusters on Proxmox VE VMs
 ├─ always: Proxmox CSI plugin, Proxmox cloud controller manager, control-plane virtual IP, CNI,
 │          Traefik, the Gateway API
 └─ chosen: Headlamp, monitoring, logs, backups, etcd snapshots, pod security, CIS hardening,
            Istio, Cilium Cluster Mesh, load balancer, certificates, GitOps, Rancher
```

## Management cluster

The management cluster is an appliance. It runs Cluster API, its providers and Kubehenge, and no workloads of anyone's. Monitoring, Rancher and everything else users run go into the clusters it creates. Keeping it bare keeps it small and keeps its attack surface to what Kubehenge needs. It also means losing a workload cluster never touches the piece that repairs it.

### Operating system

It runs Talos Linux, an immutable operating system with no shell or SSH. Talos is configured and upgraded only through its API. That suits a machine nobody should log into.

One control-plane node is enough for a lab. Three make it highly available, with Talos's own virtual IP in front of the API server. Control-plane nodes also run Kubehenge's controllers, so there are no separate workers.

Each build of a release's installer and disk image by the Talos Image Factory has different bytes. Neither can therefore be pinned by a digest that stays valid. The nodes pull the installer, Sidero's images and everything from registry.k8s.io only with their publishers' keyless signatures (Talos's `ImageVerificationConfig`). They pull the Talos providers' images only with the signature of their fork's release workflow at a version tag. The disk image is downloaded over TLS alone, since the factory publishes no checksum or signature for it.

### Pod network

Its pod network is Talos's Flannel, kube-network-policies (which enforces NetworkPolicies) and kube-proxy. The chart lets nothing reach a pod in `kubehenge-system` or `kubehenge-autoscaler` except two things. One is the controller's webhook and lifecycle hooks on port 9443, which the API server and Cluster API call. The other is its metrics on port 8443, which answer only accounts holding `kubehenge-metrics-reader`. The token Jobs ([below](#tokens-kept-out-of-the-controller)) reach only the cluster's DNS and Proxmox VE's API port.

Everything else keeps its outbound connections. The controller and the autoscalers reach Proxmox VE, the clusters' API servers, S3 stores and Git servers wherever they are.

### Cluster API providers

Cluster API's providers are installed by the Cluster API Operator. Each provider and its version is a Kubernetes object that the Terraform/OpenTofu configuration declares, so upgrading a provider is changing a version number.

The operator's own chart is published only in a Helm repository, whose index Helm checks no archive against, and as a release download. Kubehenge's release build downloads that archive and checks it against the SHA-256 `versions.tf` names. It ships the archive beside the module, which checks it again before installing it.

The operator does not download a provider itself. A hook of the providers' release (`kubehenge manifests`) downloads each provider's metadata and components from its release. It checks them against the SHA-256 `versions.tf` names for that version. It stores them in the ConfigMap the provider's `fetchConfig` selects.

Each provider runs its release's own image, by the digest `versions.tf` names, with two exceptions. The first is capmox v0.9.1. Kubehenge's release builds it from a later upstream commit. That commit carries a fix kubeadm upgrades need. The build adds fixes of Kubehenge's own (`capmox_build` in `management-cluster/versions.tf`).

The second is the Add-on Provider for Helm v0.6.4, with a fix that keeps it from creating a cluster's release twice (`helm_addon_build`). Both are built reproducibly, so the published images can be rebuilt from their sources and compared ([releasing.md](releasing.md)). A release of each that carries its fixes replaces the build.

Talos's two providers come from Kubehenge's own forks of them, [cluster-api-bootstrap-provider-talos](https://github.com/kubehenge/cluster-api-bootstrap-provider-talos) and [cluster-api-control-plane-provider-talos](https://github.com/kubehenge/cluster-api-control-plane-provider-talos). Sidero Labs' releases are on Cluster API's v1beta1 contract, which Cluster API's core removes. Its control-plane provider has no `TalosControlPlaneTemplate`, which a ClusterClass needs. The forks carry both, with fixes of their own. They are maintained primarily for Kubehenge and open to anyone who runs Talos with Cluster API.

Their releases are installed like any other, their files and images by the digests `versions.tf` names. Each fork's release workflow signs its image keylessly and attests the build provenance of its image and files. `make verify-providers` checks all of it in CI before a new pin is merged.

`capmox` signs in to Proxmox VE with an API token of its own, created for it with only the privileges it needs. cert-manager's chart is pulled from its OCI registry by digest. The operator's image is named by digest.

### Provider controllers

Every distribution's providers are installed, but their controllers run only while a cluster uses that distribution. Without one, Kubehenge's controller scales their Deployments to none. A management cluster that only ever builds RKE2 clusters therefore spends no memory on kubeadm's, k3s's or Talos's controllers. The eight take about 50 MiB each, about 400 MiB in all.

When the first cluster of a distribution is created, the controller scales them up. Once they serve, it applies the distribution's control-plane and bootstrap templates, which their webhooks have to admit. Cluster API holds the new cluster until then, which adds a minute or so to the first cluster of each distribution. When the last cluster is gone, the controller removes the templates, then scales the providers back to none.

The controller may only change the providers' replica count, through the Deployments' `scale` subresource. The operator leaves that count alone because the configuration removes it from the providers' manifests.

### etcd backup

The appliance backs up the management cluster's own etcd from outside the cluster. A restore then does not depend on what it restores.

The appliance asks a control-plane node's Talos API for an etcd snapshot on the schedule set under **Administration, Backups**. It uses a talosconfig of the `os:etcd:backup` role. It checks etcd's own checksum at the end of the stream. It encrypts the snapshot to an age key and uploads it to the chosen backup target. The key's private half is stored only in the target, encrypted with the state passphrase.

A restore downloads and decrypts the snapshot on the appliance and checks it against the SHA-256 recorded with it. With the admin talosconfig from the state, it wipes etcd on every control-plane node. It then uploads the snapshot to the first node, bootstraps etcd from it and waits for the others to join ([install.md](install.md#back-up-the-management-clusters-etcd)).

## ClusterClasses

One ClusterClass per distribution: kubeadm, RKE2, k3s and Talos. Each takes the same small set of variables:

- Kubernetes version
- control-plane and worker counts and sizes
- Proxmox VE nodes, storage and bridge
- node addresses
- control-plane virtual IP
- add-ons

Creating a cluster is a few lines whichever distribution it uses.

Each class wires in the always-installed set and passes the end-to-end suite before it is called supported ([testing.md](testing.md)). They ship in the `kubehenge` Helm chart (`charts/kubehenge`), which the management cluster's configuration installs. The chart creates each ClusterClass and its Proxmox VE templates directly. Its control-plane and bootstrap templates go into a ConfigMap named after the class, which the controller applies while a cluster uses the distribution.

### Talos class

The Talos class takes one variable more, `talosVersion`, the Talos release the nodes run. Its control planes hold the endpoint with Talos's own virtual IP rather than kube-vip. Its machine configuration is the strategic patches the Talos bootstrap provider applies to what it generates. They hold the same audit policy, admission configuration, authentication configuration and the long leader-election leases ([Control-plane rolls](#control-plane-rolls)) as the other classes. They also hold serving certificates the cluster CA signs for the kubelets, and image signatures required as on the management cluster.

The controller's external patch sets the configuration's `talosVersion` to the minor version of the release the machine boots first. Each machine is then generated in the schema its first release reads. Talos keeps SELinux as it ships it.

### Control-plane rolls

Cluster API replaces a control-plane node by adding its successor first, in every distribution. kubeadm's and RKE2's control-plane providers remove the old node first only when there are three control planes or more. k3s's never does. Removing a single control plane first would take etcd's only voting member.

A cluster with one control plane therefore runs on two etcd members during a roll, each needing the other. That lasts from the moment the new one is promoted until the old one is removed. Its API stops while either stalls for longer than etcd's election timeout.

k3s runs etcd in one process with the controller manager and the scheduler. They exit once they miss their leases' renewal. The node's etcd member goes with them, and so does the other's quorum. The classes therefore give the control planes' controller managers and schedulers the management cluster's leases (60 s, renewed within 40 s).

Kubehenge does the same for every controller it installs that exits on a missed renewal. These are the Cilium operator, the cloud controller managers and the CSI plugin's sidecars. An etcd stall of up to 40 seconds then passes without a restart.

A cluster whose second member is gone for good does not recover by itself. Kubehenge's controller therefore watches every such roll. Once the new machine has not been ready for 20 minutes, it sets the condition `ControlPlaneRollStalled` on the Cluster, with whether the API still answers. The way out is a reset of etcd to the kept machine's own database through the same machinery as an etcd restore ([troubleshooting.md](troubleshooting.md#one-control-plane-its-roll-stalled)).

RKE2's and k3s's providers remove the old server's member while that server still runs. A removed RKE2 or k3s server joins etcd again as a new member when it next starts. Its VM's deletion would then take that member away as a voter. Their servers' units therefore do not start once etcd has left its tombstone in their data directory.

### Certificate renewal

Certificates of a node last a year in kubeadm, RKE2 and k3s, and are renewed on a new machine. RKE2 and k3s renew them on a restart too, from 120 days before. The kubeadm class's control plane is replaced 60 days before its certificates run out (`rollout.before.certificatesExpiryDays`).

Kubehenge's controller replaces the other nodes 60 days before their certificates run out too. It learns when from RKE2's and k3s's own certificate monitor on each node.

It rolls a workers' pool through the topology's `rollout.after`. It rolls k3s's control plane through its own `rolloutAfter`, which the topology does not set for it. An RKE2 control plane of three or more is replaced one machine at a time by its health check (`cluster.x-k8s.io/remediate-machine`). RKE2's control-plane provider has no field to ask for a roll.

It sets the condition `CertificatesExpiring` on the Cluster while anything runs out within 60 days. It watches the kubeconfig and talosconfig kept for each cluster as well. Talos renews its nodes' certificates itself.

### VM health

Cluster API sees a node's VM only through the node. A VM paused by hand, or by QEMU after a failed disk write, keeps its machine ready until the node's lease runs out. A full storage is not seen at all.

Kubehenge's controller therefore reads Proxmox VE's resource list every minute with its own token. The token holds `VM.Audit` on the workload pool and `Datastore.Audit` on the clusters' storages. For a started VM, that list gives the state QEMU reports, such as paused or a disk error.

The controller sets `VMsNotRunning` on a Cluster whose joined machine's VM has been stopped, paused or crashed for 3 minutes. It sets `StorageNearlyFull` while the cluster's storage is 85% full on a node its VMs run on ([troubleshooting.md](troubleshooting.md#vms-that-do-not-run-storage-nearly-full)). It reports and changes nothing. Whether to resume, start or replace a VM is the administrator's call.

## Each cluster's own corner of Proxmox VE

A workload cluster runs a cloud controller manager and a CSI plugin that talk to the Proxmox VE API. A cluster therefore holds Proxmox VE credentials. Those credentials reach that cluster's VMs and volumes and nothing else, so a compromised cluster cannot touch its neighbours or the management cluster.

Within the cluster, only the cloud controller manager and the CSI plugin's controller hold them. Both run on the control-plane nodes only, so no worker ever has them. The cloud controller manager uses the nodes' own network, so it initialises new nodes before any CNI runs.

- **A pool per cluster**, nested under the workload pool: `<workload pool>/<namespace>.<cluster>`. Cluster API's Proxmox VE provider creates the cluster's VMs in it.
- **A user and API token per cluster**, with rights on its own pool only.
  - The token reads its VMs for the cloud controller manager, and configures their disks for the CSI plugin.
  - A group all clusters' users belong to adds what they share: using space on the workload storages and reading the nodes.
  - None of them can delete arbitrary volumes or change a storage's definition, unless the cluster runs the CSI plugin's newest release (below).
  - None of them reads beyond its pool and the nodes, except the Proxmox VE cluster's own configuration, for the HA group labels (below).
- **A VM that owns the cluster's volumes**, in the cluster's pool and never started. The CSI plugin creates volumes under its VMID, so only this cluster's token may attach them.
  - Deleting the cluster deletes its volumes with it.
- **Deleting a volume through its owner VM.**
  - The CSI plugin deletes a volume through the storage.
  - Proxmox VE allows that only with `Datastore.Allocate` on the whole storage, which covers every volume on it and its definition.
  - Kubehenge's controller instead deletes the volume of each released persistent volume through the owner VM, as an unused disk of that VM.
  - It uses the cluster's own token. The plugin then finds the volume gone and completes.
  - Proxmox VE deletes a VM's unused disk on the node the VM is on. For a volume on storage local to another node, the controller first moves the owner VM there.
  - The owner VM has no disks, so only its configuration moves.
- **The CSI plugin's release.** Every cluster runs the plugin's last release that works with `Datastore.AllocateSpace`.
  - Its newer releases read the storage's configuration, which takes `Datastore.Allocate`.
  - Where the management cluster allows it (`cluster_datastore_allocate`), a cluster's `csi` variable can choose the newest release.
  - The controller then grants the cluster's user a role on `/storage/<storage>`, unpropagated. The role holds `Datastore.Allocate`, `Datastore.AllocateSpace` and `Datastore.Audit`.
  - It takes the role back when the variable is switched off.
  - The admission webhook and `BeforeClusterCreate` allow that only on a storage the cluster has alone. That excludes three kinds of storage:
    - Any of the management cluster's (passed to the controller by the chart).
    - Any the VM templates have disks on (recorded every 30 seconds on the ClusterClasses as `kubehenge.dev/template-storages`).
    - Any another cluster uses.
  - They also refuse any cluster on a storage where another cluster holds the grant.
  - The controller checks the same on every reconcile. When a check fails it takes the grant back, installs the earlier release and records a `DatastoreAllocateRefused` event.
- **HA group labels.** The cloud controller manager labels each node with the HA groups, or HA node-affinity rules, of its Proxmox VE node.
  - Proxmox VE lists those only with `Sys.Audit` on `/`.
  - Unless the management cluster switches it off (`cluster_sys_audit`), the module creates a group holding `Sys.Audit` on `/`, unpropagated. The ClusterClasses default each cluster's `ccm` variable to the labels.
  - The controller adds the cluster's user to that group and takes it out when the variable is switched off. That lets the token read the Proxmox VE cluster's own configuration (firewall rules, backup jobs, HA, the corosync configuration) and change nothing.
  - The token reads nothing of the guests, storages or nodes it has no rights on otherwise. Proxmox VE lets only `Permissions.Modify` on `/` grant anything on `/`, so the controller holds `User.Modify` on that one group instead.
  - Where the management cluster switched it off, the variable defaults to off. The admission webhook and `BeforeClusterCreate` refuse it switched on.
  - The controller records a `SysAuditRefused` event when it cannot grant it.

### Volume snapshots

The CSI plugin copies a volume for a snapshot, a restore or a clone with `POST /nodes/{node}/storage/{storage}/content/{volume}`.

- Proxmox VE lets only `root@pam` call that.
- A token can make the same copy another way, by importing the volume into a new disk of a VM (`import-from`). That takes `VM.Clone` on the VM the volume belongs to, `VM.Config.Disk` on the VM it goes into and `Datastore.AllocateSpace` on the storage.
- Every cluster's role on its own pool holds `VM.Clone` for that. A cluster's `csi` variable can ask for snapshots on storage local to each node.
- No password of anyone's is involved.
- **The proxy.** The controller runs the plugin's newest release with its snapshotter sidecar.
  - Beside the plugin's controller, as a native sidecar, it runs `kubehenge csi-proxy` from Kubehenge's own image, pinned by digest.
  - The plugin's configuration for it, `kube-system/kubehenge-proxmox-proxied`, holds the cluster's own token and points at the proxy on `127.0.0.1:8006`.
  - The proxy passes every call on to Proxmox VE with the token it carries. It verifies Proxmox VE's certificate against the cluster's CA.
  - It answers three kinds of call itself.
    - **A copy.** The proxy makes it with the cluster's token.
      - It first checks four things:
        - The source is a volume of the cluster's volume owner VM or one of its holder VMs. The plugin names such volumes `vm-<vmid>-…` (below `<vmid>/` on file-based storages).
        - The copy's name is the plugin's for the owner.
        - The copy stays on the source's node.
        - No volume of that name is there.
      - It records the copy as a ConfigMap in the namespace `kubehenge-volume-copies` before anything changes.
      - It imports the volume into a free SCSI slot of a holder VM on the source's node and waits on Proxmox VE's task.
      - It detaches the new disk so that it stays as an unused disk of the holder.
      - It records the copy done with its real volume ID, `vm-<holder>-disk-<n>`, which Proxmox VE chooses.
    - **Deleting a copy.** The proxy does it through the holder (`delete=unusedN`), as the token may not delete storage content.
    - **A storage's configuration.** Proxmox VE shows it singly only to `Datastore.Allocate`, and the newest release reads it.
      - The proxy answers from the storage list.
  - Every other call goes on with the copies' names translated.
    - In the path, the query and a form body, the plugin's names become the real ones.
    - In every JSON answer, such as the storage's content, VM configurations and tasks, the real names become the plugin's again.
    - Names are matched as whole volume IDs, so `vm-101-disk-1` is never found inside `vm-101-disk-10`.
  - A record counts only when it names two things.
    - A copy of the owner.
    - A real volume of one of the cluster's holder VMs on the same storage.
  - A record written by anything else translates and deletes nothing.
  - A Role lets only the plugin's controller account read and write the records, and record events in `kube-system`.
- **Restarts and retries.** The record is written before Proxmox VE is asked.
  - It holds the holder, the slot, the volumes the holder had and, once started, the task.
  - A proxy that restarts, or the plugin's next call after a sidecar's 5-minute timeout, finds the copy from the record.
  - It waits on the same task and takes the holder's one new volume as the copy.
  - It deletes any other volume an interrupted try left, so a volume is never copied twice.
  - Copies run one at a time for each cluster, which also keeps two from holding a holder's configuration at once.
  - A copy under way is not listed under the plugin's name until it is done. The plugin therefore never takes a half-written copy for a finished one.
  - The proxy logs each copy and deletion. Each refusal or failure is a `VolumeCopyFailed` event on the proxy's pod.
- **Holder VMs.** Proxmox VE names a copy after the VM it is a disk of and lets a VM have 256 unused disks.
  - The controller therefore gives a cluster that takes snapshots a holder VM on each node its VMs may be placed on.
  - Holders are created like the volume owner VM: stopped, without disks of their own, in the cluster's pool, named `<cluster>-<digest>-copies` and tagged `volume-copies`.
  - The controller adds another holder on a node once that node's holders have 128 copies each. The proxy fills each up to 255.
  - Holders never move, since moving a VM copies its unused disks along. They stay while any copy is left.
  - Switching snapshots off removes the empty holders once no copy is recorded. Deleting the cluster destroys them with their copies.
  - The controller's deletion of released volumes ([above](#each-clusters-own-corner-of-proxmox-ve)) leaves the volumes of restored and cloned claims to the proxy. The proxy deletes them through their holders when the plugin asks.

Volume snapshots bring the VolumeSnapshotClass `proxmox`. So do the external snapshotter's CustomResourceDefinitions and snapshot controller, except on RKE2, which runs its own.

The admission webhook and `BeforeClusterCreate` allow volume snapshots only on a storage of a type the plugin copies on, local to each node. The storages are published every 30 seconds on the ClusterClasses as `kubehenge.dev/snapshot-storages`. The controller checks the same on every reconcile. It leaves the plugin reaching Proxmox VE directly and records a `VolumeSnapshotsRefused` event when a check fails.

Switching volume snapshots off removes those releases and, once no copy is left, the proxy, its configuration and the copies' namespace. The CustomResourceDefinitions stay, so that VolumeSnapshots left behind keep their volumes.

### Created before the VMs

Kubehenge's controller, in the management cluster, creates all of this before Cluster API creates any VM. Cluster API asks it through a lifecycle hook (`BeforeClusterCreate`, Cluster API's Runtime SDK) and waits until it answers. The controller hands the token to the cluster through a ClusterResourceSet. It installs the cloud controller manager and CSI plugin through the Add-on Provider for Helm. It removes the pool, user and volume owner once the cluster's VMs are gone.

Removing the owner VM deletes its volumes on shared storage and on its own node. The controller deletes those on other nodes' local storage first, and waits while a node that may hold some is offline.

While a cluster is being deleted, the controller lists every object of it whose finalizer holds the deletion. It looks among the objects Cluster API and the providers keep in the cluster's namespace. It records the list on the Cluster, with what each object reports. A deletion that can never finish is abandoned through the same controller. The controller may therefore list, patch and delete those objects ([troubleshooting.md](troubleshooting.md#a-cluster-does-not-finish-deleting)).

Its own token may create pools under the workload pool, users in the clusters' group and ACLs on those pools. It may also move and configure the VMs in the workload pool and use the workload storages as the clusters do. It may do nothing more.

With `cluster_datastore_allocate` it also holds `Datastore.Allocate` on the workload storages the management cluster does not use. That lets it grant a cluster's user the role there. Unless `cluster_sys_audit` is off, it holds `User.Modify` on the group holding `Sys.Audit` on `/`, so that it may add a cluster's user to it.

### Placement of the VMs

The controller also tells `capmox` where to put each cluster's VMs. Left to itself, `capmox` clones every VM onto the node holding its template when a cluster names no nodes. It places no VM at all while one node it names does not answer, which is when a cluster needs replacements most.

So the ClusterClasses leave the nodes to an external patch the controller serves (`GeneratePatches`). The patch uses the cluster's `allowedNodes` that are online, or every online node when it names none. Every 15 seconds the controller reads which nodes are online and records them on each Cluster (`kubehenge.dev/online-nodes`). That has Cluster API compute the topology again when the list changes.

### Release pins for RKE2 and k3s

The same patch has RKE2 and k3s nodes install their release from files they check, in place of their bootstrap providers' online install. That online install runs whatever install script and release a tag names on the day a node boots. Neither project signs its releases or images. Kubehenge therefore carries the SHA-256s of each release's checksum file and install script (`pkg/provisioner/releasepins.txt`). `make release-pins` writes the file from the newest five minor versions' releases.

A cluster runs no release it does not pin. The API server refuses one on a new cluster or a version change. The interface and auto-upgrades offer only the newest pinned release of each minor version.

A node downloads the script, the release and the airgap images of its core and its CNI. It checks each against the pinned checksum file and hands them to its bootstrap provider's air-gapped install. It pulls none of the distribution's images from a registry.

### Template records

Every 30 seconds the controller also records the Kubernetes versions its token finds templates for in the template pool. It records them on the kubeadm ClusterClass for each node OS (`kubehenge.dev/kubeadm-versions`, as JSON). On the RKE2 and k3s ClusterClasses it records the node OSes their templates are there for (`kubehenge.dev/node-os-templates`).

On all three it records the builds of every node OS template line (`kubehenge.dev/node-image-builds`) and the storages the templates have disks on (`kubehenge.dev/template-storages`). The same `GeneratePatches` reads the builds to point each machine template at one build ([Images](#images)). On the Talos ClusterClass, whose nodes run Talos rather than a node OS, it records the Talos releases it finds templates of (`kubehenge.dev/talos-templates`).

The interface offers those versions. It refuses a cluster whose template is missing. Cluster API clones with its own token. The people who create clusters therefore need no privilege on the template pool.

### HA node-affinity rules

Placing a cluster by one of Proxmox VE's HA node-affinity rules takes reading the rules, which needs `Sys.Audit` on `/`. Where an administrator has the rules offered to cluster creators ([install-settings.md](install-settings.md#ha-rule-placements)), the controller reads them every 30 seconds with its own token. That token holds the privilege already. The controller records each enabled rule's name and the nodes it allows in the ConfigMap `kubehenge-ha-rules`. It leaves out the rules it keeps for clusters' Proxmox VE HA.

The interface's account may read that ConfigMap only while the setting is on. The interface asks Proxmox VE with the user's own ticket first. When Proxmox VE refuses that user, the interface hands over the recorded names and nodes instead. It does so only for a user who holds `VM.Allocate` on the workload pool or a cluster's pool.

### Proxmox VE HA

Proxmox VE HA can be switched on ([install-settings.md](install-settings.md#proxmox-ve-ha)). The controller then also makes each joined control-plane VM of a cluster on shared storage an HA resource. It keeps two HA rules per cluster. One is a negative resource affinity among those VMs, sized to the nodes Proxmox VE's HA manager knows. The other is a strict node affinity to the cluster's nodes when it may use only some.

The controller writes the rules with a token of its own, since only `Sys.Console` on `/` may write HA configuration. Each write goes through a Job ([below](#tokens-kept-out-of-the-controller)), and the cluster tokens never see it. Two things follow from HA moving VMs that Cluster API's Proxmox VE provider does not expect:

- **A pre-terminate hook** (`pre-terminate.delete.hook.machine.cluster.x-k8s.io/kubehenge-ha`) on each control-plane Machine holds its VM's deletion until the controller has taken the VM out of HA. Proxmox VE refuses to delete an HA resource's VM.
- **A moved VM's node.** The provider deletes a VM on the node it recorded, and takes Proxmox VE's answer for a moved VM to mean it is gone. That would leave the VM running.
  - When a VM has moved, the controller therefore marks its ProxmoxMachine with the node it is on (`kubehenge.dev/proxmox-node`).
  - The provider then looks the VM up across the cluster and records its node. The hook stays until it has.

### Tokens kept out of the controller

The HA token amounts to a root shell on every Proxmox VE node. The restore token amounts to one in every cluster's VMs. The controller holds neither.

Each is in a Secret of its own beside the controller (`kubehenge-proxmox-ha`, `kubehenge-proxmox-restore`). Only a Job made from the chart's PodTemplate (`kubehenge-proxmox-ha`, `kubehenge-guest-agent`) mounts it. For each HA write or restore step the controller leaves the request in the Secret `<template>-input` and starts a Job from the template. It reads the result from its pod's termination message and deletes both.

The Job runs once, for at most its step's time, under a service account without API access. The ValidatingAdmissionPolicy `kubehenge-token-jobs` refuses any Job in the namespace that reaches either token's Secret by a volume, a projected volume or the environment. The exception is a Job whose pod is exactly its template's.

The HA Job checks each write against Proxmox VE first. It creates only HA resources of VMs with Kubehenge's comment. It creates, changes or removes only rules named and commented as Kubehenge's, over resources that carry its comment. Reads of HA state use the controller's own token, which holds `Sys.Audit` on `/`.

The controller may still read Secrets across the management cluster, as Cluster API's providers do. One that is taken over can read the tokens too. This removes the long-running process that holds them. It also removes any use of them through the controller beyond the writes and commands above.

## Images

Each distribution boots from a different VM template:

| Distribution | Template |
| --- | --- |
| kubeadm | the cluster's node OS with containerd, the kubelet and kubeadm preinstalled at one Kubernetes version. Its control-plane images are each checked against Kubernetes' release signature |
| Talos | Talos's own image of one release from the Talos Image Factory, with the QEMU guest agent. Talos is its own OS, so no node OS is chosen |
| RKE2, k3s | the cluster's node OS with the distribution's SELinux policy. The distribution installs at boot from checked files |

`kubehenge template create` fetches the image. Where the image needs preparing, it boots it once. The Administration page in the appliance does the same with an administrator's session. Either way, the image is then turned into a template `capmox` selects by its tags.

All of this goes through the Proxmox VE API. Details: [templates.md](templates.md).

### Node OSes

The operating system under RKE2, k3s and kubeadm is a node OS. Each cluster chooses it with its `nodeOS` variable. The choices are AlmaLinux 10 by default or Rocky Linux 10, both in image mode. openSUSE Leap 16.1 Immutable follows once openSUSE releases it.

Each node OS is a family in `pkg/templates`, one Go interface with its releases. Everything that differs between node OSes lives behind it. It covers where its image comes from and how its signature is checked. It covers the signing keys of the repositories a build adds. It also covers how a template is made of it (its recipe), the CPU type it needs at the least, and what its nodes enforce.

Every family enforces SELinux.

- **Image mode** (AlmaLinux, Rocky Linux). The build VM boots the family's cloud image and composes a minimal root file system from the family's signed repositories with `bootc-base-imagectl`.
  - It adds what a node of the distribution needs in a container build and checks the result with `bootc container lint`.
  - It installs the result with `bootc install` onto the template's own disk, which then boots once on its own for its first-boot checks.
  - The node's `/usr` is read-only, and its automatic updates are off. A new build of the template replaces the nodes instead.
- **Transactional** (openSUSE Leap Immutable). The build VM boots the image itself, installs what a node needs as one new snapshot with `transactional-update`, and reboots into it.
  - Its automatic updates and reboots are off for the same reason.
- **The node OS in the patch.** `GeneratePatches` points each machine template at the cluster's node OS's template line (the tag `os-<family>-<release>`).
  - It has RKE2 and k3s start with SELinux.
  - Clones keep their template's CPU type. Changing the variable rolls the nodes as an upgrade does.
  - The controller places a cluster only on the online nodes whose CPU reaches its node OS's level (read from each node's CPU flags). It places a Talos cluster on those that reach `x86-64-v2`, the CPU type of Talos's templates.
  - Its admission webhook refuses a node OS none of the cluster's nodes can run, or one that is not released. Talos clusters have no `nodeOS` variable, since Talos is its own OS.

### Node image refresh

A node OS template is a line of builds, each tagged with when it was made. Every week a CronJob in the management cluster (`kubehenge template refresh`) makes a new build of each line. The nodes get their node OS's updates by being replaced rather than patched in place, as Cluster API expects of immutable machines.

- **Which build a machine clones** is decided by the cluster's node image time. That time is the annotation `kubehenge.dev/node-image`, set to the cluster's creation time until it first moves.
  - `GeneratePatches` sets each machine template's tag selector to the newest build of its line made by then. That build must also be at least as old as the soak time (24 hours by default, `--node-image-soak`).
  - The same cluster therefore gets the same build on every reconcile, scale-up and repair. A new build changes nothing until the time moves.
- **The time moves** in two places.
  - The upgrader looks at every cluster every five minutes.
  - It sets the time to now when the cluster is settled inside its maintenance window and a newer build has soaked. A cluster whose `upgrades.nodeImages` is false is skipped.
  - The `BeforeClusterUpgrade` hook sets it too, so that any Kubernetes upgrade takes the newest builds with it. Cluster API then rolls the nodes as for an upgrade.
  - Outside the window, the upgrader records its decision to hold the cluster, the build that waits and when the window opens, in the annotation `kubehenge.dev/node-image-held`. The interface shows it.
- **The refresh** holds a Lease so that one run builds at a time. It builds each line and then deletes what nothing needs.
  - It deletes all but the two newest builds of a line. It keeps any build a cluster's node image time picks and any a ProxmoxMachine still waiting for its VM selects.
  - It writes how it went to the ConfigMap `kubehenge-template-refresh`, which the interface shows.
- **Build now and creators' requests** go through the ConfigMap `kubehenge-template-requests`. The interface may only write a request there.
  - The controller starts the CronJob's Job for it, with `--missing` for a template a cluster creator asked for.
  - The interface never holds the builder's token.
- **Its own token.** The refresh signs in to Proxmox VE with a user and token of its own.
  - They may build and delete VMs in the template pool and nothing else of the clusters' or the management cluster's ([install.md](install-settings.md#what-the-template-builds-may-do)).

### Talos upgrades

Node image refreshes do not apply to Talos nodes. Talos updates itself, so a Talos cluster's templates have no builds and its nodes are upgraded in place rather than replaced. Each release's template stands for a build of its minor version's line instead.

The refresh keeps the two newest releases of each minor. It also keeps any older one that:

- a cluster's new machines boot or its nodes are upgraded to
- a machine still being created selects
- someone asked for in that run

It deletes the rest.

- **Which template a machine clones** is the Talos release the cluster records for its new machines (`kubehenge.dev/talos-image`).
  - It is `talosVersion` when the cluster is created.
  - The `BeforeClusterUpgrade` hook moves it to the current `talosVersion` once that release's template is in the pool, since a Kubernetes upgrade replaces every machine anyway.
  - A new machine boots that release and is then upgraded like the others, so changing `talosVersion` never rolls the cluster.
- **The in-place upgrade.** The controller upgrades a node that runs another release than `talosVersion`.
  - It uses the Talos API with the cluster's talosconfig, one node at a time, control planes first.
  - It pulls the release's installer and has Talos install it to the inactive partition.
  - It drains the node, five minutes at most. A node whose pods do not all leave is rebooted anyway, with a warning event.
  - It reboots the node and uncordons it once it is Ready on the new release.
  - It deletes the DaemonSets' pods the node's kubelet rejected while it shut down. Otherwise they would keep their DaemonSets reading not ready for up to 15 minutes.
  - Talos moves a minor version at a time, so a jump of two goes through the newest release of the minor in between.
  - Progress and the last upgrade are recorded on the Cluster (`kubehenge.dev/talos-upgrading`, `kubehenge.dev/talos-upgraded`). The interface shows them.
- **A failed upgrade** stops the rest.
  - A node that does not come back Ready is recorded in `kubehenge.dev/talos-upgrade-failed`. So is one that comes back on its old release because Talos rolled back to the other partition.
  - The controller upgrades nothing more in the cluster until an operator removes the record (**Try again** on the cluster's page).
  - A stalled install (no word from the node for three minutes) is tried again. So is one that fails as an unreachable or busy node does.
  - The retries come after one, two and four minutes, and only then does it count as failed. Nothing on the node changes before the reboot.
  - A node whose machine Cluster API deletes mid-upgrade has its upgrade dropped, not failed. That happens when the cluster scales down onto it.
  - A machine being deleted is never picked.
- **Automatic patch upgrades.** Where the cluster's `upgrades.nodeImages` is on, the upgrader moves `talosVersion` to the newest patch release of its minor version inside the maintenance window.
  - Talos releases get no soak time. A new minor version is always an operator's choice.

## Add-ons

Add-ons are Helm charts installed per cluster by the Cluster API Add-on Provider for Helm, chosen when a cluster is created or later.

| Add-on | What |
| --- | --- |
| Rancher | runs in a cluster Kubehenge creates for it. Kubehenge registers its other clusters with it, so they appear in Rancher's interface |
| Headlamp | Kubernetes web UI per cluster |
| Monitoring | Prometheus, Alertmanager and Grafana per cluster, with kube-prometheus-stack's dashboards and alerts |
| Logs | Loki per cluster, fed by Grafana Alloy on every node or through the Kubernetes API, read in monitoring's Grafana |
| Backups | Velero backs up each cluster's resources and volumes to an S3 store an administrator set, encrypted with the cluster's own keys. It also restores them |
| etcd snapshots | the cluster's etcd taken by Kubehenge's controller into the same stores, encrypted with the cluster's own key. It is restored on the control-plane nodes |
| Pod security | a Pod Security Standard level enforced by Pod Security Admission, by Kyverno's policies, or both |
| CIS hardening | the rest of the CIS Kubernetes Benchmark, beyond the baseline every cluster gets, with RKE2's own CIS profile |
| Istio | Istio in ambient mode per cluster: mutual TLS between the pods of the namespaces enrolled, without sidecars |
| Cilium Cluster Mesh | connects the pod networks of clusters that run Cilium |
| Load balancer | addresses for Services of type `LoadBalancer`, with MetalLB or kube-vip ([Networking](#networking)) |
| Certificates | cert-manager with an ACME issuer (DNS-01 or HTTP-01) or a CA of the cluster's own. It adds a default certificate for Traefik and its Gateway |
| GitOps | Argo CD or Flux per cluster. It keeps the cluster in step with a directory of a Git repository |

Kubehenge's controller turns a cluster's add-on variables into HelmChartProxies beside the cluster. It deletes the proxy when an add-on is switched off, which uninstalls it. Values never carry a secret. The Add-on Provider for Helm keeps them in plain text.

Every chart is pinned by its digest, which a proxy's version carries as `<version>@sha256:<digest>`. Kubehenge's build of the Add-on Provider for Helm pulls an OCI chart by its manifest digest. It checks that the version's tag points to it. It checks a chart from an HTTP repository against the SHA-256 of its archive that the repository's index lists.

The images the charts run are pinned the same way. Each chart's entry in the controller chart's values names every image the chart runs by tag and digest. The controller merges those values under the ones it sets itself. An ImageSet named for its release, applied with Calico's Installation, pins the images the Tigera operator deploys Calico from.

### Headlamp add-on

Headlamp (`headlamp: {enabled: true}`) has no rights of its own. Its service account is bound to no role, and everyone signs in with their own token or through OIDC. Headlamp therefore shows each user exactly what their credentials allow.

`access: view` binds its service account to Kubernetes' built-in `view` ClusterRole through the chart's own ClusterRoleBinding. It starts Headlamp with `-unsafe-use-service-account-token`. Headlamp then sends every request with the account's mounted token, which the kubelet renews, and drops any token a user gives it. So everyone who reaches it reads what `view` allows (no Secrets, nodes or other cluster-wide objects but namespaces), and nobody changes anything through it. Switching it back to `token` removes the binding with the next Helm upgrade.

Headlamp is not exposed. `kubectl -n headlamp port-forward svc/headlamp 8080:80` reaches it from a workstation. `hostname` adds an Ingress on the cluster's ingress controller (RKE2 and k3s bring Traefik, and Kubehenge installs it into kubeadm and Talos clusters). The Ingress uses the certificate from `tlsSecret`, or one from the certificates add-on while it is on, or the controller's default.

Plugins are those in its image. Nothing is fetched at run time. Its pod meets Pod Security's restricted level.

### Monitoring add-on

Monitoring (`monitoring: {enabled: true}`) installs kube-prometheus-stack into the `monitoring` namespace, with cert-manager. Every scrape is authenticated and verifies its server. Nothing that serves metrics without authentication is opened beyond its node's loopback interface:

- **etcd, kube-proxy, the controller manager and the scheduler** serve their metrics on loopback only, as the CIS benchmark asks of the last two.
  - A kube-rbac-proxy sits in front of each, on the node's address and the component's own port. It takes only Prometheus's client certificate and asks the API server whether it may read `/metrics`.
  - etcd and kube-proxy serve plain HTTP. The controller manager and the scheduler serve HTTPS, which their proxies verify against the cluster CA for `localhost`, and let `/metrics` through on loopback without credentials.
  - On RKE2 their certificates come from the server CA.
  - On kubeadm they would be signed anew on every start by a CA of their own. The class therefore gives each control-plane node certificates from the cluster CA instead.
  - Prometheus gets no etcd client certificate, which would give it every key in the cluster.
  - On k3s every component runs in one process, whose kubelet already serves all their metrics. Prometheus takes them from the kubelet, as Rancher does, and the chart's alerts look for them there.
  - On Talos the controller manager and the scheduler serve with certificates they sign themselves. Talos keeps those inside their pods and gives no way to replace them, so no proxy could verify them.
  - Until Talos passes them a serving certificate of the cluster CA, they go unscraped, and their alerts with them.
  - The controller looks at their static pods' arguments. Once every one of them is passed `--tls-cert-file`, it scrapes them through proxies as on kubeadm, with no change to the class.
- **Kubelets** on kubeadm ask the cluster CA for their serving certificates.
  - Kubehenge's controller approves them when they name only the node and its Machine's addresses.
  - The API server verifies them too. RKE2 and k3s sign theirs with the server CA.
- **node-exporter** listens on loopback behind its chart's kube-rbac-proxy, which takes the same client certificate.
  - It reads the host's namespaces and file system, which only Pod Security's privileged level allows. It therefore has a namespace of its own labelled privileged.
  - The rest of the add-on meets the restricted level.
- **Certificates.** cert-manager runs a CA of the add-on's own (ClusterIssuer `kubehenge-metrics`).
  - It signs the proxies' serving certificates and Prometheus's client certificate, and nothing else.
  - The kube-prometheus-stack chart's long-lived token is sent only to targets that ask for one, never over plain HTTP.

Prometheus keeps ten days of metrics on a 20 GiB volume from the cluster's default storage class, Alertmanager its silences on 1 GiB. `hostname` serves Grafana through the cluster's ingress controller, with the certificate from `tlsSecret` or the controller's default. Without it, `kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80` reaches it. Its `admin` password is generated into the `kube-prometheus-stack-grafana` Secret. The release carries the proxies and certificates, so switching monitoring off removes them with it.

### Logs add-on

Logs (`logs: {enabled: true, collector: node|api, retentionDays: 7, sizeGiB: 10}`) need monitoring, whose Grafana reads them. The validating webhook and the interface refuse them without it, and refuse switching monitoring off under them.

Loki runs as one binary in the `logging` namespace. Its chunks and index are on a volume from the cluster's default storage class. The volume is kept when logs are switched off and only grows. Loki deletes logs by age, after `retentionDays`. Nothing deletes them by size, so `sizeGiB` has to hold the days kept.

Loki listens on loopback only, behind a kube-rbac-proxy on its pod's address. The proxy serves a certificate from the monitoring add-on's CA. It authenticates every client, by a client certificate from that CA or a token. It asks the API server whether the client may push (Alloy's user may) or read (Grafana's user may).

A NetworkPolicy lets only Alloy's and Grafana's pods reach it. Grafana gets Loki as a data source with its own client certificate.

Grafana Alloy collects the logs. One Alloy Deployment in `logging` meets the restricted level and keeps the cluster's events, which the API holds for an hour only. The pods' logs come from one of two collectors:

- **`node`**, the default, is a second Alloy release: a DaemonSet in `logging-node`. It is exempt from pod security, since it runs as root to read the host's files.
  - It mounts the pod log directory and the journal read-only, and the directory of the distribution's API server audit log. It keeps its read positions in a host directory so that a restarted collector neither loses nor repeats lines.
  - Reading the files on the node gives it the logs of pods that end within seconds and every unit's journal. On the control planes it gives the audit log too.
  - It holds no Kubernetes rights and no ServiceAccount token, since the log files' paths name the namespace, pod and container.
- **`api`**: the Deployment in `logging` reads the pods' logs through the API server too, with rights to list pods and read their logs.
  - It needs nothing of the host. It is lighter, but misses the journal, the audit log, and a pod that ends before it starts following it.

On Talos, the `node` collector reads no journal, because Talos keeps its services' logs in memory. The class has Talos send them, and the kernel's, as JSON lines over TCP to `169.254.51.70:5170` (`machine.logging.destinations` and a `KmsgLogConfig`).

That is a link-local address the class gives every node on a dummy link of its own (`kubehenge-logs`). The collector listens there through a host port.

Loopback would not work. The answers to a connection from `127.0.0.1` come back addressed to `127.0.0.1` through the pod's interface. The kernel accepts them only where the CNI allows it, as Calico and a bridge do and Cilium does not. Talos never takes a link-local address for the node's own.

The collector listens with Alloy's `otelcol.receiver.tcplog`. That component is experimental, so the collector runs at Alloy's `experimental` stability level.

A NetworkPolicy lets nothing else reach the port, so only the node's own Talos can send to it. The lines arrive under `job="talos"` with the service as `unit`, `kernel` for the kernel's.

Talos keeps the audit log in a directory only the API server's own user may enter. There the collector holds `DAC_READ_SEARCH`, its one capability, over what it mounts read-only.

Switching the collector installs or uninstalls the DaemonSet and changes the Deployment's configuration. Switching logs off uninstalls Loki and Alloy and takes Grafana's data source away.

### Backups add-on

Backups (`backups: {enabled: true, target: …}`) installs Velero with its AWS plugin into the `velero` namespace. It backs up on the cluster's schedule to one of the S3 targets an administrator set, under `<prefix>/<namespace>.<cluster>/velero`. The targets are the chart's `backupTargets`, which the ClusterClasses carry in the annotation `kubehenge.dev/backup-targets`. Velero's file-system backup reads every pod's volumes with Kopia, file by file. It works on any storage, without snapshots.

- **Keys of the cluster's own.** Kubehenge's controller makes the cluster's keys once, when backups or etcd snapshots are first switched on. It keeps them beside the Cluster in the Secret `<cluster>-backup-keys`:
  - An age identity its etcd snapshots are encrypted to (below).
  - A 32-byte SSE-C key. The plugin sends it with every request, and the store encrypts every object Velero writes with it.
  - The password of the cluster's Kopia repository, which encrypts the volumes' data before it leaves the node.
  - The keys never reach the store. Its operator, or anyone with an access key that reaches the bucket, reads nothing of a cluster's backups.
  - They are never changed, since the backups made with them can be read with nothing else.
  - The Cluster does not own their Secret. A cluster made again under the same name reads the backups of the one before.
  - Deleting the Secret makes them unreadable for good.
- **Put in before Velero.** The controller writes the keys and the store's access key into the workload cluster's `velero` namespace before the chart installs, never into values.
  - Velero would otherwise make its repository password itself, the same for every installation.
- **Access keys.** Velero signs in to the store with the cluster's own credential, typed into the interface.
  - With `existingSecret`, Velero uses a Secret the cluster's owner keeps in the `velero` namespace instead.
  - The controller reads credentials when it reconciles the cluster, with no watch on Secrets. A new or replaced one therefore reaches Velero within ten minutes.
  - The one an administrator stored for every cluster reaches every cluster's folder. Only the controller uses it, for the etcd snapshots of clusters without a key of their own.
  - It never goes into a cluster, whose admins could delete the other clusters' backups with it.
- **What is left out.**
  - Namespaces: `kube-system` (which holds the cluster's Proxmox VE token), `kube-public`, `kube-node-lease`, Velero's own namespace and those of the add-ons.
  - Kubehenge installs the add-ons again itself. Their restored copies would fight the releases that own them.
  - What each node, the API server and the CNI keep of themselves:
    - nodes, events and leases
    - CSR and volume attachments
    - Cilium's and Calico's addresses and identities
  - emptyDir volumes are skipped.
- **Rights.** Velero's server runs without root or capabilities, at Pod Security's restricted level, but as `cluster-admin`, which backing up and restoring every resource needs.
  - Its node agents find every pod's volumes through a hostPath of the kubelet's directory, unconfined by SELinux (`spc_t`).
  - Each volume is read by a privileged backup pod of its own on the pod's node. That is Velero's way past SELinux's separation of the pods' volumes.
  - The `velero` namespace is therefore exempt from pod security, as the CNIs' are. Its metrics, served without authentication, are off.
- **The interface's identities.** Two of the interface's identities in the cluster ([Interface](#interface)) live in the `velero` namespace, only while backups are on. Each has a Role over Velero's own resources and nothing else:
  - `kubehenge-backups` lists backups and restores and starts a backup, a `DeleteBackupRequest` or a `DownloadRequest` for a restore's results. It serves everyone who may see or change the cluster.
  - `kubehenge-restores` reads the backup locations and starts a restore. It serves those who may download the admin kubeconfig, since a restore writes whatever the backup holds, Secrets and RBAC among it.
  - Neither reads a Secret. A restore's results are in the store under the backup's SSE-C key.
  - The interface therefore fetches them itself from the link Velero hands out. It fetches only from the target's own endpoint, with the key from the management cluster.
- **Reading other clusters' backups.** For each cluster the `readFrom` field names, the controller adds a read-only backup location `cluster-<name>` at that cluster's prefix in the same target.
  - Its SSE-C key goes in the `velero` namespace.
  - The controller marks on the location whether the two clusters share a Kopia repository password, which Velero keeps one of for all its locations.
  - A cluster whose keys are made while it reads another's takes that cluster's password, so it restores the volumes' data too. For any other source, the interface asks only for the resources.
  - The validating webhook refuses a source that is not on the same target or whose keys Kubehenge does not have. A source taken out of `readFrom` takes its location, its Backup objects and its key out of the cluster.
- **Switching it off** removes the release, then Velero's resources with their finalizers taken off. It then removes its CRDs and the `velero` namespace with the keys in it.
  - The backups stay in the store and the keys beside the Cluster, so switching backups on again reads them.

### etcd snapshots add-on

etcd snapshots (`etcdSnapshots: {enabled: true, target: …}`) are taken by Kubehenge's controller itself, with nothing installed in the cluster. They go into the same targets under `<prefix>/<namespace>.<cluster>/etcd` ([clusters.md](clusters.md#etcd-snapshots)).

- **Reaching etcd.**
  - Every control-plane provider keeps the cluster's etcd CA beside the Cluster (`<cluster>-etcd`: kubeadm's etcd CA, RKE2's and k3s's etcd server CA). For each snapshot the controller signs a P-256 client certificate from it in memory, valid for an hour and never stored.
  - The controller reaches one member through the workload cluster's API server, as Cluster API's control-plane providers check etcd.
  - On kubeadm and RKE2 it uses a port-forward to the member's static pod. On k3s it uses one to the k3s provider's `etcd-proxy` pod, which forwards to the server's own etcd.
  - The member's serving certificate is checked against the same CA for `localhost`, a name every distribution's carries.
  - The API server sees only the forwarded bytes, which are TLS between the controller and etcd. No etcd key and no access key to the store reaches the workload cluster, unless the cluster opts into the distribution's own upload (below).
  - On Talos, etcd runs as a Talos service rather than a pod. The controller asks a control plane's Talos API for the snapshot, with the talosconfig the Talos bootstrap provider keeps beside the Cluster (`<cluster>-talosconfig`).
  - It records no revision for a Talos snapshot, which Talos does not report.
  - A Talos cluster is restored through its nodes' Talos API with its talosconfig. It needs no restore token, as Talos recovers etcd.
    - The controller reads the whole snapshot and checks it against its SHA-256 before anything changes.
    - It resets every control-plane node with its `EPHEMERAL` partition wiped, which takes etcd's data and keeps the machine configuration. It waits for each to boot again.
    - It streams the snapshot to the first node once its etcd waits to be bootstrapped. It checks the snapshot on the way and bootstraps etcd there from it.
    - The other nodes join the restored etcd by themselves, so no machine is replaced but one whose Talos API did not answer.
    - Once any node may have been wiped, only the snapshot brings etcd back. A restore that fails from there, or that the controller restarted in, is therefore tried once more from the start.
- **Streaming.** etcd's snapshot stream goes straight into an S3 multipart upload in parts of 8 MiB.
  - It is encrypted on the way with age to the cluster's identity. A database of any size therefore takes the same few buffers in the controller.
  - etcd ends the stream with the SHA-256 of what it sent, which the controller checks before keeping the object. One cut off is deleted.
  - The object's metadata then gets the database's SHA-256 and size, with the Kubernetes version, distribution and etcd revision.
  - The stream carries no revision, so the controller reads the member's from its own store just before the snapshot begins. The snapshot then holds that revision.
  - One snapshot runs at a time across clusters, each cluster up to 10 minutes after the scheduled time, at a delay of its own.
  - A failure is retried after 15 minutes, with a `Warning` event. The cluster's condition `EtcdSnapshotSucceeded` says why.
  - The newest `keep` stay. Older ones are deleted after each snapshot.
- **Restoring.** A restore is asked for with the annotation `kubehenge.dev/etcd-restore` on the Cluster.
  - The interface sets it for those with `Sys.Modify` on the cluster's pool. The restore is recorded step by step in `kubehenge.dev/etcd-restore-status`.
  - While it runs, the controller changes nothing else of the cluster and takes no scheduled snapshot of it. Cluster API is paused for it.
  - Before it stops or wipes any of etcd, the restore takes a snapshot of etcd as it is. It stores that snapshot in the same target and names it in its status.
  - A restore from the wrong snapshot can then be undone by another.
  - A cluster whose etcd does not answer, often the reason it is restored, is restored without one. The status says why.
  - The snapshot's SHA-256 comes from the store's metadata. It must be 64 lower-case hexadecimal digits, since it goes into commands run as root.
  - The steps run as root scripts through the QEMU guest agent, with the restore token ([install-settings.md](install-settings.md#etcd-restores)) only. Each step runs in a Job ([tokens](#tokens-kept-out-of-the-controller)).
  - The controller decrypts the snapshot. It stores it for the node encrypted again as `openssl enc -aes-256-ctr -pbkdf2` reads it, with a random password for that restore.
  - The controller has the node fetch it from a presigned link that expires within the hour. The node trusts the store by the target's CA where it has one.
  - The node checks the snapshot against the SHA-256 the snapshot was taken with and deletes the copy.
  - The restore ends once the cluster's API server answers on the cluster's address, not only on the restored node. That address moves by a leader election that lapses while etcd is stopped.
  - Each script reads all of itself before running, so a restart of the API server or the guest agent midway leaves no half-run step. Every step is safe to run again.
  - A restore that fails before etcd's database was swapped starts every control plane as it was. One that fails after finishes the restore, which a marker on the node tells apart.
  - A restore the controller restarted in is finished or undone by the step it had reached.
  - Without the restore token the interface writes out the same scripts as steps to run by hand.
- **The distribution's own upload.** On RKE2 and k3s, `distributionUpload: true` beside `enabled` also has the servers upload the snapshots they take themselves, to the same target under `<prefix>/<namespace>.<cluster>/<distribution>-etcd`.
  - A patch of the class writes a configuration drop-in, `60-kubehenge-etcd-s3.yaml`, in the distribution's `config.yaml.d`. It holds `etcd-s3`, the schedule, the number kept and `etcd-s3-config-secret: kubehenge-etcd-s3`.
  - The controller writes that Secret into the cluster's `kube-system`, of the type `etcd.k3s.cattle.io/s3-config-secret`. It holds the target's endpoint, bucket, folder, region, CA and access key, and TLS verification on.
  - The servers read the Secret at each snapshot, so a new key or target reaches them without a restart. The key never enters the bootstrap data.
  - CAPRKE2's `etcd.backupConfig` is not used. It puts the key into the bootstrap data, which lands on every server's disk and in its VM's configuration drive.
  - It also skips TLS verification unless told otherwise.
  - The k3s provider has no such fields, so both distributions get the drop-in.
  - `kubehenge.dev/etcd-upload-installed` on `<cluster>-backup-keys` records that `kubehenge-etcd-s3` was put into the cluster. The controller therefore removes it again when the upload goes off, and reaches no cluster that never had it.
  - It removes it only once no control-plane Machine, one being deleted included, may still run with the drop-in.
  - That means none whose bootstrap data writes it and none without bootstrap data yet.
  - A server not yet replaced in the roll reads the Secret on each scheduled snapshot.
  - The distribution reads the schedule in UTC, counts the number kept across all servers together, and stores the snapshots as etcd wrote them. Kubehenge neither encrypts, lists nor restores them.
  - The drop-in is part of the control plane's bootstrap. Switching the upload, or changing the schedule or number kept while it is on, therefore rolls the control-plane machines.

### Istio add-on

Istio (`istio: {enabled: true}`) installs Istio in ambient mode, without sidecars: istiod into `istio-system`, and ztunnel and istio-cni on every node into `istio-node`. A namespace or pod labelled `istio.io/dataplane-mode=ambient` joins the mesh. istio-cni redirects its pods' traffic through the ztunnel on their node. The ztunnel carries it to the other side's ztunnel over mutual TLS with each workload's identity. Istio's policies by identity therefore apply, such as a `PeerAuthentication` that requires mutual TLS or an `AuthorizationPolicy` by service account.

ztunnel and istio-cni reach into the nodes' network namespaces, which only Pod Security's privileged level allows, so `istio-node` is exempt from pod security. istiod meets the restricted level. With Cilium, Kubehenge's own or RKE2's, Cilium's `cni.exclusive` is off, so that Cilium leaves istio-cni's plugin in place. It stays off without Istio, since Cilium's agents read it only when they start and switching it would restart every agent.

Waypoints, Istio's layer 7 proxies, are Gateways of the class `istio-waypoint`, which istiod serves since every cluster has the Gateway API ([Networking](#networking)). Switching Istio off first removes the `istio.io/dataplane-mode` label from every namespace and pod. It waits until istio-cni has taken every pod's redirection away, since a pod still redirected once ztunnel has gone reaches nothing. Then it uninstalls the charts. Istio's CRDs stay, with any resources of them.

### Cilium Cluster Mesh add-on

Cilium Cluster Mesh (`clusterMesh: {enabled: true, peers: [...]}`) links clusters of one namespace that run Cilium, on any of the four distributions. These are Kubehenge's own Cilium on kubeadm, k3s and Talos, and RKE2's bundled one on RKE2. Kubehenge configures the bundled one through a `HelmChartConfig` the cluster's ClusterResourceSet applies.

Two clusters are linked while both have the mesh on and list each other, so neither can pull the other in. A link lets each cluster's pods reach the other's, by address and through global services. It also lets each read the other's pod names, labels and identities, which Cilium's network policies select by.

Every cluster that runs Cilium has, from the start, its own name and a Cilium cluster id unique in its namespace (the annotation `kubehenge.dev/cilium-cluster-id`). Both are fixed for its life, since changing either would restart every workload. Every new cluster also gets a pod range that no other Kubehenge cluster uses and that is clear of the nodes' network. Otherwise a packet for a peer's pod would be taken for a local one.

A link is refused, with a `ClusterMeshNotLinked` event that says why, while the ranges overlap, the ids clash or an endpoint is unknown.

With the mesh on, Cilium runs two components. The clustermesh-apiserver is the one peers reach on NodePort 32379 of the control-plane endpoint. KVStoreMesh caches the peers' state for the agents. Kubehenge's controller writes the rest into `kube-system`: the certificates, the peers' endpoints and the users peers sign in as.

Each namespace has a Cluster Mesh CA of its own, in the Secret `kubehenge-cluster-mesh-ca` beside its clusters, ECDSA P-256 and valid for ten years. Its key never leaves the management cluster, so no workload cluster can sign a certificate for another. The certificates it signs are valid for a year and renewed with a third of that left. Each peer signs in as a user of its own, which may read the cluster's state and nothing else.

A peer added or taken away changes those objects only, so nothing restarts. Switching the mesh on or off upgrades Cilium and restarts its agents. RKE2 with CIS hardening has a profile that lets only `kube-system`'s own pods reach the pods there. A NetworkPolicy there lets the peers through to the clustermesh-apiserver.

### Certificates add-on

Certificates (`certificates: {enabled: true, issuer: …, domain: …}`) installs cert-manager with its Gateway API support on. Like Traefik, it is held back until the cluster has the Gateway API. Kubehenge's controller writes the rest into the workload cluster with its admin kubeconfig once cert-manager answers:

- **The issuer** is the ClusterIssuer `kubehenge-ingress`. It is either ACME or a CA.
  - ACME is Let's Encrypt's or another server. It answers DNS-01 through one of cert-manager's DNS providers, or HTTP-01 through an HTTPRoute on Traefik's Gateway.
  - cert-manager's default issuer is set to it, so an Ingress annotated `kubernetes.io/tls-acme: "true"` gets its certificate there.
- **The default certificate**, with DNS-01 or the CA, is the Certificate `kubehenge-ingress` in `kube-system` for `domain` and `*.domain`, ECDSA P-256.
  - Its Secret is served by Traefik's default TLS store and its Gateway's `websecure` listener (HTTPS, port 8443 behind the nodes' 443).
- **How Traefik gets it.** On kubeadm and Talos both are values of the Traefik Kubehenge installs.
  - RKE2's and k3s's own Traefik reads them from the Secret `kubehenge-traefik` in `kube-system`. The cluster's ClusterResourceSet keeps that Secret in step, and the HelmChartConfig on the control planes names it.
  - The file there stays as it is, so no change of certificates replaces a node.
  - The add-ons' Ingresses take it where it covers their host name. They ask the issuer for a certificate of their own where it does not.
- **The cluster's CA**, with the issuer `ca`. The controller makes it in the management cluster, in the Secret `<cluster>-ingress-ca` beside the Cluster and owned by it.
  - It is ECDSA P-256 and valid for ten years.
  - It is name-constrained to `domain`, with every IPv4 and IPv6 address excluded. A client that trusts it therefore trusts no name outside the cluster's domain.
  - A copy goes to `cert-manager`, where the issuer reads it. It stays while the add-on is switched off and on, and a new domain makes a new one.
  - The interface hands out its certificate to whoever may see the cluster.
- **The DNS provider's credential** is a user credential (purpose `dns01`) the interface stores beside the Cluster. It may be the one stored for every cluster when the cluster sets `sharedCredential`, which needs `Sys.Modify`.
  - The controller copies it into `cert-manager` (`kubehenge-dns01`), never into a chart's values.
  - Secrets are not cached, so the interface stamps `kubehenge.dev/credentials-stored-at` on every Cluster using a credential when it stores or removes one. The controller takes the change at once.
  - `existingSecret` names a Secret of the cluster's own instead, which Kubehenge never reads.

The issuer records which CA or ACME server it stands for (`kubehenge.dev/issuer-identity`). When that changes, the controller deletes the Secret of every certificate the issuer issued, in any namespace. cert-manager then issues each again rather than keep one another CA signed.

The annotation `kubehenge.dev/certificates` marks a cluster while the workload cluster may hold these objects. Switched off, the controller deletes them and lifts the mark. cert-manager goes when neither monitoring nor Rancher needs it.

### GitOps add-on

GitOps (`gitops: {enabled: true, engine: argocd | flux, repository: {url: …, revision: …, path: …}}`) keeps a cluster in step with a directory of a Git repository. The engine applies what the directory holds, plain manifests or a kustomization, and removes what goes from it. Argo CD goes into `argocd`, Flux into `flux-system`. Both meet Pod Security's restricted level and come with NetworkPolicies of their own.

Once the engine's chart is in, Kubehenge's controller writes two things into the engine's namespace. They are the repository's credentials and the object that starts the sync. That object is an Argo CD Application `kubehenge`, or a FluxInstance `flux`. The Application syncs automatically, prunes, heals what is changed by hand and retries without end. It carries no resources finalizer.

- **Flux** is installed by the Flux Operator, from the Flux manifests inside the operator's image, at the version the controller chart names (`addons.fluxOperator.flux`).
  - The operator's digest pins Flux and every image it names. Nothing is fetched from a moving tag.
  - It runs Flux's source, kustomize, helm and notification controllers.
  - Like the operator, each requests 10m of CPU rather than Flux's 100m. That way Flux fits beside the other add-ons on a 2-vCPU RKE2 worker.
  - The Flux Operator is licensed under AGPL-3.0. Kubehenge runs it unmodified, as its project publishes it.
- **Reading the repository.** `auth` takes one of four values:
  - `none` for a public repository over HTTPS.
  - `deployKey`, an Ed25519 key pair the controller makes for the cluster and its repository and keeps in the Secret `<cluster>-gitops` beside the Cluster.
    - The interface shows its public half, to add to the repository as a read-only deploy key.
    - A new URL gets a new key, since a forge takes a deploy key for one repository only.
  - `token`, a user name and read-only token over HTTPS, the cluster's `gitops` credential.
  - `existingSecret`, a Secret of the owner's own in the engine's namespace.
- **Host keys.** Over SSH the engine trusts only the host keys in `knownHosts`, which a cluster has to give.
  - Flux reading with a Secret of the owner's takes them from that Secret.
  - The webhook and the ClusterClass's schema hold `knownHosts` to at least one line for the repository's host and port, plain, wildcarded or hashed.
  - Nothing reads a host's keys from the network, since whoever answered for the host on that first connection would be trusted from then on. The interface shows their fingerprints to compare.
  - The engine gets its copy of the credentials as `kubehenge-repository` in `argocd` or `kubehenge-gitops` in `flux-system`, never in values.
- **Argo CD's interface** signs in through the cluster's first OIDC provider, as its client or `signIn.clientID`.
  - The client is a public one with PKCE. With `signIn.clientSecret` it is a confidential one, whose secret is the cluster's `oidc-client` credential or a Secret of the owner's own.
  - Its local admin and its web terminal are off, and Dex and notifications are not installed.
  - Nobody has rights in it but the provider's groups in `admins`.
  - `hostname` serves it through the cluster's ingress controller. Without one, `kubectl -n argocd port-forward svc/argocd-server 8080:80` reaches it.
- **Flux's web interface** (`ui: true`) is the Flux Operator's, off by default. At `hostname` it shows every Flux object and the workloads they deployed, and lets those signed in reconcile, suspend and resume them.
  - It signs in through the cluster's first OIDC provider with a client secret, which the operator reads from a Secret.
  - It acts as the signed-in user and impersonates them with the provider's prefixes. The cluster's RBAC therefore decides what each may see and do.
  - Flux's NetworkPolicies take connections in `flux-system` only from the namespace itself. The controller therefore adds one, `kubehenge-acme-http01-solver`, that lets cert-manager's HTTP-01 solvers there be reached on their port, for the interface's certificate under `acme-http01`.
- **Rights.** The engine applies whatever the repository holds with cluster-wide rights, so whoever chooses the repository holds what the admin kubeconfig gives.
  - The interface asks `Sys.Modify` on the pool for it, as for the kubeconfig.
- **Switching** it off, or to the other engine, removes the engine and leaves what it deployed running.
  - For Argo CD the controller deletes the ApplicationSets and orphans their Applications. It then deletes the Applications without their finalizers, then the chart.
  - For Flux it deletes the FluxInstance, on which the operator uninstalls Flux, then the chart.
  - The engine chosen starts syncing only once the other has gone, so that two never fight over the same objects.
  - The engines' CRDs go with them. The deploy key and host keys stay beside the Cluster, for switching on again.

### Pod security add-on

Pod security (`podSecurity: {admission: …, kyverno: …}`) holds a cluster's pods to a [Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/), `baseline` or `restricted`, through either or both of two checks:

- **Pod Security Admission**, Kubernetes' own.
  - Every ClusterClass gives the API server an admission configuration. For every namespace that labels no level of its own, the default level is the cluster's `admission` value, `baseline` unless the cluster chooses another.
  - At every level, `privileged` included, it warns about and audits pods short of `restricted`.
  - The configuration is a file on the control-plane nodes, so changing the level replaces them one at a time.
- **Kyverno.** `kyverno` installs Kyverno and its Pod Security Standards policies into the `kyverno` namespace.
  - They hold every namespace to the level whatever its labels say. Pod Security Admission cannot, since anyone who may label a namespace may lift its level.
  - They deny a pod when Kyverno cannot be reached, and Kyverno's policy reports list what falls short. Switched off, the policies go first, then Kyverno.

Both leave alone the namespaces of the components that need the host's network, file system or privileges:

- `kube-system` (the cloud controller manager, the CSI plugin, Cilium, kube-vip and the distributions' own components)
- Calico's, node-exporter's and MetalLB's
- Velero's (`velero`, whose node agents read every pod's volumes)
- Istio's node agents' (`istio-node`) and Alloy's node collectors' (`logging-node`)
- those of Rancher's agents and controllers

The chart's `podSecurityExempt` lists them by what puts them there.

Pod Security Admission exempts all of them in every cluster. Its exemptions are part of the configuration file on the control-plane nodes. An exemption that followed the add-ons would replace those nodes whenever one is switched. A namespace exempt there gains nobody anything, since whoever may create or label a namespace may already set its level to `privileged`.

Kyverno, whose exclusions are objects in the cluster, exempts `kube-system` and the namespaces of only what the cluster runs. Under Kyverno nobody can therefore create, for example, a `velero` namespace in a cluster without backups and run privileged pods in it.

A workload of one's own that needs more than the level gets its namespace labelled. With Kyverno it also gets a PolicyException in the `kyverno` namespace. That is the only namespace Kyverno takes exceptions from, so only those who may write there can grant one.

### Security baseline

The security baseline of every cluster is the part of the [CIS Kubernetes Benchmark](https://www.cisecurity.org/benchmark/kubernetes) that costs workloads nothing:

- **Audit.** The API server logs who did what to which object, at the `Metadata` level.
  - It logs without request or response bodies, which would put Secrets and tokens into the log, and without health checks, leases and events.
  - It keeps a month of it, in ten files of 100 MB at most.
- **Secrets encrypted in etcd.** RKE2, k3s and Talos encrypt them themselves.
  - On kubeadm, Kubehenge's controller makes a secretbox key once for the cluster, in the Secret `<cluster>-encryption` beside the Cluster. Every control-plane node gets it.
  - The key is never replaced, since Secrets written under one key cannot be read with another.
- **Requests without credentials** are refused on RKE2 and k3s. On kubeadm they reach only the API server's health checks and the `cluster-info` ConfigMap, which `kubeadm join` reads before it has any.
- **The API server** limits the rate of events a namespace, and the cluster as a whole, may write (EventRateLimit). It does not extend projected ServiceAccount tokens past their hour.
  - It has profiling off, as do the controller manager and the scheduler, which serve on loopback only ([monitoring](#monitoring-add-on) reaches them through proxies).
  - It and the kubelets offer only forward-secret TLS 1.2 ciphers with authenticated encryption.
- **Nodes** run with the kernel settings the kubelet expects, and the kubelet refuses to start on others rather than change them. kubeadm's kubelet configuration and unit files are readable by root alone.

### CIS hardening

CIS hardening (`cis: true`), chosen when a cluster is created and fixed after, adds what costs workloads something:

- Pod Security Admission enforces `restricted`, whatever `podSecurity` says. The exempt namespaces stay exempt.
- Every container that sets no seccomp profile of its own runs under the container runtime's default.
- Exec, attach and port-forward sessions end after five idle minutes.
- RKE2 runs its own CIS profile.
  - etcd runs as a user of its own.
  - The pods in `kube-system`, `kube-public` and `default` take connections only from their own namespace, DNS and a few of RKE2's own services aside.
  - With monitoring on, Kubehenge's controller lets Prometheus through to CoreDNS's metrics.

The end-to-end tests run [kube-bench](https://github.com/aquasecurity/kube-bench) on a hardened cluster of each distribution. They test against the CIS Kubernetes Benchmark 2.0 for kubeadm and Rancher's own benchmarks for RKE2 and k3s. They fail on every failed check but one.

kubeadm runs etcd as root. Its data therefore belongs to root (check 1.1.12). Kubernetes has no supported way to run kubeadm's etcd as another user.

The benchmark's policies section is the cluster owner's. It judges what runs in the cluster, not how the cluster is built.

### Rancher add-on

Rancher (`rancher: {install: true, hostname: …}`) goes into one cluster Kubehenge creates for it, never into the management cluster, together with cert-manager. Rancher makes its own CA. Its server URL is fixed to `hostname`, which must resolve to that cluster's ingress controller for people and for every registered cluster. Kubehenge's controller registers every other cluster with it once the cluster is available, unless the cluster sets `register: false`:

- It signs in to Rancher's cluster with the kubeconfig Cluster API keeps for it. There is no Rancher account or token to look after.
- It creates the cluster's `provisioning.cattle.io` Cluster there, fetches the agent's manifest through that cluster's API server, and applies it to the registered cluster. The agent trusts only Rancher's CA, pinned by its checksum in the manifest.
- A cluster leaves Rancher before it is deleted (Cluster API's `BeforeClusterDelete` hook) or when `register` turns false, while Rancher can still clean up inside it.
  - Rancher's own cluster first removes every registered cluster.
  - Cluster API does not authenticate itself to the controller's hooks. Each hook therefore takes only the Cluster's name from a request and reads the Cluster from the API server.
  - The delete hook acts only on a Cluster that is being deleted there.

Rancher's chart refuses Kubernetes versions its release does not run on. The controller refuses to create the Rancher cluster outside them. It holds back an upgrade beyond them (`BeforeClusterUpgrade`) until a Kubehenge release brings a Rancher that runs there.

On fewer than 4 vCPUs a node running Rancher starves its own etcd and API server. `BeforeClusterCreate` therefore refuses such a cluster, and a validating webhook on Clusters refuses switching Rancher on or making its nodes smaller later. Rancher's first admin password is generated into the `bootstrap-secret` Secret in `cattle-system`, never written into values.

Rancher runs with imported-cluster version management off. With it on, Rancher would upgrade RKE2 and k3s itself beside Cluster API. Kubehenge creates and upgrades the clusters, and Rancher's upgrade jobs name their images by tag alone. The images Rancher starts itself (its agent, shell, webhook and Fleet) are named by digest through its chart's values.

Rancher also installs Turtles, its own Cluster API, without which it never takes a registered cluster as ready. Rancher reads Turtles' values only from its `rancher-config` ConfigMap, which its chart writes without them. Kubehenge's controller therefore writes that ConfigMap into Rancher's cluster before the chart installs, with Turtles' image and its Cluster API controller by digest. Helm adopts it as it installs.

## Cluster autoscaler

A worker pool whose topology entry carries Cluster API's `cluster-api-autoscaler-node-group-min-size` and `-max-size` annotations, and no replicas, is autoscaled. Cluster API copies the annotations to its MachineDeployment and leaves its replicas to whoever scales it. Kubehenge's controller runs one [cluster autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler) per cluster with such a pool. It uses the Cluster API provider with auto-discovery of that cluster's MachineDeployments alone. It removes the autoscaler once no pool is autoscaled or the cluster goes.

- **The release** is the one of the cluster's Kubernetes minor, since each carries that minor's scheduler to decide where pending pods would fit. A minor without one runs the newest earlier release, and a cluster older than all of them the oldest.
  - The chart lists one image per minor, by tag and digest, and Renovate moves each within its minor only.
  - In the management cluster they pull, as everything from registry.k8s.io does, only with the Kubernetes release signature.
- **Scale from zero.** A pool at none has no node to learn from.
  - The controller therefore writes on its MachineDeployment what a new node holds (`capacity.cluster-autoscaler.kubernetes.io/cpu`, `memory`, `ephemeral-disk` and `labels`).
  - It writes them by server-side apply under a field owner of its own.
  - The values are the class's cores, 90% of its memory and disk, and the node labels Cluster API gives its Nodes. The rest of the memory and disk stands for what the node reserves for itself and its system pods.
  - It keeps them in step with the pool's machine size.
- **Control planes** stay tainted while any pool may grow, so a workload never lands there only to be evicted once a worker comes.
- **Room.** The node addresses must hold the control planes, every pool at its most and one machine for a rollout.
  - The validating webhook and `BeforeClusterCreate` refuse a change that makes a shortage worse.
  - While a pool grows, the controller looks every minute for a worker whose VM Proxmox VE cannot place for want of memory. It records `AutoscalerNoRoom` with the provider's reason.
  - The autoscaler gives such a machine up after 30 minutes.

### Rights of the autoscaler

Each cluster's autoscaler has a ServiceAccount of its own in the `kubehenge-autoscaler` namespace, bound in its cluster's namespace to a ClusterRole. The role reads MachineDeployments, MachineSets, MachinePools, Machines and Proxmox machine templates. It patches the MachineDeployments' `scale` subresource and updates Machines, which it marks for removal.

RBAC cannot narrow that to one cluster's objects, as all clusters of a namespace share it. A ValidatingAdmissionPolicy therefore does, for every autoscaler account. Its parameter, the ConfigMap `kubehenge-autoscaler`, names for each account its cluster and the MachineDeployments of its autoscaled pools. The controller writes the ConfigMap before it binds the role and removes the entry before the account.

An account the ConfigMap does not name may change nothing. Otherwise an account may change a scale only of a pool it names. It may change a Machine only of its cluster, with nothing but the removal annotation changed.

Reads stay namespace-wide. An autoscaler sees the machines of every cluster in its namespace. That is the risk the design leaves.

Inside the cluster it is the autoscaler identity, `kubehenge-autoscaler` in `kube-system` ([Identities and health](#identities-and-health)). That identity reads what its scheduling simulation needs but no Secrets, updates Nodes and evicts pods to drain the workers it removes. It writes events, and writes no ConfigMap but its own status in `kube-system`. Its tokens, the identity's and (placed inside) its management account's, are kept in the cluster's `kube-system` the same way. They last an hour and are renewed every 20 minutes.

The chart creates the policy's ConfigMap empty. The API server looks a policy binding's parameter up before the policy's match conditions. Without the ConfigMap the binding would refuse every change of MachineDeployments and Machines, Cluster API's own included.

What an autoscaler has in `kubehenge-autoscaler` (its Deployment, account, token Secret and policy entry) lies outside its Cluster's namespace, where owner references cannot reach. The controller therefore holds the Cluster with the finalizer `kubehenge.dev/autoscaler` before it makes any of it and lifts it once all of it is gone. A cluster deleted while the controller is down waits for it.

### Where the autoscaler runs

By default the autoscaler runs in the management cluster. The management cluster then holds the token into the cluster, as it holds the cluster's kubeconfig anyway. The cluster holds nothing of the management cluster. Each autoscaler requests 64 MiB, with a limit of 512 MiB.

The `autoscaler` variable's `placement: workload` runs it on the cluster's control planes instead, as a system-cluster-critical pod. The pod tolerates their taint and reaches the management cluster at the `managementEndpoint` the module sets. The cluster then holds a management cluster token that reads every cluster's machines in the namespace. The interface therefore asks for `VM.Audit` on the workload pool before it places one there.

## Interface

A web app in a small LXC container on Proxmox VE, installed with one line in the Proxmox VE shell. It is where Kubehenge starts and what looks after it:

- **First run.** A wizard creates the management cluster through the same module `tofu apply` uses.
  - It runs with the signed-in administrator's own Proxmox VE session. No administrator token is stored.
- **Day two.** The container keeps the management cluster's state, encrypted.
  - It runs its Talos and Kubernetes upgrades, its growth from one node to three, moves of its VMs to other Proxmox VE nodes, and repair.
  - It keeps working while the management cluster is unwell, to show and fix it.
  - It holds state, so it belongs in Proxmox VE's backups. The interface warns administrators while no enabled backup job includes it (by its ID, by its pool, or by all guests on its node).

### Sign-in, sessions and permissions

- **Sign-in through the Proxmox VE API.** Every Proxmox VE realm works, PAM, Proxmox VE users, LDAP, Active Directory and OpenID Connect included.
  - There is no second user directory.
  - A second factor (TOTP, Yubico OTP or recovery keys) is passed on to Proxmox VE as its own sign-in page would.
  - A WebAuthn key is bound to Proxmox VE's own address, so its users sign in through an OpenID realm or another factor.
  - An OpenID realm sends the browser to its identity provider, which returns it to the interface. The return goes to `/api/openid/callback` under `--public-url`, to be registered with the provider.
  - Without a public URL OpenID sign-in is refused, since the address the browser sends is the client's to choose.
  - Proxmox VE checks the code, and the interface accepts the return only in the browser that started it.
- **Sessions.** The Proxmox VE ticket stays on the server, in memory only.
  - The browser holds a random session ID in a `__Host-` cookie (Secure, HttpOnly, SameSite=Strict). Every change also carries a per-session CSRF token.
  - Proxmox VE renews a ticket without asking the realm or the second factor again. A session therefore ends after a fixed time (eight hours by default) however active it is.
  - Failed sign-ins are limited per user at an address and per address, never per user alone. Nobody can lock an administrator out from elsewhere.
  - The frontend loads no inline script or style, and the Content-Security-Policy allows none.
- **Permissions follow Proxmox VE's, by pool.** Each cluster has its own pool under the workload pool.
  - Seeing a cluster needs `VM.Audit` on its pool. Creating one needs `VM.Allocate` on the workload pool.
  - These need `VM.Allocate` on the cluster's pool:
    - deleting the cluster
    - scaling it
    - upgrading it
    - changing its add-ons
  - Downloading its admin kubeconfig, and restoring one of its backups, need `Sys.Modify` there.
  - Creating also needs what Proxmox VE asks of anyone creating a VM there. That is `Datastore.AllocateSpace` on the storage and `SDN.Use` on the bridge or VNet (on the VLAN, when one is set).
  - Installing Rancher needs `Sys.Modify` on the workload pool, since Rancher receives the admin kubeconfig of every cluster that registers with it.
  - The app asks Proxmox VE for the signed-in user's effective privileges on each path, then acts with its own account. Administrators share clusters with the same ACLs they share VMs with.
  - A cluster the user may not see is answered as not found.
  - A role given to a user on a cluster's own pool replaces what the user inherits from the workload pool, for that pool. That is how Proxmox VE works everywhere.
  - The role therefore has to hold `VM.Audit` too.
- **Live pages.** The interface watches the management cluster's Clusters and Machines with its own account.
  - It tells each open page of a change as a Server-Sent Event (`/api/events`), at most once a second.
  - The page then reads what it shows again through the API.
  - An event therefore names no cluster. Each user still sees only what their privileges allow.
  - The pages poll as well, for when the stream is down.
  - A proxy in front of the interface has to pass the stream on unbuffered. nginx does on the `X-Accel-Buffering: no` the interface sends.

### Namespaces and credentials

- **Namespaces.** The interface creates and changes clusters in `clusters`, and in the namespaces an administrator adds beside it (`extra_cluster_namespaces`, the chart's `interface.extraNamespaces`).
  - The chart gives the interface's account a Role in each to create, change and delete Clusters and to read and write Secrets there. It lists them in the ConfigMap `kubehenge-interface` in `kubehenge-system`, which the interface reads.
  - Taking a namespace off the list takes the Role away and keeps the namespace and its clusters. Everywhere else the account reads only.
  - Nothing else is per namespace. The controller, the credentials policy and the health identities work cluster-wide.
  - A cluster's pool already carries its namespace (`<namespace>.<name>`).
  - The ClusterClasses are referenced from `kubehenge-system`, and the IP pools are made beside each cluster.
  - Cluster Mesh links clusters of one namespace, which share that namespace's CA.
- **Its credential in the management cluster.** The interface's account is the ServiceAccount `kubehenge-interface` in `kubehenge-system`, and it holds tokens of it that run out.
  - Each apply asks for one lasting 30 days with the admin kubeconfig, which stays in the encrypted state. It writes the token to a file of its own in the container.
  - While the appliance runs, the interface asks for the next token once the one it holds is a day old. It asks with that token, which may make tokens of its own account and no other.
  - A token copied out with a backup of the container stops working within a month. An appliance switched off for longer gets a new one from the next apply on the Administration page.
  - Deleting the ServiceAccount ends every token of it at once.
- **Credentials typed in.** What reaches a service outside the clusters needs a secret:
  - a backup target's keys
  - a DNS provider's API token for DNS-01 certificates
  - a private Git repository's token
  - an identity provider's client secret
  - The interface stores each in a Secret beside its cluster, of the type `kubehenge.dev/user-credential` and named `<cluster>.<purpose>`.
  - The Secret is labelled with its purpose (`kubehenge.dev/user-credential`) and its cluster (`kubehenge.dev/credential-cluster`). The Cluster owns it, so deleting the cluster deletes it.
  - **Who may set one:** storing, replacing or removing a cluster's credential needs `Sys.Modify` on its pool, as the admin kubeconfig does. A credential reaches as far into what the cluster runs as the admin kubeconfig does.
    - Seeing whether one is set needs what seeing the cluster does.
    - A credential for every cluster, such as an administrator's backup target, is named `kubehenge-<purpose>`. It needs the privileges on `/` that the management cluster's own settings need.
  - **Write-only:** the interface never hands a value back, only whether one is set, when, and by which Proxmox VE user (the annotations `kubehenge.dev/credential-set-by` and `kubehenge.dev/credential-set-at`).
    - Replacing one takes every value again.
    - The log records who changed which credential, never what it holds.
  - **Held to them:** RBAC cannot tell one Secret from another on create.
    - A ValidatingAdmissionPolicy in the chart (`kubehenge-interface-credentials`) therefore holds the interface's account to these Secrets.
    - The account may create, change and delete only Secrets of that type, with a purpose it knows and the name that goes with it. It may change or delete none without the label.
    - A compromised or faulty interface cannot overwrite a cluster's kubeconfig, CA or Proxmox VE token. It cannot make a Secret Kubernetes or Cluster API acts on, such as a ServiceAccount token.
    - The policy runs inside the API server, so no webhook has to answer for it, and it fails closed. It does not apply to other accounts, such as a GitOps tool's.
  - **Without the interface:** clusters managed with GitOps or kubectl name a Secret of their own instead.
    - Every feature that takes a credential also takes `existingSecret`, the name of a Secret the cluster's owner creates in the workload cluster.
    - That Secret never passes through the management cluster and needs no privilege of Kubehenge's.

### Identities and health

- **Identities inside the clusters.** Whatever the interface reads or starts inside a workload cluster is done as one of Kubehenge's identities.
  - The cluster autoscaler's work there is done as one too.
  - Each identity is a ServiceAccount `kubehenge-<identity>` with a role for one job and nothing else.
  - What serves one purpose therefore cannot do another's, whatever its holder does.
  - The workload cluster's audit log tells them apart. None may read a Secret or exec.
  - Kubehenge's controller creates each identity a cluster has once the cluster's control plane is up and the identity's namespace exists. It puts back what was changed of it.
  - The controller asks the cluster for a one-hour token of each identity with the TokenRequest API, renewed every 20 minutes. The token is never stored in the workload cluster.
  - The token is kept, with the API server's address and CA, in the Secret `<cluster>-<identity>` beside the Cluster and owned by it. Its expiry is in the annotation `kubehenge.dev/token-expires`, so none outlives an hour once the controller stops renewing it.
  - The autoscaler identity's Secret is the exception. An autoscaler running in the management cluster mounts it, and a pod mounts Secrets of its own namespace alone.
  - It is therefore kept in the `kubehenge-autoscaler` namespace, labelled with its cluster.
  - Its kubeconfig reads the CA and the token from the files beside it. The Secret goes when the cluster does.
  - An identity a cluster no longer has loses its Secret. That happens to the backup identities once backups are switched off.
  - The identity leaves the workload cluster with its namespace, the autoscaler identity with the autoscaler. The interface reads these Secrets only in its own namespace.
- **Health page.** One page says how a cluster stands, in this order:
  - what needs attention first
  - its nodes
  - unwell pods
  - waiting volume claims
  - add-on releases
  - what Cluster API reports
  - the last hour's events
  - Anyone who sees the cluster (`VM.Audit` on its pool) sees it. It shows names, reasons and messages, never a spec, an environment or a Secret.
  - **From the management cluster:** the interface's own account reads these, all read-only:
    - the cluster's machines, MachineDeployments and MachineHealthChecks
    - its control plane and HelmReleaseProxies
    - the events of its namespace
  - **From inside the cluster:** for clusters in the interface's namespaces, as two of the interface's identities in the workload cluster's `kube-system`.
    - `kubehenge-health` may get and list nodes, pods, events and PersistentVolumeClaims and read `/readyz`.
    - `kubehenge-health-logs` may only read pods' logs.
    - Once their tokens run out, the page says since when its view is stale.
  - **Cordons:** `kubehenge-nodes`, a third identity there, may get and patch nodes, for those with `VM.Allocate` on the cluster's pool.
    - The controller keeps beside it a ValidatingAdmissionPolicy of the same name. It acts on that account's requests alone and refuses any change to a node but `spec.unschedulable`.
    - A node's labels, annotations, finalizers, owners, taints, provider ID and pod ranges stay as they are.
  - **Logs apart:** logs carry secrets often enough.
    - They take `Sys.Modify` on the cluster's pool, as the admin kubeconfig does, and come from the logs identity alone.
    - The token behind every viewer's page cannot read a single line, whatever the interface does. The workload cluster's audit log tells the two apart.
    - The interface hands out the last 60 lines of one container at a time (the run before, for a container that crash-loops). It logs who read which.
  - **Cheap to keep open:** each cluster is read once for every viewer, at most every 10 seconds.
    - The read has a deadline, list limits and the API server's cache.
    - Each VM's CPU line for the last hour comes from Proxmox VE with the viewer's own ticket. It shows only the VMs that viewer may see.

### Support bundles and boot logs

A cluster's support bundle is written by one package (`pkg/diagnose`) for `kubehenge diagnose` and for the interface alike, from the management cluster.

- The bundle holds the Cluster and every object of the Cluster API groups labelled with its name (found by discovery). It holds their events and its boot-log ConfigMap too.
- It also holds log lines from the controller's, capmox's and the Add-on Provider for Helm's pods.
- A line goes in only when it names one of those objects exactly, by name with the cluster's namespace or as `<namespace>/<name>`. No line of another cluster's goes in.
- Secrets are never read for it. Fields and lines that look like tokens, passwords, keys or a talosconfig are replaced (`pkg/redact`).
- For it the interface's account may get and list every Cluster API object in its namespaces. It may list pods and read their logs in `kubehenge-system`, `proxmox-infrastructure-system` and `helm-addon-system`.
- A bundle takes `Sys.Modify` on the cluster's pool, as logs do.
- Boot logs are state on Kubernetes objects. The Cluster's `kubehenge.dev/boot-log-request` annotation names the machines.
- The controller reads each into the ConfigMap `<cluster>-boot-logs` owned by the Cluster, and takes the name off. For this it may get, create and update ConfigMaps in any namespace, as it does Secrets.
- The interface only adds the name and reads the ConfigMap.
- A kubeadm, RKE2 or k3s node's log is two files read through the VM's guest agent. The controller reads them with its own token, which holds `VM.GuestAgent.FileRead` on the workload pool for it.
- One file is cloud-init's output.
- The other is the copy of its boot's journals. A timer of the node template's writes it every few seconds into `/var/log/kubehenge`, which the node's SELinux policy lets the guest agent read.
- The journal itself is binary, and reading it otherwise takes commands run in the VM. A Talos node's log is read through its Talos API with the cluster's talosconfig.

### What the interface does

- Create, list, scale, upgrade and delete clusters.
- Choose the distribution, add-ons, identity providers and automatic upgrades.
- Download a kubeconfig.
- Open each cluster in Headlamp, signed in where the administrator lets it.
- For administrators, build VM templates, upgrade Kubehenge, and apply, change or remove the management cluster.

### Headlamp in the appliance

One Headlamp serves the clusters of the interface's namespaces on port 8443, behind the interface's sign-in.

- It is the appliance's own pinned copy, with its server and frontend.
- They are taken from Headlamp's release and checked against its checksum when Kubehenge's release is built.
- It is a separate origin, so no page it shows can run script where the interface's session lives.
- **Its kubeconfig.** The interface writes each cluster's API address and CA there. It writes no credential, since Headlamp would use it for every visitor.
  - Headlamp picks up clusters as they come and go.
  - People sign in to each cluster with their own token, or through the cluster's identity provider ([Signing in to clusters](#signing-in-to-clusters)). Headlamp keeps it in an HttpOnly cookie for that cluster's paths only.
- **Signed in for kubeconfig holders.** The module's `headlamp_sign_in` (the chart's `interface.headlampSignIn`) is off by default. With it on, the gate signs in as cluster-admin those who hold `Sys.Modify` on a cluster's pool, who may download its admin kubeconfig anyway.
  - Kubehenge's controller keeps two ServiceAccounts in the `kube-system` of each cluster in the interface's namespaces.
  - `kubehenge-headlamp` is bound to `cluster-admin`.
  - `kubehenge-headlamp-issuer` has a role that lets it make `kubehenge-headlamp`'s tokens (`serviceaccounts/token` by that name) and nothing else.
  - The issuer is one of the interface's workload identities. It is wanted while the sign-in is on and the cluster is in one of the interface's namespaces.
  - Its one-hour token is beside the Cluster (`<cluster>-headlamp-issuer`) and renewed like the others'.
  - On such a user's first request for a cluster, the interface has the cluster make a token of `kubehenge-headlamp` for that session. It lasts an hour, or until the session ends with ten minutes as the floor, the TokenRequest API's shortest.
  - The interface keeps the token in memory and sets it as the `Authorization` of that session's requests for the cluster.
  - Headlamp passes it on when the browser holds no token of its own.
  - The token never reaches the browser, so a script on Headlamp's pages cannot carry it off. The gate checks the privilege again every 30 seconds.
  - The interface logs each token it makes with the Proxmox VE user and the token's ID. The cluster's audit log records the ID with the ServiceAccount (`authentication.kubernetes.io/credential-id`).
  - Switched off, the controller deletes the ServiceAccounts, `kubehenge-headlamp` first, which ends every token made for it at once. It then deletes the issuer's Secret.
  - Headlamp's log out of a cluster posts to its `/encoded-clusters/<cluster>/clear-token`, which clears its token cookie. The gate passes it on and drops the session's token too.
  - The gate then signs the session in to that cluster no more until the session opens it from the interface again. The interface's links go through the gate's `/kubehenge/open/<cluster>` for that.
  - The session's cookie is `SameSite=Strict`, so no other site's link signs anyone in again.
- **Its actions.** The module's `headlamp_actions` (the chart's `interface.headlampActions`) is off by default. The chart writes it into the ConfigMap `kubehenge-interface`, which the interface reads every 30 seconds.
  - With it on, the gate serves two of the plugins Headlamp ships, which the appliance carries: the Helm app catalog and the Prometheus charts. They are under `/static-plugins/`, read only, and left out of Headlamp's plugin list while off.
  - The gate passes on a cluster's Helm routes by exact method and path. These are:
    - listing releases, and one release with its history
    - an action's status
    - rollback, uninstall, install and upgrade
    - listing the repositories and their charts
  - Headlamp acts there with the request's credentials, the user's own or the sign-in's.
  - An install or upgrade must name a chart of a repository (`repository/chart`) and fetch no dependencies.
  - Helm would also take a path on the appliance, an address or an OCI reference. The appliance would then read its own files or fetch from anywhere it reaches.
  - Headlamp also runs in its own empty state directory, where no relative path names a chart.
  - Headlamp keeps one list of repositories for every user and cluster. Only those who hold the setup's privileges on `/` add, update or remove one.
    - A repository is reached over HTTPS, with its certificate checked.
    - A repository's credentials are sent to its own host alone.
  - The app catalog's Artifact Hub search needs the external proxy, which stays refused (below).
- **Isolation:** the Headlamp server runs as a user of its own (`kubehenge-headlamp.service`, systemd's dynamic user) on the loopback interface. It can read nothing of the interface's state, and only the interface reaches it.
- **Only while used:** Headlamp holds more of the appliance's memory than the interface itself, so it runs only while someone has it open.
  - systemd listens on its port and starts it with the first connection (a quarter of a second).
  - It stops again once no connection has been open for 15 minutes.
- **The gate in front:** the interface takes the session cookie and strips its own cookies before passing a request on.
  - Host-only cookies reach every port.
  - It checks `VM.Audit` on the cluster's pool for every `/clusters/<namespace>.<name>/` request and rechecks every 30 seconds. It lists only those clusters.
  - It always refuses what a shared server must not offer:
    - port forwards (they would open ports on the appliance)
    - the drain routes that name the cluster in the request rather than its path
    - the external proxy
    - the WebSocket multiplexer
    - metrics
    - adding clusters or kubeconfigs
    - setting a token for others
    - plugin changes
  - A cluster's own `drain-node` route passes. Helm and the plugins Headlamp ships stay refused unless its actions are switched on (above).
- **The add-on:** a cluster that wants Headlamp at a hostname of its own uses the [Headlamp add-on](#headlamp-add-on).

### Changes and look

- **Changes planned, then followed.** A change of a cluster's settings is checked first by `POST /api/clusters/<namespace>/<name>/plan`.
  - The plan is an update of the Cluster with the API server's dry run, admission policies and webhooks included. It makes nothing.
  - The answer says for each setting which machines it replaces, read from the cluster's ClusterClass.
  - A variable that a patch feeds into the control plane's templates replaces the control-plane machines. One fed into a MachineDeployment or MachinePool class replaces the workers.
  - A version replaces every machine, replicas add or remove machines, and a variable no patch reads replaces none.
  - Applying the change records who made it, when and that answer in the annotation `kubehenge.dev/change` on the Cluster.
  - The runs list follows the change from there for a day.
    - It shows the machines of each role made since it, ready, against those the cluster asks for and those left from before it.
    - It then shows the topology reconciled at the Cluster's generation and the Cluster available.
- **Look.** The fleet is down the left and the runs down the right.
  - It uses Atkinson Hyperlegible Next with JetBrains Mono, in the system's dark or light mode.
  - It borrows no code or look from Proxmox VE's GPL-licensed toolkit.
- **Built as.** A Go backend (`pkg/ui`, run by `kubehenge serve`) uses the Cluster API and Kubernetes client libraries directly.
  - It serves a React and TypeScript frontend (`web/`) embedded in the same binary.
  - The `kubehenge` command-line tool shares the backend's code.
  - Clusters created here carry the creating user in the annotation `kubehenge.dev/created-by`. Every change is logged with the user who made it.

### Optional entry inside Proxmox VE

Proxmox VE has no supported way to extend its own web interface. An opt-in installer (`appliance/pve-entry/`) adds a Kubernetes entry that leads to the appliance:

- **What it adds:** a button beside Create VM and Create CT, and an item under Datacenter. Both open the appliance in a new tab, since the appliance refuses to be framed and stays an origin of its own.
- **How:** one line added to the page template `pve-manager` ships.
  - It loads a script of ExtJS overrides that runs before Proxmox VE builds its workspace.
- **Surviving updates:** an apt hook puts the line back after a `pve-manager` update replaces the template, and logs it. `--remove` returns the node to its packaged state.
- **Failing safe:** the script extends two of Proxmox VE's interface classes, which are no API.
  - If a release renames them, or a step of the entry fails, the entry stays out.
  - Proxmox VE's interface then renders as shipped, with the reason in the browser's console.
- **Scope:** each node serves its own interface, so it is installed per node. `appliance/install.sh` asks whether to add it on the node it runs on.
  - The appliance never depends on it.

## First run

Cluster API needs a cluster before it can create clusters. One Terraform/OpenTofu apply, or the appliance itself, creates a small management cluster on Proxmox VE VMs with everything above installed. From then on, clusters are created from the interface or from YAML.

The appliance (`appliance/install.sh`) checks everything it downloads against checksums it carries. It creates an unprivileged Debian container and gives it a certificate signed by the Proxmox VE cluster's own CA. It runs the interface as an unprivileged user on ports 443 and 8443 (the shared Headlamp).

Its first-run page runs the `management-cluster/` module with OpenTofu on the signed-in administrator's session. The ticket and CSRF token reach OpenTofu through its environment only. The state is encrypted with a key derived from a passphrase the administrator chooses. The passphrase is not stored.
Every later apply asks for it again, unless an administrator switches on automatic upgrades of the management cluster ([Upgrades](#upgrades)).

The management cluster's admin kubeconfig and talosconfig stay inside that state. An administrator can open the state from the Administration page with the passphrase, for repairs the interface cannot make (every download is logged). The interface works with a ServiceAccount of its own. It sees every cluster, but it changes clusters, reads their kubeconfigs and stores their credentials only in its namespaces ([Namespaces and credentials](#namespaces-and-credentials)).

## Signing in to clusters

Every cluster accepts client certificates and ServiceAccount tokens, as Kubernetes does. Its `oidc` variable adds OpenID Connect providers whose ID tokens its API server accepts, up to eight. They are written as Kubernetes' structured authentication configuration (one JWT authenticator per provider). Each entry has:

- the issuer URL
- the client ID, which a token's audience must name
- the claims for user name and groups
- prefixes, `oidc:` unless set, which may not start with `system:`
- the scopes sign-in tools ask for
- a CA, when a public one does not sign the provider's certificate

Who sets them, and what follows:

- **The appliance-wide default.** Providers set on the Administration page, or as the module's `cluster_oidc` (the chart's `clusterOIDC`), are the variable's default.
  - Every new cluster trusts them unless it sets its own list, and the create wizard starts from them.
  - A change applies to clusters created afterwards.
- **Who may choose.** Whoever runs a provider can issue a token for any name, so keeping or dropping the default's providers needs no extra privilege.
  - Adding any other provider needs `Sys.Modify` on the pool, as the admin kubeconfig does.
    - For a new cluster it is the workload pool. For a change it is the cluster's own pool.
- **Changing it.** Changing a cluster's providers replaces its control-plane nodes, as any change of the API server's configuration does.
- **Headlamp.** The shared Headlamp offers the first provider's sign-in beside the token prompt, as a public client with PKCE.
  - No client secret is stored.
  - The provider must accept `https://<appliance>:8443/oidc-callback` as a redirect URL.
  - The browser that starts a sign-in is the only one the gate lets finish it. It keeps a hash of the OAuth state in a short-lived cookie.

## Upgrades

### Kubehenge itself

The appliance's Administration page shows the installed release and newer ones, and upgrades with a button. It can also upgrade automatically.
That happens once a day at a chosen hour, to new patch releases or to new minor releases of the same major version.

The interface never replaces software itself. It drops a request into a spool. A separate root service (`kubehenge-update`) carries it out. Of root's capabilities it keeps only `CAP_DAC_READ_SEARCH`, to read the request the interface's user wrote, under a system-call filter. Its file system is read-only but for the software, its service units and its spool.

That service accepts only a release named in an index signed with Kubehenge's release key.
The key is Ed25519, built into each release and never configurable at run time.
The release must be newer than the installed one, and the archive must match the signed checksum. The service swaps the files by rename and restarts the interface.

It keeps the release it replaced aside until two things hold. The new interface answers its health route on the appliance's own certificate, which is trusted as itself, whoever signed it. Then systemd has not restarted the interface for 20 seconds. Otherwise it puts the old release back and restarts that.

`install.sh --upgrade` does the same with the same check. The check is `kubehenge appliance health` of the new binary. The CLI in the appliance (`kubehenge appliance`) is a client of the interface, signed in as a Proxmox VE user. Its upgrades and etcd backups therefore pass the interface's privilege checks and validation, which live only there.

A run of the management-cluster module holds a lock on the appliance's state directory, which the updater and `install.sh --upgrade` take too. No upgrade therefore restarts the interface in the middle of a run. No run starts during an upgrade. The kernel releases the lock when its holder ends, so a crash leaves none behind.

The management cluster then moves to what the new release carries when an administrator applies it again from the same page. Every apply records the release that ran it. The page therefore says when the management cluster runs what an older release applied.
It also says which component versions applying this release changes. Those are read from the module's `versions.tf` and Kubehenge's chart.

### The management cluster on its own

An apply needs the state passphrase and a Proxmox VE credential that can create users, roles, ACLs and VMs. The appliance keeps neither by default.

An administrator may switch on automatic upgrades of the management cluster. The appliance then keeps the passphrase and the API token of a Proxmox VE user of its own, `kubehenge-upgrader@pve`. That user holds the Administrator role on `/`.

Both are kept in a file only the interface's user and root can read. The file holds the passphrase itself. OpenTofu derives a new key for every state it writes, so no derived key could stand in.

Both are sealed with a key kept on a volume of the container that Proxmox VE's backups leave out. No backup holds what opens them.

The interface then applies the saved settings with them in Kubehenge's upgrade hour. It does so once the appliance runs a newer release than the one that last applied the management cluster, at most once a day.

Switching it off deletes the file, then the user. Removing the management cluster does the same.

### Clusters

The `upgrades` variable of every ClusterClass asks Kubehenge's controller to upgrade Kubernetes automatically. `patch` keeps the minor version. `minor` also moves to the next one, one minor at a time and never past the distribution's stable release.
The stable release is:

- RKE2's and k3s's release channel
- for kubeadm, the newest version a template has been built for
- for Talos, Kubernetes' own newest release of each minor version

For Talos it never goes to a release the cluster's Talos release does not run, and never while its nodes are being upgraded.

An upgrade starts only inside the window (days of the week or of the month, a start time and length in a named time zone). It starts only while the cluster is available with no rollout running, and not beyond the Kubernetes versions an installed Rancher runs on. Cluster API then rolls the nodes as for any upgrade. A rollout that has started finishes even if the window closes. The last automatic upgrade is recorded on the Cluster as `kubehenge.dev/auto-upgraded`.

## Networking

- **Node addresses** come from IP pools managed in the management cluster. No DHCP is needed on the Proxmox VE bridge.
- **The control plane** of each cluster gets a virtual IP that moves between control-plane nodes.
- **Services of type `LoadBalancer`** get addresses from the cluster's `loadBalancer` range, if it has one:
  - MetalLB in layer 2 mode, without its BGP backend.
    - Kubehenge's controller applies its address pool in the cluster.
    - The chart has no place for one.
    - It applies the pool again whenever MetalLB returns. The CRDs take the pool with them when MetalLB goes.
  - kube-vip as a DaemonSet that answers only for Services, one lease per Service so that their addresses spread over the nodes. kube-vip's cloud provider hands them out.
  - MetalLB's speakers answer from the control planes too, which kubeadm labels as excluded from load balancers. A cluster without workers therefore still has nodes to answer from.
  - On k3s the class switches servicelb off. The controller deletes the DaemonSets servicelb left.
    - k3s itself cleans them up only while its own cloud controller runs.
  - Every k3s node allows the two forwarding settings servicelb's pods need, which k3s allows only where servicelb is on.
  - Switching the load balancer off turns servicelb back on, and the control planes roll for it. Its pods then run on the old servers too.
  - Cilium's own L2 announcements are not used, since they need Cilium in place of kube-proxy.
  - Kubehenge's admission webhook and BeforeClusterCreate hook refuse addresses outside the nodes' network.
    - They also refuse addresses on the gateway, the network's own or broadcast address, the API address or the node addresses.
    - They refuse another cluster's addresses on the same bridge and VLAN too.
  - kube-vip's cloud provider never takes back an address it has recorded on a Service, and MetalLB keeps one as requested.
    - The controller therefore frees a Service of an address outside the range. The range no longer holds that address against other clusters.
    - The load balancer gives the Service a new one.
- **Pod networking** comes from each distribution's own CNI unless another is chosen for the cluster: canal for RKE2, flannel for k3s and Talos.
  - RKE2 also ships Calico and Cilium and installs either itself.
  - kubeadm has none of its own, so one is chosen when a kubeadm cluster is created, Cilium or Calico. Kubehenge's controller installs it.
  - It does the same for a k3s cluster given Cilium or Calico, whose flannel and network policy controller are then switched off. It does the same for a Talos cluster given either, whose flannel the class then leaves out.
  - With k3s's own flannel, kube-proxy runs in nftables mode, in a table of its own. k3s's network policy controller writes the iptables filter table back whole from a copy read moments before.
    - That would drop the rules kube-proxy made in between.
  - On Talos, Cilium leaves the cgroup hierarchy to Talos and runs without `SYS_MODULE`. Calico installs no FlexVolume driver into Talos's read-only `/usr`.
  - Kubehenge's Cilium chains the portmap plugin, as RKE2's does.
    - Host ports, the ingress controller's among them, then work without Cilium in place of kube-proxy.
    - On k3s the plugin is k3s's own.
  - Calico comes as the Tigera operator's chart. The operator creates Calico's CRDs.
  - Calico's Installation comes through the cluster's ClusterResourceSet once they exist, since Calico's CRD chart is too large for a Helm release.
  - The CNI is chosen at creation and stays.
- **Pod and service ranges** must stay clear of the nodes' network and of each other.
  - The controller's mutating webhook sets them on every new cluster that names none, from the interface, kubectl or GitOps alike.
  - It takes those asked for.
  - Otherwise it takes the distribution's own when they are clear of the nodes' network.
  - Failing that, it takes the first of a few private ranges that are clear.
  - A cluster that runs Cilium, which may join a Cluster Mesh later, also keeps its pods clear of every other cluster's pods and services.
  - Its validating webhook refuses ranges that are no IPv4 networks or overlap the nodes' network or each other.
  - Every class honours the Cluster's ranges, kubeadm's pod subnet among them, which the Tigera operator needs to install Calico. The k3s provider reads its own server configuration only, so the k3s class copies them there.
- **The Gateway API** is in every cluster, with no switch. It costs no pod.
  - Traefik's Gateway, Istio's waypoints and anything else that speaks it get the same API everywhere.
  - **Kubehenge's release.** Kubehenge's controller carries one release of upstream's standard-channel install file.
    - The file is built into its binary.
    - It is checked against its SHA-256 when the binary is built.
    - It applies its CRDs and the admission policy beside them (`safe-upgrades.gateway.networking.k8s.io`) with the cluster's admin kubeconfig once the control plane is up.
  - **The distributions' charts.** RKE2 and k3s ship the CRDs in their Traefik CRD charts in some releases.
    - Later releases drop them from that chart, which keeps them in the cluster. They ship all of the Gateway API, CRDs and policy, in a chart of its own instead (`rke2-gateway-api-crd`, `gateway-api-crd`), which adopts what it finds.
    - There the controller first waits until the distribution's charts are installed. These are the Traefik CRD chart and the Gateway API chart where the release has one.
    - That chart's HelmChart is there by then, as the distribution creates all of a release's HelmCharts at once.
    - Helm refuses to install a chart over objects it does not own, and Traefik's chart refuses to install without its CRD chart.
  - **Whose objects.** Where the distribution has its own Gateway API chart, the Gateway API is the distribution's.
    - The controller applies none of it, whichever release the chart carries.
    - It mixes none of its own objects in.
    - Otherwise the controller looks at each object of the bundle in the cluster:
      - One missing gets Kubehenge's.
      - One of Kubehenge's release or newer stays.
      - An older one stays while the latest revision of the Helm release that marks it as its own still holds it. It is taken over and brought to Kubehenge's release once that revision does not.
  - **Logged and guarded.** The controller logs each object it applies, and which Helm releases hold the cluster's objects whenever that changes.
    - The policy stays in place too, so that a chart bringing older CRDs, or experimental ones over standard ones, is refused.
    - The distributions' Gateway API charts install only the standard channel, as Kubehenge does. Their experimental channel adds only the `gateway.networking.x-k8s.io` CRDs, which neither Traefik's Gateway provider nor Istio's waypoints need.
  - **Traefik's Gateway provider** is on in every distribution, configured like the rest of Traefik.
    - The configuration is in the chart values on kubeadm and Talos.
    - On RKE2 and k3s it is in the `HelmChartConfig` the classes write onto the control planes before the distribution starts. The distribution's Traefik therefore never runs on its own values.
    - The certificates add-on's default certificate and HTTPS listener come from the cluster's `certificates` variable. They are chart values on kubeadm and Talos.
    - On RKE2 and k3s they go through the Secret `kubehenge-traefik` that the `HelmChartConfig` names ([Add-ons](#add-ons)). Switching them on or off replaces no node.
  - **Held back for the CRDs.** Traefik's chart creates a Gateway, which an API server without the CRDs refuses.
    - On kubeadm and Talos the controller therefore holds Traefik's chart back until the Cluster carries the annotation `kubehenge.dev/gateway-api`. The annotation records the oldest release among the CRDs once they are all there, or once the distribution's Gateway API chart is installed.
    - RKE2's and k3s's helm controller retries until the CRDs are there.
    - The controller looks at every cluster again each minute, so a distribution upgrade that drops the CRDs is followed within a minute.
