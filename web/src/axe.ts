import axe from "axe-core";
import { expect } from "vitest";

// Rules switched off, each because it cannot work in the test DOM. Every other rule runs, the
// best-practice and experimental ones included.
const unavailable: Record<string, string> = {
  // Needs real layout and painted colours, which happy-dom does not compute.
  "color-contrast": "needs rendered colours and layout",
  "color-contrast-enhanced": "needs rendered colours and layout",
};

// Rules axe leaves off by default (experimental ones) are switched on too.
const rules = Object.fromEntries(axe.getRules().map(({ ruleId }) => [ruleId, { enabled: !(ruleId in unavailable) }]));

/**
 * expectNoViolations runs every axe rule over a rendered container and fails with each violation's
 * rule, help text and the offending nodes' HTML.
 */
export async function expectNoViolations(container: Element, where: string): Promise<void> {
  const result = await axe.run(container, {
    rules,
    resultTypes: ["violations"],
  });
  const report = result.violations
    .map(
      (v) =>
        `${v.id} (${v.impact ?? "n/a"}): ${v.help}\n${v.nodes.map((n) => `  ${n.html}\n    ${n.failureSummary?.replaceAll("\n", " ") ?? ""}`).join("\n")}`,
    )
    .join("\n");
  expect(report, `axe violations on ${where}`).toBe("");
}
