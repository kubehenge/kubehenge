// The sweep clicks one button at a time, and each click decides what is checked next.
/* oxlint-disable no-await-in-loop, vitest/no-conditional-in-test */
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { expectNoViolations } from "./axe";
import { fakeServer, type Route } from "./fake-server";
import { ManagementBackups } from "./ManagementBackups";

// The pages the router loads on first use are transformed by Vite the first time they are imported.
// On a loaded machine that took longer than a test's wait for its page to leave the Suspense
// skeleton, so every one is imported while the file is collected, before any test waits on a page.
await Promise.all([
  import("./RunPage"),
  import("./Administration"),
  import("./Setup"),
  import("./ClusterDetail"),
  import("./CreateCluster"),
  import("./Health"),
]);

const when = "2026-01-02T03:04:05Z";

const machine = (name: string, role: "control-plane" | "worker") => ({
  name,
  node: `${name}-node`,
  proxmoxNode: "pve1",
  role,
  pool: role === "worker" ? "default-worker" : undefined,
  version: "v1.36.1",
  phase: "Running",
  ready: true,
  addresses: ["192.0.2.21"],
  created: when,
  console: "https://pve1.example.test:8006/",
  reboot: true,
});

const cluster = (name: string, extra: Record<string, unknown> = {}) => ({
  namespace: "default",
  name,
  class: "kubehenge-kubeadm",
  distribution: "kubeadm",
  version: "v1.36.1",
  generation: 1,
  created: when,
  createdBy: "root@pam",
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [{ type: "Available", status: "True", since: when }],
  endpoint: "https://192.0.2.30:6443",
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "default-worker", class: "default-worker", replicas: 1 }],
  variables: {},
  pool: `/pool/kubehenge/default.${name}`,
  allowed: { change: true, kubeconfig: true },
  machines: [machine(`${name}-cp-1`, "control-plane"), machine(`${name}-w-1`, "worker")],
  ...extra,
});

const section = (items: unknown[] = []) => ({ items });

const health = {
  read: when,
  verdict: "healthy",
  attention: [],
  nodes: section([
    {
      name: "web-cp-1-node",
      machine: "web-cp-1",
      role: "control-plane",
      joined: true,
      ready: true,
      pods: 8,
      kubelet: "v1.36.1",
    },
  ]),
  pods: section(),
  volumes: section(),
  addons: section(),
  pools: section([{ name: "default-worker", desired: 1, ready: 1, available: 1, upToDate: 1, rollingOut: false }]),
  controlPlane: { conditions: [] },
  healthChecks: section(),
  events: section(),
  logs: false,
};

const backups = {
  enabled: true,
  target: "main",
  reachable: true,
  location: "Available",
  backups: [],
  restores: [],
  sources: [],
  attention: [],
  canBackUp: true,
  canDelete: true,
  canRestore: true,
};

const etcd = {
  enabled: true,
  target: "main",
  schedule: "0 * * * *",
  timeZone: "UTC",
  keep: 5,
  requested: false,
  snapshots: [],
  restoring: false,
  automatic: false,
  canSnapshot: true,
  canRestore: true,
};

const activity = { since: when, entries: [], events: section() };

const nodes = { "GET /api/proxmox/nodes": { body: [{ node: "pve1", online: true }] } };

const signedIn: Record<string, Route> = {
  "GET /api/session": { body: { user: "root@pam", csrf: "c1", canCreate: true, canTrust: true } },
  "GET /api/setup": { body: { appliance: true, ready: true, allowed: true, backup: true } },
  "GET /api/clusters": { body: [cluster("web")] },
  "GET /api/fleet/health": { body: [] },
  "GET /api/runs": { body: [] },
  "DELETE /api/session": { status: 204 },
};

interface Case {
  name: string;
  path: string;
  routes?: Record<string, Route>;
  /** Text or role the page shows once it has loaded. */
  ready: () => Promise<unknown>;
  /** Walks the page beyond its first state, checking each state it reaches. */
  /** Sweeps the top bar and the rail too. */
  chrome?: true;
  walk?: (check: (what: string) => Promise<void>, user: UserEvent) => Promise<void>;
}

