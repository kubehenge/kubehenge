import {
  alertingText,
  alertingVariable,
  newAlerting,
  pendingAlerting,
  type AlertingForm,
  type AlertingSecrets,
} from "./Alerting";
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
  needsBindingKey,
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
  egressFields,
  egressProblems,
  egressText,
  egressVariable,
  newEgress,
  type EgressChoice,
  type EgressField,
} from "./EgressProxy";
import { defaultEtcdQuotaGiB, etcdText, maxEtcdQuotaGiB, minEtcdQuotaGiB } from "./EtcdSize";
import {
  etcdFieldsOf,
  etcdProblems,
  etcdSnapshotsText,
  etcdSnapshotsVariable,
  uploadsItself,
  withEtcdTarget,
  type EtcdSnapshotFields,
  type EtcdSnapshotsValue,
} from "./EtcdSnapshots";
import { securityGroupsProblem } from "./Firewall";
import {
  gitOpsFields,
  gitOpsProblems,
  gitOpsText,
  gitOpsVariable,
  newGitOps,
  type GitOpsChoice,
  type GitOpsField,
} from "./GitOps";
import { defaultGrace, graceProblem, graceText, graceVariable } from "./HostOutage";
import {
  loadBalancerFrom,
  loadBalancerProblems,
  loadBalancerText,
  loadBalancerVariable,
  bgpOffered,
  type Implementation,
  type LoadBalancer,
  type LoadBalancerValue,
  type Mode,
} from "./LoadBalancer";
import { logsProblem, logsText, logsVariable, newLogs, type Logs } from "./Logs";
import { labelsText, parseLabels } from "./namespace-labels";
import {
  addressCount,
  bitsOf,
  choiceOf,
  familiesOf,
  familyFieldLabel,
  familyLabels,
  familyNames,
  familyOf,
  hasFamily,
  inList,
  isMTU,
  isQueues,
  isVLAN,
  list,
  maxMTU,
  maxQueues,
  minMTU,
  networkOf,
  parseCidr,
  rangeProblem,
  searchDomainsProblem,
  timeServersProblem,
  whole,
  type Family,
  type FamilyChoice,
} from "./network";
import {
  noNodeCustomisation,
  nodeCustomisationProblem,
  nodeCustomisationText,
  nodeCustomisationVariable,
  type NodeCustomisation,
} from "./node-customisation";
import { choosable, defaultNodeOS, findNodeOS, fipsCNI, nodeOSKey, nodeOSRef } from "./node-os";
import {
  hasNodeSettings,
  noNodeSettings,
  nodeSettingsProblem,
  nodeSettingsText,
  nodesVariable,
  type NodeSettings,
} from "./node-settings";
import {
  certificateProblem,
  mirrorsProblem,
  mirrorsText,
  mirrorsVariable,
  trustedText,
  trustedVariable,
  type MirrorRow,
} from "./NodeSite";
import { noTimeouts, timeoutProblems, timeoutsOf, timeoutsSet, type TimeoutEntry } from "./NodeTimeouts";
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
  NodeDNS,
  Cluster,
  ClusterClass,
  CreateRequest,
  FreeAddresses,
  NodeOSChoice,
  OIDCProvider,
  PveNode,
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

export { nodeCustomisationOf } from "./node-customisation";

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
  /** The cluster the form started from, as namespace/name. Empty for none. */
  from: string;
  /** Empty while the interface creates clusters in one namespace only. */
  namespace: string;
  name: string;
  className: string;
  /** The node OS as one word, such as almalinux-10. Empty while the class offers none. */
  nodeOS: string;
  version: string;
  /** The Talos release a Talos cluster's nodes run. Empty takes the newest with a template. */
  talosVersion: string;
  controlPlaneReplicas: number;
  /** The workers, or with workerAutoscale the fewest the cluster autoscaler keeps. */
  workerReplicas: number;
  workerAutoscale: boolean;
  /** The most workers the cluster autoscaler adds, with workerAutoscale. */
  workerMax: number;
  /** How long a removed machine waits for its node, for the control planes and for the workers. Empty takes the class's. */
  controlPlaneTimeouts: TimeoutEntry;
  workerTimeouts: TimeoutEntry;
  endpoint: string;
  bridge: string;
  vlan: string;
  /** The nodes' interface MTU, empty to leave Proxmox VE's default. */
  mtu: string;
  /** The nodes' interface queues, empty for one. */
  queues: string;
  /** Proxmox VE security groups of the nodes' firewall, comma-separated, empty to leave the firewall off. */
  securityGroups: string;
  /** The address families of the nodes, the primary first. Fixed once the cluster exists. */
  families: FamilyChoice;
  /** The IPv4 network. Left out of the cluster with IPv6 alone. */
  addresses: string;
  prefix: number;
  gateway: string;
  /** The IPv6 network, with a family choice that has IPv6. */
  addresses6: string;
  prefix6: number;
  gateway6: string;
  dnsServers: string;
  searchDomains: string;
  /** NTP servers for the nodes. Empty keeps the node image's own. */
  ntpServers: string;
  /** Where the nodes pull images from, per registry. */
  registryMirrors: MirrorRow[];
  /** PEM authorities the nodes trust besides their OS's own. */
  trustedCAs: string;
  loadBalancer: Implementation;
  loadBalancerAddresses: string;
  loadBalancerMode: Mode;
  loadBalancerAsn: string;
  loadBalancerPeers: string;
  loadBalancerRouterID: string;
  loadBalancerAuthenticated: boolean;
  /** Typed here and stored as the cluster's bgp credential once it exists. Never copied from another cluster. */
  loadBalancerPassword: string;
  pods: string;
  services: string;
  storage: string;
  place: Place;
  /** Minutes machines of an away host wait before replacement, as text. */
  hostOutageGrace: string;
  proxmoxHA: boolean;
  datastoreAllocate: boolean;
  volumeSnapshots: boolean;
  haGroups: boolean;
  controlPlaneMachine: Machine;
  workerMachine: Machine;
  /** The labels and taints every worker gets, for a class that has the workerNodes variable. */
  workerNodes: NodeSettings;
  /** The files, commands, flags and audit settings of every node, for a class that has the variable. */
  nodeCustomisation: NodeCustomisation;
  cni: string;
  headlamp: boolean;
  headlampHostname: string;
  /** Headlamp serves everyone through its own account with Kubernetes' view role. */
  headlampView: boolean;
  monitoring: boolean;
  grafanaHostname: string;
  alerting: AlertingForm;
  // The secrets of the alerting settings, stored for the cluster once it is created. Never in the request.
  alertingSecrets: AlertingSecrets;
  logs: Logs;
  podSecurity: PodSecurity;
  cis: boolean;
  /** The most an etcd database may grow to, in GiB. NaN while the field is empty. */
  etcdQuotaGiB: number;
  istio: boolean;
  clusterMesh: boolean;
  meshPeers: string[];
  rancherInstall: boolean;
  rancherHostname: string;
  rancherRegister: boolean;
  certificates: CertificatesForm;
  /** The labels of every namespace Kubehenge makes, one key=value per line. */
  namespaceLabels: string;
  /** Whether the cluster's cert-manager is one it already holds. It is fixed once the cluster exists. */
  existingCertManager: boolean;
  // The DNS provider's credential, stored for the cluster once it is created. Never in the request.
  dns01Token: string;
  eabKey: string;
  gitops: GitOpsChoice;
  egress: EgressChoice;
  oidc: OIDCProvider[];
  upgradeMode: UpgradeMode;
  backups: boolean;
  backupFields: BackupFields;
  /** Whether the cluster gets an access key of its own, stored once it is created. */
  // storeBackupKey is whether the form stores the cluster's own access key for its backups.
  storeBackupKey: boolean;
  // backupKey is the one access key that backups and etcd snapshots are both written with.
  backupKey: Record<string, string>;
  etcdSnapshots: boolean;
  etcdFields: EtcdSnapshotFields;
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
  const cni = quantity(classFigures(c, "kubehenge.dev/cni-control-plane-memory")[form.cni]) ?? 0;
  return { names, mib: total, controlPlaneMiB: growth + cni };
}

