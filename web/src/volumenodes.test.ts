import { describe, expect, it } from "vitest";

import { newForm, volumeWorkersWarning } from "./create-form";
import type { Cluster, PveNode, Storage } from "./types";
import { scaleChecks, volumesCheck } from "./Upgrade";

const web: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-k3s",
  distribution: "k3s",
  version: "v1.36.5+k3s1",
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 2, ready: 2, available: 2, upToDate: 2 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 2 }],
  variables: {},
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

describe("the change checks and the nodes holding volumes", () => {
  it("warns when the workers would be fewer than the nodes with volumes", () => {
    const held: Cluster = { ...web, volumeNodes: ["pve1", "pve2", "pve3"] };
    expect(volumesCheck(held)).toStrictEqual({
      what: "Volumes",
      state: "warning",
      text: "On 3 nodes, 2 workers",
      detail: ["A pod starts only on its volume's node.", "Nodes with volumes: pve1, pve2, pve3"],
    });
    expect(scaleChecks(held, held, [])).toContainEqual(expect.objectContaining({ what: "Volumes", state: "warning" }));
  });

  it("counts an autoscaled pool at its fewest", () => {
    const scaled: Cluster = {
      ...web,
      volumeNodes: ["pve1", "pve2"],
      workerPools: [{ name: "md-0", class: "worker", replicas: 3, autoscaling: { min: 1, max: 4 } }],
    };
    expect(volumesCheck(scaled)?.state).toBe("warning");
  });

  it("passes with a worker for each node, and says nothing without volumes", () => {
    expect(volumesCheck({ ...web, volumeNodes: ["pve1", "pve2"] })).toStrictEqual({
      what: "Volumes",
      state: "ok",
      text: "On 2 nodes, 2 workers",
    });
    expect(volumesCheck(web)).toBeUndefined();
  });

  it("says nothing for a cluster without workers, whose control planes run its pods", () => {
    const controlPlanes: Cluster = { ...web, volumeNodes: ["pve1", "pve2"], workerPools: [] };
    expect(volumesCheck(controlPlanes)).toBeUndefined();
    const emptied: Cluster = { ...controlPlanes, workerPools: [{ name: "md-0", class: "worker", replicas: 0 }] };
    expect(scaleChecks(emptied, emptied, [])).not.toContainEqual(expect.objectContaining({ what: "Volumes" }));
  });
});

const node = (name: string): PveNode => ({ node: name, online: true });
const storage = (name: string, shared: boolean): Storage => ({
  storage: name,
  node: "pve1",
  shared,
  content: ["images"],
  active: true,
  avail: 1,
  total: 2,
  usable: true,
  allocatable: false,
});
const storages = [storage("ceph", true), storage("local-lvm", false)];
const nodes = [node("pve1"), node("pve2"), node("pve3")];
const warning = "Fewer workers than nodes can leave volumes without a worker.";
const note = "Volumes on node-local storage stay on one node.";

describe("the create wizard and node-local storage", () => {
  it("warns when the workers are fewer than the nodes the cluster may use", () => {
    expect(volumeWorkersWarning({ ...newForm(), storage: "local-lvm", workerReplicas: 2 }, nodes, storages)).toBe(
      warning,
    );
  });

  it("only notes node-local storage with a worker for each node or without workers, and says nothing on shared", () => {
    expect(volumeWorkersWarning({ ...newForm(), storage: "ceph", workerReplicas: 2 }, nodes, storages)).toBeUndefined();
    expect(volumeWorkersWarning({ ...newForm(), storage: "local-lvm", workerReplicas: 3 }, nodes, storages)).toBe(note);
    expect(volumeWorkersWarning({ ...newForm(), storage: "local-lvm", workerReplicas: 0 }, nodes, storages)).toBe(note);
  });

  it("counts only the nodes the cluster is kept to", () => {
    const kept = {
      ...newForm(),
      storage: "local-lvm",
      workerReplicas: 2,
      place: { placement: "nodes" as const, haRule: "", nodes: ["pve1", "pve2"] },
    };
    expect(volumeWorkersWarning(kept, nodes, storages)).toBe(note);
  });
});
