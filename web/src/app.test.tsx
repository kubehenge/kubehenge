import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { noAnswer } from "./api";
import { App } from "./App";
import { ClusterList } from "./ClusterList";
import { fieldProblems, problems, request, summary, type Form } from "./create-form";
import { fakeServer, held, reads, type Route } from "./fake-server";
import { addressCount } from "./network";
import type { Cluster, ClusterClass, Machine } from "./types";

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: "^v1\\.\\d+\\.\\d+\\+rke2r\\d+$" },
  variables: [
    { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
    { name: "network", required: true, schema: { type: "object" } },
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "allowedNodes", required: false, schema: { type: "array" } },
    {
      name: "controlPlaneMachine",
      required: false,
      schema: { type: "object", default: { cores: 2, memoryMiB: 4096, diskGiB: 30 } },
    },
    {
      name: "workerMachine",
      required: false,
      schema: { type: "object", default: { cores: 2, memoryMiB: 4096, diskGiB: 40 } },
    },
    { name: "cni", required: false, schema: { type: "string", enum: ["canal", "calico", "cilium"], default: "canal" } },
    { name: "headlamp", required: false, schema: { type: "object" } },
    { name: "monitoring", required: false, schema: { type: "object" } },
    { name: "podSecurity", required: false, schema: { type: "object" } },
    { name: "cis", required: false, schema: { type: "boolean", default: false } },
    { name: "rancher", required: false, schema: { type: "object" } },
    {
      name: "oidc",
      required: false,
      schema: {
        type: "array",
        default: [{ issuerURL: "https://sso.example", clientID: "kubernetes", usernamePrefix: "oidc:" }],
      },
    },
  ],
};

const runningOn = (name: string, proxmoxNode: string, role: Machine["role"] = "control-plane"): Machine => ({
  name,
  role,
  proxmoxNode,
  phase: "Running",
  ready: true,
  created: new Date().toISOString(),
});

const web: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  created: new Date().toISOString(),
  createdBy: "alice@pve",
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [{ type: "Available", status: "True", since: new Date().toISOString() }],
  endpoint: "10.0.0.20:6443",
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 2, ready: 2, available: 2, upToDate: 2 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 2 }],
  variables: { storage: "local-lvm" },
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

const workerCores = (): HTMLElement => within(screen.getByRole("group", { name: "Workers" })).getByLabelText("Cores");

