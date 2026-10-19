# Architecture

Kubehenge assembles maintained upstream pieces into one tested whole for Proxmox VE. It writes as little of its own as it can: the cluster templates, the add-on catalogue, the image pipeline, the first-run bootstrap and the interface.

## Why Cluster API

Cluster API is the Kubernetes project's own way to describe clusters as Kubernetes objects and have controllers create and keep them in that state. VMware's vSphere Kubernetes Service and OpenStack's Magnum both run on it. It gives Kubehenge, for free:

- **Several distributions over one infrastructure layer.** The distribution is a swappable bootstrap and control-plane provider; Proxmox VE stays the same underneath.
- **Day two, not only day one.** Rolling upgrades with nodes replaced one at a time, scaling by changing a number, health checks that replace dead nodes, clean deletion.
- **GitOps.** Clusters are YAML, so they work from Git with any GitOps tool.

A Terraform/OpenTofu-only design would create clusters once and leave upgrades and node replacement to be rebuilt for every distribution.

## Components

```text
Kubehenge interface (the appliance, an LXC container on Proxmox VE)
        │ acts with a ServiceAccount of its own
        ▼
management cluster (small, Talos, on Proxmox VE VMs; runs Kubehenge and nothing else)
 ├─ Cluster API core, through the Cluster API Operator
 ├─ capmox                       infrastructure: clones Proxmox VE VM templates, sets CPU, memory
 │                               and disks, assigns addresses from IP pools
 ├─ bootstrap + control plane    kubeadm (Cluster API core) | RKE2 (Rancher) | k3s (k3s project);
 │                               Talos to follow
 ├─ IP address management        in-cluster IP pools for node addresses
 ├─ Add-on Provider for Helm     installs each cluster's add-ons as Helm charts
 ├─ cert-manager                 certificates for the providers' webhooks
 ├─ Kubehenge ClusterClasses     one tested template per distribution
 └─ Kubehenge controller         each cluster's Proxmox VE objects, add-ons, Rancher registration,
                                 automatic upgrades
        │
        ▼ creates and manages
workload clusters on Proxmox VE VMs
 ├─ always: Proxmox CSI plugin, Proxmox cloud controller manager, control-plane virtual IP, CNI
 └─ chosen: Rancher, Headlamp, monitoring, pod security
```

## Management cluster

The management cluster is an appliance: it runs Cluster API, its providers and Kubehenge, and no workloads of anyone's. Rancher, monitoring and everything else users run go into the clusters it creates. Keeping it bare keeps it small, keeps its attack surface to what Kubehenge needs, and means losing a workload cluster never touches the piece that repairs it.

It runs Talos Linux: an immutable operating system with no shell or SSH, configured and upgraded only through its API, which suits a machine nobody should log into. One control-plane node is enough for a lab; three make it highly available, with Talos's own virtual IP in front of the API server. Control-plane nodes also run Kubehenge's controllers, so there are no separate workers.

Cluster API's providers are installed by the Cluster API Operator, so each provider and its version is a Kubernetes object that the Terraform/OpenTofu configuration declares, and upgrading a provider is changing a version number. Each runs its release's own image, with one exception while it lasts: capmox v0.9.1 runs as built by Kubehenge's release from a later upstream commit carrying a fix kubeadm upgrades need (`capmox_build` in `management-cluster/versions.tf`), reproducibly, so the published image can be rebuilt from that commit and compared ([releasing.md](releasing.md)); the next capmox release that carries the fix replaces it. `capmox` signs in to Proxmox VE with an API token of its own, created for it with only the privileges it needs.

Every distribution's providers are installed, but their controllers run only while a cluster uses that distribution. Without one, Kubehenge's controller scales their Deployments to none, so a management cluster that only ever builds RKE2 clusters spends no memory on kubeadm's or k3s's controllers. The six take about 50 MiB each, about 300 MiB in all. When the first cluster of a distribution is created, the controller scales them up. Once they serve, it applies the distribution's control-plane and bootstrap templates, which their webhooks have to admit; Cluster API holds the new cluster until then, which adds a minute or so to the first cluster of each distribution. When the last cluster is gone, the controller removes the templates, then scales the providers back to none. All the controller may do to the providers is change their replica count, through the Deployments' `scale` subresource; the Operator leaves that count alone because the configuration removes it from the providers' manifests.

