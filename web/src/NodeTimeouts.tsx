import type { JSX } from "react";

import type { NodeTimeouts } from "./types";
import { Field } from "./ui";

const secondsPerMinute = 60;
// The server's limit: a day.
const maxMinutes = 1440;

/** A form's timeouts as typed: minutes for the drain and the volumes, seconds for the node's removal. Empty is the class's. */
export interface TimeoutEntry {
  drain: string;
  detach: string;
  deletion: string;
}

export const noTimeouts: TimeoutEntry = { drain: "", detach: "", deletion: "" };

const minutesOf = (s: number | undefined): string => (s === undefined ? "" : String(s / secondsPerMinute));

/** entryOf is timeouts as a form shows them. */
export const entryOf = (t: NodeTimeouts): TimeoutEntry => ({
  drain: minutesOf(t.nodeDrainTimeoutSeconds),
  detach: minutesOf(t.nodeVolumeDetachTimeoutSeconds),
  deletion: t.nodeDeletionTimeoutSeconds === undefined ? "" : String(t.nodeDeletionTimeoutSeconds),
});

// Empty is the class's. Anything else must be a number in range, or undefined says it is not.
function seconds(text: string, perUnit: number, most: number): number | null | undefined {
  if (text.trim() === "") {
    return null;
  }
  const n = Number(text);
  if (!Number.isFinite(n) || n < 0 || n > most) {
    return undefined;
  }
  return Math.round(n * perUnit);
}

/** timeoutProblems says which timeouts are not numbers in range. */
export function timeoutProblems(e: TimeoutEntry): Partial<Record<keyof TimeoutEntry, string>> {
  const out: Partial<Record<keyof TimeoutEntry, string>> = {};
  if (seconds(e.drain, secondsPerMinute, maxMinutes) === undefined) {
    out.drain = `0 to ${maxMinutes} minutes`;
  }
  if (seconds(e.detach, secondsPerMinute, maxMinutes) === undefined) {
    out.detach = `0 to ${maxMinutes} minutes`;
  }
  if (seconds(e.deletion, 1, maxMinutes * secondsPerMinute) === undefined) {
    out.deletion = `0 to ${maxMinutes * secondsPerMinute} seconds`;
  }
  return out;
}

/** timeoutsOf is what a form's timeouts ask for. Call it only when timeoutProblems finds none. */
export function timeoutsOf(e: TimeoutEntry): NodeTimeouts {
  const drain = seconds(e.drain, secondsPerMinute, maxMinutes);
  const detach = seconds(e.detach, secondsPerMinute, maxMinutes);
  const deletion = seconds(e.deletion, 1, maxMinutes * secondsPerMinute);
  return {
    ...(typeof drain === "number" && { nodeDrainTimeoutSeconds: drain }),
    ...(typeof detach === "number" && { nodeVolumeDetachTimeoutSeconds: detach }),
    ...(typeof deletion === "number" && { nodeDeletionTimeoutSeconds: deletion }),
  };
}

/** timeoutsSet tells whether any timeout is set. */
export const timeoutsSet = (t: NodeTimeouts): boolean =>
  t.nodeDrainTimeoutSeconds !== undefined ||
  t.nodeVolumeDetachTimeoutSeconds !== undefined ||
  t.nodeDeletionTimeoutSeconds !== undefined;

const classDefault = "class default";

interface Part {
  label: string;
  key: keyof NodeTimeouts;
  minutes: boolean;
}

const parts: Part[] = [
  { label: "Drain", key: "nodeDrainTimeoutSeconds", minutes: true },
  { label: "volume detach", key: "nodeVolumeDetachTimeoutSeconds", minutes: true },
  { label: "node removal", key: "nodeDeletionTimeoutSeconds", minutes: false },
];

function partText({ label, key, minutes }: Part, own: NodeTimeouts, fallback: NodeTimeouts): string {
  const v = own[key] ?? fallback[key];
  if (v === undefined) {
    return `${label} ${classDefault}`;
  }
  let value = `${v} s`;
  if (v === 0) {
    value = "no limit";
  } else if (minutes) {
    value = `${v / secondsPerMinute} min`;
  }
  return own[key] === undefined ? `${label} ${value} (${classDefault})` : `${label} ${value}`;
}

/**
 * timeoutsText is the timeouts a machine's node gets, as they apply: the cluster's own, and for
 * those it leaves out the class's, marked as such.
 */
export function timeoutsText(own: NodeTimeouts, fallback: NodeTimeouts = {}): string {
  return parts.map((p) => partText(p, own, fallback)).join(", ");
}

const classHint = (classes: number | undefined, unit: number): string =>
  classes === undefined
    ? "Empty takes the class's, and 0 sets no limit"
    : `Empty takes ${classes / unit}, and 0 sets no limit`;

/**
 * NodeTimeoutFields are the waits a removed machine's node is given: for its pods to drain, for its
 * volumes to detach, and for its Node to be removed. The class's, where set, show as the hint.
 */
export function NodeTimeoutFields({
  value,
  onChange,
  fallback,
}: {
  value: TimeoutEntry;
  onChange: (e: TimeoutEntry) => void;
  /** The class's timeouts, which an empty field takes. */
  fallback: NodeTimeouts | undefined;
}): JSX.Element {
  const problems = timeoutProblems(value);
  return (
    <>
      <Field
        label="Drain, minutes"
        error={problems.drain}
        hint={classHint(fallback?.nodeDrainTimeoutSeconds, secondsPerMinute)}
      >
        {(id) => (
          <input
            id={id}
            inputMode="decimal"
            value={value.drain}
            onChange={(e) => onChange({ ...value, drain: e.target.value })}
          />
        )}
      </Field>
      <Field
        label="Volume detach, minutes"
        error={problems.detach}
        hint={classHint(fallback?.nodeVolumeDetachTimeoutSeconds, secondsPerMinute)}
      >
        {(id) => (
          <input
            id={id}
            inputMode="decimal"
            value={value.detach}
            onChange={(e) => onChange({ ...value, detach: e.target.value })}
          />
        )}
      </Field>
      <Field
        label="Node removal, seconds"
        error={problems.deletion}
        hint={classHint(fallback?.nodeDeletionTimeoutSeconds, 1)}
      >
        {(id) => (
          <input
            id={id}
            inputMode="decimal"
            value={value.deletion}
            onChange={(e) => onChange({ ...value, deletion: e.target.value })}
          />
        )}
      </Field>
    </>
  );
}
