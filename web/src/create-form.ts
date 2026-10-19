import { maxWorkers } from "./ClusterDetail";
import { defaultMinutes, defaultStart, describe, type UpgradeMode, type UpgradePolicy } from "./ClusterUpgrades";
import { addressBits, addressCount, inList, ipv4, isVLAN, list, rangeProblem, whole } from "./network";
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
import type { Bridge, ClusterClass, CreateRequest, OIDCProvider, Schema, Storage } from "./types";
import { browserZone, cniName, distributionName, filled, istioText, registeredWithRancher, sentence } from "./ui";
import { quantity, record, texts } from "./values";

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

export interface Form {
  name: string;
  className: string;
  version: string;
  controlPlaneReplicas: number;
  workerReplicas: number;
  endpoint: string;
  bridge: string;
  vlan: string;
  addresses: string;
  prefix: number;
  gateway: string;
  dnsServers: string;
  pods: string;
  services: string;
  storage: string;
  place: Place;
  proxmoxHA: boolean;
  controlPlaneMachine: Machine;
  workerMachine: Machine;
  cni: string;
  headlamp: boolean;
  headlampHostname: string;
  monitoring: boolean;
  grafanaHostname: string;
  podSecurity: PodSecurity;
  cis: boolean;
  istio: boolean;
  clusterMesh: boolean;
  meshPeers: string[];
  rancherInstall: boolean;
  rancherHostname: string;
  rancherRegister: boolean;
  oidc: OIDCProvider[];
  upgradeMode: UpgradeMode;
}

/** newUpgrades is the policy a new cluster starts with: every day from 02:00 for four hours, in the browser's time zone. */
export const newUpgrades = (mode: Form["upgradeMode"], timeZone: string): UpgradePolicy =>
  mode === "off" ? { mode } : { mode, window: { start: defaultStart, durationMinutes: defaultMinutes, timeZone } };

export const versionExamples: Record<string, string> = {
  rke2: "v1.36.5+rke2r1",
  k3s: "v1.36.5+k3s1",
  kubeadm: "v1.36.5",
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
  ["kyverno", "Kyverno"],
  ["istio", "Istio"],
  ["clusterMesh", "Cluster Mesh"],
  ["certManager", "cert-manager"],
  ["rancher", "Rancher"],
  ["rancherAgent", "Rancher's agent"],
] as const;

// maxCiliumName is the longest cluster name Cilium takes, which names the cluster in a mesh.
const maxCiliumName = 32;

/** offersMesh says whether a form's cluster can join a Cluster Mesh: its class offers it and it runs Cilium. */
export const offersMesh = (form: Form, c: ClusterClass | undefined): boolean =>
  variable(c, "clusterMesh") !== undefined && form.cni === "cilium";

const meshChosen = (form: Form): boolean => form.clusterMesh && form.cni === "cilium";

// classFigures reads one of a class's annotations of memory figures: kubehenge.dev/addon-memory by
// add-on, or kubehenge.dev/node-memory, what a controlPlane or worker uses itself.
function classFigures(c: ClusterClass | undefined, annotation: string): Record<string, unknown> {
  try {
    return record(JSON.parse(c?.annotations?.[annotation] ?? "{}"));
  } catch {
    // The controller refuses clusters of such a class with the reason.
    return {};
  }
}

/**
 * addonMemory is the memory, in MiB, the add-ons a form chooses use by its class's figures, and
 * which they are. Rancher's agent counts wherever the cluster would register, as the controller
 * counts it: once a cluster installs Rancher, every other one registers with it.
 */
export function addonMemory(form: Form, c: ClusterClass | undefined): { names: string[]; mib: number } {
  const figures = classFigures(c, "kubehenge.dev/addon-memory");
  const on: Record<(typeof addonNames)[number][0], boolean> = {
    headlamp: form.headlamp,
    monitoring: form.monitoring,
    kyverno: form.podSecurity.kyverno !== "off",
    istio: form.istio,
    clusterMesh: meshChosen(form),
    certManager: form.monitoring || form.rancherInstall,
    rancher: form.rancherInstall,
    rancherAgent: !form.rancherInstall && form.rancherRegister,
  };
  const names: string[] = [];
  let mib = 0;
  for (const [key, name] of addonNames) {
    const figure = quantity(figures[key]) ?? 0;
    if (on[key] && figure > 0) {
      names.push(name);
      mib += figure;
    }
  }
  return { names, mib };
}

