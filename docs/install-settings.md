# Setup settings in depth

What the management cluster's opt-in settings grant and risk, and the backup targets' fields. Each setting works the same as the module's variable of the same purpose ([install.md](install.md#variables)).

## Proxmox VE HA

Proxmox VE lets only `Sys.Console` on `/` create HA resources and rules. So this setting creates a user of its own, `kubehenge-ha@pve`, with an API token holding `Sys.Audit` and `Sys.Console` on `/`. With the setting on, every node that runs an HA resource arms Proxmox VE's watchdog and restarts itself when it loses quorum.

That privilege amounts to a root shell on every node. Whoever reads its Secret in the management cluster (`kubehenge-proxmox-ha`), or holds the admin kubeconfig, can take over the Proxmox VE cluster. Proxmox VE offers nothing narrower, so the controller never holds the token. A short-lived Job made for each write holds it and writes only HA resources and rules that carry Kubehenge's comment ([architecture.md](architecture.md#tokens-kept-out-of-the-controller)).

Switching the setting on or off needs `Sys.Console` on `/` in your own account too. Switching it off removes the token, then every HA resource and rule Kubehenge made, with your session.

## HA rule placements

Reading Proxmox VE's HA rules takes `Sys.Audit` on `/`, which reaches the whole cluster's configuration. Most people who create clusters lack it and cannot place a cluster by a rule's nodes. With this setting, Kubehenge reads the rules for them.

- **What it grants:** every 30 seconds Kubehenge's controller reads the enabled node-affinity rules and the nodes with its own API token.
  - That token already holds `Sys.Audit` on `/`, without propagation. Proxmox VE grants nothing new, and the HA token is not used.
  - It keeps each rule's name and the nodes it allows in the ConfigMap `kubehenge-ha-rules` beside the ClusterClasses. It never keeps a rule's comment, resources or strictness, nor the rules Kubehenge keeps for its own clusters.
  - The interface's account may read that ConfigMap only while the setting is on. It hands its rules to those with `VM.Allocate` on the workload pool or on a cluster's pool.
  - Those who hold `Sys.Audit` on `/` read the rules from Proxmox VE as before.
- **What it risks:** everyone who may create or change a cluster learns the rules' names and which nodes they cover. That might tell them how the nodes are grouped (by rack, hardware or tenant).
  - Name rules with that in mind. Nothing else of Proxmox VE's configuration reaches them.
- **Switching it off** has the controller remove the ConfigMap and takes the interface's access to it away.

## CSI plugin

