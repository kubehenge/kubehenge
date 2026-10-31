import { useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import { ago, deletable, outcome, phaseStatus, restorable, running, when } from "./backup-runs";
import type { OptionRow } from "./ClusterOptions";
import { CredentialField, type CredentialKey } from "./CredentialField";
import { useFleet } from "./fleet";
import { RestoreDialog, RestoredNotice, RestoresTable, ResultsDialog } from "./Restores";
import type { BackupRun, BackupSource, Cluster, ClusterBackups, ClusterClass, RestoreRun } from "./types";
import {
  Detail,
  Dialog,
  ErrorBox,
  Field,
  Info,
  Status,
  age,
  browserZone,
  errorMessage,
  filled,
  pollCache,
  pollIdle,
  pollRunning,
  sentence,
  sole,
  timeZones,
  useChange,
  usePoll,
  Skeleton,
} from "./ui";
import { oneOf, quantity, record, records, text, texts } from "./values";

const backupsCache = pollCache<ClusterBackups>();

/** The chart's defaults for a cluster's backups. */
export const defaultSchedule = "0 2 * * *";
export const defaultRetentionDays = 7;
const maxRetentionDays = 3650;
const maxScheduleLength = 128;
const maxBackupSources = 10;

export const backupVolumeChoices = ["all", "annotated"] as const;
export type BackupVolumes = (typeof backupVolumeChoices)[number];

/** The class annotation that lists the targets an administrator set. */
export const backupTargetsAnnotation = "kubehenge.dev/backup-targets";

/** The keys of a backup credential, as the interface stores them. */
export const backupKeys: CredentialKey[] = [
  { key: "accessKeyID", label: "Access key ID", plain: true },
  { key: "secretAccessKey", label: "Secret access key" },
];

export const backupsHint = "Every namespace but system and add-on ones, with volume data";

export const backupCredentialHint = "Limit it to this cluster's folder";

export const noTargets = "No backup targets. An administrator adds them in the appliance settings.";

/** A cluster's backups variable. */
export interface BackupsValue {
  enabled: boolean;
  target?: string;
  schedule?: string;
  timeZone?: string;
  retentionDays?: number;
  volumes?: BackupVolumes;
  existingSecret?: string;
  /** The clusters of its namespace whose backups the cluster's Velero lists beside its own. */
  readFrom?: string[];
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
  const sources = texts(r["readFrom"]) ?? [];
  if (sources.length > 0) {
    out.readFrom = sources;
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
  return `Cron ${s}`;
}

/** scheduleProblem says what Velero would refuse in a schedule; the server checks it in full. */
export function scheduleProblem(schedule: string): string | undefined {
  const s = schedule.trim();
  if (!s) {
    return "Enter a schedule, such as 0 2 * * *";
  }
  if (s.length > maxScheduleLength) {
    return `Schedule must be ${maxScheduleLength} characters or less`;
  }
  if (/^(?:CRON_)?TZ=/u.test(s)) {
    return "Set the time zone in its own field";
  }
  if (s.startsWith("@")) {
    return /^@(?:yearly|annually|monthly|weekly|daily|midnight|hourly|every \S+)$/u.test(s)
      ? undefined
      : "Use @hourly, @daily, @weekly, @monthly, @yearly or 5 fields";
  }
  if (!/^[\d*,/?A-Za-z-]+(?: +[\d*,/?A-Za-z-]+){4}$/u.test(s)) {
    return "Enter 5 fields: minute hour day month weekday";
  }
  return undefined;
}

/** scheduleHint reads a valid schedule back in words, or names cron's fields. */
export function scheduleHint(schedule: string): string {
  const read = scheduleText(schedule);
  return scheduleProblem(schedule) === undefined && !read.startsWith("Cron ") ? read : "minute hour day month weekday";
}

/** The backup settings as typed. */
export interface BackupFields {
  target: string;
  schedule: string;
  timeZone: string;
  retention: string;
  volumes: BackupVolumes;
  readFrom: string[];
}

/** newBackupFields are a new cluster's settings: daily at 02:00 in the browser's time zone, kept a week. */
export const newBackupFields = (): BackupFields => ({
  target: "",
  schedule: defaultSchedule,
  timeZone: browserZone(),
  retention: String(defaultRetentionDays),
  volumes: "all",
  readFrom: [],
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
    readFrom: b.readFrom ?? [],
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
    p.retention = `A whole number from 1 to ${maxRetentionDays}`;
  }
  return p;
}

/** targetProblem says what Kubehenge would refuse in the target chosen; kept is the one set already. */
export function targetProblem(target: string, targets: OfferedTarget[], kept?: string): string | undefined {
  if (targets.length === 0 && !filled(kept)) {
    return noTargets;
  }
  if (!filled(target)) {
    return "Choose a target";
  }
  if (target !== kept && !targets.some((t) => t.name === target)) {
    return "Choose one of the offered targets";
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
  if (f.readFrom.length > 0) {
    v.readFrom = f.readFrom.toSorted();
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
  const reads = f.readFrom.length > 0 ? `, reading those of ${f.readFrom.join(", ")}` : "";
  return `${scheduleText(f.schedule)} (${f.timeZone}), kept ${daysText(Number(f.retention))}, to ${f.target}${reads}`;
}

/**
 * BackupSourcesField chooses the clusters whose backups a cluster's Velero lists beside its own,
 * to restore them into it: those of its namespace that back up to the same target.
 */
export function BackupSourcesField({
  namespace,
  self,
  target,
  value,
  creating,
  onChange,
}: {
  namespace: string;
  self: string;
  target: string;
  value: string[];
  /** Whether the cluster is new, and so can share the first one's repository key. */
  creating: boolean;
  onChange: (sources: string[]) => void;
}): JSX.Element {
  const [clusters, setClusters] = useState<Cluster[]>();
  const [error, setError] = useState<string>();
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        setClusters(await api.clusters());
      } catch (err) {
        setError(errorMessage(err));
      }
    }
    void load();
  }, []);
  const backingUp = (clusters ?? []).filter((c) => {
    const b = backupsOf(c.variables["backups"]);
    return c.namespace === namespace && c.name !== self && b.enabled && b.target === target;
  });
  const offered = new Set(backingUp.map((c) => c.name));
  const names = [...new Set([...offered, ...value])].toSorted();
  const full = value.length >= maxBackupSources;
  return (
    <fieldset className="group">
      <legend>Restore from other clusters</legend>
      <p className="hint">
        Needs Sys.Modify on each one&apos;s pool{" "}
        <Info about="restoring from other clusters">
          {creating
            ? "Lets this cluster read their backups. Volume data comes back for the first one picked."
            : "Lets this cluster read their backups. Volume data comes back only with a shared repository key."}
        </Info>
      </p>
      {clusters === undefined && !filled(error) && <p className="muted">Loading…</p>}
      {clusters !== undefined && names.length === 0 && (
        <p className="muted">No other cluster here backs up to {filled(target) ? target : "this target"}</p>
      )}
      {names.map((name) => {
        const named = value.includes(name);
        return (
          <label key={name} className="check">
            <input
              type="checkbox"
              checked={named}
              disabled={!named && full}
              onChange={(e) => onChange(e.target.checked ? [...value, name] : value.filter((x) => x !== name))}
            />{" "}
            {name}
            {clusters !== undefined && !offered.has(name) && (
              <span className="muted"> (no longer backs up to {target})</span>
            )}
          </label>
        );
      })}
      {full && <p className="muted">At most {maxBackupSources} clusters.</p>}
      <ErrorBox message={error} />
    </fieldset>
  );
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
      <Field label="Target" error={problems.target}>
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
      <Field label="Schedule" hint={scheduleHint(value.schedule)} error={problems.schedule}>
        {(id) => (
          <input
            id={id}
            value={value.schedule}
            spellCheck={false}
            onChange={(e) => update({ schedule: e.target.value })}
          />
        )}
      </Field>
      <Field label="Time zone">
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
      <Field label="Days to keep" error={problems.retention}>
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
          value.volumes === "annotated" ? "Listed in a pod's backup.velero.io/backup-volumes annotation" : undefined
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
            <option value="all">All but emptyDir</option>
            <option value="annotated">Annotated only</option>
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
  return [{ label: "Backups", value: backupsText(b), group: "Maintenance", action: "backups", name: "backups" }];
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
      title="Edit backups"
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
          aria-describedby={none ? hint : undefined}
          onChange={(e) => setEnabled(e.target.checked)}
        />{" "}
        Back up with Velero
      </label>
      <Info about="backups">{backupsHint}</Info>
      {none && (
        <p className="hint" id={hint}>
          {noTargets}
        </p>
      )}
      {enabled && <BackupFieldset value={fields} onChange={setTyped} targets={targets} problems={problems} />}
      {enabled && filled(fields.target) && (
        <BackupSourcesField
          namespace={c.namespace}
          self={c.name}
          target={fields.target}
          value={fields.readFrom}
          creating={false}
          onChange={(readFrom) => setTyped({ ...fields, readFrom })}
        />
      )}
      {enabled && <BackupKey cluster={c} existingSecret={current.existingSecret} />}
      {current.enabled && !enabled && <p className="warning">Removes Velero. Backups stay in the target.</p>}
      <ErrorBox message={error} />
    </Dialog>
  );
}

