import { act, render, screen, waitFor, within, type BoundFunctions, type queries } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { cleanTargets, targetProblems } from "./BackupTargets";
import { fakeServer, reads, type Route } from "./fake-server";
import {
  addressRows,
  formOf,
  proxySummary,
  settings,
  setupFieldProblems,
  setupProblems,
  type SetupForm,
} from "./Setup";

const form: SetupForm = {
  nodes: ["pve1"],
  imageDatastore: "local",
  vmDatastore: "local-lvm",
  bridge: "vmbr0",
  vlan: "",
  mtu: "",
  families: "ipv4",
  nodeAddresses: "192.0.2.11/24",
  nodeAddresses6: "",
  apiAddress: "192.0.2.10",
  gateway: "192.0.2.1",
  gateway6: "",
  dnsServers: "192.0.2.1, 1.1.1.1",
  searchDomains: "",
  timeServers: "",
  workloadDatastores: ["local-lvm"],
  workloadBridges: ["vmbr0"],
  bootstrapDatastore: "local",
  mirrors: "",
  chartMirrors: "",
  chartMirrorCA: "",
  proxyURL: "",
  proxyUser: "",
  proxyPassword: "",
  proxyPasswordSaved: false,
  proxyPasswordAction: "keep",
  proxyExceptions: "",
  proxyCA: "",
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
  refreshNode: "",
  refreshBridge: "",
  refreshVLAN: "",
  refreshAddress: "",
  refreshGateway: "",
  refreshDNS: "",
  creatorBuilds: false,
  kubeadmPatches: true,
  clusterSysAudit: true,
  nodeBootLogs: true,
  apiAuditLog: false,
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

// A one-node Proxmox VE as it answers: local takes every kind of content, local-lvm has more room
// for disks, and the node reaches its gateway through vmbr0.
const oneNode = {
  "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] },
  "GET /api/proxmox/storages": {
    body: [
      {
        storage: "local",
        node: "pve1",
        shared: false,
        content: ["iso", "import", "images"],
        avail: 10,
        total: 100,
        usable: true,
      },
      { storage: "local-lvm", node: "pve1", shared: false, content: ["images"], avail: 90, total: 100, usable: true },
    ],
  },
  "GET /api/proxmox/nodes/pve1/bridges": {
    body: [
      {
        bridge: "vmbr0",
        node: "pve1",
        vlanAware: false,
        usable: true,
        cidr: "192.0.2.5/24",
        gateway: "192.0.2.1",
        cidr6: "2001:db8::5/64",
        gateway6: "2001:db8::1",
      },
      { bridge: "vmbr1", node: "pve1", vlanAware: true, usable: true },
    ],
  },
  // The IPv6 server stays out of an IPv4 cluster's DNS servers.
  "GET /api/proxmox/nodes/pve1/dns": {
    body: { servers: ["192.0.2.53", "2001:db8::53", "198.51.100.53"], search: "corp.example" },
  },
};

// A management cluster on both families with IPv6 first, and on IPv6 alone.
const dualStack: Partial<SetupForm> = {
  families: "ipv6-ipv4",
  nodeAddresses6: "2001:db8::11/64",
  gateway6: "2001:db8::1",
  apiAddress: "2001:db8::10",
  dnsServers: "192.0.2.53, 2001:db8::53",
};
const ipv6Only: Partial<SetupForm> = { ...dualStack, families: "ipv6", dnsServers: "2001:db8::53" };

type NetworkKey = "nodes" | "nodeAddresses" | "nodeAddresses6" | "gateway" | "gateway6" | "apiAddress" | "dnsServers";
const networkKeys: NetworkKey[] = [
  "nodes",
  "nodeAddresses",
  "nodeAddresses6",
  "gateway",
  "gateway6",
  "apiAddress",
  "dnsServers",
];

// network are the problems of the form with over, those of the nodes and their addresses alone.
function network(over: Partial<SetupForm>): Partial<Record<NetworkKey, string>> {
  const p = setupFieldProblems({ ...form, ...over });
  const out: Partial<Record<NetworkKey, string>> = {};
  for (const k of networkKeys) {
    const problem = p[k];
    if (problem !== undefined) {
      out[k] = problem;
    }
  }
  return out;
}

const next = async (user: UserEvent): Promise<void> => user.click(screen.getByRole("button", { name: "Next" }));

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

const problems = (over: Partial<SetupForm>) => setupFieldProblems({ ...form, ...over });

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
      kubeadmPatches: true,
    });
    const off = settings({ ...form, kubeadmPatches: false });
    expect(off.templateRefresh).toMatchObject({ kubeadmPatches: false });
    expect(formOf(off)).toMatchObject({ kubeadmPatches: false });
    expect(setupFieldProblems({ ...form, refreshGateway: "192.0.2.1" })).toHaveProperty("refreshAddress");
    expect(setupFieldProblems({ ...form, refreshAddress: "192.0.2.50" })).toHaveProperty("refreshAddress");
    const outside = setupFieldProblems({
      ...form,
      refreshAddress: "192.0.2.50/24",
      refreshGateway: "198.51.100.1",
      refreshDNS: "dns",
    });
    expect([outside.refreshGateway, outside.refreshDNS]).toStrictEqual([
      "Enter a gateway in the build address's network.",
      "Enter IP addresses only.",
    ]);
    expect(setupFieldProblems({ ...form, refresh: false, refreshGateway: "x" })).not.toHaveProperty("refreshAddress");
    // An IPv6 build network may route through a link-local gateway, an IPv4 one never.
    const sixBuild = { ...form, refreshAddress: "2001:db8::50/64", refreshGateway: "fe80::1" };
    expect(setupFieldProblems(sixBuild)).not.toHaveProperty("refreshGateway");
    expect(
      setupFieldProblems({ ...form, refreshAddress: "192.0.2.50/24", refreshGateway: "169.254.0.1" }),
    ).toHaveProperty("refreshGateway");
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

  it("sends chart mirrors and their authority only when set", () => {
    const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----";
    expect(settings(form)).not.toHaveProperty("chartRepositories");
    expect(settings({ ...form, chartMirrors: " \n" })).not.toHaveProperty("chartRepositories");
    expect(
      settings({
        ...form,
        chartMirrors:
          "ghcr.io = oci://registry.example.org/ghcr\ncharts.jetstack.io=https://charts.example.org/jetstack\n",
      }).chartRepositories,
    ).toStrictEqual({
      mirrors: {
        "ghcr.io": "oci://registry.example.org/ghcr",
        "charts.jetstack.io": "https://charts.example.org/jetstack",
      },
    });
    expect(
      settings({ ...form, chartMirrors: "ghcr.io=oci://m.example", chartMirrorCA: ` ${pem}\n\n` }).chartRepositories,
    ).toStrictEqual({ mirrors: { "ghcr.io": "oci://m.example" }, caBundle: `${pem}\n` });
  });

  it("reads saved chart mirrors back into the form", () => {
    const saved = {
      ...settings(form),
      chartRepositories: {
        mirrors: { "ghcr.io": "oci://m.example/ghcr", "charts.jetstack.io": "https://m.example/jetstack" },
        caBundle: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
      },
    };
    const again = formOf(saved);
    expect(again.chartMirrors).toBe("ghcr.io=oci://m.example/ghcr\ncharts.jetstack.io=https://m.example/jetstack");
    expect(again.chartMirrorCA).toBe(saved.chartRepositories.caBundle);
    expect(settings({ ...again, passphrase: "", confirm: "" }).chartRepositories).toStrictEqual(
      saved.chartRepositories,
    );
    expect(formOf(settings(form)).chartMirrors).toBe("");
  });

  it("checks the chart mirrors and their authority", () => {
    const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----";
    const ok = "ghcr.io=oci://registry.example.org/ghcr";
    expect(problems({ chartMirrors: `${ok}\ncharts.example=http://10.0.0.1:8080` })).toStrictEqual({});
    expect(problems({ chartMirrors: "ghcr.io oci://m.example" }).chartMirrors).toMatch(
      /^ghcr.io oci:\/\/m.example is not in the form host=/u,
    );
    const wrong = [
      "ghcr.io=",
      "=oci://m.example",
      "ghcr.io=registry.example.org",
      "ghcr.io=ftp://m.example",
      "ghcr.io=oci://m.example/",
      "ghcr.io=https://m.example/jetstack/",
      "ghcr.io=https://user:pw@m.example",
      "ghcr.io=oci://m.example/a b",
      "a/b=oci://m.example",
    ];
    const accepted = wrong.filter((line) => problems({ chartMirrors: line }).chartMirrors === undefined);
    expect(accepted).toStrictEqual([]);
    expect(problems({ chartMirrors: `${ok}\nbroken` }).chartMirrors).toMatch(/^broken is not/u);
    expect(problems({ chartMirrors: ok, chartMirrorCA: pem })).toStrictEqual({});
    expect(problems({ chartMirrors: ok, chartMirrorCA: "junk" }).chartMirrorCA).toBe("Paste PEM certificates.");
    expect(problems({ chartMirrors: ok, chartMirrorCA: "-----BEGIN PRIVATE KEY-----\nx" }).chartMirrorCA).toBe(
      "Paste certificates only, no private key.",
    );
    expect(problems({ chartMirrorCA: pem }).chartMirrorCA).toBe("Add a chart mirror first.");
  });

  it("sends a proxy only when it has an address", () => {
    expect(settings(form)).not.toHaveProperty("proxy");
    expect(settings({ ...form, proxyExceptions: "a.example.com", proxyCA: "x" })).not.toHaveProperty("proxy");
    expect(settings({ ...form, proxyURL: " http://proxy.example:3128 " }).proxy).toStrictEqual({
      url: "http://proxy.example:3128",
    });
    const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----";
    expect(
      settings({ ...form, proxyURL: "https://proxy.example", proxyExceptions: ".corp, 10.0.0.0/8", proxyCA: pem })
        .proxy,
    ).toStrictEqual({ url: "https://proxy.example", noProxy: [".corp", "10.0.0.0/8"], caBundle: `${pem}\n` });
  });

  it("sends the proxy's user and password apart from its address", () => {
    const base = { ...form, proxyURL: "http://proxy.example:3128", proxyUser: " alice " };
    expect(settings({ ...base, proxyPassword: "hunter2" }).proxy).toStrictEqual({
      url: "http://proxy.example:3128",
      user: "alice",
      password: "hunter2",
    });
    // A password with no user goes nowhere.
    expect(settings({ ...form, proxyURL: "http://proxy.example:3128", proxyPassword: "hunter2" }).proxy).toStrictEqual({
      url: "http://proxy.example:3128",
    });
    // A saved password never reaches the form, so a form that leaves it alone sends none, which keeps it.
    const saved = formOf({
      ...settings(base),
      proxy: { url: "http://proxy.example:3128", user: "alice", passwordSet: true },
    });
    expect(saved).toMatchObject({ proxyUser: "alice", proxyPassword: "", proxyPasswordSaved: true });
    expect(JSON.stringify(settings(saved).proxy)).not.toContain("password");
    expect(settings({ ...saved, proxyPasswordAction: "remove" }).proxy).toStrictEqual({
      url: "http://proxy.example:3128",
      user: "alice",
      clearPassword: true,
    });
    expect(settings({ ...saved, proxyPasswordAction: "replace", proxyPassword: "new" }).proxy).toMatchObject({
      password: "new",
    });
    expect(proxySummary({ url: "http://proxy.example:3128", user: "alice", passwordSet: true })).toBe(
      "http://proxy.example:3128, user alice, password set",
    );
    expect(proxySummary(undefined)).toBe("None");
  });

  it("checks the proxy without repeating what was typed", () => {
    const inside = problems({ proxyURL: "http://alice:hunter2@proxy.example:3128" });
    expect(inside.proxyURL).toBe("Leave the user name and password out. They have fields of their own.");
    expect(JSON.stringify(inside)).not.toContain("hunter2");
    expect(problems({ proxyURL: "http://proxy.example:3128", proxyPassword: "hunter2" }).proxyUser).toBe(
      "Enter the user name for the password.",
    );
    expect(problems({ proxyURL: "http://proxy.example:3128", proxyUser: "alice" })).toStrictEqual({});
    expect(problems({ proxyUser: "alice" }).proxyURL).toBe("Enter the proxy address.");
    const bad = problems({ proxyURL: "proxy.example" });
    expect(bad.proxyURL).toBe("Use an http:// or https:// address without a path.");
    expect(problems({ proxyURL: "http://proxy.example/pac" }).proxyURL).toMatch(/without a path/u);
    expect(problems({ proxyExceptions: ".corp" }).proxyURL).toBe("Enter the proxy address.");
    expect(problems({ proxyURL: "http://p.example", proxyExceptions: "ok.example, bad?" }).proxyExceptions).toBe(
      "bad? is no host, domain, address or network.",
    );
    expect(problems({ proxyURL: "http://p.example", proxyExceptions: ".corp, 10.0.0.0/8, [::1]" })).not.toHaveProperty(
      "proxyExceptions",
    );
    expect(problems({ proxyURL: "http://p.example", proxyCA: "junk" }).proxyCA).toBe("Paste PEM certificates.");
    expect(problems({ proxyURL: "http://p.example", proxyCA: "-----BEGIN PRIVATE KEY-----\nx" }).proxyCA).toBe(
      "Paste certificates only, no private key.",
    );
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

  it("reads nodes' boot logs through the guest agent unless it is switched off", () => {
    const off = settings({ ...form, nodeBootLogs: false });
    expect(off).toHaveProperty("nodeBootLogs", false);
    expect(settings(form)).not.toHaveProperty("nodeBootLogs");
    expect([formOf(settings(form)).nodeBootLogs, formOf(off).nodeBootLogs]).toStrictEqual([true, false]);
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
    expect(problem("team-a, kubehenge-system")).toMatch(/kubehenge-system is reserved/u);
    expect(problem("kube-public")).toMatch(/kube-public is reserved/u);
    expect(problem("clusters")).toMatch(/clusters is always offered/u);
    expect(problem("Team_A")).toMatch(/at most 63 lowercase letters/u);
    expect(problem("team-a, team-a")).toMatch(/listed twice/u);
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
      "Use at least 16 characters.",
    ]);
    expect(setupProblems({ ...form, confirm: "something else entirely" })).toStrictEqual(["Passphrases do not match."]);
    expect(setupProblems({ ...form, nodes: [] })).toStrictEqual(["Choose at least one Proxmox VE node."]);
    expect(setupProblems({ ...form, nodes: ["pve1", "pve2"] })).toStrictEqual(["Choose no more nodes than addresses."]);
  });

  it("tells users without the rights that an administrator has to set it up", async () => {
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c1" } },
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: false } },
    });
    render(<App />);
    await expect(screen.findByText("Not set up yet")).resolves.toBeInTheDocument();
  });

  // Enter in the passphrase submits as the button does: the wizard is a form, which is also what
  // lets password managers keep the new passphrase.
  it.each<[string, (user: UserEvent) => Promise<void>]>([
    ["the button", (user) => user.click(screen.getByRole("button", { name: "Create management cluster" }))],
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
      ...oneNode,
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
      "POST /api/setup": () => {
        routes["GET /api/setup"] = { body: { appliance: true, ready: false, allowed: true, job } };
        return { status: 202, body: job };
      },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<App />);

    // The roomiest storage for disks is chosen to start with.
    const disks = await screen.findByLabelText("VM disks");
    await waitFor(() => expect(disks).toHaveValue("local-lvm"));
    expect(screen.getByRole("button", { name: "Placement" })).toHaveAttribute("aria-current", "step");
    await next(user);

    // The bridge the node reaches its gateway through, that gateway and the node's DNS servers.
    await waitFor(() => expect(screen.getByLabelText("DNS servers")).toHaveValue("192.0.2.53, 198.51.100.53"));
    expect(screen.getByLabelText("Search domains")).toHaveValue("corp.example");
    expect(screen.getByLabelText("Bridge")).toHaveValue("vmbr0");
    expect(screen.getByLabelText("Gateway")).toHaveValue("192.0.2.1");
    const addresses = screen.getByLabelText("Node addresses");
    expect(addresses).toHaveAccessibleDescription("1 or 3 in 192.0.2.0/24");
    // Next on a step with something missing stays there, says what, and goes to it.
    await next(user);
    expect(screen.getByRole("button", { name: "Network" })).toHaveAttribute("aria-current", "step");
    expect(addresses).toHaveAccessibleDescription("Enter 1 or 3 addresses. 1 or 3 in 192.0.2.0/24");
    expect(addresses).toHaveFocus();
    expect(screen.getByRole("button", { name: "Services" })).toBeDisabled();
    await user.type(addresses, "192.0.2.11/24");
    await user.type(screen.getByLabelText("API address"), "192.0.2.10");
    await next(user);

    // Clusters are offered what the management cluster runs on.
    expect(forClusters("Storages").getByRole("checkbox", { name: "local-lvm" })).toBeChecked();
    expect(forClusters("Storages").getByRole("checkbox", { name: "local" })).not.toBeChecked();
    expect(forClusters("Bridges").getByRole("checkbox", { name: "vmbr0" })).toBeChecked();
    // A storage added keeps the one offered to start with.
    await user.click(forClusters("Storages").getByRole("checkbox", { name: "local" }));
    expect(forClusters("Storages").getByRole("checkbox", { name: "local-lvm" })).toBeChecked();
    await next(user);

    const signIn = screen.getByLabelText("Sign in as cluster-admin");
    expect(signIn).not.toBeChecked();
    expect(signIn).toHaveAccessibleDescription(/Headlamp as cluster-admin/u);
    await user.click(signIn);
    const actions = screen.getByLabelText("Helm and Prometheus charts");
    expect(actions).not.toBeChecked();
    expect(actions).toHaveAccessibleDescription(/own cluster credentials/u);
    await user.click(actions);
    await next(user);
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
        bridge: "vmbr0",
        gateway: "192.0.2.1",
        dnsServers: ["192.0.2.53", "198.51.100.53"],
        searchDomains: ["corp.example"],
        workloadDatastores: ["local-lvm", "local"],
        workloadBridges: ["vmbr0"],
        headlampSignIn: true,
        headlampActions: true,
      },
    });
  });

  it("sends the management VMs' MTU", async () => {
    const job = { user: "root@pam", started: new Date().toISOString(), state: "running", log: ["Applying"] };
    const routes: Record<string, Route> = {
      ...administrator,
      ...oneNode,
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
      "POST /api/setup": () => {
        routes["GET /api/setup"] = { body: { appliance: true, ready: false, allowed: true, job } };
        return { status: 202, body: job };
      },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await waitFor(() => expect(screen.getByLabelText("VM disks")).toHaveValue("local-lvm"));
    await next(user);
    await waitFor(() => expect(screen.getByLabelText("DNS servers")).toHaveValue("192.0.2.53, 198.51.100.53"));
    await user.type(screen.getByLabelText("Node addresses"), "192.0.2.11/24");
    await user.type(screen.getByLabelText("API address"), "192.0.2.10");
    await user.type(screen.getByLabelText("MTU"), "1279");
    await user.clear(screen.getByLabelText("MTU"));
    await user.type(screen.getByLabelText("MTU"), "1450");
    await next(user);
    await next(user);
    await next(user);
    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.type(screen.getByLabelText("Repeat passphrase"), "correct horse battery staple");
    await user.click(screen.getByRole("button", { name: "Create management cluster" }));

    await screen.findByText("Now: Applying");
    expect(calls.find((c) => c.key === "POST /api/setup")?.body).toMatchObject({ settings: { mtu: 1450 } });
  });

  it("loads the saved MTU, and leaves it empty when none is saved", () => {
    expect(formOf(settings({ ...form, mtu: "9000" })).mtu).toBe("9000");
    expect(formOf(settings(form)).mtu).toBe("");
    expect(formOf().mtu).toBe("");
    expect(settings(form)).not.toHaveProperty("mtu");
    expect(settings({ ...form, mtu: "1450" })).toMatchObject({ mtu: 1450 });
  });

  it("holds the MTU to the bounds the module takes", () => {
    for (const mtu of ["1279", "65521", "abc", "1450.5"]) {
      expect(setupFieldProblems({ ...form, mtu }).mtu).toBe("A whole number from 1280 to 65520.");
    }
    for (const mtu of ["", "1280", "65520"]) {
      expect(setupFieldProblems({ ...form, mtu }).mtu).toBeUndefined();
    }
  });

  it("sends the node, bridge and VLAN the template build is given", async () => {
    const job = { user: "root@pam", started: new Date().toISOString(), state: "running", log: ["Applying"] };
    const routes: Record<string, Route> = {
      ...administrator,
      ...oneNode,
      "GET /api/proxmox/nodes": {
        body: [
          { node: "pve1", online: true },
          { node: "pve2", online: true },
        ],
      },
      "GET /api/proxmox/nodes/pve2/bridges": { body: [] },
      "GET /api/proxmox/nodes/pve2/dns": { body: { servers: [] } },
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
      "POST /api/setup": () => {
        routes["GET /api/setup"] = { body: { appliance: true, ready: false, allowed: true, job } };
        return { status: 202, body: job };
      },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await waitFor(() => expect(screen.getByLabelText("VM disks")).toHaveValue("local-lvm"));
    await next(user);
    await waitFor(() => expect(screen.getByLabelText("DNS servers")).toHaveValue("192.0.2.53, 198.51.100.53"));
    await user.type(screen.getByLabelText("Node addresses"), "192.0.2.11/24");
    await user.type(screen.getByLabelText("API address"), "192.0.2.10");
    await next(user);
    await user.click(forClusters("Bridges").getByRole("checkbox", { name: "vmbr1" }));

    expect(screen.getByLabelText("Build node")).toHaveValue("");
    expect(screen.getByLabelText("Build bridge")).toHaveValue("");
    await user.selectOptions(screen.getByLabelText("Build node"), "pve2");
    await user.selectOptions(screen.getByLabelText("Build bridge"), "vmbr1");
    await user.type(screen.getByLabelText("Build VLAN"), "4095");
    expect(screen.getByRole("button", { name: "Next" })).toBeEnabled();
    await next(user);
    expect(screen.getByRole("button", { name: "Clusters" })).toHaveAttribute("aria-current", "step");
    await user.clear(screen.getByLabelText("Build VLAN"));
    await user.type(screen.getByLabelText("Build VLAN"), "120");
    await next(user);
    await next(user);
    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.type(screen.getByLabelText("Repeat passphrase"), "correct horse battery staple");
    await user.click(screen.getByRole("button", { name: "Create management cluster" }));

    await screen.findByText("Now: Applying");
    expect(calls.find((c) => c.key === "POST /api/setup")?.body).toMatchObject({
      settings: { templateRefresh: { enabled: true, node: "pve2", bridge: "vmbr1", vlan: 120 } },
    });
  });

  it("loads the template build's node, bridge and VLAN from the saved settings", () => {
    const saved = settings({ ...form, refreshNode: "pve2", refreshBridge: "evpn1/build", refreshVLAN: "120" });
    expect(saved.templateRefresh).toMatchObject({ node: "pve2", bridge: "evpn1/build", vlan: 120 });
    expect(formOf(saved)).toMatchObject({ refreshNode: "pve2", refreshBridge: "evpn1/build", refreshVLAN: "120" });
    expect(formOf(settings(form))).toMatchObject({ refreshNode: "", refreshBridge: "", refreshVLAN: "" });
    expect(setupFieldProblems({ ...form, refreshVLAN: "4095" })).toHaveProperty("refreshVLAN");
    expect(setupFieldProblems({ ...form, refreshVLAN: "4094" })).not.toHaveProperty("refreshVLAN");
  });

  it.each([
    ["a node without a search domain", { servers: ["192.0.2.53"], search: "" }, ""],
    ["a node with one", { servers: ["192.0.2.53"], search: "corp.example" }, "corp.example"],
  ])("prefills Search domains from %s", async (_kind, dns, expected) => {
    fakeServer({
      ...administrator,
      ...oneNode,
      "GET /api/proxmox/nodes/pve1/dns": { body: dns },
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const disks = await screen.findByLabelText("VM disks");
    await waitFor(() => expect(disks).toHaveValue("local-lvm"));
    await next(user);
    await waitFor(() => expect(screen.getByLabelText("DNS servers")).toHaveValue("192.0.2.53"));
    expect(screen.getByLabelText("Search domains")).toHaveValue(expected);
  });

  it("keeps Search domains as the user left them", async () => {
    fakeServer({
      ...administrator,
      ...oneNode,
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const disks = await screen.findByLabelText("VM disks");
    await waitFor(() => expect(disks).toHaveValue("local-lvm"));
    await next(user);
    const search = screen.getByLabelText("Search domains");
    await waitFor(() => expect(search).toHaveValue("corp.example"));
    await user.clear(search);
    expect(search).toHaveValue("");
    await user.type(search, "lab.example");
    expect(search).toHaveValue("lab.example");
  });

  it("lists VNets with their alias and zone and sends the one chosen as zone/vnet", async () => {
    const job = { user: "root@pam", started: new Date().toISOString(), state: "running", log: ["Applying"] };
    const routes: Record<string, Route> = {
      ...administrator,
      ...oneNode,
      "GET /api/proxmox/nodes/pve1/bridges": {
        body: [
          { bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true, cidr: "192.0.2.5/24", gateway: "192.0.2.1" },
          { bridge: "web", node: "pve1", vlanAware: false, usable: true, zone: "evpn1", alias: "Web tier" },
        ],
      },
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
      "POST /api/setup": () => {
        routes["GET /api/setup"] = { body: { appliance: true, ready: false, allowed: true, job } };
        return { status: 202, body: job };
      },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await waitFor(() => expect(screen.getByLabelText("VM disks")).toHaveValue("local-lvm"));
    await next(user);
    const bridge = screen.getByLabelText("Bridge");
    await waitFor(() =>
      expect(within(bridge).getByRole("option", { name: "web (Web tier), zone evpn1" })).toBeInTheDocument(),
    );
    await user.selectOptions(bridge, "evpn1/web");
    await user.clear(screen.getByLabelText("Gateway"));
    await user.type(screen.getByLabelText("Gateway"), "192.0.2.1");
    await user.type(screen.getByLabelText("Node addresses"), "192.0.2.11/24");
    await user.type(screen.getByLabelText("API address"), "192.0.2.10");
    await next(user);

    // The management cluster's own bridge is the one clusters are offered to start with.
    expect(forClusters("Bridges").getByRole("checkbox", { name: "web (Web tier), zone evpn1" })).toBeChecked();
    expect(forClusters("Bridges").getByRole("checkbox", { name: "vmbr0" })).not.toBeChecked();
    await next(user);
    await next(user);
    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.type(screen.getByLabelText("Repeat passphrase"), "correct horse battery staple");
    await user.click(screen.getByRole("button", { name: "Create management cluster" }));

    await screen.findByText("Now: Applying");
    expect(calls.find((c) => c.key === "POST /api/setup")?.body).toMatchObject({
      settings: { bridge: "evpn1/web", workloadBridges: ["evpn1/web"] },
    });
  });

  it.each<
    [string, { findings: { errors: string[]; warnings: string[] }; message: string; disabled: boolean; reason: string }]
  >([
    [
      "errors",
      {
        findings: { errors: ["Address 192.0.2.11 answers already."], warnings: [] },
        message: "Address 192.0.2.11 answers already.",
        disabled: true,
        reason: "Fix the errors above to apply.",
      },
    ],
    [
      "warnings",
      {
        findings: { errors: [], warnings: ["3 management VMs share 1 Proxmox VE node."] },
        message: "3 management VMs share 1 Proxmox VE node.",
        disabled: false,
        reason: "",
      },
    ],
  ])("lets %s decide whether Apply is possible", async (_kind, { findings, message, disabled, reason }) => {
    fakeServer({
      ...administrator,
      ...oneNode,
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
      "POST /api/setup/check": { body: findings },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    const disks = await screen.findByLabelText("VM disks");
    await waitFor(() => expect(disks).toHaveValue("local-lvm"));
    await next(user);
    await waitFor(() => expect(screen.getByLabelText("DNS servers")).toHaveValue("192.0.2.53, 198.51.100.53"));
    await user.type(screen.getByLabelText("Node addresses"), "192.0.2.11/24");
    await user.type(screen.getByLabelText("API address"), "192.0.2.10");
    await next(user);
    await next(user);
    await next(user);
    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.type(screen.getByLabelText("Repeat passphrase"), "correct horse battery staple");

    const create = screen.getByRole("button", { name: "Create management cluster" });
    await expect(screen.findByText(message)).resolves.toBeInTheDocument();
    expect(create).toHaveProperty("disabled", disabled);
    expect(create).toHaveAccessibleDescription(reason);
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
          {
            storage: "local",
            node: "pve1",
            shared: false,
            content: ["iso", "import"],
            avail: 0,
            total: 0,
            usable: true,
          },
          {
            storage: "local",
            node: "pve2",
            shared: false,
            content: ["iso", "import"],
            avail: 0,
            total: 0,
            usable: true,
          },
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
    const disks = await screen.findByLabelText("VM disks");
    await waitFor(() => expect(within(disks).getByRole("option", { name: "fast" })).toBeInTheDocument());
    await user.selectOptions(disks, "fast");
    await next(user);
    // A node that reaches no gateway through any bridge has no bridge chosen for it.
    const bridge = screen.getByLabelText("Bridge");
    await waitFor(() => expect(within(bridge).getByRole("option", { name: "vmbr1" })).toBeInTheDocument());
    expect(bridge).toHaveValue("");
    await user.selectOptions(bridge, "vmbr1");

    await user.click(screen.getByRole("button", { name: "Back" }));
    await user.click(screen.getByRole("checkbox", { name: "pve2" }));
    expect(screen.queryByRole("option", { name: "fast" })).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: "local-lvm" })).toBeInTheDocument();
    // What was chosen for pve1 alone is no longer chosen.
    expect(screen.getByLabelText("VM disks")).toHaveValue("");
    await user.selectOptions(screen.getByLabelText("VM disks"), "local-lvm");
    await next(user);
    expect(screen.queryByRole("option", { name: "vmbr1" })).not.toBeInTheDocument();
    expect(screen.getByLabelText("Bridge")).toHaveValue("");
    await user.selectOptions(screen.getByLabelText("Bridge"), "vmbr0");
    await user.type(screen.getByLabelText("Node addresses"), "192.0.2.11/24");
    await user.type(screen.getByLabelText("API address"), "192.0.2.10");
    await user.type(screen.getByLabelText("Gateway"), "192.0.2.1");
    await user.type(screen.getByLabelText("DNS servers"), "192.0.2.1");
    await next(user);

    // Clusters may still use what only one node has.
    expect(forClusters("Bridges").getByRole("checkbox", { name: "vmbr1" })).toHaveAccessibleDescription("(on pve1)");
    expect(forClusters("Bridges").getByRole("checkbox", { name: "vmbr0" })).toHaveAccessibleDescription(
      "(on pve1, pve2)",
    );
    expect(forClusters("Storages").getByRole("checkbox", { name: "fast" })).toHaveAccessibleDescription("(on pve1)");
  });

  it("checks addresses as the appliance does", () => {
    const problemsOf = (f: Partial<SetupForm>): ReturnType<typeof setupFieldProblems> =>
      setupFieldProblems({ ...form, ...f });
    expect(problemsOf({ gateway: "gw" }).gateway).toBe("Enter an IPv4 address.");
    expect(problemsOf({ nodeAddresses: "192.0.2.0/24" }).nodeAddresses).toMatch(/a network or broadcast address/u);
    expect(problemsOf({ nodeAddresses: "198.51.100.11/24" }).nodeAddresses).toMatch(/outside the gateway's network/u);
    expect(
      problemsOf({ nodeAddresses: "192.0.2.11/24 192.0.2.11/24 192.0.2.12/24", nodes: ["pve1"] }).nodeAddresses,
    ).toMatch(/listed twice/u);
    expect(problemsOf({ apiAddress: "192.0.2.11" }).apiAddress).toMatch(/unused/u);
    expect(problemsOf({ apiAddress: "198.51.100.10" }).apiAddress).toBe("Use an address in the nodes' network.");
    expect(problemsOf({ dnsServers: "dns.example" }).dnsServers).toBe("Enter IP addresses only.");
    expect(problemsOf({ dnsServers: "2001:db8::53" }).dnsServers).toBe("Enter IPv4 addresses only.");
    expect(problemsOf({ searchDomains: "corp.example, example.org" }).searchDomains).toBeUndefined();
    expect(problemsOf({ searchDomains: "corp example." }).searchDomains).toBe("Enter domain names like corp.example");
    expect(problemsOf({ searchDomains: "a b c d e f g" }).searchDomains).toBe("At most 6 domains");
    expect(settings({ ...form, searchDomains: "corp.example example.org" }).searchDomains).toStrictEqual([
      "corp.example",
      "example.org",
    ]);
    expect(settings(form)).not.toHaveProperty("searchDomains");
    expect(problemsOf({ vlan: "4095" }).vlan).toBe("A whole number from 1 to 4094.");
    expect(problemsOf({ podSubnets: "10.244.0.0/16, 10.245.0.1/16" }).podSubnets).toMatch(
      /^10.245.0.1\/16 is not a network address/u,
    );
    expect(problemsOf({ mirrors: "ghcr.io=mirror:5000" }).mirrors).toMatch(/^ghcr.io=mirror:5000 is not in the form/u);
    expect(problemsOf({ mirrors: "ghcr.io=https://mirror.example:5000/v2" }).mirrors).toBeUndefined();
    // Applying again opens the state with the passphrase given at the start: nothing to repeat.
    expect(setupFieldProblems({ ...form, confirm: "" }, false).confirm).toBeUndefined();
    // A running management cluster keeps its nodes' addresses, and grows only after them.
    const was = settings(form);
    expect(setupFieldProblems({ ...form, nodeAddresses: "192.0.2.12/24" }, false, was).nodeAddresses).toMatch(
      /^Keep 192.0.2.11\/24 first/u,
    );
    expect(
      setupFieldProblems({ ...form, nodeAddresses: "192.0.2.11/24 192.0.2.12/24 192.0.2.13/24" }, false, was)
        .nodeAddresses,
    ).toBeUndefined();
  });

  it("sends and reads back the network of each family choice", () => {
    expect(settings(form)).not.toHaveProperty("ipFamilies");
    expect(settings(form)).not.toHaveProperty("nodeAddressesIPv6");
    const saved = settings({ ...form, ...dualStack });
    expect(saved).toMatchObject({
      ipFamilies: ["IPv6", "IPv4"],
      nodeAddresses: ["192.0.2.11/24"],
      gateway: "192.0.2.1",
      nodeAddressesIPv6: ["2001:db8::11/64"],
      gatewayIPv6: "2001:db8::1",
      apiAddress: "2001:db8::10",
    });
    expect(formOf(saved)).toMatchObject({
      families: "ipv6-ipv4",
      nodeAddresses6: "2001:db8::11/64",
      gateway6: "2001:db8::1",
    });
    expect(settings({ ...form, ...ipv6Only, gateway: "192.0.2.1" })).toMatchObject({
      ipFamilies: ["IPv6"],
      nodeAddresses: [],
      gateway: "",
      nodeAddressesIPv6: ["2001:db8::11/64"],
    });
  });

  it("checks each family's addresses as the appliance does", () => {
    expect(network(dualStack)).toStrictEqual({});
    expect(network(ipv6Only)).toStrictEqual({});
    // The fields of a family the cluster lacks are left alone.
    expect(network({ ...ipv6Only, gateway: "nonsense", nodeAddresses: "" })).toStrictEqual({});
    expect(network({ ...ipv6Only, nodeAddresses6: "192.0.2.11/24" }).nodeAddresses6).toBe(
      "192.0.2.11/24 is not in the form 2001:db8::11/64.",
    );
    expect(network({ ...ipv6Only, nodeAddresses6: "2001:db8::/64" }).nodeAddresses6).toBe(
      "2001:db8::/64 is its network's own address.",
    );
    expect(network({ ...ipv6Only, nodeAddresses6: "2001:db9::11/64" }).nodeAddresses6).toMatch(
      /outside the gateway's network/u,
    );
    // A link-local IPv6 gateway reaches every IPv6 network.
    expect(network({ ...ipv6Only, gateway6: "fe80::1" })).toStrictEqual({});
    expect(network({ ...ipv6Only, gateway6: "192.0.2.1" }).gateway6).toBe("Enter an IPv6 address.");
    expect(network({ ...ipv6Only, apiAddress: "192.0.2.10" }).apiAddress).toBe("Enter an IPv6 address.");
    expect(network({ ...ipv6Only, apiAddress: "2001:db8::11" }).apiAddress).toMatch(/unused/u);
    expect(network({ ...ipv6Only, dnsServers: "192.0.2.53" }).dnsServers).toBe("Enter IPv6 addresses only.");
    expect(network({ ...dualStack, dnsServers: "192.0.2.53" }).dnsServers).toBe("Add an IPv6 DNS server too.");
    expect(network({ ...dualStack, apiAddress: "192.0.2.10" }).apiAddress).toBe("Enter an IPv6 address.");
    // Node i takes the i-th address of each family.
    expect(
      network({ ...dualStack, nodeAddresses: "192.0.2.11/24 192.0.2.12/24 192.0.2.13/24", nodes: ["pve1"] })
        .nodeAddresses6,
    ).toBe("Enter as many IPv6 as IPv4 addresses.");
    // The primary family's addresses count the nodes.
    expect(network({ ...ipv6Only, nodeAddresses: "", nodes: ["pve1", "pve2"] }).nodes).toBe(
      "Choose no more nodes than addresses.",
    );
  });

  it("keeps a management cluster's IPv6 addresses and families", () => {
    const was = settings({ ...form, ...dualStack });
    expect(
      setupFieldProblems({ ...form, ...dualStack, nodeAddresses6: "2001:db8::12/64" }, false, was).nodeAddresses6,
    ).toBe("Keep 2001:db8::11/64 first and add new addresses after.");
    expect(addressRows(was)).toStrictEqual([
      ["Address families", "IPv6 and IPv4"],
      ["Node addresses", "192.0.2.11/24"],
      ["IPv6 node addresses", "2001:db8::11/64"],
      ["API address", "2001:db8::10"],
      ["Gateway", "192.0.2.1"],
      ["IPv6 gateway", "2001:db8::1"],
    ]);
    expect(addressRows(settings(form)).map(([label]) => label)).toStrictEqual([
      "Node addresses",
      "API address",
      "Gateway",
    ]);
  });

  it("shows the fields of the families chosen", async () => {
    fakeServer({
      ...administrator,
      ...oneNode,
      "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await waitFor(() => expect(screen.getByLabelText("VM disks")).toHaveValue("local-lvm"));
    await next(user);
    const families = screen.getByLabelText("Address families");
    expect(families).toHaveValue("ipv4");
    expect(screen.queryByLabelText("IPv6 node addresses")).toBeNull();
    await user.selectOptions(families, "IPv6 and IPv4");
    expect(screen.getByLabelText("IPv6 node addresses")).toHaveAccessibleDescription("1 or 3 in 2001:db8::/64");
    expect(screen.getByLabelText("IPv6 gateway")).toHaveValue("2001:db8::1");
    expect(screen.getByLabelText("Gateway")).toHaveValue("192.0.2.1");
    await user.selectOptions(families, "IPv6");
    expect(screen.queryByLabelText("Node addresses")).toBeNull();
    expect(screen.queryByLabelText("Gateway")).toBeNull();
    expect(screen.getByLabelText("DNS servers")).toHaveValue("2001:db8::53");
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
    const alert = await screen.findByText("Last run failed");
    expect(alert).toHaveTextContent("wrong passphrase");
  });

  it("offers to take over when a newer state changed the management cluster", async () => {
    withJob({
      user: "root@pam",
      automatic: true,
      kind: "apply",
      started: new Date().toISOString(),
      state: "failed",
      error: "this appliance's state is from apply 2, and the management cluster is at apply 3",
      stale: { local: 2, cluster: 3 },
      log: [],
    });
    render(<App />);
    const alert = await screen.findByText(/A newer state changed the management cluster\./u);
    expect(alert).toHaveTextContent("This appliance's state is from apply 2.");
    expect(within(alert).getByRole("button", { name: "Take over…" })).toBeInTheDocument();
    expect(screen.queryByText(/retries at the next upgrade hour/u)).not.toBeInTheDocument();
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

  it("warns administrators that the appliance's address moved away from its certificate's", async () => {
    fakeServer({
      ...administrator,
      "GET /api/setup": {
        body: {
          appliance: true,
          ready: true,
          allowed: true,
          applianceVMID: 105,
          addressMoved: { from: ["192.0.2.40"], to: ["192.0.2.41"] },
        },
      },
      "GET /api/clusters": { body: [] },
    });
    render(<App />);
    const alert = await screen.findByText(/Appliance address moved from 192\.0\.2\.40 to 192\.0\.2\.41\./u);
    expect(within(alert).getByText("sh install.sh --certificate --ctid 105")).toBeInTheDocument();
  });

  it("warns administrators that the appliance's disk fills up", async () => {
    fakeServer({
      ...administrator,
      "GET /api/setup": {
        body: {
          appliance: true,
          ready: true,
          allowed: true,
          applianceVMID: 105,
          diskFull: { usedPercent: 86, freeMiB: 1192 },
        },
      },
      "GET /api/clusters": { body: [] },
    });
    render(<App />);
    const alert = await screen.findByText(/Appliance disk 86 % full, 1\.2 GiB free\./u);
    expect(within(alert).getByText("pct resize 105 rootfs +4G")).toBeInTheDocument();
  });

  it("warns administrators that the management cluster's credentials run out", async () => {
    fakeServer({
      ...administrator,
      "GET /api/setup": {
        body: {
          appliance: true,
          ready: true,
          allowed: true,
          credentialsExpire: new Date(Date.now() - 86_400_000).toISOString(),
        },
      },
      "GET /api/clusters": { body: [] },
    });
    render(<App />);
    const warning = await screen.findByText(/Management cluster credentials expired on/u);
    expect(warning).toHaveTextContent(/Apply the settings in Administration to renew them\.$/u);
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
    const pages = await screen.findByRole("navigation", { name: "Administration pages" });
    await user.click(within(pages).getByRole("link", { name: "Appliance" }));
    await expect(screen.findByRole("heading", { name: "Proxmox VE API" })).resolves.toBeInTheDocument();
    expect(window.location.pathname).toBe("/administration/appliance");
    expect(screen.getByText("Certificate check").nextElementSibling).toHaveTextContent("Off");
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
    expect(document.title).toBe("Administration · Kubehenge");
    expect(screen.getByText("192.0.2.11/24")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Apply again…" }));
    const dialog = screen.getByRole("dialog", { name: "Apply management cluster" });
    await user.type(within(dialog).getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(within(dialog).getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "POST /api/setup")?.body).toStrictEqual({
        settings: saved,
        passphrase: "correct horse battery staple",
      }),
    );

    await user.click(screen.getByRole("button", { name: "Edit settings" }));
    const wizard = within(screen.getByRole("region", { name: "Management cluster" }));
    // Editing shows every step at once, each a section, with no steps to go through.
    expect(wizard.getAllByRole("heading", { level: 4 }).map((h) => h.textContent)).toStrictEqual([
      "Placement",
      "Network",
      "Clusters",
      "Services",
      "Passphrase",
    ]);
    expect(wizard.queryByRole("navigation", { name: "Steps" })).not.toBeInTheDocument();
    expect(wizard.getByLabelText("State passphrase")).toHaveAttribute("autocomplete", "current-password");
    expect(wizard.queryByLabelText("Repeat passphrase")).not.toBeInTheDocument();
    expect(wizard.getByLabelText("API address")).toHaveAttribute("readonly");
    expect(wizard.getByLabelText("VM disks")).toHaveValue("local-lvm");
    await user.click(wizard.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("button", { name: "Apply again…" })).toBeInTheDocument();
  });

  it("shows a saved proxy password as set, and replaces or removes it without ever holding it", async () => {
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
      proxy: { url: "http://proxy.example:3128", user: "alice", passwordSet: true },
    };
    const calls = fakeServer({
      ...administrator,
      ...administration,
      ...oneNode,
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: true, settings: saved } },
      "POST /api/setup": {
        status: 202,
        body: { user: "root@pam", kind: "apply", started: new Date().toISOString(), state: "running", log: [] },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findByRole("heading", { name: "Management cluster" });
    expect(screen.getByText("http://proxy.example:3128, user alice, password set")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Edit settings" }));
    const wizard = within(screen.getByRole("region", { name: "Management cluster" }));
    await user.click(wizard.getByText("Advanced"));
    expect(wizard.getByLabelText("Proxy user")).toHaveValue("alice");
    expect(wizard.getByLabelText("Proxy password")).not.toHaveAttribute("type", "password");
    expect(wizard.getByRole("button", { name: "Replace proxy password" })).toBeInTheDocument();

    await user.click(wizard.getByRole("button", { name: "Replace proxy password" }));
    expect(wizard.getByLabelText("Proxy password")).toHaveAttribute("type", "password");
    await user.click(wizard.getByRole("button", { name: "Cancel replacing proxy password" }));
    await user.click(wizard.getByRole("button", { name: "Remove proxy password" }));
    expect(wizard.getByText("Removed when you apply")).toBeInTheDocument();
    await user.type(wizard.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(wizard.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(calls.find((c) => c.key === "POST /api/setup")).toBeDefined());
    expect(calls.find((c) => c.key === "POST /api/setup")?.body).toMatchObject({
      settings: { proxy: { url: "http://proxy.example:3128", user: "alice", clearPassword: true } },
    });
  });

  it("loads saved chart mirrors into the form and sends them back", async () => {
    window.history.replaceState(null, "", "/administration");
    const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n";
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
      chartRepositories: { mirrors: { "ghcr.io": "oci://registry.example.org/ghcr" }, caBundle: pem },
    };
    const calls = fakeServer({
      ...administrator,
      ...administration,
      ...oneNode,
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: true, settings: saved } },
      "POST /api/setup": {
        status: 202,
        body: { user: "root@pam", kind: "apply", started: new Date().toISOString(), state: "running", log: [] },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await screen.findByRole("heading", { name: "Management cluster" });
    expect(screen.getByText("ghcr.io=oci://registry.example.org/ghcr")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Edit settings" }));
    const wizard = within(screen.getByRole("region", { name: "Management cluster" }));
    await user.click(wizard.getByText("Advanced"));
    const mirrors = wizard.getByLabelText("Chart mirrors");
    expect(mirrors).toHaveValue("ghcr.io=oci://registry.example.org/ghcr");
    expect(wizard.getByLabelText("Chart mirror certificate")).toHaveValue(pem);
    await user.type(mirrors, "{Enter}charts.example");
    await expect(wizard.findByText(/charts.example is not in the form host=/u)).resolves.toBeInTheDocument();
    await user.type(mirrors, "=https://charts.example.org/jetstack");
    expect(wizard.queryByText(/is not in the form host=/u)).not.toBeInTheDocument();
    await user.type(wizard.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(wizard.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(calls.find((c) => c.key === "POST /api/setup")).toBeDefined());
    expect(calls.find((c) => c.key === "POST /api/setup")?.body).toMatchObject({
      settings: {
        chartRepositories: {
          mirrors: {
            "ghcr.io": "oci://registry.example.org/ghcr",
            "charts.example": "https://charts.example.org/jetstack",
          },
          caBundle: pem,
        },
      },
    });
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
    await expect(screen.findByText("By hand")).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit settings" }));
    const wizard = within(screen.getByRole("region", { name: "Management cluster" }));
    const choice = wizard.getByLabelText("Restore through guest agent");

    await user.click(choice);
    let dialog = screen.getByRole("dialog", { name: "Allow guest agent restores" });
    expect(dialog).toHaveTextContent("VM.GuestAgent.Unrestricted");
    expect(dialog).toHaveTextContent("root shell");
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(choice).not.toBeChecked();

    await user.click(choice);
    dialog = screen.getByRole("dialog", { name: "Allow guest agent restores" });
    await user.click(within(dialog).getByRole("button", { name: "Allow" }));
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
    await user.click(screen.getByRole("link", { name: "Go to clusters" }));
    await expect(screen.findByText(/Kubehenge upgraded to 0.2.0/u)).resolves.toBeInTheDocument();
    expect(screen.getByRole("main")).toHaveFocus();
  });

  it("shows the cluster list at /clusters too", async () => {
    window.history.replaceState(null, "", "/clusters");
    fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c1" } },
      "GET /api/setup": { body: { appliance: true, ready: true, allowed: false } },
      "GET /api/clusters": { body: [] },
    });
    render(<App />);
    await expect(screen.findByRole("heading", { name: "Clusters" })).resolves.toBeInTheDocument();
    expect(
      within(screen.getByRole("navigation", { name: "Pages" })).getByRole("link", { name: "Clusters" }),
    ).toHaveAttribute("aria-current", "page");
  });

  it("asks for the earlier run's passphrase once when its state is still there", async () => {
    fakeServer({
      ...administrator,
      ...oneNode,
      "GET /api/setup": {
        body: {
          appliance: true,
          ready: false,
          allowed: true,
          resumes: true,
          settings: settings(form),
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
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);
    // The earlier run's settings are whole, so every step is open.
    const step = await screen.findByRole("button", { name: "Passphrase" });
    await waitFor(() => expect(step).toBeEnabled());
    await user.click(step);
    const passphrase = screen.getByLabelText("State passphrase");
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
    expect(screen.getByText(/^Management cluster removed .* by root@pam$/u)).toBeInTheDocument();
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
      "Backup target 1: use lowercase letters, digits and dashes.",
      "Backup target 1: enter an https:// URL without query, fragment or user.",
      "Backup target 1: use 3 to 63 lowercase letters, digits, dots and dashes.",
      "Backup target 1: use letters, digits and !_.*'()-, with / only between folders, at most 256.",
      "Backup target 2: enter a region like eu-central-2.",
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
      "Backup target 1: use lowercase letters, digits and dashes.",
    );
  });
});
