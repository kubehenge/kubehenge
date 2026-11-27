import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { Options } from "./ClusterOptions";
import { datastoreAllocateAnnotation } from "./create-form";
import { fakeServer, held, type Route } from "./fake-server";
import { haAnnotation } from "./Placement";
import { signedIn, unreadable, web } from "./readstates-fixtures";
import type { Cluster, ClusterClass } from "./types";

const noop = (): void => {
  // The rows' edits are not followed here.
};

// A class that offers Proxmox VE HA and the CSI plugin's newest release, which wait on the storages.
const offering: ClusterClass = {
  name: web.class,
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  annotations: { [haAnnotation]: "true", [datastoreAllocateAnnotation]: "true" },
  variables: [
    { name: "proxmoxHA", required: false, schema: { type: "boolean" } },
    { name: "csi", required: false, schema: { type: "object" } },
  ],
};

const onCeph: Cluster = { ...web, variables: { ...web.variables, storage: "ceph" } };

function openOptions(storages: Route): void {
  window.history.replaceState(null, "", "/clusters/default/web/options");
  fakeServer({
    ...signedIn,
    "GET /api/clusters/default/web": { body: onCeph },
    "GET /api/classes": { body: [offering] },
    "GET /api/proxmox/storages": storages,
  });
  render(<App />);
}

describe("an option row whose own read has not answered", () => {
  it("shows the HA row's failed storages read with a retry, then the switch once it answers", async () => {
    const storages: Route = { ...unreadable };
    openOptions(storages);
    const row = await screen.findByRole("row", { name: /^Proxmox VE HA/u });
    await expect(within(row).findByRole("alert")).resolves.toHaveTextContent("did not answer");
    Object.assign(storages, { status: 200, body: [{ storage: "ceph", shared: true }] });
    const user = userEvent.setup({ delay: null });
    await user.click(within(row).getByRole("button", { name: "Try now" }));
    await expect(screen.findByRole("switch", { name: "Proxmox VE HA" })).resolves.toBeInTheDocument();
  });

  it("shows the HA and CSI rows' loading look while the storages are read", async () => {
    openOptions(held().route);
    const ha = await screen.findByRole("row", { name: /^Proxmox VE HA/u });
    expect(within(ha).getByText("Loading…")).toBeInTheDocument();
    const csi = screen.getByRole("row", { name: /^CSI plugin/u });
    expect(within(csi).getByText("Loading…")).toBeInTheDocument();
    expect(screen.queryByRole("switch", { name: "Proxmox VE HA" })).not.toBeInTheDocument();
  });

  it("shows the CSI row's failed storages read with a retry", async () => {
    openOptions(unreadable);
    const row = await screen.findByRole("row", { name: /^CSI plugin/u });
    await expect(within(row).findByRole("alert")).resolves.toHaveTextContent("did not answer");
    expect(within(row).getByRole("button", { name: "Try now" })).toBeInTheDocument();
  });

  it("shows the Volume snapshots row's failed classes read with a retry", async () => {
    fakeServer({ "GET /api/classes": unreadable, "GET /api/proxmox/storages": { body: [] } });
    render(<Options cluster={onCeph} can onEdit={noop} />);
    const row = await screen.findByRole("row", { name: /^Volume snapshots/u });
    await waitFor(() => expect(within(row).getByRole("alert")).toHaveTextContent("did not answer"));
    expect(within(row).getByRole("button", { name: /Try now|Retry/u })).toBeInTheDocument();
  });
});
