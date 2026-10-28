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
  Dialog,
  ErrorBox,
  Field,
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
  clusterVolumeSnapshots: boolean;
  // Sent beside the settings, never kept in them; empty keeps the one stored.
  rootPassword: string;
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
    return `${ns} is no namespace name: lowercase letters, digits and dashes, at most 63.`;
  }
  if (ns === ownNamespace) {
    return `${ns} is the interface's own namespace already.`;
  }
  if (ns === "default" || ns === "cert-manager" || ns.startsWith("kube-") || ns.endsWith("-system")) {
    return `${ns} holds Kubernetes', the providers' or Kubehenge's own objects.`;
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

/** rootPasswordOf is the root@pam password a run hands over: none while volume snapshots are off. */
export const rootPasswordOf = (f: SetupForm): string => (f.clusterVolumeSnapshots ? f.rootPassword : "");

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
  if (f.clusterVolumeSnapshots) {
    s.clusterVolumeSnapshots = true;
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
    clusterVolumeSnapshots: saved.clusterVolumeSnapshots === true,
    rootPassword: "",
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
  | "rootPassword"
  | "passphrase"
  | "confirm";

// nodeNetwork checks the node addresses against each other and the gateway, as the appliance
// does: one network, the gateway in it, no address taken twice. It returns the network or the
// problem.
function nodeNetwork(
  addresses: string[],
  gateway: number | undefined,
): { ip: number; bits: number; taken: number[] } | string {
  const wrongCount = "One, or three for high availability.";
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
      return `${a} is no IPv4 address with its prefix length, such as 192.0.2.11/24.`;
    }
    const net = (ip & maskOf(n)) >>> 0;
    if (n < pointToPoint && (ip === net || ip === (net | ~maskOf(n)) >>> 0)) {
      return `${a} is its network's own or broadcast address.`;
    }
    if (taken.includes(ip)) {
      return `${a} is listed twice.`;
    }
    if (gateway !== undefined && !sameNetwork(ip, gateway, n)) {
      return `${a} is outside the gateway's network.`;
    }
    if (first && (first.bits !== n || !sameNetwork(ip, first.ip, n))) {
      return `${a} is in another network than ${addresses[0]}.`;
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
  // Switched on again, the password stored with the first switch-on stays unless another is given.
  if (f.clusterVolumeSnapshots && was?.clusterVolumeSnapshots !== true && !filled(f.rootPassword)) {
    p.rootPassword =
      "Give root@pam's password, which the CSI plugins of clusters taking volume snapshots sign in with.";
  }
  if (passphraseShort(f.passphrase)) {
    p.passphrase = `The passphrase needs at least ${minPassphrase} characters.`;
  } else if (firstRun && f.passphrase !== f.confirm) {
    p.confirm = "The passphrases differ.";
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
    p.nodes = "Choose no more Proxmox VE nodes than there are node addresses.";
  }
  if (!f.vmDatastore) {
    p.vmDatastore = "Choose a storage for the VMs' disks.";
  }
  if (!f.imageDatastore) {
    p.imageDatastore = "Choose a storage for the Talos image.";
  }
  if (!f.bootstrapDatastore) {
    p.bootstrapDatastore = "Choose a storage for cloud-init images.";
  }
  if (!f.bridge) {
    p.bridge = "Choose a bridge.";
  }
  if (f.vlan && !isVLAN(f.vlan)) {
    p.vlan = "1 to 4094.";
  }
  return p;
}

// networkProblems are those of the management cluster's addresses.
function networkProblems(f: SetupForm, was: ManagementSettings | undefined): SetupProblems {
  const p: SetupProblems = {};
  const addresses = list(f.nodeAddresses);
  const gateway = toInt(f.gateway);
  if (gateway === undefined) {
    p.gateway = "An IPv4 address.";
  }
  const network = nodeNetwork(addresses, gateway);
  if (typeof network === "string") {
    p.nodeAddresses = network;
  }
  if (was && was.nodeAddresses.some((a, i) => addresses[i] !== a)) {
    p.nodeAddresses = `The nodes there are keep their addresses (${was.nodeAddresses.join(", ")}); more can only be added after them.`;
  }
  const apiIP = toInt(f.apiAddress);
  if (apiIP === undefined) {
    p.apiAddress = "An IPv4 address.";
  } else if (apiIP === gateway || (typeof network !== "string" && network.taken.includes(apiIP))) {
    p.apiAddress = "An unused address: neither a node address nor the gateway.";
  } else if (typeof network !== "string" && !sameNetwork(apiIP, network.ip, network.bits)) {
    p.apiAddress = "Inside the nodes' network.";
  }
  const dns = list(f.dnsServers);
  if (dns.length === 0) {
    p.dnsServers = "At least one.";
  } else if (dns.some((d) => !isIP(d))) {
    p.dnsServers = "IP addresses.";
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
    p.workload = "At least one storage for clusters.";
  } else if (f.workloadBridges.length === 0) {
    p.workload = "At least one bridge for clusters.";
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
    p.mirrors = `${bad} is no registry=endpoint line, such as ghcr.io=https://mirror.example:5000.`;
  }
  for (const key of ["podSubnets", "serviceSubnets"] as const) {
    const wrong = list(f[key]).find((r) => rangeProblem(r) !== undefined);
    if (wrong !== undefined) {
      p[key] = `${wrong} is no IPv4 network such as 10.244.0.0/16, without host bits.`;
    }
  }
  if (f.moreNamespaces) {
    const names = list(f.extraNamespaces);
    const twice = names.find((n, i) => names.indexOf(n) !== i);
    const wrong = names.map((n) => namespaceProblem(n)).find((x) => x !== undefined);
    if (names.length === 0) {
      p.extraNamespaces = "Name at least one namespace, or leave clusters in their own.";
    } else if (wrong !== undefined) {
      p.extraNamespaces = wrong;
    } else if (twice !== undefined) {
      p.extraNamespaces = `${twice} is named twice.`;
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
      p.refreshAddress = "Needed for a gateway and DNS servers; leave all three empty for DHCP.";
    }
    return p;
  }
  const [base = "", bits] = f.refreshAddress.split("/");
  const ip = toInt(base);
  if (!cidr.test(f.refreshAddress) || ip === undefined) {
    p.refreshAddress = "An IPv4 address with its prefix length, such as 192.0.2.50/24.";
    return p;
  }
  const gateway = toInt(f.refreshGateway);
  if (gateway === undefined || !sameNetwork(ip, gateway, Number(bits))) {
    p.refreshGateway = "An IPv4 address in the address's network.";
  }
  if (dns.some((d) => !isIP(d))) {
    p.refreshDNS = "IP addresses.";
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
          <h2>Kubehenge is not set up yet</h2>
        </div>
        <p className="pad">
          An administrator of this Proxmox VE has to create Kubehenge's management cluster first. It needs a user with
          the Administrator role on <code>/</code>.
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
  return (
    <>
      {manage && job.state === "succeeded" && job.kind !== "remove" && (
        <p className="muted">
          {job.automatic === true
            ? `Last applied ${when} by an automatic upgrade, as ${job.user}.`
            : `Last applied ${when} by ${job.user}.`}
        </p>
      )}
      {job.state === "succeeded" && job.kind === "remove" && (
        <p className="muted">
          The management cluster was removed {when} by {job.user}.
        </p>
      )}
      {job.state === "failed" && (
        <div className="error" role="alert">
          {job.automatic === true
            ? `The automatic upgrade of ${when} failed, and is tried again in the next day's upgrade hour: ${job.error ?? ""}`
            : `The last run failed: ${job.error ?? ""}`}
          {(job.log ?? []).length > 0 && (
            <details>
              <summary>Log</summary>
              <pre className="log">{(job.log ?? []).slice(-logLines).join("\n")}</pre>
            </details>
          )}
        </div>
      )}
    </>
  );
}

// refreshSummary says in words when and where the templates are built again.
function refreshSummary(s: ManagementSettings): string {
  const f = refreshFields(s.templateRefresh);
  if (!f.refresh) {
    return "Off";
  }
  const disks = filled(f.refreshStorage)
    ? f.refreshStorage
    : (s.workloadDatastores[0] ?? "the first storage for clusters");
  const parts = [
    `Every ${weekdayName(f.refreshWeekday)} at ${f.refreshTime} (${f.refreshTimeZone})`,
    `disks on ${disks}`,
    filled(f.refreshAddress) ? `built at ${f.refreshAddress}` : "built with DHCP",
  ];
  if (f.creatorBuilds) {
    parts.push("cluster creators may have missing templates built");
  }
  return parts.join(", ");
}

// optInRows summarise the choices, each off by default, that let Kubehenge or the clusters do more
// in Proxmox VE than their pools allow.
function optInRows(s: ManagementSettings): [string, string][] {
  return [
    ["Proxmox VE HA", s.proxmoxHA === true ? "Keeps clusters' control-plane VMs on separate nodes" : "Off"],
    ["HA rules for cluster creators", s.offerHARules === true ? "Offered by name and nodes" : "Off"],
    [
      "CSI plugin's newest release",
      s.clusterDatastoreAllocate === true ? "Clusters may choose it, on a storage of their own" : "Off",
    ],
    [
      "HA group labels",
      s.clusterSysAudit === false
        ? "Off for every cluster"
        : "On unless a cluster switches them off, with Sys.Audit on /",
    ],
    [
      "Volume snapshots",
      s.clusterVolumeSnapshots === true ? "Clusters may take them, with their CSI plugin signed in as root@pam" : "Off",
    ],
  ];
}

function headlampRows(s: ManagementSettings): [string, string][] {
  return [
    [
      "Headlamp sign-in",
      s.headlampSignIn === true ? "Signs those who may download a cluster's kubeconfig in as cluster-admin" : "Off",
    ],
    ["Headlamp actions", s.headlampActions === true ? "Helm app catalog and Prometheus charts" : "Off"],
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
    ["Storages for clusters", s.workloadDatastores.join(", ")],
    ["Bridges for clusters", s.workloadBridges.join(", ")],
    ...optInRows(s),
    ["Template refresh", refreshSummary(s)],
    ["Namespaces for clusters", [ownNamespace, ...(s.extraClusterNamespaces ?? [])].join(", ")],
    ...headlampRows(s),
    [
      "Identity providers",
      s.clusterOIDC && s.clusterOIDC.length > 0 ? s.clusterOIDC.map((p) => p.issuerURL).join(", ") : "None",
    ],
    [
      "Backup targets",
      s.backupTargets && s.backupTargets.length > 0 ? s.backupTargets.map((t) => targetSummary(t)).join(", ") : "None",
    ],
    ["etcd restores", s.etcdRestore === true ? "Through the Proxmox VE guest agent" : "By hand, with the steps shown"],
    ["Registry mirrors", s.registryMirrors ? formatMirrors(s.registryMirrors).split("\n").join(", ") : "None"],
    ["Pod ranges", s.podSubnets?.join(", ") ?? "Chosen clear of the nodes' network"],
    ["Service ranges", s.serviceSubnets?.join(", ") ?? "Chosen clear of the nodes' network"],
  ];
  if (status.upgrade) {
    rows.unshift(["Applied with", `Kubehenge ${status.upgrade.applied}`]);
  }
  return (
    <section className="panel">
      <div className="box">
        <h3>Management cluster</h3>
        <p className="muted">
          Applying again brings the management cluster to the versions this appliance carries, and puts back anything
          that drifted.
        </p>
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
            Edit…
          </button>
          <button type="button" className="primary" onClick={() => setApplying(true)}>
            Apply again…
          </button>
        </div>
      </div>
      {status.ready && (s.backupTargets ?? []).length > 0 && (
        <div className="box">
          <CredentialField
            label="Backup access key for every cluster"
            purpose="backup"
            keys={backupKeys}
            hint="Taken by every cluster that backs up without a key of its own. One key for all lets each cluster's Velero reach the others' folders in the target, though not read them: each cluster's backups are encrypted with keys of its own. A key per cluster, limited to its folder, keeps them apart."
          />
        </div>
      )}
      {applying && (
        <ApplyDialog
          title="Apply the management cluster again"
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
      <p className="muted">Runs with your Proxmox VE session, which is not stored, and takes ten to twenty minutes.</p>
      <Field
        label="State passphrase"
        hint={`The passphrase given when the management cluster was created; at least ${minPassphrase} characters.`}
      >
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
    return "less than a minute";
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
          {job.automatic === true ? `started automatically as ${job.user}` : `started by ${job.user}`},{" "}
          {elapsed(job.started)} ago
        </span>
      </div>
      <p className="pad muted">
        This takes ten to twenty minutes. The page follows along; closing it does not stop the run.
      </p>
      <p className="pad" aria-live="polite">
        {log.length > 0 ? `Now: ${log.at(-1)}` : "Starting…"}
      </p>
      {job.lines !== undefined && (
        <p className="pad muted">
          The last {log.length} of {job.lines} lines.
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

const fixedHint = "Fixed once the management cluster exists.";

// keepOffered keeps a choice once the choices are known only if it is among them.
const keepOffered = (value: string, choices: string[], ready: boolean): string =>
  !ready || choices.includes(value) ? value : "";

function passphraseHint(manage: boolean, resumes: boolean | undefined): string {
  if (manage) {
    return "The passphrase given when the management cluster was created.";
  }
  return resumes === true
    ? "The passphrase of the earlier run: its encrypted state is still on the appliance and only that passphrase opens it."
    : `At least ${minPassphrase} characters. Encrypts the management cluster's state on the appliance. It is not stored: every later upgrade or repair asks for it, and without it the state cannot be read. Keep it in a password manager.`;
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
      <legend>What clusters may use</legend>
      <p className="muted">
        Clusters run on any node, so this lists what every node has. Clusters may choose only what is ticked.
      </p>
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
        Keep clusters&apos; control-plane VMs on separate nodes with Proxmox VE HA
      </label>
      <p className="muted" id={hint}>
        For clusters on shared storage, unless a cluster opts out. Proxmox VE HA also restarts a VM on another node when
        its node fails, and a node that runs one restarts itself if it loses quorum. Kubehenge&apos;s controller gets a
        second API token with Sys.Console on /, which amounts to a root shell on every node; switching this on or off
        takes Sys.Console on / of you too.
      </p>
    </fieldset>
  );
}

function HARulesChoice({ form, set }: Omit<FieldsProps, "shown">): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>HA rules for cluster creators</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.offerHARules}
          aria-describedby={hint}
          onChange={(e) => set("offerHARules", e.target.checked)}
        />{" "}
        Offer Proxmox VE&apos;s HA node-affinity rules as placements to everyone who may create or change a cluster
      </label>
      <p className="muted" id={hint}>
        Grants: those without Sys.Audit on / of their own can place a cluster on the nodes of an HA rule, seeing each
        rule&apos;s name and the nodes it allows, never its comment or resources. Kubehenge&apos;s controller reads the
        rules with its own token, which holds Sys.Audit on / already, so Proxmox VE grants nothing new. Risks: everyone
        with VM.Allocate on the clusters&apos; pool or a cluster&apos;s own pool learns the rules&apos; names and which
        nodes they cover.
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
        Sign those who may download a cluster&apos;s kubeconfig in to it in Headlamp (recommended)
      </label>
      <p className="muted" id={hint}>
        Grants: whoever holds Sys.Modify on a cluster&apos;s pool opens it in the appliance&apos;s Headlamp as
        cluster-admin, without pasting a token, with one made for their session that never reaches the browser. No one
        else gets more than today. Risks: each cluster in the interface&apos;s namespaces gets an account bound to
        cluster-admin and one that may make its tokens, whose token waits beside the cluster in the management cluster,
        where the admin kubeconfigs already are; Headlamp acts as that account, not as the Proxmox VE user, whom the
        interface&apos;s log names beside the token&apos;s ID. Switching it off removes both accounts, which ends every
        token made.
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
        Offer Helm and Prometheus charts in the appliance&apos;s Headlamp
      </label>
      <p className="muted" id={hint}>
        Grants: Headlamp&apos;s Helm app catalog and Prometheus charts, with which everyone who sees a cluster lists,
        installs, upgrades, rolls back and uninstalls its Helm releases. Each acts in the cluster with their own
        credentials there, or the sign-in above, so the cluster lets them do no more than it already does. Only those
        who may set the appliance up add or remove a Helm repository, since one list serves every user and cluster.
        Risks: the appliance fetches the charts, from those repositories alone and over HTTPS. Node drain is offered
        either way, with the user&apos;s own credentials; port forwards work in Headlamp&apos;s desktop app alone and
        stay off here.
      </p>
    </fieldset>
  );
}

const etcdRestoreGrant =
  "Kubehenge's controller gets an API token of its own with VM.GuestAgent.Unrestricted and VM.Audit on the clusters' pool. It runs commands as root in every cluster VM through the guest agent, so whoever takes the management cluster has a root shell in all of them; switching this on takes VM.GuestAgent.Unrestricted on / of you too.";

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
        Restore clusters&apos; etcd through the Proxmox VE guest agent
      </label>
      <p className="muted" id={hint}>
        Restoring a cluster&apos;s etcd from a snapshot stops the control plane on its VMs and restores on one of them.
        Switched on, Kubehenge does that itself when someone with Sys.Modify on the cluster&apos;s pool asks; switched
        off, the interface lists the commands to run on the VMs by hand. Talos clusters are restored through their
        nodes&apos; Talos API either way. {etcdRestoreGrant}
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
        Let clusters choose the CSI plugin&apos;s newest release
      </label>
      <p className="muted" id={hint}>
        The newest release needs Datastore.Allocate on a cluster&apos;s storage. A cluster that chooses it gets that on
        its storage, which must be one of the storages for clusters that no other cluster, the management cluster or the
        VM templates use. Kubehenge&apos;s controller gets Datastore.Allocate on the storages for clusters the
        management cluster does not use, to grant it.
      </p>
    </fieldset>
  );
}

// offeredRefreshStorage sends template disks the clusters may no longer use back to the first that
// they may.
const offeredRefreshStorage = (f: SetupForm): string =>
  f.workloadDatastores.includes(f.refreshStorage) ? f.refreshStorage : "";

function RefreshChoice({ form, set, shown }: FieldsProps): JSX.Element {
  const hint = useId();
  const creatorsHint = useId();
  return (
    <fieldset className="group">
      <legend>Template refresh</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.refresh}
          aria-describedby={hint}
          onChange={(e) => set("refresh", e.target.checked)}
        />{" "}
        Build the node OS VM templates again every week
      </label>
      <p className="muted" id={hint}>
        Each build takes its node OS&apos;s updates. A cluster&apos;s nodes move to a build in the cluster&apos;s
        maintenance window once it is a day old, unless the cluster switches that off; the two newest builds of each
        template stay. The builds run with an API token of their own, which may create and delete VMs in the template
        pool and upload and delete images on the storages they use.
      </p>
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
          <Field label="Template disks" hint="One of the storages for clusters.">
            {(id) => (
              <select id={id} value={form.refreshStorage} onChange={(e) => set("refreshStorage", e.target.value)}>
                <option value="">
                  {form.workloadDatastores[0] === undefined
                    ? "The first storage for clusters"
                    : `${form.workloadDatastores[0]} (the first storage for clusters)`}
                </option>
                {form.workloadDatastores.slice(1).map((d) => (
                  <option key={d} value={d}>
                    {d}
                  </option>
                ))}
              </select>
            )}
          </Field>
          <Field
            label="Build address"
            hint="The VM that prepares an image gets its address by DHCP when this is empty. Otherwise an unused address with its prefix length, such as 192.0.2.50/24."
            error={shown("refreshAddress")}
          >
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
              <Field label="Build DNS servers" hint="Separated by commas." error={shown("refreshDNS")}>
                {(id) => <input id={id} value={form.refreshDNS} onChange={(e) => set("refreshDNS", e.target.value)} />}
              </Field>
            </>
          )}
          <label className="check">
            <input
              type="checkbox"
              checked={form.creatorBuilds}
              aria-describedby={creatorsHint}
              onChange={(e) => set("creatorBuilds", e.target.checked)}
            />{" "}
            Let those who may create clusters have a missing template built
          </label>
          <p className="muted" id={creatorsHint}>
            Recommended. A cluster creator who finds no template of the distribution on the node OS chosen, none of the
            Kubernetes version for kubeadm, or none of the Talos release for Talos, can ask for it from the create
            wizard; the build runs with the refresh&apos;s token, not theirs. Without it, only an administrator can
            build templates.
          </p>
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
        Label clusters&apos; nodes with Proxmox VE&apos;s HA groups
      </label>
      <p className="muted" id={hint}>
        The cloud controller manager reads Proxmox VE&apos;s HA groups and node-affinity rules, which takes Sys.Audit on
        /. Each cluster gets that, unpropagated, through a group Kubehenge&apos;s controller adds its user to, unless
        the cluster switches the labels off; its token can then read the Proxmox VE cluster&apos;s own configuration
        (firewall rules, backup jobs, HA, the corosync configuration), but nothing of its guests, storages or nodes.
        Off, no cluster gets Sys.Audit on / and nodes go without the labels.
      </p>
    </fieldset>
  );
}

