import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { addressRoom, freeMemory } from "./Autoscaling";
import { sizeOf } from "./MachineFields";
import { poolSize } from "./pool-machine";
import { Link } from "./router";
import type { Cluster, ClusterHealth, Effect, Plan, PveNode, UpgradeChecks } from "./types";
import { Detail, Status, bytes, errorMessage, localDates } from "./ui";

// What the machines go through, by the most a change does.
const replaces: Record<Effect, string> = {
  none: "Replaces no node",
  scales: "Adds or removes nodes",
  "control-plane": "Replaces the control-plane nodes, one at a time",
  workers: "Replaces the workers, one at a time",
  all: "Replaces every node, one at a time",
};
const effectOrder: Effect[] = ["none", "scales", "workers", "control-plane", "all"];

/** PlanRead is what the API server makes of a version, checked without being made. */
export interface PlanRead {
  version: string;
  plan?: Plan;
  error?: string;
}

/** usePlan has the API server check an upgrade to version, unless it is the version running. */
export function usePlan(c: Cluster, version: string): PlanRead | undefined {
  const [read, setRead] = useState<PlanRead>();
  const { namespace, name, generation } = c;
  const running = c.version;
  useEffect(() => {
    let current = true;
    async function check(): Promise<void> {
      try {
        const plan = await api.plan({ namespace, name, generation }, { version });
        if (current) {
          setRead({ version, plan });
        }
      } catch (err) {
        if (current) {
          setRead({ version, error: errorMessage(err) });
        }
      }
    }
    if (version !== running) {
      void check();
    }
    return () => {
      current = false;
    };
  }, [namespace, name, generation, running, version]);
  return read?.version === version ? read : undefined;
}

// info is a fact about the change that is neither a pass nor a problem, so it gets no green tick.
type CheckState = "ok" | "info" | "warning" | "error" | "progress";

/** Check is one line of what to know before a change goes ahead. */
export interface Check {
  what: string;
  state: CheckState;
  text: string;
  /** The findings behind a warning, one per line. */
  detail?: string[];
}

/** useUpgradeChecks reads what the version would run into inside the cluster. */
function useUpgradeChecks(c: Cluster, version: string | undefined): UpgradeChecks | string | undefined {
  const [read, setRead] = useState<{ version: string; checks: UpgradeChecks | string }>();
  const { namespace, name } = c;
  useEffect(() => {
    let current = true;
    async function load(to: string): Promise<void> {
      try {
        const checks = await api.upgradeChecks(namespace, name, to);
        if (current) {
          setRead({ version: to, checks });
        }
      } catch (err) {
        if (current) {
          setRead({ version: to, checks: errorMessage(err) });
        }
      }
    }
    if (version !== undefined) {
      void load(version);
    }
    return () => {
      current = false;
    };
  }, [namespace, name, version]);
  return version !== undefined && read?.version === version ? read.checks : undefined;
}

// isoDateLength is the part of a timestamp that names the day.
const isoDateLength = 10;

function removedAPIsCheck(checks: UpgradeChecks | string | undefined): Check {
  const what = "Removed APIs";
  if (checks === undefined) {
    return { what, state: "progress", text: "Checking" };
  }
  if (typeof checks === "string") {
    return { what, state: "warning", text: "Not checked", detail: [checks] };
  }
  if (checks.removedAPIs.length > 0) {
    return {
      what,
      state: "warning",
      text: `${checks.removedAPIs.length} in use`,
      detail: [
        ...checks.removedAPIs.map(
          (a) =>
            `${a.api}, removed in ${a.removedIn}${a.seen === undefined ? "" : `, last requested ${localDates(a.seen.slice(0, isoDateLength))}`}`,
        ),
        ...(checks.basis === "gauge" ? ["No request counts, so every use since start counts."] : []),
      ],
    };
  }
  const unread = checks.unchecked.filter((u) => !u.startsWith("the admission webhooks"));
  return unread.length > 0
    ? { what, state: "warning", text: "Not checked", detail: unread }
    : { what, state: "ok", text: "None in use" };
}

