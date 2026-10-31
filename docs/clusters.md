# Clusters

Every cluster Kubehenge creates is a Cluster API Cluster built from one of Kubehenge's ClusterClasses: kubeadm, RKE2, k3s or Talos. Its VMs live in a Proxmox VE pool of their own under the workload pool, `<workload pool>/<namespace>.<name>`. A cluster `prod` created in the interface lands in `kubehenge/clusters.prod`.

## What every cluster gets

- **The Proxmox VE cloud controller manager** initialises new nodes and labels them with a region and a zone. The region is the Proxmox VE cluster's name (the node's, on a single node outside any cluster) and the zone is the node.
- **The Proxmox CSI plugin**, with a default StorageClass `proxmox`: ext4 volumes on the cluster's storage, deleted when released.
- **A control-plane virtual IP**, held by kube-vip on one control-plane node and moved to another when it fails.
- **A CNI**: the distribution's own, or the one chosen at creation.
- **An ingress controller**, Traefik, on ports 80 and 443 of every node, the control planes included. A host name pointing at the cluster's API address or at any node reaches it.
  - RKE2 and k3s bring their own. Kubehenge installs it into kubeadm and Talos clusters, in `kube-system`.
- **The Gateway API**, so that Gateways and HTTPRoutes work beside Ingresses. It has the standard channel's CRDs.
  - Traefik's Gateway `traefik-gateway` in `kube-system` listens for HTTP where Traefik does. It listens for HTTPS too once the cluster has [certificates](#certificates), and takes routes from every namespace ([below](#routes-through-the-gateway-api)).
- **Health checks.** A node that stays not ready for five minutes, or does not join within twenty, is replaced, workers one at a time.
  - A machine marked with `kubectl annotate machine <name> cluster.x-k8s.io/remediate-machine=` is replaced the same way.
  - A control-plane machine is replaced only while etcd keeps its majority.
- **Control-plane nodes kept for the control plane.** Once a worker has joined, the control-plane nodes carry the taint `node-role.kubernetes.io/control-plane:NoSchedule` in every distribution.
  - A cluster scaled to no workers has the taint taken off, and its control-plane nodes run the workloads.
  - A worker that fails leaves the taint in place.
  - Pods that started on a control-plane node before the first worker joined stay there until they are next scheduled.
- **Its own Proxmox VE user and API token**, with rights on its own pool only. The cloud controller manager and the CSI plugin's controller on the control-plane nodes hold it ([architecture.md](architecture.md#each-clusters-own-corner-of-proxmox-ve)).

