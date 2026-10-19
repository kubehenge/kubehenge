# Clusters

Every cluster Kubehenge creates is a Cluster API Cluster built from one of Kubehenge's ClusterClasses: kubeadm, RKE2 or k3s. Its VMs live in a Proxmox VE pool of their own under the workload pool, `<workload pool>/<namespace>.<name>`; a cluster `prod` created in the interface lands in `kubehenge/clusters.prod`.

Every cluster gets, whatever its distribution:

- **The Proxmox VE cloud controller manager**, which initialises new nodes and labels them with their region (the Proxmox VE cluster's name, or the node's on a single node outside any cluster) and zone (the node).
- **The Proxmox CSI plugin**, with a default StorageClass `proxmox`: ext4 volumes on the cluster's storage, deleted when released.
- **A control-plane virtual IP**, held by kube-vip on one control-plane node and moved to another when it fails.
- **A CNI**: the distribution's own, or the one chosen at creation.
- **Health checks.** A node that stays not ready for five minutes, or does not join within twenty, is replaced; workers one at a time.
- **Control-plane nodes kept for the control plane.** Once a worker has joined, the control-plane nodes carry the taint `node-role.kubernetes.io/control-plane:NoSchedule` in every distribution, so workloads run on the workers and leave etcd and the API server their nodes. A cluster scaled to no workers has the taint taken off, and its control-plane nodes run the workloads. A worker that fails leaves the taint in place. Pods that started on a control-plane node before the first worker joined stay there until they are next scheduled.
- **Its own Proxmox VE user and API token**, with rights on its own pool only, held by the cloud controller manager and the CSI plugin's controller on the control-plane nodes ([architecture.md](architecture.md#each-clusters-own-corner-of-proxmox-ve)).

