# Setup settings in depth

What the management cluster's opt-in settings grant and risk, and the fields of the backup targets. The setup's table in [install.md](install.md#the-settings) says what each setting does; the module's variables of the same purpose ([install.md](install.md#variables)) work the same way.

## Proxmox VE HA

Proxmox VE lets only `Sys.Console` on `/` create HA resources and rules, so with this setting the controller gets a second user and API token holding `Sys.Audit` and `Sys.Console` on `/`. That privilege amounts to a root shell on every node: whoever reads the controller's Secret in the management cluster, or holds its admin kubeconfig, can take over the Proxmox VE cluster. The token is used for HA resources and rules alone, and the controller touches only those it made, but the privilege cannot be narrowed further. Switching the setting on or off takes `Sys.Console` on `/` of you as well; switching it off takes the controller's token away, then removes every HA resource and rule Kubehenge made, with your session. Every node that runs an HA resource arms Proxmox VE's watchdog and restarts itself when it loses quorum.

## HA rules for cluster creators

Reading Proxmox VE's HA rules takes `Sys.Audit` on `/`, which reaches the whole cluster's configuration, so most people who create clusters lack it and cannot place a cluster by a rule's nodes. With this setting, Kubehenge reads the rules for them.

- **What it grants:** every 30 seconds Kubehenge's controller reads the enabled node-affinity rules and the nodes with its own API token, which holds `Sys.Audit` on `/` without propagation from the start, so Proxmox VE grants nothing new and the HA token is not used. It keeps each rule's name and the nodes it allows in the ConfigMap `kubehenge-ha-rules` beside the ClusterClasses, never a rule's comment, resources or strictness, and leaves out the rules Kubehenge keeps for its own clusters. The interface's account may read that ConfigMap only while the setting is on, and hands its rules to those with `VM.Allocate` on the workload pool or on a cluster's pool, who may create or change a cluster; those who hold `Sys.Audit` on `/` read the rules from Proxmox VE as before.
- **What it risks:** everyone who may create or change a cluster learns the rules' names and which nodes they cover, which might tell them how the nodes are grouped (by rack, hardware or tenant). Name rules with that in mind. Nothing else of Proxmox VE's configuration reaches them.
- **Switching it off** has the controller remove the ConfigMap and takes the interface's access to it away.

## The CSI plugin's newest release

The CSI plugin's newer releases need `Datastore.Allocate` on a cluster's storage, which covers every volume on it and its definition, so clusters run its last release without it unless this setting is on. With it, a cluster may choose the newest release on a storage of its own ([clusters.md](clusters.md#persistent-volumes)); its token then holds `Datastore.Allocate` there. The setting creates a role, `<name>-cluster-datastore`, holding `Datastore.Allocate`, `Datastore.AllocateSpace` and `Datastore.Audit`, and gives Kubehenge's controller `Datastore.Allocate` on each storage for clusters that the management cluster's disks, its Talos image, the cloud-init images and the template builds' downloads and uploads are not on, so that it can grant the role there. Switching it off removes the role, and with it every cluster's grant; those clusters go back to the earlier release.

## etcd restores

Restoring a cluster's etcd from a snapshot ([clusters.md](clusters.md#restore-etcd)) stops the control plane on every control-plane VM and restores on one of them, which takes commands run as root on the VMs. With this setting the controller gets a user and API token of its own holding `VM.Audit` and `VM.GuestAgent.Unrestricted` on the workload pool, and runs each step through the QEMU guest agent. That privilege amounts to a root shell in every cluster's VMs: whoever reads the controller's Secret in the management cluster, or holds its admin kubeconfig, can read every Secret of every cluster and change any workload. It reaches no Proxmox VE node and no VM outside the workload pool, and the controller uses it only for a restore someone with `Sys.Modify` on the cluster's pool asked for. Switching the setting on takes `VM.GuestAgent.Unrestricted` on `/` of you as well. Off, there is no such token, and a restore takes the same steps, run by hand from the interface's list.

## Backup targets

Each target is an S3 store and a bucket in it, which any S3-compatible store serves: AWS S3, MinIO, Ceph's RADOS Gateway, SeaweedFS, Garage, or the S3 store a Proxmox Backup Server 4.2 or later keeps a datastore on, so that the clusters' backups and the Proxmox VE backups share one store. The store must take server-side encryption with keys the client sends (SSE-C), with which every cluster encrypts what it writes.

| Field | What it is |
| --- | --- |
| Name | How clusters name the target: lower-case letters, digits and dashes. |
| Endpoint | The store's `https://` URL; empty for AWS S3 itself. |
| Bucket | The bucket, which has to exist. |
| Region | The bucket's region; `us-east-1` when empty. |
| Prefix | A folder in the bucket. Each cluster writes under `<prefix>/<namespace>.<cluster>/velero`, and its etcd snapshots under `<prefix>/<namespace>.<cluster>/etcd`. |
| CA certificate | PEM, when the system's CAs do not cover the store's certificate. |
| Send checksums | On by default; off for a store that refuses the checksums the AWS SDK sends with every upload. |

Velero signs in to a target with an S3 access key. **Shared backup key**, below the targets once the management cluster runs, stores one that every cluster without a key of its own uses. It is the least work, but every cluster's Velero then holds a key that reaches the other clusters' folders too: their keys keep the contents unreadable, but one cluster could delete another's backups. A key per cluster, limited by the store to the cluster's folder, keeps them apart; whoever creates or looks after a cluster with `Sys.Modify` on its pool stores it on the cluster ([clusters.md](clusters.md#change-add-ons-and-sign-in)).

## What the template builds may do

The weekly build runs in the management cluster with a user and API token of its own, `kubehenge-template-builder@pve`, whose token lives in the Secret `kubehenge-template-builder` there. It holds:

| On | Privileges | For |
| --- | --- | --- |
| `/pool/kubehenge-templates`, propagated | `Pool.Audit`, `VM.Allocate`, `VM.Audit`, `VM.Config.CDROM`, `VM.Config.CPU`, `VM.Config.Disk`, `VM.Config.HWType`, `VM.Config.Memory`, `VM.Config.Network`, `VM.Config.Options`, `VM.GuestAgent.Unrestricted`, `VM.PowerMgmt` | creating a template's VM, preparing it through the guest agent, turning it into a template, and deleting builds nothing needs; `Pool.Audit` lets it see which VMs are in the pool, which Proxmox VE's resource list hides without it |
| the storages of the downloaded images and the cloud-init ISO (Talos image and Cloud-init images in the setup, unless the module names others) | `Datastore.Allocate`, `Datastore.AllocateTemplate`, `Datastore.Audit` | downloading the node OS's cloud image and uploading the ISO, and deleting both afterwards |
| the template disks' storage, also when it is one of those | `Datastore.AllocateSpace`, `Datastore.Audit` | the templates' disks: `Datastore.Allocate` does not hold `Datastore.AllocateSpace` |
| `/nodes/<node>` the builds run on | `Sys.AccessNetwork`, `Sys.Audit` | Proxmox VE's download of the image |
| `/sdn/zones/localnetwork/<bridge>` | `PVESDNUser` | attaching the VM that prepares an image |
| `/`, and `/nodes` propagated | `Sys.Audit` | reading the cluster's resources and the nodes' storages |

It can touch no VM outside the template pool, no cluster's VM and none of the management cluster's, and sees no users. Two gaps remain:

- **Proxmox VE deletes a file on a storage only for `Datastore.Allocate`**, which reaches every file and volume on that storage. By default the builds use the storages of the Talos image and the cloud-init images, often `local`, so the token could delete other ISOs, images or backups kept there. Give the module storages of their own for the builds (`template_refresh.import_datastore` and `iso_datastore`), holding nothing else.
- **Kubehenge's controller can read every Secret in the management cluster**, which it needs for the clusters' credentials, so whoever takes over the controller holds this token too, as it holds Cluster API's.

Switching the refresh off removes the user, its token and the CronJob; the templates stay.

## HA group labels

The cloud controller manager labels each node with the Proxmox VE HA groups, or node-affinity HA rules, its Proxmox VE node is in. Proxmox VE lists them only with `Sys.Audit` on `/`, so this setting is on by default: each cluster has the labels unless it switches them off ([clusters.md](clusters.md#machines)), and its token then holds `Sys.Audit` on `/`. That lets the token read the Proxmox VE cluster's own configuration, the firewall rules, backup jobs, HA groups, rules and resources, and the corosync configuration among it, and change nothing; it reads nothing of the guests, storages or nodes beyond its own pool and the nodes. The setting creates a group, `<name>-clusters-audit`, holding `Sys.Audit` on `/` without propagation, and gives Kubehenge's controller `User.Modify` on that group, so that it can add the user of a cluster with the labels to it and take it out again. The controller gets nothing on `/` itself: granting there takes `Permissions.Modify` on `/`.

To keep every cluster's token from reading the Proxmox VE cluster's configuration, untick **HA group labels** in the setup, or set `cluster_sys_audit = false`. That removes the group, and with it every cluster's `Sys.Audit` on `/`; new clusters go without the labels, and the cloud controller manager logs a permission error for each node every few minutes ([troubleshooting.md](troubleshooting.md)).

## Nodes' boot logs

A node that never joins its cluster says why in its boot log ([troubleshooting.md](troubleshooting.md#a-nodes-boot-log)). Talos nodes' logs are read through their Talos API. Every kubeadm, RKE2 and k3s node keeps the end of its boot's journals in a file of its own, beside cloud-init's output, and the controller reads both through the VM's QEMU guest agent, so this setting is on by default:

- **What it grants:** Kubehenge's controller's token holds `VM.GuestAgent.FileRead` on the workload pool, propagated. Proxmox VE lets it read files in every cluster VM through the guest agent with that, and nothing else of the agent's: no commands, no writes.
- **What it reaches:** only the files the node's guest agent may open. Kubehenge's node templates allow the agent to read cloud-init's output and the node's copy of its journals (`/var/log/kubehenge`, root's alone), and its SELinux policy keeps it from the rest of `/var/log` and from the node's keys and kubeconfigs. A VM cloned from a template of your own reaches what its agent allows.
- **Switching it off,** by unticking **Nodes' boot logs** in the setup or setting `node_boot_logs = false`, takes the privilege away; the interface and `kubectl` then offer Talos nodes' boot logs alone.

## Headlamp sign-in

Without it, everyone signs in to each cluster in the shared Headlamp with a token or an identity provider of their own. With it, those who hold `Sys.Modify` on a cluster's pool, who may download its admin kubeconfig anyway, open it signed in as cluster-admin; nobody else gets more than before. It needs no Proxmox VE privilege beyond the setup's own.

- **What it grants:** Kubehenge's controller keeps two ServiceAccounts in the `kube-system` of every cluster in the interface's namespaces: `kubehenge-headlamp`, bound to `cluster-admin`, and `kubehenge-headlamp-issuer`, which may make `kubehenge-headlamp`'s tokens and do nothing else. The issuer's token lasts an hour and is renewed every 20 minutes, in the Secret `<cluster>-headlamp-issuer` beside the Cluster, like the health page's. When such a user first opens a cluster, the interface makes a token of `kubehenge-headlamp` for their session, for an hour at most and no longer than the session (ten minutes at the least, the shortest Kubernetes makes), keeps it in memory and puts it on the requests the gate passes to Headlamp for that cluster. It never reaches the browser. A token of the user's own, or a sign-in through the cluster's identity provider, takes its place while Headlamp keeps it, and logging out of the cluster in Headlamp drops the session's token until the user opens the cluster from the interface again.
- **What it risks:** whoever reads Secrets in the interface's namespaces, as the interface and the management cluster's administrators can, can act as cluster-admin in those clusters for as long as the issuer's token lasts. They can already, with the admin kubeconfigs kept there. Headlamp acts as `kubehenge-headlamp`, so the cluster's audit log names that account rather than the Proxmox VE user; the interface logs each token it makes with its user and the token's ID, which the audit log records with every request (`authentication.kubernetes.io/credential-id`).
- **Switching it off** removes both accounts from every cluster, `kubehenge-headlamp` first, which ends every token made for it at once, and then the issuer's Secret.

## Headlamp actions

Without them, the shared Headlamp shows and changes a cluster's objects and drains its nodes, with each user's own credentials there. With them, it also offers two plugins Headlamp ships with: the app catalog, which lists, installs, upgrades, rolls back and uninstalls Helm releases, and the Prometheus charts of a cluster's metrics. They need no Proxmox VE privilege beyond the setup's own.

- **What they grant:** everyone who sees a cluster uses both plugins on it. Headlamp acts in the cluster with the user's own credentials, or the [sign-in](#headlamp-sign-in)'s, so the cluster's own rights decide what they may install or remove. Charts come only from the Helm repositories set for the appliance, named `repository/chart`; a chart named by a path on the appliance, an address or an OCI reference is refused, as are dependencies fetched from where a chart says.
- **What they risk:** Headlamp keeps one list of Helm repositories for every user and cluster, and the appliance fetches their charts. Only those who hold the setup's privileges on `/` add, update or remove a repository, over HTTPS with its certificate checked and any credentials sent to its own host alone, so nobody else can point another user's install at a chart of their choosing.
- **Not offered either way:** port forwards, which Headlamp offers in its desktop app alone, as a forward listens on the machine Headlamp runs on, here the appliance, for everyone signed in to the cluster; the external proxy, which would fetch any address from the appliance, and so the app catalog's search of Artifact Hub; and Headlamp's own metrics, which count every user's requests to every cluster.
