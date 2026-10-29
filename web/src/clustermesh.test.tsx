import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { AddOnsStep } from "./AddOnsStep";
import { App } from "./App";
import { newForm, type FieldKey, type Form } from "./create-form";
import { NetworkStep } from "./CreateSteps";
import { applyStaged, fakeServer } from "./fake-server";
import type { Cluster, ClusterClass } from "./types";

const cluster = (namespace: string, name: string, variables: Record<string, unknown>): Cluster => ({
  namespace,
  name,
  class: "kubehenge-kubeadm",
  distribution: "kubeadm",
  version: "v1.36.5",
  generation: 1,
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables,
  pool: `/pool/kubehenge/${namespace}.${name}`,
  allowed: { change: true, kubeconfig: true },
  machines: [],
});

const web: Cluster = {
  ...cluster("clusters", "web", { cni: "cilium", clusterMesh: { enabled: true, peers: ["db"] } }),
  mesh: [{ name: "db", refused: "it does not list this cluster" }],
};

// The clusters a peer can be chosen from: only the other Cilium clusters of the same namespace.
const clusters = [
  web,
  cluster("clusters", "db", { cni: "cilium" }),
  cluster("clusters", "api", { cni: "cilium" }),
  cluster("clusters", "legacy", { cni: "calico" }),
  cluster("elsewhere", "far", { cni: "cilium" }),
];

const noProblems = new Map<FieldKey, string>();

describe("a Cluster Mesh", () => {
  it("is offered in the options of a Cilium cluster, with how each peer stands", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters": { body: clusters },
      "GET /api/clusters/clusters/web": { body: web },
      "PATCH /api/clusters/clusters/web": { body: web },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    const row = await screen.findByRole("row", { name: /Cluster Mesh/u });
    expect(row).toHaveTextContent("On, waiting for db");
    await user.click(screen.getByRole("button", { name: "Edit Cluster Mesh" }));
    const dialog = screen.getByRole("dialog", { name: "Edit Cluster Mesh" });
    await user.click(within(dialog).getByRole("button", { name: "About Cluster Mesh" }));
    expect(within(dialog).getByText(/list each other/u)).toBeVisible();
    const db = await within(dialog).findByRole("checkbox", { name: /^db/u });
    expect(db).toBeChecked();
    expect(within(dialog).getByText("(Not linked)")).toBeInTheDocument();
    expect(within(dialog).getByText("it does not list this cluster")).toBeInTheDocument();
    expect(within(dialog).queryByRole("checkbox", { name: /legacy|far|web/u })).not.toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.click(within(dialog).getByRole("checkbox", { name: "api" }));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.findLast((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { clusterMesh: { enabled: true, peers: ["api", "db"] } },
    });
  });

  it.each([
    [[], "On, no peers"],
    [[{ name: "db" }, { name: "api", refused: "it is off" }], "On, linked with db, waiting for api"],
  ])("says in a line what a cluster is linked with", async (mesh, line) => {
    const on: Cluster = { ...cluster("clusters", "web", { cni: "cilium", clusterMesh: { enabled: true } }), mesh };
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: on },
    });
    render(<App />);
    const row = await screen.findByRole("row", { name: /Cluster Mesh/u });
    expect(row).toHaveTextContent(line);
  });

  it("is not offered to a cluster that does not run Cilium", async () => {
    const canal = cluster("clusters", "web", { cni: "canal" });
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: canal },
    });
    render(<App />);
    await screen.findByRole("row", { name: /Istio/u });
    expect(screen.queryByRole("row", { name: /Cluster Mesh/u })).not.toBeInTheDocument();
  });

  it("can be joined when a Cilium cluster is created", async () => {
    fakeServer({ "GET /api/clusters": { body: clusters } });
    const cls: ClusterClass = {
      name: "kubehenge-kubeadm",
      distribution: "kubeadm",
      versionFormat: { pattern: String.raw`^v1\.\d+\.\d+$` },
      variables: [
        { name: "cni", required: false, schema: { type: "string", enum: ["cilium", "calico"], default: "cilium" } },
        { name: "clusterMesh", required: false, schema: { type: "object" } },
      ],
    };
    const set = vi.fn<(k: keyof Form, v: unknown) => void>();
    const form: Form = { ...newForm(), name: "new", cni: "cilium", clusterMesh: true };
    const step = (f: Form): JSX.Element => (
      <AddOnsStep form={f} set={set} shown={(k) => noProblems.get(k)} cls={cls} canTrust namespace="clusters" />
    );
    const user = userEvent.setup({ delay: null });
    const { rerender } = render(step(form));

    expect(screen.getByLabelText("Join a Cluster Mesh")).toBeChecked();
    await user.click(await screen.findByRole("checkbox", { name: "api" }));
    expect(set).toHaveBeenLastCalledWith("meshPeers", ["api"]);
    expect(screen.queryByRole("checkbox", { name: /legacy|far/u })).not.toBeInTheDocument();

    rerender(step({ ...form, cni: "calico" }));
    expect(screen.queryByLabelText("Join a Cluster Mesh")).not.toBeInTheDocument();
  });

  it("says a Cilium cluster's ranges keep clear of the other clusters'", () => {
    const cls: ClusterClass = {
      name: "kubehenge-k3s",
      distribution: "k3s",
      versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
      annotations: { "kubehenge.dev/default-pods": "10.42.0.0/16", "kubehenge.dev/default-services": "10.43.0.0/16" },
      variables: [{ name: "cni", required: false, schema: { type: "string", enum: ["cilium", "calico"] } }],
    };
    const step = (f: Form): JSX.Element => (
      <NetworkStep
        form={f}
        set={vi.fn<(k: keyof Form, v: unknown) => void>()}
        shown={(k) => noProblems.get(k)}
        cls={cls}
        bridgeOptions={[]}
      />
    );
    const { rerender } = render(step({ ...newForm(), cni: "cilium" }));
    expect(screen.getByLabelText("Pod range")).toHaveAttribute("placeholder", "10.42.0.0/16");
    expect(screen.getByLabelText("Service range")).toHaveAttribute("placeholder", "10.43.0.0/16");
    expect(screen.getByLabelText("Pod range")).toHaveAccessibleDescription(/clear of other clusters/u);

    rerender(step({ ...newForm(), cni: "calico" }));
    expect(screen.getByLabelText("Pod range")).not.toHaveAccessibleDescription(/clear of other clusters/u);
  });
});
