import { api } from "./api";
import type { Attention, Cluster, FleetHealth, Machine, PveNode } from "./types";
import { pollCache, pollIdle, usePoll } from "./ui";

/** nodesKept keeps the Proxmox VE nodes for every view of every cluster that places machines on them. */
export const nodesKept = { cache: pollCache<PveNode[]>(), key: "nodes" };

/** useProxmoxNodes reads the Proxmox VE nodes, as every cluster view shares them. */
export const useProxmoxNodes = (): PveNode[] | undefined => usePoll(api.nodes, pollIdle, nodesKept).data;

/** offlineOf are the names of the Proxmox VE nodes that are offline. */
export const offlineOf = (nodes: PveNode[] | undefined): Set<string> =>
  new Set((nodes ?? []).filter((n) => !n.online).map((n) => n.node));

const placed = (c: Cluster, role: Machine["role"], offline: Set<string>): { ready: number; lost: number } => {
  const machines = (c.machines ?? []).filter((m) => m.role === role);
  return {
    ready: machines.filter((m) => m.ready && !offline.has(m.proxmoxNode ?? "")).length,
    lost: machines.filter((m) => m.ready && offline.has(m.proxmoxNode ?? "")).length,
  };
};

/**
 * readyCounts say how many of a cluster's control-plane nodes and workers are ready. A machine on a
 * Proxmox VE node that is offline is down whatever Cluster API last read of it.
 */
export function readyCounts(c: Cluster, offline: Set<string>): { controlPlane: number; workers: number } {
  return {
    controlPlane: Math.max(0, c.controlPlane.ready - placed(c, "control-plane", offline).lost),
    workers: Math.max(0, c.workers.ready - placed(c, "worker", offline).lost),
  };
}

/**
 * nodeFindings are what control-plane nodes lost to an offline Proxmox VE node mean for a cluster, as
 * the items its health lists. The server lists the offline node itself, and cannot tell which
 * machines of the cluster ran on it.
 */
export function nodeFindings(c: Cluster, offline: Set<string>): Attention[] {
  const ready = readyCounts(c, offline).controlPlane;
  return placed(c, "control-plane", offline).lost > 0 && ready < c.controlPlane.desired
    ? [
        {
          severity: "error",
          section: "machines",
          what: `${ready} of ${c.controlPlane.desired} control-plane nodes ready`,
        },
      ]
    : [];
}

/** withNodeFindings adds a cluster's node findings to what its health found, ahead of it. */
export function withNodeFindings(
  c: Cluster,
  found: FleetHealth | undefined,
  offline: Set<string>,
): FleetHealth | undefined {
  const extra = nodeFindings(c, offline);
  if (extra.length === 0) {
    return found;
  }
  const base: FleetHealth = found ?? {
    namespace: c.namespace,
    name: c.name,
    read: new Date().toISOString(),
    verdict: "degraded",
    attention: [],
  };
  return { ...base, attention: [...extra, ...base.attention] };
}