const bytesPerMiB = 1_048_576;
const bytesPerGiB = 1_073_741_824;

/**
 * Room is what a new cluster's VMs take of memory and disk at their most, in bytes, and what the
 * nodes it may use and its storage have free. free is missing where Proxmox VE does not say.
 */
export interface Room {
  memory: number;
  freeMemory?: number | undefined;
  /** Set where no node the cluster may use has room for its largest machine. */
  nodeWarning?: string | undefined;
  disk: number;
  freeDisk?: number | undefined;
}

/** createRoom weighs a form's VMs against the online nodes it may use and the storage it chose. */
export function createRoom(form: Form, nodes: PveNode[], storages: Storage[]): Room {
  const workers = mostWorkers(form);
  const cp = form.controlPlaneReplicas;
  const allowed = allowedNodes(form.place);
  const usable = nodes.filter((n) => n.online && (allowed === undefined || allowed.includes(n.node)));
  const known = usable.length > 0 && usable.every((n) => n.maxMemory !== undefined);
  const chosen = storages.filter((s) => s.storage === form.storage && s.total > 0);
  // A shared storage is listed once per node. It holds the same space.
  const disks = chosen.some((s) => s.shared)
    ? chosen.slice(0, 1)
    : chosen.filter((s) => usable.some((n) => n.node === s.node));
  return {
    memory: (cp * form.controlPlaneMachine.memoryMiB + workers * form.workerMachine.memoryMiB) * bytesPerMiB,
    freeMemory: known
      ? usable.reduce((sum, n) => sum + Math.max(0, (n.maxMemory ?? 0) - (n.memory ?? 0)), 0)
      : undefined,
    nodeWarning: memoryNodeWarning(form, nodes),
    disk: (cp * form.controlPlaneMachine.diskGiB + workers * form.workerMachine.diskGiB) * bytesPerGiB,
    freeDisk: disks.length > 0 ? disks.reduce((sum, s) => sum + s.avail, 0) : undefined,
  };
}

/** AddonKey names an add-on in a class's figures. */
export type AddonKey = (typeof addonNames)[number][0];

/** addonCostMiB is the memory, in MiB, a class's figures give the pods of the add-ons named. */
export function addonCostMiB(c: ClusterClass | undefined, keys: AddonKey[]): number {
  const figures = classFigures(c, "kubehenge.dev/addon-memory");
  return keys.reduce((sum, key) => sum + (quantity(figures[key]) ?? 0), 0);
}

/**
 * addonCPU is the CPU, in millicores, the pods of the add-ons a form chooses request by its class's
 * figures, and which add-ons ask for any, counted as addonMemory counts them.
 */
export function addonCPU(form: Form, c: ClusterClass | undefined): { names: string[]; millicores: number } {
  const { names, total } = addonTotal(form, c, { pods: "kubehenge.dev/addon-cpu" });
  return { names, millicores: total };
}

/**
 * addonMemoryRequests is the memory, in MiB, the pods of the add-ons a form chooses request by its
 * class's figures, and which add-ons ask for any, counted as addonMemory counts them.
 */
