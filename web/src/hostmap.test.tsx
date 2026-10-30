import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer } from "./fake-server";
import type { Cluster, Machine } from "./types";

const created = new Date(Date.now() - 3_600_000).toISOString();

const machine = (name: string, role: Machine["role"], on: { proxmoxNode?: string; ready?: boolean } = {}): Machine => ({
  name,
  node: name,
  role,
  phase: on.ready === false ? "Provisioning" : "Running",
  ready: on.ready !== false,
  created,
  ...on,
});

const web: Cluster = {
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
  controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
  workers: { desired: 2, ready: 1, available: 1, upToDate: 2 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 2 }],
  variables: {},
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
  machines: [
    machine("web-cp-a", "control-plane", { proxmoxNode: "pve1" }),
    machine("web-w-1", "worker", { proxmoxNode: "pve2" }),
    machine("web-cp-b", "control-plane", { proxmoxNode: "pve1" }),
    machine("web-cp-c", "control-plane", { proxmoxNode: "pve2" }),
    machine("web-w-2", "worker", { ready: false }),
  ],
};

describe("the cluster's summary", () => {
  it("lays its machines out by Proxmox VE node, the empty nodes after them", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({
      "GET /api/session": { body: { user: "alice@pve", csrf: "c1" } },
      "GET /api/clusters": { body: [web] },
      "GET /api/clusters/default/web": { body: web },
      "GET /api/runs": { body: [] },
      "GET /api/proxmox/nodes": {
        body: [
          { node: "pve3", online: true },
          { node: "pve2", online: true },
          { node: "pve1", online: true },
          { node: "pve4", online: false },
        ],
      },
    });
    render(<App />);
    const map = await screen.findByRole("region", { name: "Placement" });
    await within(map).findByText("pve3");
    const tiles = [...within(map).getByRole("list", { name: "Proxmox VE nodes" }).children];
    expect(tiles.map((t) => t.textContent)).toStrictEqual([
      expect.stringMatching(/^pve12 control planes/u),
      expect.stringMatching(/^pve21 control plane, 1 worker/u),
      "pve3Empty",
      "pve4Offline",
      expect.stringMatching(/^Hidden node1 worker/u),
    ]);
    // Control planes first on each node, each machine named with its role and state.
    expect(
      within(map)
        .getAllByRole("link")
        .map((l) => l.getAttribute("aria-label")),
    ).toStrictEqual([
      "web-cp-a, control plane, Ready",
      "web-cp-b, control plane, Ready",
      "web-cp-c, control plane, Ready",
      "web-w-1, worker, Ready",
      "web-w-2, worker, Not ready",
    ]);
    expect(within(map).getByText(/pve1 runs 2 of 3 control-plane VMs/u)).toBeInTheDocument();
  });
});
