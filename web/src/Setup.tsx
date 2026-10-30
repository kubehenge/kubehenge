import {
  Fragment,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type JSX,
  type ReactNode,
  type RefObject,
} from "react";

import { api, bridgesOf } from "./api";
import { backupKeys } from "./Backups";
import { BackupTargets, cleanTargets, targetProblems, targetSummary } from "./BackupTargets";
import { CredentialField } from "./CredentialField";
import { cidr, isIP, isVLAN, list, maskOf, pointToPoint, rangeProblem, sameNetwork, toInt } from "./network";
import { OIDCProviders, cleanProviders, oidcProblems } from "./OIDCProviders";
import type {
  BackupTarget,
  Bridge,
  Job,
  ManagementSettings,
  OIDCProvider,
  PveNode,
  SetupStatus,
  Storage,
} from "./types";
import {
  Choice,
  Detail,
  Dialog,
  ErrorBox,
  Field,
  Info,
  errorMessage,
  filled,
  logLines,
  minPassphrase,
  passphraseShort,
  pollIdle,
  pollRunning,
  timeZones,
  usePoll,
  weekdayName,
  weekdays,
} from "./ui";

const unique = (xs: string[]): string[] => [...new Set(xs)].toSorted();

// A management cluster has one node, or three for high availability.
const highAvailability = 3;

const msPerMinute = 60_000;

const setWhenCreated = "Set when the management cluster was created";

// Within this many pixels of its end, the log counts as read to the end.
const scrollSlack = 4;

export interface SetupForm {
  nodes: string[];
  imageDatastore: string;
  vmDatastore: string;
  bridge: string;
  vlan: string;
  nodeAddresses: string;
  apiAddress: string;
  gateway: string;
  dnsServers: string;
  workloadDatastores: string[];
  workloadBridges: string[];
  bootstrapDatastore: string;
  // registry=endpoint, one per line.
  mirrors: string;
  podSubnets: string;
  serviceSubnets: string;
  // Kept from the last run; not edited here.
  controllerImage?: string | undefined;
  clusterOIDC: OIDCProvider[];
  proxmoxHA: boolean;
  offerHARules: boolean;
  clusterDatastoreAllocate: boolean;
  backupTargets: BackupTarget[];
  etcdRestore: boolean;
  // The weekly build of the node OS VM templates.
  refresh: boolean;
  refreshWeekday: string;
  refreshTime: string;
  refreshTimeZone: string;
  // Empty: the first storage for clusters.
  refreshStorage: string;
  refreshAddress: string;
  refreshGateway: string;
  refreshDNS: string;
  creatorBuilds: boolean;
  clusterSysAudit: boolean;
  nodeBootLogs: boolean;
  moreNamespaces: boolean;
  // Separated by commas or lines.
  extraNamespaces: string;
  headlampSignIn: boolean;
  headlampActions: boolean;
  passphrase: string;
  confirm: string;
}

// When the refresh runs unless the settings say otherwise, as the module has it.
const refreshDefaults = { weekday: "Sun", time: "01:00", timeZone: "UTC" };

function refreshSettings(f: SetupForm): NonNullable<ManagementSettings["templateRefresh"]> {
  if (!f.refresh) {
    return { enabled: false };
  }
  const r: NonNullable<ManagementSettings["templateRefresh"]> = {
    enabled: true,
    weekday: f.refreshWeekday,
    time: f.refreshTime,
    timeZone: f.refreshTimeZone,
  };
  if (f.refreshStorage) {
    r.storage = f.refreshStorage;
  }
  if (f.refreshAddress) {
    r.address = f.refreshAddress;
    r.gateway = f.refreshGateway;
    if (list(f.refreshDNS).length > 0) {
      r.dns = list(f.refreshDNS);
    }
  }
  if (f.creatorBuilds) {
    r.creatorBuilds = true;
  }
  return r;
}

/** The namespace the interface creates clusters in, whatever others it is given. */
export const ownNamespace = "clusters";

// namespaceProblem says why a namespace cannot hold clusters beside ownNamespace, as the appliance
// checks it, or returns undefined.
function namespaceProblem(ns: string): string | undefined {
  if (!/^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$/u.test(ns)) {
    return `${ns} must be at most 63 lowercase letters, digits and dashes.`;
  }
  if (ns === ownNamespace) {
    return `${ns} is always offered.`;
  }
  if (ns === "default" || ns === "cert-manager" || ns.startsWith("kube-") || ns.endsWith("-system")) {
    return `${ns} is reserved for the system.`;
  }
  return undefined;
}

const mirrorLines = (text: string): string[] =>
  text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

function parseMirrors(text: string): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const line of mirrorLines(text)) {
    const [registry, endpoint] = line.split("=").map((x) => x.trim());
    if (filled(registry) && filled(endpoint)) {
      (out[registry] ??= []).push(endpoint);
    }
  }
  return out;
}

function formatMirrors(m: Record<string, string[]> | undefined): string {
  return Object.entries(m ?? {})
    .flatMap(([registry, endpoints]) => endpoints.map((e) => `${registry}=${e}`))
    .join("\n");
}

export function settings(f: SetupForm): ManagementSettings {
  const s: ManagementSettings = {
    nodes: f.nodes,
    imageDatastore: f.imageDatastore,
    vmDatastore: f.vmDatastore,
    bridge: f.bridge,
    nodeAddresses: list(f.nodeAddresses),
    apiAddress: f.apiAddress,
    gateway: f.gateway,
    dnsServers: list(f.dnsServers),
    workloadDatastores: f.workloadDatastores,
    workloadBridges: f.workloadBridges,
    bootstrapDatastore: f.bootstrapDatastore,
  };
  if (f.vlan) {
    s.vlan = Number(f.vlan);
  }
  const mirrors = parseMirrors(f.mirrors);
  if (Object.keys(mirrors).length > 0) {
    s.registryMirrors = mirrors;
  }
  if (list(f.podSubnets).length > 0) {
    s.podSubnets = list(f.podSubnets);
  }
  if (list(f.serviceSubnets).length > 0) {
    s.serviceSubnets = list(f.serviceSubnets);
  }
  if (filled(f.controllerImage)) {
    s.controllerImage = f.controllerImage;
  }
  if (f.clusterOIDC.length > 0) {
    s.clusterOIDC = cleanProviders(f.clusterOIDC);
  }
  if (f.proxmoxHA) {
    s.proxmoxHA = true;
  }
  if (f.offerHARules) {
    s.offerHARules = true;
  }
  if (f.clusterDatastoreAllocate) {
    s.clusterDatastoreAllocate = true;
  }
  if (f.backupTargets.length > 0) {
    s.backupTargets = cleanTargets(f.backupTargets);
  }
  if (f.etcdRestore) {
    s.etcdRestore = true;
  }
  s.templateRefresh = refreshSettings(f);
  if (!f.clusterSysAudit) {
    s.clusterSysAudit = false;
  }
  if (!f.nodeBootLogs) {
    s.nodeBootLogs = false;
  }
  if (f.moreNamespaces && list(f.extraNamespaces).length > 0) {
    s.extraClusterNamespaces = list(f.extraNamespaces);
  }
  if (f.headlampSignIn) {
    s.headlampSignIn = true;
  }
  if (f.headlampActions) {
    s.headlampActions = true;
  }
  return s;
}

