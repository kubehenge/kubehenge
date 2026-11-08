import { useState, type JSX } from "react";

import { CredentialField, type CredentialKey } from "./CredentialField";
import {
  maxOutputName,
  maxOutputs,
  newOutput,
  outputAuths,
  outputKinds,
  outputProblems,
  outputPurpose,
  outputStreams,
  type LogOutput,
  type OutputKind,
  type OutputStream,
} from "./output-model";
import type { ClusterRef } from "./types";
import { Field } from "./ui";
import { oneOf } from "./values";

const kindLabels: Record<OutputKind, string> = {
  syslog: "Syslog",
  otlp: "OTLP over HTTP",
  loki: "Loki",
};

const endpointHints: Record<OutputKind, string> = {
  syslog: "tls://host:6514, tcp:// or udp://",
  otlp: "The receiver's base URL",
  loki: "The whole push URL",
};

const streamLabels: Record<OutputStream, string> = {
  containers: "Containers",
  events: "Events",
  nodes: "Nodes",
  audit: "Audit",
};

const authLabels = { none: "None", basic: "User name and password", bearer: "Token" } as const;

const basicKeys: CredentialKey[] = [
  { key: "username", label: "User name", plain: true },
  { key: "password", label: "Password" },
];
const bearerKeys: CredentialKey[] = [{ key: "token", label: "Token" }];

export const outputsNeedCredential = "Store its credential in Options once the cluster exists.";

function OutputFields({
  output: o,
  index,
  error,
  cluster,
  onChange,
  onRemove,
}: {
  output: LogOutput;
  index: number;
  error: (field: string) => string | undefined;
  cluster?: ClusterRef | undefined;
  onChange: (field: string, patch: Partial<LogOutput>) => void;
  onRemove: () => void;
}): JSX.Element {
  const label = o.name === "" ? `Output ${index + 1}` : o.name;
  return (
    <fieldset className="group">
      <legend>{label}</legend>
      <Field label="Name" error={error("name")}>
        {(id) => (
          <input
            id={id}
            maxLength={maxOutputName}
            value={o.name}
            onChange={(e) => onChange("name", { name: e.target.value.trim() })}
          />
        )}
      </Field>
      <Field label="Kind">
        {(id) => (
          <select
            id={id}
            value={o.kind}
            onChange={(e) => {
              const v = e.target.value;
              if (oneOf(outputKinds, v)) {
                onChange("kind", { kind: v, auth: v === "syslog" ? "none" : o.auth });
              }
            }}
          >
            {outputKinds.map((k) => (
              <option key={k} value={k}>
                {kindLabels[k]}
              </option>
            ))}
          </select>
        )}
      </Field>
      <Field label="Endpoint" hint={endpointHints[o.kind]} error={error("endpoint")}>
        {(id) => (
          <input
            id={id}
            value={o.endpoint}
            onChange={(e) => onChange("endpoint", { endpoint: e.target.value.trim() })}
          />
        )}
      </Field>
      <fieldset className="choices">
        <legend>Streams</legend>
        {outputStreams.map((s) => (
          <label key={s} className="check">
            <input
              type="checkbox"
              checked={o.streams.includes(s)}
              onChange={(e) =>
                onChange("streams", {
                  streams: outputStreams.filter((x) => (x === s ? e.target.checked : o.streams.includes(x))),
                })
              }
            />{" "}
            {streamLabels[s]}
          </label>
        ))}
        <p className="hint">None ticked sends all.</p>
      </fieldset>
      {o.kind !== "syslog" && (
        <Field label="Authentication" error={error("auth")}>
          {(id) => (
            <select
              id={id}
              value={o.auth}
              onChange={(e) => {
                const v = e.target.value;
                if (oneOf(outputAuths, v)) {
                  onChange("auth", { auth: v });
                }
              }}
            >
              {outputAuths.map((a) => (
                <option key={a} value={a}>
                  {authLabels[a]}
                </option>
              ))}
            </select>
          )}
        </Field>
      )}
      {o.auth !== "none" &&
        o.name !== "" &&
        (cluster === undefined ? (
          <p className="hint">{outputsNeedCredential}</p>
        ) : (
          <CredentialField
            label="Credential"
            purpose={outputPurpose(o.name)}
            cluster={cluster}
            keys={o.auth === "basic" ? basicKeys : bearerKeys}
          />
        ))}
      <Field label="CA certificate" hint="PEM, for a private CA" error={error("caBundle")}>
        {(id) => (
          <textarea
            id={id}
            rows={3}
            value={o.caBundle}
            onChange={(e) => onChange("caBundle", { caBundle: e.target.value })}
          />
        )}
      </Field>
      <button type="button" onClick={onRemove}>
        Remove {label}
      </button>
    </fieldset>
  );
}

/**
 * LogOutputsFields lists the destinations every log also goes to. A field shows its problem once
 * touched, or all of them once the user tried to go on (showAll). The disclosure starts closed
 * unless outputs exist.
 */
export function LogOutputsFields({
  value,
  onChange,
  showAll = false,
  cluster,
}: {
  value: LogOutput[];
  onChange: (outputs: LogOutput[]) => void;
  showAll?: boolean;
  cluster?: ClusterRef | undefined;
}): JSX.Element {
  const [open, setOpen] = useState(value.length > 0);
  const [edited, setEdited] = useState<ReadonlySet<string>>(new Set());
  const problems = outputProblems(value);
  const change = (i: number, field: string, patch: Partial<LogOutput>): void => {
    setEdited((s) => new Set([...s, `${i}.${field}`]));
    onChange(value.map((o, j) => (j === i ? { ...o, ...patch } : o)));
  };
  return (
    <details className="advanced" open={open} onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary>Log outputs</summary>
      <p className="hint">Every log also goes to these, beside Loki.</p>
      {value.map((o, i) => (
        <OutputFields
          // The list has no ids, and an output's position is what its fields are keyed on.
          // eslint-disable-next-line react/no-array-index-key
          key={i}
          output={o}
          index={i}
          error={(field) => (showAll || edited.has(`${i}.${field}`) ? problems[`${i}.${field}`] : undefined)}
          cluster={cluster}
          onChange={(field, patch) => change(i, field, patch)}
          onRemove={() => {
            setEdited(new Set());
            onChange(value.filter((_, j) => j !== i));
          }}
        />
      ))}
      <button type="button" disabled={value.length >= maxOutputs} onClick={() => onChange([...value, newOutput()])}>
        Add output
      </button>
    </details>
  );
}
