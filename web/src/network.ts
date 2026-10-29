// Checks of IPv4 addresses and networks the forms share, so the cluster wizard and the setup page
// refuse the same things the server does.

export const ipv4 = /^(?:25[0-5]|2[0-4]\d|1?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3}$/u;
export const cidr = /^(?:25[0-5]|2[0-4]\d|1?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3}\/(?:3[0-2]|[12]?\d)$/u;

export const addressBits = 32;
const octetValues = 256;
const octets = 4;
const allOnes = 0xff_ff_ff_ff;
// A /31 is a point-to-point link and a /32 one address: neither has a network or broadcast address.
export const pointToPoint = 31;

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

/** toInt reads a dotted IPv4 address as a number; undefined when it is none. */
export function toInt(address: string): number | undefined {
  if (!ipv4.test(address)) {
    return undefined;
  }
  return address.split(".").reduce((n, octet) => n * octetValues + Number(octet), 0);
}

/** fromInt writes a number as a dotted IPv4 address. */
export const fromInt = (n: number): string =>
  Array.from({ length: octets }, (_, i) => Math.floor(n / octetValues ** (octets - 1 - i)) % octetValues).join(".");

/** networkOf is the network an address with its prefix length is in, like 192.0.2.0/24. */
export function networkOf(address: string): string | undefined {
  const [base = "", bits] = address.split("/");
  const ip = toInt(base);
  if (!cidr.test(address) || ip === undefined) {
    return undefined;
  }
  return `${fromInt((ip & maskOf(Number(bits))) >>> 0)}/${bits}`;
}

/** maskOf is the network mask of a prefix length, as a number. */
export const maskOf = (prefix: number): number =>
  prefix >= 1 && prefix <= addressBits ? (allOnes << (addressBits - prefix)) >>> 0 : 0;

/** sameNetwork tells whether two addresses share the network of a prefix length. */
export const sameNetwork = (a: number, b: number, prefix: number): boolean =>
  (a & maskOf(prefix)) >>> 0 === (b & maskOf(prefix)) >>> 0;

// addressCount counts the node addresses a list gives: ranges a-b, networks a/n (without their
// network and broadcast addresses) and single addresses; undefined when an entry is none of these.
// Those outside the network of gateway/prefix make it undefined too.
export function addressCount(entries: string[], gateway: string, prefix: number): { count: number; bad?: string } {
  const g = toInt(gateway);
  const inNetwork = (a: number): boolean => g === undefined || maskOf(prefix) === 0 || sameNetwork(a, g, prefix);
  let count = 0;
  for (const e of entries) {
    const [from = "", to] = e.split("-");
    if (to !== undefined) {
      const a = toInt(from.trim());
      const b = toInt(to.trim());
      if (a === undefined || b === undefined || b < a || !inNetwork(a) || !inNetwork(b)) {
        return { count, bad: e };
      }
      count += b - a + 1;
    } else if (e.includes("/")) {
      const [base = "", bits] = e.split("/");
      const a = toInt(base);
      if (!cidr.test(e) || a === undefined || !inNetwork(a)) {
        return { count, bad: e };
      }
      const n = Number(bits);
      count += n >= pointToPoint ? 2 ** (addressBits - n) : 2 ** (addressBits - n) - 2;
    } else {
      const a = toInt(e);
      if (a === undefined || !inNetwork(a)) {
        return { count, bad: e };
      }
      count += 1;
    }
  }
  return { count };
}

/** isNetwork tells whether a CIDR names a network: no host bits set. */
export const isNetwork = (v: string): boolean => {
  const [base, bits] = v.split("/");
  const n = Number(bits);
  const a = toInt(base ?? "");
  return a !== undefined && (n === addressBits || a % 2 ** (addressBits - n) === 0);
};

/** spanOf is the first and last address an address entry holds, as numbers; undefined when it is none. */
export function spanOf(entry: string): [number, number] | undefined {
  const [from = "", to] = entry.split("-");
  if (to !== undefined) {
    const a = toInt(from.trim());
    const b = toInt(to.trim());
    return a === undefined || b === undefined || b < a ? undefined : [a, b];
  }
  if (entry.includes("/")) {
    const [base = "", bits] = entry.split("/");
    const a = toInt(base);
    if (!cidr.test(entry) || a === undefined) {
      return undefined;
    }
    const first = (a & maskOf(Number(bits))) >>> 0;
    return [first, first + 2 ** (addressBits - Number(bits)) - 1];
  }
  const a = toInt(entry);
  return a === undefined ? undefined : [a, a];
}

/** overlapping is the first of the entries that shares an address with one of the others. */
export function overlapping(entries: string[], others: string[]): string | undefined {
  const spans = others.map((o) => spanOf(o)).filter((s) => s !== undefined);
  return entries.find((e) => {
    const s = spanOf(e);
    return s !== undefined && spans.some(([a, b]) => s[0] <= b && a <= s[1]);
  });
}

/** inList tells whether an address falls in one of the node address entries. */
export const inList = (address: string, entries: string[]): boolean =>
  toInt(address) !== undefined && overlapping([address], entries) !== undefined;

/** isIP tells whether a server address is IPv4 or looks like IPv6. */
export const isIP = (v: string): boolean => ipv4.test(v) || (/^[0-9a-f:.]+$/iu.test(v) && v.includes(":"));

/** rangeProblem says what is wrong with a pod or service range, if anything. */
export const rangeProblem = (v: string): string | undefined =>
  cidr.test(v) && isNetwork(v) ? undefined : "Enter a network like 10.244.0.0/16, without host bits";
