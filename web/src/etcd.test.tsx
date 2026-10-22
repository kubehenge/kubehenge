import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { scheduleText } from "./Backups";
import { etcdFieldsOf, etcdProblems, etcdSnapshotsText, restoreProblem } from "./EtcdSnapshots";
import { fakeServer } from "./fake-server";
import type { Cluster, ClusterClass, ClusterEtcdSnapshots, EtcdRestorePlan } from "./types";

const targets = JSON.stringify([
  { name: "store", endpoint: "https://s3.example.org:8333", bucket: "backups" },
  { name: "aws", bucket: "aws-backups", region: "eu-central-2" },
]);

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  annotations: { "kubehenge.dev/backup-targets": targets },
  variables: [{ name: "etcdSnapshots", required: false, schema: { type: "object" } }],
};

const cluster: Cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { etcdSnapshots: { enabled: false } },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

const snapshotting: Cluster = {
  ...cluster,
  variables: { etcdSnapshots: { enabled: true, target: "store", schedule: "0 */6 * * *", timeZone: "UTC", keep: 10 } },
};

const taken = "2026-10-21T06:00:00Z";
const older = "2026-10-21T00:00:00Z";

const snapshot = {
  name: "20261021T060000Z-v1.36.5+rke2r1.db.age",
  time: taken,
  kubernetesVersion: "v1.36.5+rke2r1",
  distribution: "rke2",
  revision: 48_213,
  etcdVersion: "3.6.5",
  sha256: "a".repeat(64),
  size: 12 * 1024 * 1024,
};

const view: ClusterEtcdSnapshots = {
  enabled: true,
  target: "store",
  schedule: "0 */6 * * *",
  timeZone: "UTC",
  keep: 10,
  last: snapshot,
  condition: { type: "EtcdSnapshotSucceeded", status: "True", reason: "Taken", since: taken },
  requested: false,
  snapshots: [
    snapshot,
    {
      ...snapshot,
      name: "20261021T000000Z-v1.35.2+rke2r1.db.age",
      time: older,
      kubernetesVersion: "v1.35.2+rke2r1",
      revision: 40_100,
    },
  ],
  restoring: false,
  automatic: true,
  canSnapshot: true,
  canRestore: true,
};

const plan: EtcdRestorePlan = {
  snapshot,
  distribution: "rke2",
  steps: [
    { title: "Pause the cluster", where: "the management cluster", command: "kubectl patch cluster web" },
    {
      title: "Stop etcd and the API server",
      where: "web-cp-a, web-cp-b, web-cp-c",
      command: "sudo bash <<'KUBEHENGE'\nsystemctl stop rke2-server\nKUBEHENGE",
    },
  ],
  replaced: ["web-cp-b", "web-cp-c"],
};

const signedIn = { "GET /api/session": { body: { user: "dev@pve", csrf: "c", canTrust: true } } };

const base = {
  ...signedIn,
  "GET /api/clusters/clusters/web": { body: snapshotting },
  "GET /api/clusters/clusters/web/backups": {
    body: { enabled: false, reachable: true, backups: [], restores: [], canBackUp: true, canRestore: true },
  },
};

describe("etcd snapshot settings", () => {
  it("say in words how they run, and refuse what the controller would", () => {
    expect(scheduleText("0 */12 * * *")).toBe("Every 12 hours from 00:00");
    expect(etcdSnapshotsText({ enabled: true, target: "store" })).toBe(
      "Every 12 hours from 00:00 (UTC), keeps the last 14, to store",
    );
    expect(etcdSnapshotsText({ enabled: false })).toBe("Off");
    const offered = [{ name: "store", bucket: "backups" }];
    expect(etcdProblems({ ...etcdFieldsOf({ enabled: true }), target: "store" }, offered)).toStrictEqual({});
    expect(etcdProblems({ ...etcdFieldsOf({ enabled: true }), keep: "0" }, offered)).toStrictEqual({
      target: "Choose a target.",
      keep: "A whole number from 1 to 1000.",
    });
  });

  it("allow a restore only from a snapshot of the cluster's distribution and Kubernetes minor", () => {
    const c: Cluster = snapshotting;
    expect(restoreProblem(c, snapshot)).toBeUndefined();
    expect(restoreProblem(c, { ...snapshot, kubernetesVersion: "v1.35.2+rke2r1" })).toBe(
      "The snapshot holds the objects of Kubernetes 1.35 and the cluster runs 1.36: restore one taken on 1.36.",
    );
    expect(restoreProblem(c, { ...snapshot, distribution: "k3s" })).toMatch(/of a k3s cluster, and this one is RKE2/u);
  });

  it("are switched on in a cluster's options", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/clusters/web": { body: cluster },
      "GET /api/classes": { body: [rke2] },
      "GET /api/clusters/clusters/web/credentials/backup": { body: { purpose: "backup", set: true, canChange: true } },
      "PATCH /api/clusters/clusters/web": { body: snapshotting },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit etcd snapshots" }));
    const dialog = screen.getByRole("dialog", { name: "etcd snapshots" });
    await user.click(within(dialog).getByLabelText("Take etcd snapshots"));
    expect(within(dialog).getByText("Choose a target.")).toBeInTheDocument();
    await user.selectOptions(within(dialog).getByLabelText("Target"), "store");
    expect(within(dialog).getByLabelText("Schedule")).toHaveAccessibleDescription(/Every 12 hours from 00:00/u);
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: {
        etcdSnapshots: { enabled: true, target: "store", schedule: "0 */12 * * *", timeZone: "UTC", keep: 14 },
      },
    });
  });
});

