# Troubleshooting

Where to look first, then the problems that come up most.

## Where to look

### The appliance

The appliance is the container tagged `kubehenge`. It runs no SSH server; reach it from the shell of its node with `pct`.

| Unit | What it runs |
| --- | --- |
| `kubehenge.service` | The interface on port 443, and the gate in front of the shared Headlamp on port 8443. It also runs the management cluster's applies and the VM template builds. A restart of it signs everyone out. |
| `kubehenge-headlamp.service` | The shared Headlamp server, on the loopback interface only. `kubehenge-headlamp.socket` starts it on the first visit, through `kubehenge-headlamp-proxy.service`. The proxy exits once nobody has had Headlamp open for 15 minutes, and Headlamp stops with it, so `inactive` is normal while nobody uses it. |
| `kubehenge-update.path` | Watches for upgrade requests from the interface. |
| `kubehenge-update.service` | The updater, which installs a signed newer release when asked. |

```sh
pct exec <ctid> -- systemctl status kubehenge kubehenge-headlamp.socket kubehenge-headlamp kubehenge-update.path
pct exec <ctid> -- journalctl -u kubehenge
pct exec <ctid> -- journalctl -u kubehenge-update
pct exec <ctid> -- journalctl -u kubehenge-headlamp
```

The interface logs in JSON, one line per event, and every change names the user who made it: clusters created, changed and deleted, kubeconfigs and management cluster credentials handed out, applies and removals of the management cluster, upgrades requested.

- **The last run of the management cluster** shows on the Administration page, with its error and log when it failed, and stays there after a restart; one cut off by a restart says so.
- **The last template build** shows under VM templates on the same page, kept the same way.
- **The last upgrade** shows on the Administration page; the updater keeps it in `/var/lib/kubehenge-update/status.json`.

### The management cluster

