# Uninstalling

Remove Kubehenge in this order. Each step needs the one before it: the management cluster cleans up after the clusters it created, and the appliance holds the only copy of the management cluster's state.

1. [Delete the clusters](#delete-the-clusters).
2. [Remove the management cluster](#remove-the-management-cluster).
3. [Remove the appliance](#remove-the-appliance).
4. [Remove the Proxmox VE interface entry](#remove-the-proxmox-ve-interface-entry), where it was added.

## Delete the clusters

Delete every cluster from its page (**Delete**, then type its name), or with kubectl:

```sh
kubectl delete clusters.cluster.x-k8s.io --all --all-namespaces
```

Deleting a cluster deletes its VMs and persistent volumes, and removes it from Rancher first. Once its VMs are gone, Kubehenge's controller removes the cluster's pool, its Proxmox VE user and API token, and the VM that owned its volumes, deleting the volumes on other nodes' local storage first. It waits while a node that may hold some of them is offline. A cluster is gone when it no longer shows in the list.

Deleting the cluster that runs Rancher first removes every registered cluster from Rancher.

## Remove the management cluster

### In the appliance

On the Administration page, **Remove Kubehenge** takes the state passphrase and the word `remove`. It refuses while any cluster exists, since their Proxmox VE objects are cleaned up by the management cluster, and waits while a run of the management cluster is going on. A wrong passphrase stops it before anything is deleted. It then:

- deletes the VM templates in the template pool, `kubehenge-templates`;
- destroys the management cluster with OpenTofu: its VMs, the Talos image, the three pools, the group for clusters' users, the roles, and the Cluster API and controller users with their API tokens.

The page follows the log. The appliance stays, back at its first run with the last settings filled in, ready to set up again; the emptied state is deleted, so the next setup chooses a new passphrase. Removal needs the same [privileges](install.md#who-may-set-it-up) on `/` as the setup.

### With Terraform/OpenTofu

Delete the clusters first, and the templates in the template pool (`kubehenge template delete`, or delete the VMs), since Proxmox VE keeps a pool that still holds VMs. Then:

```sh
tofu destroy
```

Destroy removes the management cluster's VMs, the Talos image, the pools, the group, the roles, and the Cluster API and controller users with their tokens. The `proxmox_objects` output lists what the module made, to check against afterwards.

## Remove the appliance

Remove the management cluster first. Without the appliance its state is gone, and whatever the management cluster made would have to be found and deleted by hand.

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

This takes the line out of Proxmox VE's page template, and removes the entry's script, its setting, the apt hook and the installed copy itself, returning the node's interface to its packaged state. Reload Proxmox VE's interface in the browser afterwards.

## What stays on Proxmox VE

After these steps, Kubehenge leaves behind only what it did not create for itself:

- **The Debian container template** the installer downloaded to the `local` storage; `pveam list local` shows it, and `pveam remove` deletes it.
- **Backups of the appliance** in your backup storages. They hold the management cluster's state, encrypted with the state passphrase.
- **API tokens you made** for Terraform/OpenTofu or for `kubehenge template create`: remove them with `pveum user token remove`.
- **ACLs and roles you added** to share clusters, on the workload pool, storages and bridges.
- **Templates outside the template pool**, if you created any with another `--pool`.
- **Redirect URLs registered with identity providers** for the interface's OpenID sign-in and the shared Headlamp.
- **The storages' content types** you enabled for Kubehenge, such as `import` on `local`.
