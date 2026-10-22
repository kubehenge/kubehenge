import { useEffect, useId, useState, type JSX } from "react";

import { api, etcdSnapshotURL } from "./api";
import {
  backupCredentialHint,
  backupKeys,
  backupsOf,
  scheduleProblem,
  scheduleText,
  targetProblem,
  targetText,
  useClassTargets,
  type OfferedTarget,
} from "./Backups";
import type { OptionRow } from "./ClusterOptions";
import { CredentialField } from "./CredentialField";
import type { Cluster, ClusterEtcdSnapshots, EtcdRestorePlan, EtcdRestoreStatus, EtcdSnapshot } from "./types";
import {
  Dialog,
  ErrorBox,
  Field,
  Status,
  age,
  bytes,
  distributionName,
  errorMessage,
  filled,
  pollIdle,
  pollRunning,
  sentence,
  sole,
  timeZones,
  useChange,
  usePoll,
  type Health,
} from "./ui";
import { quantity, record, text } from "./values";

/** The chart's defaults for a cluster's etcd snapshots. */
export const defaultEtcdSchedule = "0 */12 * * *";
export const defaultEtcdKeep = 14;
const maxEtcdKeep = 1000;

export const etcdSnapshotsHint =
  "Kubehenge copies the cluster's etcd, every object its API holds, to an S3 target an administrator set, encrypted with a key of the cluster's own that Kubehenge keeps. Nothing runs in the cluster for it: the controller reads etcd through the cluster's API server.";

export const etcdRestoreHint =
  "Restoring etcd takes every object of the cluster back to the snapshot: Deployments, Secrets, roles and the rest of what its API holds. The data in volumes stays as it is now. It is for a cluster that lost its etcd quorum, or whose state is corrupted or was deleted in bulk; to bring back applications with their data, restore a Velero backup.";

/** A cluster's etcdSnapshots variable. */
export interface EtcdSnapshotsValue {
  enabled: boolean;
  target?: string;
  schedule?: string;
  timeZone?: string;
  keep?: number;
}

/** etcdSnapshotsOf reads a cluster's etcdSnapshots variable; unset, snapshots are off. */
export function etcdSnapshotsOf(v: unknown): EtcdSnapshotsValue {
  const r = record(v);
  const out: EtcdSnapshotsValue = { enabled: r["enabled"] === true };
  for (const k of ["target", "schedule", "timeZone"] as const) {
    const s = text(r[k]);
    if (filled(s)) {
      out[k] = s;
    }
  }
  const keep = quantity(r["keep"]);
  if (keep !== undefined) {
    out.keep = keep;
  }
  return out;
}

/** The snapshot settings as typed. */
export interface EtcdSnapshotFields {
  target: string;
  schedule: string;
  timeZone: string;
  keep: string;
}

/** etcdFieldsOf are the fields of a cluster's settings; what it leaves unset is the chart's default. */
export function etcdFieldsOf(v: EtcdSnapshotsValue): EtcdSnapshotFields {
  return {
    target: v.target ?? "",
    schedule: v.schedule ?? defaultEtcdSchedule,
    timeZone: v.timeZone ?? "UTC",
    keep: String(v.keep ?? defaultEtcdKeep),
  };
}

export type EtcdSnapshotField = "target" | "schedule" | "keep";

/** etcdProblems says, by field, what Kubehenge would refuse. */
export function etcdProblems(
  f: EtcdSnapshotFields,
  targets: OfferedTarget[],
  kept?: string,
): Partial<Record<EtcdSnapshotField, string>> {
  const p: Partial<Record<EtcdSnapshotField, string>> = {};
  const target = targetProblem(f.target, targets, kept);
  if (target !== undefined) {
    p.target = target;
  }
  const schedule = scheduleProblem(f.schedule);
  if (schedule !== undefined) {
    p.schedule = schedule;
  }
  const keep = Number(f.keep);
  if (!/^\d+$/u.test(f.keep) || keep < 1 || keep > maxEtcdKeep) {
    p.keep = `A whole number from 1 to ${maxEtcdKeep}.`;
  }
  return p;
}

/** etcdSnapshotsVariable is the variable the fields set. */
export const etcdSnapshotsVariable = (f: EtcdSnapshotFields): EtcdSnapshotsValue => ({
  enabled: true,
  target: f.target,
  schedule: f.schedule.trim(),
  timeZone: f.timeZone,
  keep: Number(f.keep),
});