function webhooksCheck(checks: UpgradeChecks | string | undefined): Check | undefined {
  const what = "Webhooks";
  if (checks === undefined) {
    return { what, state: "progress", text: "Checking" };
  }
  if (typeof checks === "string") {
    return undefined;
  }
  if (checks.webhooks.length > 0) {
    return {
      what,
      state: "warning",
      text: `${checks.webhooks.length} at risk`,
      detail: checks.webhooks.map((w) => `${w.name}: Service ${w.service} ${w.why}`),
    };
  }
  const unread = checks.unchecked.filter((u) => u.startsWith("the admission webhooks"));
  return unread.length > 0
    ? { what, state: "warning", text: "Not checked", detail: unread }
    : { what, state: "ok", text: "None at risk" };
}

function changeCheck(read: PlanRead | undefined): Check {
  if (read === undefined) {
    return { what: "Change", state: "progress", text: "Checking" };
  }
  if (read.plan === undefined) {
    return { what: "Change", state: "error", text: read.error ?? "Refused" };
  }
  const effects = read.plan.changes.map((ch) => effectOrder.indexOf(ch.effect));
  return { what: "Change", state: "info", text: replaces[effectOrder[Math.max(0, ...effects)] ?? "none"] };
}

// A rollout makes each new node before it removes the old one, so it needs one address spare.
function addressCheck(c: Cluster): Check | undefined {
  const room = addressRoom(c);
  if (room === undefined) {
    return undefined;
  }
  // The pools are what a plan changes, where the workers' status is what the cluster has now.
  const workers = c.workerPools.reduce((sum, p) => sum + (p.autoscaling?.max ?? p.replicas), 0);
  const spare = room - c.controlPlane.desired - workers;
  return spare > 0
    ? { what: "Addresses", state: "ok", text: `${spare} spare` }
    : { what: "Addresses", state: "error", text: "None spare. Add one under Options." };
}

function healthCheck(health: ClusterHealth | string | undefined): Check {
  if (health === undefined) {
    return { what: "Health", state: "progress", text: "Checking" };
  }
  if (typeof health === "string") {
    return { what: "Health", state: "warning", text: health };
  }
  if (health.verdict === "unreachable") {
    return { what: "Health", state: "error", text: "Unreachable" };
  }
  const errors = health.attention.filter((a) => a.severity === "error").length;
  const warnings = health.attention.filter((a) => a.severity === "warning").length;
  if (errors + warnings === 0) {
    return { what: "Health", state: "ok", text: "No problems" };
  }
  const count = errors + warnings;
  return {
    what: "Health",
    state: errors > 0 ? "error" : "warning",
    text: `${count} ${count === 1 ? "problem" : "problems"}`,
  };
}

const bytesPerMiB = 1_048_576;

// The memory the machines a change adds need, in MiB.
function addedMiB(before: Cluster, after: Cluster): number {
  const controlPlane = sizeOf(after.variables["controlPlaneMachine"])?.memoryMiB ?? 0;
  const added = Math.max(0, after.controlPlane.desired - before.controlPlane.desired) * controlPlane;
  return after.workerPools.reduce((sum, p) => {
    const was = before.workerPools.find((x) => x.name === p.name)?.replicas ?? 0;
    return sum + Math.max(0, p.replicas - was) * (poolSize(after, p)?.memoryMiB ?? 0);
  }, added);
}

// memoryCheck compares the memory new machines need with what the nodes have free. It is advice:
// memory frees and fills as other VMs come and go.
function memoryCheck(before: Cluster, after: Cluster, nodes: PveNode[]): Check | undefined {
  const wanted = addedMiB(before, after) * bytesPerMiB;
  const free = freeMemory(after, nodes);
  if (!(wanted > 0) || free === undefined) {
    return undefined;
  }
  return wanted > free
    ? { what: "Memory", state: "warning", text: `Needs ${bytes(wanted)}, ${bytes(free)} free` }
    : { what: "Memory", state: "ok", text: `${bytes(free)} free` };
}

// nodesCheck names the Proxmox VE nodes that run the cluster's machines and are offline.
function nodesCheck(c: Cluster, nodes: PveNode[]): Check | undefined {
  if (nodes.length === 0) {
    return undefined;
  }
  const placed = c.machines ?? [];
  const down = nodes.filter((n) => !n.online && placed.some((m) => m.proxmoxNode === n.node));
  if (down.length === 0) {
    return { what: "Nodes", state: "ok", text: "All online" };
  }
  const machines = placed.filter((m) => down.some((n) => n.node === m.proxmoxNode)).length;
  return {
    what: "Nodes",
    state: "error",
    text: `${down.map((n) => n.node).join(", ")} offline, ${counted(machines, "machine")}`,
  };
}

