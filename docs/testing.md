# Testing

Kubehenge is tested in four tiers. Only the last needs Proxmox VE.

| Tier | What it checks | Needs Proxmox VE |
| --- | --- | --- |
| Lint and static | Every language and file type at its strictest settings, the known vulnerabilities the Go code reaches and the charts' values against their schemas. | no |
| Unit | The interface's backend and frontend, the template rendering and the fuzz tests' seeds. `go test -fuzz` runs one for longer. CI fuzzes every one for 3 minutes on each pull request and for half an hour each week (`.github/workflows/fuzz.yml`). | no |
| Integration | The Helm charts and Kubehenge's controllers against a real Kubernetes API server ([steps](#the-integration-test)). It carries the CRDs of the Cluster API releases the management cluster installs and of the Cluster API operator. The tests also cover server-side apply, status and scale. | no |
| End to end | Real clusters of VMs on Proxmox VE, through the `make e2e-*` commands below. The workload cluster test runs for each distribution and add-on, with each CNI and node OS in turn, with and without CIS hardening. | yes |

A ClusterClass or add-on is listed as supported only after the end-to-end tier ran it in CI on the current Proxmox VE release.

## Running them

`make test` runs `make lint` and `make unit`. `make integration` runs on its own. Their steps run side by side, one per CPU, and each also runs alone, such as `make unit-go` or `make lint-web` (see the `Makefile`).

The frontend's packages are installed again only when `web/package-lock.json` changes. The frontend is built again only when one of its sources does. Every Go step first downloads the Gateway API's install file the controller carries, once per release (`make gateway-api`). It refuses the file unless it matches the SHA-256 in the `Makefile`.