// What a first run starts from: nothing chosen, but Proxmox VE's own local storage for the Talos
// and cloud-init images.
const unsaved: ManagementSettings = {
  nodes: [],
  imageDatastore: "local",
  vmDatastore: "",
  bridge: "",
  nodeAddresses: [],
  apiAddress: "",
  gateway: "",
  dnsServers: [],
  workloadDatastores: [],
  workloadBridges: [],
  bootstrapDatastore: "local",
};

type RefreshFields = Pick<
  SetupForm,
  | "refresh"
  | "refreshWeekday"
  | "refreshTime"
  | "refreshTimeZone"
  | "refreshStorage"
  | "refreshAddress"
  | "refreshGateway"
  | "refreshDNS"
  | "creatorBuilds"
>;

// refreshFields fills the refresh's fields; settings that name no refresh have it on, with every
// default.
function refreshFields(r: ManagementSettings["templateRefresh"]): RefreshFields {
  return {
    refresh: r?.enabled ?? true,
    refreshWeekday: r?.weekday ?? refreshDefaults.weekday,
    refreshTime: r?.time ?? refreshDefaults.time,
    refreshTimeZone: r?.timeZone ?? refreshDefaults.timeZone,
    refreshStorage: r?.storage ?? "",
    refreshAddress: r?.address ?? "",
    refreshGateway: r?.gateway ?? "",
    refreshDNS: (r?.dns ?? []).join(", "),
    creatorBuilds: r?.creatorBuilds === true,
  };
}

/** formOf fills the form from saved settings, or with nothing chosen yet. */
export function formOf(saved: ManagementSettings = unsaved): SetupForm {
  return {
    nodes: saved.nodes,
    imageDatastore: saved.imageDatastore,
    vmDatastore: saved.vmDatastore,
    bridge: saved.bridge,
    vlan: saved.vlan !== undefined && saved.vlan > 0 ? String(saved.vlan) : "",
    nodeAddresses: saved.nodeAddresses.join("\n"),
    apiAddress: saved.apiAddress,
    gateway: saved.gateway,
    dnsServers: saved.dnsServers.join(", "),
    workloadDatastores: saved.workloadDatastores,
    workloadBridges: saved.workloadBridges,
    bootstrapDatastore: saved.bootstrapDatastore,
    mirrors: formatMirrors(saved.registryMirrors),
    podSubnets: (saved.podSubnets ?? []).join(", "),
    serviceSubnets: (saved.serviceSubnets ?? []).join(", "),
    controllerImage: saved.controllerImage,
    clusterOIDC: saved.clusterOIDC ?? [],
    proxmoxHA: saved.proxmoxHA === true,
    offerHARules: saved.offerHARules === true,
    clusterDatastoreAllocate: saved.clusterDatastoreAllocate === true,
    backupTargets: saved.backupTargets ?? [],
    etcdRestore: saved.etcdRestore === true,
    ...refreshFields(saved.templateRefresh),
    clusterSysAudit: saved.clusterSysAudit !== false,
    nodeBootLogs: saved.nodeBootLogs !== false,
    moreNamespaces: (saved.extraClusterNamespaces ?? []).length > 0,
    extraNamespaces: (saved.extraClusterNamespaces ?? []).join(", "),
    headlampSignIn: saved.headlampSignIn === true,
    headlampActions: saved.headlampActions === true,
    passphrase: "",
    confirm: "",
  };
}

export type SetupField =
  | "nodes"
  | "imageDatastore"
  | "vmDatastore"
  | "bridge"
  | "vlan"
  | "nodeAddresses"
  | "apiAddress"
  | "gateway"
  | "dnsServers"
  | "workload"
  | "bootstrapDatastore"
  | "clusterOIDC"
  | "backupTargets"
  | "mirrors"
  | "podSubnets"
  | "serviceSubnets"
  | "refreshAddress"
  | "refreshGateway"
  | "refreshDNS"
  | "extraNamespaces"
  | "passphrase"
  | "confirm";

// nodeNetwork checks the node addresses against each other and the gateway, as the appliance
// does: one network, the gateway in it, no address taken twice. It returns the network or the
// problem.
function nodeNetwork(
  addresses: string[],
  gateway: number | undefined,
): { ip: number; bits: number; taken: number[] } | string {
  const wrongCount = "Enter 1 or 3 addresses.";
  if (addresses.length !== 1 && addresses.length !== highAvailability) {
    return wrongCount;
  }
  const taken: number[] = [];
  let first: { ip: number; bits: number } | undefined;
  for (const a of addresses) {
    const [base, bits] = a.split("/");
    const n = Number(bits);
    const ip = toInt(base ?? "");
    if (!cidr.test(a) || ip === undefined || n === 0) {
      return `${a} is not in the form 192.0.2.11/24.`;
    }
    const net = (ip & maskOf(n)) >>> 0;
    if (n < pointToPoint && (ip === net || ip === (net | ~maskOf(n)) >>> 0)) {
      return `${a} is a network or broadcast address.`;
    }
    if (taken.includes(ip)) {
      return `${a} is listed twice.`;
    }
    if (gateway !== undefined && !sameNetwork(ip, gateway, n)) {
      return `${a} is outside the gateway's network.`;
    }
    if (first && (first.bits !== n || !sameNetwork(ip, first.ip, n))) {
      return `Put ${a} in the same network as ${addresses[0]}.`;
    }
    first ??= { ip, bits: n };
    taken.push(ip);
  }
  return first ? { ...first, taken } : wrongCount;
}

// setupFieldProblems says, by field, what keeps the settings from being applied. The first run
// asks for the passphrase twice; later runs open the state with the one given then. A management
// cluster that exists (was) keeps its nodes' addresses, as the appliance insists.
export function setupFieldProblems(
  f: SetupForm,
  firstRun = true,
  was?: ManagementSettings,
): Partial<Record<SetupField, string>> {
  const p: SetupProblems = {
    ...placementProblems(f),
    ...networkProblems(f, was),
    ...offerProblems(f),
    ...refreshProblems(f),
  };
  if (passphraseShort(f.passphrase)) {
    p.passphrase = `Use at least ${minPassphrase} characters.`;
  } else if (firstRun && f.passphrase !== f.confirm) {
    p.confirm = "Passphrases do not match.";
  }
  return p;
}

type SetupProblems = Partial<Record<SetupField, string>>;

// placementProblems are those of where the management cluster's VMs go.
function placementProblems(f: SetupForm): SetupProblems {
  const p: SetupProblems = {};
  const addresses = list(f.nodeAddresses);
  if (f.nodes.length === 0) {
    p.nodes = "Choose at least one Proxmox VE node.";
  } else if (addresses.length > 0 && f.nodes.length > addresses.length) {
    p.nodes = "Choose no more nodes than addresses.";
  }
  if (!f.vmDatastore) {
    p.vmDatastore = "Choose a storage.";
  }
  if (!f.imageDatastore) {
    p.imageDatastore = "Choose a storage.";
  }
  if (!f.bootstrapDatastore) {
    p.bootstrapDatastore = "Choose a storage.";
  }
  if (!f.bridge) {
    p.bridge = "Choose a bridge.";
  }
  if (f.vlan && !isVLAN(f.vlan)) {
    p.vlan = "A whole number from 1 to 4094.";
  }
  return p;
}

