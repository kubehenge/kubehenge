import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { applyStaged, fakeServer, reads } from "./fake-server";
import { addonChangesPendingCondition, addonConflictCondition, standing, windowTime } from "./fleet";
import { labelsLines, labelsText, maxLabelValue, maxNamespaceLabels, parseLabels } from "./namespace-labels";
import type { Cluster, ClusterClass } from "./types";
import { abnormal } from "./ui";
import { record } from "./values";

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
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: { storage: "local-lvm" },
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: true },
  machines: [],
};

const opens = "2026-11-07T02:00:00Z";
const waiting: Cluster = {
  ...web,
  addonChanges: {
    opens,
    changes: [
      { addon: "monitoring", chart: "kube-prometheus-stack", from: "70.0.0", to: "71.0.0" },
      { addon: "headlamp", chart: "headlamp", from: "0.30.0", to: "0.30.0" },
    ],
  },
};

const applyPath = "/api/clusters/default/web/addon-changes";
const session = {
  "GET /api/session": () => ({ body: { user: "alice@pve", csrf: "c1", canCreate: true, canTrust: true } }),
};

const rke2: ClusterClass = {
  name: "kubehenge-rke2",
  distribution: "rke2",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+rke2r\d+$` },
  variables: [
    { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
    { name: "network", required: true, schema: { type: "object" } },
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "certificates", required: false, schema: { type: "object" } },
    { name: "podSecurity", required: false, schema: { type: "object" } },
    { name: "namespaceLabels", required: false, schema: { type: "object" } },
    { name: "useExisting", required: false, schema: { type: "object" } },
  ],
};

describe("waiting add-on changes", () => {
  it("lists them with the window and applies them now", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const calls = fakeServer({
      ...session,
      "GET /api/clusters/default/web": () => ({ body: waiting }),
      [`POST ${applyPath}`]: { status: 202 },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const block = await screen.findByRole("group", { name: "Add-on changes" });
    expect(within(block).getByText("monitoring: kube-prometheus-stack 70.0.0 to 71.0.0")).toBeInTheDocument();
    expect(within(block).getByText("headlamp: new values")).toBeInTheDocument();
    expect(
      within(block).getByText(`Applies at ${windowTime(opens)}, when the upgrade window opens.`),
    ).toBeInTheDocument();
    expect(within(block).queryByText(/Otherwise/u)).not.toBeInTheDocument();
    const before = reads(calls, "GET /api/clusters/default/web");
    await user.click(within(block).getByRole("button", { name: "Apply now" }));
    await waitFor(() => expect(calls.map((c) => c.key)).toContain(`POST ${applyPath}`));
    await waitFor(() => expect(reads(calls, "GET /api/clusters/default/web")).toBeGreaterThan(before));
  });

  it("says why it could not apply", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({
      ...session,
      "GET /api/clusters/default/web": () => ({ body: waiting }),
      [`POST ${applyPath}`]: { status: 409, body: { error: "The cluster is changing. Try again later." } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Apply now" }));
    await expect(screen.findByText("The cluster is changing. Try again later.")).resolves.toBeInTheDocument();
  });

  it("shows nothing for a cluster with no changes waiting", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({ ...session, "GET /api/clusters/default/web": () => ({ body: web }) });
    render(<App />);
    await screen.findByText("Namespace");
    expect(screen.queryByRole("group", { name: "Add-on changes" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Apply now" })).not.toBeInTheDocument();
  });

  it("offers Apply now only to those who may change the cluster", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({
      ...session,
      "GET /api/clusters/default/web": () => ({ body: { ...waiting, allowed: { change: false, kubeconfig: true } } }),
    });
    render(<App />);
    const block = await screen.findByRole("group", { name: "Add-on changes" });
    expect(within(block).getByText("headlamp: new values")).toBeInTheDocument();
    expect(within(block).queryByRole("button", { name: "Apply now" })).not.toBeInTheDocument();
  });

  it("lists them in the Upgrade dialog, which applies them too", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const calls = fakeServer({
      ...session,
      "GET /api/clusters/default/web": () => ({ body: waiting }),
      "GET /api/versions/rke2": () => ({ body: { versions: ["v1.36.6+rke2r1"], stable: "v1.36.6+rke2r1" } }),
      [`POST ${applyPath}`]: { status: 202 },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Upgrade" }));
    const dialog = await screen.findByRole("dialog", { name: "Upgrade web" });
    const block = within(dialog).getByRole("group", { name: "Add-on changes" });
    expect(within(block).getByText("monitoring: kube-prometheus-stack 70.0.0 to 71.0.0")).toBeInTheDocument();
    await user.click(within(block).getByRole("button", { name: "Apply now" }));
    await waitFor(() => expect(calls.map((c) => c.key)).toContain(`POST ${applyPath}`));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("leaves the Upgrade dialog without the list when nothing waits", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    fakeServer({
      ...session,
      "GET /api/clusters/default/web": () => ({ body: web }),
      "GET /api/versions/rke2": () => ({ body: { versions: ["v1.36.6+rke2r1"], stable: "v1.36.6+rke2r1" } }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Upgrade" }));
    const dialog = await screen.findByRole("dialog", { name: "Upgrade web" });
    expect(within(dialog).queryByRole("group", { name: "Add-on changes" })).not.toBeInTheDocument();
  });

  it("names a conflict and keeps its message behind Details", async () => {
    window.history.replaceState(null, "", "/clusters/default/web");
    const conflicted: Cluster = {
      ...web,
      conditions: [
        {
          type: addonConflictCondition,
          status: "True",
          reason: "Installed",
          message: "cert-manager is already installed in the cluster",
          since: opens,
        },
      ],
    };
    fakeServer({ ...session, "GET /api/clusters/default/web": () => ({ body: conflicted }) });
    render(<App />);
    await screen.findByText("Add-on conflict", { selector: "strong" });
    expect(screen.getByText("cert-manager is already installed in the cluster")).toBeInTheDocument();
  });
});

const withCondition = (type: string, status = "True"): Cluster => ({
  ...web,
  conditions: [{ type, status, since: opens }],
});

describe("the add-on conditions", () => {
  it("stands for a conflict that needs someone", () => {
    expect(standing(withCondition(addonConflictCondition))).toMatchObject({
      group: "needs",
      state: "warning",
      reason: "Add-on conflict",
      verdict: "Add-on conflict",
    });
  });

  it("leaves a cluster as it is while changes wait for the window", () => {
    const held = { ...waiting, conditions: withCondition(addonChangesPendingCondition).conditions };
    expect(standing(held)).toMatchObject({ group: "healthy", state: "ok", verdict: "Available" });
    // The date and time follow the browser's locale and clock, so only what is left out is fixed: the
    // year and the seconds.
    const { reason } = standing(held);
    expect(reason).toMatch(/^Add-on changes \S/u);
    expect(reason).not.toContain(opens.slice(0, 4));
    expect(reason).not.toMatch(/\d:\d{2}:\d{2}/u);
  });

  it("stands for neither once they are false", () => {
    expect(standing(withCondition(addonConflictCondition, "False")).verdict).toBe("Available");
    expect(standing(withCondition(addonChangesPendingCondition, "False")).verdict).toBe("Available");
  });

  it("marks a conflict on the Health page only while true, and held changes never", () => {
    expect([
      abnormal(addonConflictCondition, "True"),
      abnormal(addonConflictCondition, "False"),
      abnormal(addonChangesPendingCondition, "True"),
    ]).toStrictEqual([true, false, false]);
  });
});

describe("namespace labels", () => {
  it("reads one key=value per line", () => {
    expect(parseLabels("team=payments\n\n  env = prod \n")).toStrictEqual({
      labels: { team: "payments", env: "prod" },
    });
    expect(labelsLines({ team: "payments", env: "prod" })).toBe("team=payments\nenv=prod");
    expect(labelsText({})).toBe("None");
    expect(labelsText({ team: "payments", env: "prod" })).toBe("team=payments, env=prod");
  });

  it("names the first line at fault", () => {
    expect(parseLabels("team").problem).toBe("Line 1 needs key=value");
    expect(parseLabels("a=b\n=x").problem).toBe("Line 2 has no key");
    expect(parseLabels(`a=${"x".repeat(maxLabelValue + 1)}`).problem).toBe(
      `Line 1: value over ${maxLabelValue} characters`,
    );
    expect(parseLabels(`a=${"x".repeat(maxLabelValue)}`).problem).toBeUndefined();
    const many = Array.from({ length: maxNamespaceLabels + 1 }, (_, i) => `k${i}=v`).join("\n");
    expect(parseLabels(many).problem).toBe(`At most ${maxNamespaceLabels} labels`);
    expect(parseLabels(many.split("\n").slice(1).join("\n")).problem).toBeUndefined();
  });
});

const wizard = (): ReturnType<typeof fakeServer> => {
  window.history.replaceState(null, "", "/clusters/new");
  return fakeServer({
    ...session,
    "GET /api/classes": () => ({ body: [rke2] }),
    "GET /api/proxmox/storages": () => ({
      body: [
        { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 1, total: 2, usable: true },
      ],
    }),
    "GET /api/proxmox/nodes": () => ({ body: [{ node: "pve1", online: true }] }),
    "GET /api/proxmox/nodes/pve1/bridges": () => ({
      body: [{ bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true }],
    }),
    "GET /api/proxmox/ha-rules": () => ({ body: { readable: true, rules: [] } }),
    "POST /api/clusters?dryRun=All": () => ({ body: { manifest: "kind: Cluster\n" } }),
    "POST /api/clusters": () => ({ status: 201, body: web }),
    "GET /api/clusters/default/web": () => ({ body: web }),
  });
};

const toAddOns = async (user: ReturnType<typeof userEvent.setup>): Promise<void> => {
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
};

const created = (calls: ReturnType<typeof fakeServer>): Record<string, unknown> =>
  record(record(calls.find((c) => c.key === "POST /api/clusters")?.body)["variables"]);

describe("add-on installs of a new cluster", () => {
  it("sends the existing cert-manager and the labels that were entered", async () => {
    const calls = wizard();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await toAddOns(user);
    const certificates = within(screen.getByRole("group", { name: "Certificates" }));
    await user.click(certificates.getByRole("checkbox", { name: "Use existing cert-manager" }));
    const podSecurity = within(screen.getByRole("group", { name: "Pod security" }));
    await user.type(podSecurity.getByLabelText("Namespace labels"), "team=payments{Enter}env = prod");
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Existing cert-manager")).toBeInTheDocument();
    expect(screen.getByText("team=payments, env=prod")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Create" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(window.location.pathname).toBe("/clusters/default/web"));
    expect(created(calls)).toMatchObject({
      useExisting: { certManager: true },
      namespaceLabels: { team: "payments", env: "prod" },
    });
  });

  it("sends neither while they are left alone", async () => {
    const calls = wizard();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await toAddOns(user);
    expect(screen.getByRole("checkbox", { name: "Use existing cert-manager" })).not.toBeChecked();
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Create" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(window.location.pathname).toBe("/clusters/default/web"));
    expect(created(calls)).not.toHaveProperty("useExisting");
    expect(created(calls)).not.toHaveProperty("namespaceLabels");
  });

  it("says what is wrong with the labels and stays on the step", async () => {
    wizard();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await toAddOns(user);
    const labels = screen.getByLabelText("Namespace labels");
    await user.type(labels, "team");
    expect(labels).toHaveAccessibleDescription(/^Line 1 needs key=value/u);
    await user.clear(labels);
    await user.type(labels, "=x");
    expect(labels).toHaveAccessibleDescription(/^Line 1 has no key/u);
    await user.clear(labels);
    await user.type(labels, `a=${"x".repeat(maxLabelValue + 1)}`);
    expect(labels).toHaveAccessibleDescription(new RegExp(`^Line 1: value over ${maxLabelValue} characters`, "u"));
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("group", { name: "Pod security" })).toBeInTheDocument();
  });

  it("limits the labels to the most a cluster takes", async () => {
    wizard();
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await toAddOns(user);
    const labels = screen.getByLabelText("Namespace labels");
    await user.click(labels);
    await user.paste(Array.from({ length: maxNamespaceLabels + 1 }, (_, i) => `k${i}=v`).join("\n"));
    expect(labels).toHaveAccessibleDescription(new RegExp(`^At most ${maxNamespaceLabels} labels`, "u"));
  });

  it("offers neither where the class defines neither", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    fakeServer({
      ...session,
      "GET /api/classes": () => ({ body: [{ ...rke2, variables: rke2.variables.slice(0, 3) }] }),
      "GET /api/proxmox/storages": () => ({ body: [] }),
      "GET /api/proxmox/nodes": () => ({ body: [] }),
      "GET /api/proxmox/ha-rules": () => ({ body: { readable: true, rules: [] } }),
      "GET /api/clusters": () => ({ body: [] }),
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.type(await screen.findByLabelText("Name"), "web");
    await waitFor(() => expect(screen.getByRole("option", { name: "RKE2" })).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-rke2");
    expect(screen.queryByRole("checkbox", { name: "Use existing cert-manager" })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Namespace labels")).not.toBeInTheDocument();
  });
});

describe("add-on installs of a cluster", () => {
  const labelled: Cluster = {
    ...web,
    variables: { storage: "local-lvm", namespaceLabels: { team: "payments" }, useExisting: { certManager: true } },
  };

  it("lists the labels and the existing cert-manager", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/options");
    fakeServer({ ...session, "GET /api/clusters/default/web": () => ({ body: labelled }) });
    render(<App />);
    await expect(screen.findByText("team=payments")).resolves.toBeInTheDocument();
    expect(screen.getByText("Existing cert-manager")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Edit existing cert-manager/iu })).not.toBeInTheDocument();
  });

  it("lists no existing cert-manager unless the cluster uses one", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/options");
    fakeServer({ ...session, "GET /api/clusters/default/web": () => ({ body: web }) });
    render(<App />);
    await screen.findByRole("button", { name: "Edit namespace labels" });
    expect(within(screen.getByRole("row", { name: /Namespace labels/u })).getByText("None")).toBeInTheDocument();
    expect(screen.queryByText("Existing cert-manager")).not.toBeInTheDocument();
  });

  it("saves the labels, an empty object once they are cleared", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/options");
    const calls = fakeServer({
      ...session,
      "GET /api/clusters/default/web": () => ({ body: labelled }),
      "PATCH /api/clusters/default/web": () => ({ body: labelled }),
    });
    const sent = (): unknown[] => calls.filter((c) => c.key === "PATCH /api/clusters/default/web").map((c) => c.body);
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Edit namespace labels" }));
    let dialog = screen.getByRole("dialog", { name: "Edit namespace labels" });
    const text = within(dialog).getByLabelText("Namespace labels");
    expect(text).toHaveValue("team=payments");
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.type(text, "{Enter}env=prod");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await applyStaged(user);
    await waitFor(() =>
      expect(sent()).toStrictEqual([{ variables: { namespaceLabels: { team: "payments", env: "prod" } } }]),
    );

    await user.click(screen.getByRole("button", { name: "Edit namespace labels" }));
    dialog = screen.getByRole("dialog", { name: "Edit namespace labels" });
    await user.clear(within(dialog).getByLabelText("Namespace labels"));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await applyStaged(user);
    await waitFor(() => expect(sent()).toHaveLength(2));
    expect(sent()[1]).toStrictEqual({ variables: { namespaceLabels: {} } });
  });

  it("refuses a line without a key in the dialog", async () => {
    window.history.replaceState(null, "", "/clusters/default/web/options");
    fakeServer({ ...session, "GET /api/clusters/default/web": () => ({ body: labelled }) });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Edit namespace labels" }));
    const dialog = screen.getByRole("dialog", { name: "Edit namespace labels" });
    await user.type(within(dialog).getByLabelText("Namespace labels"), "{Enter}=x");
    expect(within(dialog).getByLabelText("Namespace labels")).toHaveAccessibleDescription(/^Line 2 has no key/u);
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
  });
});
