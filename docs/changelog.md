# Changelog

Versions follow [Semantic Versioning](https://semver.org/). Before 1.0.0 a minor version may change behaviour.

## [Unreleased]

### Before you upgrade

- Pod Security Admission's default level moves from `privileged` to `baseline`. A cluster that sets `podSecurity.admission` keeps its level.
  - One that never set it moves to `baseline`, which replaces its control-plane machines.
  - Set `privileged` on it first to keep pods that need more.
- Velero and RKE2's or k3s's own etcd upload wait until a cluster has its own backup access key. Store one for each cluster in its backups ([clusters.md](clusters.md#back-up-and-restore)).
- The module's `cluster_volume_snapshots` is gone. Remove it from the module's variables.
- The management-cluster module needs the Proxmox VE provider 0.119. Run `tofu init -upgrade`.
- OpenID sign-in needs `--public-url`. The appliance sets it.
- Only `kubehenge template`, `controller`, `serve` and `csi-proxy` take `--proxmox-url`, `--proxmox-ca` and `--proxmox-insecure`. Remove them from scripts that pass them to other commands.
- The API server now refuses Clusters that break the interface's rules (listed under Fixes). Check the Clusters kept in Git first.
- Fixed pools need one node address more than their machines.
- A kubeadm control plane counts 3376 MiB for itself, up from 2392 MiB.
- Boot logs need the kubeadm, RKE2 and k3s templates built again.
- `install.sh --upgrade` adds the volume that sealed automatic-upgrade secrets need.
- Applying the management cluster replaces the nodes of Talos clusters, which get a new log target. It also replaces the control planes of kubeadm, RKE2 and k3s clusters, which get a CPU request for kube-vip among other changes ([upgrading.md](upgrading.md#when-a-release-changes-a-clusterclass)).

### Interface

- **A faster interface.** Pages load the session, setup status and fleet together.
  - A change to a cluster or machine shows at once in the fleet, the runs and the cluster's page.
- **Machines** lists the control plane and the worker pools with their sizes. It adds a pool, resizes the control plane or a pool and deletes a pool, staged with the other changes.
  - The API server refuses a new pool whose name cannot be part of its nodes' host names.
  - Each worker has **Delete**. It drains the worker and shrinks its pool by one.
  - Each machine has **Cordon** and **Uncordon**. They act through an identity in the cluster that an admission policy limits to a node's cordon.
  - Each machine has **Reboot** for those with `VM.PowerMgmt`.
  - Each machine has **Console** for those with `VM.Console`. It opens the machine's VM console in Proxmox VE.
- The fleet list at `/clusters` takes the whole page and can be filtered, grouped and sorted.
  - With no clusters yet, the fleet list shows whether a VM template is built and whether you may create clusters.
- Each cluster has one health verdict, the same in its badge, the fleet list, the fleet down the left, its header and its health page.
- A cluster's page opens at once with what the fleet list last read.
- A view seen before shows its last reading while it reads it again. A view read for the first time shows a placeholder in its shape.
- A cluster just created or deleted shows so in the list and the rail at once.
- A tab with something to look at carries a badge. A strip above the cluster's other views shows the worst problem.
- A reading that could not be renewed is greyed out as stale. While the clusters cannot be read, the list counts down to its next try.
- A failed run starts again from its page with what it was started with. It asks again for a passphrase or a typed name where it took one.
- An appliance upgrade shows its steps.
- An etcd snapshot, an etcd restore and a change show their events since they began.
- Machines on an offline Proxmox VE node show as down, and nodes of a cluster that does not answer as not read.
- **Ctrl K** goes to any cluster or page and starts a cluster's actions. The actions also open from their address.
- Shortcuts that leave a page take two keys pressed in turn, such as `G` `C` for the clusters. A stray key leaves no page.
- The theme can be set to light, dark or the system's.
- The top bar and fleet list fit phones. Fields stack on them.
- A cluster's options are grouped, with on-or-off settings switched in their rows.
- **Scale** sends only what changed.
- **Upgrade** offers the newest version it may move to and checks it first.
- A machine is replaced from its row.
- The create wizard starts from the newest working cluster's settings, or from another's or the defaults.
- The wizard takes the network from the bridge, suggests free addresses and marks typed ones in use.
- The wizard shows each add-on as a card with its memory.
- On **Confirm** the wizard has the API server check the cluster. It weighs the cluster's memory and disk against what is free.
- The cluster can be copied as YAML.
- The management cluster is set up in steps from what the nodes report. Its health is shown, and its removal sits in a danger zone.
- Each identity provider has its own sign-in button.
- A machine whose node is down shows as down, and one whose kubelet stopped reporting as not reporting.
- A read that hangs is given up after 30 seconds.
- What needs attention leads to where it is acted on: the machine's row in **Machines**, or **Backups**.
- A finished backup's or restore's run page shows the end of its Velero log.
- A failed run of the appliance links to the page it is started from.

### Operating clusters

- **`kubehenge cluster`** lists, creates, scales and deletes clusters, prints a cluster's kubeconfig and acts on its machines through the interface. It signs in with a Proxmox VE API token from `KUBEHENGE_PROXMOX_TOKEN`.
  - The interface takes any request with an `Authorization: PVEAPIToken=…` header as the token's, with the token's privileges. Scripts and CI can drive it without a password ([From the command line](clusters.md#from-the-command-line)).
- **The `kubehenge` commands say what they expect.** A mistyped subcommand fails and names the closest one: `unknown command "lsit" for "kubehenge cluster": did you mean list?`.
  - A command given the wrong arguments says which it takes, such as `give the cluster's name`.
  - `kubehenge cluster list -o` checks its format before it calls the interface.
  - In `kubehenge cluster`, only the commands that find one cluster by its name take `-n`. `list` and `create` refuse it.
  - `kubehenge appliance` takes `--json` after its subcommand, as in `kubehenge appliance upgrades --json`. So do `--user`, `--password-file` and `--totp`.
- Two new conditions on the Cluster say what holds it up, the same in kubectl as in the interface.
- The condition `KubehengeReady` carries the error Kubehenge's controller last met there, with a Warning event for each.
- The condition `MachinesStalled` says when a machine is not ready and none has become ready for 20 minutes. It carries the worst its Machines and ProxmoxMachines report.
- The cluster's page shows both above its views. The fleet counts such a cluster among those that need you.
- A change or creation whose machines are stuck shows under **Stalled** in the runs.
- A Proxmox VE task that fails is reported with its UPID and the last lines of its log rather than its exit status alone.
- The controller counts settled and stalled rollouts and how long they took (`kubehenge_machine_rollouts_total`, `kubehenge_machine_rollout_duration_seconds`).
- **Request ids.** Every answer of the interface carries a request id.
  - An error shows the request id as its reference, and every journal line of the request carries it.
- The appliance answers `ok` at `/healthz` without a sign-in.
- The interface and the controller take `--log-level` and `--log-development` (the appliance's `KUBEHENGE_LOG_LEVEL`, the chart's `controllerLogs`).
- The controller serves its metrics over HTTPS to accounts holding `kubehenge-metrics-reader`. The metrics include how often Kubehenge's Cluster API hooks held a cluster back or refused it ([troubleshooting.md](troubleshooting.md#where-to-look)).
- **Support bundles and boot logs.** `kubehenge diagnose <namespace>/<cluster>`, and **Support bundle** on the cluster's page for those who may download its kubeconfig, write one `.tar.gz`.
  - The bundle holds the Cluster and every Cluster API object of it, their events and its machines' boot logs.
  - It holds the lines of the controller's, capmox's and the Add-on Provider for Helm's logs that name its objects. On the appliance, it holds the lines of the interface's journal that name them too.
  - It holds no Secret, and tokens and passwords are replaced by `[REDACTED]`.
  - **Boot log** on each machine in the Machines view, or the Cluster's `kubehenge.dev/boot-log-request` annotation, has the controller read what a node logged as it booted. It writes the log into the ConfigMap `<cluster>-boot-logs`.
  - The log holds cloud-init's output and the journals of cloud-init, containerd, the kubelet, RKE2 or k3s. For a Talos node it holds the kernel and service logs through its Talos API ([troubleshooting.md](troubleshooting.md#a-nodes-boot-log)).
  - kubeadm, RKE2 and k3s node templates keep a copy of those journals in `/var/log/kubehenge` from boot on. The guest agent hands it out.
  - The controller's token holds `VM.GuestAgent.FileRead` on the workload pool to read the copy and cloud-init's output. It reads files only, no commands.
  - Boot logs are on by default. `node_boot_logs = false`, or **Nodes' boot logs** in the setup, takes the privilege away ([install-settings.md](install-settings.md#nodes-boot-logs)).
  - Templates need building again for it.
- The interface's account in the management cluster may now read every Cluster API object in its namespaces. It may also read the pods' logs of the controller, capmox and the Add-on Provider for Helm.
- The appliance's interface reads its own journal.

### Backups and snapshots

- **Backups, all through the interface.** A backup can be deleted before it runs out, once its name is typed (a Velero `DeleteBackupRequest`).
  - A restore can pick its namespaces, its kinds of resources and whether the volumes' data comes back.
  - A finished restore lists Velero's errors and warnings by namespace.
  - A cluster can read the backups of others of its namespace on the same target (the `backups` variable's `readFrom`), to restore them into it.
  - A cluster created reading another's restores that cluster's volumes' data too.
  - The health page and the fleet say when the last backup failed or backups are overdue. See [Back up and restore](clusters.md#back-up-and-restore).
- Volume snapshots need no `root@pam` password any more.
- A proxy beside each cluster's CSI plugin makes the volume copies with the cluster's own token. It imports each into a disk of a holder VM in the cluster's pool on the volume's node.
  - The proxy turns the plugin's names of the copies into their real ones and back. It copies only the cluster's own volumes, one at a time, and logs each.
  - The setup's **Volume snapshots** setting and the module's `cluster_volume_snapshots` are gone. Every cluster on storage local to each node may take snapshots, and each cluster's role on its pool holds `VM.Clone` for it.
  - A snapshot of a large volume turns ready once its copy is whole. Switching volume snapshots on no longer takes `Permissions.Modify` on `/`.
  - A copy stays on its volume's node.

### Providers and cluster checks

- The management-cluster module needs the Proxmox VE provider 0.119. It downloads the Talos image without first asking the Image Factory for its size.
  - That request held up one of Proxmox VE's API workers and dropped the module's other requests on it. The download no longer waits for the module's other changes.
- **Cluster variables checked from the first cluster on.** Cluster API admitted the first cluster of each distribution without checking its variables or storing their defaults.
  - A later change of its CNI or CIS hardening escaped the rules that fix them.
  - Kubehenge's admission now checks every cluster against its class's variables. Where no default was stored, it uses the one the cluster runs on.

### Security

- **Interface security.** OpenID sign-in needs the interface's public URL (`--public-url`, which the appliance sets) and is refused without it.
  - Deleting a cluster or restoring its etcd takes the cluster's name in the request, as the dialogs already ask for it.
  - A change made against a cluster someone else has changed since is refused (412) instead of undoing theirs. The page drops what was staged and shows the cluster as it is now.
  - The interface's credential in the management cluster is a token that runs out after 30 days, renewed daily while the appliance runs. It replaces one that never expires.
  - Too many failed sign-ins refuse further tries for a while, by that user from that address, and from that address overall. Failures from elsewhere cannot lock an administrator out.
  - Headlamp's port takes changes and WebSockets only from its own pages.
  - Both ports send `Strict-Transport-Security`.
- **Appliance security.** A certificate the installer renews or replaces keeps a DNS name only in one of two cases.
  - Either the cluster's CA signed it for that name, or this node's DNS resolves it to the appliance. A name only the container claims is dropped.
  - It takes a static address from the container's configuration rather than from inside it.
  - The updater keeps one capability and runs under a system-call filter, with memory that cannot be made executable and other users' processes hidden. The interface runs under the same filter.
  - The state passphrase and token that automatic upgrades of the management cluster keep are sealed. The key is on a 1 GiB volume of the container that Proxmox VE's backups leave out.
  - An appliance restored from a backup asks for them to be switched off and on again. `install.sh --upgrade` adds the volume.
  - The release index expires two weeks after it is signed, and appliances refuse an expired one.
  - Appliances read the index from `https://kubehenge.dev/releases/index.json`, signed again every week from the latest release, instead of the release itself.
  - The release key can be rotated. Builds trust every key in `pkg/release/release.pub`, and the index carries a signature of each key that signed it ([releasing.md](releasing.md#rotating-the-release-key)).
- **Cluster security.** Pod Security Admission's default level is now `baseline`, where it was `privileged`. It warns about and audits pods short of `restricted` at every level.
  - Choosing `privileged` in the interface now writes it into the cluster's variables.
  - Kyverno exempts only the namespaces of the add-ons and the CNI a cluster runs, RKE2's own Calico among them.
  - The controller's Runtime Extension hooks read the Cluster from the API server, not from the request. The delete hook acts only on a Cluster being deleted.
  - The validating webhook fails closed. The controller may not write ServiceAccount token Secrets.
  - An etcd restore first takes a snapshot of etcd as it is, and shows it on the restore. It refuses a snapshot without a well-formed SHA-256.
  - The management-cluster module installs the Cluster API Operator from a chart archive checked against its SHA-256. The release carries the archive, and a checkout runs `make capi-operator-chart`.
- **Proxmox VE HA and etcd restore tokens.** The controller no longer holds the tokens `proxmox_ha` and `etcd_restore` give it.
  - Each sits in a Secret of its own (`kubehenge-proxmox-ha`, `kubehenge-proxmox-restore`, key `token`). Only a short-lived Job mounts it, one per HA write or restore step.
  - The API server refuses any other Job that names either Secret ([architecture.md](architecture.md#tokens-kept-out-of-the-controller)).
  - The HA Job writes only HA resources of VMs and rules that carry Kubehenge's comment, and replaces only resources that carry it. HA reads use the controller's own token.
- **Management cluster network policies.** The management cluster enforces NetworkPolicies, with kube-network-policies beside Talos's Flannel.
  - Nothing reaches a pod in `kubehenge-system` or `kubehenge-autoscaler` but the controller's webhook, lifecycle hooks and metrics.
  - The HA and restore Jobs reach only DNS and Proxmox VE's API ([architecture.md](architecture.md#management-cluster)).
- The backup access key an administrator stores for every cluster stays in the management cluster. Only Kubehenge's own etcd snapshots use it.
- Velero and RKE2's or k3s's own etcd upload take the cluster's own key, and wait until it is stored.
  - A key that reached every cluster's folder let one cluster's admins delete the others' backups.

### Fixes

- When the installer removes a container whose interface failed to start or to reach Proxmox VE's API, it prints the interface's log first. That log went with the container before.
- The installer's check of the new appliance verifies the appliance's certificate instead of skipping it. `install.sh --help` lists every option.
- `kubehenge template create` and `template refresh` refuse an empty `--node` before calling Proxmox VE.
- The controller refuses a Pod Security exemption list with anything after its closing brace. It read the list only up to that brace and silently held the namespaces after it to the cluster's level.
- A template refresh releases its Lease when it ends, also when its last renewal was still under way. The release then went to an older version of the Lease, was refused, and the next refresh waited for the Lease to run out.
- After an etcd restore, a Talos cluster's nodes reach the API server and every other Service as before.
  - Talos restored the snapshot's revisions as they were. The kube-proxy and every other client on the nodes not wiped waited for revisions they had already seen.
  - They kept the endpoints from before the restore. Pods on workers got "connection refused" from the API server's Service.
  - The restore now moves the snapshot's revision on and marks the revisions before compacted, as a kubeadm, RKE2 or k3s restore does. Every client lists again.
  - The by-hand steps do the same with `etcdutl`.
- Talos clusters with Cilium send their services' and kernel's logs to Loki.
  - Under Cilium, nothing Talos sent to the node collector on loopback arrived.
  - Talos now sends to a link-local address every Talos node has on a dummy link of its own. It reaches the collector with every CNI.
- When the interface stops, as on an appliance upgrade, it lets the requests under way finish. Its Headlamp and token upkeep stop before it exits.
  - It exited as soon as the first port closed. That cut off requests and possibly a file being written.
- The interface sets itself up once, whether its handlers or its session upkeep start first. The two could each make the session store, and the upkeep then pruned one the interface no longer used.
- RKE2 clusters with Cilium start on AlmaLinux and Rocky Linux nodes.
  - Their templates keep `/opt` in `/var`, where SELinux labelled `/opt/cni/bin` so that RKE2's Cilium could not install its portmap plugin there. The cluster's first node never became ready.
  - The templates label it for containers, and a template build checks it.
- k3s and kubeadm clusters with Calico start.
  - Calico installed its FlexVolume driver into `/usr`, which is read-only on the node OS templates as on Talos. Only Talos clusters left the driver out, so elsewhere Calico's node agent never started.
- **Pod and service ranges for every cluster.** The controller gives a new cluster the pod and service ranges it does not name. Before, only the interface did.
  - A cluster gets its distribution's own ranges where they are clear of the nodes' network, and other ones where not.
  - A cluster that runs Cilium gets ranges clear of every other cluster's.
  - A cluster applied with kubectl or through GitOps gets the same ranges as one created in the interface.
  - The API server refuses pod or service ranges that overlap the nodes' network or each other, or are not IPv4 networks.
  - A kubeadm cluster with Calico applied with kubectl never got Calico. kubeadm had no pod range, without which the Tigera operator does not install it.
- Rolling a Calico cluster's nodes no longer stalls.
  - The Tigera operator and Calico's Typha tolerated every taint, a cordoned node's too. The scheduler put them back onto a node being drained and the drain never ended.
  - They now tolerate only the taints of control-plane nodes and of nodes not ready yet. This holds for RKE2's own Calico as for the one Kubehenge installs.
- Host ports, Traefik's among them, work on RKE2 and kubeadm clusters with Calico or Canal. This holds on the AlmaLinux, Rocky Linux and openSUSE Leap templates.
  - The CNI's portmap plugin runs `iptables` from the node. Those templates did not have it, so no pod with a host port started.
  - RKE2 and kubeadm templates now carry iptables on nftables, and a template build checks it.
- Calico no longer reports itself degraded on the AlmaLinux, Rocky Linux and openSUSE Leap templates. SELinux kept the Tigera operator from reading the MTU Calico found in `/var/lib/calico`.
  - The templates label it for containers to read, and a template build checks it.
- kubeadm and k3s clusters with Calico no longer run Calico's flow-log aggregator Goldmane and its viewer Whisker. Nothing read them.
  - On small nodes they took the CPU the cluster's own pods needed. RKE2's own Calico already ran neither.
- A kubeadm control plane now counts 3376 MiB for itself before any add-on, up from 2392 MiB. The figure is its measured use plus 25%.
- A kubeadm control plane with every add-on but Rancher needs 4976 MiB.
  - With Calico and every add-on on 4096 MiB, the API server sat at its memory limit and the node's caches were gone. It stalled for over a minute as Argo CD's resource types came in, and the controllers whose leases ran out restarted.
- AlmaLinux and Rocky Linux nodes keep pressure stall information. Their kernels leave it off unless asked.
  - The kubelet's pressure metrics and `/proc/pressure` tell a node short of memory, CPU or IO.
- Kyverno's admission controller runs two replicas with a disruption budget.
  - A lone one shut down whenever it could not renew its lease within 10 s, as when etcd paused for a compaction. With its policies failing closed, every pod was refused until it was back.
  - Kyverno's add-on figures rise to 640 MiB and 500 millicores.
- A Cluster applied with kubectl or through GitOps is held to the interface's rules by the API server ([clusters.md](clusters.md)):
  - a name of at most 40 characters
  - a version in the distribution's form
  - upgrades of one minor version at most and never back
  - 1, 3, 5 or 7 control-plane nodes
  - at most 100 workers in each pool
  - node addresses and a control-plane endpoint on the gateway's network, with the endpoint outside the node addresses
- Fixed pools need node addresses for every machine plus one for a rollout, as autoscaled ones already did, since a rollout otherwise waits for ever.
- Two clusters on the same bridge and VLAN can no longer be given the same node addresses or control-plane endpoint. This holds from the interface or kubectl alike.
- With `proxmox_insecure`, etcd restores through the guest agent skip the check of Proxmox VE's certificate, as the controller's other connections do.
- A Talos node whose machine is deleted while it is upgraded in place, as when the cluster scales down onto it, has its upgrade dropped.
  - Before, the install waited out its 15 minutes on the gone VM and then failed. That stopped every Talos upgrade of the cluster until someone pressed **Try again**.
  - An install that stalls for three minutes or finds the node unreachable is tried again up to four times, a minute, two and four apart.

### Recovery, health and upgrades

- **A single control plane's stalled roll is found and recovered.** A change that replaces a cluster's only control plane is warned of in the plan and by kubectl.
  - Once its new machine has not been ready for 20 minutes (`watch.stall_minutes` in the module, `watch.stallMinutes` in the chart), the Cluster gets the condition `ControlPlaneRollStalled`. The condition says why and whether the API still answers.
  - The Cluster also gets a warning event, an item on the Health page and **Recover…** on the cluster's page.
  - Recover, or the annotation `kubehenge.dev/etcd-restore` naming a `machine`, keeps that control-plane machine. It resets etcd to its own database when the API is down, and replaces the others ([troubleshooting.md](troubleshooting.md#one-control-plane-its-roll-stalled)).
- **A stuck deletion says what it waits on, and can be abandoned.** While a cluster is being deleted, its page lists every object whose finalizer holds the deletion, with what that object reports it waits for.
  - The Cluster's annotation `kubehenge.dev/deletion-status` lists them too.
  - Once the deletion has waited 20 minutes (the same `watch` setting), the Cluster gets the condition `DeletionStalled` with a warning event. The fleet and the Health page list it.
  - **Abandon…** (or the annotation `kubehenge.dev/abandon-deletion` set to the cluster's name) gives up on the deletion.
  - Machines go without draining, and what is left of the cluster is deleted.
  - Whatever has not finished five minutes later loses its finalizers. Each thing left behind in Proxmox VE is named in a `LeftBehind` event ([troubleshooting.md](troubleshooting.md#a-cluster-does-not-finish-deleting)).
- **Certificates are renewed before they run out.** kubeadm's control plane is replaced 60 days before its certificates run out.
  - For RKE2 and k3s, the controller reads what their own certificate monitor reports on each node. From 60 days before, it rolls the workers' pool and k3s's control plane, one machine at a time.
  - It does the same for an RKE2 control plane of three or more.
  - An RKE2 control plane of fewer machines says to restart `rke2-server`.
  - The Cluster gets the condition `CertificatesExpiring` with events. Its Health page and the fleet show it.
  - The kubeconfig and talosconfig kept for each cluster are watched too ([troubleshooting.md](troubleshooting.md#certificates)).
  - The interface warns administrators 30 days before the management cluster's admin credentials run out. An apply renews them.
- **A template build that fails is tried again**, three times in all. The tries are 2 and then 8 minutes apart, from the interface and the weekly build alike.
  - A failure that would repeat, such as a node whose CPU is too old, is not tried again.
  - A weekly build that still fails records a `TemplateBuildFailed` warning event on the ConfigMap `kubehenge-template-refresh` ([troubleshooting.md](troubleshooting.md#template-build-fails)).
- **VMs that do not run and storage nearly full show on the Health page and the fleet.** The controller looks every minute at the VMs of each cluster's joined machines and at the cluster's storage.
  - A VM stopped, paused by hand, paused by QEMU after a disk error, or crashed for 3 minutes sets the condition `VMsNotRunning`.
  - A storage 85% full on a node the cluster runs on sets `StorageNearlyFull`. It turns to an error from 95%.
  - Both record events ([troubleshooting.md](troubleshooting.md#vms-that-do-not-run-storage-nearly-full)).
- **The management cluster's etcd is backed up, and restored from the appliance.** **Administration, Backups** snapshots it on a schedule into a backup target, encrypted.
  - It works through the nodes' Talos API with a talosconfig that may only take etcd snapshots. It lists the snapshots there.
  - **Restore…** wipes etcd on the control-plane nodes and restores one with the state passphrase. It works while the management cluster's API is down too ([install.md](install.md#back-up-the-management-clusters-etcd)).
- **An appliance upgrade that does not start is rolled back.** The updater keeps the installed release until the new interface answers on the appliance's certificate within 2 minutes and keeps running for 20 seconds.
  - Otherwise it puts the old release back and restarts it. The Administration page shows the upgrade as rolled back, with why ([troubleshooting.md](troubleshooting.md#appliance-upgrades)).
  - `install.sh --upgrade` checks the new release the same way and puts the old one back with the reason.
- **`kubehenge appliance` in the appliance's shell** shows the upgrades and how the last one went, asks for one, and checks that the interface serves.
  - It sets up, lists, takes and restores the management cluster's etcd backups, with the Administration page's privileges and refusals ([upgrading.md](upgrading.md#from-the-appliances-shell), [install.md](install.md#back-up-the-management-clusters-etcd)).

### Template and VM fixes

- The management cluster's VMs get VM IDs picked at random between 10000 and 99999. Proxmox VE offers whatever is created at the same moment the same next free ID.
  - Another VM, such as a template being built, could take that ID first. The Terraform/OpenTofu provider for Proxmox VE then took that VM for the node it was creating.
  - It changed and started that VM and kept it in its state, to be destroyed with the node.
- A cluster's machine whose clone fails is cloned again under a new VM ID. capmox kept the ID of the failed clone.
  - A VM created at the same moment may have taken that ID. The machine then waited on that VM for ever and, once deleted, deleted it with itself.
- Deleting a cluster's machine destroys its VM only once the VM under the machine's VM ID is confirmed as the machine's. The check is by its name and SMBIOS UUID.
  - A free ID, or one held by another VM or a container, counts as the VM gone.
  - Proxmox VE hands a freed ID to the next clone. capmox's next pass over the deletion could destroy that clone, possibly a machine of another cluster.
- An AlmaLinux or Rocky Linux template build moves on from a mirror that serves a broken `repomd.xml`, as a mirror does while it syncs.
  - This holds in every step that reads the family's repositories.
  - Rocky Linux's repositories read their metalinks. Their checksums have dnf and rpm-ostree refuse a broken or stale `repomd.xml` and take the next mirror at once.
  - AlmaLinux's mirror service has no metalinks. Its mirror lists are fetched once and each failed attempt starts on the next mirror.
  - The mirror services gave a builder the same mirrors in the same order each time. Its three attempts read the same broken file, and the template was not made.
- Every add-on's pods request CPU and memory. So do the Traefik, CNI and snapshot controller RKE2 and k3s bring, and the control planes' kube-vip.
  - Many charts request none. On a node busy with every add-on starting, such pods got too little CPU to answer their liveness probes and were restarted.
  - Among them were Prometheus's operator, MetalLB's speakers and RKE2's Traefik.
  - The add-ons' CPU figures count the new requests, so a 2-core worker no longer takes every add-on on RKE2 and Talos.
- k3s clusters run without k3s's local path provisioner. Its StorageClass claimed to be the default beside the Proxmox CSI plugin's.
  - A claim without a class went to whichever was made last, and could end up on one node's disk.

## [0.3.1] - 2026-10-28

- capmox, which creates the clusters' VMs, is built from a later commit of its main branch.
- A machine whose VM needs no configuration change is started instead of being reported ready while its VM stays off. A transient error while attaching the bootstrap ISO is retried instead of failing the machine for good.
- The installer names the address of a download that still fails after its retries, where curl's own message gives only the HTTP status.

## [0.3.0] - 2026-10-27

### Before you upgrade

- Build the AlmaLinux 10 or Rocky Linux 10 templates first. Ubuntu templates are no longer made or selected, and clusters move to the new ones as the ClusterClasses change.
- Every cluster's control planes are replaced, for the API server's memory limit. RKE2 and k3s control planes are replaced for the etcd member fix too.

### Interface

- The fleet is down the left of every page, grouped into those that need you, those with work running and the healthy, each with why. It counts what each cluster's health page finds.
  - The work under way is down the right, snapshots, backups, restores, template builds and the appliance's upgrades included. A failed run shows its first error.
- A cluster's views are tabs under its name. Its summary opens on its machines laid out by Proxmox VE node.
- A cluster's changes are staged first, checked by the API server, and applied together after a plan that says what each replaces ([clusters.md](clusters.md#staged-changes)).
  - The applied change is then followed as a run whose page counts the machines replaced.
  - Every run has a page, with its first error pinned above its steps.
- Administration has four pages: the management cluster, the appliance, templates and certificates.
- **Go to or run…** (`Ctrl K`) finds any cluster, view or action by a few letters. Single keys do the rest ([clusters.md](clusters.md#finding-your-way)).
- The interface follows the system's dark or light mode, with Atkinson Hyperlegible Next for text and JetBrains Mono for names, versions and addresses.

### Talos

- **Talos clusters.** A ClusterClass for Talos Linux (`kubehenge-talos`), with the same variables, add-ons and checks as the others but `nodeOS`, since Talos is its own OS.
  - It has one more variable, `talosVersion`, the Talos release the nodes run.
  - Its control planes hold their address with Talos's own virtual IP. Its CNI is Talos's flannel unless the cluster chooses Cilium or Calico.
  - Templates come from the Talos Image Factory, one per release. They are built on the Administration page, from the create wizard where creators may ask for one, or with `kubehenge template create talos --talos-version`.
  - Changing `talosVersion` upgrades the nodes in place through the Talos API, one at a time. Control planes go first, each is drained first, and the upgrade moves a minor version at a time.
  - An upgrade that fails or rolls back stops the rest until **Try again** on the cluster's page.
  - With node image updates on, the nodes move to new Talos patch releases inside the maintenance window.
  - The cluster page shows the release, the upgrade under way and the last one. It downloads the talosconfig for those who may download the kubeconfig.
  - etcd snapshots are taken and restored through the Talos API. The restore wipes etcd on every control plane, recovers it on the first and lets the others join again.
  - Talos's service and kernel logs reach Loki through the node collector, under `job="talos"`.
  - Each machine's configuration is generated in the schema of the Talos release it boots first.
  - Of each Talos minor version the template refresh keeps the two newest releases' templates and any a cluster still boots or is upgraded to.
  - Not yet on Talos: the controller manager's and scheduler's metrics.
  - Talos serves them with certificates nothing can verify. Kubehenge scrapes them on the first Talos release that gives them certificates of the cluster CA.
  - The Talos bootstrap and control-plane providers come from Kubehenge's own forks of them.
  - The forks are [cluster-api-bootstrap-provider-talos](https://github.com/kubehenge/cluster-api-bootstrap-provider-talos) and [cluster-api-control-plane-provider-talos](https://github.com/kubehenge/cluster-api-control-plane-provider-talos).
  - The forks are on Cluster API's v1beta2 contract and have the control-plane template a ClusterClass needs.
  - Their files and images are pinned by digest. The management cluster's nodes pull the images only with the signature of the fork's release workflow.
  - Like every distribution's, their controllers run only while a cluster uses Talos.

### Backups and templates

- **Backups.** Velero backs up to the S3 stores an administrator sets, on a schedule, and keeps each backup for as long as the cluster chooses.
  - Each cluster's backups sit under a prefix of its own. They are encrypted with keys of its own (SSE-C for its resources, Kopia for its volumes' data).
  - `kube-system` is left out.
  - The cluster page lists the backups and restores, backs up at once and restores the whole cluster or chosen namespaces. Only those who may download its admin kubeconfig can restore.
- **Node OS refresh.** The management cluster builds the node OS VM templates again every week with their node OS's updates.
  - This is on by default, Sunday at 01:00 UTC, and is set on the Administration page or with the module's `template_refresh`.
  - The builds use an API token of its own that may build and delete templates in the template pool and nothing else. Each build is a template of its own.
  - A cluster's nodes move to a new one inside its maintenance window once it is a day old, one at a time. They do so whether automatic Kubernetes upgrades are on or not, unless the cluster switches node image updates off.
  - The two newest builds of each template stay, with any a cluster still runs.
  - The cluster page shows which build the nodes run and when they next move.
  - The Administration page shows the last run with its log and has Build now.
  - Where the administrator allows it, those who may create clusters can have a missing template built from the create wizard.
  - A new build keeps the CPU type of the template it replaces. With `--proxmox-insecure` the builds reach the API without verifying its certificate too.

### Features per cluster

- **HA group labels, per cluster.** The cloud controller manager labels each cluster's nodes with their Proxmox VE HA groups or HA node-affinity rules.
  - The controller no longer logs a permission error for them every few minutes.
  - The cluster's token gets `Sys.Audit` on `/`, unpropagated, through a group the controller adds its user to. That lets it read the Proxmox VE cluster's own configuration and change nothing.
  - A cluster switches the labels off in the wizard, under **Options** or with its `ccm` variable.
  - An administrator switches them off for every cluster with **HA group labels** in the setup or the module's `cluster_sys_audit`.
- **More namespaces for clusters.** An administrator can name namespaces beside `clusters` that the interface creates and manages clusters in, such as one per team.
  - The setting is **Namespaces for clusters** in the setup, `extra_cluster_namespaces` in the module and `interface.extraNamespaces` in the chart.
  - Off by default. When on, the wizard asks for the namespace and the cluster list shows it ([clusters.md](clusters.md#more-namespaces-for-clusters)).
- **Read-only Headlamp for everyone.** A cluster's own Headlamp add-on can show the cluster to everyone who reaches it without signing in.
  - The setting is `headlamp.access: view`, in the wizard and the Options view. It is off by default.
  - Its service account is bound to Kubernetes' `view` ClusterRole, and Headlamp uses it for every request.
  - It shows workloads, ConfigMaps, events and logs, no Secrets, and changes nothing.
- **Volume snapshots, per cluster.** A cluster on storage local to each node (ZFS, LVM, LVM-thin, directory or btrfs) may take VolumeSnapshots, restore them and clone volumes.
  - It needs `cluster_volume_snapshots` (**Volume snapshots** in the setup, with `root@pam`'s password).
  - A cluster switches it on in the wizard, under **Options** or with its `csi` variable's `volumeSnapshots`.
  - Proxmox VE copies volumes only for `root@pam`, so that cluster's CSI plugin signs in as `root@pam`. Whoever takes over the cluster holds the Proxmox VE cluster.
  - It gets the VolumeSnapshotClass `proxmox` and, on kubeadm and k3s, the snapshot controller. RKE2 uses its own.
  - Switching it on takes `Permissions.Modify` on `/`. Off by default ([clusters.md](clusters.md#volume-snapshots)).
- **Certificates.** cert-manager issues them from Let's Encrypt or any ACME server, or from a CA of the cluster's own.
  - ACME certificates are proved by DNS-01 through Cloudflare, Route 53, DigitalOcean, Azure DNS, Google Cloud DNS, RFC 2136, ACME-DNS or Akamai.
  - They can also be proved by HTTP-01 through Traefik's Gateway.
  - A CA of the cluster's own is constrained to its domain and downloadable from the interface.
  - With DNS-01 or the CA, a wildcard certificate for the cluster's domain is Traefik's default and its Gateway's HTTPS listener's. The add-ons' Ingresses ask for their own where it does not cover them.
  - The DNS provider's credential is typed into the interface, per cluster or, for clusters that opt in with `Sys.Modify`, once for every cluster. Or it stays in a Secret of the cluster's own.
- **GitOps.** Argo CD or Flux per cluster, off by default.
  - It keeps the cluster in step with a directory of a Git repository. It leaves what it deployed running when switched off or to the other engine.
  - A private repository is read with a deploy key Kubehenge makes and the interface shows. It can also be read with a stored token, or with a Secret of the owner's own.
  - Over SSH the server's host keys are read once and trusted from then on.
  - Argo CD signs in through the cluster's first identity provider with PKCE, its local admin disabled. It gives rights only to the groups named its admins.
  - Flux is installed at a pinned version by the Flux Operator, whose web interface is an opt-in.
  - Choosing the repository needs `Sys.Modify` on the pool, as the admin kubeconfig does.
- **Cluster autoscaler.** A worker pool can be autoscaled, at creation or later from the Scale dialog.
  - A pool's minimum is 0 to 100 workers and its maximum up to 100.
  - The cluster autoscaler of the cluster's Kubernetes minor adds workers while pods wait for room and removes those left unneeded. It scales up from none by the pool's machine size, with a 10% margin, and node labels.
  - It runs in the management cluster with a token into its cluster, 64 MiB requested each. Chosen under Options with `VM.Audit` on the workload pool, it runs on the cluster's own control planes instead.
  - An admission policy holds each autoscaler to its own cluster's pools and machines.
  - Control planes stay tainted while a pool may grow. Add-ons need a pool that keeps a worker.
  - The node addresses must hold every pool at its most. The Scale dialog warns when the Proxmox VE nodes lack the memory for the most.
  - A worker that finds no room shows `AutoscalerNoRoom`.
  - Switching it off fixes the pool at the workers it runs and removes everything the autoscaler was given.
- **etcd snapshots.** Kubehenge's controller takes snapshots of each cluster's etcd on a schedule, or at once from the cluster page. They go into the same S3 stores as backups.
  - Snapshots are encrypted with age to a key of the cluster's own, and checked against etcd's own checksum.
  - The cluster chooses how many are kept. There is no etcd key or store credential in the cluster.
  - The cluster page lists them, restores one, and downloads them for those with `Sys.Modify` on the cluster's pool.
  - A restore stops every control plane, restores etcd on one and replaces the others. It goes through the Proxmox VE guest agent where an administrator switched that on, and as steps to run by hand where not.
  - On RKE2 and k3s a cluster may also opt into the distribution's own upload of the snapshots its servers take. It is off by default and goes to the same target with its certificate verified.
  - That puts the target's access key into a Secret in the cluster's `kube-system` and keeps it out of the machines' bootstrap data. It stores those snapshots unencrypted for restoring by hand.

### Headlamp and management cluster

- **Headlamp sign-in.** An administrator can have the shared Headlamp open each cluster signed in as cluster-admin.
  - It is for those who hold `Sys.Modify` on its pool, the privilege its admin kubeconfig takes.
  - The setting is the setup's Headlamp sign-in, or the module's `headlamp_sign_in`. It is off by default.
  - The token is made for the session and stays in the appliance. It lasts an hour at most and never longer than the session.
  - The interface logs each token with its user and the token's ID. The cluster's audit log records the ID.
  - Switching it off removes the accounts behind it from every cluster. That ends every token made.
  - Logging out of a cluster in Headlamp drops the session's token until the cluster is opened from the interface again.
- **Headlamp's log out works in the shared Headlamp.** The gate refused the requests with which Headlamp clears a cluster's token and ends an identity provider's sign-in.
  - The gate now passes them on for the clusters the user sees.
- **Management cluster upgrades.** Every apply records the Kubehenge release that ran it.
  - Once the appliance runs a newer release, the Administration page says so, with **Upgrade the management cluster**.
  - **Upgrade the management cluster** lists the components whose versions change and applies the settings as they are with the state passphrase.
  - Off by default, the management cluster can also follow the appliance's upgrades on its own, in their hour, at most once a day. A failed run waits for the next day.
  - Switching it on has the appliance keep the state passphrase and the API token of a Proxmox VE user of its own. That user has the Administrator role, as the dialog says.
  - Switching it off, or removing the management cluster, deletes both.
- **Headlamp actions.** An administrator can have the shared Headlamp offer the Helm app catalog and the Prometheus charts it ships with.
  - The setup's Headlamp actions, or the module's `headlamp_actions`, is off by default.
  - Everyone who sees a cluster lists, installs, upgrades, rolls back and uninstalls its Helm releases. They act with their own credentials in it, or the sign-in's.
  - They chart its metrics from a Prometheus running there.
  - Charts come only from the Helm repositories set for the appliance. Only those who may set the appliance up add or remove them, over HTTPS.
  - Port forwards stay with Headlamp's desktop app.

### Node OSes

- **Node OSes instead of Ubuntu.** The nodes of RKE2, k3s and kubeadm clusters run a node OS each cluster chooses with its `nodeOS` variable, in the wizard or under **Options**.
  - The choices are AlmaLinux 10 by default, or Rocky Linux 10 (on `x86-64-v3` CPUs). Both are in image mode, a bootable container image made of the family's own signed packages with a read-only `/usr`.
  - openSUSE Leap 16.1 Immutable follows once openSUSE releases it.
  - Each holds only what a node needs (cloud-init, the QEMU guest agent, the SELinux policies, and for kubeadm containerd and the kubelet's tools).
  - Each enforces SELinux: containerd with its SELinux support, and RKE2 and k3s with their own policies.
  - On kubeadm the control plane's static pods and kube-vip run as `spc_t`, while every other pod stays confined.
  - The QEMU guest agent runs what Proxmox VE sends it unconfined, as a root shell would.
  - Changing the node OS replaces the nodes one at a time once its template is built.
  - Clusters are placed only on Proxmox VE nodes whose CPU runs their node OS. A cluster is refused when none of its nodes can.
  - RKE2 and k3s now have a template each per node OS, for their policies. `kubehenge template` takes `--node-os`.
  - Ubuntu templates are no longer made or selected. Build the new templates before upgrading, and existing clusters move to them as the ClusterClasses change.

### Fixes

- An AlmaLinux or Rocky Linux template build moves on from a mirror that serves its packages at under 100 kB/s. It goes to the next one in the mirror list.
  - Until now it stayed on the mirror down to 1000 bytes/s. A build could run past its 45 minutes on one serving 50 kB/s.
- An AlmaLinux or Rocky Linux template build tries each step that reads the family's repositories up to three times, half a minute apart.
  - A mirror caught syncing can serve a broken file, such as a `repomd.xml` that is not XML. dnf and rpm-ostree do not move to the next mirror for it, so a refresh failed.
- A k3s cluster with flannel runs kube-proxy in nftables mode.
  - In iptables mode, k3s's network policy controller could overwrite kube-proxy's rules while a node started. That node's pods then reached no Service, the API server among them, until a Service changed or an hour had passed.
- The nodes that run the add-ons are the workers, or else the control planes. They need room for the CPU and memory the add-ons' pods request.
  - Kubehenge refuses a cluster whose cores fall short, beside what the distribution's, the CNI's and its own pods request there. The interface raises the cores to it and shows the add-ons' requests on the Add-ons step.
  - Without it, a pod the scheduler found no room for stayed pending with `Insufficient cpu`, however idle the node was.
  - Each class carries the figures (`kubehenge.dev/addon-cpu`, `kubehenge.dev/node-cpu`, [clusters.md](clusters.md#machines)). Every add-on but Rancher fits on one worker of the default 2 cores on every distribution.
  - Kubehenge checks that a cluster's control planes have the memory its add-ons add to what each uses itself. The add-ons' memory is mostly their resource types and objects in the API server.
  - The check holds with workers as well as without. The interface raises the control planes to it.
  - With every add-on but Rancher an RKE2 control plane needs 4720 MiB, where 4096 MiB ran out of memory once Argo CD started. The figures are measured as each add-on is switched on ([clusters.md](clusters.md#machines)).
- A cluster's volume owner VM is created under another VM ID when the one Proxmox VE offered is taken. A VM created at the same moment can take it.
  - Before, the cluster's setup failed until its next try.
- A kubeadm cluster's first control-plane node moves kube-vip from kubeadm's super-admin credentials to `admin.conf` while it is still bootstrapping. It finishes only once the new kube-vip holds the control-plane address.
  - Until now the old kube-vip ran on for two minutes after `kubeadm init`. It then left the address unanswered for about ten seconds, under whatever used the cluster by then.
- An RKE2 or k3s server whose etcd member is removed, as its control plane is replaced, stays down until its VM is deleted.
  - Until now it started again, joined etcd as a new member, and went with its VM as a voter etcd still counted. A cluster with one server then lost its quorum when the control plane was replaced under load.
  - RKE2 and k3s control planes are replaced to get it.
- A control plane's API server runs with a Go memory limit of 40% of the control plane's memory. On k3s the limit covers the whole server process and is half the control plane's memory.
  - Without it, the API server's memory sat at about twice its live heap. A 4096 MiB kubeadm control plane ran out of memory when every add-on was switched off at once.
  - Control planes are replaced to get it.

## [0.2.0] - 2026-10-22

### Clusters

- **The Gateway API** in every cluster, so that Gateways and HTTPRoutes work beside Ingresses.
  - It brings the standard channel's CRDs, with the admission policy upstream ships to refuse older or experimental ones over them.
  - It brings Traefik's Gateway `traefik-gateway` in `kube-system`. The Gateway listens for HTTP where Traefik does and takes HTTPRoutes from every namespace.
  - RKE2's and k3s's CRDs stay theirs while they ship them. Kubehenge brings its own release where they do not, never an older one over a newer.
  - Istio's waypoints are Gateways of the class `istio-waypoint`.
- **Leader leases through stalls.** The control planes' controller managers and schedulers keep their leader leases through a stall of up to 40 seconds rather than restart.
  - So do the cloud controller managers, CSI controller and Cilium operator Kubehenge installs.
  - A restart would, on k3s, take its etcd member down with them while a single control plane is replaced.
- **The health page reads a stopped container's log** from its newest run that has ended.
  - The kubelet always keeps that log.
  - Before, it sometimes showed an error where a crash-looping pod's log should be.
- **The CSI plugin's newest release, per cluster.** With `cluster_datastore_allocate` (**CSI plugin** in the setup), a cluster may choose the Proxmox VE CSI plugin's newest release.
  - A cluster chooses it in the wizard, under **Options** or with its `csi` variable.
  - Its token then gets `Datastore.Allocate` on the cluster's storage. This applies only on a storage the management cluster, the VM templates and other clusters do not use.
  - Other clusters keep the release that needs no `Datastore.Allocate`.
- **HA rules for cluster creators.** An administrator can have Proxmox VE's HA node-affinity rules offered as placements to everyone who may create or change a cluster.
  - Those people need no `Sys.Audit` on `/` of their own.
  - The setting is the setup's HA rules for cluster creators, or the module's `offer_ha_rules`. It is off by default.
  - Kubehenge's controller reads the rules with its own token. That token holds the privilege already.
  - The interface hands out each rule's name and the nodes it allows, nothing more.

### Installer

- **The Proxmox VE API's address.** The installer reaches the API only at a name or address of the Proxmox VE cluster's own nodes that its certificate is valid for.
  - It never uses another host the certificate also names.
  - It prefers, in this order:
    - a name for several nodes
    - this node's names
    - its addresses
    - a name without a domain
    - the other nodes' names and addresses
  - A wildcard counts for this node's own name.
  - Where several are as good, it asks on the terminal. Where none is left, it stops and says what the certificate names.
- **The Proxmox VE API's CA.** Without `--proxmox-ca`, a certificate the cluster's CA did not sign is checked against the CA in the node's trust store it rests on.
  - The appliance and the clusters are given that CA alone.
  - Failing that, the certificate is checked against the CA the API serves, once the administrator has said yes to its fingerprint on the terminal.
  - `--proxmox-ca` takes a bundle such as a whole trust store, reduced to the CA the certificate rests on. It also takes a CA that is not self-signed.
- **`--proxmox-insecure`**, for the installer and `--upgrade`, makes these reach the API without verifying its certificate:
  - the appliance
  - OpenTofu's and Cluster API's Proxmox VE providers
  - Kubehenge's controller
  - the clusters' cloud controller manager and CSI plugin
  - The Administration page shows it. `--upgrade --proxmox-ca` turns verification back on.
- The installer's check of the API's certificate trusts only the CA file given, not the node's default trust store besides. The same holds for its check of whether it made the appliance's own certificate.

## [0.1.0] - 2026-10-22

The first release.

### Appliance and management cluster

- **Appliance.** One script on a Proxmox VE 9 node installs a small unprivileged container running Kubehenge's web interface.
  - Users sign in with their Proxmox VE accounts, any realm, and act with their own Proxmox VE permissions.
  - The appliance upgrades itself from a button, or automatically within a major version.
  - It installs only releases signed with the project's release key. It unpacks each where none of its entries can write outside the release's own directory.
  - Its certificate is signed by the Proxmox VE cluster's CA, or is one of your own. A DNS name can be added and removed again.
  - It is renewed from the Proxmox VE shell, and the interface warns before it runs out.
  - A Proxmox VE API whose certificate comes from another CA works too, with that CA given to the installer.
- **Management cluster.** Set up from the interface (or with the OpenTofu module in `management-cluster/`).
  - It is a Talos Linux cluster of one or three VMs, spread over the Proxmox VE nodes chosen. It runs Cluster API, the Proxmox VE provider, the kubeadm, RKE2 and k3s providers, IP address management and the Helm add-on provider.
  - The Proxmox VE provider runs as Kubehenge's own reproducible, signed build of an upstream commit after its v0.9.1 release. That commit carries the fix kubeadm upgrades need.
  - The build stays until a release of the provider carries the fix too.
  - A distribution's controllers run only while a cluster uses it.
  - The controllers hold their leader leases through API stalls of up to 40 seconds instead of restarting. Such stalls happen when Proxmox VE clones a disk on the same storage.
  - Every controller has a memory request, so none is the first a node short of memory stops.

### Workload clusters

- **Workload clusters** from three ClusterClasses, kubeadm, RKE2 and k3s: create, scale, upgrade by one minor version at a time, replace failed nodes, delete.
  - Each cluster gets:
    - the Proxmox VE cloud controller manager
    - the Proxmox CSI plugin
    - a control-plane virtual IP with kube-vip
    - a CNI (the distribution's own, or Cilium or Calico chosen at creation)
    - Traefik as the ingress controller on every node (RKE2's and k3s's own, and Kubehenge installs it on kubeadm)
  - Services of type `LoadBalancer` get addresses of their own from a range on the nodes' network, with MetalLB (recommended) or kube-vip, chosen per cluster. They are refused where they clash with the nodes', the API address or another cluster's.
  - Pod and service ranges, OpenID Connect sign-in and automatic upgrades within windows are set per cluster.
  - While a cluster has workers, its control-plane nodes are tainted in every distribution and run only what has to run there. A cluster without workers runs its workloads on them.
  - Control planes are sized by the distributions' server sizing guides, from 2 cores and 4096 MiB.
  - A cluster without workers adds a worker's minimum to its control planes. A cluster whose control planes are too small for its workers is refused.
  - The nodes that run the add-ons, the workers or else the control planes, need room for the add-ons chosen. The room is each add-on's measured memory, and the interface raises the machines to it.
- **Health page.** Each cluster's page says whether it is healthy, changing, degraded or unreachable, and what needs attention first:
  - nodes not ready or under pressure
  - pods that crash-loop, cannot pull their image or cannot be scheduled
  - volume claims still waiting
  - add-ons not deployed
  - failing readiness checks
  - worker pools short of machines and health checks holding back
  - It also shows each VM's CPU over the last hour, the last hour's warnings and the cluster's conditions.
  - It reads inside the cluster with read-only identities of its own, whose short-lived tokens the controller renews.
  - Pods' logs need `Sys.Modify` on the cluster's pool and come from an identity that reads logs only.
- **Placement.** A cluster's VMs spread over every online Proxmox VE node, or over the nodes chosen by hand.
  - The nodes can also be chosen from one of Proxmox VE's HA node-affinity rules. The choice can change later.
  - A node that goes down is left out within about 15 seconds, so the VMs replacing those it ran go elsewhere.
  - The cluster page shows where each VM runs and warns when control-plane VMs share a node.
  - Where the administrator switches on Proxmox VE HA, it keeps every cluster's control-plane VMs on separate nodes. It restarts them elsewhere after a node fails, for clusters on shared storage that do not opt out.

### Add-ons and security

- **Isolation between clusters.** Each cluster has its own Proxmox VE pool, user and API token with rights on its own VMs only.
  - Each cluster has a VM that owns its persistent volumes, so a compromised cluster cannot reach another's VMs or volumes.
  - Only the cloud controller manager and the CSI plugin's controller hold the token, on control-plane nodes only.
- **Add-ons.** Headlamp, per cluster or shared in the appliance behind its sign-in.
  - Rancher, installed into a Kubehenge cluster, with the other clusters registered in it automatically.
  - Monitoring with kube-prometheus-stack, scraping every node and control-plane component over verified, authenticated TLS.
  - kubeadm clusters' kubelets serve certificates from the cluster CA. Kubehenge's controller approves them and the API server verifies them.
  - Logs beside monitoring: Loki on a volume of its own, kept 7 days in 10 GiB unless set otherwise.
  - Loki sits behind a kube-rbac-proxy that lets in only requests with credentials RBAC permits. Grafana Alloy needs that permission to push, and monitoring's Grafana to read.
  - Alloy collects on every node (container logs, short-lived pods' too, the journal and the API server's audit log). Without access to the host, it collects through the Kubernetes API.
  - Pod security is a Pod Security Standard level enforced by Pod Security Admission, by Kyverno, or both. The namespaces of the components that need the host are exempt.
  - CIS hardening is chosen at creation, beyond the baseline of the CIS Kubernetes Benchmark every cluster gets. It is checked with kube-bench in the end-to-end tests.
  - Istio in ambient mode gives mutual TLS between the pods of the namespaces enrolled, without sidecars. Switching it off takes them out of the mesh first.
  - Cilium Cluster Mesh works on kubeadm, RKE2 and k3s. Clusters of one namespace that run Cilium and list each other reach each other's pods and global services.
  - Every new Cilium cluster gets a pod range of its own, and peers come and go without restarting anything.
- **Security baseline.** Every cluster:
  - audits its API server
  - encrypts Secrets in etcd
  - refuses requests without credentials beyond its health checks (and, on kubeadm, what `kubeadm join` reads)
  - limits the rate of events
  - keeps its controller manager and scheduler on loopback
  - offers only strong TLS ciphers
  - runs its kubelets on the kernel settings they expect
- **Proxmox VE integration.** An optional entry in the Proxmox VE interface leading to the appliance (`appliance/pve-entry/`), added if the administrator says yes when installing.
- **Pinned by digest.** Every image, chart, provider file and tool Kubehenge deploys or builds with is named by its digest beside its version.
  - Where the bytes are not stable or carry no digest, a signature is required instead:
    - the Talos Image Factory's images and everything the management cluster pulls from registry.k8s.io
    - the control-plane images of kubeadm templates
    - Ubuntu's cloud images and archive
  - RKE2 and k3s sign neither releases nor images. They install from files checked against the checksums recorded when a cluster first uses the release.
