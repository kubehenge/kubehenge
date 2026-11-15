import { describe, expect, it } from "vitest";

import { certificateBundleProblem } from "./pem";
import bundles from "./testdata/pem-bundles.json?raw";

interface BundleCase {
  name: string;
  lines: string[];
  lineEnd?: string;
  noFinalLineEnd?: boolean;
  repeat?: number;
  maxKiB?: number;
  problem: string;
}

const isCase = (v: unknown): v is BundleCase =>
  typeof v === "object" && v !== null && "name" in v && "lines" in v && "problem" in v;

// The server's tests read the same table (pkg/provisioner), so the two refuse the same bundles.
const parsed: unknown = JSON.parse(bundles);
const cases = (
  typeof parsed === "object" && parsed !== null && "cases" in parsed && Array.isArray(parsed.cases)
    ? parsed.cases.filter(isCase)
    : []
).map((c) => {
  const lineEnd = c.lineEnd ?? "\n";
  const once = c.lines.join(lineEnd) + (c.noFinalLineEnd === true ? "" : lineEnd);
  return [
    c.name,
    { bundle: once.repeat(c.repeat ?? 1), maxKiB: c.maxKiB },
    c.problem === "" ? undefined : c.problem,
  ] as const;
});

describe("the layout check of a certificate bundle", () => {
  it("reads every case of the shared table", () => {
    expect(cases.length).toBeGreaterThan(40);
  });

  it.each(cases)("gives the server's verdict: %s", (_, { bundle, maxKiB }, problem) => {
    expect(certificateBundleProblem(bundle, maxKiB)).toBe(problem);
  });
});
