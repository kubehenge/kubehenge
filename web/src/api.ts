import { clearPollCaches, keepCache } from "./poll-cache";
import type {
  BackupRun,
  Bridge,
  ChangeRequest,
  Cluster,
  ClusterBackups,
  ClusterClass,
  ClusterHealth,
  ClusterRef,
  ClusterEtcdSnapshots,
  CreateCheck,
  CreateRequest,
  EtcdRestorePlan,
  FleetHealth,
  FreeAddresses,
  HARules,
  Job,
  ManagementAutomatic,
  ManagementHealth,
  ManagementSettings,
  PodLogs,
  Plan,
  PveNode,
  Realm,
  RestoreRun,
  RunInfo,
  Session,
  SetupStatus,
  Storage,
  StoredCredential,
  TalosReleases,
  TemplateBuildRequest,
  TemplateBuilds,
  TemplateRefresh,
  TemplateRequest,
  Templates,
  Upgrades,
  Versions,
} from "./types";

export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

const httpUnauthorized = 401;

// The session's CSRF token, which every change has to carry in a header.
let csrf = "";

// Called when the server no longer knows the session, so the interface returns to sign-in and can
// say why.
let onSignedOut: (() => void) | undefined;

export function setSignedOutHandler(handler: () => void): void {
  onSignedOut = handler;
}

// The release that served the first answer; a different one later means the appliance was
// upgraded under this page, which then runs an old frontend against a new server.
let release = "";
let onUpgraded: ((version: string) => void) | undefined;

export function setUpgradedHandler(handler: (version: string) => void): void {
  onUpgraded = handler;
}

function noteRelease(res: Response): void {
  const v = res.headers.get("X-Kubehenge-Version");
  if (v === null || v === "") {
    return;
  }
  if (!release) {
    release = v;
  } else if (v !== release) {
    onUpgraded?.(v);
  }
}

/** The message for a request that got no answer at all, such as while the appliance restarts. */
export const noAnswer = "No answer from Kubehenge. Check the connection.";

async function fetchAnswer(path: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(path, init);
  } catch {
    // The browser's own text ("Failed to fetch", "NetworkError when…") tells users nothing.
    throw new ApiError(0, noAnswer);
  }
}

// The reads a signed-in page starts with. Each waits on the one before (the session, the setup
// status, then the fleet), so they are made together as the page loads, and the first read of each
// takes its answer.
const firstReads = ["/api/session", "/api/setup", "/api/clusters", "/api/fleet/health", "/api/runs"];

// An answer made before it was asked for is taken only this soon after, when it is still current.
const primedFor = 10_000;

const primed = keepCache(new Map<string, { at: number; answer: Promise<Response> }>());

/** primeFirstReads makes the first reads of a signed-in page at once, before the page asks. */
export function primeFirstReads(): void {
  const at = Date.now();
  for (const path of firstReads) {
    const answer = fetchAnswer(path, { method: "GET", credentials: "same-origin" });
    void unheard(answer);
    primed.set(path, { at, answer });
  }
}

// unheard waits on an answer the page may never ask for, signed out or still setting up, so that
// its failure goes unreported unless the page takes it.
async function unheard(answer: Promise<unknown>): Promise<void> {
  try {
    await answer;
  } catch {
    // The read that takes it reports it.
  }
}

function takePrimed(path: string): Promise<Response> | undefined {
  const p = primed.get(path);
  primed.delete(path);
  return p && Date.now() - p.at < primedFor ? p.answer : undefined;
}

// errorOf is the API's own message in a failed answer's body, or "" when the body is not its JSON
// error.
async function errorOf(res: Response): Promise<string> {
  try {
    const data: unknown = await res.json();
    if (typeof data === "object" && data !== null && "error" in data && typeof data.error === "string") {
      return data.error;
    }
  } catch {
    // The body was not JSON.
  }
  return "";
}

