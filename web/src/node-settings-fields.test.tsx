import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { NodeSettings } from "./node-settings";
import { NodeSettingsFields } from "./NodeSettings";

const value = {
  labels: [
    { key: "tier", value: "db" },
    { key: "zone", value: "a" },
  ],
  taints: [{ key: "dedicated", value: "db", effect: "NoSchedule" }],
};

describe("labels and taints editor", () => {
  it("keeps the rows in a container the layout follows, with every field named for the stacked layout", () => {
    const { container } = render(<NodeSettingsFields value={value} onChange={vi.fn<(v: NodeSettings) => void>()} />);

    // The stacked layout answers the width of this container, which the style sheet names.
    const containers = container.querySelectorAll(".node-rows");
    expect(containers).toHaveLength(2);
    for (const rows of containers) {
      expect(rows.querySelectorAll(".node-row").length).toBeGreaterThan(0);
    }

    const label = within(screen.getByRole("group", { name: "Label 2" }));
    expect(label.getByRole("textbox", { name: "Key" })).toHaveValue("zone");
    expect(label.getByRole("button", { name: "Remove label 2" })).toBeInTheDocument();
    // Each field carries its name as visible text for where the heading line is hidden.
    expect(
      [...screen.getByRole("group", { name: "Label 2" }).querySelectorAll(".node-field-name")].map(
        (n) => n.textContent,
      ),
    ).toStrictEqual(["Key", "Value"]);

    const taint = screen.getByRole("group", { name: "Taint 1" });
    expect(taint).toHaveClass("taint-row");
    expect([...taint.querySelectorAll(".node-field-name")].map((n) => n.textContent)).toStrictEqual([
      "Key",
      "Value",
      "Effect",
    ]);
    expect(within(taint).getByRole("combobox", { name: "Effect" })).toHaveValue("NoSchedule");
    expect(within(taint).getByRole("button", { name: "Remove taint 1" })).toBeInTheDocument();
  });
});
