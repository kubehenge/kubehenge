import { useId, useState, type JSX } from "react";

import {
  labelProblems,
  maxLabels,
  maxTaints,
  taintEffects,
  taintProblems,
  type NodeLabel,
  type NodeSettings,
  type NodeTaint,
  type RowProblem,
} from "./node-settings";

type Kind = "label" | "taint";
type Part = keyof RowProblem;

// The fields of a row carry their names as labels, and a heading line above the rows shows them.
export function Head({ parts, shape }: { parts: string[]; shape?: string }): JSX.Element {
  return (
    <div className={`node-row node-head ${shape ?? (parts.length > 2 ? "taint-row" : "")}`} aria-hidden="true">
      {parts.map((p) => (
        <span key={p}>{p}</span>
      ))}
      <span />
    </div>
  );
}

// TextPart is a key or value. Its problem shows once it was touched.
function TextPart({
  label,
  value,
  error,
  errorId,
  onChange,
  onTouch,
}: {
  label: string;
  value: string;
  error: string | undefined;
  errorId: string;
  onChange: (v: string) => void;
  onTouch: () => void;
}): JSX.Element {
  return (
    <Field label={label}>
      <input
        aria-label={label}
        aria-invalid={error === undefined ? undefined : true}
        aria-describedby={error === undefined ? undefined : errorId}
        value={value}
        autoComplete="off"
        onChange={(e) => {
          onTouch();
          onChange(e.target.value.trim());
        }}
        onBlur={onTouch}
      />
    </Field>
  );
}

// Field holds a control with its name. The name shows only where the rows stack and the heading line
// above them is gone.
export function Field({ label, children }: { label: string; children: JSX.Element }): JSX.Element {
  return (
    <div className="node-field">
      <span className="node-field-name" aria-hidden="true">
        {label}
      </span>
      {children}
    </div>
  );
}

/**
 * NodeSettingsFields edits the labels and taints of a set of nodes. A row's problem shows once the
 * row's field was touched, or all of them with showAll.
 */
