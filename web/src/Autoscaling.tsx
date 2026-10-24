import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { autoscales, controlPlaneSizes, mostWorkers, variable } from "./create-form";
import type { StepProps } from "./CreateSteps";
import { addressCount, whole } from "./network";
import type {
  AutoscalerSettings,
  Autoscaling,
  ChangeRequest,
  Cluster,
  ClusterClass,
  PveNode,
  WorkerPool,
} from "./types";
import { Dialog, ErrorBox, Field, bytes, maxWorkers, shownNumber, typedNumber, useChange } from "./ui";
import { flag, quantity, record, text, texts } from "./values";

const bytesPerMiB = 1_048_576;

const fixedOrAutoscaled = (
  <>
    <option value="fixed">A fixed number</option>
    <option value="autoscaled">Autoscaled</option>
  </>
);

const autoscaledHint =
  "The cluster autoscaler adds workers while pods wait for room and removes those left unneeded, between the fewest and the most.";

const emptiesHint =
  "With 0 the pool empties while nothing waits; the control planes still run only the control plane, and add-ons need a worker that stays.";

/** WorkerFields are the create wizard's workers: a fixed number, or the fewest and the most the cluster autoscaler keeps. */
export function WorkerFields({
  form,
  set,
  shown,
  cls,
  alone,
}: StepProps & { cls: ClusterClass | undefined; alone: string }): JSX.Element {
  const offered = variable(cls, "autoscaler") !== undefined;
  const auto = autoscales(form, cls);
  let leastHint: string | undefined;
  if (auto && form.workerReplicas === 0) {
    leastHint = emptiesHint;
  } else if (mostWorkers(form) === 0) {
    leastHint = alone;
  }
  return (
    <>
      {offered && (
        <Field label="Worker count" hint={auto ? autoscaledHint : undefined}>
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
          hint="Each needs a node address of its own; the Network step counts the most."
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

const poolMost = (p: WorkerPool): number => p.autoscaling?.max ?? p.replicas;

// The add-ons that use memory on the nodes by the classes' figures, read from the variables; the
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
function addressRoom(c: Cluster): number | undefined {
  const n = record(c.variables["network"]);
  const entries = texts(n["addresses"]) ?? [];
  const prefix = quantity(n["prefix"]);
  if (entries.length === 0 || prefix === undefined) {
    return undefined;
  }
  const { count, bad } = addressCount(entries, text(n["gateway"]) ?? "", prefix);
  return bad === undefined ? count : undefined;
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
    return { least: `A whole number from 0 to ${maxWorkers}.` };
  }
  if (!e.autoscale) {
    return undefined;
  }
  if (!whole(number(e.max), Math.max(least, 1), maxWorkers)) {
    return { most: `A whole number from ${Math.max(least, 1)} to ${maxWorkers}, no fewer than the fewest.` };
  }
  if (!kept && runsAddons(s.c)) {
    return { least: "The cluster's add-ons need a worker that stays: at least 1." };
  }
  const need = s.controlPlane + 1 + Object.values(s.entries).reduce((n, x) => n + entryMost(x), 0);
  const wasNeed = (s.c.controlPlane.desired || 1) + 1 + s.c.workerPools.reduce((n, p) => n + poolMost(p), 0);
  const have = addressRoom(s.c);
  // As the server: a change may keep a shortage the cluster has, but not make it worse.
  if (have !== undefined && need > have && need > wasNeed) {
    return {
      most: `The node addresses give ${have}; with every pool at its most the cluster runs ${need} machines at once, one for a rollout.`,
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

/** poolChange is the change the Scale dialog's entries ask for, pool by pool, of what differs; empty for none. */
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
function freeMemory(c: Cluster, nodes: PveNode[]): number | undefined {
  const allowed = texts(c.variables["allowedNodes"]);
  const usable = nodes.filter((n) => n.online && (allowed === undefined || allowed.includes(n.node)));
  if (usable.length === 0 || usable.some((n) => n.maxMemory === undefined)) {
    return undefined;
  }
  return usable.reduce((sum, n) => sum + Math.max(0, (n.maxMemory ?? 0) - (n.memory ?? 0)), 0);
}

/**
 * roomWarning warns when the workers an autoscaled pool may still add need more memory than the
 * nodes the cluster may use have free. It is advice: memory frees and fills as other VMs come and go.
 */
export function roomWarning(c: Cluster, pool: { p: WorkerPool; e: PoolEntry }, nodes: PveNode[]): string | undefined {
  const workerMiB = quantity(record(c.variables["workerMachine"])["memoryMiB"]);
  const free = freeMemory(c, nodes);
  const added = number(pool.e.max) - pool.p.replicas;
  if (!pool.e.autoscale || workerMiB === undefined || free === undefined || !(added > 0)) {
    return undefined;
  }
  const wanted = added * workerMiB * bytesPerMiB;
  return wanted > free
    ? `The ${added} more workers it may add need ${bytes(wanted)} of memory; the Proxmox VE nodes the cluster may use have ${bytes(free)} free now. Past that, a new VM finds no room and the cluster's health says so.`
    : undefined;
}

/** PoolFields edit one worker pool in the Scale dialog: a fixed number of workers, or autoscaled between two. */
export function PoolFields({
  pool: p,
  entry: e,
  problem,
  warning,
  room,
  onChange,
}: {
  pool: WorkerPool;
  entry: PoolEntry;
  problem: PoolProblem | undefined;
  warning: string | undefined;
  room: number | undefined;
  onChange: (e: PoolEntry) => void;
}): JSX.Element {
  return (
    <>
      <Field label={`Scaling (${p.name})`} hint={e.autoscale ? autoscaledHint : undefined}>
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
          <Field
            label={`Fewest workers (${p.name})`}
            error={problem?.least}
            hint="With add-ons, at least one autoscaled pool keeps 1 or more."
          >
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
            label={`Most workers (${p.name})`}
            error={problem?.most}
            hint={room === undefined ? undefined : `The node addresses leave room for ${room} here.`}
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
          label={`Workers (${p.name})`}
          error={problem?.least}
          hint={
            p.autoscaling
              ? "Switched off, the pool keeps the workers it runs now unless you change the number."
              : undefined
          }
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
        // The warning is advice; without the nodes the dialog gives none, and the cluster still
        // shows a VM that found no room.
      }
    }
    void load();
  }, []);
  return nodes;
}

const expanders = [
  ["least-waste", "The pool left with the least unused room"],
  ["most-pods", "The pool that takes the most waiting pods"],
  ["random", "Any pool that fits"],
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
  const scaled = pools === 0 ? "no pool autoscaled" : `${pools} ${pools === 1 ? "pool" : "pools"} autoscaled`;
  return `${where}, ${scaled}; removes a node unneeded for ${s.scaleDownUnneededMinutes} min`;
}

const placementHints = {
  management: "In the management cluster. The cluster holds no credential of the management cluster.",
  workload:
    "On the cluster's control planes, sparing the management cluster its memory. It needs the management cluster's API reachable from the cluster, and puts a token inside the cluster that reads the machines of every cluster in its namespace; an admission policy still lets it scale only this cluster's pools. Choosing it needs VM.Audit on the workload pool.",
} as const;

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
  const problem = whole(number(value), 1, maxMinutes)
    ? undefined
    : `A whole number of minutes from 1 to ${maxMinutes}.`;
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
  const next: Required<AutoscalerSettings> = {
    ...value,
    scaleDownDelayAfterAddMinutes: number(delay),
    scaleDownUnneededMinutes: number(unneeded),
  };
  const invalid = [delay, unneeded].some((m) => !whole(number(m), 1, maxMinutes));
  const changed = JSON.stringify(next) !== JSON.stringify(current);
  return (
    <Dialog
      title="Autoscaler"
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
            Save
          </button>
        </>
      }
    >
      <p className="muted">
        Kubehenge runs the cluster autoscaler while a worker pool is autoscaled; the Scale dialog switches pools to it.
      </p>
      <Field label="Runs" hint={placementHints[value.placement]}>
        {(id) => (
          <select
            id={id}
            value={value.placement}
            onChange={(e) =>
              setValue({ ...value, placement: e.target.value === "workload" ? "workload" : "management" })
            }
          >
            <option value="management">In the management cluster (recommended)</option>
            <option value="workload">On the cluster's own control planes</option>
          </select>
        )}
      </Field>
      <Field label="Pool that grows" hint="When several pools could take the waiting pods.">
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
        hint="Before it removes any node after a scale-up."
        value={delay}
        onChange={setDelay}
      />
      <MinutesField
        label="Unneeded for (minutes)"
        hint="How long a node stays unneeded before it goes."
        value={unneeded}
        onChange={setUnneeded}
      />
      <ErrorBox message={error} />
    </Dialog>
  );
}

/** ScaleDialog sets a cluster's control-plane nodes and, for each worker pool, its workers or its autoscaling bounds. */
export function ScaleDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
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
  return (
    <Dialog
      title={`Scale ${c.name}`}
      onClose={onClose}
      onSubmit={() => {
        void send({ controlPlaneReplicas: controlPlane, ...change });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || Object.keys(invalid).length > 0}>
            Scale
          </button>
        </>
      }
    >
      <Field label="Control-plane nodes" hint="Three or more keep the cluster running when one node fails.">
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
            entry={entry}
            problem={invalid[p.name]}
            warning={roomWarning(c, { p, e: entry }, nodes)}
            room={room[p.name]}
            onChange={(e) => setPools({ ...pools, [p.name]: e })}
          />
        );
      })}
      <ErrorBox message={error} />
    </Dialog>
  );
}
