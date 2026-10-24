import { useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import type { OptionRow } from "./ClusterOptions";
import { CredentialField, type CredentialKey } from "./CredentialField";
import type { BackupRun, Cluster, ClusterBackups, ClusterClass, RestoreRun } from "./types";
import {
  Dialog,
  ErrorBox,
  Field,
  Status,
  age,
  browserZone,
  bytes,
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
import { oneOf, quantity, record, records, text } from "./values";

/** The chart's defaults for a cluster's backups. */
export const defaultSchedule = "0 2 * * *";
export const defaultRetentionDays = 7;
const maxRetentionDays = 3650;
const maxScheduleLength = 128;
const maxRestoreNamespaces = 100;

export const backupVolumeChoices = ["all", "annotated"] as const;
export type BackupVolumes = (typeof backupVolumeChoices)[number];

/** The class annotation that lists the targets an administrator set. */
export const backupTargetsAnnotation = "kubehenge.dev/backup-targets";

/** The keys of a backup credential, as the interface stores them. */
export const backupKeys: CredentialKey[] = [
  { key: "accessKeyID", label: "Access key ID", plain: true },
  { key: "secretAccessKey", label: "Secret access key" },
];

export const backupsHint =
  "Velero backs up every namespace but Kubernetes' own and the add-ons', with the data in its pods' volumes, to an S3 store an administrator set. Each cluster writes under a folder of its own, encrypted with keys of its own that Kubehenge keeps.";

export const backupCredentialHint =
  "The S3 access key Velero signs in to the target with. Without one of its own, the cluster takes the one an administrator stored for every cluster.";

/** A cluster's backups variable. */
export interface BackupsValue {
  enabled: boolean;
  target?: string;
  schedule?: string;
  timeZone?: string;
  retentionDays?: number;
  volumes?: BackupVolumes;
  existingSecret?: string;
}

/** backupsOf reads a cluster's backups variable; unset, backups are off. */
export function backupsOf(v: unknown): BackupsValue {
  const r = record(v);
  const out: BackupsValue = { enabled: r["enabled"] === true };
  for (const k of ["target", "schedule", "timeZone", "existingSecret"] as const) {
    const s = text(r[k]);
    if (filled(s)) {
      out[k] = s;
    }
  }
  const days = quantity(r["retentionDays"]);
  if (days !== undefined) {
    out.retentionDays = days;
  }
  if (oneOf(backupVolumeChoices, r["volumes"])) {
    out.volumes = r["volumes"];
  }
  return out;
}

/** A target as a class offers it. */
export interface OfferedTarget {
  name: string;
  bucket: string;
  endpoint?: string;
}

/** classTargets lists the backup targets a class offers. */
export function classTargets(c: ClusterClass | undefined): OfferedTarget[] {
  let listed: unknown;
  try {
    listed = JSON.parse(c?.annotations?.[backupTargetsAnnotation] ?? "[]");
  } catch {
    // The controller refuses backups of such a class with the reason.
    return [];
  }
  return records(listed).flatMap((r) => {
    const name = text(r["name"]);
    const bucket = text(r["bucket"]);
    if (!filled(name) || !filled(bucket)) {
      return [];
    }
    const t: OfferedTarget = { name, bucket };
    const endpoint = text(r["endpoint"]);
    if (filled(endpoint)) {
      t.endpoint = endpoint;
    }
    return [t];
  });
}

/** targetText says where a target keeps the backups. */
export function targetText(t: OfferedTarget): string {
  if (!filled(t.endpoint)) {
    return `bucket ${t.bucket} in AWS S3`;
  }
  return `bucket ${t.bucket} at ${URL.canParse(t.endpoint) ? new URL(t.endpoint).host : t.endpoint}`;
}

/** useClassTargets reads the targets an existing cluster's class offers; undefined until known. */
export function useClassTargets(className: string): OfferedTarget[] | undefined {
  const [targets, setTargets] = useState<OfferedTarget[]>();
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        const classes = await api.classes();
        setTargets(classTargets(classes.find((c) => c.name === className)));
      } catch {
        setTargets([]);
      }
    }
    void load();
  }, [className]);
  return targets;
}

