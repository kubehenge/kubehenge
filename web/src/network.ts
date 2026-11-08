// Checks of IPv4 and IPv6 addresses and networks the forms share, so the cluster wizard and the
// setup page refuse the same things the server does.

import type { Bridge } from "./types";

export const ipv4 = /^(?:25[0-5]|2[0-4]\d|1?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3}$/u;

// The class's network.searchDomains: a DNS name of labels up to 63 characters, six at most because
// glibc's resolver reads no more.
const dnsName = /^(?:[A-Za-z\d](?:[-A-Za-z\d]{0,61}[A-Za-z\d])?\.)*[A-Za-z\d](?:[-A-Za-z\d]{0,61}[A-Za-z\d])?$/u;
export const maxSearchDomains = 6;
const maxDNSNameLength = 253;

/** isDNSName tells whether text is a domain name without a trailing dot. */
export const isDNSName = (s: string): boolean => s.length <= maxDNSNameLength && dnsName.test(s);

/** searchDomainsProblem is what the server's schema would refuse in the search domains entered, if anything. */
export function searchDomainsProblem(domains: string[]): string | undefined {
  if (domains.length > maxSearchDomains) {
    return `At most ${maxSearchDomains} domains`;
  }
  return domains.every((d) => isDNSName(d)) ? undefined : "Enter domain names like corp.example";
}

/** list splits text into entries at commas and white space. */
export const list = (s: string): string[] =>
  s
    .split(/[\s,]+/u)
    .map((x) => x.trim())
    .filter(Boolean);

/** whole tells whether n is a whole number from from to to. */
export const whole = (n: number, from: number, to = Infinity): boolean => Number.isInteger(n) && n >= from && n <= to;

// Tags 0 and 4095 are reserved.
export const maxVLAN = 4094;

/** isVLAN tells whether text is a VLAN tag. */
export const isVLAN = (v: string): boolean => whole(Number(v), 1, maxVLAN);

// Proxmox VE takes an MTU from 576, but the nodes' IPv6 does not work below 1280.
export const minMTU = 1280;
export const maxMTU = 65_520;
export const maxQueues = 64;

/** isMTU tells whether text is an interface MTU the nodes take. */
export const isMTU = (v: string): boolean => whole(Number(v), minMTU, maxMTU);

/** isQueues tells whether text is a number of queues for the nodes' interface. */
export const isQueues = (v: string): boolean => whole(Number(v), 1, maxQueues);

/** bridgeRef is how the settings name a bridge: a VNet as zone/vnet, a node's bridge by its name. */
export const bridgeRef = (b: Pick<Bridge, "bridge" | "zone">): string =>
  b.zone === undefined || b.zone === "" ? b.bridge : `${b.zone}/${b.bridge}`;

/** bridgeLabel is a bridge as listed: a node's bridge with its comment, a VNet with its alias and zone. */
export function bridgeLabel(b: Pick<Bridge, "bridge" | "zone" | "alias" | "comment">): string {
  const vnet = b.zone !== undefined && b.zone !== "";
  const note = vnet ? b.alias : b.comment;
  const named = note === undefined || note === "" ? b.bridge : `${b.bridge} (${note})`;
  return vnet ? `${named}, zone ${b.zone}` : named;
}

/** The address families of a cluster's nodes, by the names its ipFamilies variable takes. */
export type Family = "IPv4" | "IPv6";

/** An address of either family as a number, so networks and ranges are plain arithmetic. */
export interface Address {
  family: Family;
  value: bigint;
}

/** A run of addresses of one family, first to last. */
export interface Span {
  family: Family;
  first: bigint;
  last: bigint;
}

const octetBits = 8n;
const groupBits = 16n;
const v6Groups = 8;
const v4Octets = 4;
const maxGroupDigits = 4;
const hexRadix = 16;

interface Traits {
  bits: number;
  pointToPoint: number;
  reserved: bigint;
}

