import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { fakeServer } from "./fake-server";
import { Templates, buildProblems, buildRequest } from "./Templates";

const form = {
  distribution: "kubeadm" as const,
  kubernetesVersion: "v1.37.1",
  node: "pve1",
  storage: "ceph",
  importStorage: "local",
  isoStorage: "",
  bridge: "vmbr0",
  vlan: "",
  buildAddress: "",
  buildGateway: "",
  buildDNS: "",
};

describe("vM templates", () => {
  it("checks and builds the request", () => {
    expect(buildProblems(form)).toStrictEqual({});
    expect(buildProblems({ ...form, kubernetesVersion: "1.37" })).toStrictEqual({
      kubernetesVersion: "A version such as v1.36.5.",
    });
    expect(buildProblems({ ...form, buildAddress: "192.0.2.50/24" })).toStrictEqual({
      buildAddress: "A static address needs its prefix length, a gateway and a DNS server.",
    });
    expect(buildRequest({ ...form, distribution: "rke2", vlan: "30" })).toStrictEqual({
      distribution: "rke2",
      node: "pve1",
      storage: "ceph",
      importStorage: "local",
      bridge: "vmbr0",
      vlan: 30,
    });
    expect(
      buildRequest({
        ...form,
        buildAddress: "192.0.2.50/24",
        buildGateway: "192.0.2.1",
        buildDNS: "192.0.2.1, 192.0.2.2",
      }),
    ).toMatchObject({
      kubernetesVersion: "v1.37.1",
      buildAddress: "192.0.2.50/24",
      buildGateway: "192.0.2.1",
      buildDNS: ["192.0.2.1", "192.0.2.2"],
    });
  });

  it("lists templates, keeps those in use and builds a new one", async () => {
    const calls = fakeServer({
      "GET /api/templates": {
        body: {
          templates: [
            { vmid: 9001, node: "pve1", name: "kubehenge-ubuntu-26.04", tags: [], inUse: "default/web" },
            { vmid: 9002, node: "pve1", name: "kubehenge-kubeadm-v1.36.5-ubuntu-26.04", tags: [] },
          ],
          job: {
            user: "root@pam",
            kind: "template",
            started: new Date().toISOString(),
            state: "failed",
            error: "no import storage",
            log: ["Building"],
          },
        },
      },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
      "GET /api/proxmox/storages": {
        body: [
          {
            storage: "local",
            node: "pve1",
            shared: false,
            content: ["import", "iso"],
            avail: 0,
            total: 0,
            usable: false,
          },
          { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 0, total: 0, usable: true },
          { storage: "ceph", node: "pve1", shared: true, content: ["images"], avail: 0, total: 0, usable: true },
        ],
      },
      "GET /api/proxmox/nodes/pve1/bridges": {
        body: [{ bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true }],
      },
      "POST /api/templates": { status: 202 },
    });
    const sent = (): unknown => calls.find((c) => c.key === "POST /api/templates")?.body;
    const user = userEvent.setup({ delay: null });
    render(<Templates />);

    const inUse = within(await screen.findByRole("row", { name: /kubehenge-ubuntu-26.04/u }));
    expect(inUse.getByRole("button", { name: "Delete kubehenge-ubuntu-26.04" })).toBeDisabled();
    expect(screen.getByText(/failed: no import storage/u)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Build a template" }));
    const dialog = screen.getByRole("dialog", { name: "Build a template" });
    await user.selectOptions(within(dialog).getByLabelText("For"), "kubeadm");
    await user.type(within(dialog).getByLabelText("Kubernetes version"), "v1.37.1");
    await waitFor(() => expect(within(dialog).getByRole("option", { name: "ceph" })).toBeInTheDocument());
    // Shared storage comes first.
    const disks = within(within(dialog).getByLabelText("Template disk"))
      .getAllByRole("option")
      .map((o) => o.textContent);
    expect(disks).toStrictEqual(["Choose…", "ceph", "local-lvm"]);
    await user.selectOptions(within(dialog).getByLabelText("Template disk"), "ceph");
    await user.selectOptions(within(dialog).getByLabelText("Downloaded image"), "local");
    await waitFor(() => expect(within(dialog).getByRole("option", { name: "vmbr0" })).toBeInTheDocument());
    // The node's only bridge is chosen, and nothing untouched is flagged.
    expect(within(dialog).getByLabelText("Bridge")).toHaveValue("vmbr0");
    expect(within(dialog).getByLabelText("Bridge")).not.toHaveAttribute("aria-invalid");
    await user.click(within(dialog).getByRole("button", { name: "Build" }));
    await waitFor(() =>
      expect(sent()).toStrictEqual({
        distribution: "kubeadm",
        kubernetesVersion: "v1.37.1",
        node: "pve1",
        storage: "ceph",
        importStorage: "local",
        bridge: "vmbr0",
      }),
    );
  });
});