// networkProblems are those of the management cluster's addresses.
function networkProblems(f: SetupForm, was: ManagementSettings | undefined): SetupProblems {
  const p: SetupProblems = {};
  const addresses = list(f.nodeAddresses);
  const gateway = toInt(f.gateway);
  if (gateway === undefined) {
    p.gateway = "Enter an IPv4 address.";
  }
  const network = nodeNetwork(addresses, gateway);
  if (typeof network === "string") {
    p.nodeAddresses = network;
  }
  if (was && was.nodeAddresses.some((a, i) => addresses[i] !== a)) {
    p.nodeAddresses = `Keep ${was.nodeAddresses.join(", ")} first and add new addresses after.`;
  }
  const apiIP = toInt(f.apiAddress);
  if (apiIP === undefined) {
    p.apiAddress = "Enter an IPv4 address.";
  } else if (apiIP === gateway || (typeof network !== "string" && network.taken.includes(apiIP))) {
    p.apiAddress = "Use an unused address, not a node's or the gateway's.";
  } else if (typeof network !== "string" && !sameNetwork(apiIP, network.ip, network.bits)) {
    p.apiAddress = "Use an address in the nodes' network.";
  }
  const dns = list(f.dnsServers);
  if (dns.length === 0) {
    p.dnsServers = "Enter at least one DNS server.";
  } else if (dns.some((d) => !isIP(d))) {
    p.dnsServers = "Enter IP addresses only.";
  }
  return p;
}

// badMirror is the first line of the registry mirrors that is no registry=endpoint.
function badMirror(mirrors: string): string | undefined {
  return mirrorLines(mirrors).find((line) => {
    const [registry, endpoint, ...rest] = line.split("=").map((x) => x.trim());
    return (
      rest.length > 0 ||
      !filled(registry) ||
      /[\s/]/u.test(registry) ||
      !/^https?:\/\/[^\s/]+(?:\/\S*)?$/u.test(endpoint ?? "")
    );
  });
}

// offerProblems are those of what the management cluster offers the clusters it makes.
function offerProblems(f: SetupForm): SetupProblems {
  const p: SetupProblems = {};
  if (f.workloadDatastores.length === 0) {
    p.workload = "Choose at least one storage.";
  } else if (f.workloadBridges.length === 0) {
    p.workload = "Choose at least one bridge.";
  }
  const oidc = oidcProblems(f.clusterOIDC);
  if (oidc.length > 0) {
    p.clusterOIDC = oidc.join(" ");
  }
  const targets = targetProblems(f.backupTargets);
  if (targets.length > 0) {
    p.backupTargets = targets.join(" ");
  }
  const bad = badMirror(f.mirrors);
  if (bad !== undefined) {
    p.mirrors = `${bad} is not in the form ghcr.io=https://mirror.example:5000.`;
  }
  for (const key of ["podSubnets", "serviceSubnets"] as const) {
    const wrong = list(f[key]).find((r) => rangeProblem(r) !== undefined);
    if (wrong !== undefined) {
      p[key] = `${wrong} is not a network address like 10.244.0.0/16.`;
    }
  }
  if (f.moreNamespaces) {
    const names = list(f.extraNamespaces);
    const twice = names.find((n, i) => names.indexOf(n) !== i);
    const wrong = names.map((n) => namespaceProblem(n)).find((x) => x !== undefined);
    if (names.length === 0) {
      p.extraNamespaces = "Enter at least one namespace.";
    } else if (wrong !== undefined) {
      p.extraNamespaces = wrong;
    } else if (twice !== undefined) {
      p.extraNamespaces = `${twice} is listed twice.`;
    }
  }
  return p;
}

// refreshProblems are those of the static network of the VM that prepares a template's image.
function refreshProblems(f: SetupForm): SetupProblems {
  const p: SetupProblems = {};
  if (!f.refresh) {
    return p;
  }
  const dns = list(f.refreshDNS);
  if (!f.refreshAddress) {
    if (f.refreshGateway || dns.length > 0) {
      p.refreshAddress = "Enter an address, or clear gateway and DNS for DHCP.";
    }
    return p;
  }
  const [base = "", bits] = f.refreshAddress.split("/");
  const ip = toInt(base);
  if (!cidr.test(f.refreshAddress) || ip === undefined) {
    p.refreshAddress = "Enter an address like 192.0.2.50/24.";
    return p;
  }
  const gateway = toInt(f.refreshGateway);
  if (gateway === undefined || !sameNetwork(ip, gateway, Number(bits))) {
    p.refreshGateway = "Enter a gateway in the build address's network.";
  }
  if (dns.some((d) => !isIP(d))) {
    p.refreshDNS = "Enter IP addresses only.";
  }
  return p;
}

export function setupProblems(f: SetupForm, firstRun = true): string[] {
  return Object.values(setupFieldProblems(f, firstRun));
}

// running tells whether the page has to follow a run: one going on, or an apply that succeeded
// and waits for the interface to switch to the new cluster. A removal that succeeded is over.
const running = (s: SetupStatus): s is SetupStatus & { job: Job } =>
  s.job !== undefined &&
  (s.job.state === "running" || (s.job.kind !== "remove" && s.job.state === "succeeded" && !s.ready));

// Setup is the appliance's first run: it creates the management cluster with the signed-in
// administrator's Proxmox VE session.
export function Setup({ onReady }: { onReady?: () => void }): JSX.Element {
  const { data: status, error, reload } = usePoll(api.setupStatus, (s) => (s && running(s) ? pollRunning : pollIdle));
  const ready = status?.ready;
  useEffect(() => {
    if (ready === true) {
      onReady?.();
    }
  }, [ready, onReady]);

  if (!status) {
    return (
      <section className="panel">
        {filled(error) ? <ErrorBox message={error} /> : <p className="loading">Loading…</p>}
      </section>
    );
  }
  if (!status.allowed) {
    return (
      <section className="panel">
        <div className="toolbar">
          <h2>Not set up yet</h2>
        </div>
        <p className="pad">
          Ask a user with the Administrator role on <code>/</code> to set it up.
        </p>
      </section>
    );
  }
  if (running(status)) {
    return <Progress job={status.job} />;
  }
  return (
    <SetupWizard
      status={status}
      onStarted={() => {
        void reload();
      }}
    />
  );
}

// ManagementCluster is the management cluster's part of the Administration page: its settings, and
// applying them again, which brings it to what this appliance carries or repairs it.
export function ManagementCluster({
  status,
  reload,
}: {
  status: SetupStatus;
  reload: () => Promise<void>;
}): JSX.Element {
  const [editing, setEditing] = useState(false);
  if (running(status)) {
    return <Progress job={status.job} />;
  }
  const started = (): void => {
    setEditing(false);
    void reload();
  };
  if (editing || !status.settings) {
    return (
      <SetupWizard
        status={status}
        manage
        onStarted={started}
        onCancel={status.settings ? () => setEditing(false) : undefined}
      />
    );
  }
  return (
    <ManagedSummary status={status} settings={status.settings} onEdit={() => setEditing(true)} onStarted={started} />
  );
}

