import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { poolChange, poolEntry, poolProblems, roomFor, roomWarning } from "./Autoscaling";
import { fakeServer } from "./fake-server";
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
    await user.selectOptions(within(dialog).getByLabelText("Scaling (md-0)"), "autoscaled");
    const most = within(dialog).getByLabelText("Most workers (md-0)");
    expect(most).toHaveValue(4);
    expect(most).toHaveAccessibleDescription("The node addresses leave room for 6 here.");
    // Two more workers of 8 GiB, and the node has 4 GiB free.
    await within(dialog).findByText(/^The 2 more workers it may add need 16\.0 GiB of memory/u);
    await user.clear(most);
    await user.type(most, "7");
    expect(most).toHaveAccessibleDescription(/The node addresses give 8; .* runs 9 machines at once/u);
    expect(within(dialog).getByRole("button", { name: "Scale" })).toBeDisabled();
    await user.clear(most);
    await user.type(most, "4");
    const fewest = within(dialog).getByLabelText("Fewest workers (md-0)");
    await user.clear(fewest);
    await user.type(fewest, "1");
    await user.click(within(dialog).getByRole("button", { name: "Scale" }));
    await waitFor(() =>
      expect(calls.findLast((c) => c.key === "PATCH /api/clusters/default/web")?.body).toStrictEqual({
        controlPlaneReplicas: 1,
        autoscaling: { "md-0": { min: 1, max: 4 } },
      }),
    );
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
    expect(term.nextElementSibling).toHaveTextContent("md-0: 3 of 1 to 4, autoscaled");
    await user.click(screen.getByRole("button", { name: "Scale" }));
    const dialog = screen.getByRole("dialog", { name: "Scale web" });
    await user.selectOptions(within(dialog).getByLabelText("Scaling (md-0)"), "fixed");
    const workers = within(dialog).getByLabelText("Workers (md-0)");
    expect(workers).toHaveValue(3);
    expect(workers).toHaveAccessibleDescription(/keeps the workers it runs now/u);
    await user.click(within(dialog).getByRole("button", { name: "Scale" }));
    await waitFor(() =>
      expect(calls.findLast((c) => c.key === "PATCH /api/clusters/default/web")?.body).toStrictEqual({
        controlPlaneReplicas: 1,
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
      "md-0": { least: "The cluster's add-ons need a worker that stays: at least 1." },
    });
    expect(poolProblems({ c: autoscaled, controlPlane: 1, entries: none })).toStrictEqual({});
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
    const e = { ...poolEntry(scaledPool), max: "5" };
    const roomy: PveNode[] = [{ node: "pve1", online: true, memory: 0, maxMemory: 64 * gib }];
    const tight: PveNode[] = [
      { node: "pve1", online: true, memory: 60 * gib, maxMemory: 64 * gib },
      { node: "pve2", online: true, memory: 0, maxMemory: 256 * gib },
    ];
    expect(roomWarning(autoscaled, { p: scaledPool, e }, roomy)).toBeUndefined();
    expect(roomWarning(autoscaled, { p: scaledPool, e }, tight)).toBeUndefined();
    const kept: Cluster = { ...autoscaled, variables: { ...autoscaled.variables, allowedNodes: ["pve1"] } };
    expect(roomWarning(kept, { p: scaledPool, e }, tight)).toMatch(/^The 2 more workers it may add need 16\.0 GiB/u);
    expect(roomWarning(kept, { p: scaledPool, e }, [])).toBeUndefined();
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
      "In the management cluster, 1 pool autoscaled; removes a node unneeded for 10 min",
    );
    await user.click(screen.getByRole("button", { name: "Edit the autoscaler" }));
    const dialog = screen.getByRole("dialog", { name: "Autoscaler" });
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    const runs = within(dialog).getByLabelText("Runs");
    await user.selectOptions(runs, "workload");
    expect(runs).toHaveAccessibleDescription(/needs the management cluster's API reachable/u);
    const unneeded = within(dialog).getByLabelText("Unneeded for (minutes)");
    await user.clear(unneeded);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.type(unneeded, "5");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
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