/** etcdSnapshotsText sums up a cluster's etcd snapshots. */
export function etcdSnapshotsText(v: EtcdSnapshotsValue): string {
  if (!v.enabled) {
    return "Off";
  }
  const f = etcdFieldsOf(v);
  return `${scheduleText(f.schedule)} (${f.timeZone}), keeps the last ${f.keep}, to ${f.target}`;
}

/** etcdSnapshotsRows is the Options view's row of a cluster's etcd snapshots, where its class offers a target. */
export function etcdSnapshotsRows(c: Cluster, offered: boolean): OptionRow<"etcdSnapshots">[] {
  const v = etcdSnapshotsOf(c.variables["etcdSnapshots"]);
  if (!v.enabled && !offered) {
    return [];
  }
  return [{ label: "etcd snapshots", value: etcdSnapshotsText(v), action: "etcdSnapshots", name: "etcd snapshots" }];
}

// withTarget starts from the target the cluster's backups use, or the one there is.
function withTarget(f: EtcdSnapshotFields, targets: OfferedTarget[], backups: string | undefined): EtcdSnapshotFields {
  if (filled(f.target)) {
    return f;
  }
  const chosen =
    backups !== undefined && targets.some((t) => t.name === backups) ? backups : sole(targets.map((t) => t.name));
  return { ...f, target: chosen ?? "" };
}

