import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { list } from "./network";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, useChange } from "./ui";
import { record, records, text, texts } from "./values";

/** One registry mirror as the interface edits it: where the nodes pull a registry's images from. */
export interface MirrorRow {
  registry: string;
  // One address per line.
  endpoints: string;
  /** The authority of the endpoints' certificates, as PEM. Empty for the usual ones. */
  caBundle: string;
}

export const newMirror = (): MirrorRow => ({ registry: "", endpoints: "", caBundle: "" });

const blank = (m: MirrorRow): boolean =>
  m.registry.trim() === "" && m.endpoints.trim() === "" && m.caBundle.trim() === "";

/** pem makes a pasted bundle the form the schema takes: LF line ends and a closing newline. */
export const pem = (s: string): string => {
  const t = s.replaceAll("\r\n", "\n").trim();
  return t === "" ? "" : `${t}\n`;
};

export function mirrorsOf(v: unknown): MirrorRow[] {
  return records(v).map((r) => ({
    registry: text(r["registry"]) ?? "",
    endpoints: (texts(r["endpoints"]) ?? []).join("\n"),
    caBundle: text(r["caBundle"]) ?? "",
  }));
}

/** mirrorsVariable is the variable the rows set, or undefined for none. */
export function mirrorsVariable(rows: MirrorRow[]): Record<string, unknown>[] | undefined {
  const set = rows
    .filter((m) => !blank(m))
    .map((m) => {
      const v: Record<string, unknown> = { registry: m.registry.trim(), endpoints: list(m.endpoints) };
      if (m.caBundle.trim() !== "") {
        v["caBundle"] = pem(m.caBundle);
      }
      return v;
    });
  return set.length > 0 ? set : undefined;
}

export function mirrorsText(rows: MirrorRow[]): string {
  const named = rows.filter((m) => !blank(m)).map((m) => m.registry.trim());
  return named.length > 0 ? named.join(", ") : "None";
}

/** trustedOf reads the trustedCAs variable into the text its field edits. */
export const trustedOf = (v: unknown): string => (texts(v) ?? []).join("\n");

/** trustedVariable is the variable the text sets, or undefined for none. */
export const trustedVariable = (s: string): string[] | undefined => (pem(s) === "" ? undefined : [pem(s)]);

export function trustedText(s: string): string {
  const n = s.split("-----BEGIN CERTIFICATE-----").length - 1;
  if (n === 0) {
    return "None";
  }
  return n === 1 ? "1 certificate" : `${n} certificates`;
}