function LastRun({ status, manage }: { status: SetupStatus; manage: boolean }) {
  const { job } = status;
  if (!job) {
    return null;
  }
  const when = new Date(job.finished ?? job.started).toLocaleString();
  const log = job.log ?? [];
  return (
    <>
      {manage && job.state === "succeeded" && job.kind !== "remove" && (
        <p className="muted">
          {job.automatic === true ? `Last applied ${when} by automatic upgrade` : `Last applied ${when} by ${job.user}`}
        </p>
      )}
      {job.state === "succeeded" && job.kind === "remove" && (
        <p className="muted">
          Management cluster removed {when} by {job.user}
        </p>
      )}
      {job.state === "failed" && (
        <div className="error" role="alert">
          {job.automatic === true
            ? `Automatic upgrade of ${when} failed. It retries at the next upgrade hour.`
            : "Last run failed"}
          {(filled(job.error) || log.length > 0) && (
            <Detail>
              {filled(job.error) && <p>{job.error}</p>}
              {log.length > 0 && <pre className="log">{log.slice(-logLines).join("\n")}</pre>}
            </Detail>
          )}
        </div>
      )}
    </>
  );
}

// refreshSummary says when and where the templates are built again.
function refreshSummary(s: ManagementSettings): string {
  const f = refreshFields(s.templateRefresh);
  if (!f.refresh) {
    return "Off";
  }
  const disks = filled(f.refreshStorage) ? f.refreshStorage : (s.workloadDatastores[0] ?? "first cluster storage");
  const parts = [
    `${weekdayName(f.refreshWeekday)} ${f.refreshTime} ${f.refreshTimeZone}`,
    `on ${disks}`,
    filled(f.refreshAddress) ? f.refreshAddress : "DHCP",
  ];
  if (f.creatorBuilds) {
    parts.push("creators may request builds");
  }
  return parts.join(", ");
}

const onOff = (on: boolean, detail = "On"): string => (on ? detail : "Off");

// optInRows summarise the choices, each off by default, that let Kubehenge or the clusters do more
// in Proxmox VE than their pools allow.
function optInRows(s: ManagementSettings): [string, string][] {
  return [
    ["Proxmox VE HA", onOff(s.proxmoxHA === true)],
    ["HA rule placements", onOff(s.offerHARules === true)],
    ["Newest CSI release", onOff(s.clusterDatastoreAllocate === true)],
    ["HA group labels", onOff(s.clusterSysAudit !== false)],
    ["Nodes' boot logs", onOff(s.nodeBootLogs !== false)],
  ];
}

function headlampRows(s: ManagementSettings): [string, string][] {
  return [
    ["Headlamp sign-in", onOff(s.headlampSignIn === true, "On, as cluster-admin")],
    ["Headlamp actions", onOff(s.headlampActions === true)],
  ];
}

function ManagedSummary({
  status,
  settings: s,
  onEdit,
  onStarted,
}: {
  status: SetupStatus;
  settings: ManagementSettings;
  onEdit: () => void;
  onStarted: () => void;
}) {
  const [applying, setApplying] = useState(false);
  const rows: [string, string][] = [
    ["Proxmox VE nodes", s.nodes.join(", ")],
    ["VM disks", s.vmDatastore],
    ["Talos image", s.imageDatastore],
    ["Cloud-init images", s.bootstrapDatastore],
    ["Bridge", s.vlan !== undefined && s.vlan > 0 ? `${s.bridge}, VLAN ${s.vlan}` : s.bridge],
    ["Node addresses", s.nodeAddresses.join(", ")],
    ["API address", s.apiAddress],
    ["Gateway", s.gateway],
    ["DNS servers", s.dnsServers.join(", ")],
    ["Cluster storages", s.workloadDatastores.join(", ")],
    ["Cluster bridges", s.workloadBridges.join(", ")],
    ...optInRows(s),
    ["Template refresh", refreshSummary(s)],
    ["Cluster namespaces", [ownNamespace, ...(s.extraClusterNamespaces ?? [])].join(", ")],
    ...headlampRows(s),
    [
      "Identity providers",
      s.clusterOIDC && s.clusterOIDC.length > 0 ? s.clusterOIDC.map((p) => p.issuerURL).join(", ") : "None",
    ],
    [
      "Backup targets",
      s.backupTargets && s.backupTargets.length > 0 ? s.backupTargets.map((t) => targetSummary(t)).join(", ") : "None",
    ],
    ["etcd restores", s.etcdRestore === true ? "Guest agent" : "By hand"],
    ["Registry mirrors", s.registryMirrors ? formatMirrors(s.registryMirrors).split("\n").join(", ") : "None"],
    ["Pod ranges", s.podSubnets?.join(", ") ?? "Automatic"],
    ["Service ranges", s.serviceSubnets?.join(", ") ?? "Automatic"],
  ];
  if (status.upgrade) {
    rows.unshift(["Version", `Kubehenge ${status.upgrade.applied}`]);
  }
  return (
    <section className="panel">
      <div className="box">
        <h3>Management cluster</h3>
        <LastRun status={status} manage />
        <dl className="summary">
          {rows.map(([k, v]) => (
            <Fragment key={k}>
              <dt>{k}</dt>
              <dd>{v}</dd>
            </Fragment>
          ))}
        </dl>
        <div className="actions">
          <button type="button" onClick={onEdit}>
            Edit settings
          </button>
          <button type="button" className="primary" onClick={() => setApplying(true)}>
            Apply again…
          </button>
        </div>
      </div>
      {status.ready && (s.backupTargets ?? []).length > 0 && (
        <div className="box">
          <CredentialField
            label="Shared backup key"
            purpose="backup"
            keys={backupKeys}
            hint="Clusters sharing it can reach, not read, each other's backups"
          />
        </div>
      )}
      {applying && (
        <ApplyDialog
          title="Apply management cluster"
          action="Apply"
          settings={s}
          onClose={() => setApplying(false)}
          onStarted={onStarted}
        />
      )}
    </section>
  );
}

