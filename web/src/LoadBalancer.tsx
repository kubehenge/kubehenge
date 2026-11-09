import { useState, type JSX, type ReactNode } from "react";

import { CredentialField } from "./CredentialField";
import {
  addressCountIn,
  choiceOf,
  familiesOf,
  hasFamily,
  isRouterID,
  list,
  overlapping,
  parseAddress,
  reservedAddresses,
  spanOf,
  type FamilyChoice,
  type NodeNetwork,
} from "./network";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, useChange } from "./ui";
import { flag, quantity, record, text, texts } from "./values";

/** The load balancers a cluster chooses from, by their names in its loadBalancer variable; "" is none. */
export type Implementation = "" | "metallb" | "kube-vip";

// k3s brings servicelb, which answers for each Service on every node's own address until the
// cluster has a load balancer of its own.
const noneText = (distribution: string | undefined): string => (distribution === "k3s" ? "k3s's own" : "None");

const implementationOptions = (distribution: string | undefined): { value: Implementation; label: string }[] => [
  { value: "", label: noneText(distribution) },
  { value: "metallb", label: "MetalLB (recommended)" },
  { value: "kube-vip", label: "kube-vip" },
];

/** How a cluster's load balancer announces addresses: ARP on the nodes' network, or BGP to routers. */
export type Mode = "l2" | "bgp";

const modeOptions: { value: Mode; label: string }[] = [
  { value: "l2", label: "Layer 2 (ARP)" },
  { value: "bgp", label: "BGP" },
];

/** The largest 4-byte autonomous system number. */
export const maxASN = 4_294_967_295;

/** A cluster's loadBalancer variable, with none chosen while it is off. The ASN and routers are as typed. */
export interface LoadBalancer {
  implementation: Implementation;
  addresses: string[];
  mode: Mode;
  asn: string;
  peers: string;
  /** The router id every node gives, empty for each node's own IPv4 address. */
  routerID: string;
  /** Whether the sessions carry the password in the cluster's bgp credential. */
  authenticated: boolean;
}

export interface Router {
  address: string;
  asn: number;
}

const isASN = (n: number): boolean => Number.isInteger(n) && n >= 1 && n <= maxASN;

/** routersOf reads routers typed one per line, or comma-separated, as "address ASN". Undefined if one is malformed. */
export function routersOf(typed: string): Router[] | undefined {
  const routers: Router[] = [];
  for (const entry of typed.split(/[\n,]+/u).filter((e) => e.trim() !== "")) {
    const [address = "", asn, ...rest] = entry.trim().split(/\s+/u);
    if (parseAddress(address) === undefined || asn === undefined || rest.length > 0 || !isASN(Number(asn))) {
      return undefined;
    }
    routers.push({ address, asn: Number(asn) });
  }
  return routers;
}

export function loadBalancerOf(v: unknown): LoadBalancer {
  const r = record(v);
  const bgp = record(r["bgp"]);
  const peers = Array.isArray(bgp["peers"]) ? (bgp["peers"] as unknown[]) : [];
  const held = {
    addresses: texts(r["addresses"]) ?? [],
    mode: text(r["mode"]) === "bgp" ? ("bgp" as const) : ("l2" as const),
    asn: String(quantity(bgp["asn"]) ?? ""),
    peers: peers.map((p) => `${text(record(p)["address"]) ?? ""} ${quantity(record(p)["asn"]) ?? ""}`).join("\n"),
    routerID: text(bgp["routerID"]) ?? "",
    authenticated: flag(bgp["authenticated"]) === true,
  };
  if (flag(r["enabled"]) !== true) {
    return { ...held, implementation: "" };
  }
  return { ...held, implementation: text(r["implementation"]) === "kube-vip" ? "kube-vip" : "metallb" };
}

/** loadBalancerVariable is the variable a choice sets. Undefined for none, which leaves it out. */
export function loadBalancerVariable(lb: LoadBalancer): Record<string, unknown> | undefined {
  if (lb.implementation === "") {
    return undefined;
  }
  const variable = { enabled: true, implementation: lb.implementation, addresses: lb.addresses };
  if (lb.mode !== "bgp") {
    return variable;
  }
  const peers = routersOf(lb.peers) ?? [];
  return {
    ...variable,
    mode: "bgp",
    bgp: {
      asn: Number(lb.asn),
      peers,
      ...(lb.routerID === "" ? {} : { routerID: lb.routerID }),
      ...(lb.authenticated ? { authenticated: true } : {}),
    },
  };
}