## ClusterClasses

One ClusterClass per distribution: kubeadm, RKE2 and k3s, and Talos once Cluster API's Talos providers support ClusterClass in a release. Each takes the same small set of variables (Kubernetes version, control-plane and worker counts and sizes, Proxmox VE nodes, storage and bridge, node addresses, control-plane virtual IP, add-ons) so that creating a cluster is a few lines whichever distribution it uses. Each wires in the always-installed set and passes the end-to-end suite before it is called supported (`testing.md`). They ship in the `kubehenge` Helm chart (`charts/kubehenge`), which the management cluster's configuration installs. The chart creates each ClusterClass and its Proxmox VE templates directly. Its control-plane and bootstrap templates go into a ConfigMap named after the class, which the controller applies while a cluster uses the distribution.

## Each cluster's own corner of Proxmox VE

A workload cluster runs a cloud controller manager and a CSI plugin that talk to the Proxmox VE API, so a cluster holds Proxmox VE credentials. Those credentials reach that cluster's VMs and volumes and nothing else, so a compromised cluster cannot touch its neighbours or the management cluster. Within the cluster, only the cloud controller manager and the CSI plugin's controller hold them, and both run on the control-plane nodes only, so no worker ever has them. The cloud controller manager uses the nodes' own network, so it initialises new nodes before any CNI runs.

- **A pool per cluster**, nested under the workload pool: `<workload pool>/<namespace>.<cluster>`. Cluster API's Proxmox VE provider creates the cluster's VMs in it.
- **A user and API token per cluster**, with rights on its own pool only: reading its VMs for the cloud controller manager, and configuring their disks for the CSI plugin. A group all clusters' users belong to adds what they share: using space on the workload storages and reading the nodes. None of them can delete arbitrary volumes or change a storage's definition.
- **A VM that owns the cluster's volumes**, in the cluster's pool and never started. The CSI plugin creates volumes under its VMID, so only this cluster's token may attach them, and deleting the cluster deletes its volumes with it.
- **Deleting a volume through its owner VM.** The CSI plugin deletes a volume through the storage, which Proxmox VE allows only with `Datastore.Allocate` on the whole storage: every volume on it, and its definition. Kubehenge's controller instead deletes the volume of each released persistent volume through the owner VM, as an unused disk of that VM, with the cluster's own token; the plugin then finds the volume gone and completes. Proxmox VE deletes a VM's unused disk on the node the VM is on, so for a volume on storage local to another node the controller first moves the owner VM there; it has no disks, so only its configuration moves.

Kubehenge's controller, in the management cluster, creates all of this before Cluster API creates any VM: Cluster API asks it through a lifecycle hook (`BeforeClusterCreate`, Cluster API's Runtime SDK) and waits until it answers. It hands the token to the cluster through a ClusterResourceSet, installs the cloud controller manager and CSI plugin through the Add-on Provider for Helm, and removes the pool, user and volume owner once the cluster's VMs are gone. Removing the owner VM deletes its volumes on shared storage and on its own node; the controller deletes those on other nodes' local storage first, and waits while a node that may hold some is offline. Its own token may create pools under the workload pool, users in the clusters' group and ACLs on those pools, move and configure the VMs in the workload pool, use the workload storages as the clusters do, and nothing more.

The controller also tells `capmox` where to put each cluster's VMs. Left to itself, `capmox` clones every VM onto the node holding its template when a cluster names no nodes, and places no VM at all while one node it names does not answer, which is when a cluster needs replacements most. So the ClusterClasses leave the nodes to an external patch the controller serves (`GeneratePatches`): the cluster's `allowedNodes` that are online, or every online node when it names none. Every 15 seconds the controller reads which nodes are online and records them on each Cluster (`kubehenge.dev/online-nodes`), which has Cluster API compute the topology again when the list changes.

