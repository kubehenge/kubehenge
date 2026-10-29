import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { applyStaged, fakeServer } from "./fake-server";

const cluster = {
  namespace: "clusters",
  name: "rancher",
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
