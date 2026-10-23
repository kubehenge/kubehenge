# VM templates

Every cluster's nodes are clones of a Proxmox VE VM template. `kubehenge template create` makes the template a distribution needs, through the Proxmox VE API alone: no SSH to the Proxmox VE host and no build tools beside the `kubehenge` command. In the appliance, administrators build and delete them on the Administration page instead, with their own session ([install.md](install.md#create-the-vm-templates)); this page describes the command and what both do.

| Distribution | Template | One per |
| --- | --- | --- |
| Talos | the Talos Image Factory's image with the QEMU guest agent extension; made by the command only, ahead of Talos clusters, which no ClusterClass offers yet | Talos release |
| RKE2, k3s | Ubuntu's cloud image with every update and the QEMU guest agent; the distribution installs itself when a node boots, from files checked against the checksums recorded for its release ([architecture.md](architecture.md#each-clusters-own-corner-of-proxmox-ve)) | Ubuntu release |
| kubeadm | the same, plus containerd, the kubelet and kubeadm from pkgs.k8s.io as Kubernetes' installation guide sets them up, with the control-plane images pulled ahead, each kept only once Kubernetes' release signature for it checks out, and containerd set to kubeadm's pause image | Ubuntu release and Kubernetes version |

Each Kubehenge release makes Ubuntu templates from one build of each Ubuntu release's cloud image, checked against Ubuntu's signed checksums before Proxmox VE downloads it; the updates a bake installs come from Ubuntu's archive, which apt checks against its signed index. The pkgs.k8s.io signing key is accepted only with its known fingerprint.

## Create

```sh
export KUBEHENGE_PROXMOX_URL=https://pve.example.org:8006
export KUBEHENGE_PROXMOX_TOKEN='root@pam!kubehenge=...'
kubehenge template create talos --node pve1
kubehenge template create rke2 --node pve1 --storage shared-storage
kubehenge template create kubeadm --kubernetes-version v1.37.1 --node pve1
```

Ubuntu-based templates are prepared by booting the image once. That VM needs to reach Ubuntu's and Kubernetes' package mirrors: it uses DHCP on `--bridge`, or a static address given with `--build-address`, `--build-gateway` and `--build-dns`. Its cloud-init data comes on an ISO uploaded to `--iso-storage` (the node's first ISO storage by default) and removed afterwards.

Creating is idempotent: a template with the same tags in the template pool is found and reused, and for an Ubuntu template, any build of it (see below). One with those tags in another pool is not, since clusters cannot clone from it. `--new-build` makes a new build of an Ubuntu template even when one exists. A failed run removes what it made; `--keep-on-failure` leaves the VM for inspection, and a later run refuses to start until it is removed.

## How clusters find them

Each template carries a unique set of Proxmox VE tags, such as `kubehenge;talos;talos-v1.14.4`, and Cluster API's Proxmox VE provider selects it by that exact set. The template has no network card, one core and 512 MiB: each clone gets the network, size and disk its machine asks for.

An Ubuntu template is a line of builds, each a template of its own named after the time it was made, such as `kubehenge-ubuntu-26.04-build-202610180100`, and tagged with that time (`build-202610180100`) beside the line's tags. Each cluster has a node image time: its machines are clones of the newest build of their line made by then, so the same cluster always gets the same build until that time moves. It moves only when the nodes are refreshed ([clusters.md](clusters.md#node-image-refresh)), so scaling up or replacing a failed node clones the build the other nodes run. Talos templates have no builds; Talos nodes take their updates by upgrading Talos.

Clones keep the template's CPU type. A new build takes that of the newest build of its line, a line's first `x86-64-v2-AES`, unless `--cpu-type` names another. That type runs on any node, so a VM can migrate between nodes of different CPUs, but it hides the CPU's own model, and a node's kernel then takes it to be open to Meltdown and MDS and runs their mitigations, which cost the most on Intel hosts. `--cpu-type host` shows the clones the node's own CPU, at the cost of migrating only between nodes with the same one.

## The weekly build

While the management cluster's template refresh is on, which it is by default, a CronJob in it builds every Ubuntu template clusters run again each week, Sunday at 01:00 UTC unless set otherwise, with the updates Ubuntu published since: the cloud image the RKE2 and k3s clusters share, and the kubeadm template of each Kubernetes version a kubeadm cluster runs. Then it deletes the builds nothing needs: of each line it keeps the two newest, and any older one a cluster's machines still run or a machine about to be created will clone from. A kubeadm line no cluster runs or can upgrade to keeps only its newest build. A build that fails leaves the others to go on, and the next run starts over; the Administration page shows how the last run went with its log, and has Build now.

The CronJob runs `kubehenge template refresh` with an API token of its own, made for it alone ([install.md](install.md#what-the-template-builds-may-do)). One run at a time holds a Lease, so the schedule, Build now and a cluster creator's request never build at once.

```sh
kubehenge template refresh --node pve1 --storage shared-storage --import-storage local --pool kubehenge-templates
kubehenge template refresh --missing kubeadm-v1.38.0 --missing-only --node pve1
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

Both work in the template pool, `--pool` as for create; `--pool ''` takes in every pool. Delete takes every build of an Ubuntu template, or the one `--build` names.

Delete refuses a build a cluster's machines are cloned from, as the Administration page does, and names the clusters; it deletes nothing then. It reads which clusters those are from the management cluster `KUBECONFIG` points at (`--class-namespace` if Kubehenge is not in `kubehenge-system`). `--roll-clusters` deletes such a build anyway, for one found broken: the clusters that run it have their nodes replaced at once, outside their maintenance windows, with clones of the build before it. The last build of a template is never deleted while a cluster runs it.
