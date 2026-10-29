import {
  backupKeys,
  backupProblems,
  backupsText,
  backupsVariable,
  classTargets,
  newBackupFields,
  withTarget,
  type BackupFields,
} from "./Backups";
import {
  certificatesProblems,
  certificatesText,
  certificatesVariable,
  needsClusterToken,
  newCertificates,
  tokenLabel,
  type CertificatesForm,
} from "./Certificates";
import {
  defaultMinutes,
  defaultStart,
  describe,
  refreshesNodes,
  type UpgradeMode,
  type UpgradePolicy,
} from "./ClusterUpgrades";
import {
  gitOpsFields,
  gitOpsProblems,
  gitOpsText,
  gitOpsVariable,
  newGitOps,
  type GitOpsChoice,
  type GitOpsField,
} from "./GitOps";
import { addressesProblem, loadBalancerText, loadBalancerVariable, type Implementation } from "./LoadBalancer";
import { logsProblem, logsText, logsVariable, newLogs, type Logs } from "./Logs";
import { addressBits, addressCount, inList, ipv4, isVLAN, list, networkOf, rangeProblem, whole } from "./network";
import { choosable, defaultNodeOS, findNodeOS, nodeOSKey, nodeOSRef } from "./node-os";
import { cleanProviders, oidcProblems, oidcProviders } from "./OIDCProviders";
import { allowedNodes, anyNode, haText, placeProblem, placeText, type Place } from "./Placement";
import {
  cisText,
  enforced,
  podSecurityOf,
  podSecurityText,
  podSecurityVariable,
  type PodSecurity,
} from "./PodSecurity";
import { forRelease, talosRelease } from "./Talos";
import type {
  Bridge,
  Cluster,
  ClusterClass,
  CreateRequest,
  FreeAddresses,
  NodeOSChoice,
  OIDCProvider,
  Schema,
  Storage,
  TalosReleases,
  Versions,
} from "./types";
import {
  browserZone,
  cniName,
  csiText,
  haGroupsText,
  snapshotsText,
  distributionName,
  filled,
  headlampText,
  istioText,
  maxWorkers,
  registeredWithRancher,
  sentence,
  sole,
} from "./ui";
import { quantity, record, text, texts } from "./values";

export const steps = ["General", "Network", "Machines", "Add-ons", "Identity providers", "Confirm"] as const;
export type Step = (typeof steps)[number];

export interface Machine {
  cores: number;
  memoryMiB: number;
  diskGiB: number;
}

// What a machine starts with when its class names no default.
const defaultMachine: Machine = { cores: 2, memoryMiB: 4096, diskGiB: 30 };

// The least memory a machine takes when its class names no minimum, and the steps Proxmox VE
// counts it in.
export const minMemoryMiB = 1024;
export const memoryStepMiB = 8;

const mibPerGiB = 1024;

// Odd sizes keep etcd's quorum.
// oxlint-disable-next-line eslint/no-magic-numbers -- the sizes themselves, named by the constant
export const controlPlaneSizes = [1, 3, 5, 7];

export interface Form {
  /** Empty while the interface creates clusters in one namespace only. */
  namespace: string;
  name: string;
  className: string;
  /** The node OS as one word, such as almalinux-10; empty while the class offers none. */
  nodeOS: string;
  version: string;
  /** The Talos release a Talos cluster's nodes run; empty takes the newest with a template. */
  talosVersion: string;
  controlPlaneReplicas: number;
  /** The workers, or with workerAutoscale the fewest the cluster autoscaler keeps. */
  workerReplicas: number;
  workerAutoscale: boolean;
  /** The most workers the cluster autoscaler adds, with workerAutoscale. */
  workerMax: number;
  endpoint: string;
  bridge: string;
  vlan: string;
  addresses: string;
  prefix: number;
  gateway: string;
  dnsServers: string;
  loadBalancer: Implementation;
  loadBalancerAddresses: string;
  pods: string;
  services: string;
  storage: string;
  place: Place;
  proxmoxHA: boolean;
  datastoreAllocate: boolean;
  volumeSnapshots: boolean;
  haGroups: boolean;
  controlPlaneMachine: Machine;
  workerMachine: Machine;
  cni: string;
  headlamp: boolean;
  headlampHostname: string;
  /** Headlamp serves everyone through its own account with Kubernetes' view role. */
  headlampView: boolean;
  monitoring: boolean;
  grafanaHostname: string;
  logs: Logs;
  podSecurity: PodSecurity;
  cis: boolean;
  istio: boolean;
  clusterMesh: boolean;
  meshPeers: string[];
  rancherInstall: boolean;
  rancherHostname: string;
  rancherRegister: boolean;
  certificates: CertificatesForm;
  // The DNS provider's credential, stored for the cluster once it is created; never in the request.
  dns01Token: string;
  gitops: GitOpsChoice;
  oidc: OIDCProvider[];
  upgradeMode: UpgradeMode;
  backups: boolean;
  backupFields: BackupFields;
  /** Whether the cluster gets an access key of its own, stored once it is created. */
  backupOwnKey: boolean;
  backupKey: Record<string, string>;
}

/** newUpgrades is the policy a new cluster starts with: every day from 02:00 for four hours, in the browser's time zone. */
export const newUpgrades = (mode: Form["upgradeMode"], timeZone: string): UpgradePolicy => ({
  mode,
  window: { start: defaultStart, durationMinutes: defaultMinutes, timeZone },
});

export const versionExamples: Record<string, string> = {
  rke2: "v1.36.5+rke2r1",
  k3s: "v1.36.5+k3s1",
  kubeadm: "v1.36.5",
  talos: "v1.36.5",
};

export function variable(c: ClusterClass | undefined, name: string): Schema | undefined {
  return c?.variables.find((v) => v.name === name)?.schema;
}

function machineDefaults(s: Schema | undefined): Machine {
  const d = record(s?.default);
  return {
    cores: quantity(d["cores"]) ?? defaultMachine.cores,
    memoryMiB: quantity(d["memoryMiB"]) ?? defaultMachine.memoryMiB,
    diskGiB: quantity(d["diskGiB"]) ?? defaultMachine.diskGiB,
  };
}

/** machineMinimums is the least a class lets one of its machine variables give a node. */
export function machineMinimums(c: ClusterClass | undefined, key: "controlPlaneMachine" | "workerMachine"): Machine {
  const schema = variable(c, key)?.properties;
  return {
    cores: schema?.["cores"]?.minimum ?? 1,
    memoryMiB: schema?.["memoryMiB"]?.minimum ?? minMemoryMiB,
    diskGiB: schema?.["diskGiB"]?.minimum ?? 1,
  };
}

/**
 * controlPlaneMinimums is the least a class lets a cluster's control planes have. Without workers
 * they run the workloads too, so they need the class's least worker on top, as the controller asks.
 */
export function controlPlaneMinimums(c: ClusterClass | undefined, workers: number): Machine {
  const min = machineMinimums(c, "controlPlaneMachine");
  if (workers > 0) {
    return min;
  }
  const worker = variable(c, "workerMachine")?.properties;
  return {
    ...min,
    cores: min.cores + (worker?.["cores"]?.minimum ?? 0),
    memoryMiB: min.memoryMiB + (worker?.["memoryMiB"]?.minimum ?? 0),
  };
}