/** loadBalancerText says in a line which load balancer a cluster runs, and on what addresses. */
export function loadBalancerText(lb: LoadBalancer, distribution: string | undefined): string {
  if (lb.implementation === "") {
    return noneText(distribution);
  }
  const signed = lb.authenticated ? ", with a password" : "";
  const bgp = lb.mode === "bgp" ? `, over BGP as AS ${lb.asn}${signed}` : "";
  return `${lb.implementation === "kube-vip" ? "kube-vip" : "MetalLB"}, on ${lb.addresses.join(", ")}${bgp}`;
}

const loadBalancerInfo = "Gives each LoadBalancer Service its own address.";

const addressesHint = (families: FamilyChoice): string =>
  hasFamily(families, "IPv4")
    ? "Free addresses on the nodes' network, like 10.0.0.100-10.0.0.120"
    : "Free addresses on the nodes' network, like 2001:db8::100-2001:db8::120";

const routedHint = (families: FamilyChoice): string =>
  hasFamily(families, "IPv4")
    ? "Addresses the routers send to the nodes, like 198.51.100.0/28"
    : "Addresses the routers send to the nodes, like 2001:db8:1::/120";

const modeInfo = "BGP also announces the API address, from the control planes. Not on Talos.";

const peersHint = "One per line, like 10.0.0.1 64512";

export const routerIDLabel = "Router ID";

/** The hint of the router id: what an empty one means, or that IPv6 alone needs one. */
export const routerIDHint = (families: FamilyChoice): string =>
  hasFamily(families, "IPv4") ? "Empty: each node's own IPv4 address" : "Required, like 192.0.2.1";

/** What the BGP password field is called and says about itself. */
export const passwordLabel = "BGP password";

export const passwordInfo = "The same password for every router. The cluster waits for it.";

/** What is wrong with the BGP settings, if anything, by field. */
export interface BGPProblems {
  asn?: string;
  peers?: string;
  routerID?: string;
}

/** What bgpProblems needs to know of the cluster: its families, which decide the router id and the routers. */
export interface BGPContext {
  families: FamilyChoice;
}

/** bgpProblems says what is wrong with a cluster's AS number, router id and routers. */
export function bgpProblems(lb: Pick<LoadBalancer, "asn" | "peers" | "routerID">, context: BGPContext): BGPProblems {
  const problems: BGPProblems = {};
  if (!isASN(Number(lb.asn))) {
    problems.asn = `Enter a number from 1 to ${maxASN}`;
  }
  const routers = routersOf(lb.peers);
  if (routers === undefined) {
    problems.peers = `Each line needs an ${context.families === "ipv4" ? "IPv4" : "IP"} address and an AS number from 1 to 4294967295`;
  } else if (routers.length === 0) {
    problems.peers = "Enter at least one router";
  } else if (new Set(routers.map((r) => r.address)).size < routers.length) {
    problems.peers = "A router is listed twice";
  } else {
    const own = familiesOf(context.families);
    const foreign = routers.find((r) => {
      const family = parseAddress(r.address)?.family;
      return family === undefined || !own.includes(family);
    });
    if (foreign !== undefined) {
      problems.peers = `${foreign.address} is not of the cluster's address families`;
    }
  }
  if (lb.routerID === "") {
    if (!hasFamily(context.families, "IPv4")) {
      problems.routerID = "IPv6 only needs a router ID like 192.0.2.1";
    }
  } else if (!isRouterID(lb.routerID)) {
    problems.routerID = "Enter a dotted ID like 192.0.2.1, not 0.0.0.0";
  }
  return problems;
}

/** The nodes' network a cluster's load-balancer addresses are held to. The IPv6 fields are for a cluster with IPv6. */
export interface NodesNetwork {
  families: FamilyChoice;
  gateway: string;
  prefix: number;
  gateway6: string;
  prefix6: number;
  addresses: string[];
  endpoint: string;
}

/** networksOf lists the gateway and prefix length of each of a network's families, the primary first. */
export function networksOf(
  n: Pick<NodesNetwork, "families" | "gateway" | "prefix" | "gateway6" | "prefix6">,
): NodeNetwork[] {
  return familiesOf(n.families).map((f) =>
    f === "IPv4" ? { gateway: n.gateway, prefix: n.prefix } : { gateway: n.gateway6, prefix: n.prefix6 },
  );
}

/** networkOf reads the nodes' network from a cluster's variables. */
export function networkOf(c: Cluster): NodesNetwork {
  const n = record(c.variables["network"]);
  const v6 = record(n["ipv6"]);
  return {
    families: choiceOf(n["ipFamilies"]),
    gateway: text(n["gateway"]) ?? "",
    prefix: quantity(n["prefix"]) ?? 0,
    gateway6: text(v6["gateway"]) ?? "",
    prefix6: quantity(v6["prefix"]) ?? 0,
    addresses: [...(texts(n["addresses"]) ?? []), ...(texts(v6["addresses"]) ?? [])],
    endpoint: text(c.variables["controlPlaneEndpoint"]) ?? "",
  };
}

