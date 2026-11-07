import type { NodeOS, NodeOSChoice, NodeOSRef } from "./types";
import { filled } from "./ui";

/** nodeOSKey is a node OS as one word, such as almalinux-10 or almalinux-10-fips, as template names and requests carry it. */
export const nodeOSKey = (o: NodeOSRef): string => `${o.family}-${o.release}${o.fips === true ? "-fips" : ""}`;

/** nodeOSRef is what a cluster's nodeOS variable holds of a node OS. */
export const nodeOSRef = (o: NodeOSRef): NodeOSRef =>
  o.fips === true ? { family: o.family, release: o.release, fips: true } : { family: o.family, release: o.release };

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

/** fipsDistributions run on a node OS in FIPS mode. The class's own rules refuse it to the others. */
const fipsDistributions = new Set(["rke2"]);

/** forDistribution are the node OSes templates of the distribution are built of. */
export const forDistribution = <T extends NodeOS>(oses: T[], distribution: string): T[] =>
  fipsDistributions.has(distribution) ? oses : oses.filter((o) => o.fips !== true);

/** fipsCNI is the one pod network RKE2 builds for FIPS mode. */
export const fipsCNI = "canal";

/** baseTitle is the node OS's release as people read it, without FIPS mode, such as AlmaLinux 10. */
export function baseTitle(o: NodeOS, oses: NodeOS[] | undefined): string {
  return o.fips === true ? (findNodeOS(oses, nodeOSKey(nodeOSRef({ ...o, fips: false })))?.title ?? o.title) : o.title;
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
