import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer, held, type Route } from "./fake-server";
import { created, healthy, readAgain, signedIn, unreadable, web } from "./readstates-fixtures";
import type { Cluster } from "./types";

const upgrading: Cluster = {
  ...web,
  name: "edge",
  conditions: [
    { type: "Available", status: "True", since: created },
    { type: "TopologyReconciled", status: "False", reason: "ClusterUpgrading", since: created },
  ],
};

const placed: Cluster = {
  ...web,
  machines: [{ name: "web-cp-1", role: "control-plane", phase: "Running", ready: true, proxmoxNode: "pve1", created }],
};

describe("what a page shows of a failed or pending read", () => {
  it("shows the runs panel's failed read with a retry, without a count", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer({ ...signedIn, "GET /api/runs": unreadable });
    render(<App />);
    const runs = within(await screen.findByRole("complementary", { name: "Runs" }));
    await expect(runs.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(runs.getByRole("button", { name: "Try now" })).toBeInTheDocument();
    expect(runs.queryByText(/running/u)).not.toBeInTheDocument();
  });

  it("counts no runs while the runs read is pending", async () => {
    window.history.replaceState(null, "", "/");
    const pending = held();
    fakeServer({ ...signedIn, "GET /api/clusters": { body: [web, upgrading] }, "GET /api/runs": pending.route });
    render(<App />);
    const runs = within(await screen.findByRole("complementary", { name: "Runs" }));
    expect(runs.getByText("Loading…")).toBeInTheDocument();
    expect(runs.queryByText("1 running")).not.toBeInTheDocument();
  });

  it("marks the fleet's health stale, with when it was read, once a later read fails", async () => {
    window.history.replaceState(null, "", "/");
    const routes: Record<string, Route> = { ...signedIn, "GET /api/fleet/health": { body: [healthy(web)] } };
    fakeServer(routes);
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await main.findByRole("row", { name: /^web.*Available/u });
    routes["GET /api/fleet/health"] = unreadable;
    await readAgain();
    await expect(main.findByText(/^Stale, read at/u)).resolves.toBeInTheDocument();
    expect(main.getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });

  it("counts none as needing someone or healthy while the health is not read", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer({ ...signedIn, "GET /api/fleet/health": unreadable });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await main.findByRole("alert");
    const dash = String.fromCodePoint(0x20_13);
    expect(main.getByRole("button", { name: `Needs you ${dash}` })).toBeInTheDocument();
    expect(main.getByRole("button", { name: `Healthy ${dash}` })).toBeInTheDocument();
  });

  it("calls the unread group Loading while the health is read", async () => {
    window.history.replaceState(null, "", "/");
    const pending = held();
    fakeServer({ ...signedIn, "GET /api/fleet/health": pending.route });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await expect(main.findByRole("button", { name: /^Loading 1/u })).resolves.toBeInTheDocument();
    expect(main.queryByRole("button", { name: /^Unknown/u })).not.toBeInTheDocument();
  });

  it("titles a cluster Unknown when its own read failed, whatever the fleet list says", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer({
      ...signedIn,
      "GET /api/fleet/health": { body: [healthy(web)] },
      "GET /api/clusters/default/web": unreadable,
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await main.findByRole("row", { name: /^web.*Available/u });
    await user.click(main.getByRole("link", { name: "web" }));
    const title = await screen.findByRole("heading", { level: 2, name: /^web/u });
    await waitFor(() => expect(title).toHaveTextContent("Unknown"));
    expect(title).not.toHaveTextContent("Available");
  });

  it("shows Placement unknown, not loading, when the cluster's read failed", async () => {
    window.history.replaceState(null, "", "/");
    const { machines: _machines, ...listed } = web;
    fakeServer({
      ...signedIn,
      "GET /api/clusters": { body: [listed] },
      "GET /api/clusters/default/web": unreadable,
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await user.click(await main.findByRole("link", { name: "web" }));
    const placement = within(await screen.findByRole("region", { name: "Placement" }));
    await expect(placement.findByText("Unknown")).resolves.toBeInTheDocument();
    expect(placement.queryByText("Loading…")).not.toBeInTheDocument();
  });

  it("shows Placement's failed node read with a retry", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({
      ...signedIn,
      "GET /api/clusters": { body: [placed] },
      "GET /api/clusters/default/web": { body: placed },
      "GET /api/proxmox/nodes": unreadable,
    });
    render(<App />);
    const placement = within(await screen.findByRole("region", { name: "Placement" }));
    await expect(placement.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(placement.getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });

  it("lists no node in Placement while the nodes are read", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const pending = held();
    fakeServer({
      ...signedIn,
      "GET /api/clusters": { body: [placed] },
      "GET /api/clusters/default/web": { body: placed },
      "GET /api/proxmox/nodes": pending.route,
    });
    render(<App />);
    const placement = within(await screen.findByRole("region", { name: "Placement" }));
    await expect(placement.findByText("Loading…")).resolves.toBeInTheDocument();
    expect(placement.queryByText("pve1")).not.toBeInTheDocument();
  });

  it("offers no typed version while the versions are read, and a retry once they fail", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const pending = held();
    fakeServer({ ...signedIn, "GET /api/versions/rke2": pending.route });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Upgrade" }));
    const dialog = within(await screen.findByRole("dialog", { name: "Upgrade web" }));
    expect(dialog.getByText("Loading…")).toBeInTheDocument();
    expect(dialog.queryByRole("textbox")).not.toBeInTheDocument();
    pending.answer(unreadable);
    await expect(dialog.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(dialog.getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });
});
