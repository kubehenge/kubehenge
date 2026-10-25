import { useId, useState, type JSX } from "react";

import { api } from "./api";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, browserZone, errorMessage, timeZones, weekdays } from "./ui";
import { oneOf, quantities, quantity, record, text, texts } from "./values";

export const upgradeModes = ["off", "patch", "minor"] as const;
export type UpgradeMode = (typeof upgradeModes)[number];

export interface UpgradePolicy {
  mode?: UpgradeMode | undefined;
  /** The nodes move to new builds of their VM template inside the window; unset means on. */
  nodeImages?: boolean | undefined;
  window?:
    | {
        weekdays?: string[] | undefined;
        monthDays?: number[] | undefined;
        start?: string | undefined;
        durationMinutes?: number | undefined;
        timeZone?: string | undefined;
      }
    | undefined;
}

/** upgradePolicy reads a cluster's upgrades variable. */
export function upgradePolicy(v: unknown): UpgradePolicy {
  const p = record(v);
  const { mode, nodeImages } = p;
  const w = record(p["window"]);
  return {
    mode: oneOf(upgradeModes, mode) ? mode : undefined,
    nodeImages: typeof nodeImages === "boolean" ? nodeImages : undefined,
    window: {
      weekdays: texts(w["weekdays"]),
      monthDays: quantities(w["monthDays"]),
      start: text(w["start"]),
      durationMinutes: quantity(w["durationMinutes"]),
      timeZone: text(w["timeZone"]),
    },
  };
}

/** The window a policy names none of: every day from 02:00 for four hours. */
export const defaultStart = "02:00";
export const defaultMinutes = 240;

const minutesPerHour = 60;
const hoursPerDay = 24;
const lastMonthDay = 31;
// Windows are whole half hours, from one half hour to a day.
const stepMinutes = 30;
const shortestHours = 0.5;

/** refreshesNodes tells whether a policy moves the nodes to new template builds: on unless it says off, and never for Talos, whose nodes take their updates by upgrading Talos. */
export const refreshesNodes = (p: UpgradePolicy, distribution: string): boolean =>
  distribution !== "talos" && p.nodeImages !== false;

/** ModeSelect chooses an automatic upgrade policy's mode, for the dialog and the create wizard. */
export function ModeSelect({
  id,
  value,
  onChange,
}: {
  id: string;
  value: UpgradeMode;
  onChange: (m: UpgradeMode) => void;
}): JSX.Element {
  return (
    <select
      id={id}
      value={value}
      onChange={(e) => {
        const chosen = e.target.value;
        if (oneOf(upgradeModes, chosen)) {
          onChange(chosen);
        }
      }}
    >
      <option value="off">Off</option>
      <option value="patch">Patch releases</option>
      <option value="minor">Patch and minor releases</option>
    </select>
  );
}

// describe says in words when a policy upgrades a cluster, and moves its nodes to new template
// builds when nodeImages says the cluster's nodes may be.
export function describe(p: UpgradePolicy, nodeImages = false): string {
  const upgrades = p.mode !== undefined && p.mode !== "off";
  if (!upgrades && !nodeImages) {
    return "Off";
  }
  const w = p.window ?? {};
  const monthDays = w.monthDays ?? [];
  const days = [...(w.weekdays ?? []), ...(monthDays.length > 0 ? [`day ${monthDays.join(", ")} of the month`] : [])];
  const hours = (w.durationMinutes ?? defaultMinutes) / minutesPerHour;
  const what = [
    ...(upgrades ? [p.mode === "patch" ? "Patch releases" : "Patch and minor releases"] : []),
    ...(nodeImages ? [upgrades ? "new node images" : "New node images"] : []),
  ].join(" and ");
  return `${what}, ${days.length > 0 ? days.join(", ") : "every day"} from ${w.start ?? defaultStart} for ${hours} h (${
    w.timeZone ?? "UTC"
  })`;
}

const splitDays = (monthDays: string): string[] => monthDays.split(/[\s,]+/u).filter(Boolean);

// windowProblems checks a window as the class's schema will: days 1 to 31, 30 minutes to a day.
export function windowProblems(monthDays: string, hours: string): { monthDays?: string; hours?: string } {
  const p: { monthDays?: string; hours?: string } = {};
  if (splitDays(monthDays).some((d) => !/^\d+$/u.test(d) || Number(d) < 1 || Number(d) > lastMonthDay)) {
    p.monthDays = "Whole numbers from 1 to 31.";
  }
  const h = Number(hours);
  if (hours.trim() === "" || !(h >= shortestHours && h <= hoursPerDay) || (h * minutesPerHour) % stepMinutes !== 0) {
    p.hours = "0.5 to 24 hours, in half hours.";
  }
  return p;
}

// PolicyFields are the dialog's fields, as typed.
interface PolicyFields {
  mode: UpgradeMode;
  // Undefined for Talos, whose nodes have no template builds to move to.
  nodeImages: boolean | undefined;
  days: string[];
  monthDays: string;
  start: string;
  hours: string;
  timeZone: string;
}

