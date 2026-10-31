import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { applyStaged, fakeServer, type Call } from "./fake-server";
import { poolNameProblem, sizeProblem } from "./Pools";
import type { Cluster } from "./types";

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
  variables: {
    controlPlaneMachine: { cores: 2, memoryMiB: 4096, diskGiB: 30 },
    workerMachine: { cores: 2, memoryMiB: 8192, diskGiB: 40 },
  },
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const classes = [
  {
    name: "kubehenge-k3s",
    distribution: "k3s",
    variables: [
      {
        name: "workerMachine",
        required: false,
        schema: {
          type: "object",
          properties: { cores: { minimum: 2 }, memoryMiB: { minimum: 4096 }, diskGiB: { minimum: 20 } },
        },
      },
    ],
  },
];

async function machinesView(): Promise<{ calls: Call[]; user: UserEvent }> {
  window.history.replaceState(null, "", "/clusters/default/web/machines");
  const calls = fakeServer({
    "GET /api/session": { body: { user: "alice@pve", csrf: "c1" } },
    "GET /api/clusters/default/web": { body: web },
    "GET /api/classes": { body: classes },
    "GET /api/proxmox/nodes": { body: [] },
    "PATCH /api/clusters/default/web": { body: web },
  });
  render(<App />);
  await screen.findByRole("region", { name: "Pools" });
  return { calls, user: userEvent.setup({ delay: null }) };
}

const sent = (calls: Call[]): unknown => calls.findLast((c) => c.key === "PATCH /api/clusters/default/web")?.body;

describe("the pools", () => {
  it("show each pool's nodes and size", async () => {
    await machinesView();
    const rows = within(screen.getByRole("region", { name: "Pools" })).getAllByRole("row");
    expect(rows.map((r) => r.textContent)).toStrictEqual([
      "PoolNodesCoresMemoryDiskActions",
      "Control plane124 GiB30 GiBResize…",
      "md-0228 GiB40 GiBResize…Delete…",
    ]);
  });

  it("add a pool of its own size", async () => {
    const { calls, user } = await machinesView();
    await user.click(screen.getByRole("button", { name: "Add pool…" }));
    const dialog = screen.getByRole("dialog", { name: "Add pool to web" });
    await user.type(within(dialog).getByLabelText("Name"), "gpu");
    const memory = within(dialog).getByLabelText("Memory (MiB)");
    await user.clear(memory);
    await user.type(memory, "2048");
    expect(memory).toHaveAccessibleDescription(/At least 4096 MiB of memory/u);
    expect(within(dialog).getByRole("button", { name: "Add" })).toBeDisabled();
    await user.clear(memory);
    await user.type(memory, "16384");
    await user.click(within(dialog).getByRole("button", { name: "Add" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(sent(calls)).toStrictEqual({
        pools: { gpu: { replicas: 1, machine: { cores: 2, memoryMiB: 16_384, diskGiB: 40 } } },
      }),
    );
  });

  it("resize the control plane", async () => {
    const { calls, user } = await machinesView();
    await user.click(screen.getByRole("button", { name: "Resize the control plane" }));
    const dialog = screen.getByRole("dialog", { name: "Resize web's control plane" });
    const cores = within(dialog).getByLabelText("Cores");
    await user.clear(cores);
    await user.type(cores, "4");
    await user.click(within(dialog).getByRole("button", { name: "Resize" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(sent(calls)).toStrictEqual({
        variables: { controlPlaneMachine: { cores: 4, memoryMiB: 4096, diskGiB: 30 } },
      }),
    );
  });

  it("delete a pool", async () => {
    const { calls, user } = await machinesView();
    await user.click(screen.getByRole("button", { name: "Delete pool md-0" }));
    await user.click(
      within(screen.getByRole("dialog", { name: "Delete pool md-0" })).getByRole("button", { name: "Delete" }),
    );
    await applyStaged(user);
    await waitFor(() => expect(sent(calls)).toStrictEqual({ pools: { "md-0": null } }));
  });

  it("refuse a name the server would", () => {
    expect(poolNameProblem(web, "GPU")).toBe("Lowercase letters, digits and dashes, from a letter");
    expect(poolNameProblem(web, "md-0")).toBe("A pool has this name");
    expect(poolNameProblem(web, "p".repeat(42))).toBe("At most 41 characters for this cluster");
    expect(poolNameProblem(web, "gpu-2")).toBeUndefined();
  });

  it("hold a size to the class's least", () => {
    const min = { cores: 2, memoryMiB: 4096, diskGiB: 20 };
    expect(sizeProblem({ cores: 1, memoryMiB: 4096, diskGiB: 20 }, min)).toBe("At least 2 cores");
    expect(sizeProblem({ cores: 2, memoryMiB: 4100, diskGiB: 20 }, min)).toBe(
      "At least 4096 MiB of memory, in steps of 8",
    );
    expect(sizeProblem({ cores: 2, memoryMiB: 4096, diskGiB: 10 }, min)).toBe("At least 20 GiB of disk");
    expect(sizeProblem(min, min)).toBeUndefined();
  });
});