// A /31 and a /127 are point-to-point links, with no address reserved. Any larger IPv4 network
// reserves its network and broadcast addresses, an IPv6 one its subnet-router anycast address.
const traits: Record<Family, Traits> = {
  IPv4: { bits: 32, pointToPoint: 31, reserved: 2n },
  IPv6: { bits: 128, pointToPoint: 127, reserved: 1n },
};

/** bitsOf is the length of an address of the family in bits, also the longest prefix length. */
export const bitsOf = (family: Family): number => traits[family].bits;

/** pointToPointOf is the family's prefix length of a point-to-point link, /31 or /127. */
export const pointToPointOf = (family: Family): number => traits[family].pointToPoint;

const parseIPv4 = (s: string): bigint | undefined => {
  if (!ipv4.test(s)) {
    return undefined;
  }
  return s.split(".").reduce((n, octet) => (n << octetBits) + BigInt(octet), 0n);
};

// groupsOf reads the colon-separated groups of an IPv6 address half, the last holding a dotted IPv4
// address as two groups. Undefined when one is malformed.
function groupsOf(half: string, last: boolean): bigint[] | undefined {
  if (half === "") {
    return [];
  }
  const parts = half.split(":");
  const out: bigint[] = [];
  for (const [i, part] of parts.entries()) {
    if (last && i === parts.length - 1 && part.includes(".")) {
      const v = parseIPv4(part);
      if (v === undefined) {
        return undefined;
      }
      out.push(v >> groupBits, v & 0xff_ffn);
    } else if (new RegExp(`^[0-9a-f]{1,${maxGroupDigits}}$`, "iu").test(part)) {
      out.push(BigInt(Number.parseInt(part, hexRadix)));
    } else {
      return undefined;
    }
  }
  return out;
}

const parseIPv6 = (s: string): bigint | undefined => {
  if (!s.includes(":") || s.includes(":::")) {
    return undefined;
  }
  const halves = s.split("::");
  if (halves.length > 2) {
    return undefined;
  }
  const [head = "", tail] = halves;
  const front = groupsOf(head, tail === undefined);
  const back = tail === undefined ? [] : groupsOf(tail, true);
  if (front === undefined || back === undefined) {
    return undefined;
  }
  const missing = v6Groups - front.length - back.length;
  if (tail === undefined ? missing !== 0 : missing < 1) {
    return undefined;
  }
  const groups = [...front, ...Array.from({ length: tail === undefined ? 0 : missing }, () => 0n), ...back];
  return groups.reduce((n, g) => (n << groupBits) + g, 0n);
};

/** parseAddress reads an IPv4 or IPv6 address. Undefined when text is neither. */
export function parseAddress(s: string): Address | undefined {
  const v4 = parseIPv4(s);
  if (v4 !== undefined) {
    return { family: "IPv4", value: v4 };
  }
  const v6 = parseIPv6(s);
  return v6 === undefined ? undefined : { family: "IPv6", value: v6 };
}

/** familyOf is the family of an address. Undefined when text is none. */
export const familyOf = (s: string): Family | undefined => parseAddress(s)?.family;

/** isIPv6 tells whether text is an IPv6 address. */
export const isIPv6 = (s: string): boolean => familyOf(s) === "IPv6";

/** isIPv4 tells whether text is an IPv4 address. */
export const isIPv4 = (s: string): boolean => ipv4.test(s);

// The longest run of zero groups, at least two, is written as "::" (RFC 5952).
function format6(value: bigint): string {
  const groups = Array.from({ length: v6Groups }, (_, i) =>
    Number((value >> (groupBits * BigInt(v6Groups - 1 - i))) & 0xff_ffn),
  );
  let best = { at: -1, len: 0 };
  let run = { at: -1, len: 0 };
  for (const [i, g] of groups.entries()) {
    run = g === 0 ? { at: run.len === 0 ? i : run.at, len: run.len + 1 } : { at: -1, len: 0 };
    if (run.len > best.len) {
      best = run;
    }
  }
  const hex = groups.map((g) => g.toString(hexRadix));
  if (best.len < 2) {
    return hex.join(":");
  }
  return `${hex.slice(0, best.at).join(":")}::${hex.slice(best.at + best.len).join(":")}`;
}

