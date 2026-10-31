# VM templates

Every cluster's nodes are clones of a Proxmox VE VM template. `kubehenge template create` makes the template a distribution needs, through the Proxmox VE API alone. It needs no SSH to the Proxmox VE host and no build tools beside the `kubehenge` command. In the appliance, administrators build and delete them on the Administration page instead, with their own session ([install.md](install.md#create-the-vm-templates)).

## Per distribution

| Distribution | Template | One per |
| --- | --- | --- |
| Talos | the Talos Image Factory's image of the release with the QEMU guest agent extension, imported as it is. Talos is its own OS, so no node OS is part of it. Talos clusters' new machines clone it and are then upgraded in place to the cluster's release | Talos release |
| RKE2, k3s | a node OS with cloud-init, the QEMU guest agent and the distribution's SELinux policy. The distribution installs itself when a node boots, from files checked against the checksums recorded for its release ([architecture.md](architecture.md#each-clusters-own-corner-of-proxmox-ve)) | distribution and node OS |
| kubeadm | a node OS with cloud-init, the QEMU guest agent and containerd. The kubelet and kubeadm come from pkgs.k8s.io as Kubernetes' installation guide sets them up. The control-plane images are pulled ahead, each kept only once Kubernetes' release signature for it checks out. containerd is set to kubeadm's pause image | node OS and Kubernetes version |

## Node OSes

The nodes of RKE2, k3s and kubeadm clusters run a node OS. Each cluster chooses it with its `nodeOS` variable, in the wizard or under **Options**. Changing it replaces every node with a clone of the new node OS's template, one at a time as in an upgrade. That template has to be built first.

| Node OS | Built as | CPU type at the least | |
| --- | --- | --- | --- |
| AlmaLinux 10 | image mode: a bootable container image (bootc) made of AlmaLinux's own packages, `/usr` read-only | `x86-64-v2-AES` | the default |
| Rocky Linux 10 | image mode, made of Rocky Linux's own packages, `/usr` read-only | `x86-64-v3` | |
| openSUSE Leap 16.1 Immutable | transactional: a read-only root that only `transactional-update` changes, as a new snapshot | `x86-64-v2-AES` | not offered until openSUSE releases Leap 16.1 |

### What a node holds

Every node OS holds only what a node needs:

- Its family's minimal system, cloud-init and the QEMU guest agent.
- The SELinux policy packages and the kernel modules a pod network bridges through.
- For kubeadm, containerd and the kubelet's tools.
- For RKE2 and k3s, their prerequisites and SELinux policy.

`/usr/local` is writable for them to install into at boot (in image mode it lives in `/var`, labelled as `/usr/local` would be).

