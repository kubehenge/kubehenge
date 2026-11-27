import { api } from "./api";
import type { PveNode } from "./types";
import { filled, pollCache, pollIdle, usePoll, type Poll } from "./ui";

/** nodesKept keeps the Proxmox VE nodes for every view of every cluster that places machines on them. */
export const nodesKept = { cache: pollCache<PveNode[]>(), key: "nodes" };

/** useProxmoxNodesRead reads the Proxmox VE nodes, as every cluster view shares them, with how the read went. */
export const useProxmoxNodesRead = (): Poll<PveNode[]> => usePoll(api.nodes, pollIdle, nodesKept);

/** useProxmoxNodes is the Proxmox VE nodes as read. */
export const useProxmoxNodes = (): PveNode[] | undefined => useProxmoxNodesRead().data;

/**
 * unreachable is whether Proxmox VE lists a node as online while a call the appliance forwards to it
 * fails. Its VMs keep running.
 */
export const unreachable = (n: PveNode): boolean => !n.online && filled(n.fault);

/** offlineOf are the names of the Proxmox VE nodes that Proxmox VE itself lists as down. */
export const offlineOf = (nodes: PveNode[] | undefined): Set<string> =>
  new Set((nodes ?? []).filter((n) => !n.online && !unreachable(n)).map((n) => n.node));
