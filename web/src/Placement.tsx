import { useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import type { Cluster, ClusterClass, HARule, HARules, PveNode, Storage } from "./types";
import { Detail, Dialog, ErrorBox, Field, Info, errorMessage, filled, useChange } from "./ui";
import { flag, record, texts } from "./values";

/** Where a cluster's VMs may run: any node, the nodes of an HA rule, or nodes chosen one by one. */
export const placements = ["any", "rule", "nodes"] as const;
export type Placement = (typeof placements)[number];

export interface Place {
  placement: Placement;
  /** The HA rule chosen, for a placement by rule. */
  haRule: string;
  /** The nodes the VMs keep to, unless placed on any node: the rule's, or those chosen. */
  nodes: string[];
}

export const anyNode: Place = { placement: "any", haRule: "", nodes: [] };

/** placeOf reads a cluster's allowed nodes as a placement; which rule they came from is not kept. */
export const placeOf = (allowed: string[] | undefined): Place =>
  allowed === undefined || allowed.length === 0 ? anyNode : { placement: "nodes", haRule: "", nodes: allowed };

/** allowedNodes is the cluster's allowedNodes value for a placement, or undefined for any node. */
export const allowedNodes = (p: Place): string[] | undefined => (p.placement === "any" ? undefined : p.nodes);

export function placeText(p: Place): string {
  const byPlacement: Record<Placement, string> = {
    any: "Any node",
    rule: `HA rule ${p.haRule}: ${p.nodes.join(", ")}`,
    nodes: p.nodes.join(", "),
  };
  return byPlacement[p.placement];
}

export function placeProblem(p: Place): string | undefined {
  if (p.placement === "rule" && !p.haRule) {
    return "Choose an HA rule";
  }
  if (p.placement === "nodes" && p.nodes.length === 0) {
    return "Choose at least one node";
  }
  return undefined;
}

export interface HARulesRead {
  haRules: HARules | undefined;
  haRulesError: string | undefined;
}

/**
 * useHARules reads Proxmox VE's HA rules. Without them a cluster can still be placed on any node or
 * on nodes chosen, so a failure shows only where the rules would be offered.
 */
export function useHARules(): HARulesRead {
  const [haRules, setHARules] = useState<HARules>();
  const [haRulesError, setHARulesError] = useState<string>();
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        setHARules(await api.haRules());
      } catch (err) {
        setHARulesError(errorMessage(err));
      }
    }
    void load();
  }, []);
  return { haRules, haRulesError };
}

// ruleLabel names a rule with what it keeps the cluster to.
const ruleLabel = (r: HARule): string =>
  `${r.rule}${filled(r.comment) ? ` (${r.comment})` : ""}: ${r.allowed.join(", ")}${r.strict === true ? ", strict" : ""}`;

// rulesHint says why no HA rule can be chosen; a read failure's message sits behind Detail.
function rulesHint({ haRules, haRulesError }: HARulesRead): JSX.Element | undefined {
  if (filled(haRulesError)) {
    return (
      <>
        Couldn&apos;t read the HA rules
        <Detail>{haRulesError}</Detail>
      </>
    );
  }
  if (haRules && !haRules.readable) {
    return <>Reading HA rules needs Sys.Audit on /</>;
  }
  if (haRules?.rules.length === 0) {
    return <>No enabled HA node-affinity rules</>;
  }
  return undefined;
}

const noNodes: string[] = [];

// choices are the nodes to tick: those the user sees, and those the cluster has that they do not,
// which a change may keep.
function choices(nodes: PveNode[], kept: string[]): { node: string; note: string }[] {
  const seen = nodes.map((n) => ({ node: n.node, note: n.online ? "" : " (offline)" }));
  const unseen = kept
    .filter((x) => !nodes.some((n) => n.node === x))
    .map((node) => ({ node, note: " (not shown to you)" }));
  return [...seen, ...unseen];
}

