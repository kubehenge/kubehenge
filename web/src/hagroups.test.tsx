import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { haGroupsProblems, newForm, request, summary, type FieldKey } from "./create-form";
import { MachinesStep, type StepProps } from "./CreateSteps";
import { fakeServer } from "./fake-server";
import { haGroupsOffer, sysAuditAnnotation } from "./Placement";
import type { Cluster, ClusterClass } from "./types";

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  annotations: { [sysAuditAnnotation]: "true" },
  variables: [
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "ccm", required: false, schema: { type: "object" } },
  ],
};

const cluster: Cluster = {
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
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { storage: "ceph" },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const noProblems = new Map<FieldKey, string>();
const lacking = "You do not hold Sys.Audit on /, which the cluster's Proxmox VE token would get.";

describe("the HA group labels", () => {
  it("are offered where an administrator lets clusters choose them, to those holding Sys.Audit on /", () => {
    expect([
      haGroupsOffer(rke2, true),
      haGroupsOffer(rke2, false),
      haGroupsOffer(rke2, undefined),
      haGroupsOffer({ ...rke2, annotations: {} }, true),
      haGroupsOffer({ ...rke2, variables: rke2.variables.slice(0, 1) }, true),
      haGroupsOffer(undefined, true),
    ]).toStrictEqual([
      { offered: true, problem: undefined },
      { offered: true, problem: lacking },
      { offered: true, problem: undefined },
      { offered: false, problem: undefined },
      { offered: false, problem: undefined },
      { offered: false, problem: undefined },
    ]);
  });

  it("are asked for in the ccm variable only when chosen", () => {
    const form = { ...newForm(), storage: "ceph" };
    expect(request(form, rke2).variables).not.toHaveProperty("ccm");
    expect(request({ ...form, haGroups: true }, rke2).variables).toHaveProperty("ccm", { haGroups: true });
    expect(
      request({ ...form, haGroups: true }, { ...rke2, variables: rke2.variables.slice(0, 1) }).variables,
    ).not.toHaveProperty("ccm");
    expect(Object.fromEntries(summary({ ...form, haGroups: true }, rke2, { haGroups: true }))["HA group labels"]).toBe(
      "On, with Sys.Audit on / for the cluster's token",
    );
    expect(Object.fromEntries(summary(form, rke2))).not.toHaveProperty("HA group labels");
  });

  it("hold the machines step back while chosen by someone without Sys.Audit on /", () => {
    const refused = { offered: true, problem: "No." };
    const form = { ...newForm(), haGroups: true };
    expect([
      haGroupsProblems("Machines", form, refused),
      haGroupsProblems("Machines", { ...form, haGroups: false }, refused),
      haGroupsProblems("Add-ons", form, refused),
      haGroupsProblems("Machines", form, { offered: true, problem: undefined }),
    ]).toStrictEqual([{ haGroups: "No." }, {}, {}, {}]);
  });

  it("are chosen on the machines step, and only by someone holding Sys.Audit on /", async () => {
    const set = vi.fn<StepProps["set"]>();
    const step = (offered: boolean, problem?: string): JSX.Element => (
      <MachinesStep
        form={{ ...newForm(), storage: "ceph" }}
        set={set}
        shown={(k) => noProblems.get(k)}
        cls={rke2}
        usableStorages={[]}
        placed={false}
        haOffered={false}
        csi={{ offered: false, problem: undefined }}
        haGroups={{ offered, problem }}
        nodes={[]}
        rules={{ haRules: undefined, haRulesError: undefined }}
      />
    );
    const { rerender } = render(step(false));
    expect(screen.queryByLabelText(/HA groups/u)).toBeNull();
    rerender(step(true, lacking));
    expect(screen.getByLabelText(/HA groups/u)).toBeDisabled();
    expect(screen.getByText(lacking)).toBeInTheDocument();
    rerender(step(true));
    const box = screen.getByLabelText(/HA groups/u);
    expect(box).not.toBeChecked();
    await userEvent.setup({ delay: null }).click(box);
    expect(set).toHaveBeenCalledWith("haGroups", true);
  });
});

function openOptions(classes: ClusterClass[], canAudit: boolean, shown: Cluster = cluster): () => unknown {
  window.history.replaceState(null, "", "/clusters/clusters/web/options");
  const calls = fakeServer({
    "GET /api/session": { body: { user: "dev@pve", csrf: "c", canAudit } },
    "GET /api/clusters/clusters/web": { body: shown },
    "GET /api/clusters": { body: [shown] },
    "GET /api/classes": { body: classes },
    "GET /api/proxmox/storages": { body: [] },
    "PATCH /api/clusters/clusters/web": { body: shown },
  });
  render(<App />);
  return () => calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body;
}

describe("a cluster's HA group labels", () => {
  it("are switched on under Options", async () => {
    const sent = openOptions([rke2], true);
    const user = userEvent.setup({ delay: null });
    const heading = await screen.findByRole("rowheader", { name: "HA group labels" });
    expect(heading.closest("tr")).toHaveTextContent("Off");
    await user.click(screen.getByRole("button", { name: "Edit the HA group labels" }));
    const dialog = screen.getByRole("dialog", { name: "HA group labels" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await user.click(within(dialog).getByLabelText(/HA groups/u));
    await user.click(save);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { ccm: { haGroups: true } } }));
  });

  it("say why they cannot be switched on without Sys.Audit on /", async () => {
    openOptions([rke2], false);
    const user = userEvent.setup({ delay: null });
    await user.click(await screen.findByRole("button", { name: "Edit the HA group labels" }));
    const dialog = screen.getByRole("dialog", { name: "HA group labels" });
    await expect(within(dialog).findByText(lacking)).resolves.toBeInTheDocument();
    expect(within(dialog).getByLabelText(/HA groups/u)).toBeDisabled();
  });

  it("can be switched off after the administrator withdrew the choice", async () => {
    const sent = openOptions([{ ...rke2, annotations: {} }], false, {
      ...cluster,
      variables: { storage: "ceph", ccm: { haGroups: true } },
    });
    const user = userEvent.setup({ delay: null });
    const heading = await screen.findByRole("rowheader", { name: "HA group labels" });
    expect(heading.closest("tr")).toHaveTextContent("On, with Sys.Audit on /");
    await user.click(screen.getByRole("button", { name: "Edit the HA group labels" }));
    const dialog = screen.getByRole("dialog", { name: "HA group labels" });
    await user.click(within(dialog).getByLabelText(/HA groups/u));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { ccm: { haGroups: false } } }));
  });

  it("are not shown where an administrator has not let clusters choose them", async () => {
    openOptions([{ ...rke2, annotations: {} }], true);
    await screen.findByRole("rowheader", { name: "Placement" });
    await expect(screen.findByRole("rowheader", { name: "HA group labels" }, { timeout: 200 })).rejects.toThrow(
      /Unable to find/u,
    );
  });
});
