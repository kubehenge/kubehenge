import { useId, useState, type JSX } from "react";

import { list } from "./network";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, Info, filled, useChange } from "./ui";
import { flag, record, text, texts } from "./values";

/** Where the add-ons that reach beyond a cluster send their traffic. */
export type EgressMode = "management" | "own" | "none";

/** A cluster's egressProxy variable as the interface edits it. */
export interface EgressChoice {
  mode: EgressMode;
  url: string;
  // Separated by commas, spaces or lines.
  noProxy: string;
}

export const newEgress = (): EgressChoice => ({ mode: "management", url: "", noProxy: "" });

export function egressOf(v: unknown): EgressChoice {
  const r = record(v);
  if (flag(r["disabled"]) === true) {
    return { ...newEgress(), mode: "none" };
  }
  const url = text(r["url"]);
  if (filled(url)) {
    return { mode: "own", url, noProxy: (texts(r["noProxy"]) ?? []).join(", ") };
  }
  return newEgress();
}

/** egressVariable is the variable a choice sets, or undefined for the management side's proxy. */
export function egressVariable(e: EgressChoice): Record<string, unknown> | undefined {
  if (e.mode === "none") {
    return { disabled: true };
  }
  if (e.mode === "management") {
    return undefined;
  }
  const v: Record<string, unknown> = { url: e.url.trim() };
  const exceptions = list(e.noProxy);
  if (exceptions.length > 0) {
    v["noProxy"] = exceptions;
  }
  return v;
}

/** egressText says in a line where the add-ons' traffic goes. */
export function egressText(e: EgressChoice): string {
  if (e.mode === "management") {
    return "Management proxy";
  }
  if (e.mode === "none") {
    return "No proxy";
  }
  const n = list(e.noProxy).length;
  return n > 0 ? `${e.url.trim()}, ${n} ${n === 1 ? "exception" : "exceptions"}` : e.url.trim();
}

/** The fields of an egress choice a problem belongs to. */
export const egressFields = ["url", "noProxy"] as const;
export type EgressField = (typeof egressFields)[number];

// The schema's limit on the exceptions.
const maxExceptions = 64;

/** egressProblems says, by field, what is wrong with a choice. The server checks the same. */
export function egressProblems(e: EgressChoice): Partial<Record<EgressField, string>> {
  const p: Partial<Record<EgressField, string>> = {};
  if (e.mode !== "own") {
    return p;
  }
  if (!/^https?:\/\/[^\s/@?#]+\/?$/u.test(e.url.trim())) {
    p.url = "Enter an http:// or https:// address without credentials or path";
  }
  if (list(e.noProxy).length > maxExceptions) {
    p.noProxy = `At most ${maxExceptions} exceptions`;
  }
  return p;
}

const modes: { value: EgressMode; label: string }[] = [
  { value: "management", label: "Management proxy" },
  { value: "own", label: "Own proxy" },
  { value: "none", label: "No proxy" },
];

const egressInfo =
  "Cert-manager, Velero, Argo CD, Flux, Headlamp and Rancher use it. Cluster ranges, .svc and .cluster.local always go direct.";

const noProblems: Partial<Record<EgressField, string>> = {};

/** EgressFields chooses the proxy that the add-ons reaching beyond the cluster go through. */
export function EgressFields({
  value: e,
  onChange,
  problems = noProblems,
}: {
  value: EgressChoice;
  onChange: (e: EgressChoice) => void;
  problems?: Partial<Record<EgressField, string | undefined>>;
}): JSX.Element {
  const name = useId();
  return (
    <>
      <fieldset className="choices">
        <legend>Egress proxy</legend>
        {modes.map((m) => (
          <label key={m.value} className="check">
            <input
              type="radio"
              name={name}
              value={m.value}
              checked={e.mode === m.value}
              onChange={() => onChange({ ...e, mode: m.value })}
            />{" "}
            {m.label}
          </label>
        ))}
      </fieldset>
      <Info about="the egress proxy">{egressInfo}</Info>
      {e.mode === "own" && (
        <>
          <Field label="Proxy address" hint="Like http://proxy.example:3128" error={problems.url}>
            {(id) => (
              <input
                id={id}
                spellCheck={false}
                value={e.url}
                onChange={(ev) => onChange({ ...e, url: ev.target.value })}
              />
            )}
          </Field>
          <Field label="Exceptions" hint="Hosts, domains or networks, one per line" error={problems.noProxy}>
            {(id) => (
              <textarea
                id={id}
                rows={2}
                spellCheck={false}
                value={e.noProxy}
                onChange={(ev) => onChange({ ...e, noProxy: ev.target.value })}
              />
            )}
          </Field>
        </>
      )}
    </>
  );
}

export function EgressDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = egressOf(c.variables["egressProxy"]);
  const [value, setValue] = useState(current);
  const { busy, error, send } = useChange(c, onClose);
  const problems = egressProblems(value);
  const next = egressVariable(value);
  const changed = JSON.stringify(next) !== JSON.stringify(egressVariable(current));
  return (
    <Dialog
      title="Edit egress proxy"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { egressProxy: next ?? null } });
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
      <EgressFields value={value} onChange={setValue} problems={problems} />
      <ErrorBox message={error} />
    </Dialog>
  );
}
