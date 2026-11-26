import { useEffect, useState, type JSX, type ReactNode } from "react";

import { api } from "./api";
import { autoscales, controlPlaneSizes, mostWorkers, variable } from "./create-form";
import type { StepProps } from "./CreateSteps";
import { addressCount, whole } from "./network";
import { useAutoscalerInCluster } from "./Placement";
import { poolSize, poolSuffix as suffixOf } from "./pool-machine";
import type {
  AutoscalerSettings,
  Autoscaling,
  ChangeRequest,
  Cluster,
  ClusterClass,
  PveNode,
  WorkerPool,
} from "./types";
import { Dialog, ErrorBox, Field, bytes, maxWorkers, shownNumber, typedNumber, stageLabel, useChange } from "./ui";
import { flag, quantity, record, text, texts } from "./values";

const bytesPerMiB = 1_048_576;

const fixedOrAutoscaled = (
  <>
    <option value="fixed">Fixed</option>
    <option value="autoscaled">Autoscaled</option>
  </>
);

/** WorkerFields are the create wizard's workers: a fixed number, or the fewest and the most the cluster autoscaler keeps. */
export function WorkerFields({
  form,
  set,
  shown,
  cls,
  growthWarning,
}: StepProps & { cls: ClusterClass | undefined; growthWarning?: string | undefined }): JSX.Element {
  const offered = variable(cls, "autoscaler") !== undefined;
  const auto = autoscales(form, cls);
  let leastHint: string | undefined;
  if (auto && form.workerReplicas === 0) {
    leastHint = "With 0, the pool empties when idle";
  } else if (mostWorkers(form) === 0) {
    leastHint = "With 0, the control planes run the workloads";
  }
  return (
    <>
      {offered && (
        <Field label="Worker count">
          {(id) => (
            <select
              id={id}
              value={auto ? "autoscaled" : "fixed"}
              onChange={(e) => set("workerAutoscale", e.target.value === "autoscaled")}
            >
              {fixedOrAutoscaled}
            </select>
          )}
        </Field>
      )}
      <Field label={auto ? "Fewest workers" : "Workers"} error={shown("workerReplicas")} hint={leastHint}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={0}
            max={maxWorkers}
            value={shownNumber(form.workerReplicas)}
            onChange={(e) => set("workerReplicas", typedNumber(e.target.value))}
          />
        )}
      </Field>
      {auto && (
        <Field
          label="Most workers"
          error={shown("workerMax")}
          hint={growthWarning ?? "Each needs its own node address"}
        >
          {(id) => (
            <input
              id={id}
              type="number"
              min={1}
              max={maxWorkers}
              value={shownNumber(form.workerMax)}
              onChange={(e) => set("workerMax", typedNumber(e.target.value))}
            />
          )}
        </Field>
      )}
    </>
  );
}

/** poolText is how a worker pool stands: its workers, and for an autoscaled one its bounds. */
export const poolText = (p: WorkerPool): string =>
  p.autoscaling ? `${p.replicas} of ${p.autoscaling.min} to ${p.autoscaling.max}, autoscaled` : String(p.replicas);

/** A pool as the Scale dialog edits it, its numbers kept as typed. */
export interface PoolEntry {
  autoscale: boolean;
  replicas: string;
  min: string;
  max: string;
}

// A pool switched to autoscaling starts able to double.
const growth = 2;

export const poolEntry = (p: WorkerPool): PoolEntry => ({
  autoscale: p.autoscaling !== undefined,
  replicas: String(p.replicas),
  min: String(p.autoscaling?.min ?? p.replicas),
  max: String(p.autoscaling?.max ?? Math.min(maxWorkers, Math.max(p.replicas, 1) * growth)),
});

const number = (s: string): number => (/^\d+$/u.test(s) ? Number(s) : Number.NaN);

const entryMost = (e: PoolEntry): number => number(e.autoscale ? e.max : e.replicas) || 0;

const entryLeast = (e: PoolEntry): number => number(e.autoscale ? e.min : e.replicas);

/** poolMost is the most workers a pool may run: an autoscaled pool may grow to its most. */
export const poolMost = (p: WorkerPool): number => p.autoscaling?.max ?? p.replicas;

