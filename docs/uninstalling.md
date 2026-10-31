# Uninstalling

Work through these steps in order. The management cluster cleans up the Proxmox VE objects of the clusters it created. The appliance holds the only copy of the management cluster's state.

1. [Delete the clusters](#delete-the-clusters).
2. [Remove the management cluster](#remove-the-management-cluster).
3. [Remove the appliance](#remove-the-appliance).
4. [Remove the Proxmox VE interface entry](#remove-the-proxmox-ve-interface-entry), where it was added.

## Delete the clusters

Delete every cluster from its page (**Delete**, then type its name), or with kubectl:

```sh
kubectl delete clusters.cluster.x-k8s.io --all --all-namespaces
```

Deleting a cluster deletes its VMs and persistent volumes, and takes it out of Rancher where it was registered. Kubehenge's controller then removes its Proxmox VE pool, user, token and volume owner VM ([details](clusters.md#delete)). A cluster is gone when it no longer shows in the list.

Deleting the cluster that runs Rancher takes every cluster registered there out of Rancher first.

## Remove the management cluster

### In the appliance

On the Administration page, **Remove Kubehenge** takes the state passphrase and the word `remove`. It refuses while any cluster exists. It also refuses while a run of the management cluster is going on. Try again once that run has ended. A wrong passphrase stops it before anything is deleted.

#### What it does

It then:

- deletes the VM templates in the template pool, `kubehenge-templates`.
- switches automatic upgrades of the management cluster off ([upgrading.md](upgrading.md#automatically-after-kubehenge)). It deletes what the appliance kept for them and the user `kubehenge-upgrader@pve` with its token.
- destroys the management cluster with OpenTofu, with every Proxmox VE object its module made ([below](#what-the-module-made)).
- switches the [backups of its etcd](install.md#back-up-the-management-clusters-etcd) off and deletes the access key and talosconfig the appliance kept for them. The snapshots stay in the target.

The page follows the log. The appliance stays, back at its first run with the last settings filled in, ready to set up again. The emptied state is deleted. The next setup chooses a new passphrase. Removal needs the same [privileges](install.md#who-may-set-it-up) on `/` as the setup.

### With Terraform/OpenTofu

Delete the clusters first, and the templates in the template pool (`kubehenge template delete`, or delete the VMs), since Proxmox VE keeps a pool that still holds VMs. Then:

```sh
tofu destroy
```

Destroy removes every Proxmox VE object the module made ([below](#what-the-module-made)). The `proxmox_objects` output lists them, to check against afterwards.

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
- the groups: `kubehenge-clusters`, which every cluster's user joins, and `kubehenge-clusters-audit`, for the HA group labels, unless they were switched off for every cluster.
- the roles, each named `kubehenge-…`, and the permissions that grant them.

## Remove the appliance

Remove the management cluster first. Without the appliance, the management cluster's state is gone. Everything the setup made then has to be found and deleted by hand ([the list](install.md#losing-the-passphrase)).

In the shell of the node the appliance runs on, as root:

```sh
pct stop <ctid>
pct destroy <ctid> --purge
```

The appliance is the container tagged `kubehenge`. `--purge` also takes it out of backup jobs, replication jobs and HA.

## Remove the Proxmox VE interface entry

On every node where the Kubernetes entry was added (when installing the appliance, or by the entry's own installer), as root:

```sh
sh /usr/local/sbin/kubehenge-pve-entry --remove
```

This takes the line out of Proxmox VE's page template. It removes the entry's script, its setting, the apt hook and the installed copy itself. The node's interface returns to its packaged state. Reload Proxmox VE's interface in the browser afterwards.

## What stays on Proxmox VE

After these steps, Kubehenge leaves behind only what it did not create for itself:

- **The Debian container template** the installer downloaded to the `local` storage. `pveam list local` shows it, `pveam remove` deletes it.
- **Backups of the appliance** in your backup storages. They hold the management cluster's state, encrypted with the state passphrase.
- **API tokens you made** for Terraform/OpenTofu or for `kubehenge template create`. Remove them with `pveum user token remove`.
- **ACLs and roles you added** to share clusters, on the workload pool, storages and bridges.
- **Templates outside the template pool**, if you created any with another `--pool`.
- **Redirect URLs registered with identity providers** for the interface's OpenID sign-in and the shared Headlamp.
- **The storages' content types** you enabled for Kubehenge, such as `import` on `local`.
