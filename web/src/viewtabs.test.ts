import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

const styles = readFileSync(`${process.cwd()}/src/styles.css`, "utf8");

// The phone layout is CSS, which jsdom does not lay out, so the rule that keeps every tab in sight is read as written.
describe("a cluster's tabs on a phone", () => {
  it("wrap to a second row instead of scrolling sideways", () => {
    const phone = [...styles.matchAll(/@media \(width < 900px\) \{(?<rules>[\s\S]*?)\n\}\n/gu)]
      .map((m) => m.groups?.["rules"] ?? "")
      .join("\n");
    const views = /\n {2}\.views \{(?<body>[^}]*)\}/u.exec(phone)?.groups?.["body"] ?? "";
    expect(views).toContain("flex-wrap: wrap");
    expect(views).not.toContain("overflow-x");
  });
});