Every 30 seconds the controller also records on the kubeadm ClusterClass the Kubernetes versions its token finds templates for in the template pool (`kubehenge.dev/kubeadm-versions`), and on the RKE2 and k3s ClusterClasses whether their shared template is there (`kubehenge.dev/cloud-image-template`). The interface offers those versions, and refuses a cluster whose template is missing, since Cluster API clones with its own token: the people who create clusters need no privilege on the template pool.

With Proxmox VE HA switched on ([install.md](install.md#proxmox-ve-ha)), the controller also makes each joined control-plane VM of a cluster on shared storage an HA resource, and keeps two HA rules per cluster: a negative resource affinity among those VMs, sized to the nodes Proxmox VE's HA manager knows, and a strict node affinity to the cluster's nodes when it may use only some. It does so with a token of its own, since only `Sys.Console` on `/` may write HA configuration, and the cluster tokens never see it. Two things follow from HA moving VMs that Cluster API's Proxmox VE provider does not expect:

- **A pre-terminate hook** (`pre-terminate.delete.hook.machine.cluster.x-k8s.io/kubehenge-ha`) on each control-plane Machine holds its VM's deletion until the controller has taken the VM out of HA, as Proxmox VE refuses to delete an HA resource's VM.
- **Its VM's node.** The provider deletes a VM on the node it recorded, and takes Proxmox VE's answer for a VM that has moved to mean it is gone, which would leave the VM running. So when a VM has moved, the controller marks its ProxmoxMachine with the node it is on (`kubehenge.dev/proxmox-node`); the change has the provider look the VM up across the cluster and record its node, and the hook stays until it has.

## Images

Each distribution boots from a different VM template:

| Distribution | Template |
| --- | --- |
| kubeadm | Ubuntu's cloud image with containerd, the kubelet and kubeadm preinstalled at one Kubernetes version |
| Talos (clusters to follow) | Talos's own image from the Talos Image Factory, with the QEMU guest agent |
| RKE2, k3s | Ubuntu's cloud image, updated and with the QEMU guest agent; the distribution installs at boot |

`kubehenge template create` (or the Administration page in the appliance, with an administrator's session) fetches the image, prepares it by booting it once where it needs more, and turns it into a template `capmox` selects by its tags, all through the Proxmox VE API. Details: [templates.md](templates.md).

## Add-ons

Add-ons are Helm charts installed per cluster by the Cluster API Add-on Provider for Helm, chosen when a cluster is created or later.

| Add-on | What | Release |
| --- | --- | --- |
| Rancher | runs in a cluster Kubehenge creates for it; Kubehenge registers its other clusters with it, so they appear in Rancher's interface | first |
| Headlamp | Kubernetes web UI per cluster | first |
| Monitoring | Prometheus, Alertmanager and Grafana per cluster, with kube-prometheus-stack's dashboards and alerts | first |
| Pod security | a Pod Security Standard level enforced by Pod Security Admission, by Kyverno's policies, or both | first |
| CIS hardening | the rest of the CIS Kubernetes Benchmark, beyond the baseline every cluster gets, with RKE2's own CIS profile | first |
| Istio | Istio in ambient mode per cluster: mutual TLS between the pods of the namespaces enrolled, without sidecars | first |
| Cilium Cluster Mesh | connects the pod networks of clusters that run Cilium | next |

Kubehenge's controller turns a cluster's add-on variables into HelmChartProxies beside the cluster, and deletes the proxy when an add-on is switched off, which uninstalls it. Values never carry a secret: the Add-on Provider for Helm keeps them in plain text.

**Headlamp** (`headlamp: {enabled: true}`) has no rights of its own: its service account is bound to no role, and everyone signs in with their own token or through OIDC, so Headlamp shows each user exactly what their credentials allow. It is not exposed: `kubectl -n headlamp port-forward svc/headlamp 8080:80` reaches it from a workstation, and `hostname` adds an Ingress on the cluster's ingress controller (RKE2 and k3s bring Traefik; kubeadm brings none), with the certificate from `tlsSecret` or the controller's default. Plugins are those in its image; nothing is fetched at run time. Its pod meets Pod Security's restricted level.

**Monitoring** (`monitoring: {enabled: true}`) installs kube-prometheus-stack into the `monitoring` namespace, with cert-manager, and every scrape is authenticated and verifies its server; nothing that serves metrics without authentication is opened beyond its node's loopback interface:

- **etcd, kube-proxy, the controller manager and the scheduler** serve their metrics on loopback only, as the CIS benchmark asks of the last two. A kube-rbac-proxy in front of each, on the node's address and the component's own port, takes only Prometheus's client certificate and asks the API server whether it may read `/metrics`. etcd and kube-proxy serve plain HTTP; the controller manager and the scheduler serve HTTPS, which their proxies verify against the cluster CA for `localhost`, and let `/metrics` through on loopback without credentials. On RKE2 their certificates come from the server CA; kubeadm's would be signed anew on every start by a CA of their own, so the class gives each control-plane node certificates from the cluster CA instead. Prometheus gets no etcd client certificate, which would give it every key in the cluster. On k3s every component runs in one process, whose kubelet already serves all their metrics, so Prometheus takes them from the kubelet, as Rancher does, and the chart's alerts look for them there.
- **Kubelets** on kubeadm ask the cluster CA for their serving certificates, which Kubehenge's controller approves when they name only the node and its Machine's addresses; the API server verifies them too. RKE2 and k3s sign theirs with the server CA.
- **node-exporter** listens on loopback behind its chart's kube-rbac-proxy, which takes the same client certificate. It reads the host's namespaces and file system, which only Pod Security's privileged level allows, so it has a namespace of its own labelled privileged; the rest of the add-on meets the restricted level.
- **Certificates:** cert-manager runs a CA of the add-on's own (ClusterIssuer `kubehenge-metrics`), which signs the proxies' serving certificates and Prometheus's client certificate, and nothing else. The chart's long-lived token is sent only to what asks for one, never over plain HTTP.

Prometheus keeps ten days of metrics on a 20 GiB volume from the cluster's default storage class, Alertmanager its silences on 1 GiB. `hostname` serves Grafana through the cluster's ingress controller, with the certificate from `tlsSecret` or the controller's default; without it, `kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80` reaches it. Its `admin` password is generated into the `kube-prometheus-stack-grafana` Secret. The release carries the proxies and certificates, so switching monitoring off removes them with it.

**Istio** (`istio: {enabled: true}`) installs Istio in ambient mode, without sidecars: istiod into `istio-system`, and ztunnel and istio-cni on every node into `istio-node`. A namespace or pod labelled `istio.io/dataplane-mode=ambient` joins the mesh: istio-cni redirects its pods' traffic through the ztunnel on their node, which carries it to the other side's ztunnel over mutual TLS with each workload's identity, so Istio's policies by identity apply, such as a `PeerAuthentication` that requires mutual TLS or an `AuthorizationPolicy` by service account. ztunnel and istio-cni reach into the nodes' network namespaces, which only Pod Security's privileged level allows, so `istio-node` is exempt from pod security; istiod meets the restricted level. With the Cilium Kubehenge installs, Cilium's `cni.exclusive` is off while Istio is on, so that Cilium leaves istio-cni's plugin in place. Waypoints, Istio's layer 7 proxies, need the Gateway API's CRDs, which Kubehenge does not install. Switching Istio off first removes the `istio.io/dataplane-mode` label from every namespace and pod and waits until istio-cni has taken every pod's redirection away, since a pod still redirected once ztunnel has gone reaches nothing; then it uninstalls the charts. Istio's CRDs stay, with any resources of them.

**Pod security** (`podSecurity: {admission: …, kyverno: …}`) holds a cluster's pods to a [Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/), `baseline` or `restricted`, through either or both of two checks:

- **Pod Security Admission**, Kubernetes' own: every ClusterClass gives the API server an admission configuration whose default level is `admission`, for every namespace that labels no level of its own. Left at `privileged`, Kubernetes' own default, it enforces nothing; at `baseline` it also warns about and audits pods short of `restricted`. The configuration is a file on the control-plane nodes, so changing the level replaces them one at a time.
- **Kyverno**: `kyverno` installs Kyverno and its Pod Security Standards policies into the `kyverno` namespace. They hold every namespace to the level whatever its labels say, which Pod Security Admission cannot, since anyone who may label a namespace may lift its level; they deny a pod when Kyverno cannot be reached, and Kyverno's policy reports list what falls short. Switched off, the policies go first, then Kyverno.

Both leave alone the namespaces of the components that need the host's network, file system or privileges: `kube-system` (the cloud controller manager, the CSI plugin, Cilium, kube-vip and the distributions' own components), Calico's, node-exporter's and those of Rancher's agents and controllers. The chart's `podSecurityExempt` lists them. A workload of one's own that needs more than the level gets its namespace labelled, and, with Kyverno, a PolicyException in the `kyverno` namespace, the only one Kyverno takes exceptions from, so that only those who may write there can grant one.

**The security baseline** of every cluster is the part of the [CIS Kubernetes Benchmark](https://www.cisecurity.org/benchmark/kubernetes) that costs workloads nothing:

- **Audit:** the API server logs who did what to which object, at the `Metadata` level, without request or response bodies, which would put Secrets and tokens into the log, and without health checks, leases and events. It keeps a month of it, in ten files of 100 MB at most.
- **Secrets encrypted in etcd:** RKE2 and k3s encrypt them themselves; on kubeadm, Kubehenge's controller makes a secretbox key once for the cluster, in the Secret `<cluster>-encryption` beside the Cluster, which every control-plane node gets. It is never replaced, since Secrets written under one key cannot be read with another.
- **Requests without credentials** are refused on RKE2 and k3s; on kubeadm they reach only the API server's health checks and the `cluster-info` ConfigMap, which `kubeadm join` reads before it has any.
- **The API server** limits the rate of events a namespace, and the cluster as a whole, may write (EventRateLimit), does not extend projected ServiceAccount tokens past their hour, and has profiling off, as do the controller manager and the scheduler, which serve on loopback only ([monitoring](#add-ons) reaches them through proxies). It and the kubelets offer only forward-secret TLS 1.2 ciphers with authenticated encryption.
- **Nodes** run with the kernel settings the kubelet expects, and the kubelet refuses to start on others rather than change them; kubeadm's kubelet configuration and unit files are readable by root alone.

**CIS hardening** (`cis: true`), chosen when a cluster is created and fixed after, adds what costs workloads something:

- Pod Security Admission enforces `restricted`, whatever `podSecurity` says; the exempt namespaces stay exempt.
- Every container that sets no seccomp profile of its own runs under the container runtime's default.
- Exec, attach and port-forward sessions end after five idle minutes.
- RKE2 runs its own CIS profile: etcd runs as a user of its own, and the pods in `kube-system`, `kube-public` and `default` take connections only from their own namespace, DNS and a few of RKE2's own services aside. With monitoring on, Kubehenge's controller lets Prometheus through to CoreDNS's metrics.

The end-to-end tests run [kube-bench](https://github.com/aquasecurity/kube-bench) on a hardened cluster of each distribution, against the CIS Kubernetes Benchmark 2.0 for kubeadm and Rancher's own benchmarks for RKE2 and k3s, and fail on every failed check but one: kubeadm runs etcd as root, so its data belongs to root (1.1.12), and Kubernetes has no supported way to run kubeadm's etcd as another user. The benchmark's policies section is the cluster owner's, since it judges what runs in the cluster rather than how the cluster is built.

**Rancher** (`rancher: {install: true, hostname: …}`) goes into one cluster Kubehenge creates for it, never into the management cluster, together with cert-manager; Rancher makes its own CA, and its server URL is fixed to `hostname`, which must resolve to that cluster's ingress controller for people and for every registered cluster. Kubehenge's controller registers every other cluster with it once the cluster is available, unless the cluster sets `register: false`:

- It signs in to Rancher's cluster with the kubeconfig Cluster API keeps for it, so there is no Rancher account or token to look after.
- It creates the cluster's `provisioning.cattle.io` Cluster there, fetches the agent's manifest through that cluster's API server, and applies it to the registered cluster. The agent trusts only Rancher's CA, pinned by its checksum in the manifest.
- A cluster leaves Rancher before it is deleted (Cluster API's `BeforeClusterDelete` hook) or when `register` turns false, while Rancher can still clean up inside it. Rancher's own cluster first removes every registered cluster.

Rancher's chart refuses Kubernetes versions its release does not run on, so the controller refuses to create the Rancher cluster outside them and holds back an upgrade beyond them (`BeforeClusterUpgrade`) until a Kubehenge release brings a Rancher that runs there. On fewer than 4 vCPUs a node running Rancher starves its own etcd and API server, so `BeforeClusterCreate` refuses such a cluster, and a validating webhook on Clusters refuses switching Rancher on or making its nodes smaller later. Rancher's first admin password is generated into the `bootstrap-secret` Secret in `cattle-system`, never written into values.

## Interface

A web app in a small LXC container on Proxmox VE, installed with one line in the Proxmox VE shell. It is where Kubehenge starts and what looks after it:

- **First run:** a wizard creates the management cluster, through the same module `tofu apply` uses, with the signed-in administrator's own Proxmox VE session; no administrator token is stored.
- **Day two:** the container keeps the management cluster's state, encrypted, and runs its Talos and Kubernetes upgrades, its growth from one node to three, moves of its VMs to other Proxmox VE nodes, and repair. It keeps working while the management cluster is unwell, to show and fix it. It holds state, so it belongs in Proxmox VE's backups; the interface warns administrators while no enabled backup job includes it (by its ID, by its pool, or by all guests on its node).
- **Sign-in through the Proxmox VE API.** Every Proxmox realm works, PAM, Proxmox VE users, LDAP, Active Directory and OIDC included. No second user directory. A second factor is passed on to Proxmox VE as its own sign-in page would: TOTP, Yubico OTP and recovery keys. A WebAuthn key is bound to Proxmox VE's own address, so its users sign in through an OpenID realm or another factor. An OpenID realm sends the browser to its identity provider, which returns it to the interface (`/api/openid/callback` under `--public-url`, to be registered with the provider); Proxmox VE checks the code, and the interface accepts the return only in the browser that started it.
- **Sessions:** the Proxmox VE ticket stays on the server, in memory only; the browser holds a random session ID in a `__Host-` cookie (Secure, HttpOnly, SameSite=Strict) and every change also carries a per-session CSRF token. Proxmox VE renews a ticket without asking the realm or the second factor again, so a session ends after a fixed time (eight hours by default) however active it is. Failed sign-ins are limited per user and per address. The frontend loads no inline script or style, and the Content-Security-Policy allows none.
- **Permissions follow Proxmox VE's, by pool.** Each cluster has its own pool under the workload pool. Seeing a cluster needs `VM.Audit` on its pool; creating one needs `VM.Allocate` on the workload pool; scaling, upgrading, changing add-ons and deleting it need `VM.Allocate` on its pool; downloading its admin kubeconfig needs `Sys.Modify` there. Creating also needs what Proxmox VE asks of anyone creating a VM there: `Datastore.AllocateSpace` on the storage and `SDN.Use` on the bridge or VNet (on the VLAN, when one is set). Installing Rancher needs `Sys.Modify` on the workload pool, since Rancher receives the admin kubeconfig of every cluster that registers with it. The app asks Proxmox VE for the signed-in user's effective privileges on each path, then acts with its own account; administrators share clusters with the same ACLs they share VMs with. A cluster the user may not see is answered as not found. As everywhere in Proxmox VE, a role given to a user on a cluster's own pool replaces, for that pool, what the user inherits from the workload pool, so it has to hold `VM.Audit` too.
- **What it does:** create, list, scale, upgrade and delete clusters; choose the distribution, add-ons, identity providers and automatic upgrades; download a kubeconfig; open each cluster in Headlamp; and, for administrators, build VM templates, upgrade Kubehenge, and apply, change or remove the management cluster.
- **Headlamp in the appliance:** one Headlamp, the appliance's own pinned copy (its server and frontend, taken from Headlamp's release and checked against its checksum when Kubehenge's release is built), serves the clusters of the interface's namespace on port 8443, behind the interface's sign-in. Being a separate origin, no page it shows can run script where the interface's session lives.
  - **Its kubeconfig:** the interface writes each cluster's API address and CA there, never a credential, since Headlamp would use one for every visitor. Headlamp picks up clusters as they come and go. People sign in to each cluster with their own token, or through the cluster's identity provider (see Signing in to clusters), and Headlamp keeps it in an HttpOnly cookie for that cluster's paths only.
  - **Isolation:** the Headlamp server runs as a user of its own (`kubehenge-headlamp.service`, systemd's dynamic user) on the loopback interface. It can read nothing of the interface's state, and only the interface reaches it.
  - **Only while used:** Headlamp holds more of the appliance's memory than the interface itself, so it runs only while someone has it open. systemd listens on its port and starts it with the first connection (a quarter of a second). It stops again once no connection has been open for 15 minutes.
  - **The gate in front:** the interface takes the session cookie (host-only cookies reach every port) and strips its own cookies before passing a request on. It checks `VM.Audit` on the cluster's pool for every `/clusters/<namespace>.<name>/` request, rechecking every 30 seconds, and lists only those clusters. It refuses what a shared server must not offer: port forwards (they would open ports on the appliance), Helm, node draining, the external proxy, the WebSocket multiplexer, metrics, adding clusters or kubeconfigs, setting a token for others, and plugin changes.
  - **The add-on:** the in-cluster Headlamp add-on remains for a cluster that wants Headlamp at a hostname of its own.
- **Look:** it follows Proxmox VE's interface, so it feels part of it. Proxmox VE's own toolkit is GPL-licensed, so the look is Kubehenge's own theme, not borrowed code.
- **Built as:** a Go backend (`pkg/ui`, run by `kubehenge serve`), which uses the Cluster API and Kubernetes client libraries directly, serving a React and TypeScript frontend (`web/`) embedded in the same binary. The `kubehenge` command-line tool shares the backend's code. Clusters created here carry the creating user in the annotation `kubehenge.dev/created-by`, and every change is logged with the user who made it.

### Optional entry inside Proxmox VE

Proxmox VE has no supported way to extend its own web interface. An opt-in installer (`appliance/pve-entry/`) adds a Kubernetes entry that leads to the appliance:

- **What it adds:** a button beside Create VM and Create CT, and an item under Datacenter. Both open the appliance in a new tab, since the appliance refuses to be framed and stays an origin of its own.
- **How:** one line added to the page template `pve-manager` ships, loading a script of ExtJS overrides that runs before Proxmox VE builds its workspace.
- **Surviving updates:** an apt hook puts the line back after a `pve-manager` update replaces the template, and logs it. `--remove` returns the node to its packaged state.
- **Failing safe:** the script extends two of Proxmox VE's interface classes, which are no API. If a release renames them, or a step of the entry fails, the entry stays out and Proxmox VE's interface renders as shipped, with the reason in the browser's console.
- **Scope:** each node serves its own interface, so it is installed per node; `appliance/install.sh` asks whether to add it on the node it runs on. The appliance never depends on it.

## First run

Cluster API needs a cluster before it can create clusters. One Terraform/OpenTofu apply, or the appliance itself, creates a small management cluster on Proxmox VE VMs with everything above installed. From then on, clusters are created from the interface or from YAML.

The appliance (`appliance/install.sh`) checks everything it downloads against checksums it carries, creates an unprivileged Debian container, gives it a certificate signed by the Proxmox VE cluster's own CA, and runs the interface as an unprivileged user on ports 443 and 8443 (the shared Headlamp). Its first-run page runs the `management-cluster/` module with OpenTofu on the signed-in administrator's session: the ticket and CSRF token reach OpenTofu through its environment only, and the state is encrypted with a key derived from a passphrase the administrator chooses, which is never stored and is asked for again by every later apply. The management cluster's admin kubeconfig and talosconfig stay inside that state, which an administrator can open from the Administration page with the passphrase for repairs the interface cannot make (every download is logged); the interface works with a ServiceAccount of its own that sees every cluster but changes clusters, and reads their kubeconfigs, only in its namespace (`clusters`).

## Signing in to clusters

Every cluster accepts client certificates and ServiceAccount tokens, as Kubernetes does. Its `oidc` variable adds OpenID Connect providers whose ID tokens its API server accepts, up to eight, written as Kubernetes' structured authentication configuration (one JWT authenticator per provider). Each entry has:

- the issuer URL;
- the client ID, which a token's audience must name;
- the claims for user name and groups;
- prefixes, `oidc:` unless set, which may not start with `system:`;
- the scopes sign-in tools ask for;
- a CA, when a public one does not sign the provider's certificate.

- **The appliance-wide default.** Providers set on the Administration page, or as the module's `cluster_oidc` (the chart's `clusterOIDC`), are the variable's default. Every new cluster trusts them unless it sets its own list, and the create wizard starts from them. A change applies to clusters created afterwards.
- **Who may choose.** Whoever runs a provider can issue a token for any name, so a cluster may keep or drop the default's providers freely. Adding any other provider needs `Sys.Modify` on the pool, as the admin kubeconfig does: on the workload pool for a new cluster, on the cluster's own pool for a change.
- **Changing it.** Changing a cluster's providers replaces its control-plane nodes, as any change of the API server's configuration does.
- **Headlamp.** The shared Headlamp offers the first provider's sign-in beside the token prompt, as a public client with PKCE, so no client secret is stored. The provider must accept `https://<appliance>:8443/oidc-callback` as a redirect URL. The browser that starts a sign-in is the only one the gate lets finish it: it keeps a hash of the OAuth state in a short-lived cookie.

## Upgrades

**Kubehenge itself.** The appliance's Administration page shows the installed release and newer ones, and upgrades with a button; it can also upgrade automatically, once a day at a chosen hour, to new patch releases or to new minor releases of the same major version. The interface never replaces software itself: it drops a request into a spool, and a separate root service (`kubehenge-update`) carries it out. That service accepts only a release named in an index signed with Kubehenge's release key (Ed25519, built into each release, never configurable at run time), only one newer than the installed release, and only an archive matching the signed checksum; it swaps the files by rename and restarts the interface. A run of the management-cluster module holds a lock on the appliance's state directory, which the updater and `install.sh --upgrade` take too, so no upgrade restarts the interface in the middle of a run and no run starts during an upgrade; the kernel releases the lock when its holder ends, so a crash leaves none behind. The management cluster then moves to what the new release carries when an administrator applies it again from the same page.

**Clusters.** The `upgrades` variable of every ClusterClass asks Kubehenge's controller to upgrade Kubernetes automatically: `patch` keeps the minor version, `minor` also moves to the next one, one minor at a time and never past the distribution's stable release (RKE2's and k3s's release channels; for kubeadm, the newest version a template has been built for). It starts only inside the window (days of the week or of the month, a start time and length in a named time zone), only while the cluster is available with no rollout running, and not beyond the Kubernetes versions an installed Rancher runs on. Cluster API then rolls the nodes as for any upgrade; a rollout that has started finishes even if the window closes. The last automatic upgrade is recorded on the Cluster as `kubehenge.dev/auto-upgraded`.

## Networking

- **Node addresses** come from IP pools managed in the management cluster; no DHCP is needed on the Proxmox VE bridge.
- **The control plane** of each cluster gets a virtual IP that moves between control-plane nodes.
- **Pod networking** comes from each distribution's own CNI unless another is chosen for the cluster: canal for RKE2, flannel for k3s and Talos. RKE2 also ships Calico and Cilium and installs either itself. kubeadm has none of its own, so one is chosen when a kubeadm cluster is created, Cilium or Calico, and Kubehenge's controller installs it; it does the same for a k3s cluster given Cilium or Calico, whose flannel and network policy controller are then switched off. Calico comes as the Tigera operator's chart, the operator creating Calico's CRDs, and its Installation through the cluster's ClusterResourceSet once they exist: Calico's CRD chart is too large for a Helm release. The CNI is chosen at creation and stays.
- **Pod and service ranges** must stay clear of the nodes' network. Kubehenge refuses to create a cluster whose ranges, its own or its distribution's defaults (Kubernetes' 10.96.0.0/12 for kubeadm's services), overlap it. The interface sets them on every cluster it creates: those asked for, or else the distribution's own when they are clear of the nodes' network, or else the first of a few private ranges that are. Every class honours the Cluster's ranges; the k3s provider reads its own server configuration only, so the k3s class copies them there.
