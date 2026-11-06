import { describe, expect, it } from "vitest";

import { timeoutsText } from "./NodeTimeouts";
import { planRows } from "./Staging";
import type { Cluster, ClusterClass } from "./types";

const cls: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  variables: [],
  versionFormat: { pattern: "" },
  controlPlaneTimeouts: {
    nodeDrainTimeoutSeconds: 3600,
    nodeVolumeDetachTimeoutSeconds: 600,
    nodeDeletionTimeoutSeconds: 60,
  },
  workerTimeouts: {
    nodeDrainTimeoutSeconds: 3600,
    nodeVolumeDetachTimeoutSeconds: 600,
    nodeDeletionTimeoutSeconds: 60,
  },
};

const before: Cluster = {
  namespace: "clusters",
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
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: {},
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

describe("the plan's timeouts", () => {
  it("say each in words, with no limit for 0 and the class's marked", () => {
    expect(
      timeoutsText({
        nodeDrainTimeoutSeconds: 1200,
        nodeVolumeDetachTimeoutSeconds: 0,
        nodeDeletionTimeoutSeconds: 30,
      }),
    ).toBe("Drain 20 min, volume detach no limit, node removal 30 s");
    expect(timeoutsText({ nodeDrainTimeoutSeconds: 1200 }, cls.workerTimeouts)).toBe(
      "Drain 20 min, volume detach 10 min (class default), node removal 60 s (class default)",
    );
    expect(timeoutsText({})).toBe("Drain class default, volume detach class default, node removal class default");
    expect(timeoutsText({ nodeDrainTimeoutSeconds: 1200 }, cls.workerTimeouts)).not.toContain(";");
  });

  it("make one row per target, each with its own values", () => {
    const after: Cluster = {
      ...before,
      controlPlaneTimeouts: { nodeDrainTimeoutSeconds: 1800 },
      workerPools: [{ name: "md-0", class: "worker", replicas: 1, timeouts: { nodeVolumeDetachTimeoutSeconds: 0 } }],
    };
    const rows = planRows(
      { before, after },
      [
        { setting: "controlPlaneTimeouts", effect: "none" },
        { setting: "pools", effect: "none" },
      ],
      cls,
    );
    expect(rows).toStrictEqual([
      {
        label: "Control plane timeouts",
        now: "Drain 60 min (class default), volume detach 10 min (class default), node removal 60 s (class default)",
        after: "Drain 30 min, volume detach 10 min (class default), node removal 60 s (class default)",
        effect: "none",
      },
      {
        label: "Pool md-0 timeouts",
        now: "Drain 60 min (class default), volume detach 10 min (class default), node removal 60 s (class default)",
        after: "Drain 60 min (class default), volume detach no limit, node removal 60 s (class default)",
        effect: "none",
      },
    ]);
  });
});
