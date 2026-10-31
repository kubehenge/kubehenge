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

- **An error's reference:** every answer of the interface carries a request id in its `X-Request-Id` header, and an error shows it under its message as "Reference: …". Every line the interface logs for that request carries it as `requestId`: `pct exec <ctid> -- journalctl -u kubehenge --grep <reference>`.
- **More in the journal:** add `KUBEHENGE_LOG_LEVEL=debug` to `/etc/kubehenge/appliance.env` and restart `kubehenge`; `warn` or `error` log less. `KUBEHENGE_LOG_DEVELOPMENT=true` adds to each line where in the code it was logged. Upgrades keep both. The same are the `--log-level` and `--log-development` flags of `kubehenge serve`.
- **Whether it answers:** `https://<appliance>/healthz` answers `ok` to anyone, signed in or not, and says nothing more; a monitor can watch it.

- **The last run of the management cluster** shows on the Administration page, with its error and log when it failed, and stays there after a restart; one cut off by a restart says so.
- **The last template build** shows under VM templates on the same page, kept the same way.
- **The last weekly template build** shows below it under Automatic builds, with its log, from the ConfigMap `kubehenge-template-refresh` in the management cluster's `kubehenge-system`; the run itself is a Job of the CronJob of that name, whose pod's log has the whole of it: `kubectl -n kubehenge-system logs job/<name>`. A run that reads "no API token" found no Secret `kubehenge-template-builder` with the builder's token: the module makes it, and a management cluster installed from the chart alone needs it made, or `templateRefresh.enabled` set to false.
- **The last upgrade** shows on the Administration page, and with `kubehenge appliance upgrades` in the appliance's shell ([upgrading.md](upgrading.md#from-the-appliances-shell)); the updater keeps it in `/var/lib/kubehenge-update/status.json`.
- **Whether the interface serves:** `pct exec <ctid> -- kubehenge appliance health` runs the check the upgrades use: an answer on the appliance's certificate within 2 minutes, then 20 seconds without a restart.

### The management cluster

**Repair access**, under **Administration**, **Appliance**, hands administrators the management cluster's own credentials, for what the interface cannot do. It needs the state passphrase and the [setup privileges](install.md#who-may-set-it-up), is refused while a run is going on, and every download is logged.

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

The controller logs at the chart's `controllerLogs.level` (`debug`, `info`, `warn` or `error`), and with `controllerLogs.development` adds stack traces from warnings on; in JSON either way.

Its metrics are served over HTTPS by the Service `kubehenge-controller-metrics` in `kubehenge-system`, port 8443, to whoever the API server lets get `/metrics`: bind the ClusterRole `kubehenge-metrics-reader` to a scraper's account. Beside controller-runtime's own (reconciles, their errors and durations, work queues), `kubehenge_hook_waits_total`, `kubehenge_hook_refusals_total` and `kubehenge_hook_duration_seconds`, by hook, say how often Kubehenge's Cluster API hooks held a cluster back or refused it, and how long they took. The management cluster runs no scraper of its own. The controller serves them with its Runtime Extension certificate, made for `kubehenge-controller-metrics.kubehenge-system.svc` too, whose CA is `ca.crt` in the Secret `kubehenge-extension-tls`. To read them once, with the admin kubeconfig, through an account that holds the role:

```sh
kubectl -n kubehenge-system create serviceaccount metrics-reader
kubectl create clusterrolebinding metrics-reader --clusterrole kubehenge-metrics-reader --serviceaccount kubehenge-system:metrics-reader
kubectl -n kubehenge-system get secret kubehenge-extension-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > metrics-ca.crt
kubectl -n kubehenge-system port-forward service/kubehenge-controller-metrics 8443 &
curl -sS --cacert metrics-ca.crt --resolve kubehenge-controller-metrics.kubehenge-system.svc:8443:127.0.0.1 \
  -H "Authorization: Bearer $(kubectl -n kubehenge-system create token metrics-reader)" \
  https://kubehenge-controller-metrics.kubehenge-system.svc:8443/metrics
```

### Cluster API objects

A cluster's objects are in its namespace, `clusters` for those created in the interface unless another of its namespaces was chosen:

```sh
kubectl -n clusters get clusters,machines,proxmoxmachines
kubectl -n clusters describe cluster prod
kubectl -n clusters get helmchartproxies,helmreleaseproxies
```

- **The Cluster's conditions**, also on the cluster's page, say what holds it up, including what Kubehenge's hooks are waiting for. Two are Kubehenge's own, shown above the views of the cluster's page and counted among those that need you:
  - `KubehengeReady` is `False` with the error the controller last met on the cluster, until a reconcile succeeds; each error is also a Warning event `ReconcileFailed` on the Cluster, repeats counted in one. A Proxmox VE task that failed is named by its UPID, as the node's task log lists it, with the last lines of its log.
  - `MachinesStalled` is `True` when a machine is not ready and none has become ready for 20 minutes, with the worst its Machines and ProxmoxMachines report, such as `VM template not found`; it comes with a Warning event `MachinesStalled`. A change or creation in the interface then shows as stalled under **Runs**.
- **The Cluster's events** (`kubectl -n clusters describe cluster prod`) record what Kubehenge's controller did, held back or could not do, and why; the cluster's **Health** view lists the warnings among them. Each feature's events are under its problem below.
- **Machines and ProxmoxMachines** say, per node, whether the VM was cloned, started and joined.
- **HelmChartProxies and HelmReleaseProxies** are the add-ons: the cloud controller manager, the CSI plugin, a CNI that Kubehenge installs, Headlamp and Rancher.
- **The Secret `<name>-proxmox-token`** holds what the controller made for the cluster on Proxmox VE; its `ready` key turns `true` once all of it exists.

### Inside a cluster

The cluster's **Health** view in the interface is the first place to look: it lists what needs attention, most serious first. When it says Kubehenge's read-only access has not been set up, or ran out, its controller has not reached the cluster's API server; the controller's log names the cluster and the reason (`workload-identities`).

