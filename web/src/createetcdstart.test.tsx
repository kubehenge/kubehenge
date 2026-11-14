import { describe, expect, it } from "vitest";

import { newForm, request, withBackupChoices } from "./create-form";
import { fromCluster } from "./start-from";
import type { Cluster, ClusterClass } from "./types";

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+$` },
  annotations: {
    "kubehenge.dev/backup-targets": JSON.stringify([
      { name: "store", endpoint: "https://s3.example.org:8333", bucket: "backups" },
      { name: "aws", bucket: "aws-backups" },
    ]),
  },
  variables: ["etcdSnapshots", "backups"].map((name) => ({ name, required: false, schema: { type: "object" } })),
};

describe("a new cluster started from another", () => {
  const source: Cluster = {
    namespace: "default",
    name: "web",
    class: "kubehenge-rke2",
    distribution: "rke2",
    version: "v1.36.5",
    generation: 1,
    created: "2026-10-01T00:00:00Z",
    deleting: false,
    phase: "Provisioned",
    available: true,
    conditions: [],
    controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
    workers: { desired: 2, ready: 2, available: 2, upToDate: 2 },
    workerPools: [{ name: "md-0", class: "worker", replicas: 2 }],
    variables: {},
    pool: "/pool/kubehenge/default.web",
    allowed: { change: true, kubeconfig: true },
  };

  it("takes the other's etcd snapshot settings", () => {
    const started = fromCluster(
      {
        ...source,
        variables: {
          etcdSnapshots: {
            enabled: true,
            target: "aws",
            schedule: "0 */6 * * *",
            timeZone: "Europe/Zurich",
            keep: 5,
            distributionUpload: true,
          },
        },
      },
      newForm(),
    );
    expect(started).toMatchObject({ etcdSnapshots: true });
    expect(started.etcdFields).toStrictEqual({
      target: "aws",
      schedule: "0 */6 * * *",
      timeZone: "Europe/Zurich",
      keep: "5",
      upload: true,
    });
    expect(request(withBackupChoices(started, rke2, true), rke2).variables["etcdSnapshots"]).toMatchObject({
      target: "aws",
      keep: 5,
      distributionUpload: true,
    });
  });

  it("takes that they are off, and leaves the default where the other never set them", () => {
    const off = fromCluster({ ...source, variables: { etcdSnapshots: { enabled: false } } }, newForm());
    expect(off).toMatchObject({ etcdSnapshots: false });
    expect(fromCluster(source, newForm())).toMatchObject({
      etcdSnapshots: true,
      etcdFields: newForm().etcdFields,
    });
  });
});
