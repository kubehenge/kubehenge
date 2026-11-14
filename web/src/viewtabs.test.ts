import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

const styles = readFileSync(`${process.cwd()}/src/styles.css`, "utf8");

// The phone layout is CSS, which jsdom does not lay out, so the rule that keeps every tab in sight is read as written.
describe("a cluster's tabs on a phone", () => {
  it("wrap to a second row instead of scrolling sideways", () => {
    const phone = [...styles.matchAll(/@media \(width < 900px\) \{(?<rules>[\s\S]*?)\n\}\n/gu)]
      .map((m) => m.groups?.["rules"])
      .join("\n");
    const views = [...phone.matchAll(/\n {2}\.views \{(?<body>[^}]*)\}/gu)].map((m) => m.groups?.["body"]).join("\n");
    expect(views).toContain("flex-wrap: wrap");
    expect(views).not.toContain("overflow-x");
  });
});

describe("a dialog's backdrop", () => {
  it("hides most of the page and blurs the rest", () => {
    const rule = [...styles.matchAll(/\.dialog::backdrop,\n\.palette::backdrop \{(?<body>[^}]*)\}/gu)]
      .map((m) => m.groups?.["body"])
      .join("\n");
    const alpha = [...rule.matchAll(/rgb\(0 0 0 \/ (?<percent>\d+)%\)/gu)].map((m) => Number(m.groups?.["percent"]));
    expect(alpha.at(0)).toBeGreaterThanOrEqual(60);
    expect(rule).toContain("backdrop-filter: blur");
  });
});