describe("sign-in", () => {
  it("signs in with a password and a TOTP code, then sends the CSRF token", async () => {
    const calls = fakeServer({
      "GET /api/session": () => ({ status: 401, body: { error: "not signed in" } }),
      "GET /api/realms": () => ({
        body: [
          { realm: "pam", type: "pam" },
          { realm: "pve", type: "pve", default: true },
        ],
      }),
      "POST /api/session": () => ({
        body: { user: "alice@pve", csrf: "c1", secondFactor: { totp: true, yubico: false, recovery: [0, 1] } },
      }),
      "POST /api/session/second-factor": () => ({ body: { user: "alice@pve", csrf: "c2" } }),
      "GET /api/clusters": () => ({ body: [web] }),
      "DELETE /api/session": () => ({ status: 204 }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.type(await screen.findByLabelText("User name"), "alice");
    await user.type(screen.getByLabelText("Password"), "secret");
    await waitFor(() => expect(screen.getByLabelText("Realm")).toHaveValue("pve"));
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    expect(calls.find((c) => c.key === "POST /api/session")?.body).toStrictEqual({
      username: "alice",
      realm: "pve",
      password: "secret",
    });
    await user.type(await screen.findByLabelText("TOTP code"), "123456");
    await user.click(screen.getByRole("button", { name: "Confirm" }));

    await expect(screen.findByRole("link", { name: "web" })).resolves.toBeInTheDocument();
    expect(screen.getByText("alice@pve", { selector: ".user" })).toBeInTheDocument();
    expect(calls.find((c) => c.key === "POST /api/session/second-factor")?.headers.get("X-Kubehenge-CSRF")).toBe("c1");

    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(calls.find((c) => c.key === "DELETE /api/session")?.headers.get("X-Kubehenge-CSRF")).toBe("c2");
    await expect(screen.findByRole("button", { name: "Sign in" })).resolves.toBeInTheDocument();
  });

  it("sends OpenID realms to their identity provider and shows a failed return", async () => {
    window.history.replaceState(null, "", "/?signin-error=the+sign-in+through+the+identity+provider+failed");
    fakeServer({
      "GET /api/session": () => ({ status: 401 }),
      "GET /api/realms": () => ({
        body: [
          { realm: "pve", type: "pve", default: true },
          { realm: "sso", type: "openid", comment: "Company SSO" },
        ],
      }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(
      "The sign-in through the identity provider failed.",
    );
    await waitFor(() => expect(screen.getByLabelText("Realm")).toHaveValue("pve"));
    await user.selectOptions(screen.getByLabelText("Realm"), "sso");
    expect(screen.queryByLabelText("Password")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Continue with Company SSO" })).toHaveAttribute(
      "href",
      "/api/openid/start?realm=sso",
    );
  });

  it("shows Proxmox VE's refusal", async () => {
    fakeServer({
      "GET /api/session": () => ({ status: 401 }),
      "GET /api/realms": () => ({ body: [{ realm: "pve", type: "pve" }] }),
      "POST /api/session": () => ({
        status: 401,
        body: { error: "sign-in failed: check the user name, password and realm" },
      }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const name = await screen.findByLabelText("User name");
    expect(name).toHaveFocus();
    await user.type(name, "alice@pve");
    expect(screen.getByLabelText("Realm")).toBeDisabled();
    await user.type(screen.getByLabelText("Password"), "wrong");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(
      "Sign-in failed: check the user name, password and realm.",
    );
  });

  it("offers the realm last signed in with, while Proxmox VE still has it", async () => {
    const realms = [
      { realm: "pam", type: "pam" },
      { realm: "pve", type: "pve" },
    ];
    const calls = fakeServer({
      "GET /api/session": () => ({ status: 401 }),
      "GET /api/realms": () => ({ body: realms }),
      "POST /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/clusters": () => ({ body: [] }),
      "DELETE /api/session": () => ({ status: 204 }),
    });
    const user = userEvent.setup({ delay: null });
    const { unmount } = render(<App />);
    await waitFor(() => expect(screen.getByLabelText("Realm")).toHaveValue("pam"));
    await user.selectOptions(screen.getByLabelText("Realm"), "pve");
    await user.type(screen.getByLabelText("User name"), "alice");
    await user.type(screen.getByLabelText("Password"), "secret{Enter}");
    await user.click(await screen.findByRole("button", { name: "Sign out" }));
    await waitFor(() => expect(screen.getByLabelText("Realm")).toHaveValue("pve"));
    expect(calls.filter((c) => c.key === "POST /api/session")).toHaveLength(1);

    unmount();
    realms.pop();
    render(<App />);
    await waitFor(() => expect(screen.getByLabelText("Realm")).toHaveValue("pam"));
  });

  it("lets a second factor be answered with a recovery key", async () => {
    const calls = fakeServer({
      "GET /api/session": () => ({ status: 401 }),
      "GET /api/realms": () => ({ body: [{ realm: "pve", type: "pve", default: true }] }),
      "POST /api/session": () => ({
        body: { user: "alice@pve", csrf: "c1", secondFactor: { totp: true, yubico: false, recovery: [2] } },
      }),
      "POST /api/session/second-factor": () => ({ status: 401, body: { error: "the code was not accepted" } }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.type(await screen.findByLabelText("User name"), "alice");
    await user.type(screen.getByLabelText("Password"), "secret");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await expect(screen.findByLabelText("TOTP code")).resolves.toHaveFocus();
    expect(screen.queryByRole("radio", { name: "YubiKey (Yubico OTP)" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: "A recovery key" }));
    const key = screen.getByLabelText("Recovery key");
    expect(screen.getByText("Unused keys: 3")).toBeInTheDocument();
    await user.type(key, "abcd-efgh");
    await user.click(screen.getByRole("button", { name: "Confirm" }));
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("The code was not accepted.");
    expect(calls.find((c) => c.key === "POST /api/session/second-factor")?.body).toStrictEqual({
      kind: "recovery",
      code: "abcd-efgh",
    });
  });
});

describe("clusters", () => {
  const signedIn = { "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }) };

  it("deletes a cluster only once its name is typed", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": () => ({ body: web }),
      "DELETE /api/clusters/default/web": () => ({ status: 202 }),
      "GET /api/clusters": () => ({ body: [] }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const dialog = screen.getByRole("dialog", { name: "Delete web" });
    const confirm = within(dialog).getByRole("button", { name: "Delete" });
    expect(confirm).toBeDisabled();
    expect(within(dialog).getByLabelText("Confirm name")).toHaveAccessibleDescription("Type web.");
    await user.type(within(dialog).getByLabelText("Confirm name"), "web");
    await user.click(confirm);
    await waitFor(() => expect(window.location.pathname).toBe("/"));
    expect(calls.find((c) => c.key === "DELETE /api/clusters/default/web")?.headers.get("X-Kubehenge-CSRF")).toBe("c1");
  });

  it("scales a worker pool", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": () => ({ body: web }),
      "PATCH /api/clusters/default/web": () => ({ body: web }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Scale" }));
    const workers = screen.getByLabelText("Workers (md-0)");
    await user.clear(workers);
    await user.type(workers, "4");
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Scale" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PATCH /api/clusters/default/web")?.body).toStrictEqual({
        controlPlaneReplicas: 1,
        workerPools: { "md-0": 4 },
      }),
    );
  });

  it("upgrades a cluster on a minor version too old for new clusters to the next one", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const old = { ...web, version: "v1.32.4+rke2r1" };
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": () => ({ body: old }),
      "GET /api/versions/rke2": () => ({
        body: {
          versions: ["v1.37.1+rke2r1", "v1.36.5+rke2r1", "v1.35.9+rke2r2", "v1.34.12+rke2r1"],
          older: ["v1.33.13+rke2r3", "v1.32.13+rke2r2"],
          stable: "v1.36.5+rke2r1",
        },
      }),
      "PATCH /api/clusters/default/web": () => ({ body: old }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Upgrade" }));
    const dialog = screen.getByRole("dialog");
    await waitFor(() =>
      expect(
        within(dialog)
          .getAllByRole("option")
          .map((o) => o.textContent),
      ).toStrictEqual(["v1.32.4+rke2r1 (running)", "v1.33.13+rke2r3", "v1.32.13+rke2r2"]),
    );
    await user.selectOptions(within(dialog).getByLabelText("Kubernetes version"), "v1.33.13+rke2r3");
    await user.click(within(dialog).getByRole("button", { name: "Upgrade" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PATCH /api/clusters/default/web")?.body).toStrictEqual({
        version: "v1.33.13+rke2r3",
      }),
    );
  });

  it("reminds administrators to back up the appliance", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer({
      ...signedIn,
      "GET /api/setup": () => ({ body: { appliance: true, ready: true, allowed: true, backup: false } }),
      "GET /api/clusters": () => ({ body: [web] }),
    });
    render(<App />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(
      "No Proxmox VE backup job includes this appliance",
    );
  });

  it("says a cluster waits for its ClusterClass while its distribution's controllers start", async () => {
    window.history.replaceState(null, "", "/");
    const since = new Date().toISOString();
    fakeServer({
      ...signedIn,
      "GET /api/clusters": () => ({
        body: [
          {
            ...web,
            phase: "Pending",
            available: false,
            conditions: [{ type: "TopologyReconciled", status: "False", reason: "ClusterClassNotReconciled", since }],
          },
        ],
      }),
    });
    render(<App />);
    await expect(screen.findByText("Waiting for its ClusterClass")).resolves.toBeInTheDocument();
  });

  it("links a cluster to the shared Headlamp", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1", headlamp: 8444 } }),
      "GET /api/clusters/default/web": () => ({ body: { ...web, headlamp: "default.web" } }),
    });
    render(<App />);
    await screen.findByRole("button", { name: "Scale" });
    const links = screen.getAllByRole("link", { name: "Headlamp" });
    expect(links.map((a) => a.getAttribute("href"))).toStrictEqual([
      `https://${location.hostname}:8444/`,
      `https://${location.hostname}:8444/c/default.web`,
    ]);
    for (const a of links) {
      expect(a).toHaveAttribute("rel", "noopener noreferrer");
    }
  });

  it("shows machines in their own view, and says when the cluster is gone", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/machines");
    const routes: Record<string, Route> = {
      ...signedIn,
      "GET /api/clusters/default/web": {
        body: {
          ...web,
          conditions: [{ type: "ControlPlaneAvailable", status: "False", since: new Date().toISOString() }],
        },
      },
    };
    fakeServer(routes);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ delay: null, advanceTimers: vi.advanceTimersByTime });
    render(<App />);
    await expect(screen.findByText("No machines yet.")).resolves.toBeInTheDocument();
    routes["GET /api/clusters/default/web"] = { status: 404, body: { error: "no such cluster" } };
    await user.click(screen.getByRole("link", { name: "Conditions" }));
    expect(screen.getByRole("cell", { name: /^Needs attention:\s*ControlPlaneAvailable$/u })).toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: "Summary" }));
    // Nothing moves in it, so its page asks again only every 30 s.
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(screen.queryByText(/This cluster no longer exists/u)).not.toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(25_000));
    await expect(screen.findByText(/This cluster no longer exists/u)).resolves.toBeInTheDocument();
  });

  it("shows where the VMs run, and warns when control-plane VMs share a node", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/machines");
    fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": {
        body: {
          ...web,
          machines: [
            runningOn("cp-a", "pve1"),
            runningOn("cp-b", "pve1"),
            runningOn("cp-c", "pve2"),
            runningOn("md-a", "pve1", "worker"),
          ],
        },
      },
    });
    render(<App />);
    await expect(
      screen.findByText(/^pve1 runs 2 of the 3 control-plane VMs, so losing that node stops the control plane\./u),
    ).resolves.toBeInTheDocument();
    expect(screen.getAllByRole("cell", { name: "pve1" })).toHaveLength(3);
  });

  it("says what a machine that is not ready waits for", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/machines");
    const waiting = "* InfrastructureReady:\n  * VirtualMachineProvisioned: VM template not found";
    fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": {
        body: { ...web, machines: [{ ...runningOn("cp-a", "pve1"), ready: false, phase: "Provisioning", waiting }] },
      },
    });
    render(<App />);
    const name = await screen.findByRole("cell", { name: /^cp-a/u });
    expect(within(name).getByText(/VM template not found/u)).toHaveTextContent(
      "* InfrastructureReady: * VirtualMachineProvisioned: VM template not found",
    );
  });

  it("reads the setup status no more once signed out, even when the last read fails after", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const setup = held();
    const calls = fakeServer({ ...signedIn, "GET /api/setup": setup.route, "GET /api/clusters": { body: [] } });
    const { unmount } = render(<App />);
    await waitFor(() => expect(reads(calls, "GET /api/setup")).toBe(1));
    unmount();
    setup.fail(new TypeError("Failed to fetch"));
    await act(() => vi.advanceTimersByTimeAsync(60_000));
    expect(reads(calls, "GET /api/setup")).toBe(1);
  });

  it("hides what the user may not do", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": () => ({ body: { ...web, allowed: { change: false, kubeconfig: false } } }),
    });
    render(<App />);
    await expect(screen.findByRole("button", { name: "Delete" })).resolves.toBeDisabled();
    expect(screen.queryByRole("link", { name: "Kubeconfig" })).not.toBeInTheDocument();
  });
});

