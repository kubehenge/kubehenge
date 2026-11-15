import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { isSecurityGroup, list } from "./network";
import type { Cluster, DatacenterFirewall } from "./types";
import { Dialog, ErrorBox, Field, stageLabel, useChange } from "./ui";
import { quantity, record, text, texts } from "./values";

/** securityGroupsOf reads the securityGroups variable into the text its field edits. */
export const securityGroupsOf = (v: unknown): string => (texts(v) ?? []).join(", ");

/** A port the nodes accept from anywhere, as the firewall variable holds it. */
interface FirewallPort {
  protocol: "tcp" | "udp";
  port: number;
  endPort?: number;
}

/** The firewall as its fields edit it. */
export interface FirewallFields {
  on: boolean;
  /** Whether the API, ingress, the load balancer, the NodePorts and ping are open to anyone. */
  serving: boolean;
  /** Further ports, comma-separated, such as tcp/8443, udp/4000-4010. */
  ports: string;
}

/**
 * firewallOf reads the firewall variable. A cluster without it, made before it existed, has the
 * firewall its security groups give it. Its serving ports are open as the class defaults them, as
 * Cluster API's defaulting gives it at its next change.
 */
export function firewallOf(v: unknown, groups: unknown): FirewallFields {
  if (v === undefined || v === null) {
    return { on: (texts(groups) ?? []).length > 0, serving: true, ports: "" };
  }
  const fw = record(v);
  const ports = Array.isArray(fw["ports"]) ? fw["ports"].map((p) => portText(record(p))) : [];
  return { on: fw["enabled"] !== false, serving: fw["servingPorts"] !== false, ports: ports.join(", ") };
}

const portText = (p: Record<string, unknown>): string => {
  const end = p["endPort"];
  return `${text(p["protocol"]) ?? ""}/${quantity(p["port"]) ?? ""}${typeof end === "number" ? `-${end}` : ""}`;
};

const portPattern = /^(?<protocol>tcp|udp)\/(?<port>\d{1,5})(?:-(?<end>\d{1,5}))?$/u;
const maxPort = 65_535;

/** parsePorts reads the ports' text, undefined where an entry is not a port or a range. */
function parsePorts(s: string): FirewallPort[] | undefined {
  const ports: FirewallPort[] = [];
  for (const entry of list(s)) {
    const g = portPattern.exec(entry.toLowerCase())?.groups;
    if (g === undefined) {
      return undefined;
    }
    const port = Number(g["port"]);
    const end = g["end"] === undefined ? undefined : Number(g["end"]);
    if (port < 1 || port > maxPort || (end !== undefined && (end <= port || end > maxPort))) {
      return undefined;
    }
    const protocol = g["protocol"] === "udp" ? "udp" : "tcp";
    ports.push(end === undefined ? { protocol, port } : { protocol, port, endPort: end });
  }
  return ports;
}

/** firewallVariable is the firewall variable the fields give. */
export function firewallVariable(f: FirewallFields): {
  enabled: boolean;
  servingPorts?: false;
  ports?: FirewallPort[];
} {
  const ports = f.on ? (parsePorts(f.ports) ?? []) : [];
  return {
    enabled: f.on,
    ...(f.on && !f.serving ? { servingPorts: false as const } : {}),
    ...(ports.length > 0 ? { ports } : {}),
  };
}

export type DatacenterFirewallState = DatacenterFirewall["state"];

/**
 * useDatacenterFirewall reads whether Proxmox VE's datacenter firewall is on. A read that fails
 * leaves it unknown. Nothing waits on it, so it never stops a create.
 */
export function useDatacenterFirewall(): DatacenterFirewallState | undefined {
  const [state, setState] = useState<DatacenterFirewallState>();
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        const read = await api.datacenterFirewall();
        setState(read.state);
      } catch {
        setState("unknown");
      }
    }
    void load();
  }, []);
  return state;
}

/** datacenterNote is what a firewall that is on needs said of the datacenter firewall, if anything. */
export function datacenterNote(state: DatacenterFirewallState | undefined): string | undefined {
  if (state === "off") {
    return "Proxmox VE's datacenter firewall is off, so this cluster's firewall does nothing";
  }
  return state === "unknown" ? "Datacenter firewall state unknown" : undefined;
}

/**
 * clusterFirewallText is the firewall's row on a cluster's page: its setting, then its state where
 * the controller found that it does nothing or could not tell.
 */
export function clusterFirewallText(c: Cluster): string {
  const setting = firewallText(
    firewallOf(c.variables["firewall"], c.variables["securityGroups"]),
    securityGroupsOf(c.variables["securityGroups"]),
  );
  const inactive = c.conditions.find((x) => x.type === "FirewallInactive");
  if (inactive?.status === "True") {
    return `${setting}. Inactive, datacenter firewall off`;
  }
  return inactive?.status === "Unknown" ? `${setting}. State unknown` : setting;
}

