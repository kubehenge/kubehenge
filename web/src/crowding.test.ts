import { describe, expect, it } from "vitest";

import { crowding } from "./Machines";
import type { Machine } from "./types";

const unplaced = (name: string, role: Machine["role"] = "control-plane"): Machine => ({
  name,
  role,
  phase: "Running",
  ready: true,
  created: "2026-10-16T09:00:00Z",
});

const vm = (name: string, proxmoxNode: string, role: Machine["role"] = "control-plane"): Machine => ({
  ...unplaced(name, role),
  proxmoxNode,
});

const spread = [vm("a", "pve1"), vm("b", "pve2"), vm("c", "pve3"), vm("w", "pve1", "worker")];
const unknown = [vm("a", "pve1"), unplaced("b"), unplaced("c")];
const threeOnTwo = [vm("a", "pve1"), vm("b", "pve1"), vm("c", "pve2")];
const fiveOnFour = [vm("a", "pve1"), vm("b", "pve1"), vm("c", "pve2"), vm("d", "pve3"), vm("e", "pve4")];

describe("control-plane VMs sharing a node", () => {
  it("says nothing while each runs on a node of its own, workers aside", () => {
    expect(crowding(spread)).toBeUndefined();
  });

  it("says nothing of VMs whose node it does not know", () => {
    expect(crowding(unknown)).toBeUndefined();
  });

  it("names the node and what losing it costs", () => {
    expect(crowding(threeOnTwo)).toBe(
      "pve1 runs 2 of 3 control-plane VMs, so losing it stops the control plane. Migrate one to another node.",
    );
    expect(crowding(fiveOnFour)).toMatch(/^pve1 runs 2 of 5 control-plane VMs, so losing it takes 2 down\./u);
  });
});