function VolumeSnapshotsChoice({
  form,
  set,
  shown,
  fixed,
}: FieldsProps & { fixed: ManagementSettings | undefined }): JSX.Element {
  const hint = useId();
  const stored = fixed?.clusterVolumeSnapshots === true;
  return (
    <fieldset className="group">
      <legend>Volume snapshots</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.clusterVolumeSnapshots}
          aria-describedby={hint}
          onChange={(e) => set("clusterVolumeSnapshots", e.target.checked)}
        />{" "}
        Let clusters take volume snapshots
      </label>
      <p className="muted" id={hint}>
        The CSI plugin copies volumes for snapshots, restores and clones through a Proxmox VE method that only root@pam
        may call, with a password and without a second factor; no API token can. A cluster that chooses volume snapshots
        gets root@pam&apos;s password in its CSI plugin&apos;s configuration, so whoever takes over that cluster or
        reads that Secret holds the whole Proxmox VE cluster. The password is kept in the management cluster, never on
        the appliance. Switching this off removes it, and clusters go back to their own tokens.
      </p>
      {form.clusterVolumeSnapshots && (
        <Field
          label="root@pam's password"
          hint={stored ? "Leave empty to keep the one stored." : "Proxmox VE checks it before anything is applied."}
          error={shown("rootPassword")}
        >
          {(id) => (
            <input
              id={id}
              type="password"
              autoComplete="off"
              value={form.rootPassword}
              onChange={(e) => set("rootPassword", e.target.value)}
            />
          )}
        </Field>
      )}
    </fieldset>
  );
}

