# Kubernetes entry for Proxmox VE's interface

An optional extra: a **Kubernetes** button beside Create VM and Create CT, and a **Kubernetes** item under Datacenter. Both open Kubehenge's interface in a new tab. Kubehenge works the same without it.

## How it works

Proxmox VE has no supported way to extend its web interface. The entry adds one line to the page template that `pve-manager` ships. The line loads `kubehenge-entry.js`.

A `pve-manager` update replaces that template. An apt hook (`DPkg::Post-Invoke`) checks after every package run. When an update removed the line, it puts it back and logs it (`journalctl -t kubehenge-pve-entry`).

If a Proxmox VE release changes the template so the line no longer fits, the entry stays out and the interface works as shipped. The same happens if a release renames the interface classes the script extends. The browser's console says why.

## Installing

Every node serves its own interface, so run the entry's installer as root on each node. It is in `share/kubehenge/pve-entry/` of the release archive:

```sh
sh install.sh --url https://<appliance>   # add the entry, or change the address
sh install.sh --remove                    # take everything out again
```

`appliance/install.sh` asks whether to add it on the node the appliance is installed from. The installed copy lives at `/usr/local/sbin/kubehenge-pve-entry`.