// holdingReserved is the first entry holding an address of the nodes' networks that hosts there
// never answer for.
function holdingReserved(entries: string[], network: NodesNetwork): string | undefined {
  const reserved = networksOf(network).flatMap((net) => {
    const g = parseAddress(net.gateway);
    return g === undefined ? [] : reservedAddresses(g, net.prefix).map((at) => ({ family: g.family, at }));
  });
  return entries.find((e) => {
    const s = spanOf(e);
    return s !== undefined && reserved.some((r) => r.family === s.family && s.first <= r.at && r.at <= s.last);
  });
}

/**
 * addressesProblem says what is wrong with load-balancer addresses on a network, if anything. The
 * server also holds them to other clusters' addresses, which the browser cannot see.
 */
export function addressesProblem(entries: string[], network: NodesNetwork, routed = false): string | undefined {
  if (entries.length === 0) {
    return "Enter at least one address, range or network";
  }
  const own = familiesOf(network.families);
  const foreign = entries.find((e) => {
    const s = spanOf(e);
    return s !== undefined && !own.includes(s.family);
  });
  if (foreign !== undefined) {
    return `${foreign} is not of the cluster's address families`;
  }
  // Routed addresses may lie outside the nodes' networks, which a prefix length of 0 allows.
  const networks = networksOf(network).map((n) => ({ gateway: n.gateway, prefix: routed ? 0 : n.prefix }));
  const { bad } = addressCountIn(entries, networks);
  if (bad !== undefined) {
    return routed
      ? `${bad} is not an address, range or network`
      : `${bad} is not an address, range or network on the gateway's network`;
  }
  const reserved = holdingReserved(entries, network);
  if (reserved !== undefined) {
    return `${reserved} includes the network or broadcast address`;
  }
  const taken: [string, string[]][] = [
    ["the node addresses", network.addresses],
    ["the API address", [network.endpoint]],
    ["the gateway", networksOf(network).map((n) => n.gateway)],
  ];
  for (const [what, others] of taken) {
    const clash = overlapping(entries, others);
    if (clash !== undefined) {
      return `${clash} overlaps ${what}`;
    }
  }
  for (const [i, e] of entries.entries()) {
    if (overlapping([e], entries.slice(i + 1)) !== undefined) {
      return `${e} overlaps another entry`;
    }
  }
  return undefined;
}

/** What the load balancer's fields show: the addresses, ASN and routers as typed. */
export interface LoadBalancerValue {
  implementation: Implementation;
  addresses: string;
  mode: Mode;
  asn: string;
  peers: string;
  routerID: string;
  authenticated: boolean;
}

/** The problems to show beside the fields, once each has been touched. */
export interface LoadBalancerErrors {
  addresses?: string | undefined;
  asn?: string | undefined;
  peers?: string | undefined;
  routerID?: string | undefined;
}

/** The fields of a value that can change. */
export type LoadBalancerKey = keyof LoadBalancerValue;

/** bgpOffered tells whether a cluster may announce over BGP: with MetalLB, and not on Talos. */
export const bgpOffered = (distribution: string | undefined, implementation: Implementation): boolean =>
  implementation === "metallb" && distribution !== "talos";

