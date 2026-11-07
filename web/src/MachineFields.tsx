import type { JSX } from "react";

import { memoryStepMiB, type Machine } from "./create-form";
import type { MachineSize } from "./types";
import { Field, shownNumber, typedNumber } from "./ui";
import { quantity, record } from "./values";

/** sizeOf reads a machine variable's value, or undefined where it holds none. */
export function sizeOf(v: unknown): MachineSize | undefined {
  const r = record(v);
  const cores = quantity(r["cores"]);
  const memoryMiB = quantity(r["memoryMiB"]);
  const diskGiB = quantity(r["diskGiB"]);
  return cores === undefined || memoryMiB === undefined || diskGiB === undefined
    ? undefined
    : { cores, memoryMiB, diskGiB };
}

/** MachineFields edit a machine's cores, memory and disk, with a problem shown at the part it names. */
export function MachineFields({
  label,
  value,
  min,
  error,
  onChange,
}: {
  label: string;
  value: Machine;
  min: Machine;
  error?: string | undefined;
  onChange: (m: Machine) => void;
}): JSX.Element {
  const on = (part: string): string | undefined => (error?.toLowerCase().includes(part) === true ? error : undefined);
  return (
    <fieldset className="group">
      <legend>{label}</legend>
      <Field label="Cores" error={on("cores")}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={min.cores}
            value={shownNumber(value.cores)}
            onChange={(e) => onChange({ ...value, cores: typedNumber(e.target.value) })}
          />
        )}
      </Field>
      <Field label="Memory (MiB)" error={on("memory")}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={min.memoryMiB}
            step={memoryStepMiB}
            value={shownNumber(value.memoryMiB)}
            onChange={(e) => onChange({ ...value, memoryMiB: typedNumber(e.target.value) })}
          />
        )}
      </Field>
      <Field label="Disk (GiB)" error={on("disk")}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={min.diskGiB}
            value={shownNumber(value.diskGiB)}
            onChange={(e) => onChange({ ...value, diskGiB: typedNumber(e.target.value) })}
          />
        )}
      </Field>
    </fieldset>
  );
}
