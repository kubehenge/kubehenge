import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { CredentialField } from "./CredentialField";
import { fakeServer, held, type Route } from "./fake-server";
import { ManagementBackups } from "./ManagementBackups";
import { created, readAgain, signedIn, unreadable, web } from "./readstates-fixtures";
import type { Cluster, ClusterBackups } from "./types";

const dash = String.fromCodePoint(0x20_13);

const backedUp: Cluster = {
  ...web,
  variables: {
    ...web.variables,
    backups: { enabled: true, target: "store", schedule: "0 1 * * *", timeZone: "UTC", retentionDays: 7 },
  },
};

const velero: ClusterBackups = {
  enabled: true,
  target: "store",
  reachable: true,
  location: "Available",
  lastBackup: created,
  backups: [],
  restores: [],
  sources: [],
  attention: [],
  canBackUp: true,
  canDelete: true,
  canRestore: true,
};

// tryNow is the retry a failed read offers, where it shows.
const tryNow = (where: Pick<typeof screen, "findByRole">): Promise<HTMLElement> =>
  where.findByRole("button", { name: "Try now" });

// retryBeside is the retry under the error of the read that did not answer.
async function retryBeside(where: Pick<typeof screen, "findAllByRole">): Promise<HTMLElement> {
  const alerts = await where.findAllByRole("alert");
  const failed = alerts.find((a) => a.textContent.includes("did not answer"));
  const box = failed?.parentElement;
  if (box?.classList.contains("read-error") !== true) {
    throw new Error("no retry under the error of the failed read");
  }
  return within(box).getByRole("button", { name: "Try now" });
}

describe("what a page shows of a failed or pending read, once more", () => {
  it("calls the Velero target unknown and backing up off, with the reason, once a re-read fails", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/backups");
    const routes: Record<string, Route> = {
      ...signedIn,
      "GET /api/clusters": { body: [backedUp] },
      "GET /api/clusters/default/web": { body: backedUp },
      "GET /api/clusters/default/web/backups": { body: velero },
    };
    fakeServer(routes);
    render(<App />);
    await expect(screen.findByText("Reachable")).resolves.toBeInTheDocument();
    routes["GET /api/clusters/default/web/backups"] = unreadable;
    await readAgain();
    await expect(screen.findByText("Backups not read.")).resolves.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Back up now" })).toBeDisabled();
    expect(screen.queryByText("Reachable")).not.toBeInTheDocument();
  });

  it("calls a cluster unknown that the last health answer left out, once reading it again fails", async () => {
    window.history.replaceState(null, "", "/");
    const routes: Record<string, Route> = { ...signedIn, "GET /api/fleet/health": { body: [] } };
    fakeServer(routes);
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await main.findByRole("row", { name: /^web.*Loading/u });
    routes["GET /api/fleet/health"] = unreadable;
    await readAgain();
    await expect(main.findByRole("row", { name: /^web.*Unknown/u })).resolves.toBeInTheDocument();
  });

  it("counts none as needing someone or healthy while the health is read", async () => {
    window.history.replaceState(null, "", "/");
    const pending = held();
    fakeServer({ ...signedIn, "GET /api/fleet/health": pending.route });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await expect(main.findByRole("button", { name: `Needs you ${dash}` })).resolves.toBeInTheDocument();
    expect(main.getByRole("button", { name: `Healthy ${dash}` })).toBeInTheDocument();
  });

  it("offers a retry when the cluster's own read fails", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer({ ...signedIn, "GET /api/clusters/default/web": unreadable });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await user.click(await main.findByRole("link", { name: "web" }));
    await waitFor(async () => {
      await expect(retryBeside(main)).resolves.toBeInTheDocument();
    });
  });

  it("calls the machines unknown, not loading, when the cluster's read failed", async () => {
    window.history.replaceState(null, "", "/");
    const { machines: _machines, ...listed } = web;
    fakeServer({ ...signedIn, "GET /api/clusters": { body: [listed] }, "GET /api/clusters/default/web": unreadable });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await user.click(await main.findByRole("link", { name: "web" }));
    await user.click(await main.findByRole("link", { name: /^Machines/u }));
    await expect(main.findByRole("cell", { name: "Unknown" })).resolves.toBeInTheDocument();
  });

  it("shows a failed read of the classes, and keeps the options it would have offered", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/options");
    fakeServer({ ...signedIn, "GET /api/classes": unreadable });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await expect(main.findByRole("row", { name: /^Backups/u })).resolves.toBeInTheDocument();
    await expect(tryNow(main)).resolves.toBeInTheDocument();
  });

  it("shows a failed setup read on Administration with a retry, never as no right to it", async () => {
    window.history.replaceState(null, "", "/administration");
    fakeServer({ ...signedIn, "GET /api/setup": unreadable });
    render(<App />);
    const main = within(await screen.findByRole("main"));
    await expect(main.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    await expect(tryNow(main)).resolves.toBeInTheDocument();
    expect(screen.queryByText("Administrators only")).not.toBeInTheDocument();
  });

  it("shows the rail's loading look while the clusters are read", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const pending = held();
    fakeServer({ ...signedIn, "GET /api/clusters": pending.route });
    render(<App />);
    const rail = within(await screen.findByRole("navigation", { name: "Fleet" }));
    expect(rail.getByText("Loading…")).toBeInTheDocument();
  });

  it("shows a failed health read in the rail with a retry", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({ ...signedIn, "GET /api/fleet/health": unreadable });
    render(<App />);
    const rail = within(await screen.findByRole("navigation", { name: "Fleet" }));
    await expect(rail.findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(rail.getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });

  it("offers a retry when the management snapshots fail", async () => {
    fakeServer({
      "GET /api/appliance/etcd-backups": {
        body: {
          settings: { target: "minio", everyHours: 6, keep: 28, by: "root@pam", since: created },
          targets: ["minio"],
          keysStored: true,
        },
      },
      "GET /api/appliance/etcd-backups/snapshots": unreadable,
    });
    render(<ManagementBackups running={false} />);
    await expect(tryNow(screen)).resolves.toBeInTheDocument();
  });

  it("offers a retry when a stored credential fails", async () => {
    fakeServer({ "GET /api/credentials/backup": unreadable });
    render(<CredentialField label="Keys" purpose="backup" keys={[{ key: "id", label: "Key ID" }]} />);
    await expect(tryNow(screen)).resolves.toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText("Loading…")).not.toBeInTheDocument());
  });
});
