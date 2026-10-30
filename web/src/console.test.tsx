import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

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
      verdict: "Upgrade failed",
    });
    expect(standing(upgrading)).toMatchObject({ group: "running", reason: "Upgrading to v1.36.5+rke2r1" });
    expect(standing(creating)).toMatchObject({ group: "running", reason: "Creating: 1 of 3 control planes" });
    expect(standing({ ...creating, created: minutesAgo(120) })).toMatchObject({
      group: "needs",
      reason: "Not available",
      verdict: "Not available",
    });
    expect(standing({ ...base, deleting: true })).toMatchObject({ group: "running", state: "deleting" });
    expect(standing({ ...base, workers: { ...steady, upToDate: 1 } })).toMatchObject({
      group: "running",
      reason: "Workers: 1 of 2 updated",
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
      reason: "Pod shop/api: CrashLoopBackOff and 4 more",
      view: "health",
      verdict: "Degraded",
    });
    expect(standing(base, { ...crashing, verdict: "unreachable", attention: [] })).toMatchObject({
      group: "needs",
      reason: "API server unreachable",
      verdict: "Unreachable",
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

// names are the clusters the fleet table lists, in its order.
const names = (): (string | null)[] =>
  within(screen.getByRole("table"))
    .getAllByRole("link")
    .map((l) => l.textContent);

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
  it("greys the fleet out while reading it again fails, until a read succeeds", async () => {
    window.history.replaceState(null, "", "/");
    const routes = serve();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<App />);
    const rail = await screen.findByRole("navigation", { name: "Fleet" });
    await waitFor(() => expect(within(rail).getAllByRole("link")).toHaveLength(4));
    expect(rail).not.toHaveClass("rail-stale");

    routes["GET /api/clusters"] = { status: 502, body: { error: "the management cluster does not answer" } };
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    await waitFor(() => expect(rail).toHaveClass("rail-stale"));
    expect(within(rail).getByText("Stale")).toHaveAttribute("title", "the management cluster does not answer");
    expect(within(rail).getAllByRole("link")).toHaveLength(4);

    routes["GET /api/clusters"] = { body: fleet };
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    await waitFor(() => expect(rail).not.toHaveClass("rail-stale"));
    vi.useRealTimers();
  });

  it("says when it reads the clusters again while the management cluster does not answer", async () => {
    window.history.replaceState(null, "", "/clusters");
    const routes = serve();
    routes["GET /api/clusters"] = { status: 502, body: { error: "the management cluster did not answer" } };
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await expect(main.findByText("The management cluster did not answer.")).resolves.toBeInTheDocument();
    expect(main.getByText(/^Trying again in \d+ s$/u)).toBeInTheDocument();
    expect(main.queryByRole("link", { name: "Management cluster" })).not.toBeInTheDocument();
    const rail = within(screen.getByRole("navigation", { name: "Fleet" }));
    expect(rail.getByText("Not read")).toHaveAttribute("title", "the management cluster did not answer");

    routes["GET /api/clusters"] = { body: fleet };
    await user.click(main.getByRole("button", { name: "Try now" }));
    await expect(main.findByRole("row", { name: /^web/u })).resolves.toBeInTheDocument();
    expect(main.queryByText(/^Trying again/u)).not.toBeInTheDocument();
  });

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
    ).toStrictEqual(["ciCreating: 1 of 3 control planes", "dbUpgrading to v1.36.5+rke2r1"]);
  });

  it("follows the snapshots, backups and the appliance's work, a failure's error on its own page", async () => {
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
          what: "Upgrade appliance to 0.3.1",
          state: "failed",
          finished: minutesAgo(5),
          error: "the release's checksum does not match",
        },
        {
          id: "etcd-snapshot/default/db",
          kind: "etcd-snapshot",
          namespace: "default",
          cluster: "db",
          what: "Take etcd snapshot",
          state: "succeeded",
          finished: minutesAgo(12),
        },
      ],
    };
    render(<App />);
    const runs = await screen.findByRole("complementary", { name: "Runs" });
    const running = await within(runs).findByRole("link", { name: /^web Scheduled backup/u });
    expect(running).toHaveAttribute("href", "/runs/backup/default/web/daily-1");
    expect(running).toHaveTextContent("webScheduled backup120 of 300 items");
    const failed = within(within(runs).getByRole("region", { name: "Failed" })).getByRole("link");
    expect(failed).toHaveAttribute("href", "/runs/appliance-upgrade");
    expect(failed).toHaveTextContent("Upgrade appliance to 0.3.1 failed, 5m ago");
    expect(failed).not.toHaveTextContent("checksum");
    expect(within(within(runs).getByRole("region", { name: "Done" })).getByRole("link")).toHaveTextContent(
      "dbTake etcd snapshot, 12m ago",
    );
    expect(screen.getByRole("button", { name: "Running 3" })).toBeInTheDocument();

    const user = userEvent.setup({ delay: null });
    await user.click(failed);
    await expect(screen.findByRole("heading", { name: /^Upgrade appliance to 0\.3\.1/u })).resolves.toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("Errorthe release's checksum does not match");
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
          what: "Change cluster",
          state: "running",
          by: "dev@pve",
          started: minutesAgo(6),
          phase: "Replace control planes: 1 of 3 replaced",
          settings: ["version", "podSecurity"],
          steps: [
            { name: "Accepted", state: "done" },
            { name: "Replace control planes", state: "running", detail: "1 of 3 replaced" },
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
      "dbChange Kubernetes version, Pod securityReplace control planes: 1 of 3 replaced",
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
    ).toStrictEqual(["AcceptedDone", "Replace control planesRunning1 of 3 replaced", "SettleWaiting"]);
    expect(screen.getByText("by dev@pve")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open cluster" })).toHaveAttribute("href", "/clusters/default/db");
  });

  it("filters, groups and sorts the fleet list, and keeps the view in the address", async () => {
    window.history.replaceState(null, "", "/");
    serve();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await waitFor(() => expect(names()).toStrictEqual(["edge", "ci", "db", "web"]));

    await user.click(screen.getByRole("button", { name: "Cluster" }));
    expect(names()).toStrictEqual(["ci", "db", "edge", "web"]);
    expect(screen.getByRole("columnheader", { name: /Cluster/u })).toHaveAttribute("aria-sort", "ascending");
    await user.click(screen.getByRole("button", { name: /Cluster/u }));
    expect(names()).toStrictEqual(["web", "edge", "db", "ci"]);
    expect(window.location.search).toBe("?sort=name&order=desc");

    const show = within(screen.getByRole("group", { name: "Show" }));
    await user.click(show.getByRole("button", { name: "Running 2" }));
    expect(names()).toStrictEqual(["db", "ci"]);

    await user.click(show.getByRole("button", { name: "All 4" }));
    await user.type(screen.getByLabelText("Filter"), "dge");
    expect(names()).toStrictEqual(["edge"]);
    expect(screen.getByText("1 of 4")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Filter"), "zz");
    expect(screen.getByText("No clusters match")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Clear filter" }));
    expect(names()).toHaveLength(4);
    expect(window.location.search).toBe("?sort=name&order=desc");
  });

  it("marks a cluster's tabs that hold something to look at, keeping the tabs' names", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const routes = serve();
    const failing = {
      ...base,
      machines: [{ name: "web-w-9", role: "worker" as const, phase: "Failed", ready: false, created: minutesAgo(9) }],
    };
    routes["GET /api/clusters/default/web"] = { body: failing };
    routes["GET /api/fleet/health"] = {
      body: [
        {
          namespace: "default",
          name: "web",
          read: minutesAgo(0),
          verdict: "degraded",
          attention: [
            { severity: "warning", section: "addons", what: "Add-on ingress: pending-install" },
            { severity: "progress", section: "addons", what: "Add-on x installing" },
          ],
          more: 1,
        },
      ],
    };
    render(<App />);
    const tabs = within(await screen.findByRole("navigation", { name: "web's views" }));
    await waitFor(() => expect(tabs.getByRole("link", { name: "Health" })).toHaveAccessibleDescription("2 to look at"));
    expect(tabs.getByRole("link", { name: "Machines" })).toHaveAccessibleDescription("1 failed");
    expect(tabs.getByRole("link", { name: "Summary" })).not.toHaveAttribute("aria-describedby");

    // The worst of it shows on every view but Health, which lists it all.
    const strip = within(screen.getByRole("region", { name: "Needs attention" }));
    expect(strip.getByText("Add-on ingress: pending-install")).toHaveClass("status-warning");
    expect(strip.getByText("2 more")).toBeInTheDocument();
    expect(strip.getByRole("link", { name: "Health" })).toHaveAttribute("href", "/clusters/default/web/health");
    const user = userEvent.setup({ delay: null });
    await user.click(tabs.getByRole("link", { name: "Health" }));
    expect(screen.queryByRole("region", { name: "Needs attention" })).not.toBeInTheDocument();
  });

  it("says on a cluster's page that it does not answer", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/options");
    const routes = serve();
    routes["GET /api/fleet/health"] = {
      body: [
        {
          namespace: "default",
          name: "web",
          read: minutesAgo(0),
          verdict: "unreachable",
          workload: "The API server does not answer.",
          attention: [],
        },
      ],
    };
    render(<App />);
    const strip = within(await screen.findByRole("region", { name: "Needs attention" }));
    expect(strip.getByText("Unreachable")).toHaveClass("status-error");
    expect(strip.getByText("The API server does not answer.")).toBeInTheDocument();
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
    // The list says what the rail says, not that Cluster API finds the cluster available.
    const row = within(screen.getByRole("main")).getByRole("row", { name: /^web/u });
    expect(within(row).getByText("Degraded")).toBeInTheDocument();
    expect(within(row).queryByText("Available")).not.toBeInTheDocument();
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

  it("starts a cluster's actions from the palette, only those the user may", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    serve();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findAllByRole("link", { name: /^db/u });

    await user.keyboard("{Control>}k{/Control}");
    let palette = screen.getByRole("dialog", { name: "Go to or run" });
    await user.type(within(palette).getByRole("combobox"), "db upgr");
    expect(within(palette).getAllByRole("option")[0]).toHaveTextContent("db: Upgrade");
    await user.keyboard("{Enter}");
    const upgrade = await screen.findByRole("dialog", { name: "Upgrade db" });
    expect(window.location.pathname).toBe("/clusters/default/db");
    expect(window.location.search).toBe("");
    await user.click(within(upgrade).getByRole("button", { name: "Cancel" }));

    // Without the kubeconfig privilege there is no download to offer, nor to start from the address.
    await user.keyboard("{Control>}k{/Control}");
    palette = screen.getByRole("dialog", { name: "Go to or run" });
    await user.type(within(palette).getByRole("combobox"), "kubeconfig");
    expect(within(palette).queryByRole("option", { name: /Download kubeconfig/u })).not.toBeInTheDocument();
    await user.keyboard("{Escape}");
    window.history.replaceState(null, "", "/clusters/default/web?do=kubeconfig");
    window.dispatchEvent(new PopStateEvent("popstate"));
    await waitFor(() => expect(window.location.search).toBe(""));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
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

    // Leaving the page takes two keys, so a stray one does nothing.
    await user.keyboard("c");
    expect(window.location.pathname).toBe("/clusters/default/edge/machines");

    await user.keyboard("?");
    const keys = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    expect(within(keys).getByRole("rowheader", { name: "]" })).toBeInTheDocument();
    expect(within(keys).getByRole("rowheader", { name: "G C" })).toBeInTheDocument();
    // Keys pressed with a dialog open belong to the dialog.
    await user.keyboard("1");
    expect(window.location.pathname).toBe("/clusters/default/edge/machines");
    await user.click(within(keys).getByRole("button", { name: "Done" }));

    await user.keyboard("{Control>}k{/Control}");
    await user.type(screen.getByRole("combobox"), "c");
    expect(window.location.pathname).toBe("/clusters/default/edge/machines");
  });

  it("leaves the page only on two-key sequences", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    serve();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findByRole("heading", { name: /web/u });
    await user.keyboard("c");
    expect(window.location.pathname).toBe("/clusters/default/web");
    // A first key that starts no sequence leaves the next key to itself.
    await user.keyboard("x3");
    expect(window.location.pathname).toBe("/clusters/default/web/machines");
    await user.keyboard("gc");
    expect(window.location.pathname).toBe("/");
  });
});