/** formatAddress writes an address as text: dotted for IPv4, compressed for IPv6. */
export const formatAddress = (a: Address): string =>
  a.family === "IPv4"
    ? Array.from({ length: v4Octets }, (_, i) =>
        Number((a.value >> (octetBits * BigInt(v4Octets - 1 - i))) & 0xffn),
      ).join(".")
    : format6(a.value);

/** A network address with its prefix length, like 192.0.2.11/24 or 2001:db8::11/64. */
export interface Prefixed {
  address: Address;
  bits: number;
}

/** parseCidr reads an address with its prefix length. Undefined when text is none. */
export function parseCidr(s: string): Prefixed | undefined {
  const [base = "", bits, ...rest] = s.split("/");
  const address = parseAddress(base);
  if (address === undefined || bits === undefined || rest.length > 0 || !/^\d{1,3}$/u.test(bits)) {
    return undefined;
  }
  return Number(bits) <= bitsOf(address.family) ? { address, bits: Number(bits) } : undefined;
}

/** isCidr tells whether text is an address with a prefix length, of either family. */
export const isCidr = (s: string): boolean => parseCidr(s) !== undefined;

/** hostBits is how many low bits of an address the prefix length leaves to hosts. */
const hostBits = (family: Family, prefix: number): bigint => BigInt(bitsOf(family) - prefix);

/** maskOf is the network mask of a prefix length. */
export const maskOf = (family: Family, prefix: number): bigint =>
  ((1n << BigInt(prefix)) - 1n) << hostBits(family, prefix);

/** firstOf is the network address of an address with a prefix length. */
export const firstOf = (a: Address, prefix: number): bigint => a.value & maskOf(a.family, prefix);

/** networkOf is the network an address with its prefix length is in, like 192.0.2.0/24. */
export function networkOf(address: string): string | undefined {
  const c = parseCidr(address);
  if (c === undefined) {
    return undefined;
  }
  return `${formatAddress({ family: c.address.family, value: firstOf(c.address, c.bits) })}/${c.bits}`;
}

/** addressIn is the address that many past a network's own, or undefined when the network is smaller. */
export function addressIn(network: string, offset: number): string | undefined {
  const c = parseCidr(network);
  if (c === undefined) {
    return undefined;
  }
  const size = 1n << hostBits(c.address.family, c.bits);
  const wanted = BigInt(offset);
  return wanted >= size - 1n ? undefined : formatAddress({ family: c.address.family, value: c.address.value + wanted });
}

/** sameNetwork tells whether two addresses of one family share the network of a prefix length. */
export const sameNetwork = (a: Address, b: Address, prefix: number): boolean =>
  a.family === b.family && firstOf(a, prefix) === firstOf(b, prefix);

// fe80::/10, where IPv6 link-local addresses are.
const linkLocal: Address = { family: "IPv6", value: 0xfe_80_00_00_00_00_00_00_00_00_00_00_00_00_00_00n };
const linkLocalBits = 10;

/**
 * gatewayReaches tells whether a gateway is on the network of an address with a prefix length. An
 * IPv6 gateway is often a link-local address, which no prefix holds.
 */
export const gatewayReaches = (a: Address, prefix: number, gateway: Address): boolean =>
  sameNetwork(a, gateway, prefix) || (a.family === "IPv6" && sameNetwork(gateway, linkLocal, linkLocalBits));