const plainLeast = (form: Form, c: ClusterClass | undefined, key: "controlPlaneMachine" | "workerMachine"): Machine =>
  key === "controlPlaneMachine" ? controlPlaneMinimums(c, form.workerReplicas) : machineMinimums(c, key);

// carries says whether a form's machines of one kind run its add-ons: the workers, or without
// workers the control planes.
const carries = (form: Form, key: "controlPlaneMachine" | "workerMachine"): boolean =>
  (key === "workerMachine") === form.workerReplicas > 0;

/**
 * addonNeedMiB is the memory each node that carries a form's add-ons needs for them, as the
 * controller asks: what such a node uses itself, by its class, and its share of the add-ons'.
 * Without add-ons it is 0.
 */
export function addonNeedMiB(form: Form, c: ClusterClass | undefined): number {
  const key = form.workerReplicas > 0 ? "workerMachine" : "controlPlaneMachine";
  const nodes = key === "workerMachine" ? form.workerReplicas : form.controlPlaneReplicas;
  const { mib } = addonMemory(form, c);
  if (mib === 0 || nodes <= 0) {
    return 0;
  }
  const own =
    quantity(classFigures(c, "kubehenge.dev/node-memory")[key === "workerMachine" ? "worker" : "controlPlane"]) ??
    plainLeast(form, c, key).memoryMiB;
  return own + Math.ceil(mib / nodes / memoryStepMiB) * memoryStepMiB;
}

/**
 * machineLeast is the least a form lets its machines of one kind have: the class's, or for the nodes
 * that carry the add-ons what those need, whichever is more.
 */
export function machineLeast(
  form: Form,
  c: ClusterClass | undefined,
  key: "controlPlaneMachine" | "workerMachine",
): Machine {
  const min = plainLeast(form, c, key);
  return carries(form, key) ? { ...min, memoryMiB: Math.max(min.memoryMiB, addonNeedMiB(form, c)) } : min;
}

/**
 * withRoom raises a form's machines to the least its other choices ask of them: control planes that
 * run the workloads without workers, the add-ons' memory and Rancher's cores. It lowers nothing.
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

/** newForm is the wizard before anything is chosen. */
export const newForm = (): Form => ({
  name: "",
  className: "",
  version: "",
  controlPlaneReplicas: 1,
  workerReplicas: 1,
  endpoint: "",
  bridge: "",
  vlan: "",
  addresses: "",
  prefix: 24,
  gateway: "",
  dnsServers: "",
  pods: "",
  services: "",
  storage: "",
  place: anyNode,
  proxmoxHA: true,
  controlPlaneMachine: machineDefaults(undefined),
  workerMachine: machineDefaults(undefined),
  cni: "",
  headlamp: false,
  headlampHostname: "",
  monitoring: false,
  grafanaHostname: "",
  podSecurity: podSecurityOf(undefined),
  cis: false,
  istio: false,
  clusterMesh: false,
  meshPeers: [],
  rancherInstall: false,
  rancherHostname: "",
  rancherRegister: true,
  oidc: [],
  upgradeMode: "off",
});

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
    version: f.version && c && new RegExp(c.versionFormat.pattern, "u").test(f.version) ? f.version : "",
    cni: kept("cni") && (cni?.enum ?? []).includes(f.cni) ? f.cni : cniDefault,
    controlPlaneMachine: kept("controlPlaneMachine")
      ? f.controlPlaneMachine
      : machineDefaults(variable(c, "controlPlaneMachine")),
    workerMachine: kept("workerMachine") ? f.workerMachine : machineDefaults(variable(c, "workerMachine")),
    oidc: kept("oidc") ? f.oidc : oidcProviders(variable(c, "oidc")?.default),
  };
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
  };
  const nodes = allowedNodes(form.place);
  if (nodes !== undefined) {
    variables["allowedNodes"] = nodes;
  }
  // Left out, the class's default has the cluster take part once HA applies to it.
  if (!form.proxmoxHA) {
    variables["proxmoxHA"] = false;
  }
  if (form.cni) {
    variables["cni"] = form.cni;
  }
  if (form.headlamp) {
    variables["headlamp"] = form.headlampHostname
      ? { enabled: true, hostname: form.headlampHostname }
      : { enabled: true };
  }
  if (form.monitoring) {
    variables["monitoring"] = form.grafanaHostname
      ? { enabled: true, hostname: form.grafanaHostname }
      : { enabled: true };
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
  variables["oidc"] = cleanProviders(form.oidc);
  if (form.upgradeMode !== "off") {
    variables["upgrades"] = newUpgrades(form.upgradeMode, browserZone());
  }
  const req: CreateRequest = {
    name: form.name,
    class: c.name,
    version: form.version,
    controlPlaneReplicas: form.controlPlaneReplicas,
    workerReplicas: form.workerReplicas,
    variables: Object.fromEntries(Object.entries(variables).filter(([k]) => defined.has(k))),
  };
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
    return `Installed here, at ${form.rancherHostname}`;
  }
  return form.rancherRegister ? registeredWithRancher : "Not registered";
}

