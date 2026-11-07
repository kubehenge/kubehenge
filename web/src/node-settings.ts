import type { NodesVariable } from "./types";
import { oneOf, record, records, text } from "./values";

/** The effects a taint may have, as Kubernetes names them. */
export const taintEffects = ["NoSchedule", "PreferNoSchedule", "NoExecute"] as const;
export type TaintEffect = (typeof taintEffects)[number];

export interface NodeLabel {
  key: string;
  value: string;
}

/** A taint as it is edited: its effect is whatever the select holds. */
export interface NodeTaint {
  key: string;
  value: string;
  effect: string;
}

/** The labels and taints the nodes of a pool, or of the cluster's workers, get. */
export interface NodeSettings {
  labels: NodeLabel[];
  taints: NodeTaint[];
}

export const noNodeSettings: NodeSettings = { labels: [], taints: [] };

export const maxLabels = 50;
export const maxTaints = 20;

const maxPart = 63;
const maxPrefix = 253;

// A name: alphanumeric at both ends, with dashes, underscores and dots between.
const namePattern = /^[A-Za-z0-9](?:[-A-Za-z0-9_.]*[A-Za-z0-9])?$/u;
const prefixPattern = /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)*$/u;

// The keys under kubernetes.io that the kubelet lets a node carry.
const kubeletKeys = new Set([
  "kubernetes.io/hostname",
  "kubernetes.io/os",
  "kubernetes.io/arch",
  "beta.kubernetes.io/os",
  "beta.kubernetes.io/arch",
  "beta.kubernetes.io/instance-type",
  "node.kubernetes.io/instance-type",
  "topology.kubernetes.io/zone",
  "topology.kubernetes.io/region",
  "failure-domain.beta.kubernetes.io/zone",
  "failure-domain.beta.kubernetes.io/region",
]);

const reservedDomains = ["kubernetes.io", "k8s.io"];
const kubeletDomains = ["kubelet.kubernetes.io", "node.kubernetes.io"];

const within = (prefix: string, domains: string[]): boolean =>
  domains.some((d) => prefix === d || prefix.endsWith(`.${d}`));

const nameRule = "Letters, digits, dashes, dots and underscores, from a letter or digit";

/** keyProblem says what is wrong with a label or taint key, or undefined where it holds. */
export function keyProblem(key: string): string | undefined {
  if (key === "") {
    return "Enter a key";
  }
  const parts = key.split("/");
  const name = parts.at(-1) ?? "";
  const prefix = parts.length === 2 ? parts[0] : undefined;
  if (parts.length > 2) {
    return "Use name or prefix/name";
  }
  if (prefix !== undefined && (prefix.length > maxPrefix || !prefixPattern.test(prefix))) {
    return "The prefix is a DNS name of at most 253 characters";
  }
  if (name.length > maxPart) {
    return `The name is at most ${maxPart} characters`;
  }
  return namePattern.test(name) ? undefined : nameRule;
}

/** valueProblem says what is wrong with a label or taint value. An empty one is fine. */
export function valueProblem(value: string): string | undefined {
  if (value === "") {
    return undefined;
  }
  if (value.length > maxPart) {
    return `At most ${maxPart} characters`;
  }
  return namePattern.test(value) ? undefined : nameRule;
}

/** reservedProblem refuses the label keys the kubelet refuses to set on its own node. */
export function reservedProblem(key: string): string | undefined {
  const [prefix, name] = key.split("/");
  if (name === undefined || prefix === undefined || !within(prefix, reservedDomains)) {
    return undefined;
  }
  if (kubeletKeys.has(key) || within(prefix, kubeletDomains)) {
    return undefined;
  }
  return "The kubelet refuses labels in kubernetes.io and k8s.io, except kubelet.kubernetes.io/ and node.kubernetes.io/.";
}

/** What keeps one row from being sent, by the part it sits in. */
export interface RowProblem {
  key?: string;
  value?: string;
  effect?: string;
}

