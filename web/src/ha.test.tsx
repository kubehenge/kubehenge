import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { newForm, request, summary, type FieldKey } from "./create-form";
import { MachinesStep, type StepProps } from "./CreateSteps";
import { fakeServer } from "./fake-server";
import { haAnnotation, offersHA } from "./Placement";
import type { ClusterClass, Storage } from "./types";

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  annotations: { [haAnnotation]: "true" },
  variables: [
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "proxmoxHA", required: false, schema: { type: "boolean", default: true } },
  ],
};

const storage = (name: string, shared: boolean): Storage => ({
  storage: name,
  node: "pve1",
  shared,
  content: ["images"],
  avail: 1,
  total: 2,
  usable: true,
  allocatable: false,
});

const storages = [storage("ceph", true), storage("local-lvm", false)];

const noProblems = new Map<FieldKey, string>();

describe("a new cluster's Proxmox VE HA", () => {
  it("is offered for a class it applies to, on shared storage", () => {
    expect([
      offersHA(rke2, storages, "ceph"),
      offersHA(rke2, storages, "local-lvm"),
      offersHA({ ...rke2, annotations: {} }, storages, "ceph"),
      offersHA({ ...rke2, variables: rke2.variables.slice(0, 1) }, storages, "ceph"),
    ]).toStrictEqual([true, false, false, false]);
  });

  it("is left to the class's default unless switched off", () => {
    const form = { ...newForm(), storage: "ceph" };
    expect(request(form, rke2).variables).not.toHaveProperty("proxmoxHA");
    expect(request({ ...form, proxmoxHA: false }, rke2).variables).toHaveProperty("proxmoxHA", false);
    expect(Object.fromEntries(summary({ ...form, proxmoxHA: false }, rke2, { ha: true }))["Proxmox VE HA"]).toBe("Off");
    expect(Object.fromEntries(summary(form, rke2))).not.toHaveProperty("Proxmox VE HA");
  });

  it("is asked for on the machines step only where it is offered", async () => {
    const set = vi.fn<StepProps["set"]>();
    const step = (haOffered: boolean): JSX.Element => (
      <MachinesStep
        form={{ ...newForm(), storage: "ceph" }}
        set={set}
        shown={(k) => noProblems.get(k)}
        cls={rke2}
        usableStorages={storages}
        placed={false}
        haOffered={haOffered}
        csi={{ offered: false, problem: undefined }}
        haGroups={{ offered: false, problem: undefined }}
        nodes={[]}
        rules={{ haRules: undefined, haRulesError: undefined }}
      />
    );
    const { rerender } = render(step(false));
    expect(screen.queryByLabelText(/Proxmox VE HA$/u)).toBeNull();
    rerender(step(true));
    const box = screen.getByLabelText(/Proxmox VE HA$/u);
    expect(box).toBeChecked();
    await userEvent.setup({ delay: null }).click(box);
    expect(set).toHaveBeenCalledWith("proxmoxHA", false);
  });
});

const cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { storage: "ceph" },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

function openOptions(classes: ClusterClass[]): () => unknown {
  window.history.replaceState(null, "", "/clusters/clusters/web/options");
  const calls = fakeServer({
    "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
    "GET /api/clusters/clusters/web": { body: cluster },
    "GET /api/classes": { body: classes },
    "GET /api/proxmox/storages": { body: storages },
    "PATCH /api/clusters/clusters/web": { body: cluster },
  });
  render(<App />);
  return () => calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body;
}

describe("a cluster's Proxmox VE HA", () => {
  it("is switched off and on under Options", async () => {
    const sent = openOptions([rke2]);
    const user = userEvent.setup({ delay: null });
    const heading = await screen.findByRole("rowheader", { name: "Proxmox VE HA" });
    expect(heading.closest("tr")).toHaveTextContent("separate nodes");
    await user.click(screen.getByRole("button", { name: "Edit Proxmox VE HA" }));
    const dialog = screen.getByRole("dialog", { name: "Proxmox VE HA" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await user.click(within(dialog).getByLabelText(/Proxmox VE HA$/u));
    await user.click(save);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { proxmoxHA: false } }));
  });

  it("is not shown where an administrator has not enabled it", async () => {
    openOptions([{ ...rke2, annotations: {} }]);
    await screen.findByRole("rowheader", { name: "Placement" });
    await expect(screen.findByRole("rowheader", { name: "Proxmox VE HA" }, { timeout: 200 })).rejects.toThrow(
      /Unable to find/u,
    );
  });
});