/** EtcdSnapshotsDialog switches a cluster's etcd snapshots on or off and changes how they run. */
export function EtcdSnapshotsDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = etcdSnapshotsOf(c.variables["etcdSnapshots"]);
  const offered = useClassTargets(c.class);
  const targets = offered ?? [];
  const [enabled, setEnabled] = useState(current.enabled);
  const [typed, setTyped] = useState<EtcdSnapshotFields>(() => etcdFieldsOf(current));
  const { busy, error, send } = useChange(c, onClose);
  const hint = useId();
  const backups = backupsOf(c.variables["backups"]);
  const fields = withTarget(typed, targets, backups.enabled ? backups.target : undefined);
  const kept = current.enabled ? current.target : undefined;
  const problems = enabled ? etcdProblems(fields, targets, kept) : {};
  const next: EtcdSnapshotsValue = enabled ? etcdSnapshotsVariable(fields) : { ...current, enabled: false };
  const before: EtcdSnapshotsValue = current.enabled ? etcdSnapshotsVariable(etcdFieldsOf(current)) : current;
  const changed = JSON.stringify(next) !== JSON.stringify(before);
  const none = offered !== undefined && targets.length === 0 && !current.enabled;
  const update = (patch: Partial<EtcdSnapshotFields>): void => setTyped({ ...fields, ...patch });
  const listed = targets.some((t) => t.name === fields.target);
  return (
    <Dialog
      title="etcd snapshots"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { etcdSnapshots: next } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || Object.keys(problems).length > 0}>
            Save
          </button>
        </>
      }
    >
      <label className="check">
        <input
          type="checkbox"
          checked={enabled}
          disabled={none}
          aria-describedby={hint}
          onChange={(e) => setEnabled(e.target.checked)}
        />{" "}
        Take etcd snapshots
      </label>
      <p className="hint" id={hint}>
        {none
          ? "An administrator has set no backup target yet; they add one in the appliance's settings."
          : etcdSnapshotsHint}
      </p>
      {enabled && (
        <>
          <Field
            label="Target"
            hint="The snapshots go into the etcd folder of the cluster's own folder there."
            error={problems.target}
          >
            {(id) => (
              <select id={id} value={fields.target} onChange={(e) => update({ target: e.target.value })}>
                <option value="" disabled>
                  Choose…
                </option>
                {targets.map((t) => (
                  <option key={t.name} value={t.name}>
                    {t.name}: {targetText(t)}
                  </option>
                ))}
                {filled(fields.target) && !listed && (
                  <option value={fields.target}>{fields.target} (no longer offered)</option>
                )}
              </select>
            )}
          </Field>
          <Field
            label="Schedule"
            hint={`${scheduleProblem(fields.schedule) === undefined ? `${scheduleText(fields.schedule)}. ` : ""}In cron form: minute, hour, day of the month, month and day of the week.`}
            error={problems.schedule}
          >
            {(id) => (
              <input
                id={id}
                value={fields.schedule}
                spellCheck={false}
                onChange={(e) => update({ schedule: e.target.value })}
              />
            )}
          </Field>
          <Field label="Time zone" hint="The schedule's times are in this time zone.">
            {(id) => (
              <select id={id} value={fields.timeZone} onChange={(e) => update({ timeZone: e.target.value })}>
                {timeZones(fields.timeZone).map((z) => (
                  <option key={z} value={z}>
                    {z}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <Field
            label="Keep"
            hint="Snapshots. Once there are more, Kubehenge deletes the oldest from the target."
            error={problems.keep}
          >
            {(id) => (
              <input
                id={id}
                type="number"
                min={1}
                max={maxEtcdKeep}
                value={fields.keep}
                onChange={(e) => update({ keep: e.target.value })}
              />
            )}
          </Field>
          <CredentialField
            label="Access key"
            purpose="backup"
            cluster={c}
            keys={backupKeys}
            hint={`${backupCredentialHint} Kubehenge writes the snapshots with the same key.`}
          />
        </>
      )}
      {current.enabled && !enabled && (
        <p className="warning">
          Kubehenge takes no more snapshots. Those taken stay in the target, and the cluster can still be restored from
          them.
        </p>
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}

const when = (iso: string | undefined): string => (iso === undefined ? "" : new Date(iso).toLocaleString());

const poolName = (pool: string): string => pool.replace(/^\/pool\//u, "");

const minorOf = (v: string | undefined): string | undefined => /^v?(?<m>\d+\.\d+)/u.exec(v ?? "")?.groups?.["m"];

/** restoreProblem says why a cluster cannot be restored from a snapshot, as the controller would say it. */
export function restoreProblem(c: Cluster, s: EtcdSnapshot): string | undefined {
  if (filled(s.distribution) && s.distribution !== c.distribution) {
    return `The snapshot is of a ${distributionName(s.distribution)} cluster, and this one is ${distributionName(c.distribution)}.`;
  }
  const want = minorOf(s.kubernetesVersion);
  const have = minorOf(c.version);
  if (want !== undefined && have !== undefined && want !== have) {
    return `The snapshot holds the objects of Kubernetes ${want} and the cluster runs ${have}: restore one taken on ${have}.`;
  }
  return undefined;
}

// restoreState says how far a restore is, in the words of the step it is at.
function restoreState(r: EtcdRestoreStatus): { state: Health; text: string } {
  switch (r.phase) {
    case "Succeeded": {
      return { state: "ok", text: "Restored" };
    }
    case "Failed": {
      return { state: "error", text: "Failed" };
    }
    case "Preparing": {
      return { state: "progress", text: "Preparing" };
    }
    case "Fetching": {
      return { state: "progress", text: `Copying the snapshot to ${r.node ?? "the node"}` };
    }
    case "Stopping": {
      return { state: "progress", text: "Stopping the control plane" };
    }
    case "Restoring": {
      return { state: "progress", text: `Restoring etcd on ${r.node ?? "the node"}` };
    }
    case "Starting": {
      return { state: "progress", text: "Starting the control plane" };
    }
    case "Replacing": {
      break;
    }
  }
  return { state: "progress", text: "Replacing the other control-plane machines" };
}

function snapshotState(v: ClusterEtcdSnapshots): { state: Health | "idle"; text: string } {
  if (v.condition === undefined) {
    return { state: "idle", text: v.enabled ? "None taken yet" : "Off" };
  }
  return v.condition.status === "True" ? { state: "ok", text: "Taken" } : { state: "error", text: "Failed" };
}

/** EtcdSnapshotsView lists a cluster's etcd snapshots, takes one and restores from one. */
export function EtcdSnapshotsView({
  cluster: c,
  can,
  onEdit,
}: {
  cluster: Cluster;
  can: boolean;
  onEdit: (a: "etcdSnapshots") => void;
}): JSX.Element {
  const {
    data: v,
    error,
    reload,
  } = usePoll(
    () => api.etcdSnapshots(c.namespace, c.name),
    (d) => (d !== undefined && (d.requested || d.restoring) ? pollRunning : pollIdle),
  );
  const [restoring, setRestoring] = useState<EtcdSnapshot>();
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string>();

  if (!v) {
    return (
      <div className="box">
        <h3>etcd snapshots</h3>
        <ErrorBox message={error} />
        {!filled(error) && <p className="muted">Loading…</p>}
      </div>
    );
  }

  const pool = poolName(c.pool);
  const noSnapshot = v.canSnapshot ? undefined : `Taking a snapshot needs VM.Allocate on pool ${pool}.`;
  const noRestore = v.canRestore
    ? undefined
    : `Downloading a snapshot and restoring from one need Sys.Modify on pool ${pool}, as a snapshot holds every Secret of the cluster.`;
  const take = async (): Promise<void> => {
    setBusy(true);
    setActionError(undefined);
    try {
      await api.takeEtcdSnapshot(c.namespace, c.name);
      await reload();
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <div className="box">
        <h3>etcd snapshots</h3>
        <SnapshotsSummary cluster={c} view={v} />
        {v.restore !== undefined && <RestoreState restore={v.restore} snapshots={v.snapshots} />}
        {filled(v.message) && <p className="warning">{v.message}</p>}
        <div className="actions">
          {v.enabled ? (
            <button
              type="button"
              disabled={busy || !v.canSnapshot || v.requested || v.restoring}
              title={noSnapshot}
              onClick={() => {
                void take();
              }}
            >
              {v.requested ? "Taking a snapshot…" : "Take a snapshot now"}
            </button>
          ) : (
            <button type="button" disabled={!can} onClick={() => onEdit("etcdSnapshots")}>
              Set up etcd snapshots…
            </button>
          )}
        </div>
        {v.enabled && noSnapshot !== undefined && <p className="muted">{noSnapshot}</p>}
        <ErrorBox message={actionError} />
        <ErrorBox message={error} />
      </div>

      {(v.enabled || v.snapshots.length > 0) && (
        <SnapshotsTable cluster={c} view={v} noRestore={noRestore} onRestore={setRestoring} />
      )}

      {restoring !== undefined && (
        <EtcdRestoreDialog
          cluster={c}
          snapshot={restoring}
          automatic={v.automatic}
          onClose={() => setRestoring(undefined)}
          onDone={() => {
            setRestoring(undefined);
            void reload();
          }}
        />
      )}
    </>
  );
}

function SnapshotsSummary({ cluster: c, view: v }: { cluster: Cluster; view: ClusterEtcdSnapshots }): JSX.Element {
  if (!v.enabled) {
    return (
      <>
        <p>Kubehenge takes no etcd snapshots of this cluster.</p>
        <p className="hint">{etcdSnapshotsHint}</p>
      </>
    );
  }
  const s = snapshotState(v);
  return (
    <dl className="summary">
      <dt>Target</dt>
      <dd>{v.target ?? ""}</dd>
      <dt>Schedule</dt>
      <dd>{etcdSnapshotsText(etcdSnapshotsOf(c.variables["etcdSnapshots"]))}</dd>
      <dt>Last snapshot</dt>
      <dd>
        <Status state={s.state} text={s.text} />
        {v.last !== undefined && (
          <span className="muted">
            {" "}
            {when(v.last.time)} ({age(v.last.time)} ago)
          </span>
        )}
        {v.condition?.status === "False" && filled(v.condition.message) && (
          <div className="hint message">{sentence(v.condition.message)}</div>
        )}
      </dd>
    </dl>
  );
}

function RestoreState({
  restore: r,
  snapshots,
}: {
  restore: EtcdRestoreStatus;
  snapshots: EtcdSnapshot[];
}): JSX.Element {
  const s = restoreState(r);
  const at = r.finished ?? r.started;
  // By when it was taken, as the table lists it, while the target still holds it.
  const from = snapshots.find((x) => x.name === r.snapshot);
  return (
    <dl className="summary">
      <dt>Last restore</dt>
      <dd>
        <Status state={s.state} text={s.text} />
        <span className="muted" title={r.snapshot}>
          {" "}
          from {from === undefined ? r.snapshot : `the snapshot of ${when(from.time)}`}
          {filled(r.by) && `, asked by ${r.by}`}, {when(at)}
        </span>
        {filled(r.message) && <div className="hint message">{sentence(r.message)}</div>}
      </dd>
    </dl>
  );
}

function SnapshotsTable({
  cluster: c,
  view: v,
  noRestore,
  onRestore,
}: {
  cluster: Cluster;
  view: ClusterEtcdSnapshots;
  noRestore: string | undefined;
  onRestore: (s: EtcdSnapshot) => void;
}): JSX.Element {
  return (
    <div className="box scroll">
      <h3>Snapshots in the target</h3>
      <table className="grid">
        <thead>
          <tr>
            <th>Taken</th>
            <th>Kubernetes</th>
            <th>Revision</th>
            <th>Size</th>
            <th>
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {v.snapshots.length === 0 && (
            <tr>
              <td colSpan={5} className="muted">
                {filled(v.message) ? "Shown once the target can be read." : "No snapshots yet."}
              </td>
            </tr>
          )}
          {v.snapshots.map((s) => {
            const problem = restoreProblem(c, s);
            return (
              <tr key={s.name}>
                <td title={s.name}>
                  {when(s.time)} <span className="muted">({age(s.time)} ago)</span>
                </td>
                <td>{s.kubernetesVersion ?? ""}</td>
                <td>{s.revision ?? ""}</td>
                <td>{s.size === undefined ? "" : bytes(s.size)}</td>
                <td className="row-action">
                  {v.canRestore ? (
                    <a
                      className="button"
                      href={etcdSnapshotURL(c.namespace, c.name, s.name)}
                      download
                      aria-label={`Download ${s.name}`}
                    >
                      Download
                    </a>
                  ) : (
                    <button type="button" disabled title={noRestore} aria-label={`Download ${s.name}`}>
                      Download
                    </button>
                  )}{" "}
                  <button
                    type="button"
                    disabled={!v.canRestore || v.restoring || problem !== undefined}
                    title={noRestore ?? problem ?? (v.restoring ? "A restore is running." : undefined)}
                    aria-label={`Restore from ${s.name}`}
                    onClick={() => onRestore(s)}
                  >
                    Restore…
                  </button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {noRestore !== undefined && <p className="muted">{noRestore}</p>}
    </div>
  );
}

/** EtcdRestoreDialog restores a cluster's etcd from a snapshot, or says how to by hand. */
export function EtcdRestoreDialog({
  cluster: c,
  snapshot,
  automatic,
  onClose,
  onDone,
}: {
  cluster: Cluster;
  snapshot: EtcdSnapshot;
  automatic: boolean;
  onClose: () => void;
  onDone: () => void;
}): JSX.Element {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const restore = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.restoreEtcd(c.namespace, c.name, snapshot.name);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={`Restore etcd from ${when(snapshot.time)}`}
      onClose={onClose}
      onSubmit={() => {
        if (automatic) {
          void restore();
        }
      }}
      actions={
        automatic ? (
          <>
            <button type="button" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="danger" disabled={busy || typed !== c.name}>
              Restore
            </button>
          </>
        ) : (
          <button type="button" onClick={onClose}>
            Close
          </button>
        )
      }
    >
      <p className="warning">{etcdRestoreHint}</p>
      {automatic ? (
        <>
          <p>
            Kubehenge pauses the cluster, stops the control plane on every control-plane VM through the Proxmox VE guest
            agent, restores etcd on one of them and replaces the others with new machines. The cluster&apos;s API is
            down until the first is back, and whatever was created since {when(snapshot.time)} is gone.
          </p>
          <Field label="Confirm name" hint={`Type ${c.name}.`}>
            {(id) => <input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />}
          </Field>
        </>
      ) : (
        <RestoreSteps cluster={c} snapshot={snapshot} />
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}

// RestoreSteps lists the commands that restore the cluster by hand, for a management cluster whose
// controller does not reach the VMs' guest agent.
function RestoreSteps({ cluster: c, snapshot }: { cluster: Cluster; snapshot: EtcdSnapshot }): JSX.Element {
  const [plan, setPlan] = useState<EtcdRestorePlan>();
  const [error, setError] = useState<string>();
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        setPlan(await api.etcdRestorePlan(c.namespace, c.name, snapshot.name));
      } catch (err) {
        setError(errorMessage(err));
      }
    }
    void load();
  }, [c.namespace, c.name, snapshot.name]);
  return (
    <div className="restore-steps">
      <p>
        This management cluster does not restore through the Proxmox VE guest agent; an administrator can let it in the
        appliance&apos;s settings. Until then, these are the steps, run as they come: the commands on a node over SSH or
        its console, and the kubectl ones with the management cluster&apos;s kubeconfig.
      </p>
      <p>
        <a className="button" href={etcdSnapshotURL(c.namespace, c.name, snapshot.name)} download>
          Download the snapshot
        </a>{" "}
        <span className="muted">
          Decrypted{snapshot.size === undefined ? "" : `, ${bytes(snapshot.size)}`}. It holds every Secret of the
          cluster, so keep it as safe as an admin kubeconfig.
        </span>
      </p>
      <ErrorBox message={error} />
      {plan === undefined && !filled(error) && <p className="muted">Loading…</p>}
      {plan !== undefined && (
        <ol className="steps">
          {plan.steps.map((step) => (
            <li key={step.title}>
              <p>
                {step.title}. <span className="muted">On {step.where}:</span>
              </p>
              <pre className="restore-command" aria-label={step.title}>
                {step.command}
              </pre>
            </li>
          ))}
        </ol>
      )}
    </div>
  );
}
