import { useEffect, useState } from "react";

import { api } from "./api";
import { coresText, machineMinimums, memoryStepMiB } from "./create-form";
import { sizeOf } from "./MachineFields";
import { whole } from "./network";
import type { Cluster, ClusterClass, MachineSize, WorkerPool } from "./types";

/** poolSize is a pool's machine size: its own, or the cluster's for its workers. */
export const poolSize = (c: Cluster, p: WorkerPool): MachineSize | undefined =>
  p.machine ?? sizeOf(c.variables["workerMachine"]);

/** leastSize is the least size the class allows a pool's machines, or the control plane's for none. */
export const leastSize = (cls: ClusterClass | undefined, pool: WorkerPool | undefined): MachineSize =>
  machineMinimums(cls, pool === undefined ? "controlPlaneMachine" : "workerMachine");

/** sizeProblem says what keeps a size from being sent, by the class's least. */
export function sizeProblem(m: MachineSize, min: MachineSize): string | undefined {
  if (!whole(m.cores, min.cores)) {
    return `At least ${coresText(min.cores)}`;
  }
  if (!whole(m.memoryMiB, min.memoryMiB) || m.memoryMiB % memoryStepMiB !== 0) {
    return `At least ${min.memoryMiB} MiB of memory, in steps of ${memoryStepMiB}`;
  }
  if (!whole(m.diskGiB, min.diskGiB)) {
    return `At least ${min.diskGiB} GiB of disk`;
  }
  return undefined;
}

/** useClass reads the cluster's class, for the least size its machines may have. */
export function useClass(name: string): ClusterClass | undefined {
  const [cls, setCls] = useState<ClusterClass>();
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        const classes = await api.classes();
        setCls(classes.find((x) => x.name === name));
      } catch {
        // The API server checks every size. Without the class the dialog checks only the form.
      }
    }
    void load();
  }, [name]);
  return cls;
}
