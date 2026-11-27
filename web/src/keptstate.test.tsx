import { render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { api } from "./api";
import { App } from "./App";
import { fakeServer, held, type Route } from "./fake-server";
import { ClusterStatus } from "./fleet";
import { keptFor, restoreKept } from "./kept-state";
import { clearPollCaches } from "./poll-cache";
import type { Cluster, FleetHealth } from "./types";

const web: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-kubeadm",
  distribution: "kubeadm",
  version: "v1.36.1",
  generation: 1,
  created: new Date(Date.now() - 86_400_000).toISOString(),
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
  machines: [],
};

const read = new Date().toISOString();
const healthy: FleetHealth = { namespace: "default", name: "web", verdict: "healthy", read, attention: [] };
const degraded: FleetHealth = {
  ...healthy,
  verdict: "degraded",
  attention: [{ section: "workloads", severity: "error", what: "Pod api crashing" }],
};

const session = (user = "root@pam"): Route => ({ body: { user, namespace: "default", canCreate: true, csrf: "c1" } });

function serve(routes: Record<string, Route> = {}): Record<string, Route> {
  const all: Record<string, Route> = {
    "GET /api/session": session(),
    "GET /api/setup": { body: { appliance: false, ready: true, allowed: false } },
    "GET /api/clusters": { body: [web] },
    "GET /api/fleet/health": { body: [healthy] },
    "GET /api/runs": { body: [] },
    "DELETE /api/session": { status: 204 },
    "GET /api/clusters/default/web": { body: web },
    ...routes,
  };
  fakeServer(all);
  return all;
}

const kept = (): string[] => Object.keys(sessionStorage).filter((k) => k.startsWith("kubehenge-kept-"));

// refresh does to the page what a browser's reload does: the page goes, the tab's storage stays.
function refresh(unmount: () => void): void {
  unmount();
  const saved = new Map<string, string>();
  for (const k of kept()) {
    saved.set(k, sessionStorage.getItem(k) ?? "");
  }
  clearPollCaches();
  for (const [k, v] of saved) {
    sessionStorage.setItem(k, v);
  }
  restoreKept();
}

// shownOnce renders a cluster's page with the fleet beside it and waits until its health is in, as
// the tab saw it before a refresh.
async function shownOnce(): Promise<() => void> {
  window.history.replaceState(null, "", "/clusters/default/web");
  const { unmount } = render(<App />);
  const rail = await screen.findByRole("navigation", { name: "Fleet" });
  await within(rail).findByRole("region", { name: "Healthy" });
  return unmount;
}

describe("before a cluster's health is read", () => {
  it("shows no healthy verdict anywhere on a refresh", async () => {
    serve({ "GET /api/fleet/health": held().route });
    render(<App />);
    await expect(screen.findByRole("link", { name: "web" })).resolves.toBeInTheDocument();
    expect(screen.queryByText("Available")).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Healthy" })).not.toBeInTheDocument();
    expect(screen.getAllByText("Loading…").length).toBeGreaterThan(0);
  });

  it("shows no healthy verdict on a cluster's page", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    serve({ "GET /api/fleet/health": held().route });
    render(<App />);
    await expect(screen.findByRole("heading", { level: 2, name: /web/u })).resolves.toBeInTheDocument();
    expect(screen.queryByText("Available")).not.toBeInTheDocument();
  });

  it("gives a badge outside a fleet no healthy verdict", () => {
    render(<ClusterStatus cluster={web} />);
    expect(screen.queryByText("Available")).not.toBeInTheDocument();
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });
});

describe("a refresh", () => {
  it("shows the fleet as last read at once, greyed, then the fresh read", async () => {
    serve();
    const unmount = await shownOnce();
    refresh(unmount);

    const health = held();
    serve({ "GET /api/clusters": held().route, "GET /api/fleet/health": health.route });
    render(<App />);
    const rail = await screen.findByRole("navigation", { name: "Fleet" });
    await expect(within(rail).findByRole("region", { name: "Healthy" })).resolves.toBeInTheDocument();
    expect(rail).toHaveAttribute("aria-busy", "true");
    expect(rail).toHaveClass("rail-stale");
    expect(within(rail).queryByText("Stale")).not.toBeInTheDocument();
    for (const badge of screen.getAllByText("Available")) {
      expect(badge.closest("[aria-busy]")).toHaveAttribute("aria-busy", "true");
    }

    health.answer({ body: [degraded] });
    await expect(within(rail).findByRole("region", { name: "Needs you" })).resolves.toBeInTheDocument();
    expect(screen.getAllByText("Degraded").length).toBeGreaterThan(0);
    // The list is still not read again, so what shows is not current yet.
    expect(rail).toHaveAttribute("aria-busy", "true");
  });

  it("greys the kept list out until it is read again", async () => {
    serve();
    const unmount = await shownOnce();
    refresh(unmount);

    window.history.replaceState(null, "", "/");
    const clusters = held();
    const health = held();
    serve({ "GET /api/clusters": clusters.route, "GET /api/fleet/health": health.route });
    render(<App />);
    const heading = await screen.findByRole("heading", { name: "Clusters" });
    const list = heading.closest("section");
    expect(list).toHaveAttribute("aria-busy", "true");
    expect(list).toHaveClass("fleet-kept");
    expect(list).toContainElement(screen.getByRole("link", { name: "web" }));

    clusters.answer({ body: [web] });
    health.answer({ body: [healthy] });
    await waitFor(() => expect(list).toHaveAttribute("aria-busy", "false"));
    expect(list).not.toHaveClass("fleet-kept");
  });

  it("is current once both reads answer", async () => {
    serve();
    const unmount = await shownOnce();
    refresh(unmount);

    serve({ "GET /api/fleet/health": { body: [degraded] } });
    render(<App />);
    const rail = await screen.findByRole("navigation", { name: "Fleet" });
    await within(rail).findByRole("region", { name: "Needs you" });
    await waitFor(() => expect(rail).toHaveAttribute("aria-busy", "false"));
    expect(rail).not.toHaveClass("rail-stale");
  });

  it("shows nothing kept that is too old", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    serve();
    const unmount = await shownOnce();
    vi.setSystemTime(Date.now() + keptFor);
    refresh(unmount);

    serve({ "GET /api/clusters": held().route, "GET /api/fleet/health": held().route });
    render(<App />);
    const rail = await screen.findByRole("navigation", { name: "Fleet" });
    expect(within(rail).queryByRole("link", { name: /web/u })).not.toBeInTheDocument();
  });

  it("shows another user nothing kept for the one before", async () => {
    serve();
    const unmount = await shownOnce();
    refresh(unmount);

    serve({
      "GET /api/session": session("alice@pve"),
      "GET /api/clusters": held().route,
      "GET /api/fleet/health": held().route,
    });
    render(<App />);
    const rail = await screen.findByRole("navigation", { name: "Fleet" });
    expect(within(rail).queryByRole("link", { name: /web/u })).not.toBeInTheDocument();
    expect(kept()).toStrictEqual([]);
  });

  it("keeps nothing after sign-out", async () => {
    serve();
    await shownOnce();
    expect(kept()).not.toStrictEqual([]);
    await api.signOut();
    expect(kept()).toStrictEqual([]);
  });
});