/** httpPreconditionFailed answers a change made against a cluster someone has changed since. */
export const httpPreconditionFailed = 412;

/** A cluster as the user was shown it, which a change to it is made against. */
export type ClusterShown = Pick<Cluster, "namespace" | "name" | "generation">;

// What a call sends: its JSON body, and headers besides those every call has.
interface Sent {
  body?: unknown;
  headers?: Record<string, string>;
}

// send makes one call of the API and returns its answer when it is a success; otherwise it throws
// the API's own message, and a lost session returns the interface to sign-in.
async function send(method: string, path: string, { body, headers: extra = {} }: Sent = {}): Promise<Response> {
  const headers: Record<string, string> = { ...extra };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (method !== "GET" && csrf) {
    headers["X-Kubehenge-CSRF"] = csrf;
  }
  const init: RequestInit = { method, headers, credentials: "same-origin" };
  if (body !== undefined) {
    init.body = JSON.stringify(body);
  }
  const res = await ((method === "GET" ? takePrimed(path) : undefined) ?? fetchAnswer(path, init));
  noteRelease(res);
  if (!res.ok) {
    const message = (await errorOf(res)) || res.statusText;
    if (res.status === httpUnauthorized && !path.startsWith("/api/session")) {
      onSignedOut?.();
    }
    throw new ApiError(res.status, message);
  }
  return res;
}

// request makes a call whose answer is JSON of type T. The server is the release this frontend came
// with (noteRelease tells when it no longer is), so its answers have the types it declares.
async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  return answerOf<T>(await send(method, path, { body }));
}

// requestAgainst is request for a change to a cluster, made against the generation of the cluster
// its user was shown: the server refuses it once someone else has changed the cluster since.
async function requestAgainst<T>(
  method: string,
  path: string,
  change: { body: ChangeRequest; generation: number },
): Promise<T> {
  return answerOf<T>(
    await send(method, path, { body: change.body, headers: { "X-Kubehenge-Generation": String(change.generation) } }),
  );
}

async function answerOf<T>(res: Response): Promise<T> {
  // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- the server's declared answer, as request says
  return (await res.json()) as T;
}

// act makes a call whose answer has no body.
async function act(method: string, path: string, body?: unknown): Promise<void> {
  await send(method, path, { body });
}

// download fetches a file the API hands out, such as a kubeconfig.
async function download(method: string, path: string, body?: unknown): Promise<Blob> {
  const res = await send(method, path, { body });
  return res.blob();
}

function remember(s: Session): Session {
  ({ csrf } = s);
  return s;
}

const clusterPath = (ns: string, name: string): string =>
  `/api/clusters/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`;

/**
 * etcdSnapshotURL is where a cluster's snapshot downloads decrypted. A download asks for no change,
 * so a plain link fetches it and the browser streams it to disk, whatever its size.
 */
export const etcdSnapshotURL = (ns: string, name: string, snapshot: string): string =>
  `${clusterPath(ns, name)}/etcd-snapshots/${encodeURIComponent(snapshot)}`;

// A cluster's credential, or with no cluster, the one for every cluster.
const credentialPath = (purpose: string, cluster?: ClusterRef): string =>
  cluster === undefined
    ? `/api/credentials/${encodeURIComponent(purpose)}`
    : `${clusterPath(cluster.namespace, cluster.name)}/credentials/${encodeURIComponent(purpose)}`;

export interface Api {
  realms: () => Promise<Realm[]>;
  session: () => Promise<Session>;
  signIn: (username: string, realm: string, password: string) => Promise<Session>;
  secondFactor: (kind: string, code: string) => Promise<Session>;
  signOut: () => Promise<void>;

