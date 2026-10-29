import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer, type Route } from "./fake-server";
import { ranked, standing } from "./fleet";
import { score } from "./Palette";
import type { Cluster, FleetHealth } from "./types";

const minutesAgo = (n: number): string => new Date(Date.now() - n * 60_000).toISOString();

const steady = { desired: 2, ready: 2, available: 2, upToDate: 2 };

const base: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  generation: 1,
  created: minutesAgo(600),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [{ type: "Available", status: "True", since: minutesAgo(590) }],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: steady,
  workerPools: [{ name: "md-0", class: "worker", replicas: 2 }],
  variables: {},
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const failedTalos: Cluster = {
  ...base,
  name: "edge",
  distribution: "talos",
  talos: { version: "v1.15.1", failed: "edge-w-1 to v1.15.1 at 2026-10-25T19:41:11Z: no answer" },
};
const upgrading: Cluster = {
  ...base,
  name: "db",
  conditions: [
    ...base.conditions,
    { type: "TopologyReconciled", status: "False", reason: "ClusterUpgrading", since: minutesAgo(5) },
  ],
};
const creating: Cluster = {
  ...base,
  name: "ci",
  available: false,
  phase: "Provisioning",
  created: minutesAgo(8),
  conditions: [],
  controlPlane: { desired: 3, ready: 1, available: 1, upToDate: 1 },
};
const fleet = [base, upgrading, failedTalos, creating];

describe("the fleet", () => {
  it("says where each cluster stands and why", () => {
    expect(standing(base)).toStrictEqual({ group: "healthy", state: "ok", reason: "RKE2 v1.36.5+rke2r1", view: "" });
    expect(standing(failedTalos)).toStrictEqual({
      group: "needs",
      state: "error",
      reason: "Talos upgrade failed on edge-w-1",
      view: "",
    });
    expect(standing(upgrading)).toMatchObject({ group: "running", reason: "Upgrading to v1.36.5+rke2r1" });
    expect(standing(creating)).toMatchObject({ group: "running", reason: "Creating: 1 of 3 control planes ready" });
    expect(standing({ ...creating, created: minutesAgo(120) })).toMatchObject({
      group: "needs",
      reason: "Not available",
    });
    expect(standing({ ...base, deleting: true })).toMatchObject({ group: "running", state: "deleting" });
    expect(standing({ ...base, workers: { ...steady, upToDate: 1 } })).toMatchObject({
      group: "running",
      reason: "Workers rolling out: 1 of 2 up to date",
    });
    expect(
      standing({
        ...base,
        machines: [{ name: "web-w-9", role: "worker", phase: "Failed", ready: false, created: "" }],
      }),
    ).toMatchObject({ group: "needs", reason: "Machine web-w-9 failed" });
  });

  it("takes why a cluster needs someone from its health page, when the cluster itself says nothing", () => {
    const read = minutesAgo(0);
    const crashing: FleetHealth = {
      namespace: "default",
      name: "web",
      read,
      verdict: "degraded",
      attention: [
        { severity: "error", section: "pods", what: "Pod shop/api: CrashLoopBackOff" },
        { severity: "warning", section: "addons", what: "Add-on ingress: pending-install" },
      ],
      more: 3,
    };
    expect(standing(base, crashing)).toStrictEqual({
      group: "needs",
      state: "error",
      reason: "Pod shop/api: CrashLoopBackOff, and 4 more",
      view: "health",
    });
    expect(standing(base, { ...crashing, verdict: "unreachable", attention: [] })).toMatchObject({
      group: "needs",
      reason: "Its API server does not answer",
    });
    expect(
      standing(base, {
        ...crashing,
        attention: [{ severity: "progress", section: "addons", what: "Add-on x installing" }],
        more: 0,
      }),
    ).toMatchObject({ group: "running", reason: "Add-on x installing" });
    expect(standing(base, { ...crashing, verdict: "healthy", attention: [] })).toMatchObject({ group: "healthy" });
    // What is under way says more than the errors a rollout passes through.
    expect(standing(upgrading, { ...crashing, name: "db" })).toMatchObject({ group: "running" });
  });

  it("ranks those that need someone first, then the running, then the healthy", () => {
    expect(ranked(fleet).map((r) => r.cluster.name)).toStrictEqual(["edge", "ci", "db", "web"]);
  });

  it("matches a few letters of a name in order, a word's start counting more", () => {
    expect(score("dbop", "db: Options")).toBeGreaterThan(score("dbop", "dashboard operator"));
    expect(score("xyz", "db: Options")).toBe(-1);
    expect(score("", "anything")).toBe(0);
  });
});

