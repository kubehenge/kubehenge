import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { poolChange, poolEntry, poolProblems, roomFor, roomWarning, scaledCluster } from "./Autoscaling";
import { applyStaged, fakeServer } from "./fake-server";
import type { Cluster, PveNode, WorkerPool } from "./types";

const gib = 1024 * 1024 * 1024;

const fixedPool: WorkerPool = { name: "md-0", class: "worker", replicas: 2 };
const scaledPool: WorkerPool = { name: "md-0", class: "worker", replicas: 3, autoscaling: { min: 1, max: 4 } };

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
  workerPools: [fixedPool],
  variables: {
    // Eight node addresses: one control plane and one for a rollout leave six for the workers.
    network: { addresses: ["10.0.0.21-10.0.0.28"], prefix: 24, gateway: "10.0.0.1" },
    workerMachine: { cores: 2, memoryMiB: 8192, diskGiB: 40 },
  },
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const autoscaled: Cluster = { ...web, workerPools: [scaledPool] };

// mostRaised is a change of c's autoscaled pool to max workers at most, as the Scale dialog makes it.
const mostRaised = (c: Cluster, max: string): { before: Cluster; after: Cluster } => ({
  before: c,
  after: scaledCluster(c, 1, { "md-0": { ...poolEntry(scaledPool), max } }),
});