With the cluster's admin kubeconfig ([clusters.md](clusters.md#download-a-kubeconfig)): the cloud controller manager and the CSI plugin's controller run in `kube-system`, on the control-plane nodes.

```sh
kubectl --kubeconfig prod.kubeconfig -n kube-system get pods -o wide
kubectl --kubeconfig prod.kubeconfig get nodes -o wide
```

### A node's boot log

When a machine's VM was cloned and started but its node never joined, its boot log says why: **Boot log** on the machine in the cluster's **Machines** view, for those with `Sys.Modify` on the cluster's pool. The controller reads it and keeps it in the ConfigMap `<cluster>-boot-logs` beside the Cluster, one key per machine, so it is there for kubectl too; asking adds the machine to the Cluster's `kubehenge.dev/boot-log-request` annotation, which the controller takes the name off once the log is written:

```sh
kubectl -n clusters annotate cluster prod kubehenge.dev/boot-log-request=prod-cp-abcde --overwrite
kubectl -n clusters get configmap prod-boot-logs -o jsonpath='{.data.prod-cp-abcde}'
```

- **kubeadm, RKE2 and k3s:** the end of `/var/log/cloud-init-output.log`, and of the journals of cloud-init, `containerd`, `kubelet`, `rke2-server`, `rke2-agent`, `k3s` and `k3s-agent`, whichever the node has, from the copy the node keeps of them in `/var/log/kubehenge/boot.log` from boot on, a few seconds old at most. The controller reads both files through the VM's QEMU guest agent with its own token, which holds `VM.GuestAgent.FileRead` on the workload pool for it: files only, no commands, and only those the node's agent policy opens. With `node_boot_logs` off ([install.md](install.md#settings)) the log says it is switched off. A VM that is off, or whose guest agent does not answer, is named as such; a node built from a template older than the copy has cloud-init's output alone.
- **Talos:** the kernel messages and the logs of `machined`, `apid`, `kubelet` and, on control planes, `trustd` and `etcd`, read through the node's Talos API with the cluster's talosconfig. A node that has no address yet, or never took its configuration, cannot be reached that way; its VM's console in Proxmox VE shows its boot.

Tokens and passwords in the lines are replaced by `[REDACTED]`.

### A cluster's support bundle

A support bundle is one `.tar.gz` with what helps find why a cluster is stuck, to look through or to attach to an issue. **Support bundle** on the cluster's page downloads it, for those with `Sys.Modify` on the cluster's pool; with the management cluster's kubeconfig ([Repair access](#the-management-cluster)), `kubehenge diagnose` writes the same one:

```sh
KUBECONFIG=management.kubeconfig kubehenge diagnose clusters/prod
```

- `objects/`: the Cluster and every object of Cluster API and its providers that belongs to it (machines, their VMs, bootstrap configurations and control plane, IP addresses, add-on releases), never a Secret.
- `events.yaml`: their events, oldest first.
- `boot-logs.yaml`: the boot logs read for its machines, if any were asked for.
- `logs/`: the lines of the recent logs of Kubehenge's controller, capmox and the Add-on Provider for Helm that name one of its objects, and, when written on the appliance (from the interface, or `pct exec <ctid> -- env KUBECONFIG=/var/lib/kubehenge/interface.kubeconfig kubehenge diagnose clusters/prod -o /root/prod.tar.gz`), those of the interface's journal.
- `manifest.txt`: what it holds, and what could not be read and why.

Fields and lines that hold tokens, passwords, keys or a talosconfig are replaced by `[REDACTED]`. A line of a log is kept only when it names one of the cluster's objects exactly, so the bundle tells nothing of other clusters; still read it through before you share it.

### Proxmox VE

The task log shows what each Kubehenge account did. In an appliance's management cluster, Cluster API acts as `kubehenge-cluster-api@pve` and Kubehenge's controller as `kubehenge-provisioner@pve`; each cluster's own user is named after its workload pool, namespace and name, such as `kubehenge.clusters.prod@pve`.

## Common problems

### Installation fails

`install.sh` stops at the first problem and says what it was. Up to starting the interface, it then removes the container it created ("removing container … after the failure"), so running it again starts clean.

- **"this is not a Proxmox VE node"** or **"Kubehenge needs Proxmox VE 9":** run it as root in the shell of a Proxmox VE 9 node.
- **"no active storage here takes container disks (content rootdir); name one with --storage":** allow the `rootdir` content type on a storage of the node ([install.md](install.md#storage)), or name one with `--storage`.
- **"could not download …":** the node has to reach GitHub, for Kubehenge's release and OpenTofu ([install.md](install.md#internet-access)). The installer has already tried the download five more times.
- **"… does not match its checksum":** the file is not the one this `install.sh` names, and nothing of it was installed. Check that nothing between the node and GitHub changes downloads, and fetch `install.sh` again from the release you mean to install; with `--release <file>`, check the file and the checksum given with `--sha256`.
- **The Proxmox VE API's certificate:** the installer stops before it creates anything when that certificate is not valid for the URL, or rests on a CA it cannot vouch for. The message names what to give: `--proxmox-url`, or the CA with `--proxmox-ca` ([install.md](install.md#the-proxmox-ve-api)).
- **"container … got no IPv4 address":** with `--ip dhcp`, no DHCP server on the bridge answered within two minutes. Give a static `--ip` with `--gateway`, or check the bridge (`--bridge`).
- **"the service did not start; its log follows":** the interface stopped right after it started. The installer prints the interface's log, then removes the container; the log's last lines say why. If they don't help, [report the problem](#reporting-a-problem) with the installer's output.
- **"the appliance cannot use Proxmox VE's API at …; give a URL it reaches, whose certificate names it, with --proxmox-url":** the interface runs, but got no answer from the API within about 20 seconds; the installer prints the interface's log before it removes the container. The container has to reach that name or address on port 8006 from its own network, and resolve the name through its name servers (`--dns`). Run the installer again with a `--proxmox-url` it reaches.

### Sign-in and permissions

- **"Not set up yet"** for an administrator: the user lacks one of the [setup privileges](install.md#who-may-set-it-up) on `/`.
- **A cluster is missing from the list:** the user lacks `VM.Audit` on its pool. A role given on the cluster's own pool replaces, for that pool, what the user inherits from the workload pool, so it has to hold `VM.Audit` too.
- **"`<user>` needs `<privilege>` on `<path>`"** names exactly what to grant ([clusters.md](clusters.md#who-may-do-what)).
- **Buttons are disabled or missing:** Scale, Upgrade, Delete and the **Edit** buttons and switches under Options need `VM.Allocate` on the cluster's pool, and the page says so when it is missing; installing Rancher, offered when a cluster is created, needs `Sys.Modify` on the workload pool, and adding or changing identity providers needs it on the workload pool when creating and on the cluster's pool when changing; **Kubeconfig** needs `Sys.Modify` there and shows once the cluster is available. Clusters outside the interface's namespaces are read-only there.
- **No bridges to choose from:** Proxmox VE lists a node's bridges only with `Sys.Audit` on the node; type the bridge's name instead. A bridge marked "no SDN.Use" needs that privilege on `/sdn/zones/localnetwork/<bridge>`.
- **"Session ended. Sign in again.":** Proxmox VE no longer accepts the session; sign in again. Sessions last eight hours.
- **Sign-in refused after several tries:** five failures within 15 minutes, for a user or from an address, lock further attempts for a while.
- **"Security keys work only in Proxmox VE":** a WebAuthn key answers only on Proxmox VE's own page. Sign in with a TOTP code or recovery key, or through an OpenID realm.
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

- RKE2 and k3s need a build of their distribution's template on the cluster's node OS. There is one template per distribution and node OS, so every node OS the clusters' `nodeOS` names needs its own (**Build template** on the Administration page, Distribution: RKE2 or k3s, with that Node OS, or `kubehenge template create rke2 --node-os <node OS>`, `almalinux-10` unless set).
- kubeadm needs a build of the template of the cluster's exact Kubernetes version on its node OS, for creating a cluster and for upgrading one (`kubehenge template create kubeadm --kubernetes-version <version> --node-os <node OS>`).
- Talos needs the template of the release in the cluster's `kubehenge.dev/talos-image` annotation (**Build template**, Distribution: Talos, or `kubehenge template create talos --talos-version <release>`); the weekly build keeps the template of every release a cluster boots or is upgraded to, so one missing there was deleted by hand or never made.
- A cluster's machines clone the build its node image time picks ([templates.md](templates.md#how-clusters-find-them)); with that build deleted they take the one before it, or the oldest left, and with none left of the line, none.
- The template has to be in the template pool, `kubehenge-templates`, the only one Cluster API may clone from.
- Two templates with the same tags are no match either.
- A template on storage local to one node can only be cloned there; put templates on shared storage.

`kubehenge template list` shows what the template pool holds, with each template's tags, and `kubehenge template list --pool ''` every pool's, which shows a template that was made elsewhere ([templates.md](templates.md)).

### Template build fails

The error and the run's log are under VM templates on the Administration page, those of the weekly build under Automatic builds; `kubehenge template create` prints them. A failed run removes the VM it made, unless `--keep-on-failure` kept it for inspection.

A build from the interface or the weekly build is tried three times, 2 and then 8 minutes apart, each failure in the log, unless another try would meet the same failure: a VM already of that name, a CPU too old, or a key other than the known one. A weekly build that still fails records a `TemplateBuildFailed` warning event on the ConfigMap `kubehenge-template-refresh`, with the error: `kubectl -n kubehenge-system get events --field-selector reason=TemplateBuildFailed`.

- **"VM … on … is already named …: another run is building it, or a failed run left it; wait, or remove it":** a build of that template is under way, or a VM was left by one kept with `--keep-on-failure` or cut off before it could clean up. Once no build runs, delete that VM, which is not a template yet, and build again. The weekly build deletes such a VM itself once it is three hours old.
- **"node …'s CPU reaches x86-64-v…, and … needs x86-64-v…: build its templates on another node":** the node OS needs a newer CPU level than that node's ([templates.md](templates.md#node-oses)). Build on a node whose CPU reaches it.
- **"no storage on … takes ISO images":** the build VM's cloud-init data comes on an ISO. Allow the `iso` content type on a storage of that node, or choose one (**Cloud-init image** in the dialog, `--iso-storage` on the command line).
- **The downloaded image:** the storage under **Downloaded image** (`--import-storage`) must allow the `import` content type ([templates.md](templates.md#requirements)). Proxmox VE downloads the image itself and checks it against its checksum; "task … failed" names its reason, and the task's log is under the node's **Task History**.
- **"not signed by …'s key"**, **"… serves a key other than …"** or **"… is not listed in its checksum file":** the publisher's checksum file, or a package repository's key, is not the one Kubehenge knows by its fingerprint, and nothing was imported. Check that nothing between the appliance and the publisher changes downloads, and try again later; a publisher that moved to a new key needs a Kubehenge release that carries it.
- **A command in the build VM failed**, such as the package manager, with the end of the build's log below the error: the VM has to reach its node OS's package mirrors and the other addresses the build needs ([install.md](install.md#internet-access)), by DHCP on its bridge or the static address given for it. A package whose signature does not check out stops the build the same way.

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

The cloud controller manager logs `failed to get HA group for the node` with `403 Permission check failed (/, Sys.Audit)` for every node now and then. That is harmless: reading Proxmox VE's HA groups needs `Sys.Audit` on `/`, which a cluster's token holds only with **Add HA group labels** on (the `ccm` variable's `haGroups`, on by default, [clusters.md](clusters.md#machines)), and without it the nodes only go without the HA group labels. The cluster switched it off, or the administrator switched off **HA group labels** in the setup for every cluster ([install.md](install-settings.md#ha-group-labels)). With it on again, the messages stop within a few minutes; if they do not, `kubectl describe cluster` shows a `SysAuditRefused` event saying why.

### A cluster waits before any VM exists

The cluster's conditions say it is waiting for its Proxmox VE pool, user and token: Kubehenge's controller has not finished creating them. Its log says why, often a privilege missing from the controller's token after Proxmox VE objects were changed by hand. Applying the management cluster again puts the module's users, roles and ACLs back.

The first cluster of a distribution also waits while that distribution's Cluster API controllers start ([architecture.md](architecture.md#management-cluster)). Until then its conditions say the ClusterClass is not successfully reconciled. This normally takes a minute or so. If it lasts longer, look at the Deployments in that distribution's provider namespaces (`rke2-bootstrap-system` and `rke2-control-plane-system` for RKE2, and likewise for kubeadm and k3s), and at Kubehenge's controller log, which records each scaling.

### A cluster applied with kubectl is refused

Kubehenge checks a Cluster made with kubectl or GitOps by the same rules as the interface, in two places. Each refusal names the rule, in the words the interface uses; the problems below explain the common ones.

- **When it is applied:** Kubehenge's validating webhook, `clusters.kubehenge.dev`, refuses a Cluster, or a change to one, that breaks a rule it can check from the Cluster and its class alone. kubectl prints the refusal with the field, such as `spec.topology.variables[backups]`, and the rule. Nothing is stored; correct the Cluster and apply it again.
- **Before its first VM:** Cluster API asks Kubehenge's controller (its `BeforeClusterCreate` hook), which also checks what needs Proxmox VE or the other clusters: a node OS Kubehenge builds templates of, and a node whose CPU runs it, the pod and service ranges, the load-balancer addresses, Rancher, `Datastore.Allocate`, volume snapshots and `Sys.Audit` on `/`. kubectl accepts the Cluster, which then gets no VMs: its `TopologyReconciled` condition, on the cluster's page or with `kubectl describe cluster`, is false with a message that ends in `got failure response with message:` and the rule. Correct the Cluster; Cluster API asks again by itself.

### Ranges refused

A message saying "pod range … overlaps the nodes' network …" (or the service range): pod and service ranges must stay clear of the nodes' network (the gateway with the prefix length) and of each other. Choose other ranges under **Pod and service ranges**, or in `spec.clusterNetwork`. A cluster that names none gets them from Kubehenge's mutating webhook, `cluster-defaults.kubehenge.dev`; "every pod range Kubehenge tries overlaps …" means none of the ranges it tries is clear of the nodes' network (or, for a Cilium cluster, of the other clusters' ranges), so choose them yourself.

### Control planes too small

A message saying "with … workers, control planes need … vCPUs and … MiB each, not …": the cluster has more workers than its control planes' size carries, by its distribution's sizing guide ([clusters.md](clusters.md#machines)). Give the control planes the cores and memory the message names (`controlPlaneMachine`, or the control plane's override), or keep fewer workers. "Without workers, control planes need …": with no workers, or every worker pool scaled to 0, the control planes need a worker's least on top of their own; give them that, or keep a worker. A cluster that already fell short, after an upgrade of Kubehenge asked for more, can still be changed in other ways; only more workers or smaller control planes are refused.

### One control plane, its roll stalled

A cluster with one control plane runs on two etcd members while that node is replaced, by an upgrade or any change to the control planes, and each member then needs the other ([architecture.md](architecture.md#clusterclasses)). The plan before such a change, and kubectl through the admission webhook, warn of it. Nothing in Cluster API recovers a roll that stalls, so Kubehenge's controller watches for it: once the new machine has not been ready for 20 minutes (the module's `watch.stall_minutes`), the Cluster gets the condition `ControlPlaneRollStalled` and a warning event of the same name, the cluster's page shows it with **Recover…**, and its Health page lists it. The reason `APIUnreachable` means the API has stopped answering, `SuccessorNotReady` that it still answers. The message names both machines, why the new one is not ready, as Cluster API says it, and the kubectl command that recovers.

- Find what holds the new machine up: on both control-plane VMs (the old node and its successor), look at the distribution's service (`systemctl status k3s` or `rke2-server`, `journalctl -u k3s`; kubeadm's etcd and API server are static pods, `crictl ps -a`; Talos through `talosctl service etcd`), and at the kernel's log for a process killed for memory (`dmesg`). Start whatever stopped, or the VM. Once both members run again, the roll goes on, the condition goes and an event `ControlPlaneRollResumed` says so.
- If one of them cannot come back: **Recover…** keeps one control-plane machine, by default the one the roll replaces, and replaces the others. It takes the cluster's name typed and Sys.Modify on the cluster's pool. When the API still answers, etcd has its majority and the other machines are only deleted, which removes their members. When it does not, the controller pauses the cluster, stops the control plane on every machine through the guest agent, resets etcd on the one kept to a cluster of one from its own database (`--cluster-reset` on RKE2 and k3s, `etcdutl snapshot restore` of its own database on kubeadm, and on Talos a copy taken through the Talos API before the node's etcd is wiped and bootstrapped from it), starts it, deletes the others and unpauses the cluster. Whatever only the others held is gone. The same through kubectl: `kubectl annotate cluster <name> kubehenge.dev/etcd-restore='{"machine":"<machine to keep>","at":"<now, RFC 3339>"}'`; the status follows in `kubehenge.dev/etcd-restore-status` and the cluster's events (`ControlPlaneRecovered`, `ControlPlaneRecoveryFailed`). A management cluster that does not restore through the guest agent (`etcd_restore`) shows the steps to do it by hand instead.
- Three control planes keep a majority through the loss of one, rolls included.

### Not enough memory for the add-ons

A message saying "add-ons (…) need … MiB of memory on workers, which have … MiB for them": the workers together lack the memory the add-ons named take ([clusters.md](clusters.md#machines)). Give the workers more memory (`workerMachine`, or a worker pool's override), add workers, or leave an add-on off. Rancher's agent counts in every cluster that registers with Rancher; set `rancher` to `{register: false}` for a cluster that never will. "… on control planes without workers …": the same for the control planes of a cluster without workers. A cluster that already fell short, after an upgrade of Kubehenge asked for more, can still be changed in other ways; only changes that leave it shorter are refused.

### Not enough CPU for the add-ons

A message saying "add-ons (…) need … millicores of CPU on workers, which have … millicores for them": the CPU the add-ons' pods request does not fit in what the workers' cores leave beside their own pods' ([clusters.md](clusters.md#machines)). Kubernetes would leave the pods it finds no room for pending, with `Insufficient cpu` in their events, however idle the nodes are. Give the workers more cores (`workerMachine`, or a worker pool's override), add workers, or leave an add-on off; Kyverno and Istio ask for the most. The control-plane form of the message, and a cluster that already fell short, are as for memory above.

### Istio

- **Pods in the mesh reach nothing, or fail their probes, under a NetworkPolicy:** ztunnel carries the enrolled pods' traffic on port 15008/TCP, and the kubelet's probes reach them from 169.254.7.127; a policy that denies by default has to allow both ([Istio's notes](https://istio.io/latest/docs/ambient/usage/networkpolicy/)).
- **Switching Istio off takes long:** Kubehenge waits until no pod's traffic is redirected through ztunnel any more (the pod annotation `ambient.istio.io/redirection`), which needs the cluster reachable and istio-cni running on the pod's node. A pod whose node is gone keeps the annotation; delete the pod.
- **Waypoints are not created:** they need the Gateway API's CRDs, which every cluster has once the annotation `kubehenge.dev/gateway-api` is on its Cluster ([clusters.md](clusters.md#routes-through-the-gateway-api)).

### Cluster Mesh

- **A peer shows "Not linked":** the reason beside it, and the cluster's `ClusterMeshNotLinked` events, say what is missing: the peer must run Cilium, have the mesh on, list this cluster, have a Cilium cluster id and an API address, and its pod range must overlap neither this cluster's pods nor its services. Two clusters given overlapping ranges by hand cannot be linked, since a range is fixed once a cluster exists.
- **Linked, but pods do not reach the peer's:** `cilium clustermesh status` in either cluster names the peer it cannot reach. The peers' KVStoreMesh connects to port 32379/TCP of the other's API address; a firewall between the clusters has to let it through. Cilium's network policies apply across the mesh: a policy that denies by default has to allow the peer's pods.

### Rancher

- **"cluster … already runs Rancher, with which every cluster registers":** only one cluster may install it; the others register with it. The interface refuses a second one when it is created, with "Rancher: cluster … already runs it, and every cluster registers with that one". A second cluster set to install it with kubectl gets no Rancher and a `RancherRefused` event (`kubectl describe cluster <name>`).
- **"for Rancher, choose Kubernetes 1.x to 1.y, not …":** the Rancher release Kubehenge installs runs only on those versions; a new Rancher cluster at another version is created but gets no VMs; delete it and create it again at a version within the range. An existing cluster outside the range gets no Rancher, with a `RancherRefused` event. The Rancher cluster's upgrades beyond the range wait for a Kubehenge release with a newer Rancher.
- **"every node needs at least 4 vCPUs for Rancher: …":** the cluster's control planes or a worker pool with nodes have fewer cores than Rancher needs; give them 4 or more (`controlPlaneMachine` and `workerMachine`, or a worker pool's override). A worker pool scaled to 0 is not counted.
- **Removing Rancher takes long:** it waits until Rancher has let go of every registered cluster, which needs each of them reachable; the Rancher cluster shows `RemovingRancher` events meanwhile, and everything else about it is still applied.
- **Clusters do not register:** Rancher's host name must resolve, for people and for every registered cluster, to the Rancher cluster's ingress controller, which answers on the cluster's API address and on every node.
- **Rancher's agent stays after a cluster leaves Rancher:** Rancher removes it with a `cattle-cleanup-…` job in the `default` namespace, whose pod does not meet `restricted` ([rancher/rancher#58081](https://github.com/rancher/rancher/issues/58081)). In a cluster that holds `default` to `restricted`, the job shows `FailedCreate` events naming the settings it lacks, and keeps trying. Let it run by holding `default` to `baseline` until the job has finished: with Pod Security Admission, `kubectl label namespace default pod-security.kubernetes.io/enforce=baseline`, and afterwards `kubectl label namespace default pod-security.kubernetes.io/enforce-`; with Kyverno, set its level to `baseline` under **Pod security** and back afterwards. A cluster being deleted needs none of this, since its VMs go with the agent.

### Volume snapshots

- **The option is missing:** the cluster's ClusterClass has no `csi` variable.
- **The option is refused:** the cluster's storage is shared or of a type the CSI plugin does not copy on; the option says which.
- **A VolumeSnapshot or a restored or cloned claim stays pending:** `kubectl describe volumesnapshotcontent`, or the claim's events, show the CSI plugin's error, and the proxy's own: `kubectl -n kube-system get events --field-selector reason=VolumeCopyFailed` and the log of the `kubehenge-csi-proxy` container in the plugin's controller pod (`copying a volume`, `copied a volume`, `refused a volume copy`, `a volume copy failed`). `kubectl -n kubehenge-volume-copies get configmaps -o yaml` shows each copy's record: its `state`, the holder VM and node, Proxmox VE's `task` and, once done, the real volume (`copy`).
  - `is not one of the cluster's own` or `is not named after the cluster's volume owner VM`: something in the cluster asked to copy a volume that is not the cluster's; the proxy copies only the volumes the CSI plugin made for the cluster.
  - `cannot land on node`: the plugin asked for a copy on another node than its volume's; a copy stays on its volume's node, so schedule the pod that uses it there.
  - `no holder VM on node … has room for another copy yet`: Kubehenge's controller adds a holder VM on that node within a reconcile; the sidecar asks again.
  - `Permission check failed` naming `VM.Clone`: the cluster's role lacks it; apply the management cluster's module again.
  - A `VolumeSnapshotsRefused` event on the cluster says why Kubehenge took volume snapshots away from it instead.
- **A large volume's snapshot takes long:** it turns ready once Proxmox VE has finished the copy, however long that takes; the record's `task` names the copy's task in the node's **Task History**.
- **`failed to copy disk` with `pvesm export … | /usr/bin/ssh … pvesm import …` and `exit code 255`:** Proxmox VE copies a volume by piping it through SSH as root to the node the copy goes to, even when that is the same node, and the node could not reach itself. On the node, `ssh -o UserKnownHostsFile=/etc/pve/nodes/<node>/ssh_known_hosts -o GlobalKnownHostsFile=none -o BatchMode=yes -o HostKeyAlias=<node> root@<node's address> true` shows why: the SSH server is not running, `root`'s key is not among the keys it lets in, or the node has no SSH host key under its name in that file (when the file is missing, the copy's command has no `UserKnownHostsFile`). `pvecm updatecerts` puts the keys back where Proxmox VE expects them; the task's log under the node's **Task History** shows the copy's full error.
- **A large volume's snapshot or restore fails, or holds less than it should:** Proxmox VE copies the volume while the plugin waits up to 4 minutes; a copy that takes longer can leave a snapshot reported ready before its data is all written ([clusters.md](clusters.md#volume-snapshots)). Delete it and take snapshots of smaller volumes.

### The CSI plugin's newest release

- **A `DatastoreAllocateRefused` event:** the cluster asks for the plugin's newest release, but Kubehenge refuses its token `Datastore.Allocate` on the storage, for the reason the event gives, and the cluster runs the earlier release ([clusters.md](clusters.md#persistent-volumes)). Move whatever else uses the storage, or switch the option off.

### Backups

Kubehenge's controller holds a cluster's backups back, neither installing nor removing Velero, and says why in an event on the Cluster:

- **`BackupTargetMissing`:** "backup target … does not exist": an administrator removed or renamed it. Choose another under the cluster's **Options**, or have an administrator add it again ([install.md](install-settings.md#backup-targets)). What the cluster already runs stays meanwhile.
- **`BackupsInvalid`:** the backup settings are ones Velero would not take, such as a schedule it cannot read; the event says which. Correct them under **Options**, or in the `backups` variable.
- **`BackupCredentialMissing`:** "no credential for the backup target: type one in for the cluster in the interface, or have an administrator store one for every cluster, or name a Secret of the cluster's velero namespace in existingSecret". Do one of those. "Secret … holds no accessKeyID and secretAccessKey": the credential named lacks one or both of its keys; store it again. Backups wait until then.

### etcd snapshots and restores

- **`EtcdSnapshotFailed`**, "etcd snapshot failed: …": no snapshot was taken, for the reason that follows, which the **Backups** view shows as the last try's error too. The controller tries again 15 minutes later. A missing target or credential reads as for backups above. An error from the target means the management cluster does not reach its bucket, or the credential is refused there; one from etcd, that the cluster's API server or its etcd did not answer. A snapshot cut off is never kept.
- **`EtcdSnapshotsNotPruned`**, "deleting the oldest etcd snapshots: …": the snapshot was taken, but the ones beyond the number kept were not deleted. The credential has to be allowed to delete in the target.
- **`EtcdUploadHeld`:** the distribution's own upload ([clusters.md](clusters.md#the-distributions-own-upload)) has no target to upload to, for the reason the event gives, a missing target or credential as for backups above. The servers keep their snapshots on their nodes meanwhile.
- **`EtcdRestoreFailed`**, "restore from …: …": the restore stopped, for the reason that follows, which the **Backups** view shows too. One that failed before etcd was restored started every control plane again as it was; one that failed after finished the restore ([clusters.md](clusters.md#restore-etcd)). "… the control-plane nodes are left as they are: restore by hand as the interface shows": follow the steps **Restore…** lists for the snapshot. "control-plane machine … has no VM yet: restore once every one runs": wait until every control-plane machine has its VM, then restore again.

### HA writes and restore steps

Each Proxmox VE HA write and each step of an etcd restore runs in a Job of its own in `kubehenge-system` ([architecture.md](architecture.md#tokens-kept-out-of-the-controller)), which the controller deletes once it has read the result; `kubectl -n kubehenge-system get jobs,pods -l kubehenge.dev/token-job` shows those running. The controller's log, or `EtcdRestoreFailed`, says what failed:

- **"token Job … ended without a result: …":** the pod never ran its request, for the reason after the colon. `DeadlineExceeded` means it was not scheduled or did not start in time; `kubectl -n kubehenge-system get events` says why, often an image that could not be pulled.
- **"starting Job …: … only a Job made exactly from PodTemplate …":** the Job did not match its template, which happens while an upgrade of the chart replaces it. The controller tries HA writes again with the new one; ask for a restore again.
- **"not one of Kubehenge's HA objects":** a write was refused, as the HA resource or rule it was for, or a resource a rule names, no longer carries the comment `Kubehenge cluster …`. Someone changed it in Proxmox VE: put the comment back, or remove the object and let the controller make it again.

### Talos upgrades

- **`TalosUpgradeFailed`**, "node … to Talos …: …": the node did not take the release, for the reason that follows: "the node came back on Talos …: Talos booted the release before, as the new one did not come up" means Talos rolled back, and "the node is not Ready … after it rebooted" that it did not come back within 15 minutes. The controller upgrades no other node until **Try again** on the cluster's Summary, or until the annotation `kubehenge.dev/talos-upgrade-failed` is removed ([clusters.md](clusters.md#talos-upgrades)). Correct what the reason names first.
- **`TalosInstallRetry`**, "node … to Talos …, try … of 4 at …: …": the installer's pull or install stalled, or the node did not answer; the controller tries again at the time named. After the fourth try it fails as above. Check that the node reaches factory.talos.dev.
- **`TalosUpgradeAbandoned`**, "node … to Talos …: its machine is being deleted": the cluster scaled down or replaced the node while it was upgraded. Nothing to do; its successor is upgraded as any other node.
- **`TalosDrain`**, "node … reboots without all its pods moved: …": the node's pods did not all leave within five minutes, often held by a PodDisruptionBudget, and the node rebooted anyway. Check the budgets of the workloads named.

### GitOps

- **`GitOpsWaiting`**, "GitOps waits for …": the engine syncs nothing until it has what follows. "the deploy key": Kubehenge makes it once the cluster exists. "the repository's token, stored as the cluster's gitops credential": store the user name and token under the cluster's **Options**. For Flux's web interface, "a client secret, which Flux's web interface signs in with" and the like: store the client secret under **Options**, or create the Secret the message names; syncing goes on meanwhile.
- **"knownHosts: Required value"** or **"no host key is for …, the repository's host"**, when saving a cluster: a repository reached over `ssh://` needs its host's keys in `gitops.knownHosts`, as an SSH `known_hosts` file holds them, with a line for the host and port of the URL (`[host]:port` for a port other than 22). Take them from the forge's published keys, or from `ssh-keygen -F <host>` on a machine that already trusts it; `ssh-keyscan` reads them over the network unchecked, so compare its fingerprints with the forge's before using them.

### Certificates

- **`CertificatesExpiring`** on the Cluster, with **Renewing certificates** or **Certificates expire** on its Health page: certificates of the cluster run out within 60 days. The controller looks every 5 minutes at what each distribution reports: the date Cluster API records on kubeadm's control-plane machines, the warnings RKE2's and k3s's own certificate monitor records on each node (`kubectl get events --field-selector reason=CertificateExpirationWarning` in the cluster's `default` namespace), and the client certificates of the kubeconfig and, on Talos, the talosconfig kept beside the Cluster. Talos renews its nodes' certificates itself, and the kubelets of kubeadm and Talos theirs.
  - Reason `Renewing`: the machines holding them are being replaced, which renews them. kubeadm's control plane does so itself; for RKE2 and k3s the controller rolls the workers' pool, rolls k3s's control plane through its `rolloutAfter`, and has an RKE2 control plane of three or more replaced one machine at a time through its health check. An event `RenewingCertificates` names the machines; `CertificatesRenewed` says when nothing runs out within 60 days any more.
  - Reason `RestartNeeded`: an RKE2 control plane of fewer than three machines cannot be replaced one at a time without stopping the cluster. Restart `rke2-server` on the node the message names (`systemctl restart rke2-server`), which renews its certificates, as RKE2 does from 120 days before they run out.
  - Reason `NotRenewed`: they run out within 30 days and nothing renewed them. The message says how for each: replace a machine whose replacement stalled once the rest of the cluster is healthy, look at the control-plane provider's log for a kubeconfig or talosconfig it did not renew, or rotate the cluster's CAs as the distribution documents.
- **`NoDNS01Credential`**, "no DNS-01 credential is stored for the cluster (…): store one, or name an existing Secret": the `acme-dns01` issuer has no credential for the DNS provider, and the cluster's certificates wait. Type it in under the cluster's **Options**, take the one an administrator stored for every cluster (`dns01.sharedCredential`), or name a Secret of your own in `dns01.existingSecret` ([clusters.md](clusters.md#dns-providers)).

### VMs that do not run, storage nearly full

Kubehenge's controller looks every minute, with its own token, at the VMs of every cluster's machines whose node has joined, and at the storage in the cluster's `storage` variable. Both show on the cluster's Health page, and the fleet lists the cluster under what needs you.

- **`VMsNotRunning`** on the Cluster, with **VM disk error**, **VM crashed**, **VM stopped** or **VM paused**: a VM has been out of the running state for 3 minutes, so a backup in suspend mode, a migration or a reboot does not count; neither does a VM Proxmox VE holds locked for a backup, migration, snapshot or clone. The message names each machine, its VM and node. The reason is the worst found:
  - `IOError`: QEMU paused the VM because a disk write failed, most often on a full storage. Free space, or fix the disk, then resume the VM in Proxmox VE (`qm resume <vmid>`).
  - `Panicked`: the guest's kernel crashed. Reset the VM (`qm reset <vmid>`), or delete its Machine to have it replaced.
  - `Stopped`: someone or something stopped the VM. Start it (`qm start <vmid>`), or delete its Machine.
  - `Paused`: the VM was paused or suspended by hand. Resume it.
- **`StorageNearlyFull`**, with **Storage nearly full** (reason `NearlyFull`, 85% and more) or **Storage full** (reason `Full`, 95% and more): the cluster's storage, on a node its VMs run on, has that little space left. A thin storage can run out while every disk on it still has room, and QEMU then pauses each VM whose write finds none. Free space, or add capacity.

A warning event of the condition's name records each start and change; `VMsRunning` and `StorageFreed` record the end.

### Autoscaling

- **`AutoscalerNoRoom`**, "Proxmox VE has no node with the memory for a new worker of pool …: …": the new worker waits for room, and after 30 minutes the autoscaler gives the machine up and tries again. Free memory on the Proxmox VE nodes the cluster may use, or let it use more nodes ([clusters.md](clusters.md#autoscaling)).
- **`AutoscalerNotPlaced`**, "the autoscaler cannot run inside the cluster: Kubehenge was installed without the management cluster's address (managementEndpoint)": the autoscaler is set to run on the cluster's own control planes, which needs that address, and no autoscaler runs meanwhile. Apply the management cluster again, whose module sets it, or set `managementEndpoint` in the chart's values; or have the autoscaler run in the management cluster.

### A cluster does not finish deleting

The cluster's page lists what the deletion waits on: every object of the cluster whose finalizer holds it, with what that object reports. kubectl reads the same from the Cluster's annotation `kubehenge.dev/deletion-status`:

```sh
kubectl -n <namespace> get cluster <name> -o jsonpath='{.metadata.annotations.kubehenge\.dev/deletion-status}' | jq
```

- **Hook BeforeClusterDelete:** a cluster registered with Rancher leaves it first, while its VMs still run. The reason is Rancher's.
- **Machine … Drain not completed:** a node's pods are being evicted, and a PodDisruptionBudget, or a pod that does not stop, holds that up. Delete the pod in the cluster, or abandon the deletion.
- **ProxmoxMachine … (a VM on an offline node):** capmox cannot delete the VM. Bring the node back.
- **Cluster … removing the Proxmox VE objects:** Kubehenge's controller waits while a Proxmox VE node that may hold some of the cluster's volumes on local storage is offline, or Proxmox VE refuses to delete the pool, user or volume owner VM. The reason is Proxmox VE's.

Once the deletion has waited 20 minutes (the module's `watch.stall_minutes`), the Cluster gets the condition `DeletionStalled` and a warning event of the same name, and the fleet and the cluster's Health page list it.

A deletion that can never finish, such as one of a cluster whose nodes are gone for good, is abandoned with **Abandon…** on the cluster's page, which takes VM.Allocate on its pool as deleting does, or from kubectl, with the cluster's name as the value:

```sh
kubectl -n <namespace> annotate cluster <name> kubehenge.dev/abandon-deletion=<name>
```

Kubehenge's controller then has the machines deleted without draining their nodes or waiting for their volumes, deletes everything of the cluster that is not being deleted yet, and lets Cluster API go on without the BeforeClusterDelete hook. What has not finished five minutes later loses its finalizers, the Cluster's own last. What that leaves behind in Proxmox VE, such as a VM on a node that never came back, or the cluster's pool and user, is named in a `LeftBehind` event for each and a `DeletionAbandoned` event, and listed on the cluster's page while it is still there; a registration in Rancher stays as well. Delete those by hand once their node is back. The events stay as long as the management cluster keeps events, an hour by default:

```sh
kubectl -n <namespace> get events --field-selector reason=LeftBehind
```

### Appliance upgrades

- **Newer releases shows "Couldn't check":** the appliance could not fetch the release index from kubehenge.dev, the index's signature did not check out, or it has expired. The reason follows. "the release index has expired": the index it reached was not signed again within two weeks; with a mirror (`KUBEHENGE_RELEASE_INDEX`), copy the index and its signatures to it again; a clock far ahead of the time, which `timedatectl` in the container shows, makes a fresh index look expired too.
- **The last upgrade failed:** the reason is on the Administration page, and in `journalctl -u kubehenge-update` inside the appliance. The release installed before runs again in every case but one, below.
- **"release … did not start: …; rolled back to …":** the new release was installed, but its interface did not answer within 2 minutes or stopped and was restarted soon after; the updater, or `install.sh --upgrade`, put the old release back. `journalctl -u kubehenge` shows why the new one did not start. Report it with that log ([below](#reporting-a-problem)).
- **"…; rolling back to … failed too"**, or **the interface does not come back** after an upgrade: neither release serves. See `journalctl -u kubehenge`, and upgrade or reinstall the release from the shell with `install.sh --upgrade` ([upgrading.md](upgrading.md#from-the-proxmox-ve-shell)).
- **"Distribution: not installed on the management cluster"** or **"Cluster class … has no setting …"** when creating a cluster: the appliance was upgraded, and its interface offers a distribution or a setting whose ClusterClass the management cluster does not have yet. Apply the management cluster again (**Upgrade…** at the top of the Administration page), which installs the new release's ClusterClasses ([upgrading.md](upgrading.md#in-the-appliance)).

### Management cluster etcd backups

**Administration, Backups** shows the last snapshot's error, as does `kubehenge appliance etcd-backups` in the appliance's shell, and the appliance tries again 15 minutes later. `journalctl -u kubehenge` inside the appliance logs each one.

- **"reaching etcd: … etcd snapshot: …"**, or **"reaching the Talos API of …"**: no control-plane node answered on its Talos API (port 50000) from the appliance, or etcd did not answer on it.
- **"the snapshots' talosconfig: …"** or a refusal by the Talos API: the talosconfig kept for snapshots expired or no longer matches the management cluster's CA. Apply the management cluster again, which makes a new one, or **Change…** the backups.
- **"target …: …"** or **"deleting older snapshots: …"**: the target refused the access key, or the key may not delete there. The snapshot was kept in the second case; only the oldest ones were not removed.
- **"the passphrase does not open the target's key for the management cluster's snapshots"**: the target holds a key stored under another state's passphrase ([details](install-settings.md#management-cluster-etcd-backups)).
- **A restore fails**: its run's log on Administration says at which node and step. "Leaving … out" names a node whose Talos API did not answer. It keeps the etcd from before: once it runs, wipe that with `talosctl -n <address> reset --system-labels-to-wipe EPHEMERAL --reboot --graceful=false` and the admin talosconfig (**Appliance**, repair access), and it joins the restored one. "no control-plane node answers on its Talos API": start the management cluster's VMs in Proxmox VE first. "snapshot … does not match its checksum": the object in the target is damaged; restore another snapshot.

### The Proxmox VE certificate or CA changed

The appliance, the management cluster and every cluster check the Proxmox VE API's certificate against the CA chosen when the appliance was installed ([install.md](install.md#the-proxmox-ve-api)). A new certificate from the same CA, such as one Proxmox VE renewed, needs nothing. One from another CA, such as your organisation's replacing Proxmox VE's own, fails those checks: sign-in fails, and the appliance's log (`journalctl -u kubehenge`) and the clusters' cloud controller managers report a certificate error.

- Give the appliance the new CA with `install.sh --upgrade --ctid <ctid> --proxmox-ca <file>` ([upgrading.md](upgrading.md#from-the-proxmox-ve-shell)). It stops, changing nothing, unless the API's certificate is one that CA signed for the URL the appliance uses.
- Then apply the management cluster again, so that it and the clusters check the API against the new CA as well.
- The new certificate has to be valid for the same name or address the appliance reaches the API at.

### The interface is unreachable

- Check the container runs (`pct status <ctid>`) and the service is active (`pct exec <ctid> -- systemctl status kubehenge`).
- The appliance's certificate and its sign-in redirects name the address (or `--name`) it was given. If it took another address from DHCP, give it back that one, or make a certificate for the new one ([install.md](install.md#the-appliances-certificate)).
- **The browser says the certificate expired:** renew it with `install.sh --certificate --ctid <ctid>` ([install.md](install.md#the-appliances-certificate)). The interface warns administrators 30 days ahead.
- The interface keeps working while the management cluster is unwell; pages that need it say the management cluster did not answer. Use **Repair access**, or apply the management cluster again from the Administration page.

## Reporting a problem

Report bugs on GitHub with what you did, what happened, and the relevant lines of the logs above. Report vulnerabilities privately, as `SECURITY.md` describes.
