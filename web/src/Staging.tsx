import { useState, type JSX, type ReactNode } from "react";

import { api, ApiError, httpPreconditionFailed } from "./api";
import { autoscalerText } from "./Autoscaling";
import { backupsRows } from "./Backups";
import { everything, optionRows, type OptionRow } from "./ClusterOptions";
import { etcdSnapshotsRows } from "./EtcdSnapshots";
import { StagingContext, useStaging, type Staging } from "./staging-context";
import type { ChangeRequest, Cluster, Effect, PlannedChange } from "./types";
import { Dialog, ErrorBox, errorMessage } from "./ui";

// mergedPools is two changes of pools as one: a pool's later change adds to its earlier one, and a
// deletion ends both.
function mergedPools(
  a: NonNullable<ChangeRequest["pools"]>,
  b: NonNullable<ChangeRequest["pools"]>,
): NonNullable<ChangeRequest["pools"]> {
  const out = { ...a };
  for (const [name, change] of Object.entries(b)) {
    const was = out[name];
    out[name] = change === null || was === null || was === undefined ? change : { ...was, ...change };
  }
  return out;
}

// merged is two changes as one, the later one's settings winning.
export function merged(a: ChangeRequest, b: ChangeRequest): ChangeRequest {
  const out: ChangeRequest = {};
  const version = b.version ?? a.version;
  if (version !== undefined) {
    out.version = version;
  }
  const controlPlane = b.controlPlaneReplicas ?? a.controlPlaneReplicas;
  if (controlPlane !== undefined) {
    out.controlPlaneReplicas = controlPlane;
  }
  if (a.workerPools !== undefined || b.workerPools !== undefined) {
    out.workerPools = { ...a.workerPools, ...b.workerPools };
  }
  if (a.autoscaling !== undefined || b.autoscaling !== undefined) {
    out.autoscaling = { ...a.autoscaling, ...b.autoscaling };
  }
  if (a.pools !== undefined || b.pools !== undefined) {
    out.pools = mergedPools(a.pools ?? {}, b.pools ?? {});
  }
  if (a.variables !== undefined || b.variables !== undefined) {
    out.variables = { ...a.variables, ...b.variables };
  }
  return out;
}

const changedMeanwhile = "Someone changed the cluster meanwhile, so the staged changes are gone.";

/**
 * StagingProvider keeps a cluster page's changes until they are applied together. Each dialog's
 * change is planned with those staged before it, so that what the server refuses shows in the
 * dialog, and nothing changes in the cluster before Apply.
 */
export function StagingProvider({
  cluster: c,
  onApplied,
  children,
}: {
  cluster: Cluster;
  onApplied: () => void;
  children: ReactNode;
}): JSX.Element {
  const [staged, setStaged] = useState<Staging["staged"]>();
  const [dropped, setDropped] = useState<string>();
  // Changes staged against a cluster someone has changed since would undo theirs: they go, and the
  // page shows the cluster as it is now.
  const outdated = (err: unknown): boolean => {
    if (!(err instanceof ApiError) || err.status !== httpPreconditionFailed) {
      return false;
    }
    setStaged(undefined);
    setDropped(changedMeanwhile);
    onApplied();
    return true;
  };
  const staging: Staging = {
    staged,
    dropped,
    stage: async (req) => {
      const generation = staged?.generation ?? c.generation;
      const all = merged(staged?.req ?? {}, req);
      setDropped(undefined);
      try {
        const plan = await api.plan({ ...c, generation }, all);
        setStaged(plan.changes.length === 0 ? undefined : { req: all, plan, generation });
      } catch (err) {
        outdated(err);
        throw err;
      }
    },
    discard: () => setStaged(undefined),
    apply: async () => {
      if (staged === undefined) {
        return;
      }
      try {
        await api.change({ ...c, generation: staged.generation }, staged.req);
      } catch (err) {
        if (outdated(err)) {
          return;
        }
        throw err;
      }
      setStaged(undefined);
      onApplied();
    },
  };
  return <StagingContext value={staging}>{children}</StagingContext>;
}

