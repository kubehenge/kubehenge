import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { newForm, request, summary, type FieldKey } from "./create-form";
import { MachinesStep, type StepProps } from "./CreateSteps";
import { applyStaged, fakeServer } from "./fake-server";
import { offersHAGroups, sysAuditAnnotation } from "./Placement";
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
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { storage: "ceph", ccm: { haGroups: true } },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const noProblems = new Map<FieldKey, string>();

describe("the HA group labels", () => {
  it("are offered wherever an administrator lets clusters hold Sys.Audit on /, whatever the user holds", () => {
    expect([
      offersHAGroups(rke2),
      offersHAGroups({ ...rke2, annotations: {} }),
      offersHAGroups({ ...rke2, variables: rke2.variables.slice(0, 1) }),
      offersHAGroups(undefined),
    ]).toStrictEqual([true, false, false, false]);
  });

  it("are on by default, and switched off in the ccm variable", () => {
    const form = { ...newForm(), storage: "ceph" };
    expect(form).toHaveProperty("haGroups", true);
    expect(request(form, rke2).variables).not.toHaveProperty("ccm");
    expect(request({ ...form, haGroups: false }, rke2).variables).toHaveProperty("ccm", { haGroups: false });
    expect(
      request({ ...form, haGroups: false }, { ...rke2, variables: rke2.variables.slice(0, 1) }).variables,
    ).not.toHaveProperty("ccm");
    expect(Object.fromEntries(summary(form, rke2, { haGroups: true }))["HA group labels"]).toBe(
      "On, Sys.Audit on / for the token",
    );
    expect(Object.fromEntries(summary({ ...form, haGroups: false }, rke2, { haGroups: true }))["HA group labels"]).toBe(
      "Off",
    );
    expect(Object.fromEntries(summary(form, rke2))).not.toHaveProperty("HA group labels");
  });

  it("are shown ticked on the machines step where offered, and can be switched off", async () => {
    const set = vi.fn<StepProps["set"]>();
    const step = (offered: boolean): JSX.Element => (
      <MachinesStep
        form={{ ...newForm(), storage: "ceph" }}
        set={set}
        shown={(k) => noProblems.get(k)}
        cls={rke2}
        usableStorages={[]}
        placed={false}
        haOffered={false}
        csi={{ offered: false, problem: undefined }}
        snapshots={{ offered: false, problem: undefined }}
        haGroups={offered}
        nodes={[]}
        rules={{ haRules: undefined, haRulesError: undefined }}
      />
    );
    const { rerender } = render(step(false));
    expect(screen.queryByLabelText("Add HA group labels")).toBeNull();
    rerender(step(true));
    const box = screen.getByLabelText("Add HA group labels");
    expect(box).toBeChecked();
    expect(box).toBeEnabled();
    await userEvent.setup({ delay: null }).click(box);
    expect(set).toHaveBeenCalledWith("haGroups", false);
  });
});

function openOptions(classes: ClusterClass[], shown: Cluster = cluster): () => unknown {
  window.history.replaceState(null, "", "/clusters/clusters/web/options");
  const calls = fakeServer({
    "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
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
  it("are switched off under Options", async () => {
    const sent = openOptions([rke2]);
    const user = userEvent.setup({ delay: null });
    const labels = await screen.findByRole("switch", { name: "HA group labels" });
    expect(labels).toBeChecked();
    expect(labels.closest("tr")).toHaveTextContent("On, Sys.Audit on / for the token");
    await user.click(labels);
    await applyStaged(user);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { ccm: { haGroups: false } } }));
  });

  it("are switched on again by someone without Sys.Audit on / of their own", async () => {
    const sent = openOptions([rke2], { ...cluster, variables: { storage: "ceph", ccm: { haGroups: false } } });
    const user = userEvent.setup({ delay: null });
    const labels = await screen.findByRole("switch", { name: "HA group labels" });
    expect(labels.closest("tr")).toHaveTextContent("Off");
    await user.click(labels);
    await applyStaged(user);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { ccm: { haGroups: true } } }));
  });

  it("can be switched off after the administrator withdrew them", async () => {
    const sent = openOptions([{ ...rke2, annotations: {} }]);
    const user = userEvent.setup({ delay: null });
    const labels = await screen.findByRole("switch", { name: "HA group labels" });
    expect(labels.closest("tr")).toHaveTextContent("On, Sys.Audit on / for the token");
    await user.click(labels);
    await applyStaged(user);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { ccm: { haGroups: false } } }));
  });

  it("are not shown where an administrator does not let clusters have them", async () => {
    openOptions([{ ...rke2, annotations: {} }], { ...cluster, variables: { storage: "ceph" } });
    await screen.findByRole("rowheader", { name: "Placement" });
    await expect(screen.findByRole("rowheader", { name: "HA group labels" }, { timeout: 200 })).rejects.toThrow(
      /Unable to find/u,
    );
  });
});