// BackupKey is where Velero's access key comes from: a Secret of the owner's own in the cluster, or
// the one stored for it.
function BackupKey({
  cluster: c,
  existingSecret,
}: {
  cluster: Cluster;
  existingSecret: string | undefined;
}): JSX.Element {
  return filled(existingSecret) ? (
    <p className="hint">Access key: Secret {existingSecret} in velero</p>
  ) : (
    <CredentialField label="Access key" purpose="backup" cluster={c} keys={backupKeys} hint={backupCredentialHint} />
  );
}

const poolName = (pool: string): string => pool.replace(/^\/pool\//u, "");

/** readersOf are the clusters that read a cluster's backups, which it can be restored into too. */
function readersOf(c: Cluster, clusters: Cluster[] | null | undefined): string[] {
  return (clusters ?? [])
    .filter((o) => {
      const b = backupsOf(o.variables["backups"]);
      return o.namespace === c.namespace && b.enabled && (b.readFrom ?? []).includes(c.name);
    })
    .map((o) => o.name)
    .toSorted();
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
    { cache: backupsCache, key: `${c.namespace}/${c.name}` },
  );
  const fleet = useFleet();
  const [restoring, setRestoring] = useState<BackupRun>();
  const [deleting, setDeleting] = useState<BackupRun>();
  const [results, setResults] = useState<RestoreRun>();
  const [restoredInto, setRestoredInto] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string>();
  const settings = backupsOf(c.variables["backups"]);

  if (!v) {
    return (
      <div className="box">
        <ErrorBox message={error} />
        {!filled(error) && <Skeleton />}
      </div>
    );
  }
  if (!v.enabled) {
    return (
      <div className="box">
        <p>Backups are off</p>
        <div className="actions">
          <button type="button" disabled={!can} onClick={() => onEdit("backups")}>
            Set up backups…
          </button>
        </div>
      </div>
    );
  }

  const { noBackUp, noRestore, noDelete } = refusals(v, poolName(c.pool));
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
  const own = v.backups.filter((b) => b.from === undefined);
  const readers = readersOf(c, fleet?.clusters);

  return (
    <>
      <div className="box">
        {v.attention.map((a) => (
          <p key={a.what} className={a.severity === "error" ? "error" : "warning"}>
            {a.what}. {a.message}
          </p>
        ))}
        <BackupsSummary view={v} settings={settings} readers={readers} />
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
        {[noBackUp, noRestore, noBackUp === undefined ? noDelete : undefined]
          .filter((why) => filled(why))
          .map((why) => (
            <p key={why} className="muted">
              {why}
            </p>
          ))}
        {filled(restoredInto) && <RestoredNotice namespace={c.namespace} into={restoredInto} />}
        <ErrorBox message={actionError} />
        <ErrorBox message={error} />
      </div>

      <BackupsTable
        title="Backups"
        view={v}
        backups={own}
        noRestore={noRestore}
        noDelete={noDelete}
        onRestore={setRestoring}
        onDelete={setDeleting}
      />
      {v.sources.map((s) => (
        <BackupsTable
          key={s.cluster}
          title={`Backups of ${s.cluster}`}
          view={v}
          backups={v.backups.filter((b) => b.from === s.cluster)}
          noRestore={noRestore}
          onRestore={setRestoring}
        />
      ))}
      {v.restores.length > 0 && <RestoresTable restores={v.restores} onResults={setResults} />}

      {restoring !== undefined && (
        <RestoreDialog
          cluster={c}
          backup={restoring}
          volumeData={volumeDataOf(v, restoring)}
          readers={restoring.from === undefined ? readers : []}
          onClose={() => setRestoring(undefined)}
          onDone={(into) => {
            setRestoring(undefined);
            setRestoredInto(into === c.name ? undefined : into);
            void reload();
          }}
        />
      )}
      {deleting !== undefined && (
        <DeleteBackupDialog
          cluster={c}
          backup={deleting}
          onClose={() => setDeleting(undefined)}
          onDone={() => {
            setDeleting(undefined);
            void reload();
          }}
        />
      )}
      {results !== undefined && <ResultsDialog cluster={c} restore={results} onClose={() => setResults(undefined)} />}
    </>
  );
}

