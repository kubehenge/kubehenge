import { useState, type JSX } from "react";

import { api } from "./api";
import { clusterPath } from "./cluster-paths";
import { Link } from "./router";
import type { Cluster, Machine, PveNode } from "./types";
import {
  Detail,
  Dialog,
  ErrorBox,
  RowMenu,
  Skeleton,
  Status,
  age,
  errorMessage,
  filled,
  pollCache,
  pollIdle,
  usePoll,
  type Health,
} from "./ui";

// The Proxmox VE nodes, kept for every view of every cluster that places machines on them.
const nodesCache = pollCache<PveNode[]>();
const nodesKept = { cache: nodesCache, key: "nodes" };

// machineStatus says how far a machine is, as the cluster list says it of clusters. A machine on a
// Proxmox VE node that is offline is down whatever Cluster API last read of it.
export function machineStatus(m: Machine, offline = false): { state: Health; text: string } {
  if (offline) {
    return { state: "error", text: "Node offline" };
  }
  if (m.phase === "Deleting") {
    return { state: "deleting", text: "Deleting" };
  }
  if (m.replacing === true) {
    return { state: "progress", text: filled(m.waiting) ? "Replacement waits" : "Replacing" };
  }
  if (m.ready) {
    return { state: "ok", text: "Ready" };
  }
  if (m.phase === "Failed") {
    return { state: "error", text: "Failed" };
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

/**
 * Machines is the Machines view: every machine of the cluster with where it runs and how it is, and
 * a machine's replacement for those who may change the cluster.
 */
export function Machines({
  cluster: c,
  can,
  why,
  onChanged,
}: {
  cluster: Cluster;
  can: boolean;
  why: string | undefined;
  onChanged: () => void;
}): JSX.Element {
  const { data: nodes } = usePoll(api.nodes, pollIdle, nodesKept);
  const [replacing, setReplacing] = useState<Machine>();
  const [keepError, setKeepError] = useState<string>();
  const offline = new Set((nodes ?? []).filter((n) => !n.online).map((n) => n.node));
  const machines = c.machines ?? [];
  const crowded = crowding(machines);
  const talos = c.distribution === "talos";
  const keep = async (m: Machine): Promise<void> => {
    setKeepError(undefined);
    try {
      await api.keepMachine(c.namespace, c.name, m.name);
      onChanged();
    } catch (err) {
      setKeepError(errorMessage(err));
    }
  };
  const columns = [
    "Name",
    "Role",
    "Proxmox VE node",
    "Status",
    // Beside the Talos release, the version is named for what it is.
    talos ? "Kubernetes" : "Version",
    ...(talos ? ["Talos"] : []),
    "Addresses",
    "Age",
  ];
  return (
    <div className="box scroll">
      {crowded !== undefined && <p className="warning">{crowded}</p>}
      <ErrorBox message={keepError} />
      <table className="grid">
        <thead>
          <tr>
            {columns.map((h) => (
              <th key={h}>{h}</th>
            ))}
            <th>
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {machines.length === 0 && (
            <tr>
              <td colSpan={columns.length + 1} className="muted">
                {c.machines === undefined ? <Skeleton /> : "No machines yet"}
              </td>
            </tr>
          )}
          {machines.map((m) => {
            const s = machineStatus(m, offline.has(m.proxmoxNode ?? ""));
            return (
              <tr key={m.name}>
                <td>
                  <span className="nowrap">{m.node ?? m.name}</span>
                  {m.replacing !== true && filled(m.waiting) && <div className="hint message">{m.waiting}</div>}
                </td>
                <td className="nowrap">{m.role === "control-plane" ? "Control plane" : "Worker"}</td>
                <td>{m.proxmoxNode ?? ""}</td>
                <td>
                  <Status state={s.state} text={s.text} />
                  {m.replacing === true && filled(m.waiting) && <Detail>{m.waiting}</Detail>}
                </td>
                <td>{m.version ?? ""}</td>
                {talos && <td>{m.talos ?? ""}</td>}
                <td>{(m.addresses ?? []).join(", ")}</td>
                <td title={new Date(m.created).toLocaleString()}>{age(m.created)}</td>
                <td className="row-action">
                  <RowMenu label={`Actions for ${m.node ?? m.name}`}>
                    {m.replacing === true && m.phase !== "Deleting" ? (
                      <button
                        type="button"
                        disabled={!can}
                        title={why}
                        aria-label={`Keep ${m.node ?? m.name}`}
                        onClick={() => {
                          void keep(m);
                        }}
                      >
                        Keep
                      </button>
                    ) : (
                      <button
                        type="button"
                        className="danger"
                        disabled={!can || m.phase === "Deleting"}
                        title={why}
                        aria-label={`Replace ${m.node ?? m.name}`}
                        onClick={() => setReplacing(m)}
                      >
                        Replace…
                      </button>
                    )}
                  </RowMenu>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {replacing !== undefined && (
        <ReplaceDialog
          cluster={c}
          machine={replacing}
          onClose={() => setReplacing(undefined)}
          onDone={() => {
            setReplacing(undefined);
            onChanged();
          }}
        />
      )}
    </div>
  );
}

function ReplaceDialog({
  cluster: c,
  machine: m,
  onClose,
  onDone,
}: {
  cluster: Cluster;
  machine: Machine;
  onClose: () => void;
  onDone: () => void;
}): JSX.Element {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const replace = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.replaceMachine(c.namespace, c.name, m.name);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={`Replace ${m.node ?? m.name}`}
      onClose={onClose}
      onSubmit={() => {
        void replace();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={busy}>
            Replace
          </button>
        </>
      }
    >
      <p>
        {m.role === "control-plane"
          ? "Deletes the VM and its disks, then adds a new control-plane node. Waits while etcd would lose its majority."
          : "Drains the node, deletes the VM and its disks, then adds a new worker."}
      </p>
      <ErrorBox message={error} />
    </Dialog>
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
  const { data: nodes } = usePoll(api.nodes, pollIdle, nodesKept);
  const machines = c.machines ?? [];
  const crowded = crowding(machines);
  const list = hosts(machines, nodes);
  return (
    <section className="box placement" aria-label="Placement">
      <h3>Placement</h3>
      {crowded !== undefined && <p className="warning">{crowded}</p>}
      {c.machines === undefined && <Skeleton rows={3} shape="tiles" />}
      {c.machines?.length === 0 && <p className="muted">No machines yet</p>}
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
                  const s = machineStatus(m, h.online === false);
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