// The add-ons that use memory on the nodes by the classes' figures, read from the variables. The
// server counts them exactly and refuses what they leave no room for. Logs need monitoring, and
// Rancher's agent holds no worker up.
function runsAddons(c: Cluster): boolean {
  const on = (name: string): boolean => flag(record(c.variables[name])["enabled"]) === true;
  const kyverno = text(record(c.variables["podSecurity"])["kyverno"]);
  return (
    on("headlamp") ||
    on("monitoring") ||
    on("istio") ||
    on("clusterMesh") ||
    on("backups") ||
    on("certificates") ||
    on("gitops") ||
    flag(record(c.variables["rancher"])["install"]) === true ||
    c.variables["loadBalancer"] !== undefined ||
    (kyverno !== undefined && kyverno !== "off")
  );
}

/** addressRoom is how many node addresses the cluster's network gives, unknown where it does not read. */
export function addressRoom(c: Cluster): number | undefined {
  const n = record(c.variables["network"]);
  const v6 = record(n["ipv6"]);
  // Each node takes one address of every family, so the family with the fewest addresses decides.
  const rooms = (
    [
      [texts(n["addresses"]) ?? [], text(n["gateway"]) ?? "", quantity(n["prefix"])],
      [texts(v6["addresses"]) ?? [], text(v6["gateway"]) ?? "", quantity(v6["prefix"])],
    ] as const
  )
    .filter(([entries]) => entries.length > 0)
    .map(([entries, gateway, prefix]) => {
      const counted = prefix === undefined ? undefined : addressCount(entries, gateway, prefix);
      return counted?.bad === undefined ? counted?.count : undefined;
    });
  const known = rooms.filter((r) => r !== undefined);
  return known.length === 0 || known.length < rooms.length ? undefined : Math.min(...known);
}

/** A pool's problem in the Scale dialog, on its fewest (or its fixed number) or its most. */
export interface PoolProblem {
  least?: string;
  most?: string;
}

interface Scaling {
  c: Cluster;
  controlPlane: number;
  entries: Record<string, PoolEntry>;
}

function entryProblem(s: Scaling, e: PoolEntry, kept: boolean): PoolProblem | undefined {
  const least = entryLeast(e);
  if (!whole(least, 0, maxWorkers)) {
    return { least: `A whole number from 0 to ${maxWorkers}` };
  }
  if (!e.autoscale) {
    return undefined;
  }
  if (!whole(number(e.max), Math.max(least, 1), maxWorkers)) {
    return { most: `A whole number from ${Math.max(least, 1)} to ${maxWorkers}` };
  }
  if (!kept && runsAddons(s.c)) {
    return { least: "Keep at least 1 for the add-ons" };
  }
  const need = s.controlPlane + 1 + Object.values(s.entries).reduce((n, x) => n + entryMost(x), 0);
  const wasNeed = (s.c.controlPlane.desired || 1) + 1 + s.c.workerPools.reduce((n, p) => n + poolMost(p), 0);
  const have = addressRoom(s.c);
  // As the server: a change may keep a shortage the cluster has, but not make it worse.
  if (have !== undefined && need > have && need > wasNeed) {
    return {
      most: `Needs ${need} node addresses, but there are ${have}. Enter a lower number.`,
    };
  }
  return undefined;
}

/**
 * poolProblems says, by pool, what keeps the Scale dialog's entries from being sent: numbers out of
 * range, an autoscaled pool that may go down to none under the add-ons, and, as the server checks
 * it, node addresses too few for every pool at its most.
 */
export function poolProblems(s: Scaling): Record<string, PoolProblem> {
  const kept = Object.values(s.entries).some((e) => entryLeast(e) > 0);
  const problems: Record<string, PoolProblem> = {};
  for (const [name, e] of Object.entries(s.entries)) {
    const p = entryProblem(s, e, kept);
    if (p !== undefined) {
      problems[name] = p;
    }
  }
  return problems;
}

/** roomFor is how many workers the node addresses leave each pool, with the others as entered. */
export function roomFor(s: Scaling): Record<string, number> {
  const have = addressRoom(s.c);
  const all = Object.values(s.entries).reduce((n, e) => n + entryMost(e), 0);
  const room: Record<string, number> = {};
  if (have !== undefined) {
    for (const [name, e] of Object.entries(s.entries)) {
      room[name] = Math.max(0, have - s.controlPlane - 1 - (all - entryMost(e)));
    }
  }
  return room;
}

function poolDiff(p: WorkerPool, e: PoolEntry): { replicas?: number; autoscaling?: Autoscaling | null } {
  if (e.autoscale) {
    const bounds = { min: number(e.min), max: number(e.max) };
    return p.autoscaling?.min === bounds.min && p.autoscaling.max === bounds.max ? {} : { autoscaling: bounds };
  }
  // Switched off, the pool keeps the workers it runs, unless another number was typed.
  const typed = e.replicas === String(p.replicas) ? {} : { replicas: number(e.replicas) };
  return p.autoscaling ? { autoscaling: null, ...typed } : typed;
}