// The add-ons by their keys in a class's kubehenge.dev/addon-memory figures, as people know them.
const addonNames = [
  ["headlamp", "Headlamp"],
  ["monitoring", "monitoring"],
  ["logs", "logs"],
  ["kyverno", "Kyverno"],
  ["istio", "Istio"],
  ["clusterMesh", "Cluster Mesh"],
  ["loadBalancer", "the load balancer"],
  ["certManager", "cert-manager"],
  ["rancher", "Rancher"],
  ["rancherAgent", "Rancher's agent"],
  ["backups", "Velero"],
  ["argocd", "Argo CD"],
  ["flux", "Flux"],
] as const;

// maxCiliumName is the longest cluster name Cilium takes, which names the cluster in a mesh.
const maxCiliumName = 32;

/** offersMesh says whether a form's cluster can join a Cluster Mesh: its class offers it and it runs Cilium. */
export const offersMesh = (form: Form, c: ClusterClass | undefined): boolean =>
  variable(c, "clusterMesh") !== undefined && form.cni === "cilium";

const meshChosen = (form: Form): boolean => form.clusterMesh && form.cni === "cilium";

/** logsChosen says whether a form's cluster keeps logs: chosen beside monitoring, which they need. */
export const logsChosen = (form: Form): boolean => form.logs.enabled && form.monitoring;

// classFigures reads one of a class's annotations of figures: kubehenge.dev/addon-memory or
// kubehenge.dev/addon-cpu by add-on, or kubehenge.dev/node-memory or kubehenge.dev/node-cpu, what a
// controlPlane or worker takes itself.
function classFigures(c: ClusterClass | undefined, annotation: string): Record<string, unknown> {
  try {
    return record(JSON.parse(c?.annotations?.[annotation] ?? "{}"));
  } catch {
    // The controller refuses clusters of such a class with the reason.
    return {};
  }
}

/**
 * addonMemory is the memory, in MiB, the pods of the add-ons a form chooses use by its class's
 * figures, what they add to each control plane's own use, and which they are. Rancher's agent counts
 * wherever the cluster would register, as the controller counts it: once a cluster installs Rancher,
 * every other one registers with it.
 */
export function addonMemory(
  form: Form,
  c: ClusterClass | undefined,
): { names: string[]; mib: number; controlPlaneMiB: number } {
  const { names, total, growth } = addonTotal(form, c, {
    pods: "kubehenge.dev/addon-memory",
    growth: "kubehenge.dev/addon-control-plane-memory",
  });
  return { names, mib: total, controlPlaneMiB: growth };
}

/**
 * addonCPU is the CPU, in millicores, the pods of the add-ons a form chooses request by its class's
 * figures, and which add-ons ask for any, counted as addonMemory counts them.
 */
export function addonCPU(form: Form, c: ClusterClass | undefined): { names: string[]; millicores: number } {
  const { names, total } = addonTotal(form, c, { pods: "kubehenge.dev/addon-cpu" });
  return { names, millicores: total };
}

// addonTotal sums a class's figures by add-on over the add-ons a form chooses: the pods' annotation,
// and where given the one of what they add to each control plane's own use.
function addonTotal(
  form: Form,
  c: ClusterClass | undefined,
  annotations: { pods: string; growth?: string },
): { names: string[]; total: number; growth: number } {
  const figures = classFigures(c, annotations.pods);
  const grows = annotations.growth === undefined ? {} : classFigures(c, annotations.growth);
  const on: Record<(typeof addonNames)[number][0], boolean> = {
    headlamp: form.headlamp,
    monitoring: form.monitoring,
    logs: logsChosen(form),
    kyverno: form.podSecurity.kyverno !== "off",
    istio: form.istio,
    clusterMesh: meshChosen(form),
    loadBalancer: form.loadBalancer !== "",
    certManager: form.monitoring || form.rancherInstall || form.certificates.enabled,
    rancher: form.rancherInstall,
    rancherAgent: !form.rancherInstall && form.rancherRegister,
    backups: form.backups && variable(c, "backups") !== undefined,
    argocd: form.gitops.engine === "argocd",
    flux: form.gitops.engine === "flux",
  };
  const names: string[] = [];
  let total = 0;
  let growth = 0;
  for (const [key, name] of addonNames) {
    const figure = quantity(figures[key]) ?? 0;
    const grown = quantity(grows[key]) ?? 0;
    if (on[key] && (figure > 0 || grown > 0)) {
      names.push(name);
      total += figure;
      growth += grown;
    }
  }
  return { names, total, growth };
}

/**
 * mostWorkers is the most workers a form's cluster can have at once. An autoscaled pool that may
 * grow keeps the control planes to the control plane's work, whatever its fewest.
 */
export const mostWorkers = (form: Form): number => (form.workerAutoscale ? form.workerMax : form.workerReplicas);

/** nodesNeeded is how many node addresses a form's cluster takes: a rollout makes each new node before it removes an old one. */
export const nodesNeeded = (form: Form): number => form.controlPlaneReplicas + mostWorkers(form) + 1;

const plainLeast = (form: Form, c: ClusterClass | undefined, key: "controlPlaneMachine" | "workerMachine"): Machine =>
  key === "controlPlaneMachine" ? controlPlaneMinimums(c, mostWorkers(form)) : machineMinimums(c, key);

// carries says whether a form's machines of one kind run its add-ons: the workers, or without
// workers the control planes.
const carries = (form: Form, key: "controlPlaneMachine" | "workerMachine"): boolean =>
  (key === "workerMachine") === form.workerReplicas > 0;

/**
 * addonNeedMiB is the memory each node that carries a form's add-ons needs for them, as the
 * controller asks: what such a node uses itself with them, by its class, and its share of the
 * add-ons' pods. Without add-ons it is 0.
 */
export function addonNeedMiB(form: Form, c: ClusterClass | undefined): number {
  const key = form.workerReplicas > 0 ? "workerMachine" : "controlPlaneMachine";
  const nodes = key === "workerMachine" ? form.workerReplicas : form.controlPlaneReplicas;
  const { mib, controlPlaneMiB } = addonMemory(form, c);
  if (mib === 0 || nodes <= 0) {
    return 0;
  }
  const own =
    key === "workerMachine"
      ? (quantity(classFigures(c, "kubehenge.dev/node-memory")["worker"]) ?? plainLeast(form, c, key).memoryMiB)
      : (quantity(classFigures(c, "kubehenge.dev/node-memory")["controlPlane"]) ?? plainLeast(form, c, key).memoryMiB) +
        controlPlaneMiB;
  return own + Math.ceil(mib / nodes / memoryStepMiB) * memoryStepMiB;
}

const millicoresPerCore = 1000;

/**
 * addonNeedCores is the cores each node that carries a form's add-ons needs for the CPU their pods
 * request, as the controller asks: what such a node takes itself, by its class, and its share of
 * the add-ons'. Without add-ons that request CPU it is 0.
 */