/** LoadBalancerFields chooses the load balancer, how it announces and its addresses. */
export function LoadBalancerFields({
  distribution,
  families,
  value,
  errors,
  onChange,
  password,
}: {
  distribution: string | undefined;
  families: FamilyChoice;
  value: LoadBalancerValue;
  errors: LoadBalancerErrors;
  onChange: (patch: Partial<LoadBalancerValue>) => void;
  /** Where the routers' password is entered: a field in the wizard, the stored credential on a running cluster. */
  password: ReactNode;
}): JSX.Element {
  const options = implementationOptions(distribution);
  const bgp = value.mode === "bgp" && bgpOffered(distribution, value.implementation);
  return (
    <>
      <Field label="Load balancer" info={loadBalancerInfo}>
        {(id) => (
          <select
            id={id}
            value={value.implementation}
            onChange={(e) => onChange({ implementation: options.find((o) => o.value === e.target.value)?.value ?? "" })}
          >
            {options.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        )}
      </Field>
      {bgpOffered(distribution, value.implementation) && (
        <Field label="Announce with" info={modeInfo}>
          {(id) => (
            <select
              id={id}
              value={value.mode}
              onChange={(e) => onChange({ mode: modeOptions.find((o) => o.value === e.target.value)?.value ?? "l2" })}
            >
              {modeOptions.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          )}
        </Field>
      )}
      {value.implementation !== "" && (
        <Field
          label="Load-balancer addresses"
          hint={bgp ? routedHint(families) : addressesHint(families)}
          error={errors.addresses}
        >
          {(id) => (
            <textarea
              id={id}
              rows={2}
              value={value.addresses}
              onChange={(e) => onChange({ addresses: e.target.value })}
            />
          )}
        </Field>
      )}
      {bgp && (
        <>
          <Field label="Cluster AS number" error={errors.asn}>
            {(id) => (
              <input
                id={id}
                inputMode="numeric"
                value={value.asn}
                onChange={(e) => onChange({ asn: e.target.value.trim() })}
              />
            )}
          </Field>
          <Field label="Routers" hint={peersHint} error={errors.peers}>
            {(id) => (
              <textarea id={id} rows={2} value={value.peers} onChange={(e) => onChange({ peers: e.target.value })} />
            )}
          </Field>
          <Field label={routerIDLabel} hint={routerIDHint(families)} error={errors.routerID}>
            {(id) => (
              <input id={id} value={value.routerID} onChange={(e) => onChange({ routerID: e.target.value.trim() })} />
            )}
          </Field>
          <label className="check">
            <input
              type="checkbox"
              checked={value.authenticated}
              onChange={(e) => onChange({ authenticated: e.target.checked })}
            />{" "}
            Use a BGP password
          </label>
          {value.authenticated && password}
        </>
      )}
    </>
  );
}

/** loadBalancerProblems holds a value to the nodes' network, and in BGP to the routers' addresses instead. */
export function loadBalancerProblems(
  value: LoadBalancerValue,
  distribution: string | undefined,
  network: NodesNetwork,
): LoadBalancerErrors {
  if (value.implementation === "") {
    return {};
  }
  const bgp = value.mode === "bgp" && bgpOffered(distribution, value.implementation);
  return {
    addresses: addressesProblem(list(value.addresses), network, bgp),
    ...(bgp ? bgpProblems(value, { families: network.families }) : {}),
  };
}

/** loadBalancerOfValue is the settings a value stands for. A mode that is not offered stays layer 2. */
export const loadBalancerFrom = (value: LoadBalancerValue, distribution: string | undefined): LoadBalancer => ({
  ...value,
  addresses: list(value.addresses),
  mode: value.mode === "bgp" && bgpOffered(distribution, value.implementation) ? "bgp" : "l2",
});

export function LoadBalancerDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  // Switched off, the variable keeps what else it holds, the addresses among it, for switching on again.
  const held = record(c.variables["loadBalancer"]);
  const current = loadBalancerOf(held);
  const [value, setValue] = useState<LoadBalancerValue>({ ...current, addresses: current.addresses.join(", ") });
  const [touched, setTouched] = useState<Set<string>>(new Set());
  const { busy, error, send } = useChange(c, onClose);
  const next = loadBalancerFrom(value, c.distribution);
  const problems = loadBalancerProblems(value, c.distribution, networkOf(c));
  const invalid = Object.values(problems).some((p) => p !== undefined);
  const shown = (key: string, problem: string | undefined): string | undefined =>
    touched.has(key) ? problem : undefined;
  const changed = JSON.stringify(loadBalancerVariable(next)) !== JSON.stringify(loadBalancerVariable(current));
  return (
    <Dialog
      title="Edit load balancer"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { loadBalancer: loadBalancerVariable(next) ?? { ...held, enabled: false } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || invalid}>
            Save
          </button>
        </>
      }
    >
      <LoadBalancerFields
        distribution={c.distribution}
        families={networkOf(c).families}
        value={value}
        errors={{
          addresses: shown("addresses", problems.addresses),
          asn: shown("asn", problems.asn),
          peers: shown("peers", problems.peers),
          routerID: shown("routerID", problems.routerID),
        }}
        onChange={(patch) => {
          setValue((was) => ({ ...was, ...patch }));
          setTouched((was) => new Set([...was, ...Object.keys(patch)]));
        }}
        password={
          <CredentialField
            label={passwordLabel}
            hint={passwordInfo}
            purpose="bgp"
            cluster={{ namespace: c.namespace, name: c.name }}
            keys={[{ key: "password", label: "Password" }]}
          />
        }
      />
      <ErrorBox message={error} />
    </Dialog>
  );
}
