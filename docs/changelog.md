# Changelog

Versions follow [Semantic Versioning](https://semver.org/). Before 1.0.0 a minor version may change behaviour. Each release says first what to do before upgrading, then gives its highlights and its changes by kind. Changes not yet released show under `[Unreleased]` on the website.

## [Unreleased]

### Changed

- The installer is a full-screen guide on a terminal (`kubehenge install`, started by `install.sh`). It asks DHCP or static first, then only what is ambiguous, shows a summary to change, and runs a live step list. Its commands' output goes to `/var/log/kubehenge-install.log`.
- `install.sh` only downloads and checks the release. Without a terminal, a node whose own network is static needs `--ip` (`--ip dhcp` to keep the old default).
- The unit tests of the installer's shell functions are Go tests of the new installer.

### Fixed

- A container that gets no DHCP address is no longer deleted without a hint. The guide offers a static address and retries, and an unattended run names `--ip <address>/<prefix> --gateway <address>`.

## [0.4.2] - 2026-11-05

### Highlights

- New clusters' workers get 6 GiB, room for most add-ons.
- The installer lets you pick its storages and skips disabled ones.
- Activity shows every kubectl change, also across an API server restart.

### Changed

- The management cluster's nodes default to a CPU weight of 200 (`node_cpu_units`), twice a default VM's, down from 1000. A change reaches running nodes without a reboot.
- A new cluster's workers get 6144 MiB by default, up from 4096 MiB. That fits every add-on but Rancher, logs, backups and GitOps.
- Where several storages fit, the installer asks which takes the appliance's disks and which the template (`--storage`, `--template-storage`). It never uses a disabled storage, such as `local` on some nodes.
- The installer downloads the container template itself and checks its SHA-512, without `pveam`. It uses a template already on a storage where it is.
- The installer sends only its downloads through the datacenter's HTTP proxy, no longer its calls to the Proxmox VE API.
- Installs, template builds and management cluster runs no longer fail when GitHub release downloads fail for a few minutes.
- Stop or pause a workload before its volume snapshot, as the snapshot copies a volume its pods may still write to ([clusters.md](clusters.md#volume-snapshots)).

### Fixed

- Activity no longer misses a kubectl change made just before an API server's log rotates or the API server restarts.
- A Talos upgrade no longer fails when the registry drops the connection while a node pulls its installer.

### Security

- The installer prints the datacenter's proxy without its credentials.

## [0.4.1] - 2026-11-05

### Before you upgrade

- The Pod Security Admission default is `baseline`, not `privileged`. Clusters that never set `podSecurity.admission` get new control planes, so set `privileged` first where pods need it.
- Velero backups and RKE2's or k3s's own etcd uploads stop until each cluster has its own backup access key. Store one for each cluster ([clusters.md](clusters.md#back-up-and-restore)).
- Remove `cluster_volume_snapshots` from the management-cluster module's variables, as the setting is gone.
- The management-cluster module needs the Proxmox VE provider 0.120, so run `tofu init -upgrade`.
- OpenID sign-in needs `kubehenge serve --public-url`. The appliance sets it, so only an interface run by hand needs a change.
- Only `kubehenge template`, `controller`, `serve` and `csi-proxy` take `--proxmox-url`, `--proxmox-ca` and `--proxmox-insecure`. Remove those flags from scripts that pass them to other commands.
- The API server refuses Clusters that break the interface's rules (listed under Changed), so check the Clusters kept in Git first.
- Fixed pools need one node address more than their machines.
- The memory check reserves 3376 MiB for a kubeadm control plane's own use, up from 2392 MiB.
- Kubeadm and Talos workers reserve 200 millicores for CoreDNS, so a single one-core worker holds fewer add-ons ([clusters.md](clusters.md#machines)).
- A control plane with add-ons counts up to 1808 MiB more memory when its cluster runs a CNI other than the class's default ([clusters.md](clusters.md#machines)).
- Changes to a cluster already short of memory go through only when they leave it no shorter.
- Boot logs need the kubeadm, RKE2 and k3s templates built again.
- Upgrade with `install.sh --upgrade`, which adds the volume where automatic upgrades of the management cluster keep their sealed secrets.
- Applying the management cluster replaces Talos nodes, for a new log target. It replaces kubeadm, RKE2 and k3s control planes too, for a kube-vip CPU request and more ([upgrading.md](upgrading.md#when-a-release-changes-a-clusterclass)).
- Upgrade from 0.3.1 straight to 0.4.1, as 0.4.0 was never published.

### Highlights

- Activity shows who changed what, kubectl changes included.
- Notifications by webhook, ntfy or email when a cluster needs you.
- Drive clusters from scripts and CI with `kubehenge cluster` and an API token.
- Stuck clusters say what holds them and how to recover.
- The management cluster's etcd is backed up on a schedule, encrypted.

### Added

- `kubehenge serve --demo` serves the interface over seven sample clusters, marked **Demo**, and refuses every change.
- **Activity** lists who did what, newest first, in Administration and on each cluster's page. It includes the management cluster's events of the last day, and `kubehenge appliance activity` lists it too.
- The setting **kubectl changes** (`api_audit_log`) adds the changes people make with kubectl, from the audit log.
- **Machines** lists the control plane and the worker pools with their sizes. It adds, resizes and deletes pools and resizes the control plane, staged with the other changes.
- **Delete** on a worker drains it and shrinks its pool by one.
- **Cordon** and **Uncordon** act on a node through an identity that may change only its cordon.
- **Reboot** restarts a machine's VM, with `VM.PowerMgmt`. **Console** opens its console in Proxmox VE, with `VM.Console`.
- The theme can be set to light, dark or the system's.
- **Copy as YAML** on the create wizard's check step copies the cluster for kubectl or a Git repository.
- Each identity provider has its own sign-in button.
- A finished backup's or restore's run page shows the end of its Velero log.
- **Notifications** go to a webhook, an ntfy topic or email ([install.md](install.md#notifications)). They report a cluster problem lasting 5 minutes and its end, a failed run and management cluster problems.
- **Administration, Notifications** sets the targets and sends a test, as `kubehenge appliance notifications` does in the shell.
- A cluster with the monitoring add-on lists its firing alerts on its Health page.
- `kubehenge cluster` lists, creates, scales and deletes clusters, prints a kubeconfig and acts on machines through the interface.
- The interface takes a Proxmox VE API token from `KUBEHENGE_PROXMOX_TOKEN` or an `Authorization: PVEAPIToken=…` header, with the token's privileges. Scripts and CI need no password ([From the command line](clusters.md#from-the-command-line)).
- `KubehengeReady` and `MachinesStalled` say what holds a Cluster up, in kubectl as in the interface. A held-up cluster shows so above its views and counts among those that need you.
- `KubehengeReady` carries the error Kubehenge's controller last met there, with a Warning event for each.
- `MachinesStalled` says when a machine is not ready and none has become ready for 20 minutes, with the worst its Machines and ProxmoxMachines report.
- A change or creation whose machines are stuck shows under **Stalled** in the runs.
- The controller counts settled and stalled rollouts in `kubehenge_machine_rollouts_total` and times them in `kubehenge_machine_rollout_duration_seconds`.
- Every answer of the interface carries a request ID, shown as an error's reference and on every journal line of the request.
- The appliance answers `ok` at `/healthz` without a sign-in.
- The interface and the controller take `--log-level` and `--log-development`, set by `KUBEHENGE_LOG_LEVEL` on the appliance and `controllerLogs` in the chart.
- The controller serves its metrics over HTTPS to accounts holding `kubehenge-metrics-reader`. They include how often Kubehenge's Cluster API hooks held a cluster back or refused it ([troubleshooting.md](troubleshooting.md#where-to-look)).
- `kubehenge diagnose <namespace>/<cluster>` and **Support bundle** on the cluster's page write one `.tar.gz`. **Support bundle** is there for those who may download the cluster's kubeconfig.
- The bundle holds the Cluster and every Cluster API object of it, their events, its machines' boot logs and the log lines that name them.
- Its log lines come from the controller, capmox, the Add-on Provider for Helm and the interface. It holds no Secret, and tokens and passwords become `[REDACTED]`.
- **Boot log** in the Machines view, or `kubehenge.dev/boot-log-request`, saves a node's boot log in the ConfigMap `<cluster>-boot-logs` ([troubleshooting.md](troubleshooting.md#a-nodes-boot-log)).
- Boot logs are on by default and give the controller `VM.GuestAgent.FileRead`. `node_boot_logs = false`, or **Nodes' boot logs** in the setup, switches them off ([install-settings.md](install-settings.md#nodes-boot-logs)).
- A backup can be deleted before it expires, once its name is typed.
- A restore can pick its namespaces, its kinds of resources and whether the volumes' data comes back. A finished restore lists Velero's errors and warnings by namespace.
- A cluster can read the backups of others in its namespace on the same target (`backups.readFrom`), to restore them. One created to read another's also restores that cluster's volume data.
- The health page and the fleet say when the last backup failed or backups are overdue ([Back up and restore](clusters.md#back-up-and-restore)).
- The plan and kubectl warn of a change that replaces a cluster's only control plane.
- A control-plane machine not ready for 20 minutes sets `ControlPlaneRollStalled` (`watch.stall_minutes` in the module, `watch.stallMinutes` in the chart), with why and whether the API answers.
- A stalled roll also gets a warning event, an item on the Health page and **Recover…** on the cluster's page.
- Recover, or `kubehenge.dev/etcd-restore` naming a `machine`, keeps that control-plane machine and replaces the others. It resets etcd to that machine's database when the API is down ([troubleshooting.md](troubleshooting.md#one-control-plane-its-roll-stalled)).
- While a cluster is being deleted, its page and `kubehenge.dev/deletion-status` list every object holding the deletion and why.
- A deletion that waited 20 minutes sets `DeletionStalled` with a warning event, listed in the fleet and on the Health page.
- **Abandon…**, or `kubehenge.dev/abandon-deletion` set to the cluster's name, deletes the rest without draining machines. It ends once no ProxmoxMachine or ProxmoxCluster is left, or after 5 minutes.
- Each thing left behind in Proxmox VE is named in a `LeftBehind` event ([troubleshooting.md](troubleshooting.md#a-cluster-does-not-finish-deleting)).
- kubeadm's control plane is replaced 60 days before its certificates run out.
- RKE2 and k3s workers, k3s's control plane and an RKE2 control plane of three or more roll from 60 days before expiry, one machine at a time. A smaller RKE2 control plane says to restart `rke2-server`.
- The Cluster gets `CertificatesExpiring` with events, on its Health page and in the fleet, until a minute after the last replacement.
- The kubeconfig and talosconfig kept for each cluster are watched too ([troubleshooting.md](troubleshooting.md#certificates)).
- The interface warns administrators 30 days before the management cluster's admin credentials run out, and an apply renews them.
- A failed template build is tried again, from the interface, `kubehenge template create` and the weekly build, unless the failure would repeat, such as a node whose CPU is too old.
- A weekly build that still fails records `TemplateBuildFailed` on the ConfigMap `kubehenge-template-refresh` ([troubleshooting.md](troubleshooting.md#template-build-fails)).
- The Health page and the fleet show problems with a cluster's VMs and storage, checked every 15 seconds.
- A VM stopped, paused by hand, paused by QEMU after a disk error, or crashed for 3 minutes sets `VMsNotRunning`.
- A storage 85% full on a node the cluster runs on sets `StorageNearlyFull`, an error from 95%. Both conditions record events ([troubleshooting.md](troubleshooting.md#vms-that-do-not-run-storage-nearly-full)).
- A held Kubernetes upgrade sets `UpgradeHeld`, with the reason `RancherKubernetes`, `RancherElsewhere` or `TalosUpgrade`.
- The fleet and the Health page show **Upgrade held for Rancher** or **Upgrade waits for Talos**. `kubehenge cluster list` shows the reason in STATUS ([troubleshooting.md](troubleshooting.md#upgrades-held-back)).
- **Administration, Backups** snapshots the management cluster's etcd on a schedule into a backup target, encrypted, and lists the snapshots there.
- **Restore…** wipes etcd on the control-plane nodes and restores one with the state passphrase, also while the API is down ([install.md](install.md#back-up-the-management-clusters-etcd)).
- An appliance upgrade is rolled back unless the new interface answers within 2 minutes and stays up for 20 seconds.
- The Administration page shows such an upgrade as rolled back, with why ([troubleshooting.md](troubleshooting.md#appliance-upgrades)). `install.sh --upgrade` checks and rolls back the same way.
- `kubehenge appliance` in the appliance's shell shows the upgrades and how the last went, asks for one and checks the interface.
- `kubehenge appliance` sets up, lists, takes and restores the management cluster's etcd backups, with the Administration page's privileges and refusals ([upgrading.md](upgrading.md#from-the-appliances-shell)).

### Changed

- A change to a cluster or machine shows at once in the fleet, the runs and the cluster's page.
- The fleet list at `/clusters` takes the whole page and can be filtered, grouped and sorted. With no clusters yet, it shows whether a VM template is built and whether you may create clusters.
- Each cluster has one health verdict, the same wherever it shows.
- A cluster's page opens at once with what the fleet list last read. A cluster just created or deleted shows so in the list and the rail at once.
- A view seen before shows its last reading while it reads it again, and a first read shows a placeholder in its shape.
- A tab with something to look at carries a badge, and a strip above the cluster's views shows the worst problem.
- A reading that could not be renewed is greyed out as stale. While the clusters cannot be read, the list counts down to its next try.
- A failed run starts again from its page with what it was started with, and asks again for a passphrase or typed name.
- An etcd snapshot, an etcd restore and a change show their events since they began.
- Machines on an offline Proxmox VE node show as down, and those whose kubelet stopped reporting as not reporting. Nodes of a cluster that does not answer show as not read.
- The top bar and fleet list fit phones, where fields stack.
- A cluster's options are grouped, with on-or-off settings switched in their rows.
- **Scale** sends only what changed.
- **Upgrade** offers the newest version it may move to and checks it first.
- A machine is replaced from its row.
- The create wizard starts from the newest working cluster's settings, or from another's or the defaults. It takes the network from the bridge, suggests free addresses and marks typed ones in use.
- The wizard shows each add-on as a card with its memory. **Confirm** has the API server check the cluster and weigh its memory and disk against what is free.
- The management cluster is set up in steps from what the nodes report, with its health shown and its removal in a danger zone.
- A read that hangs is given up after 30 seconds.
- What needs attention leads to where it is acted on, the machine's row in **Machines** or **Backups**.
- A failed run of the appliance links to the page it is started from.
- The API server refuses a new pool whose name cannot be part of its nodes' host names.
- A mistyped `kubehenge` subcommand fails and names the closest one. A command given the wrong arguments says which it takes, such as `give the cluster's name`.
- `kubehenge cluster list -o` checks its format before it calls the interface, and `kubehenge cluster list` and `create` refuse `-n`.
- `kubehenge appliance` takes `--json`, `--user`, `--password-file` and `--totp` after its subcommand.
- A Proxmox VE task that fails is reported with its UPID and the last lines of its log, not its exit status alone.
- The interface's account in the management cluster may read every Cluster API object in its namespaces and the logs of the controller, capmox and the Add-on Provider for Helm.
- Volume snapshots need no `root@pam` password or `Permissions.Modify` on `/`. Every cluster on storage local to each node may take them, with `VM.Clone` on its pool.
- Each cluster's volume snapshots are copied with its own token, one at a time, into a VM in its pool on the volume's node.
- A snapshot of a large volume turns ready once its copy is whole.
- A Cluster applied with kubectl or GitOps is held to the interface's rules by the API server ([clusters.md](clusters.md)).
- Those rules are a name of at most 40 characters, a version in the distribution's form and 1, 3, 5 or 7 control-plane nodes.
- The rules allow upgrades of one minor version at most and never back, and at most 100 workers in each pool.
- Node addresses and the control-plane endpoint must be on the gateway's network, with the endpoint outside the node addresses.
- The controller gives a new cluster, from kubectl or GitOps too, the pod and service ranges it does not name. A cluster gets its distribution's own ranges where they are clear of the nodes' network.
- A cluster that runs Cilium gets ranges clear of every other cluster's.
- The API server refuses pod or service ranges that overlap the nodes' network or each other, or are not IPv4.
- The management cluster keeps answering on a busy host, with a CPU weight of 1000 for its nodes (`node_cpu_units`).
- Cluster API's providers allow 5 s for each health probe and six missed liveness probes, so a busy management cluster no longer restarts them.
- The Add-on Provider for Helm keeps each pinned chart once downloaded, so it no longer takes over 100 MiB reading a large repository's index.
- Every add-on's pods request CPU and memory, as do RKE2's and k3s's Traefik, CNI and snapshot controller and kube-vip. Busy nodes no longer restart them on failed liveness probes.
- The add-ons' CPU figures count the new requests, so a 2-core worker no longer takes every add-on on RKE2 and Talos.
- kubeadm and k3s clusters with Calico no longer run Goldmane and Whisker, Calico's flow-log aggregator and viewer. That saves CPU on small nodes.
- capmox, which creates the clusters' VMs, moves to its v0.9.2 release.
- A new kubeadm cluster's control planes get 4800 MiB by default, up from 4096 MiB, as a cluster with Calico needs.
- **Ctrl K** starts a cluster's actions too. Each action also opens from its own address.
- Shortcuts that leave a page take two keys in turn, such as `G` `C` for the clusters, so a stray key does nothing.

### Fixed

- Activity no longer misses kubectl changes behind a long stretch of other requests in the API server's log. It warns when lines rotated away before it read them.
- An etcd restore or reset of a CIS-hardened RKE2 cluster takes about 2 minutes, not 17.
- Applying the management-cluster module no longer finishes while core Cluster API is still being reinstalled after its upgrade.
- A template build or a cluster's storage set-up no longer fails when Proxmox VE kills a VM create before it starts. It goes on under the next VM ID and logs how to remove the VM left locked.
- A template build no longer fails on one dropped connection or 5xx answer from the publisher of a checksum, signature, key or Talos schematic.
- Deleting a template build checks that its VM ID still holds that template, as Proxmox VE reuses freed IDs.
- An add-on whose publisher moves a version's tag to a new build, as the Proxmox CSI plugin's did, still installs and upgrades.
- Rancher installs the Turtles, webhook and Fleet charts Kubehenge names, from the chart repository in its pinned image.
- A k3s or RKE2 upgrade with an older Gateway API chart installs it. Kubehenge's admission policy guards only the CRDs it installs, on kubeadm and Talos.
- Kyverno's controllers hold their leader leases through a stall of up to 50 seconds, like the other add-ons, not 10.
- Kyverno's admission controller runs two replicas with a disruption budget, so an etcd pause no longer refuses every pod. Its add-on figures rise to 640 MiB and 500 millicores.
- A kubeadm control plane with Calico and every add-on no longer stalls its API server. Its memory counts as measured use plus 25%, 4976 MiB with every add-on but Rancher.
- Applying the management-cluster module no longer ties up a Proxmox VE API worker. The Talos image downloads without waiting for the module's other changes.
- Kubehenge's admission checks the variables of every cluster, the first of each distribution included. A later change of its CNI or CIS hardening no longer escapes the rules.
- When the installer removes a container whose interface failed to start or reach the API, it prints the interface's log first.
- The installer's check of the new appliance verifies its certificate instead of skipping it, and `install.sh --help` lists every option.
- `kubehenge template create` and `template refresh` refuse an empty `--node` before calling Proxmox VE.
- The controller refuses a Pod Security exemption list with anything after its closing brace, where it silently held the namespaces after it to the cluster's level.
- The next template refresh no longer waits for the last one's Lease to run out.
- After an etcd restore, a Talos cluster's nodes reach the API server and every other Service. The by-hand steps do the same with `etcdutl`.
- After an RKE2 or k3s etcd restore, clients watching from before no longer miss changes. The by-hand steps do the same with `etcdutl` and `openssl`.
- Talos clusters with Cilium send their services' and kernel's logs to Loki.
- When the interface stops, as on an appliance upgrade, it lets requests under way finish and stops Headlamp and token upkeep first.
- RKE2 clusters with Cilium start on AlmaLinux and Rocky Linux nodes, where SELinux kept Cilium from installing its portmap plugin.
- k3s and kubeadm clusters with Calico start, where Calico's node agent failed on the nodes' read-only `/usr`.
- A kubeadm cluster with Calico, applied with kubectl, gets a pod range and so runs Calico.
- Rolling a Calico cluster's nodes no longer stalls, with RKE2's own Calico too.
- Host ports, Traefik's among them, work on RKE2 and kubeadm clusters with Calico or Canal on AlmaLinux, Rocky Linux and openSUSE Leap.
- Calico no longer reports itself degraded on the AlmaLinux, Rocky Linux and openSUSE Leap templates.
- The kubelet's pressure metrics work on AlmaLinux and Rocky Linux nodes.
- A fixed pool's rollout no longer waits for ever for a spare node address.
- Two clusters on the same bridge and VLAN can no longer be given the same node addresses or control-plane endpoint.
- With `proxmox_insecure`, etcd restores through the guest agent skip the check of Proxmox VE's certificate too.
- A scale-down during a Talos upgrade no longer holds the cluster's other Talos upgrades until **Try again**.
- A Talos install that stalls for three minutes or loses the node is tried again.
- An etcd restore that succeeded is no longer recorded as failed with "the controller restarted during the restore".
- Removing a management cluster node no longer destroys another VM that took its ID, such as a template being built. Its VMs get random IDs from 10000 to 99998.
- A machine whose clone fails is cloned again under a new VM ID. It no longer waits for ever on another VM that took the old ID and deletes it with itself.
- Deleting a machine destroys the VM under its ID only once the VM's name and SMBIOS UUID confirm it, so capmox no longer destroys the next clone given a freed ID.
- Deleting a machine finishes once its VM ID is free or held by another VM or a container.
- An AlmaLinux or Rocky Linux template build moves to the next mirror when one serves a broken `repomd.xml`, where its retries read the same broken file.
- k3s clusters run without k3s's local path provisioner, whose StorageClass also claimed to be the default, so a claim without a class no longer ends up on one node's disk.

### Security

- Deleting a cluster or restoring its etcd takes the cluster's name in the request, as the dialogs already ask for it.
- A change made against a cluster someone else has changed since is refused (412) instead of undoing theirs. The page drops what was staged and shows the cluster as it is.
- The interface's credential in the management cluster is a token that runs out after 30 days and is renewed daily. It replaces one that never expired.
- Too many failed sign-ins refuse further tries for a while, by that user from that address and from that address overall. Failures from elsewhere cannot lock an administrator out.
- Headlamp's port takes changes and WebSockets only from its own pages, and both ports send `Strict-Transport-Security`.
- A certificate the installer renews keeps only DNS names the cluster's CA signed or this node's DNS resolves to the appliance.
- The installer takes a static address from the container's configuration rather than from inside it.
- The updater keeps one capability, runs under a system-call filter with memory that cannot be made executable and hides other users' processes. The interface runs under the same filter.
- The state passphrase and token that automatic management upgrades keep are sealed, with the key on a 1 GiB volume backups leave out.
- An appliance restored from a backup asks for automatic upgrades to be switched off and on again.
- Appliances read the release index from `https://kubehenge.dev/releases/index.json`, signed again every week from the latest release.
- Appliances refuse an index signed more than two weeks ago, or one that carries no expiry.
- The release key can be rotated ([releasing.md](releasing.md#rotating-the-release-key)).
- Pod Security Admission warns about and audits pods short of `restricted` at every level. Choosing `privileged` in the interface writes it into the cluster's variables.
- Kyverno exempts only the namespaces of the add-ons and the CNI a cluster runs, RKE2's own Calico among them.
- The controller's Runtime Extension hooks read the Cluster from the API server, not from the request. The delete hook acts only on a Cluster being deleted.
- The validating webhook fails closed, and the controller may not write ServiceAccount token Secrets.
- An etcd restore first takes a snapshot of etcd as it is and shows it on the restore. It refuses a snapshot without a well-formed SHA-256.
- The management-cluster module installs the Cluster API Operator from a chart archive checked against its SHA-256.
- The controller no longer holds the tokens that `proxmox_ha` and `etcd_restore` give it. Each sits in a Secret of its own (`kubehenge-proxmox-ha`, `kubehenge-proxmox-restore`, key `token`).
- Only a short-lived Job mounts each token, one per HA write or restore step ([architecture.md](architecture.md#tokens-kept-out-of-the-controller)).
- The HA Job writes and replaces only HA resources and rules that carry Kubehenge's comment. HA reads use the controller's own token.
- kube-network-policies enforces network policies in the management cluster, beside Talos's flannel.
- Nothing reaches a pod in `kubehenge-system` or `kubehenge-autoscaler` but the controller's webhook, lifecycle hooks and metrics.
- The HA and restore Jobs reach only DNS and Proxmox VE's API ([architecture.md](architecture.md#management-cluster)).
- Each cluster gets a backup key of its own, which stays in the management cluster for Kubehenge's own etcd snapshots only. A shared key let one cluster's admins delete the others' backups.
- The management cluster's etcd snapshots use a talosconfig that may only take etcd snapshots.

## [0.3.1] - 2026-10-28

### Highlights

- New machines start reliably.

### Changed

- The installer names the address of a download that fails, where curl gives only the HTTP status.

### Fixed

- A machine whose VM needs no configuration change is started instead of being reported ready while its VM stays off.
- A passing error while attaching the bootstrap ISO no longer fails the machine for good.

## [0.3.0] - 2026-10-27

### Before you upgrade

- Build the AlmaLinux 10 or Rocky Linux 10 templates first. Clusters move off Ubuntu onto them as the ClusterClasses change.
- Every cluster's control planes are replaced, for the API server's memory limit, and RKE2 and k3s control planes also for the etcd member fix.

### Highlights

- Talos clusters, beside kubeadm, RKE2 and k3s.
- AlmaLinux 10 and Rocky Linux 10 nodes in place of Ubuntu.
- Backups, GitOps, certificates and autoscaling for each cluster.
- See what a change replaces before it applies.
- Node templates stay current by themselves.

### Added

- The fleet runs down the left of every page, grouped into clusters that need you, those with work running and the healthy, each with its reason.
- The work under way runs down the right, snapshots, backups, restores, template builds and appliance upgrades included, and a failed run shows its first error.
- A cluster's views are tabs under its name, and its summary opens on its machines laid out by Proxmox VE node.
- A cluster's changes are staged, checked by the API server and applied together after a plan of what each replaces ([clusters.md](clusters.md#staged-changes)).
- Every run has a page, with its first error pinned above its steps and a count of the machines replaced.
- Administration has four pages, the management cluster, the appliance, templates and certificates.
- **Go to or run…** (`Ctrl K`) finds any cluster, view or action by a few letters, and single keys do the rest ([clusters.md](clusters.md#finding-your-way)).
- The interface follows the system's dark or light mode.
- Talos Linux has a ClusterClass (`kubehenge-talos`) with the other classes' variables, add-ons and checks but no `nodeOS`, and one extra variable, `talosVersion`, the release the nodes run.
- Talos control planes hold their address with Talos's own virtual IP, and the CNI is Talos's flannel unless the cluster chooses Cilium or Calico.
- Talos templates come from the Talos Image Factory, one per release, built on the Administration page, from the create wizard or with `kubehenge template create talos --talos-version`.
- Changing `talosVersion` upgrades the nodes in place through the Talos API, one at a time, control planes first, each drained first and one minor version per step.
- An upgrade that fails or rolls back stops the rest until **Try again** on the cluster's page.
- With node image updates on, Talos nodes move to new patch releases inside the maintenance window.
- The cluster page shows the Talos release, the upgrade under way and the last one, and downloads the talosconfig for those who may download the kubeconfig.
- Talos etcd snapshots are taken and restored through the Talos API, and a restore wipes etcd on every control plane, recovers it on the first and lets the others rejoin.
- Talos's service and kernel logs reach Loki through the node collector, under `job="talos"`.
- The template refresh keeps the two newest Talos releases' templates of each minor, and any a cluster still uses.
- Talos's controller manager and scheduler metrics are not scraped yet, as their certificates cannot be verified. Kubehenge scrapes them once Talos signs them with the cluster CA.
- Talos's bootstrap and control-plane providers are Kubehenge's forks, [cluster-api-bootstrap-provider-talos](https://github.com/kubehenge/cluster-api-bootstrap-provider-talos) and [cluster-api-control-plane-provider-talos](https://github.com/kubehenge/cluster-api-control-plane-provider-talos), on Cluster API's v1beta2 contract.
- The Talos controllers run only while a cluster uses Talos.
- Velero backs up to the S3 stores an administrator sets, on a schedule, keeping each backup as long as the cluster chooses, under a prefix of its own per cluster and without `kube-system`.
- The cluster page lists backups and restores and backs up at once. Those who may download its admin kubeconfig can restore the whole cluster or chosen namespaces.
- The management cluster rebuilds the node OS VM templates weekly with their updates, by default on Sunday at 01:00 UTC, set on the Administration page or with the module's `template_refresh`.
- The builds use an API token of their own that may build and delete templates in the template pool and nothing else.
- Each build is a template of its own, and the two newest of each template stay, with any a cluster still runs.
- A cluster's nodes move to a new build inside the maintenance window once it is a day old, one at a time, unless the cluster switches node image updates off.
- The cluster page shows which build the nodes run and when they next move.
- The Administration page shows the last run with its log and has **Build now**.
- Where the administrator allows it, cluster creators can have a missing template built from the create wizard.
- A new build keeps the CPU type of the template it replaces. With `--proxmox-insecure`, the builds reach the API without verifying its certificate.
- The cloud controller manager labels each cluster's nodes with their Proxmox VE HA groups or HA node-affinity rules.
- The cluster's token gets `Sys.Audit` on `/`, unpropagated, through a group the controller adds its user to. It only reads the configuration and ends the permission errors logged every few minutes.
- A cluster switches the labels off in the wizard, under **Options** or with its `ccm` variable. An administrator does so for all with **HA group labels** in the setup or the module's `cluster_sys_audit`.
- Administrators can allow extra cluster namespaces (**Namespaces for clusters**, `extra_cluster_namespaces`, `interface.extraNamespaces`), off by default. The wizard then asks for one ([clusters.md](clusters.md#more-namespaces-for-clusters)).
- A cluster's Headlamp can show workloads, ConfigMaps, events and logs, but no Secrets, to everyone without sign-in, through the `view` ClusterRole. Off by default, set with `headlamp.access: view`.
- A cluster on node-local storage (ZFS, LVM, LVM-thin, directory or btrfs) may take VolumeSnapshots, restore and clone them, with the class `proxmox` and, on kubeadm and k3s, a snapshot controller.
- Off by default. An administrator enables them with `cluster_volume_snapshots` (**Volume snapshots** in the setup, with `root@pam`'s password) and a cluster with `csi.volumeSnapshots`.
- That cluster's CSI plugin signs in as `root@pam`, as only it can copy volumes, so whoever takes over the cluster holds Proxmox VE. Switching it on takes `Permissions.Modify` on `/` ([clusters.md](clusters.md#volume-snapshots)).
- cert-manager issues certificates from Let's Encrypt or any ACME server, or from a CA of the cluster's own, constrained to its domain and downloadable from the interface.
- ACME certificates are proved by DNS-01 through Cloudflare, Route 53, DigitalOcean, Azure DNS, Google Cloud DNS, RFC 2136, ACME-DNS or Akamai, or by HTTP-01 through Traefik's Gateway.
- With DNS-01 or the CA, a wildcard certificate for the cluster's domain is Traefik's default and its Gateway's HTTPS listener's. The add-ons' Ingresses ask for their own where it does not cover them.
- The DNS provider's credential is typed into the interface per cluster, or once for every cluster that opts in with `Sys.Modify`. It can also stay in a Secret of the cluster's own.
- Argo CD or Flux, off by default, keeps the cluster in step with a directory of a Git repository, and leaves what it deployed running when switched off or to the other engine.
- A private repository is read with a deploy key Kubehenge makes and the interface shows, a stored token, or the owner's own Secret. Over SSH the server's host keys are read once and trusted.
- Argo CD signs in through the cluster's first identity provider with PKCE, its local admin disabled, and gives rights only to the groups named its admins.
- Flux is installed at a pinned version by the Flux Operator, whose web interface is an opt-in.
- Choosing the repository needs `Sys.Modify` on the pool, as the admin kubeconfig does.
- A worker pool can be autoscaled, at creation or later from the Scale dialog, with a minimum of 0 to 100 workers and a maximum up to 100.
- The autoscaler of the cluster's Kubernetes minor adds workers while pods wait and removes unneeded ones. It scales up from none by machine size with a 10% margin and node labels.
- The autoscaler runs in the management cluster with a token into its cluster and a request of 64 MiB. With `VM.Audit` on the workload pool, **Options** can run it on the cluster's control planes.
- An admission policy holds each autoscaler to its own cluster's pools and machines.
- Control planes stay tainted while a pool may grow, and add-ons need a pool that keeps a worker.
- The node addresses must hold every pool at its most. The Scale dialog warns when the Proxmox VE nodes lack the memory for it.
- A worker that finds no room shows `AutoscalerNoRoom`.
- Switching the autoscaler off fixes the pool at the workers it runs and removes everything the autoscaler was given.
- The controller takes etcd snapshots of each cluster on a schedule, or at once from the cluster page, into the backup S3 stores. The cluster chooses how many are kept.
- The cluster page lists snapshots, restores one, and downloads them for those with `Sys.Modify` on the cluster's pool.
- A restore stops every control plane, restores etcd on one and replaces the others. It goes through the Proxmox VE guest agent where an administrator switched that on, and as steps by hand where not.
- On RKE2 and k3s a cluster may opt into the distribution's own upload of its servers' snapshots to the same target. It is off by default and verifies the target's certificate.
- That upload puts the target's access key into a Secret in the cluster's `kube-system`, out of the bootstrap data, and stores its snapshots unencrypted for restoring by hand.
- An administrator can have the shared Headlamp sign in to each cluster as cluster-admin for holders of `Sys.Modify` on its pool. Off by default, set with the setup's Headlamp sign-in or `headlamp_sign_in`.
- The sign-in's token is made for the session, stays in the appliance and lasts an hour at most. The interface logs each token with its user and ID, and the cluster's audit log records the ID.
- Switching Headlamp sign-in off removes its accounts from every cluster, ending every token. Logging out of a cluster in Headlamp drops the session's token until the interface opens the cluster again.
- Every apply records the Kubehenge release that ran it, and once the appliance runs a newer release the Administration page says so.
- **Upgrade the management cluster** lists the components whose versions change and applies the settings with the state passphrase.
- The management cluster can follow the appliance's upgrades on its own, off by default, in their hour and once a day at most. A failed automatic run waits for the next day.
- Automatic management upgrades keep the state passphrase and a token of a Proxmox VE user with the Administrator role. Switching them off, or removing the management cluster, deletes both.
- An administrator can have the shared Headlamp offer the Helm app catalog and the Prometheus charts it ships with. Off by default, set in the setup or with `headlamp_actions`.
- Everyone who sees a cluster can list, install, upgrade, roll back and uninstall its Helm releases and chart its metrics from a Prometheus there, with their own credentials or the sign-in's.
- Charts come only from the appliance's Helm repositories, which only those who may set the appliance up add or remove, over HTTPS.

### Changed

- RKE2, k3s and kubeadm nodes run a node OS each cluster chooses with `nodeOS`, in the wizard or under **Options**. It is AlmaLinux 10 by default, or Rocky Linux 10 on `x86-64-v3` CPUs.
- Both run in image mode, a bootable container image of the family's own signed packages with a read-only `/usr`. openSUSE Leap 16.1 Immutable follows once openSUSE releases it.
- Each node OS holds only what a node needs, cloud-init, the QEMU guest agent, SELinux policies and kubeadm's containerd and tools.
- Each node OS enforces SELinux, with containerd's SELinux support and RKE2's and k3s's own policies. On kubeadm the control plane's static pods and kube-vip run as `spc_t`.
- The QEMU guest agent runs what Proxmox VE sends it unconfined, as a root shell would.
- Changing the node OS replaces the nodes one at a time once its template is built.
- Clusters are placed only on Proxmox VE nodes whose CPU runs their node OS, and are refused when none can.
- RKE2 and k3s have a template each per node OS, for their policies, and `kubehenge template` takes `--node-os`.
- The interface raises the cores to the add-ons' requests and shows them on the Add-ons step. Each class carries the figures (`kubehenge.dev/addon-cpu`, `kubehenge.dev/node-cpu`, [clusters.md](clusters.md#machines)).
- Kubehenge checks that control planes have the memory the add-ons add, with or without workers. The interface raises them to it.
- With every add-on but Rancher an RKE2 control plane needs 4720 MiB, where 4096 MiB ran out once Argo CD started. The figures are measured as each add-on is switched on ([clusters.md](clusters.md#machines)).

### Fixed

- Logging out works in the shared Headlamp.
- A template build leaves an AlmaLinux or Rocky Linux mirror serving under 100 kB/s for the next. It stayed down to 1000 bytes/s and could run past its 45 minutes.
- A refresh no longer fails when an AlmaLinux or Rocky Linux mirror caught syncing serves a broken file, such as a `repomd.xml` that is not XML.
- A k3s cluster with flannel runs kube-proxy in nftables mode, so a starting node's pods no longer lose every Service, the API server included, to the network policy controller.
- Kubehenge refuses a cluster whose cores fall short of what the distribution's, the CNI's and its own pods request, so no pod stays pending with `Insufficient cpu` on an idle node.
- Every add-on but Rancher fits on one worker of the default 2 cores on every distribution.
- A cluster's setup no longer fails when another create takes the VM ID Proxmox VE offered for its volume owner VM.
- A new kubeadm cluster's control-plane address no longer goes unanswered for about ten seconds during bootstrap.
- An RKE2 or k3s server whose etcd member is removed, as its control plane is replaced, stays down until its VM is deleted. A one-server cluster no longer loses quorum under load.
- The API server runs with a Go memory limit of 40% of the control plane's memory, half on k3s for the whole server process. A 4096 MiB kubeadm control plane survives all add-ons switching off.

### Removed

- Ubuntu templates are no longer made or selected.

### Security

- Backups are encrypted with the cluster's own keys, SSE-C for its resources and Kopia for its volumes' data.
- Etcd snapshots are encrypted with age to a key of the cluster's own and checked against etcd's checksum, and no etcd key or store credential is in the cluster.
- The forks' files and images are pinned by digest, and the management cluster's nodes pull the fork images only with the signature of the fork's release workflow.

## [0.2.0] - 2026-10-22

### Highlights

- Gateways and HTTPRoutes work in every cluster.
- Clusters ride out short API stalls without restarting their controllers.
- The installer finds the right Proxmox VE API address and CA by itself.

### Added

- Every cluster has the Gateway API's standard CRDs, so Gateways and HTTPRoutes work beside Ingresses. Upstream's admission policy refuses older or experimental CRDs over them.
- Traefik's Gateway `traefik-gateway` in `kube-system` listens for HTTP where Traefik does and takes HTTPRoutes from every namespace.
- RKE2's and k3s's Gateway API CRDs stay theirs while they ship them. Elsewhere Kubehenge brings its own release, never an older one over a newer.
- Istio's waypoints are Gateways of the class `istio-waypoint`.
- With `cluster_datastore_allocate`, a cluster may choose the newest Proxmox VE CSI plugin in the wizard, **Options** or `csi`. Its token then gets `Datastore.Allocate` on a storage of its own.
- An administrator can offer Proxmox VE's HA node-affinity rules as placements to cluster creators, who then need no `Sys.Audit` on `/`. It is off by default (`offer_ha_rules`).
- `--proxmox-insecure` makes the appliance, its providers, controller and clusters reach the API without verifying its certificate. `--upgrade --proxmox-ca` turns verification back on.

### Changed

- Controller managers, schedulers, the CSI controller and the Cilium operator keep their leader leases through a stall of up to 40 seconds rather than restart.
- The installer reaches the Proxmox VE API only at a name or address of the cluster's own nodes that its certificate covers. Where several are as good it asks, and where none is left it stops.
- Without `--proxmox-ca`, the installer finds the CA the API's certificate rests on in the node's trust store, else asks to accept the CA the API serves. The appliance and clusters get that CA alone.
- `--proxmox-ca` takes a bundle such as a whole trust store, reduced to the CA the certificate rests on, and a CA that is not self-signed.

### Fixed

- The health page reads a stopped container's log from its newest ended run, where a crash-looping pod's log sometimes showed an error.
- On k3s, replacing a single control plane no longer takes its etcd member down with restarting controllers.

### Security

- The installer's certificate checks trust only the CA file given, not the node's default trust store besides.

## [0.1.0] - 2026-10-22

### Highlights

- Kubernetes on Proxmox VE, from one install script and a web interface.
- Sign in with your Proxmox VE account and its permissions.
- kubeadm, RKE2 and k3s clusters, each walled off from the others.
- Add-ons from Headlamp and Rancher to monitoring, logs and Istio.

### Added

- One script on a Proxmox VE 9 node installs a small unprivileged container running Kubehenge's web interface. Users sign in with their Proxmox VE accounts and act with their own permissions.
- The appliance upgrades itself from a button, or automatically within a major version.
- The appliance's certificate is signed by the Proxmox VE cluster's CA or is your own, and can take a DNS name. It is renewed from the Proxmox VE shell, and the interface warns before it runs out.
- A Proxmox VE API whose certificate comes from another CA works too, with that CA given to the installer.
- The interface or the module in `management-cluster/` sets up a Talos management cluster of one or three VMs. It runs Cluster API with the Proxmox VE, kubeadm, RKE2, k3s and Helm add-on providers.
- The Proxmox VE provider is Kubehenge's own reproducible, signed build of an upstream commit after v0.9.1, which carries the fix kubeadm upgrades need.
- A distribution's controllers run only while a cluster uses it.
- The controllers hold their leader leases through the API stalls a disk clone on the same storage causes, up to 40 seconds.
- Every controller has a memory request, so a node short of memory does not evict it first.
- Workload clusters come from three ClusterClasses, kubeadm, RKE2 and k3s. They are created, scaled, upgraded one minor version at a time, repaired when nodes fail, and deleted.
- Each cluster gets the Proxmox VE cloud controller manager, the Proxmox CSI plugin, a kube-vip control-plane address and Traefik for ingress. Its CNI is the distribution's own, Cilium or Calico.
- `LoadBalancer` Services get addresses from a range on the nodes' network, with MetalLB (recommended) or kube-vip. A range that clashes with the nodes', the API address or another cluster's is refused.
- Pod and service ranges, OpenID Connect sign-in and automatic upgrades within windows are set per cluster.
- While a cluster has workers, its control-plane nodes are tainted and run only what has to run there. Without workers, the control planes run the workloads and get a worker's minimum on top.
- Control planes are sized by the distributions' server sizing guides, from 2 cores and 4096 MiB. A cluster whose control planes are too small for its workers is refused.
- The nodes that run the add-ons need room for each add-on's measured memory, and the interface raises the machines to it.
- Each cluster's Health page says whether it is healthy, changing, degraded or unreachable, and what needs attention first. It shows failing nodes, pods, volumes and add-ons, and each VM's CPU.
- Health reads inside the cluster with read-only identities of its own. Pods' logs need `Sys.Modify` on the cluster's pool and come from an identity that reads logs only.
- A cluster's VMs spread over every online Proxmox VE node, the nodes chosen by hand or one of Proxmox VE's HA node-affinity rules. The choice can change later.
- A node that goes down is left out within about 15 seconds, so replacement VMs go elsewhere.
- The cluster page shows where each VM runs and warns when control-plane VMs share a node.
- With Proxmox VE HA on, every cluster's control-plane VMs stay on separate nodes. HA restarts them elsewhere after a node fails, for clusters on shared storage that do not opt out.
- Headlamp runs per cluster, or shared in the appliance behind its sign-in.
- Rancher installs into a Kubehenge cluster, with the other clusters registered in it automatically.
- Monitoring with kube-prometheus-stack scrapes every node and control-plane component over verified, authenticated TLS.
- Logs run beside monitoring, with Grafana Alloy collecting on every node into Loki, kept 7 days in 10 GiB unless set otherwise. Loki lets in only requests with credentials RBAC permits.
- Pod security is a Pod Security Standard level enforced by Pod Security Admission, by Kyverno, or both. The namespaces of the components that need the host are exempt.
- CIS hardening is chosen at creation, beyond the CIS Kubernetes Benchmark baseline every cluster gets.
- Istio in ambient mode gives mutual TLS between the pods of enrolled namespaces, without sidecars. Switching it off takes the namespaces out of the mesh first.
- Cilium Cluster Mesh joins clusters of one namespace that run Cilium and list each other, so they reach each other's pods and global services. Peers come and go without restarts.
- An optional entry in the Proxmox VE interface leads to the appliance (`appliance/pve-entry/`), if the administrator agrees at install.

### Security

- The appliance installs only releases signed with the project's release key, unpacked where none of their entries can write outside their own directory.
- Each cluster has its own Proxmox VE pool, user and API token with rights on its own VMs only. A VM of its own owns its volumes, so a compromised cluster cannot reach another's.
- Only the cloud controller manager and the CSI plugin's controller hold the token, on control-plane nodes only.
- kubeadm clusters' kubelets serve certificates from the cluster CA, which the controller approves and the API server verifies.
- Every cluster audits its API server, encrypts Secrets in etcd, limits the rate of events and offers only strong TLS ciphers. It refuses requests without credentials beyond its health checks.
- Every cluster keeps its controller manager and scheduler on loopback and runs its kubelets on the kernel settings they expect.
- Every image, chart, provider file and tool Kubehenge deploys or builds with is named by its digest. Where bytes are not stable or carry no digest, a signature is required instead.
- RKE2 and k3s sign neither releases nor images, so they install from files checked against checksums recorded at first use.
