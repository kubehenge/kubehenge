import { clearPollCaches, keepCache } from "./poll-cache";
import type {
  BackupRun,
  BootLog,
  Bridge,
  ChangeRequest,
  Cluster,
  ClusterBackups,
  ClusterClass,
  ClusterHealth,
  ClusterRef,
  ClusterEtcdSnapshots,
  CreateCheck,
  ControlPlaneRecovery,
  CreateRequest,
  EtcdRestorePlan,
  FleetHealth,
  HealthEvent,
  FreeAddresses,
  HARules,
  Job,
  ManagementAutomatic,
  ManagementHealth,
  ManagementBackupRequest,
  ManagementBackups,
  ManagementSettings,
  ManagementSnapshots,
  NotificationSent,
  PodLogs,
  Plan,
  PveNode,
  Realm,
  RestoreRequest,
  RestoreResults,
  VeleroLog,
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
  /** The id the interface gave the request, which its journal lines carry; "" when none came. */
  readonly requestId: string;

  constructor(status: number, message: string, requestId = "") {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.requestId = requestId;
  }
}

/** referenceMark sets a request's id apart from the message an error shows it under. */
export const referenceMark = "\nReference: ";

const httpUnauthorized = 401;

// The session's CSRF token, which every change has to carry in a header.
let csrf = "";

// Called when the server no longer knows the session, so the interface returns to sign-in and can
// say why.
let onSignedOut: (() => void) | undefined;

export function setSignedOutHandler(handler: () => void): void {
  onSignedOut = handler;
}

// The release that served the first answer. A different one later means the appliance was
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

// readDeadline bounds a read: one the network loses would otherwise hold a page's polling, which
// asks again only once the read before has answered.
const readDeadline = 30_000;

