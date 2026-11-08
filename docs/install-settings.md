# Setup settings in depth

What each opt-in setting of the management cluster grants and risks, and the fields of a backup target. Every setting has a matching module variable ([install.md](install.md#variables)).

## Proxmox VE HA

Proxmox VE lets only `Sys.Console` on `/` create HA resources and rules. So this setting creates a user of its own, `kubehenge-ha@pve`, with an API token holding `Sys.Audit` and `Sys.Console` on `/`. With the setting on, every node that runs an HA resource arms Proxmox VE's watchdog and restarts itself when it loses quorum.

That privilege amounts to a root shell on every node. Whoever reads its Secret in the management cluster (`kubehenge-proxmox-ha`), or holds the admin kubeconfig, can take over the Proxmox VE cluster. Proxmox VE offers nothing narrower, so the controller never holds the token. A short-lived Job made for each write holds it and writes only HA resources and rules that carry Kubehenge's comment ([architecture.md](architecture.md#tokens-kept-out-of-the-controller)).

Switching the setting on or off needs `Sys.Console` on `/` in your own account too. Switching it off removes the token, then every HA resource and rule Kubehenge made, with your session.

## HA rule placements

Reading Proxmox VE's HA rules takes `Sys.Audit` on `/`, which reaches the whole cluster's configuration. Most people who create clusters lack it and cannot place a cluster by a rule's nodes. With this setting, Kubehenge reads the rules for them.

### What the rules setting grants

Every 30 seconds the controller reads the enabled node-affinity rules and the nodes with its own API token. That token already holds `Sys.Audit` on `/` without propagation, so Proxmox VE grants nothing new.

- It keeps each rule's name and allowed nodes in the ConfigMap `kubehenge-ha-rules`.
- It never keeps a rule's comment, resources or strictness, nor the rules of Kubehenge's own clusters.
- The interface's account may read that ConfigMap only while the setting is on. It hands the rules to those with `VM.Allocate` on the workload pool or on a cluster's pool.
- Those who hold `Sys.Audit` on `/` read the rules from Proxmox VE as before.

### What the rules setting risks

Everyone who may create or change a cluster learns the rules' names and which nodes they cover. That might show how the nodes are grouped (by rack, hardware or tenant), so name rules with that in mind.

Switching the setting off removes the ConfigMap and the interface's access to it.

## CSI plugin

The CSI plugin's newer releases need `Datastore.Allocate` on a cluster's storage, which covers every volume on it and its definition. Without this setting, clusters run the newest release that works without it. With it, a cluster may choose the newest release on a storage of its own ([clusters.md](clusters.md#persistent-volumes)). Its token then holds `Datastore.Allocate` there.

The setting creates a role, `<name>-cluster-datastore`, holding `Datastore.Allocate`, `Datastore.AllocateSpace` and `Datastore.Audit`. It also gives Kubehenge's controller `Datastore.Allocate` on each storage for clusters, so that it can grant the role there. Storages that hold the management cluster's disks, its Talos image, the cloud-init images or the template builds' downloads and uploads are left out.

Switching it off removes the role, and with it every cluster's grant. Those clusters go back to the earlier release.

## etcd restores

Restoring a cluster's etcd from a snapshot ([clusters.md](clusters.md#restore-etcd)) stops the control plane on every control-plane VM and restores on one. That takes commands run as root on the VMs. With this setting the controller gets a user and API token of its own. The token holds `VM.Audit` and `VM.GuestAgent.Unrestricted` on the workload pool. The controller runs each step through the QEMU guest agent.

### The restore token

That privilege amounts to a root shell in every cluster's VMs. Whoever reads its Secret (`kubehenge-proxmox-restore`) or holds the admin kubeconfig can read every Secret of every cluster and change any workload. It reaches no Proxmox VE node and no VM outside the workload pool.

The controller never holds it. A short-lived Job holds it for each step that someone with `Sys.Modify` on the cluster's pool asked for ([architecture.md](architecture.md#tokens-kept-out-of-the-controller)). Switching the setting on needs `VM.GuestAgent.Unrestricted` on `/` in your own account too. With it off, you run the same steps by hand from the list the interface shows.

## Backup targets

Each target is an S3 store and a bucket in it. Any S3-compatible store serves, such as AWS S3, MinIO, Ceph's RADOS Gateway, SeaweedFS and Garage. So does the S3 store a Proxmox Backup Server 4.2 or later keeps a datastore on. Then the clusters' backups and the Proxmox VE backups share one store.

The store must take server-side encryption with keys the client sends (SSE-C). Every cluster encrypts what it writes with it.

| Field | What it is |
| --- | --- |
| Name | How clusters name the target: lower-case letters, digits and dashes. |
| Endpoint | The store's `https://` URL. Empty for AWS S3 itself. |
| Bucket | The bucket, which has to exist. |
| Region | The bucket's region. `us-east-1` when empty. |
| Prefix | A folder in the bucket. Each cluster writes under `<prefix>/<namespace>.<cluster>/velero`, and its etcd snapshots under `<prefix>/<namespace>.<cluster>/etcd`. The management cluster's go under `<prefix>/management/etcd`. |
| CA certificate | PEM, when the system's CAs do not cover the store's certificate. |
| Send checksums | On by default. Off for a store that refuses the checksums the AWS SDK sends with every upload. |

### Keys

Velero signs in to a target with an S3 access key of the cluster's own, limited by the store to the cluster's folder. Whoever creates or looks after a cluster with `Sys.Modify` on its pool stores it on the cluster ([clusters.md](clusters.md#change-settings)).

The shared backup key, shown below the targets once the management cluster runs, holds one key. Kubehenge uses it for the etcd snapshots of every cluster without a key of its own. It reaches every cluster's folder and stays in the management cluster, never in a cluster.

## Management cluster etcd backups

With the management cluster's etcd backups on ([install.md](install.md#back-up-the-management-clusters-etcd)), the appliance keeps these, readable only by root in the container:

- the target's S3 access key, which needs to write, list and delete under `<prefix>/management`.
- a talosconfig with the `os:etcd:backup` role, valid for a year and made again on every apply. It takes etcd snapshots and cannot read a node's configuration, logs or files, or reboot it.

### Snapshot key

A snapshot holds every Secret of the management cluster, so the appliance encrypts it with [age](https://age-encryption.org) before uploading, as the clusters' own snapshots are.

The key that opens the snapshots is kept in the target as `<prefix>/management/identity.age`, encrypted with the state passphrase. It is never stored on the appliance.

An appliance set up later on the same target with another state passphrase cannot open the key and is refused. Deleting `<prefix>/management` in the bucket starts afresh. The snapshots taken so far then stay unreadable.

### Restoring a snapshot

A restore needs the state passphrase. It uses the admin talosconfig from the state, so it reaches every node even with Kubernetes down. It needs the control-plane VMs running and creates no VMs. **Recreate…** makes them first ([troubleshooting](troubleshooting.md#the-management-clusters-vms-are-lost)).

### Export for a new appliance

The state, the settings and the target's access key live only in the appliance. **Export…** under **Administration, Backups**, or `kubehenge appliance export`, saves them in one file. The file is encrypted with an export passphrase you set, with [age](https://age-encryption.org). It holds the admin talosconfig, OpenTofu's state (still encrypted with the state passphrase), the applied settings and the target with its access key.

### Restore from an export

After the appliance is lost:

1. Install a new appliance on the same Proxmox VE cluster.
2. In its setup choose **Restore from an export**, or run `kubehenge appliance import <file>`. Both ask for the export passphrase.
3. Restore a snapshot under **Administration, Backups**. It asks for the state passphrase.
4. Apply the settings once, which signs the interface in to the management cluster.

An import is refused where a management cluster is already set up. A wrong passphrase and a changed file are refused before anything is written.

After a snapshot succeeds, the appliance warns when no export was taken since the target, its access key or the snapshots' key last changed. Keep the file and both passphrases apart from the appliance.

## What the template builds may do

The weekly build runs in the management cluster with a user and API token of its own, `kubehenge-template-builder@pve`, whose token lives in the Secret `kubehenge-template-builder` there. Switching the refresh off removes the user, its token and the CronJob. The templates stay. The user holds:

| On | Privileges | For |
| --- | --- | --- |
| `/pool/kubehenge-templates`, propagated | `Pool.Audit`, `VM.Allocate`, `VM.Audit`, `VM.Config.CDROM`, `VM.Config.CPU`, `VM.Config.Disk`, `VM.Config.HWType`, `VM.Config.Memory`, `VM.Config.Network`, `VM.Config.Options`, `VM.GuestAgent.Unrestricted`, `VM.PowerMgmt` | creating a template's VM, preparing it through the guest agent, turning it into a template, and deleting builds nothing needs. Without `Pool.Audit`, Proxmox VE's resource list hides the pool's VMs |
| the storages of the downloaded images and the cloud-init ISO (Talos image and Cloud-init images in the setup, unless the module names others) | `Datastore.Allocate`, `Datastore.AllocateTemplate`, `Datastore.Audit` | downloading the node OS's cloud image and uploading the ISO, and deleting both afterwards |
| the template disks' storage, also when it is one of those | `Datastore.AllocateSpace`, `Datastore.Audit` | the templates' disks. `Datastore.Allocate` does not include `Datastore.AllocateSpace` |
| `/nodes/<node>` the builds run on | `Sys.AccessNetwork`, `Sys.Audit` | Proxmox VE's download of the image |
| `/sdn/zones/localnetwork/<bridge>`, or `/sdn/zones/<zone>/<vnet>` for a VNet | `PVESDNUser` | attaching the VM that prepares an image |
| `/`, and `/nodes` propagated | `Sys.Audit` | reading the cluster's resources and the nodes' storages |

### Remaining gaps

The token can touch no VM outside the template pool, no cluster's VM and none of the management cluster's, and sees no users. Two gaps remain:

- **Proxmox VE deletes a file on a storage only for `Datastore.Allocate`**, which reaches every file and volume on that storage.
  - By default the builds use the storages of the Talos image and the cloud-init images, often `local`. So the token could delete other ISOs, images or backups kept there.
  - Give the builds storages of their own that hold nothing else (`template_refresh.import_datastore` and `iso_datastore`).
- **Kubehenge's controller can read every Secret in the management cluster**, which it needs for the clusters' credentials. Whoever takes over the controller also holds this token, as they already hold Cluster API's.

## Management network MTU

Set `mtu` (1280 to 65520) where the network carries other frames than 1500 bytes, such as 1450 on a VXLAN underlay or 9000 with jumbo frames. It goes on the management VMs' Proxmox VE interface and on the node's own link. Empty keeps the bridge's, and the setup refuses a value above it.

## Management cluster VM IDs

The management cluster's VMs get random VM IDs, because Proxmox VE offers the same lowest free ID to everything created at the same moment. The setup draws them from the `next-id` range under Datacenter, Options, so a band you reserved for Kubehenge holds them too.

- **With `next-id` set,** the IDs come from the upper half of the range, clear of the lowest free ID Proxmox VE hands out.
  - A range whose upper half holds under ten IDs per management VM is used whole.
  - A missing bound is Proxmox VE's own, 100 or 1000000 (exclusive).
- **Without it,** or without `Sys.Audit` on `/` to read it, the IDs come from 10000 to 99998.
- **Too few free IDs** in the range stop the setup. Widen `next-id`.
- **A running management cluster** keeps its VMs' IDs when the range changes. The module's `vm_id_range` sets the range, `start` included and `end` excluded.

## Where the template builds run

Three settings of `template_refresh` place the VM that builds a node image:

- **`node`** is the Proxmox VE node it runs on. Empty takes the first management node.
- **`bridge`** is its bridge, a VNet written `zone/vnet`. Empty takes the first cluster bridge.
- **`vlan`** is its VLAN tag. 0 or empty sends it untagged.

The setup checks that the node is online and carries the template disks' storage and the bridge.

## HA group labels

The cloud controller manager labels each node with the Proxmox VE HA groups, or node-affinity HA rules, its Proxmox VE node is in. Proxmox VE lists them only with `Sys.Audit` on `/`. On by default, the setting gives each cluster the labels unless it switches them off ([clusters.md](clusters.md#machines)). Its token then holds `Sys.Audit` on `/`.

### Privileges of the labels

The token can read the Proxmox VE cluster's own configuration: firewall rules, backup jobs, HA groups, rules and resources, and the corosync configuration. It changes nothing. It reads nothing of the guests, storages or nodes beyond its own pool and the nodes.

The setting creates a group, `<name>-clusters-audit`, with `Sys.Audit` on `/` without propagation. The controller gets `User.Modify` on that group, to add and remove the users of clusters with the labels. It gets nothing on `/` itself.

### Turning the labels off

Unticking **HA group labels** in the setup, or `cluster_sys_audit = false`, removes the group and with it every cluster's `Sys.Audit` on `/`. New clusters go without the labels. The cloud controller manager logs a permission error for each node every few minutes ([troubleshooting.md](troubleshooting.md)).

## Nodes' boot logs

A node that never joins its cluster says why in its boot log ([troubleshooting.md](troubleshooting.md#a-nodes-boot-log)). Talos nodes' logs come through their Talos API. Every kubeadm, RKE2 and k3s node keeps the end of its boot's journals in a file beside cloud-init's output. The controller reads those files through the VM's QEMU guest agent. The setting is on by default.

### Privilege and reach

- **What it grants:** the controller's token holds `VM.GuestAgent.FileRead` on the workload pool, propagated.
  - With it, the token may read files in every cluster VM through the guest agent. It runs no commands and writes nothing.
- **What it reaches:** only the files the node's guest agent may open.
  - Kubehenge's node templates allow the agent to read cloud-init's output and the node's copy of its journals (`/var/log/kubehenge`, root's alone).
  - Their SELinux policy keeps it from the rest of `/var/log` and from the node's keys and kubeconfigs.
  - In a VM cloned from a template of your own, it reaches whatever that VM's agent allows.
- **Switching it off,** by unticking **Nodes' boot logs** in the setup or setting `node_boot_logs = false`, takes the privilege away. The interface and `kubectl` then offer Talos nodes' boot logs alone.

## kubectl changes

The interface records what people do through it. A change made with kubectl or through the management cluster's API is not recorded. With **kubectl changes** on, the management cluster's API server logs each such change, and Activity lists it with its user. The setting is off by default.

- **What it grants:** the interface may list the pods in `kube-system` and read their logs, to read the API server's.
- **Switching it on or off,** by the setting or `api_audit_log`, restarts the management cluster's API servers one at a time. Changes logged before stay on the Activity page.

### What the API server logs

It logs the events the workload clusters' audit policy logs. They go to the API server's output instead of Talos's file on the nodes.

- `kubectl logs -n kube-system -l k8s-app=kube-apiserver` reads it.
- The kubelet keeps the last 50 MiB of it per API server, where Talos's file keeps up to 1.1 GB for 30 days. Ship the pod logs elsewhere to keep more.
- Activity lists each create, change and deletion for 90 days, with its user, address and object, never its content.
- A GitOps tool's or script's service account is listed too. The controllers Kubehenge installs are not.

## Headlamp sign-in

Without it, everyone signs in to each cluster in the shared Headlamp with a token or an identity provider of their own. With it, those who hold `Sys.Modify` on a cluster's pool open it signed in as cluster-admin. They may download its admin kubeconfig anyway. Nobody else gets more than before. It needs no Proxmox VE privilege beyond the setup's own.

### What sign-in grants

The controller keeps two ServiceAccounts in the `kube-system` of every cluster in the interface's namespaces.

- `kubehenge-headlamp` is bound to `cluster-admin`.
- `kubehenge-headlamp-issuer` may make `kubehenge-headlamp`'s tokens and do nothing else.
- The issuer's token lasts an hour and is renewed every 20 minutes. It sits in the Secret `<cluster>-headlamp-issuer` beside the Cluster, as the Health page's token does.
- When such a user first opens a cluster, the interface makes a `kubehenge-headlamp` token for their session. It lasts an hour at most and no longer than the session (ten minutes at the least, the shortest Kubernetes makes).
- The interface keeps the token in memory and puts it on the requests it passes to Headlamp for that cluster. It never reaches the browser.
- A token of the user's own, or an identity-provider sign-in, replaces it. Logging out of the cluster in Headlamp drops the session's token until the user opens the cluster from the interface again.

### What sign-in risks

- Whoever reads Secrets in the interface's namespaces can act as cluster-admin in those clusters while the issuer's token lasts. That is the interface and the management cluster's administrators, who can read the admin kubeconfigs there already.
- Headlamp acts as `kubehenge-headlamp`, so the cluster's audit log names that account rather than the Proxmox VE user.
- The interface logs each token it makes with its user and the token's ID. The audit log records the ID with every request (`authentication.kubernetes.io/credential-id`).
- Switching it off removes both accounts from every cluster, `kubehenge-headlamp` first, which ends every token made for it at once, and then the issuer's Secret.

## Headlamp actions

Without them, the shared Headlamp shows and changes a cluster's objects and drains its nodes with each user's own credentials. With them, it also offers two plugins it ships with. The app catalog lists, installs, upgrades, rolls back and uninstalls Helm releases. The Prometheus plugin charts a cluster's metrics. Neither needs a Proxmox VE privilege beyond the setup's own.

### What the plugins grant and risk

- **What they grant:** everyone who sees a cluster uses both plugins on it.
  - Headlamp acts in the cluster with the user's own credentials, or the [sign-in](#headlamp-sign-in)'s. So the cluster's own rights decide what they may install or remove.
  - Charts come only from the Helm repositories set for the appliance, named `repository/chart`.
  - A chart named by a path on the appliance, an address or an OCI reference is refused. So are dependencies fetched from where a chart says.
- **What they risk:** Headlamp keeps one list of Helm repositories for every user and cluster. The appliance fetches their charts.
  - Only those who hold the setup's privileges on `/` add, update or remove a repository.
  - A repository is reached over HTTPS with its certificate checked. Any credentials go to its own host alone.

### Not offered either way

- Port forwards, which Headlamp offers in its desktop app alone. A forward listens on the machine Headlamp runs on, here the appliance, for everyone signed in to the cluster.
- The external proxy, which would fetch any address from the appliance. The app catalog's search of Artifact Hub needs it, which is why that goes too.
- Headlamp's own metrics, which count every user's requests to every cluster.
