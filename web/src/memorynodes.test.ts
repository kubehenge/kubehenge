import { describe, expect, it } from "vitest";

import { createRoom, fieldProblems, memoryNodeWarning, newForm } from "./create-form";
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
  it("says nothing when a node has room for the largest machine", () => {
    expect(memoryNodeWarning(form, [node("pve1", 2), node("pve2", 8)])).toBeUndefined();
  });

  it("warns, without blocking the step, when no node the cluster may use has room", () => {
    const chosen = { ...form, place: { placement: "nodes" as const, haRule: "", nodes: ["pve1"] } };
    expect(memoryNodeWarning(chosen, [node("pve1", 2), node("pve2", 30)])).toBe(
      "No node has 8 GiB free, the most is 2 GiB on pve1. The cluster waits for room.",
    );
    expect(createRoom(chosen, [node("pve1", 2), node("pve2", 30)], []).nodeWarning).toBe(
      memoryNodeWarning(chosen, [node("pve1", 2), node("pve2", 30)]),
    );
    expect(createRoom(form, [node("pve1", 20)], []).nodeWarning).toBeUndefined();
    expect(fieldProblems("Machines", chosen, undefined)).not.toHaveProperty("placement");
  });

  it("leaves a node that reports no memory alone, and ignores workers that are not asked for", () => {
    expect(memoryNodeWarning(form, [{ node: "pve1", online: true }])).toBeUndefined();
    expect(memoryNodeWarning({ ...form, workerReplicas: 0 }, [node("pve1", 5)])).toBeUndefined();
  });
});
