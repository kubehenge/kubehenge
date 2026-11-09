# Changelog

Versions follow [Semantic Versioning](https://semver.org/). Before 1.0.0 a minor version may change behaviour. Changes not yet released are in `docs/changes/`, one file per pull request, and appear here under `[Unreleased]` on the website. Each release lists its highlights first, then its changes by kind.

## [Unreleased]

## [0.4.2] - 2026-11-05

### Highlights

- A new cluster's workers get 6144 MiB by default, enough for most add-ons together.
- The installer asks which storage takes the appliance's disks and which its template, and never uses a disabled one.
- Downloads from GitHub are retried for about four minutes, and OpenTofu tries each provider download up to eight times.
- Activity no longer misses a kubectl change made just before an API server's log rotates or the API server restarts.

### Changed

- The management cluster's nodes default to a CPU weight of 200 (`node_cpu_units`), twice a default VM's, down from 1000.
- A change of `node_cpu_units` reaches running nodes without a reboot.
- A new cluster's workers get 6144 MiB by default, up from 4096 MiB, and the minimums are unchanged.
- 6144 MiB fits all add-ons together except Rancher, logs, backups and GitOps, with about 2 GiB to spare.
- Where several storages fit, the installer asks which takes the appliance's disks and which the template (`--storage`, `--template-storage`).
- The installer never uses a disabled storage, such as `local` on some nodes.
- The installer downloads the container template itself and checks it against a SHA-512 it carries, without `pveam` or its indexes.
- A container template already on a storage is used where it is.
- The installer shows a progress bar for each download and a line for each finished step, including each verified checksum.
- Only the installer's own downloads go through the datacenter's HTTP proxy, where one is set.
- Downloads from GitHub, including the installer's and the template builds', are retried for about four minutes.
- The appliance's OpenTofu tries each provider download up to eight times, up from two.
- The interface follows each API server log as it is written, and after a restart reads the old container's last lines.
- A registry that refuses a Talos installer image still fails the upgrade at once.
- The docs warn that a volume snapshot copies a volume its pods may still write to, so stop or pause the workload first.

### Fixed

- Activity no longer misses a kubectl change made just before an API server's log rotates or the API server restarts.
- A Talos upgrade tries a node's installer pull again when the registry drops the connection, rather than failing.

### Security

- The installer prints the datacenter's proxy without its credentials.

## [0.4.1] - 2026-11-05

### Before you upgrade

- Pod Security Admission's default level moves from `privileged` to `baseline`.
- A cluster that sets `podSecurity.admission` keeps its level.
- A cluster that never set it moves to `baseline`, which replaces its control-plane machines.
- Set `privileged` on such a cluster first to keep pods that need more.
- Velero and RKE2's or k3s's own etcd upload wait until a cluster has its own backup access key.
- Store a backup access key for each cluster in its backups ([clusters.md](clusters.md#back-up-and-restore)).
- The module's `cluster_volume_snapshots` is gone, so remove it from the module's variables.
- The management-cluster module needs the Proxmox VE provider 0.120, so run `tofu init -upgrade`.
- OpenID sign-in needs `--public-url`, which the appliance sets.
- Only `kubehenge template`, `controller`, `serve` and `csi-proxy` take `--proxmox-url`, `--proxmox-ca` and `--proxmox-insecure`.
- Remove those flags from scripts that pass them to other commands.
- The API server refuses Clusters that break the interface's rules (listed under Changed), so check the Clusters kept in Git first.
- Fixed pools need one node address more than their machines.
- A kubeadm control plane counts 3376 MiB for itself, up from 2392 MiB.
- A kubeadm or Talos cluster's workers count 200 millicores together for CoreDNS's two replicas, which can share one worker.
- A single worker of one core then holds fewer add-ons ([clusters.md](clusters.md#machines)).
- A control plane with add-ons counts more memory, up to 1808 MiB, when its cluster runs a CNI other than the class's default.
- Changes to a cluster already short of memory go through only when they leave it no shorter ([clusters.md](clusters.md#machines)).
- A new kubeadm cluster's control planes get 4800 MiB by default, up from 4096 MiB, which a cluster with Calico needs.
- Existing clusters keep their control-plane size.
- Boot logs need the kubeadm, RKE2 and k3s templates built again.
- `install.sh --upgrade` adds the volume that sealed automatic-upgrade secrets need.
- Applying the management cluster replaces the nodes of Talos clusters, which get a new log target.
- The apply also replaces kubeadm, RKE2 and k3s control planes, which get a CPU request for kube-vip among other changes ([upgrading.md](upgrading.md#when-a-release-changes-a-clusterclass)).

### Highlights

- 0.4.1 is the first release of 0.4, as the v0.4.0 tag was never published.
- The interface gains a demo mode, Activity, notifications and a reworked fleet, Machines view and create wizard.
- `kubehenge cluster` and API tokens drive clusters from scripts and CI, and support bundles collect what a fault needs.
- Stalled control-plane rolls, stuck deletions and expiring certificates are found and recovered.
- Each cluster gets its own backup key, and volume snapshots need no `root@pam` password.

### Added

- `kubehenge serve --demo` serves the interface over seven sample clusters, with a **Demo** mark and every change refused.
- The docs' screenshots come from the demo ([testing.md](testing.md#demo-mode)).
- **Activity** lists who did what, newest first, in Administration and on each cluster's page.
- Activity includes the management cluster's events of the last day, and `kubehenge appliance activity` lists it too.
- The setting **kubectl changes** (`api_audit_log`) adds the changes people make with kubectl, from the audit log.
- **Machines** lists the control plane and the worker pools with their sizes.
- **Machines** adds, resizes and deletes pools and resizes the control plane, staged with the other changes.
- **Delete** on a worker drains it and shrinks its pool by one.
- **Cordon** and **Uncordon** act through an identity that an admission policy limits to a node's cordon.
- **Reboot** needs `VM.PowerMgmt`.
- **Console** needs `VM.Console` and opens the VM's console in Proxmox VE.
- **Ctrl K** goes to any cluster or page and starts a cluster's actions, which also open from their address.
- Shortcuts that leave a page take two keys in turn, such as `G` `C` for the clusters, so a stray key leaves no page.
- The theme can be set to light, dark or the system's.
- The cluster can be copied as YAML.
- Each identity provider has its own sign-in button.
- A finished backup's or restore's run page shows the end of its Velero log.
- **Notifications** go to a webhook, an ntfy topic or email ([install.md](install.md#notifications)).
- Notifications tell a cluster problem that stands 5 minutes and its end, a failed run and the management cluster's own trouble.
- **Administration, Notifications** sets the targets and sends a test, as `kubehenge appliance notifications` does in the shell.
- A cluster with the monitoring add-on lists its firing alerts on its Health page.
- The alerts are read through a Kubehenge identity that may only read Alertmanager.
- `kubehenge cluster` lists, creates, scales and deletes clusters, prints a kubeconfig and acts on machines through the interface.
- `kubehenge cluster` signs in with a Proxmox VE API token from `KUBEHENGE_PROXMOX_TOKEN`.
- The interface acts on an `Authorization: PVEAPIToken=…` header with the token's privileges ([From the command line](clusters.md#from-the-command-line)).
- Scripts and CI need no password for the interface.
- Two conditions say what holds a Cluster up, in kubectl as in the interface, shown above the cluster's views.
- The fleet counts a held-up cluster among those that need you.
- `KubehengeReady` carries the error Kubehenge's controller last met there, with a Warning event for each.
- `MachinesStalled` says when a machine is not ready and none has become ready for 20 minutes.
- `MachinesStalled` carries the worst its Machines and ProxmoxMachines report.
- A change or creation whose machines are stuck shows under **Stalled** in the runs.
- The controller counts settled and stalled rollouts in `kubehenge_machine_rollouts_total`.
- Rollout times are in `kubehenge_machine_rollout_duration_seconds`.
- Every answer of the interface carries a request ID, shown as an error's reference and on every journal line of the request.
- The appliance answers `ok` at `/healthz` without a sign-in.
- The interface and the controller take `--log-level` and `--log-development`.
- The appliance sets the log options with `KUBEHENGE_LOG_LEVEL`, and the chart with `controllerLogs`.
- The controller serves its metrics over HTTPS to accounts holding `kubehenge-metrics-reader`.
- The metrics include how often Kubehenge's Cluster API hooks held a cluster back or refused it ([troubleshooting.md](troubleshooting.md#where-to-look)).
- `kubehenge diagnose <namespace>/<cluster>` and **Support bundle** on the cluster's page write one `.tar.gz`.
- **Support bundle** is there for those who may download the cluster's kubeconfig.
- The bundle holds the Cluster and every Cluster API object of it, their events and its machines' boot logs.
- The bundle holds the log lines that name its objects, from the controller, capmox, the Add-on Provider for Helm and the interface.
- The bundle holds no Secret, and tokens and passwords are replaced by `[REDACTED]`.
- **Boot log** in the Machines view, or `kubehenge.dev/boot-log-request`, has the controller read what a node logged as it booted.
- A boot log goes into the ConfigMap `<cluster>-boot-logs` ([troubleshooting.md](troubleshooting.md#a-nodes-boot-log)).
- The controller's token holds `VM.GuestAgent.FileRead` on the workload pool for boot logs, which reads files only.
- Boot logs are on by default.
- `node_boot_logs = false`, or **Nodes' boot logs** in the setup, takes the privilege away ([install-settings.md](install-settings.md#nodes-boot-logs)).
- A backup can be deleted before it runs out, once its name is typed (a Velero `DeleteBackupRequest`).
- A restore can pick its namespaces, its kinds of resources and whether the volumes' data comes back.
- A finished restore lists Velero's errors and warnings by namespace.
- A cluster can read the backups of others in its namespace on the same target (`backups.readFrom`), to restore them.
- A cluster created reading another's restores that cluster's volumes' data too.
- The health page and the fleet say when the last backup failed or backups are overdue ([Back up and restore](clusters.md#back-up-and-restore)).
- A change that replaces a cluster's only control plane is warned of in the plan and by kubectl.
- A new control-plane machine not ready for 20 minutes sets `ControlPlaneRollStalled`, with why and whether the API answers.
- The stall time is `watch.stall_minutes` in the module and `watch.stallMinutes` in the chart.
- A stalled roll also gets a warning event, an item on the Health page and **Recover…** on the cluster's page.
- Recover, or `kubehenge.dev/etcd-restore` naming a `machine`, keeps that control-plane machine and replaces the others.
- Recover resets etcd to that machine's own database when the API is down ([troubleshooting.md](troubleshooting.md#one-control-plane-its-roll-stalled)).
- While a cluster is being deleted, its page and `kubehenge.dev/deletion-status` list every object holding the deletion and why.
- A deletion that waited 20 minutes sets `DeletionStalled` with a warning event, listed in the fleet and on the Health page.
- **Abandon…**, or `kubehenge.dev/abandon-deletion` set to the cluster's name, deletes what is left, its machines without draining.
- Whatever has not finished five minutes later loses its finalizers.
- Each thing left behind in Proxmox VE is named in a `LeftBehind` event ([troubleshooting.md](troubleshooting.md#a-cluster-does-not-finish-deleting)).
- kubeadm's control plane is replaced 60 days before its certificates run out.
- For RKE2 and k3s, the controller rolls the workers' pool and k3s's control plane from 60 days before, one machine at a time.
- The controller rolls an RKE2 control plane of three or more too, and a smaller one says to restart `rke2-server`.
- The Cluster gets `CertificatesExpiring` with events, on its Health page and in the fleet, until a minute after the last replacement.
- The kubeconfig and talosconfig kept for each cluster are watched too ([troubleshooting.md](troubleshooting.md#certificates)).
- The interface warns administrators 30 days before the management cluster's admin credentials run out, and an apply renews them.
- A failed template build is tried again, three times in all, 2 and then 8 minutes apart, from the interface and weekly build alike.
- A failure that would repeat, such as a node whose CPU is too old, is not tried again.
- A weekly build that still fails records `TemplateBuildFailed` on the ConfigMap `kubehenge-template-refresh` ([troubleshooting.md](troubleshooting.md#template-build-fails)).
- The controller checks each cluster's VMs and storage every 15 seconds, and the Health page and fleet show problems.
- A VM stopped, paused by hand, paused by QEMU after a disk error, or crashed for 3 minutes sets `VMsNotRunning`.
- A storage 85% full on a node the cluster runs on sets `StorageNearlyFull`, an error from 95%.
- Both conditions record events ([troubleshooting.md](troubleshooting.md#vms-that-do-not-run-storage-nearly-full)).
- A held Kubernetes upgrade sets `UpgradeHeld`, with the reason `RancherKubernetes`, `RancherElsewhere` or `TalosUpgrade`.
- The fleet and the Health page show **Upgrade held for Rancher** or **Upgrade waits for Talos**.
- `kubehenge cluster list` shows the reason in STATUS ([troubleshooting.md](troubleshooting.md#upgrades-held-back)).
- **Administration, Backups** snapshots the management cluster's etcd on a schedule into a backup target, encrypted.
- The management snapshots go through the nodes' Talos API with a talosconfig that may only take etcd snapshots.
- The page lists the management snapshots in the target.
- **Restore…** wipes etcd on the control-plane nodes and restores one with the state passphrase, also while the API is down.
- The management etcd steps are in [install.md](install.md#back-up-the-management-clusters-etcd).
- The updater keeps the installed release until the new interface answers on the appliance's certificate within 2 minutes.
- The new release must also keep running for 20 seconds, or the updater puts the old one back and restarts it.
- The Administration page shows such an upgrade as rolled back, with why ([troubleshooting.md](troubleshooting.md#appliance-upgrades)).
- `install.sh --upgrade` checks and rolls back the same way.
- `kubehenge appliance` in the appliance's shell shows the upgrades and how the last went, asks for one and checks the interface.
- `kubehenge appliance` sets up, lists, takes and restores the management cluster's etcd backups.
- It acts with the Administration page's privileges and refusals ([upgrading.md](upgrading.md#from-the-appliances-shell)).

### Changed

- Pages load the session, setup status and fleet together.
- A change to a cluster or machine shows at once in the fleet, the runs and the cluster's page.
- The fleet list at `/clusters` takes the whole page and can be filtered, grouped and sorted.
- With no clusters yet, the fleet shows whether a VM template is built and whether you may create clusters.
- Each cluster has one health verdict, the same wherever it shows.
- A cluster's page opens at once with what the fleet list last read.
- A cluster just created or deleted shows so in the list and the rail at once.
- A view seen before shows its last reading while it reads it again, and a first read shows a placeholder in its shape.
- A tab with something to look at carries a badge, and a strip above the cluster's views shows the worst problem.
- A reading that could not be renewed is greyed out as stale.
- While the clusters cannot be read, the list counts down to its next try.
- A failed run starts again from its page with what it was started with, and asks again for a passphrase or typed name.
- An appliance upgrade shows its steps.
- An etcd snapshot, an etcd restore and a change show their events since they began.
- Machines on an offline Proxmox VE node show as down, and those whose kubelet stopped reporting as not reporting.
- Nodes of a cluster that does not answer show as not read.
- The top bar and fleet list fit phones, where fields stack.
- A cluster's options are grouped, with on-or-off settings switched in their rows.
- **Scale** sends only what changed.
- **Upgrade** offers the newest version it may move to and checks it first.
- A machine is replaced from its row.
- The create wizard starts from the newest working cluster's settings, or from another's or the defaults.
- The create wizard takes the network from the bridge, suggests free addresses and marks typed ones in use.
- The create wizard shows each add-on as a card with its memory.
- The wizard's **Confirm** has the API server check the cluster and weigh its memory and disk against what is free.
- The management cluster is set up in steps from what the nodes report, with its health shown and its removal in a danger zone.
- A read that hangs is given up after 30 seconds.
- What needs attention leads to where it is acted on, the machine's row in **Machines** or **Backups**.
- A failed run of the appliance links to the page it is started from.
- The API server refuses a new pool whose name cannot be part of its nodes' host names.
- A mistyped `kubehenge` subcommand fails and names the closest one.
- A command given the wrong arguments says which it takes, such as `give the cluster's name`.
- `kubehenge cluster list -o` checks its format before it calls the interface.
- `kubehenge cluster list` and `create` refuse `-n`.
- `kubehenge appliance` takes `--json`, `--user`, `--password-file` and `--totp` after its subcommand.
- A Proxmox VE task that fails is reported with its UPID and the last lines of its log, not its exit status alone.
- The interface's account in the management cluster may read every Cluster API object in its namespaces.
- The interface's account may also read the logs of the controller, capmox and the Add-on Provider for Helm.
- Volume snapshots need no `root@pam` password and no `Permissions.Modify` on `/`.
- Every cluster on storage local to each node may take snapshots, with `VM.Clone` on its pool for it.
- A proxy beside each cluster's CSI plugin makes the copies with the cluster's own token, into a holder VM's disk in its pool.
- A copy stays on its volume's node, and the proxy copies only the cluster's own volumes, one at a time, and logs each.
- A snapshot of a large volume turns ready once its copy is whole.
- A Cluster applied with kubectl or GitOps is held to the interface's rules by the API server ([clusters.md](clusters.md)).
- Those rules are a name of at most 40 characters, a version in the distribution's form and 1, 3, 5 or 7 control-plane nodes.
- The rules allow upgrades of one minor version at most and never back, and at most 100 workers in each pool.
- Node addresses and the control-plane endpoint must be on the gateway's network, with the endpoint outside the node addresses.
- The controller gives a new cluster, from kubectl or GitOps too, the pod and service ranges it does not name.
- A cluster gets its distribution's own ranges where they are clear of the nodes' network, and other ones where not.
- A cluster that runs Cilium gets ranges clear of every other cluster's.
- The API server refuses pod or service ranges that overlap the nodes' network or each other, or are not IPv4.
- Kubehenge applies its Gateway API admission policy only beside CRDs it holds itself, on kubeadm and Talos.
- Elsewhere Kubehenge deletes the copy of that policy it applied before.
- The management cluster keeps answering on a busy host, with a CPU weight of 1000 for its nodes (`node_cpu_units`).
- Cluster API's providers allow 5 s for each health probe and six missed liveness probes before a restart.
- Kubehenge's Cluster webhook reads the ClusterClass from its cache.
- The Add-on Provider for Helm keeps each pinned chart once downloaded, instead of parsing the whole repository index each time.
- That index had taken over 100 MiB of memory for a large repository.
- An abandoned deletion ends once no ProxmoxMachine or ProxmoxCluster of the cluster is left, not after five minutes.
- The five minutes still apply while a VM could be left behind.
- Kyverno's admission controller runs two replicas with a disruption budget, and its add-on figures rise to 640 MiB and 500 millicores.
- A kubeadm control plane's own 3376 MiB is its measured use plus 25%.
- With every add-on but Rancher, a kubeadm control plane needs 4976 MiB.
- Every add-on's pods request CPU and memory, as do RKE2's and k3s's Traefik, CNI and snapshot controller and kube-vip.
- The add-ons' CPU figures count the new requests, so a 2-core worker no longer takes every add-on on RKE2 and Talos.
- kubeadm and k3s clusters with Calico no longer run Goldmane and Whisker, Calico's unused flow-log aggregator and viewer.
- Goldmane and Whisker took CPU on small nodes, and RKE2's own Calico already left them out.
- capmox and the Add-on Provider for Helm are built with ko from `scripts/ko.sh`, so their image digests match on any host.
- capmox moves to its v0.9.2 release, which carries all but two of the fixes Kubehenge built on v0.9.1.
- Kubehenge's capmox build adds only those two, so a failed clone forgets its VM ID and a VM is confirmed before it is destroyed.

### Fixed

- Activity no longer misses kubectl changes behind a long stretch of other requests in the API server's log.
- Activity reads each log to its end and warns when the kubelet rotated lines away unread.
- An etcd restore or reset of a CIS-hardened RKE2 cluster takes about 2 minutes, not 17.
- Applying the management-cluster module no longer returns while the operator reinstalls core Cluster API after upgrading it.
- The providers' release waits until the providers have stayed unchanged for 30 seconds.
- A template build or a cluster's storage set-up no longer fails when Proxmox VE kills a VM create before it starts.
- Such a build goes on under the next VM ID and logs how to remove the VM left locked.
- `kubehenge template create` tries a failed build again after 2 and 8 minutes, as the interface and the refresh do.
- A template build asks a publisher for a checksum, signature, key or Talos schematic up to four times, 2, 4 and 8 seconds apart.
- One dropped connection or 5xx answer no longer fails a template build.
- Deleting a template build checks that its VM ID still holds that template, as Proxmox VE reuses freed IDs.
- An add-on whose publisher moves a version's tag to a new build still installs and upgrades.
- The Add-on Provider for Helm no longer refuses a pinned chart whose tag moved, which had stopped the newest CSI plugin.
- Rancher installs the Turtles, webhook and Fleet charts Kubehenge names, from the chart repository in its pinned image.
- With Rancher 2.15.3 these are Turtles 0.27.4, the webhook 0.11.4, Fleet 0.16.3 and shell 0.8.3.
- A k3s or RKE2 upgrade whose Gateway API chart carries an older release than Kubehenge's installs that chart.
- Kyverno's controllers hold their leader leases through a stall of up to 50 seconds, like the other add-ons, not 10.
- A lone Kyverno admission controller no longer shuts down on an etcd pause and refuses every pod.
- The management-cluster module no longer asks the Image Factory for the Talos image's size, which held up a Proxmox VE API worker.
- The Talos image download no longer waits for the module's other changes.
- Kubehenge's admission checks every cluster's variables, where Cluster API admitted the first of each distribution unchecked.
- A later change of the first cluster's CNI or CIS hardening no longer escapes the rules.
- Where no default was stored, the admission uses the one the cluster runs on.
- When the installer removes a container whose interface failed to start or reach the API, it prints the interface's log first.
- The installer's check of the new appliance verifies its certificate instead of skipping it.
- `install.sh --help` lists every option.
- `kubehenge template create` and `template refresh` refuse an empty `--node` before calling Proxmox VE.
- The controller refuses a Pod Security exemption list with anything after its closing brace.
- Namespaces after such a brace are no longer silently held to the cluster's level.
- A template refresh releases its Lease when it ends, also when its last renewal was still under way.
- The next template refresh no longer waits for the Lease to run out.
- After an etcd restore, a Talos cluster's nodes reach the API server and every other Service as before.
- A Talos restore moves etcd's revision on, as a kubeadm, RKE2 or k3s restore does, and so do the by-hand steps with `etcdutl`.
- An RKE2 or k3s etcd restore moves the revision on by a billion and compacts the older ones, as on kubeadm.
- Clients watching from before an RKE2 or k3s restore no longer miss changes.
- The by-hand RKE2 and k3s restore steps do the same with `etcdutl` and `openssl`.
- Talos clusters with Cilium send their services' and kernel's logs to Loki.
- Talos sends those logs to the node collector on a link-local address, not loopback.
- When the interface stops, as on an appliance upgrade, it lets requests under way finish and stops Headlamp and token upkeep first.
- The interface makes one session store, whatever starts first, so the upkeep no longer prunes an unused store.
- RKE2 clusters with Cilium start on AlmaLinux and Rocky Linux nodes.
- The templates label `/opt/cni/bin` for containers, so SELinux lets RKE2's Cilium install its portmap plugin there.
- A template build checks the label on `/opt/cni/bin`.
- k3s and kubeadm clusters with Calico start, as Calico leaves out its FlexVolume driver.
- The driver's install into the read-only `/usr` had kept Calico's node agent from starting.
- A kubeadm cluster with Calico, applied with kubectl, gets a pod range and so runs Calico.
- Rolling a Calico cluster's nodes no longer stalls.
- The Tigera operator and Calico's Typha tolerate only control-plane and not-ready taints, in RKE2's own Calico too.
- Host ports, Traefik's among them, work on RKE2 and kubeadm clusters with Calico or Canal on AlmaLinux, Rocky Linux and openSUSE Leap.
- RKE2 and kubeadm templates carry the `iptables` the CNI's portmap plugin runs on nftables, and a template build checks it.
- Calico no longer reports itself degraded on the AlmaLinux, Rocky Linux and openSUSE Leap templates.
- The templates label `/var/lib/calico` for containers to read, so the Tigera operator reads the MTU Calico found.
- A template build checks the label on `/var/lib/calico`.
- On 4096 MiB with Calico and every add-on, a kubeadm API server no longer hits its memory limit and stalls.
- AlmaLinux and Rocky Linux nodes keep pressure stall information, which their kernels leave off, for the kubelet's pressure metrics.
- A fixed pool with no spare node address no longer waits for ever in its rollout, as it needs one like autoscaled pools.
- Two clusters on the same bridge and VLAN can no longer be given the same node addresses or control-plane endpoint.
- With `proxmox_insecure`, etcd restores through the guest agent skip the check of Proxmox VE's certificate too.
- A Talos node whose machine is deleted while it is upgraded in place, as on a scale-down, has its upgrade dropped.
- Such an install no longer fails after 15 minutes and holds every Talos upgrade of the cluster until **Try again**.
- A Talos install that stalls for three minutes or loses the node is tried up to four times, 1, 2 and 4 minutes apart.
- An etcd restore that succeeded is no longer recorded as failed with "the controller restarted during the restore".
- Conditions, and the certificate watch's roll of a second worker pool, no longer fail on the controller's own write just before them.
- The management cluster's VMs get random VM IDs between 10000 and 99999, as Proxmox VE offers simultaneous creates the same ID.
- The Terraform/OpenTofu provider no longer takes over another VM, such as a template being built, and destroys it with the node.
- A machine whose clone fails is cloned again under a new VM ID, where capmox kept an ID another VM may have taken.
- Such a machine no longer waits on another VM for ever and deletes it with itself.
- Deleting a machine destroys the VM under its ID only once the VM's name and SMBIOS UUID confirm it as the machine's.
- A free ID, or one held by another VM or a container, counts as the VM gone.
- capmox no longer destroys the next clone given a freed ID, possibly another cluster's machine.
- An AlmaLinux or Rocky Linux template build moves on from a mirror serving a broken `repomd.xml`, as one does while it syncs.
- Prometheus's operator, MetalLB's speakers and RKE2's Traefik no longer fail liveness probes on a busy node and restart.
- k3s clusters run without k3s's local path provisioner, whose StorageClass also claimed to be the default.
- A claim without a class no longer ends up on one node's disk.

### Security

- Deleting a cluster or restoring its etcd takes the cluster's name in the request, as the dialogs already ask for it.
- A change made against a cluster someone else has changed since is refused (412) instead of undoing theirs.
- After such a refusal the page drops what was staged and shows the cluster as it is now.
- The interface's credential in the management cluster is a token that runs out after 30 days, renewed daily.
- That token replaces one that never expired.
- Too many failed sign-ins refuse further tries for a while, by that user from that address and from that address overall.
- Failed sign-ins from elsewhere cannot lock an administrator out.
- Headlamp's port takes changes and WebSockets only from its own pages.
- Both ports send `Strict-Transport-Security`.
- A certificate the installer renews keeps only DNS names the cluster's CA signed or this node's DNS resolves to the appliance.
- The installer takes a static address from the container's configuration rather than from inside it.
- The updater keeps one capability and runs under a system-call filter, with memory that cannot be made executable.
- The updater hides other users' processes, and the interface runs under the same system-call filter.
- The state passphrase and token that automatic management upgrades keep are sealed, with the key on a 1 GiB volume backups leave out.
- An appliance restored from a backup asks for automatic upgrades to be switched off and on again.
- Appliances read the release index from `https://kubehenge.dev/releases/index.json`, signed again every week from the latest release.
- Appliances refuse an index signed more than two weeks ago, or one that carries no expiry.
- The release key can be rotated ([releasing.md](releasing.md#rotating-the-release-key)).
- Pod Security Admission warns about and audits pods short of `restricted` at every level.
- Choosing `privileged` in the interface writes it into the cluster's variables.
- Kyverno exempts only the namespaces of the add-ons and the CNI a cluster runs, RKE2's own Calico among them.
- The controller's Runtime Extension hooks read the Cluster from the API server, not from the request.
- The delete hook acts only on a Cluster being deleted.
- The validating webhook fails closed.
- The controller may not write ServiceAccount token Secrets.
- An etcd restore first takes a snapshot of etcd as it is, and shows it on the restore.
- An etcd restore refuses a snapshot without a well-formed SHA-256.
- The management-cluster module installs the Cluster API Operator from a chart archive checked against its SHA-256.
- The release carries that archive, and a checkout runs `make capi-operator-chart`.
- The controller no longer holds the tokens that `proxmox_ha` and `etcd_restore` give it.
- Each sits in a Secret of its own (`kubehenge-proxmox-ha`, `kubehenge-proxmox-restore`, key `token`).
- Only a short-lived Job mounts each token, one per HA write or restore step ([architecture.md](architecture.md#tokens-kept-out-of-the-controller)).
- The HA Job writes and replaces only HA resources and rules that carry Kubehenge's comment.
- HA reads use the controller's own token.
- kube-network-policies enforces network policies in the management cluster, beside Talos's Flannel.
- Nothing reaches a pod in `kubehenge-system` or `kubehenge-autoscaler` but the controller's webhook, lifecycle hooks and metrics.
- The HA and restore Jobs reach only DNS and Proxmox VE's API ([architecture.md](architecture.md#management-cluster)).
- The access key stored for every cluster stays in the management cluster, for Kubehenge's own etcd snapshots only.
- Each cluster gets a backup key of its own, where a shared key let one cluster's admins delete the others' backups.

## [0.3.1] - 2026-10-28

### Highlights

- A machine whose VM needs no configuration change is started, where it was reported ready with its VM off.
- A transient error while attaching the bootstrap ISO is retried instead of failing the machine.

### Changed

- capmox, which creates the clusters' VMs, is built from a later commit of its main branch.
- The installer names the address of a download that still fails after its retries, where curl gives only the HTTP status.

### Fixed

- A machine whose VM needs no configuration change is started instead of being reported ready while its VM stays off.
- A transient error while attaching the bootstrap ISO is retried instead of failing the machine for good.

## [0.3.0] - 2026-10-27

### Before you upgrade

- Build the AlmaLinux 10 or Rocky Linux 10 templates first, as Ubuntu templates are no longer made or selected.
- Clusters move to the new templates as the ClusterClasses change.
- Every cluster's control planes are replaced, for the API server's memory limit.
- RKE2 and k3s control planes are replaced for the etcd member fix too.

### Highlights

- Talos clusters join kubeadm, RKE2 and k3s, with templates from the Talos Image Factory.
- RKE2, k3s and kubeadm nodes run AlmaLinux 10 or Rocky Linux 10 in image mode, in place of Ubuntu.
- Velero backups, etcd snapshots, GitOps, certificates and the cluster autoscaler come per cluster.
- The interface has a fleet rail, staged changes with a plan, and a page for every run.
- The management cluster rebuilds node OS templates weekly and can follow the appliance's upgrades.

### Added

- The fleet runs down the left of every page, grouped into those that need you, those with work running and the healthy.
- Each fleet entry says why, from what the cluster's health page finds.
- The work under way runs down the right, snapshots, backups, restores, template builds and appliance upgrades included.
- A failed run shows its first error in that list.
- A cluster's views are tabs under its name, and its summary opens on its machines laid out by Proxmox VE node.
- A cluster's changes are staged, checked by the API server, and applied together after a plan of what each replaces ([clusters.md](clusters.md#staged-changes)).
- An applied change is followed as a run whose page counts the machines replaced.
- Every run has a page, with its first error pinned above its steps.
- Administration has four pages, the management cluster, the appliance, templates and certificates.
- **Go to or run…** (`Ctrl K`) finds any cluster, view or action by a few letters ([clusters.md](clusters.md#finding-your-way)).
- Single keys do the rest of the navigation.
- The interface follows the system's dark or light mode.
- Text is set in Atkinson Hyperlegible Next, and names, versions and addresses in JetBrains Mono.
- A ClusterClass for Talos Linux (`kubehenge-talos`) has the others' variables, add-ons and checks, but no `nodeOS`.
- Its one extra variable, `talosVersion`, is the Talos release the nodes run.
- Talos control planes hold their address with Talos's own virtual IP.
- A Talos cluster's CNI is Talos's flannel unless the cluster chooses Cilium or Calico.
- Talos templates come from the Talos Image Factory, one per release.
- Talos templates are built on the Administration page, from the create wizard or with `kubehenge template create talos --talos-version`.
- Changing `talosVersion` upgrades the nodes in place through the Talos API, one at a time, control planes first.
- Each node is drained first, and the upgrade moves a minor version at a time.
- An upgrade that fails or rolls back stops the rest until **Try again** on the cluster's page.
- With node image updates on, Talos nodes move to new patch releases inside the maintenance window.
- The cluster page shows the Talos release, the upgrade under way and the last one.
- The cluster page downloads the talosconfig for those who may download the kubeconfig.
- Talos etcd snapshots are taken and restored through the Talos API.
- A Talos restore wipes etcd on every control plane, recovers it on the first and lets the others join again.
- Talos's service and kernel logs reach Loki through the node collector, under `job="talos"`.
- Each machine's configuration is generated in the schema of the Talos release it boots first.
- The template refresh keeps the two newest Talos releases' templates of each minor, and any a cluster still uses.
- The controller manager's and scheduler's metrics are not scraped on Talos yet, as Talos serves them with unverifiable certificates.
- Kubehenge scrapes them on the first Talos release that gives them certificates of the cluster CA.
- The Talos bootstrap and control-plane providers come from Kubehenge's own forks on Cluster API's v1beta2 contract.
- The forks are [cluster-api-bootstrap-provider-talos](https://github.com/kubehenge/cluster-api-bootstrap-provider-talos) and [cluster-api-control-plane-provider-talos](https://github.com/kubehenge/cluster-api-control-plane-provider-talos).
- The forks have the control-plane template a ClusterClass needs.
- The Talos controllers run only while a cluster uses Talos.
- Velero backs up to the S3 stores an administrator sets, on a schedule, and keeps each backup as long as the cluster chooses.
- Each cluster's backups sit under a prefix of its own.
- `kube-system` is left out of backups.
- The cluster page lists backups and restores, backs up at once and restores the whole cluster or chosen namespaces.
- Only those who may download a cluster's admin kubeconfig can restore it.
- The management cluster builds the node OS VM templates again every week with their node OS's updates.
- The weekly build is on by default, Sunday at 01:00 UTC, set on the Administration page or with the module's `template_refresh`.
- The builds use an API token of their own that may build and delete templates in the template pool and nothing else.
- Each build is a template of its own.
- A cluster's nodes move to a new build inside the maintenance window once it is a day old, one at a time.
- Nodes move whether automatic Kubernetes upgrades are on or not, unless the cluster switches node image updates off.
- The two newest builds of each template stay, with any a cluster still runs.
- The cluster page shows which build the nodes run and when they next move.
- The Administration page shows the last run with its log and has Build now.
- Where the administrator allows it, cluster creators can have a missing template built from the create wizard.
- A new build keeps the CPU type of the template it replaces.
- With `--proxmox-insecure` the builds reach the API without verifying its certificate too.
- The cloud controller manager labels each cluster's nodes with their Proxmox VE HA groups or HA node-affinity rules.
- The cluster's token gets `Sys.Audit` on `/`, unpropagated, through a group the controller adds its user to.
- That privilege reads the Proxmox VE cluster's own configuration and changes nothing.
- A cluster switches HA group labels off in the wizard, under **Options** or with its `ccm` variable.
- An administrator switches them off for every cluster with **HA group labels** in the setup or the module's `cluster_sys_audit`.
- An administrator can name namespaces beside `clusters` for the interface to manage clusters in, such as one per team.
- The setting is **Namespaces for clusters** in the setup, `extra_cluster_namespaces` in the module and `interface.extraNamespaces` in the chart.
- With more namespaces, off by default, the wizard asks for the namespace and the list shows it ([clusters.md](clusters.md#more-namespaces-for-clusters)).
- A cluster's own Headlamp can show the cluster read-only to everyone who reaches it without signing in.
- Read-only Headlamp is off by default and set with `headlamp.access: view` in the wizard and the Options view.
- Its service account is bound to Kubernetes' `view` ClusterRole, and Headlamp uses it for every request.
- Read-only Headlamp shows workloads, ConfigMaps, events and logs, no Secrets, and changes nothing.
- A cluster on node-local storage (ZFS, LVM, LVM-thin, directory or btrfs) may take VolumeSnapshots, restore them and clone volumes.
- Volume snapshots need `cluster_volume_snapshots` (**Volume snapshots** in the setup, with `root@pam`'s password).
- A cluster switches volume snapshots on in the wizard, under **Options** or with its `csi` variable's `volumeSnapshots`.
- That cluster's CSI plugin signs in as `root@pam`, as Proxmox VE copies volumes only for it.
- Whoever takes over such a cluster holds the Proxmox VE cluster.
- The cluster gets the VolumeSnapshotClass `proxmox` and, on kubeadm and k3s, the snapshot controller, while RKE2 uses its own.
- Switching volume snapshots on takes `Permissions.Modify` on `/`, and it is off by default ([clusters.md](clusters.md#volume-snapshots)).
- cert-manager issues certificates from Let's Encrypt or any ACME server, or from a CA of the cluster's own.
- DNS-01 works through Cloudflare, Route 53, DigitalOcean, Azure DNS, Google Cloud DNS, RFC 2136, ACME-DNS and Akamai.
- ACME certificates can also be proved by HTTP-01 through Traefik's Gateway.
- A CA of the cluster's own is constrained to its domain and downloadable from the interface.
- With DNS-01 or the CA, a wildcard certificate for the cluster's domain is Traefik's default and its Gateway's HTTPS listener's.
- The add-ons' Ingresses ask for their own certificates where the wildcard does not cover them.
- The DNS provider's credential is typed into the interface per cluster, or once for every cluster that opts in with `Sys.Modify`.
- The credential can also stay in a Secret of the cluster's own.
- Argo CD or Flux, off by default, keeps the cluster in step with a directory of a Git repository.
- GitOps leaves what it deployed running when switched off or to the other engine.
- A private repository is read with a deploy key Kubehenge makes and the interface shows, a stored token, or the owner's own Secret.
- Over SSH the server's host keys are read once and trusted from then on.
- Argo CD signs in through the cluster's first identity provider with PKCE, its local admin disabled.
- Argo CD gives rights only to the groups named its admins.
- Flux is installed at a pinned version by the Flux Operator, whose web interface is an opt-in.
- Choosing the repository needs `Sys.Modify` on the pool, as the admin kubeconfig does.
- A worker pool can be autoscaled, at creation or later from the Scale dialog.
- An autoscaled pool's minimum is 0 to 100 workers and its maximum up to 100.
- The cluster autoscaler of the cluster's Kubernetes minor adds workers while pods wait and removes those left unneeded.
- The autoscaler scales a pool up from none by its machine size, with a 10% margin, and its node labels.
- The autoscaler runs in the management cluster with a token into its cluster and a request of 64 MiB.
- Chosen under Options with `VM.Audit` on the workload pool, the autoscaler runs on the cluster's own control planes instead.
- An admission policy holds each autoscaler to its own cluster's pools and machines.
- Control planes stay tainted while a pool may grow, and add-ons need a pool that keeps a worker.
- The node addresses must hold every pool at its most.
- The Scale dialog warns when the Proxmox VE nodes lack the memory for every pool at its most.
- A worker that finds no room shows `AutoscalerNoRoom`.
- Switching the autoscaler off fixes the pool at the workers it runs and removes everything the autoscaler was given.
- The controller takes etcd snapshots of each cluster on a schedule, or at once from the cluster page, into the backup S3 stores.
- The cluster chooses how many snapshots are kept, and no etcd key or store credential is in the cluster.
- The cluster page lists snapshots, restores one, and downloads them for those with `Sys.Modify` on the cluster's pool.
- A restore stops every control plane, restores etcd on one and replaces the others.
- A restore goes through the Proxmox VE guest agent where an administrator switched that on, and as steps by hand where not.
- On RKE2 and k3s a cluster may opt into the distribution's own upload of its servers' snapshots, to the same target.
- The distribution's upload is off by default and verifies the target's certificate.
- That upload puts the target's access key into a Secret in the cluster's `kube-system`, kept out of the bootstrap data.
- The distribution's upload stores its snapshots unencrypted, for restoring by hand.
- An administrator can have the shared Headlamp open each cluster signed in as cluster-admin.
- Headlamp sign-in is for those who hold `Sys.Modify` on the cluster's pool, the privilege its admin kubeconfig takes.
- Headlamp sign-in is off by default, set with the setup's Headlamp sign-in or the module's `headlamp_sign_in`.
- The sign-in's token is made for the session, stays in the appliance and lasts an hour at most, never past the session.
- The interface logs each token with its user and ID, and the cluster's audit log records the ID.
- Switching Headlamp sign-in off removes its accounts from every cluster, which ends every token made.
- Logging out of a cluster in Headlamp drops the session's token until the cluster is opened from the interface again.
- Every apply records the Kubehenge release that ran it.
- Once the appliance runs a newer release, the Administration page says so.
- **Upgrade the management cluster** lists the components whose versions change and applies the settings with the state passphrase.
- The management cluster can follow the appliance's upgrades on its own, off by default, in their hour and once a day at most.
- A failed automatic run waits for the next day.
- Automatic management upgrades keep the state passphrase and a token of a Proxmox VE user with the Administrator role.
- Switching automatic upgrades off, or removing the management cluster, deletes both.
- An administrator can have the shared Headlamp offer the Helm app catalog and the Prometheus charts it ships with.
- Headlamp actions are off by default, set in the setup or with the module's `headlamp_actions`.
- Everyone who sees a cluster lists, installs, upgrades, rolls back and uninstalls its Helm releases.
- They act with their own credentials in the cluster, or the sign-in's.
- They chart its metrics from a Prometheus running there.
- Charts come only from the appliance's Helm repositories, which only those who may set the appliance up add or remove, over HTTPS.
- Port forwards stay with Headlamp's desktop app.

### Changed

- RKE2, k3s and kubeadm nodes run a node OS each cluster chooses with `nodeOS`, in the wizard or under **Options**.
- The node OS is AlmaLinux 10 by default, or Rocky Linux 10 on `x86-64-v3` CPUs.
- Both run in image mode, a bootable container image of the family's own signed packages with a read-only `/usr`.
- openSUSE Leap 16.1 Immutable follows once openSUSE releases it.
- Each node OS holds only what a node needs, cloud-init, the QEMU guest agent, SELinux policies and kubeadm's containerd and tools.
- Each node OS enforces SELinux, with containerd's SELinux support and RKE2's and k3s's own policies.
- On kubeadm the control plane's static pods and kube-vip run as `spc_t`, while every other pod stays confined.
- The QEMU guest agent runs what Proxmox VE sends it unconfined, as a root shell would.
- Changing the node OS replaces the nodes one at a time once its template is built.
- Clusters are placed only on Proxmox VE nodes whose CPU runs their node OS, and are refused when none can.
- RKE2 and k3s have a template each per node OS, for their policies, and `kubehenge template` takes `--node-os`.
- The nodes that run the add-ons need room for the CPU and memory their pods request.
- Kubehenge refuses a cluster whose cores fall short of what the distribution's, the CNI's and its own pods request.
- The interface raises the cores to it and shows the add-ons' requests on the Add-ons step.
- Each class carries the figures (`kubehenge.dev/addon-cpu`, `kubehenge.dev/node-cpu`, [clusters.md](clusters.md#machines)).
- Every add-on but Rancher fits on one worker of the default 2 cores on every distribution.
- Kubehenge checks that a cluster's control planes have the memory its add-ons add to what each uses itself.
- The add-ons' memory is mostly their resource types and objects in the API server.
- The control-plane memory check holds with workers as well as without, and the interface raises the control planes to it.
- With every add-on but Rancher an RKE2 control plane needs 4720 MiB, where 4096 MiB ran out once Argo CD started.
- The figures are measured as each add-on is switched on ([clusters.md](clusters.md#machines)).
- A control plane's API server runs with a Go memory limit of 40% of the control plane's memory.
- On k3s the memory limit covers the whole server process and is half the control plane's memory.
- A k3s cluster with flannel runs kube-proxy in nftables mode.

### Fixed

- Headlamp's log out works in the shared Headlamp, as the gate passes on the requests that clear a token and end a sign-in.
- The controller no longer logs a permission error for HA group labels every few minutes.
- An AlmaLinux or Rocky Linux template build leaves a mirror serving under 100 kB/s for the next in the list.
- A build no longer runs past its 45 minutes on a mirror serving 50 kB/s, where it stayed down to 1000 bytes/s.
- An AlmaLinux or Rocky Linux build tries each step that reads the family's repositories up to three times, half a minute apart.
- A mirror caught syncing with a broken file, such as a `repomd.xml` that is not XML, no longer fails a refresh.
- A starting k3s node's pods no longer lose every Service, the API server included, to the network policy controller.
- A pod no longer stays pending with `Insufficient cpu` on an idle node.
- A volume owner VM is created under another VM ID when a simultaneous create took the one Proxmox VE offered.
- A cluster's setup no longer fails until its next try on such a clash.
- A kubeadm cluster's first control plane moves kube-vip from super-admin credentials to `admin.conf` while still bootstrapping.
- The bootstrap finishes only once the new kube-vip holds the control-plane address.
- The control-plane address no longer goes unanswered for about ten seconds two minutes after `kubeadm init`.
- An RKE2 or k3s server whose etcd member is removed, as its control plane is replaced, stays down until its VM is deleted.
- A one-server cluster no longer loses its quorum when its control plane is replaced under load.
- A 4096 MiB kubeadm control plane no longer runs out of memory when every add-on is switched off at once.

### Removed

- Ubuntu templates are no longer made or selected.

### Security

- Backups are encrypted with the cluster's own keys, SSE-C for its resources and Kopia for its volumes' data.
- Etcd snapshots are encrypted with age to a key of the cluster's own, and checked against etcd's own checksum.
- The forks' files and images are pinned by digest.
- The management cluster's nodes pull the fork images only with the signature of the fork's release workflow.

## [0.2.0] - 2026-10-22

### Highlights

- Every cluster gets the Gateway API, with Traefik's Gateway `traefik-gateway`, beside Ingresses.
- Control-plane components keep their leader leases through a stall of up to 40 seconds rather than restart.
- A cluster may choose the Proxmox VE CSI plugin's newest release.
- The installer reaches the Proxmox VE API only at a name its certificate covers, and finds the API's CA by itself.

### Added

- Every cluster has the Gateway API, so Gateways and HTTPRoutes work beside Ingresses.
- The Gateway API brings the standard channel's CRDs, with upstream's admission policy that refuses older or experimental ones over them.
- Traefik's Gateway `traefik-gateway` in `kube-system` listens for HTTP where Traefik does and takes HTTPRoutes from every namespace.
- RKE2's and k3s's Gateway API CRDs stay theirs while they ship them, and Kubehenge brings its own release where they do not.
- Kubehenge never installs an older Gateway API release over a newer one.
- Istio's waypoints are Gateways of the class `istio-waypoint`.
- With `cluster_datastore_allocate` (**CSI plugin** in the setup), a cluster may choose the Proxmox VE CSI plugin's newest release.
- A cluster chooses the CSI plugin's release in the wizard, under **Options** or with its `csi` variable.
- The newest CSI plugin's token gets `Datastore.Allocate`, only on a storage no other cluster, template or management VM uses.
- Other clusters keep the CSI plugin release that needs no `Datastore.Allocate`.
- An administrator can offer Proxmox VE's HA node-affinity rules as placements to everyone who may create or change a cluster.
- Cluster creators then need no `Sys.Audit` on `/` of their own.
- HA rules for cluster creators are off by default, and set in the setup or with the module's `offer_ha_rules`.
- The controller reads the HA rules with its own token, and the interface hands out only each rule's name and nodes.
- `--proxmox-insecure`, for the installer and `--upgrade`, reaches the API without verifying its certificate.
- `--proxmox-insecure` covers the appliance, OpenTofu's and Cluster API's Proxmox VE providers and Kubehenge's controller.
- `--proxmox-insecure` also covers the clusters' cloud controller manager and CSI plugin.
- The Administration page shows `--proxmox-insecure`, and `--upgrade --proxmox-ca` turns verification back on.

### Changed

- The control planes' controller managers and schedulers keep their leader leases through a stall of up to 40 seconds rather than restart.
- The cloud controller managers, CSI controller and Cilium operator Kubehenge installs keep their leases too.
- The installer reaches the Proxmox VE API only at a name or address of the cluster's own nodes that its certificate is valid for.
- The installer never uses another host the certificate also names.
- The installer prefers a name for several nodes, then this node's names, its addresses, a bare name, then other nodes' names and addresses.
- A wildcard counts for this node's own name.
- Where several API names are as good, the installer asks on the terminal.
- Where no API name is left, the installer stops and says what the certificate names.
- Without `--proxmox-ca`, a certificate the cluster's CA did not sign is checked against the node trust store's CA it rests on.
- The appliance and the clusters are given that CA alone.
- Failing that, the installer checks the certificate against the CA the API serves, once its fingerprint is accepted on the terminal.
- `--proxmox-ca` takes a bundle such as a whole trust store, reduced to the CA the certificate rests on.
- `--proxmox-ca` also takes a CA that is not self-signed.

### Fixed

- The health page reads a stopped container's log from its newest ended run, which the kubelet always keeps.
- A crash-looping pod's log no longer shows an error in its place.
- On k3s, replacing a single control plane no longer takes its etcd member down with restarting controllers.

### Security

- The installer's certificate checks trust only the CA file given, not the node's default trust store besides.
- That holds for the API's certificate and for whether the installer made the appliance's own certificate.

## [0.1.0] - 2026-10-22

### Highlights

- One script on a Proxmox VE 9 node installs the appliance, a small container with Kubehenge's web interface.
- Users sign in with their Proxmox VE accounts and act with their own Proxmox VE permissions.
- The interface sets up a Talos management cluster that runs Cluster API and its providers.
- Workload clusters on kubeadm, RKE2 and k3s are created, scaled, upgraded, repaired and deleted.
- Each cluster has its own Proxmox VE pool, user and token, and add-ons from Headlamp to Istio.

### Added

- One script on a Proxmox VE 9 node installs a small unprivileged container running Kubehenge's web interface.
- Users sign in with their Proxmox VE accounts, any realm, and act with their own Proxmox VE permissions.
- The appliance upgrades itself from a button, or automatically within a major version.
- The appliance's certificate is signed by the Proxmox VE cluster's CA, or is one of your own.
- A DNS name can be added to the appliance's certificate and removed again.
- The certificate is renewed from the Proxmox VE shell, and the interface warns before it runs out.
- A Proxmox VE API whose certificate comes from another CA works too, with that CA given to the installer.
- The management cluster is set up from the interface, or with the OpenTofu module in `management-cluster/`.
- The management cluster is a Talos Linux cluster of one or three VMs, spread over the Proxmox VE nodes chosen.
- It runs Cluster API, the Proxmox VE, kubeadm, RKE2 and k3s providers, IP address management and the Helm add-on provider.
- The Proxmox VE provider is Kubehenge's own reproducible, signed build of an upstream commit after v0.9.1.
- That commit carries the fix kubeadm upgrades need, and the build stays until a provider release carries it too.
- A distribution's controllers run only while a cluster uses it.
- The controllers hold their leader leases through API stalls of up to 40 seconds instead of restarting.
- Such stalls happen when Proxmox VE clones a disk on the same storage.
- Every controller has a memory request, so none is the first a node short of memory stops.
- Workload clusters come from three ClusterClasses, kubeadm, RKE2 and k3s.
- Clusters are created, scaled, upgraded one minor version at a time, repaired when nodes fail, and deleted.
- Each cluster gets the Proxmox VE cloud controller manager, the Proxmox CSI plugin and a control-plane virtual IP with kube-vip.
- Each cluster gets a CNI, the distribution's own, or Cilium or Calico chosen at creation.
- Traefik is the ingress controller on every node, RKE2's and k3s's own, and installed by Kubehenge on kubeadm.
- `LoadBalancer` Services get addresses from a range on the nodes' network, with MetalLB (recommended) or kube-vip per cluster.
- A load-balancer range is refused where it clashes with the nodes', the API address or another cluster's.
- Pod and service ranges, OpenID Connect sign-in and automatic upgrades within windows are set per cluster.
- While a cluster has workers, its control-plane nodes are tainted and run only what has to run there.
- A cluster without workers runs its workloads on its control planes, which add a worker's minimum.
- Control planes are sized by the distributions' server sizing guides, from 2 cores and 4096 MiB.
- A cluster whose control planes are too small for its workers is refused.
- The nodes that run the add-ons need room for each add-on's measured memory, and the interface raises the machines to it.
- Each cluster's Health page says whether it is healthy, changing, degraded or unreachable, and what needs attention first.
- Health lists nodes not ready or under pressure, and pods that crash-loop, cannot pull their image or cannot be scheduled.
- Health lists waiting volume claims, add-ons not deployed, failing readiness checks and worker pools short of machines.
- Health shows health checks holding back, each VM's CPU over the last hour, the last hour's warnings and the cluster's conditions.
- Health reads inside the cluster with read-only identities of its own, whose short-lived tokens the controller renews.
- Pods' logs need `Sys.Modify` on the cluster's pool and come from an identity that reads logs only.
- A cluster's VMs spread over every online Proxmox VE node, or over the nodes chosen by hand.
- The nodes can also come from one of Proxmox VE's HA node-affinity rules, and the choice can change later.
- A node that goes down is left out within about 15 seconds, so replacement VMs go elsewhere.
- The cluster page shows where each VM runs and warns when control-plane VMs share a node.
- Where the administrator switches on Proxmox VE HA, it keeps every cluster's control-plane VMs on separate nodes.
- HA restarts them elsewhere after a node fails, for clusters on shared storage that do not opt out.
- Headlamp runs per cluster, or shared in the appliance behind its sign-in.
- Rancher installs into a Kubehenge cluster, with the other clusters registered in it automatically.
- Monitoring with kube-prometheus-stack scrapes every node and control-plane component over verified, authenticated TLS.
- kubeadm clusters' kubelets serve certificates from the cluster CA, which the controller approves and the API server verifies.
- Logs run beside monitoring, with Loki on a volume of its own, kept 7 days in 10 GiB unless set otherwise.
- Loki sits behind a kube-rbac-proxy that lets in only requests with credentials RBAC permits.
- Grafana Alloy needs that permission to push, and monitoring's Grafana to read.
- Alloy collects container logs, short-lived pods' too, the journal and the API server's audit log on every node.
- Without access to the host, Alloy collects through the Kubernetes API.
- Pod security is a Pod Security Standard level enforced by Pod Security Admission, by Kyverno, or both.
- The namespaces of the components that need the host are exempt from pod security.
- CIS hardening is chosen at creation, beyond the CIS Kubernetes Benchmark baseline every cluster gets.
- Istio in ambient mode gives mutual TLS between the pods of enrolled namespaces, without sidecars.
- Switching Istio off takes the namespaces out of the mesh first.
- Cilium Cluster Mesh works on kubeadm, RKE2 and k3s.
- Clusters of one namespace that run Cilium and list each other reach each other's pods and global services.
- Every new Cilium cluster gets a pod range of its own, and peers come and go without restarting anything.
- An optional entry in the Proxmox VE interface leads to the appliance (`appliance/pve-entry/`), if the administrator agrees at install.

### Security

- The appliance installs only releases signed with the project's release key.
- The appliance unpacks each release where none of its entries can write outside the release's own directory.
- Each cluster has its own Proxmox VE pool, user and API token with rights on its own VMs only.
- Each cluster has a VM that owns its persistent volumes, so a compromised cluster cannot reach another's VMs or volumes.
- Only the cloud controller manager and the CSI plugin's controller hold the token, on control-plane nodes only.
- Every cluster audits its API server, encrypts Secrets in etcd, limits the rate of events and offers only strong TLS ciphers.
- Every cluster refuses requests without credentials beyond its health checks and, on kubeadm, what `kubeadm join` reads.
- Every cluster keeps its controller manager and scheduler on loopback and runs its kubelets on the kernel settings they expect.
- Every image, chart, provider file and tool Kubehenge deploys or builds with is named by its digest beside its version.
- Where bytes are not stable or carry no digest, a signature is required instead.
- Signatures cover the Talos Image Factory's images, the management cluster's registry.k8s.io pulls and kubeadm's control-plane images.
- Signatures also cover Ubuntu's cloud images and archive.
- RKE2 and k3s sign neither releases nor images, so they install from files checked against checksums recorded at first use.
