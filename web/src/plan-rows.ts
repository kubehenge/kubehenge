import { autoscalerText, poolText } from "./Autoscaling";
import { backupsRows } from "./Backups";
import { everything, optionRows, type OptionRow } from "./ClusterOptions";
import { etcdSnapshotsRows } from "./EtcdSnapshots";
import { nodeCustomisationChanges, nodeCustomisationOf } from "./node-customisation";
import { timeoutsText } from "./NodeTimeouts";
import { poolLabel } from "./pool-machine";
import type { Cluster, ClusterClass, Effect, PlannedChange } from "./types";

const controlPlaneTimeoutsLabel = "Control plane timeouts";
const poolTimeoutsLabel = (pool: string): string => `Pool ${poolLabel(pool)} timeouts`;

// The rows of the page a setting shows in. Most show in one.
const settingLabels: Record<string, string[]> = {
  version: ["Kubernetes version"],
  controlPlaneReplicas: ["Control plane"],
  workerPools: ["Workers"],
  pools: ["Worker pools"],
  controlPlaneTimeouts: [controlPlaneTimeoutsLabel],
  controlPlaneMachine: ["Control-plane size"],
  workerMachine: ["Worker size"],
  workerNodes: ["Worker node settings"],
  cis: ["CIS hardening"],
  nodeCustomisation: ["Node customisation"],
  autoscaling: ["Autoscaling"],
  allowedNodes: ["Placement"],
  namespaceLabels: ["Namespace labels"],
  autoscaler: ["Autoscaler"],
  backups: ["Backups"],
  ccm: ["HA rule labels"],
  certificates: ["Certificates"],
  clusterMesh: ["Cluster Mesh"],
  csi: ["CSI plugin", "Volume snapshots"],
  etcd: ["etcd"],
  etcdSnapshots: ["etcd snapshots"],
  egressProxy: ["Egress proxy"],
  registryMirrors: ["Registry mirrors"],
  trustedCAs: ["Trusted CAs"],
  releaseMirror: ["Release mirror"],
  gitops: ["GitOps"],
  headlamp: ["Headlamp"],
  istio: ["Istio"],
  loadBalancer: ["Load balancer"],
  logs: ["Logs"],
  monitoring: ["Monitoring"],
  nodeOS: ["Node OS"],
  oidc: ["Identity providers"],
  podSecurity: ["Pod security"],
  proxmoxHA: ["Proxmox VE HA"],
  rancher: ["Rancher"],
  talosVersion: ["Talos release"],
  upgrades: ["Automatic upgrades"],
};

const labelsOf = (setting: string): string[] => settingLabels[setting] ?? [setting];

/** settingLabel is what the page calls a setting. */
export const settingLabel = (setting: string): string => labelsOf(setting)[0] ?? setting;

/** effects say what each effect does to the machines. */
export const effects: Record<Effect, string> = {
  none: "Replaces no machine",
  scales: "Adds or removes machines",
  "control-plane": "Replaces control planes one by one",
  workers: "Replaces workers one by one",
  all: "Replaces every machine one by one",
};

// The effects from the least to the most a cluster goes through.
const weight: Effect[] = ["none", "scales", "workers", "control-plane", "all"];

/** worstEffect is what a change does to the machines as a whole: the most any of its settings does. */
export function worstEffect(changes: PlannedChange[]): Effect {
  const has = (e: Effect): boolean => changes.some((x) => x.effect === e);
  if (has("workers") && has("control-plane")) {
    return "all";
  }
  return changes.reduce<Effect>(
    (worst, x) => (weight.indexOf(x.effect) > weight.indexOf(worst) ? x.effect : worst),
    "none",
  );
}

// A cluster with one pool has just its number of workers. Several are told apart by name.
const workersText = (c: Cluster): string =>
  c.workerPools.length === 1
    ? String(c.workerPools[0]?.replicas)
    : c.workerPools.map((p) => `${poolLabel(p.name)} ${p.replicas}`).join(", ");

function autoscalingText(c: Cluster): string {
  const scaled = c.workerPools.flatMap((p) =>
    p.autoscaling ? [`${poolLabel(p.name)} ${p.autoscaling.min} to ${p.autoscaling.max}`] : [],
  );
  return scaled.length === 0 ? "Off" : scaled.join(", ");
}

// The timeouts of the control plane and of each pool, as they apply with the class's behind them.
function timeoutRows(c: Cluster, cls: ClusterClass | undefined): Setting[] {
  return [
    {
      label: controlPlaneTimeoutsLabel,
      value: timeoutsText(c.controlPlaneTimeouts ?? {}, cls?.controlPlaneTimeouts),
    },
    ...c.workerPools.map((p) => ({
      label: poolTimeoutsLabel(p.name),
      value: timeoutsText(p.timeouts ?? {}, cls?.workerTimeouts),
    })),
  ];
}

