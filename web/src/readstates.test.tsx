import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { EtcdSnapshotsView } from "./EtcdSnapshots";
import { fakeServer, held, type Route } from "./fake-server";
import { ManagementBackups } from "./ManagementBackups";
import { Notifications } from "./Notifications";
import type { Cluster, ClusterEtcdSnapshots } from "./types";

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
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { etcdSnapshots: { enabled: true, target: "store", schedule: "0 */6 * * *", timeZone: "UTC", keep: 10 } },
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const unreadable = { status: 502, body: { error: "the management cluster did not answer" } };

const signedIn: Record<string, Route> = {
  "GET /api/session": { body: { user: "alice@pve", csrf: "c1" } },
  "GET /api/clusters": { body: [web] },
  "GET /api/clusters/default/web": { body: web },
  "GET /api/runs": { body: [] },
  "GET /api/fleet/health": { body: [] },
};

// visible makes every poll read again, as a tab does that is shown again.
async function readAgain(): Promise<void> {
  await act(async () => {
    document.dispatchEvent(new Event("visibilitychange"));
    await Promise.resolve();
  });
}

describe("a read that failed or has not answered", () => {
  it("leaves no cluster stated well while the fleet's health is not read", async () => {
    window.history.replaceState(null, "", "/");
    const routes = { ...signedIn, "GET /api/fleet/health": unreadable };
    fakeServer(routes);
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await expect(main.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(main.getByRole("button", { name: "Try now" })).toBeInTheDocument();
    expect(main.getByRole("row", { name: /^web/u })).toHaveTextContent("Unknown");
    expect(screen.queryByText("Available")).not.toBeInTheDocument();
    expect(main.getByRole("button", { name: /^Unknown/u })).toBeInTheDocument();
    expect(main.getByRole("button", { name: /^Needs you/u })).toBeInTheDocument();
  });

  it("states a cluster well only once its health is read", async () => {
    window.history.replaceState(null, "", "/");
    const health = {
      body: [{ namespace: "default", name: "web", read: created, verdict: "healthy", attention: [] }],
    };
    fakeServer({ ...signedIn, "GET /api/fleet/health": health });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await expect(main.findByRole("row", { name: /^web.*Available/u })).resolves.toBeInTheDocument();
    expect(main.queryByRole("button", { name: /^Unknown/u })).not.toBeInTheDocument();
  });

  it("shows a run page's failed read with its error and a retry, not Loading for good", async () => {
    window.history.replaceState(null, "", "/runs/change/default/web");
    fakeServer({ ...signedIn, "GET /api/runs": unreadable });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await expect(main.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(main.getByRole("button", { name: "Try now" })).toBeInTheDocument();
    expect(main.queryByText("Loading…")).not.toBeInTheDocument();
    expect(main.queryByText(/no longer kept/u)).not.toBeInTheDocument();
  });

  it("shows a run page loading while its read is pending, without saying the run is gone", async () => {
    window.history.replaceState(null, "", "/runs/change/default/web");
    const pending = held();
    fakeServer({ ...signedIn, "GET /api/runs": pending.route });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await main.findByRole("heading", { name: "Run" });
    expect(main.getByText("Loading…")).toBeInTheDocument();
    expect(main.queryByText(/no longer kept/u)).not.toBeInTheDocument();
    expect(main.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("names no node empty while the cluster's machines are not read", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const { machines: _machines, ...unread } = web;
    fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": { body: unread },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
    });
    render(<App />);
    const placement = within(await screen.findByRole("region", { name: "Placement" }));
    await expect(placement.findByText("Loading…")).resolves.toBeInTheDocument();
    expect(placement.queryByText("Empty")).not.toBeInTheDocument();
  });

  it("disables taking an etcd snapshot, with why, once the snapshots can no longer be read", async () => {
    const view: ClusterEtcdSnapshots = {
      enabled: true,
      target: "store",
      schedule: "0 */6 * * *",
      timeZone: "UTC",
      keep: 10,
      requested: false,
      snapshots: [],
      restoring: false,
      automatic: true,
      canSnapshot: true,
      canRestore: true,
    };
    const routes: Record<string, Route> = { "GET /api/clusters/default/web/etcd-snapshots": { body: view } };
    fakeServer(routes);
    render(<EtcdSnapshotsView cluster={web} can onEdit={vi.fn<() => void>()} />);
    const take = await screen.findByRole("button", { name: "Take snapshot" });
    expect(take).toBeEnabled();
    routes["GET /api/clusters/default/web/etcd-snapshots"] = unreadable;
    await readAgain();
    await waitFor(() => expect(screen.getByRole("button", { name: "Take snapshot" })).toBeDisabled());
    expect(screen.getByText("Snapshots not read.")).toBeInTheDocument();
  });

  it("disables taking a management snapshot once the backups can no longer be read", async () => {
    const routes: Record<string, Route> = {
      "GET /api/appliance/etcd-backups": {
        body: {
          settings: { target: "minio", everyHours: 6, keep: 28, by: "root@pam", since: created },
          targets: ["minio"],
          keysStored: true,
        },
      },
      "GET /api/appliance/etcd-backups/snapshots": unreadable,
    };
    fakeServer(routes);
    render(<ManagementBackups running={false} />);
    await expect(screen.findByRole("button", { name: "Take snapshot" })).resolves.toBeEnabled();
    await expect(screen.findByText("Unknown")).resolves.toBeInTheDocument();
    routes["GET /api/appliance/etcd-backups"] = unreadable;
    await readAgain();
    await waitFor(() => expect(screen.getByRole("button", { name: "Take snapshot" })).toBeDisabled());
  });

  it("offers a retry when the notifications' last sendings are not read", async () => {
    fakeServer({
      "GET /api/notifications": unreadable,
      "GET /api/credentials/notify-webhook": { body: { purpose: "notify-webhook", set: false, canChange: true } },
      "GET /api/credentials/notify-ntfy": { body: { purpose: "notify-ntfy", set: false, canChange: true } },
      "GET /api/credentials/notify-email": { body: { purpose: "notify-email", set: false, canChange: true } },
    });
    const user = userEvent.setup({ delay: null });
    render(<Notifications />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    await user.click(screen.getByRole("button", { name: "Try now" }));
  });
});
