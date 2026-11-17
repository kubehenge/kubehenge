import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { fieldProblems, newForm, request, summary, type FieldKey, type Form } from "./create-form";
import { NetworkStep } from "./CreateSteps";
import { applyStaged, fakeServer } from "./fake-server";
import { firewallText, securityGroupsProblem } from "./Firewall";
import type { Cluster, ClusterClass } from "./types";

const withFirewall: ClusterClass = {
  name: "kubehenge-k3s",
  distribution: "k3s",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
  variables: [
    { name: "network", required: true, schema: { type: "object" } },
    { name: "securityGroups", required: false, schema: { type: "array" } },
    { name: "firewall", required: false, schema: { type: "object" } },
  ],
};
const withoutFirewall: ClusterClass = { ...withFirewall, variables: withFirewall.variables.slice(0, 1) };

const filled: Form = { ...newForm(), bridge: "vmbr0", gateway: "10.0.0.1", dnsServers: "10.0.0.1" };

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
  variables: { firewall: { enabled: true } },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

// nextUntilCreate steps through the wizard to Confirm, whose button creates.
async function nextUntilCreate(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  if (screen.queryByRole("button", { name: "Create" }) !== null) {
    return;
  }
  await user.click(screen.getByRole("button", { name: "Next" }));
  await nextUntilCreate(user);
}

