import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { newForm, withTalosRelease } from "./create-form";
import { fakeServer, held, reads } from "./fake-server";
import { failedText, forRelease, talosTargets, upgradedText, upgradingText } from "./Talos";
import type { Cluster, ClusterClass, TalosReleases } from "./types";

const talosClass: ClusterClass = {
  name: "kubehenge-talos",
  distribution: "talos",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+$` },
  variables: [
    { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
    { name: "network", required: true, schema: { type: "object" } },
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "talosVersion", required: true, schema: { type: "string" } },
    {
      name: "cni",
      required: false,
      schema: { type: "string", enum: ["flannel", "cilium", "calico"], default: "flannel" },
    },
  ],
};

const releases: TalosReleases = {
  templates: ["v1.15.1", "v1.14.4"],
  releases: ["v1.16.0", "v1.15.1"],
  default: "v1.14.4",
  kubernetes: {
    "v1.16.0": ["1.36", "1.37", "1.38"],
    "v1.15.1": ["1.34", "1.35", "1.36", "1.37"],
    "v1.14.4": ["1.33", "1.34", "1.35", "1.36"],
  },
};

const web: Cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-talos",
  distribution: "talos",
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
  variables: { talosVersion: "v1.15.1" },
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: true },
  machines: [
    {
      name: "web-cp-abc",
      node: "web-cp-abc",
      role: "control-plane",
      version: "v1.36.5",
      talos: "v1.15.1",
      phase: "Running",
      ready: true,
      created: new Date().toISOString(),
    },
  ],
  talos: { version: "v1.15.1" },
};

const shown = (iso: string): string => new Date(iso).toLocaleString();

// kubernetes lists the wizard's Kubernetes versions as offered.
const kubernetes = (): (string | null)[] =>
  within(screen.getByLabelText("Kubernetes version"))
    .getAllByRole("option")
    .map((o) => o.textContent);

const term = (name: string): Element | null => screen.getByText(name, { selector: "dt" }).nextElementSibling;

describe("talos clusters", () => {
  it("reads the controller's records of node upgrades", () => {
    expect(upgradingText("web-cp-abc to v1.15.1 since 2026-10-23T10:00:00Z")).toBe(
      `web-cp-abc to v1.15.1, since ${shown("2026-10-23T10:00:00Z")}`,
    );
    expect(upgradingText("web-cp-abc to v1.15.1 since 2026-10-23T10:00:00Z, rebooted at 2026-10-23T10:02:00Z")).toBe(
      `web-cp-abc to v1.15.1, since ${shown("2026-10-23T10:00:00Z")}, rebooted ${shown("2026-10-23T10:02:00Z")}`,
    );
    expect(upgradedText("web-cp-abc to v1.15.1 at 2026-10-23T10:05:00Z")).toBe(
      `web-cp-abc to v1.15.1, ${shown("2026-10-23T10:05:00Z")}`,
    );
    expect(
      failedText("web-md-1 to v1.15.1 at 2026-10-23T09:40:00Z: the node did not come back: timeout"),
    ).toStrictEqual({
      state: "Couldn't upgrade web-md-1 to Talos v1.15.1",
      detail: `${shown("2026-10-23T09:40:00Z")}: the node did not come back: timeout`,
    });
    expect(failedText("something else")).toStrictEqual({ state: "Talos upgrade failed", detail: "something else" });
    expect(upgradedText("something else")).toBe("something else");
    expect(talosTargets("v1.14.2", ["v1.16.0", "v1.15.3", "v1.14.4", "v1.14.2", "v1.13.9"])).toStrictEqual([
      "v1.15.3",
      "v1.14.4",
    ]);
  });

  it("offers the Kubernetes versions a Talos release runs", () => {
    const offered = { versions: ["v1.38.0", "v1.37.1", "v1.36.5", "v1.33.9"], stable: "v1.36.5" };
    expect(forRelease(offered, releases, "v1.14.4")).toStrictEqual({
      versions: ["v1.36.5", "v1.33.9"],
      stable: "v1.36.5",
    });
    // A release the table does not name is left to the server to judge.
    expect(forRelease(offered, releases, "v1.13.0")).toBe(offered);
    expect(forRelease(offered, undefined, "v1.14.4")).toBe(offered);
    expect(forRelease({ versions: ["v1.38.0"] }, releases, "v1.14.4")).toStrictEqual({
      versions: [],
      error: "Talos v1.14.4 runs none of the Kubernetes versions offered",
    });

    // The newest release with a template comes first. A version the release does not run moves to
    // the newest it does, and one it runs stays.
    const form = { ...newForm(), version: "v1.37.1" };
    expect(withTalosRelease(form, releases, offered)).toMatchObject({ talosVersion: "v1.15.1", version: "v1.37.1" });
    expect(withTalosRelease({ ...form, talosVersion: "v1.14.4" }, releases, offered)).toMatchObject({
      talosVersion: "v1.14.4",
      version: "v1.36.5",
    });
    expect(withTalosRelease({ ...form, talosVersion: "v1.13.0" }, releases, offered).version).toBe("v1.37.1");
  });

  it("creates a cluster on the newest Talos release with a template", async () => {
    window.history.replaceState(null, "", "/clusters/new");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "alice@pve", csrf: "c1" } },
      "GET /api/classes": { body: [talosClass] },
      "GET /api/versions/talos": { body: { versions: ["v1.38.0", "v1.37.1", "v1.36.5"], stable: "v1.36.5" } },
      "GET /api/talos/releases": { body: releases },
      "GET /api/template-builds": { body: { allowed: false, requested: [], building: [] } },
      "GET /api/proxmox/storages": {
        body: [
          { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 0, total: 0, usable: true },
        ],
      },
      "GET /api/proxmox/nodes": { body: [] },
      "GET /api/clusters": { body: [] },
      "POST /api/clusters?dryRun=All": { body: { manifest: "kind: Cluster\n" } },
      "POST /api/clusters": { status: 201, body: { ...web, name: "lab" } },
      "GET /api/clusters/clusters/lab": { body: { ...web, name: "lab" } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await user.type(await screen.findByLabelText("Name"), "lab");
    await waitFor(() => expect(screen.getByRole("option", { name: "Talos" })).toBeInTheDocument());
    await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-talos");
    await waitFor(() => expect(screen.getByLabelText("Talos release")).toHaveValue("v1.15.1"));
    // A release the Image Factory builds without a template is listed, but needs its template first.
    expect(screen.getByRole("option", { name: "v1.16.0 (no template yet)" })).toBeDisabled();
    await waitFor(() => expect(screen.getByLabelText("Kubernetes version")).toHaveValue("v1.36.5"));
    // Only what the release runs: v1.15.1 runs no 1.38.
    expect(kubernetes()).toStrictEqual(["Choose…", "v1.37.1 (newer than stable)", "v1.36.5 (stable)"]);
    await user.selectOptions(screen.getByLabelText("Kubernetes version"), "v1.37.1");
    // Another release that does not run the version chosen moves it to the newest it runs.
    await user.selectOptions(screen.getByLabelText("Talos release"), "v1.14.4");
    expect(screen.getByLabelText("Kubernetes version")).toHaveValue("v1.36.5");
    expect(kubernetes()).toStrictEqual(["Choose…", "v1.36.5 (stable)"]);
    await user.click(screen.getByRole("button", { name: "Next" }));

    await user.type(screen.getByLabelText("Bridge"), "vmbr0");
    await user.type(screen.getByLabelText("API address"), "10.0.0.20");
    await user.type(screen.getByLabelText("Node addresses"), "10.0.0.21-10.0.0.29");
    await user.type(screen.getByLabelText("Gateway"), "10.0.0.1");
    await user.type(screen.getByLabelText("DNS servers"), "10.0.0.1");
    expect(screen.getByLabelText("Pod network")).toHaveValue("flannel");
    expect(screen.getByRole("option", { name: "Flannel (Talos's own)" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Cilium" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() => expect(screen.getByLabelText("Storage")).toHaveValue("local-lvm"));
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Next" }));

    expect(term("Talos release")).toHaveTextContent("v1.14.4");
    expect(term("Kubernetes version")).toHaveTextContent("v1.36.5");
    await waitFor(() => expect(screen.getByRole("button", { name: "Create" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(window.location.pathname).toBe("/clusters/clusters/lab"));
    expect(calls.find((c) => c.key === "POST /api/clusters")?.body).toMatchObject({
      class: "kubehenge-talos",
      version: "v1.36.5",
      variables: { talosVersion: "v1.14.4", cni: "flannel", storage: "local-lvm" },
    });
  });

  it("shows how the nodes' Talos upgrade stands and tries a failed one again", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const upgrading: Cluster = {
      ...web,
      talos: {
        version: "v1.15.1",
        image: "v1.14.4",
        upgrading: "web-cp-abc to v1.15.1 since 2026-10-23T10:00:00Z",
        upgraded: "web-cp-xyz to v1.15.1 at 2026-10-23T09:50:00Z",
        failed: "web-md-1 to v1.15.1 at 2026-10-23T09:40:00Z: the node did not come back",
      },
    };
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: upgrading },
      "DELETE /api/clusters/clusters/web/talos-upgrade-failure": { status: 204 },
      "GET /api/clusters/clusters/web/talosconfig": { text: "context: web\n" },
    });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockReturnValue(undefined);
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await waitFor(() => expect(term("Talos release")).toHaveTextContent("v1.15.1"));
    expect(term("New nodes boot")).toHaveTextContent("v1.14.4, then v1.15.1");
    expect(term("Talos upgrade")).toHaveTextContent(`web-cp-abc to v1.15.1, since ${shown("2026-10-23T10:00:00Z")}`);
    expect(term("Last Talos upgrade")).toHaveTextContent(`web-cp-xyz to v1.15.1, ${shown("2026-10-23T09:50:00Z")}`);
    const failure = within(screen.getByRole("region", { name: "Failed Talos upgrade" }));
    expect(
      failure.getByText(/^Couldn't upgrade web-md-1 to Talos v1\.15\.1\. The other nodes wait\./u),
    ).toBeInTheDocument();
    expect(failure.getByText(/: the node did not come back$/u)).toBeInTheDocument();
    const before = reads(calls, "GET /api/clusters/clusters/web");
    await user.click(failure.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(reads(calls, "DELETE /api/clusters/clusters/web/talos-upgrade-failure")).toBe(1));
    await waitFor(() => expect(reads(calls, "GET /api/clusters/clusters/web")).toBeGreaterThan(before));

    await user.click(screen.getByRole("button", { name: "Talosconfig" }));
    await waitFor(() => expect(click).toHaveBeenCalledWith());
    expect(reads(calls, "GET /api/clusters/clusters/web/talosconfig")).toBe(1);

    await user.click(screen.getByRole("link", { name: "Machines" }));
    const row = within(await screen.findByRole("row", { name: /web-cp-abc/u }));
    expect(screen.getByRole("columnheader", { name: "Talos" })).toBeInTheDocument();
    expect(row.getAllByRole("cell").map((c) => c.textContent)).toContain("v1.15.1");
  });

  it("holds back a Talos release that does not run the cluster's Kubernetes", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: { ...web, talos: { version: "v1.14.4" } } },
      "GET /api/talos/releases": {
        body: {
          templates: ["v1.14.4"],
          releases: ["v1.15.1", "v1.14.6"],
          default: "v1.14.4",
          kubernetes: { "v1.15.1": ["1.37", "1.38"], "v1.14.6": ["1.35", "1.36"] },
        },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Upgrade Talos" }));
    const dialog = within(screen.getByRole("dialog", { name: "Upgrade Talos on web" }));
    await expect(dialog.findByRole("option", { name: "v1.15.1 (upgrade Kubernetes first)" })).resolves.toBeDisabled();
    expect(dialog.getByRole("option", { name: "v1.14.6" })).toBeEnabled();
    expect(dialog.getByText("v1.15.1 doesn't run Kubernetes 1.36. Upgrade Kubernetes first.")).toBeInTheDocument();
  });

  it("holds a Kubernetes version back that the cluster's Talos release does not run", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: { ...web, talos: { version: "v1.14.4" } } },
      "GET /api/versions/talos": { body: { versions: ["v1.37.1", "v1.36.5"], stable: "v1.36.5" } },
      "GET /api/talos/releases": { body: releases },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Upgrade" }));
    const dialog = within(screen.getByRole("dialog", { name: "Upgrade web" }));
    await expect(dialog.findByRole("option", { name: "v1.37.1 (needs newer Talos)" })).resolves.toBeDisabled();
    expect(dialog.getByText(/^Talos v1\.14\.4 does not run v1\.37\.1: upgrade Talos first/u)).toBeInTheDocument();
  });

  it("waits for the Talos releases before offering a field, and names a typed one's mistake once", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/options");
    const releasesRead = held();
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: web },
      "GET /api/talos/releases": releasesRead.route,
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await user.click(await screen.findByRole("button", { name: "Upgrade Talos" }));
    const dialog = within(screen.getByRole("dialog", { name: "Upgrade Talos on web" }));
    expect(dialog.getByLabelText("Talos release")).toBeDisabled();
    expect(dialog.getByRole("option", { name: "Loading…" })).toBeInTheDocument();
    releasesRead.fail(new Error("lost"));
    const field = await dialog.findByRole("textbox", { name: "Talos release" });
    await user.clear(field);
    await user.type(field, "v1.16");
    expect(dialog.getByText("Enter a release such as v1.14.4")).toBeInTheDocument();
    expect(dialog.queryByText("Such as v1.14.4")).not.toBeInTheDocument();
  });

  it("restores a Talos cluster's etcd through its Talos API", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web/backups");
    const snapshot = {
      name: "20261023T060000Z-v1.36.5.db.age",
      time: "2026-10-23T06:00:00Z",
      kubernetesVersion: "v1.36.5",
    };
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: web },
      "GET /api/clusters/clusters/web/backups": {
        body: { enabled: false, reachable: true, backups: [], restores: [], canBackUp: true, canRestore: true },
      },
      "GET /api/clusters/clusters/web/etcd-snapshots": {
        body: {
          enabled: false,
          requested: false,
          snapshots: [snapshot],
          restoring: false,
          automatic: true,
          canSnapshot: true,
          canRestore: true,
        },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByText("etcd snapshots are off")).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: `Restore from ${snapshot.name}` }));
    const dialog = screen.getByRole("dialog", { name: /^Restore etcd from/u });
    expect(dialog).toHaveTextContent("Wipes etcd and reboots every control-plane node.");
    expect(dialog).toHaveTextContent("The API is down until the first one is back.");
    expect(dialog).not.toHaveTextContent("guest agent");
    expect(dialog).not.toHaveTextContent("Replaces all control-plane nodes");
  });

  it("holds Try again and the talosconfig to what the user may do", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    fakeServer({
      "GET /api/session": { body: { user: "viewer@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": {
        body: {
          ...web,
          allowed: { change: false, kubeconfig: false },
          talos: { version: "v1.15.1", failed: "web-md-1 to v1.15.1 at 2026-10-23T09:40:00Z: no answer" },
        },
      },
    });
    render(<App />);
    await expect(screen.findByRole("button", { name: "Try again" })).resolves.toBeDisabled();
    expect(screen.queryByRole("button", { name: "Talosconfig" })).not.toBeInTheDocument();
  });
});
