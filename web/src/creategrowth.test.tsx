import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { capacityProblems, createRoom, newForm } from "./create-form";
import { CreateChecks } from "./CreateCheck";
import { fakeServer } from "./fake-server";
import type { ClusterClass, PveNode, Storage } from "./types";

const gib = 2 ** 30;

const cls: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: [{ name: "autoscaler", required: false, schema: { type: "object" } }],
};

// Thick LVM with 100 GiB free, the only storage, so the wizard chooses it.
const lvm: Storage = {
  storage: "local-lvm",
  node: "pve1",
  content: ["images"],
  active: true,
  total: 1024 * gib,
  avail: 100 * gib,
  usable: true,
  allocatable: true,
  shared: false,
  type: "lvm",
};

const nodes: PveNode[] = [{ node: "pve1", online: true }];

// The controller's AutoscalerNoRoom event for one control plane and five workers of 30 GiB.
const growth =
  "Storage local-lvm (on pve1) reserves each disk's whole size and has 100 GiB free. " +
  "At the autoscaler's maximum, the machines still to be made need 180 GiB. " +
  "A worker past that room waits, and the autoscaler gives it up after 30 minutes";

const scaled = (least: number): ReturnType<typeof newForm> => ({
  ...newForm(),
  storage: "local-lvm",
  workerAutoscale: true,
  workerReplicas: least,
  workerMax: 5,
});

describe("an autoscaled pool on thick storage", () => {
  it("warns on the most workers where only the maximum does not fit", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/classes": () => ({ body: [cls] }),
      "GET /api/versions/rke2": () => ({ body: { versions: ["v1.36.5+rke2r1"], stable: "v1.36.5+rke2r1" } }),
      "GET /api/clusters": () => ({ body: [] }),
      "GET /api/proxmox/storages": () => ({ body: [lvm] }),
      "GET /api/proxmox/nodes": () => ({ body: nodes }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findByRole("option", { name: /rke2/iu });
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-rke2");
    await user.selectOptions(await screen.findByLabelText("Worker count"), "autoscaled");
    const most = screen.getByLabelText("Most workers");
    await user.clear(most);
    await user.type(most, "5");
    await waitFor(() => expect(most).toHaveAccessibleDescription(growth));
    expect(most).not.toHaveAttribute("aria-invalid");
  });

  it("is created with the warning on Confirm, and refused only where its minimum does not fit", () => {
    const grows = scaled(1);
    expect(capacityProblems("Machines", grows, { nodes, storages: [lvm] })).toStrictEqual({});
    render(<CreateChecks read={undefined} room={createRoom(grows, nodes, [lvm])} />);
    expect(screen.getByLabelText("Before creating")).toHaveTextContent(`Most workers${growth}`);
    // One control plane and four workers of 30 GiB to start: 150 GiB.
    const starts = scaled(4);
    expect(capacityProblems("Machines", starts, { nodes, storages: [lvm] })).toStrictEqual({
      workerMachine: "The disks need 150.0 GiB, 100.0 GiB free. Lower a disk size or the node count.",
    });
    expect(createRoom(starts, nodes, [lvm]).growthWarning).toBeUndefined();
  });
});