/** poolChange is the change the Scale dialog's entries ask for, pool by pool, of what differs. Empty for none. */
export function poolChange(
  pools: WorkerPool[],
  entries: Record<string, PoolEntry>,
): Pick<ChangeRequest, "workerPools" | "autoscaling"> {
  const workerPools: Record<string, number> = {};
  const autoscaling: Record<string, Autoscaling | null> = {};
  for (const p of pools) {
    const e = entries[p.name];
    const d = e ? poolDiff(p, e) : {};
    if (d.replicas !== undefined) {
      workerPools[p.name] = d.replicas;
    }
    if (d.autoscaling !== undefined) {
      autoscaling[p.name] = d.autoscaling;
    }
  }
  return {
    ...(Object.keys(workerPools).length > 0 && { workerPools }),
    ...(Object.keys(autoscaling).length > 0 && { autoscaling }),
  };
}

/** freeMemory is the memory the nodes a cluster may use have free, in bytes, unknown without figures. */
export function freeMemory(c: Cluster, nodes: PveNode[]): number | undefined {
  const allowed = texts(c.variables["allowedNodes"]);
  const usable = nodes.filter((n) => n.online && (allowed === undefined || allowed.includes(n.node)));
  if (usable.length === 0 || usable.some((n) => n.maxMemory === undefined)) {
    return undefined;
  }
  return usable.reduce((sum, n) => sum + Math.max(0, (n.maxMemory ?? 0) - (n.memory ?? 0)), 0);
}

/** addedWorkers is how many more workers pool p of a changed cluster may run than it could before. */
export function addedWorkers(before: Cluster, p: WorkerPool): number {
  const was = before.workerPools.find((x) => x.name === p.name);
  return Math.max(0, poolMost(p) - (was === undefined ? 0 : poolMost(was)));
}

/** addedWorkersMiB is the memory the workers pool p of a changed cluster may add need, in MiB. */
export const addedWorkersMiB = (before: Cluster, after: Cluster, p: WorkerPool): number =>
  addedWorkers(before, p) * (poolSize(after, p)?.memoryMiB ?? 0);

/**
 * roomWarning warns when the workers an autoscaled pool may add by a change need more memory than
 * the nodes the cluster may use have free. It counts as the Memory check does, for the one pool. It
 * is advice: memory frees and fills as other VMs come and go.
 */
export function roomWarning(
  { before, after }: { before: Cluster; after: Cluster },
  pool: string,
  nodes: PveNode[],
): string | undefined {
  const p = after.workerPools.find((x) => x.name === pool);
  const free = freeMemory(after, nodes);
  if (p?.autoscaling === undefined || free === undefined) {
    return undefined;
  }
  const added = addedWorkers(before, p);
  const wanted = addedWorkersMiB(before, after, p) * bytesPerMiB;
  if (!(wanted > free)) {
    return undefined;
  }
  return `${added} more ${added === 1 ? "worker needs" : "workers need"} ${bytes(wanted)}, the nodes have ${bytes(free)} free`;
}

/** PoolFields edit one worker pool in the Scale dialog: a fixed number of workers, or autoscaled between two. */
export function PoolFields({
  pool: p,
  pools,
  entry: e,
  problem,
  warning,
  room,
  onChange,
}: {
  pool: WorkerPool;
  /** Every pool of the cluster, which decide how the pool is named. */
  pools: readonly WorkerPool[];
  entry: PoolEntry;
  problem: PoolProblem | undefined;
  warning: string | undefined;
  room: number | undefined;
  onChange: (e: PoolEntry) => void;
}): JSX.Element {
  const poolSuffix = (name: string): string => suffixOf(name, pools);
  return (
    <>
      <Field label={`Scaling${poolSuffix(p.name)}`}>
        {(id) => (
          <select
            id={id}
            value={e.autoscale ? "autoscaled" : "fixed"}
            onChange={(ev) => onChange({ ...e, autoscale: ev.target.value === "autoscaled" })}
          >
            {fixedOrAutoscaled}
          </select>
        )}
      </Field>
      {e.autoscale ? (
        <>
          <Field label={`Fewest workers${poolSuffix(p.name)}`} error={problem?.least}>
            {(id) => (
              <input
                id={id}
                type="number"
                min={0}
                max={maxWorkers}
                value={e.min}
                onChange={(ev) => onChange({ ...e, min: ev.target.value })}
              />
            )}
          </Field>
          <Field
            label={`Most workers${poolSuffix(p.name)}`}
            error={problem?.most}
            hint={room === undefined ? undefined : `Node addresses leave room for ${room}`}
          >
            {(id) => (
              <input
                id={id}
                type="number"
                min={1}
                max={maxWorkers}
                value={e.max}
                onChange={(ev) => onChange({ ...e, max: ev.target.value })}
              />
            )}
          </Field>
          {warning !== undefined && <p className="warning">{warning}</p>}
        </>
      ) : (
        <Field
          label={`Workers${poolSuffix(p.name)}`}
          error={problem?.least}
          hint={p.autoscaling ? "Keeps its current workers unless changed" : undefined}
        >
          {(id) => (
            <input
              id={id}
              type="number"
              min={0}
              max={maxWorkers}
              value={e.replicas}
              onChange={(ev) => onChange({ ...e, replicas: ev.target.value })}
            />
          )}
        </Field>
      )}
    </>
  );
}

