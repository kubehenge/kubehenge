import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { newForm, request, type Form } from "./create-form";
import { fakeServer, reads } from "./fake-server";
import { fromCluster, startDefault, startKeys } from "./start-from";
import type { Cluster, ClusterClass } from "./types";

const object = { type: "object" };
const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: [
    "controlPlaneEndpoint",
    "network",
    "storage",
    "controlPlaneMachine",
    "workerMachine",
    "allowedNodes",
    "proxmoxHA",
    "csi",
    "ccm",
    "cni",
    "loadBalancer",
    "headlamp",
    "monitoring",
    "logs",
    "podSecurity",
    "cis",
    "istio",
    "rancher",
    "oidc",
  ].map((name) => ({ name, required: false, schema: object })),
};

const web: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  generation: 1,
  created: "2026-10-01T00:00:00Z",
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 3, ready: 3, available: 3, upToDate: 3 },
  workers: { desired: 2, ready: 2, available: 2, upToDate: 2 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 2 }],
  variables: {},
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: true },
};

// A form unlike the defaults in everything a new cluster takes from another.
const chosen: Form = {
  ...newForm(),
  name: "web",
  className: "kubehenge-rke2",
  version: "v1.36.5+rke2r1",
  controlPlaneReplicas: 3,
  workerReplicas: 2,
  endpoint: "10.1.0.20",
  bridge: "vmbr1",
  vlan: "20",
  addresses: "10.1.0.21-10.1.0.29",
  prefix: 16,
  gateway: "10.1.0.1",
  dnsServers: "10.1.0.53, 1.1.1.1",
  loadBalancer: "kube-vip",
  loadBalancerAddresses: "10.1.0.40-10.1.0.49",
  storage: "tank",
  place: { placement: "nodes", haRule: "", nodes: ["pve1", "pve2"] },
  proxmoxHA: false,
  datastoreAllocate: true,
  haGroups: false,
  controlPlaneMachine: { cores: 4, memoryMiB: 6144, diskGiB: 40 },
  workerMachine: { cores: 8, memoryMiB: 16_384, diskGiB: 80 },
  cni: "calico",
  headlamp: true,
  headlampHostname: "headlamp.example",
  headlampView: true,
  monitoring: true,
  grafanaHostname: "grafana.example",
  logs: { enabled: true, collector: "api", retentionDays: 14, sizeGiB: 20 },
  podSecurity: { admission: "restricted", kyverno: "restricted" },
  cis: true,
  istio: true,
  rancherRegister: false,
  oidc: [{ issuerURL: "https://id.example", clientID: "kubernetes" }],
};

const taken = (f: Form): Partial<Form> => Object.fromEntries([...startKeys].map((k) => [k, f[k]]));

describe("a new cluster started from another", () => {
  it("takes all but what each cluster has to itself", () => {
    const existing = { ...web, variables: request(chosen, rke2).variables };
    const started = fromCluster(existing, { ...newForm(), name: "next" });
    expect(taken(started)).toStrictEqual(taken(chosen));
    expect(started).toMatchObject({
      from: "default/web",
      name: "next",
      version: "",
      endpoint: "",
      addresses: "",
      loadBalancerAddresses: "",
      headlampHostname: "",
      grafanaHostname: "",
      datastoreAllocate: false,
    });
  });

  it("keeps the version chosen while the distribution stays", () => {
    const k3s = { ...web, name: "edge", class: "kubehenge-k3s" };
    expect(fromCluster(web, chosen).version).toBe("v1.36.5+rke2r1");
    expect(fromCluster(k3s, chosen).version).toBe("");
  });

  it("starts by default from the newest cluster that works", () => {
    const failed = { ...web, name: "failed", created: "2026-10-03T00:00:00Z", available: false };
    const gone = { ...web, name: "gone", created: "2026-10-02T00:00:00Z", deleting: true };
    expect(startDefault([web, failed, gone])?.name).toBe("web");
    expect(startDefault([failed, gone])?.name).toBe("failed");
    expect(startDefault([gone])).toBeUndefined();
  });

  it("autoscales its workers where the other does", () => {
    const autoscaled = {
      ...web,
      workerPools: [{ name: "md-0", class: "worker", replicas: 3, autoscaling: { min: 2, max: 6 } }],
    };
    expect(fromCluster(autoscaled, newForm())).toMatchObject({
      workerAutoscale: true,
      workerReplicas: 2,
      workerMax: 6,
    });
    expect(fromCluster(web, newForm())).toMatchObject({ workerAutoscale: false, workerReplicas: 2 });
  });

  it("starts as the newest cluster, or from the defaults", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const variables = { cni: "canal", network: { bridge: "vmbr1", gateway: "10.1.0.1", prefix: 16 } };
    const older = { ...web, name: "old", created: "2026-01-01T00:00:00Z" };
    const gone = { ...web, name: "gone", created: "2026-10-02T00:00:00Z", deleting: true };
    fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/classes": () => ({ body: [rke2] }),
      "GET /api/versions/rke2": () => ({ body: { versions: ["v1.36.5+rke2r1"], stable: "v1.36.5+rke2r1" } }),
      "GET /api/clusters": () => ({ body: [older, gone, { ...web, variables }] }),
      "GET /api/proxmox/storages": () => ({ body: [] }),
      "GET /api/proxmox/nodes": () => ({ body: [] }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const from = await screen.findByLabelText("Start from");
    await waitFor(() => expect(from).toHaveValue("default/web"));
    expect(
      within(from)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toStrictEqual(["Defaults", "web", "old"]);
    expect(screen.getByLabelText("Distribution")).toHaveValue("kubehenge-rke2");
    expect(screen.getByLabelText("Control-plane nodes")).toHaveValue("3");

    await user.type(screen.getByLabelText("Name"), "next");
    await user.selectOptions(from, "Defaults");
    expect(screen.getByLabelText("Distribution")).toHaveValue("");
    expect(screen.getByLabelText("Control-plane nodes")).toHaveValue("1");
    expect(screen.getByLabelText("Name")).toHaveValue("next");

    await user.selectOptions(from, "web");
    await waitFor(() => expect(screen.getByLabelText("Kubernetes version")).toHaveValue("v1.36.5+rke2r1"));
    await user.click(screen.getByRole("button", { name: "Next" }));
    await expect(screen.findByLabelText("Bridge")).resolves.toHaveValue("vmbr1");
    expect(screen.getByLabelText("Gateway")).toHaveValue("10.1.0.1");
    // No node reports the network on vmbr1, so the examples come from the gateway's.
    expect(screen.getByLabelText("Node addresses")).toHaveAccessibleDescription(
      "Ranges like 10.1.0.21-10.1.0.29, networks or addresses",
    );
  });

  it("is not offered before there is a cluster", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const calls = fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/classes": () => ({ body: [rke2] }),
      "GET /api/clusters": () => ({ body: [] }),
      "GET /api/proxmox/storages": () => ({ body: [] }),
      "GET /api/proxmox/nodes": () => ({ body: [] }),
    });
    render(<App />);
    await waitFor(() => expect(reads(calls, "GET /api/clusters")).toBeGreaterThan(0));
    await screen.findByRole("option", { name: "RKE2" });
    expect(screen.queryByLabelText("Start from")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Distribution")).toHaveValue("");
  });
});
