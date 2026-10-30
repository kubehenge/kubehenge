import { useId, useState, type JSX } from "react";

import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, Info, useChange } from "./ui";
import { flag, quantity, record, text } from "./values";

/** How a cluster's logs are collected, by its logs variable's collector. */
export type Collector = "node" | "api";

/** A cluster's logs variable. */
export interface Logs {
  enabled: boolean;
  collector: Collector;
  retentionDays: number;
  sizeGiB: number;
}

const defaultRetentionDays = 7;
const defaultSizeGiB = 10;
const maxRetentionDays = 366;
const maxSizeGiB = 1024;

/** newLogs is the logs variable's default: off, collected on the nodes, a week in 10 GiB. */
export const newLogs = (): Logs => ({
  enabled: false,
  collector: "node",
  retentionDays: defaultRetentionDays,
  sizeGiB: defaultSizeGiB,
});

export function logsOf(v: unknown): Logs {
  const r = record(v);
  return {
    enabled: flag(r["enabled"]) === true,
    collector: text(r["collector"]) === "api" ? "api" : "node",
    retentionDays: quantity(r["retentionDays"]) ?? defaultRetentionDays,
    sizeGiB: quantity(r["sizeGiB"]) ?? defaultSizeGiB,
  };
}

/** logsText says in a line whether a cluster keeps logs, how it collects them and for how long. */
export function logsText(l: Logs): string {
  if (!l.enabled) {
    return "Off";
  }
  const days = l.retentionDays === 1 ? "1 day" : `${l.retentionDays} days`;
  return `On, ${l.collector === "node" ? "every node" : "Kubernetes API"}, ${days}, ${l.sizeGiB} GiB`;
}

/** What keeping logs gives, short enough for a hint. */
export const logsHint = "Logs and events, searchable in Grafana.";

export const logsNeedMonitoring = "Needs monitoring on.";

// The node collector's privileges are named because choosing it grants them.
const collectors: { value: Collector; label: string; info: string }[] = [
  {
    value: "node",
    label: "On every node (recommended)",
    info: "Also collects node journals and the audit log. Reads host log files as root, in a privileged namespace.",
  },
  {
    value: "api",
    label: "Through the Kubernetes API",
    info: "No host access and less memory. Misses journals, the audit log and pods that live only seconds.",
  },
];

// sizeProblem and daysProblem say what is wrong with a typed figure, if anything; the volume only grows.
function sizeProblem(size: number, least: number): string | undefined {
  if (!Number.isInteger(size) || size < 1 || size > maxSizeGiB) {
    return `A whole number from 1 to ${maxSizeGiB}`;
  }
  return size < least ? `The volume can't shrink. Enter ${least} or more.` : undefined;
}

function daysProblem(days: number): string | undefined {
  return Number.isInteger(days) && days >= 1 && days <= maxRetentionDays
    ? undefined
    : `A whole number from 1 to ${maxRetentionDays}`;
}

/** logsProblem says what is wrong with a logs choice, if anything; least is the volume it had. */
export const logsProblem = (l: Logs, least = 1): string | undefined =>
  l.enabled ? (daysProblem(l.retentionDays) ?? sizeProblem(l.sizeGiB, least)) : undefined;

/** LogsFields chooses how logs are collected and how long and in how much they are kept. */
export function LogsFields({
  value,
  least = 1,
  onChange,
}: {
  value: Logs;
  least?: number;
  onChange: (l: Logs) => void;
}): JSX.Element {
  const name = useId();
  return (
    <>
      <fieldset className="choices">
        <legend>Collector</legend>
        {collectors.map((c) => (
          <div key={c.value}>
            <label className="check">
              <input
                type="radio"
                name={name}
                value={c.value}
                checked={value.collector === c.value}
                onChange={() => onChange({ ...value, collector: c.value })}
              />{" "}
              {c.label}
            </label>
            <Info about={c.label}>{c.info}</Info>
          </div>
        ))}
      </fieldset>
      <Field label="Keep for (days)" error={daysProblem(value.retentionDays)}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={1}
            max={maxRetentionDays}
            value={Number.isNaN(value.retentionDays) ? "" : value.retentionDays}
            onChange={(e) => onChange({ ...value, retentionDays: e.target.valueAsNumber })}
          />
        )}
      </Field>
      <Field
        label="Volume (GiB)"
        hint="Must fit every kept day, and can only grow"
        error={sizeProblem(value.sizeGiB, least)}
      >
        {(id) => (
          <input
            id={id}
            type="number"
            min={least}
            max={maxSizeGiB}
            value={Number.isNaN(value.sizeGiB) ? "" : value.sizeGiB}
            onChange={(e) => onChange({ ...value, sizeGiB: e.target.valueAsNumber })}
          />
        )}
      </Field>
    </>
  );
}

/** logsVariable is what a logs choice sets, with whatever else the variable holds kept. */
export const logsVariable = (l: Logs, held: Record<string, unknown> = {}): Record<string, unknown> =>
  l.enabled
    ? { ...held, enabled: true, collector: l.collector, retentionDays: l.retentionDays, sizeGiB: l.sizeGiB }
    : { ...held, enabled: false };

export function LogsDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const held = record(c.variables["logs"]);
  const current = logsOf(held);
  const monitoring = flag(record(c.variables["monitoring"])["enabled"]) === true;
  // The volume only grows once there is one; a cluster that never had logs starts from nothing.
  const least = "sizeGiB" in held ? current.sizeGiB : 1;
  const [next, setNext] = useState(current);
  const { busy, error, send } = useChange(c, onClose);
  const hint = useId();
  const problem = logsProblem(next, least);
  const changed = JSON.stringify(logsVariable(next)) !== JSON.stringify(logsVariable(current));
  return (
    <Dialog
      title="Edit logs"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { logs: logsVariable(next, held) } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || problem !== undefined}>
            Save
          </button>
        </>
      }
    >
      <label className="check">
        <input
          type="checkbox"
          checked={next.enabled}
          disabled={!monitoring && !next.enabled}
          aria-describedby={monitoring ? undefined : hint}
          onChange={(e) => setNext({ ...next, enabled: e.target.checked })}
        />{" "}
        Keep logs
      </label>
      <Info about="Keep logs">{logsHint} Switching off keeps the volume.</Info>
      {!monitoring && (
        <p className="hint" id={hint}>
          {logsNeedMonitoring}
        </p>
      )}
      {next.enabled && <LogsFields value={next} least={least} onChange={setNext} />}
      <ErrorBox message={error} />
    </Dialog>
  );
}