describe("create", () => {
  const form: Form = {
    name: "web",
    className: "kubehenge-rke2",
    version: "v1.36.5+rke2r1",
    controlPlaneReplicas: 3,
    workerReplicas: 2,
    endpoint: "10.0.0.20",
    bridge: "vmbr0",
    vlan: "",
    addresses: "10.0.0.21-10.0.0.29\n10.0.0.32/28",
    prefix: 24,
    gateway: "10.0.0.1",
    dnsServers: "10.0.0.1, 1.1.1.1",
    pods: "",
    services: "",
    storage: "local-lvm",
    place: { placement: "any", haRule: "", nodes: [] },
    proxmoxHA: true,
    controlPlaneMachine: { cores: 2, memoryMiB: 4096, diskGiB: 30 },
    workerMachine: { cores: 4, memoryMiB: 8192, diskGiB: 40 },
    cni: "cilium",
    headlamp: true,
    headlampHostname: "",
    monitoring: true,
    grafanaHostname: "grafana.example",
    podSecurity: { admission: "restricted", kyverno: "off" },
    cis: false,
    rancherInstall: false,
    rancherHostname: "",
    rancherRegister: false,
    oidc: [],
    upgradeMode: "off",
  };

  it("keeps the VMs to the nodes of the placement chosen", () => {
    expect(request(form, rke2).variables).not.toHaveProperty("allowedNodes");
    const byRule = { ...form, place: { placement: "rule" as const, haRule: "fast", nodes: ["pve2", "pve3"] } };
    const chosen = { ...form, place: { placement: "nodes" as const, haRule: "", nodes: ["pve1"] } };
    const noRule = { ...form, place: { placement: "rule" as const, haRule: "", nodes: [] } };
    const noNodes = { ...form, place: { placement: "nodes" as const, haRule: "", nodes: [] } };
    expect(request(byRule, rke2).variables["allowedNodes"]).toStrictEqual(["pve2", "pve3"]);
    expect(Object.fromEntries(summary(byRule, rke2))["Placement"]).toBe("The nodes of HA rule fast: pve2, pve3");
    expect(Object.fromEntries(summary(chosen, rke2))["Placement"]).toBe("pve1");
    expect(Object.fromEntries(summary(form, rke2))["Placement"]).toBe("Any node");
    expect(problems("Machines", noRule, rke2)).toStrictEqual(["Choose an HA rule."]);
    expect(problems("Machines", noNodes, rke2)).toStrictEqual(["Choose at least one node."]);
    const unplaced = { ...rke2, variables: rke2.variables.filter((v) => v.name !== "allowedNodes") };
    expect(request(byRule, unplaced).variables).not.toHaveProperty("allowedNodes");
    expect(problems("Machines", noNodes, unplaced)).toStrictEqual([]);
  });

  it("builds the request from the form", () => {
    expect(request(form, rke2)).toStrictEqual({
      name: "web",
      class: "kubehenge-rke2",
      version: "v1.36.5+rke2r1",
      controlPlaneReplicas: 3,
      workerReplicas: 2,
      variables: {
        controlPlaneEndpoint: "10.0.0.20",
        network: {
          bridge: "vmbr0",
          addresses: ["10.0.0.21-10.0.0.29", "10.0.0.32/28"],
          prefix: 24,
          gateway: "10.0.0.1",
          dnsServers: ["10.0.0.1", "1.1.1.1"],
        },
        storage: "local-lvm",
        controlPlaneMachine: { cores: 2, memoryMiB: 4096, diskGiB: 30 },
        workerMachine: { cores: 4, memoryMiB: 8192, diskGiB: 40 },
        cni: "cilium",
        headlamp: { enabled: true },
        monitoring: { enabled: true, hostname: "grafana.example" },
        podSecurity: { admission: "restricted" },
        rancher: { register: false },
        oidc: [],
      },
    });
    const defaults = { ...form, podSecurity: { admission: "privileged" as const, kyverno: "off" as const } };
    expect(request(defaults, rke2).variables).not.toHaveProperty("podSecurity");
    expect(request(defaults, rke2).variables).not.toHaveProperty("cis");
    // CIS hardening has admission enforce restricted, and the variable says so.
    expect(request({ ...defaults, cis: true }, rke2).variables).toMatchObject({
      cis: true,
      podSecurity: { admission: "restricted" },
    });
    const unhardened = { ...rke2, variables: rke2.variables.filter((v) => v.name !== "cis") };
    expect(request({ ...defaults, cis: true }, unhardened).variables).not.toHaveProperty("podSecurity");
  });

  it("sums the form up as the Confirm step shows it", () => {
    const rows = Object.fromEntries(summary(form, rke2));
    expect(rows).toMatchObject({
      Name: "web",
      Distribution: "RKE2",
      "Node addresses": "10.0.0.21-10.0.0.29, 10.0.0.32/28",
      "Pod range": "Chosen clear of the nodes' network",
      "Worker machines": "4 cores, 8 GiB memory, 40 GiB disk",
      Headlamp: "On",
      Monitoring: "On, Grafana at grafana.example",
      "CIS hardening": "Off",
      "Pod security": "Restricted by Pod Security Admission",
      Rancher: "Not registered",
    });
    expect(
      Object.fromEntries(summary({ ...form, podSecurity: { admission: "baseline", kyverno: "restricted" } }, rke2)),
    ).toMatchObject({ "Pod security": "Baseline by Pod Security Admission, Restricted by Kyverno" });
    expect(
      Object.fromEntries(
        summary({ ...form, cis: true, podSecurity: { admission: "baseline", kyverno: "restricted" } }, rke2),
      ),
    ).toMatchObject({
      "CIS hardening": "On",
      "Pod security": "Restricted by Pod Security Admission, Restricted by Kyverno",
    });
    expect(
      Object.fromEntries(summary({ ...form, vlan: "30", rancherInstall: true, rancherHostname: "r.example" }, rke2)),
    ).toMatchObject({
      Bridge: "vmbr0, VLAN 30",
      Rancher: "Installed here, at r.example",
    });
  });

  it("starts automatic upgrades with a window in the browser's time zone", () => {
    const withUpgrades = {
      ...rke2,
      variables: [...rke2.variables, { name: "upgrades", required: false, schema: { type: "object" } }],
    };
    const zone = new Intl.DateTimeFormat().resolvedOptions().timeZone;
    expect(request({ ...form, upgradeMode: "patch" }, withUpgrades).variables["upgrades"]).toStrictEqual({
      mode: "patch",
      window: { start: "02:00", durationMinutes: 240, timeZone: zone },
    });
    expect(request(form, withUpgrades).variables).not.toHaveProperty("upgrades");
    expect(Object.fromEntries(summary({ ...form, upgradeMode: "minor" }, withUpgrades))["Automatic upgrades"]).toBe(
      `Patch and minor releases, every day from 02:00 for 4 h (${zone})`,
    );
  });

  it("keeps an emptied number empty, for the check to name", () => {
    expect(problems("General", { ...form, workerReplicas: Number.NaN }, rke2)).toStrictEqual([
      "A whole number from 0 to 100.",
    ]);
    expect(
      problems("Machines", { ...form, workerMachine: { cores: Number.NaN, memoryMiB: 4096, diskGiB: 40 } }, rke2),
    ).toHaveLength(1);
  });

  it("leaves out variables the class does not define", () => {
    const k3s = { ...rke2, variables: rke2.variables.filter((v) => v.name !== "headlamp") };
    expect(request(form, k3s).variables).not.toHaveProperty("headlamp");
  });

  it("finds what is missing on each step", () => {
    expect(problems("General", form, rke2)).toStrictEqual([]);
    expect(problems("General", { ...form, version: "v1.36.5" }, rke2)).toStrictEqual([
      "RKE2 versions look like v1.36.5+rke2r1.",
    ]);
    expect(problems("General", { ...form, name: "Web" }, rke2)).toHaveLength(1);
    // A cluster without its template would wait for ever for its first VM.
    expect(
      problems("General", form, {
        ...rke2,
        noTemplate:
          "there is no VM template for RKE2 and k3s yet; an administrator builds it on the Administration page",
      }),
    ).toStrictEqual([
      "There is no VM template for RKE2 and k3s yet; an administrator builds it on the Administration page.",
    ]);
    expect(problems("Network", { ...form, endpoint: "10.0.0", vlan: "5000" }, rke2)).toHaveLength(2);
    expect(problems("Network", { ...form, pods: "10.244.0.0", services: "10.96.0.0/16" }, rke2)).toStrictEqual([
      "An IPv4 network such as 10.244.0.0/16, without host bits.",
    ]);
    expect(request({ ...form, pods: "172.30.0.0/16" }, rke2)).toMatchObject({ pods: ["172.30.0.0/16"] });
    expect(request(form, rke2)).not.toHaveProperty("pods");
    expect(
      problems("Machines", { ...form, workerMachine: { cores: 1, memoryMiB: 1000, diskGiB: 40 } }, rke2),
    ).toHaveLength(1);
    expect(
      problems("Machines", { ...form, workerMachine: { cores: 1.5, memoryMiB: 4096, diskGiB: 40 } }, rke2),
    ).toHaveLength(1);
    expect(problems("General", { ...form, workerReplicas: 1.5 }, rke2)).toStrictEqual([
      "A whole number from 0 to 100.",
    ]);
    // Three control-plane nodes, two workers and one for rollouts need six addresses.
    expect(fieldProblems("Network", { ...form, addresses: "10.0.0.21-10.0.0.25" }, rke2).addresses).toMatch(
      /5 given; the nodes need 6/u,
    );
    expect(fieldProblems("Network", { ...form, addresses: "10.0.1.21-10.0.1.29" }, rke2).addresses).toMatch(
      /inside the gateway's network/u,
    );
    expect(fieldProblems("Network", { ...form, addresses: "10.0.0.29-10.0.0.21" }, rke2).addresses).toMatch(
      /is no address/u,
    );
    expect(fieldProblems("Network", { ...form, endpoint: "10.0.0.22" }, rke2).endpoint).toMatch(
      /^Not one of the node addresses/u,
    );
    expect(fieldProblems("Network", { ...form, endpoint: "10.9.0.2" }, rke2).endpoint).toMatch(
      /^Inside the gateway's network/u,
    );
    expect(fieldProblems("Network", { ...form, pods: "10.244.1.0/16" }, rke2).pods).toMatch(/without host bits/u);
    expect(fieldProblems("Network", form, rke2)).toStrictEqual({});
    expect(addressCount(["10.0.0.32/28", "10.0.0.21-10.0.0.29", "10.0.0.50"], "10.0.0.1", 24)).toStrictEqual({
      count: 24,
    });
    expect(problems("Add-ons", { ...form, rancherInstall: true }, rke2)).toStrictEqual(["Rancher needs a host name."]);
    const rancher = { ...form, rancherInstall: true, rancherHostname: "rancher.example.org" };
    const four = { cores: 4, memoryMiB: 8192, diskGiB: 40 };
    expect(
      fieldProblems("Machines", { ...rancher, controlPlaneMachine: { ...four, cores: 2 }, workerMachine: four }, rke2)
        .controlPlaneMachine,
    ).toMatch(/^At least 4 cores, which Rancher needs/u);
    expect(
      fieldProblems("Machines", { ...rancher, controlPlaneMachine: four, workerMachine: { ...four, cores: 2 } }, rke2)
        .workerMachine,
    ).toMatch(/Rancher/u);
    expect(
      fieldProblems(
        "Machines",
        { ...rancher, workerReplicas: 0, controlPlaneMachine: four, workerMachine: { ...four, cores: 2 } },
        rke2,
      ),
    ).toStrictEqual({});
    expect(fieldProblems("Machines", { ...form, controlPlaneMachine: { ...four, cores: 2 } }, rke2)).toStrictEqual({});
  });

  it("says why the sign-in page is back, what a lost connection means, and who may create", async () => {
    window.history.replaceState(null, "", "/");
    const routes: Record<string, Route> = {
      "GET /api/session": { body: { user: "viewer@pve", csrf: "c", canCreate: false } },
      "GET /api/realms": { body: [] },
      "GET /api/clusters": { body: [] },
    };
    fakeServer(routes);
    const { unmount } = render(<App />);
    const create = await screen.findByRole("button", { name: "Create cluster" });
    expect(create).toBeDisabled();
    expect(create).toHaveAccessibleDescription(/VM.Allocate/u);
    unmount();

    routes["GET /api/clusters"] = () => {
      throw new TypeError("Failed to fetch");
    };
    const { unmount: unmountOffline } = render(<ClusterList />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(noAnswer);
    unmountOffline();

    routes["GET /api/clusters"] = { status: 401, body: { error: "not signed in" } };
    render(<App />);
    await expect(screen.findByText(/Your session has ended/u)).resolves.toBeInTheDocument();
  });

  it("shows a field's problem under it once touched or after Next, and focuses the first", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/classes": () => ({ body: [rke2] }),
      "GET /api/proxmox/storages": () => ({ body: [] }),
      "GET /api/proxmox/nodes": () => ({ body: [] }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const name = await screen.findByLabelText("Name");
    expect(name).not.toHaveAttribute("aria-invalid");
    expect(screen.queryByText(/lowercase letters/u)).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() => expect(name).toHaveFocus());
    expect(name).toHaveAttribute("aria-invalid", "true");
    expect(name).toHaveAccessibleDescription(/^Lowercase letters.*Also names its Proxmox VE pool/u);
    expect(screen.getByLabelText("Distribution")).toHaveAttribute("aria-invalid", "true");

    await user.type(name, "lab");
    expect(name).not.toHaveAttribute("aria-invalid");
  });

  it("walks through the wizard and creates the cluster", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const calls = fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/classes": () => ({ body: [rke2] }),
      "GET /api/proxmox/storages": () => ({
        body: [
          {
            storage: "local-lvm",
            node: "pve1",
            shared: false,
            content: ["images"],
            avail: 1 << 30,
            total: 2 << 30,
            usable: true,
          },
          { storage: "tank", node: "pve1", shared: false, content: ["images"], avail: 0, total: 0, usable: false },
        ],
      }),
      "GET /api/proxmox/nodes": () => ({
        body: [
          { node: "pve1", online: true },
          { node: "pve2", online: true },
        ],
      }),
      "GET /api/proxmox/nodes/pve1/bridges": () => ({
        body: [{ bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true }],
      }),
      "GET /api/proxmox/ha-rules": () => ({
        body: {
          readable: true,
          rules: [{ rule: "fast", comment: "SSD", nodes: ["pve2"], strict: true, allowed: ["pve2"] }],
        },
      }),
      "POST /api/clusters": () => ({ status: 201, body: { ...web, name: "lab" } }),
      "GET /api/clusters/default/lab": () => ({ body: { ...web, name: "lab" } }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.type(await screen.findByLabelText("Name"), "lab");
    await waitFor(() => expect(screen.getByRole("option", { name: "RKE2" })).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-rke2");
    await user.type(screen.getByLabelText("Kubernetes version"), "v1.36.5+rke2r1");
    await user.click(screen.getByRole("button", { name: "Next" }));

    // The one bridge, like the one usable storage after, is chosen already.
    await waitFor(() => expect(screen.getByLabelText("Bridge")).toHaveValue("vmbr0"));
    await user.type(screen.getByLabelText("API address"), "10.0.0.20");
    await user.type(screen.getByLabelText("Node addresses"), "10.0.0.21-10.0.0.29");
    await user.type(screen.getByLabelText("Gateway"), "10.0.0.1");
    await user.type(screen.getByLabelText("DNS servers"), "10.0.0.1");
    expect(screen.getByLabelText("Pod network")).toHaveValue("canal");
    await user.click(screen.getByRole("button", { name: "Next" }));

    expect(screen.queryByRole("option", { name: /tank/u })).not.toBeInTheDocument();
    expect(screen.getByLabelText("Storage")).toHaveValue("local-lvm");
    await user.click(screen.getByRole("radio", { name: "Nodes I choose" }));
    expect(within(screen.getByRole("group", { name: "Nodes" })).getAllByRole("checkbox")).toHaveLength(2);
    await user.click(screen.getByRole("radio", { name: "The nodes of an HA rule" }));
    await user.selectOptions(screen.getByLabelText("HA rule"), "fast");
    expect(screen.getByRole("option", { name: "fast (SSD): pve2, strict" })).toBeInTheDocument();
    // An emptied number stays empty and is refused, instead of turning into 0.
    const cores = workerCores();
    await user.clear(cores);
    expect(cores).toHaveValue(null);
    await user.type(cores, "4");
    expect(cores).toHaveValue(4);
    await user.click(screen.getByRole("button", { name: "Next" }));
    // Rancher raises the machines to the cores it needs, and they stay raised once it is unticked.
    await user.click(screen.getByRole("checkbox", { name: /Install Rancher/u }));
    expect(screen.getByText(/Rancher needs at least 4 cores on every node/u)).toBeInTheDocument();
    await user.click(screen.getByRole("checkbox", { name: /Install Rancher/u }));
    await user.click(screen.getByRole("button", { name: "Machines" }));
    expect(within(screen.getByRole("group", { name: "Control plane" })).getByLabelText("Cores")).toHaveValue(4);
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    // The class's default providers come pre-filled; adding one more is checked before going on.
    expect(screen.getByLabelText("Issuer URL")).toHaveValue("https://sso.example");
    await user.click(screen.getByRole("button", { name: "Add identity provider" }));
    const added = within(screen.getByRole("group", { name: "Identity provider 2" }));
    expect(added.getByLabelText("Issuer URL")).toHaveFocus();
    expect(added.getByLabelText("Issuer URL")).not.toHaveAttribute("aria-invalid");
    await user.type(added.getByLabelText("Issuer URL"), "http://mine.example");
    // A field says what is wrong with it once it was edited; the others wait for Next.
    expect(added.getByLabelText("Issuer URL")).toHaveAccessibleDescription(/^The issuer is an https:\/\/ URL/u);
    expect(added.getByLabelText("Client ID")).not.toHaveAttribute("aria-invalid");
    await user.click(screen.getByRole("button", { name: "Next" }));
    // Next stays on the step, marks every problem and puts the cursor on the first.
    await waitFor(() => expect(added.getByLabelText("Issuer URL")).toHaveFocus());
    expect(added.getByLabelText("Client ID")).toHaveAccessibleDescription(/^A client ID\./u);
    expect(screen.getByRole("button", { name: "Add identity provider" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove identity provider 2" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() => expect(window.location.pathname).toBe("/clusters/default/lab"));
    expect(calls.map((c) => c.key)).toContain("GET /api/clusters/default/lab");
    const created = calls.find((c) => c.key === "POST /api/clusters");
    expect(created?.headers.get("X-Kubehenge-CSRF")).toBe("c1");
    expect(created?.body).toMatchObject({
      name: "lab",
      class: "kubehenge-rke2",
      variables: {
        storage: "local-lvm",
        allowedNodes: ["pve2"],
        cni: "canal",
        workerMachine: { cores: 4 },
        controlPlaneMachine: { cores: 4 },
        controlPlaneEndpoint: "10.0.0.20",
        oidc: [{ issuerURL: "https://sso.example", clientID: "kubernetes", usernamePrefix: "oidc:" }],
      },
    });
  });

  it("keeps choices across a change of distribution, holds back what needs Sys.Modify, and asks before discarding", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const k3s: ClusterClass = {
      ...rke2,
      name: "kubehenge-k3s",
      distribution: "k3s",
      versionFormat: { pattern: "^v1\\.\\d+\\.\\d+\\+k3s\\d+$" },
    };
    fakeServer({
      "GET /api/session": () => ({ body: { user: "dev@pve", csrf: "c1", canCreate: true, canTrust: false } }),
      "GET /api/classes": () => ({ body: [rke2, k3s] }),
      "GET /api/proxmox/storages": () => ({
        body: [
          {
            storage: "local-lvm",
            node: "pve1",
            shared: false,
            content: ["images"],
            avail: 1 << 30,
            total: 2 << 30,
            usable: true,
          },
        ],
      }),
      "GET /api/proxmox/nodes": () => ({ body: [] }),
      "GET /api/clusters": () => ({ body: [] }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.type(await screen.findByLabelText("Name"), "lab");
    await waitFor(() => expect(screen.getByRole("option", { name: "RKE2" })).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-rke2");
    await user.type(screen.getByLabelText("Kubernetes version"), "v1.36.5+rke2r1");
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.type(screen.getByLabelText("Bridge"), "vmbr0");
    await user.type(screen.getByLabelText("API address"), "10.0.0.20");
    await user.type(screen.getByLabelText("Node addresses"), "10.0.0.21-10.0.0.29");
    await user.type(screen.getByLabelText("Gateway"), "10.0.0.1");
    await user.type(screen.getByLabelText("DNS servers"), "10.0.0.1");
    await user.selectOptions(screen.getByLabelText("Pod network"), "cilium");
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.clear(workerCores());
    await user.type(workerCores(), "6");

    await user.click(screen.getByRole("button", { name: "General" }));
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-k3s");
    await user.type(screen.getByLabelText("Kubernetes version"), "v1.36.5+k3s1");
    await user.click(screen.getByRole("button", { name: "Network" }));
    expect(screen.getByLabelText("Pod network")).toHaveValue("cilium");
    await user.click(screen.getByRole("button", { name: "Machines" }));
    expect(workerCores()).toHaveValue(6);

    await user.selectOptions(screen.getByLabelText("Storage"), "local-lvm");
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("checkbox", { name: /Install Rancher/u })).toBeDisabled();
    expect(screen.getByRole("checkbox", { name: /Install Rancher/u })).toHaveAccessibleDescription(
      /needs Sys\.Modify/u,
    );
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("button", { name: "Add identity provider" })).toBeDisabled();
    expect(screen.getByLabelText("Issuer URL")).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    const dialog = screen.getByRole("dialog", { name: "Discard this cluster?" });
    await user.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    expect(window.location.pathname).toBe("/clusters/new");
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Discard" }));
    expect(window.location.pathname).toBe("/");
  });
});