Before the first cluster, the VM templates for its distribution have to exist ([install.md](install.md#create-the-vm-templates)).

## Create a cluster in the interface

**Create cluster** on the cluster list opens a wizard of six steps: General, Network, Machines, Add-ons, Identity providers and Confirm, which sums up every setting. A field says what is wrong with it once it has been changed, or when **Next** is pressed with something missing, which then moves to the first such field. Choosing another distribution brings its defaults only into what you have not set yet. **Cancel** asks before it discards what was entered. A step in the bar above opens only once the steps before it are complete. Without `VM.Allocate` on the workload pool, **Create cluster** is off and says so.

### General

| Field | What it is |
| --- | --- |
| Name | Lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit, at most 40 characters. It also names the cluster's Proxmox VE pool and VMs. |
| Distribution | kubeadm, RKE2 or k3s: one per ClusterClass in the management cluster. A distribution whose VM template is not in the template pool yet says so, and the cluster cannot be created until an administrator builds it ([templates.md](templates.md)). |
| Kubernetes version | For RKE2 and k3s, the newest release of each minor version Kubernetes still patches (the four newest, which takes in the one Kubernetes keeps a few weeks after a release) from their release channels, the stable one chosen first; a cluster already on an older minor version can still be upgraded from it; for kubeadm, the newest version of each minor version a VM template has been built for, the newest chosen first. They are in the distribution's own form: `v1.36.5` for kubeadm, `v1.36.5+rke2r1` for RKE2, `v1.36.5+k3s1` for k3s. When the list cannot be read, the version is typed in. |
| Control-plane nodes | 1, 3, 5 or 7. Three or more keep the cluster running when one fails. |
| Workers | 0 to 100. With none, the control-plane nodes run the workloads too. |

### Network

| Field | What it is |
| --- | --- |
| Bridge | The bridge the cluster's VMs attach to: one of those chosen for clusters when the management cluster was set up. Bridges you may not use (no `SDN.Use`) are shown but disabled; when only one is left to choose, it is chosen. When the management cluster names none and Proxmox VE lists no bridges to you (no `Sys.Audit` on the nodes), you type the name in. |
| VLAN tag | Optional, 1 to 4094. |
| API address | The control plane's virtual IP: an unused address in the nodes' network, outside the node addresses. |
| Node addresses | Where the nodes take their addresses from: ranges (`192.0.2.21-192.0.2.29`), networks or single addresses inside the gateway's network, one per line or separated by commas. The wizard asks for at least one address more than there are nodes, for the extra node an upgrade or a replacement adds; leave more for scaling up. |
| Prefix length | Of the nodes' network. |
| Gateway | The nodes' default gateway. |
| DNS servers | IPv4 addresses, one per line or separated by commas. |
| Pod network | The CNI, [below](#pod-network). |
| Pod range, Service range | Optional, under **Pod and service ranges**. |

The nodes' network is the gateway with the prefix length. Pod and service ranges must stay clear of it and of each other, or traffic for the nodes would be routed into the cluster. Kubehenge sets both ranges on every cluster it creates: those you give, refused if they overlap; otherwise the distribution's own when they are clear of the nodes' network (`10.244.0.0/16` and `10.96.0.0/12` for kubeadm, `10.42.0.0/16` and `10.43.0.0/16` for RKE2 and k3s); otherwise the first of a few private ranges that are clear.

#### Pod network

The CNI, chosen now and **fixed for the cluster's life**: switching a running cluster's pod network would leave the old one's interfaces, routes and addresses on every node.

| Distribution | Choices | Default |
| --- | --- | --- |
| kubeadm | Cilium, Calico | none: one has to be chosen |
| RKE2 | canal, Calico, Cilium | canal |
| k3s | flannel, Cilium, Calico | flannel |

RKE2 installs its choice itself. For kubeadm, and for k3s given Cilium or Calico, Kubehenge's controller installs it; k3s's flannel and network policy controller are then switched off.

### Machines

| Field | What it is |
| --- | --- |
| Storage | For the nodes' disks and the cluster's persistent volumes: one of those chosen for clusters when the management cluster was set up. Only storages you may allocate space on are listed, with their free space, and whether they are shared; a single one is chosen already. Shared storage lets the cluster's VMs start on any node. |
| Control plane, Workers | Cores; memory in MiB, in steps of 8; disk in GiB. |
| Placement | The Proxmox VE nodes the cluster's VMs may run on: **Any node**, **The nodes of an HA rule**, or **Nodes I choose**. **Options** changes it later. |
| Proxmox VE HA | Shown when the management cluster has Proxmox VE HA and the storage is shared; on unless you untick it. **Options** changes it later. |

Each new VM goes to one of the nodes it may use that is online: the one running the fewest of the cluster's VMs of its kind (control plane or worker) that has the memory for it, so three control planes on three nodes run one to a node. **Any node** means every node of the Proxmox VE cluster, nodes added later included. A node that goes down is left out within about 15 seconds and taken back once it is online, so the VMs that replace those it ran go to the others; while every node chosen is down, no new VM is placed elsewhere.

**The nodes of an HA rule** lists Proxmox VE's enabled HA node-affinity rules (Datacenter, HA, Affinity Rules) with the nodes each one allows: a rule's own nodes, or every other node for a rule that keeps resources off them. Choosing a rule copies its nodes into the cluster once; a later change of the rule does not move the cluster, and the cluster's VMs do not become HA resources. Reading the rules takes `Sys.Audit` on `/`; without it the choice says so, and the nodes can still be chosen one by one.

**Proxmox VE HA** keeps the control-plane VMs on separate nodes while they run, which placement alone cannot: a VM moved by hand, or restarted elsewhere, stays where it went. Once a control-plane machine has joined the cluster, Kubehenge's controller makes its VM an HA resource (Datacenter, HA, Resources), commented `Kubehenge cluster <pool>`, and adds two HA rules of the cluster's own (Datacenter, HA, Affinity Rules): one keeping the VMs apart and, when the cluster may use only some nodes, one keeping them to those. Proxmox VE's HA manager then migrates VMs that share a node apart, and restarts the VM of a failed node on another one. A node running an HA resource arms Proxmox VE's watchdog, so it restarts itself when it loses quorum: on a Proxmox VE cluster without a reliable quorum, leave HA off. Workers are not HA resources; a worker lost with its node is replaced by the health check. With more control planes than nodes, the rule keeps as many apart as there are nodes. It takes effect only when the administrator switched on Proxmox VE HA for the management cluster ([install.md](install.md#the-settings)) and the cluster's disks are on shared storage, as HA can restart a VM only where its disks are reachable. Unticking it, or deleting a control-plane machine, takes its VM out of HA first; changing the rules or resources by hand is undone by the controller.

The defaults are 2 cores and 4096 MiB, with a 30 GiB disk for the control plane and 40 GiB for workers. Disks are at least 20 GiB. A control-plane node needs at least 2 cores and 4096 MiB, the first row of the k3s and RKE2 server sizing guides; kubeadm, which documents no sizes, follows RKE2's guide. With more workers the guides ask for more, and Kubehenge refuses a cluster whose control planes fall short, when it is created and when its workers or machines change: RKE2 and kubeadm need 4 cores and 8192 MiB from 226 workers, 8 cores and 16384 MiB from 451, and 16 cores and 32768 MiB from 1,301; k3s needs the same from 351, 901 and 1,801 workers. A worker needs at least 1024 MiB on k3s, 2048 MiB on kubeadm, and 2 cores and 4096 MiB on RKE2, never less than its distribution's minimum for a node. The guides size servers whose workloads run on workers; a cluster without workers runs them on its control planes, so each needs a worker's least on top: 3 cores and 5120 MiB on k3s, 3 cores and 6144 MiB on kubeadm, 4 cores and 8192 MiB on RKE2. The interface raises the control planes to that when the workers are set to none. The interface keeps the sizes once the cluster exists; with kubectl a change of size rolls the nodes, and a disk only grows.

The add-ons need memory too, on the workers, or on the control planes of a cluster without workers. Each of those nodes needs its distribution's least, or what such a node uses itself plus its share of the add-ons' memory, whichever is more. The end-to-end tests measure the figures, which have room to grow:

| Add-on | Memory |
| --- | --- |
| Headlamp | 64 MiB |
| Monitoring | 1536 MiB |
| Kyverno | 384 MiB |
| Istio | 384 MiB |
| Cilium Cluster Mesh | 128 MiB |
| cert-manager, with monitoring or Rancher | 192 MiB |
| Rancher | 2048 MiB |
| Rancher's agent, in every cluster that registers | 512 MiB |

A worker counts 448 MiB for itself on k3s and 704 MiB on kubeadm and RKE2; a control plane counts 2560 MiB. A single k3s worker with every add-on but Rancher needs 3648 MiB, and the default machines hold every add-on but Rancher on every distribution. Kubehenge refuses a cluster short of that room when it is created and when an add-on is switched on or its nodes change. The interface raises the machines to it as add-ons are chosen and shows the add-ons' total on the Add-ons step.

### Add-ons

- **Headlamp**, a web interface inside the cluster, with an optional host name to serve it at through the cluster's ingress controller (RKE2 and k3s bring one; kubeadm brings none). It has no rights of its own: everyone signs in with their own token or through OpenID Connect. Without a host name, reach it with `kubectl -n headlamp port-forward svc/headlamp 8080:80`. Every cluster in the interface's namespace can also be opened in the appliance's shared Headlamp once it is available, without this add-on ([below](#open-a-cluster-in-headlamp)).
- **Monitoring**: Prometheus, Alertmanager and Grafana, with kube-prometheus-stack's dashboards and alerts for every node and control-plane component, each scraped over verified, authenticated TLS ([architecture.md](architecture.md#add-ons)). It installs cert-manager too, and takes about 1.5 GiB of the workers' memory ([Machines](#machines)) and a 20 GiB volume. Grafana's `admin` password is in the `kube-prometheus-stack-grafana` Secret of the `monitoring` namespace. With a host name, Grafana is served at it through the cluster's ingress controller; without one, reach it with `kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80`.
- **Pod security**: the [Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/) pods are held to, `baseline` or `restricted`, by Kubernetes' Pod Security Admission, by Kyverno, or by both, each at a level of its own. Pod Security Admission sets the level of every namespace that labels none of its own, and warns about pods short of `restricted`; Kyverno holds every namespace to its level whatever the labels say, and reports what falls short. The namespaces of the components that need the host are exempt from both ([architecture.md](architecture.md#add-ons)). A workload that needs more gets its namespace labelled, and with Kyverno a PolicyException in the `kyverno` namespace.
- **Harden to the CIS Kubernetes Benchmark**, beyond the baseline every cluster gets ([architecture.md](architecture.md#add-ons)): Pod Security Admission enforces `restricted` outside the exempt namespaces, every container that asks for no seccomp profile of its own runs under the container runtime's default, exec, attach and port-forward sessions end after five idle minutes, and RKE2 runs its own CIS profile, under which the pods in `kube-system`, `kube-public` and `default` take connections only from their own namespace. Fixed once the cluster exists.
- **Istio** in ambient mode: label a namespace `istio.io/dataplane-mode=ambient` and its pods talk to each other over mutual TLS, with their workload identities, without sidecars ([architecture.md](architecture.md#add-ons)). Under a NetworkPolicy that denies by default, allow ztunnel's port 15008/TCP between the enrolled pods, and 169.254.7.127/32, where the kubelet's probes come from.
- **Join a Cluster Mesh**, for a cluster that runs Cilium: choose the other Cilium clusters of the namespace to link with. Two clusters are linked while both are in the mesh and list each other; their pods then reach each other's, by address and through Cilium's global services, and each cluster reads the other's pod names, labels and identities ([architecture.md](architecture.md#add-ons)). A peer listed before it exists is linked once it does. Peers reach the cluster on port 32379/TCP of its API address, which has to be open between them. A cluster that runs Cilium has a name of at most 32 characters, and gets a pod range no other Kubehenge cluster uses, clear of the nodes' network.
- **Rancher.** Install Rancher into this cluster, with cert-manager; every other cluster then registers with it on its own. It needs a host name that resolves to this cluster's ingress controller (such as its API address) for people and for every registered cluster, so put it in an RKE2 or k3s cluster. Every node of Rancher's cluster needs at least 4 vCPUs, the least Rancher's documentation sizes it for: Kubehenge refuses fewer, at creation and when the machines change later, and the interface raises the cores to 4 when Rancher is chosen. Memory follows the number of clusters and nodes Rancher manages; Rancher's documentation gives 16 GB per node for up to about 150 clusters and 1,500 nodes, 32 GB with 8 vCPUs for up to 300 and 3,000, and 64 GB with 16 vCPUs for up to 1,000 and 10,000. For Rancher to stay up while a node is lost, give its cluster three control planes. Only one cluster may run Rancher: the interface refuses a second, and should one be set with kubectl, it gets no Rancher and a `RancherRefused` event says why. Kubehenge keeps Rancher's cluster within the Kubernetes versions Rancher's release runs on: it does not build it at a version outside them (the Cluster is created, gets no VMs, and its conditions say why), does not install Rancher into an existing cluster outside them, and holds back upgrades beyond them. Installing Rancher needs `Sys.Modify` on the workload pool, since Rancher receives the admin kubeconfig of every cluster that registers; without it the choice is off and says so. Its first admin password is generated into the `bootstrap-secret` Secret in the `cattle-system` namespace of that cluster.
- **Register with the Rancher Kubehenge installed**, if any: on by default for every cluster that does not install Rancher itself.
- **Upgrade automatically**: off, patch releases, or patch and minor releases, inside a window of every day from 02:00 for four hours in your browser's time zone. The cluster's **Options** change the window later ([upgrading.md](upgrading.md#automatically-inside-a-window)).

### Identity providers

The OpenID Connect providers whose ID tokens the cluster's API server accepts, besides certificates and ServiceAccount tokens, which always work. The list starts with the providers set for every cluster on the Administration page. Keeping or dropping them is free; adding or changing one needs `Sys.Modify` on the workload pool, and without it the providers can only be removed. See [Signing in with OpenID Connect](#signing-in-with-openid-connect).

### Confirm

Sums up every setting by name; **Request** below it shows what the interface sends. **Create** creates the Cluster and opens its page.

Kubehenge's controller first gives the cluster its Proxmox VE pool, user, token and the VM that will own its persistent volumes; only then does Cluster API clone the VMs. The cluster shows **Provisioning** until it is available.

## The cluster page

The buttons along the top act on the whole cluster: **Scale**, **Upgrade**, **Kubeconfig**, **Headlamp** and **Delete**. **Kubeconfig** and **Headlamp** appear once the cluster is available (Kubeconfig for those with `Sys.Modify` on its pool, Headlamp for clusters in the interface's namespace); without `VM.Allocate` on its pool, or while it is being deleted, the others are disabled and the page says why. The menu on the left switches between its views, as Proxmox VE's does for a VM:

- **Summary:** status, distribution, Kubernetes version, API endpoint, pod and service ranges, ready control-plane nodes and workers, Proxmox VE pool, namespace, who created it when, and the last automatic upgrade.
- **Machines:** each node's role, status, phase, version, addresses, age, and the Proxmox VE node its VM runs on, as far as you may see the cluster's VMs. When one Proxmox VE node runs more than one of the control-plane VMs, the page warns and says what losing that node would cost; migrating one of them in Proxmox VE to a node of its own fixes it.
- **Conditions:** Cluster API's conditions; those worth a look are marked.
- **Options:** the pod network, CIS hardening, placement, Headlamp, monitoring, pod security, Istio, Cluster Mesh (on a cluster that runs Cilium), Rancher, automatic upgrades and identity providers, each with **Edit** (**Remove** for Rancher on the cluster that runs it), disabled for those who may not change the cluster (the pod network and CIS hardening are fixed). A change of placement applies to the VMs created after it, by scaling up, an upgrade or a replacement; those running stay where they are. Nodes the cluster has that you cannot see are listed too, and may be kept.

The status reads **Provisioning**, **Available**, **Upgrading** or **Reconciling** while a change rolls out, **Deleting**, or **Failed**. A cluster deleted while its page is open says so.

## Scale

**Scale** sets the control-plane nodes (1, 3, 5 or 7: an odd number, so etcd keeps a majority when one fails) and the workers of each worker pool (0 to 100).

## Upgrade

**Upgrade** offers the newer releases of the cluster's minor version and of the next one: Kubernetes upgrades one minor version at a time and never back, and the interface refuses anything else. Cluster API replaces the control-plane nodes one by one first, then the workers. A kubeadm cluster needs the template of the new version first. Automatic upgrades inside a maintenance window are set under **Options**, **Edit** beside Automatic upgrades. Both are described in [upgrading.md](upgrading.md#workload-clusters).

## Change add-ons and sign-in

- **Headlamp:** switch it on or off, and set or clear its host name. Switching it off uninstalls it.
- **Monitoring:** switch it on or off, and set or clear Grafana's host name. Switching it off uninstalls it with its metrics proxies and certificates, and cert-manager unless Rancher runs in the cluster; Prometheus's and Alertmanager's volumes are kept.
- **Pod security:** choose the level Pod Security Admission and Kyverno each enforce; on a cluster hardened to the CIS benchmark, Pod Security Admission's stays `restricted`. A new Pod Security Admission level replaces the control-plane nodes one at a time; switching Kyverno off uninstalls its policies, then Kyverno.
- **Istio:** switch it on or off. Switching it off takes every namespace and pod out of the mesh first, then uninstalls it; its CRDs and their resources stay.
- **Cluster Mesh**, on a cluster that runs Cilium: join or leave, and choose the peers. Each listed peer shows whether it is linked, and why not; a peer you cannot see shows only that it is not linked, or does not exist. Peers come and go without restarting anything; joining or leaving upgrades Cilium and restarts its agents.
- **Rancher:** register with the Rancher Kubehenge installed, or leave it; leaving removes Rancher's agent from the cluster, except where `default` is held to `restricted` ([troubleshooting.md](troubleshooting.md#rancher)). Whether a cluster runs Rancher itself is chosen at creation. On the cluster that runs it, **Remove** beside Rancher under Options uninstalls Rancher after asking for the cluster's name: every registered cluster is first taken out of Rancher, which cleans up inside each, and Rancher's own settings, users and projects are lost. While it waits on that, the cluster shows a `RemovingRancher` event; everything else about the cluster is still applied.
- **Identity providers:** change them. Saving replaces the control-plane nodes one at a time, as any change of the API server's configuration does. Adding or changing a provider that neither the providers set for every cluster nor the cluster's own list hold needs `Sys.Modify` on the cluster's pool; without it the dialog shows the providers read-only, and any of them can be removed.

## Download a kubeconfig

**Kubeconfig** downloads `<name>.kubeconfig`, the cluster's admin kubeconfig as Cluster API keeps it, once the cluster is available. It needs `Sys.Modify` on the cluster's pool, and every download is logged with the user who made it.

## Open a cluster in Headlamp

The appliance runs one Headlamp for the clusters of the interface's namespace, on port 8443 of the appliance, behind the interface's sign-in. **Headlamp** in the top bar opens it, and **Headlamp** on a cluster's page opens that cluster (`https://<appliance>:8443/c/clusters.<name>`). It lists only the clusters you may see, checked against `VM.Audit` on each cluster's pool.

Headlamp itself holds no credentials. Sign in to each cluster with your own token, such as a ServiceAccount token made with `kubectl create token`, or through the cluster's first OpenID Connect provider: the first of its own list, or, for a cluster that sets none, the first of the providers every cluster trusts. For the latter, the provider must accept `https://<appliance>:8443/oidc-callback` as a redirect URL; Headlamp signs in as a public client with PKCE, so no client secret is stored.

The shared Headlamp does not offer what a shared server must not: port forwards, Helm, node draining, its external proxy, plugin changes, adding clusters or kubeconfigs, setting a token for others, its WebSocket multiplexer and its metrics.

## Signing in with OpenID Connect

A cluster's `oidc` setting lists up to eight OpenID Connect providers whose ID tokens its API server accepts. Each has:

| Field | Default | What it is |
| --- | --- | --- |
| Issuer URL | required | The provider's issuer, exactly as its tokens name it; `https://`. |
| Client ID | required | The client sign-in tools use; a token's audience must name it. Register it with the provider as a public client with PKCE. |
| User name claim | `email` | The claim the user name comes from. |
| User name prefix | `oidc:` | Put before every user name, so none passes for a user of another kind. It may not start with `system:`. |
| Groups claim | `groups` | The claim the groups come from. |
| Groups prefix | `oidc:` | Put before every group. It may not start with `system:`. |
| Scopes | `email`, `profile` | Asked for by sign-in tools besides `openid`. |
| CA certificate | none | PEM, when a public CA does not sign the provider's certificate. |

A token gives no rights by itself: bind the prefixed names with RBAC in the cluster, for example:

```sh
kubectl create clusterrolebinding platform-admins --clusterrole=cluster-admin --group=oidc:platform-admins
```

Headlamp signs in with the first provider. kubectl does so through an OpenID Connect credential plugin, such as kubelogin, set to the provider's issuer and client ID.

**Providers for every cluster** are set on the Administration page (**Edit…** in the Management cluster section, Identity providers), or as `cluster_oidc` with Terraform/OpenTofu. Every new cluster trusts them unless it sets its own list, and a change applies to clusters created afterwards. Whoever runs a provider can issue a token for any name, so a cluster may keep or drop these freely, while adding any other provider needs `Sys.Modify` on the pool, as the admin kubeconfig does.

## Persistent volumes

The default StorageClass `proxmox` puts volumes on the cluster's storage. They belong to a VM named `<name>-<hash>-volumes`, tagged `kubehenge` and `volumes`, in the cluster's pool, which is never started and exists so that only this cluster's token may attach them. A released volume is deleted through that VM. On storage local to each node, a volume lives on one node.

## Delete

**Delete** asks for the cluster's name. It deletes the cluster's VMs and its persistent volumes, and removes it from Rancher first. It cannot be undone. Once the VMs are gone, Kubehenge's controller removes the cluster's pool, user, token and volume owner VM. If a node that may hold some of the cluster's volumes on its local storage is offline, the controller waits for it. Cluster API removes each VM's cloud-init ISO (`user-data-<VMID>.iso` on the cloud-init storage) with the VM; only a VM deleted by hand, outside Cluster API, leaves its ISO behind, which can then be removed under the storage's ISO Images in Proxmox VE.

Deleting the cluster that runs Rancher first removes every registered cluster from Rancher.

## Who may do what

The interface checks the signed-in user's Proxmox VE privileges on each cluster's pool, then acts with its own account in the management cluster. Clusters are shared with the same ACLs as VMs.

| To | Privilege | On |
| --- | --- | --- |
| See a cluster, and open it in the shared Headlamp | `VM.Audit` | the cluster's pool |
| Create a cluster | `VM.Allocate` | the workload pool |
| | `Datastore.AllocateSpace` | `/storage/<storage>` |
| | `SDN.Use` | `/sdn/zones/<zone>/<bridge>`, or `…/<bridge>/<vlan>` with a VLAN tag |
| Place a cluster by an HA rule's nodes | `Sys.Audit` | `/` |
| Install Rancher | `Sys.Modify` | the workload pool |
| Scale, upgrade, change add-ons, automatic upgrades and sign-in, delete | `VM.Allocate` | the cluster's pool |
| Download the admin kubeconfig | `Sys.Modify` | the cluster's pool |
| Trust identity providers beyond those set for every cluster | `Sys.Modify` | the workload pool when creating, the cluster's pool when changing |
| Use the Administration page | the [setup privileges](install.md#who-may-set-it-up) | `/` |

- A node's own bridges are in the zone `localnetwork`: `/sdn/zones/localnetwork/vmbr0`.
- A cluster you may not see is answered as not found.
- As everywhere in Proxmox VE, a role given on a cluster's own pool replaces, for that pool, what is inherited from the workload pool, so it has to hold `VM.Audit` too.
- Proxmox VE's `PVEAuditor` role holds `VM.Audit`, and `PVEVMAdmin` holds `VM.Audit` and `VM.Allocate`. `Sys.Modify` needs a role of your own.

For example, to let a group create and look after clusters, and one user download the kubeconfig of one cluster:

```sh
pveum acl modify /pool/kubehenge --groups developers --roles PVEVMAdmin
pveum acl modify /storage/shared-storage --groups developers --roles PVEDatastoreUser
pveum acl modify /sdn/zones/localnetwork/vmbr0 --groups developers --roles PVESDNUser
pveum role add KubehengeClusterAdmin --privs "VM.Audit VM.Allocate Sys.Modify"
pveum acl modify /pool/kubehenge/clusters.prod --users platform@pve --roles KubehengeClusterAdmin
```

The interface changes only clusters in its own namespace, `clusters`. Clusters created with kubectl elsewhere are shown to those who may see them, but are changed with kubectl.

## With kubectl

Clusters are Kubernetes objects in the management cluster, so they can be created and changed with kubectl, or kept in Git with any GitOps tool.

Get the management cluster's admin kubeconfig from the Administration page (**Repair access**, **Download admin kubeconfig**, with the state passphrase), or with `tofu output -raw kubeconfig` if you applied it with Terraform/OpenTofu. Whoever holds it controls every cluster: Proxmox VE privileges do not apply to kubectl.

### A Cluster

```yaml
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: prod
  namespace: clusters
spec:
  clusterNetwork:
    pods: {cidrBlocks: [10.42.0.0/16]}
    services: {cidrBlocks: [10.43.0.0/16]}
  topology:
    classRef:
      name: kubehenge-rke2
      namespace: kubehenge-system
    version: v1.36.5+rke2r1
    controlPlane:
      replicas: 3
    workers:
      machineDeployments:
        - class: worker
          name: md-0
          replicas: 3
    variables:
      - name: controlPlaneEndpoint
        value: 192.0.2.50
      - name: network
        value:
          bridge: vmbr0
          addresses: [192.0.2.51-192.0.2.69]
          prefix: 24
          gateway: 192.0.2.1
          dnsServers: [192.0.2.1]
      - name: storage
        value: shared-storage
      - name: cni
        value: cilium
      - name: upgrades
        value:
          mode: patch
          window: {weekdays: [Sat], start: "02:00", durationMinutes: 240, timeZone: Europe/Berlin}
```

```sh
kubectl apply -f prod.yaml
kubectl -n clusters wait --for=condition=Available cluster/prod --timeout=40m
kubectl -n clusters get secret prod-kubeconfig -o jsonpath='{.data.value}' | base64 -d > prod.kubeconfig
```

- **The ClusterClasses** are `kubehenge-kubeadm`, `kubehenge-rke2` and `kubehenge-k3s`, in `kubehenge-system`.
- **The namespace** decides the pool: `<workload pool>/<namespace>.<name>`. Clusters in `clusters` are also looked after from the interface; any other namespace works, and the interface shows those clusters read-only. Keep names to 40 characters, as the interface does, so that the nodes' host names stay within one DNS label.
- **Worker pools** are machine deployments of the class `worker`; a Cluster may have several.
- **Ranges** come from `spec.clusterNetwork`; without it, the distribution's own apply (`10.244.0.0/16` and `10.96.0.0/12` for kubeadm, `10.42.0.0/16` and `10.43.0.0/16` for RKE2 and k3s). Kubehenge refuses to create a cluster whose ranges overlap its nodes' network.

### Variables

| Variable | Required | What it is |
| --- | --- | --- |
| `controlPlaneEndpoint` | yes | The control plane's virtual IP: an IPv4 address, unused, outside `network.addresses`. |
| `network` | yes | `bridge`, one of the bridges clusters may use; `vlan` (optional, 1 to 4094); `addresses`, a list of ranges, networks in CIDR notation or single addresses; `prefix` (1 to 32); `gateway`; `dnsServers`. |
| `storage` | yes | The storage for the nodes' disks and the persistent volumes, one of those clusters may use. |
| `allowedNodes` | no | Proxmox VE nodes the cluster's VMs may run on, at least one; every node when unset. New VMs go to those of them that are online ([above](#machines)). The interface takes only nodes the user can see, or those the cluster already has. A change applies to VMs created after it. |
| `proxmoxHA` | no | Whether Proxmox VE HA keeps the control-plane VMs on separate nodes ([above](#machines)); default true, where the management cluster has Proxmox VE HA and the storage is shared. |
| `controlPlaneMachine`, `workerMachine` | no | `cores` (default 2), `memoryMiB` (a multiple of 8, default 4096, with the minimums [above](#machines)), `diskGiB` (default 30 for the control plane and 40 for workers, at least 20; a disk only grows). |
| `cni` | kubeadm only | kubeadm: `cilium` or `calico`. RKE2: `canal` (default), `calico` or `cilium`. k3s: `flannel` (default), `cilium` or `calico`. The class refuses a change once the cluster exists. |
| `headlamp` | no | `enabled` (default false); `hostname`, to serve it through the cluster's ingress controller; `tlsSecret`, a Secret in the `headlamp` namespace with the certificate for `hostname` (the ingress controller's default certificate when unset). |
| `rancher` | no | `install` (default false), which needs `hostname` and at least 4 vCPUs on every node; `register` (default true), to register with the Rancher Kubehenge installed. Setting `install` back to false removes every registered cluster from Rancher, then uninstalls it. A second cluster with `install` gets no Rancher; `kubectl describe cluster` shows a `RancherRefused` event. |
| `oidc` | no | Up to eight providers: `issuerURL`, `clientID`, `usernameClaim`, `usernamePrefix`, `groupsClaim`, `groupsPrefix`, `scopes`, `certificateAuthority` ([above](#signing-in-with-openid-connect)). Unset, the providers set for every cluster. |
| `upgrades` | no | Automatic Kubernetes upgrades: `mode` (`off`, `patch` or `minor`; default `off`) and `window` ([upgrading.md](upgrading.md#automatically-inside-a-window)). |

### Day two

```sh
kubectl -n clusters patch cluster prod --type=json \
  -p '[{"op":"replace","path":"/spec/topology/workers/machineDeployments/0/replicas","value":5}]'
kubectl -n clusters patch cluster prod --type=merge -p '{"spec":{"topology":{"version":"v1.37.1+rke2r1"}}}'
kubectl -n clusters delete cluster prod
```