describe("a cluster's etcd snapshots", () => {
  it("are listed, taken at once and downloaded", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    const calls = fakeServer({
      ...base,
      "GET /api/clusters/clusters/web/etcd-snapshots": { body: view },
      "POST /api/clusters/clusters/web/etcd-snapshots": { status: 202, body: { by: "dev@pve", at: taken } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(
      screen.findByText("Every 6 hours from 00:00 (UTC), keeps the last 10, to store"),
    ).resolves.toBeInTheDocument();
    expect(screen.getByText("Last snapshot").nextElementSibling).toHaveTextContent(/^Taken/u);
    expect(screen.getByRole("link", { name: `Download ${snapshot.name}` })).toHaveAttribute(
      "href",
      `/api/clusters/clusters/web/etcd-snapshots/${encodeURIComponent(snapshot.name)}`,
    );
    // The older snapshot is of another Kubernetes minor, which the cluster's API server may not read.
    expect(screen.getByRole("button", { name: "Restore from 20261021T000000Z-v1.35.2+rke2r1.db.age" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Take a snapshot now" }));
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("POST /api/clusters/clusters/web/etcd-snapshots"));
  });

  it("restore through the guest agent once the cluster's name is typed", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    const calls = fakeServer({
      ...base,
      "GET /api/clusters/clusters/web/etcd-snapshots": { body: view },
      "POST /api/clusters/clusters/web/etcd-restores": { status: 202, body: { snapshot: snapshot.name } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: `Restore from ${snapshot.name}` }));
    const dialog = screen.getByRole("dialog", { name: /^Restore etcd from/u });
    expect(dialog).toHaveTextContent("The data in volumes stays as it is now.");
    expect(dialog).toHaveTextContent("lost its etcd quorum");
    const restore = within(dialog).getByRole("button", { name: "Restore" });
    expect(restore).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Confirm name"), "web");
    await user.click(restore);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.findLast((c) => c.key === "POST /api/clusters/clusters/web/etcd-restores")?.body).toStrictEqual({
      snapshot: snapshot.name,
    });
  });

  it("list the steps to restore by hand where the controller does not reach the guest agent", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    const calls = fakeServer({
      ...base,
      "GET /api/clusters/clusters/web/etcd-snapshots": { body: { ...view, automatic: false } },
      [`GET /api/clusters/clusters/web/etcd-snapshots/${encodeURIComponent(snapshot.name)}/restore-plan`]: {
        body: plan,
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: `Restore from ${snapshot.name}` }));
    const dialog = screen.getByRole("dialog", { name: /^Restore etcd from/u });
    await expect(within(dialog).findByLabelText("Stop etcd and the API server")).resolves.toHaveTextContent(
      "systemctl stop rke2-server",
    );
    expect(within(dialog).getByText(/On web-cp-a, web-cp-b, web-cp-c/u)).toBeInTheDocument();
    expect(within(dialog).getByRole("link", { name: "Download the snapshot" })).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Restore" })).not.toBeInTheDocument();
    expect(calls.map((c) => c.key)).not.toContain("POST /api/clusters/clusters/web/etcd-restores");
  });

  it("show a restore under way and say what downloading needs of those who may not", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    fakeServer({
      ...base,
      "GET /api/clusters/clusters/web/etcd-snapshots": {
        body: {
          ...view,
          canRestore: false,
          restoring: true,
          restore: {
            snapshot: snapshot.name,
            by: "root@pam",
            requestedAt: taken,
            phase: "Restoring",
            node: "web-cp-a",
            started: taken,
          },
        },
      },
    });
    render(<App />);

    await expect(screen.findByText("Restoring etcd on web-cp-a")).resolves.toBeInTheDocument();
    expect(screen.getByText(/Downloading a snapshot and restoring from one need Sys.Modify/u)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: `Download ${snapshot.name}` })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Take a snapshot now" })).toBeDisabled();
  });

  it("offer to be set up where they are off", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    fakeServer({
      ...base,
      "GET /api/clusters/clusters/web": { body: cluster },
      "GET /api/clusters/clusters/web/etcd-snapshots": {
        body: {
          enabled: false,
          requested: false,
          snapshots: [],
          restoring: false,
          automatic: true,
          canSnapshot: true,
          canRestore: true,
        } satisfies ClusterEtcdSnapshots,
      },
    });
    render(<App />);
    await expect(screen.findByText("Kubehenge takes no etcd snapshots of this cluster.")).resolves.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set up etcd snapshots…" })).toBeEnabled();
  });
});
