import { act, render, screen, waitFor, within, type BoundFunctions, type queries } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { cleanTargets, targetProblems } from "./BackupTargets";
import { fakeServer, reads, type Route } from "./fake-server";
import { formOf, settings, setupFieldProblems, setupProblems, type SetupForm } from "./Setup";

const form: SetupForm = {
  nodes: ["pve1"],
  imageDatastore: "local",
  vmDatastore: "local-lvm",
  bridge: "vmbr0",
  vlan: "",
  nodeAddresses: "192.0.2.11/24",
  apiAddress: "192.0.2.10",
  gateway: "192.0.2.1",
  dnsServers: "192.0.2.1, 1.1.1.1",
  workloadDatastores: ["local-lvm"],
  workloadBridges: ["vmbr0"],
  bootstrapDatastore: "local",
  mirrors: "",
  podSubnets: "",
  serviceSubnets: "",
  clusterOIDC: [],
  proxmoxHA: false,
  offerHARules: false,
  clusterDatastoreAllocate: false,
  backupTargets: [],
  etcdRestore: false,
  refresh: true,
  refreshWeekday: "Sun",
  refreshTime: "01:00",
  refreshTimeZone: "UTC",
  refreshStorage: "",
  refreshAddress: "",
  refreshGateway: "",
  refreshDNS: "",
  creatorBuilds: false,
  clusterSysAudit: true,
  clusterVolumeSnapshots: false,
  rootPassword: "",
  moreNamespaces: false,
  extraNamespaces: "",
  headlampSignIn: false,
  headlampActions: false,
  passphrase: "correct horse battery staple",
  confirm: "correct horse battery staple",
};

const administrator = { "GET /api/session": { body: { user: "root@pam", csrf: "c1" } } };

// What the Administration page asks for besides the setup status.
const administration = {
  "GET /api/appliance/upgrades": {
    body: { version: "0.1.0", available: [], settings: { policy: "off", hour: 3, timeZone: "UTC" } },
  },
  "GET /api/templates": { body: { templates: [] } },
};

const forClusters = (group: string): BoundFunctions<typeof queries> =>
  within(screen.getByRole("group", { name: group }));

// withJob serves a signed-in administrator of an appliance whose last run is job.
const withJob = (job: unknown): void => {
  fakeServer({
    ...administrator,
    "GET /api/setup": { body: { appliance: true, ready: false, allowed: true, job } },
    "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
    "GET /api/proxmox/storages": { body: [] },
    "GET /api/proxmox/nodes/pve1/bridges": { body: [] },
  });
};

// certificateIn serves an appliance whose certificate runs out in this many days.
const certificateIn = (days: number): void => {
  fakeServer({
    ...administrator,
    "GET /api/setup": {
      body: {
        appliance: true,
        ready: true,
        allowed: true,
        applianceVMID: 105,
        certificateExpires: new Date(Date.now() + days * 86_400_000).toISOString(),
      },
    },
    "GET /api/clusters": { body: [] },
  });
};

