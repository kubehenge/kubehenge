# Changelog

Versions follow [Semantic Versioning](https://semver.org/). Before 1.0.0 a minor version may change behaviour. Each release says first what to do before upgrading, then gives its highlights and its changes by kind. Changes not yet released show under `[Unreleased]` on the website.

## [Unreleased]

## [0.5.0] - 2026-11-12

### Before you upgrade

- A cluster whose etcd snapshots and backups use different targets is refused at its next change of either. Point both at one target first.
- Add-on chart and value changes from a new release wait for the cluster's upgrade window, shown on its page and in the Upgrade dialog.
- Build every node template again before nodes are added or replaced. kubeadm, RKE2 and k3s nodes wait at boot for a chrony that older templates do not set up.
- A scripted `install.sh` with no terminal, on a node whose own network is static, needs `--ip <address>/<prefix>` and `--gateway`. `--ip dhcp` keeps the old default.
- Give the roles of cluster users `Pool.Audit`, as the docs' examples now do. Without it, the cluster page and a reboot say the machine has no VM.
- Export the appliance once it is upgraded. The stored cluster specs open only with its key, and the fleet warns until you do.

### Highlights

- **New installer.** `install.sh` starts a full-screen guide that asks DHCP or static, shows a summary before it changes anything and refuses an address already in use.
- **Upgrade checks.** Removed APIs and weak webhooks are found before a cluster upgrades, and an automatic upgrade waits until you accept what they find.
- **Outages and recovery.** Machines of a rebooting host are kept for 30 minutes. Appliance export and restore, and recovery of a cluster that lost quorum, are new.
- **Limited-access sites.** Mirrors for charts, releases and registries, proxies and trusted CAs work for nodes and add-ons.
- **Networking.** IPv6 and dual-stack clusters, BGP load balancing, SDN VNets, security groups and a node MTU are new.

### Added

- The installer takes `--vlan` and `--mtu` for the container's network. `--tofu <file>` installs OpenTofu from a local archive, checked against the script's checksum.
- The installer tests the gateway, the name server and the API's port from inside the container. A failure names the option to check (`--vlan`, `--gateway`, `--dns` or the node's firewall).
- SDN VNets work wherever a bridge is chosen, listed with their zone and alias. `workload_bridges` takes `zone/vnet`, with `PVESDNUser` granted in that zone.
- The management cluster's setup takes **Time servers**. Left empty, its nodes use Talos's default servers.
- The setup's pre-flight checks storages, bridges, memory, disk space, addresses and leftover objects against Proxmox VE. A failure names the node or storage and the fix, and holds **Apply**.
- The setup warns of VMs sharing nodes, of an API address that is one node's, and of pod or service ranges that overlap a node's networks. It shows the ranges chosen.
- The management cluster's health shows when the controller cannot reach the Proxmox VE API from inside the cluster.
- A Talos node drained before its upgrade reboot records the event `TalosDrained` on its Cluster.
- The weekly template build takes a node, bridge and VLAN, and the setup checks that the storage and bridge are on that node.
- The upgrade dialog warns of deprecated APIs in use that the target version removes and of fail-closed webhooks with under two ready endpoints or all on one node.
- An automatic upgrade waits, with `AutoUpgradeSkipped` and a warning event, while the checks find something or cannot read the cluster. A deprecated API counts only if called in the last 7 days.
- `kubehenge cluster upgrade NAME --version V` prints the same list and asks before it upgrades.
- **Upgrade anyway** on the Health page, `kubehenge cluster accept-upgrade` or the annotation `kubehenge.dev/acknowledged-upgrade-findings` lets a skipped automatic upgrade go ahead for one minor version.
- A cluster on a Kubernetes minor older than any offered for new clusters gets `KubernetesOutOfSupport` and a Health warning, and the appliance's notifications repeat monthly.
- `UpgradeFindingsNextMinor` and `UpgradeFindingsNextPatch` hold what the next minor or patch upgrade would hit. Changing `spec.topology.version` in kubectl or GitOps prints them as warnings, never refusing.
- A paused cluster whose certificates run out gets `CertificatesExpiring` with the reason `Paused`, and one paused for 7 days shows **Paused**, with the date, on its Health page.
- The create wizard offers etcd snapshots on its add-ons step, on by default where the class offers a backup target. A cluster started from another copies its snapshot settings.
- Certificates take an ACME server's external account binding (`acme.externalAccountBinding.keyID`), its HMAC key stored as a credential. A missing key leaves no issuer, with a `NoACMEEABCredential` event.
- The `external` issuer (`issuerName`, `issuerKind`, `issuerGroup`) uses one you run, such as Vault, ADCS or a ClusterIssuer, in place of Kubehenge's own.
- Clusters can take their add-ons' Helm charts from a mirror per repository host, with its own CA. `kubehenge charts` lists what to copy ([install.md](install.md#mirroring-the-charts)).
- cert-manager, Velero, Argo CD, Flux, Headlamp and Rancher go out through a proxy (`egressProxy`), the management cluster's or the cluster's own. Cluster ranges, `.svc` and `.cluster.local` bypass it.
- Argo CD and Flux trust a certificate authority for the Git server (`gitops.repository.caBundle`).
- The monitoring add-on takes alert receivers (email, webhook, Slack, Teams), a remote write target and external Alertmanagers. Their secrets are stored as credentials.
- Switching monitoring off keeps its volumes, as its dialog says. The docs list what monitoring, logs and Headlamp leave behind.
- **Apply now**, `kubehenge cluster apply-addon-changes` or `kubehenge.dev/apply-addon-changes` apply waiting add-on changes at once.
- `useExisting.certManager` makes the add-ons use the cluster's own cert-manager. The condition `AddonConflict` names an add-on that clashes with the cluster's own install.
- `namespaceLabels` labels the add-ons' namespaces before their first pod. The docs list each add-on's namespaces, privileges and registries.
- The interface warns administrators once the appliance's disk is 80% full, with the `pct resize` command that grows it.
- The installer protects the appliance's container, so Proxmox VE refuses to destroy it or restore over it. The docs say how to test a backup without a second appliance.
- The interface warns administrators when the appliance's address no longer matches its certificate, with the command that renews it.
- `install.sh --certificate` points this node's Kubernetes entry at the appliance's new address and prints the command for the other nodes.
- Clusters take search domains with their name servers, so a pod resolves `git` as `git.corp.example`. The wizard, `network.searchDomains` and the management cluster's setup fill them from the node's own.
- RKE2 clusters can run their node OS in FIPS mode (`almalinux-10-fips` or `rocky-10-fips`) with `fips: true` in `nodeOS`. It needs Canal, and k3s and kubeadm refuse it.
- FIPS mode stays unsupported until end-to-end tests pass on it ([templates.md](templates.md#fips-mode)).
- Load balancing has a BGP mode for routed ranges and switches that drop gratuitous ARP, which needs a worker. kube-vip announces the API address over BGP too, except on Talos.
- BGP sessions can carry a TCP-MD5 password, stored as the cluster's credential and never in a ClusterClass, chart value or `BGPPeer`. The add-on room check counts frr-k8s.
- Every cluster gets an etcd database limit (`etcd` variable, `quotaGiB` 2 to 8, default 8) and periodic compaction, so etcd no longer refuses writes at the old 2 GiB default.
- The controller reads each etcd member's database size every 10 minutes for the cluster page. Health warns above 60% of the limit or on a `NOSPACE` alarm.
- The controller defragments etcd members one at a time, followers first, inside the upgrade window. `kubehenge cluster etcd defrag` and `kubehenge cluster etcd set --quota-gib` act at once.
- A Talos upgrade waits before a control-plane node while another etcd member is unhealthy or a learner, with a message on the cluster and a `TalosUpgradeWaiting` event.
- `nodeCustomisation` adds files, boot commands, kubelet and control-plane arguments, an audit policy and webhook to every distribution's nodes, from the wizard, **Options**, the CLI or kubectl.
- A changed Secret or ConfigMap that `nodeCustomisation` names replaces the nodes that read it. The plan and Activity name the source and the roles.
- Worker pools take labels and taints that nodes keep when replaced, set in the pool dialog, `workerNodes` or `kubehenge cluster pool set --label --taint`. Changing them replaces the nodes.
- The Health page notes a worker whose taints differ from its pool's setting.
- CIS hardening can be switched on after creation, in **Options** and with `kubehenge cluster update --cis`, but not off. The dialog lists running pods that the `restricted` level would refuse.
- Every kubeadm, RKE2 and k3s node image runs chrony, checked when the template is built.
- The Health page warns about a cluster node whose clock is over 30 seconds off the controller's.
- The cluster page names the budget and pod that hold a drain. A drain that runs out of time names the pods still holding it, and always tries once first.
- The weekly build makes the kubeadm template of each running minor's newest patch release and of the next minor, and `template_refresh.kubeadm_patches` switches this off.
- Administrators pin RKE2 and k3s releases from Administration or with `kubehenge release pin`, after seeing the checksum digests. Each pin records who added it.
- Logs go to up to four more destinations beside Loki (syslog, OTLP over HTTP or another Loki), each with a stored credential and a choice of streams under **Log outputs** in the logs card.
- The management cluster's setup takes an **MTU** for its VMs' interface, 1280 to 65520, and refuses one above the bridge's.
- A cluster's `network` variable takes an `mtu` (1280 to 65520) and `queues` (1 to 64) for its nodes' interface. The wizard and the controller refuse an MTU above the bridge's.
- A failure creating a cluster's Proxmox VE pool, user, token or volume owner records a `ProxmoxObjectsFailed` warning event with the error.
- `--node` of `kubehenge template create` and `refresh`, and the chart's `templateRefresh.node`, take several nodes. The build runs on the first that is online and whose CPU runs the node OS.
- The management cluster's setup takes an HTTP(S) proxy for every download it makes ([install.md](install.md#behind-a-proxy)). The saved password is never shown again.
- Machines on a Proxmox VE node that reboots or is in HA maintenance are not replaced for 30 minutes (`RemediationHeld`), and a host removed from the Proxmox VE cluster ends the wait.
- `hostOutageGraceMinutes` sets that wait, 30 by default, 0 for no ceiling, otherwise 5 to 10080. The wizard, **Options** and `kubehenge cluster create` and `update` (`--host-outage-grace-minutes`) set it.
- The management cluster's Health says when the controller's token lacks `Sys.Audit` on `/`, which hides HA maintenance and removed hosts.
- Storage unavailable on a node running a cluster's VMs shows in Health and holds replacement of the machines on it.
- A machine still deleting after 20 minutes gets the condition `MachineDeletionStalled`, with why.
- A stuck machine can be forgotten from the interface, `kubehenge cluster machine forget` or the annotation `kubehenge.dev/forget-machine`. That is refused while its VM runs on a node that answers.
- The controller tests every token it relies on every 10 minutes, privileges included. A failure shows in Health, goes out as a notification and gives the cluster `ProxmoxAccessRefused`.
- A user whose only second factor is a security key signs in with a Proxmox VE API token (**Use API token** on the sign-in page). The session has the token's privileges and the usual limits.
- Behind a reverse proxy listed in `--trusted-proxy` (`KUBEHENGE_TRUSTED_PROXIES`), the sign-in limits count the client in `X-Forwarded-For`. `install.sh --upgrade --no-trusted-proxy` removes the list.
- The install guide covers reverse proxies. The interface needs a host name of its own, not a path prefix, and the proxy forwards Headlamp's port 8443 too.
- A cluster whose machines would clone from a template on a down node is refused at creation. An existing one shows **Template node down** (`TemplateNodeDown`) until the node is back.
- A node image refresh skips a build whose template is on a down node. The cluster keeps its build with the condition `NodeImageHeld` until the node is back.
- A cluster's network takes up to 8 `ntp` time servers, as **Time servers** in the create wizard, a Cluster or `kubehenge cluster create -f`. Nodes fall back to the hypervisor's clock.
- A kubeadm, RKE2 or k3s node waits up to 3 minutes for chrony before its bootstrap makes certificates. It stops with `clock not synced` when neither a time server nor the host clock works.
- The Health page lists a Proxmox VE node that runs the cluster's VMs and whose clock is over 30 seconds from the controller's. Users with `Sys.Audit` on the node see it.
- A Proxmox VE ticket refused seconds after sign-in reads `Proxmox VE nodes disagree about the time`. An expired release index says to check the appliance's clock.
- The docs list UDP 123 among the nodes' outbound traffic.
- The create wizard says that volumes on node-local storage stay on one node. The docs cover moving one and the "volume node affinity conflict" error.
- The create wizard warns before submit when no node it may use has the free memory. A bridge missing on the allowed nodes names the nodes that have it.
- The volume owner VM is protected in Proxmox VE and says in its description not to delete it. A cluster whose owner VM disappeared gets a `VolumeOwnerMissing` event with the old VM ID.
- A new address of the nodes, management cluster or appliance reaches every running node's firewall through its `kubehenge` IPSet, without replacing a node ([clusters.md](clusters.md#firewall)). A DHCP renewal takes a minute.
- Security groups that would cut the appliance or management cluster off the Proxmox VE API are refused and reported by the pre-flight, with the addresses to add to the IPSet `management`.
- Clusters can be IPv6-only or dual-stack on Talos, kubeadm, RKE2 and k3s with each CNI, the families chosen at creation. Unsupported until an end-to-end test passes ([clusters.md](clusters.md#ipv6-and-dual-stack)).
- An IPv6 cluster gets pod and Service ranges of its own, `fd5e:4b1c:9a20::/56` and `fd5e:4b1c:9a21::/112` unless they clash.
- Load-balancer addresses, BGP routers and DNS servers can be IPv6, and the API address is of the primary family.
- An IPv6-only cluster sets BGP's router ID, as its nodes have no IPv4 address to use.
- The management cluster and the appliance take IPv6 and dual-stack addresses too.
- Templates take VM options (`--cpu-type`, `--ssd`, `--discard`, `--iothread`, `--numa`) from `kubehenge template create`, `refresh` and the Build template dialog's **Hardware**. New builds keep them.
- Each template records its hardware in `hw-` tags, shown in the Templates view and `kubehenge template list`.
- Placement keeps a cluster off Proxmox VE nodes that cannot run its templates' CPU type, and refuses a cluster that no node can run.
- A cluster whose templates use NUMA is refused machines of one core, through the interface, kubectl and the CLI alike.
- Health warns when an etcd member's write-ahead log syncs have a 99th percentile above 25 ms, on every distribution ([clusters.md](clusters.md#etcd-storage) has a sizing guide).
- A cluster's `securityGroups` names Proxmox VE security groups and turns on every node interface's firewall ([clusters.md](clusters.md#firewall)).
- Each VM then accepts the cluster's own traffic, the management cluster's and the appliance's, and the groups accept the rest. A group that Proxmox VE lacks is refused.
- The wizard asks for the groups in the Network step, and **Options** changes them on a running cluster, replacing every node one at a time.
- The management Health page, the create wizard, the pre-flight and `NodesSkipped` name an unreachable node with its status and reason ([Troubleshooting](troubleshooting.md#a-node-is-online-but-not-reachable)).
- A workload cluster's Health page lists such a node as a warning with its fix, and the fleet and Health tab count it.
- Nodes take registry mirrors, trusted CAs and a proxy on every distribution and node OS, set in the wizard or on the cluster page, and a change replaces the nodes one at a time ([clusters.md](clusters.md#sites-with-limited-access)).
- `registryMirrors` lists up to 4 mirror addresses per registry, with an optional certificate authority, for Talos, kubeadm, RKE2 and k3s.
- `trustedCAs` puts PEM certificates into Talos's trusted roots or the node OS's trust store, and Kubehenge refuses private keys, expired certificates and text around them.
- `egressProxy` serves nodes as well as add-ons, takes an `httpsUrl` and lets the cluster's own networks and API address bypass it.
- The interface warns while the management cluster's etcd backups are off, again each week. An administrator can dismiss it for 30 days.
- **Export** and `kubehenge appliance export` save the appliance's state, encrypted. **Restore from an export** and `kubehenge appliance import` read it back.
- **Recreate…** and `kubehenge appliance etcd-backups recreate` make the management cluster's VMs again and restore a snapshot.
- A Cluster whose VMs are all gone shows **VMs missing**. VMs that no Cluster owns show as **Unowned VMs**.
- Each cluster's spec, with its credentials, keys and certificate authorities, is stored in the backup target on every change. It is sealed by a key the appliance export carries.
- The fleet warns until the appliance has been exported, as the stored cluster specs open only with this appliance's key.
- Backup settings and `etcd-backups set-up --spec-recipient` take extra age public keys to seal specs to. `create --from --identity` opens those Secrets offline.
- `kubehenge cluster export` and `create --from` say whether Secrets are included. A Secret that cannot be opened stops `create --from` unless `--without-secrets` is given.
- `kubehenge appliance ready-for-reboot` exits 0 only when every etcd member is healthy. With `--node NODE`, it exits 0 when stopping that node's guests leaves every cluster's etcd a healthy majority.
- A blocked reboot prints its first blocker in one line, with the cluster, the member and the node.
- **Administration**, **Health** shows the same answer for each Proxmox VE node under **Ready for reboot**.
- The install guide's steps for patching Proxmox VE hosts reboot one host at a time, each once the check passes ([install.md](install.md)).
- A cluster whose API is silent for 10 minutes, with under a majority of control-plane VMs running, gets `ControlPlaneQuorumLost`. An event of that name and a Health item come with it.
- **Recover…** also works on a cluster that lost quorum, through the interface, the CLI and kubectl. It lists the control planes still running and warns that what only the lost members held is gone.
- The troubleshooting page covers recovering a cluster that lost two of three control planes, with the kubectl annotation.
- A release mirror serves the RKE2 and k3s releases in place of GitHub ([install.md](install.md#mirroring-the-releases)). Nodes, the interface and `kubehenge release pin --mirror` read from it.
- An RKE2 or k3s cluster can name its own mirror in `releaseMirror`, in the wizard or Options ([clusters.md](clusters.md#a-clusters-own-release-mirror)). It replaces the management cluster's for its nodes.
- A node whose release download fails names the file and the mirror or GitHub.
- While Proxmox VE nodes run different major versions, new VMs go to the oldest version's nodes with room, so a drain or HA move back works. Otherwise they go to newer nodes, with a Warning event.
- The Health page lists the nodes of each Proxmox VE version, as "Proxmox VE 8 and 9 nodes in one cluster". The cluster carries them in the annotation `kubehenge.dev/proxmox-versions`.
- The Health page lists a cluster whose VMs have different interface MTUs on different nodes. The network's `mtu` setting gives every VM the same size ([clusters.md](clusters.md#interface-mtu-and-queues)).
- [install.md](install.md) has an "Upgrading Proxmox VE" section.
- Clusters can run RKE2 and k3s v1.37.2, v1.36.6, v1.35.10 and v1.34.13.

### Changed

- The installer checks `--bridge` before it downloads anything, and takes a bridge, Open vSwitch bridge or SDN VNet. Without it, the installer uses the bridge of the node's default route, else `vmbr0`.
- `--ip` and `--gateway` take IPv4 only and are checked at once.
- The installer ranks storages: local block storage, then directories, NFS, CIFS and CephFS, each by free space. It leaves out storages with under 9 GiB free.
- The installer stops when the cluster has an appliance already. `--upgrade` and `--certificate` on the wrong node name the node that runs the container.
- The installer stops early and says why, without quorum, with only loopback name servers or with too little room for downloads. A failed download gives curl's exit code and the HTTP status.
- An option without its value reports `<option> needs a value`. A storage name is matched as written, and a `--ctid` another guest has is refused.
- A container that cannot resolve the API's name makes the installer try the other names and addresses the certificate covers. When none works, it names each one it tried.
- A refused or timed-out connection to the Proxmox VE API is reported as a failed connection, not an untrusted certificate.
- A VLAN tag on a VNet of a VLAN zone is refused.
- New management VMs get their MAC addresses from Proxmox VE, so two installs on one network never share them.
- A failed management cluster run's error carries OpenTofu's diagnostics, not only its exit status.
- Changing the management cluster's Proxmox VE nodes once it is Ready is refused, as its VMs would migrate.
- An answer from Proxmox VE larger than 1 MiB fails with an error naming the path.
- The pre-flight takes too little free space as an error on thick LVM and ZFS without sparse, and as a warning elsewhere.
- Deleting a machine's VM also removes it from HA resources, backup jobs and replication jobs, and drops disks left under its ID.
- A VM with Proxmox VE's protection flag is not destroyed, and the stalled deletion names the flag and `qm set <vmid> --protection 0`.
- A refusal from Proxmox VE reads "Proxmox VE refused:" with its own reason, such as "VM is locked (backup)". Only a lost connection, a timeout or a gateway error reads "did not answer".
- A refused second factor says it may be locked. A refused recovery key says the Proxmox VE cluster may lack quorum.
- The docs' example roles for cluster users hold `Pool.Audit`. A user without it is told so on the cluster page and on reboot, instead of "The machine has no VM in the cluster's pool".
- A Proxmox VE backup of every VM leaves cluster nodes out (`backup=0`) and never freezes them, as etcd snapshots and Velero cover them. Etcd snapshots use the backups' target and the browser's time zone.
- Prometheus reads every ServiceMonitor, PodMonitor, Probe, ScrapeConfig and PrometheusRule of the cluster, not only the release's.
- Monitoring installs and upgrades bring the Prometheus Operator's CRDs up to date.
- Kubehenge no longer installs cert-manager, Kyverno or monitoring over a cluster's own install.
- An appliance with a state older than the management cluster's, as after a snapshot rollback, cannot apply or remove it. Administrators take over from Administration or `kubehenge appliance take-over`.
- Switching automatic upgrades on names the cause when it fails.
- An appliance upgrade no longer fails when a proxy or mirror drops a download or answers 502, 503, 504 or 429. A slow link fails it only after two minutes without data.
- A node image refresh or hold starts as soon as the cluster's `upgrades` change, the cluster settles or its class gets a new build, not up to five minutes later.
- A template build that makes no progress for 10 minutes fails and is tried again, where it held the run for 45 minutes.
- The add-on room check counts the memory the add-ons' pods request, not only what they use, so their pods no longer stay pending.
- When Proxmox VE's download server drops the pinned container template, the installer takes the Debian 13 one from the signed template index and checks its signature.
- A failed management cluster run lists the pods it waited for and why, such as a node without the memory for one.
- Proxmox VE HA never moves cluster nodes to a host whose CPU is too old for the node OS, such as Rocky Linux 10 without x86-64-v3.
- A Talos upgrade first waits for the StatefulSets with a ready pod on the node, then drains for its drain timeout (60 minutes by default), not a fixed five minutes.
- Clusters do not move to a build whose kernel is older than the previous build's, and **Templates** marks it. The controller builds again an hour later, 3 times at most.
- The management cluster's VMs take random VM IDs from the upper half of Proxmox VE's `next-id` range (Datacenter, Options), else from 10000 to 99998. The setup stops when none is free.
- Clusters run only on the nodes that can clone their template, as one on unshared storage clones only on its own node. With none allowed, the refusal names the template, its storage and the fix.
- `kubehenge template create` and the build dialog say when the template's storage is not shared. Without `--storage`, the command takes the shared storage with the most room, else asks.
- Placement leaves out nodes where the cluster's storage is unavailable, and needs one online node that has it.
- A template build checks its storages' content types and its own `Datastore.AllocateSpace` before downloading. Without `--iso-storage` it takes the first ISO storage the token can write to.
- Template builds and new clusters check the datacenter's `user-tag-access` and `registered-tags` first, as Proxmox VE does. Tags match with their case, and only `Sys.Modify` on `/` bypasses the options.
- Applying a Cluster waits at most 2 seconds for Proxmox VE. When it is slower, the storage and template checks run as the cluster is created.
- The create wizard asks Proxmox VE for the user's privileges once, so 20 nodes and 30 VNets take 31 calls at most, not 600.
- New VMs avoid a Proxmox VE node in HA maintenance. With no node left, the refusal names why.
- The interface's certificate is valid from an hour before it is issued, so a browser whose clock runs behind accepts it.
- A cluster's bridge or VNet must exist on a node it may use, or the cluster is refused. Placement leaves out the nodes without it.
- The default StorageClass sets `discard` and `ssd`, so thin storage takes back the space of deleted data. The controller recreates a cluster's existing class once to apply it.
- The setup log names the missing `VM.GuestAgent.Audit` when Proxmox VE refuses the management VMs' addresses, then carries on.
- Placement, template builds and volume owners skip a Proxmox VE node the cluster cannot reach, such as one answering HTTP 596 or taking over 10 seconds.
- The management VMs get `startup: order=1,up=90`, so they start first after a host boots.
- The controller refuses an etcd reset to one machine unless the roll stalled or the quorum is lost, for kubectl and GitOps too. The restore status gives the reason.
- The installer is a full-screen guide on a terminal (`kubehenge install`, started by `install.sh`). It asks DHCP or static, then only what is ambiguous, and shows a summary before it changes anything.
- A container that gets no DHCP address names `--vlan` and the DHCP server, and the guide offers a static address and retries. A static address that already answers is refused before anything is created.
- The installer's steps show as a live list, and their commands' output goes to `/var/log/kubehenge-install.log`. An interrupt removes the half-made container, its disks and temporary files.
- `install.sh` only downloads and checks the release, then starts the guide. A run without a terminal names the missing `--ip` and `--gateway`.
- New Talos machines clone only baked templates. The template refresh deletes the ones made from the Image Factory once nothing waits on them.

### Fixed

- Cluster nodes start again when their Proxmox VE host reboots, as templates set `onboot` for their clones. Nodes from older templates get it once they are replaced from rebuilt ones.
- AlmaLinux and Rocky Linux template builds no longer fail in the hours after a kernel release, as the kernel's extra modules come with the kernel itself.
- Draining a node no longer waits on Kyverno's admission controllers, which run on different nodes.
- Changes, plans, worker deletions and etcd requests no longer fail with "the cluster changed meanwhile" while the cluster is busy. That message now means another user changed the cluster.
- A browser tab shown again after a while in the background no longer marks the fleet and health as stale before its first read answers.
- A cluster's VM that an administrator renamed is still destroyed with its machine.
- A VM of another name under a machine's VM ID, with no UUID to check, is no longer dropped silently. The machine stays, with a warning event and a condition naming the VM ID and the VM found.
- Privileges granted through a pool, or by an ACL without propagate, count as held. Storages, VNets and guests reached that way no longer show as out of reach.
- A sign-in name over 64 characters, realm included, is refused at once with the limit, not with an error that read like an outage.
- A user name with an `@` of its own, such as an Active Directory principal name, signs in with the chosen realm.
- Certificates from Sectigo, DigiCert, ZeroSSL, Google Trust Services and step-ca no longer fail with `externalAccountRequired`.
- Activity no longer misses a kubectl change that the interface first reads after the API server restarted or right after the management cluster's setup.
- An appliance whose certificate has expired upgrades again, where its health check rolled back every upgrade.
- After a restore, the Administration page says that automatic upgrades lost their key, and switching them off and on works again.
- `install.sh --upgrade --proxmox-url <URL>` moves the appliance to another address of the Proxmox VE API, checked as at install.
- An upgrade first checks for free space of about three times what is installed, so a full disk no longer leaves the old software half replaced.
- An upgrade cut off by a reboot or a kill is undone at the interface's next start, or when `install.sh --upgrade` runs again, where its status said "under way" for ever.
- After a restore to another ID or a clone, the backup check and the interface's commands still find the appliance. A guest ID that is gone no longer counts as backed up.
- The demo's health pages read each sample cluster's control plane without "no matches for kind" errors.
- A bucket with object lock no longer keeps two locked copies of each etcd snapshot, and pruning deletes all of a snapshot's objects.
- A management cluster run whose sign-in runs out keeps its state, so the next run no longer fails on pools it had made.
- Stopping the appliance during a management cluster run (migration, stop-mode backup, reboot) gives OpenTofu up to 150 seconds to save its state. The run then fails and says why.
- A VM renamed in Proxmox VE is renamed back within 15 seconds with a `VMRenamedBack` event, so Kubernetes no longer deletes its node as gone when it is next not ready.
- A worker moved to another Proxmox VE node gets its `topology.kubernetes.io/zone` label updated and its CSI plugin pod restarted, and new node-local volumes are created on the right host.
- A cluster storage outside the management cluster's `workload_datastores` is refused, naming the setting to add it to, where the clusters' tokens failed with a permission error.
- A Proxmox VE node that shows online but whose status cannot be read is left out of placement and counted in `NodesSkipped`, where it stopped every cluster from being placed.
- A cluster whose name matches a Proxmox VE pool holding other VMs is refused instead of taking the pool over.
- A machine still finds its VM when Proxmox VE lists the VM without a name, as when its node drops out of the statistics. One with no VM shows "Not reporting".
- Machines being deleted on a Proxmox VE node that left the cluster for good no longer stay in Deleting. Kubehenge lets go of them, and the Cluster gets the event `NodeGone`.
- An etcd restore goes on without control-plane VMs that no longer exist in Proxmox VE. With none left, it says to rebuild with `kubehenge cluster create --from <file>`.
- A tab whose session another tab replaced asks to retry a refused change. It returns to sign-in when the other tab signed in as someone else.
- OpenID sign-in works from any name the interface answers to, where the provider's return lost the state cookie.
- A template on a down Proxmox VE node no longer fails the Templates page, the template refresh or the create wizard's versions. The page names each such template.
- A Talos node boots after 5 minutes without time sync, where it waited for ever and was replaced again and again. The Health page says `clock not synced`.
- New machines are no longer refused with "cannot reserve … of memory" on an overcommitted host. Placement skips nodes without room for the largest machine, and sets `NodesLackMemory` when none has it.
- A removed machine waits at most 60 minutes for its node to drain and 10 for its volumes to detach, where a rollout could stall for ever. `kubehenge cluster timeouts` changes both.
- Deleting a cluster looks only at the storages it used, on the nodes where they are active, so offline nodes no longer block it. The stall message says what abandoning leaves behind.
- Deleting a namespace that holds a cluster no longer hangs for hours.
- A machine's VM is cloned once, also when the management cluster's API server is slow. No stopped second clone is left to keep a deletion from removing the pool.
- While Proxmox VE refuses HA rules until its HA groups are migrated, Kubehenge sets `HARulesPending` and tries again after 5 minutes. It writes none while nodes run different major versions.
- A kubeadm control plane is ready as soon as its first worker joins, not up to 10 minutes later. A cluster without workers no longer reports its control planes as not ready.
- Workers of a Calico or Cilium cluster need room for the pods the CNI runs once per cluster, as the interface and the controller both ask.
- The release archive carries the module's OpenTofu providers, so the appliance applies the management cluster without downloads, on a closed network too.
- A template build waits up to an hour for a publisher's checksums and their signature to match, rather than failing at once.
- After a k3s or RKE2 upgrade, a cluster keeps the admission policy of the distribution's Gateway API chart. Helm could delete it while upgrading the Traefik CRD chart.
- Nodes pull up to three images at once, so a pull stuck on a slow registry no longer holds the later ones, and kubeadm nodes cancel a stalled request after five minutes. Nodes roll to pick this up.
- Links in a release's notes on GitHub open the docs page on kubehenge.dev, not a missing page.
- Links on kubehenge.dev to a heading with an apostrophe, such as "Back up the management cluster's etcd", reach the heading.

### Security

- The health identity may read `/metrics`, webhook configurations and endpoint slices for the upgrade checks.
- The installer sets up Debian's daily security updates in the container, through its proxy, and `install.sh --upgrade` adds them to appliances that lack them.
- Release files that do not match their pins are refused before anything is installed.
- Cluster API's token holds the module's own roles, with only the privileges the provider's calls need ([install.md](install.md#what-cluster-apis-token-may-do)). It loses `VM.Migrate`, `VM.Snapshot` and others that Proxmox VE's roles granted.
- Talos templates are baked on your Proxmox VE host from Sidero's images, each pinned by digest. They are no longer imported from the Image Factory without a checksum.
- Only pinned Talos releases are offered and admitted for a new cluster or a change of release.

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