// settingRows are every setting of a cluster a change can set, as the page shows them.
function settingRows(c: Cluster, cls: ClusterClass | undefined): Setting[] {
  return [
    { label: "Kubernetes version", value: c.version },
    { label: "Control plane", value: String(c.controlPlane.desired) },
    { label: "Workers", value: workersText(c) },
    { label: "Autoscaling", value: autoscalingText(c) },
    ...timeoutRows(c, cls),
    ...optionRows<string>(c, everything, [
      ...backupsRows(c, true),
      ...etcdSnapshotsRows(c, true),
      { label: "Autoscaler", value: autoscalerText(c), group: "Nodes" },
    ]),
  ];
}

type Setting = Pick<OptionRow<string>, "label" | "value">;

/** A row of the plan: a setting as it is and as it would be, and what that does to the machines. */
export interface PlanRow {
  label: string;
  now: string;
  after: string;
  /** Unset on the rows that follow the first of a setting, which the first row's effect covers. */
  effect: Effect | undefined;
}

const valueOf = (rows: Setting[], label: string): string => rows.find((r) => r.label === label)?.value ?? "";

// nodeRows are the node customisation's row as one row per item that differs, each with what it was
// and becomes, since a line of counts would read the same on both sides. The row itself when no
// item differs.
function nodeRows(row: PlanRow, before: Cluster, after: Cluster): PlanRow[] {
  const diff = nodeCustomisationChanges(
    nodeCustomisationOf(before.variables["nodeCustomisation"]),
    nodeCustomisationOf(after.variables["nodeCustomisation"]),
  );
  if (diff.was.length === 0) {
    return [row];
  }
  return diff.was.map((was, i) => ({
    label: i === 0 ? row.label : "",
    now: was,
    after: diff.now[i] ?? "",
    effect: i === 0 ? row.effect : undefined,
  }));
}

/** planRows are the rows of a plan: one per row of the page whose value a change sets anew. */
export function planRows(
  { before, after }: { before: Cluster; after: Cluster },
  changes: PlannedChange[],
  cls?: ClusterClass,
): PlanRow[] {
  const now = settingRows(before, cls);
  const then = settingRows(after, cls);
  const pools = [...new Set([...before.workerPools, ...after.workerPools].map((p) => p.name))].map((name) =>
    poolTimeoutsLabel(name),
  );
  return changes.flatMap(({ setting, effect }) => {
    const rows = (setting === "pools" ? [...labelsOf(setting), ...pools] : labelsOf(setting)).map((label) => ({
      label,
      now: valueOf(now, label),
      after: valueOf(then, label),
      effect,
    }));
    const changed = rows.filter((r) => r.now !== r.after);
    const shown = changed.length > 0 ? changed : rows.slice(0, 1);
    return shown.flatMap((r) => (r.label === "Node customisation" ? nodeRows(r, before, after) : [r]));
  });
}

// A pool's text on one side of a change. A pool the change adds has none yet.
const poolSide = (c: Cluster, name: string): string => {
  const p = c.workerPools.find((x) => x.name === name);
  return p === undefined ? "" : poolText(p);
};

const named = (label: string, now: string, after: string): string =>
  now === "" ? `${label}: ${after}` : `${label}: ${now} to ${after}`;

/**
 * changeLines name each staged change with the numbers it sets, such as "Workers: 4 to 60" or
 * "Control plane: 3 to 5", for the bar that holds them until they are applied.
 */
export function changeLines(
  { before, after }: { before: Cluster; after: Cluster },
  changes: PlannedChange[],
  cls?: ClusterClass,
): string[] {
  return changes.flatMap((change) => {
    if (change.setting === "controlPlaneReplicas") {
      return [named("Control plane", String(before.controlPlane.desired), String(after.controlPlane.desired))];
    }
    if (change.setting === "workerPools") {
      const names = [...new Set([...before.workerPools, ...after.workerPools].map((p) => p.name))];
      return names
        .filter((n) => poolSide(before, n) !== poolSide(after, n))
        .map((n) => named(names.length === 1 ? "Workers" : poolLabel(n), poolSide(before, n), poolSide(after, n)));
    }
    return planRows({ before, after }, [change], cls)
      .filter((r) => r.label !== "")
      .map((r) => named(r.label, r.now, r.after));
  });
}