const registryName = /^(?:\*|[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?(?::[0-9]{1,5})?)$/u;
const endpointAddress =
  /^https?:\/\/[A-Za-z0-9](?:[-A-Za-z0-9.]*[A-Za-z0-9])?(?::[0-9]{1,5})?(?:\/(?:[A-Za-z0-9._-]+\/)*v2)?$/u;
const certificates = /^(?:-----BEGIN CERTIFICATE-----\n(?:[A-Za-z0-9+/=]+\n)+-----END CERTIFICATE-----\n?)+$/u;
const maxMirrors = 16;
const maxEndpoints = 4;

/** certificateProblem says what is wrong with a pasted bundle. The server also checks the dates. */
export const certificateProblem = (s: string): string | undefined =>
  pem(s) === "" || certificates.test(pem(s)) ? undefined : "Paste PEM certificates only";

/** mirrorsProblem names the first thing wrong with the rows, or nothing. The server checks the same. */
export function mirrorsProblem(rows: MirrorRow[]): string | undefined {
  const set = rows.filter((m) => !blank(m));
  if (set.length > maxMirrors) {
    return `At most ${maxMirrors} mirrors`;
  }
  const seen = new Set<string>();
  for (const [i, m] of set.entries()) {
    const at = `Mirror ${i + 1}: `;
    const addresses = list(m.endpoints);
    const problem =
      (!registryName.test(m.registry.trim()) && "enter a lower-case registry host name, or *") ||
      (seen.has(m.registry.trim()) && "each registry once") ||
      (addresses.length === 0 && "add an endpoint") ||
      (addresses.length > maxEndpoints && `at most ${maxEndpoints} endpoints`) ||
      (!addresses.every((a) => endpointAddress.test(a)) &&
        "endpoints are http:// or https:// addresses, a path ends in /v2") ||
      (certificateProblem(m.caBundle)?.toLowerCase() ?? false);
    if (problem !== false) {
      return at + problem;
    }
    seen.add(m.registry.trim());
  }
  return undefined;
}

/** MirrorFields edits the registry mirrors of a cluster's nodes. */
export function MirrorFields({
  value,
  onChange,
  error,
}: {
  value: MirrorRow[];
  onChange: (rows: MirrorRow[]) => void;
  error?: string | undefined;
}): JSX.Element {
  const change = (i: number, patch: Partial<MirrorRow>): void => {
    onChange(value.map((m, j) => (j === i ? { ...m, ...patch } : m)));
  };
  return (
    <fieldset className="group">
      <legend>Registry mirrors</legend>
      {value.map((m, i) => (
        // Rows are edited in place and have no identity of their own.
        // oxlint-disable-next-line react/no-array-index-key
        <fieldset key={i} className="group">
          <legend>{`Mirror ${i + 1}`}</legend>
          <Field label="Registry" hint="Like docker.io, or * for all">
            {(id) => (
              <input
                id={id}
                spellCheck={false}
                value={m.registry}
                onChange={(ev) => change(i, { registry: ev.target.value })}
              />
            )}
          </Field>
          <Field label="Endpoints" hint="One address per line, tried in order">
            {(id) => (
              <textarea
                id={id}
                rows={2}
                spellCheck={false}
                value={m.endpoints}
                onChange={(ev) => change(i, { endpoints: ev.target.value })}
              />
            )}
          </Field>
          <Field label="Mirror CA" hint="PEM, only if the usual authorities do not sign it">
            {(id) => (
              <textarea
                id={id}
                rows={3}
                spellCheck={false}
                value={m.caBundle}
                onChange={(ev) => change(i, { caBundle: ev.target.value })}
              />
            )}
          </Field>
          <button type="button" onClick={() => onChange(value.filter((_, j) => j !== i))}>
            {`Remove mirror ${i + 1}`}
          </button>
        </fieldset>
      ))}
      <button type="button" onClick={() => onChange([...value, newMirror()])}>
        Add mirror
      </button>
      <ErrorBox message={error} />
    </fieldset>
  );
}

/** TrustedCAField edits the authorities the nodes trust besides their OS's own. */
export function TrustedCAField({
  value,
  onChange,
  error,
}: {
  value: string;
  onChange: (s: string) => void;
  error?: string | undefined;
}): JSX.Element {
  return (
    <Field label="Trusted CAs" hint="PEM certificates, for the nodes" error={error}>
      {(id) => (
        <textarea id={id} rows={4} spellCheck={false} value={value} onChange={(ev) => onChange(ev.target.value)} />
      )}
    </Field>
  );
}

/** A cluster's own release mirror as the interface edits it. Empty takes the management cluster's. */
export interface ReleaseMirror {
  url: string;
  /** The authority of the mirror's certificate, as PEM. Empty for the usual ones. */
  caBundle: string;
}

export const newReleaseMirror = (): ReleaseMirror => ({ url: "", caBundle: "" });

export function releaseMirrorOf(v: unknown): ReleaseMirror {
  const r = record(v);
  return { url: text(r["url"]) ?? "", caBundle: text(r["caBundle"]) ?? "" };
}

/** releaseMirrorVariable is the variable the fields set, or undefined for the management cluster's. */
export function releaseMirrorVariable(m: ReleaseMirror): Record<string, string> | undefined {
  if (m.url.trim() === "" && m.caBundle.trim() === "") {
    return undefined;
  }
  const v: Record<string, string> = { url: m.url.trim() };
  if (m.caBundle.trim() !== "") {
    v["caBundle"] = pem(m.caBundle);
  }
  return v;
}

// defaultMirror names what an unset mirror falls back to: the management cluster's, or GitHub. Before
// the classes load it is not known yet.
function defaultMirror(fallback: string | undefined): string {
  if (fallback === undefined) {
    return "Default";
  }
  return `Default: ${fallback === "" ? "GitHub" : fallback}`;
}

/** releaseMirrorText is the mirror a cluster's nodes download from, given the management cluster's. */
export function releaseMirrorText(m: ReleaseMirror, fallback: string | undefined): string {
  return m.url.trim() === "" ? defaultMirror(fallback) : m.url.trim();
}

// releaseMirrorAddress is the class's pattern: http(s), a host, a path, no credentials or query.
const releaseMirrorAddress = /^https?:\/\/[A-Za-z0-9._~:[\]-]+(?<path>\/[A-Za-z0-9._~:/[\]-]*[A-Za-z0-9._~:[\]-])?$/u;

export interface ReleaseMirrorProblems {
  url?: string | undefined;
  caBundle?: string | undefined;
}

/** releaseMirrorProblems are what is wrong with the fields. The server checks the same, and the dates. */
export function releaseMirrorProblems(m: ReleaseMirror): ReleaseMirrorProblems {
  const p: ReleaseMirrorProblems = {};
  const url = m.url.trim();
  if (url !== "" && !releaseMirrorAddress.test(url)) {
    p.url = "Use an http:// or https:// address with no trailing slash, query or credentials";
  }
  if (m.caBundle.trim() !== "") {
    const problem = certificateProblem(m.caBundle);
    if (problem !== undefined) {
      p.caBundle = problem;
    } else if (url === "") {
      p.caBundle = "Add a release mirror first";
    } else if (!url.startsWith("https://")) {
      p.caBundle = "A certificate needs an https:// mirror";
    }
  }
  return p;
}

/** useReleaseMirrorDefault is the management cluster's release mirror the class's nodes take by default. */
export function useReleaseMirrorDefault(className: string): string | undefined {
  const [fallback, setFallback] = useState<string>();
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const classes = await api.classes();
        if (current) {
          setFallback(classes.find((x) => x.name === className)?.releaseMirror ?? "");
        }
      } catch {
        // Without the classes the default is not named. The cluster keeps its setting.
      }
    }
    void load();
    return () => {
      current = false;
    };
  }, [className]);
  return fallback;
}