// The rows of the page a setting shows in; most show in one.
const settingLabels: Record<string, string[]> = {
  version: ["Kubernetes version"],
  controlPlaneReplicas: ["Control plane"],
  workerPools: ["Workers"],
  pools: ["Worker pools"],
  controlPlaneMachine: ["Control-plane size"],
  workerMachine: ["Worker size"],
  autoscaling: ["Autoscaling"],
  allowedNodes: ["Placement"],
  autoscaler: ["Autoscaler"],
  backups: ["Backups"],
  ccm: ["HA group labels"],
  certificates: ["Certificates"],
  clusterMesh: ["Cluster Mesh"],
  csi: ["CSI plugin", "Volume snapshots"],
  etcdSnapshots: ["etcd snapshots"],
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

const effects: Record<Effect, string> = {
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

const workersText = (c: Cluster): string => c.workerPools.map((p) => `${p.name} ${p.replicas}`).join(", ");

function autoscalingText(c: Cluster): string {
  const scaled = c.workerPools.flatMap((p) =>
    p.autoscaling ? [`${p.name} ${p.autoscaling.min} to ${p.autoscaling.max}`] : [],
  );
  return scaled.length === 0 ? "Off" : scaled.join(", ");
}

// settingRows are every setting of a cluster a change can set, as the page shows them.
function settingRows(c: Cluster): Setting[] {
  return [
    { label: "Kubernetes version", value: c.version },
    { label: "Control plane", value: String(c.controlPlane.desired) },
    { label: "Workers", value: workersText(c) },
    { label: "Autoscaling", value: autoscalingText(c) },
    ...optionRows<string>(c, everything, [
      ...backupsRows(c, true),
      ...etcdSnapshotsRows(c, true),
      { label: "Autoscaler", value: autoscalerText(c), group: "Nodes" },
    ]),
  ];
}

type Setting = Pick<OptionRow<string>, "label" | "value">;

/** A row of the plan: a setting as it is and as it would be, and what that does to the machines. */
interface PlanRow {
  label: string;
  now: string;
  after: string;
  effect: Effect;
}

const valueOf = (rows: Setting[], label: string): string => rows.find((r) => r.label === label)?.value ?? "";

/** planRows are the rows of a plan: one per row of the page whose value a change sets anew. */
export function planRows(before: Cluster, after: Cluster, changes: PlannedChange[]): PlanRow[] {
  const now = settingRows(before);
  const then = settingRows(after);
  return changes.flatMap(({ setting, effect }) => {
    const rows = labelsOf(setting).map((label) => ({
      label,
      now: valueOf(now, label),
      after: valueOf(then, label),
      effect,
    }));
    const changed = rows.filter((r) => r.now !== r.after);
    return changed.length > 0 ? changed : rows.slice(0, 1);
  });
}

/** StagedBar says what a page has staged and what applying it does, with the plan to review and the way back. */
export function StagedBar({ cluster: c }: { cluster: Cluster }): JSX.Element | null {
  const staging = useStaging();
  const [reviewing, setReviewing] = useState(false);
  if (staging?.staged === undefined) {
    return <ErrorBox message={staging?.dropped} />;
  }
  const { changes } = staging.staged.plan;
  return (
    <section className="staged" aria-label="Staged changes">
      <strong>
        {changes.length} {changes.length === 1 ? "change" : "changes"} not applied
      </strong>
      <span className="staged-effect">{effects[worstEffect(changes)]}</span>
      <span className="spacer" />
      <button type="button" onClick={() => staging.discard()}>
        Discard
      </button>
      <button type="button" className="primary" onClick={() => setReviewing(true)}>
        Review plan
      </button>
      {reviewing && <PlanDialog cluster={c} staging={staging} onClose={() => setReviewing(false)} />}
    </section>
  );
}

function PlanDialog({
  cluster: c,
  staging,
  onClose,
}: {
  cluster: Cluster;
  staging: Staging;
  onClose: () => void;
}): JSX.Element | null {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const { staged } = staging;
  if (staged === undefined) {
    return null;
  }
  const { plan } = staged;
  const apply = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await staging.apply();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      title="Review plan"
      onClose={onClose}
      onSubmit={() => {
        void apply();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Keep staged
          </button>
          <button type="submit" className="primary" disabled={busy}>
            Apply
          </button>
        </>
      }
    >
      <p className="muted">{effects[worstEffect(plan.changes)]}.</p>
      {(plan.warnings ?? []).map((w) => (
        <p key={w} className="warning">
          {w}
        </p>
      ))}
      <div className="scroll">
        <table className="grid plan">
          <thead>
            <tr>
              <th>Setting</th>
              <th>Now</th>
              <th>After</th>
              <th>Machines</th>
            </tr>
          </thead>
          <tbody>
            {planRows(c, plan.cluster, plan.changes).map((r) => (
              <tr key={r.label}>
                <th scope="row">{r.label}</th>
                <td>{r.now}</td>
                <td>{r.after}</td>
                <td className={r.effect === "none" ? "muted" : "plan-replaces"}>{effects[r.effect]}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <ErrorBox message={error} />
    </Dialog>
  );
}
