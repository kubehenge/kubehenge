import { render, screen, type RenderResult } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { MeshPeersField } from "./ClusterMesh";
import { EtcdRestoreDialog } from "./EtcdSnapshots";
import { fakeServer, held, type Route } from "./fake-server";
import { PlacementDialog } from "./Placement";
import { created, readAgain, signedIn, unreadable, web } from "./readstates-fixtures";
import { ResultsDialog } from "./Restores";
import { ChecksList, useChecks } from "./Upgrade";

const noop = (): void => {
  // The dialog stays open for the test to read.
};

// tryNow is the retry a failed read offers.
const tryNow = (): Promise<HTMLElement> => screen.findByRole("button", { name: "Try now" });

function Checks(): ReturnType<typeof ChecksList> {
  return <ChecksList checks={useChecks({ before: web })} healthPath="/" label="Checks" />;
}

const renderRoutes = (routes: Record<string, Route>, page: Parameters<typeof render>[0]): RenderResult => {
  fakeServer(routes);
  return render(page);
};

describe("a read made once that failed", () => {
  it("offers a retry when the Cluster Mesh peers fail", async () => {
    renderRoutes(
      { "GET /api/clusters": unreadable },
      <MeshPeersField namespace="default" self="web" value={[]} onChange={noop} />,
    );
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when the placement dialog's nodes fail", async () => {
    renderRoutes(
      { "GET /api/proxmox/nodes": unreadable, "GET /api/proxmox/ha-rules": held().route },
      <PlacementDialog cluster={web} onClose={noop} />,
    );
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when the HA rules fail", async () => {
    renderRoutes(
      { "GET /api/proxmox/nodes": { body: [] }, "GET /api/proxmox/ha-rules": unreadable },
      <PlacementDialog cluster={web} onClose={noop} />,
    );
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when a restore's results fail", async () => {
    const restore = {
      name: "r1",
      backup: "b1",
      phase: "Completed",
      started: created,
      volumeData: false,
      items: 1,
      total: 1,
      warnings: 0,
      errors: 0,
      volumes: 0,
      volumesDone: 0,
      bytesDone: 0,
      bytesTotal: 0,
    };
    renderRoutes(
      { "GET /api/clusters/default/web/restores/r1/results": unreadable },
      <ResultsDialog cluster={web} restore={restore} onClose={noop} />,
    );
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when the steps of a restore by hand fail", async () => {
    const snapshot = { name: "s1", time: created };
    renderRoutes(
      { "GET /api/clusters/default/web/etcd-snapshots/s1/restore-plan": unreadable },
      <EtcdRestoreDialog cluster={web} snapshot={snapshot} automatic={false} onClose={noop} onDone={noop} />,
    );
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when a plan's health check cannot read the health", async () => {
    renderRoutes(
      { "GET /api/clusters/default/web/health": unreadable, "GET /api/proxmox/nodes": { body: [] } },
      <Checks />,
    );
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("offers a retry when the wizard's storages fail", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    renderRoutes(
      {
        ...signedIn,
        "GET /api/classes": { body: [] },
        "GET /api/proxmox/storages": unreadable,
        "GET /api/proxmox/nodes": { body: [] },
      },
      <App />,
    );
    await expect(tryNow()).resolves.toBeInTheDocument();
  });

  it("restores and deletes none of a Velero list whose read failed, and says why", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/backups");
    const backedUp = {
      ...web,
      variables: { ...web.variables, backups: { enabled: true, target: "store", schedule: "0 1 * * *" } },
    };
    const finished = { name: "b1", phase: "Completed", scheduled: true, started: created, items: 1, total: 1 };
    const routes: Record<string, Route> = {
      ...signedIn,
      "GET /api/clusters": { body: [backedUp] },
      "GET /api/clusters/default/web": { body: backedUp },
      "GET /api/clusters/default/web/backups": {
        body: {
          enabled: true,
          target: "store",
          reachable: true,
          location: "Available",
          lastBackup: created,
          backups: [finished],
          restores: [],
          sources: [],
          attention: [],
          canBackUp: true,
          canDelete: true,
          canRestore: true,
        },
      },
    };
    renderRoutes(routes, <App />);
    const restore = await screen.findByRole("button", { name: "Restore b1" });
    expect(restore).toBeEnabled();
    routes["GET /api/clusters/default/web/backups"] = unreadable;
    await readAgain();
    await expect(screen.findByText("Backups not read.")).resolves.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Restore b1" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Restore b1" })).toHaveAttribute("title", "Backups not read.");
    expect(screen.getByRole("button", { name: "Delete b1" })).toBeDisabled();
  });
});
