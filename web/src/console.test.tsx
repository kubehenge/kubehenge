import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { scrollerOf } from "./ClusterList";
import { fakeServer, held, type Route } from "./fake-server";
import { ranked, standing } from "./fleet";
import { score } from "./Palette";
import { runView } from "./Runs";
import type { Cluster, FleetHealth, RunInfo } from "./types";

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
    expect(standing(base)).toStrictEqual({
      group: "healthy",
      state: "ok",
      reason: "RKE2 v1.36.5+rke2r1",
      view: "",
      verdict: "Available",
    });
    expect(standing(failedTalos)).toStrictEqual({
      group: "needs",
      state: "error",
      reason: "Talos upgrade failed on edge-w-1",
      view: "",
      verdict: "Upgrade failed",
    });
    expect(standing(upgrading)).toMatchObject({
      group: "running",
      reason: "Upgrading to v1.36.5+rke2r1",
      verdict: "Upgrading",
    });
    expect(standing(creating)).toMatchObject({
      group: "running",
      reason: "Creating: 1 of 3 control planes",
      verdict: "Creating",
    });
    expect(standing({ ...creating, created: minutesAgo(120) })).toMatchObject({
      group: "needs",
      reason: "Not available",
      verdict: "Not available",
    });
    expect(standing({ ...base, deleting: true })).toMatchObject({
      group: "running",
      state: "deleting",
      verdict: "Deleting",
    });
    // An available cluster on its way somewhere is not shown as Available.
    expect(standing({ ...base, workers: { ...steady, upToDate: 1 } })).toMatchObject({
      group: "running",
      state: "progress",
      reason: "Workers: 1 of 2 updated",
      verdict: "Updating",
    });
    expect(
      standing({
        ...base,
        machines: [{ name: "web-w-9", role: "worker", phase: "Failed", ready: false, created: "" }],
      }),
    ).toMatchObject({ group: "needs", reason: "Machine web-w-9 failed" });
    expect(standing({ ...upgrading, controllerError: "Proxmox VE task UPID:pve1 failed: no space" })).toStrictEqual({
      group: "needs",
      state: "error",
      reason: "Controller error",
      view: "",
      verdict: "Error",
    });
    expect(standing({ ...creating, stalled: "ci-cp-1: VM template not found" })).toStrictEqual({
      group: "needs",
      state: "warning",
      reason: "Machines stuck for 20 minutes",
      view: "machines",
      verdict: "Stuck",
    });
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
    // The backups view, not the health page, says why backups need a look.
    expect(
      standing(base, {
        ...crashing,
        attention: [{ severity: "error", section: "backups", what: "Last backup failed" }],
        more: 0,
      }),
    ).toStrictEqual({
      group: "needs",
      state: "error",
      reason: "Last backup failed",
      view: "backups",
      verdict: "Degraded",
    });
    // What is under way says more than the errors a rollout passes through.
    expect(standing(upgrading, { ...crashing, name: "db" })).toMatchObject({ group: "running" });
  });

  it("sends a run's reader to the page where its kind of work is started", () => {
    expect(runView(run("template"))).toStrictEqual({ to: "/administration/templates", place: "Open templates" });
    expect(runView(run("template-refresh"))).toStrictEqual({
      to: "/administration/templates",
      place: "Open templates",
    });
    expect(runView(run("management-cluster"))).toStrictEqual({ to: "/administration", place: "Open Administration" });
    expect(runView({ ...run("restore"), namespace: "default", cluster: "web" })).toStrictEqual({
      to: "/clusters/default/web/backups",
      place: "Open backups",
    });
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

const run = (kind: RunInfo["kind"]): RunInfo => ({ id: kind, kind, what: kind, state: "failed" });

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

  it("greys the fleet out once reading it again hangs, until a read answers", async () => {
    window.history.replaceState(null, "", "/");
    const routes = serve();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<App />);
    const rail = await screen.findByRole("navigation", { name: "Fleet" });
    await waitFor(() => expect(within(rail).getAllByRole("link")).toHaveLength(4));

    routes["GET /api/clusters"] = held().route;
    await act(() => vi.advanceTimersByTimeAsync(90_000));
    expect(rail).toHaveClass("rail-stale");
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
    expect(within(runs).getByRole("button", { name: "Hide runs" })).toHaveAttribute("title", "Hide runs (T R)");

    const user = userEvent.setup({ delay: null });
    await user.click(failed);
    await expect(screen.findByRole("heading", { name: /^Upgrade appliance to 0\.3\.1/u })).resolves.toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("Errorthe release's checksum does not match");
    expect(screen.getByRole("link", { name: "Open upgrades" })).toHaveAttribute("href", "/administration/appliance");
    // An appliance upgrade takes a release to choose, so it is not started again from here.
    expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
  });

  it("starts a failed backup again from its run page, then shows the cluster's backups", async () => {
    window.history.replaceState(null, "", "/runs/backup/default/web/manual-1");
    const routes = serve();
    routes["GET /api/runs"] = {
      body: [
        {
          id: "backup/default/web/manual-1",
          kind: "backup",
          namespace: "default",
          cluster: "web",
          what: "Back up",
          state: "failed",
          started: minutesAgo(9),
          finished: minutesAgo(8),
          error: "the bucket refused the upload: access denied",
        },
      ],
    };
    routes["POST /api/clusters/default/web/backups"] = { status: 403, body: { error: "backing up needs VM.Allocate" } };
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const again = await screen.findByRole("button", { name: "Try again" });
    await user.click(again);
    await expect(screen.findByText("Backing up needs VM.Allocate.")).resolves.toBeInTheDocument();
    expect(window.location.pathname).toBe("/runs/backup/default/web/manual-1");

    routes["POST /api/clusters/default/web/backups"] = { status: 202, body: { name: "manual-2" } };
    await user.click(again);
    await waitFor(() => expect(window.location.pathname).toBe("/clusters/default/web/backups"));
  });

  it("shows the end of a finished restore's log to those who may read the cluster's logs", async () => {
    window.history.replaceState(null, "", "/runs/restore/default/web/daily-1-a");
    const routes = serve();
    routes["GET /api/runs"] = {
      body: [
        {
          id: "restore/default/web/daily-1-a",
          kind: "restore",
          namespace: "default",
          cluster: "web",
          what: "Restore from daily-1",
          state: "failed",
          finished: minutesAgo(8),
          error: "Velero could not restore 2 items",
        },
      ],
    };
    routes["GET /api/clusters/default/web/restores/daily-1-a/log"] = {
      body: { lines: ['level=info msg="restoring shop"', 'level=error msg="widgets.example.org: no such kind"'] },
    };
    const { unmount } = render(<App />);
    await expect(screen.findByRole("heading", { name: /^Restore from daily-1/u })).resolves.toBeInTheDocument();
    expect(screen.queryByLabelText("Log")).not.toBeInTheDocument();
    unmount();

    routes["GET /api/clusters"] = {
      body: [{ ...base, allowed: { change: true, kubeconfig: true } }, ...fleet.slice(1)],
    };
    render(<App />);
    await expect(screen.findByLabelText("Log")).resolves.toHaveTextContent(
      'level=info msg="restoring shop" level=error msg="widgets.example.org: no such kind"',
    );
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

  it("draws a long fleet a page at a time", async () => {
    window.history.replaceState(null, "", "/clusters");
    const routes = serve();
    const many = Array.from({ length: 250 }, (_, i) => ({ ...base, name: `c${String(i).padStart(3, "0")}` }));
    routes["GET /api/clusters"] = { body: many };
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const table = await screen.findByRole("table");
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(101));
    await user.click(screen.getByRole("button", { name: "Show 100 more" }));
    expect(within(table).getAllByRole("row")).toHaveLength(201);
    await user.click(screen.getByRole("button", { name: "Show 50 more" }));
    expect(within(table).getAllByRole("row")).toHaveLength(251);
    expect(within(table).getAllByRole("row").at(-1)).toHaveTextContent("c249");
    expect(screen.queryByRole("button", { name: /^Show \d+ more$/u })).not.toBeInTheDocument();
  });

  it("watches for the end of the table in the box that scrolls it up and down, not the one beside it", () => {
    const page = document.createElement("div");
    page.style.overflowY = "auto";
    Object.defineProperty(page, "scrollHeight", { value: 4000 });
    Object.defineProperty(page, "clientHeight", { value: 900 });
    const sideways = document.createElement("div");
    sideways.style.overflowX = "auto";
    sideways.style.overflowY = "auto";
    Object.defineProperty(sideways, "scrollHeight", { value: 3000 });
    Object.defineProperty(sideways, "clientHeight", { value: 3000 });
    const end = document.createElement("div");
    sideways.append(end);
    page.append(sideways);
    document.body.append(page);
    expect(scrollerOf(end)).toBe(page);
    page.remove();
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

  it("leads from what needs attention to where it is acted on: the machine's row, or the backups", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const routes = serve();
    const failing = {
      ...base,
      machines: [
        { name: "web-w-1", role: "worker" as const, phase: "Running", ready: true, created: minutesAgo(9) },
        { name: "web-w-2", role: "worker" as const, phase: "Running", ready: false, created: minutesAgo(9) },
      ],
    };
    routes["GET /api/clusters/default/web"] = { body: failing };
    const found = (attention: unknown[]) => ({
      body: [{ namespace: "default", name: "web", read: minutesAgo(0), verdict: "degraded", attention }],
    });
    routes["GET /api/fleet/health"] = found([
      { severity: "error", section: "nodes", what: "Worker web-w-2 failed health check", machine: "web-w-2" },
    ]);
    const user = userEvent.setup({ delay: null });
    const { unmount } = render(<App />);
    const strip = within(await screen.findByRole("region", { name: "Needs attention" }));
    expect(strip.getByRole("link", { name: "Health" })).toBeInTheDocument();
    await user.click(strip.getByRole("link", { name: "Machine" }));
    expect(window.location.pathname + window.location.search).toBe("/clusters/default/web/machines?machine=web-w-2");
    const row = await screen.findByRole("row", { name: /^web-w-2/u });
    expect(row).toHaveAttribute("aria-current", "true");
    expect(screen.getByRole("row", { name: /^web-w-1/u })).not.toHaveAttribute("aria-current");
    unmount();

    window.history.replaceState(null, "", "/clusters/default/web");
    routes["GET /api/fleet/health"] = found([{ severity: "error", section: "backups", what: "Last backup failed" }]);
    render(<App />);
    const backups = within(await screen.findByRole("region", { name: "Needs attention" }));
    expect(backups.getByRole("link", { name: "Backups" })).toHaveAttribute("href", "/clusters/default/web/backups");
    expect(backups.queryByRole("link", { name: "Health" })).not.toBeInTheDocument();
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

  it("shows a stalled change with what holds it up, and a stalled creation beside it", async () => {
    window.history.replaceState(null, "", "/");
    const routes = serve();
    const why = "no machine has become ready since 2026-10-28T09:01:00Z; db-cp-4: VM template not found";
    routes["GET /api/clusters"] = {
      body: [base, { ...upgrading, stalled: why }, failedTalos, { ...creating, stalled: "ci-cp-1: no IP address" }],
    };
    routes["GET /api/runs"] = {
      body: [
        {
          id: "change/default/db",
          kind: "change",
          namespace: "default",
          cluster: "db",
          what: "Change the cluster",
          state: "stalled",
          started: minutesAgo(40),
          phase: "Replace the control-plane machines: 0 of 3",
          settings: ["version"],
          error: why,
          steps: [{ name: "Replace the control-plane machines", state: "running", detail: "0 of 3" }],
        },
      ],
    };
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const runs = within(await screen.findByRole("complementary", { name: "Runs" }));
    const stalledRuns = within(await runs.findByRole("region", { name: "Stalled" }));
    await waitFor(() => expect(stalledRuns.getAllByRole("link")).toHaveLength(2));
    // What holds a run up stays on its page, out of the narrow column.
    expect(stalledRuns.getByRole("link", { name: /^ci/u })).toHaveTextContent("Machines stuck for 20 minutes");
    const change = stalledRuns.getByRole("link", { name: /^db Change/u });
    expect(change).not.toHaveTextContent("VM template not found");

    await user.click(change);
    await expect(screen.findByText("Stalled", { selector: ".status" })).resolves.toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(`Held up by${why}`);
  });

  it("shows on the cluster's page the error the controller met and machines that are stuck", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const routes = serve();
    routes["GET /api/clusters/default/web"] = {
      body: {
        ...base,
        controllerError: "creating the cluster's Proxmox VE objects: no space",
        stalled: "web-w-3: no IP address",
      },
    };
    render(<App />);
    const failing = await screen.findByText("Controller error");
    expect(failing.closest(".error")).toHaveTextContent("creating the cluster's Proxmox VE objects: no space");
    const stuck = screen.getByText("Machines stuck for 20 minutes").closest(".warning");
    expect(stuck).toHaveTextContent("web-w-3: no IP address");
  });

  it("counts stalled runs beside running ones above the runs", async () => {
    window.history.replaceState(null, "", "/");
    const routes = serve();
    routes["GET /api/clusters"] = { body: [base, { ...creating, stalled: "ci-cp-1: no IP address" }] };
    render(<App />);
    const runs = await screen.findByRole("complementary", { name: "Runs" });
    await waitFor(() =>
      expect(within(runs).getByRole("heading", { level: 2 })).toHaveTextContent("Runs 0 running, 1 stalled"),
    );
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

  it("badges a cluster as the rail places it, also where its controller fails or a change is under way", async () => {
    window.history.replaceState(null, "", "/");
    const routes = serve();
    const rolling = { ...base, name: "shop", workers: { ...base.workers, upToDate: 1 } };
    routes["GET /api/clusters"] = {
      body: [{ ...base, controllerError: "Proxmox VE task UPID:pve1 failed: no space" }, rolling],
    };
    render(<App />);
    const main = await screen.findByRole("main");
    const web = await within(main).findByRole("row", { name: /^web/u });
    expect(within(web).getByText("Error")).toBeInTheDocument();
    expect(within(web).queryByText("Available")).not.toBeInTheDocument();
    const shop = within(main).getByRole("row", { name: /^shop/u });
    expect(within(shop).getByText("Updating")).toBeInTheDocument();
    expect(within(shop).queryByText("Available")).not.toBeInTheDocument();
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
