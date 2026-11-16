import { useState, type JSX } from "react";

import type { Cluster, ClusterClass } from "./types";
import { Dialog, ErrorBox, Field, stageLabel, useChange } from "./ui";
import { quantity, record } from "./values";

/** The ClusterClass variable giving the VM IDs a cluster's VMs take. */
export const vmIDVariable = "vmIDRange";

/** The annotation of a class that names the site's range, as start-end. */
export const siteVMIDAnnotation = "kubehenge.dev/vm-id-range";

// The VM IDs Proxmox VE accepts.
const leastID = 100;
const mostID = 999_999_999;

const rangeFormat = /^(?<start>\d+)-(?<end>\d+)$/u;

/** vmIDRangeOf reads a cluster's variable as the text of a field, empty when it has none. */
export function vmIDRangeOf(v: unknown): string {
  const r = record(v);
  const start = quantity(r["start"]);
  const end = quantity(r["end"]);
  return start === undefined || end === undefined ? "" : `${start}-${end}`;
}

/** vmIDRangeVariable is the variable a field's text sets. Call it only for text vmIDRangeProblem accepts. */
export function vmIDRangeVariable(text: string): { start: number; end: number } {
  const [start = "", end = ""] = text.trim().split("-");
  return { start: Number(start), end: Number(end) };
}

/**
 * vmIDRangeProblem names what is wrong with a range, as the webhook and the settings check do, or
 * undefined. Empty text is no range. Given the nodes a cluster runs at most, one more for an upgrade
 * has to fit too.
 */
export function vmIDRangeProblem(text: string, nodes?: number): string | undefined {
  if (text.trim() === "") {
    return undefined;
  }
  const found = rangeFormat.exec(text.trim())?.groups;
  if (found === undefined) {
    return "Write it like 10000-10999";
  }
  const start = Number(found["start"]);
  const end = Number(found["end"]);
  if (start > end) {
    return "The start is above the end";
  }
  if (start < leastID || end > mostID) {
    return `IDs run from ${leastID} to ${mostID}`;
  }
  const need = nodes === undefined ? 0 : nodes + 1;
  if (end - start + 1 < need) {
    return `Holds ${end - start + 1} IDs. Nodes and an upgrade need ${need}`;
  }
  return undefined;
}

/** vmIDRangeText says the range as the review and the options do. */
export const vmIDRangeText = (text: string, site: string): string => {
  if (text.trim() !== "") {
    return text.trim();
  }
  return site === "" ? "Next free ID" : `${site} (site)`;
};

/** vmIDRangeHint says what an empty field takes: the site's range, else the next free ID. */
export const vmIDRangeHint = (site: string | undefined): string =>
  site === undefined ? "Empty takes the next free ID" : `Empty takes the site's, ${site}`;

/** nodesAtMost is how many nodes a cluster runs at most, an autoscaled pool at its largest. */
export const nodesAtMost = (c: Cluster): number =>
  c.controlPlane.desired + c.workerPools.reduce((n, p) => n + (p.autoscaling?.max ?? p.replicas), 0);

/** VMIDRangeField chooses the VM IDs a cluster's VMs take. */
export function VMIDRangeField({
  value,
  onChange,
  problem,
  hint,
}: {
  value: string;
  onChange: (range: string) => void;
  problem: string | undefined;
  hint: string;
}): JSX.Element {
  return (
    <Field label="VM ID range" hint={hint} error={problem}>
      {(id) => <input id={id} value={value} onChange={(e) => onChange(e.target.value.trim())} />}
    </Field>
  );
}

/** VMIDRangeStep is the wizard's field, only for a class that has the variable. */
export function VMIDRangeStep({
  cls,
  value,
  onChange,
  problem,
}: {
  cls: ClusterClass | undefined;
  value: string;
  onChange: (range: string) => void;
  problem: string | undefined;
}): JSX.Element | null {
  if (cls?.variables.some((v) => v.name === vmIDVariable) !== true) {
    return null;
  }
  return (
    <VMIDRangeField
      value={value}
      onChange={onChange}
      problem={problem}
      hint={vmIDRangeHint(cls.annotations?.[siteVMIDAnnotation])}
    />
  );
}

export function VMIDRangeDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const initial = vmIDRangeOf(c.variables[vmIDVariable]);
  const [value, setValue] = useState(initial);
  const { busy, error, send } = useChange(c, onClose);
  const problem = vmIDRangeProblem(value, nodesAtMost(c));
  const unusable = value.trim() === "" || problem !== undefined;
  return (
    <Dialog
      title="Edit VM ID range"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { [vmIDVariable]: vmIDRangeVariable(value) } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || value === initial || unusable}>
            {stageLabel}
          </button>
        </>
      }
    >
      <VMIDRangeField value={value} onChange={setValue} problem={problem} hint="Like 10000-10999" />
      <p className="muted">Replaces every node, one at a time.</p>
      <ErrorBox message={error} />
    </Dialog>
  );
}
