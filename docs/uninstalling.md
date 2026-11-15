# Uninstalling

Work through these steps in order. The management cluster cleans up the Proxmox VE objects of the clusters it created. The appliance holds the only copy of the management cluster's state.

1. [Delete the clusters](#delete-the-clusters).
2. [Remove the management cluster](#remove-the-management-cluster).
3. [Remove the appliance](#remove-the-appliance).
4. [Remove the Proxmox VE interface entry](#remove-the-proxmox-ve-interface-entry), where it was added.

## Delete the clusters

Delete every cluster from its page (**Delete**, then type its name), or with kubectl:

```sh
kubectl annotate clusters.cluster.x-k8s.io --all --all-namespaces kubehenge.dev/allow-deletion=true
kubectl delete clusters.cluster.x-k8s.io --all --all-namespaces
```

The annotation lifts the guard that refuses deleting a cluster with kubectl alone ([details](clusters.md#delete-with-kubectl-or-gitops)).

Deleting a cluster deletes its VMs and persistent volumes and takes it out of Rancher where it was registered. Kubehenge's controller then removes its Proxmox VE pool, user, token and volume owner VM ([details](clusters.md#delete)). A cluster is gone when it no longer shows in the list.

Deleting the cluster that runs Rancher takes every cluster registered there out of Rancher first.

## Remove the management cluster

### In the appliance

On the Administration page, **Remove Kubehenge** takes the state passphrase and the word `remove`. It refuses while any cluster exists or a run of the management cluster is going on. A wrong passphrase stops it before anything is deleted. It then:

- deletes the VM templates in the template pool, `kubehenge-templates`.
- switches automatic upgrades of the management cluster off ([upgrading.md](upgrading.md#automatically-after-kubehenge)). It deletes what the appliance kept for them and the user `kubehenge-upgrader@pve` with its token.
- destroys the management cluster with OpenTofu, with every Proxmox VE object its module made ([below](#what-the-module-made)). It lifts the protection of the management VMs first.
- switches the [backups of its etcd](install-settings.md#back-up-the-management-clusters-etcd) off and deletes the access key and talosconfig the appliance kept for them. The snapshots stay in the target.
- keeps the backup volume: the VM `kubehenge-backups`, tagged `backups`, with its one disk. It is protected and sits in no pool. Ask for everything to go (`removeBackups`) and the removal lifts its protection and deletes it too.

A removal that stopped part way can be run again. When the management cluster no longer answers, the removal goes on if Proxmox VE holds none of its VMs (the pool `kubehenge-management`). Otherwise it names the VMs and refuses. Start them, or remove them in Proxmox VE after `qm set <vmid> --protection 0` on each.

### After removal in the appliance

The page follows the log. The appliance stays and returns to its first run with the last settings filled in. The state is deleted, so the next setup chooses a new passphrase. Removal needs the same [privileges](install.md#who-may-set-it-up) on `/` as the setup.

### With Terraform/OpenTofu

Delete the clusters first. Then delete the templates in the template pool (`kubehenge template delete`, or delete the VMs), because Proxmox VE refuses to delete a pool that still holds VMs.

The management VMs are protected, so Proxmox VE refuses to destroy them. Lift it on each VM in the pool `kubehenge-management`, in the shell of its node as root, then destroy:

```sh
qm set <vmid> --protection 0
tofu destroy
```

Destroy removes every Proxmox VE object the module made ([below](#what-the-module-made)). The `proxmox_objects` output lists them, to check against afterwards.

The backup volume's VM is protected, so Proxmox VE refuses to destroy it. To keep the backups, take it out of the state first:

```sh
tofu state rm proxmox_virtual_environment_vm.backups
tofu destroy
```

To delete them too, set `backup_volume = { protection = false }`, run `tofu apply`, then `tofu destroy`. To delete a kept volume later, in the shell of its node as root:

```sh
qm set <vmid> --protection 0
qm destroy <vmid>
```

### What the module made

The names below are the defaults. The users, groups and roles start with the management cluster's name, `kubehenge` unless another was set.

- the management cluster's VMs, and the Talos disk image on each node.
- the pools: `kubehenge-management` for the management cluster, `kubehenge` for the workload clusters and `kubehenge-templates` for the VM templates.
- the users, each with its API token:
  - `kubehenge-cluster-api@pve` for Cluster API
  - `kubehenge-provisioner@pve` for Kubehenge's controller
  - `kubehenge-ha@pve` where Proxmox VE HA is on
  - `kubehenge-etcd-restore@pve` where etcd restores are on
  - `kubehenge-template-builder@pve` where the weekly template build is on
- the groups: `kubehenge-clusters`, which every cluster's user joins, and `kubehenge-clusters-audit`, for the HA rule labels, unless they were switched off for every cluster.
- the roles, each named `kubehenge-…`, and the permissions that grant them.

## Remove the appliance

Remove the management cluster first. Without the appliance its state is gone. Everything the setup made then has to be found and deleted by hand ([the list](#starting-over)).

In the shell of the node the appliance runs on, as root:

```sh
pct set <ctid> --protection 0
pct stop <ctid>
pct destroy <ctid> --purge
```

The appliance is the container tagged `kubehenge`. The installer protects it, so Proxmox VE refuses to destroy it until `--protection 0`. `--purge` also takes it out of backup jobs, replication jobs and HA.

## Starting over

For a lost state passphrase, or to set up again from nothing:

1. Delete every cluster in the interface and wait until the management cluster has cleaned up their Proxmox VE objects.
2. In Proxmox VE, delete what the setup made:
   - the VMs in the pools `kubehenge-management` (the management cluster) and `kubehenge-templates` (the VM templates), then the pools `kubehenge-management`, `kubehenge-templates` and `kubehenge`. The management VMs are protected: run `qm set <vmid> --protection 0` on each first.
   - the users `kubehenge-cluster-api@pve`, `kubehenge-provisioner@pve` and those of the settings that were on. Their API tokens go with them.
     - `kubehenge-template-builder@pve` (template refresh)
     - `kubehenge-ha@pve` (Proxmox VE HA)
     - `kubehenge-etcd-restore@pve` (etcd restores)
     - `kubehenge-upgrader@pve` (automatic upgrades of the management cluster)
   - the groups `kubehenge-clusters` and, where HA rule labels were on, `kubehenge-clusters-audit`.
   - the roles named `kubehenge-…`.
   - the Talos disk image, `talos-<version>-<id>-nocloud-amd64.qcow2`, on the Talos image storage of each node that ran the management cluster.
3. [Remove the appliance](#remove-the-appliance) and install it again.

## Remove the Proxmox VE interface entry

On every node where the Kubernetes entry was added (when installing the appliance, or by the entry's own installer), as root:

```sh
sh /usr/local/sbin/kubehenge-pve-entry --remove
```

This takes the line out of Proxmox VE's page template and removes the entry's script, its setting, the apt hook and the installed copy itself. Reload Proxmox VE's interface in the browser afterwards.

## What stays on Proxmox VE

These stay on Proxmox VE after removal:

- **The Debian container template** the installer downloaded, on the storage it named. `pveam list <storage>` shows it, `pveam remove` deletes it.
- **Backups of the appliance** in your backup storages. They hold the management cluster's state, encrypted with the state passphrase.
- **API tokens you made** for Terraform/OpenTofu or for `kubehenge template create`. Remove them with `pveum user token remove`.
- **ACLs and roles you added** to share clusters, on the workload pool, storages and bridges.
- **Templates outside the template pool**, if you created any with another `--pool`.
- **Redirect URLs registered with identity providers** for the interface's OpenID sign-in and the shared Headlamp.
- **The storages' content types** you enabled for Kubehenge, such as `import` on `local`.
