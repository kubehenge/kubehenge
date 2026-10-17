# Kubernetes entry for Proxmox VE's interface

An optional extra: a **Kubernetes** button beside Create VM and Create CT, and a **Kubernetes** item under Datacenter, both opening Kubehenge's appliance in a new tab. Kubehenge works the same without it.

Proxmox VE has no supported way to extend its web interface, so the entry adds one line to the page template that `pve-manager` ships, loading `kubehenge-entry.js`. A `pve-manager` update replaces that template; an apt hook (`DPkg::Post-Invoke`) puts the line back after every package run and logs it (`journalctl -t kubehenge-pve-entry`). If a Proxmox VE release changes the template so the line no longer fits, or renames the interface classes the script extends, the entry stays out and the interface works as shipped; the browser's console says why.

Every node serves its own interface, so run it as root on each node:

```sh
sh install.sh --url https://<appliance>   # add the entry, or change the address
sh install.sh --remove                    # take everything out again
```

`appliance/install.sh` asks whether to add it on the node the appliance is installed from. The installed copy lives at `/usr/local/sbin/kubehenge-pve-entry`.
