import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import {
  csiOffered,
  csiProblems,
  datastoreAllocateAnnotation,
  datastoreAllocateProblem,
  managementStoragesAnnotation,
  newForm,
  request,
  summary,
  templateStoragesAnnotation,
  type FieldKey,
} from "./create-form";
import { MachinesStep, type StepProps } from "./CreateSteps";
import { applyStaged, fakeServer, type Answer } from "./fake-server";
import type { Cluster, ClusterClass, Storage } from "./types";
import { record } from "./values";

const k3s: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
  annotations: {
    [datastoreAllocateAnnotation]: "true",
    [managementStoragesAnnotation]: "local-lvm,local",
    [templateStoragesAnnotation]: "nvme",
  },
  variables: [
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "csi", required: false, schema: { type: "object" } },
  ],
};

const storage = (name: string, allocatable: boolean): Storage => ({
  storage: name,
  node: "pve1",
  shared: false,
  content: ["images"],
  avail: 1,
  total: 2,
  usable: true,
  allocatable,
});

const storages = [storage("ceph", true), storage("local-lvm", true), storage("nvme", true), storage("tank", false)];

const cluster: Cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-k3s",
  distribution: "k3s",
  version: "v1.36.5+k3s1",
  generation: 1,
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

describe("the CSI plugin's newest release", () => {
  it("is offered where an administrator lets clusters choose it", () => {
    expect([
      csiOffered(k3s),
      csiOffered({ ...k3s, annotations: {} }),
      csiOffered({ ...k3s, variables: k3s.variables.slice(0, 1) }),
    ]).toStrictEqual([true, false, false]);
  });

  it("needs a storage of the cluster's own that the user holds Datastore.Allocate on", () => {
    const other = { ...cluster, name: "api" };
    const problem = (on: string, clusters: Cluster[] = []): string | undefined =>
      datastoreAllocateProblem({ cls: k3s, storages, storage: on, clusters, self: cluster });
    expect([
      problem("ceph", [cluster]),
      problem(""),
      problem("local"),
      problem("nvme"),
      problem("tank"),
      problem("ceph", [cluster, other]),
    ]).toStrictEqual([
      undefined,
      "Choose a storage first",
      "Unavailable on local, which the management cluster uses",
      "Unavailable on nvme, which holds the VM templates",
      "You do not hold Datastore.Allocate on tank",
      "Unavailable on ceph, which cluster clusters/api uses too",
    ]);
  });

  it("is asked for in the csi variable only when chosen", () => {
    const form = { ...newForm(), storage: "ceph" };
    expect(request(form, k3s).variables).not.toHaveProperty("csi");
    expect(request({ ...form, datastoreAllocate: true }, k3s).variables).toHaveProperty("csi", {
      datastoreAllocate: true,
    });
    expect(Object.fromEntries(summary({ ...form, datastoreAllocate: true }, k3s, { csi: true }))["CSI plugin"]).toBe(
      "Newest, with Datastore.Allocate",
    );
    expect(Object.fromEntries(summary(form, k3s))).not.toHaveProperty("CSI plugin");
  });

  it("holds the machines step back while chosen where something stands in the way", () => {
    const refused = { offered: true, problem: "No." };
    const form = { ...newForm(), datastoreAllocate: true };
    expect([
      csiProblems("Machines", form, refused),
      csiProblems("Machines", { ...form, datastoreAllocate: false }, refused),
      csiProblems("Add-ons", form, refused),
      csiProblems("Machines", form, { offered: true, problem: undefined }),
    ]).toStrictEqual([{ datastoreAllocate: "No." }, {}, {}, {}]);
  });

  it("is chosen on the machines step, and only where nothing stands in the way", async () => {
    const set = vi.fn<StepProps["set"]>();
    const step = (offered: boolean, problem?: string): JSX.Element => (
      <MachinesStep
        form={{ ...newForm(), storage: "ceph" }}
        set={set}
        shown={(k) => noProblems.get(k)}
        cls={k3s}
        usableStorages={storages}
        placed={false}
        haOffered={false}
        csi={{ offered, problem }}
        snapshots={{ offered: false, problem: undefined }}
        haGroups={false}
        nodes={[]}
        rules={{ haRules: undefined, haRulesError: undefined }}
      />
    );
    const { rerender } = render(step(false));
    expect(screen.queryByLabelText("Newest CSI plugin release")).toBeNull();
    rerender(step(true, "Unavailable on local, which the management cluster uses"));
    expect(screen.getByLabelText("Newest CSI plugin release")).toBeDisabled();
    expect(screen.getByText("Unavailable on local, which the management cluster uses")).toBeInTheDocument();
    rerender(step(true));
    const box = screen.getByLabelText("Newest CSI plugin release");
    expect(box).not.toBeChecked();
    await userEvent.setup({ delay: null }).click(box);
    expect(set).toHaveBeenCalledWith("datastoreAllocate", true);
  });
});

function openOptions(classes: ClusterClass[], clusters: Cluster[] = [cluster]): () => unknown {
  window.history.replaceState(null, "", "/clusters/clusters/web/options");
  const calls = fakeServer({
    "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
    "GET /api/clusters/clusters/web": { body: cluster },
    "GET /api/clusters": { body: clusters },
    "GET /api/classes": { body: classes },
    "GET /api/proxmox/storages": { body: storages },
    "PATCH /api/clusters/clusters/web": changed,
  });
  render(<App />);
  return () => calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body;
}

// changed answers a change with the cluster as it sets it, as the server plans it.
const changed = (body: unknown): Answer => ({
  body: { ...cluster, variables: { ...cluster.variables, ...record(record(body)["variables"]) } },
});

describe("a cluster's CSI plugin", () => {
  it("is switched to the newest release under Options", async () => {
    const sent = openOptions([k3s]);
    const user = userEvent.setup({ delay: null });
    const plugin = await screen.findByRole("switch", { name: "CSI plugin" });
    expect(plugin).not.toBeChecked();
    expect(plugin.closest("tr")).toHaveTextContent("Without Datastore.Allocate");
    await waitFor(() => expect(plugin).toBeEnabled());
    await user.click(plugin);
    await waitFor(() => expect(plugin).toBeChecked());
    expect(plugin).toHaveAccessibleDescription("The token gets Datastore.Allocate on the storage");
    await applyStaged(user);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { csi: { datastoreAllocate: true } } }));
  });

  it("says why it cannot be switched on beside another cluster on the storage", async () => {
    openOptions([k3s], [cluster, { ...cluster, namespace: "team-b", name: "api" }]);
    const plugin = await screen.findByRole("switch", { name: "CSI plugin" });
    await waitFor(() => expect(plugin).toHaveAccessibleDescription(/which cluster team-b\/api uses too/u));
    expect(plugin).toBeDisabled();
  });

  it("is not shown where an administrator has not let clusters choose it", async () => {
    openOptions([{ ...k3s, annotations: {} }]);
    await screen.findByRole("rowheader", { name: "Placement" });
    await expect(screen.findByRole("rowheader", { name: "CSI plugin" }, { timeout: 200 })).rejects.toThrow(
      /Unable to find/u,
    );
  });
});
