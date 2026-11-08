# Architecture

Kubehenge assembles maintained upstream pieces into one tested whole for Proxmox VE. It writes as little of its own as it can. What it writes is the cluster templates, the add-on catalogue, the image pipeline, the first-run bootstrap and the interface.

## Why Cluster API

Cluster API is the Kubernetes project's own way to describe clusters as Kubernetes objects and have controllers create and keep them in that state. VMware's vSphere Kubernetes Service and OpenStack's Magnum both run on it. It gives Kubehenge, for free:

- **Several distributions over one infrastructure layer.** The distribution is a swappable bootstrap and control-plane provider over the same Proxmox VE layer.
- **Day-two operations.** Rolling upgrades with nodes replaced one at a time, scaling by changing a number, health checks that replace dead nodes, clean deletion.
- **GitOps.** Clusters are YAML, so they work from Git with any GitOps tool.

A Terraform/OpenTofu-only design would create clusters once. Upgrades and node replacement would then be rebuilt for every distribution.

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

The management cluster is an appliance that runs Cluster API, its providers and Kubehenge, and nobody's workloads. Monitoring, Rancher and everything else users run go into the clusters it creates. Kept bare, it stays small. Its attack surface is what Kubehenge needs. Losing a workload cluster never touches the piece that repairs it.

### Operating system

It runs Talos Linux, an immutable operating system with no shell or SSH, configured and upgraded only through its API.

One control-plane node is enough for a lab. Three make it highly available, with Talos's own virtual IP in front of the API server. Control-plane nodes also run Kubehenge's controllers, so there are no separate workers.

The Talos Image Factory builds a release's installer and disk image with different bytes each time. No digest pins them. The nodes pull the installer, Sidero's images and everything from registry.k8s.io only with their publishers' keyless signatures (Talos's `ImageVerificationConfig`). They pull the Talos providers' images only with the signature of their fork's release workflow at a version tag. The disk image is downloaded over TLS alone, since the factory publishes no checksum or signature for it.

### Pod network

