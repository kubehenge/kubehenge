# VM templates

Every cluster's nodes are clones of a Proxmox VE VM template. `kubehenge template create` makes the template a distribution needs, through the Proxmox VE API alone: no SSH to the Proxmox VE host and no build tools beside the `kubehenge` command. In the appliance, administrators build and delete them on the Administration page instead, with their own session ([install.md](install.md#create-the-vm-templates)); this page describes the command and what both do.

| Distribution | Template | One per |
| --- | --- | --- |
| Talos | the Talos Image Factory's image with the QEMU guest agent extension; made by the command only, ahead of Talos clusters, which no ClusterClass offers yet | Talos release |
| RKE2, k3s | a node OS with cloud-init, the QEMU guest agent and the distribution's SELinux policy; the distribution installs itself when a node boots, from files checked against the checksums recorded for its release ([architecture.md](architecture.md#each-clusters-own-corner-of-proxmox-ve)) | distribution and node OS |
| kubeadm | a node OS with cloud-init, the QEMU guest agent, containerd, the kubelet and kubeadm from pkgs.k8s.io as Kubernetes' installation guide sets them up, and the control-plane images pulled ahead, each kept only once Kubernetes' release signature for it checks out, with containerd set to kubeadm's pause image | node OS and Kubernetes version |

## Node OSes

The nodes of RKE2, k3s and kubeadm clusters run a node OS, which each cluster chooses with its `nodeOS` variable, in the wizard or under **Options**. Changing it replaces every node with a clone of the new node OS's template, one at a time as in an upgrade; that template has to be built first.

| Node OS | Built as | CPU type at the least | |
| --- | --- | --- | --- |
| AlmaLinux 10 | image mode: a bootable container image (bootc) made of AlmaLinux's own packages, `/usr` read-only | `x86-64-v2-AES` | the default |
| Rocky Linux 10 | image mode, made of Rocky Linux's own packages, `/usr` read-only | `x86-64-v3` | |
| openSUSE Leap 16.1 Immutable | transactional: a read-only root that only `transactional-update` changes, as a new snapshot | `x86-64-v2-AES` | not offered until openSUSE releases Leap 16.1 |

Every node OS holds only what a node needs: its family's minimal system, cloud-init, the QEMU guest agent, the SELinux policy packages and the kernel modules a pod network bridges through, for kubeadm containerd and the kubelet's tools, for RKE2 and k3s their prerequisites and SELinux policy, with `/usr/local` writable for them to install into at boot (in image mode it lives in `/var`, labelled as `/usr/local` would be). SELinux enforces on every node, with the targeted policy, never permissive: containerd runs with its SELinux support on, so pods run confined as `container_t`, and RKE2 and k3s are started with `selinux: true` and their own policy (`rke2-selinux`, `k3s-selinux`). On kubeadm nodes the control plane's static pods (etcd, the API server, the controller manager and the scheduler) and kube-vip run as `spc_t`, the type for containers that manage the host, through kubeadm patches: they read the node's certificates and keep etcd's data on the host, which the policy keeps from confined containers. Every other pod stays confined. The guest agent runs the commands Proxmox VE sends it (guest-exec) as root unconfined, as a root shell would, rather than in its own confined domain: Cluster API's Proxmox VE provider asks cloud-init through it whether a node has booted, and the controller's etcd restore runs through it. Proxmox VE lets only holders of `VM.GuestAgent.Unrestricted` (or `VM.Monitor`) send them, who could run anything in the VM anyway. Nothing on a node updates itself: a new build of its template brings the updates ([below](#the-weekly-build)).

A node OS whose CPU type is above `x86-64-v2-AES` stops at boot on an older CPU. The template is built only on a node whose CPU reaches that level, clusters of it are placed only on such nodes, and a cluster is refused that node OS when none of the Proxmox VE nodes it may use has one.

Each Kubehenge release makes templates from one build of each node OS's cloud image, checked against its publisher's signature over the image's checksum before Proxmox VE downloads it, with the publisher's key embedded in Kubehenge and known by its fingerprint. Every package a build installs is signed, and checked against a key accepted only with its known fingerprint: the family's own, pkgs.k8s.io's for kubeadm, Rancher's for the RKE2 and k3s policies, and EPEL's for kubeadm's containerd, which the EL families do not carry (on AlmaLinux, AlmaLinux's own build of EPEL for `x86-64-v2`). The indexes of pkgs.k8s.io's and Rancher's repositories are signed and checked too; k3s publishes its policy for EL 10 only in its testing channel, whose index it signs, while its stable channel has only EL 9's, unsigned. An image-mode build takes the tool that composes the minimal system, and its manifests, from CentOS Stream's bootc image, pinned by digest.

## Create

```sh
export KUBEHENGE_PROXMOX_URL=https://pve.example.org:8006
export KUBEHENGE_PROXMOX_TOKEN='root@pam!kubehenge=...'
kubehenge template create talos --node pve1
kubehenge template create rke2 --node pve1 --storage shared-storage
kubehenge template create k3s --node-os rocky-10 --node pve1 --storage shared-storage
kubehenge template create kubeadm --kubernetes-version v1.37.1 --node pve1
```