export function addonMemoryRequests(form: Form, c: ClusterClass | undefined): { names: string[]; mib: number } {
  const { names, total } = addonTotal(form, c, { pods: "kubehenge.dev/addon-memory-requests" });
  return { names, mib: total };
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
    certManager:
      (form.monitoring || form.rancherInstall || form.certificates.enabled) && !usesExistingCertManager(form, c),
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
 * the add-ons' and of what the workers run once per cluster. Without add-ons that request CPU it is 0.
 */
export function addonNeedCores(form: Form, c: ClusterClass | undefined): number {
  const key = form.workerReplicas > 0 ? "workerMachine" : "controlPlaneMachine";
  const nodes = key === "workerMachine" ? form.workerReplicas : form.controlPlaneReplicas;
  const { millicores } = addonCPU(form, c);
  if (millicores === 0 || nodes <= 0) {
    return 0;
  }
  const figures = classFigures(c, "kubehenge.dev/node-cpu");
  const own =
    quantity(figures[key === "workerMachine" ? "worker" : "controlPlane"]) ??
    plainLeast(form, c, key).cores * millicoresPerCore;
  const once = key === "workerMachine" ? (quantity(figures["workers"]) ?? 0) : 0;
  return Math.ceil((own + Math.ceil((millicores + once) / nodes)) / millicoresPerCore);
}

/**
 * addonNeedRequestedMiB is the memory each node that carries a form's add-ons needs for what their
 * pods request, as the controller asks: what such a node keeps from requests itself, by its class,
 * and its share of the add-ons' requests and of what the workers run once per cluster. Without add-ons
 * that request memory, or without the class's figures, it is 0.
 */
export function addonNeedRequestedMiB(form: Form, c: ClusterClass | undefined): number {
  const key = form.workerReplicas > 0 ? "workerMachine" : "controlPlaneMachine";
  const nodes = key === "workerMachine" ? form.workerReplicas : form.controlPlaneReplicas;
  const { mib } = addonMemoryRequests(form, c);
  if (mib === 0 || nodes <= 0) {
    return 0;
  }
  const figures = classFigures(c, "kubehenge.dev/node-memory-requests");
  const own =
    quantity(figures[key === "workerMachine" ? "worker" : "controlPlane"]) ?? plainLeast(form, c, key).memoryMiB;
  const once = key === "workerMachine" ? (quantity(figures["workers"]) ?? 0) : 0;
  return own + Math.ceil((mib + once) / nodes / memoryStepMiB) * memoryStepMiB;
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
      memoryMiB: Math.max(min.memoryMiB, addonNeedMiB(form, c), addonNeedRequestedMiB(form, c)),
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

/** backupsChosen says whether the form's backups are on and its class has them. */
export const backupsChosen = (f: Form, c: ClusterClass | undefined): boolean =>
  f.backups && variable(c, "backups") !== undefined;

/** etcdOffered says whether a class has etcd snapshots and a backup target to take them to. */
export const etcdOffered = (c: ClusterClass | undefined): boolean =>
  variable(c, "etcdSnapshots") !== undefined && classTargets(c).length > 0;

/** etcdChosen says whether the form's etcd snapshots are on and its class offers them. */
export const etcdChosen = (f: Form, c: ClusterClass | undefined): boolean => f.etcdSnapshots && etcdOffered(c);

/** keyNeeded says whether the cluster gets the access key that its backups or etcd snapshots are written with. */
export const keyNeeded = (f: Form, c: ClusterClass | undefined): boolean => backupsChosen(f, c) || etcdChosen(f, c);

/**
 * withBackupChoices takes the one backup target a class offers until another is chosen, the backups'
 * target for etcd snapshots when it is one of them, and a key of the cluster's own only for those who
 * may store one.
 */
export function withBackupChoices(f: Form, c: ClusterClass | undefined, canTrust: boolean | undefined): Form {
  const targets = classTargets(c);
  const backupFields = withTarget(f.backupFields, targets);
  return {
    ...f,
    backupFields,
    etcdFields: withEtcdTarget(f.etcdFields, targets, f.backups ? backupFields.target : undefined),
    storeBackupKey: f.storeBackupKey && canTrust !== false,
  };
}

// etcdVariable is the form's etcdSnapshots variable. A distribution without an upload of its own
// gets none, as when the class changed after it was ticked.
const etcdVariable = (f: Form, c: ClusterClass): EtcdSnapshotsValue =>
  etcdSnapshotsVariable({ ...f.etcdFields, upload: f.etcdFields.upload && uploadsItself(c.distribution) });

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
  cidr6?: string | undefined;
  gateway6?: string | undefined;
  dns: string[];
  search: string;
}

/**
 * bridgeNetwork is the network on a bridge, from a node that reaches its gateway through it, that
 * network written like 192.0.2.0/24, and the node.
 */
export function bridgeNetwork(
  bridge: string,
  bridgesByNode: Record<string, Bridge[]>,
  dnsByNode: Record<string, NodeDNS>,
): { net?: BridgeNetwork; network?: string; network6?: string; node?: string } {
  const on = Object.values(bridgesByNode)
    .flat()
    .find((b) => b.bridge === bridge && (filled(b.gateway) || filled(b.gateway6)));
  if (on === undefined) {
    return {};
  }
  const dns = dnsByNode[on.node];
  const net = {
    cidr: on.cidr,
    gateway: on.gateway,
    cidr6: on.cidr6,
    gateway6: on.gateway6,
    dns: dns?.servers ?? [],
    search: dns?.search ?? "",
  };
  const network = on.cidr === undefined ? undefined : networkOf(on.cidr);
  const network6 = on.cidr6 === undefined ? undefined : networkOf(on.cidr6);
  return {
    net,
    node: on.node,
    ...(network === undefined ? {} : { network }),
    ...(network6 === undefined ? {} : { network6 }),
  };
}

/** addressesOf are a form's node addresses and API address, as entries to probe. */
export const addressesOf = (f: Form): string[] =>
  [
    ...(hasFamily(f.families, "IPv4") ? list(f.addresses) : []),
    ...(hasFamily(f.families, "IPv6") ? list(f.addresses6) : []),
    f.endpoint,
  ].filter((a) => filled(a));

type NetworkFields = Pick<
  Form,
  | "bridge"
  | "vlan"
  | "mtu"
  | "queues"
  | "families"
  | "prefix"
  | "gateway"
  | "prefix6"
  | "gateway6"
  | "dnsServers"
  | "searchDomains"
  | "ntpServers"
>;

/** networkFieldsOf reads a cluster's network variable into the form's fields a new cluster takes from it. */
export function networkFieldsOf(v: unknown, base: Form): NetworkFields {
  const network = record(v);
  const v6 = record(network["ipv6"]);
  return {
    families: choiceOf(network["ipFamilies"]),
    prefix6: quantity(v6["prefix"]) ?? base.prefix6,
    gateway6: text(v6["gateway"]) ?? "",
    bridge: text(network["bridge"]) ?? "",
    vlan: quantity(network["vlan"])?.toString() ?? "",
    mtu: quantity(network["mtu"])?.toString() ?? "",
    queues: quantity(network["queues"])?.toString() ?? "",
    prefix: quantity(network["prefix"]) ?? base.prefix,
    gateway: text(network["gateway"]) ?? "",
    dnsServers: (texts(network["dnsServers"]) ?? []).join(", "),
    searchDomains: (texts(network["searchDomains"]) ?? []).join(", "),
    ntpServers: (texts(network["ntp"]) ?? []).join(", "),
  };
}

/** withFreeAddresses fills the API address and the node addresses with those found free, where the user has given none. */
export function withFreeAddresses(f: Form, free: FreeAddresses | undefined, touched: (k: keyof Form) => boolean): Form {
  const filledIn = (k: "endpoint" | "addresses" | "addresses6", found: string | undefined): string =>
    f[k] === "" && !touched(k) ? (found ?? "") : f[k];
  // The API address is of the primary family.
  const endpoint = familiesOf(f.families)[0] === "IPv6" ? free?.endpoint6 : free?.endpoint;
  return {
    ...f,
    endpoint: filledIn("endpoint", endpoint),
    addresses: filledIn("addresses", free?.addresses),
    addresses6: filledIn("addresses6", free?.addresses6),
  };
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
  const bits6 = Number(net.cidr6?.split("/")[1]);
  return {
    ...f,
    prefix: touched("prefix") || !whole(bits, 1, bitsOf("IPv4")) ? f.prefix : bits,
    prefix6: touched("prefix6") || !whole(bits6, 1, bitsOf("IPv6")) ? f.prefix6 : bits6,
    gateway: f.gateway === "" && !touched("gateway") ? (net.gateway ?? "") : f.gateway,
    gateway6: f.gateway6 === "" && !touched("gateway6") ? (net.gateway6 ?? "") : f.gateway6,
    dnsServers:
      f.dnsServers === "" && !touched("dnsServers")
        ? net.dns.filter((d) => familiesOf(f.families).includes(familyOf(d) ?? "IPv4")).join(", ")
        : f.dnsServers,
    searchDomains: f.searchDomains === "" && !touched("searchDomains") ? net.search : f.searchDomains,
  };
}

/** newForm is the wizard before anything is chosen. */
export const newForm = (): Form => ({
  from: "",
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
  controlPlaneTimeouts: noTimeouts,
  workerTimeouts: noTimeouts,
  endpoint: "",
  bridge: "",
  vlan: "",
  mtu: "",
  queues: "",
  securityGroups: "",
  families: "ipv4",
  addresses: "",
  prefix: 24,
  gateway: "",
  addresses6: "",
  prefix6: 64,
  gateway6: "",
  dnsServers: "",
  searchDomains: "",
  ntpServers: "",
  registryMirrors: [],
  trustedCAs: "",
  loadBalancer: "",
  loadBalancerAddresses: "",
  loadBalancerMode: "l2",
  loadBalancerAsn: "",
  loadBalancerPeers: "",
  loadBalancerRouterID: "",
  loadBalancerAuthenticated: false,
  loadBalancerPassword: "",
  pods: "",
  services: "",
  storage: "",
  place: anyNode,
  hostOutageGrace: defaultGrace,
  proxmoxHA: true,
  datastoreAllocate: false,
  volumeSnapshots: false,
  haGroups: true,
  controlPlaneMachine: machineDefaults(undefined),
  workerMachine: machineDefaults(undefined),
  workerNodes: noNodeSettings,
  nodeCustomisation: noNodeCustomisation,
  cni: "",
  headlamp: false,
  headlampHostname: "",
  headlampView: false,
  monitoring: false,
  grafanaHostname: "",
  alerting: newAlerting(),
  alertingSecrets: {},
  logs: newLogs(),
  podSecurity: podSecurityOf(undefined),
  cis: false,
  etcdQuotaGiB: defaultEtcdQuotaGiB,
  istio: false,
  clusterMesh: false,
  meshPeers: [],
  rancherInstall: false,
  rancherHostname: "",
  rancherRegister: true,
  certificates: newCertificates(),
  namespaceLabels: "",
  existingCertManager: false,
  dns01Token: "",
  eabKey: "",
  gitops: newGitOps(),
  egress: newEgress(),
  oidc: [],
  upgradeMode: "off",
  backups: false,
  backupFields: newBackupFields(),
  storeBackupKey: true,
  backupKey: {},
  etcdSnapshots: true,
  etcdFields: etcdFieldsOf({ enabled: false }),
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

/** chosenNodeOS is the node OS a form's nodes run: the one chosen, else the class's default. None for Talos. */
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
    workerNodes: variable(c, "workerNodes") ? f.workerNodes : noNodeSettings,
    workerAutoscale: f.workerAutoscale && variable(c, "autoscaler") !== undefined,
    oidc: kept("oidc") ? f.oidc : oidcProviders(variable(c, "oidc")?.default),
  };
}

// usesExistingCertManager is whether the form ticks the existing cert-manager and the class offers it.
const usesExistingCertManager = (form: Form, c: ClusterClass | undefined): boolean =>
  form.existingCertManager && variable(c, "useExisting") !== undefined;

// hardeningVariables are the variables of the cluster's pod security, CIS profile and etcd limit.
function hardeningVariables(form: Form, defined: Set<string>): Record<string, unknown> {
  const variables: Record<string, unknown> = {};
  const cis = form.cis && defined.has("cis");
  const podSecurity = podSecurityVariable(enforced(form.podSecurity, cis));
  if (podSecurity) {
    variables["podSecurity"] = podSecurity;
  }
  if (cis) {
    variables["cis"] = true;
  }
  if (defined.has("etcd") && form.etcdQuotaGiB !== defaultEtcdQuotaGiB) {
    variables["etcd"] = { quotaGiB: form.etcdQuotaGiB };
  }
  return variables;
}

// hardeningRows sum up the cluster's CIS profile, pod security and etcd limit, as the class offers them.
function hardeningRows(form: Form, defined: Set<string>): [string, string][] {
  const rows: [string, string][] = [];
  const cis = form.cis && defined.has("cis");
  if (defined.has("cis")) {
    rows.push(["CIS hardening", cisText(cis)]);
  }
  if (defined.has("podSecurity")) {
    rows.push(["Pod security", podSecurityText(enforced(form.podSecurity, cis))]);
  }
  if (defined.has("etcd")) {
    rows.push(["etcd", etcdText(form.etcdQuotaGiB)]);
  }
  return rows;
}

// addOnVariables are the variables of the add-ons and the hardening a form chooses.
function addOnVariables(form: Form, c: ClusterClass, defined: Set<string>): Record<string, unknown> {
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
    variables["monitoring"] = {
      enabled: true,
      ...(form.grafanaHostname && { hostname: form.grafanaHostname }),
      ...alertingVariable(form.alerting),
    };
  }
  if (logsChosen(form)) {
    variables["logs"] = logsVariable(form.logs);
  }
  Object.assign(variables, hardeningVariables(form, defined));
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
  if (etcdChosen(form, c)) {
    variables["etcdSnapshots"] = etcdVariable(form, c);
  }
  const certificates = defined.has("certificates") ? certificatesVariable(form.certificates) : undefined;
  if (certificates) {
    variables["certificates"] = certificates;
  }
  return { ...variables, ...reachVariables(form, defined) };
}

