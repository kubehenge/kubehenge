import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer, held } from "./fake-server";
import { changeLines } from "./plan-rows";
import type { Cluster, Machine } from "./types";

const gib = 1_073_741_824;

const cluster: Cluster = {
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

const path = "/api/clusters/clusters/web";

const session = { "GET /api/session": { body: { user: "dev@pve", csrf: "c" } } };

const pools = (...list: [string, number][]): Cluster => ({
  ...cluster,
  workerPools: list.map(([name, replicas]) => ({ name, class: "worker", replicas })),
});

const text = async (found: Promise<HTMLElement>): Promise<string | null> => {
  const element = await found;
  return element.textContent;
};

const controlPlane = (name: string, on: string): Machine => ({
  name,
  node: name,
  role: "control-plane",
  phase: "Running",
  ready: true,
  created: cluster.created,
  proxmoxNode: on,
});

describe("review plan", () => {
  it("keeps Apply off until every check has answered", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const grown: Cluster = { ...cluster, workerPools: [{ name: "md-0", class: "worker", replicas: 2 }] };
    const health = held();
    const calls = fakeServer({
      ...session,
      [`GET ${path}`]: { body: cluster },
      [`GET ${path}/health`]: health.route,
      [`POST ${path}/plan`]: { body: { cluster: grown, changes: [{ setting: "workerPools", effect: "scales" }] } },
      [`PATCH ${path}`]: { body: grown },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Scale" }));
    const dialog = within(screen.getByRole("dialog", { name: "Scale web" }));
    await user.clear(dialog.getByLabelText("Workers"));
    await user.type(dialog.getByLabelText("Workers"), "2");
    await user.click(dialog.getByRole("button", { name: "Add to plan" }));
    await user.click(await screen.findByRole("button", { name: "Review plan" }));
    const plan = within(screen.getByRole("dialog", { name: "Review plan" }));
    expect(plan.getAllByText("Checking")).not.toHaveLength(0);
    expect(plan.getByRole("button", { name: "Apply" })).toBeDisabled();
    await user.click(plan.getByRole("button", { name: "Apply" }));
    health.answer({
      body: {
        verdict: "degraded",
        attention: [{ severity: "error", section: "nodes", what: "Node web-md-0-a not ready" }],
        nodes: { items: [] },
      },
    });
    // The degraded answer asks for the risks to be accepted, and nothing went out before it.
    await expect(plan.findByRole("checkbox", { name: "Accept the risks" })).resolves.toBeInTheDocument();
    expect(plan.getByRole("button", { name: "Apply" })).toBeDisabled();
    expect(calls.map((c) => c.key).filter((k) => k.startsWith("PATCH"))).toStrictEqual([]);
  });
});

describe("the Scale dialog and Review plan", () => {
  it("count an autoscaled pool at its new most for the addresses", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const autoscaled: Cluster = {
      ...cluster,
      workerPools: [{ name: "md-0", class: "worker", replicas: 1, autoscaling: { min: 1, max: 3 } }],
      variables: { network: { addresses: ["10.0.0.21-10.0.0.40"], prefix: 24, gateway: "10.0.0.1" } },
    };
    const raised: Cluster = {
      ...autoscaled,
      workerPools: [{ name: "md-0", class: "worker", replicas: 1, autoscaling: { min: 1, max: 10 } }],
    };
    fakeServer({
      ...session,
      [`GET ${path}`]: { body: autoscaled },
      [`POST ${path}/plan`]: { body: { cluster: raised, changes: [{ setting: "autoscaling", effect: "scales" }] } },
      [`PATCH ${path}`]: { body: raised },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Scale" }));
    const dialog = within(screen.getByRole("dialog", { name: "Scale web" }));
    await user.clear(dialog.getByLabelText("Most workers"));
    await user.type(dialog.getByLabelText("Most workers"), "10");
    const inDialog = await text(dialog.findByText(/spare$/u));
    await user.click(dialog.getByRole("button", { name: "Add to plan" }));
    await user.click(await screen.findByRole("button", { name: "Review plan" }));
    const plan = within(screen.getByRole("dialog", { name: "Review plan" }));
    const inPlan = await text(plan.findByText(/spare$/u));
    expect({ inDialog, inPlan }).toStrictEqual({ inDialog: "9 spare", inPlan: "9 spare" });
  });

  it("count no workers for the memory of a control-plane change alone", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const autoscaled: Cluster = {
      ...cluster,
      workerPools: [{ name: "md-0", class: "worker", replicas: 1, autoscaling: { min: 1, max: 10 } }],
      variables: {
        controlPlaneMachine: { cores: 2, memoryMiB: 4096, diskGiB: 30 },
        workerMachine: { cores: 4, memoryMiB: 8192, diskGiB: 50 },
      },
    };
    const three: Cluster = { ...autoscaled, controlPlane: { ...autoscaled.controlPlane, desired: 3 } };
    fakeServer({
      ...session,
      [`GET ${path}`]: { body: autoscaled },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true, memory: 0, maxMemory: 40 * gib }] },
      [`POST ${path}/plan`]: {
        body: { cluster: three, changes: [{ setting: "controlPlaneReplicas", effect: "scales" }] },
      },
      [`PATCH ${path}`]: { body: three },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Scale" }));
    const dialog = within(screen.getByRole("dialog", { name: "Scale web" }));
    await user.selectOptions(dialog.getByLabelText("Control-plane nodes"), "3");
    const inDialog = await text(within(dialog.getByLabelText("Before scaling")).findByText(/free$/u));
    await user.click(dialog.getByRole("button", { name: "Add to plan" }));
    await user.click(await screen.findByRole("button", { name: "Review plan" }));
    const plan = within(screen.getByRole("dialog", { name: "Review plan" }));
    const inPlan = await text(within(plan.getByLabelText("Before applying")).findByText(/free$/u));
    expect({ inDialog, inPlan }).toStrictEqual({ inDialog: "40.0 GiB free", inPlan: "40.0 GiB free" });
  });
});

describe("the staged bar", () => {
  it("says a deleted pool is deleted", () => {
    const before = pools(["md-0", 1], ["gpu", 2]);
    const after = pools(["md-0", 1]);
    expect(changeLines({ before, after }, [{ setting: "workerPools", effect: "scales" }])).toStrictEqual([
      "gpu: deleted",
    ]);
    expect(changeLines({ before, after }, [{ setting: "pools", effect: "scales" }])).toStrictEqual(["gpu: deleted"]);
  });

  it("keeps the default pool apart from a user's pool named workers", () => {
    const before = pools(["md-0", 1], ["workers", 2]);
    const after = pools(["md-0", 2], ["workers", 3]);
    expect(changeLines({ before, after }, [{ setting: "workerPools", effect: "scales" }])).toStrictEqual([
      "md-0: 1 to 2",
      "workers: 2 to 3",
    ]);
    expect(
      changeLines({ before: pools(["md-0", 1], ["gpu", 2]), after: pools(["md-0", 2], ["gpu", 2]) }, [
        { setting: "workerPools", effect: "scales" },
      ]),
    ).toStrictEqual(["Workers: 1 to 2"]);
  });
});

describe("a Proxmox VE node the appliance cannot reach", () => {
  it("is up, with its machines ready and counted, and a warning in Review plan", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const placed: Cluster = {
      ...cluster,
      controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
      machines: [controlPlane("a", "pve1"), controlPlane("b", "pve2"), controlPlane("c", "pve3")],
    };
    const grown: Cluster = { ...placed, workerPools: [{ name: "md-0", class: "worker", replicas: 2 }] };
    fakeServer({
      ...session,
      [`GET ${path}`]: { body: placed },
      "GET /api/clusters": { body: [placed] },
      "GET /api/proxmox/nodes": {
        body: [
          { node: "pve1", online: true },
          { node: "pve2", online: true },
          { node: "pve3", online: false, fault: "HTTP 596: certificate verify failed" },
        ],
      },
      [`POST ${path}/plan`]: { body: { cluster: grown, changes: [{ setting: "workerPools", effect: "scales" }] } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const map = await screen.findByRole("region", { name: "Placement" });
    await expect(within(map).findByText("Not reachable, 1 control plane")).resolves.toBeInTheDocument();
    expect(within(map).getByRole("link", { name: "c, control plane, Ready" })).toBeInTheDocument();
    const term = screen.getByText("Control plane", { selector: "dt" });
    expect(term.nextElementSibling).toHaveTextContent("3 of 3 ready");

    await user.click(screen.getByRole("button", { name: "Scale" }));
    const dialog = within(screen.getByRole("dialog", { name: "Scale web" }));
    await user.clear(dialog.getByLabelText("Workers"));
    await user.type(dialog.getByLabelText("Workers"), "2");
    await user.click(dialog.getByRole("button", { name: "Add to plan" }));
    await user.click(await screen.findByRole("button", { name: "Review plan" }));
    const checks = within(
      within(screen.getByRole("dialog", { name: "Review plan" })).getByLabelText("Before applying"),
    );
    await expect(checks.findByText("pve3 not reachable")).resolves.toBeInTheDocument();
    await waitFor(() => expect(checks.queryByText(/offline/u)).toBeNull());
  });
});