export function addonNeedCores(form: Form, c: ClusterClass | undefined): number {
  const key = form.workerReplicas > 0 ? "workerMachine" : "controlPlaneMachine";
  const nodes = key === "workerMachine" ? form.workerReplicas : form.controlPlaneReplicas;
  const { millicores } = addonCPU(form, c);
  if (millicores === 0 || nodes <= 0) {
    return 0;
  }
  const own =
    quantity(classFigures(c, "kubehenge.dev/node-cpu")[key === "workerMachine" ? "worker" : "controlPlane"]) ??
    plainLeast(form, c, key).cores * millicoresPerCore;
  return Math.ceil((own + Math.ceil(millicores / nodes)) / millicoresPerCore);
}

/**
 * controlPlaneNeedMiB is the memory each control plane of a form with workers and add-ons needs, as
 * the controller asks: what it uses itself, by its class, with what the add-ons add to it. Otherwise
 * it is 0.
 */
export function controlPlaneNeedMiB(form: Form, c: ClusterClass | undefined): number {
  const own = quantity(classFigures(c, "kubehenge.dev/node-memory")["controlPlane"]);
  const { names, controlPlaneMiB } = addonMemory(form, c);
  return mostWorkers(form) > 0 && names.length > 0 && own !== undefined ? own + controlPlaneMiB : 0;
}

/**
 * machineLeast is the least a form lets its machines of one kind have: the class's, or what the
 * add-ons ask of them, whichever is more.
 */
export function machineLeast(
  form: Form,
  c: ClusterClass | undefined,
  key: "controlPlaneMachine" | "workerMachine",
): Machine {
  const min = plainLeast(form, c, key);
  if (carries(form, key)) {
    return {
      ...min,
      cores: Math.max(min.cores, addonNeedCores(form, c)),
      memoryMiB: Math.max(min.memoryMiB, addonNeedMiB(form, c)),
    };
  }
  if (key === "controlPlaneMachine") {
    return { ...min, memoryMiB: Math.max(min.memoryMiB, controlPlaneNeedMiB(form, c)) };
  }
  return min;
}

/**
 * withRoom raises a form's machines to the least its other choices ask of them: control planes that
 * run the workloads without workers, the add-ons' memory and CPU, and Rancher's cores. It lowers
 * nothing.
 */
export function withRoom(form: Form, c: ClusterClass | undefined): Form {
  const raised = { ...form };
  for (const key of ["controlPlaneMachine", "workerMachine"] as const) {
    const least = machineLeast(form, c, key);
    const rancher =
      form.rancherInstall && (key === "controlPlaneMachine" || form.workerReplicas > 0) ? rancherMinimumCores : 0;
    const m = form[key];
    raised[key] = {
      ...m,
      cores: Math.max(m.cores, least.cores, rancher),
      memoryMiB: Math.max(m.memoryMiB, least.memoryMiB),
    };
  }
  return raised;
}

/**
 * withBackupChoices takes the one backup target a class offers until another is chosen, and a key of
 * the cluster's own only for those who may store one.
 */
export const withBackupChoices = (f: Form, c: ClusterClass | undefined, canTrust: boolean | undefined): Form => ({
  ...f,
  backupFields: withTarget(f.backupFields, classTargets(c)),
  backupOwnKey: f.backupOwnKey && canTrust !== false,
});

/**
 * withTalosRelease takes the newest Talos release with a template until another is chosen, and
 * the newest Kubernetes version offered that the release runs whenever the one chosen is not.
 */
export function withTalosRelease(f: Form, talos: TalosReleases | undefined, offered: Versions | undefined): Form {
  const talosVersion = filled(f.talosVersion) ? f.talosVersion : (talos?.templates[0] ?? "");
  const runs = forRelease(offered, talos, talosVersion)?.versions ?? [];
  const version = filled(f.version) && runs.length > 0 && !runs.includes(f.version) ? (runs[0] ?? "") : f.version;
  return { ...f, talosVersion, version };
}

/** BridgeNetwork is what a node says of the network on a bridge: its own address there, its gateway and its DNS servers. */
export interface BridgeNetwork {
  cidr?: string | undefined;
  gateway?: string | undefined;
  dns: string[];
}

/**
 * bridgeNetwork is the network on a bridge, from a node that reaches its gateway through it, that
 * network written like 192.0.2.0/24, and the node.
 */
export function bridgeNetwork(
  bridge: string,
  bridgesByNode: Record<string, Bridge[]>,
  dnsByNode: Record<string, string[]>,
): { net?: BridgeNetwork; network?: string; node?: string } {
  const on = Object.values(bridgesByNode)
    .flat()
    .find((b) => b.bridge === bridge && filled(b.gateway));
  if (on === undefined) {
    return {};
  }
  const net = { cidr: on.cidr, gateway: on.gateway, dns: dnsByNode[on.node] ?? [] };
  const network = on.cidr === undefined ? undefined : networkOf(on.cidr);
  return network === undefined ? { net, node: on.node } : { net, network, node: on.node };
}

/** addressesOf are a form's node addresses and API address, as entries to probe. */
export const addressesOf = (f: Form): string[] => [...list(f.addresses), f.endpoint].filter((a) => filled(a));

/** withFreeAddresses fills the API address and the node addresses with those found free, where the user has given none. */
export function withFreeAddresses(f: Form, free: FreeAddresses | undefined, touched: (k: keyof Form) => boolean): Form {
  const filledIn = (k: "endpoint" | "addresses"): string => (f[k] === "" && !touched(k) ? (free?.[k] ?? "") : f[k]);
  return { ...f, endpoint: filledIn("endpoint"), addresses: filledIn("addresses") };
}

/**
 * bridgeOf is the bridge the cluster attaches to: the one chosen, else the only one it may use, else
 * the first it may use that a node reaches its gateway through.
 */
export function bridgeOf(chosen: string, options: Bridge[], bridgesByNode: Record<string, Bridge[]>): string {
  if (filled(chosen)) {
    return chosen;
  }
  const usable = options.filter((b) => b.usable).map((b) => b.bridge);
  const uplink = usable.find((name) => bridgeNetwork(name, bridgesByNode, {}).net !== undefined);
  return sole(usable) ?? uplink ?? "";
}

/**
 * withBridgeNetwork fills the prefix length, gateway and DNS servers from the bridge's network,
 * where the user has given none of their own.
 */
export function withBridgeNetwork(f: Form, net: BridgeNetwork | undefined, touched: (k: keyof Form) => boolean): Form {
  if (net === undefined) {
    return f;
  }
  const bits = Number(net.cidr?.split("/")[1]);
  return {
    ...f,
    prefix: touched("prefix") || !whole(bits, 1, addressBits) ? f.prefix : bits,
    gateway: f.gateway === "" && !touched("gateway") ? (net.gateway ?? "") : f.gateway,
    dnsServers: f.dnsServers === "" && !touched("dnsServers") ? net.dns.join(", ") : f.dnsServers,
  };
}