// reachVariables are those of the add-ons that reach beyond the cluster: where it syncs from and
// the proxy it goes through.
function reachVariables(form: Form, defined: Set<string>): Record<string, unknown> {
  const variables: Record<string, unknown> = {};
  const gitops = gitOpsVariable(form.gitops);
  if (gitops !== undefined) {
    variables["gitops"] = gitops;
  }
  const egress = defined.has("egressProxy") ? egressVariable(form.egress) : undefined;
  if (egress !== undefined) {
    variables["egressProxy"] = egress;
  }
  const mirrors = defined.has("registryMirrors") ? mirrorsVariable(form.registryMirrors) : undefined;
  if (mirrors !== undefined) {
    variables["registryMirrors"] = mirrors;
  }
  const trusted = defined.has("trustedCAs") ? trustedVariable(form.trustedCAs) : undefined;
  if (trusted !== undefined) {
    variables["trustedCAs"] = trusted;
  }
  return { ...variables, ...installVariables(form, defined) };
}

// installVariables are the variables of where the add-ons go and what they find: set only when chosen.
function installVariables(form: Form, defined: Set<string>): Record<string, unknown> {
  const variables: Record<string, unknown> = {};
  const labels = defined.has("namespaceLabels") ? parseLabels(form.namespaceLabels).labels : {};
  if (Object.keys(labels).length > 0) {
    variables["namespaceLabels"] = labels;
  }
  if (form.existingCertManager && defined.has("useExisting")) {
    variables["useExisting"] = { certManager: true };
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
  if (keyNeeded(form, c) && form.storeBackupKey) {
    pending.push({
      purpose: "backup",
      values: form.backupKey,
      label: "S3 access key",
      section: backupsChosen(form, c) ? "Backups" : "etcd snapshots",
      forget: {},
    });
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
  if (variable(c, "certificates") && needsBindingKey(form.certificates) && filled(form.eabKey)) {
    pending.push({
      purpose: "acme-eab",
      values: { hmacKey: form.eabKey },
      label: "ACME HMAC key",
      section: "Certificates",
      forget: { eabKey: "" },
    });
  }
  if (form.monitoring && variable(c, "monitoring")) {
    for (const p of pendingAlerting(form.alerting, form.alertingSecrets)) {
      pending.push({
        ...p,
        section: "Monitoring",
        forget: { alertingSecrets: { ...form.alertingSecrets, [p.purpose]: {} } },
      });
    }
  }
  pending.push(...pendingBGPPassword(form, c));
  return pending;
}

/** pendingBGPPassword is the BGP sessions' password, when the form's load balancer peers with one. */
function pendingBGPPassword(form: Form, c: ClusterClass): PendingCredential[] {
  const lb = formLoadBalancer(form, c.distribution);
  if (
    !variable(c, "loadBalancer") ||
    lb.implementation === "" ||
    lb.mode !== "bgp" ||
    !lb.authenticated ||
    !filled(form.loadBalancerPassword)
  ) {
    return [];
  }
  return [
    {
      purpose: "bgp",
      values: { password: form.loadBalancerPassword },
      label: "BGP password",
      section: "Load balancer",
      forget: { loadBalancerPassword: "" },
    },
  ];
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

// interfaceOptions are the nodes' interface settings the user entered: the VLAN tag, MTU and queues.
function interfaceOptions(form: Form): Record<string, number> {
  const options: Record<string, number> = {};
  for (const key of ["vlan", "mtu", "queues"] as const) {
    if (form[key]) {
      options[key] = Number(form[key]);
    }
  }
  return options;
}

// timeoutRequests are the timeouts the form sets, left out where all of a group's are empty.
function timeoutRequests(form: Form): Pick<CreateRequest, "controlPlaneTimeouts" | "workerTimeouts"> {
  const controlPlane = timeoutsOf(form.controlPlaneTimeouts);
  const workers = timeoutsOf(form.workerTimeouts);
  return {
    ...(timeoutsSet(controlPlane) && { controlPlaneTimeouts: controlPlane }),
    ...(timeoutsSet(workers) && { workerTimeouts: workers }),
  };
}

// workerNodesVariable is the labels and taints the form gives the workers, where the class has them
// and there are workers.
function workerNodesVariable(form: Form, defined: ReadonlySet<string>): Record<string, unknown> {
  return defined.has("workerNodes") && mostWorkers(form) > 0 && hasNodeSettings(form.workerNodes)
    ? { workerNodes: nodesVariable(form.workerNodes) }
    : {};
}

// The wait is sent only when it differs from the class's default. request drops it if the class lacks it.
function graceVariables(form: Form): Record<string, unknown> {
  return form.hostOutageGrace === defaultGrace ? {} : { [graceVariable]: Number(form.hostOutageGrace) };
}

// networkVariable is the network variable of the form's nodes.
function networkVariable(form: Form, c: ClusterClass): Record<string, unknown> {
  const v4 = hasFamily(form.families, "IPv4");
  const classKnows = variable(c, "network")?.properties?.["ipFamilies"] !== undefined;
  return {
    bridge: form.bridge,
    ...((form.families !== "ipv4" || classKnows) && { ipFamilies: familyNames(form.families) }),
    ...(v4 && { addresses: list(form.addresses), prefix: form.prefix, gateway: form.gateway }),
    ...(hasFamily(form.families, "IPv6") && {
      ipv6: { addresses: list(form.addresses6), prefix: form.prefix6, gateway: form.gateway6 },
    }),
    dnsServers: list(form.dnsServers),
    ...interfaceOptions(form),
    ...(list(form.searchDomains).length > 0 && { searchDomains: list(form.searchDomains) }),
    ...(list(form.ntpServers).length > 0 && { ntp: list(form.ntpServers) }),
  };
}

// proxmoxVariables are the variables of where the nodes go on Proxmox VE and how it looks after them.
function proxmoxVariables(form: Form, defined: ReadonlySet<string>): Record<string, unknown> {
  const variables: Record<string, unknown> = { ...graceVariables(form) };
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
  if (list(form.securityGroups).length > 0 && defined.has("securityGroups")) {
    variables["securityGroups"] = list(form.securityGroups);
  }
  return variables;
}

// request turns the form into the API's request, with only the variables the class defines.
export function request(form: Form, c: ClusterClass): CreateRequest {
  const defined = new Set(c.variables.map((v) => v.name));
  const variables: Record<string, unknown> = {
    controlPlaneEndpoint: form.endpoint,
    network: networkVariable(form, c),
    storage: form.storage,
    controlPlaneMachine: form.controlPlaneMachine,
    workerMachine: form.workerMachine,
    ...nodeOSVariable(form, c),
    ...proxmoxVariables(form, defined),
    ...distributionVariables(form),
    ...workerNodesVariable(form, defined),
  };
  const customised = nodeCustomisationVariable(form.nodeCustomisation, c.distribution === "talos");
  if (Object.keys(customised).length > 0) {
    variables["nodeCustomisation"] = customised;
  }
  const lb = loadBalancerVariable(formLoadBalancer(form, c.distribution));
  if (lb) {
    variables["loadBalancer"] = lb;
  }
  Object.assign(variables, addOnVariables(form, c, defined));
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
    ...timeoutRequests(form),
  };
  if (autoscales(form, c)) {
    req.workerAutoscaling = { min: form.workerReplicas, max: form.workerMax };
  }
  if (list(form.pods).length > 0) {
    req.pods = list(form.pods);
  }
  if (list(form.services).length > 0) {
    req.services = list(form.services);
  }
  return req;
}

const gib = (mib: number): string => (mib % mibPerGiB === 0 ? `${mib / mibPerGiB} GiB` : `${mib} MiB`);
/** coresText counts cores, with "core" for one. */
export const coresText = (n: number): string => `${n} ${n === 1 ? "core" : "cores"}`;
const machine = (m: Machine): string => `${coresText(m.cores)}, ${gib(m.memoryMiB)} memory, ${m.diskGiB} GiB disk`;

/** formLoadBalancerValue is what the load-balancer fields show of the form. */
export const formLoadBalancerValue = (form: Form): LoadBalancerValue => ({
  implementation: form.loadBalancer,
  addresses: form.loadBalancerAddresses,
  mode: form.loadBalancerMode,
  asn: form.loadBalancerAsn,
  peers: form.loadBalancerPeers,
  routerID: form.loadBalancerRouterID,
  authenticated: form.loadBalancerAuthenticated,
});

const formLoadBalancer = (form: Form, distribution: string | undefined): LoadBalancer =>
  loadBalancerFrom(formLoadBalancerValue(form), distribution);

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

function interfaceRows(form: Form): [string, string][] {
  const rows: [string, string][] = [];
  if (form.mtu) {
    rows.push(["MTU", form.mtu]);
  }
  if (form.queues) {
    rows.push(["Queues", form.queues]);
  }
  if (list(form.securityGroups).length > 0) {
    rows.push(["Security groups", list(form.securityGroups).join(", ")]);
  }
  return rows;
}

// loadBalancerRows are the summary's rows for the load balancer: its choice, and for a BGP password
// whether it is stored once the cluster exists.
function loadBalancerRows(form: Form, c: ClusterClass): [string, string][] {
  const lb = formLoadBalancer(form, c.distribution);
  const rows: [string, string][] = [["Load balancer", loadBalancerText(lb, c.distribution)]];
  if (lb.implementation !== "" && lb.mode === "bgp" && lb.authenticated) {
    rows.push([
      "BGP password",
      filled(form.loadBalancerPassword) ? "Stored once created" : "None yet: the cluster waits",
    ]);
  }
  return rows;
}

const timeServersRow = (form: Form): [string, string][] =>
  list(form.ntpServers).length > 0 ? [["Time servers", list(form.ntpServers).join(", ")]] : [];

/** SummaryOffered names the settings the Confirm step shows only where they can be chosen. */
interface SummaryOffered {
  ha?: boolean;
  csi?: boolean;
  snapshots?: boolean;
  haGroups?: boolean;
}

// proxmoxRows are the summary's rows of where the nodes go on Proxmox VE and how it looks after them.
function proxmoxRows(form: Form, defined: ReadonlySet<string>, offered: SummaryOffered): [string, string][] {
  const rows: [string, string][] = [];
  if (defined.has("allowedNodes")) {
    rows.push(["Placement", placeText(form.place)]);
  }
  if (defined.has(graceVariable)) {
    rows.push(["Host outage wait", graceText(form.hostOutageGrace)]);
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
  return rows;
}

// summary is what the Confirm step shows: each setting by its name, as the user chose it. Proxmox
// VE HA, the CSI plugin's release and the HA group labels only where they can be chosen.
export function summary(form: Form, c: ClusterClass, offered: SummaryOffered = {}): [string, string][] {
  const defined = new Set(c.variables.map((v) => v.name));
  const shownFamilies = familiesOf(form.families).length;
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
    ...interfaceRows(form),
    ...(form.families === "ipv4" ? [] : [["Address families", familyLabels[form.families]] as [string, string]]),
    ["API address", form.endpoint],
    ...(hasFamily(form.families, "IPv4")
      ? ([
          [familyFieldLabel("Node addresses", "IPv4", shownFamilies), list(form.addresses).join(", ")],
          [familyFieldLabel("Prefix length and gateway", "IPv4", shownFamilies), `/${form.prefix}, ${form.gateway}`],
        ] as [string, string][])
      : []),
    ...(hasFamily(form.families, "IPv6")
      ? ([
          [familyFieldLabel("Node addresses", "IPv6", shownFamilies), list(form.addresses6).join(", ")],
          [familyFieldLabel("Prefix length and gateway", "IPv6", shownFamilies), `/${form.prefix6}, ${form.gateway6}`],
        ] as [string, string][])
      : []),
    ["DNS servers", list(form.dnsServers).join(", ")],
    ["Search domains", list(form.searchDomains).join(", ") || "None"],
    ...timeServersRow(form),
  ];
  if (defined.has("loadBalancer")) {
    rows.push(...loadBalancerRows(form, c));
  }
  rows.push(
    ["Pod range", form.pods || "Automatic"],
    ["Service range", form.services || "Automatic"],
    ["Storage", form.storage],
    ...proxmoxRows(form, defined, offered),
    ["Control-plane machines", machine(form.controlPlaneMachine)],
  );
  if (mostWorkers(form) > 0) {
    rows.push(["Worker machines", machine(form.workerMachine)]);
    if (defined.has("workerNodes")) {
      rows.push(["Worker labels and taints", nodeSettingsText(form.workerNodes)]);
    }
  }
  if (defined.has("cni")) {
    rows.push(["Pod network", cniName(form.cni) || "Not chosen"]);
  }
  rows.push(...customisationRows(form, defined), ...addOnRows(form, c, defined));
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

function monitoringText(form: Form): string {
  if (!form.monitoring) {
    return "Off";
  }
  const sends = alertingText(form.alerting);
  return `On${form.grafanaHostname ? `, Grafana at ${form.grafanaHostname}` : ""}${sends ? `, sends to ${sends}` : ""}`;
}

const storedOnceCreated = (typed: string): string =>
  filled(typed) ? "Stored once created" : "None yet: the cluster waits";

// certificatesRows sums up the certificates and whether the credentials they take are held.
function certificatesRows(form: Form): [string, string][] {
  const rows: [string, string][] = [["Certificates", certificatesText(form.certificates)]];
  if (needsClusterToken(form.certificates)) {
    rows.push([tokenLabel(form.certificates.provider), storedOnceCreated(form.dns01Token)]);
  }
  if (needsBindingKey(form.certificates)) {
    rows.push(["ACME HMAC key", storedOnceCreated(form.eabKey)]);
  }
  return rows;
}

const customisationRows = (form: Form, defined: Set<string>): [string, string][] =>
  defined.has("nodeCustomisation") ? [["Node customisation", nodeCustomisationText(form.nodeCustomisation)]] : [];

// backupRows sums up the backups and etcd snapshots, each with where its access key comes from.
function backupRows(form: Form, c: ClusterClass, defined: Set<string>): [string, string][] {
  const rows: [string, string][] = [];
  const key = form.storeBackupKey ? "own access key" : "access key to store later";
  if (defined.has("backups")) {
    rows.push(["Backups", form.backups ? `${backupsText(backupsVariable(form.backupFields))}, ${key}` : "Off"]);
  }
  if (etcdOffered(c)) {
    const sum = etcdSnapshotsText(etcdVariable(form, c), c.distribution);
    rows.push(["etcd snapshots", form.etcdSnapshots ? `${sum}, ${key}` : "Off"]);
  }
  return rows;
}

// addOnRows sums up the add-ons and the hardening the class offers.
function addOnRows(form: Form, c: ClusterClass, defined: Set<string>): [string, string][] {
  const rows: [string, string][] = [];
  if (defined.has("headlamp")) {
    rows.push(["Headlamp", headlampText(form.headlamp, form.headlampHostname, form.headlampView)]);
  }
  if (defined.has("monitoring")) {
    rows.push(["Monitoring", monitoringText(form)]);
  }
  if (defined.has("logs")) {
    rows.push(["Logs", logsText({ ...form.logs, enabled: logsChosen(form) })]);
  }
  rows.push(...hardeningRows(form, defined));
  if (defined.has("istio")) {
    rows.push(["Istio", istioText(form.istio)]);
  }
  if (defined.has("clusterMesh") && form.cni === "cilium") {
    rows.push(["Cluster Mesh", meshChoice(form)]);
  }
  if (defined.has("rancher")) {
    rows.push(["Rancher", rancherChoice(form)]);
  }
  rows.push(...backupRows(form, c, defined));
  if (defined.has("certificates")) {
    rows.push(...certificatesRows(form));
  }
  return [...rows, ...reachRows(form, defined)];
}

// reachRows sum up the add-ons that reach beyond the cluster.
function reachRows(form: Form, defined: Set<string>): [string, string][] {
  const rows: [string, string][] = [];
  if (defined.has("gitops")) {
    rows.push(["GitOps", gitOpsText(form.gitops)]);
  }
  if (defined.has("egressProxy")) {
    rows.push(["Egress proxy", egressText(form.egress)]);
  }
  if (defined.has("registryMirrors") && mirrorsVariable(form.registryMirrors) !== undefined) {
    rows.push(["Registry mirrors", mirrorsText(form.registryMirrors)]);
  }
  if (defined.has("trustedCAs") && trustedVariable(form.trustedCAs) !== undefined) {
    rows.push(["Trusted CAs", trustedText(form.trustedCAs)]);
  }
  return [...rows, ...installRows(form, defined)];
}

// installRows sum up where the add-ons go and what they find.
function installRows(form: Form, defined: Set<string>): [string, string][] {
  const rows: [string, string][] = [];
  if (defined.has("useExisting")) {
    rows.push(["Existing cert-manager", form.existingCertManager ? "Yes" : "No"]);
  }
  if (defined.has("namespaceLabels")) {
    rows.push(["Namespace labels", labelsText(parseLabels(form.namespaceLabels).labels)]);
  }
  return rows;
}

// rancherMinimumCores is what Rancher's documentation asks of every node of its cluster. The
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
  | "timeouts"
  | "etcdQuotaGiB"
  | "addons"
  | "bridge"
  | "vlan"
  | "mtu"
  | "queues"
  | "securityGroups"
  | "endpoint"
  | "addresses"
  | "prefix"
  | "gateway"
  | "addresses6"
  | "prefix6"
  | "gateway6"
  | "dnsServers"
  | "searchDomains"
  | "ntpServers"
  | "loadBalancerAddresses"
  | "loadBalancerAsn"
  | "loadBalancerPeers"
  | "loadBalancerRouterID"
  | "loadBalancerPassword"
  | "pods"
  | "services"
  | "storage"
  | "datastoreAllocate"
  | "volumeSnapshots"
  | "placement"
  | "hostOutageGrace"
  | "controlPlaneMachine"
  | "workerMachine"
  | "workerNodes"
  | "nodeCustomisation"
  | "cni"
  | "rancherHostname"
  | "headlampHostname"
  | "logs"
  | "backupTarget"
  | "backupSchedule"
  | "backupRetention"
  | "backupKey"
  | "etcdTarget"
  | "etcdSchedule"
  | "etcdKeep"
  | "certificates"
  | "namespaceLabels"
  | `gitops.${GitOpsField}`
  | `egress.${EgressField}`
  | "registryMirrors"
  | "trustedCAs"
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
  if (
    Object.keys(timeoutProblems(form.controlPlaneTimeouts)).length > 0 ||
    Object.keys(timeoutProblems(form.workerTimeouts)).length > 0
  ) {
    p.timeouts = "Check the node timeouts";
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

/** The form's node networks, the primary family's first. */
function familyNetworks(form: Form): {
  family: Family;
  key: "addresses" | "addresses6";
  entries: string[];
  gateway: string;
  prefix: number;
}[] {
  return familiesOf(form.families).map((family) =>
    family === "IPv4"
      ? { family, key: "addresses", entries: list(form.addresses), gateway: form.gateway, prefix: form.prefix }
      : { family, key: "addresses6", entries: list(form.addresses6), gateway: form.gateway6, prefix: form.prefix6 },
  );
}

function addressProblems(form: Form, routed: boolean): Problems {
  const p: Problems = {};
  const needed = nodesNeeded(form);
  const networks = familyNetworks(form);
  for (const n of networks) {
    const { count, bad } = addressCount(n.entries, n.gateway, n.prefix);
    const other = n.entries.find((e) => {
      const f = familyOf(e.split(/[/-]/u)[0] ?? "");
      return f !== undefined && f !== n.family;
    });
    if (n.entries.length === 0) {
      p[n.key] = "Enter at least one address, range or network";
    } else if (other !== undefined) {
      p[n.key] = `${other} is an ${n.family === "IPv4" ? "IPv6" : "IPv4"} address. Enter ${n.family} addresses here`;
    } else if (bad !== undefined) {
      p[n.key] = `${bad} is not an address, range or network on the gateway's network`;
    } else if (count < needed) {
      p[n.key] = `Enter at least ${needed} addresses, one per node plus one spare`;
    }
  }
  const [primary] = networks;
  const endpointFamily = familyOf(form.endpoint);
  if (primary === undefined || endpointFamily === undefined) {
    p.endpoint = `Enter an ${primary?.family ?? "IPv4"} address`;
  } else if (endpointFamily !== primary.family) {
    p.endpoint = `Use an ${primary.family} address, the primary family`;
  } else if (
    inList(
      form.endpoint,
      networks.flatMap((n) => n.entries),
    )
  ) {
    p.endpoint = "Use an address outside the node addresses";
  } else if (!routed && addressCount([form.endpoint], primary.gateway, primary.prefix).bad !== undefined) {
    p.endpoint = "Use an address on the gateway's network";
  }
  return p;
}

// dnsProblem holds the DNS servers to the cluster's families: each is an address of one of them,
// and a cluster with both has a server of each.
function dnsProblem(form: Form): string | undefined {
  const servers = list(form.dnsServers);
  const families = familiesOf(form.families);
  if (servers.length === 0) {
    return "Enter at least one DNS server";
  }
  const got = servers.map((d) => familyOf(d));
  const only = families.length === 1 ? (families[0] ?? "IP") : "IP";
  if (got.some((f) => f === undefined || !families.includes(f))) {
    return `DNS servers must be ${only} addresses`;
  }
  const lacking = families.find((f) => !got.includes(f));
  return lacking === undefined ? undefined : `Add an ${lacking} DNS server too`;
}

// rangesProblem holds pod or service ranges to the cluster's families: one network of each, in order.
function rangesProblem(form: Form, v: string): string | undefined {
  const ranges = list(v);
  for (const r of ranges) {
    const problem = rangeProblem(r);
    if (problem !== undefined) {
      return problem;
    }
  }
  const got = ranges.map((r) => parseCidr(r)?.address.family);
  const want = familiesOf(form.families);
  return ranges.length === 0 || got.join(",") === want.join(",")
    ? undefined
    : `Enter one range per family, in order: ${want.join(", ")}`;
}

function interfaceProblems(form: Form): Problems {
  const p: Problems = {};
  if (form.vlan && !isVLAN(form.vlan)) {
    p.vlan = "A whole number from 1 to 4094";
  }
  if (form.mtu && !isMTU(form.mtu)) {
    p.mtu = `A whole number from ${minMTU} to ${maxMTU}`;
  }
  if (form.queues && !isQueues(form.queues)) {
    p.queues = `A whole number from 1 to ${maxQueues}`;
  }
  const groups = securityGroupsProblem(form.securityGroups);
  if (groups !== undefined) {
    p.securityGroups = groups;
  }
  return p;
}

/** balancerProblems holds the load balancer's fields to the nodes' network, or in BGP to the routers. */
function balancerProblems(form: Form, c: ClusterClass | undefined): Problems {
  const lb = loadBalancerProblems(formLoadBalancerValue(form), c?.distribution, {
    families: form.families,
    gateway: form.gateway,
    prefix: form.prefix,
    gateway6: form.gateway6,
    prefix6: form.prefix6,
    addresses: [
      ...(hasFamily(form.families, "IPv4") ? list(form.addresses) : []),
      ...(hasFamily(form.families, "IPv6") ? list(form.addresses6) : []),
    ],
    endpoint: form.endpoint,
  });
  const p: Problems = {};
  if (lb.addresses !== undefined) {
    p.loadBalancerAddresses = lb.addresses;
  }
  if (lb.asn !== undefined) {
    p.loadBalancerAsn = lb.asn;
  }
  if (lb.peers !== undefined) {
    p.loadBalancerPeers = lb.peers;
  }
  if (lb.routerID !== undefined) {
    p.loadBalancerRouterID = lb.routerID;
  }
  if (form.loadBalancerAuthenticated && /[\s:,]/u.test(form.loadBalancerPassword)) {
    p.loadBalancerPassword = "No spaces, colons or commas";
  }
  return p;
}

/** setLoadBalancer sets the form fields behind the load-balancer fields that a patch holds. */
export function setLoadBalancer(
  set: <K extends keyof Form>(k: K, v: Form[K]) => void,
  patch: Partial<LoadBalancerValue>,
): void {
  if (patch.implementation !== undefined) {
    set("loadBalancer", patch.implementation);
  }
  if (patch.addresses !== undefined) {
    set("loadBalancerAddresses", patch.addresses);
  }
  if (patch.mode !== undefined) {
    set("loadBalancerMode", patch.mode);
  }
  if (patch.asn !== undefined) {
    set("loadBalancerAsn", patch.asn);
  }
  if (patch.peers !== undefined) {
    set("loadBalancerPeers", patch.peers);
  }
  if (patch.routerID !== undefined) {
    set("loadBalancerRouterID", patch.routerID);
  }
  if (patch.authenticated !== undefined) {
    set("loadBalancerAuthenticated", patch.authenticated);
  }
}

// cniProblem refuses a pod network the node OS or the cluster's name cannot run with.
function cniProblem(form: Form, c: ClusterClass | undefined): string | undefined {
  if (chosenNodeOS(form, c)?.fips === true && form.cni !== "" && form.cni !== fipsCNI) {
    return "FIPS mode needs Canal";
  }
  if (form.cni === "cilium" && form.name.length > maxCiliumName) {
    return `Cilium needs a cluster name of ${maxCiliumName} characters or fewer`;
  }
  return undefined;
}

function networkProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (!form.bridge) {
    p.bridge = "Choose a bridge";
  }
  Object.assign(p, interfaceProblems(form));
  for (const family of familiesOf(form.families)) {
    const [prefixKey, gatewayKey] =
      family === "IPv4" ? (["prefix", "gateway"] as const) : (["prefix6", "gateway6"] as const);
    if (!whole(form[prefixKey], 1, bitsOf(family))) {
      p[prefixKey] = `A whole number from 1 to ${bitsOf(family)}`;
    }
    if (familyOf(form[gatewayKey]) !== family) {
      p[gatewayKey] = `Enter an ${family} address`;
    }
  }
  const lbValue = formLoadBalancerValue(form);
  Object.assign(
    p,
    addressProblems(form, lbValue.mode === "bgp" && bgpOffered(c?.distribution, lbValue.implementation)),
  );
  const dns = dnsProblem(form);
  if (dns !== undefined) {
    p.dnsServers = dns;
  }
  const domains = searchDomainsProblem(list(form.searchDomains));
  if (domains !== undefined) {
    p.searchDomains = domains;
  }
  const ntpProblem = timeServersProblem(list(form.ntpServers));
  if (ntpProblem !== undefined) {
    p.ntpServers = ntpProblem;
  }
  Object.assign(p, nodeSiteProblems(form, c), balancerProblems(form, c));
  for (const [key, v] of [
    ["pods", form.pods],
    ["services", form.services],
  ] as const) {
    const problem = v ? rangesProblem(form, v) : undefined;
    if (problem !== undefined) {
      p[key] = problem;
    }
  }
  if (c?.variables.find((v) => v.name === "cni")?.required === true && !form.cni) {
    p.cni = "Choose a pod network";
  } else {
    const cni = cniProblem(form, c);
    if (cni !== undefined) {
      p.cni = cni;
    }
  }
  return p;
}

// machineProblem holds a machine to its least cores, memory and disk. MachineFields places the
// message by its cores, memory or disk.
function machineProblem(
  form: Form,
  c: ClusterClass | undefined,
  key: "controlPlaneMachine" | "workerMachine",
): string | undefined {
  const m = form[key];
  const min = machineLeast(form, c, key);
  const plain = plainLeast(form, c, key);
  // The reason a minimum is higher than usual: the add-ons, or control planes that run the
  // workloads without workers.
  const why = (raised: boolean): string => {
    if (raised) {
      return " for the add-ons chosen";
    }
    return key === "controlPlaneMachine" && mostWorkers(form) === 0 ? " without workers" : "";
  };
  if (!whole(m.cores, min.cores)) {
    return `A whole number of cores, at least ${min.cores}${why(min.cores > plain.cores)}`;
  }
  if (
    form.rancherInstall &&
    m.cores < rancherMinimumCores &&
    (key === "controlPlaneMachine" || form.workerReplicas > 0)
  ) {
    return `Rancher needs at least ${rancherMinimumCores} cores on every node`;
  }
  if (!whole(m.memoryMiB, min.memoryMiB) || m.memoryMiB % memoryStepMiB !== 0) {
    return `At least ${min.memoryMiB} MiB of memory${why(min.memoryMiB > plain.memoryMiB)}, in steps of ${memoryStepMiB}`;
  }
  return whole(m.diskGiB, min.diskGiB) ? undefined : `A whole number of GiB of disk, at least ${min.diskGiB}`;
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
  if (variable(c, "etcd") !== undefined && !whole(form.etcdQuotaGiB, minEtcdQuotaGiB, maxEtcdQuotaGiB)) {
    p.etcdQuotaGiB = `A whole number from ${minEtcdQuotaGiB} to ${maxEtcdQuotaGiB}`;
  }
  const grace = variable(c, graceVariable) ? graceProblem(form.hostOutageGrace) : undefined;
  if (grace !== undefined) {
    p.hostOutageGrace = grace;
  }
  for (const key of ["controlPlaneMachine", "workerMachine"] as const) {
    const problem = machineProblem(form, c, key);
    if (problem !== undefined) {
      p[key] = problem;
    }
  }
  const nodes = variable(c, "workerNodes") && mostWorkers(form) > 0 ? nodeSettingsProblem(form.workerNodes) : undefined;
  if (nodes !== undefined) {
    p.workerNodes = nodes;
  }
  const customised = variable(c, "nodeCustomisation")
    ? nodeCustomisationProblem(form.nodeCustomisation, c?.distribution === "talos")
    : undefined;
  if (customised !== undefined) {
    p.nodeCustomisation = customised;
  }
  return p;
}

// The certificates' fields show their own problems. The step holds them, to keep it from being
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
  const labels = variable(c, "namespaceLabels") ? parseLabels(form.namespaceLabels).problem : undefined;
  if (labels !== undefined) {
    p.namespaceLabels = labels;
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
  return {
    ...p,
    ...egressFormProblems(form, c),
    ...backupsProblems(form, c),
    ...etcdSnapshotsProblems(form, c),
    ...accessKeyProblems(form, c),
  };
}

// egressFormProblems are what is wrong with the egress proxy the form names, by field.
function egressFormProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (variable(c, "egressProxy") === undefined) {
    return p;
  }
  const e = egressProblems(form.egress);
  for (const field of egressFields) {
    const problem = e[field];
    if (problem !== undefined) {
      p[`egress.${field}`] = problem;
    }
  }
  return p;
}

// nodeSiteProblems are what is wrong with the registry mirrors and trusted authorities the form names.
function nodeSiteProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  const mirrors = variable(c, "registryMirrors") === undefined ? undefined : mirrorsProblem(form.registryMirrors);
  if (mirrors !== undefined) {
    p.registryMirrors = mirrors;
  }
  const trusted = variable(c, "trustedCAs") === undefined ? undefined : certificateProblem(form.trustedCAs);
  if (trusted !== undefined) {
    p.trustedCAs = trusted;
  }
  return p;
}

// backupsProblems are what keeps the backups the form switches on from being complete.
function backupsProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (!backupsChosen(form, c)) {
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
  return p;
}

// etcdSnapshotsProblems are what keeps the etcd snapshots the form switches on from being complete.
function etcdSnapshotsProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (!etcdChosen(form, c)) {
    return p;
  }
  const e = etcdProblems(form.etcdFields, classTargets(c), {
    ...(backupsChosen(form, c) && { backups: form.backupFields.target }),
  });
  for (const [key, found] of [
    ["etcdTarget", e.target],
    ["etcdSchedule", e.schedule],
    ["etcdKeep", e.keep],
  ] as const) {
    if (found !== undefined) {
      p[key] = found;
    }
  }
  return p;
}

