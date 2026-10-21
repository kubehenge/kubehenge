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

Creating is idempotent: a template with the same tags in the template pool is found and reused. One with those tags in another pool is not, since clusters cannot clone from it. A failed run removes what it made; `--keep-on-failure` leaves the VM for inspection, and a later run refuses to start until it is removed.

## How clusters find them

Each template carries a unique set of Proxmox VE tags, such as `kubehenge;talos;talos-v1.14.4`, and Cluster API's Proxmox VE provider selects it by that exact set. The template has no network card, one core and 512 MiB: each clone gets the network, size and disk its machine asks for.

Clones keep the template's CPU type, `x86-64-v2-AES` unless `--cpu-type` names another. That type runs on any node, so a VM can migrate between nodes of different CPUs, but it hides the CPU's own model, and a node's kernel then takes it to be open to Meltdown and MDS and runs their mitigations, which cost the most on Intel hosts. `--cpu-type host` shows the clones the node's own CPU, at the cost of migrating only between nodes with the same one.

Templates should be on shared storage in a Proxmox VE cluster, so that clones can start on any node.

## Requirements

- The storage given with `--import-storage` (`local` by default) must allow the `import` content type.
- The API token needs to download to that storage, create VMs in the template pool (`--pool`, `kubehenge-templates` by default) and write to the ISO storage.

## List and delete

```sh
kubehenge template list
kubehenge template delete kubeadm --kubernetes-version v1.37.1
```

Both work in the template pool, `--pool` as for create; `--pool ''` takes in every pool.
