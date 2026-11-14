import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { newForm, request, summary } from "./create-form";
import { applyStaged, fakeServer } from "./fake-server";
import type { ClusterClass } from "./types";
import { record } from "./values";

const cluster = {
  namespace: "clusters",
  name: "rancher",
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
  variables: { rancher: { install: true, hostname: "rancher.example.org" } },
  pool: "/pool/kubehenge/clusters.rancher",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

const routes = {
  "GET /api/session": { body: { user: "admin@pve", csrf: "c" } },
  "GET /api/clusters/clusters/rancher": { body: cluster },
};

describe("the cluster page", () => {
  it("shows why a kubeconfig cannot be downloaded", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/rancher");
    fakeServer({
      ...routes,
      "GET /api/clusters/clusters/rancher/kubeconfig": {
        status: 409,
        body: { error: "the cluster has no kubeconfig yet" },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Kubeconfig" }));
    await expect(screen.findByText("The cluster has no kubeconfig yet.")).resolves.toBeInTheDocument();
  });
});

describe("rancher", () => {
  it("is removed from the cluster that runs it only once its name is typed", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/rancher/options");
    const calls = fakeServer({ ...routes, "PATCH /api/clusters/clusters/rancher": { body: cluster } });
    const sent = (): unknown => calls.find((c) => c.key === "PATCH /api/clusters/clusters/rancher")?.body;
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByText("Installed at rancher.example.org")).resolves.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Options" })).toHaveAttribute("aria-current", "page");
    await user.click(screen.getByRole("button", { name: "Uninstall Rancher" }));
    const dialog = screen.getByRole("dialog", { name: "Uninstall Rancher" });
    const confirm = within(dialog).getByRole("button", { name: "Uninstall Rancher" });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Confirm name"), "rancher");
    await user.click(confirm);
    await applyStaged(user);
    await waitFor(() =>
      expect(sent()).toStrictEqual({ variables: { rancher: { install: false, hostname: "rancher.example.org" } } }),
    );
  });
});

describe("registering with Rancher in the create wizard", () => {
  const cls: ClusterClass = {
    name: "kubehenge-rke2",
    distribution: "rke2",
    versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
    variables: [{ name: "rancher", required: false, schema: { type: "object" } }],
  };

  it("says there is no Rancher yet where the user sees none, and still sends an opt-out", () => {
    const form = { ...newForm(), rancherExists: false, rancherRegister: true };
    expect(request(form, cls).variables).not.toHaveProperty("rancher");
    expect(Object.fromEntries(summary(form, cls))["Rancher"]).toBe("Registers once a cluster installs Rancher");
    // The user may not see the cluster that runs Rancher.
    const out = { ...form, rancherRegister: false };
    expect(request(out, cls).variables["rancher"]).toStrictEqual({ register: false });
    expect(Object.fromEntries(summary(out, cls))["Rancher"]).toBe("Not registered");
    const there = { ...newForm(), rancherExists: true, rancherRegister: false };
    expect(request(there, cls).variables["rancher"]).toStrictEqual({ register: false });
    expect(Object.fromEntries(summary({ ...there, rancherRegister: true }, cls))["Rancher"]).toBe(
      "Registered with Rancher",
    );
  });

  const wizardClass: ClusterClass = {
    ...cls,
    variables: [
      { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
      { name: "network", required: true, schema: { type: "object" } },
      { name: "storage", required: true, schema: { type: "string" } },
      { name: "rancher", required: false, schema: { type: "object" } },
    ],
  };

  // The wizard up to its add-ons step, with clusters the user sees.
  async function toAddOns(clusters: object[]): Promise<{ calls: ReturnType<typeof fakeServer>; user: User }> {
    window.history.replaceState(null, "", "/clusters/new");
    const calls = fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1", canCreate: true, canTrust: true } }),
      "GET /api/classes": () => ({ body: [wizardClass] }),
      "GET /api/clusters": () => ({ body: clusters }),
      "GET /api/proxmox/storages": () => ({
        body: [
          {
            storage: "local-lvm",
            node: "pve1",
            shared: false,
            content: ["images"],
            avail: 2 ** 40,
            total: 2 ** 41,
            usable: true,
          },
        ],
      }),
      "GET /api/proxmox/nodes": () => ({ body: [{ node: "pve1", online: true }] }),
      "GET /api/proxmox/nodes/pve1/bridges": () => ({
        body: [{ bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true }],
      }),
      "GET /api/proxmox/ha-rules": () => ({ body: { readable: true, rules: [] } }),
      "POST /api/clusters?dryRun=All": () => ({ body: { manifest: "kind: Cluster\n" } }),
      "POST /api/clusters": () => ({
        status: 201,
        body: { ...cluster, namespace: "default", name: "web", variables: {} },
      }),
      "GET /api/clusters/default/web": () => ({
        body: { ...cluster, namespace: "default", name: "web", variables: {} },
      }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.type(await screen.findByLabelText("Name"), "web");
    await waitFor(() => expect(screen.getByRole("option", { name: "RKE2" })).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-rke2");
    await user.type(screen.getByLabelText("Kubernetes version"), "v1.36.5+rke2r1");
    await user.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() => expect(screen.getByLabelText("Bridge")).toHaveValue("vmbr0"));
    await user.type(screen.getByLabelText("API address"), "10.0.0.20");
    await user.type(screen.getByLabelText("Node addresses"), "10.0.0.21-10.0.0.29");
    await user.type(screen.getByLabelText("Gateway"), "10.0.0.1");
    await user.type(screen.getByLabelText("DNS servers"), "10.0.0.1");
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    return { calls, user };
  }

  it.each([
    { seen: "no Rancher", clusters: [], says: "Registers once a cluster installs Rancher" },
    { seen: "a Rancher", clusters: [cluster], says: "Registered with Rancher" },
  ])("offers the box ticked where the user sees $seen, and sends no opt-out", async ({ clusters, says }) => {
    const { calls, user } = await toAddOns(clusters);
    expect(screen.getByRole("checkbox", { name: "Register with Rancher" })).toBeChecked();
    await expect(create(calls, user, says)).resolves.not.toHaveProperty("rancher");
  });

  it.each([
    { seen: "no Rancher", clusters: [] },
    { seen: "a Rancher", clusters: [cluster] },
  ])("sends the opt-out once the box is unticked where the user sees $seen", async ({ clusters }) => {
    const { calls, user } = await toAddOns(clusters);
    await user.click(screen.getByRole("checkbox", { name: "Register with Rancher" }));
    await expect(create(calls, user, "Not registered")).resolves.toHaveProperty("rancher", { register: false });
  });
});

type User = ReturnType<typeof userEvent.setup>;

// From the add-ons step, through Confirm, to the create: what Confirm says, and the variables sent.
async function create(
  calls: ReturnType<typeof fakeServer>,
  user: User,
  says: string,
): Promise<Record<string, unknown>> {
  await user.click(screen.getByRole("button", { name: "Next" }));
  await user.click(screen.getByRole("button", { name: "Next" }));
  await user.click(screen.getByRole("button", { name: "Next" }));
  expect(screen.getByText(says)).toBeInTheDocument();
  await waitFor(() => expect(screen.getByRole("button", { name: "Create" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Create" }));
  await waitFor(() => expect(calls.map((c) => c.key)).toContain("POST /api/clusters"));
  return record(record(calls.find((c) => c.key === "POST /api/clusters")?.body)["variables"]);
}