// accessKeyProblems keep the one key that backups and etcd snapshots share from being left out.
function accessKeyProblems(form: Form, c: ClusterClass | undefined): Problems {
  if (keyNeeded(form, c) && form.storeBackupKey && !backupKeys.every((k) => filled(form.backupKey[k.key]))) {
    return { backupKey: "Enter both parts of the access key" };
  }
  return {};
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
// still offered. The server checks SDN.Use on it.
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

/** bridgeMtuProblems holds the network step's MTU to the chosen bridge's, where a node lists it. */
export function bridgeMtuProblems(step: Step, form: Form, bridges: Bridge[]): Problems {
  if (step !== "Network" || !form.mtu || !isMTU(form.mtu)) {
    return {};
  }
  const mtu = bridges.find((b) => b.bridge === form.bridge)?.mtu;
  if (mtu === undefined || Number(form.mtu) <= mtu) {
    return {};
  }
  return { mtu: `The bridge has MTU ${mtu}. Use ${mtu} or less` };
}

/** csiProblems holds the machines step to the CSI plugin's release a new cluster chose. */
export function csiProblems(step: Step, form: Form, csi: CSIOffer): Problems {
  if (step !== "Machines" || !csi.offered || !form.datastoreAllocate || csi.problem === undefined) {
    return {};
  }
  return { datastoreAllocate: csi.problem };
}

/**
 * bridgeNodeProblems holds the network step to a bridge that exists on a node the cluster may use,
 * as the API server does. A node whose bridges are not listed to the user counts as having it, and
 * so does a name no node lists, which may be a VNet.
 */
export function bridgeNodeProblems(
  step: Step,
  form: Form,
  nodes: { bridgesByNode: Record<string, Bridge[]>; online: string[] },
): Problems {
  if (step !== "Network" || !form.bridge) {
    return {};
  }
  const on = (node: string): boolean | undefined => {
    const known = nodes.bridgesByNode[node];
    return known === undefined || known.length === 0 ? undefined : known.some((b) => b.bridge === form.bridge);
  };
  const allowed = allowedNodes(form.place) ?? nodes.online;
  const holders = Object.keys(nodes.bridgesByNode).filter((node) => on(node) === true);
  if (holders.length === 0 || allowed.length === 0 || allowed.some((node) => on(node) !== false)) {
    return {};
  }
  const [only, ...rest] = holders.toReversed();
  const named = rest.length === 0 ? holders.join("") : `${rest.toReversed().join(", ")} and ${only}`;
  const allow = rest.length === 0 ? `Allow ${named}` : "Allow one of them";
  return { bridge: `${form.bridge} is only on ${named}. ${allow} or choose another bridge.` };
}

/**
 * memoryNodeWarning warns when no node the cluster may use has the free memory for its largest
 * machine, as the Placer judges them. The server accepts such a cluster, so this never blocks. A
 * node whose memory is not reported counts as having it.
 */
export function memoryNodeWarning(form: Form, nodes: PveNode[]): string | undefined {
  const chosen = allowedNodes(form.place);
  const candidates = nodes.filter((n) => n.online && (chosen === undefined || chosen.includes(n.node)));
  const need = Math.max(form.controlPlaneMachine.memoryMiB, form.workerReplicas > 0 ? form.workerMachine.memoryMiB : 0);
  const [best] = candidates.toSorted((a, b) => freeMiB(b) - freeMiB(a));
  if (best === undefined || freeMiB(best) >= need) {
    return undefined;
  }
  const needGiB = Math.ceil(need / mibPerGiB);
  return `No node has ${needGiB} GiB free, the most is ${Math.floor(freeMiB(best) / mibPerGiB)} GiB on ${best.node}. The cluster waits for room.`;
}

// freeMiB is a node's free memory, unlimited while it reports none.
function freeMiB(n: PveNode): number {
  return n.maxMemory === undefined || n.memory === undefined ? Infinity : (n.maxMemory - n.memory) / bytesPerMiB;
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

// The class lists the bridges and storages clusters may use. Without such a list, what any node
// offers.
export const classBridges = (c: ClusterClass | undefined): string[] | undefined =>
  texts(variable(c, "network")?.properties?.["bridge"]?.enum);
export const classStorages = (c: ClusterClass | undefined): string[] | undefined => texts(variable(c, "storage")?.enum);
