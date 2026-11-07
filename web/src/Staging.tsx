import { useEffect, useState, type JSX, type ReactNode } from "react";

import { api, ApiError, httpPreconditionFailed } from "./api";
import { effects, planRows, worstEffect } from "./plan-rows";
import { StagingContext, useStaging, type Staging } from "./staging-context";
import type { ChangeRequest, Cluster, ClusterClass } from "./types";
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
  const timeouts = b.controlPlaneTimeouts ?? a.controlPlaneTimeouts;
  if (timeouts !== undefined) {
    out.controlPlaneTimeouts = timeouts;
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
  const [cls, setCls] = useState<ClusterClass>();
  const className = c.class;
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        const classes = await api.classes();
        setCls(classes.find((x) => x.name === className));
      } catch {
        // Without the class the plan shows the timeouts a cluster sets, and "class default" for the rest.
      }
    }
    void load();
  }, [className]);
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
        <table className="grid plan stack">
          <thead>
            <tr>
              <th>Setting</th>
              <th>Now</th>
              <th>After</th>
              <th>Machines</th>
            </tr>
          </thead>
          <tbody>
            {planRows({ before: c, after: plan.cluster }, plan.changes, cls).map((r, i) => (
              // The item rows of a setting share its label, so the position tells them apart.
              // oxlint-disable-next-line react/no-array-index-key
              <tr key={`${r.label}-${i}`}>
                <th scope="row">{r.label}</th>
                <td data-label="Now">{r.now}</td>
                <td data-label="After">{r.after}</td>
                <td data-label="Machines" className={r.effect === "none" ? "muted" : "plan-replaces"}>
                  {r.effect === undefined ? "" : effects[r.effect]}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <ErrorBox message={error} />
    </Dialog>
  );
}
