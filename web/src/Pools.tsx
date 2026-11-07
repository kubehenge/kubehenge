import { useState, type JSX } from "react";

import { poolText } from "./Autoscaling";
import { machineMinimums } from "./create-form";
import { MachineFields } from "./MachineFields";
import { whole } from "./network";
import { poolSize, sizeOf, sizeProblem, useClass } from "./pool-machine";
import { NodesDialog, NodeTags, poolNodes } from "./PoolNodes";
import { useStaging } from "./staging-context";
import type { Cluster, MachineSize, WorkerPool } from "./types";
import { Dialog, ErrorBox, Field, RowMenu, maxWorkers, shownNumber, typedNumber, useChange } from "./ui";

// As the API server holds them: a DNS label, and host names of <cluster>-<pool>-<three random
// parts> within one.
const poolNamePattern = /^[a-z](?:[-a-z0-9]*[a-z0-9])?$/u;
const maxPoolNames = 44;

const mibPerGiB = 1024;

const memoryText = (mib: number): string => (mib % mibPerGiB === 0 ? `${mib / mibPerGiB} GiB` : `${mib} MiB`);

/** poolNameProblem says what is wrong with a new pool's name, as the API server would. */
export function poolNameProblem(c: Cluster, name: string): string | undefined {
  if (!poolNamePattern.test(name)) {
    return "Lowercase letters, digits and dashes, from a letter";
  }
  if (c.name.length + name.length > maxPoolNames) {
    return `At most ${maxPoolNames - c.name.length} characters for this cluster`;
  }
  return c.workerPools.some((p) => p.name === name) ? "A pool has this name" : undefined;
}

type PoolAction =
  | { kind: "add" }
  | { kind: "resize"; pool?: WorkerPool }
  | { kind: "nodes"; pool: WorkerPool }
  | { kind: "delete"; pool: WorkerPool };

function SizeCells({ size }: { size: MachineSize | undefined }): JSX.Element {
  return (
    <>
      <td className="num">{size?.cores ?? ""}</td>
      <td className="num">{size === undefined ? "" : memoryText(size.memoryMiB)}</td>
      <td className="num">{size === undefined ? "" : `${size.diskGiB} GiB`}</td>
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
      <table className="grid">
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
            <td>Control plane</td>
            <td>{c.controlPlane.desired}</td>
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
              </RowMenu>
            </td>
          </tr>
          {c.workerPools.map((p) => (
            <tr key={p.name}>
              <td className="id">
                {p.name}
                <NodeTags nodes={poolNodes(c, p)} />
              </td>
              <td>{poolText(p)}</td>
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
      {action?.kind === "nodes" && <NodesDialog cluster={c} pool={action.pool} onClose={close} />}
      {action?.kind === "delete" && <DeletePoolDialog cluster={c} pool={action.pool} onClose={close} />}
    </section>
  );
}

const fallbackSize: MachineSize = { cores: 2, memoryMiB: 4096, diskGiB: 30 };

function AddPoolDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const cls = useClass(c.class);
  const [name, setName] = useState("");
  const [workers, setWorkers] = useState(1);
  const [size, setSize] = useState<MachineSize>(sizeOf(c.variables["workerMachine"]) ?? fallbackSize);
  const { busy, error, send } = useChange(c, onClose);
  const min = machineMinimums(cls, "workerMachine");
  const nameProblem = name === "" ? undefined : poolNameProblem(c, name);
  const workersProblem = whole(workers, 0, maxWorkers) ? undefined : `A whole number from 0 to ${maxWorkers}`;
  const problem = sizeProblem(size, min);
  const ready = name !== "" && nameProblem === undefined && workersProblem === undefined && problem === undefined;
  return (
    <Dialog
      title={`Add pool to ${c.name}`}
      onClose={onClose}
      onSubmit={() => {
        void send({ pools: { [name]: { replicas: workers, machine: size } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !ready}>
            Add
          </button>
        </>
      }
    >
      <Field label="Name" error={nameProblem}>
        {(id) => <input id={id} value={name} autoComplete="off" onChange={(e) => setName(e.target.value.trim())} />}
      </Field>
      <Field label="Workers" error={workersProblem}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={0}
            max={maxWorkers}
            value={shownNumber(workers)}
            onChange={(e) => setWorkers(typedNumber(e.target.value))}
          />
        )}
      </Field>
      <MachineFields label="Each worker" value={size} min={min} error={problem} onChange={setSize} />
      <ErrorBox message={error} />
    </Dialog>
  );
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
  const min = machineMinimums(cls, pool === undefined ? "controlPlaneMachine" : "workerMachine");
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
