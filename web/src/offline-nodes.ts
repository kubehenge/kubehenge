import { useEffect } from "react";

import { api } from "./api";
import type { Attention, Cluster, FleetHealth, Machine, PveNode } from "./types";
import { pollCache, pollIdle, usePoll } from "./ui";

/** nodesKept keeps the Proxmox VE nodes for every view of every cluster that places machines on them. */
export const nodesKept = { cache: pollCache<PveNode[]>(), key: "nodes" };

// Until the appliance is set up there are no clusters, and no nodes to ask for.
const noNodes = async (): Promise<PveNode[]> => [];

/** useProxmoxNodes reads the Proxmox VE nodes, as every cluster view shares them, once `on`. */
export function useProxmoxNodes(on = true): PveNode[] | undefined {
  const { data, reload } = usePoll(on ? api.nodes : noNodes, pollIdle, on ? nodesKept : undefined);
  useEffect(() => {
    if (on) {
      void reload();
    }
  }, [on, reload]);
  return data;
}

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
 * nodeFindings are what an offline Proxmox VE node or a control-plane node that is not ready means
 * for a cluster, as the items its health lists, so that no view calls such a cluster well.
 */
export function nodeFindings(c: Cluster, offline: Set<string>): Attention[] {
  const machines = c.machines ?? [];
  const down = [...offline].filter((n) => machines.some((m) => m.proxmoxNode === n));
  const found: Attention[] = down.map((n) => ({
    severity: "error",
    section: "machines",
    what: `Node ${n} offline`,
    message: `${machines.filter((m) => m.proxmoxNode === n).length} machines run on it.`,
  }));
  const short = c.controlPlane.desired - readyCounts(c, offline).controlPlane;
  if (short > 0) {
    found.push({
      severity: "error",
      section: "machines",
      what: `${c.controlPlane.desired - short} of ${c.controlPlane.desired} control-plane nodes ready`,
    });
  }
  return found;
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