/** ReleaseMirrorFields edit the mirror a cluster's nodes download their release from. */
export function ReleaseMirrorFields({
  value,
  onChange,
  fallback,
  errors,
}: {
  value: ReleaseMirror;
  onChange: (m: ReleaseMirror) => void;
  fallback: string | undefined;
  errors?: ReleaseMirrorProblems | undefined;
}): JSX.Element {
  return (
    <>
      <Field label="Release mirror" hint={defaultMirror(fallback)} error={errors?.url}>
        {(id) => (
          <input
            id={id}
            spellCheck={false}
            value={value.url}
            onChange={(ev) => onChange({ ...value, url: ev.target.value })}
          />
        )}
      </Field>
      <Field label="Release mirror certificate" hint="PEM, for an https:// mirror" error={errors?.caBundle}>
        {(id) => (
          <textarea
            id={id}
            rows={3}
            spellCheck={false}
            value={value.caBundle}
            onChange={(ev) => onChange({ ...value, caBundle: ev.target.value })}
          />
        )}
      </Field>
    </>
  );
}

export function ReleaseMirrorDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = releaseMirrorOf(c.variables["releaseMirror"]);
  const [value, setValue] = useState(current);
  const [attempted, setAttempted] = useState(false);
  const fallback = useReleaseMirrorDefault(c.class);
  const { busy, error, send } = useChange(c, onClose);
  const found = releaseMirrorProblems(value);
  const next = releaseMirrorVariable(value);
  const changed = JSON.stringify(next) !== JSON.stringify(releaseMirrorVariable(current));
  return (
    <Dialog
      title="Edit release mirror"
      onClose={onClose}
      onSubmit={() => {
        if (found.url !== undefined || found.caBundle !== undefined) {
          setAttempted(true);
          return;
        }
        void send({ variables: { releaseMirror: next ?? null } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed}>
            Save
          </button>
        </>
      }
    >
      <ReleaseMirrorFields
        value={value}
        onChange={setValue}
        fallback={fallback}
        errors={attempted ? found : undefined}
      />
      <ErrorBox message={error} />
    </Dialog>
  );
}

export function MirrorsDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = mirrorsOf(c.variables["registryMirrors"]);
  const [value, setValue] = useState(current);
  const [attempted, setAttempted] = useState(false);
  const { busy, error, send } = useChange(c, onClose);
  const found = mirrorsProblem(value);
  const next = mirrorsVariable(value);
  const changed = JSON.stringify(next) !== JSON.stringify(mirrorsVariable(current));
  return (
    <Dialog
      title="Edit registry mirrors"
      onClose={onClose}
      onSubmit={() => {
        if (found !== undefined) {
          setAttempted(true);
          return;
        }
        void send({ variables: { registryMirrors: next ?? null } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed}>
            Save
          </button>
        </>
      }
    >
      <MirrorFields value={value} onChange={setValue} error={attempted ? found : undefined} />
      <ErrorBox message={error} />
    </Dialog>
  );
}

export function TrustedCAsDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = trustedOf(c.variables["trustedCAs"]);
  const [value, setValue] = useState(current);
  const [attempted, setAttempted] = useState(false);
  const { busy, error, send } = useChange(c, onClose);
  const found = certificateProblem(value);
  const next = trustedVariable(value);
  const changed = JSON.stringify(next) !== JSON.stringify(trustedVariable(current));
  return (
    <Dialog
      title="Edit trusted CAs"
      onClose={onClose}
      onSubmit={() => {
        if (found !== undefined) {
          setAttempted(true);
          return;
        }
        void send({ variables: { trustedCAs: next ?? null } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed}>
            Save
          </button>
        </>
      }
    >
      <TrustedCAField value={value} onChange={setValue} error={attempted ? found : undefined} />
      <ErrorBox message={error} />
    </Dialog>
  );
}