Its pod network is Talos's Flannel, kube-network-policies (which enforces NetworkPolicies) and kube-proxy. The chart lets nothing reach a pod in `kubehenge-system` or `kubehenge-autoscaler` but two of the controller's ports. Port 9443 serves the webhook and lifecycle hooks the API server and Cluster API call. Port 8443 serves metrics to accounts holding `kubehenge-metrics-reader` only. The token Jobs ([below](#tokens-kept-out-of-the-controller)) reach only the cluster's DNS and Proxmox VE's API port.

Outbound connections stay open. The controller and the autoscalers reach Proxmox VE, the clusters' API servers, S3 stores and Git servers wherever they are.

### Cluster API providers

The Cluster API Operator installs Cluster API's providers. Each provider and its version is a Kubernetes object the Terraform/OpenTofu configuration declares. An upgrade therefore changes a version number.

Kubehenge's release build checks the operator's chart archive against the SHA-256 `versions.tf` names and ships it beside the module. The module checks it again before installing it. The operator's image and cert-manager's chart (from its OCI registry) are pinned by digest. `capmox` signs in to Proxmox VE with an API token of its own that holds only the privileges it needs.

#### Installing the providers

The operator downloads no provider itself. A hook of the providers' release (`kubehenge manifests`) downloads each provider's metadata and components and checks them against the SHA-256s `versions.tf` names. It stores them in the ConfigMap the provider's `fetchConfig` selects.

Another hook (`kubehenge wait providers`) holds the release until the providers run:

- The operator reports each provider's current generation installed or upgraded.
- The hash of what it applied matches on the provider and on the Secret it caches the manifests in.
- Every provider Deployment has rolled out.
- All of that holds for 30 seconds with the same Deployments. Right after an install, the operator can install a provider again from a stale read and recreate its Deployments.

Each provider's probes allow 5 s. Its liveness probe tolerates six misses. The k3s providers have no probes. On a busy host, upstream's 1 s probe restarts Cluster API's only replica. Its webhooks then refuse every change.

#### Provider images built by Kubehenge

Each provider runs its release's own image, by the digest `versions.tf` names, with two exceptions:

- **capmox v0.9.2**, with five fixes of its own that upstream's release lacks (`capmox_build` in `management-cluster/versions.tf`). It also writes a VM's firewall from the security groups a cluster names ([clusters.md](clusters.md#firewall)), which upstream has no setting for.
- **The Add-on Provider for Helm v0.6.4**, with three fixes (`helm_addon_build`):
  - The provider creates a cluster's release once, never twice.
  - It pulls or checks each chart by the digest its version carries.
  - It keeps each pinned chart once downloaded, so a reconcile reads no chart repository index.

Both are built reproducibly, so the published images can be rebuilt from their sources and compared ([releasing.md](releasing.md)). A release of each that carries its fixes replaces the build.

#### Talos providers

Talos's two providers come from Kubehenge's own forks of them, [cluster-api-bootstrap-provider-talos](https://github.com/kubehenge/cluster-api-bootstrap-provider-talos) and [cluster-api-control-plane-provider-talos](https://github.com/kubehenge/cluster-api-control-plane-provider-talos). Sidero Labs' releases are on Cluster API's v1beta1 contract, which Cluster API's core removes. Its control-plane provider has no `TalosControlPlaneTemplate`, which a ClusterClass needs. The forks carry both, with fixes of their own.

They install like any other provider, by the digests `versions.tf` names. Each fork's release workflow signs its image keylessly and attests the build provenance of its image and files. `make verify-providers` checks all of it in CI before a new pin is merged.

### Provider controllers

Every distribution's providers are installed. Kubehenge's controller scales their Deployments to none while no cluster uses that distribution. The eight controllers take about 50 MiB each, about 400 MiB in all.

When the first cluster of a distribution is created, the controller scales them up. Once they serve, it applies the distribution's control-plane and bootstrap templates, which their webhooks have to admit. Cluster API holds the new cluster until then. That adds a minute or so to the first cluster of each distribution. When the last cluster is gone, the controller removes the templates, then scales the providers back to none.

The controller may only change the providers' replica count, through the Deployments' `scale` subresource. The operator leaves that count alone because the configuration removes it from the providers' manifests.

### etcd backup

The appliance backs up the management cluster's etcd from outside it. A restore then does not depend on what it restores.

On the schedule set under **Administration, Backups**, it asks a control-plane node's Talos API for a snapshot with a talosconfig of the `os:etcd:backup` role. It checks etcd's own checksum at the end of the stream. It encrypts the snapshot to an age key and uploads it to the chosen backup target. The key's private half is stored only in the target, encrypted with the state passphrase.

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

Each class wires in the always-installed set. It passes the end-to-end suite before it is called supported ([testing.md](testing.md)). The classes ship in the `kubehenge` Helm chart (`charts/kubehenge`), which the management cluster's configuration installs. The chart creates each ClusterClass and its Proxmox VE templates directly. Its control-plane and bootstrap templates go into a ConfigMap named after the class, which the controller applies while a cluster uses the distribution.

### Talos class

The Talos class takes one variable more, `talosVersion`, the Talos release the nodes run. Its control planes hold the endpoint with Talos's own virtual IP rather than kube-vip. Its machine configuration is the strategic patches the Talos bootstrap provider applies to what it generates. They hold the same audit policy, admission configuration, authentication configuration and the long leader-election leases ([Leader-election leases](#leader-election-leases)) as the other classes. They also hold serving certificates the cluster CA signs for the kubelets, and image signatures required as on the management cluster.

The controller's external patch sets the configuration's `talosVersion` to the minor version of the release the machine boots first. Each machine is then generated in the schema its first release reads. Talos keeps SELinux as it ships it.

### Control-plane rolls

Cluster API replaces a control-plane node by adding its successor first, in every distribution. kubeadm's and RKE2's providers remove the old node first only with three control planes or more. k3s's never does.

With one control plane, etcd therefore runs on two members from the new one's promotion until the old one's removal. The API stops while either stalls for longer than etcd's election timeout.

When a roll's new machine is not ready after 20 minutes, the controller sets `ControlPlaneRollStalled` on the Cluster, with whether the API still answers. Recovery resets etcd to the kept machine's own database ([troubleshooting.md](troubleshooting.md#one-control-plane-its-roll-stalled)).

RKE2's and k3s's providers remove the old server's etcd member while that server still runs. Their servers' units do not start once etcd has left its tombstone in their data directory, so a removed server never joins etcd again.

#### Leader-election leases

k3s runs etcd in one process with the controller manager and the scheduler, which exit once they miss a lease renewal. The node's etcd member goes with them. The other member's quorum goes too. The classes therefore give the control planes' controller managers and schedulers the management cluster's leases (60 s, renewed within 40 s).

Kubehenge does the same for every controller it installs that exits on one: the Cilium operator, the cloud controller managers and the CSI plugin's sidecars. An etcd stall of up to 40 seconds then passes without a restart.

### Certificate renewal

Certificates of a node last a year in kubeadm, RKE2 and k3s. A new machine gets new ones. RKE2 and k3s also renew them on a restart, from 120 days before. The kubeadm class's control plane is replaced 60 days before its certificates run out (`rollout.before.certificatesExpiryDays`).

Kubehenge's controller replaces the other nodes 60 days ahead too. RKE2's and k3s's certificate monitor on each node gives it the dates.

It rolls a workers' pool through the topology's `rollout.after`. It rolls k3s's control plane through its own `rolloutAfter`, which the topology does not set for it. An RKE2 control plane of three or more is replaced one machine at a time by its health check (`cluster.x-k8s.io/remediate-machine`).

It sets `CertificatesExpiring` on the Cluster while anything runs out within 60 days, also for the kubeconfig and talosconfig kept for each cluster. A paused cluster gets it with the reason `Paused` and no replacement. Talos renews its nodes' certificates itself.

### VM health

Cluster API sees a VM only through its node. A paused VM keeps its machine ready until the node's lease runs out. A full storage is not seen at all.

Kubehenge's controller therefore reads Proxmox VE's resource list every 15 seconds, with `VM.Audit` on the workload pool and `Datastore.Audit` on the clusters' storages. For a started VM, the list gives the state QEMU reports, such as paused or a disk error.

The controller sets `VMsNotRunning` on a Cluster whose joined machine's VM has been stopped, paused or crashed for 3 minutes. It sets `StorageNearlyFull` while the cluster's storage is 85% full on a node its VMs run on ([troubleshooting.md](troubleshooting.md#vms-that-do-not-run-storage-nearly-full)). Resuming, starting or replacing a VM is the administrator's call.

#### Names and zones set back

For the VMs in a cluster's own pool, the same read sets back two things the cloud controller manager and the CSI plugin go by:

- **The VM's name.** The controller renames it back to its machine's, with `VM.Config.Options`.
  - A VM whose name does not start with its node's name makes the cloud controller manager report the instance gone. Kubernetes then deletes the Node once it is not ready.
- **The node's zone label.** `topology.kubernetes.io/zone` follows the Proxmox VE node a moved VM runs on. The controller sets it through the cluster's kubeconfig and restarts the CSI plugin's node pod on that node ([clusters.md](clusters.md#a-worker-moved-to-another-proxmox-ve-node)).

#### Hosts that are down

Every 15 seconds the controller reads the nodes and the HA manager's status (`/cluster/ha/status/manager_status`). A machine whose VM sits on a node that is offline or in maintenance gets Cluster API's `cluster.x-k8s.io/skip-remediation` annotation. So does one on a node where the cluster's storage is unavailable. The Cluster gets `RemediationHeld` ([troubleshooting.md](troubleshooting.md#machines-whose-replacement-waits-for-a-host)).

The hold ends when the node is back and the machine's node is Ready. It also ends after the cluster's `hostOutageGraceMinutes` (30 by default, 0 for no ceiling), which each machine's `remediation-hold-until` records. The placer leaves such nodes out of `kubehenge.dev/online-nodes`. Cluster API's patch of the cluster is refused while no node is left.

## Each cluster's own corner of Proxmox VE

A workload cluster's cloud controller manager and CSI plugin controller reach Proxmox VE with the cluster's own token. It reaches that cluster's VMs and volumes alone. Both run on control planes only, so no worker holds it.

- **A pool per cluster** for its VMs, under the workload pool: `<workload pool>/<namespace>.<cluster>`.
- **A user and API token per cluster**, with rights on its pool only.
  - The token reads the VMs and configures their disks.
  - A group of all clusters' users adds space on the workload storages and reading the nodes.
  - No token may delete arbitrary volumes, change a storage's definition or read beyond its pool and nodes. The CSI plugin's newest release and the HA group labels are the exceptions (below).
- **A volume owner VM**, in the pool, never started. The CSI plugin creates volumes under its VMID, so only this cluster's token may attach them.
  - Deleting the cluster deletes its volumes.

### Deleting volumes

The CSI plugin deletes a volume through the storage. Proxmox VE allows that only with `Datastore.Allocate` on the whole storage, which covers every volume on it and its definition.

Kubehenge's controller instead deletes each released persistent volume's volume as an unused disk of the owner VM, with the cluster's own token. The plugin then finds it gone and completes. Proxmox VE deletes a VM's unused disk on the node the VM is on. For a volume on storage local to another node, the controller first moves the diskless owner VM there.

### The CSI plugin's release

Every cluster runs the plugin's last release that works with `Datastore.AllocateSpace`. Its newer releases read the storage's configuration, which takes `Datastore.Allocate`.

Where the management cluster allows it (`cluster_datastore_allocate`), a cluster's `csi` variable can choose the newest release. The controller then grants the cluster's user a role on `/storage/<storage>`, unpropagated. The role holds `Datastore.Allocate`, `Datastore.AllocateSpace` and `Datastore.Audit`. It takes the role back when the variable is switched off.

The admission webhook and `BeforeClusterCreate` allow that only on a storage the cluster has alone. That excludes:

- the management cluster's (passed to the controller by the chart)
- those the VM templates have disks on (recorded every 30 seconds on the ClusterClasses as `kubehenge.dev/template-storages`)
- those another cluster uses

They also refuse any cluster on a storage where another cluster holds the grant. The controller checks the same on every reconcile. When a check fails it takes the grant back, installs the earlier release and records a `DatastoreAllocateRefused` event.

### HA group labels

The cloud controller manager labels each node with the HA groups, or HA node-affinity rules, of its Proxmox VE node. Proxmox VE lists those only with `Sys.Audit` on `/`.

Unless the management cluster switches it off (`cluster_sys_audit`), the module creates a group holding `Sys.Audit` on `/`, unpropagated. The ClusterClasses default each cluster's `ccm` variable to the labels. The controller adds the cluster's user to that group and takes it out when the variable is switched off.

The token can then read the Proxmox VE cluster's own configuration (firewall rules, backup jobs, HA, corosync) and change nothing. It reads no guest, storage or node it has no other rights on. Proxmox VE lets only `Permissions.Modify` on `/` grant anything on `/`. The controller therefore holds `User.Modify` on that one group instead.

Where the management cluster switched it off, the variable defaults to off. The admission webhook and `BeforeClusterCreate` refuse it on. The controller records a `SysAuditRefused` event when it cannot grant it.

### Volume snapshots

The CSI plugin copies a volume for a snapshot, a restore or a clone with `POST /nodes/{node}/storage/{storage}/content/{volume}`, which only `root@pam` may call. Kubehenge instead imports the volume into a new disk of a VM (`import-from`). That takes `VM.Clone` on the source VM, which every cluster's role holds, `VM.Config.Disk` on the target VM and `Datastore.AllocateSpace` on the storage.

A cluster's `csi` variable can ask for snapshots on node-local storage. They bring the VolumeSnapshotClass `proxmox`, and the external snapshotter with its CustomResourceDefinitions except on RKE2, which has its own.

The admission webhook and `BeforeClusterCreate` allow them only on a node-local storage of a type the plugin copies on (`kubehenge.dev/snapshot-storages` on the ClusterClasses). The controller checks again on every reconcile. A failed check leaves the plugin reaching Proxmox VE directly, with a `VolumeSnapshotsRefused` event.

Switching snapshots off removes those releases, then the proxy, its configuration and the copies' namespace once no copy is left. The CustomResourceDefinitions stay, so VolumeSnapshots left behind keep their volumes.

#### The copy proxy

The controller runs the plugin's newest release with its snapshotter sidecar. Beside the plugin's controller, as a native sidecar, it runs `kubehenge csi-proxy` from Kubehenge's own image, pinned by digest. The plugin's configuration for it, `kube-system/kubehenge-proxmox-proxied`, holds the cluster's own token and points at the proxy on `127.0.0.1:8006`.

The proxy passes every call on to Proxmox VE with that token. It verifies Proxmox VE's certificate against the cluster's CA. It answers three kinds of call itself:

- **A copy**, made with the cluster's token in the steps below.
- **Deleting a copy**, through the holder (`delete=unusedN`), as the token may not delete storage content.
- **A storage's configuration**, from the storage list. Proxmox VE shows it singly only to `Datastore.Allocate`.

#### How a copy is made

1. The proxy checks four things:
   - The source is a volume of the cluster's volume owner VM or one of its holder VMs. The plugin names such volumes `vm-<vmid>-…` (below `<vmid>/` on file-based storages).
   - The copy's name is the plugin's for the owner.
   - The copy stays on the source's node.
   - No volume of that name is there.
2. It records the copy as a ConfigMap in the namespace `kubehenge-volume-copies` before anything changes.
3. It imports the volume into a free SCSI slot of a holder VM on the source's node and waits on Proxmox VE's task.
4. It detaches the new disk so that it stays as an unused disk of the holder.
5. It records the copy done with its real volume ID, `vm-<holder>-disk-<n>`, which Proxmox VE chooses.

#### Names translated

Every other call goes on with the copies' names translated:

- In the path, the query and a form body, the plugin's names become the real ones.
- In every JSON answer, such as the storage's content, VM configurations and tasks, the real names become the plugin's again.
- Names are matched as whole volume IDs, so `vm-101-disk-1` is never found inside `vm-101-disk-10`.

A record counts only when it names a copy of the owner and a real volume of the cluster's holders on that storage. Any other record translates and deletes nothing. A Role lets only the plugin's controller account read and write the records, and record events in `kube-system`.

#### Restarts and retries

The record holds the holder, the slot, the volumes the holder had and, once started, the task.

- A proxy that restarts, or the plugin's next call after a sidecar's 5-minute timeout, finds the copy from the record. It waits on the same task and takes the holder's one new volume as the copy.
- It deletes any other volume an interrupted try left, so a volume is never copied twice.
- Copies run one at a time for each cluster. That keeps two from holding a holder's configuration at once.
- A copy under way is not listed under the plugin's name until done. The plugin never takes a half-written copy for a finished one.
- The proxy logs each copy and deletion. Each refusal or failure is a `VolumeCopyFailed` event on the proxy's pod.

#### Holder VMs

Proxmox VE names a copy after the VM it is a disk of and lets a VM have 256 unused disks. The controller therefore gives a cluster that takes snapshots a holder VM on each node it may use.

- Holders are created like the volume owner VM: stopped, without disks of their own, in the cluster's pool, named `<cluster>-<digest>-copies` and tagged `volume-copies`.
- The controller adds another holder on a node once that node's holders have 128 copies each. The proxy fills each up to 255.
- Holders never move, since moving a VM copies its unused disks along. They stay while any copy is left.
- Switching snapshots off removes the empty holders once no copy is recorded. Deleting the cluster destroys them with their copies.
- The controller's deletion of released volumes ([above](#deleting-volumes)) leaves the volumes of restored and cloned claims to the proxy. It deletes them through their holders when the plugin asks.

### Created before the VMs

Kubehenge's controller, in the management cluster, creates all of this before Cluster API creates any VM. Cluster API asks it through a lifecycle hook (`BeforeClusterCreate`, Cluster API's Runtime SDK) and waits until it answers. The controller hands the token to the cluster through a ClusterResourceSet. It installs the cloud controller manager and CSI plugin through the Add-on Provider for Helm.

Its own token may create pools under the workload pool, users in the clusters' group and ACLs on those pools. It may also move and configure the VMs in the workload pool and use the workload storages as the clusters do, and nothing more. With `cluster_datastore_allocate` it holds `Datastore.Allocate` on the workload storages the management cluster does not use, to grant the CSI role. It holds `User.Modify` on the `Sys.Audit` group (above).

### Removed after the VMs

The controller removes the pool, user and volume owner once the cluster's VMs are gone. Removing the owner VM deletes its volumes on shared storage and on its own node. The controller deletes those on other nodes' local storage first. It waits while a node that may hold some is offline.

During a deletion, the controller records on the Cluster the objects in its namespace whose finalizers hold it, with what each reports. It may list, patch and delete those objects, to abandon a deletion that can never finish ([troubleshooting.md](troubleshooting.md#a-cluster-does-not-finish-deleting)).

### Placement of the VMs

The controller also tells `capmox` where to put each cluster's VMs. Left to itself, `capmox` clones every VM onto its template's node when a cluster names no nodes. It places no VM at all while one named node does not answer, which is when a cluster needs replacements most.

So the ClusterClasses leave the nodes to an external patch the controller serves (`GeneratePatches`). The patch uses the cluster's online `allowedNodes`, or every online node when it names none. Every 15 seconds the controller records the online nodes on each Cluster (`kubehenge.dev/online-nodes`). Cluster API computes the topology again when the list changes.

### Release pins for RKE2 and k3s

The same patch has RKE2 and k3s nodes install their release from files they check, in place of their bootstrap providers' online install. That install runs whatever install script and release a tag names on the day a node boots. Neither project signs its releases or images. Kubehenge therefore carries the SHA-256s of each release's checksum file and install script (`pkg/provisioner/releasepins.txt`). `make release-pins` writes the file from the newest five minor versions' releases.

The API server refuses a release Kubehenge does not pin, on a new cluster or a version change. The interface and auto-upgrades offer only the newest pinned release of each minor version.

A node downloads the script, the release and the airgap images of its core and its CNI. It checks each against the pinned checksum file and hands them to its bootstrap provider's air-gapped install. No registry is involved.

### Template records

Every 30 seconds the controller also records what its token finds in the template pool. On the kubeadm ClusterClass it records the Kubernetes versions with templates for each node OS (`kubehenge.dev/kubeadm-versions`, as JSON). On the RKE2 and k3s ClusterClasses it records the node OSes their templates are there for (`kubehenge.dev/node-os-templates`).

On all three it records the builds of every node OS template line (`kubehenge.dev/node-image-builds`) and the storages the templates have disks on (`kubehenge.dev/template-storages`). The same `GeneratePatches` reads the builds to point each machine template at one build ([Images](#images)). On the Talos ClusterClass, whose nodes run Talos rather than a node OS, it records the Talos releases it finds templates of (`kubehenge.dev/talos-templates`).

The interface offers those versions and refuses a cluster whose template is missing. Cluster creators need no privilege on the template pool, since Cluster API clones with its own token.

### HA node-affinity rules

Placing a cluster by one of Proxmox VE's HA node-affinity rules takes reading the rules, which needs `Sys.Audit` on `/`. Where an administrator offers the rules to cluster creators ([install-settings.md](install-settings.md#ha-rule-placements)), the controller reads them every 30 seconds. Its own token holds the privilege. It records each enabled rule's name and allowed nodes in the ConfigMap `kubehenge-ha-rules`, except the rules of clusters' Proxmox VE HA.

The interface's account may read that ConfigMap only while the setting is on. The interface asks Proxmox VE with the user's own ticket first. When Proxmox VE refuses, it hands over the recorded rules, but only to a user holding `VM.Allocate` on the workload pool or a cluster's pool.

### Proxmox VE HA

With Proxmox VE HA switched on ([install-settings.md](install-settings.md#proxmox-ve-ha)), the controller makes each joined control-plane VM on shared storage an HA resource. Each cluster gets a negative resource affinity among those VMs, sized to the nodes Proxmox VE's HA manager knows. It also gets a strict node affinity to the cluster's nodes when it may use only some.

Writing HA configuration takes `Sys.Console` on `/`. The controller writes it with a token of its own, in a Job ([below](#tokens-kept-out-of-the-controller)). HA moves VMs behind Cluster API's Proxmox VE provider, so:

- **A pre-terminate hook** (`pre-terminate.delete.hook.machine.cluster.x-k8s.io/kubehenge-ha`) on each control-plane Machine holds the VM's deletion until the controller has taken it out of HA. Proxmox VE refuses to delete an HA resource's VM.
- **A moved VM's node.** The provider would take a moved VM for gone and leave it running.
  - The controller records the node on its ProxmoxMachine (`kubehenge.dev/proxmox-node`). The hook holds until the provider has recorded it too.

### Tokens kept out of the controller

The HA token amounts to a root shell on every Proxmox VE node. The restore token amounts to one in every cluster's VMs. The controller holds neither.

Each is in a Secret of its own beside the controller (`kubehenge-proxmox-ha`, `kubehenge-proxmox-restore`). Only a Job made from the chart's PodTemplate (`kubehenge-proxmox-ha`, `kubehenge-guest-agent`) mounts it. For each HA write, restore step or token test, the controller leaves the request in the Secret `<template>-input`. It then starts a Job from the template. It reads the result from its pod's termination message and deletes both.

Every Job of a template reads that one Secret. The controller therefore runs a template's requests one at a time.

### What a token Job may do

The Job runs once, for at most its step's time, under a service account without API access. The ValidatingAdmissionPolicy `kubehenge-token-jobs` refuses a Job in the namespace that reaches either Secret by a volume or the environment, unless its pod is exactly its template's.

The HA Job checks each write against Proxmox VE first. It creates HA resources only of VMs with Kubehenge's comment. It creates, changes or removes only rules named and commented as Kubehenge's, over resources that carry its comment. Reads of HA state use the controller's own token, which holds `Sys.Audit` on `/`.

The controller may still read Secrets across the management cluster, as Cluster API's providers do. One that is taken over can read the tokens too. What this removes is a long-running process that holds them. It also removes any use of them through the controller beyond the writes and commands above.

## Images

Each distribution boots from a different VM template:

| Distribution | Template |
| --- | --- |
| kubeadm | the cluster's node OS with containerd, the kubelet and kubeadm preinstalled at one Kubernetes version. Its control-plane images are each checked against Kubernetes' release signature |
| Talos | Talos's own image of one release from the Talos Image Factory, with the QEMU guest agent. Talos is its own OS, so no node OS is chosen |
| RKE2, k3s | the cluster's node OS with the distribution's SELinux policy. The distribution installs at boot from checked files |

`kubehenge template create`, or the appliance's Administration page with an administrator's session, fetches the image. It boots the image once where it needs preparing. It then turns the image into a template `capmox` selects by its tags. All of it goes through the Proxmox VE API ([templates.md](templates.md)).

### Node OSes

The operating system under RKE2, k3s and kubeadm is the node OS, chosen per cluster with `nodeOS`. It is AlmaLinux 10 by default or Rocky Linux 10, both in image mode. openSUSE Leap 16.1 Immutable follows once released.

Each node OS is a family in `pkg/templates`, which holds its image source, signature check, signing keys, template recipe, lowest CPU type and enforcement. Every family enforces SELinux.

- **Image mode** (AlmaLinux, Rocky Linux). The build VM composes a minimal root file system from the family's signed repositories with `bootc-base-imagectl`.
  - A container build adds what the distribution needs, checked with `bootc container lint`. `bootc install` puts it on the template's disk, which boots once for its checks.
  - The node's `/usr` is read-only and its automatic updates are off. A new template build replaces the nodes instead.
- **Transactional** (openSUSE Leap Immutable). The build VM installs what a node needs as one `transactional-update` snapshot, with automatic updates and reboots off.

#### A cluster's node OS

`GeneratePatches` points each machine template at the cluster's node OS's template line (the tag `os-<family>-<release>`). It has RKE2 and k3s start with SELinux. Clones keep their template's CPU type. Changing the variable rolls the nodes as an upgrade does.

The controller places a cluster only on the online nodes whose CPU reaches its node OS's level (read from each node's CPU flags). It places a Talos cluster on those that reach `x86-64-v2`, the CPU type of Talos's templates. Its admission webhook refuses a node OS none of the cluster's nodes can run, or one that is not released.

### Node image refresh

A node OS template is a line of timestamped builds. Every week a CronJob in the management cluster (`kubehenge template refresh`) makes a new build of each line.

- **Which build a machine clones** follows the cluster's node image time (`kubehenge.dev/node-image`), at first its creation time.
  - `GeneratePatches` selects the newest build of each line made by then and past the soak time (24 hours by default, `--node-image-soak`).
  - Scale-ups and repairs get the same build until the time moves.
- **The time moves** two ways.
  - The upgrader moves it to now inside the upgrade window, once a newer build has soaked and the cluster settled. `upgrades.nodeImages: false` stops that.
  - It checks every five minutes, and at once on a change of `upgrades`, a settled cluster or new builds.
  - The `BeforeClusterUpgrade` hook moves it too, so a Kubernetes upgrade takes the newest builds.
  - A hold outside the window is recorded in `kubehenge.dev/node-image-held`, with the waiting build and when the window opens.

#### The refresh job

The refresh holds a Lease so that one run builds at a time. It builds each line, then deletes all but its two newest builds. It keeps any build a cluster's node image time picks and any a ProxmoxMachine still waiting for its VM selects. It writes how it went to the ConfigMap `kubehenge-template-refresh`, which the interface shows.

Build now and creators' requests go through the ConfigMap `kubehenge-template-requests`, where the interface may only write a request. The controller starts the CronJob's Job for it (with `--missing` for a creator's request). The interface never holds the builder's token.

The refresh's Proxmox VE user and token may build and delete VMs in the template pool and nothing else ([install-settings.md](install-settings.md#what-the-template-builds-may-do)).

### Talos upgrades

Talos nodes are upgraded in place, not replaced. Each Talos release's template stands for a build of its minor version's line. The refresh keeps the two newest releases of each minor, and any older one that:

- a cluster's new machines boot or its nodes are upgraded to
- a machine still being created selects
- someone asked for in that run

It deletes the rest.

- **Which template a machine clones** is the Talos release the cluster records for new machines (`kubehenge.dev/talos-image`), at first `talosVersion`.
  - The `BeforeClusterUpgrade` hook moves it to the current `talosVersion` once that template is in the pool.
  - A new machine boots that release and is then upgraded like the others. Changing `talosVersion` never rolls the cluster.
- **Automatic patch upgrades.** With `upgrades.nodeImages` on, the upgrader moves `talosVersion` to its minor version's newest patch release, inside the upgrade window.
  - Talos releases get no soak time. A new minor version is always an operator's choice.

#### The in-place upgrade

The controller upgrades a node off `talosVersion` through the Talos API, one at a time, control planes first:

1. It has Talos install the release's installer to the inactive partition.
2. It waits while a StatefulSet with a ready pod on the node is short of replicas, then drains and reboots the node.
   - Each wait is bounded by the node's `nodeDrainTimeoutSeconds` (the control plane's or pool's, else the class's, else 60 minutes).
   - At the timeout it reboots the node with a `TalosDrain` warning event. What holds the node is in `kubehenge.dev/talos-waiting`.
3. It uncordons the node once it is Ready on the new release.
4. It deletes the DaemonSet pods the kubelet rejected while shutting down.

Talos moves one minor at a time, so a jump of two passes the newest release between. Progress and the last upgrade are recorded on the Cluster (`kubehenge.dev/talos-upgrading`, `kubehenge.dev/talos-upgraded`). A Kubernetes upgrade waits meanwhile, with `UpgradeHeld` at reason `TalosUpgrade`.

#### A failed Talos upgrade

A failed upgrade stops the rest:

- A node that does not come back Ready, or comes back on its old release because Talos rolled back, is recorded in `kubehenge.dev/talos-upgrade-failed`.
- The controller upgrades nothing more in the cluster until an operator removes the record (**Try again** on the cluster's page).
- A stalled install (no word for three minutes) or an unreachable or busy node is retried after one, two and four minutes. Nothing on the node changes before the reboot.
- A node whose machine Cluster API deletes mid-upgrade, as when the cluster scales down onto it, has its upgrade dropped, not failed. A machine being deleted is never picked.

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

Kubehenge's controller turns a cluster's add-on variables into HelmChartProxies beside the cluster. Switching an add-on off deletes its proxy, which uninstalls it. Values never carry a secret, since the Add-on Provider for Helm keeps them in plain text.

### Charts and images pinned

Every chart is pinned by its digest, which a proxy's version carries as `<version>@sha256:<digest>`. Kubehenge's build of the Add-on Provider for Helm pulls an OCI chart by its manifest digest alone. It checks that the chart is the pinned version. A publisher that moves the version's tag to a new build changes nothing for a cluster. It checks a chart from an HTTP repository against the SHA-256 of its archive that the repository's index lists.

The images are pinned the same way. Each chart's entry in the controller chart's values names every image it runs by tag and digest. The controller merges those values under the ones it sets itself. An ImageSet named for its release, applied with Calico's Installation, pins the images the Tigera operator deploys Calico from.

### Headlamp add-on

Headlamp (`headlamp: {enabled: true}`) has no rights of its own. Its service account is bound to no role. Everyone signs in with their own token or through OIDC and sees exactly what their credentials allow.

`access: view` binds its service account to Kubernetes' built-in `view` ClusterRole. It starts Headlamp with `-unsafe-use-service-account-token`, which sends every request with the account's token and drops any token a user gives. Everyone who reaches it then reads what `view` allows (no Secrets, nodes or other cluster-wide objects but namespaces) and changes nothing. Switching back to `token` removes the binding.

Headlamp is not exposed. `kubectl -n headlamp port-forward svc/headlamp 8080:80` reaches it from a workstation. `hostname` adds an Ingress on the cluster's ingress controller. It uses the certificate from `tlsSecret`, or one from the certificates add-on while it is on, or the controller's default.

Its plugins are those in its image, with nothing fetched at run time. Its pod meets Pod Security's restricted level.

### Monitoring add-on

Monitoring (`monitoring: {enabled: true}`) installs kube-prometheus-stack into the `monitoring` namespace, with cert-manager.

Prometheus keeps ten days of metrics on a 20 GiB volume from the cluster's default storage class. Alertmanager keeps its silences on 1 GiB. `hostname` serves Grafana through the cluster's ingress controller, with the certificate from `tlsSecret` or the controller's default. Without it, `kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80` reaches it. Its `admin` password is generated into the `kube-prometheus-stack-grafana` Secret.

A cert-manager CA of the add-on's own (ClusterIssuer `kubehenge-metrics`) signs only the scrape proxies' certificates and Prometheus's client certificate. The chart's long-lived token goes only to targets that ask for one, over HTTPS.

#### Authenticated scrapes

Every scrape is authenticated and verifies its server:

- **etcd, kube-proxy, the controller manager and the scheduler** serve metrics on loopback only.
  - A kube-rbac-proxy on the node's address takes only Prometheus's client certificate and asks the API server whether it may read `/metrics`. It lets loopback through without credentials.
  - The proxies verify the controller manager's and scheduler's HTTPS against the cluster CA for `localhost`. etcd and kube-proxy serve plain HTTP.
  - Prometheus gets no etcd client certificate.
- **Kubelets** on kubeadm get serving certificates from the cluster CA, approved by the controller when they name only the node and its Machine's addresses. RKE2 and k3s sign theirs with the server CA.
- **node-exporter** listens on loopback behind a kube-rbac-proxy that takes the same client certificate. Its namespace is privileged, the rest of the add-on's restricted.

#### Control-plane metrics by distribution

How the controller manager and the scheduler are scraped differs by distribution:

| Distribution | Their certificates and metrics |
| --- | --- |
| RKE2 | certificates from the server CA |
| kubeadm | certificates from the cluster CA, which the class gives each control-plane node. Left alone, a CA of their own would sign them anew on every start |
| k3s | every component runs in one process, whose kubelet already serves all their metrics. Prometheus takes them from the kubelet, as Rancher does. The chart's alerts look for them there |
| Talos | certificates signed by the components themselves, and kept inside their pods with no way to replace them. No proxy could verify them, so they go unscraped, with their alerts. Once their static pods' arguments all pass `--tls-cert-file`, the controller scrapes them through proxies as on kubeadm, with no change to the class |

The release carries the proxies and certificates, so switching monitoring off removes them with it.

### Logs add-on

Logs (`logs: {enabled: true, collector: node|api, retentionDays: 7, sizeGiB: 10}`) need monitoring, whose Grafana reads them. The validating webhook and the interface refuse them without it, and refuse switching monitoring off under them.

Loki runs as one binary in the `logging` namespace, with its chunks and index on a volume from the cluster's default storage class. The volume only grows and is kept when logs are switched off. Loki deletes logs after `retentionDays`, never by size, so `sizeGiB` has to hold the days kept.

Loki listens on loopback only, behind a kube-rbac-proxy with a certificate from the monitoring add-on's CA. The proxy takes a client certificate from that CA or a token. It asks the API server whether the client may push (Alloy's user) or read (Grafana's user). A NetworkPolicy lets only Alloy's and Grafana's pods reach it.

Switching logs off uninstalls Loki and Alloy and takes Grafana's Loki data source away.

#### Log outputs

`logs.outputs` adds up to four destinations beside Loki, which always runs with logs. Each source's `forward_to` lists Loki and every output that takes its stream. Syslog and OTLP outputs pass through `otelcol.receiver.loki`. Loki outputs are another `loki.write`.

An output's credential is a stored credential with the purpose `log-<name>`. The controller copies it as an optional Secret into both Alloy namespaces. It records on the Cluster what it wrote, and reaches the workload cluster only when that changes. The configuration reads the files with `local.file`, so no secret is in the chart values.

#### Collectors

One Grafana Alloy Deployment in `logging`, at the restricted level, keeps the cluster's events. The pods' logs come from one of two collectors:

- **`node`**, the default, is a second Alloy release: a DaemonSet in `logging-node`, exempt from pod security as it runs as root.
  - It mounts the pod logs, the journal and the API server's audit log directory read-only. It keeps its read positions on the host, so a restart neither loses nor repeats lines.
  - It gets the logs of pods that end within seconds, every unit's journal and, on control planes, the audit log. It holds no Kubernetes rights or token.
- **`api`**: the Deployment in `logging` also reads pods' logs through the API server, with rights to list pods and read logs.
  - It is lighter, but misses the journal, the audit log, and pods that end before it follows them.

Switching the collector installs or uninstalls the DaemonSet and changes the Deployment's configuration.

#### Logs of Talos nodes

On Talos, the `node` collector reads no journal, because Talos keeps its services' logs in memory. The class has Talos send them, and the kernel's, as JSON lines over TCP to `169.254.51.70:5170` (`machine.logging.destinations` and a `KmsgLogConfig`). Every node gets that link-local address on a dummy link of its own (`kubehenge-logs`), where the collector listens through a host port.

The collector listens with Alloy's experimental `otelcol.receiver.tcplog` and runs at Alloy's `experimental` stability level for it. A NetworkPolicy lets nothing else reach the port, so only the node's own Talos can send to it. The lines arrive under `job="talos"` with the service as `unit`, `kernel` for the kernel's.

Talos keeps the audit log in a directory only the API server's own user may enter. There the collector holds `DAC_READ_SEARCH`, its one capability, over what it mounts read-only.

### Backups add-on

Backups (`backups: {enabled: true, target: …}`) installs Velero with its AWS plugin into the `velero` namespace. It backs up on the cluster's schedule to one of the S3 targets an administrator set (the chart's `backupTargets`, on the ClusterClasses as `kubehenge.dev/backup-targets`), under `<prefix>/<namespace>.<cluster>/velero`. Velero's file-system backup reads every pod's volumes with Kopia, file by file, on any storage and without snapshots.

Switching it off removes the release, Velero's resources with their finalizers taken off, its CRDs and the `velero` namespace with the keys in it. The backups stay in the store and the keys beside the Cluster, for switching on again.

What is left out of a backup:

- Namespaces: `kube-system` (which holds the cluster's Proxmox VE token), `kube-public`, `kube-node-lease`, Velero's own and the add-ons'. Kubehenge installs the add-ons again itself.
- What each node, the API server and the CNI keep of themselves:
  - nodes, events and leases
  - CSR and volume attachments
  - Cilium's and Calico's addresses and identities
- emptyDir volumes.

#### Keys

Kubehenge's controller makes the cluster's keys once, when backups or etcd snapshots are first switched on. It keeps them beside the Cluster in the Secret `<cluster>-backup-keys`:

- An age identity its etcd snapshots are encrypted to (below).
- A 32-byte SSE-C key, sent with every request, with which the store encrypts every object Velero writes.
- The password of the cluster's Kopia repository, which encrypts the volumes' data before it leaves the node.

The keys never reach the store, so nobody with access to the bucket reads a cluster's backups. They are never changed. Deleting the Secret makes the backups unreadable for good. The Cluster does not own the Secret, so a cluster made again under the same name reads the backups of the one before.

Before the chart installs, the controller writes the keys and the store's access key into the workload cluster's `velero` namespace, never into values.

#### Access keys

Velero signs in to the store with the cluster's own credential, typed into the interface. Alternatively `existingSecret` names a Secret the owner keeps in `velero`. The controller reads credentials when it reconciles the cluster, with no watch on Secrets. A new one reaches Velero within ten minutes.

The credential an administrator stored for every cluster reaches every cluster's folder. Only the controller uses it, for the etcd snapshots of clusters without a key of their own. It never goes into a cluster, whose admins could delete the other clusters' backups with it.

#### Velero's rights

Velero's server runs without root or capabilities, at Pod Security's restricted level, but as `cluster-admin`, which backing up and restoring every resource needs.

- Its node agents find every pod's volumes through a hostPath of the kubelet's directory, unconfined by SELinux (`spc_t`).
- Each volume is read by a privileged backup pod of its own on the pod's node.
- The `velero` namespace is therefore exempt from pod security, as the CNIs' are. Its metrics, served without authentication, are off.

#### The interface's backup identities

Two of the interface's identities in the cluster ([Interface](#interface)) live in `velero` while backups are on. Each has a Role over Velero's own resources and nothing else:

- `kubehenge-backups` lists backups and restores and starts a backup, a `DeleteBackupRequest` or a `DownloadRequest` for a restore's results. It serves everyone who may see or change the cluster.
- `kubehenge-restores` reads the backup locations and starts a restore. It serves those who may download the admin kubeconfig, since a restore writes Secrets and RBAC too.

Neither reads a Secret. The interface fetches a restore's results from the link Velero hands out. It fetches only from the target's own endpoint, with the SSE-C key from the management cluster.

#### Reading other clusters' backups

The controller adds a read-only backup location `cluster-<name>` for each cluster the `readFrom` field names. It points at that cluster's prefix in the same target.

- Its SSE-C key goes in the `velero` namespace. The controller marks on the location whether the two clusters share a Kopia repository password, which Velero keeps one of for all its locations.
- A cluster whose keys are made while it reads another's takes that cluster's password, so it restores the volumes' data too. For any other source, the interface asks only for the resources.
- The validating webhook refuses a source that is not on the same target or whose keys Kubehenge does not have. A source taken out of `readFrom` takes its location, its Backup objects and its key out of the cluster.

### etcd snapshots add-on

etcd snapshots (`etcdSnapshots: {enabled: true, target: …}`) are taken by Kubehenge's controller itself, with nothing installed in the cluster. They go into the same targets under `<prefix>/<namespace>.<cluster>/etcd` ([clusters.md](clusters.md#etcd-snapshots)).

#### Reaching etcd

Every control-plane provider keeps the cluster's etcd CA beside the Cluster (`<cluster>-etcd`). For each snapshot the controller signs a P-256 client certificate from it in memory, valid for an hour and never stored.

The controller reaches one member through the workload cluster's API server. On kubeadm and RKE2 it port-forwards to the member's static pod, on k3s to the k3s provider's `etcd-proxy` pod. It checks the member's serving certificate against the same CA for `localhost`.

The API server sees only TLS between the controller and etcd. No etcd key and no access key to the store reaches the workload cluster, unless the cluster opts into the distribution's own upload (below).

On Talos, etcd runs as a Talos service rather than a pod. The controller asks a control plane's Talos API for the snapshot, which has no revision, with the talosconfig beside the Cluster (`<cluster>-talosconfig`).

#### Streaming

etcd's snapshot stream goes straight into an S3 multipart upload in parts of 8 MiB, encrypted on the way with age to the cluster's identity. The controller holds the same few buffers whatever the database's size.

- etcd ends the stream with the SHA-256 of what it sent, which the controller checks before keeping the object. One cut off is deleted.
- A JSON object beside it (`<snapshot>.json`) holds the database's SHA-256 and size, the Kubernetes version, the distribution and the etcd revision, read just before the snapshot. The snapshot itself is never rewritten.
- One snapshot runs at a time across clusters, each up to 10 minutes after the scheduled time.
- A failure is retried after 15 minutes, with a `Warning` event and the reason in the condition `EtcdSnapshotSucceeded`.
- After each snapshot, all but the newest `keep` are deleted, each with its JSON object.

#### Restoring

A restore is asked for with the annotation `kubehenge.dev/etcd-restore` on the Cluster, which the interface sets for those with `Sys.Modify` on the cluster's pool. Each step is recorded in `kubehenge.dev/etcd-restore-status`.

- While it runs, Cluster API is paused and the controller changes nothing else of the cluster, scheduled snapshots included.
- First the restore snapshots etcd as it is into the same target, which can undo a restore from the wrong snapshot. Its status names it.
- A cluster whose etcd does not answer is restored without that snapshot. The status says why.
- The snapshot's SHA-256 comes from its JSON object. It must be 64 lower-case hexadecimal digits, since it goes into commands run as root.

#### Restoring kubeadm, RKE2 and k3s

Each step runs as a root script through the QEMU guest agent, in a Job ([tokens](#tokens-kept-out-of-the-controller)) with the restore token ([install-settings.md](install-settings.md#etcd-restores)).

- The controller stores the snapshot encrypted again for `openssl enc -aes-256-ctr -pbkdf2`, with a one-off random password.
- For RKE2 and k3s, whose restore keeps the revision, it first moves the revision on in a checked copy (etcdutl's `--bump-revision 1000000000 --mark-compacted`).
- The node fetches it from a presigned link valid for under an hour, with the target's CA. It checks the SHA-256 and deletes the copy.
- The restore ends once the API server answers on the cluster's address, not only on the restored node.
- Every step is safe to rerun, so no restart leaves one half-run.
- A restore that fails before etcd's database was swapped starts every control plane as it was. One that fails after finishes the restore.
- Without the restore token the interface writes out the same scripts as steps to run by hand.

#### Restoring Talos

A Talos cluster is restored through its nodes' Talos API with its talosconfig, with no restore token.

1. The controller reads the whole snapshot and checks it against its SHA-256 before anything changes.
2. It resets every control-plane node with its `EPHEMERAL` partition wiped. That takes etcd's data and keeps the machine configuration.
3. It waits for each node to boot.
4. Once the first node's etcd waits to be bootstrapped, it streams the snapshot there, checks it on the way and bootstraps etcd from it.
5. The other nodes join the restored etcd by themselves, so no machine is replaced but one whose Talos API did not answer.

Once any node may have been wiped, only the snapshot brings etcd back. A restore that fails from there, or that the controller restarted in, is therefore tried once more from the start.

#### The distribution's own upload

On RKE2 and k3s, `distributionUpload: true` beside `enabled` also has the servers upload their own snapshots, to the same target under `<prefix>/<namespace>.<cluster>/<distribution>-etcd`.

A patch of the class writes the drop-in `60-kubehenge-etcd-s3.yaml` into the distribution's `config.yaml.d`. It holds `etcd-s3`, the schedule, the number kept and `etcd-s3-config-secret: kubehenge-etcd-s3`. Changing the upload, its schedule or the number kept rolls the control-plane machines.

The controller writes that Secret into the cluster's `kube-system` (type `etcd.k3s.cattle.io/s3-config-secret`). It holds the target's endpoint, bucket, folder, region, CA and access key, with TLS verification on. The servers read it at each snapshot, so a new key or target needs no restart. The key never enters the bootstrap data.

- `kubehenge.dev/etcd-upload-installed` on `<cluster>-backup-keys` records that the Secret went into the cluster. The controller removes it once the upload is off and no control-plane Machine may still use the drop-in.
- The distribution reads the schedule in UTC and counts the number kept across all servers.
- Kubehenge neither encrypts, lists nor restores these snapshots.

### Istio add-on

Istio (`istio: {enabled: true}`) installs Istio in ambient mode, without sidecars: istiod into `istio-system`, and ztunnel and istio-cni on every node into `istio-node`. A namespace or pod labelled `istio.io/dataplane-mode=ambient` joins the mesh. Its traffic goes through the node's ztunnel to the other side's, over mutual TLS with each workload's identity. Istio's policies by identity therefore apply, such as `PeerAuthentication` or an `AuthorizationPolicy` by service account.

ztunnel and istio-cni need Pod Security's privileged level, so `istio-node` is exempt. istiod meets the restricted level. Cilium's `cni.exclusive` is off, with or without Istio, so Cilium leaves istio-cni's plugin in place.

Waypoints, Istio's layer 7 proxies, are Gateways of the class `istio-waypoint` ([Networking](#networking)). Switching Istio off first removes the `istio.io/dataplane-mode` label everywhere. Once istio-cni has removed every pod's redirection, it uninstalls the charts. Istio's CRDs stay, with any resources of them.

### Cilium Cluster Mesh add-on

Cilium Cluster Mesh (`clusterMesh: {enabled: true, peers: [...]}`) links clusters of one namespace that run Cilium: Kubehenge's own on kubeadm, k3s and Talos, or RKE2's bundled one, configured through a `HelmChartConfig`.

Two clusters are linked while both have the mesh on and list each other. A link lets each cluster's pods reach the other's, by address and through global services. Each reads the other's pod names, labels and identities, which Cilium's network policies select by.

Every Cilium cluster has, from the start, a name and a Cilium cluster id unique in its namespace (`kubehenge.dev/cilium-cluster-id`), both fixed for its life. Its pod range stays clear of every other cluster's ([Networking](#networking)).

A link is refused, with a `ClusterMeshNotLinked` event that says why, while the ranges overlap, the ids clash or an endpoint is unknown.

#### Mesh certificates and peers

With the mesh on, Cilium runs the clustermesh-apiserver, which peers reach on NodePort 32379 of the control-plane endpoint. KVStoreMesh caches the peers' state for the agents. Kubehenge's controller writes the certificates, the peers' endpoints and the peers' users into `kube-system`.

Each namespace has a Cluster Mesh CA in the Secret `kubehenge-cluster-mesh-ca` beside its clusters, ECDSA P-256 and valid for ten years. Its key never leaves the management cluster. The certificates it signs last a year and are renewed with a third of that left. Each peer signs in as a user of its own that may read the cluster's state only.

Adding or removing a peer restarts nothing. Switching the mesh on or off upgrades Cilium and restarts its agents. On RKE2 with CIS hardening, a NetworkPolicy lets the peers through to the clustermesh-apiserver.

### Certificates add-on

Certificates (`certificates: {enabled: true, issuer: …, domain: …}`) installs cert-manager with its Gateway API support on, once the cluster has the Gateway API. Kubehenge's controller writes the rest into the workload cluster once cert-manager answers.

The issuer is the ClusterIssuer `kubehenge-ingress`: ACME, a CA, or none with the issuer `external`. Every certificate and cert-manager's default issuer name the one the owner gave. An Ingress annotated `kubernetes.io/tls-acme: "true"` gets its certificate from it.

- ACME answers DNS-01 through one of cert-manager's DNS providers, or HTTP-01 through an HTTPRoute on Traefik's Gateway.
- An external account binding reads its HMAC key from the Secret `kubehenge-acme-eab` in `cert-manager`, copied from the `acme-eab` credential. Without it the issuer is left out.

The annotation `kubehenge.dev/certificates` marks a cluster while the workload cluster may hold these objects. Switched off, the controller deletes them and lifts the mark. cert-manager goes when neither monitoring nor Rancher needs it.

#### The default certificate

With DNS-01 or the CA, the Certificate `kubehenge-ingress` in `kube-system` covers `domain` and `*.domain`, ECDSA P-256.

- Traefik's default TLS store and its Gateway's `websecure` listener (HTTPS, port 8443 behind the nodes' 443) serve it.
- The add-ons' Ingresses take it where it covers their host name, and ask the issuer otherwise.
- On kubeadm and Talos it is in the values of the Traefik Kubehenge installs. RKE2's and k3s's own Traefik reads it from the Secret `kubehenge-traefik` in `kube-system`, kept in step by the ClusterResourceSet.
- No change of certificates replaces a node.

#### The cluster's CA and credentials

- **The cluster's CA**, with the issuer `ca`, is the Secret `<cluster>-ingress-ca` beside the Cluster and owned by it, ECDSA P-256, valid for ten years.
  - It is name-constrained to `domain`, with every IP address excluded.
  - The issuer reads a copy in `cert-manager`. The CA survives switching the add-on off and on, but not a new domain.
  - The interface hands out its certificate to whoever may see the cluster.
- **The DNS provider's credential** is a user credential (purpose `dns01`) beside the Cluster, or with `sharedCredential` (needs `Sys.Modify`) the one stored for every cluster.
  - The controller copies it into `cert-manager` (`kubehenge-dns01`), never into a chart's values.
  - Storing or removing one stamps `kubehenge.dev/credentials-stored-at` on the Clusters using it, which the controller takes at once.
  - `existingSecret` names a Secret of the cluster's own instead, which Kubehenge never reads.

When the CA or ACME server behind the issuer changes (`kubehenge.dev/issuer-identity`), the controller deletes the Secret of every certificate it issued. cert-manager then issues each again.

### GitOps add-on

GitOps (`gitops: {enabled: true, engine: argocd | flux, repository: {url: …, revision: …, path: …}}`) keeps a cluster in step with a directory of a Git repository. The engine applies what the directory holds, plain manifests or a kustomization, and removes what goes from it. Argo CD goes into `argocd`, Flux into `flux-system`, both at Pod Security's restricted level with NetworkPolicies of their own.

The engine applies with cluster-wide rights, so choosing it takes `Sys.Modify` on the pool, as the admin kubeconfig does.

Once the engine's chart is in, the controller writes the repository's credentials and the object that starts the sync into its namespace. That is an Argo CD Application `kubehenge`, which syncs automatically, prunes, heals manual changes and retries without end, with no resources finalizer. For Flux it is a FluxInstance `flux`.

#### Flux

The Flux Operator installs Flux from the manifests inside its image, at the version the controller chart names (`addons.fluxOperator.flux`).

- The operator's digest pins Flux and every image it names.
- Flux's source, kustomize, helm and notification controllers and the operator each request 10m of CPU rather than Flux's 100m. That fits them beside the other add-ons on a 2-vCPU RKE2 worker.
- The Flux Operator is licensed under AGPL-3.0. Kubehenge runs it unmodified.

#### Switching the engine

Switching GitOps off, or to the other engine, removes the engine and leaves what it deployed running.

- For Argo CD the controller deletes the ApplicationSets, orphaning their Applications, then the Applications without their finalizers, then the chart.
- For Flux it deletes the FluxInstance, on which the operator uninstalls Flux, then the chart.
- The engine chosen starts syncing only once the other has gone.
- The engines' CRDs go with them. The deploy key and host keys stay beside the Cluster, for switching on again.

#### Reading the repository

`auth` takes one of four values:

- `none` for a public repository over HTTPS.
- `deployKey`, an Ed25519 key pair the controller makes and keeps in the Secret `<cluster>-gitops` beside the Cluster.
  - The interface shows its public half, to add to the repository as a read-only deploy key.
  - A new URL gets a new key.
- `token`, a user name and read-only token over HTTPS, the cluster's `gitops` credential.
- `existingSecret`, a Secret of the owner's own in the engine's namespace.

The engine gets its copy of the credentials as `kubehenge-repository` in `argocd` or `kubehenge-gitops` in `flux-system`, never in values.

Over SSH the engine trusts only the host keys in `knownHosts`, or in the owner's own Secret for Flux. The webhook and the ClusterClass's schema require at least one line for the repository's host and port, plain, wildcarded or hashed. Nothing reads a host's keys from the network. The interface shows their fingerprints to compare.

#### The engines' web interfaces

**Argo CD's interface** signs in through the cluster's first OIDC provider, as its client or `signIn.clientID`.

- The client is a public one with PKCE. With `signIn.clientSecret` it is confidential, its secret the `oidc-client` credential or the owner's own Secret.
- Its local admin and web terminal are off, and Dex and notifications are not installed. Only the provider's groups in `admins` have rights in it.
- `hostname` serves it through the cluster's ingress controller. Without one, `kubectl -n argocd port-forward svc/argocd-server 8080:80` reaches it.

**Flux's web interface** (`ui: true`) is the Flux Operator's, off by default. At `hostname` it shows every Flux object and the workloads they deployed, which those signed in may reconcile, suspend and resume.

- It signs in through the cluster's first OIDC provider with a client secret from a Secret.
- It impersonates the signed-in user with the provider's prefixes, so the cluster's RBAC decides what each may do.
- With `acme-http01`, the NetworkPolicy `kubehenge-acme-http01-solver` lets cert-manager's HTTP-01 solvers in `flux-system` be reached for its certificate.

### Pod security add-on

Pod security (`podSecurity: {admission: …, kyverno: …}`) holds a cluster's pods to a [Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/), `baseline` or `restricted`, through either or both of two checks:

- **Pod Security Admission**, Kubernetes' own.
  - Every ClusterClass gives the API server an admission configuration. A namespace that labels no level of its own gets the cluster's `admission` value, `baseline` by default.
  - At every level, `privileged` included, it warns about and audits pods short of `restricted`.
  - The configuration is a file on the control-plane nodes, so changing the level replaces them one at a time.
- **Kyverno.** `kyverno` installs Kyverno and its Pod Security Standards policies into the `kyverno` namespace.
  - They hold every namespace to the level whatever its labels say. Pod Security Admission cannot, since anyone who may label a namespace may lift its level.
  - They deny a pod when Kyverno cannot be reached. Kyverno's policy reports list what falls short.
  - Switched off, the policies go first, then Kyverno.

#### Exempt namespaces

Both leave alone the namespaces of the components that need the host's network, file system or privileges:

- `kube-system` (the cloud controller manager, the CSI plugin, Cilium, kube-vip and the distributions' own components)
- Calico's, node-exporter's and MetalLB's
- Velero's (`velero`, whose node agents read every pod's volumes)
- Istio's node agents' (`istio-node`) and Alloy's node collectors' (`logging-node`)
- those of Rancher's agents and controllers

The chart's `podSecurityExempt` lists them.

Pod Security Admission exempts all of them in every cluster, since its exemptions live in a file on the control planes. Whoever may label a namespace may set it to `privileged` anyway.

Kyverno exempts `kube-system` and the namespaces of only what the cluster runs. Under Kyverno, a `velero` namespace in a cluster without backups is not exempt.

A workload of one's own that needs more than the level gets its namespace labelled. With Kyverno it also needs a PolicyException in the `kyverno` namespace, the only place Kyverno takes them from.

### Security baseline

Every cluster gets the part of the [CIS Kubernetes Benchmark](https://www.cisecurity.org/benchmark/kubernetes) that costs workloads nothing:

- **Audit.** The API server logs every request at the `Metadata` level, kept a month in ten 100 MB files at most. It skips bodies, health checks, leases and events.
- **Secrets encrypted in etcd.** RKE2, k3s and Talos encrypt them themselves. On kubeadm the Secret `<cluster>-encryption` gives every control plane one secretbox key, never replaced.
- **Requests without credentials** are refused on RKE2 and k3s. On kubeadm they reach health checks and the `cluster-info` ConfigMap alone.
- **The API server** limits event rates (EventRateLimit) and does not extend projected ServiceAccount tokens past their hour. It and the kubelets offer only forward-secret, authenticated TLS 1.2 ciphers.
  - Profiling is off in it, the controller manager and the scheduler, which serve on loopback only.
- **Nodes** run with the kernel settings the kubelet expects. kubeadm's kubelet configuration and unit files are readable by root alone.

### CIS hardening

CIS hardening (`cis: true`), which can be switched on after creation and never off, adds what costs workloads something:

- Pod Security Admission enforces `restricted`, whatever `podSecurity` says. The exempt namespaces stay exempt.
- Every container that sets no seccomp profile of its own runs under the container runtime's default.
- Exec, attach and port-forward sessions end after five idle minutes.
- RKE2 runs its own CIS profile.
  - etcd runs as a user of its own.
  - The pods in `kube-system`, `kube-public` and `default` take connections only from their own namespace, DNS and a few of RKE2's own services aside.
  - With monitoring on, Kubehenge's controller lets Prometheus through to CoreDNS's metrics.

#### Benchmark checks

The end-to-end tests run [kube-bench](https://github.com/aquasecurity/kube-bench) on a hardened cluster of each distribution. They test against the CIS Kubernetes Benchmark 2.0 for kubeadm and Rancher's own benchmarks for RKE2 and k3s. They fail on every failed check but two exceptions:

- 1.1.12 on kubeadm, which runs etcd as root and has no supported way to run it as another user.
- 1.1.9 and 1.1.10 on k3s with Cilium or Calico. k3s's benchmark looks only in `/var/lib/cni/networks`, flannel's lease folder, where these CNIs write nothing.

For those two, the test checks the same thing itself. Every node's files in `/etc/cni/net.d` and `/var/lib/cni/networks` must be mode 600 or tighter and owned by root:root.

The benchmark's policies section is the cluster owner's, since it judges what runs in the cluster, not how the cluster is built.

### Rancher add-on

Rancher (`rancher: {install: true, hostname: …}`) goes, with cert-manager, into one cluster Kubehenge creates for it, never into the management cluster. Its server URL is fixed to `hostname`, which must resolve to that cluster's ingress controller. Rancher's first admin password is generated into the `bootstrap-secret` Secret in `cattle-system`, never into values.

Kubehenge's controller registers every other cluster with Rancher once it is available, unless it sets `register: false`:

- It signs in to Rancher's cluster with its kubeconfig, with no Rancher account or token.
- It creates the cluster's `provisioning.cattle.io` Cluster there and applies the agent's manifest to the registered cluster. The agent trusts only Rancher's CA, pinned by its checksum.
- A cluster leaves Rancher before it is deleted (`BeforeClusterDelete`) or when `register` turns false. Rancher's own cluster first removes every registered cluster.
- Each hook reads the Cluster a request names from the API server, since Cluster API does not authenticate itself to the hooks. The delete hook acts only on one being deleted.

#### Versions and size for Rancher

The controller refuses to create the Rancher cluster on Kubernetes versions Rancher's release does not run on. It holds back an upgrade beyond them (`BeforeClusterUpgrade`) until a Kubehenge release brings a Rancher that runs there.

The hook records each hold as the Cluster's condition `UpgradeHeld`, which the interface, `kubehenge cluster list` and kubectl show ([troubleshooting.md](troubleshooting.md#upgrades-held-back)). The condition goes once nothing holds the upgrade, also when the Cluster goes back to the version it runs.

On fewer than 4 vCPUs a node running Rancher starves its own etcd and API server. `BeforeClusterCreate` refuses such a cluster. A validating webhook refuses switching Rancher on or shrinking its nodes later.

#### Rancher's own components

Rancher runs with imported-cluster version management off, so it never upgrades RKE2 and k3s beside Cluster API. The images Rancher starts itself (its agent, shell, webhook and Fleet) are named by digest through its chart's values.

Rancher also installs Turtles, its own Cluster API, without which it never takes a registered cluster as ready. Rancher reads Turtles' values only from its `rancher-config` ConfigMap. Kubehenge's controller writes that ConfigMap, with Turtles' image and its Cluster API controller by digest, before the chart installs and adopts it.

Kubehenge sets the chart versions of Turtles, the webhook and Fleet in Rancher's settings, beside the images it pins for each. Rancher takes the charts from the copy of its chart repository inside its image (`useBundledSystemChart`), never from the repository's Git branch.

## Cluster autoscaler

A worker pool with Cluster API's `cluster-api-autoscaler-node-group-min-size` and `-max-size` annotations and no replicas is autoscaled. Kubehenge's controller runs one [cluster autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler) for each cluster with such a pool, and removes it once none is left.

- **The release** matches the cluster's Kubernetes minor, or the newest earlier one, or the oldest for an older cluster. The chart pins one image per minor by digest, pulled only with the Kubernetes release signature.
- **Scale from zero.** The controller writes on each autoscaled MachineDeployment what a new node holds (`capacity.cluster-autoscaler.kubernetes.io/cpu`, `memory`, `ephemeral-disk` and `labels`). That is the class's cores, 90% of its memory and disk, and the labels Cluster API gives its Nodes.
- **Control planes** stay tainted while any pool may grow.

### Room to grow

The node addresses must hold the control planes, every pool at its most and one machine for a rollout. The validating webhook and `BeforeClusterCreate` refuse a change that makes a shortage worse.

While a pool grows, the controller records `AutoscalerNoRoom` for a worker whose VM Proxmox VE cannot place for want of memory. The autoscaler gives that machine up after 30 minutes.

### Rights of the autoscaler

Each cluster's autoscaler has a ServiceAccount of its own in the `kubehenge-autoscaler` namespace, bound in its cluster's namespace to a ClusterRole. The role reads MachineDeployments, MachineSets, MachinePools, Machines and Proxmox machine templates. It patches the MachineDeployments' `scale` subresource and marks Machines for removal.

A ValidatingAdmissionPolicy narrows that to one cluster's objects. Its parameter, the ConfigMap `kubehenge-autoscaler`, names each account's cluster and autoscaled MachineDeployments. An account it does not name may change nothing. Otherwise it may change only the scale of a pool it names and the removal annotation of its cluster's Machines. The chart creates the ConfigMap empty, as the policy refuses every change without it.

Reads stay namespace-wide, across every cluster's machines.

An autoscaler's Deployment, account, token Secret and policy entry lie outside its Cluster's namespace. The controller holds the Cluster with the finalizer `kubehenge.dev/autoscaler` until all of them are gone. A cluster deleted while the controller is down waits for it.

#### The autoscaler inside the cluster

Inside the cluster it is the autoscaler identity, `kubehenge-autoscaler` in `kube-system` ([Identities and health](#identities-and-health)). That identity reads what its scheduling simulation needs but no Secrets. It updates Nodes and evicts pods to drain the workers it removes. It writes events, and writes no ConfigMap but its own status in `kube-system`. Its tokens are kept and renewed like every identity's.

### Where the autoscaler runs

By default the autoscaler runs in the management cluster, which holds the token into the cluster as it holds the cluster's kubeconfig anyway. The cluster holds nothing of the management cluster. Each autoscaler requests 64 MiB, with a limit of 512 MiB.

The `autoscaler` variable's `placement: workload` runs it on the cluster's control planes instead, as a system-cluster-critical pod. It tolerates their taint and reaches the management cluster at the `managementEndpoint` the module sets. The cluster then holds a management cluster token that reads every cluster's machines in the namespace. The interface therefore asks for `VM.Audit` on the workload pool first.

## Interface

A web app in a small LXC container on Proxmox VE, installed with one line in the Proxmox VE shell. Users create, scale, upgrade and delete clusters there, download kubeconfigs and open each cluster in Headlamp. Administrators also build VM templates, upgrade Kubehenge, and apply, change or remove the management cluster.

- **First run.** A wizard creates the management cluster through the same module `tofu apply` uses, with the administrator's own session ([First run](#first-run)). No administrator token is stored.
- **Day two.** The container keeps the management cluster's state, encrypted.
  - It runs that cluster's Talos and Kubernetes upgrades, its growth from one node to three, moves of its VMs, and repair.
  - It keeps working while the management cluster is unwell, to show and fix it.
  - Its state belongs in Proxmox VE's backups. The interface warns administrators while no enabled backup job includes it by ID, pool or node.

### Sign-in, sessions and permissions

Users sign in through the Proxmox VE API, with no second user directory. Every Proxmox VE realm works, PAM, Proxmox VE users, LDAP, Active Directory and OpenID Connect included.

- A second factor (TOTP, Yubico OTP or recovery keys) is passed on to Proxmox VE as its own sign-in page would.
- A WebAuthn key is bound to Proxmox VE's own address, so its users sign in with an API token (`POST /api/session/token`). That makes a normal session with the same limits.
- A token session ends at its next renewal once Proxmox VE stops taking the token. Its privileges are the token's, which privilege separation cuts down to its user's.

#### OpenID Connect sign-in

- An OpenID realm sends the browser to its identity provider, which returns it to `/api/openid/callback` under `--public-url`. Register that address with the provider.
- Without a public URL OpenID sign-in is refused, since the browser chooses the address it sends.
- The state cookie is host-only, so a sign-in started under another name first redirects to the same path under `--public-url`.
- Proxmox VE checks the code. The interface accepts the return only in the browser that started it.

#### Sessions

- The Proxmox VE ticket stays on the server, in memory only. The browser holds a random session ID in a `__Host-` cookie (Secure, HttpOnly, SameSite=Strict).
- Every change also carries a per-session CSRF token.
- Proxmox VE renews a ticket without asking the realm or the second factor again. A session therefore ends after a fixed time (eight hours by default) however active it is.
- Failed sign-ins are limited per user at an address and per address, never per user alone. Nobody can lock an administrator out from elsewhere.
- The address is the connection's peer. From a peer listed in `--trusted-proxy` it is the right-most `X-Forwarded-For` entry that is not listed.
- The frontend loads no inline script or style. The Content-Security-Policy allows none.

#### Live pages

- The interface watches the management cluster's Clusters and Machines with its own account. It tells each open page of a change as a Server-Sent Event (`/api/events`), at most once a second.
- The page then reads again through the API, so an event names no cluster and each user sees only what they may. Pages also poll, for when the stream is down.
- A proxy in front of the interface has to pass the stream on unbuffered. nginx does on the `X-Accel-Buffering: no` the interface sends.

#### Permissions

Permissions follow Proxmox VE's, by pool. Each cluster has its own pool under the workload pool. The app asks Proxmox VE for the signed-in user's effective privileges on each path, then acts with its own account. Administrators share clusters with the same ACLs they share VMs with.

| Action | Needs |
| --- | --- |
| Seeing a cluster | `VM.Audit` on its pool. A cluster the user may not see is answered as not found |
| Creating a cluster | `VM.Allocate` on the workload pool, and what Proxmox VE asks of anyone creating a VM: `Datastore.AllocateSpace` on the storage and `SDN.Use` on the bridge, VNet or VLAN |
| Deleting, scaling or upgrading a cluster, or changing its add-ons | `VM.Allocate` on its pool |
| Downloading its admin kubeconfig, or restoring one of its backups | `Sys.Modify` on its pool |
| Installing Rancher | `Sys.Modify` on the workload pool, since Rancher receives the admin kubeconfig of every cluster that registers with it |

A role given to a user on a cluster's own pool replaces, as everywhere in Proxmox VE, what they inherit from the workload pool. It therefore has to hold `VM.Audit` too.

### Namespaces and credentials

The interface creates and changes clusters in `clusters`, and in the namespaces an administrator adds beside it (`extra_cluster_namespaces`, the chart's `interface.extraNamespaces`).

- The chart gives the interface's account a Role in each to create, change and delete Clusters and to read and write Secrets. It lists them in the ConfigMap `kubehenge-interface` in `kubehenge-system`, which the interface reads.
- Taking a namespace off the list takes the Role away and keeps the namespace and its clusters. Everywhere else the account reads only.
- Nothing else is per namespace. The controller, the credentials policy and the health identities work cluster-wide.
- A cluster's pool already carries its namespace (`<namespace>.<name>`), and the IP pools are made beside each cluster.
- The ClusterClasses are referenced from `kubehenge-system`.

#### The interface's own token

The interface's account is the ServiceAccount `kubehenge-interface` in `kubehenge-system`. It holds tokens of it that run out.

- Each apply asks for one lasting 30 days with the admin kubeconfig, which stays in the encrypted state. The token goes into a file of its own in the container.
- While the appliance runs, the interface asks for the next token once the one it holds is a day old. It asks with that token, which may make tokens of its own account and no other.
- A token copied out with a backup of the container stops working within a month. An appliance switched off for longer gets a new one from the next apply on the Administration page.
- Deleting the ServiceAccount ends every token of it at once.

#### Credentials typed in

What reaches a service outside the clusters needs a secret:

- a backup target's keys
- a DNS provider's API token for DNS-01 certificates
- a private Git repository's token
- an identity provider's client secret
- the monitoring add-on's alert receivers, remote write target and external Alertmanagers (purposes `alerts-email`, `alerts-webhook`, `alerts-slack`, `alerts-teams`, `remote-write`, `external-alertmanager`)

The interface stores each in a Secret `<cluster>.<purpose>` of the type `kubehenge.dev/user-credential`, owned by the Cluster. The Secret is labelled with its purpose (`kubehenge.dev/user-credential`) and its cluster (`kubehenge.dev/credential-cluster`). The controller copies the monitoring credentials into the Secrets `kubehenge-alertmanager` and `kubehenge-prometheus` in `monitoring`, read as files, never into the chart's values.

Every feature that takes a credential also takes `existingSecret`, a Secret the cluster's owner creates in the workload cluster. It never passes through the management cluster and needs no privilege of Kubehenge's.

#### Who may set a credential

- **Setting one** (storing, replacing or removing) needs `Sys.Modify` on the cluster's pool, as the admin kubeconfig does. Seeing whether one is set needs what seeing the cluster does.
  - A credential for every cluster (`kubehenge-<purpose>`) needs the setup's privileges on `/`.
- **Write-only:** the interface never hands a value back, only whether one is set, when and by whom (`kubehenge.dev/credential-set-by`, `kubehenge.dev/credential-set-at`). Replacing one takes every value again.
  - The log records who changed which credential, never what it holds.
- **Held to them:** the ValidatingAdmissionPolicy `kubehenge-interface-credentials` holds the interface's account to these Secrets.
  - The account may create, change and delete only Secrets of that type, with a purpose it knows and the matching name. It may change or delete none without the label.
  - It cannot overwrite a cluster's kubeconfig, CA or Proxmox VE token, or make a Secret Kubernetes or Cluster API acts on.
  - The policy runs inside the API server, fails closed and binds no other account.

### Identities and health

The management VMs resolve the Proxmox VE API's address with the setup's DNS servers, which may know less than the node the installer ran on. Every minute the controller asks the API's `/version` from its own pod and records the outcome in the ConfigMap `kubehenge-proxmox-reach`. The management cluster's health card reads it and shows the error when the API does not answer.

#### Identities inside the clusters

The interface and the cluster autoscaler act inside a workload cluster as Kubehenge's identities. Each is a ServiceAccount `kubehenge-<identity>` with a role for one job and no right to read a Secret or exec.

- The controller creates each identity once its namespace exists, and puts back what was changed of it.
- Every 20 minutes it asks for a one-hour token of each (TokenRequest API), never stored in the workload cluster.
- The token is kept, with the API server's address and CA, in the Secret `<cluster>-<identity>` beside the Cluster and owned by it. Its expiry is in `kubehenge.dev/token-expires`.
- The autoscaler identity's Secret is kept in the `kubehenge-autoscaler` namespace instead, where the autoscaler's pod mounts it. It is labelled with its cluster and goes when the cluster does.
- An identity a cluster no longer has loses its Secret, and leaves the workload cluster with its namespace.
- The interface reads these Secrets only in its own namespace.

#### Health page

One page says how a cluster stands, in this order:

- what needs attention first
- its nodes
- unwell pods
- waiting volume claims
- the alerts firing, for a cluster with the monitoring add-on
- add-on releases
- what Cluster API reports
- the last hour's events

Anyone who sees the cluster (`VM.Audit` on its pool) sees it. It shows names, reasons and messages, never a spec, an environment or a Secret.

- **Cheap to keep open:** each cluster is read once for every viewer, at most every 10 seconds. The read has a deadline, list limits and the API server's cache.
- Each VM's CPU line for the last hour comes from Proxmox VE with the viewer's own ticket. It shows only the VMs that viewer may see.

#### Where the health page reads

- **From the management cluster**, the interface's own account reads the cluster's machines, MachineDeployments, MachineHealthChecks, control plane, HelmReleaseProxies and its namespace's events.
- **From inside the cluster**, for clusters in the interface's namespaces, identities in the workload cluster's `kube-system` read the rest.
  - `kubehenge-health` may get and list nodes, pods, events and PersistentVolumeClaims. It may read `/readyz` and the nodes' Leases in `kube-node-lease` alone.
  - With monitoring on, `kubehenge-alerts` in `monitoring` may only GET through the API server's proxy to Alertmanager's port, so it cannot set a silence.
  - Once their tokens run out, the page says since when its view is stale.

#### Cordons and logs

- **Cordons:** `kubehenge-nodes`, a third identity there, may get and patch nodes, for those with `VM.Allocate` on the cluster's pool.
  - A ValidatingAdmissionPolicy of the same name refuses that account any change to a node but `spec.unschedulable`.
- **Logs** take `Sys.Modify` on the cluster's pool, as the admin kubeconfig does, since they often carry secrets.
  - They come from `kubehenge-health-logs` alone, which may only read pods' logs. The token behind every viewer's page cannot read a line.
  - The interface hands out the last 60 lines of one container at a time (the run before, for a container that crash-loops). It logs who read which.

### Support bundles and boot logs

A cluster's support bundle is written by one package (`pkg/diagnose`) for `kubehenge diagnose` and for the interface alike, from the management cluster.

- The bundle holds the Cluster and every Cluster API object labelled with its name, with their events and its boot-log ConfigMap.
- It also holds the lines of the controller's, capmox's and the Add-on Provider for Helm's logs that name one of those objects exactly.
- Secrets are never read for it. Fields and lines that look like tokens, passwords, keys or a talosconfig are replaced (`pkg/redact`).
- For it the interface's account may get and list every Cluster API object in its namespaces. It may list pods and read their logs in `kubehenge-system`, `proxmox-infrastructure-system` and `helm-addon-system`.
- A bundle takes `Sys.Modify` on the cluster's pool, as logs do.

#### Boot logs

The interface adds a machine's name to the Cluster's `kubehenge.dev/boot-log-request` annotation and reads the ConfigMap `<cluster>-boot-logs`. The controller reads each log into that ConfigMap, owned by the Cluster, and takes the name off. For this it may get, create and update ConfigMaps in any namespace.

A kubeadm, RKE2 or k3s node's log is two files the controller reads through the VM's guest agent, with its token's `VM.GuestAgent.FileRead` on the workload pool. One is cloud-init's output. The other is a copy of the boot's journals, which a timer of the node template's writes every few seconds into `/var/log/kubehenge`.

A Talos node's log is read through its Talos API with the cluster's talosconfig.

### Activity

The interface logs each sign-in and change with its user. Each such line also goes into `activity.jsonl` in the appliance's folder, which the Activity pages read. It keeps 90 days and stays under 8 MiB by dropping its older half.

#### The management cluster's audit events

The management cluster's API server audits with the workload clusters' policy (`charts/kubehenge/files/audit-policy.yaml`), set through Talos's `KubeAuditPolicyConfig`. With `api_audit_log`, it writes those events to its output instead of a file on the node.

- The interface follows each API server pod's log as it is written. After a restart it first reads the old container's last lines.
- A stream that ends, or brings nothing for 5 minutes, opens again within a minute at the last line read.
- The interface warns when the kubelet rotated lines away before it read them.
- It adds each create, change and deletion once, by its audit ID. A GitOps tool's or a script's service account counts as a person.
- Nodes, the control plane's own users and the service accounts in the module's namespaces are left out. The interface's own changes are in its own record.
- For this it may list pods and read their logs in `kube-system`.

### Headlamp in the appliance

One Headlamp serves the clusters of the interface's namespaces on port 8443, behind the interface's sign-in.

- It is the appliance's own pinned copy of Headlamp, checked against its release's checksum at build time. Its separate origin keeps its pages' scripts away from the interface's session.
- **Its kubeconfig** holds each cluster's API address and CA, and no credential.
  - People sign in to each cluster with their own token or identity provider ([Signing in to clusters](#signing-in-to-clusters)). Headlamp keeps it in an HttpOnly cookie for that cluster's paths only.
- **Isolation:** the server runs as a user of its own (`kubehenge-headlamp.service`, systemd's dynamic user) on loopback. It reads nothing of the interface's state, and only the interface reaches it.
- **Only while used:** systemd starts it on the first connection to its port, in a quarter of a second. It stops after 15 minutes without one.
- Headlamp at a cluster's own hostname is the [Headlamp add-on](#headlamp-add-on).

#### The gate in front

The interface takes the session cookie and strips its own cookies before passing a request on. Host-only cookies reach every port. It checks `VM.Audit` on the cluster's pool for every `/clusters/<namespace>.<name>/` request and rechecks every 30 seconds. It lists only those clusters.

It always refuses what a shared server must not offer:

- port forwards (they would open ports on the appliance)
- the drain routes that name the cluster in the request rather than its path
- the external proxy
- the WebSocket multiplexer
- metrics
- adding clusters or kubeconfigs
- setting a token for others
- plugin changes

A cluster's own `drain-node` route passes. Helm and the plugins Headlamp ships stay refused unless its actions are switched on ([below](#headlamps-actions)).

#### Signed in for kubeconfig holders

The module's `headlamp_sign_in` (the chart's `interface.headlampSignIn`) is off by default. With it on, the gate signs in as cluster-admin those who hold `Sys.Modify` on a cluster's pool, who may download its admin kubeconfig anyway.

Kubehenge's controller keeps two ServiceAccounts in the `kube-system` of each cluster in the interface's namespaces:

- `kubehenge-headlamp` is bound to `cluster-admin`.
- `kubehenge-headlamp-issuer` may make `kubehenge-headlamp`'s tokens (`serviceaccounts/token` by that name) and nothing else. Its one-hour token is beside the Cluster (`<cluster>-headlamp-issuer`) and renewed like the other identities'.

Switched off, the controller deletes the ServiceAccounts, `kubehenge-headlamp` first, which ends every token made for it at once. It then deletes the issuer's Secret.

#### The session's cluster token

On such a user's first request for a cluster, the interface has the cluster make a token of `kubehenge-headlamp` for that session. It lasts an hour, or until the session ends with ten minutes as the floor.

- The interface keeps the token in memory and sets it on that session's requests for the cluster. Headlamp passes it on when the browser holds no token of its own.
- The token never reaches the browser. The gate checks the privilege again every 30 seconds.
- The interface logs each token with the Proxmox VE user and the token's ID, which the cluster's audit log records (`authentication.kubernetes.io/credential-id`).

Headlamp's log out of a cluster (`/encoded-clusters/<cluster>/clear-token`) also drops the session's token. The session is signed in to that cluster again only once it opens it from the interface (`/kubehenge/open/<cluster>`). The session's cookie is `SameSite=Strict`, so no other site's link does that.

#### Headlamp's actions

The module's `headlamp_actions` (the chart's `interface.headlampActions`) is off by default. The interface reads it from the ConfigMap `kubehenge-interface` every 30 seconds.

With it on, the gate serves two plugins Headlamp ships, the Helm app catalogue and the Prometheus charts, read only under `/static-plugins/`. While off they are hidden. The gate passes on a cluster's Helm routes by exact method and path:

- listing releases, and one release with its history
- an action's status
- rollback, uninstall, install and upgrade
- listing the repositories and their charts

#### Helm through the shared Headlamp

Headlamp acts on those routes with the request's credentials, the user's own or the sign-in's.

- An install or upgrade must name a chart of a repository (`repository/chart`) and fetch no dependencies. Headlamp runs in an empty state directory, where no relative path names a chart.
- Headlamp keeps one list of repositories for every user and cluster. Only those who hold the setup's privileges on `/` change it.
  - A repository is reached over HTTPS, with its certificate checked. Its credentials go to its own host alone.
- The app catalogue's Artifact Hub search needs the external proxy, which stays refused ([above](#the-gate-in-front)).

### Changes planned, then followed

A change of a cluster's settings is checked first by `POST /api/clusters/<namespace>/<name>/plan`, a dry-run update of the Cluster that makes nothing. Admission policies and webhooks run on it too. The answer says for each setting which machines it replaces, read from the cluster's ClusterClass:

- A variable that a patch feeds into the control plane's templates replaces the control-plane machines. One fed into a MachineDeployment or MachinePool class replaces the workers.
- A version replaces every machine, replicas add or remove machines, and a variable no patch reads replaces none.

Applying the change records who made it, when and that answer in the annotation `kubehenge.dev/change` on the Cluster. The runs list follows the change from there for a day. It shows the machines of each role made since, ready, against those the cluster asks for and those left from before. It then shows the topology reconciled at the Cluster's generation and the Cluster available.

### Look and build

The fleet is down the left and the runs down the right. It uses Atkinson Hyperlegible Next with JetBrains Mono, in the system's dark or light mode. It borrows no code or look from Proxmox VE's GPL-licensed toolkit.

A Go backend (`pkg/ui`, run by `kubehenge serve`) uses the Cluster API and Kubernetes client libraries directly. It serves a React and TypeScript frontend (`web/`) embedded in the same binary. The `kubehenge` command-line tool shares the backend's code.

Clusters created here carry the creating user in the annotation `kubehenge.dev/created-by`. Every change is logged with the user who made it.

### Optional entry inside Proxmox VE

Proxmox VE has no supported way to extend its own web interface. An opt-in installer (`appliance/pve-entry/`) adds a Kubernetes entry that leads to the appliance:

- **What it adds:** a button beside Create VM and Create CT, and an item under Datacenter. Both open the appliance in a new tab.
- **How:** one line added to the page template `pve-manager` ships loads a script of ExtJS overrides.
- **Surviving updates:** an apt hook puts the line back after a `pve-manager` update replaces the template, and logs it. `--remove` returns the node to its packaged state.
- **Failing safe:** if a release renames the two interface classes the script extends, or a step fails, Proxmox VE's interface renders as shipped. The browser's console gives the reason.
- **Scope:** it is installed per node, and `appliance/install.sh` offers it on the node it runs on. The appliance never depends on it.

## First run

One Terraform/OpenTofu apply, or the appliance, creates a small management cluster on Proxmox VE VMs with everything above installed.

The appliance (`appliance/install.sh`) checks everything it downloads against checksums it carries. It creates an unprivileged Debian container with a certificate from the Proxmox VE cluster's own CA. The interface runs as an unprivileged user on ports 443 and 8443 (the shared Headlamp).

Its first-run page runs the `management-cluster/` module with OpenTofu on the administrator's session, whose ticket and CSRF token reach OpenTofu through its environment only. The state is encrypted with a key derived from a passphrase nothing stores. Every later apply asks for it again, unless automatic upgrades of the management cluster are on ([Upgrades](#upgrades)).

The management cluster's admin kubeconfig and talosconfig stay inside that state. An administrator can open the state with the passphrase on the Administration page, and every download is logged.

## Signing in to clusters

Every cluster accepts client certificates and ServiceAccount tokens, as Kubernetes does. Its `oidc` variable adds OpenID Connect providers whose ID tokens its API server accepts, up to eight. They are written as Kubernetes' structured authentication configuration (one JWT authenticator per provider). Each entry has:

- the issuer URL
- the client ID, which a token's audience must name
- the claims for user name and groups
- prefixes, `oidc:` unless set, which may not start with `system:`
- the scopes sign-in tools ask for
- a CA, when a public one does not sign the provider's certificate

### Who sets the providers

- **The appliance-wide default.** Providers set on the Administration page, or as the module's `cluster_oidc` (the chart's `clusterOIDC`), are the variable's default.
  - Every new cluster trusts them unless it sets its own list, and the create wizard starts from them. A change applies to clusters created afterwards.
- **Who may choose.** Keeping or dropping the default's providers needs no extra privilege.
  - Adding any other provider needs `Sys.Modify`, as the admin kubeconfig does. That is on the workload pool for a new cluster, and on the cluster's own for a change.
- **Changing it** replaces the cluster's control-plane nodes.
- **Headlamp.** The shared Headlamp offers the first provider's sign-in beside the token prompt, as a public client with PKCE and no stored secret.
  - The provider must accept `https://<appliance>:8443/oidc-callback` as a redirect URL.
  - Only the browser that starts a sign-in may finish it. The gate keeps a hash of the OAuth state in a short-lived cookie.

## Upgrades

### Kubehenge itself

The appliance's Administration page shows the installed release and newer ones, and upgrades with a button. It can also upgrade once a day at a chosen hour, to new patch releases or to new minor releases of the same major version.

`install.sh --upgrade` does the same, with `kubehenge appliance health` of the new binary as the check. The CLI in the appliance (`kubehenge appliance`) is a client of the interface, signed in as a Proxmox VE user. Its upgrades and etcd backups therefore pass the interface's privilege checks and validation.

The management cluster moves to what the new release carries when an administrator applies it again from the same page. Every apply records the release that ran it. The page says when the management cluster runs what an older release applied, and which component versions applying this release changes.

#### The updater

The interface never replaces software itself. A separate root service (`kubehenge-update`) carries out the requests it drops into a spool. Of root's capabilities it keeps only `CAP_DAC_READ_SEARCH`, under a system-call filter. Its file system is read-only but for the software, its service units and its spool.

It accepts only a release named in an index signed with Kubehenge's Ed25519 release key, built into each release. The release must be newer and its archive match the signed checksum. The service swaps the files by rename and restarts the interface.

It keeps the replaced release until the new interface answers its health route. The interface must then run 20 seconds without a restart, or the old release goes back.

Runs of the management-cluster module, the updater and `install.sh --upgrade` share a lock on the appliance's state directory, so an upgrade and a run never overlap. A crash leaves no lock behind.

### The management cluster on its own

An apply needs the state passphrase and a Proxmox VE credential that can create users, roles, ACLs and VMs. The appliance keeps neither by default.

With automatic upgrades of the management cluster on, the appliance keeps the passphrase and an API token of its own Proxmox VE user. That user, `kubehenge-upgrader@pve`, holds the Administrator role on `/`.

Both are in a file only the interface's user and root can read. They are sealed with a key on a volume of the container that Proxmox VE's backups leave out, so no backup holds what opens them.

The interface then applies the saved settings in Kubehenge's upgrade hour, at most once a day. It does so once the appliance runs a newer release than the last apply did.

Switching it off, or removing the management cluster, deletes the file, then the user.

### Clusters

The `upgrades` variable of every ClusterClass asks Kubehenge's controller to upgrade Kubernetes automatically. `patch` keeps the minor version. `minor` also moves to the next one, one minor at a time and never past the distribution's stable release. That is:

- RKE2's and k3s's release channel
- for kubeadm, the newest version a template has been built for
- for Talos, Kubernetes' own newest release of each minor version

For Talos it never goes beyond what the cluster's Talos release runs, nor during a Talos upgrade.

An upgrade starts only inside the window (days of the week or of the month, a start time and length in a named time zone). It starts only while the cluster is available with no rollout running, and not beyond the Kubernetes versions an installed Rancher runs on. A rollout that has started finishes even if the window closes. The last automatic upgrade is recorded on the Cluster as `kubehenge.dev/auto-upgraded`.

#### Add-on changes

The upgrade window gates the controller's changes to a cluster's add-ons as well. Each HelmChartProxy records the release stamp (the controller's build and pins) and the settings stamp (the cluster's variables but `upgrades`) its spec was made for. A spec that differs from the stored one with a new release stamp and an unchanged settings stamp waits while the window is closed.

Any other change applies at once: a new proxy, a change of the cluster's variables, a spec the same release made. A Cluster without a window has none held. The Cluster's annotation `kubehenge.dev/addon-changes` and condition `AddonChangesPending` record what waits. The annotation `kubehenge.dev/apply-addon-changes` asks for it now.

#### Software already in the cluster

Before it first installs cert-manager, Kyverno or monitoring, the controller asks the workload cluster for the custom resource definitions of that software. It leaves the add-on out when one exists that its release does not own, and records the condition `AddonConflict`. `useExisting.certManager` leaves cert-manager's chart out, and the issuers and certificates are made in the existing install. The controller puts `namespaceLabels` on the namespaces of the proxies before it applies them. It creates a missing namespace with them.

## Networking

- **Node addresses** come from IP pools managed in the management cluster. No DHCP is needed on the Proxmox VE bridge.
- **The control plane** of each cluster gets a virtual IP that moves between control-plane nodes. It is held by ARP, or over BGP where the cluster's load balancer is in BGP mode.

### Load balancers

Services of type `LoadBalancer` get addresses from the cluster's `loadBalancer` range, if it has one, from one of two load balancers:

- **MetalLB**, in layer 2 mode without its BGP backend, or in BGP mode with frr-k8s, the backend its chart recommends. The FRR mode inside the speaker is deprecated and not used.
- **kube-vip** as a DaemonSet that answers only for Services, one lease per Service so that their addresses spread over the nodes. kube-vip's cloud provider hands them out.

Kubehenge's controller applies the MetalLB address pool, again whenever MetalLB returns. It applies an `L2Advertisement` in layer 2, or in BGP mode a `BGPAdvertisement` and a `BGPPeer` for each router. It deletes the objects of the other mode and peers no longer listed.

MetalLB's speakers answer from the control planes too, so a cluster without workers still has nodes to answer from.

#### Addresses refused

Kubehenge's admission webhook and BeforeClusterCreate hook refuse these:

- addresses outside the nodes' network in layer 2. In BGP mode they let routed addresses and API address through.
- an invalid AS number or router, a router that is also an announced address, kube-vip as the load balancer and Talos
- the gateway, the network's own or broadcast address, and the API and node addresses
- other clusters' addresses on the same bridge and VLAN

The controller frees a Service of an address outside the range, which kube-vip and MetalLB would otherwise keep. The load balancer gives the Service a new one.

#### k3s's servicelb

On k3s the class switches servicelb off, and the controller deletes the DaemonSets servicelb left. Every k3s node allows the two forwarding settings servicelb's pods need. Switching the load balancer off turns servicelb back on, and the control planes roll for it. Its pods then run on the old servers too.

#### BGP

Most routers refuse two sessions from one address. kube-vip announces the API address from the control planes (`vip_arp` off, BGP on), so MetalLB peers from workers only. The webhook refuses BGP with the load balancer on in a cluster without workers. It refuses BGP on Talos, whose virtual IP has no BGP option.

In BGP mode MetalLB's frr-k8s requests 45m CPU and 176 MiB, which the class's `loadBalancerBGP` figures count.

With `bgp.authenticated`, the controller writes kube-vip's manifest, password included, into the Secret `<cluster>-kube-vip`. The ClusterClasses write the file from it with `contentFrom.secret`, which all three bootstrap providers support. MetalLB's peers name `passwordSecret`, a Secret in `metallb-system` kept from the same credential.

### Pod networking

Each distribution's own CNI is the default: canal for RKE2, flannel for k3s and Talos. The CNI is chosen at creation and stays.

- RKE2 also ships Calico and Cilium and installs either itself.
- kubeadm has none of its own. A kubeadm cluster is created with Cilium or Calico, which Kubehenge's controller installs.
- So does a k3s or Talos cluster given Cilium or Calico, whose flannel is then left out. k3s's network policy controller goes too.
- With k3s's own flannel, kube-proxy runs in nftables mode, in a table of its own.
- On Talos, Cilium runs without `SYS_MODULE` and leaves cgroups to Talos. Calico installs no FlexVolume driver.
- Kubehenge's Cilium chains the portmap plugin, k3s's own on k3s, so host ports work without Cilium in place of kube-proxy.
- Calico comes as the Tigera operator's chart. Its Installation comes through the ClusterResourceSet once the operator has made the CRDs.

### Pod and service ranges

Pod and service ranges must stay clear of the nodes' network and of each other.

- The controller's mutating webhook sets them on every new cluster that names none, from the interface, kubectl or GitOps alike.
- It takes the ranges asked for. Otherwise it takes the distribution's own if clear of the nodes' network, else the first clear one of a few private ranges.
- A cluster that runs Cilium, which may join a Cluster Mesh later, also keeps its pods clear of every other cluster's pods and services.
- Its validating webhook refuses ranges that are no IPv4 networks or overlap the nodes' network or each other.
- Every class honours the Cluster's ranges, kubeadm's pod subnet among them, which the Tigera operator needs to install Calico. The k3s provider reads its own server configuration only, so the k3s class copies them there.

### The Gateway API

The Gateway API is in every cluster, with no switch, and costs no pod.

Kubehenge's controller carries one release of upstream's standard-channel install file, built into its binary and checked against its SHA-256 at build time. It applies its CRDs once the control plane is up. On kubeadm and Talos it applies the admission policy beside them (`safe-upgrades.gateway.networking.k8s.io`).

RKE2 and k3s ship the CRDs in their Traefik CRD charts in some releases. Later releases ship the CRDs and policy in a chart of their own (`rke2-gateway-api-crd`, `gateway-api-crd`), which adopts what it finds.

- There the controller first waits until the Traefik CRD chart, and the Gateway API chart where the release has one, are installed.
- The distributions' Gateway API charts install only the standard channel, as Kubehenge does. Neither Traefik's Gateway provider nor Istio's waypoints need the experimental channel.

#### Whose objects

Where the distribution has its own Gateway API chart, the Gateway API is the distribution's, and the controller applies none of it. Otherwise it looks at each object of the bundle in the cluster:

- One missing gets Kubehenge's.
- One of Kubehenge's release or newer stays.
- An older one stays while the latest revision of the Helm release that owns it still holds it. Otherwise it is taken over and brought to Kubehenge's release.

#### The Gateway API's admission policy

The admission policy, which refuses CRDs older than its own release, goes with the CRDs.

- The controller keeps its copy on kubeadm and Talos while no Helm release ships a CRD of the bundle. It refuses older CRDs there, and experimental ones over standard ones.
- On RKE2 and k3s, and wherever a Helm release ships a CRD, the controller deletes its copy. A later Gateway API chart can then install its CRDs, older than Kubehenge's or not.
- A policy that a Helm release ships, or that someone else applied, stays.

The controller logs each object it applies or deletes, and which Helm releases hold the cluster's objects whenever that changes.

#### Traefik's Gateway provider

Traefik's Gateway provider is on in every distribution. It is configured in the chart values on kubeadm and Talos. On RKE2 and k3s it is in the `HelmChartConfig` the classes write onto the control planes before the distribution starts. The certificates add-on's default certificate and HTTPS listener reach it as the [certificates add-on](#certificates-add-on) describes.

Traefik's chart creates a Gateway, which an API server without the CRDs refuses.

- On kubeadm and Talos the controller therefore holds Traefik's chart back until the Cluster carries the annotation `kubehenge.dev/gateway-api`. The annotation records the oldest release among the CRDs once they are all there, or once the distribution's Gateway API chart is installed.
- RKE2's and k3s's helm controller retries until the CRDs are there.
- The controller looks at every cluster again each minute, so a distribution upgrade that drops the CRDs is followed within a minute.
