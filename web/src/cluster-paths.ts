import type { Attention, Cluster } from "./types";
import { filled } from "./ui";

/** Intent is what a cluster's address asks its page to start, in its do parameter. */
export type Intent = "scale" | "upgrade" | "kubeconfig" | "talosconfig" | "delete";

export interface ClusterAction {
  intent: Intent;
  label: string;
}

/** clusterActions are what the user may start on a cluster from anywhere, as its page offers them. */
export function clusterActions(c: Cluster): ClusterAction[] {
  const can = c.allowed.change && !c.deleting;
  const actions: ClusterAction[] = [];
  if (can) {
    actions.push({ intent: "scale", label: "Scale" }, { intent: "upgrade", label: "Upgrade" });
  }
  if (c.allowed.kubeconfig && c.available) {
    actions.push({ intent: "kubeconfig", label: "Download kubeconfig" });
  }
  if (c.allowed.kubeconfig && c.distribution === "talos") {
    actions.push({ intent: "talosconfig", label: "Download talosconfig" });
  }
  if (can) {
    actions.push({ intent: "delete", label: "Delete" });
  }
  return actions;
}

/** The views of a cluster's page, as the last part of its address. The summary has none. */
export const views = [
  { path: "", label: "Summary" },
  { path: "health", label: "Health" },
  { path: "machines", label: "Machines" },
  { path: "backups", label: "Backups" },
  { path: "options", label: "Options" },
  { path: "activity", label: "Activity" },
] as const;

export type View = (typeof views)[number]["path"];

/** clusterPath is the address of a cluster's page, or of one of its views. */
export const clusterPath = (namespace: string, name: string, view: View = ""): string =>
  `/clusters/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}${view ? `/${view}` : ""}`;

/**
 * attentionFix is where something a cluster's health found is acted on: the machine's row for one
 * about a machine, the backups view for its backups. Undefined where the health page is the place.
 */
export function attentionFix(
  c: { namespace: string; name: string },
  a: Attention,
): { view: View; to: string } | undefined {
  if (filled(a.machine)) {
    return {
      view: "machines",
      to: `${clusterPath(c.namespace, c.name, "machines")}?machine=${encodeURIComponent(a.machine)}`,
    };
  }
  return a.section === "backups" ? { view: "backups", to: clusterPath(c.namespace, c.name, "backups") } : undefined;
}

/** intentPath is a cluster's page asked to start an action. */
export const intentPath = (c: Cluster, intent: Intent): string => `${clusterPath(c.namespace, c.name)}?do=${intent}`;

/**
 * headlampURL is a cluster's page in the shared Headlamp, served on its own port of this host.
 * A cluster's goes through the gate's opening of it, which signs in again a session that signed
 * out of the cluster in Headlamp, and leads on to the cluster's page there.
 */
export function headlampURL(port: number, cluster?: string): string {
  const host = location.hostname.includes(":") ? `[${location.hostname}]` : location.hostname;
  return `https://${host}:${port}/${filled(cluster) ? `kubehenge/open/${encodeURIComponent(cluster)}` : ""}`;
}
