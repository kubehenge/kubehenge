import type { JSX } from "react";

import { adminPath, type AdminView } from "./admin-views";
import { clusterPath, type View } from "./cluster-paths";
import { ranked, useFleet, type Standing } from "./fleet";
import { Link } from "./router";
import { settingLabel } from "./Staging";
import type { RunInfo } from "./types";
import { age, filled } from "./ui";

/**
 * Where a run stands in the column: under way, under way but stopped coming, failed and waiting for
 * a look, or done lately.
 */
export type RunGroup = "running" | "stalled" | "failed" | "done";

/** A piece of work somewhere in the fleet or the appliance. */
export interface Run {
  key: string;
  group: RunGroup;
  title: string;
  what: string;
  /** How far a running run is, when it says. */
  phase?: string;
  state: Standing["state"];
  to: string;
}

const runGroups: { group: RunGroup; label: string }[] = [
  { group: "running", label: "Running" },
  { group: "stalled", label: "Stalled" },
  { group: "failed", label: "Failed" },
  { group: "done", label: "Done" },
];

// The view of a cluster's page that shows each kind of run.
const runViews: Partial<Record<RunInfo["kind"], View>> = {
  "etcd-snapshot": "backups",
  "etcd-restore": "backups",
  backup: "backups",
  restore: "backups",
};

// Where the page of each kind of run sends its reader for more.
const places: Partial<Record<RunInfo["kind"], string>> = {
  "etcd-snapshot": "Open backups",
  "etcd-restore": "Open backups",
  backup: "Open backups",
  restore: "Open backups",
  change: "Open cluster",
};

// The page of Administration with the appliance's own work of each kind, where it is started again.
const adminPlaces: Partial<Record<RunInfo["kind"], { view: AdminView; place: string }>> = {
  template: { view: "templates", place: "Open templates" },
  "template-refresh": { view: "templates", place: "Open templates" },
  "appliance-upgrade": { view: "appliance", place: "Open upgrades" },
};

/** runPath is the address of a run's page. */
export const runPath = (id: string): string => `/runs/${id}`;

/** runTitle names a run: a change by the settings it sets. */
export function runTitle(r: RunInfo): string {
  if (r.kind !== "change" || (r.settings ?? []).length === 0) {
    return r.what;
  }
  return `Change ${(r.settings ?? []).map((x) => settingLabel(x)).join(", ")}`;
}

/** runView is where a run's work shows in the rest of the interface, and how a link there reads. */
export function runView(r: RunInfo): { to: string; place: string } {
  if (r.cluster === undefined) {
    const admin = adminPlaces[r.kind];
    return admin === undefined
      ? { to: adminPath(""), place: "Open Administration" }
      : { to: adminPath(admin.view), place: admin.place };
  }
  return {
    to: clusterPath(r.namespace ?? "", r.cluster, runViews[r.kind] ?? ""),
    place: places[r.kind] ?? "Open cluster",
  };
}

// The appliance's runs carry no cluster; each names what it works on.
const applianceTitles: Partial<Record<RunInfo["kind"], string>> = {
  "management-cluster": "Management cluster",
  template: "Templates",
  "template-refresh": "Templates",
  "appliance-upgrade": "Appliance",
};

// described is what a run does and, once it ended, how and when; a failure's message stays on the
// run's page, out of the narrow column.
function described(r: RunInfo): string {
  const what = runTitle(r);
  if (r.state === "running" || r.state === "stalled") {
    return what;
  }
  const when = r.finished === undefined ? "" : `, ${age(r.finished)} ago`;
  return `${what}${r.state === "failed" ? " failed" : ""}${when}`;
}

const groupOf: Record<RunInfo["state"], RunGroup> = {
  running: "running",
  stalled: "stalled",
  failed: "failed",
  succeeded: "done",
};
const stateOf: Record<RunInfo["state"], Standing["state"]> = {
  running: "progress",
  stalled: "warning",
  failed: "error",
  succeeded: "ok",
};

const shownName = (namespace: string, name: string): string =>
  namespace === "default" ? name : `${namespace}/${name}`;

function fromServer(r: RunInfo): Run {
  const ns = r.namespace ?? "";
  const cluster = r.cluster ?? "";
  const onCluster = cluster !== "";
  return {
    key: r.id,
    group: groupOf[r.state],
    title: onCluster ? shownName(ns, cluster) : (applianceTitles[r.kind] ?? "Appliance"),
    what: described(r),
    ...(r.state === "running" && r.phase !== undefined && { phase: r.phase }),
    state: stateOf[r.state],
    to: runPath(r.id),
  };
}

/**
 * useRuns is the work under way and lately finished: the changes the fleet's clusters show, then
 * what the appliance and the clusters' snapshots, backups and restores report.
 */
export function useRuns(): Run[] {
  const fleet = useFleet();
  const runs = fleet?.runs ?? [];
  // A change made here is followed by its own run; the fleet's standing says the same in less.
  const followed = new Set(
    runs
      .filter((r) => r.kind === "change" && (r.state === "running" || r.state === "stalled"))
      .map((r) => `${r.namespace}/${r.cluster}`),
  );
  // A creation, or a change made with kubectl, whose machines stopped coming shows as stalled too.
  const changes: Run[] = ranked(fleet?.clusters ?? [], fleet?.health)
    .filter(
      ({ cluster: c, standing }) =>
        (standing.group === "running" || filled(c.stalled)) && !followed.has(`${c.namespace}/${c.name}`),
    )
    .map(({ cluster: c, standing: s }) => {
      const run: Run = {
        key: `${c.namespace}/${c.name}`,
        group: filled(c.stalled) ? "stalled" : "running",
        title: shownName(c.namespace, c.name),
        what: s.reason,
        state: s.state,
        to: clusterPath(c.namespace, c.name, s.view),
      };
      return run;
    });
  return [...changes, ...runs.map((r) => fromServer(r))];
}

/** RunsPanel follows the work under way beside whatever page is open, and folds away. */
export function RunsPanel({ runs, onFold }: { runs: Run[]; onFold: () => void }): JSX.Element {
  const running = runs.filter((r) => r.group === "running").length;
  const stalled = runs.filter((r) => r.group === "stalled").length;
  return (
    <aside className="runs" aria-label="Runs">
      <div className="runs-head">
        <h2>
          Runs{" "}
          <span className="count">
            {running} running{stalled > 0 && `, ${stalled} stalled`}
          </span>
        </h2>
        <button type="button" className="icon" aria-label="Hide runs" title="Hide runs (T R)" onClick={onFold}>
          ›
        </button>
      </div>
      {runGroups.map(({ group, label }) => {
        const members = runs.filter((r) => r.group === group);
        if (members.length === 0) {
          return null;
        }
        return (
          <section key={group} className={`runs-group runs-${group}`} aria-label={label}>
            <h3>{label}</h3>
            <ul>
              {members.map((r) => (
                <li key={r.key}>
                  <Link to={r.to} className="run">
                    <span className={`dot dot-${r.state}`} aria-hidden="true" />
                    <span className="run-title">{r.title}</span>
                    <span className="run-what">{r.what}</span>
                    {r.phase !== undefined && <span className="run-what">{r.phase}</span>}
                  </Link>
                </li>
              ))}
            </ul>
          </section>
        );
      })}
    </aside>
  );
}
