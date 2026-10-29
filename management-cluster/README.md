# Management cluster

One Terraform/OpenTofu apply creates Kubehenge's management cluster on Proxmox VE: Talos Linux VMs running Kubernetes, with Cluster API and every provider Kubehenge uses installed and ready to create clusters.

## What it creates

On Proxmox VE:

- **Three pools.** One for the management cluster's own VMs, one for the VMs of the clusters it creates, and one for the VM templates those clusters are cloned from.
- **A user and API token for Cluster API,** with only what it needs: it can audit nodes, manage VMs in the workload pool and the clusters' pools nested in it, clone templates from the template pool, allocate disk space on the listed storages, use the listed bridges, and write cloud-init ISOs to one storage. It cannot see or touch the management cluster's own VMs.
- **A user and API token for Kubehenge's controller,** which may create pools inside the workload pool, users in the clusters' group, ACLs on those pools and the VMs that own clusters' volumes, move and configure VMs in the workload pool, and use the listed storages as the clusters do (to delete a removed cluster's volumes on node-local storage), and nothing else.
- **With `proxmox_ha`, a user and API token for Proxmox VE HA,** holding `Sys.Audit` and `Sys.Console` on `/`, which amounts to a root shell on every node; the controller uses it only to keep each cluster's control-plane VMs apart with HA resources and rules ([install](../docs/install.md#proxmox-ve-ha)).
- **With `cluster_datastore_allocate`, a role holding `Datastore.Allocate`, `Datastore.AllocateSpace` and `Datastore.Audit`,** which the controller grants a cluster's user on the cluster's storage when the cluster runs the CSI plugin's newest release; the controller's token holds `Datastore.Allocate` on the listed storages the management cluster does not use, to grant it ([clusters](../docs/clusters.md#persistent-volumes)).
- **With `template_refresh` on, as it is by default, a user and API token for the weekly build of the VM templates,** which may create, prepare and delete VMs in the template pool, download and upload to the storages it builds with (and so delete any file there: give it storages of their own with `import_datastore` and `iso_datastore`), and use its node's network for the download and its bridge; nothing outside the template pool ([install](../docs/install.md#what-the-template-builds-may-do)). The chart's CronJob runs `kubehenge template refresh` with it each week, and clusters' nodes move to the new builds in their maintenance windows once they are `soak_hours` old.
- **With `cluster_sys_audit` on, as it is by default, a group holding `Sys.Audit` on `/`, unpropagated,** which the controller adds a cluster's user to for its nodes' HA group labels, unless the cluster switches them off; it lets the cluster's token read the Proxmox VE cluster's own configuration (firewall rules, backup jobs, HA, the corosync configuration), nothing of its guests, storages or nodes. The controller's token holds `User.Modify` on that group, to add it ([clusters](../docs/clusters.md#machines)). Set `cluster_sys_audit = false` to give no cluster `Sys.Audit` on `/`.
- **With `node_boot_logs` on, as it is by default, `VM.GuestAgent.FileRead` on the workload pool for the controller's token,** with which it reads kubeadm, RKE2 and k3s nodes' boot logs through their guest agents: files only, those the node's agent policy opens ([install](../docs/install.md#nodes-boot-logs)). Set `node_boot_logs = false` to leave the token without it.
- **With `etcd_restore`, a user and API token for etcd restores,** holding `VM.Audit` and `VM.GuestAgent.Unrestricted` on the workload pool, which amounts to a root shell in every cluster's VMs; the controller uses it only to run a restore an authorised user asked for on the cluster's control-plane VMs ([clusters](../docs/clusters.md#restore-etcd)).
- **A group for the clusters' own users,** allowed to use space on the listed storages and to read the nodes. Each cluster's user, created by the controller, adds rights on its own pool only ([architecture](../docs/architecture.md#each-clusters-own-corner-of-proxmox-ve)).
- **The Talos disk image,** built by the Talos Image Factory with the QEMU guest agent.
- **One, three or five VMs,** each a Talos control-plane node, with static addresses and a virtual IP in front of the Kubernetes API.

In the cluster:

- cert-manager.
- The Cluster API Operator, and through it Cluster API core (with the ClusterClass and Runtime SDK features on), the Proxmox VE infrastructure provider (`capmox`), the bootstrap and control-plane providers for kubeadm, RKE2 and k3s (Talos's join with a Talos ClusterClass), the in-cluster IP address manager, and the Add-on Provider for Helm.
- The `kubehenge` chart (`../charts/kubehenge`): Kubehenge's ClusterClasses and its controller.

`registry_mirrors` points the nodes at image registry mirrors, for networks without direct access to the public registries. The nodes pull the Image Factory's installer, Sidero's images and everything from registry.k8s.io only with their publishers' keyless signatures, so a mirror of those registries serves the signatures too. The Talos disk image the VMs boot from is downloaded from the Image Factory over TLS alone: the factory builds it again with different bytes, and publishes no checksum or signature for it. `cluster_oidc` lists OpenID Connect providers every new workload cluster's API server trusts unless the cluster names its own; tokens and certificates keep working beside them. `capmox` verifies the Proxmox VE API's certificate against the CA you give it; upstream's default is to skip that check. The nodes do not report to Sidero's public discovery service.

`cluster_volume_snapshots` lets clusters take volume snapshots, with their CSI plugin signed in to Proxmox VE as `root@pam` ([clusters](../docs/clusters.md#volume-snapshots)). The module creates nothing on Proxmox VE for it and never holds the password, which would otherwise sit in the state. Kubehenge's appliance writes it into the management cluster; without the appliance, after the apply, run `kubectl -n kubehenge-system create secret generic kubehenge-root-pam --from-file=password=/dev/stdin`, type root@pam's password, Enter and Ctrl-D. Delete the Secret again when setting the variable back to false.

## Before you apply

- **Proxmox VE 9.** The storage named in `image_datastore` must allow the `import` content type (Datacenter, Storage, Edit, Content).
- **An API token with administrator rights** for this configuration, for example `pveum user token add root@pam kubehenge --privsep 0`. It creates users, roles, pools and VMs. Or an administrator's session instead: leave `proxmox_api_token` unset and put a ticket and its CSRF token from `POST /access/ticket` in `PROXMOX_VE_AUTH_TICKET` and `PROXMOX_VE_CSRF_PREVENTION_TOKEN`, as Kubehenge's appliance does; a ticket lasts two hours, so the apply must finish within that.
- **The Proxmox VE CA certificate,** `/etc/pve/pve-root-ca.pem` on any node unless you replaced the API's certificate. The machine running Terraform/OpenTofu must trust it too (add it to the system's trust store), since the Proxmox VE provider verifies the API's certificate.
- **Free addresses** for the nodes and the API's virtual IP, all in one subnet on the bridge.
- **Pod and service ranges that the network does not use.** The defaults are Kubernetes' own, 10.244.0.0/16 for pods and 10.96.0.0/12 (10.96.0.0 to 10.111.255.255) for services; if the nodes' subnet is inside either, set `pod_subnets` or `service_subnets`. The plan refuses overlapping ranges, since Talos ignores node addresses inside them.
- **Network access:** the machine running Terraform/OpenTofu reaches the nodes on ports 50000 (Talos API) and 6443 (Kubernetes API); the nodes reach the internet for images and provider manifests.

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

`-lockfile=readonly` holds the providers to the checksums in `.terraform.lock.hcl`. A root module of your own that calls this one needs a copy of that file beside it, since only the root module's is read. `terraform` works the same as `tofu`, from version 1.11.

The state holds the cluster's secrets and its administrator credentials. Keep it in an encrypted backend, or use OpenTofu's state encryption.

## Upgrade

Changing `node_cores` or `node_memory_mb` updates the VMs without rebooting them; each node takes the new size at its next reboot, which a Talos upgrade does one node at a time.

The versions of Talos, Kubernetes and every provider are pinned together in `versions.tf`, as the set Kubehenge has tested. A newer Kubehenge release raises them; applying it upgrades Talos node by node (each drained first when there are several), Kubernetes component by component, and each provider through the operator. `version_overrides` holds a component back below the tested set. capmox at its tested release runs Kubehenge's reproducible build of a later upstream commit (`capmox_build` in `versions.tf`, [releasing.md](../docs/releasing.md)). `provider_images` runs a provider's controller from another image, keyed `"<kind>/<name>"` (`"InfrastructureProvider/proxmox"`): a registry mirror, or a build carrying a fix its release does not have yet; the provider's manifests still come from its version.

## Remove

Delete the clusters the management cluster created first, so their VMs are removed with them, and the templates in the template pool (`kubehenge template delete`), since Proxmox VE keeps a pool that still holds VMs. Then `tofu destroy`. Destroy removes the management cluster's VMs, the Talos image, the pools, the group, the Cluster API, controller and template build users and their tokens.
