# Management cluster

One Terraform/OpenTofu apply creates Kubehenge's management cluster on Proxmox VE. It is Talos Linux VMs running Kubernetes, with Cluster API and every provider Kubehenge uses installed and ready to create clusters.

## What it creates

### On Proxmox VE

- **Three pools,** for the management cluster's VMs, the clusters' VMs and the VM templates those clusters are cloned from.
- **A user and API token for Cluster API.**
  - It can audit nodes, manage VMs in the workload pool and the clusters' pools nested in it, and clone templates from the template pool.
  - It can also allocate disk space on the listed storages, use the listed bridges and write cloud-init ISOs to one storage.
  - It cannot see or touch the management cluster's own VMs.
- **A user and API token for Kubehenge's controller,** which may do only this:
  - create pools inside the workload pool and users in the clusters' group
  - create ACLs on those pools and the VMs that own clusters' volumes
  - move and configure VMs in the workload pool
  - use the listed storages as the clusters do (to delete a removed cluster's volumes on node-local storage)
- **A group for the clusters' own users,** allowed to use space on the listed storages and to read the nodes. Each cluster's user, created by the controller, adds rights on its own pool only ([architecture](../docs/architecture.md#each-clusters-own-corner-of-proxmox-ve)).
- **The Talos disk image,** built by the Talos Image Factory with the QEMU guest agent.
- **One, three or five VMs,** each a Talos control-plane node, with static addresses and a virtual IP in front of the Kubernetes API.
  - Their VM IDs are picked at random between 10000 and 99999, away from the lowest free ID. Proxmox VE offers that ID to anything else created at the same moment.

### Optional users and rights

- **`proxmox_ha`** adds a user and API token for Proxmox VE HA.
  - It holds `Sys.Audit` and `Sys.Console` on `/`, which together amount to a root shell on every node.
  - Only a short-lived Job holds it, for each write of the HA resources and rules that keep each cluster's control-plane VMs apart ([install settings](../docs/install-settings.md#proxmox-ve-ha)).
- **`cluster_datastore_allocate`** adds a role holding `Datastore.Allocate`, `Datastore.AllocateSpace` and `Datastore.Audit`.
  - The controller grants it to a cluster's user on the cluster's storage when the cluster runs the CSI plugin's newest release.
  - To grant it, the controller's token holds `Datastore.Allocate` on the listed storages the management cluster does not use ([clusters](../docs/clusters.md#persistent-volumes)).
- **`template_refresh`**, on by default, adds a user and API token for the weekly build of the VM templates.
  - It may create, prepare and delete VMs in the template pool and nothing outside it ([install settings](../docs/install-settings.md#what-the-template-builds-may-do)).
  - It may download and upload to the storages it builds with, and so delete any file there. Give it storages of their own with `import_datastore` and `iso_datastore`.
  - It may use its node's network for the download and its bridge.
  - The chart's CronJob runs `kubehenge template refresh` with it each week. Clusters' nodes move to the new builds in their maintenance windows once they are `soak_hours` old.
- **`cluster_sys_audit`**, on by default, adds a group holding `Sys.Audit` on `/`, unpropagated.
  - The controller adds a cluster's user to it for its nodes' HA group labels, unless the cluster switches them off. The controller's token holds `User.Modify` on the group to do so ([clusters](../docs/clusters.md#machines)).
  - The cluster's token can then read the Proxmox VE cluster's own configuration (firewall rules, backup jobs, HA, the corosync configuration). It sees nothing of its guests, storages or nodes.
- **`node_boot_logs`**, on by default, gives the controller's token `VM.GuestAgent.FileRead` on the workload pool.
  - With it the controller reads the boot logs of kubeadm, RKE2 and k3s nodes through their guest agents. It reads only the files the node's agent policy opens ([install settings](../docs/install-settings.md#nodes-boot-logs)).
- **`etcd_restore`** adds a user and API token for etcd restores.
  - It holds `VM.Audit` and `VM.GuestAgent.Unrestricted` on the workload pool, which together amount to a root shell in every cluster's VMs.
  - Only a short-lived Job holds it, for each step of a restore an authorised user asked for on the cluster's control-plane VMs ([clusters](../docs/clusters.md#restore-etcd)).

### In the cluster

- Flannel as the pod network and kube-network-policies to enforce NetworkPolicies.
- cert-manager.
- The Cluster API Operator, and through it:
  - Cluster API core (with the ClusterClass and Runtime SDK features on)
  - the Proxmox VE infrastructure provider (`capmox`)
  - the bootstrap and control-plane providers for kubeadm, RKE2, k3s and Talos (Talos's from Kubehenge's forks)
  - the in-cluster IP address manager
  - the Add-on Provider for Helm
- The `kubehenge` chart (`../charts/kubehenge`): Kubehenge's ClusterClasses, its controller, and NetworkPolicies that let nothing reach Kubehenge's pods but the controller's webhook port.

### Mirrors, OpenID Connect and verification

`registry_mirrors` points the nodes at image registry mirrors, for networks without direct access to the public registries. The nodes pull the Image Factory's installer, Sidero's images and everything from registry.k8s.io only with their publishers' keyless signatures. A mirror of those registries must serve the signatures too.

The Talos disk image the VMs boot from is downloaded from the Image Factory over TLS alone. The factory builds it again with different bytes and publishes no checksum or signature for it.

`cluster_oidc` lists OpenID Connect providers every new workload cluster's API server trusts unless the cluster names its own. Tokens and certificates keep working beside them.

`capmox` verifies the Proxmox VE API's certificate against the CA you give it. The nodes do not report to Sidero's public discovery service.

## Before you apply

- **Proxmox VE 9.** The storage named in `image_datastore` must allow the `import` content type (Datacenter, Storage, Edit, Content).
- **An API token with administrator rights**, for example `pveum user token add root@pam kubehenge --privsep 0`. The apply creates users, roles, pools and VMs.
- **Or an administrator's session.**
  - Leave `proxmox_api_token` unset.
  - Put a ticket and its CSRF token from `POST /access/ticket` in `PROXMOX_VE_AUTH_TICKET` and `PROXMOX_VE_CSRF_PREVENTION_TOKEN`, as Kubehenge's appliance does.
  - The apply must finish within the ticket's two hours.
- **The Proxmox VE CA certificate,** `/etc/pve/pve-root-ca.pem` on any node unless you replaced the API's certificate. The machine running Terraform/OpenTofu must trust it too (in its system trust store), since the Proxmox VE provider verifies the API's certificate.
- **Free addresses** for the nodes and the API's virtual IP, all in one subnet on the bridge.
- **Pod and service ranges that the network does not use.** The defaults are Kubernetes' own.
  - Pods use 10.244.0.0/16, services 10.96.0.0/12 (10.96.0.0 to 10.111.255.255).
  - If the nodes' subnet is inside either, set `pod_subnets` or `service_subnets`. The plan refuses overlapping ranges, since Talos ignores node addresses inside them.
- **Network access.** The machine running Terraform/OpenTofu reaches the nodes on ports 50000 (Talos API) and 6443 (Kubernetes API). The nodes reach the internet for images and provider manifests.

## Apply

```sh
cp terraform.tfvars.example terraform.tfvars   # then fill it in
export TF_VAR_proxmox_api_token='root@pam!kubehenge=...'
export TF_VAR_proxmox_ca_certificate="$(cat pve-root-ca.pem)"
tofu init -lockfile=readonly
tofu apply
tofu output -raw kubeconfig > kubeconfig
tofu output -raw talosconfig > talosconfig
```

The module installs the Cluster API Operator from its chart's archive in `charts/`, which a release's archive carries. In a checkout of the repository, `make capi-operator-chart` downloads it there first. The apply refuses an archive whose SHA-256 is not the one `versions.tf` names.

`-lockfile=readonly` holds the providers to the checksums in `.terraform.lock.hcl`. A root module of your own that calls this one needs a copy of that file beside it, since only the root module's is read. `terraform` works the same as `tofu`, from version 1.11.

The state holds the cluster's secrets and its administrator credentials. Keep it in an encrypted backend, or use OpenTofu's state encryption.

## Upgrade

Changing `node_cores` or `node_memory_mb` updates the VMs without rebooting them. Each node takes the new size at its next reboot. A Talos upgrade reboots one node at a time.

The versions of Talos, Kubernetes and every provider are pinned together in `versions.tf`, as the set Kubehenge has tested. A newer Kubehenge release raises them. Applying it upgrades Talos node by node (each drained first when there are several), Kubernetes component by component, and each provider through the operator.

`version_overrides` holds a component back below the tested set. `provider_images` runs a provider's controller from another image, keyed `"<kind>/<name>"` (`"InfrastructureProvider/proxmox"`), such as a registry mirror or a build with a fix its release lacks. The provider's manifests still come from its version.

capmox at its tested release runs Kubehenge's reproducible build of a later upstream commit (`capmox_build` in `versions.tf`, [releasing.md](../docs/releasing.md)).

## Remove

First delete the clusters the management cluster created, so their VMs go with them. Delete the templates in the template pool too (`kubehenge template delete`), since Proxmox VE keeps a pool that still holds VMs. Then `tofu destroy`. Destroy removes the management cluster's VMs, the Talos image, the pools, the group, the Cluster API, controller and template build users and their tokens.
