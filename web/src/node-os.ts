import type { NodeOS, NodeOSChoice, NodeOSRef } from "./types";
import { filled } from "./ui";

/** nodeOSKey is a node OS as one word, such as almalinux-10, as template names and requests carry it. */
export const nodeOSKey = (o: NodeOSRef): string => `${o.family}-${o.release}`;

/** nodeOSRef is what a cluster's nodeOS variable holds of a node OS. */
export const nodeOSRef = (o: NodeOSRef): NodeOSRef => ({ family: o.family, release: o.release });

/** choosable says whether a node OS can be chosen: it is released. */
export const choosable = (o: NodeOS): boolean => !filled(o.unreleased);

/** defaultNodeOS is the one a list marks as the default, else its first that can be chosen. */
export function defaultNodeOS<T extends NodeOS>(oses: T[] | undefined): T | undefined {
  return oses?.find((o) => o.default === true) ?? oses?.find((o) => choosable(o));
}

/** findNodeOS is the node OS of a list that a key such as almalinux-10 names. */
export function findNodeOS<T extends NodeOS>(oses: T[] | undefined, key: string): T | undefined {
  return oses?.find((o) => nodeOSKey(o) === key);
}

const macNames: Record<string, string> = { selinux: "SELinux", apparmor: "AppArmor" };

/** macName is a mandatory access control as its project writes it. */
export const macName = (mac: string): string => macNames[mac] ?? mac;

/**
 * cpuNote says what a node OS asks of the Proxmox VE nodes' CPUs beyond what the default one asks,
 * since its VMs start only on nodes whose CPUs offer its CPU type.
 */
export function cpuNote(o: NodeOS | undefined, oses: NodeOS[] | undefined): string | undefined {
  const usual = defaultNodeOS(oses)?.cpuType;
  if (o === undefined || usual === undefined || o.cpuType === usual) {
    return undefined;
  }
  return `Needs Proxmox VE nodes with ${o.cpuType} CPUs`;
}

/** nodeOSLabel names a node OS among the choices, with why it cannot be chosen or built on yet. */
export function nodeOSLabel(o: NodeOSChoice): string {
  if (filled(o.unreleased)) {
    return `${o.title}: ${o.unreleased}`;
  }
  return filled(o.noTemplate) ? `${o.title}, no template yet` : o.title;
}
