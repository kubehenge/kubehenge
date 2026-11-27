import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { fakeServer, held, type Route } from "./fake-server";
import { FleetProvider } from "./fleet";
import { ManagementBackups } from "./ManagementBackups";
import { Notifications } from "./Notifications";
import { created, readAgain, signedIn, unreadable } from "./readstates-fixtures";
import { DangerZone } from "./RemoveManagement";

describe("what Administration shows of a failed or pending read", () => {
  it("offers no removal of Kubehenge while the clusters are not read", async () => {
    fakeServer({ ...signedIn, "GET /api/clusters": unreadable });
    render(
      <FleetProvider>
        <DangerZone running={false} onStarted={vi.fn<() => void>()} />
      </FleetProvider>,
    );
    await expect(screen.findByText("Clusters not read.")).resolves.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove…" })).toBeDisabled();
    expect(screen.queryByText("Keeps the appliance")).not.toBeInTheDocument();
  });

  it("offers no removal of Kubehenge while the clusters are being read", async () => {
    const pending = held();
    fakeServer({ ...signedIn, "GET /api/clusters": pending.route });
    render(
      <FleetProvider>
        <DangerZone running={false} onStarted={vi.fn<() => void>()} />
      </FleetProvider>,
    );
    await expect(screen.findByText("Loading…")).resolves.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove…" })).toBeDisabled();
  });

  it("offers a retry when the management backups' first read fails", async () => {
    fakeServer({ "GET /api/appliance/etcd-backups": unreadable });
    render(<ManagementBackups running={false} />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(screen.getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });

  it("says why taking a management snapshot is off, and no snapshot as unknown, once a read fails", async () => {
    const routes: Record<string, Route> = {
      "GET /api/appliance/etcd-backups": {
        body: {
          settings: { target: "minio", everyHours: 6, keep: 28, by: "root@pam", since: created },
          targets: ["minio"],
          keysStored: true,
        },
      },
      "GET /api/appliance/etcd-backups/snapshots": { body: { snapshots: [] } },
    };
    fakeServer(routes);
    render(<ManagementBackups running={false} />);
    await expect(screen.findByText("None yet")).resolves.toBeInTheDocument();
    routes["GET /api/appliance/etcd-backups"] = unreadable;
    await readAgain();
    await expect(screen.findByText("Backups not read.")).resolves.toBeInTheDocument();
    expect(screen.getByText("Unknown")).toBeInTheDocument();
    expect(screen.queryByText("None yet")).not.toBeInTheDocument();
  });

  it("offers no test sending while no target is read", async () => {
    fakeServer({
      "GET /api/notifications": { body: [] },
      "GET /api/credentials/notify-webhook": unreadable,
      "GET /api/credentials/notify-ntfy": unreadable,
      "GET /api/credentials/notify-email": unreadable,
    });
    render(<Notifications />);
    await expect(screen.findByText("Targets not read.")).resolves.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Send test" })).toBeDisabled();
  });
});