// ApplyDialog applies the saved settings again as they are, once given the state passphrase; what
// it brings goes above the passphrase.
export function ApplyDialog({
  title,
  action,
  settings: s,
  onClose,
  onStarted,
  children,
}: {
  title: string;
  action: string;
  settings: ManagementSettings;
  onClose: () => void;
  onStarted: () => void;
  children?: ReactNode;
}): JSX.Element {
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const short = passphraseShort(passphrase);
  const apply = async (): Promise<void> => {
    if (short) {
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.setup(s, passphrase);
      onStarted();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={title}
      onClose={onClose}
      onSubmit={() => {
        void apply();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || short}>
            {action}
          </button>
        </>
      }
    >
      {children}
      <p className="muted">Takes 10 to 20 minutes.</p>
      <Field label="State passphrase" hint={setWhenCreated}>
        {(id) => (
          <input
            id={id}
            type="password"
            autoComplete="current-password"
            value={passphrase}
            onChange={(e) => setPassphrase(e.target.value)}
          />
        )}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}

// elapsed says how long ago a run started, in whole minutes.
function elapsed(started: string, now = Date.now()): string {
  const minutes = Math.max(0, Math.floor((now - new Date(started).getTime()) / msPerMinute));
  if (minutes < 1) {
    return "under a minute";
  }
  return minutes === 1 ? "1 minute" : `${minutes} minutes`;
}

function Progress({ job }: { job: Job }): JSX.Element {
  const log = job.log ?? [];
  const box = useRef<HTMLPreElement>(null);
  // The log follows new lines as long as the reader has not scrolled up to read.
  const atEnd = useRef(true);
  useLayoutEffect(() => {
    const b = box.current;
    if (b && atEnd.current) {
      b.scrollTop = b.scrollHeight;
    }
  });
  return (
    <section className="panel">
      <div className="toolbar">
        <h2>{job.kind === "remove" ? "Removing the management cluster" : "Applying the management cluster"}</h2>
        <span className="spacer" />
        <span className="muted">
          {job.automatic === true ? "Started automatically" : `Started by ${job.user}`} {elapsed(job.started)} ago
        </span>
      </div>
      <p className="pad muted">Takes 10 to 20 minutes and keeps running if you close the page.</p>
      <p className="pad" aria-live="polite">
        {log.length > 0 ? `Now: ${log.at(-1)}` : "Starting…"}
      </p>
      {job.lines !== undefined && (
        <p className="pad muted">
          Last {log.length} of {job.lines} lines
        </p>
      )}
      <pre
        ref={box}
        className="confirm log"
        aria-label="Log"
        onScroll={(e) => {
          const b = e.currentTarget;
          atEnd.current = b.scrollTop + b.clientHeight >= b.scrollHeight - scrollSlack;
        }}
      >
        {log.join("\n")}
      </pre>
    </section>
  );
}

// choiceNote says where a storage or bridge clusters may use is: shared, on some nodes, or no
// longer found at all.
function choiceNote(nodes: string[], shared: boolean, allNodes: number): string {
  if (nodes.length === 0) {
    return "not found";
  }
  if (shared) {
    return "shared";
  }
  return allNodes > 1 ? `on ${nodes.join(", ")}` : "";
}

const fixedHint = "Fixed after creation";

// keepOffered keeps a choice once the choices are known only if it is among them.
const keepOffered = (value: string, choices: string[], ready: boolean): string =>
  !ready || choices.includes(value) ? value : "";

function passphraseHint(manage: boolean, resumes: boolean | undefined): string {
  if (manage) {
    return setWhenCreated;
  }
  return resumes === true
    ? "The passphrase of the earlier run"
    : `At least ${minPassphrase} characters, and it cannot be recovered`;
}

type Toggle = (k: "nodes" | "workloadDatastores" | "workloadBridges", v: string, on: boolean) => void;

interface FieldsProps {
  form: SetupForm;
  set: <K extends keyof SetupForm>(k: K, v: SetupForm[K]) => void;
  shown: (k: SetupField) => string | undefined;
}

function FieldError({ message }: { message: string | undefined }): JSX.Element | null {
  return message === undefined ? null : <p className="field-error">{message}</p>;
}

interface Offered {
  name: string;
  note: string;
}

function WorkloadChoices({
  form,
  storages,
  bridges,
  problem,
  onToggle,
}: {
  form: SetupForm;
  storages: Offered[];
  bridges: Offered[];
  problem: string | undefined;
  onToggle: Toggle;
}): JSX.Element {
  const ids = useId();
  return (
    <fieldset className="group">
      <legend>Available to clusters</legend>
      <fieldset className="choices">
        <legend>Storages</legend>
        {storages.map(({ name, note }, i) => (
          <label key={name} className="check">
            <input
              type="checkbox"
              aria-describedby={note ? `${ids}-storage-${i}` : undefined}
              checked={form.workloadDatastores.includes(name)}
              onChange={(e) => onToggle("workloadDatastores", name, e.target.checked)}
            />{" "}
            {name}
            {note && (
              <span className="muted" id={`${ids}-storage-${i}`} aria-hidden="true">
                ({note})
              </span>
            )}
          </label>
        ))}
      </fieldset>
      <fieldset className="choices">
        <legend>Bridges</legend>
        {bridges.map(({ name, note }, i) => (
          <label key={name} className="check">
            <input
              type="checkbox"
              aria-describedby={note ? `${ids}-bridge-${i}` : undefined}
              checked={form.workloadBridges.includes(name)}
              onChange={(e) => onToggle("workloadBridges", name, e.target.checked)}
            />{" "}
            {name}
            {note && (
              <span className="muted" id={`${ids}-bridge-${i}`} aria-hidden="true">
                ({note})
              </span>
            )}
          </label>
        ))}
      </fieldset>
      <FieldError message={problem} />
    </fieldset>
  );
}

function HAChoice({ form, set }: Omit<FieldsProps, "shown">): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>Proxmox VE HA</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.proxmoxHA}
          aria-describedby={hint}
          onChange={(e) => set("proxmoxHA", e.target.checked)}
        />{" "}
        Spread control-plane VMs
      </label>
      <Info about="Proxmox VE HA">
        For clusters on shared storage. It restarts a failed node&apos;s VMs, and a node that loses quorum reboots.
      </Info>
      <p className="hint" id={hint}>
        Grants Sys.Console on /, a root shell on every node.
      </p>
    </fieldset>
  );
}

function HARulesChoice({ form, set }: Omit<FieldsProps, "shown">): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>HA rule placements</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.offerHARules}
          aria-describedby={hint}
          onChange={(e) => set("offerHARules", e.target.checked)}
        />{" "}
        Offer HA node-affinity rules
      </label>
      <p className="hint" id={hint}>
        Cluster creators see each rule&apos;s name and nodes.
      </p>
    </fieldset>
  );
}

function HeadlampChoice({ form, set }: Omit<FieldsProps, "shown">): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>Headlamp sign-in</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.headlampSignIn}
          aria-describedby={hint}
          onChange={(e) => set("headlampSignIn", e.target.checked)}
        />{" "}
        Sign in as cluster-admin
      </label>
      <Info about="Headlamp sign-in">
        Each cluster gets a cluster-admin account that Headlamp acts as. Switching it off deletes the accounts and ends
        their tokens.
      </Info>
      <p className="hint" id={hint}>
        Kubeconfig holders open clusters in Headlamp as cluster-admin.
      </p>
    </fieldset>
  );
}

function HeadlampActionsChoice({ form, set }: Omit<FieldsProps, "shown">): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>Headlamp actions</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.headlampActions}
          aria-describedby={hint}
          onChange={(e) => set("headlampActions", e.target.checked)}
        />{" "}
        Helm and Prometheus charts
      </label>
      <Info about="Headlamp actions">
        Only setup administrators add Helm repositories. The appliance fetches charts from them over HTTPS.
      </Info>
      <p className="hint" id={hint}>
        Users manage Helm releases with their own cluster credentials.
      </p>
    </fieldset>
  );
}