function serve(): Record<string, Route> {
  const routes: Record<string, Route> = {
    "GET /api/session": { body: { user: "alice@pve", csrf: "c1" } },
    "GET /api/clusters": { body: fleet },
    "GET /api/runs": { body: [] },
  };
  for (const c of fleet) {
    routes[`GET /api/clusters/default/${c.name}`] = { body: c };
  }
  fakeServer(routes);
  return routes;
}

describe("the console", () => {
  it("lists the fleet by where each cluster stands, with why, and follows the running work", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    serve();
    render(<App />);
    const rail = await screen.findByRole("navigation", { name: "Fleet" });
    await waitFor(() => expect(within(rail).getAllByRole("link")).toHaveLength(4));
    const needs = within(rail).getByRole("region", { name: "Needs you" });
    expect(within(needs).getByRole("link")).toHaveTextContent("edgeTalos upgrade failed on edge-w-1");
    const healthy = within(rail).getByRole("region", { name: "Healthy" });
    expect(within(healthy).getByRole("link")).toHaveAttribute("aria-current", "page");

    const runs = within(screen.getByRole("complementary", { name: "Runs" })).getByRole("region", { name: "Running" });
    expect(
      within(runs)
        .getAllByRole("link")
        .map((l) => l.textContent),
    ).toStrictEqual(["ciCreating: 1 of 3 control planes ready", "dbUpgrading to v1.36.5+rke2r1"]);
  });

  it("follows the snapshots, backups and the appliance's work, a failure with its first error", async () => {
    window.history.replaceState(null, "", "/");
    const routes = serve();
    routes["GET /api/runs"] = {
      body: [
        {
          id: "backup/default/web/daily-1",
          kind: "backup",
          namespace: "default",
          cluster: "web",
          what: "Scheduled backup",
          state: "running",
          phase: "120 of 300 items",
        },
        {
          id: "appliance-upgrade",
          kind: "appliance-upgrade",
          what: "Upgrade the appliance to 0.3.1",
          state: "failed",
          finished: minutesAgo(5),
          error: "the release's checksum does not match",
        },
        {
          id: "etcd-snapshot/default/db",
          kind: "etcd-snapshot",
          namespace: "default",
          cluster: "db",
          what: "Take an etcd snapshot",
          state: "succeeded",
          finished: minutesAgo(12),
        },
      ],
    };
    render(<App />);
    const runs = await screen.findByRole("complementary", { name: "Runs" });
    const running = await within(runs).findByRole("link", { name: /^web Scheduled backup/u });
    expect(running).toHaveAttribute("href", "/runs/backup/default/web/daily-1");
    expect(running).toHaveTextContent("Scheduled backup: 120 of 300 items");
    const failed = within(within(runs).getByRole("region", { name: "Failed" })).getByRole("link");
    expect(failed).toHaveAttribute("href", "/runs/appliance-upgrade");
    expect(failed).toHaveTextContent("Upgrade the appliance to 0.3.1 failed, 5m ago");
    expect(failed).toHaveTextContent("the release's checksum does not match");
    expect(within(within(runs).getByRole("region", { name: "Done" })).getByRole("link")).toHaveTextContent(
      "dbTake an etcd snapshot, 12m ago",
    );
    expect(screen.getByRole("button", { name: "Running 3" })).toBeInTheDocument();

    const user = userEvent.setup({ delay: null });
    await user.click(failed);
    await expect(
      screen.findByRole("heading", { name: /^Upgrade the appliance to 0\.3\.1/u }),
    ).resolves.toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("First errorthe release's checksum does not match");
    expect(screen.getByRole("link", { name: "Open Administration" })).toHaveAttribute("href", "/administration");
  });

  it("follows a change made here by its steps, in place of the cluster's standing", async () => {
    window.history.replaceState(null, "", "/");
    const routes = serve();
    routes["GET /api/runs"] = {
      body: [
        {
          id: "change/default/db",
          kind: "change",
          namespace: "default",
          cluster: "db",
          what: "Change the cluster",
          state: "running",
          by: "dev@pve",
          started: minutesAgo(6),
          phase: "Replace the control-plane machines: 1 of 3",
          settings: ["version", "podSecurity"],
          steps: [
            { name: "Accepted by the API server", state: "done" },
            { name: "Replace the control-plane machines", state: "running", detail: "1 of 3" },
            { name: "Settle", state: "waiting" },
          ],
        },
      ],
    };
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const runs = within(await screen.findByRole("complementary", { name: "Runs" }));
    const change = await runs.findByRole("link", { name: /^db Change/u });
    expect(change).toHaveTextContent(
      "Change Kubernetes version, Pod security: Replace the control-plane machines: 1 of 3",
    );
    expect(runs.queryByRole("link", { name: /Upgrading to/u })).not.toBeInTheDocument();

    await user.click(change);
    await expect(
      screen.findByRole("heading", { name: /^Change Kubernetes version, Pod security/u }),
    ).resolves.toBeInTheDocument();
    expect(
      within(screen.getByRole("list", { name: "Steps" }))
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toStrictEqual([
      "Accepted by the API serverDone",
      "Replace the control-plane machinesUnder way1 of 3",
      "SettleWaiting",
    ]);
    expect(screen.getByText("by dev@pve")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the cluster" })).toHaveAttribute("href", "/clusters/default/db");
  });

  it("moves a cluster its health page finds trouble in to those needing you, linked to that page", async () => {
    window.history.replaceState(null, "", "/");
    const routes = serve();
    routes["GET /api/fleet/health"] = {
      body: [
        {
          namespace: "default",
          name: "web",
          read: minutesAgo(0),
          verdict: "degraded",
          attention: [{ severity: "error", section: "pods", what: "Pod shop/api: CrashLoopBackOff" }],
        },
      ],
    };
    render(<App />);
    const rail = await screen.findByRole("navigation", { name: "Fleet" });
    await waitFor(() =>
      expect(within(within(rail).getByRole("region", { name: "Needs you" })).getAllByRole("link")).toHaveLength(2),
    );
    const web = within(rail).getByRole("link", { name: /^web Pod shop/u });
    expect(web).toHaveAttribute("href", "/clusters/default/web/health");
    expect(within(screen.getByRole("main")).getByText("Pod shop/api: CrashLoopBackOff")).toBeInTheDocument();
  });

  it("goes anywhere from the palette, by a few letters typed", async () => {
    window.history.replaceState(null, "", "/");
    serve();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findByRole("navigation", { name: "Fleet" });
    await screen.findAllByRole("link", { name: /^db/u });

    await user.keyboard("{Control>}k{/Control}");
    const palette = screen.getByRole("dialog", { name: "Go to or run" });
    const field = within(palette).getByRole("combobox");
    expect(field).toHaveFocus();
    await user.type(field, "db opt");
    expect(within(palette).getAllByRole("option")[0]).toHaveTextContent("db: Options");
    await user.keyboard("{Enter}");

    expect(window.location.pathname).toBe("/clusters/default/db/options");
    expect(screen.queryByRole("dialog", { name: "Go to or run" })).not.toBeInTheDocument();
  });

  it("opens each of Administration's pages from the palette, for those who may set Kubehenge up", async () => {
    window.history.replaceState(null, "", "/");
    const routes = serve();
    routes["GET /api/setup"] = { body: { appliance: true, ready: true, allowed: true } };
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findByRole("link", { name: "Administration" });

    await user.keyboard("{Control>}k{/Control}");
    const palette = screen.getByRole("dialog", { name: "Go to or run" });
    await user.type(within(palette).getByRole("combobox"), "adm temp");
    expect(within(palette).getAllByRole("option")[0]).toHaveTextContent("Administration: Templates");
    await user.keyboard("{Enter}");
    expect(window.location.pathname).toBe("/administration/templates");
    expect(screen.getByRole("link", { name: "Administration" })).toHaveAttribute("aria-current", "page");
  });

  it("answers single keys outside fields: a cluster's views, the next cluster, the keys", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    serve();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findByRole("heading", { name: /web/u });
    await waitFor(() =>
      expect(within(screen.getByRole("navigation", { name: "Fleet" })).getAllByRole("link")).toHaveLength(4),
    );

    await user.keyboard("3");
    expect(window.location.pathname).toBe("/clusters/default/web/machines");
    expect(screen.getByRole("link", { name: "Machines" })).toHaveAttribute("aria-current", "page");

    // web is the last in the rail's order, so the next one is the first, kept on the same view.
    await user.keyboard("]");
    expect(window.location.pathname).toBe("/clusters/default/edge/machines");

    await user.keyboard("?");
    const keys = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    expect(within(keys).getByRole("rowheader", { name: "]" })).toBeInTheDocument();
    // Keys pressed with a dialog open belong to the dialog.
    await user.keyboard("1");
    expect(window.location.pathname).toBe("/clusters/default/edge/machines");
    await user.click(within(keys).getByRole("button", { name: "Done" }));

    await user.keyboard("{Control>}k{/Control}");
    await user.type(screen.getByRole("combobox"), "c");
    expect(window.location.pathname).toBe("/clusters/default/edge/machines");
  });
});
