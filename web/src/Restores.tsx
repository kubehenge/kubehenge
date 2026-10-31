import { useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import {
  ago,
  finished,
  newRestoreFields,
  outcome,
  phaseStatus,
  restoreProblems,
  restoreProgress,
  restoreRequest,
  restoreScope,
  runningPhases,
  when,
  type RestoreField,
  type RestoreFields,
} from "./backup-runs";
import { clusterPath } from "./cluster-paths";
import { Link } from "./router";
import type { BackupRun, Cluster, ClusterBackups, RestoreMessages, RestoreResults, RestoreRun } from "./types";
import { Dialog, ErrorBox, Field, Status, errorMessage, filled, sentence } from "./ui";

/** RestoredNotice says where a restore into another cluster can be followed. */
export function RestoredNotice({ namespace, into }: { namespace: string; into: string }): JSX.Element {
  return (
    <output className="muted notice">
      The restore runs in {into}, whose Backups view follows it.{" "}
      <Link to={clusterPath(namespace, into, "backups")}>Open it</Link>
    </output>
  );
}

/** RestoresTable lists a cluster's restores: what each brings back, how far it is and what it left. */
export function RestoresTable({
  restores,
  onResults,
}: {
  restores: RestoreRun[];
  onResults: (r: RestoreRun) => void;
}): JSX.Element {
  return (
    <div className="box scroll">
      <h3>Restores</h3>
      <table className="grid">
        <thead>
          <tr>
            <th>Backup</th>
            <th>What</th>
            <th>Started</th>
            <th>Status</th>
            <th>
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {restores.map((r) => {
            const s = phaseStatus(r.phase);
            const said = outcome(r);
            const refused = r.refused ?? [];
            const progress = restoreProgress(r);
            const hasResults = finished(r.phase) && r.phase !== "FailedValidation";
            return (
              <tr key={r.name}>
                <td>
                  {r.backup}
                  {filled(r.from) && <div className="hint">Of {r.from}</div>}
                </td>
                <td>{restoreScope(r)}</td>
                <td className="started">
                  <span title={when(r.started)}>{ago(r.started)}</span>
                  {filled(r.by) && <div className="hint message">By {r.by}</div>}
                </td>
                <td className="state">
                  <Status state={s.state} text={s.text} />
                  {progress.map((p) => (
                    <div key={p} className="hint">
                      {p}
                    </div>
                  ))}
                  {r.bytesTotal > 0 && runningPhases.has(r.phase) && (
                    <div>
                      <progress value={r.bytesDone} max={r.bytesTotal} aria-label={`Restore of ${r.backup}`} />
                    </div>
                  )}
                  {said && <div className="hint message">{said}</div>}
                  {refused.map((m) => (
                    <div key={m} className="hint message">
                      {sentence(m)}
                    </div>
                  ))}
                </td>
                <td className="row-action">
                  <button
                    type="button"
                    disabled={!hasResults}
                    title={hasResults ? undefined : "Restore not finished"}
                    aria-label={`Results of ${r.name}`}
                    onClick={() => onResults(r)}
                  >
                    Results…
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

// MessagesTable lists a restore's warnings or errors by where they arose.
function MessagesTable({ title, messages: m }: { title: string; messages: RestoreMessages }): JSX.Element {
  const rows: { where: string; message: string; key: string }[] = [];
  const add = (where: string, messages: string[]): void => {
    for (const message of messages) {
      // Velero repeats a message for each item it concerns, so its place in the list tells them apart.
      rows.push({ where, message, key: String(rows.length) });
    }
  };
  add("Velero", m.velero);
  add("Cluster-wide", m.cluster);
  for (const n of m.namespaces) {
    add(n.namespace, n.messages);
  }
  return (
    <section className="results">
      <h3>{title}</h3>
      {rows.length === 0 ? (
        <p className="muted">None.</p>
      ) : (
        <table className="grid">
          <thead>
            <tr>
              <th>Where</th>
              <th>Message</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.key}>
                <td className="message">{r.where}</td>
                <td className="message">{r.message}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {(m.more ?? 0) > 0 && <p className="muted">{m.more} more are not shown.</p>}
    </section>
  );
}

/** ResultsDialog shows what a finished restore left: its errors and warnings, by namespace. */
export function ResultsDialog({
  cluster: c,
  restore,
  onClose,
}: {
  cluster: Cluster;
  restore: RestoreRun;
  onClose: () => void;
}): JSX.Element {
  const [results, setResults] = useState<RestoreResults>();
  const [error, setError] = useState<string>();
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const read = await api.restoreResults(c.namespace, c.name, restore.name);
        if (current) {
          setResults(read);
        }
      } catch (err) {
        if (current) {
          setError(errorMessage(err));
        }
      }
    }
    void load();
    return () => {
      current = false;
    };
  }, [c.namespace, c.name, restore.name]);
  return (
    <Dialog
      title={`Results of ${restore.name}`}
      onClose={onClose}
      actions={
        <button type="button" onClick={onClose}>
          Close
        </button>
      }
    >
      <p className="hint">{sentence(`${restoreScope(restore)}, from ${restore.backup}`)}</p>
      {results === undefined && !filled(error) && <p className="muted">Reading the results from the target…</p>}
      {results !== undefined && (
        <>
          <MessagesTable title="Errors" messages={results.errors} />
          <MessagesTable title="Warnings" messages={results.warnings} />
        </>
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}

/** A cluster a backup can be restored into: the cluster itself, or one that reads its backups. */
export interface RestoreTarget {
  name: string;
  /** Whether the volumes' data can be restored there; undefined until known. */
  volumeData?: boolean;
  /** Why it cannot take the backup yet, if it cannot. */
  waiting?: string;
}

// useReader reads whether another cluster that reads this one's backups has listed the backup yet,
// and whether its Velero reads their volumes' data; undefined until read.
function useReader(c: Cluster, backup: string, into: string): RestoreTarget | undefined {
  const [read, setRead] = useState<RestoreTarget>();
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      if (into === c.name) {
        return;
      }
      let t: RestoreTarget;
      try {
        const v: ClusterBackups = await api.backups(c.namespace, into);
        const source = v.sources.find((s) => s.cluster === c.name);
        const listed = v.backups.some((b) => b.name === backup && b.from === c.name);
        t = { name: into, volumeData: source?.volumeData === true };
        if (!listed) {
          t.waiting = `Velero in ${into} has not listed this backup yet. It reads ${c.name}'s backups every minute.`;
        }
      } catch (err) {
        t = { name: into, waiting: errorMessage(err) };
      }
      if (current) {
        setRead(t);
      }
    }
    void load();
    return () => {
      current = false;
    };
  }, [c.namespace, c.name, backup, into]);
  // A cluster chosen before is read until the one chosen now answers.
  return read?.name === into ? read : undefined;
}

// dataText says what restoring, or not restoring, the volumes' data does.
function dataText(target: RestoreTarget | undefined, data: boolean): string {
  const empty = "Pods restart on empty volumes.";
  if (target !== undefined && target.volumeData !== true) {
    return `${target.name} cannot read this backup's volume data. ${empty}`;
  }
  return data ? "Pods wait for their volume data." : empty;
}

// NamesField is a field of names typed with commas or spaces between them.
function NamesField({
  label,
  hint,
  error,
  value,
  onChange,
}: {
  label: string;
  hint: string;
  error: string | undefined;
  value: string;
  onChange: (typed: string) => void;
}): JSX.Element {
  return (
    <Field label={label} hint={hint} error={error}>
      {(id) => <input id={id} value={value} spellCheck={false} onChange={(e) => onChange(e.target.value)} />}
    </Field>
  );
}

/** RestoreDialog restores a backup, whole or in part, into the cluster or one that reads its backups. */
export function RestoreDialog({
  cluster: c,
  backup,
  volumeData,
  readers,
  onClose,
  onDone,
}: {
  cluster: Cluster;
  backup: BackupRun;
  /** Whether the cluster's Velero reads the backup's volumes' data. */
  volumeData: boolean;
  /** The clusters that read this cluster's backups, which it can be restored into too. */
  readers: string[];
  onClose: () => void;
  onDone: (into: string) => void;
}): JSX.Element {
  const [into, setInto] = useState(c.name);
  const [typed, setTyped] = useState<RestoreFields>(newRestoreFields);
  const [edited, setEdited] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const dataHint = useId();
  const reader = useReader(c, backup.name, into);
  const target = into === c.name ? { name: c.name, volumeData } : reader;
  const readable = target?.volumeData === true;
  const fields = { ...typed, volumeData: readable && typed.volumeData };
  const problems = restoreProblems(fields);
  const update = (patch: Partial<RestoreFields>): void => {
    setTyped({ ...typed, ...patch });
    setEdited(true);
  };

  const restore = async (): Promise<void> => {
    if (Object.keys(problems).length > 0) {
      setEdited(true);
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.restore(c.namespace, into, restoreRequest(backup.name, fields));
      onDone(into);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  const shown = (f: RestoreField): string | undefined => (edited ? problems[f] : undefined);
  return (
    <Dialog
      title={`Restore ${backup.name}`}
      onClose={onClose}
      onSubmit={() => {
        void restore();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || target === undefined || filled(target.waiting)}>
            Restore
          </button>
        </>
      }
    >
      <p>
        {filled(backup.from) && `Backup of ${backup.from}. `}
        Skips resources that exist. Delete a namespace first to restore it fully.
      </p>
      {readers.length > 0 && (
        <Field label="Into" hint="Writes everything the backup holds, Secrets included">
          {(id) => (
            <select id={id} value={into} onChange={(e) => setInto(e.target.value)}>
              <option value={c.name}>{c.name}, this cluster</option>
              {readers.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          )}
        </Field>
      )}
      {target === undefined && <p className="muted">Reading the backups of {into}…</p>}
      {filled(target?.waiting) && <p className="warning">{target.waiting}</p>}
      <fieldset className="group">
        <legend>Namespaces</legend>
        <label className="check">
          <input
            type="radio"
            name="restore-scope"
            checked={fields.whole}
            onChange={() => setTyped({ ...typed, whole: true })}
          />{" "}
          All namespaces
        </label>
        <label className="check">
          <input
            type="radio"
            name="restore-scope"
            checked={!fields.whole}
            onChange={() => setTyped({ ...typed, whole: false })}
          />{" "}
          Chosen namespaces
        </label>
        {fields.whole ? (
          <NamesField
            label="Leave out"
            hint="Optional, separate with commas or spaces"
            error={shown("excludedNamespaces")}
            value={fields.excludedNamespaces}
            onChange={(excludedNamespaces) => update({ excludedNamespaces })}
          />
        ) : (
          <NamesField
            label="Namespaces"
            hint="Separate with commas or spaces"
            error={shown("namespaces")}
            value={fields.namespaces}
            onChange={(namespaces) => update({ namespaces })}
          />
        )}
      </fieldset>
      <details className="advanced">
        <summary>Kinds of resources</summary>
        <NamesField
          label="Only these kinds"
          hint="Plural names like deployments, empty for every kind"
          error={shown("resources")}
          value={fields.resources}
          onChange={(resources) => update({ resources })}
        />
        <NamesField
          label="Leave out these kinds"
          hint="Optional."
          error={shown("excludedResources")}
          value={fields.excludedResources}
          onChange={(excludedResources) => update({ excludedResources })}
        />
      </details>
      <label className="check">
        <input
          type="checkbox"
          checked={fields.volumeData}
          disabled={!readable}
          aria-describedby={dataHint}
          onChange={(e) => update({ volumeData: e.target.checked })}
        />{" "}
        Restore the volumes&apos; data
      </label>
      <p className="hint" id={dataHint}>
        {dataText(target, fields.volumeData)}
      </p>
      <ErrorBox message={error} />
    </Dialog>
  );
}
