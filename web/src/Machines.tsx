import { useState, type JSX } from "react";

import { api } from "./api";
import { BootLogDialog } from "./BootLog";
import { clusterPath } from "./ClusterDetail";
import { Link } from "./router";
import type { Cluster, Machine, PveNode } from "./types";
import { Status, age, filled, pollIdle, usePoll, type Health } from "./ui";

// machineStatus says how far a machine is, as the cluster list says it of clusters.
export function machineStatus(m: Machine): { state: Health; text: string } {
  if (m.ready) {
    return { state: "ok", text: "Ready" };
  }
  if (m.phase === "Failed") {
    return { state: "error", text: "Failed" };
  }
  if (m.phase === "Deleting") {
    return { state: "deleting", text: "Deleting" };
  }
  return { state: "progress", text: "Not ready" };
}

/**
 * crowding says which Proxmox VE node runs more than one of the cluster's control-plane VMs, and
 * what losing it costs, or undefined when none does.
 */
export function crowding(machines: Machine[]): string | undefined {
  const controlPlane = machines.filter((m) => m.role === "control-plane");
  const perNode = new Map<string, number>();
  for (const m of controlPlane) {
    if (filled(m.proxmoxNode)) {
      perNode.set(m.proxmoxNode, (perNode.get(m.proxmoxNode) ?? 0) + 1);
    }
  }
  const [node, count] = [...perNode.entries()].toSorted((a, b) => b[1] - a[1])[0] ?? ["", 0];
  if (count < 2) {
    return undefined;
  }
  // etcd needs a majority of the control-plane nodes.
  const cost = count * 2 > controlPlane.length ? "stops the control plane" : `takes ${count} down`;
  return `${node} runs ${count} of ${controlPlane.length} control-plane VMs, so losing it ${cost}. Migrate one to another node.`;
}

