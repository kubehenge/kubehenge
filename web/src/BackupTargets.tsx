import { useState, type JSX } from "react";

import type { BackupTarget } from "./types";
import { Field, filled, sentence } from "./ui";

export const maxTargets = 16;

// The chart's schema's patterns for a target.
const targetName = /^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/u;
const targetBucket = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/u;
const targetRegion = /^[a-z0-9-]{1,64}$/u;
const targetPrefix = /^[A-Za-z0-9!_.*'()-]+(?:\/[A-Za-z0-9!_.*'()-]+)*$/u;
const maxPrefix = 256;

type TargetField = "name" | "endpoint" | "bucket" | "region" | "prefix" | "caCertificate";
type TargetFieldProblems = Partial<Record<TargetField, string>>;

const fieldOrder: TargetField[] = ["name", "endpoint", "bucket", "region", "prefix", "caCertificate"];

function endpointProblem(endpoint: string): string | undefined {
  let url: URL | undefined;
  try {
    url = new URL(endpoint);
  } catch {
    url = undefined;
  }
  return !url || url.protocol !== "https:" || url.search || url.hash || url.username || url.password
    ? "enter an https:// URL without query, fragment or user."
    : undefined;
}

/** fieldProblemsOf finds, field by field, what Kubehenge would refuse in each target. */
function fieldProblemsOf(targets: BackupTarget[]): TargetFieldProblems[] {
  const seen = new Set<string>();
  return targets.map((t) => {
    const p: TargetFieldProblems = {};
    if (!targetName.test(t.name)) {
      p.name = "use lowercase letters, digits and dashes.";
    } else if (seen.has(t.name)) {
      p.name = "this name is already used.";
    }
    seen.add(t.name);
    const endpoint = filled(t.endpoint) ? endpointProblem(t.endpoint) : undefined;
    if (endpoint !== undefined) {
      p.endpoint = endpoint;
    }
    if (!targetBucket.test(t.bucket)) {
      p.bucket = "use 3 to 63 lowercase letters, digits, dots and dashes.";
    }
    if (filled(t.region) && !targetRegion.test(t.region)) {
      p.region = "enter a region like eu-central-2.";
    }
    if (filled(t.prefix) && (t.prefix.length > maxPrefix || !targetPrefix.test(t.prefix))) {
      p.prefix = `use letters, digits and !_.*'()-, with / only between folders, at most ${maxPrefix}.`;
    }
    if (filled(t.caCertificate) && !t.caCertificate.includes("-----BEGIN CERTIFICATE-----")) {
      p.caCertificate = "paste a PEM certificate.";
    }
    return p;
  });
}

/** targetProblems lists what Kubehenge would refuse in a list of targets. */
export function targetProblems(targets: BackupTarget[]): string[] {
  const p: string[] = targets.length > maxTargets ? [`At most ${maxTargets} backup targets.`] : [];
  fieldProblemsOf(targets).forEach((found, i) => {
    for (const k of fieldOrder) {
      if (found[k] !== undefined) {
        p.push(`Backup target ${i + 1}: ${found[k]}`);
      }
    }
  });
  return p;
}

/** cleanTargets drops empty optional fields, so they take the module's defaults. */
export function cleanTargets(targets: BackupTarget[]): BackupTarget[] {
  return targets.map((t) => {
    const out: BackupTarget = { name: t.name.trim(), bucket: t.bucket.trim() };
    for (const k of ["endpoint", "region", "prefix"] as const) {
      const v = t[k]?.trim();
      if (filled(v)) {
        out[k] = v;
      }
    }
    if (filled(t.caCertificate)) {
      out.caCertificate = t.caCertificate;
    }
    if (t.noChecksums === true) {
      out.noChecksums = true;
    }
    return out;
  });
}

/** targetSummary names a target and where it keeps the backups. */
export function targetSummary(t: BackupTarget): string {
  let where = "AWS S3";
  if (filled(t.endpoint)) {
    where = URL.canParse(t.endpoint) ? new URL(t.endpoint).host : t.endpoint;
  }
  return `${t.name} (${t.bucket}${filled(t.prefix) ? `/${t.prefix}` : ""} at ${where})`;
}

interface Entry {
  key: number;
  target: BackupTarget;
}