function NamespacesChoice({ form, set, shown }: FieldsProps): JSX.Element {
  const hint = useId();
  return (
    <fieldset className="group">
      <legend>Namespaces for clusters</legend>
      <label className="check">
        <input
          type="checkbox"
          checked={form.moreNamespaces}
          aria-describedby={hint}
          onChange={(e) => set("moreNamespaces", e.target.checked)}
        />{" "}
        Let cluster creators choose among more namespaces than {ownNamespace}
      </label>
      <p className="muted" id={hint}>
        Such as one per team. Each cluster still has its own pool, and seeing or changing it still takes the privileges
        on that pool. A namespace is created if it is missing; one taken off the list keeps its clusters, which are then
        changed with kubectl.
      </p>
      {form.moreNamespaces && (
        <Field label="Namespaces" hint="Separated by commas, such as team-a, team-b." error={shown("extraNamespaces")}>
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
      title="Restore etcd through the guest agent?"
      onClose={onClose}
      onSubmit={onConfirm}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger">
            Switch on
          </button>
        </>
      }
    >
      <p>
        <strong>What it grants.</strong> {etcdRestoreGrant}
      </p>
      <p>
        <strong>What it risks.</strong> The token sits in the management cluster beside Kubehenge&apos;s other
        credentials. Anyone who reads it, or who controls Kubehenge&apos;s controller, can run anything as root in any
        cluster VM: read every Secret, change any workload, or stop a cluster. A restore itself takes every object of a
        cluster back to the snapshot.
      </p>
      <p>
        <strong>Left off</strong>, there is no such token: a restore takes the same steps, run by hand by someone with a
        shell on the VMs.
      </p>
      <p className="hint">The change takes effect when the settings are applied.</p>
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
  const rangeHint = fixed ? fixedHint : "Chosen clear of the nodes' network when empty.";
  return (
    <details className="advanced" ref={detailsRef}>
      <summary>Advanced</summary>
      <Field
        label="Registry mirrors"
        hint="registry=endpoint, one per line, such as ghcr.io=https://mirror.example:5000. Tried before the registry itself."
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
      <Field label="State passphrase" hint={hint} error={shown("passphrase")}>
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
      await api.setup(settings(effective), form.passphrase, rootPasswordOf(effective));
      setForm((f) => ({ ...f, passphrase: "", confirm: "", rootPassword: "" }));
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
        <p className="muted">
          {manage
            ? "Applying brings the management cluster to these settings and the versions this appliance carries. It runs with your Proxmox VE session, which is not stored."
            : "Kubehenge's management cluster runs Cluster API and creates every other cluster. It is created with your Proxmox VE session, which is not stored."}
        </p>
        <LastRun status={status} manage={manage} />
        <fieldset className="group">
          <legend>Proxmox VE nodes</legend>
          <p className="muted">
            The management cluster's VMs are placed on these in turn. Three nodes for three VMs keep it running when one
            node fails.
          </p>
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
        <Field
          label="VM disks"
          hint={fixed ? fixedHint : "Storage for the management cluster's disks."}
          error={shown("vmDatastore")}
        >
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
        <Field
          label="Talos image"
          hint="Storage the Talos disk image is downloaded to (content type import)."
          error={shown("imageDatastore")}
        >
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
          hint="Cluster API keeps each new VM's cloud-init ISO here and gets full control of it."
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
        <Field label="VLAN tag" hint="Optional." error={shown("vlan")}>
          {(id) => (
            <input id={id} inputMode="numeric" value={form.vlan} onChange={(e) => set("vlan", e.target.value.trim())} />
          )}
        </Field>
        <Field
          label="Node addresses"
          hint={
            fixed
              ? "The nodes there are keep their addresses. A single node grows to three by adding two addresses after its own."
              : "One, or three for high availability, with prefix length: 192.0.2.11/24."
          }
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
          hint={fixed ? fixedHint : "An unused address in the same network; it moves to a healthy node."}
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
        <Field label="DNS servers" hint="One per line or separated by commas." error={shown("dnsServers")}>
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
        <VolumeSnapshotsChoice form={form} set={set} shown={shown} fixed={fixed} />
        <NamespacesChoice form={form} set={set} shown={shown} />
        <HeadlampChoice form={form} set={set} />
        <HeadlampActionsChoice form={form} set={set} />
        <fieldset className="group">
          <legend>Identity providers</legend>
          <p className="muted">
            OpenID Connect providers every new cluster accepts sign-ins from, besides tokens and certificates. A cluster
            can drop them or, with Sys.Modify on its pool, add its own. Clusters made before a change keep what they
            had.
          </p>
          <OIDCProviders initial={form.clusterOIDC} showAll={attempted} onChange={(v) => set("clusterOIDC", v)} />
        </fieldset>
        <fieldset className="group">
          <legend>Backup targets</legend>
          <p className="muted">
            S3 stores clusters back up to, with Velero and as etcd snapshots. Each cluster writes into a folder of its
            own beneath the prefix, with an access key of its own or the one for every cluster, and encrypts what it
            writes with keys of its own.
          </p>
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
            {manage ? "Apply" : "Create the management cluster"}
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