// refusals say what the user may not do with the backups, and the privilege it needs.
function refusals(
  v: ClusterBackups,
  pool: string,
): { noBackUp: string | undefined; noRestore: string | undefined; noDelete: string | undefined } {
  return {
    noBackUp: v.canBackUp ? undefined : `Backing up needs VM.Allocate on pool ${pool}.`,
    noRestore: v.canRestore ? undefined : `Restoring needs Sys.Modify on pool ${pool}.`,
    noDelete: v.canDelete ? undefined : `Deleting a backup needs VM.Allocate on pool ${pool}.`,
  };
}

// volumeDataOf tells whether the cluster's Velero reads a backup's volumes' data: its own always,
// another cluster's where the two share a repository key.
const volumeDataOf = (v: ClusterBackups, b: BackupRun): boolean =>
  b.from === undefined || v.sources.some((s) => s.cluster === b.from && s.volumeData);

// sourceState is how Velero's location for another cluster's backups stands.
function sourceState(s: BackupSource): "ok" | "error" | "progress" {
  if (s.phase === "Available") {
    return "ok";
  }
  return s.phase === "Unavailable" ? "error" : "progress";
}

// BackupsSummary says where a cluster's backups go, how they run, and whose others it reads.
function BackupsSummary({
  view: v,
  settings,
  readers,
}: {
  view: ClusterBackups;
  settings: BackupsValue;
  readers: string[];
}): JSX.Element {
  const location = v.location === "Available" ? "Reachable" : (v.location ?? "Not checked yet");
  return (
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
        {filled(v.locationMessage) && <Detail>{sentence(v.locationMessage)}</Detail>}
      </dd>
      <dt>Last scheduled backup</dt>
      <dd>{v.lastBackup === undefined ? "None yet" : `${when(v.lastBackup)} (${age(v.lastBackup)} ago)`}</dd>
      {v.sources.length > 0 && (
        <>
          <dt>Reads the backups of</dt>
          <dd>
            {v.sources.map((s) => (
              <div key={s.cluster}>
                <Status state={sourceState(s)} text={s.cluster} />
                <span className="muted">
                  {" "}
                  {s.volumeData ? "with their volumes' data" : "their resources only, not their volumes' data"}
                </span>
                {filled(s.message) && <div className="hint message">{sentence(s.message)}</div>}
              </div>
            ))}
          </dd>
        </>
      )}
      {readers.length > 0 && (
        <>
          <dt>Read by</dt>
          <dd>{readers.join(", ")}</dd>
        </>
      )}
    </dl>
  );
}

