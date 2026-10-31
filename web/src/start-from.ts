import { newForm, type Form, type Machine } from "./create-form";
import { loadBalancerOf } from "./LoadBalancer";
import { logsOf } from "./Logs";
import { nodeOSKey } from "./node-os";
import { oidcProviders } from "./OIDCProviders";
import { placeOf } from "./Placement";
import { podSecurityOf } from "./PodSecurity";
import type { Cluster } from "./types";
import { flag, quantity, record, text, texts } from "./values";

/** clusterKey names a cluster across namespaces. */
export const clusterKey = (c: Pick<Cluster, "namespace" | "name">): string => `${c.namespace}/${c.name}`;

/** startable are the clusters a new one may start from, the newest first. */
export const startable = (clusters: Cluster[]): Cluster[] =>
  clusters.filter((c) => !c.deleting).toSorted((a, b) => b.created.localeCompare(a.created));

/**
 * startKeys are what a new cluster takes from the one it starts from. The rest each cluster has to
 * itself: its name, addresses, ranges, hostnames, credentials and Cluster Mesh peers, and the CSI
 * plugin's Datastore.Allocate, which no two clusters hold on one storage.
 */
export const startKeys: ReadonlySet<keyof Form> = new Set<keyof Form>([
  "className",
  "nodeOS",
  "controlPlaneReplicas",
  "workerReplicas",
  "workerAutoscale",
  "workerMax",
  "bridge",
  "vlan",
  "prefix",
  "gateway",
  "dnsServers",
  "loadBalancer",
  "storage",
  "place",
  "proxmoxHA",
  "haGroups",
  "controlPlaneMachine",
  "workerMachine",
  "cni",
  "headlamp",
  "headlampView",
  "monitoring",
  "logs",
  "podSecurity",
  "cis",
  "istio",
  "rancherRegister",
  "oidc",
]);

function machineOf(v: unknown, fallback: Machine): Machine {
  const r = record(v);
  return {
    cores: quantity(r["cores"]) ?? fallback.cores,
    memoryMiB: quantity(r["memoryMiB"]) ?? fallback.memoryMiB,
    diskGiB: quantity(r["diskGiB"]) ?? fallback.diskGiB,
  };
}

/** startDefault is the cluster a new one starts from unless another is chosen: the newest that works, else the newest. */
export function startDefault(clusters: Cluster[]): Cluster | undefined {
  const starts = startable(clusters);
  return starts.find((c) => c.available) ?? starts[0];
}

/**
 * fromCluster is a new cluster's form set up as c is, keeping the name and namespace already
 * entered, and the version while the distribution stays: its versions are not offered again.
 */
export function fromCluster(c: Cluster, entered: Pick<Form, "name" | "namespace" | "className" | "version">): Form {
  const v = c.variables;
  const base = newForm();
  const network = record(v["network"]);
  const vlan = quantity(network["vlan"]);
  const headlamp = record(v["headlamp"]);
  const autoscaling = c.workerPools.find((p) => p.autoscaling !== undefined)?.autoscaling;
  return {
    ...base,
    from: clusterKey(c),
    name: entered.name,
    namespace: entered.namespace,
    className: c.class,
    version: entered.className === c.class ? entered.version : "",
    nodeOS: c.nodeOS === undefined ? "" : nodeOSKey(c.nodeOS),
    controlPlaneReplicas: c.controlPlane.desired,
    workerReplicas: autoscaling?.min ?? c.workers.desired,
    workerAutoscale: autoscaling !== undefined,
    workerMax: autoscaling?.max ?? base.workerMax,
    bridge: text(network["bridge"]) ?? "",
    vlan: vlan === undefined ? "" : String(vlan),
    prefix: quantity(network["prefix"]) ?? base.prefix,
    gateway: text(network["gateway"]) ?? "",
    dnsServers: (texts(network["dnsServers"]) ?? []).join(", "),
    loadBalancer: loadBalancerOf(v["loadBalancer"]).implementation,
    storage: text(v["storage"]) ?? "",
    place: placeOf(texts(v["allowedNodes"])),
    proxmoxHA: flag(v["proxmoxHA"]) !== false,
    haGroups: flag(record(v["ccm"])["haGroups"]) !== false,
    controlPlaneMachine: machineOf(v["controlPlaneMachine"], base.controlPlaneMachine),
    workerMachine: machineOf(v["workerMachine"], base.workerMachine),
    cni: text(v["cni"]) ?? "",
    headlamp: flag(headlamp["enabled"]) === true,
    headlampView: text(headlamp["access"]) === "view",
    monitoring: flag(record(v["monitoring"])["enabled"]) === true,
    logs: logsOf(v["logs"]),
    podSecurity: podSecurityOf(v["podSecurity"]),
    cis: flag(v["cis"]) === true,
    istio: flag(record(v["istio"])["enabled"]) === true,
    rancherRegister: flag(record(v["rancher"])["register"]) !== false,
    oidc: oidcProviders(v["oidc"]),
  };
}