export function PlacementFields({
  value,
  onChange,
  problem,
  hint,
  nodes,
  kept = noNodes,
  rules: read,
  framed = true,
}: {
  value: Place;
  onChange: (p: Place) => void;
  problem: string | undefined;
  hint?: string;
  nodes: PveNode[];
  /** The nodes the cluster already has, offered even where the user cannot see them. */
  kept?: string[] | undefined;
  rules: HARulesRead;
  /** False where a dialog's title already says Placement: the group then draws no box and no legend. */
  framed?: boolean;
}): JSX.Element {
  const rules = read.haRules?.rules ?? [];
  const nodesOf = (rule: string): string[] => rules.find((r) => r.rule === rule)?.allowed ?? [];
  const choose = (placement: Placement): void => {
    const chosen: Record<Placement, string[]> = { any: [], rule: nodesOf(value.haRule), nodes: value.nodes };
    onChange({ ...value, placement, nodes: chosen[placement] });
  };
  const rulesNote = rulesHint(read);
  return (
    <fieldset
      className={framed ? "group" : "bare"}
      aria-label={framed ? undefined : "Placement"}
      aria-describedby={hint === undefined ? undefined : "placement-hint"}
    >
      {framed && <legend>Placement</legend>}
      {hint !== undefined && (
        <p className="hint" id="placement-hint">
          {hint}
        </p>
      )}
      <label className="check">
        <input type="radio" name="placement" checked={value.placement === "any"} onChange={() => choose("any")} /> Any
        node
      </label>
      <label className="check">
        <input
          type="radio"
          name="placement"
          checked={value.placement === "rule"}
          disabled={rules.length === 0}
          onChange={() => choose("rule")}
        />{" "}
        HA rule nodes
      </label>
      {value.placement === "rule" && (
        <Field label="HA rule" error={problem}>
          {(id) => (
            <select
              id={id}
              value={value.haRule}
              onChange={(e) => onChange({ ...value, haRule: e.target.value, nodes: nodesOf(e.target.value) })}
            >
              <option value="" disabled>
                Choose…
              </option>
              {rules.map((r) => (
                <option key={r.rule} value={r.rule}>
                  {ruleLabel(r)}
                </option>
              ))}
            </select>
          )}
        </Field>
      )}
      {rulesNote !== undefined && <div className="hint">{rulesNote}</div>}
      <label className="check">
        <input type="radio" name="placement" checked={value.placement === "nodes"} onChange={() => choose("nodes")} />{" "}
        Chosen nodes
      </label>
      {value.placement === "nodes" && (
        <fieldset className="group">
          <legend>Nodes</legend>
          {choices(nodes, kept).map(({ node, note }) => (
            <label key={node} className="check">
              <input
                type="checkbox"
                aria-invalid={problem === undefined ? undefined : true}
                checked={value.nodes.includes(node)}
                onChange={(e) =>
                  onChange({
                    ...value,
                    nodes: e.target.checked ? [...value.nodes, node] : value.nodes.filter((x) => x !== node),
                  })
                }
              />{" "}
              {node}
              {note}
            </label>
          ))}
          {problem !== undefined && (
            <p className="field-error" role="alert">
              {problem}
            </p>
          )}
        </fieldset>
      )}
    </fieldset>
  );
}

/** haAnnotation marks the classes whose proxmoxHA variable takes effect: an administrator enabled HA. */
export const haAnnotation = "kubehenge.dev/proxmox-ha";

/**
 * offersHA tells whether Proxmox VE HA would keep apart the control planes of a cluster of the
 * class on the storage: it moves and restarts VMs only where every node reaches their disks.
 */
export function offersHA(c: ClusterClass | undefined, storages: Storage[], storage: string | undefined): boolean {
  return (
    c?.annotations?.[haAnnotation] === "true" &&
    c.variables.some((v) => v.name === "proxmoxHA") &&
    storages.some((s) => s.storage === storage && s.shared)
  );
}

/** useHAOffered reads whether an existing cluster's class and storage take part in Proxmox VE HA. */
export function useHAOffered(className: string, storage: string | undefined): boolean {
  const [offered, setOffered] = useState(false);
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        const [classes, storages] = await Promise.all([api.classes(), api.storages()]);
        setOffered(
          offersHA(
            classes.find((c) => c.name === className),
            storages,
            storage,
          ),
        );
      } catch {
        // Without the classes or storages the option is not shown; the cluster keeps its setting.
        setOffered(false);
      }
    }
    void load();
  }, [className, storage]);
  return offered;
}

/** haText describes a cluster's Proxmox VE HA setting; unset means it takes part. */
export const haText = (on: boolean): string => (on ? "On" : "Off");

/**
 * HAField switches Proxmox VE HA for a cluster's control-plane VMs. The watchdog an HA VM arms on its
 * node stays in view: it restarts the node itself on a loss of quorum.
 */
export function HAField({ checked, onChange }: { checked: boolean; onChange: (on: boolean) => void }): JSX.Element {
  const hint = useId();
  return (
    <>
      <label className="check">
        <input type="checkbox" checked={checked} aria-describedby={hint} onChange={(e) => onChange(e.target.checked)} />{" "}
        Use Proxmox VE HA
      </label>
      <Info about="Proxmox VE HA">
        Keeps control-plane VMs on separate nodes and restarts them elsewhere when a node fails.
      </Info>
      <p className="hint" id={hint}>
        Nodes running HA VMs reboot if they lose quorum
      </p>
    </>
  );
}

