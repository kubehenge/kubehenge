import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { newAlerting } from "./Alerting";
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
    "hostOutageGraceMinutes",
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
    "namespaceLabels",
    "useExisting",
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
  mtu: "1450",
  queues: "2",
  addresses: "10.1.0.21-10.1.0.29",
  prefix: 16,
  gateway: "10.1.0.1",
  dnsServers: "10.1.0.53, 1.1.1.1",
  searchDomains: "corp.example, example.org",
  ntpServers: "ntp.example.org, 192.0.2.5",
  loadBalancer: "metallb",
  loadBalancerAddresses: "10.1.0.40-10.1.0.49",
  loadBalancerMode: "bgp",
  loadBalancerAsn: "65001",
  loadBalancerPeers: "10.1.0.1 64512",
  loadBalancerAuthenticated: true,
  storage: "tank",
  place: { placement: "nodes", haRule: "", nodes: ["pve1", "pve2"] },
  hostOutageGrace: "90",
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
  alerting: {
    ...newAlerting(),
    email: {
      on: true,
      smarthost: "mail.example:587",
      from: "alerts@example.org",
      to: "ops@example.org, oncall@example.org",
      username: "alerts",
      requireTLS: false,
    },
    slack: { on: true, channel: "#ops" },
    remoteWrite: { on: true, url: "https://mimir.example/push", auth: "basic", username: "tenant" },
    external: { on: true, urls: "https://am.example\nhttps://am2.example:9093", auth: "bearer", username: "" },
  },
  logs: { enabled: true, collector: "api", retentionDays: 14, sizeGiB: 20, outputs: [] },
  podSecurity: { admission: "restricted", kyverno: "restricted" },
  cis: true,
  istio: true,
  rancherRegister: false,
  oidc: [{ issuerURL: "https://id.example", clientID: "kubernetes" }],
  namespaceLabels: "team=payments\nenv=prod",
  existingCertManager: true,
};

const taken = (f: Form): Partial<Form> => Object.fromEntries([...startKeys].map((k) => [k, f[k]]));