Nothing on a node updates itself. A new build of its template brings the updates ([below](#the-weekly-build)).

### SELinux

SELinux enforces on every node, with the targeted policy, never permissive. containerd runs with its SELinux support on, so pods run confined as `container_t`. RKE2 and k3s are started with `selinux: true` and their own policy (`rke2-selinux`, `k3s-selinux`).

On kubeadm nodes, kube-vip and the control plane's static pods run as `spc_t` through kubeadm patches. The static pods are etcd, the API server, the controller manager and the scheduler.

The type `spc_t` is for containers that manage the host. These pods read the node's certificates and keep etcd's data on the host. The policy keeps both from confined containers. Every other pod stays confined.

### The guest agent

The guest agent runs the commands Proxmox VE sends it (guest-exec) as root unconfined, as a root shell would. Cluster API's Proxmox VE provider asks cloud-init through it whether a node has booted. The controller's etcd restore runs through it.

Proxmox VE lets only holders of `VM.GuestAgent.Unrestricted` (or `VM.Monitor`) send them. Those holders could run anything in the VM anyway.

#### The boot log

The agent also hands out the node's boot log to holders of `VM.GuestAgent.FileRead`. The log is cloud-init's output and a copy of its boot's journals (cloud-init, containerd, the kubelet, RKE2 or k3s). A timer writes the copy into `/var/log/kubehenge` every 15 seconds from 5 seconds after boot, the last 200 lines of each. It is readable by root alone.

The node's SELinux policy lets the agent read those files and no others of `/var/log`. Every build proves through Proxmox VE that it hands them out ([troubleshooting.md](troubleshooting.md#a-nodes-boot-log)).

### CPU type

A node OS whose CPU type is above `x86-64-v2-AES` stops at boot on an older CPU. The template is built only on a node whose CPU reaches that level. Clusters of it are placed only on such nodes. A cluster is refused that node OS when none of the Proxmox VE nodes it may use has one.

### Image and package checks

Each Kubehenge release makes templates from one build of each node OS's cloud image. The image is checked against its publisher's signature over the image's checksum before Proxmox VE downloads it. The publisher's key is embedded in Kubehenge and known by its fingerprint.

#### Packages

Every package a build installs is signed, and checked against a key accepted only with its known fingerprint. The keys are the family's own, pkgs.k8s.io's for kubeadm and Rancher's for the RKE2 and k3s policies. For kubeadm's containerd, which the EL families do not carry, the key is EPEL's (on AlmaLinux, AlmaLinux's own build of EPEL for `x86-64-v2`).

#### Repository indexes

The indexes of pkgs.k8s.io's and Rancher's repositories are signed and checked too. k3s publishes its policy for EL 10 only in its testing channel, whose index it signs. Its stable channel has only EL 9's, unsigned. An image-mode build takes `bootc-base-imagectl`, which composes the minimal system, and its manifests from CentOS Stream's bootc image, pinned by digest.

## Create

```sh
export KUBEHENGE_PROXMOX_URL=https://pve.example.org:8006
export KUBEHENGE_PROXMOX_TOKEN='root@pam!kubehenge=...'
kubehenge template create talos --node pve1
kubehenge template create rke2 --node pve1 --storage shared-storage
kubehenge template create k3s --node-os rocky-10 --node pve1 --storage shared-storage
kubehenge template create kubeadm --kubernetes-version v1.36.5 --node pve1
```

### Building a node OS template

`--node-os` chooses the node OS, `almalinux-10` unless set. A Talos template takes none, because Talos is its own OS. A node OS template is built by a VM of its own, which boots the family's cloud image. In image mode the VM composes the node image from the family's packages and checks it with `bootc container lint`. It installs the image onto the template's disk, then boots it once on its own.

A transactional node OS installs what a node needs as a new snapshot and reboots into it.

That VM needs to reach the package repositories. It uses DHCP on `--bridge`, or a static address given with `--build-address`, `--build-gateway` and `--build-dns`. Its cloud-init data comes on an ISO uploaded to `--iso-storage` (the node's first ISO storage by default) and removed afterwards.

### Reuse and failures

Creating is idempotent. A template with the same tags in the template pool is found and reused. For a node OS template, any build of it is reused ([Builds](#builds)). One with those tags in another pool is not, since clusters cannot clone from it. `--new-build` makes a new build of a node OS template even when one exists.

A failed run removes what it made. `--keep-on-failure` leaves the VM for inspection, and a later run refuses to start until it is removed.

## How clusters find them

Each template carries a unique set of Proxmox VE tags, such as `kubehenge;talos;talos-v1.14.5` or `kubehenge;os-almalinux-10;rke2`. Cluster API's Proxmox VE provider selects it by that exact set. The template has no network card, one core and 512 MiB. Each clone gets the network, size and disk its machine asks for.

### Builds

A node OS template is a line of builds. Each build is a template of its own, named after the time it was made, such as `kubehenge-rke2-almalinux-10-build-202610180100`. It is tagged with that time (`build-202610180100`) beside the line's tags.

Each cluster has a node image time. Its machines are clones of the newest build of their line made by then. The same cluster therefore always gets the same build until that time moves. It moves only when the nodes are refreshed ([clusters.md](clusters.md#node-image-refresh)). Scaling up or replacing a failed node therefore clones the build the other nodes run.

Talos templates have no builds. Talos nodes take their updates by upgrading Talos.

### CPU type of clones

Clones keep the template's CPU type. A new build takes that of the newest build of its line. A line's first build takes the least its node OS runs on (`x86-64-v2-AES`, or `x86-64-v3` for Rocky Linux 10). `--cpu-type` names another instead.

That type runs on any node, so a VM can migrate between nodes of different CPUs. It hides the CPU's own model. A node's kernel then takes the CPU to be open to Meltdown and MDS and runs their mitigations. These cost the most on Intel hosts. `--cpu-type host` shows the clones the node's own CPU, at the cost of migrating only between nodes with the same one.

## The weekly build

While the management cluster's template refresh is on, a CronJob in it builds every node OS template again each week. The refresh is on by default. The CronJob runs Sunday at 01:00 UTC unless the management cluster's `template_refresh` settings (`weekday`, `time`, `time_zone`) say otherwise. Each build has the updates its node OS published since. The CronJob rebuilds:

- RKE2's on the default node OS,
- every RKE2 and k3s template in the template pool, and
- the kubeadm template of each Kubernetes version and node OS a kubeadm cluster runs.

### Cleanup

Then it deletes the builds nothing needs. Of each line it keeps the two newest. It also keeps any older one a cluster's machines still run or a machine about to be created will clone from. A kubeadm line no cluster runs or can upgrade to keeps only its newest build.

A build that fails leaves the others to go on. The next run starts over. The Administration page shows how the last run went with its log, and has Build now.

### The command

The CronJob runs `kubehenge template refresh` with an API token of its own, made for it alone ([install-settings.md](install-settings.md#what-the-template-builds-may-do)). One run at a time holds a Lease, so the schedule, Build now and a cluster creator's request never build at once.

```sh
kubehenge template refresh --node pve1 --storage shared-storage --import-storage local --pool kubehenge-templates
kubehenge template refresh --missing kubeadm-v1.36.5-almalinux-10 --missing-only --node pve1
```

`kubehenge template refresh` reads the clusters from the Kubernetes cluster its kubeconfig or service account names. `--keep` and `--soak` set how many builds stay and how old a build is before clusters move to it.

### Talos templates

It also makes the Talos template of each release a Talos cluster runs or boots that the pool lacks. It never builds a Talos template again, since a release's image does not change.

### Kept Talos templates

Each Talos release's template stands for a build of its minor version's line. Of each minor version the two newest releases' templates stay. Any older one stays too if:

- a cluster boots or is upgraded to it,
- a machine about to be created selects it, or
- the run was asked to make it.

The others are deleted. A cluster creator's request names a Talos template as `talos-v1.14.5`.

## Requirements

- In a Proxmox VE cluster, templates should go on shared storage, so that clones can start on any node.
- The storage given with `--import-storage` (`local` by default) must allow the `import` content type.
- The API token needs what the weekly build's own token holds ([install-settings.md](install-settings.md#what-the-template-builds-may-do)):
  - On the template pool (`--pool`, `kubehenge-templates` by default), propagated: `Pool.Audit`, `VM.Allocate`, `VM.Audit`, `VM.Config.CDROM`, `VM.Config.CPU`, `VM.Config.Disk`, `VM.Config.HWType`, `VM.Config.Memory`, `VM.Config.Network`, `VM.Config.Options`, `VM.GuestAgent.Unrestricted` and `VM.PowerMgmt`.
  - On the storages of `--import-storage` and `--iso-storage`: `Datastore.Allocate`, `Datastore.AllocateTemplate` and `Datastore.Audit`.
  - On the storage of `--storage`: `Datastore.AllocateSpace` and `Datastore.Audit`.
  - On `/nodes/<node>` of `--node`: `Sys.AccessNetwork` and `Sys.Audit`.
  - On `/sdn/zones/localnetwork/<bridge>` of `--bridge`: the role `PVESDNUser`.
  - On `/`, and on `/nodes` propagated: `Sys.Audit`.

  A token of a user with the Administrator role on `/`, such as `root@pam`'s in the examples, holds all of them.

## List and delete

```sh
kubehenge template list
kubehenge template delete kubeadm --kubernetes-version v1.36.5
kubehenge template delete rke2 --build build-202610110100
```

Both work in the template pool, `--pool` as for create. `--pool ''` covers every pool. Delete takes every build of a node OS template, `--node-os` as for create, or the one `--build` names.

Delete refuses a build a cluster's machines are cloned from, as the Administration page does, and names the clusters. It deletes nothing then. It reads which clusters those are from the management cluster `KUBECONFIG` points at (`--class-namespace` if Kubehenge is not in `kubehenge-system`).

`--roll-clusters` deletes such a build anyway, for one found broken. The clusters that run it have their nodes replaced at once, outside their maintenance windows, with clones of the build before it. The last build of a template is never deleted while a cluster runs it.
