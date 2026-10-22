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
  CreateRequest,
  EtcdRestorePlan,
  HARules,
  Job,
  ManagementAutomatic,
  ManagementSettings,
  PodLogs,
  PveNode,
  Realm,
  RestoreRun,
  Session,
  SetupStatus,
  Storage,
  StoredCredential,
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
export const noAnswer = "Kubehenge did not answer. Check the connection; the page tries again on its own.";

async function fetchAnswer(path: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(path, init);
  } catch {
    // The browser's own text ("Failed to fetch", "NetworkError when…") tells users nothing.
    throw new ApiError(0, noAnswer);
  }
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

// send makes one call of the API and returns its answer when it is a success; otherwise it throws
// the API's own message, and a lost session returns the interface to sign-in.
async function send(method: string, path: string, body?: unknown): Promise<Response> {
  const headers: Record<string, string> = {};
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
  const res = await fetchAnswer(path, init);
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
  const res = await send(method, path, body);
  // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- the server's declared answer, as above
  return (await res.json()) as T;
}

// act makes a call whose answer has no body.
async function act(method: string, path: string, body?: unknown): Promise<void> {
  await send(method, path, body);
}

// download fetches a file the API hands out, such as a kubeconfig.
async function download(method: string, path: string, body?: unknown): Promise<Blob> {
  const res = await send(method, path, body);
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
  change: (ns: string, name: string, req: ChangeRequest) => Promise<Cluster>;
  remove: (ns: string, name: string) => Promise<void>;
  backups: (ns: string, name: string) => Promise<ClusterBackups>;
  backUp: (ns: string, name: string) => Promise<BackupRun>;
  /** restore brings back a backup: of the namespaces named, or of every one when none is. */
  restore: (ns: string, name: string, req: { backup: string; namespaces: string[] }) => Promise<RestoreRun>;
  etcdSnapshots: (ns: string, name: string) => Promise<ClusterEtcdSnapshots>;
  takeEtcdSnapshot: (ns: string, name: string) => Promise<void>;
  etcdRestorePlan: (ns: string, name: string, snapshot: string) => Promise<EtcdRestorePlan>;
  /** restoreEtcd asks the controller to take the cluster's etcd back to a snapshot. */
  restoreEtcd: (ns: string, name: string, snapshot: string) => Promise<void>;

  setupStatus: () => Promise<SetupStatus>;
  /** rootPassword is root@pam's for volume snapshots; empty keeps the one stored. */
  setup: (settings: ManagementSettings, passphrase: string, rootPassword?: string) => Promise<Job>;

  health: (ns: string, name: string) => Promise<ClusterHealth>;
  podLogs: (cluster: ClusterRef, pod: { namespace: string; name: string }, container: string) => Promise<PodLogs>;
  kubeconfig: (ns: string, name: string) => Promise<Blob>;
  ingressCA: (ns: string, name: string) => Promise<Blob>;
  storedCredential: (purpose: string, cluster?: ClusterRef) => Promise<StoredCredential>;
  storeCredential: (purpose: string, values: Record<string, string>, cluster?: ClusterRef) => Promise<StoredCredential>;
  removeCredential: (purpose: string, cluster?: ClusterRef) => Promise<void>;
  credential: (name: "kubeconfig" | "talosconfig", passphrase: string) => Promise<Blob>;
  removeManagement: (passphrase: string) => Promise<Job>;
  upgrades: () => Promise<Upgrades>;
  upgrade: (version: string) => Promise<void>;
  versions: (distribution: string) => Promise<Versions>;
  templates: () => Promise<Templates>;
  createTemplate: (req: TemplateRequest) => Promise<Job>;
  deleteTemplate: (vmid: number) => Promise<void>;
  /** Has the weekly template build run now. */
  refreshTemplates: () => Promise<TemplateRefresh>;
  templateBuilds: () => Promise<TemplateBuilds>;
  /** Asks for a missing template, for one who may create clusters. */
  requestTemplateBuild: (distribution: string, kubernetesVersion?: string) => Promise<void>;

  upgradeSettings: (settings: Upgrades["settings"]) => Promise<Upgrades["settings"]>;
  /** Lets the appliance keep the state passphrase and a token of its own to apply unattended. */
  automaticManagement: (passphrase: string) => Promise<ManagementAutomatic>;
  /** Deletes what automaticManagement kept, and its Proxmox VE user. */
  stopAutomaticManagement: () => Promise<ManagementAutomatic>;

  storages: () => Promise<Storage[]>;
  nodes: () => Promise<PveNode[]>;
  bridges: (node: string) => Promise<Bridge[]>;
  haRules: () => Promise<HARules>;
}