function meshChoice(form: Form): string {
  if (!meshChosen(form)) {
    return "Off";
  }
  return form.meshPeers.length > 0 ? `On, with ${form.meshPeers.toSorted().join(", ")}` : "On, with no peers yet";
}

// summary is what the Confirm step shows: each setting by its name, as the user chose it; Proxmox
// VE HA only where it applies.
export function summary(form: Form, c: ClusterClass, haOffered = false): [string, string][] {
  const defined = new Set(c.variables.map((v) => v.name));
  const chosenRange =
    form.cni === "cilium"
      ? "Chosen clear of the nodes' network and the other clusters' ranges"
      : "Chosen clear of the nodes' network";
  const rows: [string, string][] = [
    ["Name", form.name],
    ["Distribution", distributionName(c.distribution)],
    ["Kubernetes version", form.version],
    ["Control-plane nodes", String(form.controlPlaneReplicas)],
    ["Workers", String(form.workerReplicas)],
    ["Bridge", form.vlan ? `${form.bridge}, VLAN ${form.vlan}` : form.bridge],
    ["API address", form.endpoint],
    ["Node addresses", list(form.addresses).join(", ")],
    ["Prefix length and gateway", `/${form.prefix}, ${form.gateway}`],
    ["DNS servers", list(form.dnsServers).join(", ")],
    ["Pod range", form.pods || chosenRange],
    ["Service range", form.services || chosenRange],
    ["Storage", form.storage],
  ];
  if (defined.has("allowedNodes")) {
    rows.push(["Placement", placeText(form.place)]);
  }
  if (haOffered) {
    rows.push(["Proxmox VE HA", haText(form.proxmoxHA)]);
  }
  rows.push(["Control-plane machines", machine(form.controlPlaneMachine)]);
  if (form.workerReplicas > 0) {
    rows.push(["Worker machines", machine(form.workerMachine)]);
  }
  if (defined.has("cni")) {
    rows.push(["Pod network", cniName(form.cni) || "The distribution's own"]);
  }
  rows.push(...addOnRows(form, defined));
  if (defined.has("upgrades")) {
    const upgrades = newUpgrades(form.upgradeMode, browserZone());
    rows.push(["Automatic upgrades", describe(upgrades)]);
  }
  if (defined.has("oidc")) {
    const providers = cleanProviders(form.oidc);
    rows.push([
      "Identity providers",
      providers.length > 0 ? providers.map((p) => p.issuerURL).join(", ") : "None: tokens and certificates only",
    ]);
  }
  return rows;
}

// addOnRows sums up the add-ons and the hardening the class offers.
function addOnRows(form: Form, defined: Set<string>): [string, string][] {
  const rows: [string, string][] = [];
  if (defined.has("headlamp")) {
    rows.push([
      "Headlamp",
      form.headlamp ? `On${form.headlampHostname ? `, at ${form.headlampHostname}` : ""}` : "Off",
    ]);
  }
  if (defined.has("monitoring")) {
    rows.push([
      "Monitoring",
      form.monitoring ? `On${form.grafanaHostname ? `, Grafana at ${form.grafanaHostname}` : ""}` : "Off",
    ]);
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
  return rows;
}

// rancherMinimumCores is what Rancher's documentation asks of every node of its cluster; the
// controller refuses fewer.
export const rancherMinimumCores = 4;

// FieldKey names a field of the wizard a problem belongs to.
export type FieldKey =
  | "name"
  | "class"
  | "version"
  | "controlPlaneReplicas"
  | "workerReplicas"
  | "bridge"
  | "vlan"
  | "endpoint"
  | "addresses"
  | "prefix"
  | "gateway"
  | "dnsServers"
  | "pods"
  | "services"
  | "storage"
  | "placement"
  | "controlPlaneMachine"
  | "workerMachine"
  | "cni"
  | "rancherHostname"
  | "headlampHostname"
  | "oidc";

type Problems = Partial<Record<FieldKey, string>>;

function generalProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (!/^[a-z](?:[-a-z0-9]{0,38}[a-z0-9])?$/u.test(form.name)) {
    p.name =
      "Lowercase letters, digits and dashes, starting with a letter and ending with a letter or digit, at most 40 characters.";
  }
  if (!c) {
    p.class = "Choose a distribution.";
  } else if (filled(c.noTemplate)) {
    p.class = sentence(c.noTemplate);
  } else if (!new RegExp(c.versionFormat.pattern, "u").test(form.version)) {
    p.version = `${distributionName(c.distribution)} versions look like ${versionExamples[c.distribution] ?? "v1.36.5"}.`;
  }
  if (!whole(form.workerReplicas, 0, maxWorkers)) {
    p.workerReplicas = `A whole number from 0 to ${maxWorkers}.`;
  }
  return p;
}