/** newForm is the wizard before anything is chosen. */
export const newForm = (): Form => ({
  namespace: "",
  name: "",
  className: "",
  nodeOS: "",
  version: "",
  talosVersion: "",
  controlPlaneReplicas: 1,
  workerReplicas: 1,
  workerAutoscale: false,
  workerMax: 3,
  endpoint: "",
  bridge: "",
  vlan: "",
  addresses: "",
  prefix: 24,
  gateway: "",
  dnsServers: "",
  loadBalancer: "",
  loadBalancerAddresses: "",
  pods: "",
  services: "",
  storage: "",
  place: anyNode,
  proxmoxHA: true,
  datastoreAllocate: false,
  volumeSnapshots: false,
  haGroups: true,
  controlPlaneMachine: machineDefaults(undefined),
  workerMachine: machineDefaults(undefined),
  cni: "",
  headlamp: false,
  headlampHostname: "",
  headlampView: false,
  monitoring: false,
  grafanaHostname: "",
  logs: newLogs(),
  podSecurity: podSecurityOf(undefined),
  cis: false,
  istio: false,
  clusterMesh: false,
  meshPeers: [],
  rancherInstall: false,
  rancherHostname: "",
  rancherRegister: true,
  certificates: newCertificates(),
  dns01Token: "",
  gitops: newGitOps(),
  oidc: [],
  upgradeMode: "off",
  backups: false,
  backupFields: newBackupFields(),
  backupOwnKey: true,
  backupKey: {},
});

/** namespaceOf is the namespace the cluster goes to: the one chosen, else the interface's first. */
export const namespaceOf = (f: Form, namespaces: string[]): string => f.namespace || (namespaces[0] ?? "");

/**
 * withNamespace moves the cluster to another namespace. Cluster Mesh links clusters of one
 * namespace only, so the peers chosen in the one it was in are dropped.
 */
export function withNamespace(f: Form, namespace: string, current = f.namespace): Form {
  return { ...f, namespace, meshPeers: namespace === current ? f.meshPeers : [] };
}

/** chosenNodeOS is the node OS a form's nodes run: the one chosen, else the class's default; none for Talos. */
export function chosenNodeOS(f: Form, c: ClusterClass | undefined): NodeOSChoice | undefined {
  return findNodeOS(c?.nodeOSes, f.nodeOS) ?? defaultNodeOS(c?.nodeOSes);
}

/** templateMissing says why no cluster of the class on the form's node OS can be built yet, if none can. */
export function templateMissing(f: Form, c: ClusterClass | undefined): string | undefined {
  return (c?.nodeOSes ?? []).length > 0 ? chosenNodeOS(f, c)?.noTemplate : c?.noTemplate;
}

// classNodeOS is the node OS a form keeps on a change of class: the one chosen, while the class
// offers it, else the class's default.
function classNodeOS(f: Form, c: ClusterClass | undefined, kept: boolean): string {
  const held = findNodeOS(c?.nodeOSes, f.nodeOS);
  const os = kept && held !== undefined && choosable(held) ? held : defaultNodeOS(c?.nodeOSes);
  return os === undefined ? "" : nodeOSKey(os);
}

/** versionsNodeOS is the node OS the versions offered depend on: kubeadm's, whose templates are per version. */
export function versionsNodeOS(f: Form, c: ClusterClass | undefined): string | undefined {
  const os = c?.distribution === "kubeadm" ? chosenNodeOS(f, c) : undefined;
  return os === undefined ? undefined : nodeOSKey(os);
}

// nodeOSVariable is the nodeOS variable of a form, for a class with node OSes.
function nodeOSVariable(form: Form, c: ClusterClass): Record<string, unknown> {
  const os = chosenNodeOS(form, c);
  return os === undefined ? {} : { nodeOS: nodeOSRef(os) };
}

/**
 * withClass chooses a distribution's class, bringing its defaults, but only into what the user has
 * not set: a choice made on a later step stays, unless the new distribution does not offer it.
 */
export function withClass(f: Form, c: ClusterClass | undefined, kept: (k: keyof Form) => boolean): Form {
  const cni = variable(c, "cni");
  const cniDefault = typeof cni?.default === "string" ? cni.default : "";
  return {
    ...f,
    className: c?.name ?? "",
    nodeOS: classNodeOS(f, c, kept("nodeOS")),
    version: f.version && c && new RegExp(c.versionFormat.pattern, "u").test(f.version) ? f.version : "",
    cni: kept("cni") && (cni?.enum ?? []).includes(f.cni) ? f.cni : cniDefault,
    controlPlaneMachine: kept("controlPlaneMachine")
      ? f.controlPlaneMachine
      : machineDefaults(variable(c, "controlPlaneMachine")),
    workerMachine: kept("workerMachine") ? f.workerMachine : machineDefaults(variable(c, "workerMachine")),
    workerAutoscale: f.workerAutoscale && variable(c, "autoscaler") !== undefined,
    oidc: kept("oidc") ? f.oidc : oidcProviders(variable(c, "oidc")?.default),
  };
}

// addOnVariables are the variables of the add-ons and the hardening a form chooses.
function addOnVariables(form: Form, defined: Set<string>): Record<string, unknown> {
  const variables: Record<string, unknown> = {};
  if (form.headlamp) {
    const headlamp: Record<string, unknown> = { enabled: true };
    if (form.headlampHostname) {
      headlamp["hostname"] = form.headlampHostname;
    }
    if (form.headlampView) {
      headlamp["access"] = "view";
    }
    variables["headlamp"] = headlamp;
  }
  if (form.monitoring) {
    variables["monitoring"] = form.grafanaHostname
      ? { enabled: true, hostname: form.grafanaHostname }
      : { enabled: true };
  }
  if (logsChosen(form)) {
    variables["logs"] = logsVariable(form.logs);
  }
  const cis = form.cis && defined.has("cis");
  const podSecurity = podSecurityVariable(enforced(form.podSecurity, cis));
  if (podSecurity) {
    variables["podSecurity"] = podSecurity;
  }
  if (cis) {
    variables["cis"] = true;
  }
  if (form.istio && defined.has("istio")) {
    variables["istio"] = { enabled: true };
  }
  if (meshChosen(form)) {
    variables["clusterMesh"] = { enabled: true, peers: form.meshPeers.toSorted() };
  }
  if (form.rancherInstall) {
    variables["rancher"] = { install: true, hostname: form.rancherHostname };
  } else if (!form.rancherRegister) {
    variables["rancher"] = { register: false };
  }
  if (form.backups) {
    variables["backups"] = backupsVariable(form.backupFields);
  }
  const certificates = defined.has("certificates") ? certificatesVariable(form.certificates) : undefined;
  if (certificates) {
    variables["certificates"] = certificates;
  }
  const gitops = gitOpsVariable(form.gitops);
  if (gitops !== undefined) {
    variables["gitops"] = gitops;
  }
  return variables;
}

/**
 * A credential the form holds for the cluster, stored once the cluster exists: label names it for
 * people, section is the cluster's option that stores it later, and forget is the form without it,
 * so that the summary no longer says it is stored.
 */