describe("a new cluster started from another", () => {
  it("takes all but what each cluster has to itself", () => {
    const existing = { ...web, variables: request(chosen, rke2).variables };
    const started = fromCluster(existing, { ...newForm(), name: "next" });
    expect(taken(started)).toStrictEqual(taken(chosen));
    expect(started.ntpServers).toBe("ntp.example.org, 192.0.2.5");
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

  it("suggests addresses on the network it took, not the bridge's", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const network = { bridge: "vmbr0", gateway: "10.0.70.1", prefix: 24 };
    const asked: Record<string, unknown>[] = [];
    fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/classes": () => ({ body: [rke2] }),
      "GET /api/versions/rke2": () => ({ body: { versions: ["v1.36.5+rke2r1"], stable: "v1.36.5+rke2r1" } }),
      "GET /api/clusters": () => ({ body: [{ ...web, variables: { network } }] }),
      "GET /api/proxmox/storages": () => ({ body: [] }),
      "GET /api/proxmox/nodes": () => ({ body: [{ node: "pve1", online: true }] }),
      "GET /api/proxmox/nodes/pve1/bridges": () => ({
        body: [
          { bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true, cidr: "10.0.0.5/16", gateway: "10.0.0.1" },
        ],
      }),
      "GET /api/proxmox/nodes/pve1/dns": () => ({ body: { servers: ["10.0.0.53"], search: "" } }),
      "POST /api/proxmox/nodes/pve1/bridges/vmbr0/free": (body) => {
        const req = body as { nodes: number; gateway?: string; prefix?: number };
        if (req.nodes > 0) {
          asked.push(req);
        }
        // The server searches the network the gateway and prefix length give.
        const lan = (req.gateway ?? "10.0.0.1").split(".").slice(0, 3).join(".");
        if (req.prefix === 30) {
          return { body: { network: `${lan}.0/30`, full: true, answering: [] } };
        }
        return {
          body: {
            network: `${lan}.0/${req.prefix ?? 16}`,
            endpoint: `${lan}.20`,
            addresses: `${lan}.21-${lan}.23`,
            answering: [],
          },
        };
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await waitFor(async () => expect(await screen.findByLabelText("Start from")).toHaveValue("default/web"));
    await user.type(screen.getByLabelText("Name"), "next");
    await waitFor(() => expect(screen.getByLabelText("Kubernetes version")).toHaveValue("v1.36.5+rke2r1"));
    await user.click(screen.getByRole("button", { name: "Next" }));

    const addresses = await screen.findByLabelText("Node addresses");
    await waitFor(() => expect(addresses).toHaveValue("10.0.70.21-10.0.70.23"));
    expect(screen.getByLabelText("API address")).toHaveValue("10.0.70.20");
    expect(asked.at(-1)).toMatchObject({ gateway: "10.0.70.1", prefix: 24 });
    expect(addresses).toHaveAccessibleDescription("Ranges like 10.0.70.21-10.0.70.29, networks or addresses");

    // The suggestion follows the gateway.
    const gateway = screen.getByLabelText("Gateway");
    await user.clear(gateway);
    await user.type(gateway, "10.0.80.1");
    await waitFor(() => expect(addresses).toHaveValue("10.0.80.21-10.0.80.23"));
    expect(screen.getByLabelText("API address")).toHaveValue("10.0.80.20");

    // A full network leaves the fields empty and says so.
    const prefix = screen.getByLabelText("Prefix length");
    await user.clear(prefix);
    await user.type(prefix, "30");
    await waitFor(() => expect(addresses).toHaveAccessibleDescription("No free addresses found in 10.0.80.0/30"));
    expect(addresses).toHaveValue("");
    expect(screen.getByLabelText("API address")).toHaveValue("");

    // Typed addresses replace the message.
    await user.type(addresses, "10.0.80.2");
    expect(addresses).toHaveAccessibleDescription(/Ranges like/u);
    expect(addresses).not.toHaveAccessibleDescription(/No free addresses/u);
  });

  it("suggests IPv6 addresses on the IPv6 network it took", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const network = { bridge: "vmbr0", ipFamilies: ["IPv6"], ipv6: { gateway: "2001:db8:7::1", prefix: 64 } };
    const families: ClusterClass = {
      ...rke2,
      variables: rke2.variables.map((v) =>
        v.name === "network" ? { ...v, schema: { type: "object", properties: { ipFamilies: { type: "array" } } } } : v,
      ),
    };
    fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/classes": () => ({ body: [families] }),
      "GET /api/versions/rke2": () => ({ body: { versions: ["v1.36.5+rke2r1"], stable: "v1.36.5+rke2r1" } }),
      "GET /api/clusters": () => ({ body: [{ ...web, variables: { network } }] }),
      "GET /api/proxmox/storages": () => ({ body: [] }),
      "GET /api/proxmox/nodes": () => ({ body: [{ node: "pve1", online: true }] }),
      "GET /api/proxmox/nodes/pve1/bridges": () => ({
        body: [
          {
            bridge: "vmbr0",
            node: "pve1",
            vlanAware: false,
            usable: true,
            cidr: "10.0.0.5/16",
            gateway: "10.0.0.1",
            cidr6: "2001:db8:1::5/64",
            gateway6: "2001:db8:1::1",
          },
        ],
      }),
      "GET /api/proxmox/nodes/pve1/dns": () => ({ body: { servers: [], search: "" } }),
      "POST /api/proxmox/nodes/pve1/bridges/vmbr0/free": (body) => {
        const req = body as { gateway6?: string; prefix6?: number };
        return req.gateway6 === "2001:db8:7::1" && req.prefix6 === 64
          ? {
              body: {
                network6: "2001:db8:7::/64",
                endpoint6: "2001:db8:7::14",
                addresses6: "2001:db8:7::15-2001:db8:7::17",
                answering: [],
              },
            }
          : {
              body: {
                network6: "2001:db8:1::/64",
                endpoint6: "2001:db8:1::14",
                addresses6: "2001:db8:1::15",
                answering: [],
              },
            };
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await waitFor(async () => expect(await screen.findByLabelText("Start from")).toHaveValue("default/web"));
    await user.type(screen.getByLabelText("Name"), "next");
    await waitFor(() => expect(screen.getByLabelText("Kubernetes version")).toHaveValue("v1.36.5+rke2r1"));
    await user.click(screen.getByRole("button", { name: "Next" }));

    const addresses = await screen.findByLabelText("Node addresses");
    await waitFor(() => expect(addresses).toHaveValue("2001:db8:7::15-2001:db8:7::17"));
    expect(screen.getByLabelText("API address")).toHaveValue("2001:db8:7::14");
    expect(screen.getByLabelText("Gateway")).toHaveValue("2001:db8:7::1");
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