function fieldsOf(current: UpgradePolicy, distribution: string): PolicyFields {
  const w = current.window ?? {};
  const nodeImages = distribution === "talos" ? undefined : refreshesNodes(current, distribution);
  const active = (current.mode !== undefined && current.mode !== "off") || nodeImages === true;
  return {
    mode: current.mode ?? "off",
    nodeImages,
    days: w.weekdays ?? [],
    monthDays: (w.monthDays ?? []).join(", "),
    start: w.start ?? defaultStart,
    hours: String((w.durationMinutes ?? defaultMinutes) / minutesPerHour),
    // What is in effect: UTC when a policy names none; a new policy starts in the browser's zone.
    timeZone: w.timeZone ?? (active ? "UTC" : browserZone()),
  };
}

// windowed tells whether the fields need a window: something happens in it.
const windowed = (f: Pick<PolicyFields, "mode" | "nodeImages">): boolean => f.mode !== "off" || f.nodeImages === true;

// policyOf is the policy the fields save; nodeImages is written only when off, as on is the default.
function policyOf(f: PolicyFields): UpgradePolicy {
  const off = f.nodeImages === false ? { nodeImages: false } : {};
  if (!windowed(f)) {
    return { mode: "off", ...off };
  }
  const window: NonNullable<UpgradePolicy["window"]> = {
    start: f.start,
    durationMinutes: Math.round(Number(f.hours) * minutesPerHour),
    timeZone: f.timeZone,
  };
  if (f.days.length > 0) {
    window.weekdays = weekdays.filter((d) => f.days.includes(d));
  }
  const md = splitDays(f.monthDays).map(Number);
  if (md.length > 0) {
    window.monthDays = md;
  }
  return { mode: f.mode, ...off, window };
}

export function UpgradesDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const initial = fieldsOf(upgradePolicy(c.variables["upgrades"]), c.distribution);
  const [mode, setMode] = useState<UpgradeMode>(initial.mode);
  const [nodeImages, setNodeImages] = useState(initial.nodeImages);
  const [days, setDays] = useState<string[]>(initial.days);
  const [monthDays, setMonthDays] = useState(initial.monthDays);
  const [start, setStart] = useState(initial.start);
  const [hours, setHours] = useState(initial.hours);
  const [timeZone, setTimeZone] = useState(initial.timeZone);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const nodeImagesHint = useId();
  const withWindow = windowed({ mode, nodeImages });
  const problems = withWindow ? windowProblems(monthDays, hours) : {};
  const policy = (): UpgradePolicy => policyOf({ mode, nodeImages, days, monthDays, start, hours, timeZone });
  const changed = JSON.stringify(policy()) !== JSON.stringify(policyOf(initial));

  const save = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.change(c.namespace, c.name, { variables: { upgrades: policy() } });
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="Automatic upgrades"
      onClose={onClose}
      onSubmit={() => {
        void save();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || Object.keys(problems).length > 0}>
            Save
          </button>
        </>
      }
    >
      <p className="muted">
        Kubehenge upgrades Kubernetes inside the window: one minor version at a time, never past the distribution's
        stable release, only while the cluster is healthy and nothing else rolls out. A started upgrade finishes even if
        the window closes first.
      </p>
      <Field label="Upgrade automatically">{(id) => <ModeSelect id={id} value={mode} onChange={setMode} />}</Field>
      {nodeImages !== undefined && (
        <>
          <label className="check">
            <input
              type="checkbox"
              checked={nodeImages}
              aria-describedby={nodeImagesHint}
              onChange={(e) => setNodeImages(e.target.checked)}
            />{" "}
            Move the nodes to new template builds
          </label>
          <p className="muted" id={nodeImagesHint}>
            When the VM templates are built again with the node OS&apos;s updates, the nodes are replaced one at a time
            with clones of the new build inside the window, once the build is a day old, whatever the upgrade setting
            says.
          </p>
        </>
      )}
      {withWindow && (
        <>
          <fieldset className="group">
            <legend>Days</legend>
            {weekdays.map((d) => (
              <label key={d} className="check inline">
                <input
                  type="checkbox"
                  checked={days.includes(d)}
                  onChange={(e) => setDays(e.target.checked ? [...days, d] : days.filter((x) => x !== d))}
                />{" "}
                {d}
              </label>
            ))}
            <Field
              label="Days of the month"
              hint="Such as 1, 15. A day listed here or above counts; every day when both are empty."
              error={problems.monthDays}
            >
              {(id) => <input id={id} value={monthDays} onChange={(e) => setMonthDays(e.target.value)} />}
            </Field>
          </fieldset>
          <Field label="From">
            {(id) => <input id={id} type="time" value={start} onChange={(e) => setStart(e.target.value)} />}
          </Field>
          <Field label="Duration (hours)" error={problems.hours}>
            {(id) => (
              <input
                id={id}
                type="number"
                min={shortestHours}
                max={hoursPerDay}
                step={shortestHours}
                value={hours}
                onChange={(e) => setHours(e.target.value)}
              />
            )}
          </Field>
          <Field label="Time zone" hint="The window's times are in this time zone.">
            {(id) => (
              <select id={id} value={timeZone} onChange={(e) => setTimeZone(e.target.value)}>
                {timeZones(timeZone).map((z) => (
                  <option key={z} value={z}>
                    {z}
                  </option>
                ))}
              </select>
            )}
          </Field>
        </>
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}