const weekdayNames = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const lastMinute = 59;
const lastHour = 23;
const daysPerWeek = 7;

const descriptors: Record<string, string> = {
  "@hourly": "Every hour",
  "@daily": "Every day at 00:00",
  "@midnight": "Every day at 00:00",
  "@weekly": "Every Sunday at 00:00",
  "@monthly": "On the first of every month at 00:00",
  "@yearly": "Every 1 January at 00:00",
  "@annually": "Every 1 January at 00:00",
};

const twoDigits = (n: number): string => String(n).padStart(2, "0");

/** scheduleText says in words when a cron schedule runs, for the forms people mostly write. */
export function scheduleText(schedule: string): string {
  const s = schedule.trim().replaceAll(/\s+/gu, " ");
  const named = descriptors[s];
  if (named !== undefined) {
    return named;
  }
  const at = /^(?<m>\d{1,2}) (?<h>\d{1,2}) \* \* (?<d>\*|[0-7](?:,[0-7])*)$/u.exec(s)?.groups;
  if (at !== undefined) {
    const m = Number(at["m"]);
    const h = Number(at["h"]);
    if (m <= lastMinute && h <= lastHour) {
      const time = `${twoDigits(h)}:${twoDigits(m)}`;
      const days = at["d"] ?? "*";
      if (days === "*") {
        return `Every day at ${time}`;
      }
      const names = [...new Set(days.split(",").map((d) => weekdayNames[Number(d) % daysPerWeek] ?? d))];
      return `Every ${names.join(", ")} at ${time}`;
    }
  }
  const hourly = /^(?<m>\d{1,2}) \* \* \* \*$/u.exec(s)?.groups;
  if (hourly !== undefined && Number(hourly["m"]) <= lastMinute) {
    return `Every hour at minute ${Number(hourly["m"])}`;
  }
  const hours = /^(?<m>\d{1,2}) \*\/(?<n>\d{1,2}) \* \* \*$/u.exec(s)?.groups;
  if (hours !== undefined && Number(hours["m"]) <= lastMinute && Number(hours["n"]) >= 1) {
    return `Every ${Number(hours["n"])} hours from 00:${twoDigits(Number(hours["m"]))}`;
  }
  return `By the cron schedule ${s}`;
}

/** scheduleProblem says what Velero would refuse in a schedule; the server checks it in full. */
export function scheduleProblem(schedule: string): string | undefined {
  const s = schedule.trim();
  if (!s) {
    return "A schedule, such as 0 2 * * * for every day at 02:00.";
  }
  if (s.length > maxScheduleLength) {
    return `At most ${maxScheduleLength} characters.`;
  }
  if (/^(?:CRON_)?TZ=/u.test(s)) {
    return "The time zone is chosen on its own, not in the schedule.";
  }
  if (s.startsWith("@")) {
    return /^@(?:yearly|annually|monthly|weekly|daily|midnight|hourly|every \S+)$/u.test(s)
      ? undefined
      : "One of @hourly, @daily, @weekly, @monthly or @yearly, or five cron fields.";
  }
  if (!/^[\d*,/?A-Za-z-]+(?: +[\d*,/?A-Za-z-]+){4}$/u.test(s)) {
    return "Five fields, as cron takes them: minute, hour, day of the month, month and day of the week.";
  }
  return undefined;
}

/** The backup settings as typed. */
export interface BackupFields {
  target: string;
  schedule: string;
  timeZone: string;
  retention: string;
  volumes: BackupVolumes;
}

/** newBackupFields are a new cluster's settings: daily at 02:00 in the browser's time zone, kept a week. */
export const newBackupFields = (): BackupFields => ({
  target: "",
  schedule: defaultSchedule,
  timeZone: browserZone(),
  retention: String(defaultRetentionDays),
  volumes: "all",
});

