import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { applyStaged, fakeServer } from "./fake-server";
import type { HARules } from "./types";

const cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { allowedNodes: ["pve9"] },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const readRules: HARules = { readable: true, rules: [{ rule: "fast", nodes: ["pve2"], allowed: ["pve2"] }] };

async function openPlacement(haRules: HARules = readRules): Promise<{
  dialog: HTMLElement;
  sent: () => unknown;
  user: ReturnType<typeof userEvent.setup>;
}> {
  window.history.replaceState(null, "", "/clusters/clusters/web/options");
  const calls = fakeServer({
    "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
    "GET /api/clusters/clusters/web": { body: cluster },
    "GET /api/proxmox/nodes": {
      body: [
        { node: "pve1", online: true },
        { node: "pve2", online: false },
      ],
    },
    "GET /api/proxmox/ha-rules": { body: haRules },
    "PATCH /api/clusters/clusters/web": { body: cluster },
  });
  const user = userEvent.setup({ delay: null });
  render(<App />);
  const heading = await screen.findByRole("rowheader", { name: "Placement" });
  expect(heading.closest("tr")).toHaveTextContent("pve9");
  await user.click(screen.getByRole("button", { name: "Edit placement" }));
  const dialog = screen.getByRole("dialog", { name: "Placement" });
  await expect(within(dialog).findByLabelText(/^pve1/u)).resolves.toBeInTheDocument();
  return { dialog, sent: () => calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body, user };
}

describe("a cluster's placement", () => {
  it("keeps the nodes it has, those the user cannot see too, and saves only a change", async () => {
    const { dialog, sent, user } = await openPlacement();
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(within(dialog).getByLabelText("Nodes I choose")).toBeChecked();
    expect(within(dialog).getByLabelText("Nodes I choose")).toHaveFocus();
    expect(within(dialog).getByRole("group", { name: "Placement" })).toBeInTheDocument();
    expect(within(dialog).getAllByText("Placement")).toHaveLength(1);
    expect(within(dialog).getByLabelText(/^pve9 \(not shown to you\)$/u)).toBeChecked();
    expect(within(dialog).getByLabelText(/^pve2 \(offline\)$/u)).not.toBeChecked();
    expect(save).toBeDisabled();
    await user.click(within(dialog).getByLabelText(/^pve9/u));
    expect(save).toBeDisabled();
    expect(within(dialog).getByRole("alert")).toHaveTextContent("Choose at least one node.");
    await user.click(within(dialog).getByLabelText(/^pve1/u));
    await user.click(within(dialog).getByLabelText(/^pve9/u));
    await user.click(save);
    await applyStaged(user);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { allowedNodes: ["pve1", "pve9"] } }));
  });

  it("moves new VMs to the nodes of an HA rule, chosen again after another choice", async () => {
    const { dialog, sent, user } = await openPlacement();
    await user.click(within(dialog).getByLabelText("The nodes of an HA rule"));
    await user.selectOptions(within(dialog).getByLabelText("HA rule"), "fast");
    await user.click(within(dialog).getByLabelText("Any node"));
    await user.click(within(dialog).getByLabelText("The nodes of an HA rule"));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await applyStaged(user);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { allowedNodes: ["pve2"] } }));
  });

  it("takes the nodes of an HA rule offered to those who may not read the rules", async () => {
    const { dialog, sent, user } = await openPlacement({
      readable: true,
      offered: true,
      rules: [{ rule: "racks-a", nodes: ["pve1", "pve2"], allowed: ["pve1", "pve2"] }],
    });
    await user.click(within(dialog).getByLabelText("The nodes of an HA rule"));
    const picker = within(dialog).getByLabelText("HA rule");
    expect(within(picker).getByRole("option", { name: "racks-a: pve1, pve2" })).toBeInTheDocument();
    await user.selectOptions(picker, "racks-a");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await applyStaged(user);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { allowedNodes: ["pve1", "pve2"] } }));
  });

  it("says how the HA rules are read where the user may not", async () => {
    const { dialog } = await openPlacement({ readable: false, rules: [] });
    expect(within(dialog).getByLabelText("The nodes of an HA rule")).toBeDisabled();
    expect(dialog).toHaveTextContent("takes Sys.Audit on /, unless an administrator offers them.");
  });

  it("lets new VMs go to any node", async () => {
    const { dialog, sent, user } = await openPlacement();
    await user.click(within(dialog).getByLabelText("Any node"));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await applyStaged(user);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { allowedNodes: null } }));
  });
});
