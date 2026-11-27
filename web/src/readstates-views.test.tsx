import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { BackupsView } from "./Backups";
import { EtcdSnapshotsView } from "./EtcdSnapshots";
import { fakeServer } from "./fake-server";
import { HealthView } from "./Health";
import { ManagementHealthCard } from "./ManagementHealth";
import { unreadable, web } from "./readstates-fixtures";
import type { ClusterEtcdSnapshots } from "./types";

const snapshotsView: ClusterEtcdSnapshots = {
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

describe("what a cluster's views show of a failed read", () => {
  it("says no etcd snapshot was taken only when the target was read", async () => {
    fakeServer({
      "GET /api/clusters/default/web/etcd-snapshots": {
        body: { ...snapshotsView, message: "The backup volume could not be read" },
      },
    });
    render(<EtcdSnapshotsView cluster={web} can onEdit={vi.fn<() => void>()} />);
    await screen.findByText("Last taken");
    expect(screen.getByText("Unknown")).toBeInTheDocument();
    expect(screen.queryByText("None yet")).not.toBeInTheDocument();
  });

  it("offers a retry when the etcd snapshots' first read fails", async () => {
    fakeServer({ "GET /api/clusters/default/web2/etcd-snapshots": unreadable });
    render(<EtcdSnapshotsView cluster={{ ...web, name: "web2" }} can onEdit={vi.fn<() => void>()} />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(screen.getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });

  it("says no backup was taken only while Velero is read", async () => {
    fakeServer({
      "GET /api/clusters/default/web/backups": {
        body: {
          enabled: true,
          target: "store",
          reachable: false,
          message: "Velero did not answer",
          backups: [],
          restores: [],
          sources: [],
          attention: [],
          canBackUp: true,
          canDelete: true,
          canRestore: true,
        },
      },
    });
    render(<BackupsView cluster={web} can onEdit={vi.fn<() => void>()} />);
    const last = await screen.findByText("Last scheduled backup");
    expect(last.nextElementSibling).toHaveTextContent("Unknown");
  });

  it("offers a retry when a cluster's backups are not read", async () => {
    fakeServer({ "GET /api/clusters/default/web3/backups": unreadable });
    render(<BackupsView cluster={{ ...web, name: "web3" }} can onEdit={vi.fn<() => void>()} />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(screen.getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });

  it("offers a retry when a cluster's health is not read", async () => {
    fakeServer({ "GET /api/clusters/default/web4/health": unreadable });
    render(<HealthView cluster={{ ...web, name: "web4" }} headlamp={undefined} />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(screen.getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });

  it("offers a retry when the management cluster's health is not read", async () => {
    fakeServer({ "GET /api/setup/health": unreadable, "GET /api/setup/reboot-ready": unreadable });
    render(<ManagementHealthCard />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(screen.getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });
});
