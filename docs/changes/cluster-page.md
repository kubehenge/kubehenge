### Changed

- Every dialog on the cluster page that stages a change says **Add to plan**, and the bar names each change with its numbers, such as `Workers: 4 to 60`.
- **Review plan** lists the same checks as the dialog: addresses, memory, offline nodes, removed APIs, webhooks and health. Applying asks to accept the risks.
- The Scale dialog shows spare addresses and free memory from the pools it would leave, as Review plan does.
- The cluster page counts ready nodes without those on offline Proxmox VE nodes. Its tabs wrap on a phone, and a tab's key hint no longer sits beside its count.
- A cluster with a VM on an offline Proxmox VE node is degraded in the list, the rail and the health page, with the node named as the first item.
- The default worker pool shows as **Workers**, the machines of a cluster are called machines throughout, and a node's name stays whole on the placement tiles.
- Dialogs dim the page behind them in both colour schemes. The Administration option reads **HA rule labels**.
- The demo lists its backup target in Administration and runs its controllers.
