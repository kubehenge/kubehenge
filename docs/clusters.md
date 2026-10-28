# Clusters

Every cluster Kubehenge creates is a Cluster API Cluster built from one of Kubehenge's ClusterClasses: kubeadm, RKE2, k3s or Talos. Its VMs live in a Proxmox VE pool of their own under the workload pool, `<workload pool>/<namespace>.<name>`; a cluster `prod` created in the interface lands in `kubehenge/clusters.prod`.

Every cluster gets, whatever its distribution:

- **The Proxmox VE cloud controller manager**, which initialises new nodes and labels them with their region (the Proxmox VE cluster's name, or the node's on a single node outside any cluster) and zone (the node).
- **The Proxmox CSI plugin**, with a default StorageClass `proxmox`: ext4 volumes on the cluster's storage, deleted when released.
- **A control-plane virtual IP**, held by kube-vip on one control-plane node and moved to another when it fails.
- **A CNI**: the distribution's own, or the one chosen at creation.
- **An ingress controller**, Traefik, on ports 80 and 443 of every node, the control planes included, so a host name pointing at the cluster's API address or at any node reaches it. RKE2 and k3s bring their own; Kubehenge installs it into kubeadm clusters, in `kube-system`.
- **The Gateway API**, so that Gateways and HTTPRoutes work beside Ingresses: its standard channel's CRDs, with the admission policy upstream ships to refuse older or experimental ones over them, and Traefik's Gateway `traefik-gateway` in `kube-system`, which listens for HTTP where Traefik does, and for HTTPS too once the cluster has [certificates](#certificates), and takes routes from every namespace ([below](#routes-through-the-gateway-api)). RKE2 and k3s ship the CRDs with their Traefik in some releases, and all of the Gateway API in a chart of its own in later ones; there they stay the distribution's while it ships them.
- **Health checks.** A node that stays not ready for five minutes, or does not join within twenty, is replaced; workers one at a time.
- **Control-plane nodes kept for the control plane.** Once a worker has joined, the control-plane nodes carry the taint `node-role.kubernetes.io/control-plane:NoSchedule` in every distribution, so workloads run on the workers and leave etcd and the API server their nodes. A cluster scaled to no workers has the taint taken off, and its control-plane nodes run the workloads. A worker that fails leaves the taint in place. Pods that started on a control-plane node before the first worker joined stay there until they are next scheduled.
- **Its own Proxmox VE user and API token**, with rights on its own pool only, held by the cloud controller manager and the CSI plugin's controller on the control-plane nodes ([architecture.md](architecture.md#each-clusters-own-corner-of-proxmox-ve)).

Before the first cluster, the VM templates for its distribution have to exist ([install.md](install.md#create-the-vm-templates)).

## Create a cluster in the interface

**Create cluster** on the cluster list opens a wizard of six steps: General, Network, Machines, Add-ons, Identity providers and Confirm, which sums up every setting. A field says what is wrong with it once it has been changed, or when **Next** is pressed with something missing, which then moves to the first such field. Choosing another distribution brings its defaults only into what you have not set yet. **Cancel** asks before it discards what was entered. A step in the bar above opens only once the steps before it are complete. Without `VM.Allocate` on the workload pool, **Create cluster** is off and says so.

### General

| Field | What it is |
| --- | --- |
| Namespace | Only where an administrator has named [more namespaces for clusters](#more-namespaces-for-clusters): the namespace in the management cluster the cluster goes to, `clusters` unless another is chosen. |
| Name | Lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit, at most 40 characters. It also names the cluster's Proxmox VE pool and VMs. |
| Distribution | kubeadm, RKE2, k3s or Talos: one per ClusterClass in the management cluster. A distribution whose VM template is not in the template pool yet says so, and the cluster cannot be created until it is built ([templates.md](templates.md)): by an administrator, or, where the administrator lets cluster creators have missing templates built, with **Have the template built** right there. |
| Node OS | For kubeadm, RKE2 and k3s: the operating system of the nodes, AlmaLinux 10 unless another is chosen ([templates.md](templates.md#node-oses)); one not released yet is shown but cannot be chosen, and one without a template on the distribution says so. Talos nodes run Talos, which is its own OS, so a Talos cluster has none to choose. |
| Talos release | For Talos only: the release the nodes run, among those the template pool has a template of, the newest chosen first. The newest release of each minor version the Talos Image Factory builds is listed too, marked as having no template yet; where cluster creators may have missing templates built, one of those can be asked for here. Later releases are an upgrade away ([below](#talos-upgrades)). |
| Kubernetes version | For RKE2 and k3s, the newest release of each minor version Kubernetes still patches (the four newest, which takes in the one Kubernetes keeps a few weeks after a release) from their release channels, the stable one chosen first; a cluster already on an older minor version can still be upgraded from it; for kubeadm, the newest version of each minor version a VM template has been built for on the node OS chosen, the newest chosen first; where cluster creators may have missing templates built, **Kubernetes version to build** asks for another version's, which is offered here once built, after about twenty minutes; for Talos, Kubernetes' own newest release of each minor version the chosen Talos release runs, the one it installs by default chosen first. They are in the distribution's own form: `v1.36.5` for kubeadm and Talos, `v1.36.5+rke2r1` for RKE2, `v1.36.5+k3s1` for k3s. When the list cannot be read, the version is typed in. |
| Control-plane nodes | 1, 3, 5 or 7. Three or more keep the cluster running when one fails. |
| Workers | 0 to 100. With none, the control-plane nodes run the workloads too. **Worker count**, **Autoscaled** instead has the cluster autoscaler keep the workers between the **Fewest workers** (0 to 100) and the **Most workers** (1 to 100), [below](#autoscaling). |

### Network

| Field | What it is |
| --- | --- |
| Bridge | The bridge the cluster's VMs attach to: one of those chosen for clusters when the management cluster was set up. Bridges you may not use (no `SDN.Use`) are shown but disabled; when only one is left to choose, it is chosen. When the management cluster names none and Proxmox VE lists no bridges to you (no `Sys.Audit` on the nodes), you type the name in. |
| VLAN tag | Optional, 1 to 4094. |
| API address | The control plane's virtual IP: an unused address in the nodes' network, outside the node addresses. |
| Node addresses | Where the nodes take their addresses from: ranges (`192.0.2.21-192.0.2.29`), networks or single addresses inside the gateway's network, one per line or separated by commas. The wizard asks for at least one address more than there are nodes, for the extra node an upgrade or a replacement adds, counting an autoscaled pool at its most; leave more for scaling up. |
| Prefix length | Of the nodes' network. |
| Gateway | The nodes' default gateway. |
| DNS servers | IPv4 addresses, one per line or separated by commas. |
| Load balancer | What gives Services of type `LoadBalancer` their addresses, [below](#load-balancer). |
| Load-balancer addresses | With a load balancer: the addresses it hands out, as ranges, networks or single addresses. |
| Pod network | The CNI, [below](#pod-network). |
| Pod range, Service range | Optional, under **Pod and service ranges**. |

The nodes' network is the gateway with the prefix length. Pod and service ranges must stay clear of it and of each other, or traffic for the nodes would be routed into the cluster. Kubehenge sets both ranges on every cluster it creates: those you give, refused if they overlap; otherwise the distribution's own when they are clear of the nodes' network (`10.244.0.0/16` and `10.96.0.0/12` for kubeadm, `10.42.0.0/16` and `10.43.0.0/16` for RKE2 and k3s); otherwise the first of a few private ranges that are clear.

#### Load balancer

Without one, kubeadm and RKE2 clusters give Services of type `LoadBalancer` no address, and k3s's own servicelb answers for each of them on every node's address, on any port but 80 and 443, which the ingress controller holds there. With one, each such Service takes an address of its own from the load-balancer addresses, which one node at a time answers for on the nodes' network; when that node fails, another takes over. Choose:

- **MetalLB** (recommended), in layer 2 mode, in the `metallb-system` namespace.
- **kube-vip**, which already holds the API address, in `kube-system`, with kube-vip's cloud provider handing out the addresses.

The addresses must be unused and on the nodes' network, apart from the node addresses, the API address, the gateway and the network's own and broadcast addresses. Kubehenge refuses addresses that clash with these, or with the node addresses, API address or load-balancer addresses of another cluster on the same bridge and VLAN, when the cluster is created and whenever its addresses change. On k3s, a load balancer switches servicelb off. The load balancer can be switched, or changed to other addresses, later ([below](#change-add-ons-and-sign-in)): Services whose addresses are outside the new range then take new ones from it, and a switch from MetalLB to kube-vip hands every Service its address afresh.

#### Pod network

The CNI, chosen now and **fixed for the cluster's life**: switching a running cluster's pod network would leave the old one's interfaces, routes and addresses on every node.

| Distribution | Choices | Default |
| --- | --- | --- |
| kubeadm | Cilium, Calico | none: one has to be chosen |
| RKE2 | canal, Calico, Cilium | canal |
| k3s | flannel, Cilium, Calico | flannel |
| Talos | flannel, Cilium, Calico | flannel |

RKE2 installs its choice itself. For kubeadm, and for k3s and Talos given Cilium or Calico, Kubehenge's controller installs it; k3s's flannel and network policy controller are then switched off, and Talos's flannel is left out.

### Machines

| Field | What it is |
| --- | --- |
| Storage | For the nodes' disks and the cluster's persistent volumes: one of those chosen for clusters when the management cluster was set up. Only storages you may allocate space on are listed, with their free space, and whether they are shared; a single one is chosen already. Shared storage lets the cluster's VMs start on any node. |
| Control plane, Workers | Cores; memory in MiB, in steps of 8; disk in GiB. |
| Placement | The Proxmox VE nodes the cluster's VMs may run on: **Any node**, **The nodes of an HA rule**, or **Nodes I choose**. **Options** changes it later. |
| Proxmox VE HA | Shown when the management cluster has Proxmox VE HA and the storage is shared; on unless you untick it. **Options** changes it later. |
| CSI plugin's newest release | Shown when the management cluster lets clusters choose it; off unless you tick it. **Options** changes it later. See [Persistent volumes](#persistent-volumes). |
| HA group labels | Shown unless the management cluster switched them off for every cluster; on unless you untick it. **Options** changes it later. See below. |
| Volume snapshots | Shown when the management cluster lets clusters take them and the storage is local to each node; off unless you tick it. **Options** changes it later. See [Volume snapshots](#volume-snapshots). |

Each new VM goes to one of the nodes it may use that is online: the one running the fewest of the cluster's VMs of its kind (control plane or worker) that has the memory for it, so three control planes on three nodes run one to a node. **Any node** means every node of the Proxmox VE cluster, nodes added later included. A node that goes down is left out within about 15 seconds and taken back once it is online, so the VMs that replace those it ran go to the others; while every node chosen is down, no new VM is placed elsewhere.

**The nodes of an HA rule** lists Proxmox VE's enabled HA node-affinity rules (Datacenter, HA, Affinity Rules) with the nodes each one allows: a rule's own nodes, or every other node for a rule that keeps resources off them. Choosing a rule copies its nodes into the cluster once; a later change of the rule does not move the cluster, and the cluster's VMs do not become HA resources. Reading the rules takes `Sys.Audit` on `/`. Without it the choice says so, and the nodes can still be chosen one by one, unless an administrator has the rules offered to cluster creators ([install.md](install.md#ha-rules-for-cluster-creators)): then everyone who may create or change a cluster sees each rule's name and the nodes it allows, without its comment or whether it is strict, as Kubehenge read them up to half a minute before. The rules Kubehenge keeps for its own clusters' Proxmox VE HA are not offered.

**Proxmox VE HA** keeps the control-plane VMs on separate nodes while they run, which placement alone cannot: a VM moved by hand, or restarted elsewhere, stays where it went. Once a control-plane machine has joined the cluster, Kubehenge's controller makes its VM an HA resource (Datacenter, HA, Resources), commented `Kubehenge cluster <pool>`, and adds two HA rules of the cluster's own (Datacenter, HA, Affinity Rules): one keeping the VMs apart and, when the cluster may use only some nodes, one keeping them to those. Proxmox VE's HA manager then migrates VMs that share a node apart, and restarts the VM of a failed node on another one. A node running an HA resource arms Proxmox VE's watchdog, so it restarts itself when it loses quorum: on a Proxmox VE cluster without a reliable quorum, leave HA off. Workers are not HA resources; a worker lost with its node is replaced by the health check. With more control planes than nodes, the rule keeps as many apart as there are nodes. It takes effect only when the administrator switched on Proxmox VE HA for the management cluster ([install.md](install.md#the-settings)) and the cluster's disks are on shared storage, as HA can restart a VM only where its disks are reachable. Unticking it, or deleting a control-plane machine, takes its VM out of HA first; changing the rules or resources by hand is undone by the controller.

**Label the nodes with their Proxmox VE HA groups** has the cloud controller manager label each node with the HA groups, or HA node-affinity rules, its Proxmox VE node belongs to: `group.topology.proxmox.sinextra.dev/<group or rule>`, with an empty value. Proxmox VE lists them only with `Sys.Audit` on `/`, so the cluster's token gets it there, unpropagated, while the option is on: Kubehenge's controller adds the cluster's user to a group holding it, and takes it out again when switched off. That lets the token read the Proxmox VE cluster's own configuration (firewall rules, backup jobs, HA, the corosync configuration), but change nothing, and read nothing of the guests, storages or nodes beyond what it reads anyway. It is on by default unless the administrator switched it off for every cluster ([install.md](install.md#ha-group-labels)), and switching it on or off takes no privilege beyond changing the cluster. Unticked, the cloud controller manager logs a permission error for each node every few minutes and the nodes go without the labels. The labels follow within a few minutes; the cloud controller manager does not remove them when the option is switched off. When the controller cannot grant it, `kubectl describe cluster` shows a `SysAuditRefused` event saying why.

The defaults are 2 cores and 4096 MiB, with a 30 GiB disk for the control plane and 40 GiB for workers. Disks are at least 20 GiB. A control-plane node needs at least 2 cores and 4096 MiB, the first row of the k3s and RKE2 server sizing guides; kubeadm, which documents no sizes, follows RKE2's guide. With more workers the guides ask for more, and Kubehenge refuses a cluster whose control planes fall short, when it is created and when its workers or machines change: RKE2 and kubeadm need 4 cores and 8192 MiB from 226 workers, 8 cores and 16384 MiB from 451, and 16 cores and 32768 MiB from 1,301; k3s needs the same from 351, 901 and 1,801 workers. A worker needs at least 1024 MiB on k3s, 2048 MiB on kubeadm, and 2 cores and 4096 MiB on RKE2, never less than its distribution's minimum for a node. The guides size servers whose workloads run on workers; a cluster without workers runs them on its control planes, so each needs a worker's least on top: 3 cores and 5120 MiB on k3s, 3 cores and 6144 MiB on kubeadm, 4 cores and 8192 MiB on RKE2. The interface raises the control planes to that when the workers are set to none. The interface keeps the sizes once the cluster exists; with kubectl a change of size rolls the nodes, and a disk only grows.

The add-ons need memory and CPU too. Their pods run on the workers, or on the control planes of a cluster without workers, and each of those nodes needs its distribution's least, or what such a node takes itself plus its share of the pods' memory and CPU, whichever is more. Every control plane also holds the add-ons' resource types and objects in its API server, so the memory it uses itself grows with each add-on, workers or not. The API server runs with a Go memory limit of 40 % of its control plane's memory (on k3s, half of it for the server's whole process), so it frees memory once it nears the limit rather than at twice what it holds. The end-to-end tests measure the memory, which has room to grow. The CPU is what the add-ons' pods request as Kubehenge installs them, counting one node's share of those that run on every node: Kubernetes schedules a pod only onto a node with that much CPU not yet requested, however idle the node is, and leaves it pending otherwise.

| Add-on | Pods' memory | Each control plane, kubeadm, RKE2 and Talos | Each control plane, k3s | Pods' CPU |
| --- | --- | --- | --- | --- |
| Headlamp | 64 MiB | 8 MiB | | 10 millicores |
| Monitoring | 1536 MiB | 32 MiB | 616 MiB | 20 millicores |
| Logs | 512 MiB | 56 MiB | | 75 millicores |
| Backups | 384 MiB | 184 MiB | 24 MiB | 70 millicores |
| Kyverno | 384 MiB | 552 MiB | 520 MiB | 400 millicores |
| Istio | 384 MiB | 288 MiB | 504 MiB | 400 millicores |
| Cilium Cluster Mesh | 128 MiB | | | none |
| Load balancer | 128 MiB | | | 50 millicores |
| GitOps with Argo CD | 512 MiB | 432 MiB | 360 MiB | none |
| GitOps with Flux | 384 MiB | | | 50 millicores |
| cert-manager, with monitoring, Rancher or certificates | 192 MiB | 48 MiB | | none |
| Rancher | 2048 MiB | 512 MiB | 512 MiB | 10 millicores |
| Rancher's agent, in every cluster that registers | 512 MiB | | | none |

A worker counts 448 MiB for itself on k3s and 704 MiB on kubeadm, RKE2 and Talos. A control plane counts 1848 MiB on k3s, 2392 MiB on kubeadm and Talos and 3120 MiB on RKE2 before any add-on; with every add-on but Rancher it needs 3872 MiB on k3s, 3992 MiB on kubeadm and Talos and 4720 MiB on RKE2, above RKE2's least of 4096 MiB. A single k3s worker with every add-on but Rancher and GitOps needs 4672 MiB, 4288 MiB without backups, 4160 MiB without logs, or 3776 MiB without either, and GitOps with Argo CD on top takes each 512 MiB further; the default machines hold every add-on but Rancher, logs, backups and GitOps on every distribution, and a second worker makes room for logs and backups too.

For CPU a worker counts what its distribution, its CNI and Kubehenge's own pods request there: 230 millicores on kubeadm, 320 on k3s, 480 on Talos, which keeps 50 of them back for the system and whose flannel asks for 200 with its network policy controller, and 745 on RKE2, whose kube-proxy and canal ask for 250 each. Without workers, a control plane counts 1040 millicores on kubeadm, 380 on k3s, 900 on Talos and 1555 on RKE2. Every add-on but Rancher takes 1075 millicores, so it fits on one worker of the default 2 cores on every distribution. On a worker of a single core, the least kubeadm, k3s and Talos allow, Kyverno and Istio do not fit together, nor on Talos Kyverno with every other add-on but Istio; such a worker needs a second core, or a second worker beside it.

Kubehenge refuses a cluster short of that room when it is created and when an add-on is switched on or its nodes change. The interface raises the machines to it as add-ons are chosen and shows the add-ons' totals on the Add-ons step.

### Add-ons

- **Headlamp**, a web interface inside the cluster, with an optional host name to serve it at through the cluster's ingress controller. It has no rights of its own: everyone signs in with their own token or through OpenID Connect. With **Read-only for everyone, without signing in** (off by default), its service account is bound to Kubernetes' built-in `view` ClusterRole and Headlamp uses that account for every request, so anyone who reaches it, at its host name or through a port-forward, sees the workloads, Services, ConfigMaps, events and pod logs of every namespace, and custom resources whose roles add themselves to `view`, without signing in. It shows no Secrets and no nodes, and changes nothing; a token given to it is not used, so changes go through kubectl or the shared Headlamp. Without a host name, reach it with `kubectl -n headlamp port-forward svc/headlamp 8080:80`. Every cluster in the interface's namespaces can also be opened in the appliance's shared Headlamp once it is available, without this add-on ([below](#open-a-cluster-in-headlamp)).
- **Monitoring**: Prometheus, Alertmanager and Grafana, with kube-prometheus-stack's dashboards and alerts for every node and control-plane component, each scraped over verified, authenticated TLS ([architecture.md](architecture.md#add-ons)). It installs cert-manager too, and takes about 1.5 GiB of the workers' memory ([Machines](#machines)) and a 20 GiB volume. Grafana's `admin` password is in the `kube-prometheus-stack-grafana` Secret of the `monitoring` namespace. With a host name, Grafana is served at it through the cluster's ingress controller; without one, reach it with `kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80`.
- **Logs**, beside monitoring: Loki keeps the cluster's logs on a volume of its own, Grafana Alloy collects them, and monitoring's Grafana reads them as its Loki data source ([architecture.md](architecture.md#add-ons)). Logs are kept for 7 days in a 10 GiB volume unless set otherwise; the volume only grows and should hold the days kept, since Loki deletes by age, not by size. The collector is chosen with them:
  - **On every node** (recommended): Alloy runs on every node in the `logging-node` namespace, exempt from pod security, and reads the host's log files read-only. That gives it every container's log, short-lived pods' too, the node's journal and, on the control planes, the API server's audit log.
  - **Through the Kubernetes API**: one Alloy in `logging` reads the pods' logs through the API server, with no access to the host. It is lighter, but misses the journal, the audit log and pods that end before it reads them.

  Without monitoring there are no logs: Kubehenge refuses them, and refuses switching monitoring off while they are on.
- **Backups** with Velero, to one of the S3 stores an administrator set ([install.md](install.md#backup-targets)), daily at 02:00 by default, each kept for 7 days. Velero backs up every namespace but Kubernetes' own and the add-ons', with the data in its pods' volumes; the cluster writes under a folder of its own in the store, encrypted with keys of its own ([Back up and restore](#back-up-and-restore)). Choose the target, the schedule, its time zone, how long each backup is kept and the access key Velero signs in to the store with: one of the cluster's own, which needs `Sys.Modify` on the workload pool, or the one an administrator stored for every cluster. Without any target, the choice is off and says so.
- **Certificates** from cert-manager for the cluster's Ingresses and routes: from Let's Encrypt or another ACME server, proved through your DNS provider's API (DNS-01) or over HTTP (HTTP-01), or from a CA of the cluster's own. With DNS-01 or the cluster's CA, a certificate for a domain and every name one label beneath it is served wherever no other is asked for, and Traefik's Gateway listens for HTTPS with it ([below](#certificates)). DNS-01 takes the DNS provider's credential, typed in here and stored by the interface, or the one an administrator stored for every cluster. It installs cert-manager too.
- **Pod security**: the [Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/) pods are held to, `baseline` or `restricted`, by Kubernetes' Pod Security Admission, by Kyverno, or by both, each at a level of its own. Pod Security Admission sets the level of every namespace that labels none of its own, and warns about pods short of `restricted`; Kyverno holds every namespace to its level whatever the labels say, and reports what falls short. The namespaces of the components that need the host are exempt from both ([architecture.md](architecture.md#add-ons)). A workload that needs more gets its namespace labelled, and with Kyverno a PolicyException in the `kyverno` namespace.
- **Harden to the CIS Kubernetes Benchmark**, beyond the baseline every cluster gets ([architecture.md](architecture.md#add-ons)): Pod Security Admission enforces `restricted` outside the exempt namespaces, every container that asks for no seccomp profile of its own runs under the container runtime's default, exec, attach and port-forward sessions end after five idle minutes, and RKE2 runs its own CIS profile, under which the pods in `kube-system`, `kube-public` and `default` take connections only from their own namespace. Fixed once the cluster exists.
- **Istio** in ambient mode: label a namespace `istio.io/dataplane-mode=ambient` and its pods talk to each other over mutual TLS, with their workload identities, without sidecars ([architecture.md](architecture.md#add-ons)). Under a NetworkPolicy that denies by default, allow ztunnel's port 15008/TCP between the enrolled pods, and 169.254.7.127/32, where the kubelet's probes come from.
- **Join a Cluster Mesh**, for a cluster that runs Cilium: choose the other Cilium clusters of the namespace to link with. Two clusters are linked while both are in the mesh and list each other; their pods then reach each other's, by address and through Cilium's global services, and each cluster reads the other's pod names, labels and identities ([architecture.md](architecture.md#add-ons)). A peer listed before it exists is linked once it does. Peers reach the cluster on port 32379/TCP of its API address, which has to be open between them. A cluster that runs Cilium has a name of at most 32 characters, and gets a pod range no other Kubehenge cluster uses, clear of the nodes' network.
- **GitOps**: Argo CD (recommended) or Flux keeps the cluster in step with a directory of a Git repository: what it holds is applied, and what goes from it is removed ([architecture.md](architecture.md#add-ons)). Give the repository's URL, branch or ref and directory, and how it is read: as it is, for a public repository over HTTPS; with a deploy key over SSH (recommended for a private repository), which Kubehenge makes once the cluster is saved and shows under its **Options**, to add to the repository as a read-only deploy key; with a user name and read-only token over HTTPS, stored under **Options** once the cluster exists; or with a Secret of your own. Over SSH, Kubehenge reads the server's host keys once and trusts them from then on, and shows their fingerprints beside the deploy key to compare, unless you give the keys yourself. Argo CD signs in through the cluster's first identity provider, its local admin disabled; the provider's groups listed as admins administer it, and nobody else has any rights in it. With a host name it is served through the cluster's ingress controller; without one, reach it with `kubectl -n argocd port-forward svc/argocd-server 8080:80`. Flux's web interface is off by default: switched on, it shows every Flux object and the workloads they deployed at its host name, and lets those signed in reconcile, suspend and resume them, with the rights the cluster's RBAC gives them; it signs in with a client secret. Choosing the repository needs `Sys.Modify` on the workload pool, since the engine applies whatever the repository holds with cluster-wide rights. Flux is installed by the Flux Operator, which is licensed under AGPL-3.0; Kubehenge runs it unmodified.
- **Rancher.** Install Rancher into this cluster, with cert-manager; every other cluster then registers with it on its own. Kubehenge creates and upgrades the clusters, so Rancher's upgrades of registered RKE2 and k3s clusters are switched off. It needs a host name that resolves to this cluster's ingress controller (such as its API address) for people and for every registered cluster. Every node of Rancher's cluster needs at least 4 vCPUs, the least Rancher's documentation sizes it for: Kubehenge refuses fewer, at creation and when the machines change later, and the interface raises the cores to 4 when Rancher is chosen. Memory follows the number of clusters and nodes Rancher manages; Rancher's documentation gives 16 GB per node for up to about 150 clusters and 1,500 nodes, 32 GB with 8 vCPUs for up to 300 and 3,000, and 64 GB with 16 vCPUs for up to 1,000 and 10,000. For Rancher to stay up while a node is lost, give its cluster three control planes. Only one cluster may run Rancher: the interface refuses a second, and should one be set with kubectl, it gets no Rancher and a `RancherRefused` event says why. Kubehenge keeps Rancher's cluster within the Kubernetes versions Rancher's release runs on: it does not build it at a version outside them (the Cluster is created, gets no VMs, and its conditions say why), does not install Rancher into an existing cluster outside them, and holds back upgrades beyond them. Installing Rancher needs `Sys.Modify` on the workload pool, since Rancher receives the admin kubeconfig of every cluster that registers; without it the choice is off and says so. Its first admin password is generated into the `bootstrap-secret` Secret in the `cattle-system` namespace of that cluster.
- **Register with the Rancher Kubehenge installed**, if any: on by default for every cluster that does not install Rancher itself.
- **Upgrade automatically**: off, patch releases, or patch and minor releases, inside a window of every day from 02:00 for four hours in your browser's time zone. The nodes of a kubeadm, RKE2 or k3s cluster move to new builds of their VM template in the same window whatever this says ([below](#node-image-refresh)). The cluster's **Options** change the window later ([upgrading.md](upgrading.md#automatically-inside-a-window)).

### Identity providers

The OpenID Connect providers whose ID tokens the cluster's API server accepts, besides certificates and ServiceAccount tokens, which always work. The list starts with the providers set for every cluster on the Administration page. Keeping or dropping them is free; adding or changing one needs `Sys.Modify` on the workload pool, and without it the providers can only be removed. See [Signing in with OpenID Connect](#signing-in-with-openid-connect).

### Confirm

Sums up every setting by name; **Request** below it shows what the interface sends. **Create** creates the Cluster and opens its page.

Kubehenge's controller first gives the cluster its Proxmox VE pool, user, token and the VM that will own its persistent volumes; only then does Cluster API clone the VMs. The cluster shows **Provisioning** until it is available.

## Finding your way

Every page shows the fleet down its left side, grouped by where each cluster stands, each with why in a few words:

- **Needs you:** something failed and waits for someone: a failed Talos upgrade, a failed machine, a cluster Cluster API reports failed, or one still not available an hour after it was created. So does a cluster whose [health page](#the-cluster-page) finds trouble, such as an API server that does not answer, a node that is not ready or a pod that keeps crashing; the line names the worst of it.
- **Running:** a change is under way: a cluster being created, upgraded, rolled out, scaled or deleted, or an add-on still being installed.
- **Healthy:** nothing is going on and its health page finds nothing; the line under the name says what it runs.

Each entry opens the view that says more: the health page for what it found, the machines for a rollout. The health pages are read for the whole fleet at most every 30 seconds.

Down the right side of a wide window, **Runs** follows the work that goes on by itself once started: the same changes to clusters, and each cluster's etcd snapshots and restores and its backups and restores, each with how far it has come. Those who may set up the management cluster also see its applies, the template builds and refreshes, and the appliance's upgrades. A run that failed stays there for a day with the first error it reported; one that went well, for an hour. It folds away with **›** or `R`; in a narrower window **Running** in the top bar shows it.

Each run opens a page of its own: what it does and on which cluster, who started it and how long it has taken, the first error pinned at the top, and its steps, each done, under way or waiting. A change [applied here](#staged-changes) is a run too, kept for a day, in place of the cluster's line under **Running**: accepted by the API server, then the control-plane machines or the workers it replaces, counted as the new ones are ready and the old ones gone, or the machines it adds or removes, then the cluster settling once its topology has taken the change in and it is available. An etcd restore shows its phases; the management cluster's applies and the template builds show the end of their log.

**Go to or run…** in the top bar, or `Ctrl K` (`⌘ K` on a Mac), finds any cluster, any of its views, a page or an action by a few letters of its name: `db opt` finds the options of `db`. Outside fields, single keys work too, and `?` lists them:

| Key | Does |
| --- | --- |
| `/` | the same as `Ctrl K` |
| `C` | the clusters |
| `N` | a new cluster |
| `A` | Administration, for those who may open it |
| `[`, `]` | the cluster before or after the open one, in the fleet's order, on the same view |
| `1` to `5` | the open cluster's Summary, Health, Machines, Backups and Options |
| `R` | shows or folds away the running work |

## The cluster page

The buttons beside the cluster's name act on the whole cluster: **Scale**, **Upgrade**, **Kubeconfig**, **Headlamp** and **Delete**. **Kubeconfig** and **Headlamp** appear once the cluster is available (Kubeconfig for those with `Sys.Modify` on its pool, Headlamp for clusters in the interface's namespaces); without `VM.Allocate` on its pool, or while it is being deleted, the others are disabled and the page says why. The tabs under the name switch between its views:

- **Summary:** first the cluster's machines laid out by the Proxmox VE node their VMs run on, control planes first, each with its state; the other nodes you may see follow empty, as places new machines may go, and the machines whose node you may not see come last. It carries the Machines view's warning when one node runs more than one control-plane VM. Then its status, distribution, Kubernetes version, API endpoint, pod and service ranges, ready control-plane nodes and workers, with autoscaled pools as `md-0: 2 of 0 to 5, autoscaled`, Proxmox VE pool, namespace, who created it when, the last automatic upgrade, and for kubeadm, RKE2 and k3s which build of their VM template the nodes run, when they next move to a newer one and when they last did ([Node image refresh](#node-image-refresh)), and for Talos the release the nodes run, the one new machines boot first when it differs, the node being upgraded and the last upgrade ([Talos upgrades](#talos-upgrades)).
- **Health:** whether the cluster is healthy, changing, degraded or unreachable, with tiles for the control plane (and etcd, on kubeadm), workers, node pressure, unwell pods, add-ons and machine health checks; then what needs attention, most serious first, each linking to its details. Below come the nodes (ready, pressure, cordons and taints, what their pods ask for, and the VM's CPU over the last hour and its memory, as far as you may see the VMs), the pods that are not well (crash loops, images that do not pull, pods that cannot be scheduled, containers killed for memory), volume claims still waiting, the add-ons' Helm releases, the worker pools, the control plane's and the health checks' conditions, the last hour's warnings from inside the cluster and from Cluster API, and last the cluster's own conditions. Those with `Sys.Modify` on the cluster's pool also get **Logs** on each unwell pod: the last 60 lines of a container, from its last run when it has not run since. With the shared Headlamp running, each pod also links to its page there. The page reads inside clusters of the interface's namespaces only; for others it shows what Cluster API knows.
- **Machines:** each node's role, status, phase, version, addresses, age, and the Proxmox VE node its VM runs on, as far as you may see the cluster's VMs. When one Proxmox VE node runs more than one of the control-plane VMs, the page warns and says what losing that node would cost; migrating one of them in Proxmox VE to a node of its own fixes it.
- **Backups:** the cluster's backups and restores, with **Back up now** and **Restore** ([Back up and restore](#back-up-and-restore)).
- **Options:** the pod network, CIS hardening, placement, Headlamp, monitoring, logs, backups, certificates, the autoscaler ([below](#autoscaling)), pod security, Istio, Cluster Mesh (on a cluster that runs Cilium), GitOps, Rancher, automatic upgrades and identity providers, each with **Edit** (**Remove** for Rancher on the cluster that runs it), disabled for those who may not change the cluster (the pod network and CIS hardening are fixed). A change of placement applies to the VMs created after it, by scaling up, an upgrade or a replacement; those running stay where they are. Nodes the cluster has that you cannot see are listed too, and may be kept.

The status reads **Provisioning**, **Available**, **Upgrading** or **Reconciling** while a change rolls out, **Deleting**, or **Failed**. A cluster deleted while its page is open says so.

### Staged changes

A cluster's settings change in two steps. **Save** in a dialog stages the change: the API server checks it with those staged before it, as it would make them, so a change it refuses shows its reason in the dialog, and nothing changes in the cluster yet. A bar above the views then counts what is staged and says what applying it does to the machines, and **Options** shows each staged value under the one in force. A dialog opened again starts from what is staged. **Review plan** lists each setting as it is and as it would be, and whether it replaces no machine, adds or removes machines, or replaces the control-plane machines, the workers or every machine, one at a time; **Apply** makes the changes together, as one change of the cluster. **Discard** drops them. Scaling, upgrading, Talos upgrades and every setting under **Options** are staged this way; deleting the cluster, backups, restores and snapshots run at once.

## Scale

**Scale** sets the control-plane nodes (1, 3, 5 or 7: an odd number, so etcd keeps a majority when one fails) and, for each worker pool, either its workers (0 to 100) or, under **Scaling**, **Autoscaled** between its fewest and most workers ([below](#autoscaling)).

## Autoscaling

An autoscaled pool is sized by the [cluster autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler) through Cluster API: it adds a worker while pods wait for room it would give them, up to the pool's most, and removes a worker that has stayed unneeded for 10 minutes, down to its fewest. Kubehenge's controller runs one autoscaler per cluster while any of its pools is autoscaled, and removes it with everything it was given once none is.

- **Switching on** in the Scale dialog sets the pool's fewest and most and leaves its number of workers to the autoscaler, starting from those it has. **Switching off** fixes the pool at the workers it runs at that moment, unless you type another number; nothing is added or removed by switching.
- **From none.** A pool with 0 as its fewest empties while nothing waits. The autoscaler reads what a new worker would hold from the pool's machines, cores and 90% of their memory and disk, the rest being what the node keeps for itself, and the pool's node labels (`node.cluster.x-k8s.io/…`), so a pod's node selector can bring a worker up from none. A pod that asks for more than 90% of a worker's memory is never given one.
- **Control planes** stay kept for the control plane while a pool may grow, even while every pool is at none: a workload never lands on them only to be moved once a worker comes. A cluster whose pools may all go down to none therefore runs no workloads at those times.
- **Add-ons** need a worker that stays: with any add-on that uses memory, at least one autoscaled pool keeps a fewest of 1 or more, and the add-ons' memory is shared out over the pools' fewest. The interface says so on the fewest, and Kubehenge refuses the cluster otherwise. Rancher's agent alone does not count: it runs only once there is a Rancher to register with, and its pod then brings a worker up from none.
- **Addresses.** The node addresses must hold every pool at its most, the control planes and one more for a rollout. The Scale dialog shows the room left on the most, and Kubehenge refuses a change that leaves fewer addresses than that, unless the cluster was already short by as much.
- **Room on Proxmox VE.** When the workers a pool may still add need more memory than the Proxmox VE nodes the cluster may use have free, the Scale dialog warns; it is advice, since memory frees and fills as other VMs come and go. A worker that finds no room waits: the cluster's events show `AutoscalerNoRoom` with the Proxmox VE provider's reason, and after 30 minutes the autoscaler gives the machine up and tries again. Shared storage suits autoscaled pools best, as it lets a new worker start on whichever allowed node has the room.
- **Expander and timing**, under **Options**, **Edit** beside Autoscaler: which pool grows when several could take the waiting pods (the one left with the least unused room, by default, the one that takes the most pods, or any), how long after adding a worker before any is removed, and how long a worker stays unneeded before it goes, each 10 minutes by default.

**Which release.** A cluster runs the cluster autoscaler release of its Kubernetes minor version, since each carries that minor's scheduler; a minor without a release yet runs the newest earlier one, and a cluster older than every release Kubehenge carries runs the oldest. They are, by Kubernetes minor:

| Kubernetes | Cluster autoscaler |
| --- | --- |
| 1.33 and older | 1.33.6 |
| 1.34 | 1.34.5 |
| 1.35 | 1.35.2 |
| 1.36 and newer | 1.36.1 |

**Where it runs.** By default each cluster's autoscaler runs in the management cluster, in the `kubehenge-autoscaler` namespace, and reaches its cluster with a token of its own there; the cluster holds no credential of the management cluster. Each asks for 64 MiB of memory, with a limit of 512 MiB. **Runs**, **On the cluster's own control planes** moves it into the cluster, sparing the management cluster that memory. It then needs the management cluster's API reachable from the cluster's nodes, and puts a management cluster token inside the cluster. That token reads the MachineDeployments, MachineSets and Machines of every cluster in the namespace, which an admission policy cannot narrow; the policy still lets it scale only this cluster's pools and mark only its machines. Anyone with the cluster's admin rights can read the token, so this choice needs `VM.Audit` on the workload pool, which sees every cluster's VMs anyway. Without the management cluster's address, set by its module, the autoscaler stays out and the cluster shows an `AutoscalerNotPlaced` event.

## Upgrade

**Upgrade** offers the newer releases of the cluster's minor version and of the next one: Kubernetes upgrades one minor version at a time and never back, and the interface refuses anything else. Cluster API replaces the control-plane nodes one by one first, then the workers. A kubeadm cluster needs the template of the new version first. Automatic upgrades inside a maintenance window are set under **Options**, **Edit** beside Automatic upgrades. Both are described in [upgrading.md](upgrading.md#workload-clusters).

## Node image refresh

The nodes of kubeadm, RKE2 and k3s clusters are clones of a VM template of their node OS, which the management cluster builds again every week with the node OS's updates ([templates.md](templates.md#the-weekly-build)). Once a new build is a day old, each cluster's nodes are replaced with clones of it inside the cluster's maintenance window, one at a time as in an upgrade, only while the cluster is healthy and nothing else rolls out, whether automatic Kubernetes upgrades are on or not. The day gives a broken build time to be noticed before every cluster takes it; deleting the build stops clusters that have not moved yet. Scaling up or replacing a failed node in between clones the build the other nodes run.

The cluster's **Summary** shows the build the nodes run, when they next move and to which build, and the last move. **Options**, **Edit** beside Automatic upgrades, switches it off for the cluster (**Move the nodes to new template builds**), which leaves them on their build until it is switched on again; with it on and Kubernetes upgrades off, the window still applies. Talos nodes take their updates by upgrading Talos instead ([below](#talos-upgrades)).

The administrator switches the weekly build on or off and sets its day and time under **Administration**, **Templates**; it is on by default.

## Talos upgrades

A Talos cluster's nodes are upgraded in place rather than replaced: **Options**, **Upgrade Talos** beside the Talos release, offers the newer releases of the cluster's minor version and the newest of the next one, which runs the cluster's Kubernetes version (or `talosVersion` with kubectl). Kubehenge's controller then upgrades one node at a time, control planes first: Talos installs the release to its other partition, the node is drained, rebooted and taken back into service once it is Ready on the new release. A node whose pods do not all leave within five minutes is rebooted anyway, with a warning event. Talos upgrades a minor version at a time and never back, and the interface and the cluster's checks refuse anything else.

The cluster's **Summary** shows the release, the node being upgraded and the last upgrade. A node that does not come back Ready, or comes back on its old release because Talos rolled back, stops the upgrade there: the Summary says which node and why, and the controller upgrades no other node until **Try again** (which needs `VM.Allocate` on the cluster's pool), or until the annotation `kubehenge.dev/talos-upgrade-failed` is removed with kubectl.

New machines, from scaling up or a repair, clone the template of the release the cluster was created on, or upgraded Kubernetes on last, and are upgraded to the cluster's release once they join, so a Talos upgrade replaces no machine. A Kubernetes upgrade moves new machines to the cluster's release, once its template is in the pool.

With **Move the nodes to new Talos patch releases** on under Automatic upgrades (`upgrades.nodeImages`, on by default), the controller moves the cluster to the newest patch release of its minor version inside the maintenance window, while the cluster is healthy and nothing else rolls out. A new minor version is always chosen by hand.

## Change add-ons and sign-in

- **Headlamp:** switch it on or off, set or clear its host name, and switch read-only access for everyone on or off. Switching it off uninstalls it.
- **Monitoring:** switch it on or off, and set or clear Grafana's host name. Switching it off uninstalls it with its metrics proxies and certificates, and cert-manager unless Rancher or the certificates add-on runs in the cluster; Prometheus's and Alertmanager's volumes are kept. With logs on, the dialog switches them off too.
- **Logs:** switch them on or off, choose the collector, and change the days kept or grow the volume. Switching the collector replaces one Alloy with the other; switching logs off uninstalls Loki and Alloy, and Loki's volume is kept. Without monitoring they cannot be switched on.
- **Backups:** switch them on or off, choose the target, change the schedule, its time zone and how long each backup is kept, and store, replace or remove the cluster's own access key, which needs `Sys.Modify` on the cluster's pool. A new target or schedule applies from the next backup; those made already stay where they are and keep the time they were given. Switching backups off uninstalls Velero; the backups stay in the store, and the cluster's keys beside it.
- **Certificates:** switch them on or off, change the issuer, the domain or the DNS provider, and store, replace or remove the DNS provider's credential; with the cluster's CA, **Download CA certificate** in the dialog, or **CA certificate** on the cluster's page, gives its certificate for clients to trust. A new issuer issues every certificate of the old one again. No change of them replaces a node. Switching them off removes the issuer, the default certificate and the credential's copy from the cluster, and cert-manager unless monitoring or Rancher runs there; the cluster's CA is kept for when they are switched on again.
- **Pod security:** choose the level Pod Security Admission and Kyverno each enforce; on a cluster hardened to the CIS benchmark, Pod Security Admission's stays `restricted`. A new Pod Security Admission level replaces the control-plane nodes one at a time; switching Kyverno off uninstalls its policies, then Kyverno.
- **Istio:** switch it on or off. Switching it off takes every namespace and pod out of the mesh first, then uninstalls it; its CRDs and their resources stay.
- **Cluster Mesh**, on a cluster that runs Cilium: join or leave, and choose the peers. Each listed peer shows whether it is linked, and why not; a peer you cannot see shows only that it is not linked, or does not exist. Peers come and go without restarting anything; joining or leaving upgrades Cilium and restarts its agents.
- **Load balancer:** choose MetalLB, kube-vip or none, and change its addresses. Switching uninstalls the one before; switched off, the cluster keeps the addresses for when it is switched on again.
- **GitOps:** switch it on or off, and choose the engine, the repository, how it is read and how the web interface signs in. The dialog shows the deploy key, with a button to copy it, and the fingerprints of the server's host keys, and stores a token or client secret. Switching it off, or to the other engine, removes the engine and leaves what it deployed running; the engine chosen starts syncing once the other has gone. Pointing it at a repository needs `Sys.Modify` on the cluster's pool; switching it off does not.
- **Rancher:** register with the Rancher Kubehenge installed, or leave it; leaving removes Rancher's agent from the cluster, except where `default` is held to `restricted` ([troubleshooting.md](troubleshooting.md#rancher)). Whether a cluster runs Rancher itself is chosen at creation. On the cluster that runs it, **Remove** beside Rancher under Options uninstalls Rancher after asking for the cluster's name: every registered cluster is first taken out of Rancher, which cleans up inside each, and Rancher's own settings, users and projects are lost. While it waits on that, the cluster shows a `RemovingRancher` event; everything else about the cluster is still applied.
- **Identity providers:** change them. Saving replaces the control-plane nodes one at a time, as any change of the API server's configuration does. Adding or changing a provider that neither the providers set for every cluster nor the cluster's own list hold needs `Sys.Modify` on the cluster's pool; without it the dialog shows the providers read-only, and any of them can be removed.

## Download a kubeconfig

**Kubeconfig** downloads `<name>.kubeconfig`, the cluster's admin kubeconfig as Cluster API keeps it, once the cluster is available. It needs `Sys.Modify` on the cluster's pool, and every download is logged with the user who made it.

On a Talos cluster, **Talosconfig** beside it downloads `<name>.talosconfig`, the credential for the nodes' Talos API (`talosctl`) as the Talos bootstrap provider keeps it, with the same privilege and the same log.

## Open a cluster in Headlamp

The appliance runs one Headlamp for the clusters of the interface's namespaces, on port 8443 of the appliance, behind the interface's sign-in. **Headlamp** in the top bar opens it, and **Headlamp** on a cluster's page opens that cluster (`https://<appliance>:8443/kubehenge/open/<namespace>.<name>`, which leads on to `/c/<namespace>.<name>`, such as `/c/clusters.web`). It lists only the clusters you may see, checked against `VM.Audit` on each cluster's pool.

Headlamp itself holds no credentials. Sign in to each cluster with your own token, such as a ServiceAccount token made with `kubectl create token`, or through the cluster's first OpenID Connect provider: the first of its own list, or, for a cluster that sets none, the first of the providers every cluster trusts. For the latter, the provider must accept `https://<appliance>:8443/oidc-callback` as a redirect URL; Headlamp signs in as a public client with PKCE, so no client secret is stored.

Where the administrator switched on the [Headlamp sign-in](install.md#headlamp-sign-in), a cluster opens already signed in as cluster-admin for whoever holds `Sys.Modify` on its pool, the privilege the admin kubeconfig takes. The token is made for your session, lasts an hour at most and never longer than the session, and stays in the appliance: the browser never holds it. A token of your own, or a sign-in through the identity provider, takes its place while Headlamp keeps it. In the cluster's audit log, what you do appears as the ServiceAccount `kube-system/kubehenge-headlamp`; the appliance's log names you beside the token's ID, which the audit log records with each request. Losing `Sys.Modify` ends the sign-in within 30 seconds; switching the setting off ends every token made. **Log out** in Headlamp signs you out of that cluster: the appliance drops your session's token, and Headlamp asks for a token or a sign-in of your own, until you open the cluster again with **Headlamp** on its page here, which signs you in again.

Where the administrator switched on the [Headlamp actions](install.md#headlamp-actions), Headlamp also offers the Helm app catalog and the Prometheus charts it ships with. The catalog lists a cluster's Helm releases and installs, upgrades, rolls back and uninstalls them, acting with your own credentials in the cluster, or the sign-in's, so the cluster lets you do no more than it already does; it installs charts only from the Helm repositories set for the appliance, which only those who may set the appliance up add or remove. The Prometheus plugin charts a cluster's metrics from a Prometheus running in it, such as the monitoring add-on's, through the cluster's API with your credentials. Node drain is offered either way, with your own credentials.

The shared Headlamp does not offer what a shared server must not: port forwards (Headlamp offers them in its desktop app alone, where a forward listens on your own machine), its external proxy, plugin changes, adding clusters or kubeconfigs, setting a token for others, its WebSocket multiplexer and its own metrics.

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

## Routes through the Gateway API

Every cluster has the Gateway API's standard channel: GatewayClass, Gateway, HTTPRoute, GRPCRoute, TLSRoute, TCPRoute, UDPRoute, ReferenceGrant, BackendTLSPolicy and ListenerSet. Traefik serves the GatewayClass `traefik`, and its Gateway `traefik-gateway` in `kube-system` has one listener, `web`, for HTTP on port 80 of every node and of the API address, which it reports as its address. Any namespace may attach routes to it, as any namespace may claim a host name with an Ingress:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: web
  namespace: shop
spec:
  parentRefs:
    - name: traefik-gateway
      namespace: kube-system
      sectionName: web
  hostnames: [shop.example.org]
  rules:
    - backendRefs:
        - name: web
          port: 80
```

With [certificates](#certificates) from DNS-01 or the cluster's CA, the Gateway also has a listener `websecure`, for HTTPS on port 443, which serves the default certificate and takes routes from every namespace: `sectionName: websecure` puts a route there.

A Gateway of your own with the class `traefik` works too; its listeners use Traefik's ports, 8000 for HTTP and 8443 for HTTPS, which the nodes' ports 80 and 443 lead to. Istio's waypoints, its layer 7 proxies, are Gateways of the class `istio-waypoint`, which istiod serves once the Istio add-on is on.

The Gateway API's admission policy (`safe-upgrades.gateway.networking.k8s.io`) refuses CRDs of the group from the experimental channel over standard ones, and from releases before v1.5.0, so that a chart that brings old CRDs fails rather than downgrades the cluster's. Kubehenge applies its release of the CRDs where they are missing, or older and no longer shipped by the distribution, and never puts an older release over a newer one; where the distribution's release has its own Gateway API chart (RKE2's `rke2-gateway-api-crd`, k3s's `gateway-api-crd`), the Gateway API is that chart's alone, at the chart's release ([architecture.md](architecture.md#networking)). The annotation `kubehenge.dev/gateway-api` on the Cluster names the oldest release among the CRDs once every one is in place.

## Certificates

The certificates add-on (`certificates: {enabled: true, …}`) installs cert-manager and a ClusterIssuer `kubehenge-ingress`, which signs or orders every certificate the cluster asks for. There are three issuers:

- **`acme-dns01`** (the default): an ACME server proves the names through a TXT record that cert-manager sets with the DNS provider's API. Works for clusters no one reaches from the internet, and for wildcards. Needs a `domain` and a DNS provider.
- **`acme-http01`**: the ACME server fetches a token over HTTP on port 80 of the name, which cert-manager serves through Traefik's Gateway. Needs every name to resolve publicly to the cluster and port 80 open; it can prove no wildcard, so it gives no default certificate.
- **`ca`**: a CA of the cluster's own, made by Kubehenge's controller, valid for ten years, and constrained to `domain`: it signs names in the domain and nothing else, no other name and no address, so trusting it trusts nothing beyond the cluster. Clients trust it once: **Download CA** under the cluster's **Options**, or the `<cluster>-ingress-ca` Secret beside the Cluster (`tls.crt`). A new domain makes a new CA. Add-on host names have to lie in the domain, or name a `tlsSecret`.

With `acme-dns01` and `ca`, the certificate `kubehenge-ingress` in `kube-system` covers `domain` and `*.domain`. Traefik serves it for every Ingress and route that names no certificate of its own, and on the Gateway's `websecure` listener. A name further down, or any name with `acme-http01`, gets its own certificate: the add-ons' Ingresses (Headlamp's, Grafana's, Argo CD's and Flux's web interface) ask for one themselves, and an Ingress of yours asks with either annotation:

```yaml
metadata:
  annotations:
    kubernetes.io/tls-acme: "true"   # or cert-manager.io/cluster-issuer: kubehenge-ingress
spec:
  tls:
    - hosts: [shop.apps.lab.example.org]
      secretName: shop-tls
```

A Gateway of your own asks for its listeners' certificates with `cert-manager.io/cluster-issuer: kubehenge-ingress` on the Gateway.

The ACME server is Let's Encrypt's, or its staging directory with `acme.staging` (certificates no browser trusts, for trying things out), or any other with `acme.server`; `acme.caBundle` is the CA of a server whose own certificate the usual CAs do not sign. `acme.email` receives the server's notices.

### DNS providers

| `dns01.provider` | Settings | The credential |
| --- | --- | --- |
| `cloudflare` | | an API token with Zone:DNS:Edit and Zone:Zone:Read on the zone |
| `route53` | `route53.region`, `route53.accessKeyID`, optional `route53.hostedZoneID` | the secret access key |
| `digitalocean` | | an API token with write access to the domain |
| `azuredns` | `azureDNS.subscriptionID`, `resourceGroupName`, `tenantID`, `clientID`, optional `hostedZoneName`, `environment` | the service principal's client secret |
| `clouddns` | `cloudDNS.project`, optional `hostedZoneName` | the service account's JSON key |
| `rfc2136` | `rfc2136.nameserver` (host:port), `tsigKeyName`, `tsigAlgorithm` (default `HMACSHA256`) | the TSIG secret, base64 as BIND writes it |
| `acmedns` | `acmeDNS.host` | the account JSON `acme-dns register` gives, by domain |
| `akamai` | `akamai.serviceConsumerDomain` | client token, client secret and access token, in an existing Secret only |

The credential comes from one of three places, the first that applies:

1. **`dns01.existingSecret`**: a Secret you create in the workload cluster's `cert-manager` namespace, with the credential under the key `token` (Akamai's under `clientToken`, `clientSecret` and `accessToken`). Kubehenge never sees it.
2. **`dns01.sharedCredential: true`**: the DNS-01 credential an administrator stored for every cluster under **Administration**, **Certificates**. Taking it needs `Sys.Modify` ([Who may do what](#who-may-do-what)), since it reaches every zone it was made for.
3. **The cluster's own**, typed in when creating the cluster or under **Options**. The interface keeps it in the Secret `<cluster>.dns01` beside the Cluster and never shows it again.

Kubehenge's controller copies the stored credential into the `cert-manager` namespace of the workload cluster (`kubehenge-dns01`) and replaces the copy, or removes it, as soon as the credential is stored again or removed. A cluster without a credential gets a `NoDNS01Credential` event, and its certificates wait.

Where the domain's DNS answers differently inside the network than outside (split-horizon DNS), `dns01.nameservers` names the servers (host:port) cert-manager checks the challenge's record with, such as the domain's authoritative servers.

## Persistent volumes

The default StorageClass `proxmox` puts volumes on the cluster's storage. They belong to a VM named `<name>-<hash>-volumes`, tagged `kubehenge` and `volumes`, in the cluster's pool, which is never started and exists so that only this cluster's token may attach them. A released volume is deleted through that VM. On storage local to each node, a volume lives on one node.

Every cluster runs the last release of the Proxmox VE CSI plugin that needs no more than `Datastore.AllocateSpace` on the cluster's storage. Its newer releases read the storage's configuration and need `Datastore.Allocate` there, which lets the cluster's token delete any volume on that storage and change the storage's definition. Where the administrator lets clusters choose it ([install.md](install.md#the-csi-plugins-newest-release)), a cluster can run the newest release instead (**Run the newest release of the Proxmox VE CSI plugin**, or the `csi` variable's `datastoreAllocate`); its token then gets `Datastore.Allocate` on the cluster's storage, and loses it again when switched off. The storage has to be the cluster's alone: Kubehenge refuses it on a storage the management cluster uses (its disks, its Talos image and the cloud-init images, and those the template builds download and upload to), one that VM templates have disks on, or one another cluster uses, and refuses another cluster on a storage a cluster holds it on. Switching it on takes your own `Datastore.Allocate` on the storage. When a refusal comes up after the cluster switched it on, such as a template built on its storage later, the cluster goes back to the earlier release and `kubectl describe cluster` shows a `DatastoreAllocateRefused` event saying why.

## Back up and restore

A cluster with backups on runs Velero, which backs up on the cluster's schedule to its target, under `<prefix>/<namespace>.<name>/velero` in the target's bucket. Each backup holds the cluster's resources, every namespace but `kube-system`, `kube-public`, `kube-node-lease`, Velero's and the add-ons', which Kubehenge installs again itself, and the files of every volume its pods mount, emptyDir volumes aside. `kube-system` holds the cluster's Proxmox VE token, which no backup carries.

What lands in the store is encrypted with keys of the cluster's own, made when backups are first switched on and kept beside the Cluster in the management cluster, in the Secret `<name>-backup-keys`: the store encrypts the resources with the cluster's SSE-C key, and Kopia, which Velero reads the volumes with, encrypts their data with the cluster's repository password. Neither key is in the store, so whoever can read the bucket, with the access key every cluster shares among them, sees the names of the backups and namespaces and nothing of what they hold. The Secret outlives the cluster, so that a cluster created again under the same name, backing up to the same target, can restore what the one before backed up; deleting it leaves those backups unreadable for good.

The **Backups** view says whether Velero reaches the target and when it last backed up, lists every backup with its state, who started it or whether the schedule did, when it started, when it runs out, how many items it holds and what went wrong, and the restores below with how far each has come. Seeing it needs `VM.Audit` on the cluster's pool.

- **Back up now** makes a backup at once, as the schedule would. It needs `VM.Allocate` on the cluster's pool.
- **Restore**, beside a finished backup, brings back the whole backup or the namespaces named. What the cluster still has is left as it is, so a deleted namespace comes back whole, and an object that still exists keeps its current state. It needs `Sys.Modify` on the cluster's pool, as the admin kubeconfig does: a restore writes whatever the backup holds, Secrets and roles among it.

The view reaches Velero through two accounts of Kubehenge's in the cluster's `velero` namespace, one that reads the backups and starts one, and one that starts a restore; neither reads a Secret or anything outside Velero's own resources. With kubectl, `velero backup create` and `velero restore create --from-backup <backup>` work as anywhere else.

Velero runs as `cluster-admin`, which backing up and restoring every resource needs, and its privileged backup pods read every pod's volumes as root from the host, so the `velero` namespace is exempt from pod security. Restoring a backup into another cluster, and deleting a backup before it runs out, are done with the Velero CLI against the cluster.

### Volume snapshots

With **Take volume snapshots** (the `csi` variable's `volumeSnapshots`), the cluster's CSI plugin takes VolumeSnapshots of its volumes, restores them into new volumes and clones volumes (a PersistentVolumeClaim whose `dataSource` is another claim). Proxmox VE copies a volume only for `root@pam` signed in with its password, never for an API token, so the plugin of such a cluster signs in as `root@pam` instead of with the cluster's token; the cloud controller manager keeps the token.

- **What it grants:** `root@pam` holds every privilege on the whole Proxmox VE cluster. Whoever takes over the cluster, or reads the Secret its CSI plugin signs in with (`kube-system/kubehenge-proxmox-root` in the cluster), can do anything on every Proxmox VE node. Switch it on only for clusters run by those you would give `root@pam` to.
- **Where it is offered:** where the administrator stored `root@pam`'s password ([install.md](install.md#volume-snapshots)), and only on storage local to each node of a type the plugin copies on: ZFS, LVM, LVM-thin, directory or btrfs. Kubehenge refuses it on shared storage and on any other type, and refuses another storage for a cluster that has it on. `root@pam` must sign in without a second factor. Switching it on takes your own `Permissions.Modify` on `/`, which only those who could make themselves `root@pam`'s equal hold.
- **What the cluster gets:** the plugin's newest release with its `csi-snapshotter` sidecar, the VolumeSnapshotClass `proxmox` (the default, deleting a snapshot's volume with it), and on kubeadm and k3s the external snapshotter's CustomResourceDefinitions and snapshot controller. RKE2 runs a snapshot controller of its own, which the cluster uses.
- **How long a copy may take:** the plugin waits up to 4 minutes for Proxmox VE to copy a volume, and the sidecars wait 5 minutes for the plugin. A copy that takes longer, a volume of a few tens of GB on a single disk, fails; when the sidecar asks again, the plugin may find the half-written copy and report the snapshot or volume ready while Proxmox VE is still writing it. Keep the volumes you snapshot well below that size, or check a large snapshot's data before relying on it.
- **Switching it off** removes the sidecar, the VolumeSnapshotClass and the snapshot controller, and the plugin signs in with the cluster's token again. The CustomResourceDefinitions stay, so VolumeSnapshots still in the cluster keep their volumes on Proxmox VE: delete them while the option is on, or they go when the cluster is deleted.

When Kubehenge cannot give the plugin `root@pam`, such as after the administrator removed the password, the plugin keeps the cluster's token and `kubectl describe cluster` shows a `VolumeSnapshotsRefused` event saying why.

## etcd snapshots

A cluster with etcd snapshots on has Kubehenge's controller copy its etcd database, every object the cluster's API holds, to one of the backup targets an administrator set: on the cluster's schedule, by default every 12 hours from 00:00 UTC with each cluster a few minutes after the hour of its own, and whenever asked. It keeps the newest 14 by default and deletes older ones. Snapshots go under `<prefix>/<namespace>.<name>/etcd` in the target's bucket, beside the cluster's Velero backups, named by when they were taken in UTC and the Kubernetes version, as `20261021T120000Z-v1.36.5-rke2r1.db.age`. Each object carries the Kubernetes version, the distribution, etcd's revision and the SHA-256 and size of the database in its metadata. Snapshots work on their own, with or without Velero backups.

Nothing runs in the cluster for them, and no etcd key or S3 access key enters it, unless the distribution's own upload is on ([below](#the-distributions-own-upload)). For each snapshot the controller signs itself an etcd client certificate valid for an hour, in memory, from the cluster's etcd CA, which the control-plane provider keeps in the management cluster. It reaches one etcd member through the cluster's API server, by a port-forward to the member's pod on kubeadm and RKE2 or to the k3s provider's etcd proxy on k3s, and streams the database into the target as it comes, checking etcd's own checksum at its end, so that a snapshot cut off is never kept. On Talos, where etcd is a service of Talos's own rather than a pod, the controller asks a control plane's Talos API for the snapshot instead, with the cluster's talosconfig. Each snapshot is encrypted with [age](https://age-encryption.org) to a key of the cluster's own, `etcd-snapshot-identity` in the Secret `<name>-backup-keys` beside the Cluster, made with the cluster's other backup keys and outliving it as they do. It is written with the same access key as Velero's backups. RKE2 and k3s also keep the snapshots they take themselves on their nodes, as they always do.

The **Backups** view's etcd snapshots section says when the last snapshot was taken and what went wrong with the last try, and lists the snapshots in the target. Snapshots are switched on, and their target, schedule, time zone and number kept chosen, under Options or from **Set up etcd snapshots…**.

- **Take a snapshot now** has the controller take one at once. It needs `VM.Allocate` on the cluster's pool.
- **Download** hands the snapshot out decrypted, as etcd wrote it. It holds every Secret of the cluster, so it needs `Sys.Modify` on the cluster's pool, as the admin kubeconfig does.
- **Restore…** restores the cluster's etcd from it ([below](#restore-etcd)). It needs `Sys.Modify` on the cluster's pool.

With kubectl, the annotation `kubehenge.dev/etcd-snapshot-requested` with `{"at":"<time>"}` asks for a snapshot, and `kubehenge.dev/etcd-snapshot` records the last one. A snapshot copied from the target decrypts with `kubehenge etcd-snapshot decrypt --identity <key file> <snapshot.db.age> <snapshot.db>`, which checks etcd's checksum too, or with any age client, the key file holding the key from `<name>-backup-keys`:

```sh
kubectl --namespace clusters get secret web-backup-keys -o jsonpath='{.data.etcd-snapshot-identity}' | base64 -d > web.key
age --decrypt --identity web.key --output snapshot.db 20261021T120000Z-v1.36.5-rke2r1.db.age
```

### The distribution's own upload

On RKE2 and k3s, **Also have RKE2 upload snapshots of its own** (`distributionUpload: true`, off by default) has the cluster's servers upload the snapshots they take themselves to the same target too, under `<prefix>/<namespace>.<name>/rke2-etcd` or `k3s-etcd`, on the cluster's schedule and keeping as many. They go on while the management cluster is down, which Kubehenge's own snapshots do not. kubeadm has no upload of its own. Both distributions get the same: a configuration drop-in that points the servers at a Secret Kubehenge writes into the cluster, with the target's certificate verified.

What it grants: the target's access key goes into the Secret `kubehenge-etcd-s3` in the cluster's `kube-system`. Whoever may read Secrets there, every cluster admin among them, reads it, and with it everything the key reaches in the target: the other clusters' backups and snapshots too when it is the credential for every cluster, so give the cluster a credential of its own for a bucket or folder of its own first. The key stays out of the machines' bootstrap data, which is why CAPRKE2's own `etcd.backupConfig` is not used: that puts the key into every server's bootstrap data and skips TLS verification by default; the k3s provider has no such setting at all.

What it risks and costs:

- The snapshots are stored as etcd wrote them, not encrypted by Kubehenge, and hold every Secret of the cluster.
- Kubehenge neither lists nor restores them. They are restored by hand with the distribution's own steps: on the first server, with every server stopped, `rke2 server --cluster-reset --etcd-s3 --etcd-s3-bucket=<bucket> --etcd-s3-folder=<folder> --etcd-s3-endpoint=<host> --etcd-s3-access-key=… --etcd-s3-secret-key=… --cluster-reset-restore-path=<snapshot>` (or `k3s server` the same), then the other servers rejoin as in step 6 [below](#restore-etcd).
- The distribution reads the schedule in UTC whatever the time zone chosen, and counts the number kept across all servers together.
- The drop-in is part of the control plane's bootstrap, so switching the upload on or off, or changing the schedule or the number kept while it is on, replaces the control-plane machines one at a time.

The upload needs etcd snapshots on, and is refused on kubeadm. Switching snapshots off switches it off too, and the controller takes the Secret out of the cluster again once every server that still has the drop-in has been replaced, so that the servers not yet replaced keep uploading on schedule.

### Restore etcd

Restoring etcd takes every object of the cluster back to the snapshot: Deployments, Secrets, roles, custom resources and the rest of what its API holds return to how they were, and whatever was created since is gone. The data in volumes is not restored and stays as it is now. It is for a cluster that lost its etcd quorum, or whose state is corrupted or was deleted in bulk; to bring back applications with their data, restore a [Velero backup](#back-up-and-restore). A cluster is restored only from a snapshot of its own distribution and Kubernetes minor version, whose objects its API server reads. A Talos cluster is restored through its nodes' Talos API whether or not the management cluster restores through the guest agent: Kubehenge wipes etcd on every control-plane node, which reboots each, restores it from the snapshot on the first, and the others join it again, so no machine is replaced.

A restore:

1. pauses the cluster in Cluster API, so that it replaces no machine meanwhile;
2. copies the snapshot to the first control-plane node and checks its SHA-256;
3. stops etcd and the API server on every control-plane node, on kubeadm with the controller manager and the scheduler;
4. restores etcd on the first node as a cluster of one member: with `etcdutl snapshot restore --bump-revision 1000000000 --mark-compacted`, run from the etcd image the node already runs, on kubeadm, keeping the database from before beside it, so that every client's watch ends and it reads the restored objects afresh; with `rke2 server --cluster-reset --cluster-reset-restore-path=<file> --etcd-s3=false` on RKE2, and `k3s server` the same on k3s;
5. starts its control plane and waits for its API server;
6. deletes the other control-plane machines, whose etcd still holds the cluster from before, and gives the cluster back to Cluster API, which makes them again to join the restored member.

Where an administrator has switched on [etcd restores](install.md#etcd-restores), **Restore…** asks for the cluster's name and Kubehenge runs these steps itself through the control-plane VMs' guest agents, the section showing which step it is at. The snapshot reaches the node decrypted by the controller and encrypted again for the copy with a password of its own for that restore, from a link into the target that expires within the hour. A control-plane VM that is off is replaced with the others. A restore that fails before etcd was restored starts every control plane again as it was; one that fails after finishes the restore.

Otherwise **Restore…** lists the steps for the chosen snapshot, with the commands for the cluster's distribution: those for the nodes run as root over SSH or the VM's console, after downloading the snapshot to the first node as `/var/lib/kubehenge-restore/snapshot.db`, and the `kubectl` ones with the management cluster's kubeconfig.

## Delete

**Delete** asks for the cluster's name. It deletes the cluster's VMs and its persistent volumes, and removes it from Rancher first. It cannot be undone. Its backups and etcd snapshots stay in their target, and its backup keys beside it ([Back up and restore](#back-up-and-restore)). Once the VMs are gone, Kubehenge's controller removes the cluster's pool, user, token and volume owner VM. If a node that may hold some of the cluster's volumes on its local storage is offline, the controller waits for it. Cluster API removes each VM's cloud-init ISO (`user-data-<VMID>.iso` on the cloud-init storage) with the VM; only a VM deleted by hand, outside Cluster API, leaves its ISO behind, which can then be removed under the storage's ISO Images in Proxmox VE.

Deleting the cluster that runs Rancher first removes every registered cluster from Rancher.

## Who may do what

The interface checks the signed-in user's Proxmox VE privileges on each cluster's pool, then acts with its own account in the management cluster. Clusters are shared with the same ACLs as VMs.

| To | Privilege | On |
| --- | --- | --- |
| See a cluster and its health page, open it in the shared Headlamp, and download its CA | `VM.Audit` | the cluster's pool |
| Create a cluster | `VM.Allocate` | the workload pool |
| | `Datastore.AllocateSpace` | `/storage/<storage>` |
| | `SDN.Use` | `/sdn/zones/<zone>/<bridge>`, or `…/<bridge>/<vlan>` with a VLAN tag |
| Place a cluster by an HA rule's nodes | `Sys.Audit`, or `VM.Allocate` where the rules are offered to cluster creators | `/`; the workload pool or the cluster's pool |
| Install Rancher | `Sys.Modify` | the workload pool |
| Run the CSI plugin's newest release | `Datastore.Allocate` | `/storage/<storage>` |
| Have a missing template built, where the administrator allows it | `VM.Allocate` | the workload pool |
| Switch volume snapshots on | `Permissions.Modify` | `/` |
| Use the DNS-01 credential stored for every cluster | `Sys.Modify` | the workload pool when creating, the cluster's pool when changing |
| Scale, upgrade, change add-ons, automatic upgrades and sign-in, delete | `VM.Allocate` | the cluster's pool |
| See its backups and restores | `VM.Audit` | the cluster's pool |
| Back up now | `VM.Allocate` | the cluster's pool |
| Restore a backup | `Sys.Modify` | the cluster's pool |
| See its etcd snapshots | `VM.Audit` | the cluster's pool |
| Take an etcd snapshot now | `VM.Allocate` | the cluster's pool |
| Download an etcd snapshot, restore etcd | `Sys.Modify` | the cluster's pool |
| Store a cluster's own backup access key | `Sys.Modify` | the workload pool when creating, the cluster's pool when changing |
| Download the admin kubeconfig | `Sys.Modify` | the cluster's pool |
| Open the cluster in the shared Headlamp signed in as cluster-admin, where the [Headlamp sign-in](install.md#headlamp-sign-in) is on | `Sys.Modify` | the cluster's pool |
| Use the Helm app catalog and the Prometheus charts in the shared Headlamp, where the [Headlamp actions](install.md#headlamp-actions) are on, with the cluster's own rights for what they do in it | `VM.Audit` | the cluster's pool |
| Add, update or remove a Helm repository in the shared Headlamp | the setup's privileges | `/` |
| Read an unwell pod's logs on the health page | `Sys.Modify` | the cluster's pool |
| Trust identity providers beyond those set for every cluster | `Sys.Modify` | the workload pool when creating, the cluster's pool when changing |
| Choose the repository GitOps syncs from | `Sys.Modify` | the workload pool when creating, the cluster's pool when changing |
| Run a cluster's autoscaler on its own control planes | `VM.Audit` | the workload pool |
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

The interface changes only clusters in its own namespaces: `clusters`, and those an administrator adds below. Clusters created with kubectl elsewhere are shown to those who may see them, but are changed with kubectl.

### More namespaces for clusters

An administrator can let cluster creators choose among more namespaces than `clusters`, such as one per team: **Namespaces for clusters** in the setup and on the Administration page, or the module's `extra_cluster_namespaces` ([install.md](install.md)). The wizard then asks for the namespace, the cluster list shows each cluster's namespace beside its name, and the interface looks after the clusters of every one of them as it does those of `clusters`.

- A cluster's pool still carries its namespace, `<workload pool>/<namespace>.<name>`, so the privileges above work the same in every namespace: creating one takes `VM.Allocate` on the workload pool, whichever namespace it goes to.
- A namespace that does not exist yet is created. One taken off the list keeps its clusters; the interface then shows them read-only, and they are changed with kubectl.
- Cluster Mesh links clusters of one namespace only.
- Names Kubernetes, the providers and Kubehenge use themselves are refused: `default`, `cert-manager`, `kube-*` and `*-system`.

## With kubectl

Clusters are Kubernetes objects in the management cluster, so they can be created and changed with kubectl, or kept in Git with any GitOps tool.

Get the management cluster's admin kubeconfig under **Administration**, **Appliance** (**Repair access**, **Download admin kubeconfig**, with the state passphrase), or with `tofu output -raw kubeconfig` if you applied it with Terraform/OpenTofu. Whoever holds it controls every cluster: Proxmox VE privileges do not apply to kubectl.

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
- **The namespace** decides the pool: `<workload pool>/<namespace>.<name>`. Clusters in the interface's namespaces (`clusters`, and any [added for clusters](#more-namespaces-for-clusters)) are also looked after from the interface; any other namespace works, and the interface shows those clusters read-only. The ClusterClasses stay in `kubehenge-system`, which `topology.classRef.namespace` names, so every namespace uses the same ones. Keep names to 40 characters, as the interface does, so that the nodes' host names stay within one DNS label.
- **Worker pools** are machine deployments of the class `worker`; a Cluster may have several. An autoscaled pool has no `replicas` and carries Cluster API's annotations `cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size` and `-max-size` under `metadata.annotations` (at most 100), with the pool's node labels under `metadata.labels`.
- **Ranges** come from `spec.clusterNetwork`; without it, the distribution's own apply (`10.244.0.0/16` and `10.96.0.0/12` for kubeadm, `10.42.0.0/16` and `10.43.0.0/16` for RKE2 and k3s). Kubehenge refuses to create a cluster whose ranges overlap its nodes' network.

### Variables

| Variable | Required | What it is |
| --- | --- | --- |
| `controlPlaneEndpoint` | yes | The control plane's virtual IP: an IPv4 address, unused, outside `network.addresses`. |
| `network` | yes | `bridge`, one of the bridges clusters may use; `vlan` (optional, 1 to 4094); `addresses`, a list of ranges, networks in CIDR notation or single addresses; `prefix` (1 to 32); `gateway`; `dnsServers`. |
| `storage` | yes | The storage for the nodes' disks and the persistent volumes, one of those clusters may use. |
| `allowedNodes` | no | Proxmox VE nodes the cluster's VMs may run on, at least one; every node when unset. New VMs go to those of them that are online ([above](#machines)). The interface takes only nodes the user can see, or those the cluster already has. A change applies to VMs created after it. |
| `proxmoxHA` | no | Whether Proxmox VE HA keeps the control-plane VMs on separate nodes ([above](#machines)); default true, where the management cluster has Proxmox VE HA and the storage is shared. |
| `csi` | no | `datastoreAllocate` (default false): run the CSI plugin's newest release, with `Datastore.Allocate` for the cluster's token on its storage ([above](#persistent-volumes)), where the management cluster lets clusters choose it and on a storage of the cluster's own. `volumeSnapshots` (default false): take volume snapshots, with the CSI plugin signed in as `root@pam` ([above](#volume-snapshots)), where the management cluster lets clusters take them and on storage local to each node. |
| `ccm` | no | `haGroups`: have the cloud controller manager label the nodes with their Proxmox VE HA groups, with `Sys.Audit` on `/` for the cluster's token ([above](#machines)); default true unless the management cluster switched the labels off for every cluster, where it may not be true. |
| `controlPlaneMachine`, `workerMachine` | no | `cores` (default 2), `memoryMiB` (a multiple of 8, default 4096, with the minimums [above](#machines)), `diskGiB` (default 30 for the control plane and 40 for workers, at least 20; a disk only grows). |
| `cni` | kubeadm only | kubeadm: `cilium` or `calico`. RKE2: `canal` (default), `calico` or `cilium`. k3s and Talos: `flannel` (default), `cilium` or `calico`. The class refuses a change once the cluster exists. |
| `loadBalancer` | no | Addresses for Services of type `LoadBalancer` ([above](#load-balancer)): `enabled` (default false); `implementation`, `metallb` (default) or `kube-vip`; `addresses`, a list of ranges, networks in CIDR notation or single addresses on the nodes' network, needed while `enabled`. |
| `headlamp` | no | `enabled` (default false); `hostname`, to serve it through the cluster's ingress controller; `tlsSecret`, a Secret in the `headlamp` namespace with the certificate for `hostname` (when unset, one from the certificates add-on while it is on, else the ingress controller's default certificate); `access`, `token` (default: everyone signs in with their own token or through OpenID Connect) or `view` (Headlamp shows everyone who reaches it the cluster read-only through its own account, bound to the `view` ClusterRole). |
| `backups` | no | Velero ([above](#back-up-and-restore)): `enabled` (default false); `target`, the name of one of the administrator's targets, needed while `enabled`; `schedule`, a cron expression of five fields or a descriptor such as `@weekly` (default `0 2 * * *`); `timeZone` (default `UTC`); `retentionDays` (1 to 3650, default 7); `volumes`, `all` (default) or `annotated`, only the volumes a pod names in its `backup.velero.io/backup-volumes` annotation; `existingSecret`, a Secret of the owner's own in the cluster's `velero` namespace with the store's keys as Velero's credentials file under the key `cloud`, instead of the credential stored through the interface for the cluster, or else for every cluster; Kubehenge creates the namespace, and switching backups off deletes it with the Secret. |
| `etcdSnapshots` | no | etcd snapshots ([above](#etcd-snapshots)): `enabled` (default false); `target`, the name of one of the administrator's targets, needed while `enabled`; `schedule`, as for `backups` (default `0 */12 * * *`); `timeZone` (default `UTC`); `keep`, the number of snapshots kept (1 to 1000, default 14); `distributionUpload` (default false), on RKE2 and k3s only and while `enabled`, the distribution's own upload ([above](#the-distributions-own-upload)). They are written with the credential stored through the interface for the cluster, or else for every cluster. |
| `certificates` | no | `enabled` (default false); `issuer`, `acme-dns01` (default), `acme-http01` or `ca`; `domain`, needed with `acme-dns01` and `ca`; `acme`: `email`, `server`, `staging`, `caBundle`; `dns01`: `provider`, its settings, `existingSecret`, `sharedCredential`, `nameservers` ([above](#certificates)). |
| `rancher` | no | `install` (default false), which needs `hostname` and at least 4 vCPUs on every node; `register` (default true), to register with the Rancher Kubehenge installed. Setting `install` back to false removes every registered cluster from Rancher, then uninstalls it. A second cluster with `install` gets no Rancher; `kubectl describe cluster` shows a `RancherRefused` event. |
| `gitops` | no | `enabled` (default false); `engine`, `argocd` (default) or `flux`; `repository`, with `url` (`https://` or `ssh://`), `revision` (a branch or full ref, default `main`) and `path` (default `.`); `auth`, `none` (default), `deployKey` (over `ssh://`; the key is in the Secret `<name>-gitops` beside the Cluster, its public half under `identity.pub`), `token` (the cluster's `gitops` credential, `username` and `password`) or `existingSecret` with `existingSecret`; `knownHosts`, the server's host keys as known_hosts lines, read from the server once when unset; `hostname` and `tlsSecret`, to serve the web interface through the ingress controller; `admins`, the first provider's groups that administer Argo CD; `signIn`, with `clientID` (the provider's own when unset) and `clientSecret`, `none` (default, PKCE), `stored` (the cluster's `oidc-client` credential, key `clientSecret`) or `existingSecret` with `existingSecret`; `ui` (default false), Flux's web interface, which needs `hostname` and a client secret. `ui` and `admins` need an identity provider in `oidc`. |
| `oidc` | no | Up to eight providers: `issuerURL`, `clientID`, `usernameClaim`, `usernamePrefix`, `groupsClaim`, `groupsPrefix`, `scopes`, `certificateAuthority` ([above](#signing-in-with-openid-connect)). Unset, the providers set for every cluster. |
| `upgrades` | no | Automatic Kubernetes upgrades: `mode` (`off`, `patch` or `minor`; default `off`) and `window` ([upgrading.md](upgrading.md#automatically-inside-a-window)); `nodeImages` (default true), whether the nodes move to new builds of their VM template in the window ([above](#node-image-refresh)), and on Talos to new Talos patch releases ([above](#talos-upgrades)). |
| `talosVersion` | Talos only | The Talos release the nodes run, such as `v1.14.4`. Changing it upgrades them in place ([above](#talos-upgrades)): a minor version at a time, never back, and only to a release that runs the cluster's Kubernetes version. |
| `autoscaler` | no | How the cluster autoscaler sizes the autoscaled pools ([above](#autoscaling)): `placement`, `management` (default) or `workload`; `expander`, `least-waste` (default), `most-pods` or `random`; `scaleDownDelayAfterAddMinutes` and `scaleDownUnneededMinutes`, 1 to 1440, default 10. |

### Day two

```sh
kubectl -n clusters patch cluster prod --type=json \
  -p '[{"op":"replace","path":"/spec/topology/workers/machineDeployments/0/replicas","value":5}]'
kubectl -n clusters patch cluster prod --type=merge -p '{"spec":{"topology":{"version":"v1.37.1+rke2r1"}}}'
kubectl -n clusters delete cluster prod
```
