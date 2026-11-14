import { describe, expect, it } from "vitest";

import { capacityProblems, createRoom, fieldProblems, memoryNodeWarning, newForm } from "./create-form";
import type { PveNode, Storage } from "./types";

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

describe("the machines step and what the cluster could never fit", () => {
  const local: Storage = {
    storage: "local-lvm",
    node: "pve1",
    shared: false,
    content: ["images"],
    active: true,
    avail: 610 * gib,
    total: 900 * gib,
    usable: true,
    allocatable: true,
  };
  const sized = { ...form, storage: "local-lvm" };

  it("marks a machine larger than every node's memory, and disks beyond the storage's free space", () => {
    const big = { ...sized, workerMachine: { cores: 4, memoryMiB: 900_000, diskGiB: 9000 } };
    expect(capacityProblems("Machines", big, { nodes: [node("pve1", 20)], storages: [local] })).toStrictEqual({
      workerMachine: "No node has 878.9 GiB of memory. Lower the machine's memory.",
    });
    const disks = { ...sized, workerMachine: { cores: 4, memoryMiB: 4096, diskGiB: 9000 } };
    expect(
      capacityProblems("Machines", disks, { nodes: [node("pve1", 20)], storages: [{ ...local, type: "lvm" }] }),
    ).toStrictEqual({
      workerMachine: "The disks need 8.8 TiB, 610.0 GiB free. Lower a disk size or the node count.",
    });
  });

  it("blocks disks only on thick storage, and warns on thin or unknown storage", () => {
    const disks = { ...sized, workerMachine: { cores: 4, memoryMiB: 4096, diskGiB: 9000 } };
    const on = (s: Storage) => capacityProblems("Machines", disks, { nodes: [node("pve1", 20)], storages: [s] });
    expect(on({ ...local, type: "lvm" })).toHaveProperty("workerMachine");
    expect(on({ ...local, type: "zfspool", sparse: false })).toHaveProperty("workerMachine");
    for (const s of [{ type: "lvmthin" }, { type: "zfspool", sparse: true }, { type: "rbd" }, { type: "nfs" }, {}]) {
      expect(on({ ...local, ...s })).toStrictEqual({});
    }
  });

  it("only warns, elsewhere and for memory in use, since the cluster waits for room", () => {
    expect(capacityProblems("Machines", sized, { nodes: [node("pve1", 2)], storages: [local] })).toStrictEqual({});
    expect(
      capacityProblems(
        "Network",
        { ...sized, workerMachine: { cores: 4, memoryMiB: 900_000, diskGiB: 30 } },
        { nodes: [node("pve1", 2)], storages: [local] },
      ),
    ).toStrictEqual({});
  });
});