/** spanOf is the first and last address an address entry holds. Undefined when it is none. */
export function spanOf(entry: string): Span | undefined {
  const [from = "", to, ...rest] = entry.split("-");
  if (to !== undefined) {
    const a = parseAddress(from.trim());
    const b = parseAddress(to.trim());
    return rest.length > 0 || a === undefined || b === undefined || a.family !== b.family || b.value < a.value
      ? undefined
      : { family: a.family, first: a.value, last: b.value };
  }
  if (entry.includes("/")) {
    const c = parseCidr(entry);
    if (c === undefined) {
      return undefined;
    }
    const first = firstOf(c.address, c.bits);
    return { family: c.address.family, first, last: first + (1n << hostBits(c.address.family, c.bits)) - 1n };
  }
  const a = parseAddress(entry);
  return a === undefined ? undefined : { family: a.family, first: a.value, last: a.value };
}

/** A node network as the forms hold it: its gateway and prefix length, as typed. */
export interface NodeNetwork {
  gateway: string;
  prefix: number;
}

// How many addresses a network of a prefix length gives nodes.
function networkSize(family: Family, bits: number): number {
  const all = 1n << hostBits(family, bits);
  const { pointToPoint, reserved } = traits[family];
  return Number(all - (bits >= pointToPoint ? 0n : reserved));
}

/** What a list of node addresses gives: how many, or the first entry that is not one. */
export interface Counted {
  count: number;
  bad?: string;
}

/**
 * addressCountIn counts the node addresses a list gives: ranges a-b, networks a/n and single
 * addresses. An entry that is none of these, or outside every network of the gateways (a prefix of
 * 0 holds none to), is returned as bad.
 */
export function addressCountIn(entries: string[], networks: NodeNetwork[]): Counted {
  const known = networks.map((n) => ({ g: parseAddress(n.gateway), prefix: n.prefix }));
  const inNetwork = (first: Address): boolean =>
    known.every((n) => n.g === undefined) ||
    known.some((n) => n.g === undefined || n.prefix === 0 || sameNetwork(first, n.g, n.prefix));
  let count = 0;
  for (const e of entries) {
    const span = spanOf(e);
    if (
      span === undefined ||
      !inNetwork({ family: span.family, value: span.first }) ||
      !inNetwork({ family: span.family, value: span.last })
    ) {
      return { count, bad: e };
    }
    count +=
      e.includes("/") && !e.includes("-")
        ? networkSize(span.family, parseCidr(e)?.bits ?? 0)
        : Number(span.last - span.first) + 1;
  }
  return { count };
}

/** addressCount is addressCountIn for the one network of a gateway and its prefix length. */
export const addressCount = (entries: string[], gateway: string, prefix: number): Counted =>
  addressCountIn(entries, [{ gateway, prefix }]);

/** isNetwork tells whether a CIDR names a network: no host bits set. */
export const isNetwork = (v: string): boolean => {
  const c = parseCidr(v);
  return c !== undefined && firstOf(c.address, c.bits) === c.address.value;
};

/** overlapping is the first of the entries that shares an address with one of the others. */
export function overlapping(entries: string[], others: string[]): string | undefined {
  const spans = others.map((o) => spanOf(o)).filter((s) => s !== undefined);
  return entries.find((e) => {
    const s = spanOf(e);
    return s !== undefined && spans.some((o) => o.family === s.family && s.first <= o.last && o.first <= s.last);
  });
}

/** inList tells whether an address falls in one of the node address entries. */
export const inList = (address: string, entries: string[]): boolean =>
  parseAddress(address) !== undefined && overlapping([address], entries) !== undefined;

/** isIP tells whether a server address is an IPv4 or IPv6 address. */
export const isIP = (v: string): boolean => parseAddress(v) !== undefined;

/** maxTimeServers is how many NTP servers a cluster takes. */
export const maxTimeServers = 8;
const longestHostName = 253;
const hostName = /^(?!-)[a-z0-9-]{1,63}(?<!-)(?:\.(?!-)[a-z0-9-]{1,63}(?<!-))*\.?$/iu;