| Command | What it runs | Needs |
| --- | --- | --- |
| `make lint` | Lint and static: Go (golangci-lint with every linter but those that contradict Go's conventions, gofumpt), the known vulnerabilities the Go code reaches (govulncheck), the frontend (oxfmt, type-aware oxlint, TypeScript), the Proxmox VE entry's script (oxfmt, oxlint), shell (ShellCheck with every optional check, shfmt), Terraform/OpenTofu (fmt, validate, TFLint), the Helm charts with their values, the workflows (actionlint, zizmor), YAML (yamllint), Markdown (markdownlint) and the e2e image's Dockerfile (hadolint). `make fmt` rewrites what the format checks refuse. | the tools from `scripts/tools.sh` |
| `make unit` | Unit: the Go packages under the race detector (the interface's API against a fake Proxmox VE and management cluster), the frontend (Vitest), each Terraform/OpenTofu configuration against mocked providers and the shell scripts' own tests (`test/unit/`). These cover how the installer chooses the Proxmox VE API's address and CA, against certificates they make and a TLS server on loopback. They also cover how an AlmaLinux or Rocky Linux template build leaves a mirror that serves a broken `repomd.xml`, against a mirror service and mirrors on loopback. | Go, Node.js, OpenTofu, openssl, Python 3 |
| `make integration` | Integration ([steps](#the-integration-test)). | Linux on amd64. Go, Helm, curl. envtest's kube-apiserver and etcd and the providers' CRDs are downloaded on the first run and checked against their checksums (`test/integration/assets.sh`). |
| `make verify-providers` | The providers from Kubehenge's forks, as the module pins them. Each Talos provider's release files have the SHA-256 `versions.tf` names, and their components the pinned image. Files and image carry the build provenance of the fork's release workflow at that version's tag, the image also its cosign signature. CI runs it beside the other tiers, so a pin Renovate moves is checked before it is merged. | the GitHub CLI (signed in, or `GH_TOKEN`), cosign, curl |
| `make e2e-templates` | Create Talos's template and kubeadm's on every node OS offered (the workload cluster tests make RKE2's and k3s's), and find each again instead of rebuilding it. Clone and boot it as Cluster API does with a larger disk. Check from inside that it is a fresh first boot of its node OS, grown to its disk, with SELinux enforcing and nothing denied. Delete it and check nothing is left. A node OS no node's CPU runs is skipped. | a Proxmox VE host. Settings in `KUBEHENGE_PROXMOX_*` and `KUBEHENGE_E2E_*`. |
| `make e2e-rancher` | A one-node k3s cluster installs Rancher at a name the test itself resolves to the cluster's virtual IP (by default `rancher.kubehenge.test`). curl's `--resolve` does so on the test machine and k3s's CoreDNS in the registered cluster, with no DNS record or outside service. Another one-node k3s cluster registers on its own (agent pinned to Rancher's CA), leaves with Rancher cleaning up inside it, and registers again. Rancher's cluster is held at the Kubernetes versions Rancher runs on. Deleting the registered cluster removes it from Rancher first, and nothing is left. Both run without workers, at the least memory and cores that allow it. | a running management cluster (`KUBECONFIG`), the k3s template. Settings in `test/e2e/rancher.sh`. |
| `make e2e-cluster-mesh` | Two one-node k3s clusters run Cilium, each listing the other. They get Cilium cluster ids of their own and are linked through their clustermesh-apiservers on the virtual IPs. A pod of one reaches a pod of the other by address and through a global service. Both back up to an S3 store the test serves. The second shares the first's Kopia password and reads its backups from the start. A namespace with a Secret and a volume, backed up in the first, is restored into the second through the interface's identities, data included. Once one stops listing the other the link goes, with an event saying why. Once one leaves the mesh, its clustermesh-apiserver, peers and certificates go. Nothing is left. It prints the clustermesh-apiserver's memory. | a running management cluster (`KUBECONFIG`), the k3s template. A backup target served from the test machine, as for the workload cluster test. Settings in `test/e2e/cluster-mesh.sh` and `test/e2e/backups.sh`. |
| `make e2e-autoscaler` | A one-node k3s cluster has one pool autoscaled from 0 to 2 workers by node label. The autoscaler of the cluster's Kubernetes minor runs in the management cluster, pinned by digest and named by the admission policy. The pool carries its capacity and labels, while the control plane stays tainted. A pod waiting for room brings a worker up from none, a second pod a second worker. With both gone, the pool goes back to none after a minute. Switching autoscaling off removes the autoscaler's Deployment, Secret, account, rights and policy entry in the management cluster. In the cluster it removes its account, rights and status. The pool loses its capacity and is fixed at 0, and the control plane is untainted. Delete, check nothing is left. It prints the most VMs at once, three, and the autoscaler's memory. | a running management cluster (`KUBECONFIG`), the k3s template. Settings in `test/e2e/autoscaler.sh`. |
| `make e2e-control-plane-ha` | A k3s cluster's control plane grows from one server to three, each a ready etcd member. The server holding the cluster's virtual IP is stopped, and another takes the address within the lease. The API there takes writes on two of three etcd members until the control plane's health check has the dead server replaced and its VM removed. The control plane shrinks back to one server and keeps what was written. Delete, check nothing is left. | a running management cluster (`KUBECONFIG`), the k3s template, six node addresses. Settings in `test/e2e/control-plane-ha.sh`. |
| `make e2e-two-nodes` | On a Proxmox VE cluster of two nodes sharing a storage. A k3s cluster kept to the second node (`allowedNodes`) is cloned there from a template on the shared storage and comes up across the nodes' network. Kept to the first and rolled out, every machine replaced moves there. With the second node out of the Proxmox VE cluster, Kubehenge leaves it out of the cluster's placement within seconds. The cluster, let onto every node again, keeps to the first and grows there. With the second node back, delete, check nothing is left. | a running management cluster (`KUBECONFIG`) on the first node, the k3s template on the shared storage, commands that take the second node out and back. Settings in `test/e2e/two-nodes.sh`. |
| `make e2e-interface` | Make the web interface's kubeconfig as the appliance does after setting the management cluster up, and check its rights. It sees clusters, machines and Kubehenge's ClusterClasses everywhere. It reads (without changing) the MachineDeployments, MachineHealthChecks, control planes, add-on releases and events the health page shows. It creates, changes and deletes clusters and reads Secrets only in its own namespaces, and does nothing else. | a running management cluster with an admin kubeconfig in `KUBECONFIG`, its API in `KUBEHENGE_E2E_API_ENDPOINT`. `KUBEHENGE_E2E_NAMESPACE` adds one of the module's `extra_cluster_namespaces` to the check. |
| `make e2e-node-image-refresh` | A one-node k3s cluster runs the only build of k3s's template on the default node OS. A run of the weekly template build, asked for as the interface's Build now asks, makes a new build. It hands out its boot log through its guest agent. While the maintenance window is closed, the controller holds the cluster on its old build (`kubehenge.dev/node-image-held`). Once it opens, the node is replaced by a clone of the new build. A second run makes a third build and deletes the first, which nothing runs any more. Changing the node OS to Rocky Linux 10 replaces the node with a clone of that template on an `x86-64-v3` CPU type. It runs with SELinux enforcing and nothing denied. The controller reads its boot log (cloud-init's output and the journals of cloud-init and k3s) through its guest agent on request. Nothing is left. | a running management cluster (`KUBECONFIG`) with its template refresh on and no soak (`template_refresh.soak_hours = 0`). k3s's templates on the default node OS and on Rocky Linux 10. Settings in `test/e2e/node-image-refresh.sh`. |
| `make e2e-appliance RELEASE=<archive> NEWER=<archive> NEWEST=<archive> SETTINGS=<file>` | The appliance's install, upgrades and removal ([steps](#the-appliance-test)). | root on a Proxmox VE node, jq, curl and openssl. SeaweedFS, which it downloads. Three archives from `make dist` with rising `VERSION`s, built with `RELEASE_PUBLIC_KEYS` set to the public key of a key made with `kubehenge release keygen`, whose file `KUBEHENGE_E2E_RELEASE_KEY` names. The management cluster's settings as a JSON file, as the interface's setup sends them. Settings in `KUBEHENGE_E2E_*` (`test/e2e/appliance.sh`). |
| `make e2e-volumes` | As Kubehenge's controller, create a cluster's pool, user, token and volume owner VM. Make volumes as the CSI plugin does on every node's local storage. Delete one through the owner VM with the cluster's token, the owner moving to the volume's node. Remove the cluster and check every volume, the owner, the pool and the user are gone. | a Proxmox VE host with the management cluster's Proxmox VE objects (the controller's token). Settings in `KUBEHENGE_PROXMOX_*` and `KUBEHENGE_E2E_*` (`pkg/provisioner/e2e_test.go`). Two or more nodes to check the move. |
| `make e2e-management-cluster DIR=<dir>` | Apply the management cluster and check every provider is ready. Check that NetworkPolicies let a pod reach the controller only on its webhook and metrics ports, and a token Job only Proxmox VE's API. Check that Cluster API's and the template build's API tokens hold no more than they need. Destroy and check nothing is left. | a Proxmox VE host, kubectl, jq, curl. `<dir>` is a root configuration applying `management-cluster/`, with `TF_VAR_proxmox_api_token` set. |
| `make e2e-management-cluster-upgrade DIR=<dir>` | Create the management cluster one step behind on Talos, Kubernetes, Cluster API and `capmox`, and upgrade it in place to the tested set. Check the same VMs run the new versions, then destroy. | as above, with `<dir>` passing `version_overrides` through |
| `make e2e-workload-cluster CLASS=<class> VERSION=<v> UPGRADE=<v>` | One cluster of a ClusterClass through every check and add-on but Rancher ([steps](#the-workload-cluster-test)). | a running management cluster (`KUBECONFIG`) set up with etcd restores. The class's VM templates (on Talos of both releases). Go, kubectl, jq, curl, openssl, python3, ssh-keygen, and talosctl on Talos. An address of the test machine the cluster's nodes reach (`KUBEHENGE_E2E_OIDC_ADDRESS`). A backup target of the management cluster's served from that address (`KUBEHENGE_E2E_BACKUP_TARGET`, with its CA, certificate and key in `KUBEHENGE_E2E_BACKUP_CA`, `_CERT` and `_KEY`). Settings in `KUBEHENGE_PROXMOX_*` and `KUBEHENGE_E2E_*` (`test/e2e/workload-cluster.sh`). |

### The integration test

`make integration` takes these steps:

- Install the providers' chart with the module's values (the module's tests hold the two to the same file) and Kubehenge's chart.
- For each distribution, start its providers for the first cluster and apply its templates once they serve. After the last cluster, remove them and scale the providers to none.
- Take a cluster through Kubehenge's controller (finalizer, credentials owned by the cluster, the add-ons' objects) and delete it. Its Proxmox VE objects (faked) are removed only once its last machine is gone.
- Apply the Gateway API release the controller carries. Every CRD is established and its admission policy refuses older and experimental CRDs.
- Find an RKE2 Gateway API chart by its HelmChart, before and after helm-controller's CRD is there, and wait until its release is installed.
- Render the RKE2 and k3s classes' Traefik configuration with its Gateway provider on, with and without the certificates add-on's default certificate.
- Act as the interface's account, which writes and deletes the credentials users type in and no other Secret.
- With Cluster API's Cluster and ClusterClass webhooks and ClusterClass controller beside the API server, as the management cluster runs them (`test/integration`):
  - every class passes Cluster API's checks
  - every CEL rule of every class gives its message, refused and passed, before the class's templates exist and after
  - each class's defaults are stored on a cluster made without them, and the rules on changes hold against them
  - the interface's requests go through the API server as its own account, for a cluster of every class with every add-on on
  - they create it as its form sends it, send changes with their dry-run plans, and delete it
  - every refusal of the management cluster's (a CEL rule, a schema, Kubehenge's own checks) reaches the user as its reason.

### The appliance test

`make e2e-appliance` takes these steps.

1. Install the appliance from a release archive with `appliance/install.sh`, on a certificate the Proxmox VE cluster's CA signed for its address.
2. Sign in with Proxmox VE users. A wrong password and a user without privileges are refused.
3. Set the management cluster up from the interface. A second setup and an upgrade from the shell are refused while it runs.
4. Offer placement by an HA rule to a cluster creator without `Sys.Audit` on `/`, who sees only the rule's name and nodes. An administrator reads it in full.
5. Reach the shared Headlamp behind the interface's sign-in. With the settings' `headlampActions` it offers the Helm app catalog and the Prometheus charts it ships. It refuses a chart from a path on the appliance, a Helm repository over HTTP and a port forward.
6. Log out of a cluster in it as its frontend does, which clears the token cookie, then open the cluster again from the interface's link. The logout is refused to those who do not see the cluster and to a request from another origin or none.
7. Back the management cluster's etcd up into an S3 store served on the node, refused to a user without privileges and with a wrong passphrase. A scheduled snapshot and one asked for are stored encrypted and listed.
8. Restore the newer snapshot from the appliance's shell with `kubehenge appliance`, which refuses a wrong passphrase as the interface does. A Secret made since is gone and the ClusterClasses are there.
9. Show the backups, list and take a snapshot and show the upgrades with `kubehenge appliance`. A user without privileges is refused in the interface's words.
10. Switch automatic upgrades of the management cluster on, refused with a wrong passphrase, which makes their Proxmox VE user. Switch them off, which removes it, then on again.
11. Have the updater read a release index signed with a key made for the run, served from the S3 store. It upgrades to a release whose interface does not start, rolls that back with the reason, then upgrades to the newer release.
12. Upgrade the appliance with `install.sh --upgrade`, under the running management cluster, to a release whose interface does not start. It rolls that back with the reason, then upgrades to the newest release and refuses an older one.
13. See that the management cluster runs what the first release applied, and upgrade it to what the newest one carries. A snapshot is still taken after that.
14. Remove the management cluster from the interface, refused unconfirmed or with a wrong passphrase. Its automatic upgrades' user goes with it and its etcd backups are switched off.
15. Remove the appliance and check that Proxmox VE is left exactly as it was.

### Memory and CPU figures

The workload and Rancher tests print each node's and namespace's memory and the CPU requested of what each node offers. They print it before the add-ons and with every add-on on. The workload test also prints the control plane's memory after each add-on.

The classes' memory figures come from these prints: the add-ons', what they add to a control plane, and the nodes' own (`kubehenge.dev/addon-memory`, `kubehenge.dev/addon-control-plane-memory` and `kubehenge.dev/node-memory`, [clusters.md](clusters.md#machines)). The workload test gives its control plane and worker the least the figures let them have. A figure a new add-on or release outgrows is raised in `charts/kubehenge/templates`.

The CPU figures (`kubehenge.dev/addon-cpu` and `kubehenge.dev/node-cpu`) are the requests of the pinned charts and distributions as Kubehenge configures them, and move with them. A figure that falls behind shows in the requests the tests print. The test's worker, given the fewest cores the figures allow, then leaves a pod pending.

### The workload cluster test

`make e2e-workload-cluster` takes one cluster through these steps, in this order.

#### Creating and checking the cluster

1. Create a cluster from a ClusterClass:
   - with `KUBEHENGE_E2E_NAMESPACE`, as the interface's account in that one of the interface's namespaces, which the k3s run uses
   - on Talos at the release `KUBEHENGE_E2E_TALOS_VERSION` names, which CI sets by default to the one before the templates' release
   - with its worker at the least memory and cores the class allows with every add-on but Rancher
   - trusting an OpenID issuer the test serves
   - hardened to the CIS benchmark, unless `KUBEHENGE_E2E_CIS=false`, the classes' default
   - with Pod Security Admission enforcing the restricted level, so every component Kubehenge installs runs under it.

   On Talos its talosconfig, read as the interface hands it out, holds an administrator's certificate that reaches every node through Talos's API.
2. Check with kube-bench that a control-plane node and a worker fail none of the benchmark's checks but the documented exceptions (`test/e2e/cis-benchmark.sh`). It is skipped on Talos, for which kube-bench has no benchmark, and on a cluster without the hardening.
3. Check that the issuer's ID tokens sign in with prefixed user and groups, while tokens for another audience or from another key do not. ServiceAccount tokens still work. Requests without credentials reach nothing but the health checks of kubeadm and Talos, again after the upgrade.
4. Check that the `kubernetes` Service and every pod take their addresses from the cluster's ranges.
5. Check that the chosen CNI (`KUBEHENGE_E2E_CNI`, else the distribution's own) runs and enforces a NetworkPolicy, with flannel off beside Cilium or Calico.
6. Check that the health page's identity sees a crash-looping pod as such and reads the API server's readiness. The logs identity reads that pod's last run. Neither may read Secrets, change anything or do the other's work.
7. Check the cluster's own Proxmox VE pool, user and token scope. Its nodes are initialised by the cloud controller manager, and its control-plane nodes tainted once its worker has joined. Its `KubehengeReady` condition is true and `MachinesStalled` false.
8. Check that the controller reads every machine's boot log as the interface asks for it.
   - That is cloud-init's output and the journals of cloud-init and the distribution's services, through the guest agent with `VM.GuestAgent.FileRead` alone. On Talos it is the kernel's and Talos services' logs through Talos's API.
   - The support bundle `kubehenge diagnose` writes holds the Cluster and those boot logs, but no Secret and none of the cluster's secrets the test knows.
9. Check that the controller's `/healthz` answers. Its metrics, verified against its CA, refuse a request without credentials and an account without `kubehenge-metrics-reader`, and answer one with it.
10. Check that Traefik serves a host name through an Ingress on the API address and every node. It also serves an HTTPRoute through its Gateway on the API address, which the Gateway reports.
11. Check that every object of the Gateway API is held once, and again after the upgrade.
    - One a Helm release's latest revision ships is marked as that release's, shipped by no other and of the release that chart ships.
    - One that no release ships is of Kubehenge's release or newer.
    - By the upgrade, RKE2's and k3s's own Gateway API chart has to be installed and ship every object.
12. Check that a Service of type `LoadBalancer` answers the test machine on an address from the cluster's load-balancer range. The load balancer is `KUBEHENGE_E2E_LOAD_BALANCER` (MetalLB unless kube-vip) on `KUBEHENGE_E2E_LOAD_BALANCER_ADDRESSES`, on k3s without servicelb beside it. Switch it off again. On k3s servicelb then answers for the Service once the control planes are replaced.
13. Create and delete a persistent volume.
14. On the k3s run (`KUBEHENGE_E2E_VOLUME_SNAPSHOTS`), switch volume snapshots on.
    - The CSI plugin keeps the cluster's token behind the copy proxy. The proxy runs from the controller's image under test, which the control-plane nodes pull from `KUBEHENGE_E2E_REGISTRY` meanwhile, and its configuration has no root@pam or password.
    - The cluster's pool holds stopped holder VMs, and its token has `VM.Clone`.
    - A VolumeSnapshot of a volume with data is ready. A volume restored from it and a clone of the volume hold the same data. The three copies are in the proxy's log and on the holder VMs.
    - The proxy refuses a copy of a node VM's disk, with a `VolumeCopyFailed` event and nothing left behind.
    - Deleting the snapshot removes its copy from its holder VM. Deleting the claims then leaves no copy or record.
15. Switch volume snapshots off. The plugin reaches Proxmox VE directly with the cluster's token again. Its snapshotter sidecar, the proxy, the copies' namespace, the empty holder VMs, the VolumeSnapshotClass and the snapshot controller are gone.

#### Add-ons

1. Switch Headlamp on. Reachable through the API server, it has no rights of its own and lists no pods without a token. With read-only access for everyone it does, while its account still neither reads Secrets nor changes anything. Without that access it refuses again. Switch it off.
2. Check the shared Headlamp's sign-in identities. They are made only for clusters in the interface's namespaces (the k3s run's `KUBEHENGE_E2E_NAMESPACE` among them), and their issuer mints tokens for the sign-in account alone.
3. Pod Security Admission refuses a privileged pod. Once Kyverno is switched on, Kyverno refuses it in a namespace labelled privileged. Once Kyverno is off, it is admitted there again.
4. Switch Istio on and off before monitoring. That keeps the CNI's roll Istio sets off from landing on a worker already carrying the monitoring stack.
   - On, the pods of a namespace enrolled in the mesh are redirected through ztunnel. Once mutual TLS is required, a client outside the namespace is refused while one inside reaches the server, its workload identity in ztunnel's logs.
   - Off, the namespace is taken out of the mesh first. Both clients then reach the server again.
5. Switch monitoring on, then off with its metrics proxies. Every component's job is scraped, each with its server verified. On k3s the controller manager's, scheduler's and kube-proxy's series arrive in the kubelet's. On Talos the controller manager and scheduler are scraped only if the Talos release passes them serving certificates of the cluster CA.
6. Switch logs on beside monitoring.
   - Grafana's Loki data source is healthy, and Loki refuses a read without credentials.
   - The collector on every node sends a pod's lines, those of a pod ending within seconds, a node's journal and the API server's audit log.
   - On Talos, which keeps no journal, the node's logs are the kubelet's and the kernel's that Talos sends the collector. The audit log comes from `/var/log/audit/kube`.
   - Collecting through the Kubernetes API instead removes the collector's DaemonSet, and a pod's lines arrive once. Collection then returns to every node.
   - Switching monitoring off is refused while logs are on. Logs then go off, with Loki's volume kept.
7. Switch backups on to an S3 store the test serves with SeaweedFS over TLS.
   - The interface's two identities hold only their rights.
   - The schedule makes a backup of a namespace with a Secret and a volume, `kube-system` left out of it. Every object is under the cluster's prefix.
   - Its resources are unreadable without the cluster's SSE-C key, and its volume's data is nowhere in the store in plain text.
8. Restore and delete as the interface does, through its identities.
   - Delete the namespace and a second one of the backup, then restore the first alone with its Secret and the file on its volume.
   - A restore of Secrets alone leaves the cluster's Secret as it is. Its results, read through the link Velero signs (to the target, keyless, unreadable without the SSE-C key), say the Secret was kept.
   - A second backup is deleted from Velero and the store while the first stays.
   - A restore without the volumes' data and ConfigMaps brings back the Secret and the volume's claim, not the pod or the ConfigMap. A pod started on the claim finds a new, empty volume.
9. Check reading other clusters' backups through kubectl.
   - It is refused for the cluster itself and for a cluster with no keys beside it. A cluster backing up to a second target (`KUBEHENGE_E2E_BACKUP_OTHER_TARGET`) is refused this cluster's backups in the first.
   - It is allowed for a cluster deleted since whose keys are kept. Its backups get a read-only location in Velero with its SSE-C key, and its volumes' data reads through the shared Kopia password.
   - The location and its key go once the cluster no longer reads them.
10. Switch certificates on from the cluster's own CA. The CA is kept beside the Cluster, ECDSA P-256, constrained to the cluster's domain and to no address, and copied into the workload cluster. Its default certificate covers the domain and every name one level below it. It serves an Ingress that names no certificate and an HTTPRoute on the Gateway's HTTPS listener, both verified against that CA.
11. Then take certificates from an ACME server the test runs in the cluster (Pebble, with BIND answering for its challenges).
    - Over DNS-01 it uses the cluster's own DNS credential, an RFC 2136 TSIG key copied into the workload cluster. cert-manager checks its records at BIND alone. The default certificate is issued again from the ACME server. Headlamp at a name the default certificate does not cover gets its own, and Grafana at one it covers gets none.
    - Over HTTP-01 it goes through the Gateway at names the test gives the virtual IP in the cluster's DNS and BIND. Nothing outside the cluster resolves them. The default certificate, its Secret, the DNS credential and the HTTPS listener are gone. An Ingress with ingress-shim's default issuer and Headlamp are each served with their own.
12. Switch certificates off. The issuer and the Cluster's `kubehenge.dev/certificates` annotation are gone, and cert-manager with nothing left needing it. The cluster's CA is kept.
13. Switch GitOps on with Flux. It syncs a new commit of a private repository on a Git server inside the cluster, through the deploy key Kubehenge made.
14. Switch GitOps to Argo CD. Flux is uninstalled first, with its CRDs and its copy of the key, while what it synced keeps running. Argo CD, its local admin disabled, syncs a new commit, its application synced and healthy.

#### Upgrade, restore and teardown

1. On Talos, with every add-on on, upgrade every node in place to the release `KUBEHENGE_E2E_TALOS_UPGRADE` names, one at a time and the control plane first.
   - The machines stay the same, each drained and let back, none left cordoned.
   - Each node's kubelet and Talos's API report the new release, which `kubehenge.dev/talos-upgraded` records, with no `kubehenge.dev/talos-upgrade-failed`.
   - New machines still boot the release before until the next Kubernetes upgrade.
   - Every add-on's release is deployed, every workload is ready and every monitoring target is up again.
2. Switch every add-on off at once. Argo CD's CRDs and copy of the key go, while what it synced stays. Velero and its CRDs go, while the backups and the cluster's keys stay.
3. Take an etcd snapshot on request into the same S3 store as the backups.
   - It is the only object in the cluster's `etcd` folder, encrypted with age. A ConfigMap made before it is nowhere in the store in plain text.
   - `kubehenge etcd-snapshot decrypt` decrypts it with the cluster's key to the SHA-256 it was taken with. `etcdutl snapshot status` reads it, and it holds the ConfigMap.
   - On Talos, whose API does not tell it, it is recorded without a revision.
4. Restore etcd from it through the guest agent, on Talos through the Talos API.
   - The cluster is available again with the ConfigMap made before the snapshot back and one made after it gone.
   - Beside it the store holds only the snapshot the restore took first, encrypted and holding the later ConfigMap.
   - On Talos the control-plane machines are the same afterwards, each a voting member of the restored etcd again.
5. On RKE2 and k3s, switch the distribution's own upload on. The Secret `kubehenge-etcd-s3` in `kube-system` verifies the store's certificate, and the control planes are replaced. A snapshot the server takes itself is found in the cluster's `<distribution>-etcd` folder. Switching it off with the snapshots removes the Secret and replaces the control planes again.
6. Scale workers to two and back. On the k3s run it goes through the interface with `kubehenge cluster`, signed in with a Proxmox VE API token. It lists the cluster, fetches its kubeconfig and scales the pool to two. A worker is cordoned and uncordoned, rebooted and still the same machine, replaced, and deleted with its pool one smaller.
7. Upgrade by one minor version.
   - On RKE2 and k3s the old servers refuse the first new server until its cloud-init has ended. Its first start fails, and the roll survives it.
   - On Talos the new machines are cloned from the template of the release the nodes were upgraded to, and need no upgrade in place.
8. Check that every node runs the cluster's node OS (`KUBEHENGE_E2E_NODE_OS`, else the class's default) with SELinux enforcing, a read-only `/usr` and no denial in its logs. Talos, whose nodes run Talos, is skipped.
9. Replace a stopped worker, delete the cluster and check nothing is left.

#### Running half of it

`KUBEHENGE_E2E_PART` runs half of the test, for a machine whose memory holds the guests of only one half. CI runs each class's test this way, one half per machine.

- `lifecycle` (the `kubeadm`, `rke2`, `k3s` and `talos` tests) sizes the nodes at the least the class allows with the add-ons a cluster starts with (the load balancer and Rancher's agent). It switches on no other add-on.
- `addons` (`kubeadm-addons`, `rke2-addons`, `k3s-addons` and `talos-addons`) sizes them for every add-on. It leaves out the steps that replace a machine. Those are RKE2's and k3s's own upload, the load balancer off, scaling, the upgrade and the dead worker.

Both run the rest, on Talos the upgrade in place too, and check SELinux at their end.

## End to end without a Proxmox VE host

CI runs the end-to-end tier with Proxmox VE in a container (`.github/workflows/e2e.yml`), one test per machine.

- The merge queue runs the tests all the changes of a merge group need together (`test/e2e/ci/scope.sh`), on the tree that would land on main. It picks none for documentation alone and all of them for a change it cannot place.
- A pull request runs the other tiers and leaves this one to the queue.
- The nightly run (`test/e2e/ci/nightly.sh`) runs every test. It adds the management cluster's upgrade, Rancher, a control plane of three servers and a two-node Proxmox VE cluster.
- It runs both halves of each workload distribution's test once more on another CNI of its class and another node OS. That run goes without CIS hardening every other night, and the combination moves on so every one comes in turn.
- It builds Proxmox VE's image afresh and keeps it, with Go's build cache, for the day's runs.
- A dispatch of the workflow names the tests to run, or runs that night's set.

A machine whose KVM lacks pause-loop exiting hands its test to a fresh one, up to three times. There, guests that outnumber its CPUs starve each other whatever their sizes. The check that nothing restarts with every add-on on would only measure the machine. A test still on such a machine at the fourth try runs there with restarts reported, not failed (`KUBEHENGE_E2E_RESTARTS=report`).

### Running them yourself

Any Linux machine with Docker, `/dev/kvm` and about 16 GB of free memory can do the same:

```sh
scripts/tools.sh ~/.local/kubehenge-tools && export PATH=~/.local/kubehenge-tools:$PATH
test/e2e/ci/up.sh /tmp/e2e          # Proxmox VE in a container, a registry beside it
test/e2e/ci/run.sh /tmp/e2e rke2    # or management-cluster, management-cluster-upgrade, appliance, kubeadm, k3s, talos, kubeadm-addons, rke2-addons, k3s-addons, talos-addons, rancher, cluster-mesh, node-image-refresh, autoscaler, control-plane-ha, two-nodes
test/e2e/ci/run.sh /tmp/e2e kubeadm+calico+rocky-10+cis-off   # a workload test or its add-on half with its CNI, node OS or no CIS hardening
```

The control plane of three servers stays on k3s. Three k3s servers at the class's least of 4096 MiB fit beside a worker and the management cluster. RKE2's and kubeadm's need bigger workers too. A minor upgrade of three servers needs a fourth beside them while each is replaced, which the machine's memory does not hold.

The two Proxmox VE nodes share the machine, so they test placement, clones and a node leaving, not a node's hardware failing. Proxmox VE HA restarting VMs on another node is not run, since it fences a failed node through a watchdog, which a container lacks.

### What the machine records

Each try records what the machine and its VMs take every 15 seconds (`test/e2e/ci/guests.sh record`). That is the machine's memory, swap, disk and CPUs, and per VM the CPU time, resident memory and swap of its QEMU process. The log's "The guests' load" step sums it up per minute and gives each VM's busiest minute and most memory. A step that outgrows the machine shows there as a VM swapped out, or the CPUs of all of them above the machine's.

Under pressure the machine keeps its page cache rather than the guests' memory (`vm.swappiness=10`) and starts reclaiming earlier (`vm.watermark_scale_factor=200`). Cloning a template then evicts cached disk blocks instead of a running control plane.

Swap does not count as room for the guests. On a slower machine, a guest read back from swap stalls until its own checks fail, such as a container restarting with every add-on on. Every test's VMs therefore fit the machine's memory with room for its own processes, the management node at the module's default 4 GiB among them. The one exception is the RKE2 test's control-plane replacements. There three machines of RKE2's least 4 GiB beside the management node outgrow the memory by about 2 GiB.

### The guard

A guard (`test/e2e/ci/guard.sh`) keeps the CI agent's own processes out of the OOM killer's choice and their memory out of reclaim. It samples the machine's memory, swap, pressure and free disk every 5 seconds. It ends the test while the machine can still report, once any of these holds:

- memory and swap together fall under 1.5 GiB
- every task has waited on memory for half of the last minute
- the OOM killer has ended a process
- a file system has under 2 GiB free

It reports what held the memory (the VMs by name, the largest other processes, the last samples and the kernel's lines on memory). The test then fails like any other and keeps its artifacts. The log's "The runner's room" step gives how near each run came to these floors. It also gives the least memory available and how much was read back from swap.

### What a failed test leaves

When a test fails, the try's artifact holds the clusters' objects and pods' logs, with the earlier run of every container that restarted. It also holds what each running VM held through its guest agent (`guests.sh collect`):

- its state and pressure
- the distribution's services' journal and every line about etcd in it
- the logs of the static pods and of restarted containers as the kubelet keeps them
- the kernel's log
- etcd's members, status, health and metrics as the node sees them

A Talos VM's guest agent runs no commands, so a Talos VM is read through Talos's API with talosctl and its cluster's talosconfig:

- its stage, its services' states and events, its memory, pressure and processes and the kubelet's containers
- the logs of machined, apid, trustd, both containerd instances, the kubelet and etcd
- the API server's and restarted containers' logs as the kubelet keeps them
- the kernel's log
- on a control plane, etcd's members, status and alarms

A node whose API does not answer is asked once, and every call is cut off after a minute. These still read when a cluster's Kubernetes API no longer answers. Tokens and passwords are redacted from them, and the talosconfigs stay out of them.

### The Proxmox VE container

`up.sh` builds `test/e2e/pve`, a one-node Proxmox VE on Debian 13 that runs on the machine's own kernel and starts VMs on its KVM. Like every node it runs an SSH server and reaches itself over SSH as root, which `up.sh` checks. Proxmox VE copies a volume, such as a CSI volume snapshot, through SSH even within one node.

`up.sh` gives the VMs the bridge 172.20.99.0/24 with a name server that asks this machine's own, so they need no public one. It gives the containers 172.21.99.0/24, routes this machine to the bridge (with `sudo`) and writes the settings `run.sh` reads.

`run.sh` builds the controller from the working tree and pushes it to the registry. It applies the management cluster (`test/e2e/ci/main.tf`) with the container's CA trusted only by its own processes (`SSL_CERT_FILE`), not the machine's trust store. Meanwhile it creates the templates the test needs from the working tree's recipes. Each kubeadm, RKE2 or k3s build must hand out its boot log through its guest agent. Then it runs the test.

`management-cluster` runs the templates and volumes tests while the management cluster is up, before destroying it. For `appliance`, `run.sh` builds three releases instead and runs the test in the container, whose appliance sets the management cluster up itself.

For `two-nodes`, `run.sh` first joins a second node, `pve2`, from the same image to the first in a Proxmox VE cluster (`test/e2e/ci/second-node.sh`). The two VM bridges are joined at layer 2 through a Docker network of their own, the first keeping the gateway and name server. A directory both mount is their shared storage `shared`, which the management cluster and the templates then use.

Remove everything with `docker rm -f kubehenge-e2e-pve kubehenge-e2e-pve2 kubehenge-e2e-registry && docker network rm kubehenge-e2e kubehenge-e2e-vms` (the second node and network only where `two-nodes` ran) and `sudo ip route del 172.20.99.0/24`.