The Administration page's **Repair access** hands administrators the management cluster's own credentials, for what the interface cannot do. It needs the state passphrase and the [setup privileges](install.md#who-may-set-it-up), is refused while a run is going on, and every download is logged.

- **Download admin kubeconfig** saves `management.kubeconfig`, for kubectl.
- **Download admin talosconfig** saves `management.talosconfig`, for `talosctl`.

With Terraform/OpenTofu, take them from the state: `tofu output -raw kubeconfig` and `tofu output -raw talosconfig`.

```sh
export KUBECONFIG=management.kubeconfig
kubectl get pods --all-namespaces
kubectl -n kubehenge-system logs deploy/kubehenge-controller
talosctl --talosconfig management.talosconfig --nodes 192.0.2.11 health
```

Kubehenge's controller, `kubehenge-controller` in `kubehenge-system`, creates each cluster's Proxmox VE objects, installs its add-ons, registers it with Rancher, deletes released volumes and starts automatic upgrades. Its log says what it is waiting for. The providers it works with:

| Namespace | What |
| --- | --- |
| `capi-system` | Cluster API core |
| `proxmox-infrastructure-system` | Cluster API's Proxmox VE provider, `capmox-controller-manager`: clones, sizes and starts the VMs |
| `kubeadm-bootstrap-system`, `kubeadm-control-plane-system` | kubeadm |
| `rke2-bootstrap-system`, `rke2-control-plane-system` | RKE2 |
| `k3s-bootstrap-system`, `k3s-control-plane-system` | k3s |
| `in-cluster-ipam-system` | the IP pools nodes take their addresses from |
| `helm-addon-system` | the Add-on Provider for Helm, which installs add-ons into clusters |
| `capi-operator-system` | the Cluster API Operator, which installs and upgrades the providers |

### Cluster API objects

A cluster's objects are in its namespace, `clusters` for those created in the interface:

```sh
kubectl -n clusters get clusters,machines,proxmoxmachines
kubectl -n clusters describe cluster prod
kubectl -n clusters get helmchartproxies,helmreleaseproxies
```

- **The Cluster's conditions**, also on the cluster's page, say what holds it up, including what Kubehenge's hooks are waiting for.
- **Machines and ProxmoxMachines** say, per node, whether the VM was cloned, started and joined.
- **HelmChartProxies and HelmReleaseProxies** are the add-ons: the cloud controller manager, the CSI plugin, a CNI that Kubehenge installs, Headlamp and Rancher.
- **The Secret `<name>-proxmox-token`** holds what the controller made for the cluster on Proxmox VE; its `ready` key turns `true` once all of it exists.

### Inside a cluster

The cluster's **Health** view in the interface is the first place to look: it lists what needs attention, most serious first. When it says Kubehenge's read-only access has not been set up, or ran out, its controller has not reached the cluster's API server; the controller's log names the cluster and the reason (`health-identity`).

With the cluster's admin kubeconfig ([clusters.md](clusters.md#download-a-kubeconfig)): the cloud controller manager and the CSI plugin's controller run in `kube-system`, on the control-plane nodes.

```sh
kubectl --kubeconfig prod.kubeconfig -n kube-system get pods -o wide
kubectl --kubeconfig prod.kubeconfig get nodes -o wide
```

### Proxmox VE

The task log shows what each Kubehenge account did. In an appliance's management cluster, Cluster API acts as `kubehenge-cluster-api@pve` and Kubehenge's controller as `kubehenge-provisioner@pve`; each cluster's own user is named after its workload pool, namespace and name, such as `kubehenge.clusters.prod@pve`.

## Common problems

### Sign-in and permissions

- **"Kubehenge is not set up yet"** for an administrator: the user lacks one of the [setup privileges](install.md#who-may-set-it-up) on `/`.
- **A cluster is missing from the list:** the user lacks `VM.Audit` on its pool. A role given on the cluster's own pool replaces, for that pool, what the user inherits from the workload pool, so it has to hold `VM.Audit` too.
- **"`<user>` needs `<privilege>` on `<path>`"** names exactly what to grant ([clusters.md](clusters.md#who-may-do-what)).
- **Buttons are disabled or missing:** Scale, Upgrade, Delete and the **Edit** buttons under Options need `VM.Allocate` on the cluster's pool, and the page says so when it is missing; installing Rancher, offered when a cluster is created, needs `Sys.Modify` on the workload pool, and adding or changing identity providers needs it on the workload pool when creating and on the cluster's pool when changing; **Kubeconfig** needs `Sys.Modify` there and shows once the cluster is available. Clusters outside the interface's namespace are read-only there.
- **No bridges to choose from:** Proxmox VE lists a node's bridges only with `Sys.Audit` on the node; type the bridge's name instead. A bridge marked "no SDN.Use" needs that privilege on `/sdn/zones/localnetwork/<bridge>`.
- **"Proxmox VE no longer accepts the session":** the session ended; sign in again. Sessions last eight hours.
- **Sign-in refused after several tries:** five failures within 15 minutes, for a user or from an address, lock further attempts for a while.
- **"This account's second factor is a security key":** a WebAuthn key answers only on Proxmox VE's own page. Sign in with a TOTP code or recovery key, or through an OpenID realm.
- **An OpenID sign-in does not come back:** register `https://<appliance>/api/openid/callback` with the identity provider, and `https://<appliance>:8443/oidc-callback` for the shared Headlamp. A Headlamp sign-in can only be finished in the browser that started it.

### Setting up the management cluster fails

The Administration page (or the setup page) shows the error and the run's log. Fix the cause and apply again; a run picks up from what exists.

- **Storage:** the Talos image storage must allow the `import` content type.
- **Addresses:** the node addresses and the API address must be free, in one subnet, and reachable from the appliance on ports 50000 and 6443.
- **Internet access** from the appliance, the Proxmox VE node and the new nodes ([install.md](install.md#internet-access)), or registry mirrors for the nodes.
- **Pod and service ranges** that overlap the nodes' network are refused; set others under Advanced.
- **Time:** the run acts on a Proxmox VE ticket that lasts two hours.
- **"The management cluster is being applied or removed"** or **"the appliance is being upgraded":** wait for that to finish.
- **A wrong passphrase:** an apply cannot read the state, and repair access and removal answer "the passphrase does not open the management cluster's state". It has to be the passphrase given when the management cluster was created. Repair access refuses further tries for a while after five wrong ones. A passphrase that is lost cannot be recovered: [what still works, and how to start over](install.md#the-state-passphrase).
- **"The appliance stopped during the run":** the appliance restarted while a run was going on; apply again.
- **"job wait-for-providers failed"** after ten minutes: a provider was not installed or did not roll out. `kubectl -n capi-operator-system logs job/wait-for-providers` names each one with the Cluster API Operator's reason. A version lowered through `version_overrides` is refused ("Downgrade is not supported"). Setting it back does not repair the provider object on its own, since Helm compares against the last release that succeeded: set its `spec.version` back with `kubectl patch`, delete its cache Secret in its namespace (`<type>-<name>-<version>-cache`, such as `bootstrap-k3s-v0.4.0-cache`), which makes the operator install it again and renew its conditions, then apply again.
- **"job provider-manifests failed":** a provider's files could not be downloaded within ten minutes, or one did not match its digest. `kubectl -n capi-operator-system logs job/provider-manifests` names the file. A mismatch means the release's file is not the one the tested set names; nothing of it was installed.
- **"job wait-for-operator failed":** the Cluster API Operator's webhooks did not answer within ten minutes, so no provider was changed. `kubectl -n capi-operator-system logs job/wait-for-operator` gives their last refusal, and `kubectl -n capi-operator-system get pods` shows whether the operator runs; once it does, apply again.
- **"Provider produced inconsistent result after apply"** for `helm_release`, naming `.metadata.revision` and `.metadata.last_deployed`, on the apply after one that failed: the release was upgraded, and the Helm provider reports its own new revision as a fault (hashicorp/terraform-provider-helm, version 3.3.0). Apply once more; it then completes with nothing left to change.

### Template missing

The interface refuses a cluster whose template the controller does not find in the template pool. A cluster made with kubectl, or whose template went after it was made, has machines that stay provisioning; the cluster's **Machines** view shows under each what it waits for, such as `VM template not found`. Cluster API's Proxmox VE provider selects a template by its exact set of tags and needs exactly one match:

- RKE2 and k3s need the shared template (**Build a template**, For: RKE2 and k3s, on the Administration page, or `kubehenge template create rke2` with the default `--os`).
- kubeadm needs the template of the cluster's exact Kubernetes version, for creating a cluster and for upgrading one.
- The template has to be in the template pool, `kubehenge-templates`, the only one Cluster API may clone from.
- Two templates with the same tags are no match either.
- A template on storage local to one node can only be cloned there; put templates on shared storage.

`kubehenge template list` shows what the template pool holds, with each template's tags, and `kubehenge template list --pool ''` every pool's, which shows a template that was made elsewhere ([templates.md](templates.md)).

### Node addresses run out

A machine stays pending without an address. Every node takes one address from the cluster's node addresses, and an upgrade, a scale-up or a node replacement creates a new node before an old one goes. The IP pools show what is taken:

```sh
kubectl -n clusters get inclusterippools,ipaddressclaims,ipaddresses
```

The network is fixed once a cluster exists in the interface, so give each cluster a range with room to spare: at least one address per node, and more for the nodes a rollout adds.

### Virtual IP collisions

Each cluster's API address, and the management cluster's, is a virtual IP that one control-plane node answers for, by ARP. If another machine uses the same address, the API is unreachable or reaches the wrong machine: kubectl fails, nodes do not join, or certificates do not match.

- Choose an address that nothing else uses: not a node address, not in a DHCP server's range, not another cluster's API address.
- Keep it outside the cluster's node addresses, in the same subnet as the nodes.
- Check it is free before creating the cluster, for example with `ping` or `arping` from a machine on the same network.

### Nodes not initialised

Nodes keep the taint `node.cloudprovider.kubernetes.io/uninitialized`, and pods stay pending. The Proxmox VE cloud controller manager initialises new nodes, and it has to reach the Proxmox VE API:

- The cluster's nodes must reach the Proxmox VE API at the address the management cluster uses: in the appliance, the installing node's address on the appliance's bridge, port 8006; with Terraform/OpenTofu, `proxmox_endpoint`.
- Look at the cloud controller manager's pods and logs in `kube-system`, on the control-plane nodes.
- Check that the cluster's pool, user and token exist on Proxmox VE (`<name>-proxmox-token` reads `ready`), and that its VMs are in its pool.

Persistent volumes that stay pending point the same way: the CSI plugin's controller uses the same address and token.

The cloud controller manager logs `failed to get HA group for the node` with `403 Permission check failed (/, Sys.Audit)` for every node now and then. That is expected and harmless: reading Proxmox VE's HA groups needs `Sys.Audit` on `/`, which would let the cluster's token read the whole Proxmox VE cluster's configuration, so Kubehenge does not grant it, and the nodes only go without the HA group labels.

### A cluster waits before any VM exists

The cluster's conditions say it is waiting for its Proxmox VE pool, user and token: Kubehenge's controller has not finished creating them. Its log says why, often a privilege missing from the controller's token after Proxmox VE objects were changed by hand. Applying the management cluster again puts the module's users, roles and ACLs back.

The first cluster of a distribution also waits while that distribution's Cluster API controllers start ([architecture.md](architecture.md#management-cluster)). Until then its conditions say the ClusterClass is not successfully reconciled. This normally takes a minute or so. If it lasts longer, look at the Deployments in that distribution's provider namespaces (`rke2-bootstrap-system` and `rke2-control-plane-system` for RKE2, and likewise for kubeadm and k3s), and at Kubehenge's controller log, which records each scaling.

### Ranges refused

A message saying "the pod range … overlaps the nodes' network …" (or the service range): pod and service ranges must stay clear of the nodes' network (the gateway with the prefix length) and of each other. Choose other ranges under **Pod and service ranges**, or in `spec.clusterNetwork`.

### Control planes too small

A message saying "the control planes of a cluster with … workers need at least …": the cluster has more workers than its control planes' size carries, by its distribution's sizing guide ([clusters.md](clusters.md#machines)). Give the control planes the cores and memory the message names (`controlPlaneMachine`, or the control plane's override), or keep fewer workers. "The control planes of a cluster without workers run its workloads too and need at least …": with no workers, or every worker pool scaled to 0, the control planes need a worker's least on top of their own; give them that, or keep a worker. A cluster that already fell short, after an upgrade of Kubehenge asked for more, can still be changed in other ways; only more workers or smaller control planes are refused.

### One control plane, unreachable after a roll

A cluster with one control plane runs on two etcd members while that node is replaced, by an upgrade or any change to the control planes, and each member then needs the other ([architecture.md](architecture.md#clusterclasses)). If the API stops answering during the roll and does not come back, one of the two is not running, and nothing in Cluster API recovers that by itself.

- Find which: on both control-plane VMs (the old node and its successor), look at the distribution's service (`systemctl status k3s` or `rke2-server`, `journalctl -u k3s`; kubeadm's etcd and API server are static pods, `crictl ps -a`), and at the kernel's log for a process killed for memory (`dmesg`). Start whatever stopped, or the VM. Once both members run again, the cluster recovers and the roll goes on.
- If one of them cannot come back, its VM or disk lost: stop the service on the other, reset its etcd to itself alone (`k3s server --cluster-reset` or `rke2 server --cluster-reset`, as the distribution's documentation describes), and start the service again. Then delete the Machine of the lost node in the management cluster, and Cluster API replaces it.
- Three control planes keep a majority through the loss of one, rolls included.

### Not enough memory for the add-ons

A message saying "the cluster's add-ons (…) need … MiB of memory on its workers, beyond the … MiB a worker uses itself": the workers together lack the memory the add-ons named take ([clusters.md](clusters.md#machines)). Give the workers more memory (`workerMachine`, or a worker pool's override), add workers, or leave an add-on off. Rancher's agent counts in every cluster that registers with Rancher; set `rancher` to `{register: false}` for a cluster that never will. "… on its control planes, which run them without workers …": the same for the control planes of a cluster without workers. A cluster that already fell short, after an upgrade of Kubehenge asked for more, can still be changed in other ways; only changes that leave it shorter are refused.

### Istio

- **Pods in the mesh reach nothing, or fail their probes, under a NetworkPolicy:** ztunnel carries the enrolled pods' traffic on port 15008/TCP, and the kubelet's probes reach them from 169.254.7.127; a policy that denies by default has to allow both ([Istio's notes](https://istio.io/latest/docs/ambient/usage/networkpolicy/)).
- **Switching Istio off takes long:** Kubehenge waits until no pod's traffic is redirected through ztunnel any more (the pod annotation `ambient.istio.io/redirection`), which needs the cluster reachable and istio-cni running on the pod's node. A pod whose node is gone keeps the annotation; delete the pod.
- **Waypoints are not created:** they need the Gateway API's CRDs, which every cluster has once the annotation `kubehenge.dev/gateway-api` is on its Cluster ([clusters.md](clusters.md#routes-through-the-gateway-api)).

### Cluster Mesh

- **A peer shows "not linked":** the reason beside it, and the cluster's `ClusterMeshNotLinked` events, say what is missing: the peer must run Cilium, have the mesh on, list this cluster, have a Cilium cluster id and an API address, and its pod range must overlap neither this cluster's pods nor its services. Two clusters given overlapping ranges by hand cannot be linked, since a range is fixed once a cluster exists.
- **Linked, but pods do not reach the peer's:** `cilium clustermesh status` in either cluster names the peer it cannot reach. The peers' KVStoreMesh connects to port 32379/TCP of the other's API address; a firewall between the clusters has to let it through. Cilium's network policies apply across the mesh: a policy that denies by default has to allow the peer's pods.

### Rancher

- **"cluster … already runs Rancher, with which every cluster registers":** only one cluster may install it; the others register with it. The interface refuses a second one when it is created, with "Rancher: cluster … already runs it, and every cluster registers with that one". A second cluster set to install it with kubectl gets no Rancher and a `RancherRefused` event (`kubectl describe cluster <name>`).
- **"the Rancher release Kubehenge installs runs on Kubernetes 1.x to 1.y, not …":** a new Rancher cluster at another version is created but gets no VMs; delete it and create it again at a version within the range. An existing cluster outside the range gets no Rancher, with a `RancherRefused` event. The Rancher cluster's upgrades beyond the range wait for a Kubehenge release with a newer Rancher.
- **"every node of the cluster Rancher runs on needs at least 4 vCPUs":** the cluster's control planes or a worker pool with nodes have fewer cores than Rancher needs; give them 4 or more (`controlPlaneMachine` and `workerMachine`, or a worker pool's override). A worker pool scaled to 0 is not counted.
- **Removing Rancher takes long:** it waits until Rancher has let go of every registered cluster, which needs each of them reachable; the Rancher cluster shows `RemovingRancher` events meanwhile, and everything else about it is still applied.
- **Clusters do not register:** Rancher's host name must resolve, for people and for every registered cluster, to the Rancher cluster's ingress controller, which answers on the cluster's API address and on every node.
- **Rancher's agent stays after a cluster leaves Rancher:** Rancher removes it with a `cattle-cleanup-…` job in the `default` namespace, whose pod does not meet `restricted` ([rancher/rancher#58081](https://github.com/rancher/rancher/issues/58081)). In a cluster that holds `default` to `restricted`, the job shows `FailedCreate` events naming the settings it lacks, and keeps trying. Let it run by holding `default` to `baseline` until the job has finished: with Pod Security Admission, `kubectl label namespace default pod-security.kubernetes.io/enforce=baseline`, and afterwards `kubectl label namespace default pod-security.kubernetes.io/enforce-`; with Kyverno, set its level to `baseline` under **Pod security** and back afterwards. A cluster being deleted needs none of this, since its VMs go with the agent.

### A cluster does not finish deleting

- Kubehenge's controller waits while a Proxmox VE node that may hold some of the cluster's volumes on local storage is offline. Bring the node back.
- A cluster registered with Rancher leaves it first, while its VMs still run; the conditions say so while that is going on.
- The controller's log says what it is waiting for, such as machines not yet deleted.

### Appliance upgrades

- **Newer releases shows "unknown":** the appliance could not fetch the release index from GitHub, or the index's signature did not check out. The reason follows.
- **The last upgrade failed:** the reason is on the Administration page, and in `journalctl -u kubehenge-update` inside the appliance. Unless the reason says the release was installed but restarting failed, the release installed before keeps running.
- **The interface does not come back** after an upgrade: see `journalctl -u kubehenge`, and upgrade or reinstall the release from the shell with `install.sh --upgrade` ([upgrading.md](upgrading.md#from-the-proxmox-ve-shell)).

### The interface is unreachable

- Check the container runs (`pct status <ctid>`) and the service is active (`pct exec <ctid> -- systemctl status kubehenge`).
- The appliance's certificate and its sign-in redirects name the address (or `--name`) it was given. If it took another address from DHCP, give it back that one, or make a certificate for the new one ([install.md](install.md#the-appliances-certificate)).
- **The browser says the certificate expired:** renew it with `install.sh --certificate --ctid <ctid>` ([install.md](install.md#the-appliances-certificate)). The interface warns administrators 30 days ahead.
- The interface keeps working while the management cluster is unwell; pages that need it say the management cluster did not answer. Use **Repair access**, or apply the management cluster again from the Administration page.

## Reporting a problem

Report bugs on GitHub with what you did, what happened, and the relevant lines of the logs above. Report vulnerabilities privately, as `SECURITY.md` describes.