The CSI plugin's newer releases need `Datastore.Allocate` on a cluster's storage, which covers every volume on it and its definition. Without this setting, clusters run the newest release that works without it. With it, a cluster may choose the newest release on a storage of its own ([clusters.md](clusters.md#persistent-volumes)). Its token then holds `Datastore.Allocate` there.

The setting creates a role, `<name>-cluster-datastore`, holding `Datastore.Allocate`, `Datastore.AllocateSpace` and `Datastore.Audit`. It also gives Kubehenge's controller `Datastore.Allocate` on each storage for clusters, so that it can grant the role there. Storages that hold the management cluster's disks, its Talos image, the cloud-init images or the template builds' downloads and uploads are left out.

Switching it off removes the role, and with it every cluster's grant. Those clusters go back to the earlier release.

## etcd restores

Restoring a cluster's etcd from a snapshot ([clusters.md](clusters.md#restore-etcd)) stops the control plane on every control-plane VM and restores on one of them. That takes commands run as root on the VMs. With this setting the controller gets a user and API token of its own holding `VM.Audit` and `VM.GuestAgent.Unrestricted` on the workload pool. It runs each step through the QEMU guest agent.

That privilege amounts to a root shell in every cluster's VMs. Whoever reads its Secret in the management cluster (`kubehenge-proxmox-restore`), or holds the admin kubeconfig, can read every Secret of every cluster and change any workload. It reaches no Proxmox VE node and no VM outside the workload pool.

The controller never holds it. A short-lived Job holds it for each step of a restore that someone with `Sys.Modify` on the cluster's pool asked for ([architecture.md](architecture.md#tokens-kept-out-of-the-controller)). Switching the setting on needs `VM.GuestAgent.Unrestricted` on `/` in your own account too. With it off, a restore takes the same steps, run by hand from the list the interface shows.

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

**Shared backup key**, shown below the targets once the management cluster runs, stores one key. Kubehenge uses it for its own etcd snapshots of every cluster without a key of its own. It reaches every cluster's folder and stays in the management cluster, never in a cluster.

## Management cluster etcd backups

With the management cluster's etcd backups on ([install.md](install.md#back-up-the-management-clusters-etcd)), the appliance keeps these, readable only by root in the container:

- the target's S3 access key, which needs to write, list and delete under `<prefix>/management`.
- a talosconfig with the `os:etcd:backup` role, valid for a year and made again on every apply. It takes etcd snapshots and cannot read a node's configuration, logs or files, or reboot it.

A snapshot holds every Secret of the management cluster. So the appliance encrypts it with [age](https://age-encryption.org) before uploading, as the clusters' own snapshots are.

The key that opens the snapshots is kept in the target as `<prefix>/management/identity.age`, encrypted with the state passphrase. It is never stored on the appliance.

An appliance set up later on the same target, whose state has another passphrase, cannot open the key and is refused. Deleting `<prefix>/management` in the bucket starts afresh. The snapshots taken so far then stay unreadable.

A restore needs the passphrase too. It uses the admin talosconfig from the state, so it reaches every node even with Kubernetes down. It needs the control-plane VMs running and creates no VMs.

## What the template builds may do

The weekly build runs in the management cluster with a user and API token of its own, `kubehenge-template-builder@pve`, whose token lives in the Secret `kubehenge-template-builder` there. It holds:

| On | Privileges | For |
| --- | --- | --- |
| `/pool/kubehenge-templates`, propagated | `Pool.Audit`, `VM.Allocate`, `VM.Audit`, `VM.Config.CDROM`, `VM.Config.CPU`, `VM.Config.Disk`, `VM.Config.HWType`, `VM.Config.Memory`, `VM.Config.Network`, `VM.Config.Options`, `VM.GuestAgent.Unrestricted`, `VM.PowerMgmt` | creating a template's VM, preparing it through the guest agent, turning it into a template, and deleting builds nothing needs. Without `Pool.Audit`, Proxmox VE's resource list hides the pool's VMs |
| the storages of the downloaded images and the cloud-init ISO (Talos image and Cloud-init images in the setup, unless the module names others) | `Datastore.Allocate`, `Datastore.AllocateTemplate`, `Datastore.Audit` | downloading the node OS's cloud image and uploading the ISO, and deleting both afterwards |
| the template disks' storage, also when it is one of those | `Datastore.AllocateSpace`, `Datastore.Audit` | the templates' disks. `Datastore.Allocate` does not include `Datastore.AllocateSpace` |
| `/nodes/<node>` the builds run on | `Sys.AccessNetwork`, `Sys.Audit` | Proxmox VE's download of the image |
| `/sdn/zones/localnetwork/<bridge>` | `PVESDNUser` | attaching the VM that prepares an image |
| `/`, and `/nodes` propagated | `Sys.Audit` | reading the cluster's resources and the nodes' storages |

It can touch no VM outside the template pool, no cluster's VM and none of the management cluster's, and sees no users. Two gaps remain:

- **Proxmox VE deletes a file on a storage only for `Datastore.Allocate`**, which reaches every file and volume on that storage.
  - By default the builds use the storages of the Talos image and the cloud-init images, often `local`. So the token could delete other ISOs, images or backups kept there.
  - Give the builds storages of their own that hold nothing else (`template_refresh.import_datastore` and `iso_datastore`).
- **Kubehenge's controller can read every Secret in the management cluster**, which it needs for the clusters' credentials. So whoever takes over the controller holds this token too, as it holds Cluster API's.

Switching the refresh off removes the user, its token and the CronJob. The templates stay.

## HA group labels

The cloud controller manager labels each node with the Proxmox VE HA groups, or node-affinity HA rules, its Proxmox VE node is in. Proxmox VE lists them only with `Sys.Audit` on `/`. On by default, the setting gives each cluster the labels unless it switches them off ([clusters.md](clusters.md#machines)). Its token then holds `Sys.Audit` on `/`.

That lets the token read the Proxmox VE cluster's own configuration. Examples are its firewall rules, backup jobs, HA groups, rules and resources and the corosync configuration. It changes nothing. It reads nothing of the guests, storages or nodes beyond its own pool and the nodes.

The setting creates a group, `<name>-clusters-audit`, with `Sys.Audit` on `/` without propagation. Kubehenge's controller gets `User.Modify` on that group, to add and remove the users of clusters with the labels. It gets nothing on `/` itself.

Unticking **HA group labels** in the setup, or `cluster_sys_audit = false`, removes the group and with it every cluster's `Sys.Audit` on `/`. New clusters go without the labels. The cloud controller manager logs a permission error for each node every few minutes ([troubleshooting.md](troubleshooting.md)).

## Nodes' boot logs

A node that never joins its cluster says why in its boot log ([troubleshooting.md](troubleshooting.md#a-nodes-boot-log)). Talos nodes' logs are read through their Talos API. Every kubeadm, RKE2 and k3s node keeps the end of its boot's journals in a file of its own, beside cloud-init's output. The controller reads both through the VM's QEMU guest agent. The setting is on by default.

- **What it grants:** Kubehenge's controller's token holds `VM.GuestAgent.FileRead` on the workload pool, propagated.
  - With it, the token may read files in every cluster VM through the guest agent. It runs no commands and writes nothing.
- **What it reaches:** only the files the node's guest agent may open.
  - Kubehenge's node templates allow the agent to read cloud-init's output and the node's copy of its journals (`/var/log/kubehenge`, root's alone).
  - Their SELinux policy keeps it from the rest of `/var/log` and from the node's keys and kubeconfigs.
  - In a VM cloned from a template of your own, it reaches whatever that VM's agent allows.
- **Switching it off,** by unticking **Nodes' boot logs** in the setup or setting `node_boot_logs = false`, takes the privilege away. The interface and `kubectl` then offer Talos nodes' boot logs alone.

## kubectl changes

The interface records what people do through it. A change made with kubectl or through the management cluster's API passes it by. With **kubectl changes** on, the management cluster's API server logs each such change, and Activity lists it with its user. The setting is off by default.

- **What the API server logs:** each create, change and deletion a person makes, with its user, address and object, never its content.
  - Reads are left out, and so are changes by controllers, nodes and the interface itself.
  - The log goes to the API server's output. Talos's audit log file on the nodes stops.
- **What it grants:** the interface may list the pods in `kube-system` and read their logs, to read the API server's.
- **Switching it on or off,** by the setting or `api_audit_log`, restarts the management cluster's API servers one at a time. Changes logged before stay on the Activity page.

## Headlamp sign-in

Without it, everyone signs in to each cluster in the shared Headlamp with a token or an identity provider of their own. With it, those who hold `Sys.Modify` on a cluster's pool open it signed in as cluster-admin. They may download its admin kubeconfig anyway. Nobody else gets more than before. It needs no Proxmox VE privilege beyond the setup's own.

- **What it grants:**
  - Kubehenge's controller keeps two ServiceAccounts in the `kube-system` of every cluster in the interface's namespaces.
    - `kubehenge-headlamp` is bound to `cluster-admin`.
    - `kubehenge-headlamp-issuer` may make `kubehenge-headlamp`'s tokens and do nothing else.
  - The issuer's token lasts an hour and is renewed every 20 minutes. It sits in the Secret `<cluster>-headlamp-issuer` beside the Cluster, as the Health page's token does.
  - When such a user first opens a cluster, the interface makes a token of `kubehenge-headlamp` for their session. It lasts an hour at most and no longer than the session (ten minutes at the least, the shortest Kubernetes makes).
  - The interface keeps the token in memory and puts it on the requests it passes to Headlamp for that cluster. It never reaches the browser.
  - A token of the user's own, or a sign-in through the cluster's identity provider, takes its place while Headlamp keeps it. Logging out of the cluster in Headlamp drops the session's token until the user opens the cluster from the interface again.
- **What it risks:**
  - Whoever reads Secrets in the interface's namespaces can act as cluster-admin in those clusters while the issuer's token lasts. That is the interface and the management cluster's administrators, who can read the admin kubeconfigs there already.
  - Headlamp acts as `kubehenge-headlamp`, so the cluster's audit log names that account rather than the Proxmox VE user.
  - The interface logs each token it makes with its user and the token's ID. The audit log records the ID with every request (`authentication.kubernetes.io/credential-id`).
- **Switching it off** removes both accounts from every cluster, `kubehenge-headlamp` first, which ends every token made for it at once, and then the issuer's Secret.

## Headlamp actions

Without them, the shared Headlamp shows and changes a cluster's objects and drains its nodes, with each user's own credentials there. With them, it also offers two plugins Headlamp ships with. The app catalog lists, installs, upgrades, rolls back and uninstalls Helm releases. The Prometheus plugin charts a cluster's metrics. They need no Proxmox VE privilege beyond the setup's own.

- **What they grant:** everyone who sees a cluster uses both plugins on it.
  - Headlamp acts in the cluster with the user's own credentials, or the [sign-in](#headlamp-sign-in)'s. So the cluster's own rights decide what they may install or remove.
  - Charts come only from the Helm repositories set for the appliance, named `repository/chart`.
  - A chart named by a path on the appliance, an address or an OCI reference is refused. So are dependencies fetched from where a chart says.
- **What they risk:** Headlamp keeps one list of Helm repositories for every user and cluster. The appliance fetches their charts.
  - Only those who hold the setup's privileges on `/` add, update or remove a repository.
  - A repository is reached over HTTPS with its certificate checked. Any credentials go to its own host alone.
- **Not offered either way:**
  - Port forwards, which Headlamp offers in its desktop app alone. A forward listens on the machine Headlamp runs on, here the appliance, for everyone signed in to the cluster.
  - The external proxy, which would fetch any address from the appliance. The app catalog's search of Artifact Hub needs it, so that goes too.
  - Headlamp's own metrics, which count every user's requests to every cluster.