export interface PendingCredential {
  purpose: string;
  values: Record<string, string>;
  label: string;
  section: string;
  forget: Partial<Form>;
}

/** pendingCredentials are the credentials to store for the created cluster that the form holds. */
export function pendingCredentials(form: Form, c: ClusterClass): PendingCredential[] {
  const pending: PendingCredential[] = [];
  if (form.backups && form.backupOwnKey && variable(c, "backups")) {
    pending.push({ purpose: "backup", values: form.backupKey, label: "S3 access key", section: "Backups", forget: {} });
  }
  if (variable(c, "certificates") && needsClusterToken(form.certificates) && filled(form.dns01Token)) {
    pending.push({
      purpose: "dns01",
      values: { token: form.dns01Token },
      label: tokenLabel(form.certificates.provider),
      section: "Certificates",
      forget: { dns01Token: "" },
    });
  }
  return pending;
}

// distributionVariables are what the form chooses of the distribution itself: its pod network and,
// on Talos, the release.
function distributionVariables(form: Form): Record<string, unknown> {
  const variables: Record<string, unknown> = {};
  if (form.cni) {
    variables["cni"] = form.cni;
  }
  if (form.talosVersion) {
    variables["talosVersion"] = form.talosVersion;
  }
  return variables;
}

// request turns the form into the API's request, with only the variables the class defines.
export function request(form: Form, c: ClusterClass): CreateRequest {
  const defined = new Set(c.variables.map((v) => v.name));
  const network: Record<string, unknown> = {
    bridge: form.bridge,
    addresses: list(form.addresses),
    prefix: form.prefix,
    gateway: form.gateway,
    dnsServers: list(form.dnsServers),
  };
  if (form.vlan) {
    network["vlan"] = Number(form.vlan);
  }
  const variables: Record<string, unknown> = {
    controlPlaneEndpoint: form.endpoint,
    network,
    storage: form.storage,
    controlPlaneMachine: form.controlPlaneMachine,
    workerMachine: form.workerMachine,
    ...nodeOSVariable(form, c),
  };
  const nodes = allowedNodes(form.place);
  if (nodes !== undefined) {
    variables["allowedNodes"] = nodes;
  }
  // Left out, the class's default has the cluster take part once HA applies to it.
  if (!form.proxmoxHA) {
    variables["proxmoxHA"] = false;
  }
  if ((form.datastoreAllocate || form.volumeSnapshots) && defined.has("csi")) {
    variables["csi"] = {
      ...(form.datastoreAllocate && { datastoreAllocate: true }),
      ...(form.volumeSnapshots && { volumeSnapshots: true }),
    };
  }
  // On is the class's default wherever its clusters may have the labels, and off where they may not.
  if (!form.haGroups && defined.has("ccm")) {
    variables["ccm"] = { haGroups: false };
  }
  Object.assign(variables, distributionVariables(form));
  const lb = loadBalancerVariable({ implementation: form.loadBalancer, addresses: list(form.loadBalancerAddresses) });
  if (lb) {
    variables["loadBalancer"] = lb;
  }
  Object.assign(variables, addOnVariables(form, defined));
  variables["oidc"] = cleanProviders(form.oidc);
  // A cluster whose nodes move to new template builds needs its window in the browser's time zone
  // even with Kubernetes upgrades off.
  const upgrades = newUpgrades(form.upgradeMode, browserZone());
  if (form.upgradeMode !== "off" || (defined.has("upgrades") && refreshesNodes(upgrades))) {
    variables["upgrades"] = upgrades;
  }
  const req: CreateRequest = {
    ...(form.namespace ? { namespace: form.namespace } : {}),
    name: form.name,
    class: c.name,
    version: form.version,
    controlPlaneReplicas: form.controlPlaneReplicas,
    workerReplicas: form.workerReplicas,
    variables: Object.fromEntries(Object.entries(variables).filter(([k]) => defined.has(k))),
  };
  if (autoscales(form, c)) {
    req.workerAutoscaling = { min: form.workerReplicas, max: form.workerMax };
  }
  if (form.pods) {
    req.pods = [form.pods];
  }
  if (form.services) {
    req.services = [form.services];
  }
  return req;
}

const gib = (mib: number): string => (mib % mibPerGiB === 0 ? `${mib / mibPerGiB} GiB` : `${mib} MiB`);
const machine = (m: Machine): string =>
  `${m.cores} ${m.cores === 1 ? "core" : "cores"}, ${gib(m.memoryMiB)} memory, ${m.diskGiB} GiB disk`;

function rancherChoice(form: Form): string {
  if (form.rancherInstall) {
    return `Installed at ${form.rancherHostname}`;
  }
  return form.rancherRegister ? registeredWithRancher : "Not registered";
}

function meshChoice(form: Form): string {
  if (!meshChosen(form)) {
    return "Off";
  }
  return form.meshPeers.length > 0 ? `On, with ${form.meshPeers.toSorted().join(", ")}` : "On, no peers";
}

// releaseRows say what the cluster runs: its distribution, the node OS or on Talos, which is its own
// OS, the Talos release, and Kubernetes.
function releaseRows(form: Form, c: ClusterClass, defined: Set<string>): [string, string][] {
  const rows: [string, string][] = [["Distribution", distributionName(c.distribution)]];
  const os = chosenNodeOS(form, c);
  if (os !== undefined) {
    rows.push(["Node OS", os.title]);
  }
  if (defined.has("talosVersion")) {
    rows.push(["Talos release", form.talosVersion]);
  }
  rows.push(["Kubernetes version", form.version]);
  return rows;
}

// summary is what the Confirm step shows: each setting by its name, as the user chose it; Proxmox
// VE HA, the CSI plugin's release and the HA group labels only where they can be chosen.
export function summary(
  form: Form,
  c: ClusterClass,
  offered: { ha?: boolean; csi?: boolean; snapshots?: boolean; haGroups?: boolean } = {},
): [string, string][] {
  const defined = new Set(c.variables.map((v) => v.name));
  const rows: [string, string][] = [
    ...(form.namespace ? [["Namespace", form.namespace] as [string, string]] : []),
    ["Name", form.name],
    ...releaseRows(form, c, defined),
    ["Control-plane nodes", String(form.controlPlaneReplicas)],
    [
      "Workers",
      autoscales(form, c) ? `${form.workerReplicas} to ${form.workerMax}, autoscaled` : String(form.workerReplicas),
    ],
    ["Bridge", form.vlan ? `${form.bridge}, VLAN ${form.vlan}` : form.bridge],
    ["API address", form.endpoint],
    ["Node addresses", list(form.addresses).join(", ")],
    ["Prefix length and gateway", `/${form.prefix}, ${form.gateway}`],
    ["DNS servers", list(form.dnsServers).join(", ")],
  ];
  if (defined.has("loadBalancer")) {
    rows.push([
      "Load balancer",
      loadBalancerText(
        { implementation: form.loadBalancer, addresses: list(form.loadBalancerAddresses) },
        c.distribution,
      ),
    ]);
  }
  rows.push(
    ["Pod range", form.pods || "Automatic"],
    ["Service range", form.services || "Automatic"],
    ["Storage", form.storage],
  );
  if (defined.has("allowedNodes")) {
    rows.push(["Placement", placeText(form.place)]);
  }
  if (offered.ha === true) {
    rows.push(["Proxmox VE HA", haText(form.proxmoxHA)]);
  }
  if (offered.csi === true) {
    rows.push(["CSI plugin", csiText(form.datastoreAllocate)]);
  }
  if (offered.snapshots === true) {
    rows.push(["Volume snapshots", snapshotsText(form.volumeSnapshots)]);
  }
  if (offered.haGroups === true) {
    rows.push(["HA group labels", haGroupsText(form.haGroups)]);
  }
  rows.push(["Control-plane machines", machine(form.controlPlaneMachine)]);
  if (mostWorkers(form) > 0) {
    rows.push(["Worker machines", machine(form.workerMachine)]);
  }
  if (defined.has("cni")) {
    rows.push(["Pod network", cniName(form.cni) || "Distribution's own"]);
  }
  rows.push(...addOnRows(form, defined));
  if (defined.has("upgrades")) {
    const upgrades = newUpgrades(form.upgradeMode, browserZone());
    rows.push(["Automatic upgrades", describe(upgrades, refreshesNodes(upgrades), c.distribution)]);
  }
  if (defined.has("oidc")) {
    const providers = cleanProviders(form.oidc);
    rows.push(["Identity providers", providers.length > 0 ? providers.map((p) => p.issuerURL).join(", ") : "None"]);
  }
  return rows;
}