// BackupsTable lists backups: the cluster's own, which may be deleted, or another cluster's.
function BackupsTable({
  title,
  view: v,
  backups,
  noRestore,
  noDelete,
  onRestore,
  onDelete,
}: {
  title: string;
  view: ClusterBackups;
  backups: BackupRun[];
  noRestore: string | undefined;
  noDelete?: string | undefined;
  onRestore: (backup: BackupRun) => void;
  /** Unset for another cluster's backups, which are deleted where they are made. */
  onDelete?: (backup: BackupRun) => void;
}): JSX.Element {
  return (
    <div className="box scroll">
      <h3>{title}</h3>
      <table className="grid">
        <thead>
          <tr>
            <th>Name</th>
            <th>Started</th>
            <th>Status</th>
            <th>Deleted on</th>
            <th>
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {backups.length === 0 && (
            <tr>
              <td colSpan={5} className="muted">
                {v.reachable ? "No backups yet" : "Waiting for Velero"}
              </td>
            </tr>
          )}
          {backups.map((b) => {
            const s = b.deleting === true ? phaseStatus("Deleting") : phaseStatus(b.phase);
            const said = outcome(b);
            const mayDelete = v.canDelete && deletable(b);
            return (
              <tr key={b.name}>
                <td>{b.name}</td>
                <td className="started">
                  <span title={when(b.started)}>{ago(b.started)}</span>
                  {(b.scheduled || filled(b.by)) && (
                    <div className="hint message">{b.scheduled ? "By the schedule" : `By ${b.by}`}</div>
                  )}
                </td>
                <td className="state">
                  <Status state={s.state} text={s.text} />
                  {b.total > 0 && (
                    <div className="hint">
                      {b.items} of {b.total} items
                    </div>
                  )}
                  {said && <div className="hint message">{said}</div>}
                  {(b.deleteErrors ?? []).map((m) => (
                    <div key={m} className="hint message">
                      {sentence(`Not deleted: ${m}`)}
                    </div>
                  ))}
                </td>
                <td className="nowrap">{b.expires === undefined ? "" : new Date(b.expires).toLocaleDateString()}</td>
                <td className="row-action">
                  <button
                    type="button"
                    disabled={!v.canRestore || !restorable(b) || b.deleting === true}
                    title={noRestore ?? (restorable(b) ? undefined : "Backup not finished")}
                    aria-label={`Restore ${b.name}`}
                    onClick={() => onRestore(b)}
                  >
                    Restore…
                  </button>
                  {onDelete !== undefined && (
                    <>
                      {" "}
                      <button
                        type="button"
                        className="danger"
                        disabled={!mayDelete}
                        title={noDelete ?? (deletable(b) ? undefined : "Backup not finished")}
                        aria-label={`Delete ${b.name}`}
                        onClick={() => onDelete(b)}
                      >
                        Delete…
                      </button>
                    </>
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

// DeleteBackupDialog has Velero delete one of the cluster's backups, from the target too, once its
// name is typed: the server asks the same of everyone.
function DeleteBackupDialog({
  cluster: c,
  backup: b,
  onClose,
  onDone,
}: {
  cluster: Cluster;
  backup: BackupRun;
  onClose: () => void;
  onDone: () => void;
}): JSX.Element {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const confirmed = typed === b.name;
  const remove = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.deleteBackup(c.namespace, c.name, { backup: b.name, confirm: typed });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={`Delete ${b.name}`}
      onClose={onClose}
      onSubmit={() => {
        if (confirmed) {
          void remove();
        }
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={busy || !confirmed}>
            Delete backup
          </button>
        </>
      }
    >
      <p>Deletes the backup and its volume data from the target. It cannot be restored afterwards.</p>
      <Field label="Confirm name" hint={`Type ${b.name}.`}>
        {(id) => (
          <input
            id={id}
            value={typed}
            spellCheck={false}
            autoComplete="off"
            onChange={(e) => setTyped(e.target.value)}
          />
        )}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