/** usePveNodes reads the Proxmox VE nodes for the room warning. */
export function usePveNodes(): PveNode[] {
  const [nodes, setNodes] = useState<PveNode[]>([]);
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        setNodes(await api.nodes());
      } catch {
        // The warning is advice. Without the nodes the dialog gives none, and the cluster still
        // shows a VM that found no room.
      }
    }
    void load();
  }, []);
  return nodes;
}

const expanders = [
  ["least-waste", "Least unused room"],
  ["most-pods", "Most waiting pods"],
  ["random", "Random"],
] as const;

const defaultMinutes = 10;

const maxMinutes = 1440;

function settingsOf(v: unknown): Required<AutoscalerSettings> {
  const r = record(v);
  const expander = text(r["expander"]);
  return {
    placement: text(r["placement"]) === "workload" ? "workload" : "management",
    expander: expanders.find(([k]) => k === expander)?.[0] ?? "least-waste",
    scaleDownDelayAfterAddMinutes: quantity(r["scaleDownDelayAfterAddMinutes"]) ?? defaultMinutes,
    scaleDownUnneededMinutes: quantity(r["scaleDownUnneededMinutes"]) ?? defaultMinutes,
  };
}

/** autoscalerText sums up how the cluster autoscaler runs, for the Options view. */
export function autoscalerText(c: Cluster): string {
  const s = settingsOf(c.variables["autoscaler"]);
  const where = s.placement === "workload" ? "On the control planes" : "In the management cluster";
  const pools = c.workerPools.filter((p) => p.autoscaling).length;
  const scaled = pools === 0 ? "no pools" : `${pools} ${pools === 1 ? "pool" : "pools"}`;
  return `${where}, ${scaled}, scales down after ${s.scaleDownUnneededMinutes} min`;
}

function MinutesField({
  label,
  hint,
  value,
  onChange,
}: {
  label: string;
  hint: string;
  value: string;
  onChange: (v: string) => void;
}): JSX.Element {
  const problem = whole(number(value), 1, maxMinutes) ? undefined : `A whole number from 1 to ${maxMinutes}`;
  return (
    <Field label={label} error={problem} hint={hint}>
      {(id) => (
        <input
          id={id}
          type="number"
          min={1}
          max={maxMinutes}
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </Field>
  );
}

/** AutoscalerDialog tunes the cluster autoscaler and chooses where it runs. */
export function AutoscalerDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = settingsOf(c.variables["autoscaler"]);
  const [value, setValue] = useState(current);
  const [delay, setDelay] = useState(String(current.scaleDownDelayAfterAddMinutes));
  const [unneeded, setUnneeded] = useState(String(current.scaleDownUnneededMinutes));
  const { busy, error, send } = useChange(c, onClose);
  const inCluster = useAutoscalerInCluster(c);
  const next: Required<AutoscalerSettings> = {
    ...value,
    scaleDownDelayAfterAddMinutes: number(delay),
    scaleDownUnneededMinutes: number(unneeded),
  };
  const invalid = [delay, unneeded].some((m) => !whole(number(m), 1, maxMinutes));
  const changed = JSON.stringify(next) !== JSON.stringify(current);
  return (
    <Dialog
      title="Edit autoscaler"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { autoscaler: next } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || invalid}>
            {stageLabel}
          </button>
        </>
      }
    >
      <Field
        label="Runs"
        hint={value.placement === "workload" ? "Its token reads the machines of the namespace's clusters" : undefined}
        info={
          value.placement === "workload"
            ? "Needs VM.Audit on the workload pool and the management cluster's API reachable."
            : undefined
        }
      >
        {(id) => (
          <select
            id={id}
            value={value.placement}
            onChange={(e) =>
              setValue({ ...value, placement: e.target.value === "workload" ? "workload" : "management" })
            }
          >
            <option value="management">Management cluster (recommended)</option>
            {(inCluster || value.placement === "workload") && (
              <option value="workload">Cluster&apos;s control planes</option>
            )}
          </select>
        )}
      </Field>
      <Field label="Pool that grows">
        {(id) => (
          <select
            id={id}
            value={value.expander}
            onChange={(e) =>
              setValue({ ...value, expander: expanders.find(([k]) => k === e.target.value)?.[0] ?? "least-waste" })
            }
          >
            {expanders.map(([k, label]) => (
              <option key={k} value={k}>
                {label}
              </option>
            ))}
          </select>
        )}
      </Field>
      <MinutesField
        label="Wait after adding (minutes)"
        hint="Before removing nodes after a scale-up"
        value={delay}
        onChange={setDelay}
      />
      <MinutesField
        label="Unneeded for (minutes)"
        hint="Before an unneeded node is removed"
        value={unneeded}
        onChange={setUnneeded}
      />
      <ErrorBox message={error} />
    </Dialog>
  );
}

