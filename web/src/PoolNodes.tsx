import { useState, type JSX } from "react";

import {
  hasNodeSettings,
  labelText,
  nodeSettingsProblem,
  nodesOf,
  nodesVariable,
  sameNodeSettings,
  taintText,
  type NodeSettings,
} from "./node-settings";
import { NodeSettingsFields } from "./NodeSettings";
import type { Cluster, WorkerPool } from "./types";
import { Dialog, ErrorBox, useChange } from "./ui";

/** poolNodes are the labels and taints a pool's workers get: its own, or the cluster's for its workers. */
export const poolNodes = (c: Cluster, p: WorkerPool): NodeSettings => nodesOf(p.nodes ?? c.variables["workerNodes"]);

/** NodeTags shows labels and taints as small tags. */
export function NodeTags({ nodes }: { nodes: NodeSettings }): JSX.Element | null {
  if (!hasNodeSettings(nodes)) {
    return null;
  }
  return (
    <ul className="node-tags">
      {nodes.labels.map((l) => (
        <li key={`label ${l.key}`}>{labelText(l)}</li>
      ))}
      {nodes.taints.map((t) => (
        <li key={`taint ${t.key}`} className="taint">
          {taintText(t)}
        </li>
      ))}
    </ul>
  );
}

/** NodesDialog edits the labels and taints of a pool's workers, or has the pool take the cluster's. */
export function NodesDialog({
  cluster: c,
  pool,
  onClose,
}: {
  cluster: Cluster;
  pool: WorkerPool;
  onClose: () => void;
}): JSX.Element {
  const was = poolNodes(c, pool);
  const [value, setValue] = useState<NodeSettings>(was);
  // The pool takes the cluster's again: saved as null.
  const [inherit, setInherit] = useState(false);
  const { busy, error, send } = useChange(c, onClose);
  const problem = inherit ? undefined : nodeSettingsProblem(value);
  const changed = inherit ? pool.nodes !== undefined : !sameNodeSettings(value, was);
  return (
    <Dialog
      title={`Edit pool ${pool.name} nodes`}
      onClose={onClose}
      onSubmit={() => {
        void send({ pools: { [pool.name]: { nodes: inherit ? null : nodesVariable(value) } } });
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
      {changed && <p className="muted">Changing labels or taints replaces this pool's nodes, one at a time.</p>}
      <NodeSettingsFields
        value={value}
        onChange={(v) => {
          setInherit(false);
          setValue(v);
        }}
      />
      <button
        type="button"
        disabled={inherit || (pool.nodes === undefined && !changed)}
        onClick={() => {
          setInherit(true);
          setValue(nodesOf(c.variables["workerNodes"]));
        }}
      >
        Use the cluster's
      </button>
      <ErrorBox message={error} />
    </Dialog>
  );
}