// addOnRows sums up the add-ons and the hardening the class offers.
function addOnRows(form: Form, defined: Set<string>): [string, string][] {
  const rows: [string, string][] = [];
  if (defined.has("headlamp")) {
    rows.push(["Headlamp", headlampText(form.headlamp, form.headlampHostname, form.headlampView)]);
  }
  if (defined.has("monitoring")) {
    rows.push([
      "Monitoring",
      form.monitoring ? `On${form.grafanaHostname ? `, Grafana at ${form.grafanaHostname}` : ""}` : "Off",
    ]);
  }
  if (defined.has("logs")) {
    rows.push(["Logs", logsText({ ...form.logs, enabled: logsChosen(form) })]);
  }
  const cis = form.cis && defined.has("cis");
  if (defined.has("cis")) {
    rows.push(["CIS hardening", cisText(cis)]);
  }
  if (defined.has("podSecurity")) {
    rows.push(["Pod security", podSecurityText(enforced(form.podSecurity, cis))]);
  }
  if (defined.has("istio")) {
    rows.push(["Istio", istioText(form.istio)]);
  }
  if (defined.has("clusterMesh") && form.cni === "cilium") {
    rows.push(["Cluster Mesh", meshChoice(form)]);
  }
  if (defined.has("rancher")) {
    rows.push(["Rancher", rancherChoice(form)]);
  }
  if (defined.has("backups")) {
    const key = form.backupOwnKey ? "own access key" : "shared access key";
    rows.push(["Backups", form.backups ? `${backupsText(backupsVariable(form.backupFields))}, ${key}` : "Off"]);
  }
  if (defined.has("certificates")) {
    rows.push(["Certificates", certificatesText(form.certificates)]);
    if (needsClusterToken(form.certificates)) {
      rows.push([
        tokenLabel(form.certificates.provider),
        filled(form.dns01Token) ? "Stored once created" : "None yet: the cluster waits",
      ]);
    }
  }
  if (defined.has("gitops")) {
    rows.push(["GitOps", gitOpsText(form.gitops)]);
  }
  return rows;
}

// rancherMinimumCores is what Rancher's documentation asks of every node of its cluster; the
// controller refuses fewer.
export const rancherMinimumCores = 4;

// FieldKey names a field of the wizard a problem belongs to.
export type FieldKey =
  | "name"
  | "class"
  | "nodeOS"
  | "version"
  | "talosVersion"
  | "controlPlaneReplicas"
  | "workerReplicas"
  | "workerMax"
  | "addons"
  | "bridge"
  | "vlan"
  | "endpoint"
  | "addresses"
  | "prefix"
  | "gateway"
  | "dnsServers"
  | "loadBalancerAddresses"
  | "pods"
  | "services"
  | "storage"
  | "datastoreAllocate"
  | "volumeSnapshots"
  | "placement"
  | "controlPlaneMachine"
  | "workerMachine"
  | "cni"
  | "rancherHostname"
  | "headlampHostname"
  | "logs"
  | "backupTarget"
  | "backupSchedule"
  | "backupRetention"
  | "backupKey"
  | "certificates"
  | `gitops.${GitOpsField}`
  | "oidc";

type Problems = Partial<Record<FieldKey, string>>;

const maxName = 40;

// nameProblem names the first rule a cluster name breaks, so the message says what to change.
function nameProblem(name: string): string | undefined {
  if (name === "") {
    return "Enter a name";
  }
  if (name.length > maxName) {
    return `Name must be ${maxName} characters or fewer`;
  }
  if (!/^[-a-z0-9]+$/u.test(name)) {
    return "Use only lowercase letters, digits and dashes";
  }
  if (!/^[a-z]/u.test(name)) {
    return "Start the name with a letter";
  }
  return name.endsWith("-") ? "End the name with a letter or digit" : undefined;
}

function generalProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  const name = nameProblem(form.name);
  if (name !== undefined) {
    p.name = name;
  }
  const os = chosenNodeOS(form, c);
  const missing = templateMissing(form, c);
  if (!c) {
    p.class = "Choose a distribution";
  } else if (os !== undefined && !choosable(os)) {
    p.nodeOS = `${os.title} is not released yet`;
  } else if (filled(missing)) {
    p[os === undefined ? "class" : "nodeOS"] = sentence(missing);
  } else {
    if (variable(c, "talosVersion") !== undefined && !talosRelease.test(form.talosVersion)) {
      p.talosVersion = "Choose a Talos release with a VM template";
    }
    if (!new RegExp(c.versionFormat.pattern, "u").test(form.version)) {
      p.version = `Enter a version like ${versionExamples[c.distribution] ?? "v1.36.5"}`;
    }
  }
  if (!whole(form.workerReplicas, 0, maxWorkers)) {
    p.workerReplicas = `A whole number from 0 to ${maxWorkers}`;
  }
  if (autoscales(form, c)) {
    const least = Number.isNaN(form.workerReplicas) ? 0 : Math.max(form.workerReplicas, 1);
    if (!whole(form.workerMax, least, maxWorkers)) {
      p.workerMax = `A whole number from ${least} to ${maxWorkers}`;
    }
  }
  return p;
}

/** autoscales says whether a form's worker pool is autoscaled: chosen, and offered by its class. */
export const autoscales = (form: Form, c: ClusterClass | undefined): boolean =>
  form.workerAutoscale && variable(c, "autoscaler") !== undefined;

/**
 * addonsProblem is what keeps a form's add-ons from running: an autoscaled pool that may go down to
 * no workers, while the control planes stay kept for the control plane.
 */
