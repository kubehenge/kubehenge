import { within } from "@testing-library/react";
import axe from "axe-core";
import { expect } from "vitest";

// A name a screen reader can read out has a letter or digit in it: "×" alone does not count.
const readable = /[\p{L}\p{N}]/u;

// Enough of an element's HTML to find it in the source, short enough to keep a failure readable.
const htmlExcerpt = 200;

/**
 * unnamed lists the open dialogs and the buttons that have no readable accessible name. axe's
 * dialog-name skips happy-dom's native dialog, and its button-name takes a symbol such as "×".
 */
function unnamed(container: HTMLElement): string[] {
  const scope = within(container);
  const missing = (role: "dialog" | "button") => {
    const named = new Set(scope.queryAllByRole(role, { name: readable }));
    return scope
      .queryAllByRole(role)
      .filter((element) => !named.has(element))
      .map((element) => `${role} without a readable name: ${element.outerHTML.slice(0, htmlExcerpt)}`);
  };
  return [...missing("dialog"), ...missing("button")];
}

// Rules switched off, each because it cannot work in the test DOM. Every other rule runs, the
// best-practice and experimental ones included.
const unavailable: Record<string, string> = {
  // Needs real layout and painted colours, which happy-dom does not compute.
  "color-contrast": "needs rendered colours and layout",
  "color-contrast-enhanced": "needs rendered colours and layout",
  // Every box is 0 by 0 here, so it passes every target while it builds a grid of the whole page.
  "target-size": "needs layout",
};

// Rules axe leaves off by default (experimental ones) are switched on too.
const rules = Object.fromEntries(axe.getRules().map(({ ruleId }) => [ruleId, { enabled: !(ruleId in unavailable) }]));

/**
 * expectNoViolations runs every axe rule over a rendered container, or only over the parts of it
 * given, and checks every open dialog and button of the container for a readable name. It fails
 * with each violation's rule, help text and the offending nodes' HTML.
 */
export async function expectNoViolations(
  container: HTMLElement,
  where: string,
  parts?: readonly Element[],
): Promise<void> {
  expect(unnamed(container).join("\n"), `unnamed dialogs and buttons on ${where}`).toBe("");
  const result = await axe.run(parts === undefined ? container : [...parts], {
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
