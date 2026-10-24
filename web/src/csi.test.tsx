import { render, screen, waitFor, within } from "@testing-library/react";
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
import { fakeServer } from "./fake-server";
import type { Cluster, ClusterClass, Storage } from "./types";

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
      "Choose the cluster's storage first.",
      "The management cluster uses storage local, so no cluster may hold Datastore.Allocate on it.",
      "VM templates have disks on storage nvme, so no cluster may hold Datastore.Allocate on it.",
      "You do not hold Datastore.Allocate on storage tank, which the cluster's token would get.",
      "Cluster clusters/api uses storage ceph too; Datastore.Allocate needs a storage of the cluster's own.",
    ]);
  });

  it("is asked for in the csi variable only when chosen", () => {
    const form = { ...newForm(), storage: "ceph" };
    expect(request(form, k3s).variables).not.toHaveProperty("csi");
    expect(request({ ...form, datastoreAllocate: true }, k3s).variables).toHaveProperty("csi", {
      datastoreAllocate: true,
    });
    expect(Object.fromEntries(summary({ ...form, datastoreAllocate: true }, k3s, { csi: true }))["CSI plugin"]).toBe(
      "Newest release, with Datastore.Allocate on the cluster's storage",
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
    expect(screen.queryByLabelText(/newest release/u)).toBeNull();
    rerender(step(true, "The management cluster uses storage local."));
    expect(screen.getByLabelText(/newest release/u)).toBeDisabled();
    expect(screen.getByText("The management cluster uses storage local.")).toBeInTheDocument();
    rerender(step(true));
    const box = screen.getByLabelText(/newest release/u);
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
    "PATCH /api/clusters/clusters/web": { body: cluster },
  });
  render(<App />);
  return () => calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body;
}

describe("a cluster's CSI plugin", () => {
  it("is switched to the newest release under Options", async () => {
    const sent = openOptions([k3s]);
    const user = userEvent.setup({ delay: null });
    const heading = await screen.findByRole("rowheader", { name: "CSI plugin" });
    expect(heading.closest("tr")).toHaveTextContent("needs no Datastore.Allocate");
    await user.click(screen.getByRole("button", { name: "Edit the CSI plugin" }));
    const dialog = screen.getByRole("dialog", { name: "CSI plugin" });
    const save = within(dialog).getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await waitFor(() => expect(within(dialog).getByLabelText(/newest release/u)).toBeEnabled());
    await user.click(within(dialog).getByLabelText(/newest release/u));
    await user.click(save);
    await waitFor(() => expect(sent()).toStrictEqual({ variables: { csi: { datastoreAllocate: true } } }));
  });

  it("says why it cannot be switched on beside another cluster on the storage", async () => {
    openOptions([k3s], [cluster, { ...cluster, namespace: "team-b", name: "api" }]);
    const user = userEvent.setup({ delay: null });
    await user.click(await screen.findByRole("button", { name: "Edit the CSI plugin" }));
    const dialog = screen.getByRole("dialog", { name: "CSI plugin" });
    await expect(within(dialog).findByText(/Cluster team-b\/api uses storage ceph too/u)).resolves.toBeInTheDocument();
    expect(within(dialog).getByLabelText(/newest release/u)).toBeDisabled();
  });

  it("is not shown where an administrator has not let clusters choose it", async () => {
    openOptions([{ ...k3s, annotations: {} }]);
    await screen.findByRole("rowheader", { name: "Placement" });
    await expect(screen.findByRole("rowheader", { name: "CSI plugin" }, { timeout: 200 })).rejects.toThrow(
      /Unable to find/u,
    );
  });
});