export function addonsProblem(form: Form, c: ClusterClass | undefined): string | undefined {
  // Rancher's agent alone holds no worker up: it runs only once there is a Rancher to register with,
  // and its pod waiting for room brings a worker up from none.
  const agent =
    form.rancherInstall || !form.rancherRegister
      ? 0
      : (quantity(classFigures(c, "kubehenge.dev/addon-memory")["rancherAgent"]) ?? 0);
  return autoscales(form, c) && form.workerReplicas === 0 && addonMemory(form, c).mib - agent > 0
    ? "The add-ons need a worker. On General, set the fewest workers to 1 or more."
    : undefined;
}

function addressProblems(form: Form): Problems {
  const p: Problems = {};
  const entries = list(form.addresses);
  const { count, bad } = addressCount(entries, form.gateway, form.prefix);
  const needed = nodesNeeded(form);
  if (entries.length === 0) {
    p.addresses = "Enter at least one address, range or network";
  } else if (bad !== undefined) {
    p.addresses = `${bad} is not an address, range or network on the gateway's network`;
  } else if (count < needed) {
    p.addresses = `Enter at least ${needed} addresses, one per node plus one spare`;
  }
  if (!ipv4.test(form.endpoint)) {
    p.endpoint = "Enter an IPv4 address";
  } else if (inList(form.endpoint, entries)) {
    p.endpoint = "Use an address outside the node addresses";
  } else if (addressCount([form.endpoint], form.gateway, form.prefix).bad !== undefined) {
    p.endpoint = "Use an address on the gateway's network";
  }
  return p;
}

function networkProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (!form.bridge) {
    p.bridge = "Choose a bridge";
  }
  if (form.vlan && !isVLAN(form.vlan)) {
    p.vlan = "A whole number from 1 to 4094";
  }
  if (!whole(form.prefix, 1, addressBits)) {
    p.prefix = `A whole number from 1 to ${addressBits}`;
  }
  if (!ipv4.test(form.gateway)) {
    p.gateway = "Enter an IPv4 address";
  }
  Object.assign(p, addressProblems(form));
  if (list(form.dnsServers).length === 0) {
    p.dnsServers = "Enter at least one DNS server";
  } else if (list(form.dnsServers).some((d) => !ipv4.test(d))) {
    p.dnsServers = "DNS servers must be IPv4 addresses";
  }
  const lb =
    form.loadBalancer === ""
      ? undefined
      : addressesProblem(list(form.loadBalancerAddresses), {
          gateway: form.gateway,
          prefix: form.prefix,
          addresses: list(form.addresses),
          endpoint: form.endpoint,
        });
  if (lb !== undefined) {
    p.loadBalancerAddresses = lb;
  }
  for (const [key, v] of [
    ["pods", form.pods],
    ["services", form.services],
  ] as const) {
    const problem = v ? rangeProblem(v) : undefined;
    if (problem !== undefined) {
      p[key] = problem;
    }
  }
  if (c?.variables.find((v) => v.name === "cni")?.required === true && !form.cni) {
    p.cni = "Choose a pod network";
  } else if (form.cni === "cilium" && form.name.length > maxCiliumName) {
    p.cni = `Cilium needs a cluster name of ${maxCiliumName} characters or fewer`;
  }
  return p;
}

function machineProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (!form.storage) {
    p.storage = "Choose a storage";
  }
  const placement = variable(c, "allowedNodes") ? placeProblem(form.place) : undefined;
  if (placement !== undefined) {
    p.placement = placement;
  }
  for (const [key, m] of [
    ["controlPlaneMachine", form.controlPlaneMachine],
    ["workerMachine", form.workerMachine],
  ] as const) {
    const min = machineLeast(form, c, key);
    const plain = plainLeast(form, c, key);
    // The reason a minimum is higher than usual: the add-ons, or control planes that run the
    // workloads without workers. MachineFields places the message by its cores, memory or disk.
    const why = (raised: boolean): string => {
      if (raised) {
        return " for the add-ons chosen";
      }
      return key === "controlPlaneMachine" && mostWorkers(form) === 0 ? " without workers" : "";
    };
    if (!whole(m.cores, min.cores)) {
      p[key] = `A whole number of cores, at least ${min.cores}${why(min.cores > plain.cores)}`;
    } else if (
      form.rancherInstall &&
      m.cores < rancherMinimumCores &&
      (key === "controlPlaneMachine" || form.workerReplicas > 0)
    ) {
      p[key] = `Rancher needs at least ${rancherMinimumCores} cores on every node`;
    } else if (!whole(m.memoryMiB, min.memoryMiB) || m.memoryMiB % memoryStepMiB !== 0) {
      p[key] =
        `At least ${min.memoryMiB} MiB of memory${why(min.memoryMiB > plain.memoryMiB)}, in steps of ${memoryStepMiB}`;
    } else if (!whole(m.diskGiB, min.diskGiB)) {
      p[key] = `A whole number of GiB of disk, at least ${min.diskGiB}`;
    }
  }
  return p;
}

// The certificates' fields show their own problems; the step holds them, to keep it from being
// complete.
function addOnProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (form.rancherInstall && !form.rancherHostname) {
    p.rancherHostname = "Enter a host name";
  }
  const logs = logsChosen(form) ? logsProblem(form.logs) : undefined;
  if (logs !== undefined) {
    p.logs = logs;
  }
  const addons = addonsProblem(form, c);
  if (addons !== undefined) {
    p.addons = addons;
  }
  const certificates = variable(c, "certificates") ? Object.values(certificatesProblems(form.certificates)) : [];
  if (certificates.length > 0) {
    p.certificates = certificates.join(" ");
  }
  if (variable(c, "gitops") !== undefined) {
    const g = gitOpsProblems(form.gitops);
    for (const field of gitOpsFields) {
      const problem = g[field];
      if (problem !== undefined) {
        p[`gitops.${field}`] = problem;
      }
    }
  }
  return { ...p, ...backupsProblems(form, c) };
}

// backupsProblems are what keeps the backups the form switches on from being complete.
function backupsProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (!form.backups || variable(c, "backups") === undefined) {
    return p;
  }
  const b = backupProblems(form.backupFields, classTargets(c));
  for (const [key, found] of [
    ["backupTarget", b.target],
    ["backupSchedule", b.schedule],
    ["backupRetention", b.retention],
  ] as const) {
    if (found !== undefined) {
      p[key] = found;
    }
  }
  if (form.backupOwnKey && !backupKeys.every((k) => filled(form.backupKey[k.key]))) {
    p.backupKey = "Enter both parts of the access key";
  }
  return p;
}

// fieldProblems says, by field, what keeps a step from being complete.
export function fieldProblems(step: Step, form: Form, c: ClusterClass | undefined): Problems {
  switch (step) {
    case "General": {
      return generalProblems(form, c);
    }
    case "Network": {
      return networkProblems(form, c);
    }
    case "Machines": {
      return machineProblems(form, c);
    }
    case "Add-ons": {
      return addOnProblems(form, c);
    }
    case "Identity providers": {
      const o = oidcProblems(form.oidc);
      return o.length > 0 ? { oidc: o.join(" ") } : {};
    }
    case "Confirm": {
      break;
    }
  }
  return {};
}