/** haOn reads the cluster's proxmoxHA variable, which takes part unless it is false. */
export const haOn = (c: Cluster): boolean => flag(c.variables["proxmoxHA"]) !== false;

export function HADialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [on, setOn] = useState(haOn(c));
  const { busy, error, send } = useChange(c, onClose);
  return (
    <Dialog
      title="Edit Proxmox VE HA"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { proxmoxHA: on } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || on === haOn(c)}>
            Save
          </button>
        </>
      }
    >
      <HAField checked={on} onChange={setOn} />
      <ErrorBox message={error} />
    </Dialog>
  );
}

export function PlacementDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const kept = texts(c.variables["allowedNodes"]);
  const initial = placeOf(kept);
  const [value, setValue] = useState(initial);
  const [nodes, setNodes] = useState<PveNode[]>([]);
  const [nodesError, setNodesError] = useState<string>();
  const rules = useHARules();
  const { busy, error, send } = useChange(c, onClose);
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        setNodes(await api.nodes());
      } catch (err) {
        setNodesError(errorMessage(err));
      }
    }
    void load();
  }, []);
  const problem = placeProblem(value);
  const next = allowedNodes(value) ?? null;
  const changed = JSON.stringify(next) !== JSON.stringify(allowedNodes(initial) ?? null);
  return (
    <Dialog
      title="Edit placement"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { allowedNodes: next } });
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
      <PlacementFields
        value={value}
        onChange={setValue}
        problem={problem}
        hint="New VMs follow it, running VMs stay where they are"
        nodes={nodes}
        kept={kept}
        rules={rules}
        framed={false}
      />
      <ErrorBox message={nodesError ?? error} />
    </Dialog>
  );
}

/** sysAuditAnnotation marks the classes whose clusters may choose the HA group labels. */
export const sysAuditAnnotation = "kubehenge.dev/sys-audit";

/**
 * offersHAGroups tells whether the class's clusters may have the HA group labels. They take no right
 * of the user's own: Sys.Audit on / reads the Proxmox VE cluster's configuration alone, and
 * Kubehenge's administrator decides whether clusters may hold it.
 */
export const offersHAGroups = (cls: ClusterClass | undefined): boolean =>
  cls?.annotations?.[sysAuditAnnotation] === "true" && cls.variables.some((v) => v.name === "ccm");

/** haGroupsOn reads whether the cluster's nodes are labelled with their Proxmox VE HA groups. */
export const haGroupsOn = (c: Cluster): boolean => flag(record(c.variables["ccm"])["haGroups"]) === true;

/** HAGroupsField chooses the HA group labels. */
export function HAGroupsField({
  checked,
  onChange,
}: {
  checked: boolean;
  onChange: (on: boolean) => void;
}): JSX.Element {
  const hint = useId();
  return (
    <>
      <label className="check">
        <input type="checkbox" checked={checked} aria-describedby={hint} onChange={(e) => onChange(e.target.checked)} />{" "}
        Add HA group labels
      </label>
      <Info about="HA group labels">
        Labels nodes group.topology.proxmox.sinextra.dev/&lt;group&gt; by HA group and node-affinity rule. Sys.Audit
        cannot read guests or storages.
      </Info>
      <p className="hint" id={hint}>
        The token gets Sys.Audit on /: reads the cluster configuration
      </p>
    </>
  );
}

/** useHAGroupsOffered reads whether an existing cluster may have the HA group labels. */
export function useHAGroupsOffered(c: Cluster): boolean {
  const [offered, setOffered] = useState(false);
  const className = c.class;
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        const classes = await api.classes();
        setOffered(offersHAGroups(classes.find((x) => x.name === className)));
      } catch {
        // Without the classes the option is not shown; the cluster keeps its setting.
        setOffered(false);
      }
    }
    void load();
  }, [className]);
  return offered;
}

export function HAGroupsDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [on, setOn] = useState(haGroupsOn(c));
  const { busy, error, send } = useChange(c, onClose);
  return (
    <Dialog
      title="Edit HA group labels"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { ccm: { haGroups: on } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || on === haGroupsOn(c)}>
            Save
          </button>
        </>
      }
    >
      <HAGroupsField checked={on} onChange={setOn} />
      <ErrorBox message={error} />
    </Dialog>
  );
}