function EtcdRestoreChoice({ form, set, onAsk }: Omit<FieldsProps, "shown"> & { onAsk: () => void }): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>etcd restores</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.etcdRestore}
          aria-describedby={hint}
          onChange={(e) => {
            if (e.target.checked) {
              onAsk();
            } else {
              set("etcdRestore", false);
            }
          }}
        />{" "}
        Restore through guest agent
      </label>
      <Info about="etcd restores">
        Off, the interface shows the restore steps to run by hand. Talos clusters restore through the Talos API either
        way.
      </Info>
      <p className="hint" id={hint}>
        Grants a root shell in every cluster VM.
      </p>
    </fieldset>
  );
}

function DatastoreAllocateChoice({ form, set }: Omit<FieldsProps, "shown">): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>CSI plugin</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.clusterDatastoreAllocate}
          aria-describedby={hint}
          onChange={(e) => set("clusterDatastoreAllocate", e.target.checked)}
        />{" "}
        Offer newest CSI release
      </label>
      <p className="hint" id={hint}>
        Grants Datastore.Allocate on a storage only that cluster uses.
      </p>
    </fieldset>
  );
}

// offeredRefreshStorage sends template disks the clusters may no longer use back to the first that
// they may.
const offeredRefreshStorage = (f: SetupForm): string =>
  f.workloadDatastores.includes(f.refreshStorage) ? f.refreshStorage : "";