const counted = (n: number, what: string): string => `${n} ${what}${n === 1 ? "" : "s"}`;

/**
 * scaleChecks are the addresses and memory a change of the cluster's size needs, from the same
 * figures in the Scale dialog as in Review plan.
 */
export function scaleChecks(before: Cluster, after: Cluster, nodes: PveNode[]): Check[] {
  return [addressCheck(after), memoryCheck(before, after, nodes)].filter((ch) => ch !== undefined);
}

/**
 * useChecks reads what to know before a change goes ahead: what it replaces, whether the nodes have
 * the addresses and memory it needs, the removed APIs and fail-closed webhooks an upgrade meets,
 * the offline nodes, and what is wrong with the cluster already. The dialog of a change and the
 * plan that applies it show the same list. Nothing is read while it is off.
 */
export function useChecks({
  before,
  after = before,
  version,
  read,
  change = false,
  on = true,
}: {
  before: Cluster;
  /** The cluster as the change would leave it. */
  after?: Cluster;
  /** The version to read the removed APIs and webhooks for, unless the change sets none. */
  version?: string | undefined;
  /** What the API server makes of the version, for the Change line that a dialog shows. */
  read?: PlanRead | undefined;
  change?: boolean;
  on?: boolean;
}): Check[] {
  const found = useUpgradeChecks(before, on ? version : undefined);
  const [health, setHealth] = useState<ClusterHealth | string>();
  const [nodes, setNodes] = useState<PveNode[]>([]);
  const { namespace, name } = before;
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const h = await api.health(namespace, name);
        if (current) {
          setHealth(h);
        }
      } catch (err) {
        if (current) {
          setHealth(errorMessage(err));
        }
      }
      try {
        const listed = await api.nodes();
        if (current) {
          setNodes(listed);
        }
      } catch {
        // Without the nodes there is no offline or memory line, and the cluster shows a VM that found no room.
      }
    }
    if (on) {
      void load();
    }
    return () => {
      current = false;
    };
  }, [namespace, name, on]);
  const optional = (check: Check | undefined): Check[] => (check === undefined ? [] : [check]);
  return [
    ...(change ? [changeCheck(read)] : []),
    ...scaleChecks(before, after, nodes),
    ...optional(nodesCheck(before, nodes)),
    ...(version === undefined ? [] : [removedAPIsCheck(found), ...optional(webhooksCheck(found))]),
    healthCheck(health),
  ];
}

/** needsAccepting is whether a list holds a problem that goes ahead only on the admin's say. */
export const needsAccepting = (checks: Check[]): boolean =>
  checks.some((ch) => ch.state === "warning" || ch.state === "error");

/** ChecksList shows the checks, a problem with its findings behind Detail. */
export function ChecksList({
  checks,
  healthPath,
  label,
}: {
  checks: Check[];
  healthPath: string;
  label: string;
}): JSX.Element {
  return (
    <dl className="preflight" aria-label={label}>
      {checks.map((ch) => (
        <div key={ch.what}>
          <dt>{ch.what === "Health" ? <Link to={healthPath}>Health</Link> : ch.what}</dt>
          <dd>
            <Status state={ch.state === "info" ? "idle" : ch.state} text={ch.text} />
            {ch.detail !== undefined && (
              <Detail>
                <ul>
                  {ch.detail.map((line) => (
                    <li key={line}>{line}</li>
                  ))}
                </ul>
              </Detail>
            )}
          </dd>
        </div>
      ))}
    </dl>
  );
}

/**
 * Accept is the box to tick before a change goes ahead that met a problem or replaces nodes, as the
 * command line asks.
 */
export function Accept({
  required,
  accepted,
  onChange,
}: {
  required: boolean;
  accepted: boolean;
  onChange: (accepted: boolean) => void;
}): JSX.Element | null {
  if (!required) {
    return null;
  }
  return (
    <label className="accept">
      <input type="checkbox" checked={accepted} onChange={(e) => onChange(e.target.checked)} />
      Accept the risks
    </label>
  );
}
