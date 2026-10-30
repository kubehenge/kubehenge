import type { BackupRun, ClusterBackups, RestoreRequest, RestoreRun } from "./types";
import { age, bytes, filled, sentence, type Health } from "./ui";

/** Velero's phases of a backup or restore still under way. */
export const runningPhases = new Set([
  "New",
  "InProgress",
  "WaitingForPluginOperations",
  "WaitingForPluginOperationsPartiallyFailed",
  "Finalizing",
  "FinalizingPartiallyFailed",
]);

const finishedPhases = new Set(["Completed", "PartiallyFailed", "Failed", "FailedValidation"]);

/** finished tells whether a backup or restore has ended, well or not. */
export const finished = (phase: string): boolean => finishedPhases.has(phase);

/** phaseStatus says how far a backup or restore is, as the cluster list says it of clusters. */
export function phaseStatus(phase: string): { state: Health; text: string } {
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

/** running tells whether anything in the view is under way, a deletion among it. */
export const running = (v: ClusterBackups | undefined): boolean =>
  v !== undefined &&
  ([...v.backups, ...v.restores].some((r) => r.phase === "" || runningPhases.has(r.phase)) ||
    v.backups.some((b) => b.deleting === true));

/** restorable tells whether a backup has finished well enough to be restored. */
export const restorable = (b: BackupRun): boolean => b.phase === "Completed" || b.phase === "PartiallyFailed";

/** deletable tells whether a backup of the cluster's own may be deleted now. */
export const deletable = (b: BackupRun): boolean => b.from === undefined && b.deleting !== true && finished(b.phase);

export const when = (iso: string | undefined): string => (iso === undefined ? "" : new Date(iso).toLocaleString());

export const ago = (iso: string | undefined): string => (iso === undefined ? "" : `${age(iso)} ago`);

/** counted says how many warnings and errors a run had, or "" for none. */
export function counted(r: { warnings: number; errors: number }): string {
  const parts: string[] = [];
  if (r.errors > 0) {
    parts.push(r.errors === 1 ? "1 error" : `${r.errors} errors`);
  }
  if (r.warnings > 0) {
    parts.push(r.warnings === 1 ? "1 warning" : `${r.warnings} warnings`);
  }
  return parts.length > 0 ? sentence(parts.join(", ")) : "";
}

/** outcome sums up what a run did: why it failed, and its warnings and errors. */
export function outcome(r: { warnings: number; errors: number; failure?: string }): string {
  return [filled(r.failure) ? sentence(r.failure) : "", counted(r)].filter(Boolean).join(" ");
}

/** restoreProgress says how far a restore has come: its items, and its volumes' data. */
export function restoreProgress(r: RestoreRun): string[] {
  const out: string[] = [];
  if (r.total > 0) {
    out.push(`${r.items} of ${r.total} items`);
  }
  if (r.volumes > 0) {
    out.push(`${r.volumesDone} of ${r.volumes} volumes, ${bytes(r.bytesDone)} of ${bytes(r.bytesTotal)}`);
  }
  return out;
}

const list = (names: string[]): string => names.join(", ");

/** restoreScope says what a restore brings back, in words. */
export function restoreScope(r: RestoreRun): string {
  const namespaces = r.namespaces ?? [];
  const parts = [namespaces.length === 0 ? "Every namespace" : list(namespaces)];
  if ((r.excludedNamespaces ?? []).length > 0) {
    parts[0] += ` but ${list(r.excludedNamespaces ?? [])}`;
  }
  if ((r.resources ?? []).length > 0) {
    parts.push(`only ${list(r.resources ?? [])}`);
  }
  const left = (r.excludedResources ?? []).filter((k) => r.volumeData || k !== "pods");
  if (left.length > 0) {
    parts.push(`without ${list(left)}`);
  }
  if (!r.volumeData) {
    parts.push("without volume data");
  }
  return parts.join("; ");
}

const dnsLabel = /^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/u;
const dnsSubdomain = /^(?=.{1,253}$)[a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)*$/u;
const maxRestoreNames = 100;

/** splitNames reads a list typed with commas or spaces between its names. */
export const splitNames = (typed: string): string[] => typed.split(/[\s,]+/u).filter(Boolean);

/** namesProblem says what the server would refuse in a list of a restore's namespaces or kinds. */
export function namesProblem(names: string[], what: "namespace" | "kind"): string | undefined {
  if (names.length > maxRestoreNames) {
    return `Enter ${maxRestoreNames} or fewer`;
  }
  const valid = what === "namespace" ? dnsLabel : dnsSubdomain;
  const bad = names.find((n) => !valid.test(n));
  if (bad !== undefined) {
    return `${bad} is not a ${what} name`;
  }
  const twice = names.find((n, i) => names.indexOf(n) !== i);
  return twice === undefined ? undefined : `${twice} is listed twice`;
}

/** namespaceProblem says what the server would refuse in the namespaces chosen to restore. */
export function namespaceProblem(names: string[]): string | undefined {
  return names.length === 0 ? "Enter at least one namespace" : namesProblem(names, "namespace");
}

/** What a restore is to bring back, as typed: lists of names with commas or spaces between them. */
export interface RestoreFields {
  /** Every namespace in the backup but those left out, rather than those named. */
  whole: boolean;
  namespaces: string;
  excludedNamespaces: string;
  resources: string;
  excludedResources: string;
  volumeData: boolean;
}

export const newRestoreFields = (): RestoreFields => ({
  whole: true,
  namespaces: "",
  excludedNamespaces: "",
  resources: "",
  excludedResources: "",
  volumeData: true,
});

export type RestoreField = "namespaces" | "excludedNamespaces" | "resources" | "excludedResources";

/** restoreProblems says, by field, what the server would refuse in a restore. */
export function restoreProblems(f: RestoreFields): Partial<Record<RestoreField, string>> {
  const p: Partial<Record<RestoreField, string>> = {};
  const only = splitNames(f.resources);
  const without = splitNames(f.excludedResources);
  const namespaces = f.whole
    ? namesProblem(splitNames(f.excludedNamespaces), "namespace")
    : namespaceProblem(splitNames(f.namespaces));
  if (namespaces !== undefined) {
    p[f.whole ? "excludedNamespaces" : "namespaces"] = namespaces;
  }
  const resources =
    namesProblem(only, "kind") ??
    (!f.volumeData && only.includes("pods") ? "Without the volumes' data, the pods stay out." : undefined);
  if (resources !== undefined) {
    p.resources = resources;
  }
  const excluded =
    namesProblem(without, "kind") ??
    (only.some((k) => without.includes(k)) ? "A kind is both restored and left out" : undefined);
  if (excluded !== undefined) {
    p.excludedResources = excluded;
  }
  return p;
}

/** restoreRequest asks for what the fields pick, leaving out what is the server's default. */
export function restoreRequest(backup: string, f: RestoreFields): RestoreRequest {
  const req: RestoreRequest = { backup };
  const left = splitNames(f.excludedNamespaces);
  const only = splitNames(f.resources);
  const without = splitNames(f.excludedResources);
  if (!f.whole) {
    req.namespaces = splitNames(f.namespaces);
  } else if (left.length > 0) {
    req.excludedNamespaces = left;
  }
  if (only.length > 0) {
    req.resources = only;
  }
  if (without.length > 0) {
    req.excludedResources = without;
  }
  if (!f.volumeData) {
    req.volumeData = false;
  }
  return req;
}