/** backupFieldsOf are the fields of a cluster's settings; what it leaves unset is the chart's default. */
export function backupFieldsOf(b: BackupsValue): BackupFields {
  return {
    target: b.target ?? "",
    schedule: b.schedule ?? defaultSchedule,
    // What is in effect: UTC when backups name none; backups switched on start in the browser's zone.
    timeZone: b.timeZone ?? (b.enabled ? "UTC" : browserZone()),
    retention: String(b.retentionDays ?? defaultRetentionDays),
    volumes: b.volumes ?? "all",
  };
}

/** withTarget chooses the one target there is, until another is chosen. */
export const withTarget = (f: BackupFields, targets: OfferedTarget[]): BackupFields =>
  filled(f.target) ? f : { ...f, target: sole(targets.map((t) => t.name)) ?? "" };

export type BackupField = "target" | "schedule" | "retention";

/**
 * backupProblems says, by field, what Kubehenge would refuse. A target an administrator has since
 * taken away stays allowed for the cluster that names it, as the controller allows it.
 */
export function backupProblems(
  f: BackupFields,
  targets: OfferedTarget[],
  kept?: string,
): Partial<Record<BackupField, string>> {
  const p: Partial<Record<BackupField, string>> = {};
  const target = targetProblem(f.target, targets, kept);
  if (target !== undefined) {
    p.target = target;
  }
  const schedule = scheduleProblem(f.schedule);
  if (schedule !== undefined) {
    p.schedule = schedule;
  }
  const days = Number(f.retention);
  if (!/^\d+$/u.test(f.retention) || days < 1 || days > maxRetentionDays) {
    p.retention = `A whole number of days from 1 to ${maxRetentionDays}.`;
  }
  return p;
}

/** targetProblem says what Kubehenge would refuse in the target chosen; kept is the one set already. */
export function targetProblem(target: string, targets: OfferedTarget[], kept?: string): string | undefined {
  if (targets.length === 0 && !filled(kept)) {
    return "An administrator has set no backup target yet; they add one in the appliance's settings.";
  }
  if (!filled(target)) {
    return "Choose a target.";
  }
  if (target !== kept && !targets.some((t) => t.name === target)) {
    return "Not one of the targets an administrator set.";
  }
  return undefined;
}

/** backupsVariable is the variable the fields set, keeping a Secret the cluster names for the keys. */
export function backupsVariable(f: BackupFields, existingSecret?: string): BackupsValue {
  const v: BackupsValue = {
    enabled: true,
    target: f.target,
    schedule: f.schedule.trim(),
    timeZone: f.timeZone,
    retentionDays: Number(f.retention),
    volumes: f.volumes,
  };
  if (filled(existingSecret)) {
    v.existingSecret = existingSecret;
  }
  return v;
}

const daysText = (n: number): string => (n === 1 ? "1 day" : `${n} days`);

/** backupsText sums up a cluster's backups. */
export function backupsText(b: BackupsValue): string {
  if (!b.enabled) {
    return "Off";
  }
  const f = backupFieldsOf(b);
  return `${scheduleText(f.schedule)} (${f.timeZone}), kept ${daysText(Number(f.retention))}, to ${f.target}`;
}

