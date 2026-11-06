import { describe, expect, it } from "vitest";

import { memoryNodeProblems, newForm } from "./create-form";
import type { PveNode } from "./types";

const gib = 1_073_741_824;
const node = (name: string, freeGiB: number): PveNode => ({
  node: name,
  online: true,
  memory: (64 - freeGiB) * gib,
  maxMemory: 64 * gib,
});

const form = {
  ...newForm(),
  workerReplicas: 1,
  workerMachine: { cores: 2, memoryMiB: 8192, diskGiB: 30 },
  controlPlaneMachine: { cores: 2, memoryMiB: 4096, diskGiB: 30 },
};

describe("the machines step and the nodes' free memory", () => {
  it("accepts a node with room for the largest machine", () => {
    expect(memoryNodeProblems("Machines", form, [node("pve1", 2), node("pve2", 8)])).toStrictEqual({});
  });

  it("refuses when no node the cluster may use has room", () => {
    const chosen = { ...form, place: { placement: "nodes" as const, haRule: "", nodes: ["pve1"] } };
    expect(memoryNodeProblems("Machines", chosen, [node("pve1", 2), node("pve2", 30)])).toStrictEqual({
      placement: "No node has 8192 MiB free. The best, pve1, has 2048 MiB.",
    });
    expect(memoryNodeProblems("Network", chosen, [node("pve1", 2)])).toStrictEqual({});
  });

  it("leaves a node that reports no memory alone, and ignores workers that are not asked for", () => {
    expect(memoryNodeProblems("Machines", form, [{ node: "pve1", online: true }])).toStrictEqual({});
    const alone = { ...form, workerReplicas: 0 };
    expect(memoryNodeProblems("Machines", alone, [node("pve1", 5)])).toStrictEqual({});
  });
});
