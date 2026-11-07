import { useState, type JSX } from "react";

import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, useChange } from "./ui";
import { quantity } from "./values";

/** The ClusterClass variable giving the minutes machines of an away host wait before replacement. */
export const graceVariable = "hostOutageGraceMinutes";

/** The class's default, which a cluster that sets nothing has. */
export const defaultGrace = "30";

const leastGrace = 5;
const mostGrace = 10_080;

/** graceOf reads a cluster's variable as the text of a field. */
export const graceOf = (v: unknown): string => {
  const minutes = quantity(v);
  return minutes === undefined ? defaultGrace : String(minutes);
};

/** graceProblem names what is wrong with the minutes, as the class's schema and the webhook do. */
export function graceProblem(text: string): string | undefined {
  const minutes = Number(text);
  if (text === "" || !Number.isInteger(minutes)) {
    return "A whole number of minutes";
  }
  if (minutes !== 0 && (minutes < leastGrace || minutes > mostGrace)) {
    return `0, or ${leastGrace} to ${mostGrace}`;
  }
  return undefined;
}

/** graceText says the wait as the review and the options do. */
export const graceText = (text: string): string => (text === "0" ? "Until the host returns" : `${text} minutes`);

/** HostOutageField chooses how long machines wait for a host that reboots or is in maintenance. */
export function HostOutageField({
  value,
  onChange,
  problem,
}: {
  value: string;
  onChange: (minutes: string) => void;
  problem: string | undefined;
}): JSX.Element {
  return (
    <Field label="Host outage wait" hint="Minutes, 0 waits for the host" error={problem}>
      {(id) => <input id={id} inputMode="numeric" value={value} onChange={(e) => onChange(e.target.value.trim())} />}
    </Field>
  );
}

export function HostOutageDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const initial = graceOf(c.variables[graceVariable]);
  const [value, setValue] = useState(initial);
  const { busy, error, send } = useChange(c, onClose);
  const problem = graceProblem(value);
  return (
    <Dialog
      title="Edit host outage wait"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { [graceVariable]: Number(value) } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || value === initial || problem !== undefined}>
            Save
          </button>
        </>
      }
    >
      <HostOutageField value={value} onChange={setValue} problem={problem} />
      <ErrorBox message={error} />
    </Dialog>
  );
}
