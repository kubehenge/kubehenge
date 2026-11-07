import { useState, type JSX } from "react";

import { AddPoolDialog, fallbackSize } from "./AddPool";
import { poolText } from "./Autoscaling";
import { MachineFields, sizeOf } from "./MachineFields";
import { leastSize, poolSize, sizeProblem, useClass } from "./pool-machine";
import { NodesDialog, NodeTags, poolNodes } from "./PoolNodes";
import { TimeoutsDialog } from "./PoolTimeouts";
import { useStaging } from "./staging-context";
import type { Cluster, MachineSize, WorkerPool } from "./types";
import { Dialog, ErrorBox, RowMenu, useChange } from "./ui";

const mibPerGiB = 1024;

const memoryText = (mib: number): string => (mib % mibPerGiB === 0 ? `${mib / mibPerGiB} GiB` : `${mib} MiB`);

type PoolAction =
  | { kind: "add" }
  | { kind: "resize"; pool?: WorkerPool }
  | { kind: "timeouts"; pool?: WorkerPool }
  | { kind: "nodes"; pool: WorkerPool }
  | { kind: "delete"; pool: WorkerPool };

function SizeCells({ size }: { size: MachineSize | undefined }): JSX.Element {
  return (
    <>
      <td className="num" data-label="Cores">
        {size?.cores ?? ""}
      </td>
      <td className="num" data-label="Memory">
        {size === undefined ? "" : memoryText(size.memoryMiB)}
      </td>
      <td className="num" data-label="Disk">
        {size === undefined ? "" : `${size.diskGiB} GiB`}
      </td>
    </>
  );
}

/**
 * Pools lists the control plane and each worker pool with its nodes and machine size, and for
 * those who may change the cluster adds, resizes and deletes pools, staged with the page's other
 * changes.
 */