// deadline is a signal that aborts a request once it has run for readDeadline.
function deadline(): { signal: AbortSignal; clear: () => void } {
  const ctl = new AbortController();
  const t = setTimeout(() => ctl.abort(), readDeadline);
  return { signal: ctl.signal, clear: () => clearTimeout(t) };
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
    const answer = fetchAnswer(path, { method: "GET", credentials: "same-origin", signal: deadline().signal });
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
// error, and the request's id, from the body or else the header.
async function errorOf(res: Response): Promise<{ message: string; requestId: string }> {
  const found = { message: "", requestId: res.headers.get("X-Request-Id") ?? "" };
  try {
    const data: unknown = await res.json();
    if (typeof data === "object" && data !== null && "error" in data && typeof data.error === "string") {
      found.message = data.error;
      if ("requestId" in data && typeof data.requestId === "string" && data.requestId !== "") {
        found.requestId = data.requestId;
      }
    }
  } catch {
    // The body was not JSON.
  }
  return found;
}

/** httpPreconditionFailed answers a change made against a cluster someone has changed since. */
export const httpPreconditionFailed = 412;

/** A cluster as the user was shown it, which a change to it is made against. */
export type ClusterShown = Pick<Cluster, "namespace" | "name" | "generation">;

// What a call sends: its JSON body, and headers besides those every call has.
interface Sent {
  body?: unknown;
  headers?: Record<string, string>;
  signal?: AbortSignal;
}

// send makes one call of the API and returns its answer when it is a success. Otherwise it throws
// the API's own message, and a lost session returns the interface to sign-in.
async function send(method: string, path: string, { body, headers: extra = {}, signal }: Sent = {}): Promise<Response> {
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
  if (signal !== undefined) {
    init.signal = signal;
  }
  const res = await ((method === "GET" ? takePrimed(path) : undefined) ?? fetchAnswer(path, init));
  noteRelease(res);
  if (!res.ok) {
    const { message, requestId } = await errorOf(res);
    if (res.status === httpUnauthorized && !path.startsWith("/api/session")) {
      onSignedOut?.();
    }
    throw new ApiError(res.status, message || res.statusText, requestId);
  }
  return res;
}

// request makes a call whose answer is JSON of type T. The server is the release this frontend came
// with (noteRelease tells when it no longer is), so its answers have the types it declares.
async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  if (method !== "GET") {
    return answerOf<T>(await send(method, path, { body }));
  }
  const d = deadline();
  try {
    return await answerOf<T>(await send(method, path, { signal: d.signal }));
  } catch (err) {
    throw d.signal.aborted ? new ApiError(0, noAnswer) : err;
  } finally {
    d.clear();
  }
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
  /** change is made against the cluster as its user was shown it. A newer generation refuses it. */
  change: (c: ClusterShown, req: ChangeRequest) => Promise<Cluster>;
  plan: (c: ClusterShown, req: ChangeRequest) => Promise<Plan>;
  /** remove deletes the cluster. confirm is its name as the user typed it. */
  remove: (ns: string, name: string, confirm: string) => Promise<void>;
  backups: (ns: string, name: string) => Promise<ClusterBackups>;
  backUp: (ns: string, name: string) => Promise<BackupRun>;
  /** deleteBackup has Velero delete a backup before it runs out, once its name is typed to confirm. */
  deleteBackup: (ns: string, name: string, typed: { backup: string; confirm: string }) => Promise<BackupRun>;
  /** restore brings back a backup into the cluster, whole or what the request picks of it. */
  restore: (ns: string, name: string, req: RestoreRequest) => Promise<RestoreRun>;
  restoreResults: (ns: string, name: string, restore: string) => Promise<RestoreResults>;
  /** backupLog and restoreLog are the end of a finished backup's or restore's log. */
  backupLog: (ns: string, name: string, backup: string) => Promise<VeleroLog>;
  restoreLog: (ns: string, name: string, restore: string) => Promise<VeleroLog>;
  etcdSnapshots: (ns: string, name: string) => Promise<ClusterEtcdSnapshots>;
  takeEtcdSnapshot: (ns: string, name: string) => Promise<void>;
  etcdRestorePlan: (ns: string, name: string, snapshot: string) => Promise<EtcdRestorePlan>;
  /**
   * restoreEtcd asks the controller to take the cluster's etcd back to a snapshot. confirm is the
   * cluster's name as the user typed it.
   */
  restoreEtcd: (ns: string, name: string, req: { snapshot: string; confirm: string }) => Promise<void>;
  /** controlPlaneRecovery says how the control plane is recovered keeping machine: by the controller, or by hand. */
  controlPlaneRecovery: (ns: string, name: string, machine: string) => Promise<ControlPlaneRecovery>;
  /** recoverControlPlane asks the controller to reset etcd to machine's own and replace the other control planes. */
  recoverControlPlane: (cluster: ClusterRef, ask: { machine: string; confirm: string }) => Promise<void>;
  /** abandonDeletion has the controller give up on what the cluster's deletion waits for. */
  abandonDeletion: (cluster: ClusterRef, confirm: string) => Promise<void>;

  setupStatus: () => Promise<SetupStatus>;
  setup: (settings: ManagementSettings, passphrase: string) => Promise<Job>;

  health: (ns: string, name: string) => Promise<ClusterHealth>;
  /** runEvents are the management cluster's events about a cluster's objects since a time, oldest first. */
  runEvents: (ns: string, name: string, since: string) => Promise<HealthEvent[]>;
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
  /** cordonMachine keeps new pods off a machine's node. */
  cordonMachine: (ns: string, name: string, machine: string) => Promise<void>;
  /** uncordonMachine lets pods onto a machine's node again. */
  uncordonMachine: (ns: string, name: string, machine: string) => Promise<void>;
  /** rebootMachine has Proxmox VE shut a machine's VM down and start it again. */
  rebootMachine: (ns: string, name: string, machine: string) => Promise<void>;
  /** supportBundle is a cluster's .tar.gz of its objects, events and the logs naming them. */
  supportBundle: (ns: string, name: string) => Promise<Blob>;
  bootLog: (ns: string, name: string, machine: string) => Promise<BootLog>;
  /** askBootLog has the controller read a machine's boot log again. */
  askBootLog: (ns: string, name: string, machine: string) => Promise<BootLog>;
  ingressCA: (ns: string, name: string) => Promise<Blob>;
  storedCredential: (purpose: string, cluster?: ClusterRef) => Promise<StoredCredential>;
  storeCredential: (purpose: string, values: Record<string, string>, cluster?: ClusterRef) => Promise<StoredCredential>;
  removeCredential: (purpose: string, cluster?: ClusterRef) => Promise<void>;
  notifications: () => Promise<NotificationSent[]>;
  testNotifications: () => Promise<NotificationSent[]>;
  credential: (name: "kubeconfig" | "talosconfig", passphrase: string) => Promise<Blob>;
  removeManagement: (passphrase: string) => Promise<Job>;
  managementHealth: () => Promise<ManagementHealth>;
  managementBackups: () => Promise<ManagementBackups>;
  configureManagementBackups: (req: ManagementBackupRequest) => Promise<ManagementBackups>;
  stopManagementBackups: () => Promise<void>;
  managementSnapshots: () => Promise<ManagementSnapshots>;
  takeManagementSnapshot: () => Promise<ManagementBackups>;
  /** restoreManagement restores the management cluster's etcd as a run of the appliance's. */
  restoreManagement: (snapshot: string, passphrase: string) => Promise<Job>;
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
  deleteBackup: (ns, name, { backup, confirm }) =>
    request("DELETE", `${clusterPath(ns, name)}/backups/${encodeURIComponent(backup)}`, { confirm }),
  restore: (ns, name, req) => request("POST", `${clusterPath(ns, name)}/restores`, req),
  restoreResults: (ns, name, restore) =>
    request("GET", `${clusterPath(ns, name)}/restores/${encodeURIComponent(restore)}/results`),
  backupLog: (ns, name, backup) => request("GET", `${clusterPath(ns, name)}/backups/${encodeURIComponent(backup)}/log`),
  restoreLog: (ns, name, restore) =>
    request("GET", `${clusterPath(ns, name)}/restores/${encodeURIComponent(restore)}/log`),
  etcdSnapshots: (ns, name) => request("GET", `${clusterPath(ns, name)}/etcd-snapshots`),
  takeEtcdSnapshot: (ns, name) => act("POST", `${clusterPath(ns, name)}/etcd-snapshots`),
  etcdRestorePlan: (ns, name, snapshot) => request("GET", `${etcdSnapshotURL(ns, name, snapshot)}/restore-plan`),
  restoreEtcd: (ns, name, req) => act("POST", `${clusterPath(ns, name)}/etcd-restores`, req),
  controlPlaneRecovery: (ns, name, machine) =>
    request("GET", `${clusterPath(ns, name)}/control-plane-recovery?machine=${encodeURIComponent(machine)}`),
  recoverControlPlane: (cluster, ask) =>
    act("POST", `${clusterPath(cluster.namespace, cluster.name)}/control-plane-recovery`, ask),
  abandonDeletion: (cluster, confirm) =>
    act("POST", `${clusterPath(cluster.namespace, cluster.name)}/abandon-deletion`, { confirm }),

  setupStatus: () => request("GET", "/api/setup"),
  setup: (settings, passphrase) => request("POST", "/api/setup", { settings, passphrase }),

  health: (ns, name) => request("GET", `${clusterPath(ns, name)}/health`),
  runEvents: (ns, name, since) => request("GET", `${clusterPath(ns, name)}/events?since=${encodeURIComponent(since)}`),
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
  cordonMachine: (ns, name, machine) =>
    act("PUT", `${clusterPath(ns, name)}/machines/${encodeURIComponent(machine)}/cordon`),
  uncordonMachine: (ns, name, machine) =>
    act("DELETE", `${clusterPath(ns, name)}/machines/${encodeURIComponent(machine)}/cordon`),
  rebootMachine: (ns, name, machine) =>
    act("POST", `${clusterPath(ns, name)}/machines/${encodeURIComponent(machine)}/reboot`),
  supportBundle: (ns, name) => download("GET", `${clusterPath(ns, name)}/support-bundle`),
  bootLog: (ns, name, machine) =>
    request("GET", `${clusterPath(ns, name)}/machines/${encodeURIComponent(machine)}/boot-log`),
  askBootLog: (ns, name, machine) =>
    request("POST", `${clusterPath(ns, name)}/machines/${encodeURIComponent(machine)}/boot-log`),
  ingressCA: (ns, name) => download("GET", `${clusterPath(ns, name)}/certificates/ca`),
  storedCredential: (purpose, cluster) => request("GET", credentialPath(purpose, cluster)),
  storeCredential: (purpose, values, cluster) => request("PUT", credentialPath(purpose, cluster), { values }),
  removeCredential: (purpose, cluster) => act("DELETE", credentialPath(purpose, cluster)),
  notifications: () => request("GET", "/api/notifications"),
  testNotifications: () => request("POST", "/api/notifications/test"),
  credential: (name, passphrase) => download("POST", "/api/setup/credential", { name, passphrase }),
  removeManagement: (passphrase) => request("POST", "/api/setup/remove", { passphrase, confirm: "remove" }),
  managementHealth: () => request("GET", "/api/setup/health"),
  managementBackups: () => request("GET", "/api/appliance/etcd-backups"),
  configureManagementBackups: (req) => request("PUT", "/api/appliance/etcd-backups", req),
  stopManagementBackups: () => act("DELETE", "/api/appliance/etcd-backups"),
  managementSnapshots: () => request("GET", "/api/appliance/etcd-backups/snapshots"),
  takeManagementSnapshot: () => request("POST", "/api/appliance/etcd-backups/snapshots"),
  restoreManagement: (snapshot, passphrase) =>
    request("POST", `/api/appliance/etcd-backups/snapshots/${encodeURIComponent(snapshot)}/restore`, { passphrase }),
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

/** dnsOf lists the DNS servers a node resolves with. A node whose servers cannot be read offers none. */
export async function dnsOf(node: string): Promise<string[]> {
  try {
    const { servers } = await api.dns(node);
    return servers;
  } catch {
    return [];
  }
}

/** bridgesOf lists a node's bridges. A node whose bridges cannot be read offers none. */
export async function bridgesOf(node: string): Promise<Bridge[]> {
  try {
    return await api.bridges(node);
  } catch {
    return [];
  }
}
