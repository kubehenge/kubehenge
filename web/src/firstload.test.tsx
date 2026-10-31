import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { api, primeFirstReads } from "./api";
import { App } from "./App";
import { fakeServer, held, reads } from "./fake-server";
import type { Cluster } from "./types";

const web: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-kubeadm",
  distribution: "kubeadm",
  version: "v1.36.1",
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [],
  variables: {},
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: true },
};

const session = { user: "root@pam", namespace: "default", canCreate: true, csrf: "c1" };

describe("the first load", () => {
  it("asks for the session, the setup status and the fleet together", async () => {
    const answer = held();
    const calls = fakeServer({
      "GET /api/session": answer.route,
      "GET /api/setup": { body: { appliance: false, ready: true, allowed: false } },
      "GET /api/clusters": { body: [web] },
      "GET /api/fleet/health": { body: [] },
      "GET /api/runs": { body: [] },
    });
    primeFirstReads();
    render(<App />);
    expect(calls.map((c) => c.key)).toStrictEqual([
      "GET /api/session",
      "GET /api/setup",
      "GET /api/clusters",
      "GET /api/fleet/health",
      "GET /api/runs",
    ]);

    answer.answer({ body: session });
    await expect(screen.findByRole("link", { name: "web" })).resolves.toBeInTheDocument();
    expect(reads(calls, "GET /api/clusters")).toBe(1);
    expect(reads(calls, "GET /api/setup")).toBe(1);
  });

  it("asks again for what was read before signing in", async () => {
    const calls = fakeServer({
      "POST /api/session": { body: session },
      "GET /api/clusters": { status: 401, body: { error: "signed out" } },
    });
    primeFirstReads();
    await api.signIn("root", "pam", "secret");
    await expect(api.clusters()).rejects.toThrow("signed out");
    expect(reads(calls, "GET /api/clusters")).toBe(2);
  });

  it("asks again once what was read early is out of date", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const calls = fakeServer({ "GET /api/runs": { body: [] } });
    primeFirstReads();
    vi.setSystemTime(Date.now() + 60_000);
    await api.runs();
    expect(reads(calls, "GET /api/runs")).toBe(2);
  });
});
