import { useState, type JSX } from "react";

import { addressBits, addressCount, list, maskOf, overlapping, pointToPoint, spanOf, toInt } from "./network";
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

/** A cluster's loadBalancer variable, with none chosen while it is off. */
export interface LoadBalancer {
  implementation: Implementation;
  addresses: string[];
}

export function loadBalancerOf(v: unknown): LoadBalancer {
  const r = record(v);
  const addresses = texts(r["addresses"]) ?? [];
  if (flag(r["enabled"]) !== true) {
    return { implementation: "", addresses };
  }
  return { implementation: text(r["implementation"]) === "kube-vip" ? "kube-vip" : "metallb", addresses };
}

/** loadBalancerVariable is the variable a choice sets; undefined for none, which leaves it out. */
export const loadBalancerVariable = (lb: LoadBalancer): Record<string, unknown> | undefined =>
  lb.implementation === "" ? undefined : { enabled: true, implementation: lb.implementation, addresses: lb.addresses };

/** loadBalancerText says in a line which load balancer a cluster runs, and on what addresses. */
export function loadBalancerText(lb: LoadBalancer, distribution: string | undefined): string {
  if (lb.implementation === "") {
    return noneText(distribution);
  }
  return `${lb.implementation === "kube-vip" ? "kube-vip" : "MetalLB"}, on ${lb.addresses.join(", ")}`;
}

const loadBalancerInfo = "Gives each LoadBalancer Service its own address.";

const addressesHint = "Free addresses on the nodes' network, like 10.0.0.100-10.0.0.120";

/** The nodes' network a cluster's load-balancer addresses are held to. */
export interface NodesNetwork {
  gateway: string;
  prefix: number;
  addresses: string[];
  endpoint: string;
}

/** networkOf reads the nodes' network from a cluster's variables. */
export function networkOf(c: Cluster): NodesNetwork {
  const n = record(c.variables["network"]);
  return {
    gateway: text(n["gateway"]) ?? "",
    prefix: quantity(n["prefix"]) ?? 0,
    addresses: texts(n["addresses"]) ?? [],
    endpoint: text(c.variables["controlPlaneEndpoint"]) ?? "",
  };
}

// holdingReserved is the first entry holding the network's own or its broadcast address, which
// hosts on the network never answer for; a /31 or /32 has neither.
function holdingReserved(entries: string[], network: NodesNetwork): string | undefined {
  const g = toInt(network.gateway);
  if (g === undefined || network.prefix < 1 || network.prefix >= pointToPoint) {
    return undefined;
  }
  const own = (g & maskOf(network.prefix)) >>> 0;
  const broadcast = own + 2 ** (addressBits - network.prefix) - 1;
  return entries.find((e) => {
    const s = spanOf(e);
    return s !== undefined && [own, broadcast].some((a) => s[0] <= a && a <= s[1]);
  });
}

/**
 * addressesProblem says what is wrong with load-balancer addresses on a network, if anything. The
 * server also holds them to other clusters' addresses, which the browser cannot see.
 */
export function addressesProblem(entries: string[], network: NodesNetwork): string | undefined {
  if (entries.length === 0) {
    return "Enter at least one address, range or network";
  }
  const { bad } = addressCount(entries, network.gateway, network.prefix);
  if (bad !== undefined) {
    return `${bad} is not an address, range or network on the gateway's network`;
  }
  const reserved = holdingReserved(entries, network);
  if (reserved !== undefined) {
    return `${reserved} includes the network or broadcast address`;
  }
  const taken: [string, string[]][] = [
    ["the node addresses", network.addresses],
    ["the API address", [network.endpoint]],
    ["the gateway", [network.gateway]],
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

/** LoadBalancerFields chooses the load balancer and its addresses. */
export function LoadBalancerFields({
  distribution,
  implementation,
  addresses,
  error,
  onImplementation,
  onAddresses,
}: {
  distribution: string | undefined;
  implementation: Implementation;
  addresses: string;
  error?: string | undefined;
  onImplementation: (i: Implementation) => void;
  onAddresses: (a: string) => void;
}): JSX.Element {
  const options = implementationOptions(distribution);
  return (
    <>
      <Field label="Load balancer" info={loadBalancerInfo}>
        {(id) => (
          <select
            id={id}
            value={implementation}
            onChange={(e) => onImplementation(options.find((o) => o.value === e.target.value)?.value ?? "")}
          >
            {options.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        )}
      </Field>
      {implementation !== "" && (
        <Field label="Load-balancer addresses" hint={addressesHint} error={error}>
          {(id) => <textarea id={id} rows={2} value={addresses} onChange={(e) => onAddresses(e.target.value)} />}
        </Field>
      )}
    </>
  );
}

export function LoadBalancerDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  // Switched off, the variable keeps what else it holds, the addresses among it, for switching on again.
  const held = record(c.variables["loadBalancer"]);
  const current = loadBalancerOf(held);
  const [implementation, setImplementation] = useState(current.implementation);
  const [addresses, setAddresses] = useState(current.addresses.join(", "));
  const [typed, setTyped] = useState(false);
  const { busy, error, send } = useChange(c, onClose);
  const next: LoadBalancer = { implementation, addresses: list(addresses) };
  const problem = implementation === "" ? undefined : addressesProblem(next.addresses, networkOf(c));
  const changed =
    implementation !== current.implementation ||
    (implementation !== "" && JSON.stringify(next.addresses) !== JSON.stringify(current.addresses));
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
          <button type="submit" className="primary" disabled={busy || !changed || problem !== undefined}>
            Save
          </button>
        </>
      }
    >
      <LoadBalancerFields
        distribution={c.distribution}
        implementation={implementation}
        addresses={addresses}
        error={typed ? problem : undefined}
        onImplementation={setImplementation}
        onAddresses={(a) => {
          setAddresses(a);
          setTyped(true);
        }}
      />
      <ErrorBox message={error} />
    </Dialog>
  );
}
