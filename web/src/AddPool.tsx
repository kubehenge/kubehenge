import { useState, type JSX } from "react";

import { machineMinimums } from "./create-form";
import { MachineFields } from "./MachineFields";
import { whole } from "./network";
import { hasNodeSettings, nodeSettingsProblem, nodesVariable, type NodeSettings } from "./node-settings";
import { NodeSettingsFields } from "./NodeSettings";
import { sizeOf, sizeProblem, useClass } from "./pool-machine";
import type { Cluster, MachineSize } from "./types";
import { Dialog, ErrorBox, Field, maxWorkers, shownNumber, typedNumber, useChange } from "./ui";

// As the API server holds them: a DNS label, and host names of <cluster>-<pool>-<three random
// parts> within one.
const poolNamePattern = /^[a-z](?:[-a-z0-9]*[a-z0-9])?$/u;
const maxPoolNames = 44;

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

export const fallbackSize: MachineSize = { cores: 2, memoryMiB: 4096, diskGiB: 30 };

/** AddPoolDialog adds a worker pool with its size and, when set, its labels and taints. */
export function AddPoolDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const cls = useClass(c.class);
  const [name, setName] = useState("");
  const [workers, setWorkers] = useState(1);
  const [size, setSize] = useState<MachineSize>(sizeOf(c.variables["workerMachine"]) ?? fallbackSize);
  const [nodes, setNodes] = useState<NodeSettings>({ labels: [], taints: [] });
  const { busy, error, send } = useChange(c, onClose);
  const min = machineMinimums(cls, "workerMachine");
  const nameProblem = name === "" ? undefined : poolNameProblem(c, name);
  const workersProblem = whole(workers, 0, maxWorkers) ? undefined : `A whole number from 0 to ${maxWorkers}`;
  const problem = sizeProblem(size, min);
  const nodesProblem = nodeSettingsProblem(nodes);
  const ready =
    name !== "" &&
    nameProblem === undefined &&
    workersProblem === undefined &&
    problem === undefined &&
    nodesProblem === undefined;
  return (
    <Dialog
      title={`Add pool to ${c.name}`}
      onClose={onClose}
      onSubmit={() => {
        void send({
          pools: {
            [name]: {
              replicas: workers,
              machine: size,
              ...(hasNodeSettings(nodes) && { nodes: nodesVariable(nodes) }),
            },
          },
        });
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
      <NodeSettingsFields value={nodes} onChange={setNodes} />
      <ErrorBox message={error} />
    </Dialog>
  );
}