const object = { type: "object" };
const machineSchema = (cores: number, disk: number) => ({
  type: "object",
  default: { cores, memoryMiB: 4096, diskGiB: disk },
  properties: {
    cores: { type: "integer", minimum: 2 },
    memoryMiB: { type: "integer", minimum: 4096 },
    diskGiB: { type: "integer", minimum: 20 },
  },
});
const kubeadmClass = {
  name: "kubehenge-kubeadm",
  distribution: "kubeadm",
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+$` },
  variables: [
    { name: "controlPlaneEndpoint", required: true, schema: { type: "string" } },
    { name: "network", required: true, schema: object },
    { name: "storage", required: true, schema: { type: "string" } },
    { name: "allowedNodes", required: false, schema: { type: "array" } },
    { name: "controlPlaneMachine", required: false, schema: machineSchema(2, 30) },
    { name: "workerMachine", required: false, schema: machineSchema(2, 40) },
    { name: "cni", required: false, schema: { type: "string", enum: ["canal", "calico", "cilium"], default: "canal" } },
    { name: "headlamp", required: false, schema: object },
    { name: "monitoring", required: false, schema: object },
    { name: "podSecurity", required: false, schema: object },
    { name: "certificates", required: false, schema: object },
    { name: "cis", required: false, schema: { type: "boolean", default: false } },
    { name: "rancher", required: false, schema: object },
    { name: "oidc", required: false, schema: { type: "array" } },
    { name: "namespaceLabels", required: false, schema: object },
    { name: "useExisting", required: false, schema: object },
  ],
};

const storages = {
  "GET /api/proxmox/storages": {
    body: [
      {
        storage: "local-lvm",
        node: "pve1",
        shared: false,
        content: ["images", "iso", "import"],
        avail: 1 << 30,
        total: 2 << 30,
        usable: true,
      },
    ],
  },
  "GET /api/proxmox/nodes/pve1/bridges": {
    body: [
      { bridge: "vmbr0", node: "pve1", vlanAware: false, usable: true, cidr: "192.0.2.5/24", gateway: "192.0.2.1" },
    ],
  },
  "GET /api/proxmox/nodes/pve1/dns": { body: { servers: ["192.0.2.53"], search: "" } },
};

const managementSettings = {
  nodes: ["pve1"],
  imageDatastore: "local-lvm",
  vmDatastore: "local-lvm",
  bridge: "vmbr0",
  nodeAddresses: ["192.0.2.11/24"],
  apiAddress: "192.0.2.10",
  gateway: "192.0.2.1",
  dnsServers: ["192.0.2.53"],
  workloadDatastores: ["local-lvm"],
  workloadBridges: ["vmbr0"],
  bootstrapDatastore: "local-lvm",
};

const admin: Record<string, Route> = {
  "GET /api/setup": {
    body: {
      appliance: true,
      ready: true,
      allowed: true,
      backup: true,
      settings: managementSettings,
      applianceVMID: 105,
    },
  },
  "GET /api/setup/health": { body: { answers: true, nodes: [{ name: "mgmt-1", ready: true }], controllers: [] } },
  "GET /api/appliance/upgrades": {
    body: { version: "0.1.0", available: ["0.2.0"], settings: { policy: "off", hour: 3, timeZone: "UTC" } },
  },
  "GET /api/appliance/etcd-backups": { body: { targets: ["main"], keysStored: true } },
  "GET /api/appliance/etcd-backups/snapshots": { body: { snapshots: [] } },
  "GET /api/templates": { body: { templates: [] } },
  "GET /api/credentials/dns01": { body: { purpose: "dns01", set: false, canChange: true } },
  "GET /api/credentials/notify-webhook": { body: { purpose: "notify-webhook", set: false, canChange: true } },
  "GET /api/credentials/notify-ntfy": { body: { purpose: "notify-ntfy", set: false, canChange: true } },
  "GET /api/credentials/notify-email": { body: { purpose: "notify-email", set: false, canChange: true } },
  "GET /api/notifications": {
    body: [{ kind: "notify-webhook" }, { kind: "notify-ntfy" }, { kind: "notify-email" }],
  },
  "GET /api/activity?days=7": { body: activity },
  ...nodes,
  ...storages,
};
const create: Record<string, Route> = {
  "GET /api/classes": { body: [kubeadmClass] },
  "GET /api/versions/kubeadm": { body: { versions: ["v1.36.1", "v1.36.0"], stable: "v1.36.1" } },
  "GET /api/template-builds": { body: { allowed: true, requested: [], building: [] } },
  "GET /api/proxmox/ha-rules": { body: { readable: true, rules: [] } },
  ...nodes,
  ...storages,
};

const heading = (name: string | RegExp) => async () => screen.findByRole("heading", { name });
// loaded is any page that has left its skeleton behind.
const loaded = async (): Promise<void> => {
  await screen.findByRole("main");
  await waitFor(() => expect(document.querySelector(".skeleton, .loading")).toBeNull());
};
const text = (t: string | RegExp) => async () => screen.findAllByText(t);

const detail: Record<string, Route> = {
  "GET /api/clusters/default/web": { body: cluster("web") },
  "GET /api/clusters/default/web/health": { body: health },
  "GET /api/clusters/default/web/backups": { body: backups },
  "GET /api/clusters/default/web/etcd-snapshots": { body: etcd },
  "GET /api/clusters/default/web/activity?days=7": { body: activity },
  "GET /api/classes": { body: [kubeadmClass] },
  ...nodes,
  ...storages,
};

const snapshot = {
  name: "snap-1",
  time: when,
  kubernetesVersion: "v1.36.1",
  distribution: "kubeadm",
  revision: 12,
  etcdVersion: "3.6.1",
  size: 4_000_000,
  storedSize: 1_000_000,
};

// db is a cluster with something to show in every part: add-ons, faults, backups and snapshots.
const db = cluster("db", {
  stalled: "Waiting for a machine",
  conditions: [
    { type: "Available", status: "False", reason: "NotReady", message: "1 of 3 control plane nodes", since: when },
  ],
  controlPlane: { desired: 3, ready: 2, available: 2, upToDate: 2 },
  workers: { desired: 2, ready: 1, available: 1, upToDate: 1 },
  workerPools: [
    { name: "a", class: "default-worker", replicas: 1, machine: { cores: 4, memoryMiB: 8192, diskGiB: 50 } },
    { name: "b", class: "default-worker", replicas: 1, autoscaling: { min: 1, max: 3 } },
  ],
  headlamp: "https://192.0.2.30:4466/",
  pods: ["10.244.0.0/16"],
  services: ["10.96.0.0/12"],
  variables: {
    storage: "local-lvm",
    cni: "canal",
    cis: true,
    allowedNodes: ["pve1"],
    headlamp: { enabled: true, hostname: "headlamp.example.test" },
    monitoring: { enabled: true },
    logs: { enabled: true },
    rancher: { enabled: true, hostname: "rancher.example.test" },
    istio: { enabled: true },
    upgrades: { policy: "patch" },
    oidc: [{ issuerURL: "https://sso.example.test", clientID: "kubernetes", usernamePrefix: "oidc:" }],
    podSecurity: { enforce: "baseline" },
    loadBalancer: { enabled: true, ranges: ["192.0.2.100-192.0.2.110"] },
    csi: { enabled: true },
    controlPlaneMachine: { cores: 2, memoryMiB: 4096, diskGiB: 30 },
    workerMachine: { cores: 2, memoryMiB: 4096, diskGiB: 40 },
  },
  machines: [
    machine("db-cp-1", "control-plane"),
    {
      ...machine("db-cp-2", "control-plane"),
      ready: false,
      phase: "Provisioning",
      waiting: "Waiting for the VM",
      replacing: true,
    },
    { ...machine("db-w-1", "worker"), pool: "a" },
  ],
});

const attention = [
  {
    severity: "error",
    section: "nodes",
    what: "Node db-cp-2-node not ready",
    message: "Kubelet stopped",
    machine: "db-cp-2",
  },
  { severity: "warning", section: "pods", what: "Pod api restarting", pod: { namespace: "apps", name: "api-0" } },
  { severity: "progress", section: "pools", what: "Pool a rolling out" },
];

const richHealth = {
  ...health,
  verdict: "degraded",
  attention,
  nodes: section([
    {
      name: "db-cp-1-node",
      machine: "db-cp-1",
      role: "control-plane",
      joined: true,
      ready: true,
      pods: 12,
      cpu: { requested: 500, allocatable: 2000 },
      memory: { requested: 1 << 30, allocatable: 4 << 30 },
      vm: {
        id: 101,
        node: "pve1",
        status: "running",
        cpu: 0.2,
        cores: 2,
        memory: 1 << 30,
        maxMemory: 4 << 30,
        uptime: 3600,
        load: [0.1, null, 0.3],
      },
    },
    {
      name: "db-cp-2-node",
      machine: "db-cp-2",
      role: "control-plane",
      joined: true,
      ready: false,
      down: true,
      since: when,
      message: "Kubelet stopped",
      pressure: ["MemoryPressure"],
      unschedulable: true,
      taints: ["node.kubernetes.io/unreachable"],
      pods: 3,
    },
    { name: "db-w-1-node", machine: "db-w-1", role: "worker", pool: "a", joined: false, ready: false, pods: 0 },
  ]),
  pods: section([
    {
      namespace: "apps",
      name: "api-0",
      node: "db-w-1-node",
      severity: "warning",
      reason: "CrashLoopBackOff",
      message: "back-off",
      restarts: 7,
      since: when,
      containers: ["api"],
    },
  ]),
  volumes: section([{ namespace: "apps", name: "data", storageClass: "local-lvm", since: when }]),
  addons: section([
    {
      name: "headlamp",
      release: "headlamp",
      namespace: "headlamp",
      chart: "headlamp",
      version: "0.30.0",
      status: "deployed",
      revision: 1,
      ready: true,
    },
    {
      name: "monitoring",
      release: "monitoring",
      chart: "kube-prometheus-stack",
      status: "failed",
      ready: false,
      message: "timed out",
      since: when,
    },
  ]),
  pools: section([
    { name: "a", desired: 1, ready: 0, available: 0, upToDate: 0, rollingOut: true, message: "rolling" },
    { name: "b", desired: 1, ready: 1, available: 1, upToDate: 1, rollingOut: false, autoscaling: { min: 1, max: 3 } },
  ]),
  controlPlane: {
    kind: "KubeadmControlPlane",
    conditions: [{ type: "Ready", status: "False", reason: "Waiting", message: "waiting", since: when }],
  },
  healthChecks: section([
    { name: "workers", expected: 2, healthy: 1, allowed: true, message: "one unhealthy", remediating: ["db-w-1"] },
  ]),
  events: section([
    {
      source: "workload",
      type: "Warning",
      reason: "BackOff",
      message: "Back-off restarting",
      object: "Pod/api-0",
      count: 5,
      last: when,
    },
    {
      source: "management",
      type: "Normal",
      reason: "Scaled",
      message: "Scaled up",
      object: "Cluster/db",
      count: 1,
      last: when,
      action: "Scale",
    },
  ]),
  alerts: section([
    {
      name: "KubePodCrashLooping",
      severity: "warning",
      namespace: "apps",
      summary: "Pod is crash looping",
      since: when,
    },
  ]),
  vms: "3 VMs on pve1",
  logs: true,
};

const richBackups = {
  ...backups,
  lastBackup: when,
  backups: [
    {
      name: "b-1",
      phase: "Completed",
      scheduled: true,
      started: when,
      completed: when,
      expires: when,
      items: 40,
      total: 40,
      warnings: 1,
      errors: 0,
    },
    {
      name: "b-2",
      phase: "Failed",
      scheduled: false,
      by: "root@pam",
      started: when,
      items: 2,
      total: 40,
      warnings: 0,
      errors: 3,
      failure: "target unreachable",
    },
  ],
  restores: [
    {
      name: "r-1",
      backup: "b-1",
      phase: "PartiallyFailed",
      by: "root@pam",
      namespaces: ["apps"],
      volumeData: true,
      started: when,
      completed: when,
      items: 10,
      total: 12,
      warnings: 2,
      errors: 1,
      volumes: 2,
      volumesDone: 1,
      bytesDone: 1000,
      bytesTotal: 5000,
    },
  ],
  sources: [{ cluster: "db", phase: "Available", volumeData: true }],
  attention: [{ severity: "warning", section: "backups", what: "Last backup failed" }],
};

const richEtcd = {
  ...etcd,
  last: snapshot,
  snapshots: [snapshot, { ...snapshot, name: "snap-2", revision: 13 }],
  condition: { type: "Ready", status: "True", since: when },
};

const richActivity = {
  since: when,
  entries: [
    { at: when, user: "root@pam", what: "Scaled db", cluster: "default/db", details: { workers: "2" } },
    { at: when, user: "alice@pve", what: "Deleted backup b-3", cluster: "default/db", failed: true },
  ],
  events: section([
    {
      source: "management",
      type: "Normal",
      reason: "Scaled",
      message: "Scaled up",
      object: "Cluster/db",
      count: 1,
      last: when,
    },
  ]),
};

const richDetail: Record<string, Route> = {
  ...detail,
  "GET /api/clusters": { body: [cluster("web"), db, cluster("old", { deleting: true, phase: "Deleting" })] },
  "GET /api/clusters/default/db": { body: db },
  "GET /api/clusters/default/db/health": { body: richHealth },
  "GET /api/clusters/default/db/backups": { body: richBackups },
  "GET /api/clusters/default/db/etcd-snapshots": { body: richEtcd },
  "GET /api/clusters/default/db/activity?days=7": { body: richActivity },
  "GET /api/fleet/health": {
    body: [{ namespace: "default", name: "db", read: when, verdict: "degraded", attention, more: 2 }],
  },
};

const runs = [
  {
    id: "change-1",
    kind: "change",
    namespace: "default",
    cluster: "db",
    what: "Scale db",
    state: "stalled",
    phase: "Scaling",
    by: "root@pam",
    started: when,
    error: "Waiting for a machine",
    settings: ["workers"],
    eventsSince: when,
  },
  {
    id: "mgmt-1",
    kind: "management-cluster",
    what: "Apply management cluster",
    state: "failed",
    action: "apply",
    by: "root@pam",
    started: when,
    finished: when,
    error: "timed out",
    steps: [
      { name: "Create VMs", state: "done" },
      { name: "Bootstrap", state: "failed", detail: "timed out" },
      { name: "Add-ons", state: "waiting" },
    ],
  },
  { id: "tmpl-1", kind: "template", what: "Build template", state: "running", by: "root@pam", started: when },
  {
    id: "up-1",
    kind: "appliance-upgrade",
    what: "Upgrade appliance to 0.2.0",
    state: "succeeded",
    release: "0.2.0",
    started: when,
    finished: when,
  },
];

const template = {
  vmid: 9000,
  node: "pve1",
  name: "kh-kubeadm-1.36",
  tags: ["kubehenge"],
  line: "kubeadm v1.36",
  build: "1",
  built: when,
  newest: true,
  inUse: "default/web",
};

const richAdmin: Record<string, Route> = {
  ...admin,
  "GET /api/runs": { body: runs },
  "GET /api/clusters/default/db/events?since=2026-01-02T03%3A04%3A05Z": { body: richActivity.events.items },
  "GET /api/setup": {
    body: {
      appliance: true,
      ready: true,
      allowed: true,
      backup: true,
      settings: managementSettings,
      applianceVMID: 105,
      upgrade: {
        applied: "1.0.0",
        carried: "1.1.0",
        behind: true,
        changes: [{ component: "capmox", from: "1", to: "2" }],
      },
      job: {
        user: "root@pam",
        kind: "apply",
        started: when,
        finished: when,
        state: "failed",
        error: "timed out",
        log: ["step 1", "step 2"],
      },
    },
  },
  "GET /api/setup/health": {
    body: {
      answers: true,
      nodes: [{ name: "mgmt-1", ready: false, message: "down", kubelet: "v1.36.1" }],
      controllers: [{ namespace: "kubehenge", name: "controller", ready: 0, wanted: 1 }],
    },
  },
  "GET /api/appliance/upgrades": {
    body: {
      version: "0.1.0",
      available: ["0.2.0", "0.3.0"],
      error: "registry unreachable",
      settings: { policy: "patch", hour: 3, timeZone: "UTC" },
      status: { state: "failed", from: "0.1.0", to: "0.2.0", time: when, error: "checksum", rolledBack: true },
      management: { possible: true, on: true, by: "root@pam", since: when, user: "root@pam" },
    },
  },
  "GET /api/appliance/etcd-backups": {
    body: {
      settings: { target: "main", everyHours: 6, keep: 10, by: "root@pam", since: when },
      last: snapshot,
      next: when,
      targets: ["main"],
      keysStored: true,
    },
  },
  "GET /api/appliance/etcd-backups/snapshots": { body: { snapshots: [snapshot, { ...snapshot, name: "snap-2" }] } },
  "GET /api/templates": {
    body: {
      templates: [
        template,
        { ...template, vmid: 9001, name: "kh-talos", line: "talos", newest: false, inUse: undefined },
      ],
      job: { user: "root@pam", kind: "template", started: when, state: "running", log: ["building"] },
      refresh: {
        weekday: "Sun",
        time: "01:00",
        timeZone: "UTC",
        soakHours: 24,
        creatorBuilds: false,
        next: when,
        requested: ["kubeadm"],
        last: { state: "failed", started: when, finished: when, lines: ["a"], error: "no space", log: ["a", "b"] },
      },
    },
  },
  "GET /api/credentials/dns01": {
    body: { purpose: "dns01", set: true, setBy: "root@pam", setAt: when, canChange: true },
  },
  "GET /api/notifications": {
    body: [
      { kind: "notify-webhook", at: when, title: "web: VM paused" },
      { kind: "notify-ntfy" },
      { kind: "notify-email", at: when, title: "web: VM paused", error: "535 bad credentials" },
    ],
  },
  "GET /api/credentials/notify-webhook": {
    body: { purpose: "notify-webhook", set: true, setBy: "root@pam", setAt: when, canChange: true },
  },
  "GET /api/activity?days=7": { body: richActivity },
};

// tal is a Talos cluster on Cilium whose upgrade failed and whose etcd was restored.
const tal = cluster("tal", {
  class: "kubehenge-talos",
  distribution: "talos",
  talos: {
    version: "v1.12.0",
    image: "v1.11.0",
    failed: "tal-cp-1 to v1.12.0 at 2026-01-02T03:04:05Z: etcd unhealthy",
  },
  nodeOS: { family: "talos", release: "1", title: "Talos", cpuType: "x86-64-v2-AES", mac: "enforcing" },
  nodeImage: {
    refreshes: true,
    since: when,
    builds: [{ template: "kubehenge-talos-1", build: "build-202601020304", built: when }],
  },
  recovery: { snapshot: "snap-1", by: "root@pam", requestedAt: when, phase: "Failed", message: "etcd refused" },
  mesh: [{ cluster: "default/web", state: "Connected" }],
  gitops: { public: "ssh-ed25519 AAAA", fingerprint: "SHA256:abc" },
  variables: { cni: "cilium", rancher: { install: true, hostname: "r.example.test" }, certificates: { enabled: true } },
});

// gone is a cluster whose deletion waits on something.
const gone = cluster("gone", {
  deleting: true,
  phase: "Deleting",
  deletedAt: when,
  deletion: {
    waiting: [
      { kind: "Machine", name: "gone-cp-1", finalizers: ["machine.cluster.x-k8s.io"], reason: "VM not removed" },
    ],
    mayLeave: ["VM 120 on pve1"],
  },
});

const richer: Record<string, Route> = {
  ...richDetail,
  "GET /api/clusters": { body: [cluster("web"), db, tal, gone] },
  "GET /api/clusters/default/tal": { body: tal },
  "GET /api/clusters/default/tal/health": { body: health },
  "GET /api/clusters/default/tal/backups": { body: backups },
  "GET /api/clusters/default/tal/etcd-snapshots": { body: richEtcd },
  "GET /api/clusters/default/tal/activity?days=7": { body: activity },
  "GET /api/clusters/default/gone": { body: gone },
  "GET /api/clusters/default/gone/health": { body: health },
  "GET /api/clusters/default/gone/backups": { body: backups },
  "GET /api/clusters/default/gone/etcd-snapshots": { body: etcd },
  "GET /api/clusters/default/gone/activity?days=7": { body: activity },
  "GET /api/talos/releases": {
    body: {
      templates: ["v1.12.0", "v1.11.0"],
      releases: ["v1.12.0", "v1.11.0"],
      default: "v1.11.0",
      kubernetes: { "v1.12.0": ["1.35", "1.36"], "v1.11.0": ["1.34", "1.35", "1.36"] },
    },
  },
  "GET /api/versions/talos": { body: { versions: ["v1.36.1"], stable: "v1.36.1" } },
};

const backupTargets = JSON.stringify([{ name: "store", endpoint: "https://s3.example.test:8333", bucket: "backups" }]);
const backingClass = {
  ...kubeadmClass,
  name: "kubehenge-k3s",
  distribution: "k3s",
  annotations: { "kubehenge.dev/backup-targets": backupTargets, "kubehenge.dev/addon-memory": '{"backups":384}' },
  versionFormat: { pattern: String.raw`^v1\.\d+\.\d+\+k3s\d+$` },
  variables: [
    ...kubeadmClass.variables,
    { name: "backups", required: false, schema: object },
    { name: "etcdSnapshots", required: false, schema: object },
  ],
};
const bk = cluster("bk", {
  class: "kubehenge-k3s",
  distribution: "k3s",
  version: "v1.36.1+k3s1",
  variables: { backups: { enabled: false }, etcdSnapshots: { enabled: false } },
});
const bkRoutes: Record<string, Route> = {
  ...detail,
  "GET /api/classes": { body: [backingClass] },
  "GET /api/clusters": { body: [bk] },
  "GET /api/clusters/default/bk": { body: bk },
  "GET /api/clusters/default/bk/health": { body: health },
  "GET /api/clusters/default/bk/backups": { body: backups },
  "GET /api/clusters/default/bk/etcd-snapshots": { body: etcd },
  "GET /api/clusters/default/bk/activity?days=7": { body: activity },
  "GET /api/clusters/default/bk/credentials/backup": { body: { purpose: "backup", set: true, canChange: true } },
};

const machineDown = { ...machine("stuck-cp-b", "control-plane"), ready: false, waiting: "Waiting for a Node" };
const stuck = cluster("stuck", {
  controlPlane: { desired: 1, ready: 0, available: 0, upToDate: 0 },
  conditions: [
    {
      type: "ControlPlaneRollStalled",
      status: "True",
      reason: "APIUnreachable",
      message: "control-plane machine stuck-cp-b, which replaces stuck-cp-a, has not become ready in 25m0s",
      since: when,
    },
  ],
  machines: [machineDown, machine("stuck-cp-a", "control-plane")],
});
const stuckRoutes: Record<string, Route> = {
  ...detail,
  "GET /api/clusters": { body: [stuck] },
  "GET /api/clusters/default/stuck": { body: stuck },
  "GET /api/clusters/default/stuck/health": { body: health },
  "GET /api/clusters/default/stuck/backups": { body: backups },
  "GET /api/clusters/default/stuck/etcd-snapshots": { body: etcd },
  "GET /api/clusters/default/stuck/activity?days=7": { body: activity },
  "GET /api/clusters/default/stuck/control-plane-recovery?machine=stuck-cp-a": { body: { automatic: true } },
  "GET /api/clusters/default/stuck/control-plane-recovery?machine=stuck-cp-b": { body: { automatic: true } },
};

const waiting = cluster("waiting", {
  conditions: [
    { type: "Available", status: "True", since: when },
    { type: "AddonChangesPending", status: "True", reason: "WindowClosed", message: "2 changes wait", since: when },
    {
      type: "AddonConflict",
      status: "True",
      reason: "Installed",
      message: "cert-manager is already installed",
      since: when,
    },
  ],
  addonChanges: {
    opens: "2026-01-03T02:00:00Z",
    changes: [
      { addon: "monitoring", chart: "kube-prometheus-stack", from: "70.0.0", to: "71.0.0" },
      { addon: "headlamp", chart: "headlamp", from: "0.30.0", to: "0.30.0" },
    ],
  },
  variables: { namespaceLabels: { team: "payments" }, useExisting: { certManager: true } },
});
const waitingRoutes: Record<string, Route> = {
  ...detail,
  "GET /api/clusters": { body: [waiting] },
  "GET /api/clusters/default/waiting": { body: waiting },
  "GET /api/clusters/default/waiting/health": { body: health },
  "GET /api/clusters/default/waiting/backups": { body: backups },
  "GET /api/clusters/default/waiting/etcd-snapshots": { body: etcd },
  "GET /api/clusters/default/waiting/activity?days=7": { body: activity },
  "GET /api/versions/kubeadm": { body: { versions: ["v1.36.2"], stable: "v1.36.2" } },
  "POST /api/clusters/default/waiting/addon-changes": { status: 202 },
};

const oops = { status: 500, body: { error: "Disk on fire. Try again." } };
const failing = (key: string): Record<string, Route> => ({ [key]: oops });
const shown = text(/Disk on fire/u);

const cases: Case[] = [
  {
    name: "keyboard shortcuts and palette",
    path: "/",
    ready: text("web"),
    walk: async (check, user) => {
      await user.keyboard("?");
      await screen.findByRole("dialog", { name: "Keyboard shortcuts" });
      await check("Keyboard shortcuts");
      await user.click(screen.getByRole("button", { name: "Close" }));
      await user.keyboard("{Control>}k{/Control}");
      await screen.findByRole("dialog", { hidden: true });
      await settle(document.body);
      await check("Palette");
      await user.type(screen.getByRole("combobox"), "adm");
      await check("Palette, searched");
    },
  },
  {
    name: "staged changes and their plan",
    path: "/clusters/default/web/options",
    routes: { ...detail, "PATCH /api/clusters/default/web": { body: cluster("web") } },
    ready: loaded,
    walk: async (check, user) => {
      await user.click(await screen.findByRole("button", { name: "Edit Headlamp" }));
      await user.click(
        within(screen.getByRole("dialog", { name: "Edit Headlamp" })).getByLabelText("Install Headlamp"),
      );
      await user.click(
        within(screen.getByRole("dialog", { name: "Edit Headlamp" })).getByRole("button", { name: "Save" }),
      );
      await screen.findByRole("region", { name: "Staged changes" });
      await check("Staged changes bar");
      await user.click(screen.getByRole("button", { name: "Review plan" }));
      await screen.findByRole("dialog", { name: "Review plan" });
      await settle(document.body);
      await check("Review plan");
      await user.click(screen.getByRole("button", { name: "Close" }));
    },
  },
  {
    name: "options with etcd snapshots and backups",
    path: "/clusters/default/bk/options",
    routes: bkRoutes,
    ready: loaded,
  },
  { name: "stalled control plane", path: "/clusters/default/stuck", routes: stuckRoutes, ready: loaded },
  {
    name: "waiting add-on changes",
    path: "/clusters/default/waiting",
    routes: waitingRoutes,
    ready: loaded,
    walk: async (check, user) => {
      await screen.findByRole("group", { name: "Add-on changes" });
      await user.click(screen.getByRole("button", { name: "Upgrade" }));
      const dialog = await screen.findByRole("dialog", { name: "Upgrade waiting" });
      await within(dialog).findByRole("group", { name: "Add-on changes" });
      await check("Upgrade dialog with add-on changes");
      await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    },
  },
  {
    name: "options with namespace labels",
    path: "/clusters/default/waiting/options",
    routes: waitingRoutes,
    ready: loaded,
    walk: async (check, user) => {
      await user.click(await screen.findByRole("button", { name: "Edit namespace labels" }));
      const dialog = await screen.findByRole("dialog", { name: "Edit namespace labels" });
      await user.type(within(dialog).getByLabelText("Namespace labels"), "{Enter}broken");
      await check("Edit namespace labels with a problem");
      await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    },
  },
  {
    name: "guest agent restores",
    path: "/administration",
    routes: admin,
    ready: loaded,
    walk: async (check, user) => {
      await user.click(await screen.findByRole("button", { name: "Edit settings" }));
      await user.click(await screen.findByLabelText("Restore through guest agent"));
      await screen.findByRole("dialog", { name: "Allow guest agent restores" });
      await check("Allow guest agent restores");
      await user.click(
        within(screen.getByRole("dialog", { name: "Allow guest agent restores" })).getByRole("button", {
          name: "Cancel",
        }),
      );
    },
  },
  {
    name: "templates, free to delete",
    path: "/administration/templates",
    routes: { ...admin, "GET /api/templates": { body: { templates: [{ ...template, inUse: undefined }] } } },
    ready: loaded,
  },
  { name: "error: clusters", path: "/", routes: failing("GET /api/clusters"), ready: loaded },
  {
    name: "error: cluster",
    path: "/clusters/default/web",
    routes: { ...detail, ...failing("GET /api/clusters/default/web") },
    ready: shown,
  },
  {
    name: "error: health",
    path: "/clusters/default/web/health",
    routes: { ...detail, ...failing("GET /api/clusters/default/web/health") },
    ready: shown,
  },
  {
    name: "error: backups",
    path: "/clusters/default/web/backups",
    routes: { ...detail, ...failing("GET /api/clusters/default/web/backups") },
    ready: shown,
  },
  {
    name: "error: activity",
    path: "/clusters/default/web/activity",
    routes: { ...detail, ...failing("GET /api/clusters/default/web/activity?days=7") },
    ready: shown,
  },
  {
    name: "error: create cluster",
    path: "/clusters/new",
    routes: { ...create, ...failing("GET /api/classes") },
    ready: shown,
  },
  {
    name: "error: management",
    path: "/administration",
    routes: { ...admin, ...failing("GET /api/setup/health") },
    ready: shown,
  },
  {
    name: "error: appliance",
    path: "/administration/appliance",
    routes: { ...admin, ...failing("GET /api/appliance/upgrades") },
    ready: shown,
  },
  {
    name: "error: etcd backups",
    path: "/administration/backups",
    routes: { ...admin, ...failing("GET /api/appliance/etcd-backups") },
    ready: shown,
  },
  {
    name: "error: templates",
    path: "/administration/templates",
    routes: { ...admin, ...failing("GET /api/templates") },
    ready: shown,
  },
  {
    name: "error: certificates",
    path: "/administration/certificates",
    routes: { ...admin, ...failing("GET /api/credentials/dns01") },
    ready: shown,
  },
  {
    name: "error: notifications",
    path: "/administration/notifications",
    routes: { ...admin, ...failing("GET /api/notifications") },
    ready: shown,
  },
  {
    name: "error: activity log",
    path: "/administration/activity",
    routes: { ...admin, ...failing("GET /api/activity?days=7") },
    ready: shown,
  },
  { name: "error: run", path: "/runs/r1", routes: failing("GET /api/runs"), ready: loaded },
  {
    name: "error: sign in",
    path: "/",
    routes: { "GET /api/session": { status: 401, body: { error: "not signed in" } }, ...failing("GET /api/realms") },
    ready: heading(/Kubehenge/u),
  },
  { name: "talos cluster summary", path: "/clusters/default/tal", routes: richer, ready: loaded },
  { name: "talos cluster options", path: "/clusters/default/tal/options", routes: richer, ready: loaded },
  { name: "talos cluster backups", path: "/clusters/default/tal/backups", routes: richer, ready: loaded },
  { name: "cluster being deleted", path: "/clusters/default/gone", routes: richer, ready: loaded },
  {
    name: "run, stalled change",
    path: "/runs/change-1",
    routes: { ...richAdmin, ...richDetail, "GET /api/runs": { body: runs } },
    ready: loaded,
  },
  { name: "run, failed apply", path: "/runs/mgmt-1", routes: richAdmin, ready: loaded },
  { name: "run, running template", path: "/runs/tmpl-1", routes: richAdmin, ready: loaded },
  { name: "run, finished upgrade", path: "/runs/up-1", routes: richAdmin, ready: loaded },
  { name: "management, failed", path: "/administration", routes: richAdmin, ready: loaded },
  { name: "appliance, failed upgrade", path: "/administration/appliance", routes: richAdmin, ready: loaded },
  { name: "backups, snapshots", path: "/administration/backups", routes: richAdmin, ready: loaded },
  { name: "templates, built", path: "/administration/templates", routes: richAdmin, ready: loaded },
  { name: "certificates, token stored", path: "/administration/certificates", routes: richAdmin, ready: loaded },
  { name: "notifications, sent", path: "/administration/notifications", routes: richAdmin, ready: loaded },
  { name: "activity, entries", path: "/administration/activity", routes: richAdmin, ready: loaded },
  {
    name: "administrators only",
    path: "/administration",
    routes: { "GET /api/setup": { body: { appliance: true, ready: true, allowed: false } } },
    ready: heading("Administrators only"),
  },
  {
    name: "sign in",
    path: "/",
    routes: {
      "GET /api/session": { status: 401, body: { error: "not signed in" } },
      "GET /api/realms": {
        body: [
          { realm: "pve", type: "pve", default: true },
          { realm: "pam", type: "pam" },
        ],
      },
    },
    ready: heading(/Kubehenge/u),
  },
  {
    name: "first run",
    path: "/",
    routes: { "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } }, ...nodes, ...storages },
    ready: loaded,
  },
  {
    name: "first run, cluster without administrator",
    path: "/",
    routes: { "GET /api/setup": { body: { appliance: true, ready: false, allowed: false } } },
    ready: loaded,
  },
  { name: "clusters, with faults and a deletion", path: "/", routes: richDetail, ready: text("db") },
  { name: "rich cluster summary", path: "/clusters/default/db", routes: richDetail, ready: loaded },
  { name: "rich cluster health", path: "/clusters/default/db/health", routes: richDetail, ready: loaded },
  { name: "rich cluster machines", path: "/clusters/default/db/machines", routes: richDetail, ready: loaded },
  { name: "rich cluster backups", path: "/clusters/default/db/backups", routes: richDetail, ready: text("b-1") },
  { name: "rich cluster options", path: "/clusters/default/db/options", routes: richDetail, ready: loaded },
  { name: "rich cluster activity", path: "/clusters/default/db/activity", routes: richDetail, ready: loaded },
  { name: "clusters, populated", path: "/", ready: text("web"), chrome: true },
  {
    name: "clusters, empty",
    path: "/",
    routes: { "GET /api/clusters": { body: [] }, "GET /api/classes": { body: [] } },
    ready: heading(/Clusters/u),
  },
  { name: "cluster summary", path: "/clusters/default/web", routes: detail, ready: heading(/web/u) },
  { name: "cluster health", path: "/clusters/default/web/health", routes: detail, ready: loaded },
  { name: "cluster backups", path: "/clusters/default/web/backups", routes: detail, ready: loaded },
  { name: "cluster activity", path: "/clusters/default/web/activity", routes: detail, ready: loaded },
  { name: "no such page", path: "/nothing", ready: heading("No such page") },
  { name: "run", path: "/runs/r1", ready: loaded },
  { name: "administration, management", path: "/administration", routes: admin, ready: loaded },
  { name: "administration, appliance", path: "/administration/appliance", routes: admin, ready: loaded },
  { name: "administration, backups", path: "/administration/backups", routes: admin, ready: loaded },
  { name: "administration, templates", path: "/administration/templates", routes: admin, ready: loaded },
  { name: "administration, certificates", path: "/administration/certificates", routes: admin, ready: loaded },
  { name: "administration, notifications", path: "/administration/notifications", routes: admin, ready: loaded },
  { name: "administration, activity", path: "/administration/activity", routes: admin, ready: loaded },
  {
    name: "create cluster",
    path: "/clusters/new",
    routes: {
      ...create,
      "POST /api/proxmox/nodes/pve1/bridges/vmbr0/free": {
        body: { api: "192.0.2.30", nodes: "192.0.2.31-192.0.2.33" },
      },
      "POST /api/clusters?dryRun=All": { body: { manifest: "kind: Cluster\nmetadata:\n  name: lab\n" } },
    },
    ready: loaded,
    walk: async (check, user) => {
      await user.type(await screen.findByLabelText("Name"), "lab");
      await waitFor(() => expect(screen.getByRole("option", { name: /kubeadm/iu })).toBeInTheDocument());
      await user.selectOptions(screen.getByLabelText("Distribution"), "kubehenge-kubeadm");
      await user.type(screen.getByLabelText("Kubernetes version"), "v1.36.1");
      await check("General");
      await user.click(screen.getByRole("button", { name: "Next" }));
      await waitFor(() => expect(screen.getByLabelText("Bridge")).toHaveValue("vmbr0"));
      await user.type(screen.getByLabelText("Gateway"), "192.0.2.1");
      await user.type(screen.getByLabelText("DNS servers"), "192.0.2.53");
      await user.type(screen.getByLabelText("API address"), "192.0.2.30");
      await user.type(screen.getByLabelText("Node addresses"), "192.0.2.31-192.0.2.39");
      await settle(document.body);
      await check("Network");
      await user.click(screen.getByRole("button", { name: "Next" }));
      await check("Machines");
      await user.click(screen.getByRole("button", { name: "Next" }));
      await check("Add-ons");
      await user.click(screen.getByRole("checkbox", { name: "Use existing cert-manager" }));
      await user.type(screen.getByLabelText("Namespace labels"), "team");
      await check("Add-ons with a label problem");
      await user.clear(screen.getByLabelText("Namespace labels"));
      await user.click(screen.getByRole("button", { name: "Next" }));
      await check("Identity providers");
      await user.click(screen.getByRole("button", { name: "Next" }));
      await screen.findByLabelText("Before creating");
      await settle(document.body);
      await check("Confirm");
      await user.click(screen.getByRole("button", { name: "Cancel" }));
      await screen.findByRole("dialog", { name: "Discard new cluster" });
      await check("Discard dialog");
      await user.click(screen.getByRole("button", { name: "Keep editing" }));
    },
  },
  {
    name: "create cluster, nothing entered",
    path: "/clusters/new",
    routes: create,
    ready: loaded,
    walk: async (check, user) => {
      await user.click(screen.getByRole("button", { name: "Next" }));
      await check("General with problems");
    },
  },
  {
    name: "first run walk",
    path: "/",
    routes: { "GET /api/setup": { body: { appliance: true, ready: false, allowed: true } }, ...nodes, ...storages },
    ready: loaded,
    walk: async (check, user) => {
      const disks = await screen.findByLabelText("VM disks");
      await waitFor(() => expect(disks).toHaveValue("local-lvm"));
      await check("Placement");
      const next = async () => user.click(screen.getByRole("button", { name: "Next" }));
      await next();
      await waitFor(() => expect(screen.getByLabelText("DNS servers")).toHaveValue("192.0.2.53"));
      await next();
      await check("Network with problems");
      await user.type(screen.getByLabelText("Node addresses"), "192.0.2.11/24");
      await user.type(screen.getByLabelText("API address"), "192.0.2.10");
      await check("Network");
      await next();
      await check("Services");
      await next();
      await check("Add-ons");
      await next();
      await check("Confirm");
      await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple");
      await user.type(screen.getByLabelText("Repeat passphrase"), "correct horse battery staple");
      await check("Passphrase");
    },
  },
];

// settle waits for what a click set going: the skeletons gone and a moment for the next render.
async function settle(container: Element): Promise<void> {
  // A timer takes a callback, so a promise is the only way to wait on one.
  // oxlint-disable-next-line promise/avoid-new, no-promise-executor-return, typescript/strict-void-return
  await new Promise((resolve) => setTimeout(resolve, 30));
  await waitFor(() => expect(container.querySelector(".skeleton")).toBeNull());
}

// The titles of the dialogs the cases opened, which the last tests hold to the interface's list.
const opened = new Set<string>();
function noteDialogs(container: Element): void {
  for (const d of container.querySelectorAll("dialog[open]")) {
    opened.add(d.querySelector("h2")?.textContent ?? d.getAttribute("aria-label") ?? "untitled");
  }
}

const dialogsExpected = [
  /^Scale /u,
  /^Upgrade [a-z]+$/u,
  /^Delete cluster /u,
  /^Abandon .*deletion$/u,
  /^Add pool to /u,
  /^Resize .*control plane$/u,
  /^Resize pool /u,
  /^Delete pool /u,
  /^Boot log of /u,
  /^Reboot /u,
  /^Replace /u,
  /^Delete [a-z]+-w-1-node$/u,
  /^Logs of /u,
  /^Edit placement$/u,
  /^Edit load balancer$/u,
  /^Edit pod security$/u,
  /^Edit identity providers$/u,
  /^Edit Headlamp$/u,
  /^Edit monitoring$/u,
  /^Edit logs$/u,
  /^Edit Rancher$/u,
  /^Uninstall Rancher$/u,
  /^Edit GitOps$/u,
  /^Edit egress proxy$/u,
  /^Edit certificates$/u,
  /^Edit namespace labels$/u,
  /^Edit automatic upgrades$/u,
  /^Edit autoscaler$/u,
  /^Edit node OS$/u,
  /^Edit Cluster Mesh$/u,
  /^Edit etcd snapshots$/u,
  /^Edit backups$/u,
  /^Upgrade Talos on /u,
  /^Recover .* control plane$/u,
  /^Restore b-1$/u,
  /^Delete b-1$/u,
  /^Results of /u,
  /^Restore etcd from /u,
  /^Restore etcd snapshot$/u,
  /^Restoring etcd$/u,
  /^(?:Set up|Change) etcd backups$/u,
  /^Upgrade Kubehenge to /u,
  /^Upgrade management cluster/u,
  /^Apply management cluster$/u,
  /^Allow guest agent restores$/u,
  /^Build template$/u,
  /^Delete template /u,
  /^Remove Kubehenge$/u,
  /^Review plan$/u,
  /^Keyboard shortcuts$/u,
  /^Go to or run$/u,
  /^Discard new cluster$/u,
];

// The top bar and rail are the same on every page: the first case sweeps them, the rest sweep the page.
const sweepable = (container: Element, c: Case): HTMLButtonElement[] =>
  [...container.querySelectorAll(c.chrome === true ? "button" : "main button")].filter(
    (b): b is HTMLButtonElement => b instanceof HTMLButtonElement && !b.disabled && b.closest("dialog") === null,
  );

// changedParts gives the outermost elements a click's mutations touched. The page as a whole was
// checked once before the sweep, so each click only needs what it changed checked.
function changedParts(records: readonly MutationRecord[], container: Element): Element[] {
  const touched = new Set<Element>();
  for (const r of records) {
    const element = r.target instanceof Element ? r.target : r.target.parentElement;
    if (element?.isConnected === true && container.contains(element)) {
      touched.add(element);
    }
  }
  return [...touched].filter((e) => ![...touched].some((o) => o !== e && o.contains(e)));
}

// sweep clicks every button of the page in turn. What opens, a dialog or another state, is checked
// and put away again. A click that leaves the page, or changes its buttons, starts the page afresh.
async function sweep(c: Case, routes: Record<string, Route>, user: UserEvent): Promise<void> {
  let at = 0;
  for (;;) {
    window.history.replaceState(null, "", c.path);
    fakeServer(routes);
    const { container, unmount } = render(<App />);
    await c.ready();
    await settle(container);
    const count = sweepable(container, c).length;
    while (at < count) {
      const buttons = sweepable(container, c);
      const button = buttons[at];
      at += 1;
      if (button === undefined || buttons.length !== count) {
        break;
      }
      const words = button.textContent.trim();
      const label = words === "" ? (button.getAttribute("aria-label") ?? "button") : words;
      const records: MutationRecord[] = [];
      const watch = new MutationObserver((r) => {
        records.push(...r);
      });
      watch.observe(container, { subtree: true, childList: true, attributes: true, characterData: true });
      await user.click(button);
      await settle(container);
      records.push(...watch.takeRecords());
      watch.disconnect();
      const parts = changedParts(records, container);
      if (parts.length > 0) {
        await expectNoViolations(container, `${c.name}, after "${label}"`, parts);
      }
      const dialog = container.querySelector("dialog[open]");
      if (dialog !== null) {
        noteDialogs(container);
        const close = dialog.querySelector<HTMLElement>("button[aria-label='Close']");
        if (close === null) {
          break;
        }
        await user.click(close);
        await settle(container);
      }
      if (window.location.pathname !== c.path || sweepable(container, c).length !== count) {
        break;
      }
    }
    unmount();
    if (at >= count) {
      return;
    }
  }
}

// A case clicks every button of its page and runs every axe rule on what each click changed: about
// 20 runs of 150 ms on the slowest page here, 5 s to 6 s on this machine. A hosted CI runner takes
// three to four times as long, so the global 20 s leaves the slowest cases with no headroom.
describe("accessibility", { timeout: 60_000 }, () => {
  it.each(cases)("$name", async (c) => {
    const routes = { ...signedIn, ...c.routes };
    window.history.replaceState(null, "", c.path);
    const calls = fakeServer(routes);
    const view = render(<App />);
    await c.ready();
    await settle(view.container);
    const check = async (what: string): Promise<void> => {
      noteDialogs(view.container);
      await expectNoViolations(view.container, `${c.name}, ${what}`);
    };
    await check("page");
    const unrouted = [...new Set(calls.map((x) => x.key))].filter((k) => k.startsWith("GET ") && !(k in routes));
    if (unrouted.length > 0) {
      throw new Error(`${c.name} asked for routes the case does not give: ${unrouted.join(" | ")}`);
    }
    const user = userEvent.setup({ delay: null });
    await c.walk?.(check, user);
    view.unmount();
    await expect(sweep(c, routes, user)).resolves.toBeUndefined();
  });

  it("restoring etcd without a run page to follow", async () => {
    window.history.replaceState(null, "", "/");
    fakeServer({
      ...richAdmin,
      "POST /api/appliance/etcd-backups/snapshots/snap-1/restore": {
        status: 202,
        body: { user: "root@pam", kind: "etcd-restore", started: when, state: "running", log: [] },
      },
    });
    const user = userEvent.setup({ delay: null });
    const view = render(<ManagementBackups running={false} />);
    await user.click(await screen.findByRole("button", { name: "Restore from snap-1" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore etcd snapshot" });
    noteDialogs(view.container);
    await expectNoViolations(view.container, "Restore etcd snapshot");
    await user.type(within(dialog).getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(within(dialog).getByRole("button", { name: "Restore" }));
    await screen.findByRole("dialog", { name: "Restoring etcd" });
    noteDialogs(view.container);
    await expect(expectNoViolations(view.container, "Restoring etcd")).resolves.toBeUndefined();
  });

  // Runs last: every kind of dialog the interface has was opened by the cases above.
  it.each(dialogsExpected)("opened a dialog titled %s", (title) => {
    expect([...opened].filter((t) => title.test(t)).length).toBeGreaterThan(0);
  });
});
