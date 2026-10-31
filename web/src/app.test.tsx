import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { noAnswer } from "./api";
import { App } from "./App";
import { ClusterList } from "./ClusterList";
import {
  addonCPU,
  addonCostMiB,
  addonMemory,
  addonNeedMiB,
  fieldProblems,
  newForm,
  problems,
  request,
  summary,
  withRoom,
  type Form,
} from "./create-form";
import { applyStaged, fakeServer, held, reads, type Route } from "./fake-server";
import { addressCount } from "./network";
import type { Cluster, ClusterClass, ClusterHealth, Machine, NodeOSChoice } from "./types";

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  annotations: {
    "kubehenge.dev/addon-memory":
      '{"headlamp":64,"monitoring":1536,"kyverno":384,"certManager":192,"rancher":2048,"rancherAgent":512}',
    "kubehenge.dev/node-memory": '{"controlPlane":2560,"worker":704}',
    "kubehenge.dev/addon-cpu":
      '{"headlamp":10,"monitoring":20,"kyverno":400,"certManager":0,"rancher":10,"rancherAgent":0}',
    "kubehenge.dev/node-cpu": '{"controlPlane":1555,"worker":745}',
  },
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: [
    { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
    { name: "network", required: true, schema: { type: "object" } },
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "allowedNodes", required: false, schema: { type: "array" } },
    {
      name: "controlPlaneMachine",
      required: false,
      schema: {
        type: "object",
        default: { cores: 2, memoryMiB: 4096, diskGiB: 30 },
        properties: {
          cores: { type: "integer", minimum: 2 },
          memoryMiB: { type: "integer", minimum: 4096 },
          diskGiB: { type: "integer", minimum: 20 },
        },
      },
    },
    {
      name: "workerMachine",
      required: false,
      schema: {
        type: "object",
        default: { cores: 2, memoryMiB: 4096, diskGiB: 40 },
        properties: {
          cores: { type: "integer", minimum: 2 },
          memoryMiB: { type: "integer", minimum: 4096 },
          diskGiB: { type: "integer", minimum: 20 },
        },
      },
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

const alma: NodeOSChoice = {
  family: "almalinux",
  release: "10",
  title: "AlmaLinux 10",
  cpuType: "x86-64-v2-AES",
  mac: "selinux",
  default: true,
};

const rocky: NodeOSChoice = {
  family: "rocky",
  release: "10",
  title: "Rocky Linux 10",
  cpuType: "x86-64-v3",
  mac: "selinux",
};

// rke2OS is rke2 as the server lists it: with the node OSes its clusters may run.
const rke2OS: ClusterClass = {
  ...rke2,
  variables: [...rke2.variables, { name: "nodeOS", required: false, schema: { type: "object" } }],
  nodeOSes: [alma, rocky],
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
  generation: 1,
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

const quietHealth: ClusterHealth = {
  read: new Date().toISOString(),
  verdict: "healthy",
  attention: [],
  nodes: { items: [] },
  pods: { items: [] },
  volumes: { items: [] },
  addons: { items: [] },
  pools: { items: [] },
  controlPlane: { conditions: [] },
  healthChecks: { items: [] },
  events: { items: [] },
  logs: false,
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
    render(<App />);
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(
      "The sign-in through the identity provider failed.",
    );
    // One click away beside the password form, and not among the password realms.
    const sso = await screen.findByRole("link", { name: "Continue with Company SSO" });
    expect(sso).toHaveAttribute("href", "/api/openid/start?realm=sso");
    expect(screen.getByLabelText("Realm")).toHaveValue("pve");
    expect(screen.queryByRole("option", { name: /sso/u })).not.toBeInTheDocument();
    expect(screen.getByLabelText("User name")).toHaveFocus();
  });

  it("offers the identity provider last signed in with first", async () => {
    localStorage.setItem("kubehenge-realm", "sso");
    fakeServer({
      "GET /api/session": () => ({ status: 401 }),
      "GET /api/realms": () => ({
        body: [
          { realm: "pve", type: "pve", default: true },
          { realm: "sso", type: "openid", comment: "Company SSO" },
        ],
      }),
    });
    render(<App />);
    const sso = await screen.findByRole("link", { name: "Continue with Company SSO" });
    await waitFor(() => expect(sso).toHaveFocus());
    expect(sso).toHaveClass("primary");
    expect(screen.getByRole("button", { name: "Sign in" })).not.toHaveClass("primary");
    localStorage.removeItem("kubehenge-realm");
  });

  it("asks for no password where every realm is an identity provider", async () => {
    fakeServer({
      "GET /api/session": () => ({ status: 401 }),
      "GET /api/realms": () => ({ body: [{ realm: "sso", type: "openid" }] }),
    });
    render(<App />);
    await expect(screen.findByRole("link", { name: "Continue with sso" })).resolves.toBeInTheDocument();
    expect(screen.queryByLabelText("Password")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in" })).not.toBeInTheDocument();
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
    expect(screen.queryByRole("radio", { name: "YubiKey" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: "Recovery key" }));
    const key = screen.getByRole("textbox", { name: "Recovery key" });
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
    const dialog = screen.getByRole("dialog", { name: "Delete cluster web" });
    const confirm = within(dialog).getByRole("button", { name: "Delete cluster" });
    expect(confirm).toBeDisabled();
    expect(within(dialog).getByLabelText("Confirm name")).toHaveAccessibleDescription("Type web.");
    await user.type(within(dialog).getByLabelText("Confirm name"), "web");
    await user.click(confirm);
    await waitFor(() => expect(window.location.pathname).toBe("/"));
    const deletion = calls.find((c) => c.key === "DELETE /api/clusters/default/web");
    expect(deletion?.headers.get("X-Kubehenge-CSRF")).toBe("c1");
    expect(deletion?.body).toStrictEqual({ confirm: "web" });
  });

  it("says what runs the workloads of a cluster without workers", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const alone: Cluster = { ...web, workers: { desired: 0, ready: 0, available: 0, upToDate: 0 } };
    fakeServer({ ...signedIn, "GET /api/clusters/default/web": () => ({ body: alone }) });
    render(<App />);
    const term = await screen.findByText("Workers", { selector: "dt" });
    expect(term.nextElementSibling).toHaveTextContent(/^None$/u);
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
    await applyStaged(user);
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PATCH /api/clusters/default/web")?.body).toStrictEqual({
        workerPools: { "md-0": 4 },
      }),
    );
  });

  it("offers the newest version it may move to, and says what stands in the way before upgrading", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const old: Cluster = {
      ...web,
      version: "v1.32.4+rke2r1",
      controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
      workers: { desired: 2, ready: 2, available: 2, upToDate: 2 },
      variables: { ...web.variables, network: { gateway: "10.0.0.1", prefix: 24, addresses: ["10.0.0.21-10.0.0.23"] } },
    };
    fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": () => ({ body: old }),
      "GET /api/clusters/default/web/health": () => ({
        body: {
          verdict: "degraded",
          attention: [
            { severity: "error", section: "nodes", what: "Node web-md-0-a not ready" },
            { severity: "warning", section: "pods", what: "Pod crash-looping" },
          ],
        },
      }),
      "POST /api/clusters/default/web/plan": () => ({
        status: 422,
        body: { error: "version v1.33.13+rke2r3 has no VM template" },
      }),
      "GET /api/versions/rke2": () => ({
        body: { versions: ["v1.36.5+rke2r1"], older: ["v1.33.12+rke2r1", "v1.33.13+rke2r3", "v1.32.13+rke2r2"] },
      }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Upgrade" }));
    const dialog = within(screen.getByRole("dialog"));
    await waitFor(() => expect(dialog.getByLabelText("Kubernetes version")).toHaveValue("v1.33.13+rke2r3"));
    await waitFor(() => expect(dialog.getByText("version v1.33.13+rke2r3 has no VM template")).toBeInTheDocument());
    expect(dialog.getByText("None spare. Add one under Options.")).toBeInTheDocument();
    await waitFor(() => expect(dialog.getByText("2 problems")).toBeInTheDocument());
    expect(dialog.getByRole("link", { name: "Health" })).toHaveAttribute("href", "/clusters/default/web/health");
    expect(dialog.getByRole("button", { name: "Upgrade" })).toBeDisabled();
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
    await applyStaged(user);
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
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("No backup job covers this appliance");
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
    const main = await screen.findByRole("main");
    await expect(within(main).findByText("Waiting for ClusterClass")).resolves.toBeInTheDocument();
    const rail = screen.getByRole("navigation", { name: "Fleet" });
    expect(within(within(rail).getByRole("region", { name: "Running" })).getByRole("link")).toHaveTextContent(
      "webWaiting for ClusterClass",
    );
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
      `https://${location.hostname}:8444/kubehenge/open/default.web`,
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
      "GET /api/clusters/default/web/health": { body: quietHealth },
    };
    fakeServer(routes);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ delay: null, advanceTimers: vi.advanceTimersByTime });
    render(<App />);
    await expect(screen.findByText("No machines yet")).resolves.toBeInTheDocument();
    routes["GET /api/clusters/default/web"] = { status: 404, body: { error: "no such cluster" } };
    // The cluster's own conditions close its health page.
    await user.click(screen.getByRole("link", { name: "Health" }));
    await expect(
      screen.findByRole("cell", { name: /^Needs attention:\s*ControlPlaneAvailable$/u }),
    ).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: "Summary" }));
    // Nothing moves in it, so its page asks again only every 30 s.
    await act(() => vi.advanceTimersByTimeAsync(5000));
    expect(screen.queryByText(/Cluster not found/u)).not.toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(25_000));
    await expect(screen.findByText(/Cluster not found/u)).resolves.toBeInTheDocument();
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
      screen.findByText(/^pve1 runs 2 of 3 control-plane VMs, so losing it stops the control plane\./u),
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

  it("replaces a machine after asking, and keeps one marked for replacement", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/machines");
    const routes: Record<string, Route> = {
      ...signedIn,
      "GET /api/clusters/default/web": {
        body: { ...web, machines: [runningOn("cp-a", "pve1"), runningOn("md-a", "pve1", "worker")] },
      },
      "PUT /api/clusters/default/web/machines/md-a/replacement": { status: 204 },
      "DELETE /api/clusters/default/web/machines/cp-a/replacement": { status: 204 },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Replace md-a" }));
    const dialog = screen.getByRole("dialog", { name: "Replace md-a" });
    expect(dialog).toHaveTextContent("Drains the node, deletes the VM and its disks, then adds a new worker.");
    routes["GET /api/clusters/default/web"] = {
      body: {
        ...web,
        machines: [{ ...runningOn("cp-a", "pve1"), replacing: true }, runningOn("md-a", "pve1", "worker")],
      },
    };
    await user.click(within(dialog).getByRole("button", { name: "Replace" }));
    await waitFor(() => expect(reads(calls, "PUT /api/clusters/default/web/machines/md-a/replacement")).toBe(1));

    const marked = await screen.findByRole("row", { name: /^cp-a .*Replacing/u });
    await user.click(within(marked).getByRole("button", { name: "Keep cp-a" }));
    await waitFor(() => expect(reads(calls, "DELETE /api/clusters/default/web/machines/cp-a/replacement")).toBe(1));
  });

  it("deletes a worker after asking, and opens a machine's console", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/machines");
    const console = "https://pve.example.org:8006/?console=kvm&novnc=1&vmid=201&node=pve1";
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": {
        body: {
          ...web,
          machines: [
            { ...runningOn("cp-a", "pve1"), console },
            { ...runningOn("md-a", "pve1", "worker"), pool: "md-0" },
          ],
        },
      },
      "DELETE /api/clusters/default/web/machines/md-a": { status: 204 },
    });
    const user = userEvent.setup();
    render(<App />);
    const link = await screen.findByRole("link", { name: "Console of cp-a" });
    expect(link).toHaveAttribute("href", console);
    expect(link).toHaveAttribute("target", "_blank");
    expect(screen.queryByRole("link", { name: "Console of md-a" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete cp-a" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Delete md-a" }));
    const dialog = screen.getByRole("dialog", { name: "Delete md-a" });
    expect(dialog).toHaveTextContent("Pool md-0 shrinks by one.");
    await user.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(reads(calls, "DELETE /api/clusters/default/web/machines/md-a")).toBe(1));
  });

  it("cordons and uncordons a node as the health read shows it, and reboots after asking", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/machines");
    const calls = fakeServer({
      ...signedIn,
      "GET /api/clusters/default/web": {
        body: {
          ...web,
          machines: [
            { ...runningOn("cp-a", "pve1"), node: "cp-a", reboot: true },
            { ...runningOn("md-a", "pve1", "worker"), node: "md-a", pool: "md-0" },
          ],
        },
      },
      "GET /api/clusters/default/web/health": {
        body: { verdict: "healthy", nodes: { items: [{ name: "md-a", machine: "md-a", unschedulable: true }] } },
      },
      "PUT /api/clusters/default/web/machines/cp-a/cordon": { status: 204 },
      "DELETE /api/clusters/default/web/machines/md-a/cordon": { status: 204 },
      "POST /api/clusters/default/web/machines/cp-a/reboot": { status: 202 },
    });
    const user = userEvent.setup();
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Uncordon md-a" }));
    await waitFor(() => expect(reads(calls, "DELETE /api/clusters/default/web/machines/md-a/cordon")).toBe(1));
    expect(screen.getByText("Cordoned")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Cordon cp-a" }));
    await waitFor(() => expect(reads(calls, "PUT /api/clusters/default/web/machines/cp-a/cordon")).toBe(1));
    expect(screen.queryByRole("button", { name: "Reboot md-a" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Reboot cp-a" }));
    const dialog = screen.getByRole("dialog", { name: "Reboot cp-a" });
    await user.click(within(dialog).getByRole("button", { name: "Reboot" }));
    await waitFor(() => expect(reads(calls, "POST /api/clusters/default/web/machines/cp-a/reboot")).toBe(1));
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

// The free addresses on vmbr0's network, where a host answers on 10.0.0.33.
const freeOn: Route = (body) => {
  const sent = JSON.stringify(body);
  if (sent.includes('"nodes":0')) {
    return { body: { network: "10.0.0.0/16", answering: sent.includes("10.0.0.33") ? ["10.0.0.33"] : [] } };
  }
  return { body: { network: "10.0.0.0/16", endpoint: "10.0.0.30", addresses: "10.0.0.31-10.0.0.33", answering: [] } };
};

describe("create", () => {
  const form: Form = {
    from: "",
    namespace: "",
    name: "web",
    className: "kubehenge-rke2",
    nodeOS: "",
    version: "v1.36.5+rke2r1",
    talosVersion: "",
    controlPlaneReplicas: 3,
    workerReplicas: 2,
    workerAutoscale: false,
    workerMax: 3,
    endpoint: "10.0.0.20",
    bridge: "vmbr0",
    vlan: "",
    addresses: "10.0.0.21-10.0.0.29\n10.0.0.32/28",
    prefix: 24,
    gateway: "10.0.0.1",
    dnsServers: "10.0.0.1, 1.1.1.1",
    loadBalancer: "",
    loadBalancerAddresses: "",
    pods: "",
    services: "",
    storage: "local-lvm",
    place: { placement: "any", haRule: "", nodes: [] },
    proxmoxHA: true,
    datastoreAllocate: false,
    volumeSnapshots: false,
    haGroups: true,
    controlPlaneMachine: { cores: 2, memoryMiB: 4096, diskGiB: 30 },
    workerMachine: { cores: 4, memoryMiB: 8192, diskGiB: 40 },
    cni: "cilium",
    headlamp: true,
    headlampHostname: "",
    headlampView: false,
    monitoring: true,
    grafanaHostname: "grafana.example",
    logs: { enabled: false, collector: "node", retentionDays: 7, sizeGiB: 10 },
    podSecurity: { admission: "restricted", kyverno: "off" },
    cis: false,
    istio: false,
    clusterMesh: false,
    meshPeers: [],
    rancherInstall: false,
    rancherHostname: "",
    rancherRegister: false,
    certificates: newForm().certificates,
    dns01Token: "",
    gitops: newForm().gitops,
    oidc: [],
    upgradeMode: "off",
    backups: false,
    backupFields: {
      target: "",
      schedule: "0 2 * * *",
      timeZone: "UTC",
      retention: "7",
      volumes: "all",
      readFrom: [],
    },
    storeBackupKey: true,
    backupKey: {},
  };

  it("keeps the VMs to the nodes of the placement chosen", () => {
    expect(request(form, rke2).variables).not.toHaveProperty("allowedNodes");
    const byRule = { ...form, place: { placement: "rule" as const, haRule: "fast", nodes: ["pve2", "pve3"] } };
    const chosen = { ...form, place: { placement: "nodes" as const, haRule: "", nodes: ["pve1"] } };
    const noRule = { ...form, place: { placement: "rule" as const, haRule: "", nodes: [] } };
    const noNodes = { ...form, place: { placement: "nodes" as const, haRule: "", nodes: [] } };
    expect(request(byRule, rke2).variables["allowedNodes"]).toStrictEqual(["pve2", "pve3"]);
    expect(Object.fromEntries(summary(byRule, rke2))["Placement"]).toBe("HA rule fast: pve2, pve3");
    expect(Object.fromEntries(summary(chosen, rke2))["Placement"]).toBe("pve1");
    expect(Object.fromEntries(summary(form, rke2))["Placement"]).toBe("Any node");
    expect(problems("Machines", noRule, rke2)).toStrictEqual(["Choose an HA rule"]);
    expect(problems("Machines", noNodes, rke2)).toStrictEqual(["Choose at least one node"]);
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
    const defaults = { ...form, podSecurity: { admission: "baseline" as const, kyverno: "off" as const } };
    expect(request(defaults, rke2).variables).not.toHaveProperty("podSecurity");
    // Privileged is below the classes' baseline, so it has to be asked for.
    expect(
      request({ ...defaults, podSecurity: { admission: "privileged", kyverno: "off" } }, rke2).variables,
    ).toMatchObject({ podSecurity: { admission: "privileged" } });
    expect(request(defaults, rke2).variables).not.toHaveProperty("cis");
    // CIS hardening has admission enforce restricted, and the variable says so.
    expect(request({ ...defaults, cis: true }, rke2).variables).toMatchObject({
      cis: true,
      podSecurity: { admission: "restricted" },
    });
    const unhardened = { ...rke2, variables: rke2.variables.filter((v) => v.name !== "cis") };
    expect(request({ ...defaults, cis: true }, unhardened).variables).not.toHaveProperty("podSecurity");
  });

  it("asks for Headlamp's view access only when chosen", () => {
    const open = { ...form, headlampHostname: "headlamp.example", headlampView: true };
    expect(request(open, rke2).variables["headlamp"]).toStrictEqual({
      enabled: true,
      hostname: "headlamp.example",
      access: "view",
    });
    expect(Object.fromEntries(summary(open, rke2))["Headlamp"]).toBe("On, at headlamp.example, read-only for everyone");
    expect(request({ ...form, headlamp: false, headlampView: true }, rke2).variables).not.toHaveProperty("headlamp");
  });

  it("sums the form up as the Confirm step shows it", () => {
    const rows = Object.fromEntries(summary(form, rke2));
    expect(rows).toMatchObject({
      Name: "web",
      Distribution: "RKE2",
      "Node addresses": "10.0.0.21-10.0.0.29, 10.0.0.32/28",
      "Pod range": "Automatic",
      "Worker machines": "4 cores, 8 GiB memory, 40 GiB disk",
      Headlamp: "On",
      Monitoring: "On, Grafana at grafana.example",
      "CIS hardening": "Off",
      "Pod security": "Restricted",
      Rancher: "Not registered",
    });
    expect(
      Object.fromEntries(summary({ ...form, podSecurity: { admission: "baseline", kyverno: "restricted" } }, rke2)),
    ).toMatchObject({ "Pod security": "Baseline, Kyverno restricted" });
    const alone = Object.fromEntries(summary({ ...form, workerReplicas: 0, cni: "canal" }, rke2));
    expect(alone).not.toHaveProperty("Worker machines");
    expect(alone["Pod range"]).toBe("Automatic");
    expect(alone["Pod network"]).toBe("Canal");
    expect(
      Object.fromEntries(
        summary({ ...form, cis: true, podSecurity: { admission: "baseline", kyverno: "restricted" } }, rke2),
      ),
    ).toMatchObject({
      "CIS hardening": "On",
      "Pod security": "Restricted, Kyverno restricted",
    });
    expect(
      Object.fromEntries(summary({ ...form, vlan: "30", rancherInstall: true, rancherHostname: "r.example" }, rke2)),
    ).toMatchObject({
      Bridge: "vmbr0, VLAN 30",
      Rancher: "Installed at r.example",
    });
    const withIstio = {
      ...rke2,
      variables: [...rke2.variables, { name: "istio", required: false, schema: { type: "object" } }],
    };
    expect(Object.fromEntries(summary(form, withIstio))).toMatchObject({ Istio: "Off" });
    expect(Object.fromEntries(summary({ ...form, istio: true }, withIstio))).toMatchObject({
      Istio: "On, ambient mode",
    });
    expect(request({ ...form, istio: true }, withIstio).variables["istio"]).toStrictEqual({ enabled: true });
    expect(request({ ...form, istio: true }, rke2).variables).not.toHaveProperty("istio");
  });

  it("joins a Cluster Mesh only where the cluster runs Cilium", () => {
    const withMesh: ClusterClass = {
      ...rke2,
      annotations: { "kubehenge.dev/addon-memory": '{"clusterMesh":256}' },
      variables: [...rke2.variables, { name: "clusterMesh", required: false, schema: { type: "object" } }],
    };
    const meshed = { ...form, clusterMesh: true, meshPeers: ["db", "api"] };
    expect(Object.fromEntries(summary(form, withMesh))).toMatchObject({ "Cluster Mesh": "Off" });
    expect(Object.fromEntries(summary(meshed, withMesh))).toMatchObject({ "Cluster Mesh": "On, with api, db" });
    expect(Object.fromEntries(summary({ ...meshed, meshPeers: [] }, withMesh))).toMatchObject({
      "Cluster Mesh": "On, no peers",
    });
    expect(request(meshed, withMesh).variables["clusterMesh"]).toStrictEqual({ enabled: true, peers: ["api", "db"] });
    expect(addonMemory(meshed, withMesh)).toStrictEqual({ names: ["Cluster Mesh"], mib: 256, controlPlaneMiB: 0 });

    const canal = { ...meshed, cni: "canal" };
    expect(Object.fromEntries(summary(canal, withMesh))).not.toHaveProperty("Cluster Mesh");
    expect(request(canal, withMesh).variables).not.toHaveProperty("clusterMesh");
    expect(addonMemory(canal, withMesh).mib).toBe(0);
    expect(request(meshed, rke2).variables).not.toHaveProperty("clusterMesh");

    const long = "a".repeat(33);
    expect(fieldProblems("Network", { ...form, name: long }, withMesh)).toStrictEqual({
      cni: "Cilium needs a cluster name of 32 characters or fewer",
    });
    expect(fieldProblems("Network", { ...form, name: long, cni: "canal" }, withMesh)).toStrictEqual({});
  });

  it("keeps logs only beside monitoring", () => {
    const withLogs: ClusterClass = {
      ...rke2,
      annotations: { "kubehenge.dev/addon-memory": '{"monitoring":1536,"logs":512}' },
      variables: [...rke2.variables, { name: "logs", required: false, schema: { type: "object" } }],
    };
    const logged = { ...form, logs: { enabled: true, collector: "api" as const, retentionDays: 14, sizeGiB: 20 } };
    expect(request(logged, withLogs).variables["logs"]).toStrictEqual({
      enabled: true,
      collector: "api",
      retentionDays: 14,
      sizeGiB: 20,
    });
    expect(Object.fromEntries(summary(logged, withLogs))).toMatchObject({
      Logs: "On, Kubernetes API, 14 days, 20 GiB",
    });
    expect(addonMemory(logged, withLogs)).toStrictEqual({
      names: ["monitoring", "logs"],
      mib: 2048,
      controlPlaneMiB: 0,
    });

    const unmonitored = { ...logged, monitoring: false };
    expect(request(unmonitored, withLogs).variables).not.toHaveProperty("logs");
    expect(Object.fromEntries(summary(unmonitored, withLogs))).toMatchObject({ Logs: "Off" });
    expect(addonMemory(unmonitored, withLogs).mib).toBe(0);
    // A card's cost is the add-on's own, whether it is chosen or not.
    expect(addonCostMiB(withLogs, ["logs"])).toBe(512);
    expect(addonCostMiB(withLogs, ["monitoring", "logs"])).toBe(2048);
    expect(addonCostMiB(rke2, ["logs"])).toBe(0);
    expect(request(logged, rke2).variables).not.toHaveProperty("logs");

    expect(fieldProblems("Add-ons", { ...logged, logs: { ...logged.logs, sizeGiB: 0 } }, withLogs)).toStrictEqual({
      logs: "A whole number from 1 to 1024",
    });
    expect(fieldProblems("Add-ons", { ...unmonitored, logs: { ...logged.logs, sizeGiB: 0 } }, withLogs)).toStrictEqual(
      {},
    );
  });

  it("gives Services addresses of their own from the load balancer chosen", () => {
    const withLB: ClusterClass = {
      ...rke2,
      annotations: { "kubehenge.dev/addon-memory": '{"loadBalancer":96}' },
      variables: [...rke2.variables, { name: "loadBalancer", required: false, schema: { type: "object" } }],
    };
    expect(Object.fromEntries(summary(form, withLB))).toMatchObject({ "Load balancer": "None" });
    expect(Object.fromEntries(summary(form, { ...withLB, distribution: "k3s" }))).toMatchObject({
      "Load balancer": "k3s's own",
    });
    expect(request(form, withLB).variables).not.toHaveProperty("loadBalancer");
    expect(addonMemory(form, withLB).mib).toBe(0);

    const metallb = {
      ...form,
      loadBalancer: "metallb" as const,
      loadBalancerAddresses: "10.0.0.100-10.0.0.120\n10.0.0.130",
    };
    expect(Object.fromEntries(summary(metallb, withLB))).toMatchObject({
      "Load balancer": "MetalLB, on 10.0.0.100-10.0.0.120, 10.0.0.130",
    });
    expect(request(metallb, withLB).variables["loadBalancer"]).toStrictEqual({
      enabled: true,
      implementation: "metallb",
      addresses: ["10.0.0.100-10.0.0.120", "10.0.0.130"],
    });
    expect(request({ ...metallb, loadBalancer: "kube-vip" }, withLB).variables["loadBalancer"]).toMatchObject({
      implementation: "kube-vip",
    });
    expect(request(metallb, rke2).variables).not.toHaveProperty("loadBalancer");
    expect(addonMemory(metallb, withLB)).toStrictEqual({ names: ["the load balancer"], mib: 96, controlPlaneMiB: 0 });
    expect(fieldProblems("Network", metallb, withLB)).toStrictEqual({});

    for (const [addresses, problem] of [
      ["", "Enter at least one address, range or network"],
      ["10.0.1.100-10.0.1.120", "10.0.1.100-10.0.1.120 is not an address, range or network on the gateway's network"],
      ["10.0.0.25-10.0.0.30", "10.0.0.25-10.0.0.30 overlaps the node addresses"],
      ["10.0.0.40/28", "10.0.0.40/28 overlaps the node addresses"],
      ["10.0.0.20", "10.0.0.20 overlaps the API address"],
      ["10.0.0.1-10.0.0.3", "10.0.0.1-10.0.0.3 overlaps the gateway"],
      ["10.0.0.0/30", "10.0.0.0/30 includes the network or broadcast address"],
      ["10.0.0.240/28", "10.0.0.240/28 includes the network or broadcast address"],
      ["10.0.0.100-10.0.0.120, 10.0.0.110", "10.0.0.100-10.0.0.120 overlaps another entry"],
    ] as const) {
      expect(fieldProblems("Network", { ...metallb, loadBalancerAddresses: addresses }, withLB)).toStrictEqual({
        loadBalancerAddresses: problem,
      });
    }
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
    // The nodes still move to new template builds in that window.
    expect(request(form, withUpgrades).variables["upgrades"]).toStrictEqual({
      mode: "off",
      window: { start: "02:00", durationMinutes: 240, timeZone: zone },
    });
    expect(request(form, rke2).variables).not.toHaveProperty("upgrades");
    expect(Object.fromEntries(summary({ ...form, upgradeMode: "minor" }, withUpgrades))["Automatic upgrades"]).toBe(
      `Patch and minor releases and node images, daily 02:00 for 4 h ${zone}`,
    );
    // Talos nodes take Talos patch releases in the same window.
    const talos = { ...withUpgrades, distribution: "talos" };
    expect(request(form, talos).variables["upgrades"]).toStrictEqual({
      mode: "off",
      window: { start: "02:00", durationMinutes: 240, timeZone: zone },
    });
    expect(Object.fromEntries(summary(form, talos))["Automatic upgrades"]).toBe(
      `Talos patch releases, daily 02:00 for 4 h ${zone}`,
    );
  });

  it("autoscales the workers between the fewest and the most", () => {
    const withAutoscaler: ClusterClass = {
      ...rke2,
      variables: [...rke2.variables, { name: "autoscaler", required: false, schema: { type: "object" } }],
    };
    const scaled = { ...form, workerAutoscale: true, workerReplicas: 1, workerMax: 4 };
    // A class without the autoscaler takes the fixed number.
    expect(request(scaled, rke2)).not.toHaveProperty("workerAutoscaling");
    expect(request(scaled, withAutoscaler)).toMatchObject({ workerReplicas: 1, workerAutoscaling: { min: 1, max: 4 } });
    expect(Object.fromEntries(summary(scaled, withAutoscaler))["Workers"]).toBe("1 to 4, autoscaled");
    expect(problems("General", scaled, withAutoscaler)).toStrictEqual([]);
    expect(fieldProblems("General", { ...scaled, workerMax: 0 }, withAutoscaler)).toStrictEqual({
      workerMax: "A whole number from 1 to 100",
    });
    expect(fieldProblems("General", { ...scaled, workerReplicas: 5 }, withAutoscaler)).toStrictEqual({
      workerMax: "A whole number from 5 to 100",
    });
    // The addresses count every worker the pool may have: three control planes, four workers and one for rollouts.
    expect(fieldProblems("Network", { ...scaled, addresses: "10.0.0.21-10.0.0.27" }, withAutoscaler).addresses).toMatch(
      /^Enter at least 8 addresses/u,
    );
    // A pool that may grow keeps the control planes to the control plane's work, even from none.
    const fromNone = { ...scaled, workerReplicas: 0, headlamp: false, monitoring: false };
    expect(fieldProblems("Machines", fromNone, withAutoscaler)).toStrictEqual({});
    // Rancher's agent alone holds no worker up; the add-ons need a worker that stays.
    expect(fieldProblems("Add-ons", fromNone, withAutoscaler)).toStrictEqual({});
    expect(fieldProblems("Add-ons", { ...fromNone, headlamp: true }, withAutoscaler)).toStrictEqual({
      addons: "The add-ons need a worker. On General, set the fewest workers to 1 or more.",
    });
    expect(fieldProblems("Add-ons", { ...scaled, headlamp: true }, withAutoscaler)).toStrictEqual({});
  });

  it("keeps an emptied number empty, for the check to name", () => {
    expect(problems("General", { ...form, workerReplicas: Number.NaN }, rke2)).toStrictEqual([
      "A whole number from 0 to 100",
    ]);
    expect(
      problems("Machines", { ...form, workerMachine: { cores: Number.NaN, memoryMiB: 4096, diskGiB: 40 } }, rke2),
    ).toHaveLength(1);
  });

  it("runs the nodes on the node OS chosen, the class's default unless another is", () => {
    const missing =
      "there is no RKE2 VM template on Rocky Linux 10 yet; an administrator builds it on the Administration page";
    const onOS = { ...rke2OS, nodeOSes: [alma, { ...rocky, noTemplate: missing }] };
    expect(request(form, onOS).variables["nodeOS"]).toStrictEqual({ family: "almalinux", release: "10" });
    expect(Object.fromEntries(summary(form, onOS))["Node OS"]).toBe("AlmaLinux 10");
    expect(problems("General", form, onOS)).toStrictEqual([]);
    const onRocky = { ...form, nodeOS: "rocky-10" };
    expect(request(onRocky, onOS).variables["nodeOS"]).toStrictEqual({ family: "rocky", release: "10" });
    expect(Object.fromEntries(summary(onRocky, onOS))["Node OS"]).toBe("Rocky Linux 10");
    // The class's own message is for its default node OS; the one chosen has its own.
    expect(fieldProblems("General", onRocky, { ...onOS, noTemplate: "for the default" })).toStrictEqual({
      nodeOS:
        "There is no RKE2 VM template on Rocky Linux 10 yet; an administrator builds it on the Administration page.",
    });
    expect(fieldProblems("General", form, { ...onOS, noTemplate: "for the default" })).toStrictEqual({});
    // Talos offers none, and a class that does not define the variable is sent none.
    expect(request(form, rke2).variables).not.toHaveProperty("nodeOS");
    expect(Object.fromEntries(summary(form, rke2))).not.toHaveProperty("Node OS");
  });

  it("leaves out variables the class does not define", () => {
    const k3s = { ...rke2, variables: rke2.variables.filter((v) => v.name !== "headlamp") };
    expect(request(form, k3s).variables).not.toHaveProperty("headlamp");
  });

  it("finds what is missing on each step", () => {
    expect(problems("General", form, rke2)).toStrictEqual([]);
    expect(problems("General", { ...form, version: "v1.36.5" }, rke2)).toStrictEqual([
      "Enter a version like v1.36.5+rke2r1",
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
      "Enter a network like 10.244.0.0/16, without host bits",
    ]);
    expect(request({ ...form, pods: "172.30.0.0/16" }, rke2)).toMatchObject({ pods: ["172.30.0.0/16"] });
    expect(request(form, rke2)).not.toHaveProperty("pods");
    expect(
      problems("Machines", { ...form, workerMachine: { cores: 1, memoryMiB: 1000, diskGiB: 40 } }, rke2),
    ).toHaveLength(1);
    expect(
      problems("Machines", { ...form, workerMachine: { cores: 1.5, memoryMiB: 4096, diskGiB: 40 } }, rke2),
    ).toHaveLength(1);
    // The class's own minimums, which follow its distribution's sizing guide.
    expect(
      fieldProblems("Machines", { ...form, controlPlaneMachine: { cores: 1, memoryMiB: 4096, diskGiB: 30 } }, rke2),
    ).toStrictEqual({ controlPlaneMachine: "A whole number of cores, at least 2" });
    expect(
      fieldProblems("Machines", { ...form, workerMachine: { cores: 2, memoryMiB: 2048, diskGiB: 40 } }, rke2),
    ).toStrictEqual({ workerMachine: "At least 4096 MiB of memory, in steps of 8" });
    // Without workers the control planes run the workloads too, so they need a worker's least on top.
    const alone = { ...form, workerReplicas: 0 };
    expect(
      fieldProblems("Machines", { ...alone, controlPlaneMachine: { cores: 2, memoryMiB: 8192, diskGiB: 30 } }, rke2),
    ).toStrictEqual({
      controlPlaneMachine: "A whole number of cores, at least 4 without workers",
    });
    expect(
      fieldProblems("Machines", { ...alone, controlPlaneMachine: { cores: 4, memoryMiB: 4096, diskGiB: 30 } }, rke2),
    ).toStrictEqual({
      controlPlaneMachine: "At least 8192 MiB of memory without workers, in steps of 8",
    });
    expect(
      fieldProblems("Machines", { ...alone, controlPlaneMachine: { cores: 4, memoryMiB: 8192, diskGiB: 30 } }, rke2),
    ).toStrictEqual({});
    expect(problems("General", { ...form, workerReplicas: 1.5 }, rke2)).toStrictEqual(["A whole number from 0 to 100"]);
    // Three control-plane nodes, two workers and one for rollouts need six addresses.
    expect(fieldProblems("Network", { ...form, addresses: "10.0.0.21-10.0.0.25" }, rke2).addresses).toMatch(
      /^Enter at least 6 addresses/u,
    );
    expect(fieldProblems("Network", { ...form, addresses: "10.0.1.21-10.0.1.29" }, rke2).addresses).toMatch(
      /on the gateway's network$/u,
    );
    expect(fieldProblems("Network", { ...form, addresses: "10.0.0.29-10.0.0.21" }, rke2).addresses).toMatch(
      /is not an address/u,
    );
    expect(fieldProblems("Network", { ...form, endpoint: "10.0.0.22" }, rke2).endpoint).toMatch(
      /^Use an address outside the node addresses/u,
    );
    expect(fieldProblems("Network", { ...form, endpoint: "10.9.0.2" }, rke2).endpoint).toMatch(
      /^Use an address on the gateway's network/u,
    );
    expect(fieldProblems("Network", { ...form, pods: "10.244.1.0/16" }, rke2).pods).toMatch(/without host bits/u);
    expect(fieldProblems("Network", form, rke2)).toStrictEqual({});
    expect(addressCount(["10.0.0.32/28", "10.0.0.21-10.0.0.29", "10.0.0.50"], "10.0.0.1", 24)).toStrictEqual({
      count: 24,
    });
    expect(problems("Add-ons", { ...form, rancherInstall: true }, rke2)).toStrictEqual(["Enter a host name"]);
    const rancher = { ...form, rancherInstall: true, rancherHostname: "rancher.example.org" };
    const four = { cores: 4, memoryMiB: 8192, diskGiB: 40 };
    expect(
      fieldProblems("Machines", { ...rancher, controlPlaneMachine: { ...four, cores: 2 }, workerMachine: four }, rke2)
        .controlPlaneMachine,
    ).toMatch(/^Rancher needs at least 4 cores on every node/u);
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

  it("gives the nodes that carry the add-ons room for them", () => {
    const addons =
      '{"headlamp":50,"monitoring":6000,"kyverno":300,"istio":200,"certManager":100,"rancher":1500,"rancherAgent":250}';
    const figured: ClusterClass = {
      ...rke2,
      annotations: {
        "kubehenge.dev/addon-memory": addons,
        "kubehenge.dev/node-memory": '{"controlPlane":4096,"worker":640}',
      },
    };
    expect(addonMemory(form, figured)).toStrictEqual({
      names: ["Headlamp", "monitoring", "cert-manager"],
      mib: 6150,
      controlPlaneMiB: 0,
    });
    expect(addonMemory({ ...form, rancherRegister: true }, figured).mib).toBe(6400);
    expect(
      addonMemory(
        { ...form, rancherInstall: true, istio: true, podSecurity: { admission: "privileged", kyverno: "baseline" } },
        figured,
      ).names,
    ).toStrictEqual(["Headlamp", "monitoring", "Kyverno", "Istio", "cert-manager", "Rancher"]);
    const plain: ClusterClass = { ...rke2, annotations: {} };
    expect(addonMemory(form, plain).mib).toBe(0);
    // A worker needs the class's least, or what it uses itself and its share of the add-ons in
    // Proxmox VE's steps, whichever is more: two least workers hold them, one does not.
    const least = { cores: 4, memoryMiB: 4096, diskGiB: 40 };
    expect(fieldProblems("Machines", { ...form, workerMachine: least }, figured)).toStrictEqual({});
    const one = { ...form, workerReplicas: 1, workerMachine: least };
    expect(fieldProblems("Machines", one, figured)).toStrictEqual({
      workerMachine: "At least 6792 MiB of memory for the add-ons chosen, in steps of 8",
    });
    expect(fieldProblems("Machines", one, plain)).toStrictEqual({});
    expect(withRoom(one, figured).workerMachine).toStrictEqual({ ...least, memoryMiB: 6792 });
    expect(withRoom(form, figured)).toStrictEqual(form);
    // A class that says nothing of its nodes' own use counts its least for it.
    const unmeasured: ClusterClass = { ...figured, annotations: { "kubehenge.dev/addon-memory": addons } };
    expect(fieldProblems("Machines", one, unmeasured).workerMachine).toMatch(/^At least 10248 MiB of memory/u);
    // Without workers the control planes carry them.
    const alone = {
      ...form,
      workerReplicas: 0,
      controlPlaneReplicas: 1,
      controlPlaneMachine: { cores: 4, memoryMiB: 8192, diskGiB: 30 },
    };
    expect(fieldProblems("Machines", alone, figured)).toStrictEqual({
      controlPlaneMachine: "At least 10248 MiB of memory for the add-ons chosen, in steps of 8",
    });
    expect(
      withRoom({ ...alone, controlPlaneMachine: { cores: 2, memoryMiB: 4096, diskGiB: 30 } }, figured),
    ).toMatchObject({
      controlPlaneMachine: { cores: 4, memoryMiB: 10_248 },
      workerMachine: form.workerMachine,
    });
  });

  it("gives the control planes room for what the add-ons add to their own use", () => {
    const grown: ClusterClass = {
      ...rke2,
      annotations: {
        "kubehenge.dev/addon-memory": '{"headlamp":50,"monitoring":1500,"certManager":100}',
        "kubehenge.dev/addon-control-plane-memory": '{"monitoring":900,"certManager":100}',
        "kubehenge.dev/node-memory": '{"controlPlane":3200,"worker":640}',
      },
    };
    const least = { cores: 2, memoryMiB: 4096, diskGiB: 30 };
    const withWorkers = { ...form, workerReplicas: 1, controlPlaneMachine: least };
    expect(addonMemory(withWorkers, grown)).toStrictEqual({
      names: ["Headlamp", "monitoring", "cert-manager"],
      mib: 1650,
      controlPlaneMiB: 1000,
    });
    expect(fieldProblems("Machines", withWorkers, grown).controlPlaneMachine).toBe(
      "At least 4200 MiB of memory for the add-ons chosen, in steps of 8",
    );
    expect(withRoom(withWorkers, grown).controlPlaneMachine).toStrictEqual({ ...least, memoryMiB: 4200 });
    // Headlamp alone adds nothing to them.
    const headlamp = { ...withWorkers, monitoring: false, rancherRegister: false };
    expect(fieldProblems("Machines", headlamp, grown).controlPlaneMachine).toBeUndefined();
    // Without workers it adds to what they use themselves, under the add-ons' pods.
    const alone = { ...withWorkers, workerReplicas: 0, controlPlaneReplicas: 1 };
    expect(addonNeedMiB(alone, grown)).toBe(3200 + 1000 + 1656);
  });

  it("gives the nodes that carry the add-ons the CPU their pods ask for", () => {
    // Flux's figure as its pods asked for before Kubehenge gave them 10m each.
    const figures =
      '{"headlamp":10,"monitoring":20,"logs":75,"kyverno":400,"istio":400,"clusterMesh":0,"loadBalancer":50,"certManager":0,"rancherAgent":0,"flux":500}';
    const figured: ClusterClass = {
      ...rke2,
      annotations: {
        "kubehenge.dev/addon-cpu": figures,
        "kubehenge.dev/node-cpu": '{"controlPlane":1555,"worker":745}',
      },
    };
    expect(addonCPU(form, figured)).toStrictEqual({ names: ["Headlamp", "monitoring"], millicores: 30 });
    const least = { cores: 2, memoryMiB: 8192, diskGiB: 40 };
    const heavy: Form = {
      ...form,
      workerReplicas: 1,
      workerMachine: least,
      istio: true,
      podSecurity: { admission: "privileged", kyverno: "baseline" },
      logs: { ...form.logs, enabled: true },
      loadBalancer: "metallb",
      gitops: { ...form.gitops, engine: "flux" },
    };
    expect(addonCPU(heavy, figured)).toStrictEqual({
      names: ["Headlamp", "monitoring", "logs", "Kyverno", "Istio", "the load balancer", "Flux"],
      millicores: 1455,
    });
    // A worker needs the cores for what it requests itself and its share: one such worker does not
    // hold them on two cores, two do.
    expect(fieldProblems("Machines", heavy, figured)).toStrictEqual({
      workerMachine: "A whole number of cores, at least 3 for the add-ons chosen",
    });
    expect(withRoom(heavy, figured).workerMachine).toStrictEqual({ ...least, cores: 3 });
    expect(fieldProblems("Machines", { ...heavy, workerReplicas: 2 }, figured)).toStrictEqual({});
    // A class that says nothing of its nodes' own CPU counts their least for it.
    const unmeasured: ClusterClass = { ...rke2, annotations: { "kubehenge.dev/addon-cpu": figures } };
    expect(fieldProblems("Machines", heavy, unmeasured).workerMachine).toMatch(
      /^A whole number of cores, at least 4 for the add-ons chosen$/u,
    );
    expect(fieldProblems("Machines", heavy, { ...rke2, annotations: {} })).toStrictEqual({});
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
    await expect(screen.findByText(/Session ended/u)).resolves.toBeInTheDocument();
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
    expect(name).toHaveAccessibleDescription("Enter a name");
    expect(screen.getByLabelText("Distribution")).toHaveAttribute("aria-invalid", "true");

    await user.type(name, "lab");
    expect(name).not.toHaveAttribute("aria-invalid");
  });

  it("walks through the wizard and creates the cluster", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const calls = fakeServer({
      "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1" } }),
      "GET /api/classes": () => ({ body: [rke2OS] }),
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
          { node: "pve2", online: true, memory: 0, maxMemory: 2 ** 32 },
        ],
      }),
      "GET /api/proxmox/nodes/pve1/bridges": () => ({
        body: [
          { bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true, cidr: "10.0.0.5/16", gateway: "10.0.0.1" },
        ],
      }),
      "GET /api/proxmox/nodes/pve1/dns": () => ({ body: { servers: ["10.0.0.53"] } }),
      "POST /api/proxmox/nodes/pve1/bridges/vmbr0/free": freeOn,
      "GET /api/proxmox/ha-rules": () => ({
        body: {
          readable: true,
          rules: [{ rule: "fast", comment: "SSD", nodes: ["pve2"], strict: true, allowed: ["pve2"] }],
        },
      }),
      "POST /api/clusters?dryRun=All": () => ({ body: { manifest: "kind: Cluster\nmetadata:\n  name: lab\n" } }),
      "POST /api/clusters": () => ({ status: 201, body: { ...web, name: "lab" } }),
      "GET /api/clusters/default/lab": () => ({ body: { ...web, name: "lab" } }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.type(await screen.findByLabelText("Name"), "lab");
    await waitFor(() => expect(screen.getByRole("option", { name: "RKE2" })).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-rke2");
    expect(screen.getByLabelText("Node OS")).toHaveValue("almalinux-10");
    await user.selectOptions(screen.getByLabelText("Node OS"), "rocky-10");
    expect(screen.getByLabelText("Node OS")).toHaveAccessibleDescription(/Needs Proxmox VE nodes with x86-64-v3 CPUs/u);
    await user.type(screen.getByLabelText("Kubernetes version"), "v1.36.5+rke2r1");
    await user.click(screen.getByRole("button", { name: "Next" }));

    // The one bridge, like the one usable storage after, is chosen already.
    await waitFor(() => expect(screen.getByLabelText("Bridge")).toHaveValue("vmbr0"));
    // The bridge's network gives the prefix length, gateway and DNS servers, and the examples.
    await waitFor(() => expect(screen.getByLabelText("DNS servers")).toHaveValue("10.0.0.53"));
    expect(screen.getByLabelText("Gateway")).toHaveValue("10.0.0.1");
    expect(screen.getByLabelText("Prefix length")).toHaveValue(16);
    expect(screen.getByLabelText("API address")).toHaveAttribute("placeholder", "10.0.0.20");
    expect(screen.getByLabelText("Node addresses")).toHaveAccessibleDescription(
      "Ranges like 10.0.0.21-10.0.0.29, networks or addresses",
    );
    // Free addresses are filled in; one typed that a host answers on is marked until it changes.
    await waitFor(() => expect(screen.getByLabelText("API address")).toHaveValue("10.0.0.30"));
    const addresses = screen.getByLabelText("Node addresses");
    expect(addresses).toHaveValue("10.0.0.31-10.0.0.33");
    await user.clear(addresses);
    await user.type(addresses, "10.0.0.33-10.0.0.39");
    expect(screen.getByLabelText("API address")).toHaveValue("10.0.0.30");
    await waitFor(() => expect(addresses).toHaveAccessibleDescription("In use: 10.0.0.33"));
    await user.clear(addresses);
    await user.type(addresses, "10.0.0.40-10.0.0.46");
    await waitFor(() => expect(addresses).toHaveAccessibleDescription(/^Ranges like/u));
    // A VLAN's network is not the bridge's own.
    await user.type(screen.getByLabelText("VLAN tag"), "10");
    expect(screen.getByLabelText("Gateway")).toHaveValue("");
    expect(screen.getByLabelText("API address")).toHaveValue("");
    await user.clear(screen.getByLabelText("VLAN tag"));
    expect(screen.getByLabelText("Gateway")).toHaveValue("10.0.0.1");
    await waitFor(() => expect(screen.getByLabelText("API address")).toHaveValue("10.0.0.30"));
    expect(screen.getByLabelText("Pod network")).toHaveValue("canal");
    await user.click(screen.getByRole("button", { name: "Next" }));

    expect(screen.queryByRole("option", { name: /tank/u })).not.toBeInTheDocument();
    expect(screen.getByLabelText("Storage")).toHaveValue("local-lvm");
    await user.click(screen.getByRole("radio", { name: "Chosen nodes" }));
    expect(within(screen.getByRole("group", { name: "Nodes" })).getAllByRole("checkbox")).toHaveLength(2);
    await user.click(screen.getByRole("radio", { name: "HA rule nodes" }));
    await user.selectOptions(screen.getByLabelText("HA rule"), "fast");
    expect(screen.getByRole("option", { name: "fast (SSD): pve2, strict" })).toBeInTheDocument();
    // An emptied number stays empty and is refused, instead of turning into 0.
    const cores = workerCores();
    await user.clear(cores);
    expect(cores).toHaveValue(null);
    await user.type(cores, "4");
    expect(cores).toHaveValue(4);
    await user.click(screen.getByRole("button", { name: "Next" }));
    // The add-ons' memory shows as they are chosen; Rancher's agent counts while the cluster registers.
    expect(screen.getByText(/^Each worker needs at least 4096 MiB/u)).toBeInTheDocument();
    expect(screen.getByText("Add-ons use about 512 MiB of memory: Rancher's agent.")).toBeInTheDocument();
    // Rancher raises the machines to the cores it needs, and they stay raised once it is unticked.
    expect(screen.getByText(/Needs 4 cores per node\.$/u)).toBeInTheDocument();
    await user.click(screen.getByRole("checkbox", { name: /Install Rancher/u }));
    expect(
      screen.getByText("Add-ons use about 2240 MiB of memory and 10 millicores of CPU: cert-manager, Rancher."),
    ).toBeInTheDocument();
    expect(screen.getByText(/^Each worker needs at least \d+ MiB and 4 cores$/u)).toBeInTheDocument();
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
    expect(added.getByLabelText("Issuer URL")).toHaveAccessibleDescription(/^Enter an https:\/\/ URL/u);
    expect(added.getByLabelText("Client ID")).not.toHaveAttribute("aria-invalid");
    await user.click(screen.getByRole("button", { name: "Next" }));
    // Next stays on the step, marks every problem and puts the cursor on the first.
    await waitFor(() => expect(added.getByLabelText("Issuer URL")).toHaveFocus());
    expect(added.getByLabelText("Client ID")).toHaveAccessibleDescription(/^Enter a client ID /u);
    expect(screen.getByRole("button", { name: "Add identity provider" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove identity provider 2" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    // Confirm has the API server check the cluster, and weighs it against the nodes it may use.
    const before = screen.getByLabelText("Before creating");
    await waitFor(() => expect(before).toHaveTextContent("Accepts it"));
    expect(before).toHaveTextContent(/MemoryNeeds [\d.]+ GiB, 4\.0 GiB free/u);
    expect(screen.getByLabelText("YAML")).toHaveTextContent("name: lab");
    await user.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() => expect(window.location.pathname).toBe("/clusters/default/lab"));
    expect(calls.map((c) => c.key)).toContain("GET /api/clusters/default/lab");
    const created = calls.find((c) => c.key === "POST /api/clusters");
    expect(calls.find((c) => c.key === "POST /api/clusters?dryRun=All")?.body).toStrictEqual(created?.body);
    expect(created?.headers.get("X-Kubehenge-CSRF")).toBe("c1");
    expect(created?.body).toMatchObject({
      name: "lab",
      class: "kubehenge-rke2",
      variables: {
        storage: "local-lvm",
        allowedNodes: ["pve2"],
        nodeOS: { family: "rocky", release: "10" },
        cni: "canal",
        workerMachine: { cores: 4 },
        controlPlaneMachine: { cores: 4 },
        controlPlaneEndpoint: "10.0.0.30",
        network: {
          bridge: "vmbr0",
          prefix: 16,
          gateway: "10.0.0.1",
          dnsServers: ["10.0.0.53"],
          addresses: ["10.0.0.40-10.0.0.46"],
        },
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
      versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
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
      "Needs Sys.Modify on the workload pool",
    );
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("button", { name: "Add identity provider" })).toBeDisabled();
    expect(screen.getByLabelText("Issuer URL")).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    const dialog = screen.getByRole("dialog", { name: "Discard new cluster" });
    await user.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    expect(window.location.pathname).toBe("/clusters/new");
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Discard" }));
    expect(window.location.pathname).toBe("/");
  });
});
