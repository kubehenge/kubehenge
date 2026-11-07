import { record, text } from "./values";

// The limits the namespaceLabels variable carries in every ClusterClass.
export const maxNamespaceLabels = 20;
export const maxLabelValue = 63;

export interface ParsedLabels {
  labels: Record<string, string>;
  /** What keeps the text from being the labels. It names the first line at fault. */
  problem?: string;
}

/** parseLabels reads one key=value per line. A blank line is skipped. */
export function parseLabels(input: string): ParsedLabels {
  const labels: Record<string, string> = {};
  const lines = input
    .split("\n")
    .map((line, index) => ({ line: line.trim(), number: index + 1 }))
    .filter((l) => l.line !== "");
  if (lines.length > maxNamespaceLabels) {
    return { labels, problem: `At most ${maxNamespaceLabels} labels` };
  }
  for (const { line, number } of lines) {
    const at = line.indexOf("=");
    if (at === -1) {
      return { labels, problem: `Line ${number} needs key=value` };
    }
    const key = line.slice(0, at).trim();
    const value = line.slice(at + 1).trim();
    if (key === "") {
      return { labels, problem: `Line ${number} has no key` };
    }
    if (value.length > maxLabelValue) {
      return { labels, problem: `Line ${number}: value over ${maxLabelValue} characters` };
    }
    labels[key] = value;
  }
  return { labels };
}

/** labelsOf reads the namespaceLabels variable. */
export function labelsOf(v: unknown): Record<string, string> {
  const labels: Record<string, string> = {};
  for (const [key, value] of Object.entries(record(v))) {
    const s = text(value);
    if (s !== undefined) {
      labels[key] = s;
    }
  }
  return labels;
}

/** labelsLines is the labels as the text area holds them, one per line. */
export const labelsLines = (labels: Record<string, string>): string =>
  Object.entries(labels)
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");

/** labelsText is the labels on one line, or None. */
export function labelsText(labels: Record<string, string>): string {
  const pairs = Object.entries(labels).map(([k, v]) => `${k}=${v}`);
  return pairs.length > 0 ? pairs.join(", ") : "None";
}