function RefreshChoice({ form, set, shown }: FieldsProps): JSX.Element {
  return (
    <fieldset className="group">
      <legend>Template refresh</legend>
      <label className="check">
        <input type="checkbox" checked={form.refresh} onChange={(e) => set("refresh", e.target.checked)} /> Rebuild VM
        templates weekly
      </label>
      <Info about="template refresh">
        Clusters move to a build once it is a day old, in their maintenance window. The two newest builds stay.
      </Info>
      {form.refresh && (
        <>
          <Field label="Day">
            {(id) => (
              <select id={id} value={form.refreshWeekday} onChange={(e) => set("refreshWeekday", e.target.value)}>
                {weekdays.map((d) => (
                  <option key={d} value={d}>
                    {weekdayName(d)}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <Field label="Time">
            {(id) => (
              <input
                id={id}
                type="time"
                value={form.refreshTime}
                onChange={(e) => set("refreshTime", e.target.value || refreshDefaults.time)}
              />
            )}
          </Field>
          <Field label="Time zone">
            {(id) => (
              <select id={id} value={form.refreshTimeZone} onChange={(e) => set("refreshTimeZone", e.target.value)}>
                {timeZones(form.refreshTimeZone).map((z) => (
                  <option key={z} value={z}>
                    {z}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <Field label="Template disks">
            {(id) => (
              <select id={id} value={form.refreshStorage} onChange={(e) => set("refreshStorage", e.target.value)}>
                <option value="">
                  {form.workloadDatastores[0] === undefined
                    ? "First cluster storage"
                    : `${form.workloadDatastores[0]} (default)`}
                </option>
                {form.workloadDatastores.slice(1).map((d) => (
                  <option key={d} value={d}>
                    {d}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <Field label="Build address" hint="Empty for DHCP, or like 192.0.2.50/24" error={shown("refreshAddress")}>
            {(id) => (
              <input
                id={id}
                value={form.refreshAddress}
                onChange={(e) => set("refreshAddress", e.target.value.trim())}
              />
            )}
          </Field>
          {form.refreshAddress && (
            <>
              <Field label="Build gateway" error={shown("refreshGateway")}>
                {(id) => (
                  <input
                    id={id}
                    value={form.refreshGateway}
                    onChange={(e) => set("refreshGateway", e.target.value.trim())}
                  />
                )}
              </Field>
              <Field label="Build DNS servers" error={shown("refreshDNS")}>
                {(id) => <input id={id} value={form.refreshDNS} onChange={(e) => set("refreshDNS", e.target.value)} />}
              </Field>
            </>
          )}
          <label className="check">
            <input
              type="checkbox"
              checked={form.creatorBuilds}
              onChange={(e) => set("creatorBuilds", e.target.checked)}
            />{" "}
            Let creators request builds
          </label>
          <Info about="build requests">
            Cluster creators missing a template can have it built from the create wizard. Off, only administrators build
            templates.
          </Info>
        </>
      )}
    </fieldset>
  );
}

function SysAuditChoice({ form, set }: Omit<FieldsProps, "shown">): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>HA group labels</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.clusterSysAudit}
          aria-describedby={hint}
          onChange={(e) => set("clusterSysAudit", e.target.checked)}
        />{" "}
        Add HA group labels
      </label>
      <Info about="HA group labels">
        Clusters can read firewall rules, backup jobs and HA and corosync settings, not guests or storage. Each cluster
        can switch it off.
      </Info>
      <p className="hint" id={hint}>
        Grants each cluster Sys.Audit on /.
      </p>
    </fieldset>
  );
}

function BootLogsChoice({ form, set }: Omit<FieldsProps, "shown">): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>Nodes&apos; boot logs</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.nodeBootLogs}
          aria-describedby={hint}
          onChange={(e) => set("nodeBootLogs", e.target.checked)}
        />{" "}
        Read the boot logs of kubeadm, RKE2 and k3s nodes through the guest agent
      </label>
      <p className="muted" id={hint}>
        Recommended. A node that does not join says why in its boot log, which a cluster&apos;s Machines tab and support
        bundle then show. Kubehenge&apos;s controller reads it with VM.GuestAgent.FileRead on the clusters&apos; pool:
        files only, no commands, and only those the node&apos;s agent may open, which Kubehenge&apos;s templates limit
        to cloud-init&apos;s output and the node&apos;s copy of its boot&apos;s journals. Off, only Talos nodes&apos;
        boot logs are read, through their Talos API.
      </p>
    </fieldset>
  );
}

function NamespacesChoice({ form, set, shown }: FieldsProps): JSX.Element {
  return (
    <fieldset className="group">
      <legend>Cluster namespaces</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.moreNamespaces}
          onChange={(e) => set("moreNamespaces", e.target.checked)}
        />{" "}
        Offer more than {ownNamespace}
      </label>
      <Info about="cluster namespaces">
        Missing namespaces are created. One taken off the list keeps its clusters, which kubectl then manages.
      </Info>
      {form.moreNamespaces && (
        <Field label="Namespaces" hint="Separated by commas, like team-a, team-b" error={shown("extraNamespaces")}>
          {(id) => (
            <input id={id} value={form.extraNamespaces} onChange={(e) => set("extraNamespaces", e.target.value)} />
          )}
        </Field>
      )}
    </fieldset>
  );
}

// EtcdRestoreConfirm asks before the restore token is granted. It stands outside the setup form,
// whose submit would otherwise apply the settings along with the dialog's.
function EtcdRestoreConfirm({
  open,
  onClose,
  onConfirm,
}: {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
}): JSX.Element | null {
  if (!open) {
    return null;
  }
  return (
    <Dialog
      title="Allow guest agent restores"
      onClose={onClose}
      onSubmit={onConfirm}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger">
            Allow
          </button>
        </>
      }
    >
      <p>
        Kubehenge gets VM.GuestAgent.Unrestricted on the clusters&apos; pool. Whoever holds that token has a root shell
        in every cluster VM.
      </p>
      <p className="hint">Takes effect when you apply the settings.</p>
    </Dialog>
  );
}

function AdvancedFields({
  form,
  set,
  shown,
  fixed,
  detailsRef,
}: FieldsProps & { fixed: boolean; detailsRef: RefObject<HTMLDetailsElement | null> }): JSX.Element {
  const rangeHint = fixed ? fixedHint : "Empty picks a free range";
  return (
    <details className="advanced" ref={detailsRef}>
      <summary>Advanced</summary>
      <Field
        label="Registry mirrors"
        hint="One per line, like ghcr.io=https://mirror.example:5000"
        error={shown("mirrors")}
      >
        {(id) => <textarea id={id} rows={2} value={form.mirrors} onChange={(e) => set("mirrors", e.target.value)} />}
      </Field>
      <Field label="Pod ranges" hint={rangeHint} error={shown("podSubnets")}>
        {(id) => (
          <input id={id} readOnly={fixed} value={form.podSubnets} onChange={(e) => set("podSubnets", e.target.value)} />
        )}
      </Field>
      <Field label="Service ranges" hint={rangeHint} error={shown("serviceSubnets")}>
        {(id) => (
          <input
            id={id}
            readOnly={fixed}
            value={form.serviceSubnets}
            onChange={(e) => set("serviceSubnets", e.target.value)}
          />
        )}
      </Field>
    </details>
  );
}

function PassphraseFields({
  form,
  set,
  shown,
  hint,
  firstRun,
}: FieldsProps & { hint: string; firstRun: boolean }): JSX.Element {
  return (
    <>
      <Field
        label="State passphrase"
        hint={hint}
        info={
          firstRun
            ? "Encrypts the management cluster's state on the appliance. Every upgrade and repair asks for it."
            : undefined
        }
        error={shown("passphrase")}
      >
        {(id) => (
          <input
            id={id}
            type="password"
            autoComplete={firstRun ? "new-password" : "current-password"}
            value={form.passphrase}
            onChange={(e) => set("passphrase", e.target.value)}
          />
        )}
      </Field>
      {firstRun && (
        <Field label="Repeat passphrase" error={shown("confirm")}>
          {(id) => (
            <input
              id={id}
              type="password"
              autoComplete="new-password"
              value={form.confirm}
              onChange={(e) => set("confirm", e.target.value)}
            />
          )}
        </Field>
      )}
    </>
  );
}

function SetupWizard({
  status,
  manage = false,
  onStarted,
  onCancel,
}: {
  status: SetupStatus;
  manage?: boolean;
  onStarted: () => void;
  onCancel?: (() => void) | undefined;
}): JSX.Element {
  const [nodes, setNodes] = useState<PveNode[]>([]);
  const [storages, setStorages] = useState<Storage[]>();
  const [bridgesByNode, setBridgesByNode] = useState<Record<string, Bridge[]>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [form, setForm] = useState<SetupForm>(() => formOf(status.settings));
  // A field's problem shows once it has been changed, or after the run was asked for.
  const [touched, setTouched] = useState<ReadonlySet<string>>(new Set());
  const [attempted, setAttempted] = useState(false);
  const [askingRestore, setAskingRestore] = useState(false);
  const touch = (k: string): void => setTouched((t) => (t.has(k) ? t : new Set([...t, k])));
  const set = <K extends keyof SetupForm>(k: K, v: SetupForm[K]): void => {
    setForm((f) => ({ ...f, [k]: v }));
    touch(k);
  };
  const toggle: Toggle = (k, v, on) => {
    setForm((f) => ({ ...f, [k]: on ? [...f[k], v] : f[k].filter((x) => x !== v) }));
    touch(k === "nodes" ? "nodes" : "workload");
  };
  const wizardRef = useRef<HTMLFormElement>(null);
  const headingId = useId();
  const advancedRef = useRef<HTMLDetailsElement>(null);

  useEffect(() => {
    async function loadNodes(): Promise<void> {
      try {
        const n = await api.nodes();
        setNodes(n);
        const first = (n.find((x) => x.online) ?? n[0])?.node;
        setForm((f) => (f.nodes.length > 0 || !filled(first) ? f : { ...f, nodes: [first] }));
        await Promise.all(
          n
            .filter((x) => x.online)
            .map(async ({ node }) => {
              const b = await bridgesOf(node);
              setBridgesByNode((all) => ({ ...all, [node]: b }));
            }),
        );
      } catch (err) {
        setError(errorMessage(err));
      }
    }
    async function loadStorages(): Promise<void> {
      try {
        setStorages(await api.storages());
      } catch (err) {
        setStorages([]);
        setError(errorMessage(err));
      }
    }
    void loadNodes();
    void loadStorages();
  }, []);

  const all = storages ?? [];
  // The management cluster's storages and bridge must be on every node it runs on; clusters may use
  // what any node has.
  const onEveryNode = (c: string): string[] =>
    unique(all.filter((s) => s.content.includes(c)).map((s) => s.storage)).filter((name) =>
      form.nodes.every((n) => all.some((s) => s.node === n && s.storage === name && s.content.includes(c))),
    );
  const bridges = useMemo(
    () =>
      unique(Object.values(bridgesByNode).flatMap((b) => b.map((x) => x.bridge))).filter((name) =>
        form.nodes.every((n) => (bridgesByNode[n] ?? []).some((b) => b.bridge === name)),
      ),
    [bridgesByNode, form.nodes],
  );
  const bridgesKnown = form.nodes.every((n) => bridgesByNode[n] !== undefined);
  // Once the choices are known, a preset that is not among them gives way: a default storage to the
  // first that fits, a choice made for other nodes to none, so nothing is sent that the page does
  // not show.
  const known = storages !== undefined;
  const fit = (value: string, choices: string[]): string =>
    !known || choices.includes(value) ? value : (choices[0] ?? "");
  // What the running management cluster was made with; its network and disks stay as they are.
  const fixed = manage && status.ready ? status.settings : undefined;
  const effective: SetupForm = {
    ...form,
    vmDatastore: fixed ? fixed.vmDatastore : keepOffered(form.vmDatastore, onEveryNode("images"), known),
    imageDatastore: fit(form.imageDatastore, onEveryNode("import")),
    bootstrapDatastore: fit(form.bootstrapDatastore, onEveryNode("iso")),
    bridge: keepOffered(form.bridge, bridges, bridgesKnown),
    refreshStorage: offeredRefreshStorage(form),
  };
  // A new passphrase is chosen, and repeated, only when no earlier run left a state to open.
  const firstRun = !manage && status.resumes !== true;
  const byField = setupFieldProblems(effective, firstRun, fixed);
  const problems = Object.values(byField);
  const shown = (k: SetupField): string | undefined => (attempted || touched.has(k) ? byField[k] : undefined);

  const onlineNodes = nodes.filter((n) => n.online).length;
  const clusterStorages = unique([
    ...all.filter((s) => s.content.includes("images")).map((s) => s.storage),
    ...(known ? form.workloadDatastores : []),
  ]).map((name) => {
    const here = all.filter((s) => s.storage === name && s.content.includes("images"));
    return {
      name,
      note: choiceNote(
        unique(here.map((s) => s.node)),
        here.some((s) => s.shared),
        onlineNodes,
      ),
    };
  });
  const bridgesLoaded = Object.keys(bridgesByNode).length >= onlineNodes && onlineNodes > 0;
  const clusterBridges = unique([
    ...Object.values(bridgesByNode).flatMap((b) => b.map((x) => x.bridge)),
    ...(bridgesLoaded ? form.workloadBridges : []),
  ]).map((name) => {
    const on = Object.entries(bridgesByNode)
      .filter(([, b]) => b.some((x) => x.bridge === name))
      .map(([n]) => n)
      .toSorted();
    return { name, note: choiceNote(on, false, onlineNodes) };
  });

  const start = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.setup(settings(effective), form.passphrase);
      setForm((f) => ({ ...f, passphrase: "", confirm: "" }));
      onStarted();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel" aria-labelledby={headingId}>
      {/* On the Administration page it is one section among others, headed as they are. */}
      {!manage && (
        <div className="toolbar">
          <h2 id={headingId}>Set up Kubehenge</h2>
        </div>
      )}
      {/* A form, so that password managers offer to keep the new passphrase. */}
      <form
        className={manage ? "box" : "wizard"}
        ref={wizardRef}
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          if (busy) {
            return;
          }
          if (problems.length === 0) {
            void start();
            return;
          }
          // Every problem shows, and the cursor goes to the first.
          setAttempted(true);
          const inAdvanced = [byField.mirrors, byField.podSubnets, byField.serviceSubnets].some((x) => x !== undefined);
          if (advancedRef.current && inAdvanced) {
            advancedRef.current.open = true;
          }
          requestAnimationFrame(() => wizardRef.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus());
        }}
      >
        {manage && <h3 id={headingId}>Management cluster</h3>}
        <LastRun status={status} manage={manage} />
        <fieldset className="group">
          <legend>Proxmox VE nodes</legend>
          {nodes.map((n) => (
            <label key={n.node} className="check">
              <input
                type="checkbox"
                disabled={!n.online && !form.nodes.includes(n.node)}
                checked={form.nodes.includes(n.node)}
                onChange={(e) => toggle("nodes", n.node, e.target.checked)}
              />{" "}
              {n.node}
              {!n.online && <span className="muted"> (offline)</span>}
            </label>
          ))}
          <FieldError message={shown("nodes")} />
        </fieldset>
        <Field label="VM disks" hint={fixed ? fixedHint : undefined} error={shown("vmDatastore")}>
          {(id) =>
            fixed ? (
              <input id={id} readOnly value={fixed.vmDatastore} />
            ) : (
              <Choice
                id={id}
                value={effective.vmDatastore}
                options={onEveryNode("images")}
                onChoose={(v) => set("vmDatastore", v)}
              />
            )
          }
        </Field>
        <Field label="Talos image" hint="Storages with the import content type" error={shown("imageDatastore")}>
          {(id) => (
            <Choice
              id={id}
              value={effective.imageDatastore}
              options={onEveryNode("import")}
              onChoose={(v) => set("imageDatastore", v)}
            />
          )}
        </Field>
        <Field
          label="Cloud-init images"
          hint="ISO storage that Kubehenge gets full control of"
          error={shown("bootstrapDatastore")}
        >
          {(id) => (
            <Choice
              id={id}
              value={effective.bootstrapDatastore}
              options={onEveryNode("iso")}
              onChoose={(v) => set("bootstrapDatastore", v)}
            />
          )}
        </Field>
        <Field label="Bridge" error={shown("bridge")}>
          {(id) => (
            <select id={id} value={effective.bridge} onChange={(e) => set("bridge", e.target.value)}>
              <option value="" disabled>
                Choose…
              </option>
              {bridges.map((b) => (
                <option key={b} value={b}>
                  {b}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label="VLAN tag" hint="Optional" error={shown("vlan")}>
          {(id) => (
            <input id={id} inputMode="numeric" value={form.vlan} onChange={(e) => set("vlan", e.target.value.trim())} />
          )}
        </Field>
        <Field
          label="Node addresses"
          hint={fixed ? "Add 2 addresses after the first to grow to 3" : "1 or 3, like 192.0.2.11/24"}
          error={shown("nodeAddresses")}
        >
          {(id) => (
            <textarea
              id={id}
              rows={3}
              value={form.nodeAddresses}
              onChange={(e) => set("nodeAddresses", e.target.value)}
            />
          )}
        </Field>
        <Field
          label="API address"
          hint={fixed ? fixedHint : "A free address in the nodes' network"}
          error={shown("apiAddress")}
        >
          {(id) => (
            <input
              id={id}
              readOnly={fixed !== undefined}
              value={form.apiAddress}
              onChange={(e) => set("apiAddress", e.target.value.trim())}
            />
          )}
        </Field>
        <Field label="Gateway" hint={fixed ? fixedHint : undefined} error={shown("gateway")}>
          {(id) => (
            <input
              id={id}
              readOnly={fixed !== undefined}
              value={form.gateway}
              onChange={(e) => set("gateway", e.target.value.trim())}
            />
          )}
        </Field>
        <Field label="DNS servers" hint="Separated by commas" error={shown("dnsServers")}>
          {(id) => <input id={id} value={form.dnsServers} onChange={(e) => set("dnsServers", e.target.value)} />}
        </Field>
        <WorkloadChoices
          form={form}
          storages={clusterStorages}
          bridges={clusterBridges}
          problem={shown("workload")}
          onToggle={toggle}
        />
        <HAChoice form={form} set={set} />
        <HARulesChoice form={form} set={set} />
        <DatastoreAllocateChoice form={form} set={set} />
        <RefreshChoice form={effective} set={set} shown={shown} />
        <SysAuditChoice form={form} set={set} />
        <BootLogsChoice form={form} set={set} />
        <NamespacesChoice form={form} set={set} shown={shown} />
        <HeadlampChoice form={form} set={set} />
        <HeadlampActionsChoice form={form} set={set} />
        <fieldset className="group">
          <legend>Identity providers</legend>
          <OIDCProviders initial={form.clusterOIDC} showAll={attempted} onChange={(v) => set("clusterOIDC", v)} />
        </fieldset>
        <fieldset className="group">
          <legend>Backup targets</legend>
          <BackupTargets initial={form.backupTargets} showAll={attempted} onChange={(v) => set("backupTargets", v)} />
        </fieldset>
        <EtcdRestoreChoice form={form} set={set} onAsk={() => setAskingRestore(true)} />
        <AdvancedFields form={form} set={set} shown={shown} fixed={fixed !== undefined} detailsRef={advancedRef} />
        <PassphraseFields
          form={form}
          set={set}
          shown={shown}
          hint={passphraseHint(manage, status.resumes)}
          firstRun={firstRun}
        />
        <ErrorBox message={error} />
        <div className="actions">
          {onCancel && (
            <button type="button" onClick={onCancel}>
              Cancel
            </button>
          )}
          <button type="submit" className="primary" disabled={busy}>
            {manage ? "Apply" : "Create management cluster"}
          </button>
        </div>
      </form>
      <EtcdRestoreConfirm
        open={askingRestore}
        onClose={() => setAskingRestore(false)}
        onConfirm={() => {
          set("etcdRestore", true);
          setAskingRestore(false);
        }}
      />
    </section>
  );
}