let nextKey = 0;
const entry = (target: BackupTarget): Entry => ({ key: nextKey++, target });

/**
 * BackupTargets edits the list of S3 stores clusters back up to. It starts from initial. Each entry
 * keeps its key while those before it are removed, so its inputs keep their contents and focus. A
 * field shows its problem once it was edited, or every field once the user tried to go on.
 */
export function BackupTargets({
  initial,
  showAll = false,
  onChange,
}: {
  initial: BackupTarget[];
  showAll?: boolean;
  onChange: (v: BackupTarget[]) => void;
}): JSX.Element {
  const [entries, setEntries] = useState<Entry[]>(() => initial.map((t) => entry(t)));
  const [edited, setEdited] = useState<ReadonlySet<string>>(new Set());
  // The target just added, whose first field takes the focus.
  const [added, setAdded] = useState<number>();
  const change = (next: Entry[]) => {
    setEntries(next);
    onChange(next.map((e) => e.target));
  };
  const update = (i: number, patch: Partial<BackupTarget>) => {
    const key = entries[i]?.key;
    setEdited((s) => new Set([...s, ...Object.keys(patch).map((k) => `${key}.${k}`)]));
    change(entries.map((e, j) => (j === i ? { ...e, target: { ...e.target, ...patch } } : e)));
  };
  const remove = (i: number) => change(entries.filter((_, j) => j !== i));
  const value = entries.map((e) => e.target);
  const problems = fieldProblemsOf(value);
  return (
    <div className="providers">
      {value.length === 0 && <p className="muted">No backup targets</p>}
      {entries.map(({ key, target: t }, i) => {
        const error = (k: TargetField): string | undefined => {
          const found = problems[i]?.[k];
          return found !== undefined && (showAll || edited.has(`${key}.${k}`)) ? sentence(found) : undefined;
        };
        return (
          <fieldset key={key} className="group">
            <legend>Backup target {i + 1}</legend>
            <Field label="Name" error={error("name")}>
              {(id) => (
                <input
                  id={id}
                  value={t.name}
                  ref={(el) => {
                    if (el && key === added) {
                      el.focus();
                      setAdded(undefined);
                    }
                  }}
                  onChange={(e) => update(i, { name: e.target.value.trim() })}
                />
              )}
            </Field>
            <Field label="Endpoint" hint="An https:// URL, or empty for AWS S3" error={error("endpoint")}>
              {(id) => (
                <input
                  id={id}
                  value={t.endpoint ?? ""}
                  onChange={(e) => update(i, { endpoint: e.target.value.trim() })}
                />
              )}
            </Field>
            <Field label="Bucket" error={error("bucket")}>
              {(id) => (
                <input id={id} value={t.bucket} onChange={(e) => update(i, { bucket: e.target.value.trim() })} />
              )}
            </Field>
            <Field label="Region" hint="Empty for us-east-1" error={error("region")}>
              {(id) => (
                <input id={id} value={t.region ?? ""} onChange={(e) => update(i, { region: e.target.value.trim() })} />
              )}
            </Field>
            <Field label="Prefix" error={error("prefix")}>
              {(id) => (
                <input id={id} value={t.prefix ?? ""} onChange={(e) => update(i, { prefix: e.target.value.trim() })} />
              )}
            </Field>
            <Field label="CA certificate" hint="PEM, for a private CA only" error={error("caCertificate")}>
              {(id) => (
                <textarea
                  id={id}
                  rows={3}
                  value={t.caCertificate ?? ""}
                  onChange={(e) => update(i, { caCertificate: e.target.value })}
                />
              )}
            </Field>
            <label className="check">
              <input
                type="checkbox"
                checked={t.noChecksums !== true}
                onChange={(e) => update(i, { noChecksums: !e.target.checked })}
              />{" "}
              Send checksums
            </label>
            <p className="hint">Switch off if the store refuses checksums.</p>
            <button type="button" onClick={() => remove(i)}>
              Remove target {i + 1}
            </button>
          </fieldset>
        );
      })}
      {value.length < maxTargets && (
        <button
          type="button"
          onClick={() => {
            const fresh = entry({ name: "", bucket: "" });
            setAdded(fresh.key);
            change([...entries, fresh]);
          }}
        >
          Add backup target
        </button>
      )}
    </div>
  );
}
