import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { fakeServer } from "./fake-server";
import type { Cluster, ClusterClass, NodeOS, NodeOSChoice } from "./types";

const alma: NodeOS = {
  family: "almalinux",
  release: "10",
  title: "AlmaLinux 10",
  cpuType: "x86-64-v2-AES",
  mac: "selinux",
  default: true,
};

const rocky: NodeOS = { family: "rocky", release: "10", title: "Rocky Linux 10", cpuType: "x86-64-v3", mac: "selinux" };

const leap: NodeOS = {
  family: "leap",
  release: "16.1",
  title: "openSUSE Leap 16.1",
  cpuType: "x86-64-v2-AES",
  mac: "selinux",
  unreleased: "not released yet",
};

const choices: NodeOSChoice[] = [alma, rocky, leap];

const kubeadm: ClusterClass = {
  name: "kubehenge-kubeadm",
  distribution: "kubeadm",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+$` },
  variables: [{ name: "nodeOS", required: false, schema: { type: "object" } }],
  nodeOSes: choices,
};

const talos: ClusterClass = {
  name: "kubehenge-talos",
  distribution: "talos",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+$` },
  variables: [],
};

const plain: Cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-kubeadm",
  distribution: "kubeadm",
  version: "v1.36.5",
  created: new Date().toISOString(),
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: {},
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

const cluster: Cluster = { ...plain, variables: { nodeOS: { family: "almalinux", release: "10" } }, nodeOS: alma };

const session = { body: { user: "dev@pve", csrf: "c" } };

describe("the node OS", () => {
  it("is chosen in the wizard, the default first, and narrows kubeadm's versions to its templates", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const calls = fakeServer({
      "GET /api/session": session,
      "GET /api/classes": { body: [kubeadm, talos] },
      "GET /api/proxmox/storages": { body: [] },
      "GET /api/proxmox/nodes": { body: [] },
      "GET /api/versions/kubeadm?nodeOS=almalinux-10": { body: { versions: ["v1.37.1", "v1.36.5"] } },
      "GET /api/versions/kubeadm?nodeOS=rocky-10": { body: { versions: ["v1.36.5"] } },
      "GET /api/versions/talos": { body: { versions: ["v1.37.1"] } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await waitFor(() => expect(screen.getByRole("option", { name: "kubeadm" })).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-kubeadm");

    const nodeOS = screen.getByLabelText("Node OS");
    expect(nodeOS).toHaveValue("almalinux-10");
    await user.click(screen.getByRole("button", { name: "About Node OS" }));
    expect(screen.getByText("Minimal, from AlmaLinux 10's signed packages, with SELinux enforcing.")).toBeVisible();
    expect(within(nodeOS).getByRole("option", { name: "openSUSE Leap 16.1: not released yet" })).toBeDisabled();
    await waitFor(() => expect(screen.getByLabelText("Kubernetes version")).toHaveValue("v1.37.1"));

    await user.selectOptions(nodeOS, "rocky-10");
    expect(nodeOS).toHaveAccessibleDescription(/^Needs Proxmox VE nodes with x86-64-v3 CPUs/u);
    // v1.37.1 has no template on Rocky Linux 10, so the version moves to one that has.
    await waitFor(() => expect(screen.getByLabelText("Kubernetes version")).toHaveValue("v1.36.5"));
    const version = within(screen.getByLabelText("Kubernetes version"));
    expect(version.queryByRole("option", { name: /v1\.37\.1/u })).not.toBeInTheDocument();
    expect(calls.map((c) => c.key)).toContain("GET /api/versions/kubeadm?nodeOS=rocky-10");

    // Talos nodes run Talos, so there is nothing to choose.
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-talos");
    expect(screen.queryByLabelText("Node OS")).not.toBeInTheDocument();
  });

  it("shows on the cluster's page", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    fakeServer({ "GET /api/session": session, "GET /api/clusters/clusters/web": { body: cluster } });
    render(<App />);
    const term = await screen.findByText("Node OS", { selector: "dt" });
    expect(term.nextElementSibling).toHaveTextContent("AlmaLinux 10");
  });

  it("is changed under Options, to those with a template only, and the server's refusal shows", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const missing =
      "there is no kubeadm VM template of v1.36.5 on Rocky Linux 10 yet; an administrator builds it on the Administration page";
    const calls = fakeServer({
      "GET /api/session": session,
      "GET /api/clusters/clusters/web": { body: cluster },
      "GET /api/classes": {
        body: [
          {
            ...kubeadm,
            nodeOSes: [
              alma,
              rocky,
              {
                ...alma,
                release: "11",
                title: "AlmaLinux 11",
                default: false,
                noTemplate: "there is no kubeadm VM template on AlmaLinux 11 yet",
              },
              leap,
            ],
          },
        ],
      },
      "PATCH /api/clusters/clusters/web": { status: 409, body: { error: missing } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByRole("row", { name: /Node OS/u })).resolves.toHaveTextContent("AlmaLinux 10");
    await user.click(screen.getByRole("button", { name: "Edit the node OS" }));
    const dialog = screen.getByRole("dialog", { name: "Edit node OS" });
    expect(within(dialog).getByText(/replaces every node, one at a time/u)).toBeInTheDocument();
    const choice = (name: RegExp): HTMLElement => within(dialog).getByRole("radio", { name });
    await waitFor(() => expect(choice(/^AlmaLinux 10/u)).toBeChecked());
    expect(choice(/^AlmaLinux 10/u)).toHaveAccessibleName("AlmaLinux 10 (running)");
    expect(choice(/^AlmaLinux 11/u)).toBeDisabled();
    expect(choice(/^AlmaLinux 11/u)).toHaveAccessibleDescription(
      "There is no kubeadm VM template on AlmaLinux 11 yet.",
    );
    expect(choice(/^openSUSE Leap/u)).toBeDisabled();
    expect(choice(/^openSUSE Leap/u)).toHaveAccessibleDescription("Not released yet.");
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();

    await user.click(choice(/^Rocky Linux 10/u));
    expect(choice(/^Rocky Linux 10/u)).toHaveAccessibleDescription("Needs Proxmox VE nodes with x86-64-v3 CPUs");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await expect(within(dialog).findByRole("alert")).resolves.toHaveTextContent(
      /^There is no kubeadm VM template of v1\.36\.5 on Rocky Linux 10 yet; .*Administration page\.$/u,
    );
    expect(screen.queryByRole("button", { name: "Review plan" })).not.toBeInTheDocument();
    expect(calls.find((c) => c.key === "POST /api/clusters/clusters/web/plan")?.body).toStrictEqual({
      variables: { nodeOS: { family: "rocky", release: "10" } },
    });
  });

  it("has no row under Options for Talos", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    fakeServer({
      "GET /api/session": session,
      "GET /api/clusters/clusters/web": { body: { ...plain, class: "kubehenge-talos", distribution: "talos" } },
    });
    render(<App />);
    await screen.findByRole("row", { name: /Pod network/u });
    expect(screen.queryByRole("row", { name: /Node OS/u })).not.toBeInTheDocument();
  });
});
