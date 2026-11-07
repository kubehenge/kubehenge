import { useState, type JSX } from "react";

import { NodeTimeoutFields, entryOf, timeoutProblems, timeoutsOf, type TimeoutEntry } from "./NodeTimeouts";
import type { Cluster, ClusterClass, WorkerPool } from "./types";
import { Dialog, ErrorBox, useChange } from "./ui";

/** TimeoutsDialog sets how long a removed machine of the control planes, or of a pool, waits for its node. */
export function TimeoutsDialog({
  cluster: c,
  pool,
  cls,
  onClose,
}: {
  cluster: Cluster;
  /** Undefined for the control planes. */
  pool: WorkerPool | undefined;
  cls: ClusterClass | undefined;
  onClose: () => void;
}): JSX.Element {
  const was = (pool === undefined ? c.controlPlaneTimeouts : pool.timeouts) ?? {};
  const [entry, setEntry] = useState<TimeoutEntry>(entryOf(was));
  const { busy, error, send } = useChange(c, onClose);
  const invalid = Object.keys(timeoutProblems(entry)).length > 0;
  const timeouts = invalid ? {} : timeoutsOf(entry);
  const same = JSON.stringify(timeouts) === JSON.stringify(was);
  return (
    <Dialog
      title={pool === undefined ? "Control plane timeouts" : `Pool ${pool.name} timeouts`}
      onClose={onClose}
      onSubmit={() => {
        void send(pool === undefined ? { controlPlaneTimeouts: timeouts } : { pools: { [pool.name]: { timeouts } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || same || invalid}>
            Set
          </button>
        </>
      }
    >
      <NodeTimeoutFields
        value={entry}
        onChange={setEntry}
        fallback={pool === undefined ? cls?.controlPlaneTimeouts : cls?.workerTimeouts}
      />
      <ErrorBox message={error} />
    </Dialog>
  );
}
