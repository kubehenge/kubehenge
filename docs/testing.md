# Testing

Kubehenge is tested in four tiers. Only the last needs Proxmox VE.

## The tiers

| Tier | What it checks | Needs Proxmox VE |
| --- | --- | --- |
| Lint and static | Formatting, lint and types of Go, and the known vulnerabilities the Go code reaches. The frontend, shell, Terraform/OpenTofu, YAML, Markdown, the workflows and the e2e image's Dockerfile. Each at its strictest settings, except the Go linters that contradict Go's conventions. The charts' values against their schemas. | no |
| Unit | The interface's backend and frontend, and the template rendering. The fuzz tests' seeds. `go test -fuzz` runs one for longer. CI fuzzes every one for 3 minutes on each pull request and for half an hour each week (`.github/workflows/fuzz.yml`). | no |
| Integration | The Helm charts and Kubehenge's controllers run against a real Kubernetes API server. It carries the CRDs of the Cluster API releases the management cluster installs, and of the Cluster API operator. Cluster API's own webhooks and ClusterClass controller run beside it where a test needs them. The tests cover every object against its schema and Cluster API's checks, and every CEL rule of the classes. They also cover server-side apply, finalizers, status and scale, and the interface's requests as its own account. | no |
| End to end | Real clusters of VMs on Proxmox VE. The tests create a cluster. They scale workers up and down by hand and by the cluster autoscaler. They grow a control plane to three servers and lose one. They upgrade Kubernetes by one minor version with nodes replaced one at a time. They replace a failed node, place clusters across two Proxmox VE nodes and delete the cluster. The workload cluster test runs for each distribution and add-on, with each CNI and node OS in turn, with and without CIS hardening. | yes |

Supported means the end-to-end tier passed. A ClusterClass or add-on is listed as supported only once that tier has run it in CI. The run is against a Proxmox VE host of the current release.

## Running them

`make test` runs `make lint` and `make unit`. `make integration` runs on its own. Their steps run side by side, one per CPU. Each step can also run on its own, such as `make unit-go` or `make lint-web` (the targets are in the `Makefile`).

The frontend's packages are installed again only when `web/package-lock.json` changes. The frontend is built again only when one of its sources does. Every Go step first downloads the Gateway API's install file the controller carries, with curl, once per release (`make gateway-api`). It refuses the file unless it matches the SHA-256 in the `Makefile`.

### The commands

