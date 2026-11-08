import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { optionRows, type OptionRow } from "./ClusterOptions";
import { FamiliesField } from "./Families";
import type { Cluster } from "./types";

const offered = { ha: false, csi: false, snapshots: false, haGroups: false };

// familiesRow is the Options view's address families row of a cluster with this network variable.
function familiesRow(network: Record<string, unknown>): OptionRow | undefined {
  // optionRows reads only the fields given here.
  // oxlint-disable-next-line typescript/no-unsafe-type-assertion
  const c = { namespace: "clusters", name: "web", distribution: "rke2", variables: { network } } as unknown as Cluster;
  return optionRows(c, offered).find((r) => r.label === "Address families");
}

describe("a cluster's address families", () => {
  it("are shown under Options, with nothing to change them", () => {
    expect(familiesRow({ ipFamilies: ["IPv6", "IPv4"] })).toStrictEqual({
      label: "Address families",
      value: "IPv6 and IPv4",
      group: "Network",
    });
    expect(familiesRow({ ipFamilies: ["IPv6"] })?.value).toBe("IPv6");
    expect(familiesRow({})?.value).toBe("IPv4");
  });

  it("are chosen from the four choices, the primary first", async () => {
    const user = userEvent.setup();
    const onChoose = vi.fn<(choice: string) => void>();
    render(<FamiliesField value="ipv4" hint="Fixed once the cluster exists" onChoose={onChoose} />);
    await user.selectOptions(screen.getByLabelText("Address families"), "IPv6 and IPv4");
    expect(onChoose).toHaveBeenCalledWith("ipv6-ipv4");
  });

  it("read as text where they are fixed", () => {
    render(<FamiliesField value="ipv6" hint="Fixed after creation" fixed onChoose={vi.fn<() => void>()} />);
    expect(screen.getByLabelText("Address families")).toHaveValue("IPv6");
    expect(screen.getByLabelText("Address families")).toHaveAttribute("readonly");
    expect(screen.getByLabelText("Address families")).toHaveAccessibleDescription("Fixed after creation");
  });
});