/** timeServersProblem says what is wrong with a list of NTP servers, or nothing. */
export function timeServersProblem(servers: string[]): string | undefined {
  if (servers.length > maxTimeServers) {
    return `Enter at most ${maxTimeServers} time servers`;
  }
  const bad = servers.find((s) => !isIP(s) && !(hostName.test(s) && s.length <= longestHostName));
  if (bad !== undefined) {
    return `${bad} is not a host name or IP address`;
  }
  const twice = servers.find((s, i) => servers.indexOf(s) !== i);
  return twice === undefined ? undefined : `${twice} is listed twice`;
}

/** rangeProblem says what is wrong with a pod or service range, if anything. */
export function rangeProblem(v: string): string | undefined {
  if (isNetwork(v)) {
    return undefined;
  }
  return isIPv6(v.split("/")[0] ?? "")
    ? "Enter a network like fd5e:4b1c:9a20::/56, without host bits"
    : "Enter a network like 10.244.0.0/16, without host bits";
}

/** The address families a cluster can have, the first being the primary one. */
export type FamilyChoice = "ipv4" | "ipv4-ipv6" | "ipv6-ipv4" | "ipv6";

/** familiesOf lists a choice's families, the primary first. */
export const familiesOf = (choice: FamilyChoice): readonly Family[] => familyOrder[choice];

const familyOrder: Record<FamilyChoice, readonly Family[]> = {
  ipv4: ["IPv4"],
  "ipv4-ipv6": ["IPv4", "IPv6"],
  "ipv6-ipv4": ["IPv6", "IPv4"],
  ipv6: ["IPv6"],
};

/** familyNames writes a choice as the ipFamilies variable. */
export const familyNames = (choice: FamilyChoice): Family[] => [...familiesOf(choice)];

/** choiceOf reads the ipFamilies variable back into a choice. Anything unreadable is IPv4. */
export function choiceOf(names: unknown): FamilyChoice {
  const text = (Array.isArray(names) ? (names as unknown[]) : []).join(",");
  return familyChoices.find((c) => familyOrder[c].join(",") === text) ?? "ipv4";
}

/** The family choices in the order a select offers them. */
export const familyChoices: readonly FamilyChoice[] = ["ipv4", "ipv4-ipv6", "ipv6-ipv4", "ipv6"];

/** familyChoiceOf reads a select's value back into a choice. */
export const familyChoiceOf = (v: string): FamilyChoice => familyChoices.find((c) => c === v) ?? "ipv4";

/** How a choice of families reads on a page. */
export const familyLabels: Record<FamilyChoice, string> = {
  ipv4: "IPv4",
  "ipv4-ipv6": "IPv4 and IPv6",
  "ipv6-ipv4": "IPv6 and IPv4",
  ipv6: "IPv6",
};

/**
 * familyFieldLabel is the label of one family's field, such as "Node addresses". Where both
 * families show, each names its family ("IPv4 node addresses"). With one, the plain label stands.
 */
export function familyFieldLabel(label: string, family: Family, shown: number): string {
  return shown > 1 ? `${family} ${label.charAt(0).toLowerCase()}${label.slice(1)}` : label;
}

/** reservedAddresses are the addresses of a gateway's network no node takes: its own and, in IPv4, its broadcast address. A point-to-point link has none. */
export function reservedAddresses(gateway: Address, prefix: number): bigint[] {
  if (prefix < 1 || prefix >= traits[gateway.family].pointToPoint) {
    return [];
  }
  const own = firstOf(gateway, prefix);
  return gateway.family === "IPv4" ? [own, own + (1n << hostBits(gateway.family, prefix)) - 1n] : [own];
}

/** hasFamily tells whether a choice includes a family. */
export const hasFamily = (choice: FamilyChoice, family: Family): boolean => familiesOf(choice).includes(family);

/** A router id is a dotted 32-bit number, like an IPv4 address, and never 0.0.0.0. */
export const isRouterID = (v: string): boolean => ipv4.test(v) && v !== "0.0.0.0";