describe("setup", () => {
  it("builds the settings", () => {
    expect(settings({ ...form, vlan: "30" })).toStrictEqual({
      nodes: ["pve1"],
      imageDatastore: "local",
      vmDatastore: "local-lvm",
      bridge: "vmbr0",
      vlan: 30,
      nodeAddresses: ["192.0.2.11/24"],
      apiAddress: "192.0.2.10",
      gateway: "192.0.2.1",
      dnsServers: ["192.0.2.1", "1.1.1.1"],
      workloadDatastores: ["local-lvm"],
      workloadBridges: ["vmbr0"],
      bootstrapDatastore: "local",
      templateRefresh: { enabled: true, weekday: "Sun", time: "01:00", timeZone: "UTC" },
    });
  });

  it("builds the templates again every week unless switched off", () => {
    const { templateRefresh: _, ...unnamed } = settings(form);
    expect(formOf(unnamed)).toHaveProperty("refresh", true);
    expect(settings({ ...form, refresh: false }).templateRefresh).toStrictEqual({ enabled: false });
    const saved = settings({
      ...form,
      refreshWeekday: "Wed",
      refreshStorage: "local-lvm",
      refreshAddress: "192.0.2.50/24",
      refreshGateway: "192.0.2.1",
      refreshDNS: "192.0.2.1, 1.1.1.1",
      creatorBuilds: true,
    });
    expect(saved.templateRefresh).toStrictEqual({
      enabled: true,
      weekday: "Wed",
      time: "01:00",
      timeZone: "UTC",
      storage: "local-lvm",
      address: "192.0.2.50/24",
      gateway: "192.0.2.1",
      dns: ["192.0.2.1", "1.1.1.1"],
      creatorBuilds: true,
    });
    expect(formOf(saved)).toMatchObject({
      refreshWeekday: "Wed",
      creatorBuilds: true,
    });
    expect(setupFieldProblems({ ...form, refreshGateway: "192.0.2.1" })).toHaveProperty("refreshAddress");
    expect(setupFieldProblems({ ...form, refreshAddress: "192.0.2.50" })).toHaveProperty("refreshAddress");
    const outside = setupFieldProblems({
      ...form,
      refreshAddress: "192.0.2.50/24",
      refreshGateway: "198.51.100.1",
      refreshDNS: "dns",
    });
    expect([outside.refreshGateway, outside.refreshDNS]).toStrictEqual([
      "An IPv4 address in the address's network.",
      "IP addresses.",
    ]);
    expect(setupFieldProblems({ ...form, refresh: false, refreshGateway: "x" })).not.toHaveProperty("refreshAddress");
  });

  it("reads registry mirrors and ranges", () => {
    const s = settings({
      ...form,
      mirrors: "ghcr.io=http://10.0.0.1:5000\ndocker.io = https://a\ndocker.io=https://b\n",
      serviceSubnets: "172.20.0.0/16",
    });
    expect(s.registryMirrors).toStrictEqual({
      "ghcr.io": ["http://10.0.0.1:5000"],
      "docker.io": ["https://a", "https://b"],
    });
    expect(s.serviceSubnets).toStrictEqual(["172.20.0.0/16"]);
    expect(s.podSubnets).toBeUndefined();
  });

  it("asks for Proxmox VE HA only when it is switched on", () => {
    expect(settings({ ...form, proxmoxHA: true })).toHaveProperty("proxmoxHA", true);
    expect(settings(form)).not.toHaveProperty("proxmoxHA");
    expect([formOf(settings({ ...form, proxmoxHA: true })).proxmoxHA, formOf(settings(form)).proxmoxHA]).toStrictEqual([
      true,
      false,
    ]);
  });

  it("offers the HA rules to cluster creators only when an administrator says so", () => {
    expect(settings({ ...form, offerHARules: true })).toHaveProperty("offerHARules", true);
    expect(settings(form)).not.toHaveProperty("offerHARules");
    expect([
      formOf(settings({ ...form, offerHARules: true })).offerHARules,
      formOf(settings(form)).offerHARules,
    ]).toStrictEqual([true, false]);
  });

  it("lets clusters choose the CSI plugin's newest release only when it is switched on", () => {
    const on = settings({ ...form, clusterDatastoreAllocate: true });
    expect(on).toHaveProperty("clusterDatastoreAllocate", true);
    expect(settings(form)).not.toHaveProperty("clusterDatastoreAllocate");
    expect([formOf(on).clusterDatastoreAllocate, formOf(settings(form)).clusterDatastoreAllocate]).toStrictEqual([
      true,
      false,
    ]);
  });

  it("gives clusters the HA group labels unless it is switched off", () => {
    const off = settings({ ...form, clusterSysAudit: false });
    expect(off).toHaveProperty("clusterSysAudit", false);
    expect(settings(form)).not.toHaveProperty("clusterSysAudit");
    expect([formOf(settings(form)).clusterSysAudit, formOf(off).clusterSysAudit]).toStrictEqual([true, false]);
  });

  it("names more namespaces for clusters only when they are switched on", () => {
    const on = { ...form, moreNamespaces: true, extraNamespaces: "team-a, team-b" };
    expect(settings(on).extraClusterNamespaces).toStrictEqual(["team-a", "team-b"]);
    expect(settings({ ...on, moreNamespaces: false })).not.toHaveProperty("extraClusterNamespaces");
    expect(settings(form)).not.toHaveProperty("extraClusterNamespaces");
    const back = formOf(settings(on));
    expect([back.moreNamespaces, back.extraNamespaces, formOf(settings(form)).moreNamespaces]).toStrictEqual([
      true,
      "team-a, team-b",
      false,
    ]);
  });

  it("refuses namespaces that cannot hold clusters", () => {
    const problem = (extraNamespaces: string): string | undefined =>
      setupFieldProblems({ ...form, moreNamespaces: true, extraNamespaces }).extraNamespaces;
    expect(problem("team-a, team-b")).toBeUndefined();
    expect(problem("")).toMatch(/at least one namespace/u);
    expect(problem("team-a, kubehenge-system")).toMatch(/kubehenge-system holds/u);
    expect(problem("kube-public")).toMatch(/kube-public holds/u);
    expect(problem("clusters")).toMatch(/interface's own namespace/u);
    expect(problem("Team_A")).toMatch(/no namespace name/u);
    expect(problem("team-a, team-a")).toMatch(/named twice/u);
    expect(setupFieldProblems({ ...form, extraNamespaces: "kube-system" }).extraNamespaces).toBeUndefined();
  });

  it("asks for restores through the guest agent only when they are switched on", () => {
    expect(settings({ ...form, etcdRestore: true })).toHaveProperty("etcdRestore", true);
    expect(settings(form)).not.toHaveProperty("etcdRestore");
    expect([
      formOf(settings({ ...form, etcdRestore: true })).etcdRestore,
      formOf(settings(form)).etcdRestore,
    ]).toStrictEqual([true, false]);
  });

  it("asks for the Headlamp sign-in only when it is switched on", () => {
    expect(settings({ ...form, headlampSignIn: true })).toHaveProperty("headlampSignIn", true);
    expect(settings(form)).not.toHaveProperty("headlampSignIn");
    expect([
      formOf(settings({ ...form, headlampSignIn: true })).headlampSignIn,
      formOf(settings(form)).headlampSignIn,
    ]).toStrictEqual([true, false]);
  });

  it("asks for the Headlamp actions only when they are switched on", () => {
    expect(settings({ ...form, headlampActions: true })).toHaveProperty("headlampActions", true);
    expect(settings(form)).not.toHaveProperty("headlampActions");
    expect([
      formOf(settings({ ...form, headlampActions: true })).headlampActions,
      formOf(settings(form)).headlampActions,
    ]).toStrictEqual([true, false]);
  });

  it("passes the clusters' identity providers on, empty fields left to the defaults", () => {
    const s = settings({
      ...form,
      clusterOIDC: [
        {
          issuerURL: " https://sso.example ",
          clientID: "kubernetes",
          usernameClaim: "",
          scopes: [],
          usernamePrefix: "",
        },
      ],
    });
    expect(s.clusterOIDC).toStrictEqual([
      { issuerURL: "https://sso.example", clientID: "kubernetes", usernamePrefix: "" },
    ]);
    expect(settings(form).clusterOIDC).toBeUndefined();
    const oidc = setupFieldProblems({
      ...form,
      clusterOIDC: [{ issuerURL: "http://sso.example", clientID: "" }],
    }).clusterOIDC;
    expect(oidc).toMatch(/https/u);
    expect(oidc).toMatch(/[Cc]lient/u);
  });

  it("finds what is missing", () => {
    expect(setupProblems(form)).toStrictEqual([]);
    expect(setupProblems({ ...form, nodeAddresses: "192.0.2.11/24\n192.0.2.12/24" })).toHaveLength(1);
    expect(setupProblems({ ...form, nodeAddresses: "192.0.2.11" })).toHaveLength(1);
    expect(setupProblems({ ...form, passphrase: "short", confirm: "short" })).toStrictEqual([
      "The passphrase needs at least 16 characters.",
    ]);
    expect(setupProblems({ ...form, confirm: "something else entirely" })).toStrictEqual(["The passphrases differ."]);
    expect(setupProblems({ ...form, nodes: [] })).toStrictEqual(["Choose at least one Proxmox VE node."]);
    expect(setupProblems({ ...form, nodes: ["pve1", "pve2"] })).toStrictEqual([
      "Choose no more Proxmox VE nodes than there are node addresses.",
    ]);
  });

  it("tells users without the rights that an administrator has to set it up", async () => {
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c1" } },
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: false } },
    });
    render(<App />);
    await expect(screen.findByText("Kubehenge is not set up yet")).resolves.toBeInTheDocument();
  });

  // Enter in the passphrase submits as the button does: the wizard is a form, which is also what
  // lets password managers keep the new passphrase.
  it.each<[string, (user: UserEvent) => Promise<void>]>([
    ["the button", (user) => user.click(screen.getByRole("button", { name: "Create the management cluster" }))],
    ["Enter", (user) => user.keyboard("{Enter}")],
  ])("starts the run from %s and follows it", async (_how, submit) => {
    const job = {
      user: "root@pam",
      started: new Date().toISOString(),
      state: "running",
      log: ["Applying the management cluster"],
    };
    const routes: Record<string, Route> = {
      ...administrator,
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
      "GET /api/proxmox/storages": {
        body: [
          {
            storage: "local",
            node: "pve1",
            shared: false,
            content: ["iso", "import", "images"],
            avail: 0,
            total: 0,
            usable: true,
          },
          { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 0, total: 0, usable: true },
        ],
      },
      "GET /api/proxmox/nodes/pve1/bridges": {
        body: [{ bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true }],
      },
      "POST /api/setup": () => {
        routes["GET /api/setup"] = { body: { appliance: true, ready: false, allowed: true, job } };
        return { status: 202, body: job };
      },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<App />);

    // Asked of the one select rather than the whole form, whose size makes a page-wide role query
    // slower than waitFor's interval allows on a busy machine.
    const disks = await screen.findByLabelText("VM disks");
    await waitFor(() => expect(within(disks).getByRole("option", { name: "local-lvm" })).toBeInTheDocument());
    await user.selectOptions(disks, "local-lvm");
    const bridge = screen.getByLabelText("Bridge");
    await waitFor(() => expect(within(bridge).getByRole("option", { name: "vmbr0" })).toBeInTheDocument());
    await user.selectOptions(bridge, "vmbr0");
    await user.type(screen.getByLabelText("Node addresses"), "192.0.2.11/24");
    await user.type(screen.getByLabelText("API address"), "192.0.2.10");
    await user.type(screen.getByLabelText("Gateway"), "192.0.2.1");
    await user.type(screen.getByLabelText("DNS servers"), "192.0.2.1");
    await user.click(forClusters("Storages").getByRole("checkbox", { name: "local-lvm" }));
    await user.click(forClusters("Bridges").getByRole("checkbox", { name: "vmbr0" }));
    const signIn = screen.getByLabelText(/^Sign those who may download a cluster's kubeconfig in/u);
    expect(signIn).not.toBeChecked();
    expect(signIn).toHaveAccessibleDescription(/Grants: .* cluster-admin.* Risks: /u);
    await user.click(signIn);
    const actions = screen.getByLabelText("Offer Helm and Prometheus charts in the appliance's Headlamp");
    expect(actions).not.toBeChecked();
    expect(actions).toHaveAccessibleDescription(/Grants: .* own credentials.* Risks: /u);
    await user.click(actions);
    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.type(screen.getByLabelText("Repeat passphrase"), "correct horse battery staple");
    await submit(user);

    await expect(
      screen.findByRole("heading", { name: "Applying the management cluster" }),
    ).resolves.toBeInTheDocument();
    expect(screen.getByText("Now: Applying the management cluster")).toBeInTheDocument();
    expect(calls.find((c) => c.key === "POST /api/setup")?.body).toMatchObject({
      passphrase: "correct horse battery staple",
      settings: {
        nodes: ["pve1"],
        vmDatastore: "local-lvm",
        imageDatastore: "local",
        bootstrapDatastore: "local",
        workloadBridges: ["vmbr0"],
        headlampSignIn: true,
        headlampActions: true,
      },
    });
  });

  it("offers the management cluster only what every chosen node has, and clusters what any node has", async () => {
    fakeServer({
      ...administrator,
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
      "GET /api/proxmox/nodes": {
        body: [
          { node: "pve1", online: true },
          { node: "pve2", online: true },
        ],
      },
      "GET /api/proxmox/storages": {
        body: [
          { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 0, total: 0, usable: true },
          { storage: "local-lvm", node: "pve2", shared: false, content: ["images"], avail: 0, total: 0, usable: true },
          { storage: "fast", node: "pve1", shared: false, content: ["images"], avail: 0, total: 0, usable: true },
        ],
      },
      "GET /api/proxmox/nodes/pve1/bridges": {
        body: [
          { bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true },
          { bridge: "vmbr1", node: "pve1", vlanAware: false, usable: true },
        ],
      },
      "GET /api/proxmox/nodes/pve2/bridges": {
        body: [{ bridge: "vmbr0", node: "pve2", vlanAware: false, usable: true }],
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await waitFor(() =>
      expect(forClusters("Bridges").getByRole("checkbox", { name: "vmbr1" })).toHaveAccessibleDescription("(on pve1)"),
    );
    expect(forClusters("Bridges").getByRole("checkbox", { name: "vmbr0" })).toHaveAccessibleDescription(
      "(on pve1, pve2)",
    );
    expect(forClusters("Storages").getByRole("checkbox", { name: "fast" })).toHaveAccessibleDescription("(on pve1)");
    expect(screen.getByRole("option", { name: "fast" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "vmbr1" })).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText("VM disks"), "fast");
    await user.selectOptions(screen.getByLabelText("Bridge"), "vmbr1");

    await user.click(screen.getByRole("checkbox", { name: "pve2" }));
    expect(screen.queryByRole("option", { name: "fast" })).not.toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "vmbr1" })).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: "local-lvm" })).toBeInTheDocument();
    // What was chosen for pve1 alone is no longer chosen.
    expect(screen.getByLabelText("VM disks")).toHaveValue("");
    expect(screen.getByLabelText("Bridge")).toHaveValue("");
    // Clusters may still use what only one node has.
    expect(forClusters("Storages").getByRole("checkbox", { name: "fast" })).toBeInTheDocument();
  });

  it("checks addresses as the appliance does", () => {
    const problemsOf = (f: Partial<SetupForm>): ReturnType<typeof setupFieldProblems> =>
      setupFieldProblems({ ...form, ...f });
    expect(problemsOf({ gateway: "gw" }).gateway).toBe("An IPv4 address.");
    expect(problemsOf({ nodeAddresses: "192.0.2.0/24" }).nodeAddresses).toMatch(/network's own or broadcast/u);
    expect(problemsOf({ nodeAddresses: "198.51.100.11/24" }).nodeAddresses).toMatch(/outside the gateway's network/u);
    expect(
      problemsOf({ nodeAddresses: "192.0.2.11/24 192.0.2.11/24 192.0.2.12/24", nodes: ["pve1"] }).nodeAddresses,
    ).toMatch(/listed twice/u);
    expect(problemsOf({ apiAddress: "192.0.2.11" }).apiAddress).toMatch(/unused/u);
    expect(problemsOf({ apiAddress: "198.51.100.10" }).apiAddress).toBe("Inside the nodes' network.");
    expect(problemsOf({ dnsServers: "dns.example" }).dnsServers).toBe("IP addresses.");
    expect(problemsOf({ dnsServers: "2001:db8::53" }).dnsServers).toBeUndefined();
    expect(problemsOf({ vlan: "4095" }).vlan).toBe("1 to 4094.");
    expect(problemsOf({ podSubnets: "10.244.0.0/16, 10.245.0.1/16" }).podSubnets).toMatch(
      /^10.245.0.1\/16 is no IPv4/u,
    );
    expect(problemsOf({ mirrors: "ghcr.io=mirror:5000" }).mirrors).toMatch(/^ghcr.io=mirror:5000 is no/u);
    expect(problemsOf({ mirrors: "ghcr.io=https://mirror.example:5000/v2" }).mirrors).toBeUndefined();
    // Applying again opens the state with the passphrase given at the start: nothing to repeat.
    expect(setupFieldProblems({ ...form, confirm: "" }, false).confirm).toBeUndefined();
    // A running management cluster keeps its nodes' addresses, and grows only after them.
    const was = settings(form);
    expect(setupFieldProblems({ ...form, nodeAddresses: "192.0.2.12/24" }, false, was).nodeAddresses).toMatch(
      /keep their addresses \(192.0.2.11\/24\)/u,
    );
    expect(
      setupFieldProblems({ ...form, nodeAddresses: "192.0.2.11/24 192.0.2.12/24 192.0.2.13/24" }, false, was)
        .nodeAddresses,
    ).toBeUndefined();
  });

  it("follows a run that has logged nothing yet", async () => {
    withJob({ user: "root@pam", kind: "apply", started: new Date().toISOString(), state: "running", log: null });
    render(<App />);
    await expect(
      screen.findByRole("heading", { name: "Applying the management cluster" }),
    ).resolves.toBeInTheDocument();
  });

  it("shows a failed run without a log", async () => {
    withJob({
      user: "root@pam",
      kind: "remove",
      started: new Date().toISOString(),
      state: "failed",
      error: "wrong passphrase",
      log: null,
    });
    render(<App />);
    await expect(screen.findByText(/The last run failed: wrong passphrase/u)).resolves.toBeInTheDocument();
  });

  it("warns administrators of a certificate about to run out, and no one long before", async () => {
    certificateIn(10);
    const { unmount } = render(<App />);
    await expect(screen.findByText(/certificate expires on/u)).resolves.toBeInTheDocument();
    expect(screen.getByText("sh install.sh --certificate --ctid 105")).toBeInTheDocument();
    unmount();

    certificateIn(300);
    render(<App />);
    await expect(screen.findByRole("link", { name: "Administration" })).resolves.toBeInTheDocument();
    expect(screen.queryByText(/certificate expires on/u)).not.toBeInTheDocument();
  });

  it("shows administrators that the Proxmox VE API's certificate goes unverified, and nothing when it is checked", async () => {
    window.history.replaceState(null, "", "/administration");
    const routes: Record<string, Route> = {
      ...administrator,
      ...administration,
      "GET /api/setup": {
        body: { appliance: true, ready: true, allowed: true, applianceVMID: 105, proxmoxInsecure: true },
      },
    };
    fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    const { unmount } = render(<App />);
    const pages = await screen.findByRole("navigation", { name: "Administration's pages" });
    await user.click(within(pages).getByRole("link", { name: "Appliance" }));
    await expect(screen.findByRole("heading", { name: "Proxmox VE API" })).resolves.toBeInTheDocument();
    expect(window.location.pathname).toBe("/administration/appliance");
    expect(screen.getByText(/Certificate verification is off/u)).toBeInTheDocument();
    expect(screen.getByText("sh install.sh --upgrade --ctid 105 --proxmox-ca <file>")).toBeInTheDocument();
    unmount();

    routes["GET /api/setup"] = { body: { appliance: true, ready: true, allowed: true } };
    render(<App />);
    await expect(screen.findByRole("heading", { name: "Repair access" })).resolves.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Proxmox VE API" })).not.toBeInTheDocument();
  });

  it("asks about the setup every few seconds only while a run goes on", async () => {
    window.history.replaceState(null, "", "/administration");
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const routes: Record<string, Route> = {
      ...administrator,
      ...administration,
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: true } },
    };
    const calls = fakeServer(routes);
    render(<App />);
    await expect(screen.findByRole("heading", { name: "Management cluster" })).resolves.toBeInTheDocument();
    const idle = reads(calls, "GET /api/setup");
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(reads(calls, "GET /api/setup")).toBe(idle);
    routes["GET /api/setup"] = {
      body: {
        appliance: true,
        ready: true,
        allowed: true,
        job: { user: "root@pam", kind: "apply", started: new Date().toISOString(), state: "running", log: [] },
      },
    };
    await act(() => vi.advanceTimersByTimeAsync(20_000));
    expect(reads(calls, "GET /api/setup")).toBe(idle + 1);
    await act(() => vi.advanceTimersByTimeAsync(3000));
    expect(reads(calls, "GET /api/setup")).toBe(idle + 2);
  });

  it("shows the management cluster's settings and applies them again with the passphrase alone", async () => {
    window.history.replaceState(null, "", "/administration");
    const saved = {
      nodes: ["pve1"],
      imageDatastore: "local",
      vmDatastore: "local-lvm",
      bridge: "vmbr0",
      nodeAddresses: ["192.0.2.11/24"],
      apiAddress: "192.0.2.10",
      gateway: "192.0.2.1",
      dnsServers: ["192.0.2.1"],
      workloadDatastores: ["local-lvm"],
      workloadBridges: ["vmbr0"],
      bootstrapDatastore: "local",
    };
    const calls = fakeServer({
      ...administrator,
      ...administration,
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: true, settings: saved } },
      "POST /api/setup": {
        status: 202,
        body: { user: "root@pam", kind: "apply", started: new Date().toISOString(), state: "running", log: [] },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByRole("heading", { name: "Management cluster" })).resolves.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Administration" })).toHaveAttribute("aria-current", "page");
    expect(document.title).toBe("Administration - Kubehenge");
    expect(screen.getByText("192.0.2.11/24")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Apply again…" }));
    const dialog = screen.getByRole("dialog", { name: "Apply the management cluster again" });
    await user.type(within(dialog).getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(within(dialog).getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "POST /api/setup")?.body).toStrictEqual({
        settings: saved,
        passphrase: "correct horse battery staple",
      }),
    );

    await user.click(screen.getByRole("button", { name: "Edit…" }));
    const wizard = within(screen.getByRole("region", { name: "Management cluster" }));
    expect(wizard.getByLabelText("State passphrase")).toHaveAttribute("autocomplete", "current-password");
    expect(wizard.queryByLabelText("Repeat passphrase")).not.toBeInTheDocument();
    expect(wizard.getByLabelText("API address")).toHaveAttribute("readonly");
    expect(wizard.getByLabelText("VM disks")).toHaveValue("local-lvm");
    await user.click(wizard.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Apply again…" })).toBeInTheDocument();
  });

  it("switches restores through the guest agent on only once what they grant is confirmed", async () => {
    window.history.replaceState(null, "", "/administration");
    const saved = {
      nodes: ["pve1"],
      imageDatastore: "local",
      vmDatastore: "local-lvm",
      bridge: "vmbr0",
      nodeAddresses: ["192.0.2.11/24"],
      apiAddress: "192.0.2.10",
      gateway: "192.0.2.1",
      dnsServers: ["192.0.2.1"],
      workloadDatastores: ["local-lvm"],
      workloadBridges: ["vmbr0"],
      bootstrapDatastore: "local",
    };
    const calls = fakeServer({
      ...administrator,
      ...administration,
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: true, settings: saved } },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
      "GET /api/proxmox/storages": {
        body: [
          {
            storage: "local",
            node: "pve1",
            shared: false,
            content: ["iso", "import", "images"],
            avail: 0,
            total: 0,
            usable: true,
          },
          { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 0, total: 0, usable: true },
        ],
      },
      "GET /api/proxmox/nodes/pve1/bridges": {
        body: [{ bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true }],
      },
      "POST /api/setup": {
        status: 202,
        body: { user: "root@pam", kind: "apply", started: new Date().toISOString(), state: "running", log: [] },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByText("By hand, with the steps shown")).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit…" }));
    const wizard = within(screen.getByRole("region", { name: "Management cluster" }));
    const choice = wizard.getByLabelText("Restore clusters' etcd through the Proxmox VE guest agent");

    await user.click(choice);
    let dialog = screen.getByRole("dialog", { name: "Restore etcd through the guest agent?" });
    expect(dialog).toHaveTextContent("VM.GuestAgent.Unrestricted");
    expect(dialog).toHaveTextContent("root shell");
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(choice).not.toBeChecked();

    await user.click(choice);
    dialog = screen.getByRole("dialog", { name: "Restore etcd through the guest agent?" });
    await user.click(within(dialog).getByRole("button", { name: "Switch on" }));
    expect(choice).toBeChecked();
    expect(calls.map((c) => c.key)).not.toContain("POST /api/setup");

    await user.type(wizard.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(wizard.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "POST /api/setup")?.body).toMatchObject({
        settings: { etcdRestore: true },
        passphrase: "correct horse battery staple",
      }),
    );
  });

  it("says so for an address with no page, and tells of an upgrade under the page", async () => {
    window.history.replaceState(null, "", "/nowhere");
    const headers = { "X-Kubehenge-Version": "0.1.0" };
    fakeServer(
      {
        "GET /api/session": { body: { user: "dev@pve", csrf: "c1" } },
        "GET /api/setup": { body: { appliance: true, ready: true, allowed: false } },
        "GET /api/clusters": { body: [] },
      },
      headers,
    );
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByRole("heading", { name: "No such page" })).resolves.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Administration" })).not.toBeInTheDocument();
    headers["X-Kubehenge-Version"] = "0.2.0";
    await user.click(screen.getByRole("link", { name: "Go to the clusters" }));
    await expect(screen.findByText(/Kubehenge was upgraded to 0.2.0/u)).resolves.toBeInTheDocument();
    expect(screen.getByRole("main")).toHaveFocus();
  });

  it("asks for the earlier run's passphrase once when its state is still there", async () => {
    fakeServer({
      ...administrator,
      "GET /api/setup": {
        body: {
          appliance: true,
          ready: false,
          allowed: true,
          resumes: true,
          job: {
            user: "root@pam",
            kind: "apply",
            started: new Date().toISOString(),
            state: "failed",
            error: "tofu apply: timeout",
            log: [],
          },
        },
      },
      "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
      "GET /api/proxmox/storages": { body: [] },
      "GET /api/proxmox/nodes/pve1/bridges": { body: [] },
    });
    render(<App />);
    const passphrase = await screen.findByLabelText("State passphrase");
    expect(passphrase).toHaveAttribute("autocomplete", "current-password");
    expect(passphrase).toHaveAccessibleDescription(/passphrase of the earlier run/u);
    expect(screen.queryByLabelText("Repeat passphrase")).not.toBeInTheDocument();
  });

  it("offers the setup again once the management cluster has been removed", async () => {
    withJob({
      user: "root@pam",
      kind: "remove",
      started: new Date().toISOString(),
      finished: new Date().toISOString(),
      state: "succeeded",
      log: ["removed"],
    });
    render(<App />);
    await expect(screen.findByRole("heading", { name: "Set up Kubehenge" })).resolves.toBeInTheDocument();
    expect(screen.getByText(/The management cluster was removed .* by root@pam\./u)).toBeInTheDocument();
  });
});