export function NodeSettingsFields({
  value,
  onChange,
  showAll = false,
  workers = false,
}: {
  value: NodeSettings;
  onChange: (v: NodeSettings) => void;
  showAll?: boolean;
  /** The settings are the cluster's workers' as a whole, which the legends say. */
  workers?: boolean;
}): JSX.Element {
  const base = useId();
  const [touched, setTouched] = useState<ReadonlySet<string>>(new Set());
  const touch = (kind: Kind, i: number, part: Part): void => setTouched((s) => new Set([...s, `${kind}.${i}.${part}`]));
  const problems = {
    label: labelProblems(value.labels),
    taint: taintProblems(value.taints),
  };
  const error = (kind: Kind, i: number, part: Part): string | undefined =>
    showAll || touched.has(`${kind}.${i}.${part}`) ? problems[kind][i]?.[part] : undefined;
  // One message a row, the first of its fields' that shows.
  const rowError = (kind: Kind, i: number): { id: string; message: string } | undefined => {
    const message = error(kind, i, "key") ?? error(kind, i, "value") ?? error(kind, i, "effect");
    return message === undefined ? undefined : { id: `${base}-${kind}-${i}`, message };
  };
  const setLabel = (i: number, patch: Partial<NodeLabel>): void =>
    onChange({
      ...value,
      labels: value.labels.map((l, j) => (j === i ? { ...l, ...patch } : l)),
    });
  const setTaint = (i: number, patch: Partial<NodeTaint>): void =>
    onChange({
      ...value,
      taints: value.taints.map((t, j) => (j === i ? { ...t, ...patch } : t)),
    });
  // Rows are edited in place and have no identity, so removing one clears what was touched.
  const removeLabel = (i: number): void => {
    setTouched(new Set());
    onChange({ ...value, labels: value.labels.filter((_, j) => j !== i) });
  };
  const removeTaint = (i: number): void => {
    setTouched(new Set());
    onChange({ ...value, taints: value.taints.filter((_, j) => j !== i) });
  };
  return (
    <>
      <fieldset className="group node-settings">
        <legend>{workers ? "Worker labels" : "Labels"}</legend>
        <div className="node-rows">
          {value.labels.length > 0 && <Head parts={["Key", "Value"]} />}
          {value.labels.map((l, i) => {
            const failed = rowError("label", i);
            return (
              // oxlint-disable-next-line react/no-array-index-key -- rows are edited in place
              <fieldset key={i} aria-label={`Label ${i + 1}`} className="bare node-row">
                <TextPart
                  label="Key"
                  value={l.key}
                  error={error("label", i, "key")}
                  errorId={`${base}-label-${i}`}
                  onChange={(key) => setLabel(i, { key })}
                  onTouch={() => touch("label", i, "key")}
                />
                <TextPart
                  label="Value"
                  value={l.value}
                  error={error("label", i, "value")}
                  errorId={`${base}-label-${i}`}
                  onChange={(v) => setLabel(i, { value: v })}
                  onTouch={() => touch("label", i, "value")}
                />
                <button type="button" aria-label={`Remove label ${i + 1}`} onClick={() => removeLabel(i)}>
                  Remove
                </button>
                {failed !== undefined && (
                  <div className="field-error" id={failed.id}>
                    {failed.message}
                  </div>
                )}
              </fieldset>
            );
          })}
        </div>
        {value.labels.length === 0 && <p className="muted">No labels</p>}
        <button
          type="button"
          disabled={value.labels.length >= maxLabels}
          onClick={() =>
            onChange({
              ...value,
              labels: [...value.labels, { key: "", value: "" }],
            })
          }
        >
          Add label
        </button>
      </fieldset>
      <fieldset className="group node-settings">
        <legend>{workers ? "Worker taints" : "Taints"}</legend>
        <div className="node-rows">
          {value.taints.length > 0 && <Head parts={["Key", "Value", "Effect"]} />}
          {value.taints.map((t, i) => {
            const failed = rowError("taint", i);
            return (
              // oxlint-disable-next-line react/no-array-index-key -- rows are edited in place
              <fieldset key={i} aria-label={`Taint ${i + 1}`} className="bare node-row taint-row">
                <TextPart
                  label="Key"
                  value={t.key}
                  error={error("taint", i, "key")}
                  errorId={`${base}-taint-${i}`}
                  onChange={(key) => setTaint(i, { key })}
                  onTouch={() => touch("taint", i, "key")}
                />
                <TextPart
                  label="Value"
                  value={t.value}
                  error={error("taint", i, "value")}
                  errorId={`${base}-taint-${i}`}
                  onChange={(v) => setTaint(i, { value: v })}
                  onTouch={() => touch("taint", i, "value")}
                />
                <Field label="Effect">
                  <select
                    aria-label="Effect"
                    value={t.effect}
                    onChange={(e) => {
                      touch("taint", i, "effect");
                      setTaint(i, { effect: e.target.value });
                    }}
                  >
                    {taintEffects.map((effect) => (
                      <option key={effect} value={effect}>
                        {effect}
                      </option>
                    ))}
                  </select>
                </Field>
                <button type="button" aria-label={`Remove taint ${i + 1}`} onClick={() => removeTaint(i)}>
                  Remove
                </button>
                {failed !== undefined && (
                  <div className="field-error" id={failed.id}>
                    {failed.message}
                  </div>
                )}
              </fieldset>
            );
          })}
        </div>
        {value.taints.length === 0 && <p className="muted">No taints</p>}
        <button
          type="button"
          disabled={value.taints.length >= maxTaints}
          onClick={() =>
            onChange({
              ...value,
              taints: [...value.taints, { key: "", value: "", effect: "NoSchedule" }],
            })
          }
        >
          Add taint
        </button>
      </fieldset>
    </>
  );
}
