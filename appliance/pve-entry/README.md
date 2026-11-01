# Kubernetes entry for Proxmox VE's interface

An optional extra: a **Kubernetes** button beside Create VM and Create CT, and a **Kubernetes** item under Datacenter. Both open Kubehenge's interface in a new tab. Kubehenge works the same without it.

Proxmox VE has no supported way to extend its interface, so the entry adds a line loading `kubehenge-entry.js` to `pve-manager`'s page template. A `pve-manager` update replaces that template. An apt hook (`DPkg::Post-Invoke`) puts the line back after every package run and logs it (`journalctl -t kubehenge-pve-entry`).

A Proxmox VE release may change the template so the line no longer fits, or rename the interface classes the script extends. The entry then stays out, the interface works as shipped and the browser's console says why.

Every node serves its own interface. Run the installer from `share/kubehenge/pve-entry/` in the release archive as root on each node:

```sh
sh install.sh --url https://<appliance>   # add the entry, or change the address
sh install.sh --remove                    # take everything out again
```

`appliance/install.sh` asks whether to add it on the node it runs on. The installed copy is `/usr/local/sbin/kubehenge-pve-entry`.