| Command | Tier | Needs |
| --- | --- | --- |
| `make lint` | Lint and static. Go (golangci-lint with every linter but those that contradict Go's conventions, gofumpt). The known vulnerabilities the Go code reaches (govulncheck). The frontend (oxfmt, type-aware oxlint, TypeScript) and the Proxmox VE entry's script (oxfmt, oxlint). Shell (ShellCheck with every optional check, shfmt). Terraform/OpenTofu (fmt, validate, TFLint). The Helm charts with their values. The workflows (actionlint, zizmor). YAML (yamllint). Markdown (markdownlint). The e2e image's Dockerfile (hadolint). `make fmt` rewrites what the format checks refuse. | the tools from `scripts/tools.sh` |
| `make unit` | Unit. The Go packages under the race detector (the interface's API against a fake Proxmox VE and management cluster). The frontend (Vitest). Each Terraform/OpenTofu configuration against mocked providers. The shell scripts' own tests (`test/unit/`). They cover how the installer chooses the Proxmox VE API's address and CA. Those tests run against certificates the test makes and a TLS server on loopback. They also cover how an AlmaLinux or Rocky Linux template build leaves a mirror that serves a broken `repomd.xml`. That test runs against a mirror service and mirrors on loopback. | Go, Node.js, OpenTofu, openssl, Python 3 |
| `make integration` | Integration. The steps are [below](#the-integration-test). | Linux on amd64. Go, Helm, curl. envtest's kube-apiserver and etcd and the providers' CRDs are downloaded on the first run and checked against their checksums (`test/integration/assets.sh`). |
| `make verify-providers` | The providers from Kubehenge's forks, as the module pins them. Each Talos provider's release files have the SHA-256 `versions.tf` names. Their components have the pinned image. The files and image carry the build provenance of the fork's release workflow at that version's tag. The image also carries the cosign signature. CI runs it beside the other tiers. A pin Renovate moves is therefore checked before it is merged. | the GitHub CLI (signed in, or `GH_TOKEN`), cosign, curl |
| `make e2e-templates` | End to end. Create Talos's template and kubeadm's on every node OS offered (RKE2's and k3s's are made by the workload cluster tests). Find each again instead of rebuilding it. Clone and boot it as Cluster API does with a larger disk. Check from inside that it is a fresh first boot of its node OS, grown to its disk, with SELinux enforcing and nothing denied. Delete it and check nothing is left. A node OS no node's CPU runs is skipped. | a Proxmox VE host. Settings in `KUBEHENGE_PROXMOX_*` and `KUBEHENGE_E2E_*`. |
| `make e2e-rancher` | End to end. A one-node k3s cluster installs Rancher, at a name the test resolves to that cluster's virtual IP itself (by default `rancher.kubehenge.test`). On the test machine it resolves through curl's `--resolve`. In the registered cluster it resolves through k3s's CoreDNS. No DNS record or outside service is needed. Another one-node k3s cluster registers with it on its own (agent pinned to Rancher's CA). The registered cluster then leaves Rancher, which cleans up inside it, and registers again. Rancher's cluster is held at the Kubernetes versions Rancher runs on. Deleting the registered cluster removes it from Rancher first. Nothing is left. Both clusters run without workers, at the least memory and cores that allow it. | a running management cluster (`KUBECONFIG`), the k3s template. Settings in `test/e2e/rancher.sh`. |
| `make e2e-cluster-mesh` | End to end. Two one-node k3s clusters run Cilium, each listing the other. They get Cilium cluster ids of their own and are linked through their clustermesh-apiservers on the virtual IPs. A pod of one reaches a pod of the other by address and through a global service. Both back up to an S3 store the test serves. The second reads the first's backups from the start. It therefore shares the first's Kopia password. A namespace with a Secret and a volume is backed up in the first. It is restored into the second through the interface's identities, with the Secret and the volume's data. Once one stops listing the other the link goes, with an event saying why. Once one leaves the mesh, its clustermesh-apiserver, peers and certificates go. Nothing is left. It prints the clustermesh-apiserver's memory. | a running management cluster (`KUBECONFIG`), the k3s template. A backup target served from the test machine, as for the workload cluster test. Settings in `test/e2e/cluster-mesh.sh` and `test/e2e/backups.sh`. |
| `make e2e-autoscaler` | End to end. A one-node k3s cluster with one pool autoscaled from 0 to 2 workers by node label. The autoscaler of the cluster's Kubernetes minor runs in the management cluster, pinned by digest. The admission policy names it. The pool carries its capacity and labels, while the control plane stays tainted. A pod waiting for room brings a worker up from none. A second pod brings a second worker. With both gone, the pool goes back to none after a minute. Switching autoscaling off removes the autoscaler's Deployment, Secret, account, rights and policy entry in the management cluster. It removes its account, rights and status in the cluster. It takes the capacity off the pool and fixes it at 0. The control plane is then untainted. Delete, check nothing is left. It prints the most VMs at once, three, and the autoscaler's memory. | a running management cluster (`KUBECONFIG`), the k3s template. Settings in `test/e2e/autoscaler.sh`. |
| `make e2e-control-plane-ha` | End to end. A k3s cluster's control plane grows from one server to three, each a ready etcd member. The server holding the cluster's virtual IP is stopped. Another server takes the address within the lease. The API there takes writes on two of three etcd members. This lasts until the control plane's health check has the dead server replaced and its VM removed. The control plane shrinks back to one server and keeps what was written. Delete, check nothing is left. | a running management cluster (`KUBECONFIG`), the k3s template, six node addresses. Settings in `test/e2e/control-plane-ha.sh`. |
| `make e2e-two-nodes` | End to end, on a Proxmox VE cluster of two nodes sharing a storage. A k3s cluster kept to the second node (`allowedNodes`) is cloned there from a template on the shared storage and comes up across the nodes' network. Kept to the first and rolled out, every machine replaced moves there. With the second node out of the Proxmox VE cluster, Kubehenge leaves it out of the cluster's placement within seconds. The cluster, let onto every node again, keeps to the first and grows there. With the second node back, delete, check nothing is left. | a running management cluster (`KUBECONFIG`) on the first node, the k3s template on the shared storage, commands that take the second node out and back. Settings in `test/e2e/two-nodes.sh`. |
| `make e2e-interface` | End to end. Make the web interface's kubeconfig from the management cluster as the appliance does after setting it up, and check its rights. It sees clusters, machines and Kubehenge's ClusterClasses everywhere. It reads (without changing) the MachineDeployments, MachineHealthChecks, control planes, add-on releases and events the health page shows. It creates, changes and deletes clusters and reads Secrets only in its own namespaces. It can do nothing else. | a running management cluster with an admin kubeconfig in `KUBECONFIG`, its API in `KUBEHENGE_E2E_API_ENDPOINT`. `KUBEHENGE_E2E_NAMESPACE` adds one of the module's `extra_cluster_namespaces` to the check. |
| `make e2e-node-image-refresh` | End to end. A one-node k3s cluster runs the only build of k3s's template on the default node OS. A run of the weekly template build, asked for as the interface's Build now asks, makes a new build. The build is seen to hand out its boot log through its guest agent. The controller holds the cluster for it (its `kubehenge.dev/node-image-held` annotation) with the node still on its build while its maintenance window is closed. The node takes the new build once the window opens, replaced by a clone of it. A second run makes a third build and deletes the first, which nothing runs any more. The two newest stay. The cluster's node OS is then changed to Rocky Linux 10. That replaces the node with a clone of that template, on an `x86-64-v3` CPU type. It runs Rocky Linux with SELinux enforcing. The controller reads its boot log (cloud-init's output and the journals of cloud-init and k3s) through its guest agent on request. Nothing is denied. Nothing is left. | a running management cluster (`KUBECONFIG`) with its template refresh on and no soak (`template_refresh.soak_hours = 0`). k3s's templates on the default node OS and on Rocky Linux 10. Settings in `test/e2e/node-image-refresh.sh`. |
| `make e2e-appliance RELEASE=<archive> NEWER=<archive> NEWEST=<archive> SETTINGS=<file>` | End to end. The steps are [below](#the-appliance-test). | root on a Proxmox VE node, jq, curl and openssl. SeaweedFS, which it downloads. Three archives from `make dist` with rising `VERSION`s, built with `RELEASE_PUBLIC_KEYS` set to the public key of a key made with `kubehenge release keygen`. `KUBEHENGE_E2E_RELEASE_KEY` names the key's file. The management cluster's settings as a JSON file, as the interface's setup sends them. Settings in `KUBEHENGE_E2E_*` (`test/e2e/appliance.sh`). |
| `make e2e-volumes` | End to end. As Kubehenge's controller, create a cluster's pool, user, token and volume owner VM. Make volumes as the CSI plugin does on every node's local storage. Delete one through the owner VM with the cluster's token. The owner moves to the volume's node. Remove the cluster and check every volume, the owner, the pool and the user are gone. | a Proxmox VE host with the management cluster's Proxmox VE objects (the controller's token). Settings in `KUBEHENGE_PROXMOX_*` and `KUBEHENGE_E2E_*` (`pkg/provisioner/e2e_test.go`). Two or more nodes to check the move. |
| `make e2e-management-cluster DIR=<dir>` | End to end. Apply the management cluster. Check every provider is ready. Check that NetworkPolicies let a pod reach the controller only on its webhook and metrics ports and a token Job only Proxmox VE's API. Check that Cluster API's and the template build's API tokens hold no more than they need. Destroy and check nothing is left. | a Proxmox VE host, kubectl, jq, curl. `<dir>` is a root configuration applying `management-cluster/`. `TF_VAR_proxmox_api_token` is set. |
| `make e2e-management-cluster-upgrade DIR=<dir>` | End to end. Create the management cluster one step behind on Talos, Kubernetes, Cluster API and `capmox`. Upgrade it in place to the tested set. Check the same VMs run the new versions. Destroy. | as above. `<dir>` passes `version_overrides` through. |
| `make e2e-workload-cluster CLASS=<class> VERSION=<v> UPGRADE=<v>` | End to end. One cluster of a ClusterClass is taken through the security checks. Every add-on but Rancher is switched on and off. The cluster also gets etcd snapshots and a restore, scaling, an upgrade and a node replacement. The steps are [below](#the-workload-cluster-test). | a running management cluster (`KUBECONFIG`) set up with etcd restores. The class's VM templates (on Talos of both releases). Go, kubectl, jq, curl, openssl, python3, ssh-keygen, and talosctl on Talos. An address of the test machine the cluster's nodes reach (`KUBEHENGE_E2E_OIDC_ADDRESS`). A backup target of the management cluster's served from that address (`KUBEHENGE_E2E_BACKUP_TARGET`, with its CA, certificate and key in `KUBEHENGE_E2E_BACKUP_CA`, `_CERT` and `_KEY`). Settings in `KUBEHENGE_PROXMOX_*` and `KUBEHENGE_E2E_*` (`test/e2e/workload-cluster.sh`). |

### The integration test

`make integration` takes these steps.

- Install the providers' chart with the module's values (the module's tests hold the two to the same file) and Kubehenge's chart.
- Then for each distribution, start its providers for the first cluster and apply its templates once they serve.
- Remove them and scale the providers to none after the last cluster.
- Take a cluster through Kubehenge's controller (finalizer, credentials owned by the cluster, the add-ons' objects) and delete it.
- Its Proxmox VE objects are removed only once its last machine is gone (Proxmox VE itself is faked).
- Apply the Gateway API release the controller carries to an API server.
- Every CRD is established and its admission policy refuses older and experimental CRDs.
- Find an RKE2 Gateway API chart by its HelmChart, before and after helm-controller's CRD is there.
- Wait until its release is installed.
- Render the RKE2 and k3s classes' Traefik configuration with its Gateway provider on.
- Render it with and without the certificates add-on's default certificate.
- Act as the interface's account, which writes and deletes the credentials users type in and no other Secret.
- Run Cluster API's Cluster and ClusterClass webhooks and its ClusterClass controller beside the API server as the management cluster runs them (`test/integration`).
- See every class pass Cluster API's checks.
- Hold every CEL rule of every class to its message, refused and passed, before the class's templates exist and after.
- Check each class's defaults stored on a cluster made without them, and the rules on changes held against them.
- Send the interface's requests through the API server as the interface's own account.
- They create a cluster of every class as its form sends it, with every add-on on.
- They send changes and their dry-run plans, and a deletion.
- Every refusal of the management cluster's (a CEL rule, a schema, Kubehenge's own checks) reaches the user as its reason.

### The appliance test

`make e2e-appliance` takes these steps.

1. Install the appliance from a release archive with `appliance/install.sh`, on a certificate the Proxmox VE cluster's CA signed for its address.
2. Sign in with Proxmox VE users.
   - A wrong password and a user without privileges are refused.
3. Set the management cluster up from the interface.
   - A second setup and an upgrade from the shell are refused while it runs.
4. Offer placement by an HA rule to a cluster creator without `Sys.Audit` on `/`.
   - It shows only the rule's name and nodes.
   - An administrator reads it in full.
5. Reach the shared Headlamp behind the interface's sign-in.
   - Once the settings' `headlampActions` reaches the interface, it offers the Helm app catalog and the Prometheus charts it ships.
   - A chart from a path on the appliance, a Helm repository over HTTP and a port forward are refused.
6. Log out of a cluster in it as its frontend does.
   - The token cookie is cleared.
   - The logout is refused to those who do not see the cluster and to a request from another origin or none.
7. Open the cluster again from the interface's link.
8. Back the management cluster's etcd up into an S3 store served on the node.
   - This is refused to a user without privileges and with a wrong passphrase.
   - A scheduled snapshot and one asked for are stored encrypted and listed.
9. Restore the newer snapshot from the appliance's shell with `kubehenge appliance`.
   - A wrong passphrase is refused there as in the interface.
   - A Secret made since is gone and the ClusterClasses are there.
10. Show the backups, list and take a snapshot and show the upgrades with `kubehenge appliance`.
    - These are refused to a user without privileges with the interface's words.
11. Switch automatic upgrades of the management cluster on, refused with a wrong passphrase.
    - Their Proxmox VE user is made.
12. Switch them off, and their user is gone.
13. Switch them on again.
14. Have the updater upgrade to a release whose interface does not start.
    - The updater reads a release index signed with a key made for the run and served from the S3 store.
    - It rolls that back with the reason.
    - Then it upgrades to the newer release.
15. Upgrade the appliance with `install.sh --upgrade` under the running management cluster to a release whose interface does not start.
    - It rolls that back with the reason.
    - Then it upgrades to the newest release and refuses an older one.
16. See that the management cluster runs what the first release applied.
17. Upgrade it to what the newest one carries.
    - A snapshot is still taken after that.
18. Remove the management cluster from the interface, its automatic upgrades' user with it.
    - This is refused unconfirmed or with a wrong passphrase.
    - Its etcd backups are switched off with it.
19. Remove the appliance.
20. Check that Proxmox VE is left exactly as it was.

### Memory and CPU figures

The workload and Rancher tests print the memory each node uses and each namespace's pods. They also print the CPU they request of what each node offers, before the add-ons and with every add-on on. The workload test prints the control plane's memory after each add-on is switched on.

The classes' memory figures come from these prints. They cover the add-ons, what they add to a control plane and the nodes' own (`kubehenge.dev/addon-memory`, `kubehenge.dev/addon-control-plane-memory` and `kubehenge.dev/node-memory`, [clusters.md](clusters.md#machines)). The workload test gives its control plane and worker the least the figures let them have. A figure a new add-on or release outgrows is raised in `charts/kubehenge/templates`.

#### CPU figures

The CPU figures (`kubehenge.dev/addon-cpu` and `kubehenge.dev/node-cpu`) are the requests of the pinned charts and distributions as Kubehenge configures them. They move with them. When a figure falls behind, the requests the tests print show it. The test's worker, given the fewest cores the figures allow, then leaves a pod pending.

### The workload cluster test

`make e2e-workload-cluster` takes one cluster through these steps, in this order.

#### Creating and securing the cluster

1. Create a cluster from a ClusterClass.
   - With `KUBEHENGE_E2E_NAMESPACE`, it is created as the interface's account, in that one of the interface's namespaces. The k3s run uses it.
   - On Talos it runs at the release `KUBEHENGE_E2E_TALOS_VERSION` names. By default CI sets it to the release before the one the templates are made from.
   - The talosconfig is read as the interface hands it out. It holds an administrator's certificate that reaches every node through Talos's API.
   - Its worker has the least memory and cores the class lets it have with every add-on but Rancher.
   - It trusts an OpenID issuer the test serves.
   - It is hardened to the CIS benchmark, unless `KUBEHENGE_E2E_CIS=false`.
     - The classes default to no hardening.
     - The nightly run sets `KUBEHENGE_E2E_CIS=false` every other night.
   - Pod Security Admission enforces the restricted level, so every component Kubehenge installs runs under it.

##### Benchmark and tokens

1. Check with kube-bench that a control-plane node and a worker fail none of the benchmark's checks but the documented exceptions (`test/e2e/cis-benchmark.sh`).
   - It is not run on Talos, for which kube-bench has no benchmark, nor on a cluster without the hardening.
2. Check the issuer's tokens and anonymous access.
   - The issuer's ID tokens sign in with prefixed user and groups.
   - Tokens for another audience or from another key do not.
   - ServiceAccount tokens still work.
   - Requests without credentials reach nothing but the health checks of kubeadm and Talos (again after the upgrade).
3. Check that the `kubernetes` Service and every pod take their addresses from the cluster's ranges.

##### Network and identities

1. Check the CNI chosen for it (`KUBEHENGE_E2E_CNI`, else the distribution's own).
   - It is the one running, with flannel off beside Cilium or Calico.
   - It enforces a NetworkPolicy.
2. Check the health page's and logs identities.
   - The health page's identity sees a crash-looping pod as such and reads the API server's readiness.
   - Its logs identity reads that pod's last run.
   - Neither may read Secrets, change anything or do the other's work.

#### The cluster's own objects and services

1. Check the cluster's own objects and services.
   - Its own Proxmox VE pool, user and token scope.
   - Its nodes are initialised by the cloud controller manager. Its control-plane nodes are tainted once its worker has joined.
   - Its `KubehengeReady` condition is true and `MachinesStalled` is false.
   - The controller reads every machine's boot log as the interface asks for it.
   - Those logs are cloud-init's output and the journals of cloud-init and the distribution's services, through the guest agent with `VM.GuestAgent.FileRead` alone.
   - On Talos they are the kernel's and Talos services' logs through Talos's API.
   - The support bundle `kubehenge diagnose` writes holds the Cluster and those boot logs. It holds no Secret nor any of the cluster's secrets the test knows.
   - The controller's `/healthz` answers.
   - Its metrics are verified against its CA.
   - They refuse a request without credentials and an account without `kubehenge-metrics-reader`. They answer one with it.

##### Traefik and the Gateway API

- Traefik serves a host name through an Ingress on the API address and every node.
- Traefik serves an HTTPRoute through its Gateway on the API address. The Gateway reports it.
- Every object of the Gateway API is held once, and again after the upgrade.
- One that a Helm release's latest revision ships is marked as that release's. It is shipped by no other and is of the release that chart ships.
- One that no release ships is of Kubehenge's release or newer.
- By the upgrade, RKE2's and k3s's own Gateway API chart has to be installed and ship every object.

##### Load balancer and volume

- A Service of type `LoadBalancer` answers the test machine on an address from the cluster's load-balancer range. The settings are `KUBEHENGE_E2E_LOAD_BALANCER` (MetalLB unless kube-vip) and `KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES`.
- On k3s this runs without servicelb beside it.
- The load balancer is switched off again. On k3s, servicelb then answers for the Service once the control planes are replaced.
- A persistent volume is created and deleted.

##### Volume snapshots

1. On the k3s run (`KUBEHENGE_E2E_VOLUME_SNAPSHOTS`), volume snapshots are switched on and then off.
   - With them on, the CSI plugin keeps the cluster's token behind the copy proxy.
   - The proxy runs from the controller's image under test, which the control-plane nodes pull from `KUBEHENGE_E2E_REGISTRY` meanwhile. Its configuration has no root@pam or password.
   - The cluster's pool holds stopped holder VMs. Its token has `VM.Clone`.
   - A VolumeSnapshot of a volume with data is ready. A volume restored from it and a clone of the volume each hold the same data.
   - The three copies are in the proxy's log and on the holder VMs.
   - The proxy refuses a copy of a node VM's disk, with a `VolumeCopyFailed` event and nothing left behind.
   - The snapshot is deleted and its copy is gone from its holder VM. The claims are then deleted and no copy or record is left.

##### Volume snapshots off

- With them off, the plugin reaches Proxmox VE directly with the cluster's token again.
- Its snapshotter sidecar, the proxy, the copies' namespace, the empty holder VMs, the VolumeSnapshotClass and the snapshot controller are gone.

#### Add-ons

##### Headlamp

1. Headlamp is switched on, then given read-only access for everyone, then taken back and switched off.
   - On, it is reachable through the API server. It has no rights of its own and refuses to list pods without a token, at Pod Security's restricted level.
   - With read-only access for everyone, it lists pods without a token. Its account cannot read Secrets or change anything.
   - Without that access it refuses again.
   - Then it is switched off.
2. The shared Headlamp's sign-in identities.
    - They are made only for clusters in the interface's namespaces (the k3s run's `KUBEHENGE_E2E_NAMESPACE` among them).
    - Their issuer can mint tokens for the sign-in account and nothing else.

##### Admission policy and the mesh

 1. A privileged pod is tried three times.
    - Pod Security Admission refuses it.
    - Once Kyverno is switched on, Kyverno refuses it in a namespace labelled privileged.
    - Once Kyverno is off, it is admitted there again.
 2. Istio is switched on and then off, before monitoring.
    - The order keeps the CNI's roll that Istio sets off from landing on a worker already carrying the monitoring stack.
    - On, the pods of a namespace enrolled in the mesh are redirected through ztunnel.
    - A client outside the namespace is refused once mutual TLS is required. One inside reaches the server.
    - Its workload identity is in ztunnel's logs.
    - Off, the namespace is taken out of the mesh first. Both clients then reach the server again.

##### Monitoring and logs

 1. Monitoring is switched on and then off.
    - On, every component's job is scraped, each with its server verified.
    - On k3s, the controller manager's, scheduler's and kube-proxy's series arrive in the kubelet's.
    - On Talos, every job is scraped but the controller manager's and scheduler's. Those two are scraped only if the Talos release passes them serving certificates of the cluster CA.
    - Its namespace is at Pod Security's restricted level.
    - Off, it goes with its metrics proxies.

##### Logs

 1. Logs are switched on beside monitoring.
    - Grafana's Loki data source is healthy.
    - A pod's lines and a pod's that ends within seconds arrive from the collector on every node.
    - So do a node's journal and the API server's audit log.
    - On Talos, which keeps no journal, the node's logs are the kubelet's and the kernel's that Talos sends the collector. The audit log comes from `/var/log/audit/kube`.
    - Loki refuses a read without credentials.
    - `logging` is at Pod Security's restricted level.
    - Logs are then collected through the Kubernetes API instead. The collector's DaemonSet is gone and a pod's lines arrive once.
    - Collection returns to every node.
    - Switching monitoring off is refused while logs are on. Logs then go off, with Loki's volume kept.

##### Backups

 1. Backups are switched on to an S3 store the test serves with SeaweedFS over TLS.
    - Velero's server is at Pod Security's restricted level. The interface's two identities hold only their rights.
    - A backup of a namespace with a Secret and a volume is made from the schedule, with `kube-system` left out of it.
    - Every object is under the cluster's prefix.
    - Its resources are unreadable without the cluster's SSE-C key. Its volume's data is nowhere in the store in plain text.

##### Restores and deletions

- Each restore and deletion is done as the interface does it, through its identities.
- The namespace and a second one of the backup are deleted. The first is restored alone, with its Secret and the file on its volume.
- The second is left out.
- A restore of Secrets alone leaves the Secret the cluster has as it is.
- Its results are read through the link Velero signs (to the target, without the key in it, unreadable without the cluster's SSE-C key). They tell for that namespace that the Secret was kept.
- A second backup is deleted. It is gone from Velero and from the store while the first stays.
- A restore without the volumes' data and without ConfigMaps brings back the Secret and the volume's claim but not the pod or the ConfigMap.
- No volume is restored. A pod started on the claim finds a new, empty volume.

##### Reading other clusters' backups

- Reading other clusters' backups is refused through kubectl for the cluster itself and for a cluster with no keys beside it.
- A cluster backing up to a second target (`KUBEHENGE_E2E_BACKUP_OTHER_TARGET`) is refused this cluster's backups in the first target too.
- It is allowed for a cluster deleted since whose keys are kept. Its backups' read-only location in Velero has its SSE-C key, and its volumes' data is readable through the shared Kopia password.
- The location is then removed again with its key once the cluster no longer reads them.

##### Certificates

 1. Certificates are tested from the cluster's own CA, then from an ACME server, then switched off.
    - From the cluster's own CA, certificates are switched on.
    - The CA is kept beside the Cluster, ECDSA P-256, constrained to the cluster's domain and to no address. It is copied into the workload cluster.
    - Its default certificate covers the domain and every name one level below it.
    - It serves an Ingress that names no certificate and an HTTPRoute on the Gateway's HTTPS listener.
    - Both are verified against that CA.

##### Certificates from an ACME server

- Then comes an ACME server the test runs in the cluster (Pebble, with BIND answering for its challenges).
- Over DNS-01 it uses the cluster's own DNS credential, an RFC 2136 TSIG key copied into the workload cluster. cert-manager checks its records at BIND alone.
- The default certificate is issued again from the ACME server.
- Headlamp at a name the default certificate does not cover gets its own. Grafana at one it covers gets none.

##### HTTP-01 and switching off

- Over HTTP-01 it goes through the Gateway, at names the test gives the virtual IP in the cluster's DNS and in BIND. Nothing outside the cluster resolves them.
- The default certificate, its Secret, the DNS credential and the HTTPS listener are gone. An Ingress with ingress-shim's default issuer and Headlamp are each served with their own.
- Off, the issuer is gone and the Cluster's `kubehenge.dev/certificates` annotation is removed.
- cert-manager is gone with nothing left needing it. The cluster's CA is kept.

##### GitOps

 1. GitOps is switched on with Flux, then switched to Argo CD.
    - Flux syncs a private repository on a Git server inside the cluster through the deploy key Kubehenge made.
    - A new commit is synced. Flux is at Pod Security's restricted level.
    - Flux is then uninstalled first, with its CRDs and its copy of the key, while what it synced keeps running.
    - Argo CD syncs a new commit. Its application is synced and healthy.
    - Its local admin is disabled. It is at the restricted level.

#### Upgrade, restore and teardown

##### Talos upgrade

 1. On Talos, with every add-on on, every node is upgraded in place to the release `KUBEHENGE_E2E_TALOS_UPGRADE` names.
    - The nodes go one at a time, the control plane first.
    - The machines are the same ones.
    - Each is drained and let back. None is left cordoned.
    - Each node's kubelet and Talos's API report the new release.
    - `kubehenge.dev/talos-upgraded` records it. There is no `kubehenge.dev/talos-upgrade-failed`.
    - New machines still boot the release before until the next Kubernetes upgrade.
    - Every add-on's release is deployed, every workload is ready and every monitoring target is up again.

##### Switching every add-on off

 1. Every add-on is switched off at once, Argo CD and backups among them.
    - Argo CD's CRDs and its copy of the key are gone. What it synced is still there.
    - Backups are switched off. Velero and its CRDs are gone.
    - The backups and the cluster's keys are kept.

##### etcd snapshots

 1. An etcd snapshot is taken on request into the same S3 store as the backups.
    - It is the only object in the cluster's `etcd` folder, encrypted with age.
    - A ConfigMap made before it is nowhere in the store in plain text.
    - `kubehenge etcd-snapshot decrypt` decrypts it with the cluster's key to the SHA-256 it was taken with.
    - `etcdutl snapshot status` reads it. It holds the ConfigMap.
    - On Talos, whose API does not tell it, it is recorded without a revision.

##### etcd restore

 1. etcd is restored from it through the guest agent.
    - The cluster is available again with the ConfigMap made before the snapshot back and one made after it gone.
    - The only other snapshot in the store is the one the restore took of etcd before restoring. It is encrypted and holds the ConfigMap made after it.
    - On Talos the restore goes through the Talos API. The control-plane machines are the same afterwards, each a voting member of the restored etcd again.

##### The distribution's own upload

 1. On RKE2 and k3s, the distribution's own upload is switched on.
    - The Secret `kubehenge-etcd-s3` in `kube-system` verifies the store's certificate.
    - The control planes are replaced.
    - A snapshot the server takes itself is found in the cluster's `<distribution>-etcd` folder.
    - The upload is then switched off with the snapshots. The Secret is gone and the control planes are replaced again.

##### Scaling, upgrade and deletion

 1. The cluster is scaled, upgraded and deleted.
    - Workers are scaled to two and back.
    - On the k3s run, the scaling goes through the interface instead, with `kubehenge cluster` signed in with a Proxmox VE API token.
    - The cluster is listed and its kubeconfig fetched. The pool is scaled to two.
    - A worker is cordoned and uncordoned. It is rebooted and still the same machine.
    - It is replaced. It is deleted with its pool one smaller.
    - The upgrade is by one minor version.

##### The upgrade

- On RKE2 and k3s the test has the old servers refuse the first new server until its cloud-init has ended.
- That makes its first start fail. The roll survives it.
- On Talos the new machines are cloned from the template of the release the nodes were upgraded to. They need no upgrade in place.
- After the upgrade, every node runs the cluster's node OS (`KUBEHENGE_E2E_NODE_OS`, else the class's default).
- SELinux is enforcing, `/usr` is read-only and there is no denial in its logs. This is not checked on Talos, whose nodes run Talos.
- A stopped worker is replaced.
- The cluster is deleted and nothing is left.

#### Running half of it

`KUBEHENGE_E2E_PART` runs half of it, for a machine whose memory holds the guests of only one half. CI runs each class's test this way, one half per machine.

- `lifecycle` runs the `kubeadm`, `rke2`, `k3s` and `talos` tests.
  - It sizes the nodes at the least the class allows with the add-ons a cluster starts with (the load balancer and Rancher's agent).
  - It switches on no other add-on.
- `addons` runs `kubeadm-addons`, `rke2-addons`, `k3s-addons` and `talos-addons`.
  - It sizes the nodes for every add-on.
  - It leaves out every step that replaces a machine. Those are RKE2's and k3s's own upload, the load balancer switched off, scaling, the upgrade and the dead worker.

Both run the rest. On Talos that includes the upgrade in place. Both check SELinux at their end.

## End to end without a Proxmox VE host

CI runs the end-to-end tier with Proxmox VE in a container (`.github/workflows/e2e.yml`), one test per machine.

### What runs when

#### Which tests run

- The merge queue runs the tests that all the changes of a merge group need together (`test/e2e/ci/scope.sh`). It picks none for documentation alone and all of them for a change it cannot place.
- Because it runs them on the tree that would land on main, no change reaches main without the tests it needs.
- A pull request runs the other tiers and leaves this one to the queue.

#### The nightly run

- The nightly run (`test/e2e/ci/nightly.sh`) runs every test. It adds the management cluster's upgrade, Rancher, a control plane of three servers and a Proxmox VE cluster of two nodes.
- It runs both halves of each workload distribution's test once more on another CNI of its class and another node OS.
- That extra run goes without CIS hardening every other night. The combination moves on each night so that every one comes in turn.
- It builds Proxmox VE's image afresh and keeps it, with Go's build cache, for the day's runs.
- A dispatch of the workflow names the tests to run, or runs that night's set.

#### Running them yourself

Any Linux machine with Docker, `/dev/kvm` and about 16 GB of free memory can do the same:

```sh
scripts/tools.sh ~/.local/kubehenge-tools && export PATH=~/.local/kubehenge-tools:$PATH
test/e2e/ci/up.sh /tmp/e2e          # Proxmox VE in a container, a registry beside it
test/e2e/ci/run.sh /tmp/e2e rke2    # or management-cluster, management-cluster-upgrade, appliance, kubeadm, k3s, talos, kubeadm-addons, rke2-addons, k3s-addons, talos-addons, rancher, cluster-mesh, node-image-refresh, autoscaler, control-plane-ha, two-nodes
test/e2e/ci/run.sh /tmp/e2e kubeadm+calico+rocky-10+cis-off   # a workload test or its add-on half with its CNI, node OS or no CIS hardening
```

#### What it cannot test

The control plane of three servers stays on k3s. Its servers fit the machine three at a time at the class's least of 4096 MiB beside a worker and the management cluster. RKE2's and kubeadm's need bigger workers too. A minor upgrade of three servers needs a fourth beside them while each is replaced. The machine's memory does not hold these.

The two Proxmox VE nodes share the machine. They test placement, clones and a node leaving, not a node's hardware failing. Proxmox VE HA restarting VMs on another node is not run. It fences a failed node through its watchdog. A container lacks one.

#### Machines without pause-loop exiting

A machine whose KVM lacks pause-loop exiting hands its test to a fresh one, up to three times. On such a machine, guests that outnumber its CPUs starve each other whatever their sizes. The check that nothing restarts with every add-on on would then only measure the machine. A test still on such a machine at the fourth try runs there with restarts reported, not failed (`KUBEHENGE_E2E_RESTARTS=report`).

### What the machine records

Each try records what the machine and its VMs take while the test runs, every 15 seconds. That is the machine's memory, swap, disk and CPUs, and per VM the CPU time, resident memory and swap of its QEMU process (`test/e2e/ci/guests.sh record`).

The log's "The guests' load" step sums it up per minute and gives each VM's busiest minute and most memory. A step that outgrows the machine shows there as a VM swapped out, or the CPUs of all of them above the machine's.

#### Memory pressure

The machine keeps its page cache rather than the guests' memory under pressure (`vm.swappiness=10`) and starts reclaiming earlier (`vm.watermark_scale_factor=200`). Cloning a template then evicts cached disk blocks instead of a running control plane.

#### Fitting the machine

Swap does not count as room for the guests. On a slower machine a guest whose memory has to be read back from swap stalls until its own checks fail. One example is a container restarting with every add-on on. Every test's VMs therefore fit the machine's memory with room for its own processes. The management node at the module's default 4 GiB is among them.

The workload test is split in halves for it.

The one exception is the RKE2 test's control-plane replacements. There three machines of RKE2's least 4 GiB beside the management node outgrow the memory by about 2 GiB.

### The guard

A guard (`test/e2e/ci/guard.sh`) keeps the CI agent's own processes out of the OOM killer's choice and their memory out of reclaim. It samples the machine's memory, swap, pressure and free disk every 5 seconds. It ends the test while the machine can still report, once any of these holds:

- memory and swap together fall under 1.5 GiB
- every task has waited on memory for half of the last minute
- the OOM killer has ended a process
- a file system has under 2 GiB free

#### What it reports

It reports what held the memory (the VMs by name, the largest other processes, the last samples and the kernel's lines on memory). The test then fails like any other and keeps its artifacts. The log's "The runner's room" step gives how near each run came to these floors. It also gives the least memory the run had available and how much it read back from swap.

### What a failed test leaves

When a test fails, the try's artifact holds the clusters' objects and pods' logs (and the earlier run of every container that restarted). Beside them it holds what each running VM held through its guest agent (`guests.sh collect`):

- its state and pressure
- the distribution's services' journal and every line about etcd in it
- the logs of the static pods and of restarted containers as the kubelet keeps them
- the kernel's log
- etcd's members, status, health and metrics as the node sees them

#### Talos VMs

A Talos VM's guest agent runs no commands. A Talos VM is read through Talos's API instead, with talosctl and its cluster's talosconfig:

- its stage, its services' states and events, its memory, pressure and processes and the kubelet's containers
- the logs of machined, apid, trustd, both containerd instances, the kubelet and etcd
- the API server's and restarted containers' logs as the kubelet keeps them
- the kernel's log
- on a control plane, etcd's members, status and alarms

A node whose API does not answer is asked once. Every call is cut off after a minute. These still read when a cluster's Kubernetes API no longer answers. Tokens and passwords are redacted from them. The talosconfigs stay out of them.

### The Proxmox VE container

`up.sh` builds `test/e2e/pve`, a one-node Proxmox VE on Debian 13 that runs on the machine's own kernel and starts VMs on its KVM. The clusters' VMs are therefore nested one level deeper than the machine itself.

Like every node it runs an SSH server and reaches itself over SSH as root. `up.sh` checks that. Proxmox VE copies a volume, such as a CSI volume snapshot, through SSH even within one node.

#### The scripts

`up.sh` also does the following.

- It gives the VMs the bridge 172.20.99.0/24, with a name server on it that asks this machine's own. The VMs therefore need no public one.
- It gives the containers 172.21.99.0/24.
- It routes this machine to the bridge (with `sudo`).
- It writes the settings `run.sh` reads.

#### Running a test

`run.sh` runs a test in these steps.

- It builds the controller from the working tree and pushes it to the registry.
- It applies the management cluster (`test/e2e/ci/main.tf`) with the container's CA trusted by its own processes only (`SSL_CERT_FILE`). The machine's trust store is left as it is.
- It creates the templates the test needs meanwhile from the working tree's recipes. Each kubeadm, RKE2 or k3s build is seen to hand out its boot log through its guest agent.
- It runs the test.
- `management-cluster` runs the templates and volumes tests while the management cluster is up, before it destroys it.
- For `appliance` it builds three releases instead and runs the test in the container. The container's appliance sets the management cluster up itself.

#### Second node and cleanup

For `two-nodes`, `run.sh` first joins a second node, `pve2`, from the same image to the first in a Proxmox VE cluster (`test/e2e/ci/second-node.sh`). The two VM bridges are joined at layer 2 through a Docker network of their own. The first keeps the gateway and name server. A directory both mount is their shared storage `shared`. The management cluster and the templates then use it.

Remove everything with `docker rm -f kubehenge-e2e-pve kubehenge-e2e-pve2 kubehenge-e2e-registry && docker network rm kubehenge-e2e kubehenge-e2e-vms`. The second node and network exist only where `two-nodes` ran. Then remove the route with `sudo ip route del 172.20.99.0/24`.