export const api: Api = {
  realms: () => request("GET", "/api/realms"),
  session: async () => remember(await request("GET", "/api/session")),
  signIn: async (username, realm, password) =>
    remember(await request("POST", "/api/session", { username, realm, password })),
  secondFactor: async (kind, code) => remember(await request("POST", "/api/session/second-factor", { kind, code })),
  signOut: async () => {
    await act("DELETE", "/api/session");
    csrf = "";
  },

  classes: () => request("GET", "/api/classes"),
  clusters: () => request("GET", "/api/clusters"),
  cluster: (ns, name) => request("GET", clusterPath(ns, name)),
  create: (req) => request("POST", "/api/clusters", req),
  change: (ns, name, req) => request("PATCH", clusterPath(ns, name), req),
  remove: (ns, name) => act("DELETE", clusterPath(ns, name)),
  backups: (ns, name) => request("GET", `${clusterPath(ns, name)}/backups`),
  backUp: (ns, name) => request("POST", `${clusterPath(ns, name)}/backups`),
  restore: (ns, name, req) => request("POST", `${clusterPath(ns, name)}/restores`, req),
  etcdSnapshots: (ns, name) => request("GET", `${clusterPath(ns, name)}/etcd-snapshots`),
  takeEtcdSnapshot: (ns, name) => act("POST", `${clusterPath(ns, name)}/etcd-snapshots`),
  etcdRestorePlan: (ns, name, snapshot) => request("GET", `${etcdSnapshotURL(ns, name, snapshot)}/restore-plan`),
  restoreEtcd: (ns, name, snapshot) => act("POST", `${clusterPath(ns, name)}/etcd-restores`, { snapshot }),

  setupStatus: () => request("GET", "/api/setup"),
  setup: (settings, passphrase, rootPassword) =>
    request("POST", "/api/setup", {
      settings,
      passphrase,
      ...(rootPassword !== undefined && rootPassword !== "" && { rootPassword }),
    }),

  health: (ns, name) => request("GET", `${clusterPath(ns, name)}/health`),
  podLogs: (cluster, pod, container) =>
    request(
      "GET",
      `${clusterPath(cluster.namespace, cluster.name)}/pods/${encodeURIComponent(pod.namespace)}/${encodeURIComponent(pod.name)}/logs?container=${encodeURIComponent(container)}`,
    ),
  kubeconfig: (ns, name) => download("GET", `${clusterPath(ns, name)}/kubeconfig`),
  ingressCA: (ns, name) => download("GET", `${clusterPath(ns, name)}/certificates/ca`),
  storedCredential: (purpose, cluster) => request("GET", credentialPath(purpose, cluster)),
  storeCredential: (purpose, values, cluster) => request("PUT", credentialPath(purpose, cluster), { values }),
  removeCredential: (purpose, cluster) => act("DELETE", credentialPath(purpose, cluster)),
  credential: (name, passphrase) => download("POST", "/api/setup/credential", { name, passphrase }),
  removeManagement: (passphrase) => request("POST", "/api/setup/remove", { passphrase, confirm: "remove" }),
  upgrades: () => request("GET", "/api/appliance/upgrades"),
  upgrade: (version) => act("POST", "/api/appliance/upgrades", { version }),
  versions: (distribution) => request("GET", `/api/versions/${encodeURIComponent(distribution)}`),
  templates: () => request("GET", "/api/templates"),
  createTemplate: (req) => request("POST", "/api/templates", req),
  deleteTemplate: (vmid) => act("DELETE", `/api/templates/${vmid}`),
  refreshTemplates: () => request("POST", "/api/templates/refresh"),
  templateBuilds: () => request("GET", "/api/template-builds"),
  requestTemplateBuild: (distribution, kubernetesVersion) =>
    act("POST", "/api/template-builds", { distribution, kubernetesVersion }),

  upgradeSettings: (settings) => request("PUT", "/api/appliance/upgrades/settings", settings),
  automaticManagement: (passphrase) => request("PUT", "/api/appliance/upgrades/management", { passphrase }),
  stopAutomaticManagement: () => request("DELETE", "/api/appliance/upgrades/management"),

  storages: () => request("GET", "/api/proxmox/storages"),
  nodes: () => request("GET", "/api/proxmox/nodes"),
  bridges: (node) => request("GET", `/api/proxmox/nodes/${encodeURIComponent(node)}/bridges`),
  haRules: () => request("GET", "/api/proxmox/ha-rules"),
};

/** bridgesOf lists a node's bridges; a node whose bridges cannot be read offers none. */
export async function bridgesOf(node: string): Promise<Bridge[]> {
  try {
    return await api.bridges(node);
  } catch {
    return [];
  }
}