const duplicate = "Each key once per pool.";

function repeated(keys: string[]): boolean[] {
  return keys.map((k, i) => k !== "" && keys.indexOf(k) !== i);
}

/** labelProblems lists each label's problem, in the labels' order. */
export function labelProblems(labels: NodeLabel[]): RowProblem[] {
  const again = repeated(labels.map((l) => l.key));
  return labels.map((l, i) => {
    const key = keyProblem(l.key) ?? reservedProblem(l.key) ?? (again[i] === true ? duplicate : undefined);
    const value = valueProblem(l.value);
    return { ...(key !== undefined && { key }), ...(value !== undefined && { value }) };
  });
}

/** taintProblems lists each taint's problem, in the taints' order. */
export function taintProblems(taints: NodeTaint[]): RowProblem[] {
  const again = repeated(taints.map((t) => t.key));
  return taints.map((t, i) => {
    const key = keyProblem(t.key) ?? (again[i] === true ? duplicate : undefined);
    const value = valueProblem(t.value);
    const effect = oneOf(taintEffects, t.effect) ? undefined : `Choose ${taintEffects.join(", ")}`;
    return {
      ...(key !== undefined && { key }),
      ...(value !== undefined && { value }),
      ...(effect !== undefined && { effect }),
    };
  });
}

const firstOf = (rows: RowProblem[]): string | undefined =>
  rows.map((r) => r.key ?? r.value ?? r.effect).find((p) => p !== undefined);

/** nodeSettingsProblem says what keeps the settings from being sent: the counts, then the first row's. */
export function nodeSettingsProblem(s: NodeSettings): string | undefined {
  if (s.labels.length > maxLabels) {
    return `At most ${maxLabels} labels`;
  }
  if (s.taints.length > maxTaints) {
    return `At most ${maxTaints} taints`;
  }
  return firstOf(labelProblems(s.labels)) ?? firstOf(taintProblems(s.taints));
}

/** nodesOf reads a workerNodes variable, or a pool's own nodes. */
export function nodesOf(v: unknown): NodeSettings {
  const r = record(v);
  return {
    labels: records(r["labels"]).map((l) => ({ key: text(l["key"]) ?? "", value: text(l["value"]) ?? "" })),
    taints: records(r["taints"]).map((t) => ({
      key: text(t["key"]) ?? "",
      value: text(t["value"]) ?? "",
      effect: text(t["effect"]) ?? "NoSchedule",
    })),
  };
}

/** nodesVariable is the settings as the API takes them: a taint's empty value left out. */
export function nodesVariable(s: NodeSettings): NodesVariable {
  return {
    labels: s.labels.map((l) => ({ key: l.key, value: l.value })),
    taints: s.taints.map((t) => ({ key: t.key, ...(t.value !== "" && { value: t.value }), effect: t.effect })),
  };
}

export const hasNodeSettings = (s: NodeSettings): boolean => s.labels.length > 0 || s.taints.length > 0;

export const sameNodeSettings = (a: NodeSettings, b: NodeSettings): boolean =>
  JSON.stringify(nodesVariable(a)) === JSON.stringify(nodesVariable(b));

/** labelText shows a label as kubectl does. */
export const labelText = (l: NodeLabel): string => `${l.key}=${l.value}`;

/** taintText shows a taint as kubectl does. */
export const taintText = (t: NodeTaint): string => `${t.key}${t.value === "" ? "" : `=${t.value}`}:${t.effect}`;

/** nodeSettingsText sums the settings up in a line, for a summary. */
export function nodeSettingsText(s: NodeSettings): string {
  const parts: string[] = [];
  if (s.labels.length > 0) {
    parts.push(`${s.labels.length} ${s.labels.length === 1 ? "label" : "labels"}`);
  }
  if (s.taints.length > 0) {
    parts.push(`${s.taints.length} ${s.taints.length === 1 ? "taint" : "taints"}`);
  }
  return parts.length > 0 ? parts.join(", ") : "None";
}
