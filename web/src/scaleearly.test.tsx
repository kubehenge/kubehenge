import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer, held } from "./fake-server";
import type { Cluster } from "./types";

const created = new Date(Date.now() - 3_600_000).toISOString();

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
  workers: { desired: 2, ready: 2, available: 2, upToDate: 2 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 2 }],
  variables: {},
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
};

describe("the Scale button before the cluster has loaded", () => {
  it("opens the dialog on the cluster the list has, and keeps it open when the cluster loads", async () => {
    const detail = held();
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({
      "GET /api/session": { body: { user: "alice@pve", csrf: "c1" } },
      "GET /api/clusters": { body: [web] },
      "GET /api/clusters/default/web": detail.route,
      "GET /api/runs": { body: [] },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Scale" }));
    await expect(screen.findByRole("dialog", { name: "Scale web" })).resolves.toBeInTheDocument();
    detail.answer({ body: { ...web, generation: 2 } });
    await waitFor(() => expect(screen.getByRole("dialog", { name: "Scale web" })).toBeInTheDocument());
  });
});