/**
 * scaledCluster is the cluster as the Scale dialog's entries leave it, in the shape the server's plan
 * gives, so that the dialog and Review plan check the same pools. An autoscaled pool keeps the
 * workers it runs and takes the bounds entered.
 */
export function scaledCluster(c: Cluster, controlPlane: number, entries: Record<string, PoolEntry>): Cluster {
  const workerPools = c.workerPools.map((p): WorkerPool => {
    const e = entries[p.name] ?? poolEntry(p);
    if (e.autoscale) {
      return { ...p, autoscaling: { min: entryLeast(e) || 0, max: entryMost(e) } };
    }
    const { autoscaling: _, ...fixed } = p;
    return { ...fixed, replicas: entryMost(e) };
  });
  return {
    ...c,
    controlPlane: { ...c.controlPlane, desired: controlPlane },
    workers: { ...c.workers, desired: workerPools.reduce((sum, p) => sum + p.replicas, 0) },
    workerPools,
  };
}

/** ScaleDialog sets a cluster's control-plane nodes and, for each worker pool, its workers or its autoscaling bounds. */
export function ScaleDialog({
  cluster: c,
  onClose,
  renderChecks,
}: {
  cluster: Cluster;
  onClose: () => void;
  /** The checks of the cluster as the entries would leave it, as Review plan lists them. */
  renderChecks: (after: Cluster, nodes: PveNode[]) => ReactNode;
}): JSX.Element {
  const current = c.controlPlane.desired || 1;
  const [controlPlane, setControlPlane] = useState(current);
  // Kept as typed, so a cleared field stays empty instead of turning into 0.
  const [pools, setPools] = useState<Record<string, PoolEntry>>(
    Object.fromEntries(c.workerPools.map((p) => [p.name, poolEntry(p)])),
  );
  const nodes = usePveNodes();
  const { busy, error, send } = useChange(c, onClose);
  const scaling = { c, controlPlane, entries: pools };
  const invalid = poolProblems(scaling);
  const room = roomFor(scaling);
  const change = poolChange(c.workerPools, pools);
  const changed = controlPlane !== current || Object.keys(change).length > 0;
  const after = scaledCluster(c, controlPlane, pools);
  return (
    <Dialog
      title={`Scale ${c.name}`}
      onClose={onClose}
      onSubmit={() => {
        void send({ ...(controlPlane !== current && { controlPlaneReplicas: controlPlane }), ...change });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || Object.keys(invalid).length > 0}>
            {stageLabel}
          </button>
        </>
      }
    >
      <Field label="Control-plane nodes">
        {(id) => (
          <select id={id} value={controlPlane} onChange={(e) => setControlPlane(Number(e.target.value))}>
            {[...new Set([...controlPlaneSizes, current])]
              .toSorted((a, b) => a - b)
              .map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
          </select>
        )}
      </Field>
      {c.workerPools.map((p) => {
        const entry = pools[p.name] ?? poolEntry(p);
        return (
          <PoolFields
            key={p.name}
            pool={p}
            pools={c.workerPools}
            entry={entry}
            problem={invalid[p.name]}
            warning={roomWarning({ before: c, after }, p.name, nodes)}
            room={room[p.name]}
            onChange={(e) => setPools({ ...pools, [p.name]: e })}
          />
        );
      })}
      {changed && renderChecks(after, nodes)}
      <ErrorBox message={error} />
    </Dialog>
  );
}