describe("a new cluster's firewall", () => {
  it("is on by default on the Network step, with its ports and groups", async () => {
    const set = vi.fn<(k: keyof Form, v: unknown) => void>();
    const noProblems = new Map<FieldKey, string>();
    const step = (cls: ClusterClass, form = newForm()): JSX.Element => (
      <NetworkStep form={form} set={set} shown={(k) => noProblems.get(k)} cls={cls} bridgeOptions={[]} />
    );
    const user = userEvent.setup({ delay: null });
    const { rerender } = render(step(withFirewall));

    expect(screen.getByRole("checkbox", { name: "Firewall" })).toBeChecked();
    await user.type(screen.getByLabelText("Further open ports"), "t");
    expect(set).toHaveBeenLastCalledWith("firewallPorts", "t");
    await user.type(screen.getByLabelText("Firewall security groups"), "k");
    expect(set).toHaveBeenLastCalledWith("securityGroups", "k");
    expect(screen.getByRole("checkbox", { name: "Serving ports open" })).toBeChecked();
    await user.click(screen.getByRole("checkbox", { name: "Serving ports open" }));
    expect(set).toHaveBeenCalledWith("firewallServing", false);
    await user.click(screen.getByRole("checkbox", { name: "Firewall" }));
    expect(set).toHaveBeenCalledWith("firewall", false);

    rerender(step(withFirewall, { ...newForm(), firewall: false }));
    expect(screen.queryByRole("checkbox", { name: "Serving ports open" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Further open ports")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Firewall security groups")).not.toBeInTheDocument();

    rerender(step(withoutFirewall));
    expect(screen.queryByRole("checkbox", { name: "Firewall" })).not.toBeInTheDocument();
  });

  it("is sent explicitly and shown on Confirm", () => {
    expect(request(filled, withFirewall).variables["firewall"]).toStrictEqual({ enabled: true });
    expect(request(filled, withFirewall).variables).not.toHaveProperty("securityGroups");
    expect(Object.fromEntries(summary(filled, withFirewall))["Firewall"]).toBe("On, serving ports open");

    const open = { ...filled, firewallPorts: "tcp/8443, udp/4000-4010", securityGroups: " k8s, corp-base " };
    expect(request(open, withFirewall).variables["firewall"]).toStrictEqual({
      enabled: true,
      ports: [
        { protocol: "tcp", port: 8443 },
        { protocol: "udp", port: 4000, endPort: 4010 },
      ],
    });
    expect(request(open, withFirewall).variables["securityGroups"]).toStrictEqual(["k8s", "corp-base"]);
    expect(Object.fromEntries(summary(open, withFirewall))["Firewall"]).toBe(
      "On, serving ports open, also tcp/8443, udp/4000-4010, security groups k8s, corp-base",
    );

    const closed = { ...filled, firewallServing: false, securityGroups: "k8s" };
    expect(request(closed, withFirewall).variables["firewall"]).toStrictEqual({ enabled: true, servingPorts: false });
    expect(Object.fromEntries(summary(closed, withFirewall))["Firewall"]).toBe(
      "On, serving ports closed, security groups k8s",
    );

    const off = { ...open, firewall: false, firewallServing: false };
    expect(request(off, withFirewall).variables["firewall"]).toStrictEqual({ enabled: false });
    expect(request(off, withFirewall).variables).not.toHaveProperty("securityGroups");
    expect(Object.fromEntries(summary(off, withFirewall))["Firewall"]).toBe("Off");
    expect(request(filled, withoutFirewall).variables).not.toHaveProperty("firewall");
  });

  it.each([
    { state: "off", says: "Proxmox VE's datacenter firewall is off, so this cluster's firewall does nothing" },
    { state: "unknown", says: "Datacenter firewall state unknown" },
  ] as const)("says on its step and on Confirm when the datacenter firewall is $state", (seen) => {
    const step = (form: Form, datacenter?: "on" | "off" | "unknown"): JSX.Element => (
      <NetworkStep
        form={form}
        set={vi.fn<(k: keyof Form, v: unknown) => void>()}
        shown={(k) => new Map<FieldKey, string>().get(k)}
        cls={withFirewall}
        bridgeOptions={[]}
        datacenter={datacenter}
      />
    );
    const { rerender } = render(step(filled, seen.state));
    expect(screen.getByText(seen.says)).toBeInTheDocument();
    expect(Object.fromEntries(summary(filled, withFirewall, { datacenterFirewall: seen.state }))["Firewall"]).toBe(
      `On, serving ports open. ${seen.says}`,
    );

    rerender(step(filled, "on"));
    expect(screen.queryByText(seen.says)).not.toBeInTheDocument();
    expect(Object.fromEntries(summary(filled, withFirewall, { datacenterFirewall: "on" }))["Firewall"]).toBe(
      "On, serving ports open",
    );

    const off = { ...filled, firewall: false };
    rerender(step(off, seen.state));
    expect(screen.queryByText(seen.says)).not.toBeInTheDocument();
    expect(Object.fromEntries(summary(off, withFirewall, { datacenterFirewall: seen.state }))["Firewall"]).toBe("Off");
  });

  it("reads the datacenter firewall through the session and still creates with it off", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const cls: ClusterClass = {
      ...withFirewall,
      variables: [
        { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
        ...withFirewall.variables,
      ],
    };
    const says = "Proxmox VE's datacenter firewall is off, so this cluster's firewall does nothing";
    const calls = fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1", canCreate: true, canTrust: true } }),
      "GET /api/classes": () => ({ body: [cls] }),
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
      "GET /api/proxmox/datacenter-firewall": () => ({ body: { state: "off" } }),
      "POST /api/clusters?dryRun=All": () => ({ body: { manifest: "kind: Cluster\n" } }),
      "POST /api/clusters": () => ({ status: 201, body: { ...cluster, namespace: "default" } }),
      "GET /api/clusters/default/web": () => ({ body: { ...cluster, namespace: "default" } }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.type(await screen.findByLabelText("Name"), "web");
    await waitFor(() => expect(screen.getByRole("option", { name: "k3s" })).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-k3s");
    await user.type(screen.getByLabelText("Kubernetes version"), "v1.36.5+k3s1");
    await user.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() => expect(screen.getByLabelText("Bridge")).toHaveValue("vmbr0"));
    await expect(screen.findByText(says)).resolves.toBeInTheDocument();
    await user.type(screen.getByLabelText("API address"), "10.0.0.20");
    await user.type(screen.getByLabelText("Node addresses"), "10.0.0.21-10.0.0.29");
    await user.type(screen.getByLabelText("Gateway"), "10.0.0.1");
    await user.type(screen.getByLabelText("DNS servers"), "10.0.0.1");
    await nextUntilCreate(user);
    expect(screen.getByText(`On, serving ports open. ${says}`)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Create" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("POST /api/clusters"));
  });

  it("refuses ports it cannot write and group names Proxmox VE refuses", () => {
    for (const ok of ["", "tcp/8443", "udp/1-65535, tcp/80"]) {
      expect(fieldProblems("Network", { ...filled, firewallPorts: ok }, withFirewall)).not.toHaveProperty(
        "firewallPorts",
      );
    }
    for (const bad of ["8443", "sctp/9", "tcp/0", "tcp/70000", "udp/10-5"]) {
      expect(fieldProblems("Network", { ...filled, firewallPorts: bad }, withFirewall)["firewallPorts"]).toBe(
        "Ports as tcp/8443 or ranges as udp/4000-4010",
      );
    }
    const off = fieldProblems(
      "Network",
      { ...filled, firewall: false, firewallPorts: "bad", securityGroups: "1k8s" },
      withFirewall,
    );
    expect(off).not.toHaveProperty("firewallPorts");
    expect(off).not.toHaveProperty("securityGroups");
    for (const ok of ["", "k8s", "corp_base, k8s-nodes", "abcdefghijklmnopqr"]) {
      expect(securityGroupsProblem(ok)).toBeUndefined();
    }
    for (const bad of ["1k8s", "k", "abcdefghijklmnopqrs", "k8s.nodes", "k8s nodes;x"]) {
      expect(securityGroupsProblem(bad)).toBe(
        "Names of 2 to 18 letters, digits, hyphens and underscores, starting with a letter",
      );
    }
    expect(securityGroupsProblem("k8s, k8s")).toBe("Each group once");
    expect(securityGroupsProblem("k8s", false)).toBe("Security groups need the firewall on");
  });
});

describe("a cluster's firewall in its options", () => {
  it.each([
    { status: "True", says: "On, serving ports open. Inactive, datacenter firewall off" },
    { status: "Unknown", says: "On, serving ports open. State unknown" },
    { status: "False", says: "On, serving ports open" },
  ])("says what the controller found of it, $status", async ({ status, says }) => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const found = {
      ...cluster,
      conditions: [{ type: "FirewallInactive", status, reason: "DatacenterFirewallOff", since: cluster.created }],
    };
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: found },
    });
    render(<App />);
    await expect(screen.findByRole("cell", { name: says })).resolves.toBeInTheDocument();
  });

  it("switches off, which drops the groups", async () => {
    expect(firewallText({ on: false, serving: true, ports: "" }, "k8s")).toBe("Off");
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const held = { ...cluster, variables: { firewall: { enabled: true }, securityGroups: ["k8s"] } };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: held },
      "PATCH /api/clusters/clusters/web": { body: held },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(
      screen.findByRole("cell", { name: "On, serving ports open, security groups k8s" }),
    ).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit the firewall" }));
    const dialog = screen.getByRole("dialog", { name: "Edit firewall" });
    const save = within(dialog).getByRole("button", { name: "Add to plan" });
    expect(save).toBeDisabled();
    await user.click(within(dialog).getByRole("checkbox", { name: "Firewall" }));
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { firewall: { enabled: false }, securityGroups: null },
    });
  });

  it("opens the serving ports when it turns on the firewall of a cluster made before it", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const older = { ...cluster, variables: {} };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: older },
      "PATCH /api/clusters/clusters/web": { body: older },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit the firewall" }));
    const dialog = screen.getByRole("dialog", { name: "Edit firewall" });
    await user.click(within(dialog).getByRole("checkbox", { name: "Firewall" }));
    expect(within(dialog).getByRole("checkbox", { name: "Serving ports open" })).toBeChecked();
    await user.click(within(dialog).getByRole("button", { name: "Add to plan" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    // The class defaults servingPorts to true, so the variable without it opens them.
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { firewall: { enabled: true }, securityGroups: null },
    });
  });

  it("closes and opens the serving ports", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const held = {
      ...cluster,
      variables: { firewall: { enabled: true, servingPorts: false }, securityGroups: ["k8s"] },
    };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: held },
      "PATCH /api/clusters/clusters/web": { body: held },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(
      screen.findByRole("cell", { name: "On, serving ports closed, security groups k8s" }),
    ).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit the firewall" }));
    const dialog = screen.getByRole("dialog", { name: "Edit firewall" });
    const serving = within(dialog).getByRole("checkbox", { name: "Serving ports open" });
    expect(serving).not.toBeChecked();
    const save = within(dialog).getByRole("button", { name: "Add to plan" });
    expect(save).toBeDisabled();
    await user.click(serving);
    await user.click(save);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { firewall: { enabled: true }, securityGroups: ["k8s"] },
    });
  });

  it("adds a port, refusing one it cannot write", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.click(await screen.findByRole("button", { name: "Edit the firewall" }));
    const dialog = screen.getByRole("dialog", { name: "Edit firewall" });
    const ports = within(dialog).getByLabelText("Further open ports");
    await user.type(ports, "8443");
    await user.click(within(dialog).getByRole("button", { name: "Add to plan" }));
    expect(within(dialog).getByText("Ports as tcp/8443 or ranges as udp/4000-4010")).toBeInTheDocument();
    await user.clear(ports);
    await user.type(ports, "tcp/8443");
    await user.click(within(dialog).getByRole("button", { name: "Add to plan" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await applyStaged(user);
    expect(calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body).toStrictEqual({
      variables: { firewall: { enabled: true, ports: [{ protocol: "tcp", port: 8443 }] }, securityGroups: null },
    });
  });
});