`--node-os` chooses the node OS, `almalinux-10` unless set. A node OS template is built by a VM of its own, which boots the family's cloud image: in image mode it composes the node image from the family's packages, checks it with `bootc container lint` and installs it onto the template's disk, then boots it once on its own; a transactional node OS installs what a node needs as a new snapshot and reboots into it. That VM needs to reach the package repositories: it uses DHCP on `--bridge`, or a static address given with `--build-address`, `--build-gateway` and `--build-dns`. Its cloud-init data comes on an ISO uploaded to `--iso-storage` (the node's first ISO storage by default) and removed afterwards.

Creating is idempotent: a template with the same tags in the template pool is found and reused, and for a node OS template, any build of it (see below). One with those tags in another pool is not, since clusters cannot clone from it. `--new-build` makes a new build of a node OS template even when one exists. A failed run removes what it made; `--keep-on-failure` leaves the VM for inspection, and a later run refuses to start until it is removed.

## How clusters find them

Each template carries a unique set of Proxmox VE tags, such as `kubehenge;talos;talos-v1.14.4` or `kubehenge;os-almalinux-10;rke2`, and Cluster API's Proxmox VE provider selects it by that exact set. The template has no network card, one core and 512 MiB: each clone gets the network, size and disk its machine asks for.

A node OS template is a line of builds, each a template of its own named after the time it was made, such as `kubehenge-rke2-almalinux-10-build-202610180100`, and tagged with that time (`build-202610180100`) beside the line's tags. Each cluster has a node image time: its machines are clones of the newest build of their line made by then, so the same cluster always gets the same build until that time moves. It moves only when the nodes are refreshed ([clusters.md](clusters.md#node-image-refresh)), so scaling up or replacing a failed node clones the build the other nodes run. Talos templates have no builds; Talos nodes take their updates by upgrading Talos.

Clones keep the template's CPU type. A new build takes that of the newest build of its line, a line's first the least its node OS runs on (`x86-64-v2-AES` for most), unless `--cpu-type` names another. That type runs on any node, so a VM can migrate between nodes of different CPUs, but it hides the CPU's own model, and a node's kernel then takes it to be open to Meltdown and MDS and runs their mitigations, which cost the most on Intel hosts. `--cpu-type host` shows the clones the node's own CPU, at the cost of migrating only between nodes with the same one.

## The weekly build

While the management cluster's template refresh is on, which it is by default, a CronJob in it builds every node OS template again each week, Sunday at 01:00 UTC unless set otherwise, with the updates its node OS published since: RKE2's on the default node OS, every RKE2 and k3s template in the template pool, and the kubeadm template of each Kubernetes version and node OS a kubeadm cluster runs. Then it deletes the builds nothing needs: of each line it keeps the two newest, and any older one a cluster's machines still run or a machine about to be created will clone from. A kubeadm line no cluster runs or can upgrade to keeps only its newest build. A build that fails leaves the others to go on, and the next run starts over; the Administration page shows how the last run went with its log, and has Build now.

The CronJob runs `kubehenge template refresh` with an API token of its own, made for it alone ([install.md](install.md#what-the-template-builds-may-do)). One run at a time holds a Lease, so the schedule, Build now and a cluster creator's request never build at once.

```sh
kubehenge template refresh --node pve1 --storage shared-storage --import-storage local --pool kubehenge-templates
kubehenge template refresh --missing kubeadm-v1.38.0-almalinux-10 --missing-only --node pve1
```

It reads the clusters from the Kubernetes cluster its kubeconfig or service account names; `--keep` and `--soak` set how many builds stay and how old a build is before clusters move to it.

Templates should be on shared storage in a Proxmox VE cluster, so that clones can start on any node.

## Requirements

- The storage given with `--import-storage` (`local` by default) must allow the `import` content type.
- The API token needs to download to that storage, create VMs in the template pool (`--pool`, `kubehenge-templates` by default) and write to the ISO storage.

## List and delete

```sh
kubehenge template list
kubehenge template delete kubeadm --kubernetes-version v1.37.1
kubehenge template delete rke2 --build build-202610110100
```

Both work in the template pool, `--pool` as for create; `--pool ''` takes in every pool. Delete takes every build of a node OS template, `--node-os` as for create, or the one `--build` names.

Delete refuses a build a cluster's machines are cloned from, as the Administration page does, and names the clusters; it deletes nothing then. It reads which clusters those are from the management cluster `KUBECONFIG` points at (`--class-namespace` if Kubehenge is not in `kubehenge-system`). `--roll-clusters` deletes such a build anyway, for one found broken: the clusters that run it have their nodes replaced at once, outside their maintenance windows, with clones of the build before it. The last build of a template is never deleted while a cluster runs it.
