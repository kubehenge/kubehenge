# Setup settings in depth

What each opt-in setting of the management cluster grants and risks. Backups, notifications and the module's variables are here too.

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

With the management cluster's etcd backups on ([below](#back-up-the-management-clusters-etcd)), the appliance keeps these, readable only by root in the container:

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

## HA rule labels

The cloud controller manager labels each node with the Proxmox VE HA groups, or node-affinity HA rules, its Proxmox VE node is in. Proxmox VE lists them only with `Sys.Audit` on `/`. On by default, the setting gives each cluster the labels unless it switches them off ([clusters.md](clusters.md#machines)). Its token then holds `Sys.Audit` on `/`.

### Privileges of the labels

The token can read the Proxmox VE cluster's own configuration: firewall rules, backup jobs, HA groups, rules and resources, and the corosync configuration. It changes nothing. It reads nothing of the guests, storages or nodes beyond its own pool and the nodes.

The setting creates a group, `<name>-clusters-audit`, with `Sys.Audit` on `/` without propagation. The controller gets `User.Modify` on that group, to add and remove the users of clusters with the labels. It gets nothing on `/` itself.

### Turning the labels off

Unticking **Add HA rule labels** in the setup, or `cluster_sys_audit = false`, removes the group and with it every cluster's `Sys.Audit` on `/`. New clusters go without the labels. The cloud controller manager logs a permission error for each node every few minutes ([troubleshooting.md](troubleshooting.md#ha-group-errors-in-the-cloud-controller-managers-log)).

## Nodes' boot logs

A node that never joins its cluster says why in its boot log ([troubleshooting.md](troubleshooting.md#a-nodes-boot-log)). Talos nodes' logs come through their Talos API. Every kubeadm, RKE2 and k3s node keeps the end of its boot's journals in a file beside cloud-init's output. The controller reads those files through the VM's QEMU guest agent. The setting is on by default.

### Privilege and reach

- **What it grants:** the controller's token holds `VM.GuestAgent.FileRead` and `VM.GuestAgent.Audit` on the workload pool, propagated.
  - With them, the token may read files in every cluster VM through the guest agent, and the network addresses the agent reports. It runs no commands and writes nothing.
  - The addresses show which machines' VMs lack the address they were given, as when another host holds it.
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

## Back up the appliance

The appliance holds the only copy of the management cluster's state. Add it to a Proxmox VE backup job (Datacenter, Backup) by its ID, its pool or all guests on its node. Once the management cluster exists, and until a backup job includes the appliance, administrators with `Sys.Audit` on `/` see a warning.

The installer sets the container's protection, so Proxmox VE refuses to destroy it or restore a backup over it. Take it off with `pct set <ctid> --protection 0` first.

Backups leave out the key of automatic upgrades. After a restore, switch automatic upgrades off and on again on the Administration page.

### Test a backup

Restore it so that it cannot meet the running appliance. Two appliances on one management cluster each apply their own state to it.

```sh
pct restore <new ctid> <backup> --unique 1
pct config <new ctid> | grep ^net0
pct set <new ctid> --onboot 0 --net0 <that net0>,link_down=1
```

`--unique 1` gives the copy a MAC address of its own, and `link_down=1` keeps it off the network. A `pct clone` keeps the original's address too, and copies the key of automatic upgrades.

### A state older than the management cluster

Each apply counts a generation up, kept with the appliance's state and in the management cluster. An apply or removal whose state is behind the cluster's is refused before it changes anything. A snapshot rollback, an older backup, a storage replica that took over after a crash or a second appliance leaves the state behind.

The Administration page then offers **Take over…**, with the state passphrase. The next apply brings the management cluster to this state and undoes the newer changes. In the appliance's shell, run `kubehenge appliance take-over --overwrite --passphrase-file <file>`.

While the management cluster does not answer, an administrator's apply goes ahead unchecked and its log says so. An automatic upgrade is refused.

## Back up the management cluster's etcd

The management cluster's etcd holds every cluster's objects. **Administration, Backups** snapshots it into one of the [backup targets](#backup-targets) every 6 hours unless set otherwise. It keeps the newest 28.

**Set up…** takes the target, the schedule, an S3 access key for the target and the state passphrase. The first snapshot follows within a minute. **Take snapshot** takes one at once.

The appliance takes each snapshot itself through the nodes' Talos API, with a talosconfig that may take etcd snapshots and nothing else. It encrypts the snapshot before it leaves the appliance. The key that opens the snapshots is kept in the target, encrypted with the state passphrase ([details](#management-cluster-etcd-backups)).

### The backup volume

With no settings at all, every management cluster backs up onto its own backup volume. That is the claim `kubehenge-system/kubehenge-backups`, the disk of the stopped, protected VM `kubehenge-backups`. Proxmox VE deletes a VM's own disks only, so the volume outlives the loss of every management VM.

Every 6 hours the appliance puts an etcd snapshot there, encrypted with the state passphrase, and keeps the newest 28. Beside each goes an export of Kubehenge's objects: the clusters, their classes, the Secrets Kubehenge owns and the setup settings. A backup target, when set up, gets snapshots on its own schedule.

On shared storage (Ceph RBD, NFS, shared LVM) the volume follows the management VM that serves it. On node-local storage (directory, LVM, LVM-thin, ZFS) it stays on the setup's first Proxmox VE node and attaches only during a backup or a recovery. The disk is detached the rest of the time, so Proxmox VE can migrate the management VMs and a node can go into maintenance. A backup needs one management VM on the disk's node.

On node-local storage, or in a Proxmox VE cluster of one node, losing that node can lose the backup volume. The appliance's copy can go with it when the appliance runs there too. Only a [backup target](#backup-targets) survives that, so set one up as above. Setup, Administration and Health warn until one is set.

The appliance keeps a copy of the latest snapshot, its export and its key, up to 1 GiB in all. A copy over that cap is not made, and the last copy that fit stays. The disk warning names the copy once it reaches 80 % of its cap, or once the disk has no room for the next one.

**Administration, Backups**, the management cluster's Health and `kubehenge appliance etcd-backups` show where the backups are and the last one. A failed backup degrades Health until the next one succeeds.

Removing the management cluster stops these backups. **Delete the backup volume** deletes the appliance's copy too. Kept, the copy is never restored onto a management cluster set up later.

### Recover after losing the management VMs

Use **Recover…** in the backup volume's box, or `kubehenge appliance etcd-backups recover`. Both ask for the state passphrase. The run applies the management cluster and waits up to 10 minutes for the backup volume. It then restores whichever is newer, the volume's newest snapshot or the appliance's copy, and applies again. Its log names the snapshot, where it was read and why.

### Restoring

**Restore…** on a snapshot wipes etcd on every control-plane node of the management cluster and restores it from the snapshot. Every cluster's objects go back to then.

After a restore, and then daily, the controller compares each cluster with its VMs. A cluster whose VMs are all gone shows **VMs missing** on its Health page. VMs in Kubehenge's pools that no cluster owns show as **Unowned VMs** on the management cluster's Health. Nothing deletes them.

A restore asks for the state passphrase and counts as a run of the management cluster. It needs only the nodes' Talos API, so it works while the management cluster's own API does not answer.

### The warning while backups are off

While neither a target nor the backup volume takes the backups, a warning stands on **Administration, Backups** and above the cluster list. Every cluster's CAs, kubeconfigs and backup keys live only in this etcd.

An administrator can dismiss the warning for 30 days. The page records who did and when. Notifications repeat it weekly.

The appliance's shell has the same commands, with the page's privileges and refusals ([signing in there](upgrading.md#from-the-appliances-shell)):

| Command | Does |
| --- | --- |
| `kubehenge appliance etcd-backups` | shows the target, the schedule, the last snapshot and the next |
| `kubehenge appliance etcd-backups set-up --target … --access-key-id … --secret-access-key-file … --passphrase-file …` | sets the backups up or changes them. `--every-hours` and `--keep` set the schedule. `--spec-recipient age1...` (repeatable) adds an offline key for the specs' Secrets |
| `kubehenge appliance etcd-backups off` | switches them off. The snapshots stay in the target |
| `kubehenge appliance etcd-backups snapshots` | lists the snapshots in the target, newest first |
| `kubehenge appliance etcd-backups take` | takes a snapshot and waits until it is stored |
| `kubehenge appliance etcd-backups restore … --passphrase-file …` | restores a snapshot by name and prints the run's log until it ends |
| `kubehenge appliance etcd-backups recreate … --passphrase-file …` | makes the management cluster's VMs again, then restores a snapshot into them |
| `kubehenge appliance etcd-backups recover --passphrase-file …` | makes the management cluster again from the newest snapshot on the backup volume or the appliance's copy |
| `kubehenge appliance etcd-backups dismiss-warning` | silences the warning that the backups are off for 30 days |
| `kubehenge appliance export` | saves the appliance's state, encrypted with `--export-passphrase-file`, for a new appliance ([details](#export-for-a-new-appliance)) |
| `kubehenge appliance import <file>` | reads an export into a new appliance |

## Notifications

The appliance tells a webhook, an ntfy topic or an email address when something needs a look:

- an item on a cluster's Health page that has stood for 5 minutes, and again once it clears
- an alert firing in a cluster with the monitoring add-on, which shows on its Health page too
- the management cluster not answering, one of its nodes not ready or a controller short of replicas
- a failed run: a setup, a template build, a backup, an etcd snapshot or an upgrade

**Administration, Notifications** sets each target and shows its last sending. **Send test** sends one to each. Both need `Sys.Modify` on `/`.

| Target | Settings |
| --- | --- |
| Webhook | a URL. Each notice is a JSON POST with `key`, `severity` (`error`, `warning`, `resolved` or `info`), `title`, `text`, `cluster`, `link` and `at` |
| ntfy | the topic's URL, such as `https://ntfy.sh/<topic>`, and an access token for a topic that needs one |
| Email | the SMTP server as host:port, the sender and the recipients, comma-separated. A user name and password where the server wants them. Port 465 uses TLS, any other STARTTLS where the server offers it |

More than 5 notices at once arrive as one. `link` opens the cluster's Health page once the appliance knows its own address (`--public-url`).

### Targets in Git

Each target is a Secret in the interface's namespace (`clusters` unless set otherwise):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: kubehenge-notify-ntfy   # kubehenge-notify-webhook, kubehenge-notify-email
  namespace: clusters
  labels:
    kubehenge.dev/user-credential: notify-ntfy
type: kubehenge.dev/user-credential
stringData:
  url: https://ntfy.sh/my-clusters
  token: tk_example   # optional
```

The webhook's key is `url`. The email's are `server`, `from`, `to`, and optionally `username` and `password`.

In the appliance's shell, `kubehenge appliance notifications` shows each target's last sending and `kubehenge appliance notifications test` sends a test.

## Module variables

The management cluster's module, `management-cluster/`, takes these variables. The appliance sets the same ones from its setup page.

| Variable | Default | What it is |
| --- | --- | --- |
| `proxmox_endpoint` | required | The Proxmox VE API, such as `https://pve.example.org:8006`. Every cluster's nodes reach Proxmox VE at this address too. |
| `proxmox_api_token` | none | `user@realm!name=secret`, with administrator rights. Unset, a session from the environment is used. |
| `proxmox_ca_certificate` | required | PEM of the CA that signed the API's certificate. Cluster API's Proxmox VE provider verifies the API against it. |
| `proxmox_insecure` | `false` | Skip verifying the API's certificate everywhere. That covers this configuration, Cluster API, Kubehenge's controller, the template builds and the clusters' cloud controller manager and CSI plugin. |
| `proxmox_nodes` | required | The nodes the management cluster's VMs run on, taken in turn, such as `["pve1", "pve2", "pve3"]`. Each needs the bridge and both storages below. A VM given another node is migrated there. |
| `image_datastore` | `local` | Storage the Talos image is downloaded to, on each node. It must allow `import`. |
| `vm_datastore` | `local-lvm` | Storage for the management cluster's disks. |
| `backup_volume` | `{}` | The [backup volume](#the-backup-volume), with `size_gb` (default 20, a whole number of GB, and raising it grows the volume), `protection` (default `true`) and `shared`. `shared` says whether `vm_datastore` reaches every node and is read from Proxmox VE when unset. The appliance sets `protection` only, so it keeps the default size. |
| `bridge`, `vlan_id` | `vmbr0`, none | Bridge and VLAN tag of the management cluster's VMs. The bridge takes `zone/vnet` too. |
| `mtu` | none | MTU of the management VMs' network interface, 1280 to 65520. None keeps the bridge's ([details](#management-network-mtu)). |
| `search_domains` | `[]` | Domains the nodes append to a name without dots, at most six. Empty derives one from each node's host name. |
| `time_servers` | `[]` | NTP servers of the management cluster's nodes, as host names or addresses. Empty keeps Talos's default public servers. |
| `vm_id_range` | `{ start = 10000, end = 99999 }` | The range the management VMs' IDs are drawn from at random, `end` excluded ([details](#management-cluster-vm-ids)). |
| `workload_vm_id_range` | none | The VM IDs of new clusters' VMs and the VM templates, `start` and `end` both included, such as `{ start = 100000, end = 100999 }`. None takes Proxmox VE's next free ID. It may not overlap `vm_id_range`. A cluster may set its own `vmIDRange` ([clusters.md](clusters.md#variables)). Also **VM ID range** in Setup and Administration, and `kubehenge appliance settings set --vm-id-range`. |
| `cluster_name` | `kubehenge` | The management cluster's name, a DNS label of at most 32 characters, also the prefix of its VMs' names and of the users, roles and group the module creates. |
| `ip_families` | `["IPv4"]` | `["IPv4"]`, `["IPv6"]`, or both in either order. The first is the primary family. |
| `node_addresses` | required | One, three or five IPv4 addresses with prefix length, such as `["192.0.2.11/24"]`. `[]` on an IPv6-only cluster. |
| `node_addresses_ipv6` | `[]` | The IPv6 ones, such as `["2001:db8::11/64"]`. A dual-stack cluster gives as many as `node_addresses`. |
| `api_address` | required | The Kubernetes API's virtual IP, of the primary family, in the nodes' subnet and unused. |
| `gateway`, `gateway_ipv6`, `dns_servers` | as the families need | The nodes' gateway of each family and their DNS servers, one of each family on a dual-stack cluster. |
| `pod_subnets`, `service_subnets` | none | The management cluster's own ranges, one per family in `ip_families` order. Unset, they are `10.244.0.0/16` and `10.96.0.0/12` for IPv4, and `fd5e:4b1c:9a20::/56` and `fd5e:4b1c:9a21::/112` for IPv6. The plan refuses ranges that overlap the nodes' network. |
| `node_cores`, `node_memory_mb`, `node_disk_gb` | `4`, `4096`, `32` | Size of each node. |
| `node_cpu_units` | `200` | Proxmox VE CPU weight of each node, from 1 to 10000. Proxmox VE's default is 100. When the host's CPUs are contended, the nodes get twice a default VM's share. A change reaches running nodes at once, without a reboot. |
| `node_memory_min_mb` | none | Least memory Proxmox VE's ballooning takes each node down to while its host is above 80 % memory use. From 2048 up to `node_memory_mb`. |
| `cpu_type` | `x86-64-v2-AES` | QEMU CPU type. Talos needs at least x86-64-v2. `host` spares the nodes mitigations their CPU does not need. It lets them migrate only between nodes with the same CPU ([templates.md](templates.md#how-clusters-find-them)). |
| `workload_pool` | `kubehenge` | Pool for the clusters' VMs, each cluster in a pool of its own under it. A pool name, nested at most one level. |
| `template_pool` | `kubehenge-templates` | Pool of the VM templates. |
| `management_pool` | `kubehenge-management` | Pool of the management cluster's own VMs, out of Cluster API's reach. |
| `workload_datastores` | `["local-lvm"]` | Storages clusters may use. |
| `workload_bridges` | `["vmbr0"]` | Bridges clusters may attach to, with any VLAN on them. A node's bridge is its name. An SDN VNet is `zone/vnet`, such as `evpn1/web`. |
| `bootstrap_datastore` | `local` | Storage for cloud-init ISOs. Cluster API gets full control of it. |
| `proxy` | `{}` | The HTTP(S) proxy for downloads: `url`, `no_proxy` (comma-separated, with the Proxmox VE host, the nodes' network and the pod and service ranges) and `ca_bundle` (PEM). Sensitive. Workload clusters without a proxy of their own take it for their nodes and add-ons, unless `url` holds credentials. |
| `chart_repositories` | `{}` | Mirrors of the Helm chart repositories. `mirrors` maps a repository host to an `oci://` or `http(s)://` address of the same kind as the host. Each mirror serves the same charts under the same digests. `ca_bundle` (PEM) is the authority of mirrors the usual ones do not sign ([details](clusters.md#mirroring-the-charts)). |
| `release_mirror` | `{}` | A mirror of the RKE2 and k3s releases on GitHub's paths. `url` is its `http(s)://` address and `ca_bundle` (PEM) the authority of an `https://` mirror the usual ones do not sign ([details](clusters.md#mirroring-the-releases)). |
| `registry_mirrors` | `{}` | Registry mirrors for the management cluster's nodes, by registry name. Each takes endpoint URLs tried in order before the registry itself. A mirror of `factory.talos.dev`, `ghcr.io` or `registry.k8s.io` must serve the images' cosign signatures too. The nodes require them on Sidero's and Kubernetes' images and on the Talos providers'. |
| `backup_targets` | `[]` | S3 stores clusters back up to with Velero ([details](#backup-targets)). Up to sixteen, each with `name` and `bucket`. Optional fields are `endpoint` (https, AWS S3 when unset), `region`, `prefix`, `ca_certificate` and `checksums` (default true). Access keys are stored through the interface, or named per cluster with `existingSecret` ([clusters.md](clusters.md#variables)). |
| `cluster_oidc` | `[]` | OpenID Connect providers every new cluster trusts unless it names its own. Up to eight, each with `issuer_url` (https) and `client_id`. Optional fields are `username_claim`, `username_prefix`, `groups_claim`, `groups_prefix`, `scopes` and `certificate_authority`. |
| `version_overrides` | `{}` | Versions to use instead of the tested set, keyed by component, to hold a component back. A provider held back needs its image in `provider_images` and its files' digests in `provider_manifests`. Lowering a version on a running cluster is not supported. |
| `provider_images` | `{}` | Controller images by digest to run instead of the tested ones, by `"<kind>/<name>"`, such as `"InfrastructureProvider/proxmox"`. |
| `provider_manifests` | `{}` | The SHA-256 of provider files outside the tested set, by their address under `https://github.com/`, such as `"kubernetes-sigs/cluster-api/releases/download/v1.14.2/core-components.yaml"`. These are `metadata.yaml` and the components file of each provider held back. |
| `controller_image` | the chart's | Kubehenge's controller image by digest, as `repository@sha256:<digest>`. A release's chart names the image it was released with. A checkout's names none, so a module taken from git needs one. |
| `proxmox_ha` | `false` | Keep each cluster's control-plane VMs on separate nodes with Proxmox VE HA. It uses a token with `Sys.Audit` and `Sys.Console` on `/` that only a short-lived Job per write holds ([details](#proxmox-ve-ha)). After setting it back to false, remove the HA resources and rules whose comment starts with `Kubehenge cluster`, as the appliance does. Without the token the controller cannot. |
| `offer_ha_rules` | `false` | Offer Proxmox VE's HA node-affinity rules (names and nodes) to those with `VM.Allocate` on the workload pool or a cluster's pool who lack `Sys.Audit` on `/`. The controller reads them with its own token and asks nothing new of Proxmox VE ([details](#ha-rule-placements)). |
| `cluster_datastore_allocate` | `false` | Let clusters choose the CSI plugin's newest release. The controller grants the cluster's token `Datastore.Allocate` on a storage of its own ([details](#csi-plugin)). |
| `template_refresh` | [below](#template_refresh-fields) | The weekly build of the node OS VM templates ([templates.md](templates.md#the-weekly-build)) and its own API token ([details](#what-the-template-builds-may-do)). |
| `cluster_sys_audit` | `true` | Give each cluster the cloud controller manager's HA rule labels unless it switches them off. This gives the cluster's token `Sys.Audit` on `/`, unpropagated, through a group the controller adds its user to. It reads the Proxmox VE cluster's configuration and changes nothing ([details](#ha-rule-labels)). `false` gives no cluster `Sys.Audit` on `/`. |
| `watch` | `stall_minutes = 20` | `stall_minutes` (1 to 1440) is how long Kubehenge's controller waits before it reports a stall on the Cluster and the Health page. That covers a new machine in the roll of a cluster's only control plane becoming ready ([troubleshooting.md](troubleshooting.md#one-control-plane-its-roll-stalled)) and a cluster's deletion finishing ([troubleshooting.md](troubleshooting.md#a-cluster-does-not-finish-deleting)). |
| `node_boot_logs` | `true` | Let the controller read kubeadm, RKE2 and k3s nodes' boot logs through their guest agents (files only). Its token gets `VM.GuestAgent.FileRead` and `VM.GuestAgent.Audit` on the workload pool ([details](#nodes-boot-logs)). `false` takes the privilege away. Talos nodes' boot logs are read through their Talos API either way. |
| `extra_cluster_namespaces` | `[]` | Namespaces the interface creates and manages clusters in beside `clusters` ([clusters.md](clusters.md#more-namespaces-for-clusters)). Each is created if missing and kept when taken off the list. `default`, `cert-manager`, `kube-*` and `*-system` are refused. |
| `etcd_restore` | `false` | Have the controller restore clusters' etcd itself through the VMs' guest agents. It uses a token holding `VM.Audit` and `VM.GuestAgent.Unrestricted` on the workload pool that only a short-lived Job per step holds ([details](#etcd-restores)). Without it, the interface shows the steps to restore by hand. |
| `headlamp_sign_in` | `false` | Have the appliance's shared Headlamp sign those who may download a cluster's admin kubeconfig in as cluster-admin. It does so through two ServiceAccounts the controller keeps in every cluster of the interface's namespaces ([details](#headlamp-sign-in)). Without the appliance it does nothing but keep those accounts. |
| `headlamp_actions` | `false` | Have the appliance's shared Headlamp offer the Helm app catalogue and the Prometheus charts it ships with. They act with each user's own credentials in the cluster ([details](#headlamp-actions)). Without the appliance it does nothing. |
| `api_audit_log` | `false` | Have the management cluster's API server log each change people make, which the interface's Activity page then lists with its user ([details](#kubectl-changes)). |

### `template_refresh` fields

| Field | Default | What it is |
| --- | --- | --- |
| `enabled` | `true` | Whether the weekly build runs. |
| `weekday`, `time`, `time_zone` | `Sun`, `01:00`, `UTC` | When it runs. `time` is `HH:MM`. |
| `soak_hours` | `24` | How old a build is before clusters move to it, 0 to 720. |
| `node` | the first of `proxmox_nodes` | The node the builds run on. |
| `storage` | the first of `workload_datastores` | The template disks' storage. |
| `import_datastore`, `iso_datastore` | `image_datastore`, `bootstrap_datastore` | Storages for the downloaded image and the cloud-init ISO. |
| `bridge`, `vlan` | the first of `workload_bridges`, `0` (none) | Where the VM that prepares an image attaches. The bridge takes `zone/vnet` too. |
| `address`, `gateway`, `dns` | DHCP | A static address with prefix, gateway and DNS servers for that VM. |
| `creator_builds` | `false` | Lets those who may create clusters have a missing template built. |
| `kubeadm_patches` | `true` | Also builds the kubeadm template of the newest patch release of each Kubernetes minor version a kubeadm cluster runs, and of the next minor version. |

Outputs: `kubeconfig` and `talosconfig` (sensitive), `api_endpoint`, and `proxmox_objects`, which lists everything the module made on Proxmox VE.
