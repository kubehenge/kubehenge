# Upgrading

Kubehenge itself, the management cluster and the workload clusters are upgraded separately, in this order.

1. **Kubehenge itself:** the appliance's software, from the Administration page or the Proxmox VE shell.
2. **The management cluster:** Talos, Kubernetes, Cluster API and its providers, and Kubehenge's ClusterClasses and controller, to the versions the new release carries, by applying it again.
3. **Workload clusters:** Kubernetes in each cluster, by hand or automatically inside a maintenance window, and the nodes' operating system, which moves to each week's build of the VM templates inside the same window.

Before 1.0.0, a minor release may change behaviour: read its release notes ([changelog](changelog.md)) first.

## Kubehenge

### From the Administration page

**Administration** in the top bar opens the Administration page, for those with the [setup privileges](install.md#who-may-set-it-up) on `/`. Its **Kubehenge** box shows:

- **Installed:** the running release.
- **Newer releases:** those published since, or why they are unknown (the release index could not be fetched or checked).
- **Last upgrade:** from which release to which, that the installed release is already the newest allowed, that one is in progress, or why it failed, with the time.

**Upgrade to `<version>`** installs the newest release, once confirmed. The interface restarts, so it is unavailable for about a minute. No upgrade starts while the management cluster is being applied or removed or a VM template is being built. The restart signs everyone out. A page left open across an upgrade says so and offers to reload, since it still runs the old interface.

### Automatically

**Automatic upgrades** in the same box:

| Setting | Upgrades to |
| --- | --- |
| off | nothing (the default) |
| patch releases | the newest release of the installed minor version, such as 0.1.x |
| minor releases | the newest release of the installed major version, such as 0.x |

**Hour** picks the hour of the day (03:00 by default), in the **Time zone** below it, which starts out as the browser's when automatic upgrades are switched on. The interface checks once a day in that hour, never during a run of the management cluster or a template build, and only Kubehenge's own software is upgraded: the workload clusters are not touched, and the management cluster follows only where its own automatic upgrades are on ([below](#automatically-after-kubehenge)).

### What is installed

The interface never replaces software itself. It drops a request into a spool, and a separate service running as root, `kubehenge-update`, carries it out:

- It fetches the release index, `https://kubehenge.dev/releases/index.json`, and its signatures, and accepts the index only if one of Kubehenge's release keys signed it and it has not expired. The public keys are built into each release and cannot be configured. The index expires two weeks after it was signed, and is signed again every week ([releasing.md](releasing.md#the-index-appliances-read)), so that nobody can keep an appliance on an old index, unaware of a newer release, for longer. A mirror of the index and its signatures can be named with `KUBEHENGE_RELEASE_INDEX=<URL>` in the appliance's `/etc/kubehenge/appliance.env`, which both the interface and the updater read; the checks are the same, so a mirror has to copy the index at least weekly.
- It installs only a release named in that index, newer than the installed one, and only an archive matching the checksum in the index. It also installs the OpenTofu release that Kubehenge release names, checked the same way.
- It swaps the files by rename and restarts the interface. It holds the lock the management cluster's runs take, so an upgrade never cuts a run off and no run starts during an upgrade.
- It runs in a sandbox: of root's capabilities it keeps only the one that reads the request the interface's user wrote, under a system-call filter, with the file system read-only but for the software, its service units and its spool.
- It keeps the installed release aside until the new one serves: the interface has to answer on its own certificate within 2 minutes and keep running for 20 seconds more. If it does not, or the restart fails, the updater puts the release before back, restarts it, and says so in the upgrade's error: "release … did not start: …; rolled back to …".

A release published without its signed index upgrades no appliance.

### From the Proxmox VE shell

`install.sh` upgrades an installed appliance too. In the shell of the node the appliance runs on, as root:

```sh
curl -fsSLO https://github.com/kubehenge/kubehenge/releases/latest/download/install.sh && sh install.sh --upgrade --ctid <ctid>
```

- It checks every download against the checksums it carries, as when installing ([install.md](install.md#install-the-appliance)); `--release <file> --sha256 <hex>` installs a local archive instead.
- It refuses a container that is not Kubehenge's appliance (tagged `kubehenge`), and a release older than the installed one. The same release may be installed again.
- It keeps the appliance's state, settings and certificate, and keeps a newer OpenTofu that the appliance's own upgrades brought.
- `--proxmox-ca <file>` replaces the CA the appliance checks the Proxmox VE API against, after the API's certificate was replaced with one from another CA ([install.md](install.md#options)); it stops, changing nothing, unless the API's certificate is one that CA signed. A file holding several certificates is reduced to those the API's certificate rests on. It also turns certificate verification back on where `--proxmox-insecure` turned it off. Apply the management cluster again afterwards, so that it and the clusters check the API against the new CA as well.
- `--proxmox-insecure` has the appliance stop verifying the Proxmox VE API's certificate; applying the management cluster again afterwards carries it to the management cluster and the clusters. Without either option the appliance keeps what it had.
- It stops, changing nothing, while the management cluster is being applied or removed, a VM template is being built, or the appliance is being upgraded; run it again once that has finished.
- It keeps the installed release aside until the new one serves, with the same check as the updater's: the interface has to answer on its own certificate within 2 minutes and keep running for 20 seconds more. Otherwise it puts the release before back, restarts it and stops with "release … did not start: …; rolled back to …".

### From the appliance's shell

`kubehenge appliance` inside the appliance does what the Administration page does, as a client of its interface: it signs in with a Proxmox VE user, so the same privileges and refusals apply. `--user` names the user (or `KUBEHENGE_USER`), and `--password-file` a file holding the password (or `KUBEHENGE_PASSWORD`); `--totp` gives a second factor's code.

```sh
pct exec <ctid> -- kubehenge appliance --user root@pam --password-file /root/password upgrades
pct exec <ctid> -- kubehenge appliance --user root@pam --password-file /root/password upgrade 0.2.0
```

`upgrades` shows the installed release, the newer ones and the last upgrade, with the reason when it failed or was rolled back; `--json` prints the interface's answer as it is. `upgrade` asks the updater for a release, as **Upgrade to** does. `kubehenge appliance health` runs the check the updater and `install.sh` use, without signing in.

## The management cluster

Each Kubehenge release pins the versions of Talos, Kubernetes, Cluster API, every provider and cert-manager together, as the set it has tested. Upgrading Kubehenge does not change the running management cluster; applying it again does.

### In the appliance

On the Administration page, the **Management cluster** section shows the settings of the last run. **Apply again…** asks for the state passphrase given when the management cluster was created and applies them as they are; **Edit settings** opens them for changes first, applied with the same passphrase. Applying again:

- upgrades Talos node by node, each drained first when there are several;
- upgrades Kubernetes component by component;
- upgrades each Cluster API provider through the Cluster API Operator, and Kubehenge's ClusterClasses and controller;
- puts back anything that drifted from the settings.

**When it is behind.** Every apply records the Kubehenge release that ran it, which the section shows as **Version**. Once Kubehenge has been upgraded past that release, the top of the Administration page says so: "Management cluster upgrade: 0.1.0 to 0.2.0". **Upgrade…** beside it lists the components whose versions change, from the old to the new (Talos, Kubernetes, Cluster API and its providers, cert-manager, the Cluster API Operator, and Kubehenge's ClusterClasses and controller by its chart's version), asks for the state passphrase, and applies the settings as they are, as **Apply again…** does.

**What a running management cluster keeps.** Its nodes keep their addresses, and nodes are only added after them: a single node grows to three by adding two addresses after its own, but none is taken away, since a node given another address is cut off from Talos and etcd, and a node removed leaves its etcd member behind. The API address, gateway, pod and service ranges and the VM disks' storage stay as they were. The appliance refuses such changes. The page shows the API address, gateway, pod and service ranges and VM disks read-only, and keeps Node addresses editable only for adding addresses after the existing ones. Anything else, such as the nodes the VMs run on, the bridge, DNS servers, registry mirrors, identity providers and what clusters may use, can change. With Terraform/OpenTofu the same holds, but nothing refuses it: do not change `node_addresses`, `api_address`, `gateway`, `pod_subnets`, `service_subnets` or `vm_datastore` of a running management cluster, and only append to `node_addresses`.

**When a release changes a ClusterClass**, applying it rolls the clusters of that class: a ClusterClass's machine and bootstrap templates are named after their contents, and Cluster API replaces the nodes of every cluster whose class now points at new templates, one at a time as for an upgrade, so each cluster needs [spare addresses](install.md#network-and-addresses). The [changelog](changelog.md) says which classes a release changes; apply such a release when the rollouts suit you.

The same page can move the management cluster to other Proxmox VE nodes or add nodes to it: a VM given another node is migrated there.

It runs with your Proxmox VE session, like the first run, and takes about as long. The page follows the log. If it fails, the page shows the error and the log; fix the cause and apply again.

### Automatically after Kubehenge

**Management cluster**, under the automatic upgrades in the Kubehenge box, is **Upgrade by hand** by default. Set to **Upgrade automatically**, the appliance applies the management cluster again on its own once it runs a newer Kubehenge release than the one that last applied it:

- in the hour Kubehenge's own upgrades use (**Hour**, **Time zone**), whether or not those are on;
- at most once a day, never during another run or a template build, and only after an upgrade of Kubehenge asked for in the same hour, which brings what it moves to;
- as a run like any other, which the page follows, shown as started automatically.

A run that fails stays on the page with its error and log, and is tried again in the next day's hour, not before; **Upgrade…** runs it at once.

Nobody is signed in then, so switching it on has the appliance keep what an apply needs, which it otherwise never does:

- **the state passphrase**, asked for and checked against the state when switching it on. No key derived from it can stand in: OpenTofu derives a new key for every state it writes.
- **an API token of a Proxmox VE user of its own**, `kubehenge-upgrader@pve`, with the Administrator role on `/`: an apply creates users, roles, ACLs, pools and VMs, as the first run does. Switching it on creates the user and its token with your session, so it takes `Sys.Console` on `/` of you besides the [setup privileges](install.md#who-may-set-it-up), as the token holds it.

Both are kept sealed (AES-256-GCM) in a file in the appliance's state directory, which only the interface's own user, and root, can read. The key that opens them is made anew each time it is switched on and kept on a volume of its own (`/var/lib/kubehenge-keys`), which Proxmox VE's backups of the container leave out (`backup=0`): a backup holds the state and the sealed file, never what opens them. An appliance restored from a backup shows the setting as on, and its next automatic run fails saying the key is gone; switch it off and on again. **What that risks:** anyone who gets root on the running appliance, or takes over its interface, can still read the management cluster's state, which holds its admin kubeconfig and talosconfig, and act as an administrator of the whole Proxmox VE cluster with the token. Switching it off deletes the file and the key first, then the user with its token; removing the management cluster does the same.

### With Terraform/OpenTofu

Take the module from the new release's archive and apply it with the same variables and the same state:

```sh
tofu init -upgrade
tofu apply
```

`version_overrides` holds a component back below the tested set, by the keys in `versions.tf`; lowering a version on a running cluster is not supported. Changing `node_cores` or `node_memory_mb` updates the VMs without rebooting them; each node takes the new size at its next reboot, which a Talos upgrade does one node at a time.

## Workload clusters

### By hand

**Upgrade** on a cluster's page offers the newest release of its own minor version, when that is newer than the one running, and the newest of the next minor version, in the distribution's own form (`v1.37.1`, `v1.37.1+rke2r1`, `v1.37.1+k3s1`): RKE2's and k3s's from their release channels, kubeadm's from the VM templates there are, Talos's from Kubernetes' own, among the minor versions the cluster's Talos release runs. When there is neither, the version can be typed in; for kubeadm, the dialog asks for a template of a newer version first. The interface refuses skipping a minor version or going back. Cluster API replaces the control-plane nodes one by one, then the workers, each new node created before an old one is removed. With kubectl, set `spec.topology.version` ([clusters.md](clusters.md#day-two)).

- **kubeadm** nodes boot from a template built for their exact Kubernetes version: create the template of the new version first ([templates.md](templates.md)).
- **RKE2 and k3s** install the requested version when a node boots, from the same template.
- **Talos** nodes run the Kubernetes version Talos is told to, from the template of the cluster's Talos release, which the new nodes move to with the upgrade once it is in the pool. A Kubernetes version the Talos release does not run needs a Talos upgrade first ([clusters.md](clusters.md#talos-upgrades)).

### Automatically inside a window

**Edit** beside Automatic upgrades, in a cluster's **Options**, sets its `upgrades` variable; the create wizard can switch it on from the start, with the default window in the browser's time zone:

| Field | Variable | What it is |
| --- | --- | --- |
| Upgrade automatically | `mode` | `off` (the default); `patch` releases: the newest release of the cluster's minor version; `minor`, patch and minor releases: also the next minor version. |
| Update node images | `nodeImages` | On by default, for kubeadm, RKE2 and k3s: replace the nodes with clones of each new build of their VM template once it is a day old, whatever `mode` says ([below](#the-nodes-operating-system)). On Talos, **Apply Talos patch releases**: upgrade the nodes in place to the newest patch release of their Talos minor version ([clusters.md](clusters.md#talos-upgrades)). |
| Days | `window.weekdays` | Days of the week the window opens on. |
| Days of the month | `window.monthDays` | Such as `1, 15`. A day listed here or in Days counts; every day when both are empty. |
| From | `window.start` | When the window opens, `HH:MM`; `02:00` by default. |
| Duration (hours) | `window.durationMinutes` | How long it stays open: 30 minutes to 24 hours, in half hours; four hours by default. A window may run past midnight. |
| Time zone | `window.timeZone` | An IANA time zone, such as `Europe/Berlin`. The variable defaults to `UTC`; switching automatic upgrades on in the dialog, or in the create wizard, starts from the browser's zone. |

Kubehenge's controller looks at every cluster every five minutes. It starts an upgrade only:

- inside the cluster's window;
- while the cluster is available, with every node at the same version and no rollout running;
- one step at a time: in `patch` mode to the newest release of the cluster's minor version; in `minor` mode to the newest release of the next minor version once the distribution's stable release has reached it, and otherwise as in `patch` mode. The next step waits until the cluster is settled again.

Where the versions come from:

| Distribution | Newest releases and stable minor version |
| --- | --- |
| RKE2, k3s | the distribution's own release channels; the management cluster's nodes need to reach them |
| kubeadm | the kubeadm templates on Proxmox VE: the newest version a template has been built for, so building a newer template is what offers one |
| Talos | Kubernetes' own newest release of each minor version (`dl.k8s.io`), the stable one being what the Talos release Kubehenge carries installs by default; never one the cluster's Talos release does not run, and never while its nodes are being upgraded |

Cluster API then rolls the nodes as for any upgrade. A rollout that has started finishes even if the window closes. The last automatic upgrade shows in the cluster's **Summary** as **Last automatic upgrade**, and is recorded on the Cluster in the annotation `kubehenge.dev/auto-upgraded`.

### The nodes' operating system

The management cluster builds the node OS VM templates again every week, with the updates each node OS published since ([templates.md](templates.md#the-weekly-build)). Changing a cluster's node OS (its `nodeOS` variable, under **Options**) replaces its nodes the same way, one at a time, at once rather than in the window, once the new node OS's template is built. Inside each cluster's window, under the same conditions as an upgrade, the controller moves the nodes of a cluster with `nodeImages` on to the newest build that is at least a day old: it sets the cluster's node image time (the annotation `kubehenge.dev/node-image`) to now, which has every machine template select that build, and Cluster API replaces the nodes one at a time. A Kubernetes upgrade due at the same time comes first; the nodes it creates are already clones of the newest build. The last move shows in the cluster's **Summary** as **Last node refresh**, recorded in the annotation `kubehenge.dev/node-image-refreshed`. While a newer build waits for the window, the controller records that it holds the cluster, with the build and when the window opens, in the annotation `kubehenge.dev/node-image-held` (such as `build-202610180100 at 2026-10-24T00:00:00Z`), which the **Summary** shows as **Next node refresh**; it is gone once the move starts.

Holding a cluster on its build is switching `nodeImages` off. Deleting a build that turns out broken stops the clusters that have not moved to it yet. Neither the Administration page nor `kubehenge template delete … --build` deletes a build a cluster runs; both name the clusters instead. For a build found broken that clusters already run, `--roll-clusters` deletes it anyway, and those clusters' nodes are replaced at once, outside their windows, with clones of the build before it. The last build of a template is never deleted while a cluster runs it.

### What holds an upgrade back

- **Rancher.** The cluster that runs Rancher stays within the Kubernetes versions Rancher's release runs on. An upgrade beyond them, by hand or automatic, waits (the cluster's conditions say why) until a Kubehenge release brings a Rancher that runs there.
- **A missing kubeadm template.** New kubeadm nodes cannot be created without the template of their version ([troubleshooting.md](troubleshooting.md#template-missing)).
- **No spare addresses.** Each rollout creates a node before removing one ([troubleshooting.md](troubleshooting.md#node-addresses-run-out)).
