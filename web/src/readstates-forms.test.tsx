import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { BackupsDialog } from "./Backups";
import { OptionDialog } from "./ClusterOptions";
import { CredentialField } from "./CredentialField";
import { EtcdSnapshotsDialog } from "./EtcdSnapshots";
import { fakeServer, held } from "./fake-server";
import { signedIn, unreadable, web } from "./readstates-fixtures";
import type { Cluster } from "./types";

const noop = (): void => {
  // The dialog stays open for the test to read.
};

const backedUp: Cluster = {
  ...web,
  variables: {
    ...web.variables,
    backups: { enabled: true, target: "offsite", schedule: "0 1 * * *", timeZone: "UTC", retentionDays: 7 },
  },
};

// unreadForm waits for a dialog whose form needs a read that failed to show the error with a retry
// and none of the form's fields.
async function unreadForm(title: string): Promise<HTMLElement> {
  const shown = await screen.findByRole("dialog", { name: title });
  const dialog = within(shown);
  await expect(dialog.findByRole("button", { name: "Try now" })).resolves.toBeInTheDocument();
  expect(dialog.getByRole("alert")).toHaveTextContent("did not answer");
  expect(dialog.queryByRole("combobox")).not.toBeInTheDocument();
  return shown;
}

describe("a form made from a read that failed or has not answered", () => {
  it("shows the backups dialog's failed classes read in place of its form", async () => {
    fakeServer({ "GET /api/classes": unreadable });
    render(<BackupsDialog cluster={backedUp} onClose={noop} />);
    const dialog = await unreadForm("Edit backups");
    expect(within(dialog).queryByRole("button", { name: "Add to plan" })).not.toBeInTheDocument();
    expect(screen.queryByText(/no longer offered/u)).not.toBeInTheDocument();
  });

  it("offers the backups dialog no save while the classes are read", async () => {
    fakeServer({ "GET /api/classes": held().route });
    render(<BackupsDialog cluster={backedUp} onClose={noop} />);
    const dialog = within(await screen.findByRole("dialog", { name: "Edit backups" }));
    expect(dialog.queryByRole("button", { name: "Add to plan" })).not.toBeInTheDocument();
    expect(dialog.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });

  it("shows the etcd snapshots dialog's failed classes read in place of its form", async () => {
    fakeServer({ "GET /api/classes": unreadable });
    render(<EtcdSnapshotsDialog cluster={web} onClose={noop} />);
    const dialog = await unreadForm("Edit etcd snapshots");
    expect(within(dialog).queryByRole("button", { name: "Add to plan" })).not.toBeInTheDocument();
  });

  it("shows the upgrades dialog's failed classes read in place of its form", async () => {
    fakeServer({ "GET /api/classes": unreadable });
    render(<OptionDialog action="upgrades" cluster={web} onClose={noop} />);
    const dialog = await unreadForm("Edit automatic upgrades");
    expect(within(dialog).queryByRole("button", { name: "Add to plan" })).not.toBeInTheDocument();
  });

  it("shows the release mirror dialog's failed classes read in place of its form", async () => {
    fakeServer({ "GET /api/classes": unreadable });
    render(<OptionDialog action="releaseMirror" cluster={web} onClose={noop} />);
    const dialog = await unreadForm("Edit release mirror");
    expect(within(dialog).queryByRole("button", { name: "Add to plan" })).not.toBeInTheDocument();
  });

  it("shows the node OS dialog's failed classes read with a retry, and no save", async () => {
    fakeServer({ "GET /api/classes": unreadable });
    render(<OptionDialog action="nodeOS" cluster={web} onClose={noop} />);
    const dialog = await unreadForm("Edit node OS");
    expect(within(dialog).queryByRole("button", { name: "Add to plan" })).not.toBeInTheDocument();
  });

  it("names the site's zone in the upgrades row where the window names none", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/options");
    const upgrading: Cluster = {
      ...web,
      variables: { ...web.variables, upgrades: { mode: "patch", window: { weekdays: ["Sat"], start: "02:00" } } },
    };
    fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": { body: upgrading },
      "GET /api/classes": {
        body: [
          {
            name: web.class,
            distribution: "rke2",
            variables: [],
            annotations: { "kubehenge.dev/time-zone": "Europe/Zurich" },
          },
        ],
      },
    });
    render(<App />);
    const row = await screen.findByRole("row", { name: /^Automatic upgrades/u });
    expect(row).toHaveTextContent("Europe/Zurich");
    expect(row).not.toHaveTextContent("UTC");
  });

  it("reads a credential field's new purpose at once", async () => {
    fakeServer({
      "GET /api/clusters/default/web/credentials/log-a": { body: { purpose: "log-a", set: false, canChange: true } },
      "GET /api/clusters/default/web/credentials/log-loki": {
        body: { purpose: "log-loki", set: true, setBy: "alice@pve", canChange: true },
      },
    });
    const keys = [{ key: "password", label: "Password" }];
    const { rerender } = render(<CredentialField label="Password" purpose="log-a" cluster={web} keys={keys} />);
    await expect(screen.findByText("Not set")).resolves.toBeInTheDocument();
    rerender(<CredentialField label="Password" purpose="log-loki" cluster={web} keys={keys} />);
    await expect(screen.findByText("Set by alice@pve")).resolves.toBeInTheDocument();
  });
});