function addressProblems(form: Form): Problems {
  const p: Problems = {};
  const entries = list(form.addresses);
  const { count, bad } = addressCount(entries, form.gateway, form.prefix);
  // A rollout makes each new node before it removes an old one.
  const needed = form.controlPlaneReplicas + form.workerReplicas + 1;
  if (entries.length === 0) {
    p.addresses = "At least one range, network or address.";
  } else if (bad !== undefined) {
    p.addresses = `${bad} is no address, range or network inside the gateway's network.`;
  } else if (count < needed) {
    p.addresses = `${count} given; the nodes need ${needed}, one more than there are for rollouts.`;
  }
  if (!ipv4.test(form.endpoint)) {
    p.endpoint = "An IPv4 address.";
  } else if (inList(form.endpoint, entries)) {
    p.endpoint = "Not one of the node addresses, which the nodes take.";
  } else if (addressCount([form.endpoint], form.gateway, form.prefix).bad !== undefined) {
    p.endpoint = "Inside the gateway's network.";
  }
  return p;
}

function networkProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (!form.bridge) {
    p.bridge = "Choose a bridge.";
  }
  if (form.vlan && !isVLAN(form.vlan)) {
    p.vlan = "1 to 4094.";
  }
  if (!whole(form.prefix, 1, addressBits)) {
    p.prefix = "1 to 32.";
  }
  if (!ipv4.test(form.gateway)) {
    p.gateway = "An IPv4 address.";
  }
  Object.assign(p, addressProblems(form));
  if (list(form.dnsServers).length === 0) {
    p.dnsServers = "At least one.";
  } else if (list(form.dnsServers).some((d) => !ipv4.test(d))) {
    p.dnsServers = "IPv4 addresses.";
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
    p.cni = "Choose a pod network.";
  } else if (form.cni === "cilium" && form.name.length > maxCiliumName) {
    p.cni = `Cilium takes cluster names of at most ${maxCiliumName} characters; this one has ${form.name.length}.`;
  }
  return p;
}

function machineProblems(form: Form, c: ClusterClass | undefined): Problems {
  const p: Problems = {};
  if (!form.storage) {
    p.storage = "Choose a storage.";
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
    const alone =
      key === "controlPlaneMachine" && form.workerReplicas === 0 ? " Without workers, they run the workloads too." : "";
    const addons =
      min.memoryMiB > plainLeast(form, c, key).memoryMiB
        ? ` The add-ons chosen need that much: ${addonMemory(form, c).mib} MiB, shared by the ${key === "workerMachine" ? "workers" : "control planes"}.`
        : "";
    if (!whole(m.cores, min.cores)) {
      p[key] = `A whole number of cores, at least ${min.cores}.${alone}`;
    } else if (
      form.rancherInstall &&
      m.cores < rancherMinimumCores &&
      (key === "controlPlaneMachine" || form.workerReplicas > 0)
    ) {
      p[key] = `At least ${rancherMinimumCores} cores, which Rancher needs on every node.`;
    } else if (!whole(m.memoryMiB, min.memoryMiB) || m.memoryMiB % memoryStepMiB !== 0) {
      p[key] = `Memory of at least ${min.memoryMiB} MiB, in steps of ${memoryStepMiB}.${alone}${addons}`;
    } else if (!whole(m.diskGiB, min.diskGiB)) {
      p[key] = `A whole number of GiB of disk, at least ${min.diskGiB}.`;
    }
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
      return form.rancherInstall && !form.rancherHostname ? { rancherHostname: "Rancher needs a host name." } : {};
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
