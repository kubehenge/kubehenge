# Upgrading

Kubehenge itself, the management cluster and the workload clusters are upgraded separately, in this order.

1. **Kubehenge itself** is the appliance's software, upgraded from the Administration page or the Proxmox VE shell.
2. **The management cluster** is Talos, Kubernetes, Cluster API and its providers, and Kubehenge's ClusterClasses and controller.
   - Applying it again moves them to the versions the new release carries.
3. **Workload clusters** upgrade Kubernetes in each cluster, by hand or automatically inside a maintenance window.
   - The nodes' operating system moves to each week's build of the VM templates inside the same window.

Before 1.0.0, a minor release may change behaviour. Read its entry in the [changelog](changelog.md) first.

## Kubehenge

### From the Administration page

**Administration** in the top bar opens the Administration page, for those with the [setup privileges](install.md#who-may-set-it-up) on `/`. Its **Kubehenge** box shows:

- **Installed** is the running release.
- **Newer releases** lists those published since, or why they are unknown (the release index could not be fetched or checked).
- **Last upgrade** gives from which release to which, with the time. It also says when the installed release is already the newest allowed, when one is in progress, or why it failed.

**Upgrade to `<version>`** installs the newest release, once confirmed. The interface restarts and is unavailable for about a minute. No upgrade starts while the management cluster is being applied or removed or a VM template is being built. The restart signs everyone out. A page left open across an upgrade says so and offers to reload, since it still runs the old interface.

### Automatically

**Automatic upgrades** in the same box:

| Setting | Upgrades to |
| --- | --- |
| off | nothing (the default) |
| patch releases | the newest release of the installed minor version, such as 0.1.x |
| minor releases | the newest release of the installed major version, such as 0.x |

**Hour** picks the hour of the day (03:00 by default), in the **Time zone** below it. The zone starts out as the browser's when automatic upgrades are switched on. The interface checks once a day in that hour, never during a run of the management cluster or a template build.

Only Kubehenge's own software is upgraded. The workload clusters are not touched. The management cluster follows only where its own automatic upgrades are on ([below](#automatically-after-kubehenge)).

### What is installed

The interface does not install releases itself. It writes a request that `kubehenge-update`, a service running as root, carries out.

- It fetches the release index, `https://kubehenge.dev/releases/index.json`, and its signatures. It accepts the index only if one of Kubehenge's release keys signed it and it has not expired.
  - The public keys are built into each release and cannot be configured.
  - The index expires two weeks after it was signed, and is signed again every week ([releasing.md](releasing.md#the-index-appliances-read)).
  - A mirror of the index and its signatures can be named with `KUBEHENGE_RELEASE_INDEX=<URL>` in the appliance's `/etc/kubehenge/appliance.env`. Both the interface and the updater read it and apply the same checks, so a mirror has to copy the index at least weekly.
  - Appliances see a new release only once the signed index lists it.
- It installs only a release named in that index, newer than the installed one, and only an archive matching the checksum in the index. It also installs the OpenTofu release that release names, checked the same way.
- It swaps the files by rename and restarts the interface. It holds the lock the management cluster's runs take, so an upgrade never cuts a run off and no run starts during an upgrade.
- It keeps the installed release aside until the new one serves.
  - The interface has to answer on its own certificate within 2 minutes and keep running for 20 seconds more.
  - If it does not, or the restart fails, the updater restores the previous release and restarts it. The upgrade's error then reads `release … did not start: …; rolled back to …`.

### From the Proxmox VE shell

`install.sh` upgrades an installed appliance too. In the shell of the node the appliance runs on, as root:

```sh
curl -fsSLO https://github.com/kubehenge/kubehenge/releases/latest/download/install.sh && sh install.sh --upgrade --ctid <ctid>
```

- It checks every download against the checksums it carries, as when installing ([install.md](install.md#install-the-appliance)).
- `--release <file> --sha256 <hex>` installs a local archive instead.
- It refuses a container that is not Kubehenge's appliance (tagged `kubehenge`), and a release older than the installed one. The same release may be installed again.
- It keeps the appliance's state, settings and certificate. It also keeps a newer OpenTofu that the appliance's own upgrades brought.

#### CA and certificate options

- `--proxmox-ca <file>` replaces the CA the appliance checks the Proxmox VE API against. Use it after the API's certificate was replaced with one from another CA ([install.md](install.md#options)).
  - It stops, changing nothing, unless the API's certificate is one that CA signed.
  - A file holding several certificates is reduced to those the API's certificate rests on.
  - It also turns certificate verification back on where `--proxmox-insecure` turned it off.
  - Apply the management cluster again afterwards, so that it and the clusters check the API against the new CA as well.
- `--proxmox-insecure` has the appliance stop verifying the Proxmox VE API's certificate.
  - Applying the management cluster again afterwards carries it to the management cluster and the clusters.
- Without either option the appliance keeps what it had.

#### While it runs

- It stops, changing nothing, while the management cluster is being applied or removed, a VM template is being built, or the appliance is being upgraded. Run it again once that has finished.
- It keeps the installed release aside until the new one serves, with the same check as the updater's.
  - Otherwise it puts the release before back, restarts it and stops with `release … did not start: …; rolled back to …`.

### From the appliance's shell

`kubehenge appliance` inside the appliance does what the Administration page does, as a client of its interface. It signs in with a Proxmox VE user, so the same privileges and refusals apply. `--user` names the user (or `KUBEHENGE_USER`), and `--password-file` a file holding the password (or `KUBEHENGE_PASSWORD`). `--totp` gives a second factor's code.

```sh
pct exec <ctid> -- kubehenge appliance upgrades --user root@pam --password-file /root/password
pct exec <ctid> -- kubehenge appliance upgrade 0.2.0 --user root@pam --password-file /root/password
```

`upgrades` shows the installed release, the newer ones and the last upgrade, with the reason when it failed or was rolled back. `--json` prints the interface's answer as it is. `upgrade` asks the updater for a release, as **Upgrade to** does. `kubehenge appliance health` runs the check the updater and `install.sh` use, without signing in.

## The management cluster

Each Kubehenge release pins the versions of Talos, Kubernetes, Cluster API, every provider and cert-manager together, as the set it has tested. Upgrading Kubehenge does not change the running management cluster. Applying it again does.

### In the appliance

On the Administration page, the **Management cluster** section shows the settings of the last run. **Apply again…** asks for the state passphrase given when the management cluster was created and applies them as they are. **Edit settings** opens them for changes first, applied with the same passphrase. Applying again:

- upgrades Talos node by node, each drained first when there are several;
- upgrades Kubernetes component by component;
- upgrades each Cluster API provider through the Cluster API Operator, and Kubehenge's ClusterClasses and controller;
- puts back anything that drifted from the settings.

#### When it is behind

Every apply records the Kubehenge release that ran it. The section shows it as **Version**. Once Kubehenge has been upgraded past that release, the top of the Administration page says `Management cluster upgrade: 0.1.0 to 0.2.0`.

**Upgrade…** beside it lists the components whose versions change, from the old to the new. The list covers Talos, Kubernetes, Cluster API and its providers, cert-manager and the Cluster API Operator. It also covers Kubehenge's ClusterClasses and controller (by its chart's version). It asks for the state passphrase and applies the settings as they are, as **Apply again…** does.

#### What a running management cluster keeps

Node addresses can only be added, after the existing ones. A single node grows to three by adding two addresses after its own. A node given another address is cut off from Talos and etcd. A removed node leaves its etcd member behind.

The API address, gateway, pod and service ranges and the VM disks' storage cannot change. The interface shows them read-only and refuses such changes. With Terraform/OpenTofu nothing refuses them. Only append to `node_addresses`, and leave `api_address`, `gateway`, `pod_subnets`, `service_subnets` and `vm_datastore` as they are.

Anything else can change, such as the nodes the VMs run on, the bridge, DNS servers, registry mirrors, identity providers and what clusters may use.

#### When a release changes a ClusterClass

Applying such a release rolls the clusters of that class. A ClusterClass's machine and bootstrap templates are named after their contents. Cluster API replaces the nodes of every cluster whose class now points at new templates, one at a time as for an upgrade. Each cluster therefore needs [spare addresses](install.md#network-and-addresses).

The [changelog](changelog.md) says which classes a release changes. Apply such a release when the rollouts suit you.

#### Moving nodes

The same page can move the management cluster to other Proxmox VE nodes or add nodes to it. A VM given another node is migrated there.

Applying again runs with your Proxmox VE session, like the first run, and takes about as long. The page follows the log. If it fails, the page shows the error and the log. Fix the cause and apply again.

### Automatically after Kubehenge

**Management cluster**, under the automatic upgrades in the Kubehenge box, is **Upgrade by hand** by default. Set to **Upgrade automatically**, the appliance applies the management cluster again on its own. It does so once it runs a newer Kubehenge release than the one that last applied it. The run happens:

- in the hour Kubehenge's own upgrades use (**Hour**, **Time zone**), whether or not those are on;
- at most once a day, never during another run or a template build;
- only after any Kubehenge upgrade asked for in the same hour has begun, so the run applies the newer release;
- as an ordinary run. The page follows it and marks it as started automatically.

A run that fails stays on the page with its error and log. It is tried again in the next day's hour, not before. **Upgrade…** runs it at once.

#### What the appliance keeps

Nobody is signed in when the run starts. Switching it on has the appliance keep the two things an apply needs, which it keeps at no other time:

- **the state passphrase**, asked for and checked against the state when switching it on.
- **an API token of a Proxmox VE user of its own**, `kubehenge-upgrader@pve`, with the Administrator role on `/`.
  - An apply creates users, roles, ACLs, pools and VMs, as the first run does.
  - Switching it on creates the user and its token with your session. Because the token holds `Sys.Console` on `/`, you need it too, besides the [setup privileges](install.md#who-may-set-it-up).

#### Sealed storage

Both are kept sealed (AES-256-GCM) in a file in the appliance's state directory. Only the interface's own user, and root, can read it. The key that opens them is made anew each time it is switched on. It is kept on a volume of its own (`/var/lib/kubehenge-keys`), which Proxmox VE's backups of the container leave out (`backup=0`). A backup holds the state and the sealed file, never what opens them.

An appliance restored from a backup shows the setting as on. Its next automatic run fails saying the key is gone. Switch it off and on again.

#### What that risks

Anyone who gets root on the running appliance, or takes over its interface, can still read the management cluster's state. The state holds its admin kubeconfig and talosconfig. They can also act as an administrator of the whole Proxmox VE cluster with the token. Switching it off deletes the file and the key first, then the user with its token. Removing the management cluster does the same.

### With Terraform/OpenTofu

Take the module from the new release's archive and apply it with the same variables and the same state:

```sh
tofu init -upgrade
tofu apply
```

`version_overrides` holds a component back below the tested set, by the keys in `versions.tf`. Lowering a version on a running cluster is not supported. Changing `node_cores` or `node_memory_mb` updates the VMs without rebooting them. Each node takes the new size at its next reboot. A Talos upgrade reboots one node at a time.

## Workload clusters

### By hand

**Upgrade** on a cluster's page offers the newest release of its own minor version, when that is newer than the one running. It also offers the newest of the next minor version. Both come in the distribution's own form (`v1.37.1`, `v1.37.1+rke2r1`, `v1.37.1+k3s1`). RKE2's and k3s's come from their release channels, and kubeadm's from the versions its VM templates are built for. Talos's come from Kubernetes' own, among the minor versions the cluster's Talos release runs.

When there is neither, the version can be typed in. For kubeadm, the dialog asks for a template of a newer version first. The interface refuses skipping a minor version or going back.

Cluster API replaces the control-plane nodes one by one, then the workers. Each new node is created before an old one is removed. With kubectl, set `spec.topology.version` ([clusters.md](clusters.md#day-two)).

#### Per distribution

- **kubeadm** nodes boot from a template built for their exact Kubernetes version. Create the template of the new version first ([templates.md](templates.md)).
- **RKE2 and k3s** install the requested version when a node boots, from the same template.
- **Talos** nodes run the Kubernetes version Talos is told to, from the template of the cluster's Talos release.
  - The new nodes move to that template with the upgrade once it is in the pool.
  - A Kubernetes version the Talos release does not run needs a Talos upgrade first ([clusters.md](clusters.md#talos-upgrades)).

### Automatically inside a window

**Edit** beside Automatic upgrades, in a cluster's **Options**, sets its `upgrades` variable. The create wizard can switch it on from the start, with the default window in the browser's time zone.

#### Fields

| Field | Variable | What it is |
| --- | --- | --- |
| Upgrade automatically | `mode` | `off` (the default). `patch` takes patch releases, the newest release of the cluster's minor version. `minor` takes patch and minor releases, so also the next minor version. |
| Update node images | `nodeImages` | kubeadm, RKE2 and k3s. On by default. Replaces the nodes with clones of each new template build once it is a day old, whatever `mode` says ([below](#the-nodes-operating-system)). |
| Apply Talos patch releases | `nodeImages` | Talos. On by default. Upgrades the nodes in place to the newest patch release of their Talos minor version ([clusters.md](clusters.md#talos-upgrades)). |

#### Window

| Field | Variable | What it is |
| --- | --- | --- |
| Days | `window.weekdays` | Days of the week the window opens on. |
| Days of the month | `window.monthDays` | Such as `1, 15`. A day listed here or in Days counts. Every day when both are empty. |
| From | `window.start` | When the window opens, `HH:MM`. `02:00` by default. |
| Duration (hours) | `window.durationMinutes` | How long it stays open, 30 minutes to 24 hours in half hours. Four hours by default. A window may run past midnight. |
| Time zone | `window.timeZone` | An IANA time zone, such as `Europe/Berlin`. The variable defaults to `UTC`. Switching automatic upgrades on in the dialog, or in the create wizard, starts from the browser's zone. |

#### When it starts

Kubehenge's controller looks at every cluster every five minutes. It starts an upgrade only:

- inside the cluster's window;
- while the cluster is available, with every node at the same version and no rollout running;
- one step at a time.
  - In `patch` mode the step goes to the newest release of the cluster's minor version.
  - In `minor` mode it goes to the newest release of the next minor version once the distribution's stable release has reached it.
  - Otherwise `minor` mode works as `patch` mode does.
  - The next step waits until the cluster is settled again.

#### Where the versions come from

| Distribution | Newest releases and stable minor version |
| --- | --- |
| RKE2, k3s | the distribution's own release channels. The management cluster's nodes need to reach them. |
| kubeadm | the kubeadm templates on Proxmox VE. The newest version a template exists for. Building a template of a newer version offers it. |
| Talos | Kubernetes' own newest release of each minor version (`dl.k8s.io`). The stable one is what the Talos release Kubehenge carries installs by default. Never one the cluster's Talos release does not run, and never while its nodes are being upgraded. |

Cluster API then rolls the nodes as for any upgrade. A rollout that has started finishes even if the window closes. The last automatic upgrade shows in the cluster's **Summary** as **Last automatic upgrade**, and is recorded on the Cluster in the annotation `kubehenge.dev/auto-upgraded`.

### The nodes' operating system

The management cluster builds the node OS VM templates again every week, with the updates each node OS published since ([templates.md](templates.md#the-weekly-build)). Changing a cluster's node OS (its `nodeOS` variable, under **Options**) replaces its nodes the same way, one at a time. It happens at once rather than in the window, once the new node OS's template is built.

#### Refreshing nodes

Inside each cluster's window, under the same conditions as an upgrade, the controller looks at each cluster with `nodeImages` on. It moves the nodes to the newest build that is at least a day old. It sets the cluster's node image time (the annotation `kubehenge.dev/node-image`) to now. Every machine template then selects that build. Cluster API replaces the nodes one at a time.

A Kubernetes upgrade due at the same time comes first. The nodes it creates are already clones of the newest build.

#### Last and next refresh

The last move shows in the cluster's **Summary** as **Last node refresh**, recorded in the annotation `kubehenge.dev/node-image-refreshed`.

While a newer build waits for the window, the annotation `kubehenge.dev/node-image-held` names the build and when the window opens, such as `build-202610180100 at 2026-10-24T00:00:00Z`. The **Summary** shows it as **Next node refresh**. It is gone once the move starts.

#### Holding and deleting builds

To hold a cluster on its build, switch `nodeImages` off. Deleting a broken build keeps the clusters that have not moved to it yet from moving. Neither the Administration page nor `kubehenge template delete … --build` deletes a build a cluster runs. Both name the clusters instead. For a build found broken that clusters already run, `--roll-clusters` deletes it anyway.

Those clusters' nodes are replaced at once, outside their windows, with clones of the build before it. The last build of a template is never deleted while a cluster runs it.

### What holds an upgrade back

- **Rancher.** The cluster that runs Rancher stays within the Kubernetes versions Rancher's release runs on.
  - An upgrade beyond them, by hand or automatic, waits (the cluster's conditions say why) until a Kubehenge release brings a Rancher that runs there.
- **A missing kubeadm template.** New kubeadm nodes cannot be created without the template of their version ([troubleshooting.md](troubleshooting.md#template-missing)).
- **No spare addresses.** Each rollout creates a node before removing one ([troubleshooting.md](troubleshooting.md#node-addresses-run-out)).