describe("backup targets", () => {
  it("are checked as the appliance checks them", () => {
    expect(targetProblems([{ name: "store", bucket: "backups", endpoint: "https://s3.example.org" }])).toStrictEqual(
      [],
    );
    expect(
      targetProblems([
        { name: "Store", bucket: "b", endpoint: "http://s3.example.org", prefix: "/x" },
        { name: "aws", bucket: "aws-backups", region: "EU" },
      ]),
    ).toStrictEqual([
      "Backup target 1: a name of lower-case letters, digits and dashes.",
      "Backup target 1: the endpoint is an https:// URL without query, fragment or user.",
      "Backup target 1: an S3 bucket name: 3 to 63 lower-case letters, digits, dots and dashes.",
      "Backup target 1: folders of letters, digits and !_.*'()- apart by /, without one at either end.",
      "Backup target 2: a region such as eu-central-2.",
    ]);
    expect(
      cleanTargets([{ name: "aws", bucket: "aws-backups", endpoint: "", region: " ", noChecksums: false }]),
    ).toStrictEqual([{ name: "aws", bucket: "aws-backups" }]);
  });

  it("are part of the appliance's settings", () => {
    const store = { name: "store", bucket: "backups", endpoint: "https://s3.example.org", noChecksums: true };
    const saved = settings({ ...formOf(), backupTargets: [store] });
    expect(saved.backupTargets).toStrictEqual([store]);
    expect(formOf(saved).backupTargets).toStrictEqual([store]);
    expect(settings(formOf())).not.toHaveProperty("backupTargets");
    expect(setupFieldProblems({ ...formOf(), backupTargets: [{ name: "", bucket: "backups" }] }).backupTargets).toBe(
      "Backup target 1: a name of lower-case letters, digits and dashes.",
    );
  });
});