/** firewallText is how Confirm and the settings list the firewall. */
export function firewallText(f: FirewallFields, groups: string): string {
  if (!f.on) {
    return "Off";
  }
  const parts = [f.serving ? "On, serving ports open" : "On, serving ports closed"];
  if (list(f.ports).length > 0) {
    parts.push(`also ${list(f.ports).join(", ")}`);
  }
  if (list(groups).length > 0) {
    parts.push(`security groups ${list(groups).join(", ")}`);
  }
  return parts.join(", ");
}

/** firewallPortsProblem names what is wrong with the ports, or nothing. */
export function firewallPortsProblem(f: FirewallFields): string | undefined {
  return f.on && parsePorts(f.ports) === undefined ? "Ports as tcp/8443 or ranges as udp/4000-4010" : undefined;
}

/** securityGroupsProblem names what is wrong with the groups, or nothing. The server checks they exist. */
export function securityGroupsProblem(s: string, on = true): string | undefined {
  const groups = list(s);
  if (groups.length > 0 && !on) {
    return "Security groups need the firewall on";
  }
  if (groups.some((g) => !isSecurityGroup(g))) {
    return "Names of 2 to 18 letters, digits, hyphens and underscores, starting with a letter";
  }
  return new Set(groups).size < groups.length ? "Each group once" : undefined;
}

/** FirewallField edits a cluster's firewall: on or off, its further ports and its security groups. */
export function FirewallField({
  value,
  onChange,
  groups,
  onGroups,
  portsError,
  groupsError,
  datacenter,
}: {
  value: FirewallFields;
  onChange: (f: FirewallFields) => void;
  groups: string;
  onGroups: (s: string) => void;
  portsError?: string | undefined;
  groupsError?: string | undefined;
  /** The datacenter firewall's state, where it was read. */
  datacenter?: DatacenterFirewallState | undefined;
}): JSX.Element {
  const note = value.on ? datacenterNote(datacenter) : undefined;
  return (
    <>
      <label className="check">
        <input
          type="checkbox"
          checked={value.on}
          onChange={(e) => {
            onChange({ ...value, on: e.target.checked });
          }}
        />{" "}
        Firewall
      </label>
      {!value.on && <p className="hint">Every port open</p>}
      {note !== undefined && <p className="warning">{note}</p>}
      {value.on && (
        <>
          <label className="check">
            <input
              type="checkbox"
              checked={value.serving}
              onChange={(e) => {
                onChange({ ...value, serving: e.target.checked });
              }}
            />{" "}
            Serving ports open
          </label>
          <p className="hint">API, ingress, load balancer, NodePorts and ping from anywhere</p>
        </>
      )}
      {value.on && (
        <Field label="Further open ports" hint="Optional, such as tcp/8443, udp/4000-4010" error={portsError}>
          {(id) => (
            <input
              id={id}
              spellCheck={false}
              value={value.ports}
              onChange={(e) => {
                onChange({ ...value, ports: e.target.value });
              }}
            />
          )}
        </Field>
      )}
      {value.on && (
        <Field label="Firewall security groups" hint="Optional, comma-separated" error={groupsError}>
          {(id) => (
            <input
              id={id}
              spellCheck={false}
              value={groups}
              onChange={(e) => {
                onGroups(e.target.value);
              }}
            />
          )}
        </Field>
      )}
    </>
  );
}

export function FirewallDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const currentGroups = securityGroupsOf(c.variables["securityGroups"]);
  const current = firewallOf(c.variables["firewall"], c.variables["securityGroups"]);
  const [value, setValue] = useState(current);
  const [groups, setGroups] = useState(currentGroups);
  const [attempted, setAttempted] = useState(false);
  const { busy, error, send } = useChange(c, onClose);
  const portsFound = firewallPortsProblem(value);
  const groupsFound = value.on ? securityGroupsProblem(groups) : undefined;
  const changed =
    value.on !== current.on ||
    (value.on && value.serving !== current.serving) ||
    (value.on && list(value.ports).join(",") !== list(current.ports).join(",")) ||
    (value.on && list(groups).join(",") !== list(currentGroups).join(","));
  return (
    <Dialog
      title="Edit firewall"
      onClose={onClose}
      onSubmit={() => {
        if (portsFound !== undefined || groupsFound !== undefined) {
          setAttempted(true);
          return;
        }
        const named = value.on ? list(groups) : [];
        void send({
          variables: { firewall: firewallVariable(value), securityGroups: named.length > 0 ? named : null },
        });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed}>
            {stageLabel}
          </button>
        </>
      }
    >
      <FirewallField
        value={value}
        onChange={setValue}
        groups={groups}
        onGroups={setGroups}
        portsError={attempted ? portsFound : undefined}
        groupsError={attempted ? groupsFound : undefined}
      />
      <p className="muted">Replaces every node, one at a time.</p>
      <ErrorBox message={error} />
    </Dialog>
  );
}