/** BackupFieldset edits a cluster's backup settings; problems are those to show. */
export function BackupFieldset({
  value,
  onChange,
  targets,
  problems,
}: {
  value: BackupFields;
  onChange: (f: BackupFields) => void;
  targets: OfferedTarget[];
  problems: Partial<Record<BackupField, string | undefined>>;
}): JSX.Element {
  const update = (patch: Partial<BackupFields>): void => onChange({ ...value, ...patch });
  const listed = targets.some((t) => t.name === value.target);
  return (
    <>
      <Field
        label="Target"
        hint="The cluster's backups go into a folder of its own there, named for its namespace and name."
        error={problems.target}
      >
        {(id) => (
          <select id={id} value={value.target} onChange={(e) => update({ target: e.target.value })}>
            <option value="" disabled>
              Choose…
            </option>
            {targets.map((t) => (
              <option key={t.name} value={t.name}>
                {t.name}: {targetText(t)}
              </option>
            ))}
            {filled(value.target) && !listed && (
              <option value={value.target}>{value.target} (no longer offered)</option>
            )}
          </select>
        )}
      </Field>
      <Field
        label="Schedule"
        hint={`${scheduleProblem(value.schedule) === undefined ? `${scheduleText(value.schedule)}. ` : ""}In cron form: minute, hour, day of the month, month and day of the week.`}
        error={problems.schedule}
      >
        {(id) => (
          <input
            id={id}
            value={value.schedule}
            spellCheck={false}
            onChange={(e) => update({ schedule: e.target.value })}
          />
        )}
      </Field>
      <Field label="Time zone" hint="The schedule's times are in this time zone.">
        {(id) => (
          <select id={id} value={value.timeZone} onChange={(e) => update({ timeZone: e.target.value })}>
            {timeZones(value.timeZone).map((z) => (
              <option key={z} value={z}>
                {z}
              </option>
            ))}
          </select>
        )}
      </Field>
      <Field
        label="Keep each for"
        hint="Days. Velero deletes a backup from the target this long after making it."
        error={problems.retention}
      >
        {(id) => (
          <input
            id={id}
            type="number"
            min={1}
            max={maxRetentionDays}
            value={value.retention}
            onChange={(e) => update({ retention: e.target.value })}
          />
        )}
      </Field>
      <Field
        label="Volumes"
        hint={
          value.volumes === "all"
            ? "The data in every pod volume but emptyDir ones."
            : "Only the volumes a pod names in its backup.velero.io/backup-volumes annotation."
        }
      >
        {(id) => (
          <select
            id={id}
            value={value.volumes}
            onChange={(e) => {
              const v = e.target.value;
              if (oneOf(backupVolumeChoices, v)) {
                update({ volumes: v });
              }
            }}
          >
            <option value="all">Every pod volume</option>
            <option value="annotated">Those their pods name</option>
          </select>
        )}
      </Field>
    </>
  );
}

/** backupsRows is the Options view's row of a cluster's backups, where its class offers them. */
export function backupsRows(c: Cluster, offered: boolean): OptionRow<"backups">[] {
  const b = backupsOf(c.variables["backups"]);
  if (!b.enabled && !offered) {
    return [];
  }
  return [{ label: "Backups", value: backupsText(b), action: "backups", name: "backups" }];
}