Before the first cluster, the VM templates for its distribution have to exist ([install.md](install.md#create-the-vm-templates)).

## Create a cluster in the interface

**Create cluster** on the cluster list opens a wizard of six steps: General, Network, Machines, Add-ons, Identity providers and Confirm.

A field shows what is wrong with it once it has been changed, or when **Next** is pressed with something missing. **Next** then moves to the first such field. A step in the bar above opens only once the steps before it are complete. **Cancel** asks before it discards what was entered.

Choosing another distribution brings its defaults only into what you have not set yet.

Without `VM.Allocate` on the workload pool, **Create cluster** is off and says so. Before the first cluster, the list shows which distributions have a VM template, with **Build one** for administrators where none has. It also shows whether you may create clusters.

### General

| Field | What it is |
| --- | --- |
| Start from | Once there is a cluster: the cluster to copy settings from, by default the newest that works, or **Defaults**. It copies the distribution, node OS, node counts and sizes, bridge, VLAN, prefix length, gateway, DNS servers, load balancer, pod network and storage. It also copies placement, Proxmox VE HA, HA group labels, Headlamp, monitoring, logs, Pod Security, CIS hardening, Istio, Rancher registration and identity providers. Everything else starts at its default. Choosing another cluster keeps the name, the namespace, and the Kubernetes version while the distribution stays. |
| Namespace | Only where an administrator has named [more namespaces for clusters](#more-namespaces-for-clusters): the namespace in the management cluster the cluster goes to, `clusters` unless another is chosen. |
| Name | Lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit, at most 40 characters. It also names the cluster's Proxmox VE pool and VMs. |
| Distribution | kubeadm, RKE2, k3s or Talos: one per ClusterClass in the management cluster. A distribution whose VM template is not in the template pool yet says so. The cluster cannot be created until the template is built ([templates.md](templates.md)). An administrator builds it. Where cluster creators may have missing templates built, **Build template** builds it from the wizard. |
| Node OS | For kubeadm, RKE2 and k3s: the operating system of the nodes, AlmaLinux 10 unless another is chosen ([templates.md](templates.md#node-oses)). One not released yet is shown but cannot be chosen. One without a template on the distribution says so. A Talos cluster's nodes run Talos, so there is none to choose. |
| Talos release | For Talos only: the release the nodes run, among those the template pool has a template of, the newest chosen first. The newest release of each minor version the Talos Image Factory builds is listed too, marked as having no template yet. Where cluster creators may have missing templates built, one of those can be asked for here. Later releases are an upgrade away ([below](#talos-upgrades)). |
| Kubernetes version | For RKE2 and k3s: the newest release of each of the four newest minor versions. They come from the distribution's release channels as far as this Kubehenge release pins them ([architecture.md](architecture.md#release-pins-for-rke2-and-k3s)). The stable one is chosen first. A release it does not pin is refused, from kubectl too. A cluster on an older minor version can still be upgraded from it. For kubeadm: the newest version of each minor version a VM template has been built for on the chosen node OS, the newest chosen first. Where cluster creators may have missing templates built, **Kubernetes version to build** asks for another version's template. It is offered here once built, after about twenty minutes. For Talos: Kubernetes' newest release of each minor version the chosen Talos release runs, its default chosen first. Versions are in the distribution's own form: `v1.36.5` for kubeadm and Talos, `v1.36.5+rke2r1` for RKE2, `v1.36.5+k3s1` for k3s. When the list cannot be read, type the version in. |
| Control-plane nodes | 1, 3, 5 or 7. Three or more keep the cluster running when one fails. |
| Workers | 0 to 100. With none, the control-plane nodes run the workloads too. **Autoscaled**, instead of **Worker count**, has the cluster autoscaler keep the workers between **Fewest workers** and **Most workers**. **Fewest workers** takes 0 to 100, **Most workers** 1 to 100 ([below](#autoscaling)). |

### Network

| Field | What it is |
| --- | --- |
| Bridge | The bridge the cluster's VMs attach to, one of those chosen for clusters when the management cluster was set up. Bridges you may not use (no `SDN.Use`) are shown but disabled. The wizard chooses the only one left, or else the first a node reaches its gateway through. That node's network on the bridge fills in the prefix length, gateway and DNS servers, and the example API and node addresses. All of them can be changed. When the management cluster names no bridges and Proxmox VE lists none to you (no `Sys.Audit` on the nodes), type the name in. |
| VLAN tag | Optional, 1 to 4094. With a tag, the wizard fills in nothing from the bridge's untagged network. |
| API address | The control plane's virtual IP: an unused address in the nodes' network, outside the node addresses. |
| Node addresses | Where the nodes take their addresses from: ranges (`192.0.2.21-192.0.2.29`), networks or single addresses inside the gateway's network, one per line or separated by commas. The wizard asks for at least one address more than there are nodes, for the extra node an upgrade or a replacement adds. It counts an autoscaled pool at its most. Leave more for scaling up. |
| Prefix length | Of the nodes' network. |
| Gateway | The nodes' default gateway. |
| DNS servers | IPv4 addresses, one per line or separated by commas. |
| Load balancer | What gives Services of type `LoadBalancer` their addresses, [below](#load-balancer). |
| Load-balancer addresses | With a load balancer: the addresses it hands out, as ranges, networks or single addresses. |
| Pod network | The CNI, [below](#pod-network). |
| Pod range, Service range | Optional, under **Pod and service ranges**. |

Where a node reports the bridge's network, the wizard fills in the API address and the node addresses. It takes a free run from the network's 20th address on, clear of:

- the gateway, the node and every other cluster's addresses on the bridge
- any address where a host answers the management cluster's ping
- any address where a host answers a connection from the management cluster on port 22, 80, 443, 6443, 8006 or 50000

Addresses you type are probed the same way. Those a host answers on are marked **In use**. A host that answers none of these probes looks free, so the probe is only advice. The controller refuses only addresses another cluster holds.

The interface's pod pings without privileges through the `net.ipv4.ping_group_range` sysctl. Without it, only the connections are tried.

The nodes' network is the gateway with the prefix length. Pod and service ranges must stay clear of it and of each other, or traffic for the nodes would be routed into the cluster.

Kubehenge sets both ranges on every cluster it creates. It takes those you give, and refuses them if they overlap. Otherwise it takes the distribution's own when they are clear of the nodes' network (`10.244.0.0/16` and `10.96.0.0/12` for kubeadm and Talos, `10.42.0.0/16` and `10.43.0.0/16` for RKE2 and k3s). Otherwise it takes the first of a few private ranges that are clear.

#### Load balancer

Without one, kubeadm and RKE2 clusters give Services of type `LoadBalancer` no address. On k3s, its own servicelb answers for each of them on every node's address, on any port but 80 and 443. The ingress controller holds those two there.

With one, each such Service takes an address of its own from the load-balancer addresses. One node at a time answers for it on the nodes' network. When that node fails, another takes over. Choose:

- **MetalLB** (recommended), in layer 2 mode, in the `metallb-system` namespace.
- **kube-vip**, which already holds the API address, in `kube-system`, with kube-vip's cloud provider handing out the addresses.

The addresses must be unused and on the nodes' network. They must differ from the node addresses, the API address, the gateway and the network's own and broadcast addresses. Kubehenge refuses addresses that clash with these when the cluster is created and whenever its addresses change. It refuses those that clash with the node addresses, API address or load-balancer addresses of another cluster on the same bridge and VLAN too.

On k3s, a load balancer switches servicelb off. The load balancer can be switched, or changed to other addresses, later ([below](#change-settings)). Services whose addresses are outside the new range then take new ones from it. A switch from MetalLB to kube-vip hands every Service its address afresh.

#### Pod network

The CNI is chosen now and fixed for the cluster's life. Switching a running cluster's pod network would leave the old one's interfaces, routes and addresses on every node.

| Distribution | Choices | Default |
| --- | --- | --- |
| kubeadm | Cilium, Calico | none: one has to be chosen |
| RKE2 | canal, Calico, Cilium | canal |
| k3s | flannel, Cilium, Calico | flannel |
| Talos | flannel, Cilium, Calico | flannel |

RKE2 installs its choice itself. For kubeadm, and for k3s and Talos given Cilium or Calico, Kubehenge's controller installs it. Then k3s's flannel and network policy controller are switched off, and Talos's flannel is left out.

### Machines

| Field | What it is |
| --- | --- |
| Storage | For the nodes' disks and the cluster's persistent volumes: one of those chosen for clusters when the management cluster was set up. Only storages you may allocate space on are listed, with their free space, and whether they are shared. A single one is chosen already. Shared storage lets the cluster's VMs start on any node. |
| Control plane, Workers | Cores, memory in MiB (in steps of 8) and disk in GiB. |
| Placement | The Proxmox VE nodes the cluster's VMs may run on: **Any node**, **HA rule nodes**, or **Chosen nodes**. |
| Proxmox VE HA | Shown when the management cluster has Proxmox VE HA and the storage is shared. On by default. |
| Newest CSI plugin release | Shown when the management cluster lets clusters choose it. Off by default. See [Persistent volumes](#persistent-volumes). |
| HA group labels | Shown unless the management cluster switched them off for every cluster. On by default. See below. |
| Volume snapshots | Shown when the storage is local to each node. Off by default. See [Volume snapshots](#volume-snapshots). |

Placement and the four options below it change later under **Options**.

#### Placement

Each new VM goes to an online node it may use. It is the one running the fewest of the cluster's VMs of its kind (control plane or worker) with the memory for it. So three control planes on three nodes run one to a node.

**Any node** means every node of the Proxmox VE cluster, nodes added later included. A node that goes down is left out within about 15 seconds. It is taken back once it is online. The VMs that replace those it ran go to the others. While every node chosen is down, no new VM is placed elsewhere.

**HA rule nodes** lists Proxmox VE's enabled HA node-affinity rules (Datacenter, HA, Affinity Rules) with the nodes each one allows. That is a rule's own nodes, or every other node for a rule that keeps resources off them. Choosing a rule copies its nodes into the cluster once. A later change of the rule does not move the cluster, and the cluster's VMs do not become HA resources. Reading the rules takes `Sys.Audit` on `/`.

Without it the choice says so. The nodes can still be chosen one by one, unless an administrator has the rules offered to cluster creators ([install-settings.md](install-settings.md#ha-rule-placements)). In that case everyone who may create or change a cluster sees each rule's name and the nodes it allows. They do not see its comment or whether it is strict. What they see is up to 30 seconds old.

The rules Kubehenge keeps for its own clusters' Proxmox VE HA are not offered.

#### Proxmox VE HA

**Proxmox VE HA** keeps the control-plane VMs on separate nodes while they run, which placement alone cannot do. A VM moved by hand, or restarted elsewhere, stays where it went. Once a control-plane machine has joined the cluster, Kubehenge's controller makes its VM an HA resource (Datacenter, HA, Resources), commented `Kubehenge cluster <pool>`. It also adds an HA rule of the cluster's own that keeps the VMs apart (Datacenter, HA, Affinity Rules). When the cluster may use only some nodes, a second rule keeps them to those.

Proxmox VE's HA manager then migrates VMs that share a node apart, and restarts the VM of a failed node on another one. A node running an HA resource arms Proxmox VE's watchdog, so it restarts itself when it loses quorum. On a Proxmox VE cluster without a reliable quorum, leave HA off. Workers are not HA resources. A worker lost with its node is replaced by the health check.

With more control planes than nodes, the rule keeps as many apart as there are nodes. Proxmox VE HA takes effect only when the administrator switched it on for the management cluster ([install.md](install.md#the-settings)). The cluster's disks must also be on shared storage. HA can restart a VM only where its disks are reachable. Unticking **Proxmox VE HA**, or deleting a control-plane machine, takes its VM out of HA first.

Changing the rules or resources by hand is undone by the controller.

#### HA group labels

**Add HA group labels** has the cloud controller manager label each node with its Proxmox VE node's HA groups (or HA node-affinity rules). The label is `group.topology.proxmox.sinextra.dev/<group or rule>`, with an empty value. Proxmox VE lists them only with `Sys.Audit` on `/`, so the cluster's token gets it there, unpropagated, while the option is on. Kubehenge's controller adds the cluster's user to a group holding it, and takes it out again when switched off.

That lets the token read the Proxmox VE cluster's own configuration (firewall rules, backup jobs, HA, the corosync configuration), but change nothing. It reads nothing of the guests, storages or nodes beyond what it reads anyway. The option is on by default unless the administrator switched it off for every cluster ([install-settings.md](install-settings.md#ha-group-labels)). Switching it on or off takes no privilege beyond changing the cluster.

Unticked, the cloud controller manager logs a permission error for each node every few minutes and the nodes go without the labels. Ticked again, the nodes get the labels within a few minutes. The cloud controller manager does not remove them when the option is switched off. When the controller cannot grant `Sys.Audit`, `kubectl describe cluster` shows a `SysAuditRefused` event saying why.

#### Sizes

The defaults are 2 cores and 4096 MiB, with a 30 GiB disk for the control plane and 40 GiB for workers. Disks are at least 20 GiB. A control-plane node needs at least 2 cores and 4096 MiB, the first row of the k3s and RKE2 server sizing guides. For kubeadm, which documents no sizes, RKE2's guide applies.

With more workers the guides ask for more. Kubehenge refuses a cluster whose control planes fall short, when it is created and when its workers or machines change. RKE2 and kubeadm need 4 cores and 8192 MiB from 226 workers. They need 8 cores and 16384 MiB from 451 and 16 cores and 32768 MiB from 1,301. For k3s the same sizes start at 351, 901 and 1,801 workers.

A worker needs at least 1024 MiB on k3s, 2048 MiB on kubeadm, and 2 cores and 4096 MiB on RKE2. It never gets less than its distribution's minimum for a node.

The guides size servers whose workloads run on workers. A cluster without workers runs them on its control planes. So each needs a worker's least on top. On k3s that is 3 cores and 5120 MiB, on kubeadm 3 cores and 6144 MiB, and on RKE2 4 cores and 8192 MiB.

The interface raises the control planes to that when the workers are set to none. It raises sizes only while a cluster is created. Once the cluster exists, only **Resize** ([Pools](#pools)) changes them. With kubectl a change of size rolls the nodes, and a disk only grows.

The add-ons need memory and CPU too. Their pods run on the workers, or on the control planes of a cluster without workers. Each of those nodes needs the larger of two figures. One is its distribution's least. The other is what such a node takes itself plus its share of the pods' memory and CPU.

Every control plane also holds the add-ons' resource types and objects in its API server. So the memory it uses itself grows with each add-on, workers or not.

The API server runs with a Go memory limit of 40% of its control plane's memory. On k3s the limit is half of it, for the server's whole process. So the API server frees memory as it nears the limit, not at twice what it holds. The end-to-end tests measure the memory, and the figures leave room to grow.

The CPU is what the add-ons' pods request as Kubehenge installs them. It counts one node's share of those that run on every node. Kubernetes schedules a pod only onto a node with that much CPU not yet requested, however idle the node is, and leaves it pending otherwise.

| Add-on | Pods' memory | Each control plane, kubeadm, RKE2 and Talos | Each control plane, k3s | Pods' CPU |
| --- | --- | --- | --- | --- |
| Headlamp | 64 MiB | 8 MiB | none | 10 millicores |
| Monitoring | 1536 MiB | 32 MiB | 616 MiB | 300 millicores |
| Logs | 512 MiB | 56 MiB | none | 75 millicores |
| Backups | 384 MiB | 184 MiB | 24 MiB | 70 millicores |
| Kyverno | 512 MiB | 552 MiB | 520 MiB | 500 millicores |
| Istio | 384 MiB | 288 MiB | 504 MiB | 400 millicores |
| Cilium Cluster Mesh | 128 MiB | none | none | 30 millicores |
| Load balancer | 128 MiB | none | none | 60 millicores |
| GitOps with Argo CD | 512 MiB | 432 MiB | 360 MiB | 90 millicores |
| GitOps with Flux | 384 MiB | none | none | 50 millicores |
| cert-manager, with monitoring, Rancher or certificates | 192 MiB | 48 MiB | none | 30 millicores |
| Rancher | 2048 MiB | 512 MiB | 512 MiB | 120 millicores |
| Rancher's agent, in every cluster that registers | 512 MiB | none | none | 30 millicores |

A worker counts 448 MiB for itself on k3s and 704 MiB on kubeadm, RKE2 and Talos. A control plane counts 1848 MiB on k3s, 2392 MiB on Talos, 3120 MiB on RKE2 and 3376 MiB on kubeadm before any add-on. With every add-on but Rancher it needs 3872 MiB on k3s, 3992 MiB on Talos, 4720 MiB on RKE2 and 4976 MiB on kubeadm. These are above the least of 4096 MiB on RKE2 and kubeadm.

A single k3s worker with every add-on but Rancher and GitOps needs 4672 MiB. It needs 4288 MiB without backups, 4160 MiB without logs, or 3776 MiB without either. GitOps with Argo CD on top takes each 512 MiB further.

The default machines hold every add-on but Rancher, logs, backups and GitOps on every distribution. The exception is kubeadm's control plane, which needs 4304 MiB for them, above the default 4096 MiB. A second worker makes room for logs and backups too.

For CPU a worker counts what its distribution, its CNI and Kubehenge's own pods request there. That is 250 millicores on kubeadm, 350 on k3s, 490 on Talos and 775 on RKE2. Talos keeps 50 of them back for the system, and its flannel asks for 200 with its network policy controller. On RKE2, kube-proxy asks for 250 and canal for 260. Without workers, a control plane counts 1080 millicores on kubeadm, 430 on k3s, 910 on Talos and 1605 on RKE2.

Every add-on but Rancher takes 1595 millicores with Argo CD, or 1555 with Flux. So it fits on one worker of the default 2 cores on kubeadm and k3s. Talos and RKE2 need a third core for it, or a second worker.

On a worker of a single core, the least kubeadm, k3s and Talos allow, Kyverno and Istio do not fit together. Nor do either of them with every other add-on. Such a worker needs a second core, or a second worker beside it.

Kubehenge refuses a cluster short of that room when it is created and when an add-on is switched on or its nodes change. The interface raises the machines to it as add-ons are chosen and shows the add-ons' totals on the Add-ons step.

### Add-ons

Each add-on the class offers is a card with the memory its pods take by the class's figures. It opens on its settings once chosen. Under the cards is the least memory and cores each node that carries them needs.

- **Headlamp**, a web interface inside the cluster, with an optional host name to serve it at through the cluster's ingress controller.
  - It has no rights of its own. Everyone signs in with their own token or through OpenID Connect.
  - With **Read-only without sign-in** (off by default), its service account is bound to Kubernetes' built-in `view` ClusterRole. Headlamp uses that account for every request.
  - Anyone who reaches it, at its host name or through a port-forward, sees the workloads, Services, ConfigMaps, events and pod logs of every namespace.
  - They also see custom resources whose roles add themselves to `view`. None of this needs a sign-in.
  - It shows no Secrets and no nodes, and changes nothing. A token given to it is not used, so changes go through kubectl or the shared Headlamp.
  - Without a host name, reach it with `kubectl -n headlamp port-forward svc/headlamp 8080:80`.
  - Every cluster in the interface's namespaces can also be opened in the appliance's shared Headlamp once it is available. That needs no add-on ([below](#open-a-cluster-in-headlamp)).
- **Monitoring**: Prometheus, Alertmanager and Grafana, with kube-prometheus-stack's dashboards and alerts for every node and control-plane component.
  - Each component is scraped over verified, authenticated TLS ([architecture.md](architecture.md#add-ons)).
  - It installs cert-manager too, and takes about 1.5 GiB of the workers' memory ([Machines](#machines)) and a 20 GiB volume.
  - Grafana's `admin` password is in the `kube-prometheus-stack-grafana` Secret of the `monitoring` namespace.
  - With a host name, Grafana is served at it through the cluster's ingress controller. Without one, reach it with `kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80`.
- **Logs**, beside monitoring: Loki keeps the cluster's logs on a volume of its own and Grafana Alloy collects them.
  - Monitoring's Grafana reads them as its Loki data source ([architecture.md](architecture.md#add-ons)).
  - Logs are kept for 7 days in a 10 GiB volume unless set otherwise. The volume only grows and should hold the days kept, since Loki deletes by age, not by size.
  - Without monitoring there are no logs. Kubehenge refuses them, and refuses switching monitoring off while they are on.
  - The collector is chosen with them:
    - **On every node** (recommended): Alloy runs on every node in the `logging-node` namespace, exempt from pod security, and reads the host's log files read-only.
      - That gives it every container's log, short-lived pods' too, and the node's journal. On the control planes it also gives the API server's audit log.
    - **Through the Kubernetes API**: one Alloy in `logging` reads the pods' logs through the API server, with no access to the host.
      - It is lighter, but misses the journal, the audit log and pods that end before it reads them.
- **Backups** with Velero, to one of the S3 stores an administrator set ([install-settings.md](install-settings.md#backup-targets)). They run daily at 02:00 by default, each kept for 7 days.
  - Velero backs up every namespace but Kubernetes' own and the add-ons', with the data in its pods' volumes. The cluster writes under a folder of its own in the store, encrypted with keys of its own ([Back up and restore](#back-up-and-restore)).
  - Choose the target, the schedule, its time zone and how long each backup is kept. Choose the cluster's own access key too, which Velero signs in to the store with.
  - Storing that key needs `Sys.Modify` on the workload pool. Without it, backups wait until someone holding that stores one.
  - The key an administrator stored for every cluster never goes into a cluster. Without any target, the choice is off and says so.
- **Certificates** from cert-manager for the cluster's Ingresses and routes.
  - They come from Let's Encrypt or another ACME server, or from a CA of the cluster's own. An ACME server proves the names through your DNS provider's API (DNS-01) or over HTTP (HTTP-01).
  - With DNS-01 or the cluster's CA, a certificate covers a domain and every name one label beneath it. It is served wherever no other is asked for.
  - Traefik's Gateway listens for HTTPS with it ([below](#certificates)).
  - DNS-01 takes the DNS provider's credential, typed in here and stored by the interface, or the one an administrator stored for every cluster.
  - It installs cert-manager too.
- **Pod security**: the [Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/) pods are held to, `baseline` or `restricted`. Kubernetes' Pod Security Admission, Kyverno or both enforce it, each at a level of its own.
  - Pod Security Admission sets the level of every namespace that labels none of its own, `baseline` unless another is chosen. It warns about and audits pods short of `restricted` at every level.
  - Kyverno holds every namespace to its level whatever the labels say, and reports what falls short.
  - `kube-system` and the namespaces of the add-ons whose components need the host are exempt from Pod Security Admission in every cluster.
  - They are exempt from Kyverno only where the cluster runs that add-on ([architecture.md](architecture.md#add-ons)). So switching an add-on never replaces nodes.
  - Who may run privileged pods is decided, as everywhere in Kubernetes, by who may create and label namespaces. A namespace's labels set its own level.
  - A workload that needs more gets its namespace labelled, and with Kyverno a PolicyException in the `kyverno` namespace.
- **CIS hardening** to the CIS Kubernetes Benchmark, beyond the baseline every cluster gets ([architecture.md](architecture.md#add-ons)). It is fixed once the cluster exists.
  - Pod Security Admission enforces `restricted` outside the exempt namespaces.
  - Every container that asks for no seccomp profile of its own runs under the container runtime's default.
  - Exec, attach and port-forward sessions end after five idle minutes.
  - RKE2 runs its own CIS profile, under which the pods in `kube-system`, `kube-public` and `default` take connections only from their own namespace.
- **Istio** in ambient mode: label a namespace `istio.io/dataplane-mode=ambient` and its pods talk to each other over mutual TLS, with their workload identities, without sidecars ([architecture.md](architecture.md#add-ons)).
  - Under a NetworkPolicy that denies by default, allow ztunnel's port 15008/TCP between the enrolled pods. Allow 169.254.7.127/32 too, where the kubelet's probes come from.
- **Join a Cluster Mesh**, for a cluster that runs Cilium: choose the other Cilium clusters of the namespace to link with.
  - Two clusters are linked while both are in the mesh and list each other.
  - Their pods then reach each other's, by address and through Cilium's global services. Each cluster reads the other's pod names, labels and identities ([architecture.md](architecture.md#add-ons)).
  - A peer listed before it exists is linked once it does.
  - Peers reach the cluster on port 32379/TCP of its API address, which has to be open between them.
  - A cluster that runs Cilium has a name of at most 32 characters. It gets a pod range no other Kubehenge cluster uses, clear of the nodes' network.
- **GitOps**: Argo CD (recommended) or Flux keeps the cluster in step with a directory of a Git repository.
  - What the directory holds is applied, and what goes from it is removed ([architecture.md](architecture.md#add-ons)).
  - Give the repository's URL, branch or ref and directory, and how it is read.
    - As it is, for a public repository over HTTPS.
    - With a deploy key over SSH (recommended for a private repository).
      - Kubehenge makes the key once the cluster is saved. It shows the key under its **Options**, to add to the repository as a read-only deploy key.
    - With a user name and read-only token over HTTPS, stored under **Options** once the cluster exists.
    - With a Secret of your own.
  - Over SSH, give the server's host keys as `known_hosts` lines, from the forge's published keys or `ssh-keygen -F <host>`. Kubehenge never reads them from the network, and shows their fingerprints beside the deploy key.
  - Argo CD signs in through the cluster's first identity provider, its local admin disabled. The provider's groups listed as admins administer it, and nobody else has any rights in it.
  - With a host name it is served through the cluster's ingress controller. Without one, reach it with `kubectl -n argocd port-forward svc/argocd-server 8080:80`.
  - Flux's web interface is off by default. Switched on, it shows every Flux object and the workloads they deployed at its host name.
  - It lets those signed in reconcile, suspend and resume them, with the rights the cluster's RBAC gives them. It signs in with a client secret.
  - Choosing the repository needs `Sys.Modify` on the workload pool, since the engine applies whatever the repository holds with cluster-wide rights.
  - Flux is installed by the Flux Operator, which is licensed under AGPL-3.0. Kubehenge runs it unmodified.
- **Rancher.** Install Rancher into this cluster, with cert-manager.
  - Every other cluster then registers with it on its own.
  - Kubehenge creates and upgrades the clusters, so Rancher's upgrades of registered RKE2 and k3s clusters are switched off.
  - It needs a host name that resolves to this cluster's ingress controller (such as its API address) for people and for every registered cluster.
  - Every node of Rancher's cluster needs at least 4 vCPUs, the least Rancher's documentation sizes it for. Kubehenge refuses fewer, at creation and when the machines change later.
  - The interface raises the cores to 4 when Rancher is chosen.
  - Memory follows the number of clusters and nodes Rancher manages. Rancher's documentation gives:
    - 16 GB per node for up to about 150 clusters and 1,500 nodes.
    - 32 GB with 8 vCPUs for up to 300 and 3,000.
    - 64 GB with 16 vCPUs for up to 1,000 and 10,000.
  - For Rancher to stay up while a node is lost, give its cluster three control planes.
  - Only one cluster may run Rancher. The interface refuses a second, and should one be set with kubectl, it gets no Rancher and a `RancherRefused` event says why.
  - Kubehenge keeps Rancher's cluster within the Kubernetes versions Rancher's release runs on.
  - It does not build it at a version outside them. The Cluster is created, gets no VMs, and its conditions say why.
  - It does not install Rancher into an existing cluster outside them, and holds back upgrades beyond them.
  - Installing Rancher needs `Sys.Modify` on the workload pool, since Rancher receives the admin kubeconfig of every cluster that registers. Without it the choice is off and says so.
  - Its first admin password is generated into the `bootstrap-secret` Secret in the `cattle-system` namespace of that cluster.
- **Register with Rancher**, the one Kubehenge installed, if any: on by default for every cluster that does not install Rancher itself.
- **Upgrade automatically**: off, patch releases, or patch and minor releases, inside a window. The window is every day from 02:00 for four hours in your browser's time zone.
  - The nodes of a kubeadm, RKE2 or k3s cluster move to new builds of their VM template in the same window. This happens whatever this setting says ([below](#node-image-refresh)).
  - The cluster's **Options** change the window later ([upgrading.md](upgrading.md#automatically-inside-a-window)).

### Identity providers

The OpenID Connect providers whose ID tokens the cluster's API server accepts, besides certificates and ServiceAccount tokens, which always work. The list starts with the providers set for every cluster on the Administration page. Keeping or dropping them needs no further privilege. Adding or changing one needs `Sys.Modify` on the workload pool, and without it the providers can only be removed. See [Signing in with OpenID Connect](#signing-in-with-openid-connect).

### Confirm

Checks the cluster before it exists. The API server runs every webhook and admission policy on it without creating it (`POST /api/clusters?dryRun=All`, as `kubectl create --dry-run=server` would). The memory and disk its VMs take at their most are weighed against what the nodes it may use have free. Its storage is weighed the same way. This is advice.

Below are every setting by name and the Cluster as the API server would store it, as YAML. **Copy as YAML** copies it for `kubectl apply` or a Git repository. Credentials typed into the wizard are not in it, and are stored once the cluster exists. **Create** is offered once the API server accepts the cluster. It creates the cluster and opens its page.

Kubehenge's controller first gives the cluster its Proxmox VE pool, user, token and the VM that will own its persistent volumes. Only then does Cluster API clone the VMs. The cluster shows **Provisioning** until it is available.

## Finding your way

Every page shows the fleet down its left side, grouped by where each cluster stands, each with why in a few words:

- **Needs you:** something failed and waits for someone.
  - A failed Talos upgrade, a failed machine, or a cluster Cluster API reports failed.
  - A cluster still not available an hour after it was created, one Kubehenge's controller fails on, or one whose machines are stuck ([below](#the-cluster-page)).
  - A cluster whose [health page](#the-cluster-page) finds trouble: an API server that does not answer, a node not ready or a pod that keeps crashing.
  - The line names the worst of it.
- **Running:** a change is under way: a cluster being created, upgraded, rolled out, scaled or deleted, or an add-on still being installed.
- **Healthy:** nothing is going on and its health page finds nothing. The line under the name says what it runs.

Each entry opens the view that says more: the health page for what it found, the machines for a rollout. The health pages are read for the whole fleet at most every 30 seconds.

Down the right side of a wide window, **Runs** follows the work that goes on by itself once started. That is the changes listed under **Running**. It is also each cluster's etcd snapshots and restores and its backups and restores, each with how far it has come. Those who may set up the management cluster also see its applies, the template builds and refreshes, and the appliance's upgrades.

A run that failed stays there for a day with the first error it reported. One that went well stays for an hour. A change or a creation whose machines are stuck moves under **Stalled**, with the worst its machines report. It moves back under **Running** once one of them is ready.

**Runs** folds away with **›** or `T` `R`. In a narrower window **Running** in the top bar shows it.

Each run opens a page of its own. It shows what the run does and on which cluster, and who started it and how long it has taken. It pins the first error at the top and lists its steps, each done, under way or waiting.

A failed run has **Try again**. It starts the run again as it was started. A restore keeps its choices and a template build its own. An upgrade goes to its release. An etcd restore uses its snapshot.

Where the run took the state passphrase or a typed name, Try again asks for it again. A new backup or restore opens the cluster's backups, where it shows first. Every run links to the page it is started from.

A change [applied here](#staged-changes) is a run too, kept for a day, in place of the cluster's line under **Running**. Its first step is the API server accepting it. Then come the machines it changes:

- For a replacement these are the control-plane machines or the workers, counted as the new ones are ready and the old ones gone.
- For an addition or removal they are the machines it adds or removes.

Then the cluster settles once its topology has taken the change in and it is available.

An etcd restore and an upgrade of the appliance show their steps. An etcd snapshot, an etcd restore and a change show the events the controller and Cluster API reported of them since they began. The management cluster keeps events a day, as long as a failed run stays listed.

The management cluster's applies and the template builds show the end of their log. A finished backup or restore shows the end of the log Velero kept of it in the target. Those who may download the cluster's kubeconfig see it.

**Go to or run…** in the top bar, or `Ctrl K` (`⌘ K` on a Mac), finds any cluster, any of its views, a page or an action. A few letters of its name are enough: `db opt` finds the options of `db`. A cluster's actions are there too, those you may start: Scale, Upgrade, Delete and its kubeconfig or talosconfig. So `db upg` opens the upgrade of `db` on its page.

Each is an address as well, the cluster's page with `?do=scale`, `upgrade`, `delete`, `kubeconfig` or `talosconfig`. Opening it starts the action once and removes `?do=` from the address. Outside fields, keys work too, and `?` lists them. Those that leave the page or change its layout are two keys pressed one after the other. So a stray key does neither:

| Keys | Does |
| --- | --- |
| `/` | the same as `Ctrl K` |
| `G` `C` | the clusters |
| `G` `N` | a new cluster |
| `G` `A` | Administration, for those who may open it |
| `[`, `]` | the cluster before or after the open one, in the fleet's order, on the same view |
| `1` to `5` | the open cluster's Summary, Health, Machines, Backups and Options |
| `T` `R` | shows or folds away the running work |

## The cluster page

The buttons beside the cluster's name act on the whole cluster: **Scale**, **Upgrade**, **Kubeconfig**, **Support bundle**, **Headlamp** and **Delete**. **Kubeconfig** and **Headlamp** appear once the cluster is available (Kubeconfig for those with `Sys.Modify` on its pool, Headlamp for clusters in the interface's namespaces). **Support bundle** appears for those with `Sys.Modify` on its pool. It downloads the same bundle as `kubehenge diagnose` ([troubleshooting.md](troubleshooting.md#a-clusters-support-bundle)). Without `VM.Allocate` on its pool, or while it is being deleted, the others are disabled and the page says why. The tabs under the name switch between its views:

- **Summary:** first the cluster's machines laid out by the Proxmox VE node their VMs run on, control planes first, each with its state.
  - The other nodes you may see follow empty, as places new machines may go. The machines whose node you may not see come last.
  - It carries the Machines view's warning when one node runs more than one control-plane VM.
  - Then its status, distribution, Kubernetes version, API endpoint, and pod and service ranges.
  - Then the ready control-plane nodes and workers, with autoscaled pools as `md-0: 2 of 0 to 5, autoscaled`.
  - Then the Proxmox VE pool, namespace, who created it when, and the last automatic upgrade.
  - For kubeadm, RKE2 and k3s it shows which build of their VM template the nodes run. It also shows when they next move to a newer one and when they last did ([Node image refresh](#node-image-refresh)).
  - For Talos it shows the release the nodes run and the one new machines boot first when it differs. It also shows the node being upgraded and the last upgrade ([Talos upgrades](#talos-upgrades)).
- **Health:** whether the cluster is healthy, changing, degraded or unreachable. It has tiles for the control plane (and etcd, on kubeadm), workers, node pressure, unwell pods, add-ons and machine health checks.
  - Then what needs attention, most serious first, each linking to its details.
  - Below come the nodes, with ready, pressure, cordons and taints and what their pods ask for.
  - They also show the VM's CPU over the last hour and its memory, as far as you may see the VMs.
  - Then the pods that are not well (crash loops, images that do not pull, pods that cannot be scheduled, containers killed for memory).
  - Then volume claims still waiting, the add-ons' Helm releases, the worker pools, and the control plane's and the health checks' conditions.
  - Then the last hour's warnings from inside the cluster and from Cluster API, and last the cluster's own conditions.
  - Those with `Sys.Modify` on the cluster's pool also get **Logs** on each unwell pod. It shows the last 60 lines of a container, from its last run when it has not run since.
  - With the shared Headlamp running, each pod also links to its page there.
  - The page reads inside clusters of the interface's namespaces only. For others it shows what Cluster API knows.
- **Machines:** each node's role, status, phase, version, addresses and age. It also shows the Proxmox VE node its VM runs on, as far as you may see the cluster's VMs.
  - When one Proxmox VE node runs more than one of the control-plane VMs, the page warns and says what losing that node would cost. Migrating one of them in Proxmox VE to a node of its own fixes it.
  - Those with `VM.Allocate` on the cluster's pool get **Replace** on each machine, which sets the annotation `cluster.x-k8s.io/remediate-machine`. They also get **Keep** to take it back before the replacement starts.
  - While the replacement waits, as for a control plane that cannot lose a member, the machine says why.
  - On a worker they also get **Delete**. It drains the machine and deletes its VM and disks, and its pool shrinks by one.
  - As with kubectl, **Delete** marks the machine with `cluster.x-k8s.io/delete-machine=` and lowers the pool's `replicas` in the Cluster, so Cluster API deletes that machine before any other.
  - An autoscaled pool's workers are the autoscaler's to delete.
  - **Cordon** keeps new pods off a machine's node and **Uncordon** lets them on again, as `kubectl cordon` and `kubectl uncordon` do. The node shows as cordoned.
  - Those with `VM.PowerMgmt` get **Reboot**, which has Proxmox VE shut the VM down and start it again. Those with `VM.Console` get **Console**, which opens the VM's console in Proxmox VE's web interface, signed in there as it asks.
  - Those with `Sys.Modify` on the cluster's pool also get **Boot log** on each machine. It shows what its node logged as it booted, read by the controller, for a node that never joined ([troubleshooting.md](troubleshooting.md#a-nodes-boot-log)).
- **Backups:** the cluster's backups and restores, with **Back up now** and **Restore…** ([Back up and restore](#back-up-and-restore)).
- **Options:** every setting that can change once the cluster exists, each listed with what changing it does [below](#change-settings).
  - Each has **Edit** (**Upgrade** for the Talos release, **Uninstall** for Rancher on the cluster that runs it). It is disabled for those who may not change the cluster.
  - The pod network and CIS hardening are fixed, so they have none.

The status reads **Provisioning**, **Available**, **Upgrading**, **Reconciling** (while a change rolls out), **Deleting** or **Failed**. A cluster deleted while its page is open says so. While Health finds something, every other view shows the most serious of it under the tabs. It shows how many more there are and the way to Health, as it does when the cluster does not answer.

When the interface cannot read the cluster's health or the fleet again, it keeps the last reading greyed out and marked **Stale**. It does so until a read succeeds.

When it cannot read the clusters at all, as while the management cluster does not answer, the list counts down to its next try. **Try now** tries at once. The list also gives administrators the way to the management cluster's page in Administration. That page works while the management cluster does not answer.

Above the views, the page shows what Kubehenge's controller reports on the Cluster, as kubectl shows it too ([troubleshooting.md](troubleshooting.md#cluster-api-objects)):

- **Controller error**, the error Kubehenge's controller last met there, until a reconcile succeeds (the condition `KubehengeReady`).
  - An example is a Proxmox VE task that failed, shown with the end of its log.
- **Machines stuck for 20 minutes**, when a machine is not ready and none has become ready for 20 minutes.
  - That is the time the machine health check gives a node to join.
  - It shows the worst the cluster's Machines and ProxmoxMachines report, such as `VM template not found` (the condition `MachinesStalled`).
  - Cluster API keeps trying meanwhile. The line goes once a machine is ready.

### Staged changes

A cluster's settings change in two steps. **Save** in a dialog, or a switch under **Options**, stages the change. The API server checks it together with the changes staged before it. A change it refuses shows the reason in the dialog.

Nothing changes in the cluster until the changes are applied. A bar above the views then counts what is staged and says what applying it does to the machines.

**Options** shows each staged value under the one in force. A dialog opened again starts from what is staged. **Review plan** lists each setting as it is and as it would be. It also says what the change does to the machines. It replaces none, adds or removes some, or replaces the control-plane machines, the workers or every machine, one at a time.

**Apply** makes the changes together, as one change of the cluster. **Discard** drops them. Scaling, upgrading, Talos upgrades and every setting under **Options** are staged this way. Deleting the cluster, backups, restores and snapshots run at once.

## Scale

**Scale** sets the control-plane nodes: 1, 3, 5 or 7. The number is odd, so etcd keeps a majority when one fails. For each worker pool it sets either its workers (0 to 100) or, under **Scaling**, **Autoscaled** between its fewest and most workers ([below](#autoscaling)).

## Pools

**Machines** lists the control plane and each worker pool with its nodes, cores, memory and disk. **Add pool** adds a worker pool with its own name, workers and size. A new cluster starts with one, `md-0`.

**Resize** gives the control plane, or one pool, another size, and replaces those nodes one at a time. A resized pool keeps its own size from then on. The other pools keep the cluster's. **Delete** drains and deletes a pool's workers and their disks.

Each is staged with the cluster's other changes. The API server holds it to the class's least size and to the room the add-ons need.

A pool's name takes lowercase letters, digits and dashes and starts with a letter. With the cluster's name it takes at most 44 characters, so that its nodes' host names stay within one DNS label.

## Autoscaling

An autoscaled pool is sized by the [cluster autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler) through Cluster API. It adds a worker while pods wait for room it would give them, up to the pool's most. It removes a worker that has stayed unneeded for 10 minutes, down to its fewest. Kubehenge's controller runs one autoscaler per cluster while any of its pools is autoscaled. It removes the autoscaler with everything it was given once none is.

- **Switching on** in the Scale dialog sets the pool's fewest and most and leaves its number of workers to the autoscaler.
  - The autoscaler starts from the workers the pool has.
- **Switching off** fixes the pool at the workers it runs at that moment, unless you type another number. Nothing is added or removed by switching.
- **From none.** A pool with 0 as its fewest empties while nothing waits.
  - The autoscaler reads what a new worker would hold from the pool's machines, cores and 90% of their memory and disk. The rest is what the node keeps for itself.
  - It also reads the pool's node labels (`node.cluster.x-k8s.io/…`), so a pod's node selector can bring a worker up from none.
  - A pod that asks for more than 90% of a worker's memory is never given one.
- **Control planes** stay kept for the control plane while a pool may grow, even while every pool is at none.
  - A workload never lands on them only to be moved once a worker comes.
  - A cluster whose pools may all go down to none therefore runs no workloads at those times.
- **Add-ons** need a worker that stays.
  - With any add-on that uses memory, at least one autoscaled pool keeps a fewest of 1 or more. The add-ons' memory is shared out over the pools' fewest.
  - The interface says so on the fewest, and Kubehenge refuses the cluster otherwise.
  - Rancher's agent alone does not count. It runs only once there is a Rancher to register with, and its pod then brings a worker up from none.
- **Addresses.** The node addresses must hold every pool at its most, the control planes and one more for a rollout.
  - The Scale dialog shows the room left on the most.
  - Kubehenge refuses a change that leaves fewer addresses than that, unless the cluster was already short by as much.
- **Room on Proxmox VE.** The Scale dialog warns when a pool's remaining workers need more memory than the cluster's allowed Proxmox VE nodes have free.
  - It is advice, since memory frees and fills as other VMs come and go.
  - A worker that finds no room waits. The cluster's events show `AutoscalerNoRoom` with the Proxmox VE provider's reason, and after 30 minutes the autoscaler gives the machine up and tries again.
  - Shared storage suits autoscaled pools best, as it lets a new worker start on whichever allowed node has the room.
- **Expander and timing**, under **Options**, **Edit** beside Autoscaler.
  - They set which pool grows when several could take the waiting pods. The choices are the one left with the least unused room (the default), the one that takes the most pods, or any.
  - They also set how long after adding a worker before any is removed, and how long a worker stays unneeded before it goes. Each is 10 minutes by default.

**Which release.** A cluster runs the cluster autoscaler release of its Kubernetes minor version, since each carries that minor's scheduler. A minor without a release yet runs the newest earlier one. A cluster older than every release Kubehenge carries runs the oldest. They are, by Kubernetes minor:

| Kubernetes | Cluster autoscaler |
| --- | --- |
| 1.33 and older | 1.33.6 |
| 1.34 | 1.34.5 |
| 1.35 | 1.35.2 |
| 1.36 and newer | 1.36.1 |

**Where it runs.** By default each cluster's autoscaler runs in the management cluster, in the `kubehenge-autoscaler` namespace. It reaches its cluster with a token of its own there. The cluster holds no credential of the management cluster. Each asks for 64 MiB of memory, with a limit of 512 MiB.

Setting **Runs** to **Cluster's control planes** (under **Options**, **Edit** beside Autoscaler) moves it into the cluster. The management cluster is spared that memory. The autoscaler then needs the management cluster's API reachable from the cluster's nodes.

This choice also puts a management cluster token inside the cluster. That token reads the MachineDeployments, MachineSets and Machines of every cluster in the namespace, which an admission policy cannot narrow. The policy still lets it scale only this cluster's pools and mark only its machines.

Anyone with the cluster's admin rights can read the token. This choice therefore needs `VM.Audit` on the workload pool, which sees every cluster's VMs anyway. Without the management cluster's address, set by its module, the autoscaler stays out. The cluster then shows an `AutoscalerNotPlaced` event.

## Upgrade

**Upgrade** offers the newer releases of the cluster's minor version and of the next one. Kubernetes upgrades one minor version at a time and never back, and the interface refuses anything else. Cluster API replaces the control-plane nodes one by one first, then the workers. A kubeadm cluster needs the template of the new version first.

The newest version the cluster may move to is chosen already. Under it the dialog checks before you upgrade:

- the API server's dry run of the change and what it replaces (a refusal disables **Upgrade**)
- whether the node addresses leave one spare for the extra node a rollout adds
- how many problems the cluster's Health page lists

Automatic upgrades inside a maintenance window are set under **Options**, **Edit** beside Automatic upgrades. Both are described in [upgrading.md](upgrading.md#workload-clusters).

A cluster with one control plane runs on two etcd members while its control plane is replaced. Its API stops if either does. The plan of such a change warns of it, as kubectl does.

If the new machine is not ready after 20 minutes, the cluster's page says the roll stalled. It says why, and whether the API still answers. It offers **Recover…**, which keeps one control-plane machine and replaces the others ([troubleshooting.md](troubleshooting.md#one-control-plane-its-roll-stalled)).

## Node image refresh

The nodes of kubeadm, RKE2 and k3s clusters are clones of a VM template of their node OS. The management cluster builds the template again every week with the node OS's updates ([templates.md](templates.md#the-weekly-build)).

Once a new build is a day old, each cluster's nodes are replaced with clones of it inside the cluster's maintenance window. They are replaced one at a time as in an upgrade. This happens only while the cluster is healthy and nothing else rolls out. It happens whether automatic Kubernetes upgrades are on or not.

The day gives a broken build time to be noticed before every cluster takes it. Deleting the build stops clusters that have not moved yet. Scaling up or replacing a failed node in between clones the build the other nodes run.

The cluster's **Summary** shows the build the nodes run, when they next move and to which build, and the last move. **Options**, **Edit** beside Automatic upgrades, switches it off for the cluster (**Update node images**). That leaves the nodes on their build until it is switched on again. With it on and Kubernetes upgrades off, the window still applies. Talos nodes take their updates by upgrading Talos instead ([below](#talos-upgrades)).

The administrator switches the weekly build on or off and sets its day and time under **Administration**, **Templates**. It is on by default.

## Talos upgrades

A Talos cluster's nodes are upgraded in place rather than replaced. **Options**, **Upgrade Talos** beside the Talos release, offers the newer releases of the cluster's minor version. It also offers the newest of the next one, which runs the cluster's Kubernetes version. With kubectl, set `talosVersion`. Kubehenge's controller then upgrades one node at a time, control planes first.

Talos installs the release to its other partition. The node is drained and rebooted. It is taken back into service once it is Ready on the new release.

A node whose pods do not all leave within five minutes is rebooted anyway, with a warning event.

Talos upgrades a minor version at a time and never back. The interface and the cluster's checks refuse anything else.

The cluster's **Summary** shows the release, the node being upgraded and the last upgrade. A node that does not come back Ready stops the upgrade there. So does a node that comes back on its old release because Talos rolled back. The Summary says which node and why.

The controller upgrades no other node until **Try again** (which needs `VM.Allocate` on the cluster's pool). The annotation `kubehenge.dev/talos-upgrade-failed` removed with kubectl does the same.

New machines, from scaling up or a repair, clone the template of the release the cluster was created on, or upgraded Kubernetes on last. They are upgraded to the cluster's release once they join, so a Talos upgrade replaces no machine. A Kubernetes upgrade moves new machines to the cluster's release once its template is in the pool.

**Apply Talos patch releases** is on by default (`upgrades.nodeImages`, under Automatic upgrades). With it on, the controller moves the cluster to the newest patch release of its minor version. It does so inside the maintenance window. The cluster must be healthy and nothing else may roll out. A new minor version is always chosen by hand.

## Change settings

**Options** lists every setting that can change once the cluster exists, grouped into Nodes, Network, Storage, Add-ons, Security and Maintenance. Some show only where the cluster or the management cluster offers them.

A setting that is only on or off has a switch in its row. These are Proxmox VE HA, HA group labels, the CSI plugin's newest release, volume snapshots and Istio. The switch stages the change as a dialog's **Save** does. It says what switching it on brings.

One that cannot be switched on says why. The rest have **Edit**.

| Group | Setting | Shown | Changing it |
| --- | --- | --- | --- |
| Nodes | Node OS | kubeadm, RKE2 and k3s | Replaces every node with a clone of the new node OS's template, one at a time as in an upgrade. That template has to be built first ([templates.md](templates.md#node-oses)). A node OS not released yet, or without a template, cannot be chosen. |
| Nodes | Talos release | Talos | **Upgrade** upgrades the nodes in place ([Talos upgrades](#talos-upgrades)). |
| Nodes | Placement | always | Applies to the VMs created after it, by scaling up, an upgrade or a replacement. Those running stay where they are. Nodes the cluster has that you cannot see are listed too, and may be kept ([Machines](#machines)). |
| Nodes | Proxmox VE HA | where the management cluster has Proxmox VE HA and the storage is shared | Takes the control-plane VMs into HA or out of it ([Machines](#machines)). |
| Nodes | HA group labels | unless the management cluster switched them off for every cluster, or while they are on | [Machines](#machines). |
| Nodes | Autoscaler | always | Its expander, its timing and where it runs ([Autoscaling](#autoscaling)). |
| Network | Pod network | always | Fixed for the cluster's life ([Pod network](#pod-network)). |
| Network | Load balancer | always | See the list below. |
| Network | Cluster Mesh | on a cluster that runs Cilium | See the list below. |
| Storage | CSI plugin | where the management cluster lets clusters choose its newest release, or while the cluster runs it | [Persistent volumes](#persistent-volumes). |
| Storage | Volume snapshots | where the management cluster lets clusters take them, or while they are on | [Volume snapshots](#volume-snapshots). |
| Add-ons | Headlamp, Monitoring, Logs, Certificates, Istio, Rancher, GitOps | always | See the list below. |
| Security | CIS hardening | always | Fixed for the cluster's life ([Add-ons](#add-ons)). |
| Security | Pod security, Identity providers | always | See the list below. |
| Maintenance | Backups | where an administrator set a backup target, or while they are on | See the list below. |
| Maintenance | etcd snapshots | where an administrator set a backup target, or while they are on | [etcd snapshots](#etcd-snapshots). |
| Maintenance | Automatic upgrades | always | The Kubernetes upgrades, their window and the move to new node images ([upgrading.md](upgrading.md#automatically-inside-a-window), [Node image refresh](#node-image-refresh)). |

The add-ons and sign-in:

- **Headlamp:** switch it on or off, set or clear its host name, and switch read-only access for everyone on or off. Switching it off uninstalls it.
- **Monitoring:** switch it on or off, and set or clear Grafana's host name.
  - Switching it off uninstalls it with its metrics proxies and certificates, and cert-manager unless Rancher or the certificates add-on runs in the cluster. Prometheus's and Alertmanager's volumes are kept.
  - With logs on, the dialog switches them off too.
- **Logs:** switch them on or off, choose the collector, and change the days kept or grow the volume.
  - Switching the collector replaces one Alloy with the other. Switching logs off uninstalls Loki and Alloy, and Loki's volume is kept.
  - Without monitoring they cannot be switched on.
- **Backups:** switch them on or off, choose the target, and change the schedule, its time zone and how long each backup is kept.
  - You can store, replace or remove the cluster's own access key, which needs `Sys.Modify` on the cluster's pool.
  - A new target or schedule applies from the next backup. Those made already stay where they are and keep the time they were given.
  - Switching backups off uninstalls Velero. The backups stay in the store, and the cluster's keys beside it.
- **Certificates:** switch them on or off, change the issuer, the domain or the DNS provider, and store, replace or remove the DNS provider's credential.
  - With the cluster's CA, **Download CA certificate** in the dialog gives its certificate for clients to trust. So does **CA certificate** on the cluster's page.
  - A new issuer issues every certificate of the old one again. No change of them replaces a node.
  - Switching them off removes the issuer, the default certificate and the credential's copy from the cluster. It removes cert-manager too, unless monitoring or Rancher runs there.
  - The cluster's CA is kept for when they are switched on again.
- **Pod security:** choose the level Pod Security Admission and Kyverno each enforce.
  - On a cluster hardened to the CIS benchmark, Pod Security Admission's stays `restricted`.
  - A new Pod Security Admission level replaces the control-plane nodes one at a time. Switching Kyverno off uninstalls its policies, then Kyverno.
- **Istio:** switch it on or off.
  - Switching it off takes every namespace and pod out of the mesh first, then uninstalls it. Its CRDs and their resources stay.
- **Cluster Mesh**, on a cluster that runs Cilium: join or leave, and choose the peers.
  - Each listed peer shows whether it is linked, and why not. A peer you cannot see shows only that it is not linked, or does not exist.
  - Peers come and go without restarting anything. Joining or leaving upgrades Cilium and restarts its agents.
- **Load balancer:** choose MetalLB, kube-vip or none, and change its addresses.
  - Switching uninstalls the one before. Switched off, the cluster keeps the addresses for when it is switched on again.
- **GitOps:** switch it on or off, and choose the engine, the repository, how it is read and how the web interface signs in.
  - The dialog shows the deploy key, with a button to copy it, and the fingerprints of the server's host keys. It also stores a token or client secret.
  - Switching it off, or to the other engine, removes the engine and leaves what it deployed running. The engine chosen starts syncing once the other has gone.
  - Pointing it at a repository needs `Sys.Modify` on the cluster's pool. Switching it off does not.
- **Rancher:** register with the Rancher Kubehenge installed, or leave it.
  - Leaving removes Rancher's agent from the cluster, except where `default` is held to `restricted` ([troubleshooting.md](troubleshooting.md#rancher)). Whether a cluster runs Rancher itself is chosen at creation.
  - On the cluster that runs it, **Uninstall** beside Rancher under Options uninstalls Rancher after asking for the cluster's name.
  - Every registered cluster is first taken out of Rancher, which cleans up inside each. Rancher's own settings, users and projects are lost.
  - While it waits on that, the cluster shows a `RemovingRancher` event. Everything else about the cluster is still applied.
- **Identity providers:** change them.
  - Saving replaces the control-plane nodes one at a time, as any change of the API server's configuration does.
  - Adding or changing a provider needs `Sys.Modify` on the cluster's pool. It does not when the providers set for every cluster or the cluster's own list already hold it.
  - Without the privilege the dialog shows the providers read-only. Any of them can be removed.

## Download a kubeconfig

**Kubeconfig** downloads `<name>.kubeconfig`, the cluster's admin kubeconfig as Cluster API keeps it, once the cluster is available. It needs `Sys.Modify` on the cluster's pool, and every download is logged with the user who made it.

On a Talos cluster, **Talosconfig** beside it downloads `<name>.talosconfig`. It is the credential for the nodes' Talos API (`talosctl`) as the Talos bootstrap provider keeps it. It needs the same privilege and is logged the same way.

## Open a cluster in Headlamp

The appliance runs one Headlamp for the clusters of the interface's namespaces, on port 8443 of the appliance, behind the interface's sign-in. **Headlamp** in the top bar opens it. **Headlamp** on a cluster's page opens that cluster (`https://<appliance>:8443/kubehenge/open/<namespace>.<name>`, which leads on to `/c/<namespace>.<name>`, such as `/c/clusters.web`). It lists only the clusters you may see, checked against `VM.Audit` on each cluster's pool.

Headlamp itself holds no credentials. Sign in to each cluster with your own token, such as a ServiceAccount token made with `kubectl create token`. Or sign in through the cluster's first OpenID Connect provider. That is the first of its own list or, for a cluster that sets none, the first of the providers every cluster trusts.

For the latter, the provider must accept `https://<appliance>:8443/oidc-callback` as a redirect URL. Headlamp signs in as a public client with PKCE, so no client secret is stored.

Where the administrator switched on the [Headlamp sign-in](install-settings.md#headlamp-sign-in), a cluster opens already signed in as cluster-admin for whoever holds `Sys.Modify` on its pool. That is the privilege the admin kubeconfig takes. The token is made for your session and lasts an hour at most, and never longer than the session. It stays in the appliance, and the browser never holds it. A token of your own, or a sign-in through the identity provider, takes its place while Headlamp keeps it.

In the cluster's audit log, what you do appears as the ServiceAccount `kube-system/kubehenge-headlamp`. The appliance's log names you beside the token's ID, which the audit log records with each request. Losing `Sys.Modify` ends the sign-in within 30 seconds. Switching the setting off ends every token made.

**Log out** in Headlamp signs you out of that cluster. The appliance drops your session's token, and Headlamp asks for a token or a sign-in of your own. That lasts until you open the cluster again with **Headlamp** on its page here, which signs you in again.

Where the administrator switched on the [Headlamp actions](install-settings.md#headlamp-actions), Headlamp also offers the Helm app catalog and the Prometheus charts it ships with. The catalog lists a cluster's Helm releases and installs, upgrades, rolls back and uninstalls them. It acts with your own credentials in the cluster, or the sign-in's. So it does no more than the cluster already lets you.

The catalog installs charts only from the Helm repositories set for the appliance. Only those who may set up the appliance add or remove them.

The Prometheus plugin charts a cluster's metrics from a Prometheus running in it, such as the monitoring add-on's. It reads them through the cluster's API with your credentials. Node drain is offered either way, with your own credentials.

The shared Headlamp does not offer what a shared server must not:

- port forwards (Headlamp offers them in its desktop app alone, where a forward listens on your own machine)
- its external proxy
- plugin changes
- adding clusters or kubeconfigs
- setting a token for others
- its WebSocket multiplexer and its own metrics

## Signing in with OpenID Connect

A cluster's `oidc` setting lists up to eight OpenID Connect providers whose ID tokens its API server accepts. Each has:

| Field | Default | What it is |
| --- | --- | --- |
| Issuer URL | required | The provider's issuer, exactly as its tokens name it, with `https://`. |
| Client ID | required | The client sign-in tools use. A token's audience must name it. Register it with the provider as a public client with PKCE. |
| User name claim | `email` | The claim the user name comes from. |
| User name prefix | `oidc:` | Put before every user name, so none passes for a user of another kind. It may not start with `system:`. |
| Groups claim | `groups` | The claim the groups come from. |
| Groups prefix | `oidc:` | Put before every group. It may not start with `system:`. |
| Scopes | `email`, `profile` | Asked for by sign-in tools besides `openid`. |
| CA certificate | none | PEM, when a public CA does not sign the provider's certificate. |

A token gives no rights by itself. Bind the prefixed names with RBAC in the cluster, for example:

```sh
kubectl create clusterrolebinding platform-admins --clusterrole=cluster-admin --group=oidc:platform-admins
```

Headlamp signs in with the first provider. kubectl does so through an OpenID Connect credential plugin, such as kubelogin, set to the provider's issuer and client ID.

**Providers for every cluster** are set on the Administration page (**Edit settings** in the Management cluster section, Identity providers), or as `cluster_oidc` with Terraform/OpenTofu. Every new cluster trusts them unless it sets its own list, and a change applies to clusters created afterwards. Whoever runs a provider can issue a token for any name. So adding any other provider needs `Sys.Modify` on the pool, as the admin kubeconfig does. A cluster may keep or drop these with no further privilege.

## Routes through the Gateway API

Every cluster has the Gateway API's standard channel: GatewayClass, Gateway, HTTPRoute, GRPCRoute, TLSRoute, TCPRoute, UDPRoute, ReferenceGrant, BackendTLSPolicy and ListenerSet. Traefik serves the GatewayClass `traefik`. Its Gateway `traefik-gateway` in `kube-system` has one listener, `web`, for HTTP on port 80 of every node and of the API address. It reports that as its address. Any namespace may attach routes to it, as any namespace may claim a host name with an Ingress:

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

With [certificates](#certificates) from DNS-01 or the cluster's CA, the Gateway also has a listener `websecure`, for HTTPS on port 443. It serves the default certificate and takes routes from every namespace. `sectionName: websecure` puts a route there.

A Gateway of your own with the class `traefik` works too. Its listeners use Traefik's ports, 8000 for HTTP and 8443 for HTTPS. The nodes' ports 80 and 443 lead to them. Istio's waypoints, its layer 7 proxies, are Gateways of the class `istio-waypoint`. Istiod serves them once the Istio add-on is on.

The Gateway API's admission policy (`safe-upgrades.gateway.networking.k8s.io`) refuses CRDs of the group from the experimental channel over standard ones, and from releases before v1.5.0. So a chart that brings old CRDs fails rather than downgrades the cluster's.

Kubehenge applies its release of the CRDs where they are missing, or older and no longer shipped by the distribution. It never puts an older release over a newer one. Where the distribution's release has its own Gateway API chart (RKE2's `rke2-gateway-api-crd`, k3s's `gateway-api-crd`), the Gateway API is that chart's alone. It is at the chart's release ([architecture.md](architecture.md#networking)).

The annotation `kubehenge.dev/gateway-api` on the Cluster names the oldest release among the CRDs once every one is in place.

## Certificates

The certificates add-on (`certificates: {enabled: true, …}`) installs cert-manager and a ClusterIssuer `kubehenge-ingress`, which signs or orders every certificate the cluster asks for. There are three issuers:

- **`acme-dns01`** (the default): an ACME server proves the names through a TXT record that cert-manager sets with the DNS provider's API.
  - Works for clusters no one reaches from the internet, and for wildcards. Needs a `domain` and a DNS provider.
- **`acme-http01`**: the ACME server fetches a token over HTTP on port 80 of the name, which cert-manager serves through Traefik's Gateway.
  - Needs every name to resolve publicly to the cluster and port 80 open. It can prove no wildcard, so it gives no default certificate.
- **`ca`**: a CA of the cluster's own, made by Kubehenge's controller, valid for ten years, and constrained to `domain`.
  - It signs names in the domain and nothing else, no other name and no address. So trusting it trusts nothing beyond the cluster.
  - Clients trust it once: **Download CA certificate** under the cluster's **Options**, or the `<cluster>-ingress-ca` Secret beside the Cluster (`tls.crt`).
  - A new domain makes a new CA. Add-on host names have to lie in the domain, or name a `tlsSecret`.

With `acme-dns01` and `ca`, the certificate `kubehenge-ingress` in `kube-system` covers `domain` and `*.domain`. Traefik serves it for every Ingress and route that names no certificate of its own, and on the Gateway's `websecure` listener. A name further down, or any name with `acme-http01`, gets its own certificate. The add-ons' Ingresses (Headlamp's, Grafana's, Argo CD's and Flux's web interface) ask for one themselves. An Ingress of yours asks with either annotation:

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

The ACME server is Let's Encrypt's. With `acme.staging` it is its staging directory (certificates no browser trusts, for trying things out). With `acme.server` it is any other. `acme.caBundle` is the CA of a server whose own certificate the usual CAs do not sign. `acme.email` receives the server's notices.

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

1. **`dns01.existingSecret`**: a Secret you create in the workload cluster's `cert-manager` namespace, with the credential under the key `token` (Akamai's under `clientToken`, `clientSecret` and `accessToken`).
   - Kubehenge never sees it.
2. **`dns01.sharedCredential: true`**: the DNS-01 credential an administrator stored for every cluster under **Administration**, **Certificates**.
   - Taking it needs `Sys.Modify` ([Who may do what](#who-may-do-what)), since it reaches every zone it was made for.
3. **The cluster's own**, typed in when creating the cluster or under **Options**.
   - The interface keeps it in the Secret `<cluster>.dns01` beside the Cluster and never shows it again.

Kubehenge's controller copies the stored credential into the `cert-manager` namespace of the workload cluster (`kubehenge-dns01`). It replaces the copy, or removes it, as soon as the credential is stored again or removed. A cluster without a credential gets a `NoDNS01Credential` event, and its certificates wait.

Where the domain's DNS answers differently inside the network than outside (split-horizon DNS), `dns01.nameservers` names the servers (host:port). They are the ones cert-manager checks the challenge's record with, such as the domain's authoritative servers.

## Persistent volumes

The default StorageClass `proxmox` puts volumes on the cluster's storage. They belong to a VM named `<name>-<hash>-volumes`, tagged `kubehenge` and `volumes`, in the cluster's pool. The VM is never started and exists so that only this cluster's token may attach them. A released volume is deleted through that VM. On storage local to each node, a volume lives on one node.

By default a cluster runs the last release of the Proxmox VE CSI plugin that needs no more than `Datastore.AllocateSpace` on the cluster's storage. Its newer releases read the storage's configuration and need `Datastore.Allocate` there. That lets the cluster's token delete any volume on that storage and change the storage's definition.

Where the administrator lets clusters choose it ([install-settings.md](install-settings.md#csi-plugin)), a cluster can run the newest release instead. Choose it with **Newest CSI plugin release**, or the `csi` variable's `datastoreAllocate`. Its token then gets `Datastore.Allocate` on the cluster's storage, and loses it again when switched off.

The newest release needs a storage of the cluster's own. Kubehenge refuses it on:

- a storage the management cluster uses (its disks, its Talos image and the cloud-init images, and those the template builds download and upload to)
- a storage that VM templates have disks on
- a storage another cluster uses

It also refuses another cluster on a storage a cluster holds it on. Switching it on takes your own `Datastore.Allocate` on the storage.

A refusal can come up after the cluster switched it on, such as a template built on its storage later. The cluster then goes back to the earlier release. `kubectl describe cluster` shows a `DatastoreAllocateRefused` event saying why.

## Back up and restore

A cluster with backups on runs Velero, which backs up on the cluster's schedule to its target, under `<prefix>/<namespace>.<name>/velero` in the target's bucket. Each backup holds the cluster's resources and the files of every volume its pods mount, emptyDir volumes aside. It covers every namespace but `kube-system`, `kube-public`, `kube-node-lease`, Velero's and the add-ons'. Kubehenge installs those again itself. `kube-system` holds the cluster's Proxmox VE token, which no backup carries.

What lands in the store is encrypted with keys of the cluster's own. They are made when backups are first switched on. They are kept beside the Cluster in the management cluster, in the Secret `<name>-backup-keys`. The store encrypts the resources with the cluster's SSE-C key. Kopia, which Velero reads the volumes with, encrypts their data with the cluster's repository password.

Neither key is in the store. So whoever can read the bucket sees the names of the backups and namespaces and nothing of what they hold. That holds even with the access key every cluster shares among them.

The Secret outlives the cluster. So a cluster created again under the same name, backing up to the same target, can restore what the one before backed up. Deleting the Secret leaves those backups unreadable for good.

The **Backups** view says whether Velero reaches the target and when it last backed up. It lists every backup with its state, who started it or whether the schedule did, and when it started and runs out. It also shows how many items each holds and what went wrong. The restores below show what each brings back and how far it has come. Seeing it needs `VM.Audit` on the cluster's pool.

- **Back up now** makes a backup at once, as the schedule would. It needs `VM.Allocate` on the cluster's pool.
- **Restore…**, beside a finished backup, brings back the whole backup or part of it.
  - Choose the namespaces named, or every one but those left out.
  - Under **Kinds of resources**, choose only some kinds or all but some, by their plural names as kubectl lists them (`deployments`, `certificates.cert-manager.io`).
  - Choose the volumes' data, or not.
  - What the cluster still has is left as it is. So a deleted namespace comes back whole, and an object that still exists keeps its current state.
  - Without the volumes' data the restore leaves the pods out, as Velero would otherwise hold each one until its volumes' data came back. Their Deployments and StatefulSets start them again on new, empty volumes.
  - It needs `Sys.Modify` on the cluster's pool, as the admin kubeconfig does. A restore writes whatever the backup holds, Secrets and roles among it.
- **Results**, beside a finished restore, lists Velero's errors and warnings for it, up to 500 of each.
  - They are Velero's own, the cluster-wide ones and those of each namespace.
  - Kubehenge reads them from the target with the cluster's SSE-C key. It needs `VM.Audit` on the cluster's pool.
- **Delete**, beside a finished backup, has Velero delete it before it runs out, from the target too, with the volume data no other backup shares.
  - The backup's name has to be typed to confirm it, which the server checks too.
  - The backup shows as deleting until Velero is done, and why Velero did not delete it, if it did not.
  - It needs `VM.Allocate` on the cluster's pool.

The view reaches Velero through two accounts of Kubehenge's in the cluster's `velero` namespace. One reads the backups and starts one, a deletion or a download of a restore's results. The other starts a restore. Neither reads a Secret or anything outside Velero's own resources.

The interface creates Velero's own objects: a `Backup`, a `Restore` or a `DeleteBackupRequest`. So these work as anywhere else with kubectl:

- `velero backup create`
- `velero restore create --from-backup <backup>`, with `--include-namespaces`, `--exclude-resources` and `--restore-volumes=false`
- `velero restore describe --details`
- `velero backup delete`

Velero runs as `cluster-admin`, which backing up and restoring every resource needs. Its privileged backup pods read every pod's volumes as root from the host, so the `velero` namespace is exempt from pod security.

When backups are on, the cluster's health page and the fleet's **Needs you** list report a failed or partly failed last backup. They also say so if no backup has completed while the schedule came round twice. That counts from the last backup that did complete, or from when backups were switched on.

Each links to the Backups view. Kubehenge reads this from Velero's own schedule and backups, so it holds for a schedule changed with kubectl too.

### Restore into another cluster

A cluster reads the backups of others of its namespace that back up to the same target. It does so when the `backups` variable's `readFrom` names them, or **Read the backups of** in the Backups dialog ticks them. Its Velero then lists their backups beside its own, in a read-only location named `cluster-<name>` for each.

The Backups view shows them in a table of their own, to restore into the cluster. The source cluster's Backups view offers its own backups to restore into each cluster that reads them. Deleting a backup stays with the cluster that made it.

Kubehenge gives the reading cluster's Velero the source's SSE-C key. Whoever runs the reading cluster can therefore read everything the source's backups hold. Naming a cluster needs `Sys.Modify` on the source's pool, as downloading its admin kubeconfig does. Kubehenge refuses a source that is not backed up to the same target, or whose keys it does not have. Restoring one of its backups needs `Sys.Modify` on the reading cluster's pool and `VM.Audit` on the source's.

Kopia's repository password is one for each cluster's Velero. So a cluster restores the volumes' data of another's backups only when the two share it.

A cluster whose backups are first switched on while it already reads another's takes the password of the first cluster named. So a cluster created to take over from another can restore it whole. For any other source, the view says the volumes' data cannot be read. Only the resources are restored, without the pods.

A cluster that has been deleted keeps its keys, so a cluster can still read its backups. Naming it with kubectl in `readFrom` works as long as the Secret `<name>-backup-keys` is there.

### Volume snapshots

With **Take volume snapshots** (the `csi` variable's `volumeSnapshots`), the cluster's CSI plugin takes VolumeSnapshots of its volumes. It restores them into new volumes and clones volumes (a PersistentVolumeClaim whose `dataSource` is another claim).

Proxmox VE's own volume copy is for `root@pam` alone. So a small proxy beside the plugin, `kubehenge-csi-proxy`, makes each copy with the cluster's own token instead. It imports the volume into a new disk of one of the cluster's holder VMs and keeps it there as an unused disk. Proxmox VE names the copy after the holder. So the proxy records, in the cluster, the name the plugin knows it by, and translates between the two.

- **What it grants:** copies of the cluster's own volumes, onto the cluster's own storage, and nothing else.
  - The proxy copies only volumes of the cluster's volume owner VM and holder VMs, as the plugin names them.
  - It copies them into names the plugin gives the cluster's volumes, and only when no volume of that name is there. It makes one copy at a time, each logged.
  - The cluster's token holds `VM.Clone` on its own pool for it. That lets it copy disks of the cluster's own VMs and clone no VM.
  - The proxy's records are ConfigMaps in the namespace `kubehenge-volume-copies`, which only the plugin's controller may change.
- **Where it is offered:** on storage local to each node of a type the plugin copies on: ZFS, LVM, LVM-thin, directory or btrfs.
  - Kubehenge refuses it on shared storage and on any other type, and refuses another storage for a cluster that has it on.
  - A copy stays on its volume's node.
    - A snapshot or clone of a volume is made on the node the volume is on.
    - A pod using a restored or cloned volume runs where its node VM can attach it.
  - Switching it on takes no privilege of your own beyond changing the cluster.
- **What the cluster gets:**
  - the plugin's newest release with its `csi-snapshotter` sidecar and the proxy beside its controller
  - the VolumeSnapshotClass `proxmox` (the default, which deletes a snapshot's volume with it)
  - holder VMs in its pool (`<cluster>-<digest>-copies`, stopped, one or more on each node)
  - on kubeadm and k3s, the external snapshotter's CustomResourceDefinitions and snapshot controller
  - RKE2 runs a snapshot controller of its own, which the cluster uses
  - Deleting a snapshot, or a restored or cloned claim, deletes its copy from its holder VM.
- **How long a copy may take:** as long as Proxmox VE needs, up to a day.
  - The sidecars give up on a call after 5 minutes and ask again. The next call waits on the same copy instead of starting another.
  - The copy is not listed under its name until it is finished. A large volume's snapshot therefore turns ready only once its copy is whole.
- **Switching it off** removes the sidecar, the VolumeSnapshotClass and the snapshot controller.
  - The proxy stays while any copy is left, since only it knows their names. It makes no copies.
  - Once the last restored or cloned claim and snapshot is deleted, the proxy goes with its namespace and the empty holder VMs. The plugin reaches Proxmox VE directly with the cluster's token again.
  - The CustomResourceDefinitions stay, so VolumeSnapshots still in the cluster keep their copies. Delete them while the option is on, or they go when the cluster is deleted.

Kubehenge may be unable to take volume snapshots for the cluster, such as on a storage that is not local to each node. The plugin then goes without them. `kubectl describe cluster` shows a `VolumeSnapshotsRefused` event saying why. A copy the proxy refuses or that fails shows as a `VolumeCopyFailed` event on the CSI plugin's controller pod in `kube-system`. It also shows as the CSI plugin's error on the VolumeSnapshotContent or PersistentVolumeClaim.

## etcd snapshots

A cluster with etcd snapshots on has Kubehenge's controller copy its etcd database to one of the backup targets an administrator set. The database holds every object the cluster's API holds. The copy happens on the cluster's schedule and whenever asked.

The schedule is by default every 12 hours from 00:00 UTC, with each cluster a few minutes after the hour of its own. The controller keeps the newest 14 by default and deletes older ones.

Snapshots go under `<prefix>/<namespace>.<name>/etcd` in the target's bucket, beside the cluster's Velero backups. They are named by when they were taken in UTC and the Kubernetes version, as `20261021T120000Z-v1.36.5-rke2r1.db.age`. Each object carries the Kubernetes version, the distribution, etcd's revision and the SHA-256 and size of the database in its metadata. Snapshots work on their own, with or without Velero backups.

Nothing runs in the cluster for them. No etcd key or S3 access key enters it, unless the distribution's own upload is on ([below](#the-distributions-own-upload)). For each snapshot the controller signs itself an etcd client certificate valid for an hour, in memory, from the cluster's etcd CA. The control-plane provider keeps that CA in the management cluster.

The controller reaches one etcd member through the cluster's API server, or a control plane's Talos API on Talos. It keeps a snapshot only once etcd's own checksum matches.

Each snapshot is encrypted with [age](https://age-encryption.org) to a key of the cluster's own, `etcd-snapshot-identity` in the Secret `<name>-backup-keys` beside the Cluster. The key is made with the cluster's other backup keys and outlives the cluster as they do. The snapshot is written with the same access key as Velero's backups. RKE2 and k3s also keep the snapshots they take themselves on their nodes, as they always do.

The **Backups** view's etcd snapshots section says when the last snapshot was taken and what went wrong with the last try. It lists the snapshots in the target. Snapshots are switched on under Options or from **Set up snapshots…**. The target, schedule, time zone and number kept are chosen there too.

- **Take snapshot** has the controller take one at once. It needs `VM.Allocate` on the cluster's pool.
- **Download** hands the snapshot out decrypted, as etcd wrote it. It holds every Secret of the cluster, so it needs `Sys.Modify` on the cluster's pool, as the admin kubeconfig does.
- **Restore…** restores the cluster's etcd from it ([below](#restore-etcd)). It needs `Sys.Modify` on the cluster's pool.

With kubectl, the annotation `kubehenge.dev/etcd-snapshot-requested` with `{"at":"<time>"}` asks for a snapshot. The annotation `kubehenge.dev/etcd-snapshot` records the last one. A snapshot copied from the target decrypts with `kubehenge etcd-snapshot decrypt --identity <key file> <snapshot.db.age> <snapshot.db>`, which checks etcd's checksum too. Any age client works as well, the key file holding the key from `<name>-backup-keys`:

```sh
kubectl --namespace clusters get secret web-backup-keys -o jsonpath='{.data.etcd-snapshot-identity}' | base64 -d > web.key
age --decrypt --identity web.key --output snapshot.db 20261021T120000Z-v1.36.5-rke2r1.db.age
```

### The distribution's own upload

On RKE2 and k3s, **Also take RKE2 snapshots** (`distributionUpload: true`, off by default) has the cluster's servers upload their own snapshots to the same target too.

They go under `<prefix>/<namespace>.<name>/rke2-etcd` or `k3s-etcd`. They run on the cluster's schedule and keep as many. They go on while the management cluster is down. Kubehenge's own snapshots do not.

kubeadm has no upload of its own. RKE2 and k3s both get a configuration drop-in. It points the servers at a Secret Kubehenge writes into the cluster, with the target's certificate verified.

What it grants: the cluster's own access key to the target goes into the Secret `kubehenge-etcd-s3` in the cluster's `kube-system`. Whoever may read Secrets there, every cluster admin among them, reads it. With it they read everything the key reaches in the target. Limit it to the cluster's bucket or folder.

The credential for every cluster never goes into a cluster. Without one of the cluster's own, the upload waits with an `EtcdUploadHeld` event.

What it risks and costs:

- The snapshots are stored as etcd wrote them, not encrypted by Kubehenge, and hold every Secret of the cluster.
- Kubehenge neither lists nor restores them. They are restored by hand with the distribution's own steps.
  - On the first server, with every server stopped: `rke2 server --cluster-reset --etcd-s3 --etcd-s3-bucket=<bucket> --etcd-s3-folder=<folder> --etcd-s3-endpoint=<host> --etcd-s3-access-key=… --etcd-s3-secret-key=… --cluster-reset-restore-path=<snapshot>` (or `k3s server` the same).
  - Then the other servers rejoin as in step 6 [below](#restore-etcd).
- The distribution reads the schedule in UTC whatever the time zone chosen, and counts the number kept across all servers together.
- The drop-in is part of the control plane's bootstrap. Switching the upload on or off therefore replaces the control-plane machines one at a time.
  - So does changing the schedule or the number kept while it is on.

The upload needs etcd snapshots on, and is refused on kubeadm. Switching snapshots off switches it off too.

The controller takes the Secret out of the cluster again once every server that still has the drop-in has been replaced. The servers not yet replaced keep uploading on schedule.

### Restore etcd

Restoring etcd takes every object of the cluster back to the snapshot. Deployments, Secrets, roles, custom resources and the rest of what its API holds return to how they were. Whatever was created since is gone. The data in volumes is not restored and stays as it is now.

It is for a cluster that lost its etcd quorum, or whose state is corrupted or was deleted in bulk. To bring back applications with their data, restore a [Velero backup](#back-up-and-restore).

A cluster is restored only from a snapshot of its own distribution and Kubernetes minor version, whose objects its API server reads. A Talos cluster is restored through its nodes' Talos API, whether or not the management cluster restores through the guest agent.

Kubehenge wipes etcd on every control-plane node, which reboots each. It restores etcd from the snapshot on the first. As on kubeadm, the revision moves on and the revisions before are marked compacted. The others join it again, so no machine is replaced.

A restore:

1. pauses the cluster in Cluster API, so that it replaces no machine meanwhile.
2. copies the snapshot to the first control-plane node and checks its SHA-256, which must be 64 lower-case hexadecimal digits.
3. stops etcd and the API server on every control-plane node, on kubeadm with the controller manager and the scheduler.
4. restores etcd on the first node as a cluster of one member.
   - On kubeadm, with `etcdutl snapshot restore --bump-revision 1000000000 --mark-compacted`, run from the etcd image the node already runs.
     - The revision bump ends every client's watch, and each reads the restored objects afresh.
     - The database from before is kept beside it.
   - On RKE2, with `rke2 server --cluster-reset --cluster-reset-restore-path=<file> --etcd-s3=false`.
   - On k3s, with `k3s server` the same.
5. starts its control plane and waits for its API server.
6. deletes the other control-plane machines, whose etcd still holds the cluster from before, and gives the cluster back to Cluster API.
   - Cluster API makes them again to join the restored member.

Where an administrator has switched on [etcd restores](install-settings.md#etcd-restores), **Restore…** asks for the cluster's name. Kubehenge then runs these steps itself through the control-plane VMs' guest agents. The section shows which step it is at.

Before Kubehenge stops anything, it takes a snapshot of etcd as it is into the same target. It names that snapshot in the restore's status, so that a restore from the wrong snapshot can be undone with another. A cluster whose etcd does not answer is restored without one. The status says why.

The controller decrypts the snapshot and encrypts it again for the copy, with a password of its own for that restore. The snapshot reaches the node from a link into the target that expires within the hour. A control-plane VM that is off is replaced with the others. A restore that fails before etcd was restored starts every control plane again as it was. One that fails after finishes the restore.

Otherwise **Restore…** lists the steps for the chosen snapshot, with the commands for the cluster's distribution. Those for the nodes run as root over SSH or the VM's console, after downloading the snapshot to the first node as `/var/lib/kubehenge-restore/snapshot.db`. The `kubectl` ones run with the management cluster's kubeconfig.

## Delete

**Delete** asks for the cluster's name. It deletes the cluster's VMs and its persistent volumes, and removes it from Rancher first. It cannot be undone. Its backups and etcd snapshots stay in their target, and its backup keys beside it ([Back up and restore](#back-up-and-restore)).

Once the VMs are gone, Kubehenge's controller removes the cluster's pool, user, token and volume owner VM. If a node that may hold some of the cluster's volumes on its local storage is offline, the controller waits for it.

Cluster API removes each VM's cloud-init ISO (`user-data-<VMID>.iso` on the cloud-init storage) with the VM. Only a VM deleted by hand, outside Cluster API, leaves its ISO behind. It can then be removed under the storage's ISO Images in Proxmox VE.

Deleting the cluster that runs Rancher first removes every registered cluster from Rancher.

While the cluster is being deleted, its page lists what the deletion waits on. That is each object whose finalizer holds it, with what that object reports. Examples are a node drain a disruption budget refuses, or a VM on a node that is offline. A deletion still waiting after 20 minutes is reported stalled. One that can never finish can be abandoned ([troubleshooting.md](troubleshooting.md#a-cluster-does-not-finish-deleting)).

## Who may do what

The interface checks the signed-in user's Proxmox VE privileges on each cluster's pool. It then acts with its own account in the management cluster. Clusters are shared with the same ACLs as VMs.

| To | Privilege | On |
| --- | --- | --- |
| See a cluster and its health page, open it in the shared Headlamp, and download its CA | `VM.Audit` | the cluster's pool |
| Create a cluster | `VM.Allocate` | the workload pool |
| | `Datastore.AllocateSpace` | `/storage/<storage>` |
| | `SDN.Use` | `/sdn/zones/<zone>/<bridge>`, or `…/<bridge>/<vlan>` with a VLAN tag |
| Place a cluster by an HA rule's nodes | `Sys.Audit`, or `VM.Allocate` where the rules are offered to cluster creators | `/` for `Sys.Audit`, the workload pool or the cluster's pool for `VM.Allocate` |
| Install Rancher | `Sys.Modify` | the workload pool |
| Run the CSI plugin's newest release | `Datastore.Allocate` | `/storage/<storage>` |
| Have a missing template built, where the administrator allows it | `VM.Allocate` | the workload pool |
| Use the DNS-01 credential stored for every cluster | `Sys.Modify` | the workload pool when creating, the cluster's pool when changing |
| Scale, upgrade, change add-ons, automatic upgrades and sign-in, delete | `VM.Allocate` | the cluster's pool |
| Replace or delete a machine | `VM.Allocate` | the cluster's pool |
| Cordon or uncordon a machine's node | `VM.Allocate` | the cluster's pool |
| Reboot a machine | `VM.PowerMgmt` | the cluster's pool |
| Open a machine's console | `VM.Console` | the cluster's pool |
| See its backups and restores, and a restore's results | `VM.Audit` | the cluster's pool |
| Back up now, delete a backup | `VM.Allocate` | the cluster's pool |
| Restore a backup | `Sys.Modify` | the cluster's pool |
| Restore another cluster's backup into a cluster that reads them | `Sys.Modify`, `VM.Audit` | the reading cluster's pool, the source's pool |
| Have a cluster read another's backups | `Sys.Modify` | the source's pool |
| See its etcd snapshots | `VM.Audit` | the cluster's pool |
| Take an etcd snapshot now | `VM.Allocate` | the cluster's pool |
| Download an etcd snapshot, restore etcd | `Sys.Modify` | the cluster's pool |
| Store a cluster's own backup access key | `Sys.Modify` | the workload pool when creating, the cluster's pool when changing |
| Download the admin kubeconfig | `Sys.Modify` | the cluster's pool |
| Open the cluster in the shared Headlamp signed in as cluster-admin, where the [Headlamp sign-in](install-settings.md#headlamp-sign-in) is on | `Sys.Modify` | the cluster's pool |
| Use the Helm app catalog and the Prometheus charts in the shared Headlamp, where the [Headlamp actions](install-settings.md#headlamp-actions) are on. In the cluster they act with the cluster's own rights | `VM.Audit` | the cluster's pool |
| Add, update or remove a Helm repository in the shared Headlamp | the [setup privileges](install.md#who-may-set-it-up) | `/` |
| Read an unwell pod's logs on the health page | `Sys.Modify` | the cluster's pool |
| Trust identity providers beyond those set for every cluster | `Sys.Modify` | the workload pool when creating, the cluster's pool when changing |
| Choose the repository GitOps syncs from | `Sys.Modify` | the workload pool when creating, the cluster's pool when changing |
| Run a cluster's autoscaler on its own control planes | `VM.Audit` | the workload pool |
| Use the Administration page | the [setup privileges](install.md#who-may-set-it-up) | `/` |

- A node's own bridges are in the zone `localnetwork`, as in `/sdn/zones/localnetwork/vmbr0`.
- A cluster you may not see is answered as not found.
- As everywhere in Proxmox VE, a role given on a cluster's own pool replaces, for that pool, what is inherited from the workload pool. It has to hold `VM.Audit` too.
- Proxmox VE's `PVEAuditor` role holds `VM.Audit`, and `PVEVMAdmin` holds `VM.Audit` and `VM.Allocate`. `Sys.Modify` needs a role of your own.

For example, to let a group create and look after clusters, and one user download the kubeconfig of one cluster:

```sh
pveum acl modify /pool/kubehenge --groups developers --roles PVEVMAdmin
pveum acl modify /storage/shared-storage --groups developers --roles PVEDatastoreUser
pveum acl modify /sdn/zones/localnetwork/vmbr0 --groups developers --roles PVESDNUser
pveum role add KubehengeClusterAdmin --privs "VM.Audit VM.Allocate Sys.Modify"
pveum acl modify /pool/kubehenge/clusters.prod --users platform@pve --roles KubehengeClusterAdmin
```

The interface changes only clusters in its own namespaces. Those are `clusters` and the ones an administrator adds below. Clusters created with kubectl elsewhere are shown to those who may see them. They are changed with kubectl.

### More namespaces for clusters

An administrator can let cluster creators choose among more namespaces than `clusters`, such as one per team. The setting is **Cluster namespaces** in the setup and on the Administration page, or the module's `extra_cluster_namespaces` ([install.md](install.md)). The wizard then asks for the namespace. The cluster list shows each cluster's namespace beside its name. The interface looks after the clusters of every one of them as it does those of `clusters`.

- A cluster's pool still carries its namespace, `<workload pool>/<namespace>.<name>`.
  - The privileges above therefore work the same in every namespace.
  - Creating a cluster takes `VM.Allocate` on the workload pool, whichever namespace it goes to.
- A namespace that does not exist yet is created.
  - One taken off the list keeps its clusters.
  - The interface then shows them read-only, and they are changed with kubectl.
- Cluster Mesh links clusters of one namespace only.
- Names Kubernetes, the providers and Kubehenge use themselves are refused. They are `default`, `cert-manager`, `kube-*` and `*-system`.

## From the command line

`kubehenge cluster` lists, creates, scales and deletes clusters through the interface, signed in with a Proxmox VE API token instead of a password. The token is held to the privileges above as a user is. One with privilege separation holds only what both it and its user are given. One without holds its user's. Proxmox VE is asked about a token at most once a minute, so one revoked there stops working within that minute.

```sh
export KUBEHENGE_URL=https://kubehenge.example.org
export KUBEHENGE_PROXMOX_TOKEN='ci@pve!kubehenge=<secret>'
kubehenge cluster list
kubehenge cluster create -f web.yaml
kubehenge cluster scale web --control-planes 3 --workers md-0=5
kubehenge cluster kubeconfig web > web.kubeconfig
kubehenge cluster get web
kubehenge cluster machine cordon web web-md-0-x7k2p
kubehenge cluster delete web --yes
```

- The token is read only from `KUBEHENGE_PROXMOX_TOKEN`, so it never shows in a process list. `--ca` (or `KUBEHENGE_CA`) names the CA of the interface's certificate where the system does not trust it.
- A cluster is found by its name. `-n` names its namespace where more than one has a cluster of that name.
- `create` takes what the wizard sends, in YAML or JSON. That is `name`, `class`, `version`, `controlPlaneReplicas`, `workerReplicas` and `variables`, and `namespace` where there are [more namespaces](#more-namespaces-for-clusters).
  - The interface checks it as it checks the wizard's.
- `machine` takes a machine's name, as `get` lists it. `replace`, `keep`, `delete`, `cordon`, `uncordon` and `reboot` do what a machine's actions in the interface do.
- `get` and `list -o json` print the interface's own description of a cluster for scripts to read.
- Any HTTP client can do the same: send `Authorization: PVEAPIToken=<user>@<realm>!<name>=<secret>` to the interface's `/api/clusters`.

## With kubectl

Clusters are Kubernetes objects in the management cluster, so they can be created and changed with kubectl, or kept in Git with any GitOps tool.

Get the management cluster's admin kubeconfig under **Administration**, **Appliance**. Use **Repair access**, **Download admin kubeconfig**, with the state passphrase. Or get it with `tofu output -raw kubeconfig` if you applied it with Terraform/OpenTofu. Whoever holds it controls every cluster. Proxmox VE privileges do not apply to kubectl.

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

- **The ClusterClasses** are `kubehenge-kubeadm`, `kubehenge-rke2`, `kubehenge-k3s` and `kubehenge-talos`, in `kubehenge-system`.
- **The interface's rules hold here too.** The API server checks them as the Cluster is applied.
  - A name of at most 40 characters, and a version in the distribution's form.
  - An upgrade of one minor version at most, and never back.
  - 1, 3, 5 or 7 control-plane nodes, and 0 to 100 workers in each pool.
  - Enough node addresses for every machine at once plus one for a rollout.
  - Node addresses and a control-plane endpoint on the gateway's network, with the endpoint outside the node addresses.
  - None of another cluster's addresses on the same bridge and VLAN.
- **The namespace** decides the pool: `<workload pool>/<namespace>.<name>`.
  - Clusters in the interface's namespaces (`clusters`, and any [added for clusters](#more-namespaces-for-clusters)) are also looked after from the interface. Any other namespace works, and the interface shows those clusters read-only.
  - The ClusterClasses stay in `kubehenge-system`, which `topology.classRef.namespace` names, so every namespace uses the same ones.
  - Names are held to 40 characters so that the nodes' host names stay within one DNS label.
- **Worker pools** are machine deployments of the class `worker`.
  - A Cluster may have several, each named once. A pool added later is held to the [pool name rule](#pools).
  - A pool's own size is an override of `workerMachine` under `variables.overrides`.
  - An autoscaled pool has no `replicas`. It carries Cluster API's annotations `cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size` and `-max-size` under `metadata.annotations` (at most 100), with the pool's node labels under `metadata.labels`.
- **Ranges** come from `spec.clusterNetwork`.
  - Without it, Kubehenge picks them as the interface does ([Network](#network)). It takes the distribution's own (`10.244.0.0/16` and `10.96.0.0/12` for kubeadm and Talos, `10.42.0.0/16` and `10.43.0.0/16` for RKE2 and k3s) when they are clear of the nodes' network.
    - Otherwise it takes the first clear private range.
  - Kubehenge refuses to create a cluster whose ranges overlap its nodes' network.

### Variables

| Variable | Required | What it is |
| --- | --- | --- |
| `controlPlaneEndpoint` | yes | The control plane's virtual IP: an IPv4 address, unused, outside `network.addresses`. |
| `network` | yes | `bridge`, one of the bridges clusters may use. `vlan` (optional, 1 to 4094). `addresses`, a list of ranges, networks in CIDR notation or single addresses. `prefix` (1 to 32). `gateway`. `dnsServers`. |
| `storage` | yes | The storage for the nodes' disks and the persistent volumes, one of those clusters may use. |
| `allowedNodes` | no | Proxmox VE nodes the cluster's VMs may run on, at least one. Every node when unset. New VMs go to those of them that are online ([above](#machines)). The interface takes only nodes the user can see, or those the cluster already has. A change applies to VMs created after it. |
| `proxmoxHA` | no | Whether Proxmox VE HA keeps the control-plane VMs on separate nodes ([above](#machines)). Default true, where the management cluster has Proxmox VE HA and the storage is shared. |
| `csi` | no | `datastoreAllocate` (default false) runs the CSI plugin's newest release, with `Datastore.Allocate` for the cluster's token on its storage ([above](#persistent-volumes)). It needs a management cluster that lets clusters choose it and a storage of the cluster's own. `volumeSnapshots` (default false) takes volume snapshots, with the cluster's own volumes copied with its own token ([above](#volume-snapshots)), on storage local to each node. |
| `ccm` | no | `haGroups` has the cloud controller manager label the nodes with their Proxmox VE HA groups, with `Sys.Audit` on `/` for the cluster's token ([above](#machines)). Default true, unless the management cluster switched the labels off for every cluster. Then it may not be true. |
| `controlPlaneMachine`, `workerMachine` | no | `cores` (default 2), `memoryMiB` (a multiple of 8, default 4096, with the minimums [above](#machines)), `diskGiB` (default 30 for the control plane and 40 for workers, at least 20). A disk only grows. |
| `cni` | kubeadm only | kubeadm: `cilium` or `calico`. RKE2: `canal` (default), `calico` or `cilium`. k3s and Talos: `flannel` (default), `cilium` or `calico`. The class refuses a change once the cluster exists. |
| `nodeOS` | no | kubeadm, RKE2 and k3s: `family` and `release`, a node OS Kubehenge builds templates of ([templates.md](templates.md#node-oses)). Default AlmaLinux 10. Changing it replaces every node with a clone of the new node OS's template, one at a time as in an upgrade. That template has to be built first. |
| `cis` | no | Harden the cluster to the CIS Kubernetes Benchmark ([above](#add-ons)). Default false. The class refuses a change once the cluster exists. |
| `loadBalancer` | no | Addresses for Services of type `LoadBalancer` ([above](#load-balancer)). `enabled` (default false). `implementation`, `metallb` (default) or `kube-vip`. `addresses`, a list of ranges, networks in CIDR notation or single addresses on the nodes' network, needed while `enabled`. |
| `headlamp` | no | `enabled` (default false). `hostname`, to serve it through the cluster's ingress controller. `tlsSecret`, a Secret in the `headlamp` namespace with the certificate for `hostname`. When unset, one from the certificates add-on while it is on, else the ingress controller's default certificate. `access`, `token` (default, where everyone signs in with their own token or through OpenID Connect) or `view`. With `view`, Headlamp shows everyone who reaches it the cluster read-only through its own account, bound to the `view` ClusterRole. |
| `monitoring` | no | `enabled` (default false), which installs cert-manager too ([above](#add-ons)). `hostname`, to serve Grafana through the cluster's ingress controller. `tlsSecret`, a Secret in the `monitoring` namespace with the certificate for `hostname`. When unset, as for `headlamp`. |
| `logs` | no | Loki and Grafana Alloy, which need `monitoring` ([above](#add-ons)). `enabled` (default false). `collector`, `node` (default) or `api`. `retentionDays` (1 to 366, default 7). `sizeGiB`, Loki's volume (1 to 1024, default 10), which only grows. |
| `backups` | no | Velero ([above](#back-up-and-restore)). `enabled` (default false). `target`, the name of one of the administrator's targets, needed while `enabled`. `schedule`, a cron expression of five fields or a descriptor such as `@weekly` (default `0 2 * * *`). `timeZone` (default `UTC`). `retentionDays` (1 to 3650, default 7). `volumes`, `all` (default) or `annotated`, only the volumes a pod names in its `backup.velero.io/backup-volumes` annotation. `existingSecret`, a Secret of your own in the cluster's `velero` namespace. It holds the store's keys as Velero's credentials file under the key `cloud`. It replaces the credential stored through the interface for the cluster, or else for every cluster. Kubehenge creates the namespace, and switching backups off deletes it with the Secret. `readFrom`, up to 10 clusters of the same namespace backing up to the same target whose backups this one lists to restore ([Restore into another cluster](#restore-into-another-cluster)). |
| `etcdSnapshots` | no | etcd snapshots ([above](#etcd-snapshots)). `enabled` (default false). `target`, the name of one of the administrator's targets, needed while `enabled`. `schedule`, as for `backups` (default `0 */12 * * *`). `timeZone` (default `UTC`). `keep`, the number of snapshots kept (1 to 1000, default 14). `distributionUpload` (default false), on RKE2 and k3s only and while `enabled`, the distribution's own upload ([above](#the-distributions-own-upload)). They are written with the credential stored through the interface for the cluster, or else for every cluster. |
| `podSecurity` | no | `admission`, the level Pod Security Admission enforces in every namespace that labels none of its own: `privileged`, `baseline` (default) or `restricted`. Changing it replaces the control-plane machines. `kyverno`, the level Kyverno enforces in every namespace: `off` (default), `baseline` or `restricted` ([above](#add-ons)). |
| `istio` | no | `enabled` (default false): Istio in ambient mode ([above](#add-ons)). |
| `clusterMesh` | no | For a cluster that runs Cilium ([above](#add-ons)). `enabled` (default false). `peers`, the clusters of this namespace to link with, by name. |
| `certificates` | no | `enabled` (default false). `issuer`, `acme-dns01` (default), `acme-http01` or `ca`. `domain`, needed with `acme-dns01` and `ca`. `acme` holds `email`, `server`, `staging`, `caBundle`. `dns01` holds `provider`, its settings, `existingSecret`, `sharedCredential`, `nameservers` ([above](#certificates)). |
| `rancher` | no | `install` (default false), which needs `hostname` and at least 4 vCPUs on every node. `register` (default true), to register with the Rancher Kubehenge installed. Setting `install` back to false removes every registered cluster from Rancher, then uninstalls it. A second cluster with `install` gets no Rancher. `kubectl describe cluster` shows a `RancherRefused` event. |
| `gitops` | no | `enabled` (default false). `engine`, `argocd` (default) or `flux`. `repository`, with `url` (`https://` or `ssh://`), `revision` (a branch or full ref, default `main`) and `path` (default `.`). `auth`, `none` (default), `deployKey`, `token` or `existingSecret`. `deployKey` works over `ssh://`. The key is in the Secret `<name>-gitops` beside the Cluster, its public half under `identity.pub`. `token` is the cluster's `gitops` credential, `username` and `password`. With `existingSecret`, the field `existingSecret` names the Secret. `knownHosts`, the server's host keys as known_hosts lines, at least one for the URL's host and port. It is required over `ssh://` except for Flux with `existingSecret`. `hostname` and `tlsSecret`, to serve the web interface through the ingress controller. `admins`, the first provider's groups that administer Argo CD. `signIn`, with `clientID` (the provider's own when unset) and `clientSecret`, `none` (default, PKCE), `stored` or `existingSecret`. `stored` is the cluster's `oidc-client` credential, key `clientSecret`. With `existingSecret`, the field `signIn.existingSecret` names the Secret. `ui` (default false), Flux's web interface, which needs `hostname` and a client secret. `ui` and `admins` need an identity provider in `oidc`. |
| `oidc` | no | Up to eight providers: `issuerURL`, `clientID`, `usernameClaim`, `usernamePrefix`, `groupsClaim`, `groupsPrefix`, `scopes`, `certificateAuthority` ([above](#signing-in-with-openid-connect)). Unset, the providers set for every cluster. |
| `upgrades` | no | Automatic Kubernetes upgrades. `mode` (`off`, `patch` or `minor`, default `off`) and `window` ([upgrading.md](upgrading.md#automatically-inside-a-window)). `nodeImages` (default true) sets whether the nodes move to new builds of their VM template in the window ([above](#node-image-refresh)). On Talos it sets whether they move to new Talos patch releases ([above](#talos-upgrades)). |
| `talosVersion` | Talos only | The Talos release the nodes run, such as `v1.14.4`. Changing it upgrades them in place ([above](#talos-upgrades)). That goes a minor version at a time, never back, and only to a release that runs the cluster's Kubernetes version. |
| `autoscaler` | no | How the cluster autoscaler sizes the autoscaled pools ([above](#autoscaling)). `placement`, `management` (default) or `workload`. `expander`, `least-waste` (default), `most-pods` or `random`. `scaleDownDelayAfterAddMinutes` and `scaleDownUnneededMinutes`, 1 to 1440, default 10. |

### Day two

```sh
kubectl -n clusters patch cluster prod --type=json \
  -p '[{"op":"replace","path":"/spec/topology/workers/machineDeployments/0/replicas","value":5}]'
kubectl -n clusters patch cluster prod --type=merge -p '{"spec":{"topology":{"version":"v1.37.1+rke2r1"}}}'
kubectl -n clusters delete cluster prod
```

A control plane whose roll stalled is recovered keeping one of its machines, as **Recover…** does. The cluster's conditions and events say how it goes:

```sh
kubectl -n clusters annotate cluster prod \
  kubehenge.dev/etcd-restore="{\"machine\":\"prod-cp-abcde\",\"at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
```

What Kubehenge's controller reports on a cluster is on the Cluster itself, as in the interface:

```sh
kubectl -n clusters get cluster prod -o jsonpath='{range .status.conditions[?(@.type=="KubehengeReady")]}{.status} {.message}{"\n"}{end}'
kubectl -n clusters get cluster prod -o jsonpath='{range .status.conditions[?(@.type=="MachinesStalled")]}{.status} {.message}{"\n"}{end}'
kubectl -n clusters get events --field-selector involvedObject.name=prod,type=Warning
```