  classes: () => Promise<ClusterClass[]>;
  clusters: () => Promise<Cluster[]>;
  cluster: (ns: string, name: string) => Promise<Cluster>;
  create: (req: CreateRequest) => Promise<Cluster>;
  /** checkCreate has the API server check a cluster without creating it, and gives it as YAML to apply. */
  checkCreate: (req: CreateRequest) => Promise<CreateCheck>;
  /** change is made against the cluster as its user was shown it; a newer generation refuses it. */
  change: (c: ClusterShown, req: ChangeRequest) => Promise<Cluster>;
  plan: (c: ClusterShown, req: ChangeRequest) => Promise<Plan>;
  /** remove deletes the cluster; confirm is its name as the user typed it. */
  remove: (ns: string, name: string, confirm: string) => Promise<void>;
  backups: (ns: string, name: string) => Promise<ClusterBackups>;
  backUp: (ns: string, name: string) => Promise<BackupRun>;
  /** restore brings back a backup: of the namespaces named, or of every one when none is. */
  restore: (ns: string, name: string, req: { backup: string; namespaces: string[] }) => Promise<RestoreRun>;
  etcdSnapshots: (ns: string, name: string) => Promise<ClusterEtcdSnapshots>;
  takeEtcdSnapshot: (ns: string, name: string) => Promise<void>;
  etcdRestorePlan: (ns: string, name: string, snapshot: string) => Promise<EtcdRestorePlan>;
  /**
   * restoreEtcd asks the controller to take the cluster's etcd back to a snapshot; confirm is the
   * cluster's name as the user typed it.
   */
  restoreEtcd: (ns: string, name: string, req: { snapshot: string; confirm: string }) => Promise<void>;

  setupStatus: () => Promise<SetupStatus>;
  setup: (settings: ManagementSettings, passphrase: string) => Promise<Job>;

  health: (ns: string, name: string) => Promise<ClusterHealth>;
  fleetHealth: () => Promise<FleetHealth[]>;
  runs: () => Promise<RunInfo[]>;
  podLogs: (cluster: ClusterRef, pod: { namespace: string; name: string }, container: string) => Promise<PodLogs>;
  kubeconfig: (ns: string, name: string) => Promise<Blob>;
  /** talosconfig is a Talos cluster's, which signs in to its nodes' Talos API. */
  talosconfig: (ns: string, name: string) => Promise<Blob>;
  /** retryTalosUpgrade takes back the failed node upgrade that holds a Talos cluster's others. */
  retryTalosUpgrade: (ns: string, name: string) => Promise<void>;
  /** replaceMachine marks a machine for its cluster's health check to replace. */
  replaceMachine: (ns: string, name: string, machine: string) => Promise<void>;
  /** keepMachine takes that mark back. */
  keepMachine: (ns: string, name: string, machine: string) => Promise<void>;
  /** deleteMachine deletes a worker, its pool shrinking by one. */
  deleteMachine: (ns: string, name: string, machine: string) => Promise<void>;
  ingressCA: (ns: string, name: string) => Promise<Blob>;
  storedCredential: (purpose: string, cluster?: ClusterRef) => Promise<StoredCredential>;
  storeCredential: (purpose: string, values: Record<string, string>, cluster?: ClusterRef) => Promise<StoredCredential>;
  removeCredential: (purpose: string, cluster?: ClusterRef) => Promise<void>;
  credential: (name: "kubeconfig" | "talosconfig", passphrase: string) => Promise<Blob>;
  removeManagement: (passphrase: string) => Promise<Job>;
  managementHealth: () => Promise<ManagementHealth>;
  upgrades: () => Promise<Upgrades>;
  upgrade: (version: string) => Promise<void>;
  /** nodeOS, such as almalinux-10, narrows kubeadm's versions to those with a template on it. */
  versions: (distribution: string, nodeOS?: string) => Promise<Versions>;
  talosReleases: () => Promise<TalosReleases>;
  templates: () => Promise<Templates>;
  createTemplate: (req: TemplateRequest) => Promise<Job>;
  deleteTemplate: (vmid: number) => Promise<void>;
  /** Has the weekly template build run now. */
  refreshTemplates: () => Promise<TemplateRefresh>;
  templateBuilds: () => Promise<TemplateBuilds>;
  /** Asks for a missing template, for one who may create clusters. */
  requestTemplateBuild: (req: TemplateBuildRequest) => Promise<void>;

