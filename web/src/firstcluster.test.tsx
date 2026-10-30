import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { ClusterList } from "./ClusterList";
import { fakeServer } from "./fake-server";
import { withTemplate } from "./FirstCluster";
import type { ClusterClass, NodeOSChoice } from "./types";

const version = { pattern: "^v1" };
const alma: NodeOSChoice = {
  family: "almalinux",
  release: "10",
  title: "AlmaLinux 10",
  cpuType: "x86-64-v2-AES",
  mac: "selinux",
};
const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: version,
  variables: [],
  nodeOSes: [
    { ...alma, noTemplate: "No template" },
    { ...alma, family: "rocky", title: "Rocky Linux 10" },
  ],
};
const k3s: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: version,
  variables: [],
  nodeOSes: [{ ...alma, noTemplate: "No template" }],
};
const talos: ClusterClass = { name: "kubehenge-talos", distribution: "talos", versionFormat: version, variables: [] };

function list(classes: ClusterClass[], may: { canCreate?: boolean; canManage?: boolean }): void {
  fakeServer({
    "GET /api/clusters": () => ({ body: [] }),
    "GET /api/fleet/health": () => ({ body: [] }),
    "GET /api/classes": () => ({ body: classes }),
  });
  render(<ClusterList canCreate={may.canCreate} canManage={may.canManage ?? false} />);
}

describe("the fleet list before the first cluster", () => {
  it("names the distributions with a VM template on some node OS", () => {
    expect(withTemplate([rke2, k3s, talos])).toStrictEqual(["RKE2", "Talos"]);
    expect(withTemplate([k3s, { ...talos, noTemplate: "No template" }])).toStrictEqual([]);
  });

  it("says what a first cluster needs, beside the button that creates it", async () => {
    list([rke2, k3s], {});
    const checks = await screen.findByLabelText("First cluster");
    await expect(within(checks).findByText("RKE2")).resolves.toHaveClass("status-ok");
    expect(within(checks).getByText("May create clusters")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create cluster" })).toBeEnabled();
  });

  it("sends administrators to build a template, and others nowhere", async () => {
    list([k3s], { canManage: true });
    const checks = await screen.findByLabelText("First cluster");
    await expect(within(checks).findByRole("link", { name: "Build one" })).resolves.toHaveAttribute(
      "href",
      "/administration/templates",
    );
  });

  it("names the privilege a creator lacks", async () => {
    list([k3s], { canCreate: false });
    const checks = await screen.findByLabelText("First cluster");
    await expect(within(checks).findByText("None yet", { selector: ".status-warning" })).resolves.toBeVisible();
    expect(within(checks).queryByRole("link")).not.toBeInTheDocument();
    expect(within(checks).getByText("Needs VM.Allocate")).toHaveClass("status-warning");
    expect(screen.getByRole("button", { name: "Create cluster" })).toBeDisabled();
    expect(screen.queryByText(/workload pool/u)).not.toBeInTheDocument();
  });
});