/** BackupsDialog switches a cluster's backups on or off and changes how they run. */
export function BackupsDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = backupsOf(c.variables["backups"]);
  const offered = useClassTargets(c.class);
  const targets = offered ?? [];
  const [enabled, setEnabled] = useState(current.enabled);
  const [typed, setTyped] = useState<BackupFields>(() => backupFieldsOf(current));
  const { busy, error, send } = useChange(c, onClose);
  const hint = useId();
  const fields = withTarget(typed, targets);
  const kept = current.enabled ? current.target : undefined;
  const problems = enabled ? backupProblems(fields, targets, kept) : {};
  const next: BackupsValue = enabled ? backupsVariable(fields, current.existingSecret) : { ...current, enabled: false };
  const before: BackupsValue = current.enabled
    ? backupsVariable(backupFieldsOf(current), current.existingSecret)
    : current;
  const changed = JSON.stringify(next) !== JSON.stringify(before);
  const none = offered !== undefined && targets.length === 0 && !current.enabled;
  return (
    <Dialog
      title="Backups"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { backups: next } });
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
        Back up with Velero
      </label>
      <p className="hint" id={hint}>
        {none
          ? "An administrator has set no backup target yet; they add one in the appliance's settings."
          : backupsHint}
      </p>
      {enabled && <BackupFieldset value={fields} onChange={setTyped} targets={targets} problems={problems} />}
      {enabled && filled(current.existingSecret) && (
        <p className="hint">
          The access key comes from Secret {current.existingSecret} in the cluster&apos;s velero namespace.
        </p>
      )}
      {enabled && !filled(current.existingSecret) && (
        <CredentialField
          label="Access key"
          purpose="backup"
          cluster={c}
          keys={backupKeys}
          hint={backupCredentialHint}
        />
      )}
      {current.enabled && !enabled && (
        <p className="warning">
          Velero and its schedule leave the cluster. The backups stay in the target, and Kubehenge keeps the
          cluster&apos;s keys, so switching backups on again with the same target brings them back into view.
        </p>
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}

const runningPhases = new Set(["New", "InProgress", "WaitingForPluginOperations", "Finalizing"]);

/** phaseStatus says how far a backup or restore is, as the cluster list says it of clusters. */
function phaseStatus(phase: string): { state: Health; text: string } {
  switch (phase) {
    case "Completed": {
      return { state: "ok", text: "Completed" };
    }
    case "PartiallyFailed": {
      return { state: "error", text: "Partly failed" };
    }
    case "Failed":
    case "FailedValidation": {
      return { state: "error", text: "Failed" };
    }
    case "Deleting": {
      return { state: "deleting", text: "Deleting" };
    }
    default: {
      return { state: "progress", text: phase === "" || phase === "New" ? "Waiting" : "Running" };
    }
  }
}

const running = (v: ClusterBackups | undefined): boolean =>
  v !== undefined && [...v.backups, ...v.restores].some((r) => r.phase === "" || runningPhases.has(r.phase));

const restorable = (b: BackupRun): boolean => b.phase === "Completed" || b.phase === "PartiallyFailed";

const when = (iso: string | undefined): string => (iso === undefined ? "" : new Date(iso).toLocaleString());

const poolName = (pool: string): string => pool.replace(/^\/pool\//u, "");

// outcome sums up what a run did: its warnings and errors, and why it failed.
function outcome(r: { warnings: number; errors: number; failure?: string }): string {
  const parts: string[] = [];
  if (r.errors > 0) {
    parts.push(r.errors === 1 ? "1 error" : `${r.errors} errors`);
  }
  if (r.warnings > 0) {
    parts.push(r.warnings === 1 ? "1 warning" : `${r.warnings} warnings`);
  }
  const counted = parts.length > 0 ? `${sentence(parts.join(", "))} Velero's describe command lists them.` : "";
  return [filled(r.failure) ? sentence(r.failure) : "", counted].filter(Boolean).join(" ");
}

function restoreProgress(r: RestoreRun): string {
  if (r.volumes > 0) {
    return `${r.volumesDone} of ${r.volumes} volumes, ${bytes(r.bytesDone)} of ${bytes(r.bytesTotal)}`;
  }
  return r.total > 0 ? `${r.items} of ${r.total} items` : "";
}

/** BackupsView lists a cluster's backups and restores, and starts either. */
export function BackupsView({
  cluster: c,
  can,
  onEdit,
}: {
  cluster: Cluster;
  can: boolean;
  onEdit: (a: "backups") => void;
}): JSX.Element {
  const {
    data: v,
    error,
    reload,
  } = usePoll(
    () => api.backups(c.namespace, c.name),
    (d) => (running(d) ? pollRunning : pollIdle),
  );
  const [restoring, setRestoring] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string>();
  const settings = backupsOf(c.variables["backups"]);

  if (!v) {
    return (
      <div className="box">
        <ErrorBox message={error} />
        {!filled(error) && <p className="muted">Loading…</p>}
      </div>
    );
  }
  if (!v.enabled) {
    return (
      <div className="box">
        <p>Backups are off for this cluster.</p>
        <p className="hint">{backupsHint}</p>
        <div className="actions">
          <button type="button" disabled={!can} onClick={() => onEdit("backups")}>
            Set up backups…
          </button>
        </div>
      </div>
    );
  }

  const pool = poolName(c.pool);
  const noBackUp = v.canBackUp ? undefined : `Backing up needs VM.Allocate on pool ${pool}.`;
  const noRestore = v.canRestore
    ? undefined
    : `Restoring needs Sys.Modify on pool ${pool}, as a restore writes whatever the backup holds, Secrets and roles among it.`;
  const backUp = async (): Promise<void> => {
    setBusy(true);
    setActionError(undefined);
    try {
      await api.backUp(c.namespace, c.name);
      await reload();
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  const location = v.location === "Available" ? "Reachable" : (v.location ?? "Not checked yet");

  return (
    <>
      <div className="box">
        <dl className="summary">
          <dt>Target</dt>
          <dd>{v.target ?? ""}</dd>
          <dt>Schedule</dt>
          <dd>{backupsText(settings)}</dd>
          <dt>Target status</dt>
          <dd>
            {v.reachable ? (
              <Status state={v.location === "Available" ? "ok" : "error"} text={location} />
            ) : (
              <Status state="idle" text="Unknown" />
            )}
            {filled(v.locationMessage) && <div className="hint message">{sentence(v.locationMessage)}</div>}
          </dd>
          <dt>Last scheduled backup</dt>
          <dd>{v.lastBackup === undefined ? "None yet" : `${when(v.lastBackup)} (${age(v.lastBackup)} ago)`}</dd>
        </dl>
        {filled(v.message) && <p className="warning">{v.message}</p>}
        <div className="actions">
          <button
            type="button"
            disabled={busy || !v.canBackUp || !v.reachable}
            title={noBackUp}
            onClick={() => {
              void backUp();
            }}
          >
            Back up now
          </button>
        </div>
        {noBackUp !== undefined && <p className="muted">{noBackUp}</p>}
        {noRestore !== undefined && <p className="muted">{noRestore}</p>}
        <ErrorBox message={actionError} />
        <ErrorBox message={error} />
      </div>

      <BackupsTable view={v} noRestore={noRestore} onRestore={setRestoring} />
      {v.restores.length > 0 && <RestoresTable restores={v.restores} />}

      {restoring !== undefined && (
        <RestoreDialog
          cluster={c}
          backup={restoring}
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

const ago = (iso: string | undefined): string => (iso === undefined ? "" : `${age(iso)} ago`);

function BackupsTable({
  view: v,
  noRestore,
  onRestore,
}: {
  view: ClusterBackups;
  noRestore: string | undefined;
  onRestore: (backup: string) => void;
}): JSX.Element {
  return (
    <div className="box scroll">
      <h3>Backups</h3>
      <table className="grid">
        <thead>
          <tr>
            <th>Name</th>
            <th>Made by</th>
            <th>Started</th>
            <th>Status</th>
            <th>Items</th>
            <th>Deleted on</th>
            <th>
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {v.backups.length === 0 && (
            <tr>
              <td colSpan={7} className="muted">
                {v.reachable ? "No backups yet." : "Shown once Velero answers."}
              </td>
            </tr>
          )}
          {v.backups.map((b) => {
            const s = phaseStatus(b.phase);
            const said = outcome(b);
            return (
              <tr key={b.name}>
                <td>{b.name}</td>
                <td>{b.scheduled ? "The schedule" : (b.by ?? "")}</td>
                <td title={when(b.started)}>{ago(b.started)}</td>
                <td>
                  <Status state={s.state} text={s.text} />
                  {said && <div className="hint message">{said}</div>}
                </td>
                <td>{b.total > 0 ? `${b.items} of ${b.total}` : ""}</td>
                <td>{b.expires === undefined ? "" : new Date(b.expires).toLocaleDateString()}</td>
                <td className="row-action">
                  <button
                    type="button"
                    disabled={!v.canRestore || !restorable(b)}
                    title={noRestore ?? (restorable(b) ? undefined : "Only a finished backup can be restored.")}
                    aria-label={`Restore ${b.name}`}
                    onClick={() => onRestore(b.name)}
                  >
                    Restore…
                  </button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function RestoresTable({ restores }: { restores: RestoreRun[] }): JSX.Element {
  return (
    <div className="box scroll">
      <h3>Restores</h3>
      <table className="grid">
        <thead>
          <tr>
            <th>Backup</th>
            <th>Namespaces</th>
            <th>Asked by</th>
            <th>Started</th>
            <th>Status</th>
            <th>Progress</th>
          </tr>
        </thead>
        <tbody>
          {restores.map((r) => {
            const s = phaseStatus(r.phase);
            const said = outcome(r);
            const whole = r.namespaces === undefined || r.namespaces.includes("*");
            return (
              <tr key={r.name}>
                <td>{r.backup}</td>
                <td>{whole ? "Every one" : (r.namespaces ?? []).join(", ")}</td>
                <td>{r.by ?? ""}</td>
                <td title={when(r.started)}>{ago(r.started)}</td>
                <td>
                  <Status state={s.state} text={s.text} />
                  {said && <div className="hint message">{said}</div>}
                </td>
                <td>
                  {restoreProgress(r)}
                  {r.bytesTotal > 0 && runningPhases.has(r.phase) && (
                    <div>
                      <progress value={r.bytesDone} max={r.bytesTotal} aria-label={`Restore of ${r.backup}`} />
                    </div>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

const namespaceName = /^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/u;

/** namespaceProblem says what the server would refuse in a list of namespaces to restore. */
export function namespaceProblem(names: string[]): string | undefined {
  if (names.length === 0) {
    return "Name at least one namespace.";
  }
  if (names.length > maxRestoreNamespaces) {
    return `At most ${maxRestoreNamespaces} namespaces.`;
  }
  const bad = names.find((n) => !namespaceName.test(n));
  if (bad !== undefined) {
    return `${bad} is not a namespace's name.`;
  }
  const twice = names.find((n, i) => names.indexOf(n) !== i);
  return twice === undefined ? undefined : `${twice} is named twice.`;
}

const splitNames = (typed: string): string[] => typed.split(/[\s,]+/u).filter(Boolean);

function RestoreDialog({
  cluster: c,
  backup,
  onClose,
  onDone,
}: {
  cluster: Cluster;
  backup: string;
  onClose: () => void;
  onDone: () => void;
}): JSX.Element {
  const [whole, setWhole] = useState(true);
  const [typed, setTyped] = useState("");
  const [edited, setEdited] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const names = splitNames(typed);
  const problem = whole ? undefined : namespaceProblem(names);
  const restore = async (): Promise<void> => {
    if (problem !== undefined) {
      setEdited(true);
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.restore(c.namespace, c.name, { backup, namespaces: whole ? [] : names });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={`Restore ${backup}`}
      onClose={onClose}
      onSubmit={() => {
        void restore();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy}>
            Restore
          </button>
        </>
      }
    >
      <p>
        Velero brings back what the backup holds: its resources and the data in its pods&apos; volumes. What the cluster
        still has is left as it is, so delete a namespace first to have it back as it was.
      </p>
      <fieldset className="group">
        <legend>What to restore</legend>
        <label className="check">
          <input type="radio" name="restore-scope" checked={whole} onChange={() => setWhole(true)} /> The whole backup
        </label>
        <label className="check">
          <input type="radio" name="restore-scope" checked={!whole} onChange={() => setWhole(false)} /> Chosen
          namespaces
        </label>
        {!whole && (
          <Field label="Namespaces" hint="Separated by commas or spaces." error={edited ? problem : undefined}>
            {(id) => (
              <input
                id={id}
                value={typed}
                spellCheck={false}
                onChange={(e) => {
                  setTyped(e.target.value);
                  setEdited(true);
                }}
              />
            )}
          </Field>
        )}
      </fieldset>
      <ErrorBox message={error} />
    </Dialog>
  );
}