// problems lists what keeps a step from being complete.
export function problems(step: Step, form: Form, c: ClusterClass | undefined): string[] {
  return Object.values(fieldProblems(step, form, c));
}

const minorOf = (v: string): number => Number(/^v1\.(?<minor>\d+)\./u.exec(v)?.groups?.["minor"] ?? 0);

// versionLabel marks the stable release, and those of minors newer than it.
export function versionLabel(v: string, stable: string | undefined): string {
  if (!filled(stable)) {
    return v;
  }
  if (v === stable) {
    return `${v} (stable)`;
  }
  return minorOf(v) > minorOf(stable) ? `${v} (newer than stable)` : v;
}

// offeredBridges are the bridges a cluster may attach to: the class's list when it has one, with
// what the nodes say of each, else every bridge a node lists. A bridge the user cannot list is
// still offered; the server checks SDN.Use on it.
export function offeredBridges(byNode: Record<string, Bridge[]>, allowed: string[] | undefined): Bridge[] {
  const seen = new Map<string, Bridge>();
  for (const b of Object.values(byNode).flat()) {
    const known = seen.get(b.bridge);
    if (!known || (!known.usable && b.usable)) {
      seen.set(b.bridge, b);
    }
  }
  if (!allowed) {
    return [...seen.values()];
  }
  return allowed.map((name) => seen.get(name) ?? { bridge: name, node: "", vlanAware: false, usable: true });
}

/** datastoreAllocateAnnotation marks the classes whose clusters may choose the CSI plugin's newest release. */
export const datastoreAllocateAnnotation = "kubehenge.dev/datastore-allocate";
/** The storages, comma-separated, of the management cluster and of the VM templates. */
export const managementStoragesAnnotation = "kubehenge.dev/management-storages";
export const templateStoragesAnnotation = "kubehenge.dev/template-storages";

const listed = (c: ClusterClass, annotation: string): string[] =>
  (c.annotations?.[annotation] ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");

/** csiOffered tells whether an administrator lets the class's clusters choose the plugin's newest release. */
export const csiOffered = (c: ClusterClass | undefined): boolean =>
  c?.annotations?.[datastoreAllocateAnnotation] === "true" && c.variables.some((v) => v.name === "csi");

/** What a cluster's Datastore.Allocate on its storage is checked against. */
export interface DatastoreAllocateCheck {
  cls: ClusterClass;
  storages: Storage[];
  storage: string | undefined;
  /** clusters are those the user sees. */
  clusters: Cluster[];
  /** self is the cluster itself, once there is one. */
  self?: { namespace: string; name: string } | undefined;
}

/** datastoreAllocateProblem says why the cluster may not hold Datastore.Allocate on its storage, or gives undefined. */
export function datastoreAllocateProblem({
  cls,
  storages,
  storage,
  clusters,
  self,
}: DatastoreAllocateCheck): string | undefined {
  if (storage === undefined || storage === "") {
    return "Choose a storage first";
  }
  if (listed(cls, managementStoragesAnnotation).includes(storage)) {
    return `Unavailable on ${storage}, which the management cluster uses`;
  }
  if (listed(cls, templateStoragesAnnotation).includes(storage)) {
    return `Unavailable on ${storage}, which holds the VM templates`;
  }
  if (!storages.some((s) => s.storage === storage && s.allocatable)) {
    return `You do not hold Datastore.Allocate on ${storage}`;
  }
  const other = clusters.find(
    (o) => text(o.variables["storage"]) === storage && !(o.namespace === self?.namespace && o.name === self.name),
  );
  if (other !== undefined) {
    return `Unavailable on ${storage}, which cluster ${other.namespace}/${other.name} uses too`;
  }
  return undefined;
}

/** CSIOffer is whether a cluster may choose the plugin's newest release, and what stands in the way. */
export interface CSIOffer {
  offered: boolean;
  problem: string | undefined;
}

/** csiOffer is what the class and the storage make of a cluster's choice of the plugin's newest release. */
export function csiOffer(check: Omit<DatastoreAllocateCheck, "cls"> & { cls: ClusterClass | undefined }): CSIOffer {
  const { cls } = check;
  return { offered: csiOffered(cls), problem: cls && datastoreAllocateProblem({ ...check, cls }) };
}

/** The storages, comma-separated, the CSI plugin takes volume snapshots on. */
export const snapshotStoragesAnnotation = "kubehenge.dev/snapshot-storages";

/** SnapshotsOffer is whether a cluster may take volume snapshots, and what stands in the way. */
export interface SnapshotsOffer {
  offered: boolean;
  problem: string | undefined;
}

/**
 * snapshotsOffer is what the class and the storage make of a cluster's choice of volume snapshots.
 * It takes no privilege of the user's own: Kubehenge copies only the cluster's own volumes, onto the
 * storage the cluster already uses.
 */
export function snapshotsOffer(cls: ClusterClass | undefined, storage: string | undefined): SnapshotsOffer {
  const offered = cls?.variables.some((v) => v.name === "csi") === true;
  if (!cls || !offered) {
    return { offered, problem: undefined };
  }
  if (storage === undefined || storage === "") {
    return { offered, problem: "Choose a storage first" };
  }
  if (!listed(cls, snapshotStoragesAnnotation).includes(storage)) {
    return {
      offered,
      problem: `Unavailable on ${storage}. Snapshots need ZFS, LVM, LVM-thin, directory or btrfs storage.`,
    };
  }
  return { offered, problem: undefined };
}

/** snapshotsProblems holds the machines step to the volume snapshots a new cluster chose. */
export function snapshotsProblems(step: Step, form: Form, offer: SnapshotsOffer): Problems {
  if (step !== "Machines" || !offer.offered || !form.volumeSnapshots || offer.problem === undefined) {
    return {};
  }
  return { volumeSnapshots: offer.problem };
}

/** csiProblems holds the machines step to the CSI plugin's release a new cluster chose. */
export function csiProblems(step: Step, form: Form, csi: CSIOffer): Problems {
  if (step !== "Machines" || !csi.offered || !form.datastoreAllocate || csi.problem === undefined) {
    return {};
  }
  return { datastoreAllocate: csi.problem };
}

// offeredStorages are the storages, once each, that take disks, the user may use and the class
// allows.
export function offeredStorages(storages: Storage[], allowed: string[] | undefined): Storage[] {
  const seen = new Map<string, Storage>();
  for (const s of storages) {
    if (s.usable && !seen.has(s.storage)) {
      seen.set(s.storage, s);
    }
  }
  return [...seen.values()].filter((s) => !allowed || allowed.includes(s.storage));
}

// The class lists the bridges and storages clusters may use; without such a list, what any node
// offers.
export const classBridges = (c: ClusterClass | undefined): string[] | undefined =>
  texts(variable(c, "network")?.properties?.["bridge"]?.enum);
export const classStorages = (c: ClusterClass | undefined): string[] | undefined => texts(variable(c, "storage")?.enum);
