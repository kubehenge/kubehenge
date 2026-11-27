import { act } from "@testing-library/react";

import type { Route } from "./fake-server";
import type { Cluster } from "./types";

/** Fixtures for the tests of what a failed or pending read shows. */
const hourMs = 3_600_000;

export const created = new Date(Date.now() - hourMs).toISOString();

export const web: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  generation: 1,
  created,
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [{ type: "Available", status: "True", since: created }],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { etcdSnapshots: { enabled: true, target: "store", schedule: "0 */6 * * *", timeZone: "UTC", keep: 10 } },
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

export const unreadable = { status: 502, body: { error: "the management cluster did not answer" } };

export const signedIn: Record<string, Route> = {
  "GET /api/session": { body: { user: "alice@pve", csrf: "c1" } },
  "GET /api/clusters": { body: [web] },
  "GET /api/clusters/default/web": { body: web },
  "GET /api/runs": { body: [] },
  "GET /api/fleet/health": { body: [] },
};

/** healthy is a cluster's health as the fleet reads it, found well. */
export const healthy = (c: Cluster): Record<string, unknown> => ({
  namespace: c.namespace,
  name: c.name,
  read: created,
  verdict: "healthy",
  attention: [],
});

/** readAgain makes every poll read again, as a tab does that is shown again. */
export async function readAgain(): Promise<void> {
  await act(async () => {
    document.dispatchEvent(new Event("visibilitychange"));
    await Promise.resolve();
  });
}