/** Machines is the Machines view: every machine of the cluster with where it runs and how it is. */
export function Machines({ cluster: c }: { cluster: Cluster }): JSX.Element {
  const machines = c.machines ?? [];
  const crowded = crowding(machines);
  const talos = c.distribution === "talos";
  const [bootLog, setBootLog] = useState<Machine>();
  const columns = [
    "Name",
    "Role",
    "Proxmox VE node",
    "Status",
    "Phase",
    // Beside the Talos release, the version is named for what it is.
    talos ? "Kubernetes" : "Version",
    ...(talos ? ["Talos"] : []),
    "Addresses",
    "Age",
    // Boot logs carry secrets often enough, so they go to those the admin kubeconfig goes to.
    ...(c.allowed.kubeconfig ? ["Logs"] : []),
  ];
  return (
    <div className="box scroll">
      {bootLog !== undefined && <BootLogDialog cluster={c} machine={bootLog} onClose={() => setBootLog(undefined)} />}
      {crowded !== undefined && <p className="warning">{crowded}</p>}
      <table className="grid">
        <thead>
          <tr>
            {columns.map((h) => (
              <th key={h}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {machines.length === 0 && (
            <tr>
              <td colSpan={columns.length} className="muted">
                No machines yet
              </td>
            </tr>
          )}
          {machines.map((m) => {
            const s = machineStatus(m);
            return (
              <tr key={m.name}>
                <td>
                  {m.node ?? m.name}
                  {filled(m.waiting) && <div className="hint message">{m.waiting}</div>}
                </td>
                <td>{m.role === "control-plane" ? "Control plane" : "Worker"}</td>
                <td>{m.proxmoxNode ?? ""}</td>
                <td>
                  <Status state={s.state} text={s.text} />
                </td>
                <td>{m.phase}</td>
                <td>{m.version ?? ""}</td>
                {talos && <td>{m.talos ?? ""}</td>}
                <td>{(m.addresses ?? []).join(", ")}</td>
                <td title={new Date(m.created).toLocaleString()}>{age(m.created)}</td>
                {c.allowed.kubeconfig && (
                  <td>
                    <button
                      type="button"
                      aria-label={`Boot log of ${m.node ?? m.name}`}
                      title="What the node logged as it booted: cloud-init and the kubelet, RKE2 or k3s, or on Talos its kernel and services"
                      onClick={() => setBootLog(m)}
                    >
                      Boot log
                    </button>
                  </td>
                )}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

const unplaced = "";

const rolesFirst = (a: Machine, b: Machine): number =>
  Number(a.role === "worker") - Number(b.role === "worker") || a.name.localeCompare(b.name);

interface Host {
  name: string;
  online: boolean | undefined;
  machines: Machine[];
}

// hosts are the Proxmox VE nodes with the cluster's machines on them, then the others the user
// sees, empty, as places a new machine may go; machines whose node the user may not see last.
function hosts(machines: Machine[], nodes: PveNode[] | undefined): Host[] {
  const byName = new Map<string, Host>();
  for (const n of nodes ?? []) {
    byName.set(n.node, { name: n.node, online: n.online, machines: [] });
  }
  for (const m of machines) {
    const name = m.proxmoxNode ?? unplaced;
    const host = byName.get(name) ?? { name, online: undefined, machines: [] };
    host.machines.push(m);
    byName.set(name, host);
  }
  for (const h of byName.values()) {
    h.machines.sort(rolesFirst);
  }
  return [...byName.values()].toSorted(
    (a, b) =>
      Number(a.name === unplaced) - Number(b.name === unplaced) ||
      Number(a.machines.length === 0) - Number(b.machines.length === 0) ||
      a.name.localeCompare(b.name),
  );
}

const countedRoles = (machines: Machine[]): string => {
  const cp = machines.filter((m) => m.role === "control-plane").length;
  const workers = machines.length - cp;
  const parts = [
    ...(cp > 0 ? [`${cp} control plane${cp === 1 ? "" : "s"}`] : []),
    ...(workers > 0 ? [`${workers} worker${workers === 1 ? "" : "s"}`] : []),
  ];
  return parts.join(", ");
};

// hostState is a node tile's line: whether it is offline, and which of the cluster's machines it runs.
function hostState(h: Host): string {
  const roles = countedRoles(h.machines);
  if (h.online === false) {
    return roles === "" ? "Offline" : `Offline, ${roles}`;
  }
  return roles === "" ? "Empty" : roles;
}

/**
 * Placement shows where a cluster's machines run: one tile per Proxmox VE node, each machine with
 * its role and state, so losing a node shows what it takes with it.
 */
export function Placement({ cluster: c }: { cluster: Cluster }): JSX.Element {
  // Only those who see the nodes get them; the others see the nodes of their machines alone.
  const { data: nodes } = usePoll(api.nodes, pollIdle);
  const machines = c.machines ?? [];
  const crowded = crowding(machines);
  const list = hosts(machines, nodes);
  return (
    <section className="box placement" aria-label="Placement">
      <h3>Placement</h3>
      {crowded !== undefined && <p className="warning">{crowded}</p>}
      {machines.length === 0 && <p className="muted">No machines yet</p>}
      <ul className="hosts" aria-label="Proxmox VE nodes">
        {list.map((h) => (
          <li
            key={h.name}
            className={`host${h.machines.length === 0 ? " host-empty" : ""}${h.online === false ? " host-offline" : ""}`}
          >
            <div className="host-head">
              <span className="host-name">{h.name === unplaced ? "Hidden node" : h.name}</span>
              <span className="host-count">{hostState(h)}</span>
            </div>
            {h.machines.length > 0 && (
              <ul className="host-machines">
                {h.machines.map((m) => {
                  const s = machineStatus(m);
                  return (
                    <li key={m.name}>
                      <Link
                        to={clusterPath(c.namespace, c.name, "machines")}
                        className="host-machine"
                        label={`${m.node ?? m.name}, ${m.role === "control-plane" ? "control plane" : "worker"}, ${s.text}`}
                      >
                        <span className={`dot dot-${s.state}`} aria-hidden="true" />
                        <span className="host-machine-name">{m.node ?? m.name}</span>
                        <span className="host-machine-role">{m.role === "control-plane" ? "CP" : "W"}</span>
                      </Link>
                    </li>
                  );
                })}
              </ul>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}
