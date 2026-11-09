import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { fakeServer } from "./fake-server";
import { Templates, buildProblems, buildRequest } from "./Templates";

const form = {
  distribution: "kubeadm" as const,
  nodeOS: "",
  kubernetesVersion: "v1.37.1",
  talosVersion: "",
  node: "pve1",
  storage: "ceph",
  importStorage: "local",
  isoStorage: "",
  bridge: "vmbr0",
  vlan: "",
  buildAddress: "",
  buildGateway: "",
  buildDNS: "",
  cpuType: "",
  ssd: "" as const,
  discard: "" as const,
  ioThread: "" as const,
  numa: "" as const,
};

describe("vM templates", () => {
  it("checks and builds the request", () => {
    expect(buildProblems(form)).toStrictEqual({});
    expect(buildProblems({ ...form, kubernetesVersion: "1.37" })).toStrictEqual({
      kubernetesVersion: "Enter a version such as v1.36.5",
    });
    expect(buildProblems({ ...form, buildAddress: "192.0.2.50/24" })).toStrictEqual({
      buildAddress: "Enter the prefix length, a gateway and a DNS server",
    });
    // Hardware left open is not sent, so the newest build's stands.
    expect(buildRequest(form)).not.toHaveProperty("cpuType");
    expect(buildRequest(form)).not.toHaveProperty("ssd");
    expect(
      buildRequest({ ...form, cpuType: " host ", ssd: "off", discard: "on", ioThread: "on", numa: "off" }),
    ).toMatchObject({ cpuType: "host", ssd: false, discard: true, ioThread: true, numa: false });
    expect(buildProblems({ ...form, cpuType: "x86 64" })).toStrictEqual({
      cpuType: "Enter a CPU type such as host or x86-64-v3",
    });
    expect(buildRequest({ ...form, distribution: "k3s", nodeOS: "rocky-10", vlan: "30" })).toStrictEqual({
      distribution: "k3s",
      nodeOS: "rocky-10",
      node: "pve1",
      storage: "ceph",
      importStorage: "local",
      bridge: "vmbr0",
      vlan: 30,
    });
    expect(
      buildRequest({
        ...form,
        buildAddress: "192.0.2.50/24",
        buildGateway: "192.0.2.1",
        buildDNS: "192.0.2.1, 192.0.2.2",
      }),
    ).toMatchObject({
      kubernetesVersion: "v1.37.1",
      buildAddress: "192.0.2.50/24",
      buildGateway: "192.0.2.1",
      buildDNS: ["192.0.2.1", "192.0.2.2"],
    });
    // A Talos image is not booted to be built, so it takes none of what that VM needs, and Talos is
    // its own OS.
    const talos = { ...form, distribution: "talos" as const, nodeOS: "almalinux-10", talosVersion: "v1.14" };
    expect(buildProblems(talos)).toStrictEqual({ talosVersion: "Enter a release such as v1.14.4" });
    expect(
      buildRequest({
        ...talos,
        talosVersion: "v1.14.4",
        vlan: "30",
        isoStorage: "local",
        buildAddress: "192.0.2.50/24",
      }),
    ).toStrictEqual({
      distribution: "talos",
      talosVersion: "v1.14.4",
      node: "pve1",
      storage: "ceph",
      importStorage: "local",
      bridge: "vmbr0",
    });
  });

  it("makes a Talos release's template, offering the releases the Image Factory builds", async () => {
    const calls = fakeServer({
      "GET /api/templates": {
        body: {
          templates: [
            {
              vmid: 9002,
              node: "pve1",
              name: "kubehenge-talos-v1.14.4",
              line: "kubehenge-talos-v1.14.4",
              tags: ["kubehenge", "talos", "talos-v1.14.4"],
            },
          ],
        },
      },
      "GET /api/talos/releases": {
        body: { templates: ["v1.14.4"], releases: ["v1.15.1", "v1.14.4"], default: "v1.14.4", kubernetes: {} },
      },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
      "GET /api/proxmox/storages": {
        body: [
          { storage: "local", node: "pve1", shared: false, content: ["import"], avail: 0, total: 0, usable: false },
          { storage: "ceph", node: "pve1", shared: true, content: ["images"], avail: 0, total: 0, usable: true },
        ],
      },
      "GET /api/proxmox/nodes/pve1/bridges": {
        body: [{ bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true }],
      },
      "POST /api/templates": { status: 202 },
    });
    const user = userEvent.setup({ delay: null });
    render(<Templates />);
    // A Talos template is made once, never rebuilt.
    expect(
      within(await screen.findByRole("row", { name: /kubehenge-talos-v1\.14\.4/u })).getByText("Made once"),
    ).toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Build template" }));
    const dialog = within(screen.getByRole("dialog", { name: "Build template" }));
    await user.selectOptions(dialog.getByLabelText("Distribution"), "talos");
    // The default release has its template already, so the newest without one is chosen.
    await waitFor(() => expect(dialog.getByLabelText("Talos release")).toHaveValue("v1.15.1"));
    expect(
      within(dialog.getByLabelText("Talos release"))
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toStrictEqual(["v1.15.1", "v1.14.4 (default, has a template)"]);
    expect(dialog.queryByLabelText("Cloud-init image")).not.toBeInTheDocument();
    expect(dialog.queryByText("Static address")).not.toBeInTheDocument();
    await waitFor(() => expect(dialog.getByLabelText("Bridge")).toHaveValue("vmbr0"));
    await user.click(dialog.getByRole("button", { name: "Build" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "POST /api/templates")?.body).toStrictEqual({
        distribution: "talos",
        talosVersion: "v1.15.1",
        node: "pve1",
        storage: "ceph",
        importStorage: "local",
        bridge: "vmbr0",
      }),
    );
  });

  it("lists templates, keeps those in use and builds a new one", async () => {
    const calls = fakeServer({
      "GET /api/templates": {
        body: {
          templates: [
            {
              vmid: 9001,
              node: "pve1",
              name: "kubehenge-rke2-almalinux-10-build-202610110100",
              line: "kubehenge-rke2-almalinux-10",
              build: "build-202610110100",
              built: "2026-10-11T01:00:00Z",
              newest: true,
              tags: ["kubehenge", "rke2", "os-almalinux-10", "build-202610110100"],
              inUse: "default/web",
              nodeOS: "AlmaLinux 10",
              hardware: { cpuType: "x86-64-v2-AES", ssd: true, discard: true, ioThread: false, numa: false },
            },
            {
              vmid: 9002,
              node: "pve1",
              name: "kubehenge-kubeadm-v1.36.5-rocky-10-build-202610040100",
              line: "kubehenge-kubeadm-v1.36.5-rocky-10",
              build: "build-202610040100",
              built: "2026-10-04T01:00:00Z",
              tags: ["kubehenge", "kubeadm", "os-rocky-10", "kubernetes-v1.36.5", "build-202610040100"],
              nodeOS: "Rocky Linux 10",
            },
            { vmid: 9003, node: "pve1", name: "kubehenge-talos-v1.12.0", line: "kubehenge-talos-v1.12.0", tags: [] },
          ],
          nodeOSes: [
            {
              family: "almalinux",
              release: "10",
              title: "AlmaLinux 10",
              cpuType: "x86-64-v2-AES",
              mac: "selinux",
              default: true,
            },
            {
              family: "almalinux",
              release: "10",
              fips: true,
              title: "AlmaLinux 10 FIPS",
              cpuType: "x86-64-v2-AES",
              mac: "selinux",
            },
            { family: "rocky", release: "10", title: "Rocky Linux 10", cpuType: "x86-64-v3", mac: "selinux" },
            {
              family: "leap",
              release: "16.1",
              title: "openSUSE Leap 16.1",
              cpuType: "x86-64-v2-AES",
              mac: "selinux",
              unreleased: "not released yet",
            },
          ],
          job: {
            user: "root@pam",
            kind: "template",
            started: new Date().toISOString(),
            state: "failed",
            error: "no import storage",
            log: ["Building"],
          },
        },
      },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
      "GET /api/proxmox/storages": {
        body: [
          {
            storage: "local",
            node: "pve1",
            shared: false,
            content: ["import", "iso"],
            avail: 0,
            total: 0,
            usable: false,
          },
          { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 0, total: 0, usable: true },
          { storage: "ceph", node: "pve1", shared: true, content: ["images"], avail: 0, total: 0, usable: true },
        ],
      },
      "GET /api/proxmox/nodes/pve1/bridges": {
        body: [{ bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true }],
      },
      "POST /api/templates": { status: 202 },
    });
    const sent = (): unknown => calls.find((c) => c.key === "POST /api/templates")?.body;
    const user = userEvent.setup({ delay: null });
    render(<Templates />);

    const inUse = within(await screen.findByRole("row", { name: /kubehenge-rke2-almalinux-10 \(9001\)/u }));
    expect(inUse.getByText(/\(newest\)/u)).toBeInTheDocument();
    expect(inUse.getByRole("cell", { name: "AlmaLinux 10" })).toBeInTheDocument();
    expect(inUse.getByRole("cell", { name: "x86-64-v2-AES, SSD, discard" })).toBeInTheDocument();
    expect(inUse.getByRole("button", { name: "Delete kubehenge-rke2-almalinux-10-build-202610110100" })).toBeDisabled();
    const kubeadm = within(screen.getByRole("row", { name: /kubehenge-kubeadm-v1.36.5-rocky-10 \(9002\)/u }));
    expect(kubeadm.getByRole("cell", { name: "Rocky Linux 10" })).toBeInTheDocument();
    expect(kubeadm.getByText("Not recorded")).toBeInTheDocument();
    const talos = within(screen.getByRole("row", { name: /kubehenge-talos-v1.12.0 \(9003\)/u }));
    expect(talos.getByRole("cell", { name: "Talos Linux" })).toBeInTheDocument();
    expect(screen.getByText("Build by root@pam failed")).toBeInTheDocument();
    expect(screen.getByText("no import storage")).toBeInTheDocument();
    // The weekly build is off on this appliance.
    expect(within(screen.getByRole("region", { name: "Automatic builds" })).getByText(/^Off\./u)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Build template" }));
    const dialog = screen.getByRole("dialog", { name: "Build template" });
    const distributions = within(within(dialog).getByLabelText("Distribution"))
      .getAllByRole("option")
      .map((o) => o.textContent);
    expect(distributions).toStrictEqual(["RKE2", "k3s", "kubeadm", "Talos"]);
    // The default node OS comes chosen. One not released yet is shown but cannot be chosen.
    const nodeOS = within(dialog).getByLabelText("Node OS");
    expect(nodeOS).toHaveValue("almalinux-10");
    expect(within(nodeOS).getByRole("option", { name: "openSUSE Leap 16.1: not released yet" })).toBeDisabled();
    // FIPS mode is offered for RKE2 alone. A distribution without it goes back to the default.
    await user.selectOptions(nodeOS, "almalinux-10-fips");
    await user.selectOptions(within(dialog).getByLabelText("Distribution"), "k3s");
    expect(within(dialog).getByLabelText("Node OS")).toHaveValue("almalinux-10");
    expect(
      within(within(dialog).getByLabelText("Node OS")).queryByRole("option", { name: /FIPS/u }),
    ).not.toBeInTheDocument();
    await user.selectOptions(within(dialog).getByLabelText("Distribution"), "rke2");
    await user.selectOptions(within(dialog).getByLabelText("Node OS"), "rocky-10");
    expect(nodeOS).toHaveAccessibleDescription("Needs Proxmox VE nodes with x86-64-v3 CPUs");
    // Talos is its own OS, so it offers none. The choice stays for the others.
    await user.selectOptions(within(dialog).getByLabelText("Distribution"), "talos");
    expect(within(dialog).queryByLabelText("Node OS")).not.toBeInTheDocument();
    await user.selectOptions(within(dialog).getByLabelText("Distribution"), "kubeadm");
    expect(within(dialog).getByLabelText("Node OS")).toHaveValue("rocky-10");
    await user.type(within(dialog).getByLabelText("Kubernetes version"), "v1.37.1");
    await waitFor(() => expect(within(dialog).getByRole("option", { name: "ceph" })).toBeInTheDocument());
    // Shared storage comes first.
    const disks = within(within(dialog).getByLabelText("Template disk"))
      .getAllByRole("option")
      .map((o) => o.textContent);
    expect(disks).toStrictEqual(["Choose…", "ceph", "local-lvm"]);
    await user.selectOptions(within(dialog).getByLabelText("Template disk"), "ceph");
    await user.selectOptions(within(dialog).getByLabelText("Downloaded image"), "local");
    await waitFor(() => expect(within(dialog).getByRole("option", { name: "vmbr0" })).toBeInTheDocument());
    // The node's only bridge is chosen, and nothing untouched is flagged.
    expect(within(dialog).getByLabelText("Bridge")).toHaveValue("vmbr0");
    expect(within(dialog).getByLabelText("Bridge")).not.toHaveAttribute("aria-invalid");
    // Hardware keeps the newest build's unless set.
    await user.type(within(dialog).getByLabelText("CPU type"), "host");
    await user.selectOptions(within(dialog).getByLabelText("NUMA"), "on");
    expect(within(dialog).getByLabelText("SSD")).toHaveValue("");
    await user.click(within(dialog).getByRole("button", { name: "Build" }));
    await waitFor(() =>
      expect(sent()).toStrictEqual({
        distribution: "kubeadm",
        kubernetesVersion: "v1.37.1",
        nodeOS: "rocky-10",
        node: "pve1",
        storage: "ceph",
        importStorage: "local",
        bridge: "vmbr0",
        cpuType: "host",
        numa: true,
      }),
    );
  });

  it("says which templates are on a node that is down and lists the others", async () => {
    fakeServer({
      "GET /api/release-pins": { body: { pins: [], checks: false } },
      "GET /api/templates": {
        body: {
          templates: [
            {
              vmid: 9002,
              node: "pve1",
              name: "kubehenge-talos-v1.14.4",
              line: "kubehenge-talos-v1.14.4",
              tags: ["kubehenge", "talos", "talos-v1.14.4"],
            },
          ],
          down: [
            { vmid: 9005, node: "pve2", name: "kubehenge-rke2-almalinux-10-build-202610150000" },
            { vmid: 9006, node: "pve3" },
          ],
        },
      },
    });
    render(<Templates />);
    const first = "Template kubehenge-rke2-almalinux-10-build-202610150000 is on node pve2, which is down.";
    await waitFor(() => expect(screen.getByText(first)).toBeInTheDocument());
    expect(screen.getByText("Template VM 9006 is on node pve3, which is down.")).toBeInTheDocument();
    expect(screen.getByRole("row", { name: /kubehenge-talos-v1\.14\.4/u })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    // On a phone the table scrolls inside its card. Outside a scroller it widens the whole page.
    expect(screen.getByRole("table").closest(".scroll")).not.toBeNull();
  });

  it("shows the weekly build and runs it now", async () => {
    const refresh = {
      weekday: "Sun",
      time: "01:00",
      timeZone: "Europe/Zurich",
      soakHours: 24,
      creatorBuilds: false,
      next: "2026-10-25T00:00:00Z",
      requested: [] as string[],
      last: {
        state: "failed",
        trigger: "schedule",
        finished: "2026-10-18T00:20:00Z",
        lines: ["kubehenge-rke2-almalinux-10"],
        error: "the mirror is down",
        log: ["building", "failed"],
      },
    };
    const calls = fakeServer({
      "GET /api/templates": () => ({ body: { templates: [], refresh } }),
      "POST /api/templates/refresh": () => {
        refresh.requested = ["refresh"];
        return { status: 202, body: refresh };
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<Templates />);
    const builds = within(await screen.findByRole("region", { name: "Automatic builds" }));
    expect(builds.getByText("Every Sunday, 01:00 Europe/Zurich")).toBeInTheDocument();
    expect(builds.getByText("After 24 h, in upgrade windows")).toBeInTheDocument();
    expect(builds.getByText(/^Last run failed, /u)).toBeInTheDocument();
    expect(builds.getByText("the mirror is down")).toBeInTheDocument();
    await user.click(builds.getByRole("button", { name: "Build now" }));
    await waitFor(() => expect(builds.getByRole("button", { name: "Build now" })).toBeDisabled());
    expect(builds.getByText("Starting within a minute")).toBeInTheDocument();
    expect(calls.filter((c) => c.key === "POST /api/templates/refresh")).toHaveLength(1);
  });

  it("says when the template disk is on storage that is not shared", async () => {
    fakeServer({
      "GET /api/templates": { body: { templates: [] } },
      "GET /api/release-pins": { body: { pins: [], checks: false } },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
      "GET /api/proxmox/storages": {
        body: [
          { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 0, total: 0, usable: true },
        ],
      },
      "GET /api/proxmox/nodes/pve1/bridges": { body: [] },
    });
    const user = userEvent.setup({ delay: null });
    render(<Templates />);
    await user.click(await screen.findByRole("button", { name: "Build template" }));
    const dialog = within(screen.getByRole("dialog", { name: "Build template" }));
    await waitFor(() => expect(dialog.getByText("Clones start only on pve1")).toBeInTheDocument());
  });

  it("marks a build whose kernel is older than the previous build's", async () => {
    fakeServer({
      "GET /api/templates": {
        body: {
          templates: [
            {
              vmid: 9004,
              node: "pve1",
              name: "kubehenge-rke2-almalinux-10-build-202610150000",
              line: "kubehenge-rke2-almalinux-10",
              tags: ["kubehenge", "rke2"],
              built: "2026-10-15T00:00:00Z",
              newest: true,
              older: true,
            },
            {
              vmid: 9001,
              node: "pve1",
              name: "kubehenge-rke2-almalinux-10-build-202610010000",
              line: "kubehenge-rke2-almalinux-10",
              tags: ["kubehenge", "rke2"],
              built: "2026-10-01T00:00:00Z",
            },
          ],
        },
      },
      "GET /api/release-pins": { body: { pins: [], checks: false } },
    });
    render(<Templates />);
    await expect(screen.findByText("Older than the previous build")).resolves.toBeInTheDocument();
    expect(screen.getAllByText("Older than the previous build")).toHaveLength(1);
  });

  it("shows a release's digests before pinning it, and unpins one no cluster runs", async () => {
    const digests = { version: "v1.36.6+k3s1", file: "a".repeat(64), script: "b".repeat(64) };
    const calls = fakeServer({
      "GET /api/templates": { body: { templates: [] } },
      "GET /api/release-pins": {
        body: {
          pins: [
            {
              version: "v1.36.5+rke2r1",
              file: "c".repeat(64),
              script: "d".repeat(64),
              by: "admin@pve",
              inUse: "clusters/web",
            },
            { version: "v1.36.4+rke2r1", file: "e".repeat(64), script: "f".repeat(64), by: "admin@pve" },
          ],
          checks: true,
        },
      },
      "POST /api/release-pins/check": { body: digests },
      "POST /api/release-pins": { status: 201, body: digests },
      "DELETE /api/release-pins/v1.36.4%2Brke2r1": { status: 204 },
    });
    const user = userEvent.setup({ delay: null });
    render(<Templates />);
    await expect(screen.findByRole("button", { name: "Unpin v1.36.5+rke2r1" })).resolves.toBeDisabled();
    await user.type(screen.getByLabelText("Release"), "v1.36.6+k3s1");
    await user.click(screen.getByRole("button", { name: "Check release" }));
    await expect(screen.findByText("a".repeat(64))).resolves.toBeInTheDocument();
    expect(screen.getByText("b".repeat(64))).toBeInTheDocument();
    expect(calls.map((c) => c.key)).not.toContain("POST /api/release-pins");
    await user.click(screen.getByRole("button", { name: "Pin v1.36.6+k3s1" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "POST /api/release-pins")?.body).toStrictEqual({
        version: "v1.36.6+k3s1",
        file: "a".repeat(64),
        script: "b".repeat(64),
      }),
    );
    await user.click(screen.getByRole("button", { name: "Unpin v1.36.4+rke2r1" }));
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("DELETE /api/release-pins/v1.36.4%2Brke2r1"));
  });
});