export function Pools({
  cluster: current,
  can,
  why,
}: {
  cluster: Cluster;
  can: boolean;
  why: string | undefined;
}): JSX.Element {
  const c = useStaging()?.staged?.plan.cluster ?? current;
  const [action, setAction] = useState<PoolAction>();
  const close = (): void => setAction(undefined);
  return (
    <section className="box scroll" aria-label="Pools">
      <div className="section-head">
        <h3>Pools</h3>
        <button type="button" disabled={!can} title={why} onClick={() => setAction({ kind: "add" })}>
          Add pool…
        </button>
      </div>
      <table className="grid stack pools">
        <thead>
          <tr>
            <th>Pool</th>
            <th>Nodes</th>
            <th className="num">Cores</th>
            <th className="num">Memory</th>
            <th className="num">Disk</th>
            <th>
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td className="pool-name">Control plane</td>
            <td data-label="Nodes">{c.controlPlane.desired}</td>
            <SizeCells size={sizeOf(c.variables["controlPlaneMachine"])} />
            <td className="row-action">
              <RowMenu label="Control-plane actions">
                <button
                  type="button"
                  disabled={!can}
                  title={why}
                  aria-label="Resize the control plane"
                  onClick={() => setAction({ kind: "resize" })}
                >
                  Resize…
                </button>
                <button
                  type="button"
                  disabled={!can}
                  title={why}
                  aria-label="Control plane timeouts"
                  onClick={() => setAction({ kind: "timeouts" })}
                >
                  Timeouts…
                </button>
              </RowMenu>
            </td>
          </tr>
          {c.workerPools.map((p) => (
            <tr key={p.name}>
              <td className="id pool-name">
                {p.name}
                <NodeTags nodes={poolNodes(c, p)} />
              </td>
              <td data-label="Nodes">{poolText(p)}</td>
              <SizeCells size={poolSize(c, p)} />
              <td className="row-action">
                <RowMenu label={`Actions for pool ${p.name}`}>
                  <button
                    type="button"
                    disabled={!can}
                    title={why}
                    aria-label={`Resize pool ${p.name}`}
                    onClick={() => setAction({ kind: "resize", pool: p })}
                  >
                    Resize…
                  </button>
                  <button
                    type="button"
                    disabled={!can}
                    title={why}
                    aria-label={`Pool ${p.name} timeouts`}
                    onClick={() => setAction({ kind: "timeouts", pool: p })}
                  >
                    Timeouts…
                  </button>
                  <button
                    type="button"
                    disabled={!can}
                    title={why}
                    aria-label={`Edit pool ${p.name} nodes`}
                    onClick={() => setAction({ kind: "nodes", pool: p })}
                  >
                    Labels and taints…
                  </button>
                  <button
                    type="button"
                    className="danger"
                    disabled={!can}
                    title={why}
                    aria-label={`Delete pool ${p.name}`}
                    onClick={() => setAction({ kind: "delete", pool: p })}
                  >
                    Delete…
                  </button>
                </RowMenu>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {action?.kind === "add" && <AddPoolDialog cluster={c} onClose={close} />}
      {action?.kind === "resize" && <ResizeDialog cluster={c} pool={action.pool} onClose={close} />}
      {action?.kind === "timeouts" && <TimeoutsAction cluster={c} pool={action.pool} onClose={close} />}
      {action?.kind === "nodes" && <NodesDialog cluster={c} pool={action.pool} onClose={close} />}
      {action?.kind === "delete" && <DeletePoolDialog cluster={c} pool={action.pool} onClose={close} />}
    </section>
  );
}

function TimeoutsAction({
  cluster: c,
  pool,
  onClose,
}: {
  cluster: Cluster;
  pool: WorkerPool | undefined;
  onClose: () => void;
}): JSX.Element {
  return <TimeoutsDialog cluster={c} pool={pool} cls={useClass(c.class)} onClose={onClose} />;
}

function ResizeDialog({
  cluster: c,
  pool,
  onClose,
}: {
  cluster: Cluster;
  pool: WorkerPool | undefined;
  onClose: () => void;
}): JSX.Element {
  const cls = useClass(c.class);
  const was = (pool === undefined ? sizeOf(c.variables["controlPlaneMachine"]) : poolSize(c, pool)) ?? fallbackSize;
  const [size, setSize] = useState<MachineSize>(was);
  const { busy, error, send } = useChange(c, onClose);
  const min = leastSize(cls, pool);
  const problem = sizeProblem(size, min);
  const same = size.cores === was.cores && size.memoryMiB === was.memoryMiB && size.diskGiB === was.diskGiB;
  return (
    <Dialog
      title={pool === undefined ? `Resize ${c.name}'s control plane` : `Resize pool ${pool.name}`}
      onClose={onClose}
      onSubmit={() => {
        void send(
          pool === undefined
            ? { variables: { controlPlaneMachine: size } }
            : { pools: { [pool.name]: { machine: size } } },
        );
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || same || problem !== undefined}>
            Resize
          </button>
        </>
      }
    >
      <p>Replaces the nodes one at a time.</p>
      <MachineFields
        label={pool === undefined ? "Each control-plane node" : "Each worker"}
        value={size}
        min={min}
        error={problem}
        onChange={setSize}
      />
      <ErrorBox message={error} />
    </Dialog>
  );
}

function DeletePoolDialog({
  cluster: c,
  pool,
  onClose,
}: {
  cluster: Cluster;
  pool: WorkerPool;
  onClose: () => void;
}): JSX.Element {
  const { busy, error, send } = useChange(c, onClose);
  return (
    <Dialog
      title={`Delete pool ${pool.name}`}
      onClose={onClose}
      onSubmit={() => {
        void send({ pools: { [pool.name]: null } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={busy}>
            Delete
          </button>
        </>
      }
    >
      <p>Drains and deletes its workers and their disks.</p>
      <ErrorBox message={error} />
    </Dialog>
  );
}