const signedIn = { "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }) };

describe("the Scale dialog", () => {
  it("switches a pool to the autoscaler", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const nodes: PveNode[] = [{ node: "pve1", online: true, memory: 60 * gib, maxMemory: 64 * gib }];
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": { body: web },
      "GET /api/proxmox/nodes": { body: nodes },
      "PATCH /api/clusters/default/web": { body: autoscaled },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Scale" }));
    const dialog = screen.getByRole("dialog", { name: "Scale web" });
    await user.selectOptions(within(dialog).getByLabelText("Scaling"), "autoscaled");
    const most = within(dialog).getByLabelText("Most workers");
    expect(most).toHaveValue(4);
    expect(most).toHaveAccessibleDescription("Node addresses leave room for 6");
    // Two more workers of 8 GiB, and the node has 4 GiB free.
    await within(dialog).findByText(/^2 more workers need 16\.0 GiB, the nodes have/u);
    await user.clear(most);
    await user.type(most, "7");
    expect(most).toHaveAccessibleDescription(/Needs 9 node addresses, but there are 8/u);
    expect(within(dialog).getByRole("button", { name: "Add to plan" })).toBeDisabled();
    await user.clear(most);
    await user.type(most, "4");
    const fewest = within(dialog).getByLabelText("Fewest workers");
    await user.clear(fewest);
    await user.type(fewest, "1");
    await user.click(within(dialog).getByRole("button", { name: "Add to plan" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(calls.findLast((c) => c.key === "PATCH /api/clusters/default/web")?.body).toStrictEqual({
        autoscaling: { "md-0": { min: 1, max: 4 } },
      }),
    );
  });

  it("shows the addresses and memory the new count needs, as Review plan does", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const nodes: PveNode[] = [{ node: "pve1", online: true, memory: 60 * gib, maxMemory: 64 * gib }];
    fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": { body: web },
      "GET /api/proxmox/nodes": { body: nodes },
      "PATCH /api/clusters/default/web": { body: web },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Scale" }));
    const dialog = within(screen.getByRole("dialog", { name: "Scale web" }));
    const workers = dialog.getByLabelText("Workers");
    await user.clear(workers);
    await user.type(workers, "4");
    // Eight addresses, a control plane and four workers leave three. Two more workers of 8 GiB need 16 GiB of 4 free.
    await expect(dialog.findByText("3 spare")).resolves.toHaveClass("status-ok");
    expect(dialog.getByText(/^Needs 16\.0 GiB/u)).toHaveClass("status-warning");
  });

  it("switches it off at the workers it runs", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": { body: autoscaled },
      "PATCH /api/clusters/default/web": { body: web },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const term = await screen.findByText("Worker pools", { selector: "dt" });
    expect(term.nextElementSibling).toHaveTextContent("Workers: 3 of 1 to 4, autoscaled");
    await user.click(screen.getByRole("button", { name: "Scale" }));
    const dialog = screen.getByRole("dialog", { name: "Scale web" });
    await user.selectOptions(within(dialog).getByLabelText("Scaling"), "fixed");
    const workers = within(dialog).getByLabelText("Workers");
    expect(workers).toHaveValue(3);
    expect(workers).toHaveAccessibleDescription("Keeps its current workers unless changed");
    await user.click(within(dialog).getByRole("button", { name: "Add to plan" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(calls.findLast((c) => c.key === "PATCH /api/clusters/default/web")?.body).toStrictEqual({
        autoscaling: { "md-0": null },
      }),
    );
  });
});

describe("the pools' checks", () => {
  it("keeps a worker under the add-ons", () => {
    const withAddons: Cluster = { ...autoscaled, variables: { ...autoscaled.variables, headlamp: { enabled: true } } };
    const kept = { "md-0": poolEntry(scaledPool) };
    expect(poolProblems({ c: withAddons, controlPlane: 1, entries: kept })).toStrictEqual({});
    const none = { "md-0": { ...poolEntry(scaledPool), min: "0" } };
    expect(poolProblems({ c: withAddons, controlPlane: 1, entries: none })).toStrictEqual({
      "md-0": { least: "Keep at least 1 for the add-ons" },
    });
    expect(poolProblems({ c: autoscaled, controlPlane: 1, entries: none })).toStrictEqual({});
    for (const variables of [
      { backups: { enabled: true } },
      { certificates: { enabled: true } },
      { gitops: { enabled: true } },
    ]) {
      const c: Cluster = { ...autoscaled, variables: { ...autoscaled.variables, ...variables } };
      expect(poolProblems({ c, controlPlane: 1, entries: none })).toStrictEqual({
        "md-0": { least: "Keep at least 1 for the add-ons" },
      });
    }
  });

  it("holds the most to the fewest and to whole numbers", () => {
    for (const [min, max] of [
      ["3", "2"],
      ["0", "0"],
      ["1", "101"],
      ["1", "x"],
    ] as const) {
      const entries = { "md-0": { ...poolEntry(scaledPool), min, max } };
      expect(poolProblems({ c: autoscaled, controlPlane: 1, entries })).toHaveProperty(["md-0", "most"]);
    }
  });

  it("lets a shortage the cluster has stand but not grow", () => {
    const network = { addresses: ["10.0.0.21-10.0.0.23"], prefix: 24, gateway: "10.0.0.1" };
    const short: Cluster = { ...autoscaled, variables: { ...autoscaled.variables, network } };
    const entries = { "md-0": poolEntry(scaledPool) };
    expect(poolProblems({ c: short, controlPlane: 1, entries })).toStrictEqual({});
    expect(roomFor({ c: short, controlPlane: 1, entries })).toStrictEqual({ "md-0": 1 });
    const more = { "md-0": { ...poolEntry(scaledPool), max: "5" } };
    expect(poolProblems({ c: short, controlPlane: 1, entries: more })).toHaveProperty(["md-0", "most"]);
  });

  it("sends only what differs", () => {
    expect(poolChange([scaledPool], { "md-0": poolEntry(scaledPool) })).toStrictEqual({});
    expect(
      poolChange([scaledPool], { "md-0": { ...poolEntry(scaledPool), autoscale: false, replicas: "2" } }),
    ).toStrictEqual({ workerPools: { "md-0": 2 }, autoscaling: { "md-0": null } });
    expect(poolChange([fixedPool], { "md-0": { ...poolEntry(fixedPool), replicas: "5" } })).toStrictEqual({
      workerPools: { "md-0": 5 },
    });
  });

  it("warns only when the nodes the cluster may use lack the memory", () => {
    const roomy: PveNode[] = [{ node: "pve1", online: true, memory: 0, maxMemory: 64 * gib }];
    const tight: PveNode[] = [
      { node: "pve1", online: true, memory: 60 * gib, maxMemory: 64 * gib },
      { node: "pve2", online: true, memory: 0, maxMemory: 256 * gib },
    ];
    expect(roomWarning(mostRaised(autoscaled, "5"), "md-0", roomy)).toBeUndefined();
    expect(roomWarning(mostRaised(autoscaled, "5"), "md-0", tight)).toBeUndefined();
    const kept: Cluster = { ...autoscaled, variables: { ...autoscaled.variables, allowedNodes: ["pve1"] } };
    // The most goes from 4 to 5 and to 6: one and two more workers of 8 GiB, on 4 GiB free.
    expect(roomWarning(mostRaised(kept, "5"), "md-0", tight)).toMatch(/^1 more worker needs 8\.0 GiB/u);
    expect(roomWarning(mostRaised(kept, "6"), "md-0", tight)).toMatch(/^2 more workers need 16\.0 GiB/u);
    expect(roomWarning(mostRaised(kept, "4"), "md-0", tight)).toBeUndefined();
    expect(roomWarning(mostRaised(kept, "5"), "md-0", [])).toBeUndefined();
  });
});

describe("the autoscaler's options", () => {
  it("tunes the autoscaler and moves it into the cluster", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/options");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": { body: autoscaled },
      "PATCH /api/clusters/default/web": { body: autoscaled },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByRole("row", { name: /Autoscaler/u })).resolves.toHaveTextContent(
      "In the management cluster, 1 pool, scales down after 10 min",
    );
    await user.click(screen.getByRole("button", { name: "Edit the autoscaler" }));
    const dialog = screen.getByRole("dialog", { name: "Edit autoscaler" });
    expect(within(dialog).getByRole("button", { name: "Add to plan" })).toBeDisabled();
    const runs = within(dialog).getByLabelText("Runs");
    await user.selectOptions(runs, "workload");
    expect(runs).toHaveAccessibleDescription("Its token reads the machines of the namespace's clusters");
    const unneeded = within(dialog).getByLabelText("Unneeded for (minutes)");
    await user.clear(unneeded);
    expect(within(dialog).getByRole("button", { name: "Add to plan" })).toBeDisabled();
    await user.type(unneeded, "5");
    await user.click(within(dialog).getByRole("button", { name: "Add to plan" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(calls.findLast((c) => c.key === "PATCH /api/clusters/default/web")?.body).toStrictEqual({
        variables: {
          autoscaler: {
            placement: "workload",
            expander: "least-waste",
            scaleDownDelayAfterAddMinutes: 10,
            scaleDownUnneededMinutes: 5,
          },
        },
      }),
    );
  });
});