  upgradeSettings: (settings: Upgrades["settings"]) => Promise<Upgrades["settings"]>;
  /** Lets the appliance keep the state passphrase and a token of its own to apply unattended. */
  automaticManagement: (passphrase: string) => Promise<ManagementAutomatic>;
  /** Deletes what automaticManagement kept, and its Proxmox VE user. */
  stopAutomaticManagement: () => Promise<ManagementAutomatic>;

  storages: () => Promise<Storage[]>;
  nodes: () => Promise<PveNode[]>;
  bridges: (node: string) => Promise<Bridge[]>;
  /** The DNS servers a node resolves with. */
  dns: (node: string) => Promise<{ servers: string[] }>;
  /** Free addresses on a bridge's network for a new cluster's API and nodes, where nodes is above 0, and which of check a host answers on. */
  freeAddresses: (
    node: string,
    bridge: string,
    req: { vlan: number; nodes: number; check: string[] },
  ) => Promise<FreeAddresses>;
  haRules: () => Promise<HARules>;
}

export const api: Api = {
  realms: () => request("GET", "/api/realms"),
  session: async () => remember(await request("GET", "/api/session")),
  signIn: async (username, realm, password) => {
    clearPollCaches();
    return remember(await request("POST", "/api/session", { username, realm, password }));
  },
  secondFactor: async (kind, code) => remember(await request("POST", "/api/session/second-factor", { kind, code })),
  signOut: async () => {
    clearPollCaches();
    await act("DELETE", "/api/session");
    csrf = "";
  },

  classes: () => request("GET", "/api/classes"),
  clusters: () => request("GET", "/api/clusters"),
  cluster: (ns, name) => request("GET", clusterPath(ns, name)),
  create: (req) => request("POST", "/api/clusters", req),
  checkCreate: (req) => request("POST", "/api/clusters?dryRun=All", req),
  change: (c, req) =>
    requestAgainst("PATCH", clusterPath(c.namespace, c.name), { body: req, generation: c.generation }),
  plan: (c, req) =>
    requestAgainst("POST", `${clusterPath(c.namespace, c.name)}/plan`, { body: req, generation: c.generation }),
  remove: (ns, name, confirm) => act("DELETE", clusterPath(ns, name), { confirm }),
  backups: (ns, name) => request("GET", `${clusterPath(ns, name)}/backups`),
  backUp: (ns, name) => request("POST", `${clusterPath(ns, name)}/backups`),
  restore: (ns, name, req) => request("POST", `${clusterPath(ns, name)}/restores`, req),
  etcdSnapshots: (ns, name) => request("GET", `${clusterPath(ns, name)}/etcd-snapshots`),
  takeEtcdSnapshot: (ns, name) => act("POST", `${clusterPath(ns, name)}/etcd-snapshots`),
  etcdRestorePlan: (ns, name, snapshot) => request("GET", `${etcdSnapshotURL(ns, name, snapshot)}/restore-plan`),
  restoreEtcd: (ns, name, req) => act("POST", `${clusterPath(ns, name)}/etcd-restores`, req),

  setupStatus: () => request("GET", "/api/setup"),
  setup: (settings, passphrase) => request("POST", "/api/setup", { settings, passphrase }),

  health: (ns, name) => request("GET", `${clusterPath(ns, name)}/health`),
  fleetHealth: () => request("GET", "/api/fleet/health"),
  runs: () => request("GET", "/api/runs"),
  podLogs: (cluster, pod, container) =>
    request(
      "GET",
      `${clusterPath(cluster.namespace, cluster.name)}/pods/${encodeURIComponent(pod.namespace)}/${encodeURIComponent(pod.name)}/logs?container=${encodeURIComponent(container)}`,
    ),
  kubeconfig: (ns, name) => download("GET", `${clusterPath(ns, name)}/kubeconfig`),
  talosconfig: (ns, name) => download("GET", `${clusterPath(ns, name)}/talosconfig`),
  retryTalosUpgrade: (ns, name) => act("DELETE", `${clusterPath(ns, name)}/talos-upgrade-failure`),
  replaceMachine: (ns, name, machine) =>
    act("PUT", `${clusterPath(ns, name)}/machines/${encodeURIComponent(machine)}/replacement`),
  keepMachine: (ns, name, machine) =>
    act("DELETE", `${clusterPath(ns, name)}/machines/${encodeURIComponent(machine)}/replacement`),
  deleteMachine: (ns, name, machine) =>
    act("DELETE", `${clusterPath(ns, name)}/machines/${encodeURIComponent(machine)}`),
  ingressCA: (ns, name) => download("GET", `${clusterPath(ns, name)}/certificates/ca`),
  storedCredential: (purpose, cluster) => request("GET", credentialPath(purpose, cluster)),
  storeCredential: (purpose, values, cluster) => request("PUT", credentialPath(purpose, cluster), { values }),
  removeCredential: (purpose, cluster) => act("DELETE", credentialPath(purpose, cluster)),
  credential: (name, passphrase) => download("POST", "/api/setup/credential", { name, passphrase }),
  removeManagement: (passphrase) => request("POST", "/api/setup/remove", { passphrase, confirm: "remove" }),
  managementHealth: () => request("GET", "/api/setup/health"),
  upgrades: () => request("GET", "/api/appliance/upgrades"),
  upgrade: (version) => act("POST", "/api/appliance/upgrades", { version }),
  versions: (distribution, nodeOS) =>
    request(
      "GET",
      `/api/versions/${encodeURIComponent(distribution)}${nodeOS === undefined ? "" : `?nodeOS=${encodeURIComponent(nodeOS)}`}`,
    ),
  talosReleases: () => request("GET", "/api/talos/releases"),
  templates: () => request("GET", "/api/templates"),
  createTemplate: (req) => request("POST", "/api/templates", req),
  deleteTemplate: (vmid) => act("DELETE", `/api/templates/${vmid}`),
  refreshTemplates: () => request("POST", "/api/templates/refresh"),
  templateBuilds: () => request("GET", "/api/template-builds"),
  requestTemplateBuild: (req) => act("POST", "/api/template-builds", req),

  upgradeSettings: (settings) => request("PUT", "/api/appliance/upgrades/settings", settings),
  automaticManagement: (passphrase) => request("PUT", "/api/appliance/upgrades/management", { passphrase }),
  stopAutomaticManagement: () => request("DELETE", "/api/appliance/upgrades/management"),

  storages: () => request("GET", "/api/proxmox/storages"),
  nodes: () => request("GET", "/api/proxmox/nodes"),
  bridges: (node) => request("GET", `/api/proxmox/nodes/${encodeURIComponent(node)}/bridges`),
  dns: (node) => request("GET", `/api/proxmox/nodes/${encodeURIComponent(node)}/dns`),
  freeAddresses: (node, bridge, req) =>
    request("POST", `/api/proxmox/nodes/${encodeURIComponent(node)}/bridges/${encodeURIComponent(bridge)}/free`, req),
  haRules: () => request("GET", "/api/proxmox/ha-rules"),
};

/** dnsOf lists the DNS servers a node resolves with; a node whose servers cannot be read offers none. */
export async function dnsOf(node: string): Promise<string[]> {
  try {
    const { servers } = await api.dns(node);
    return servers;
  } catch {
    return [];
  }
}

/** bridgesOf lists a node's bridges; a node whose bridges cannot be read offers none. */
export async function bridgesOf(node: string): Promise<Bridge[]> {
  try {
    return await api.bridges(node);
  } catch {
    return [];
  }
}
