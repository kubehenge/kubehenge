import { createContext, use, useEffect, useMemo, useRef, type JSX, type ReactNode } from "react";

import { api } from "./api";
import { clusterPath, type View } from "./ClusterDetail";
import { Link } from "./router";
import type { Cluster, FleetHealth, Replicas, RunInfo } from "./types";
import { Status, distributionName, filled, health, pollIdle, settled, usePoll } from "./ui";

// How often the fleet is read again while any cluster is changing.
const pollChanging = 10_000;

// A cluster that is still not available an hour after it was created is no longer coming up on
// its own.
const provisioningGrace = 3_600_000;

/** The clusters the user may see, read once for the whole page: the rail, the list and the runs. */
export interface Fleet {
  clusters: Cluster[] | undefined;
  /** How each cluster's health page sums it up, by cluster key; undefined until read. */
  health: Map<string, FleetHealth> | undefined;
  /** The work running or lately finished that the user may see; undefined until read. */
  runs: RunInfo[] | undefined;
  error: string | undefined;
  reload: () => Promise<void>;
}

const FleetContext = createContext<Fleet | null>(null);

export const fleetKey = (c: { namespace: string; name: string }): string => `${c.namespace}/${c.name}`;

// Until the appliance is set up there is no fleet to read.
const noFleet = (): Promise<undefined> => Promise.resolve(undefined);

/**
 * FleetProvider reads the fleet for everything inside it, once `reads` is on; it stays in place
 * while it is off, so the page round it is not built again when reading starts.
 */
export function FleetProvider({ reads = true, children }: { reads?: boolean; children: ReactNode }): JSX.Element {
  const { data, error, reload } = usePoll<Cluster[] | undefined>(reads ? api.clusters : noFleet, (list) =>
    list?.every((c) => settled(c)) === true ? pollIdle : pollChanging,
  );
  // The clusters show without their health, which only adds why some need someone.
  const { data: read, reload: reloadHealth } = usePoll<FleetHealth[] | undefined>(
    reads ? api.fleetHealth : noFleet,
    pollIdle,
  );
  const { data: runs, reload: reloadRuns } = usePoll<RunInfo[] | undefined>(reads ? api.runs : noFleet, (list) =>
    list?.some((r) => r.state === "running") === true ? pollChanging : pollIdle,
  );
  const first = useRef(true);
  useEffect(() => {
    if (first.current) {
      first.current = false;
    } else if (reads) {
      void reload();
      void reloadHealth();
      void reloadRuns();
    }
  }, [reads, reload, reloadHealth, reloadRuns]);
  const fleet = useMemo(
    () => ({ clusters: data, health: read && new Map(read.map((h) => [fleetKey(h), h])), runs, error, reload }),
    [data, read, runs, error, reload],
  );
  return <FleetContext value={fleet}>{children}</FleetContext>;
}

/** useFleet is the fleet the page reads, or null outside a FleetProvider. */
export function useFleet(): Fleet | null {
  return use(FleetContext);
}

/** Where a cluster stands in the fleet: whether it needs someone, is on its way, or is well. */
export type Group = "needs" | "running" | "healthy";

export interface Standing {
  group: Group;
  state: "ok" | "progress" | "error" | "warning" | "deleting";
  /** Why it stands there, in a few words. */
  reason: string;
  /** The view of the cluster's page that shows more of why. */
  view: View;
  /** What a badge says of a cluster that needs someone, in place of what Cluster API says of it. */
  verdict?: string;
}

export const groups: { group: Group; label: string }[] = [
  { group: "needs", label: "Needs you" },
  { group: "running", label: "Running" },
  { group: "healthy", label: "Healthy" },
];

const steady = (r: Replicas): boolean => r.ready === r.desired && r.available === r.desired && r.upToDate === r.desired;

// firstWord is the node a Talos record names first: "<node> to <release> ...".
const firstWord = (s: string): string => s.split(" ")[0] ?? s;

// rollout says how far a set of machines is on its way to its desired state.
function rollout(what: string, r: Replicas): string {
  if (r.upToDate < r.desired) {
    return `${what}: ${r.upToDate} of ${r.desired} updated`;
  }
  return `${what}: ${r.ready} of ${r.desired} ready`;
}

/**
 * standing sums up a cluster for the fleet: from what its own answer says first, then, for one
 * that says nothing is wrong or under way, from what its health page found.
 */
export function standing(c: Cluster, found?: FleetHealth, now = Date.now()): Standing {
  if (c.deleting) {
    return { group: "running", state: "deleting", reason: "Deleting", view: "" };
  }
  return (
    failure(c) ??
    (c.available ? underWay(c) : comingUp(c, now)) ??
    finding(found) ?? {
      group: "healthy",
      state: "ok",
      reason: c.talos ? `Talos ${c.talos.version}, ${c.version}` : `${distributionName(c.distribution)} ${c.version}`,
      view: "",
    }
  );
}

// failure is what has failed in a cluster and waits for someone, if anything has.
function failure(c: Cluster): Standing | undefined {
  if (c.phase === "Failed") {
    return { group: "needs", state: "error", reason: "Failed", view: "health", verdict: "Failed" };
  }
  if (filled(c.talos?.failed)) {
    return {
      group: "needs",
      state: "error",
      reason: `Talos upgrade failed on ${firstWord(c.talos.failed)}`,
      view: "",
      verdict: "Upgrade failed",
    };
  }
  const failed = (c.machines ?? []).filter((m) => m.phase === "Failed");
  if (failed.length > 0) {
    const reason = failed.length === 1 ? `Machine ${failed[0]?.name ?? ""} failed` : `${failed.length} machines failed`;
    return { group: "needs", state: "error", reason, view: "machines", verdict: "Degraded" };
  }
  return undefined;
}

// comingUp is how far a cluster that is not available yet is, or why it stopped coming up.
function comingUp(c: Cluster, now: number): Standing {
  if (c.conditions.some((x) => x.type === "TopologyReconciled" && x.reason === "ClusterClassNotReconciled")) {
    return { group: "running", state: "progress", reason: "Waiting for ClusterClass", view: "" };
  }
  if (now - Date.parse(c.created) < provisioningGrace) {
    const cp = c.controlPlane;
    return {
      group: "running",
      state: "progress",
      reason: `Creating: ${cp.ready} of ${cp.desired} control planes`,
      view: "machines",
    };
  }
  const why = c.conditions.find((x) => x.type === "Available" && x.status !== "True");
  return {
    group: "needs",
    state: "error",
    reason: filled(why?.reason) ? `Not available: ${why.reason}` : "Not available",
    view: "health",
    verdict: "Not available",
  };
}

const moving = (reason: string): Standing => ({ group: "running", state: "progress", reason, view: "machines" });

// underWay is the change going on in an available cluster, if one is.
function underWay(c: Cluster): Standing | undefined {
  if (filled(c.talos?.upgrading)) {
    return moving(`Upgrading Talos on ${firstWord(c.talos.upgrading)}`);
  }
  const reconciling = c.conditions.find((x) => x.type === "TopologyReconciled" && x.status !== "True");
  if (reconciling) {
    return moving(reconciling.reason === "ClusterUpgrading" ? `Upgrading to ${c.version}` : "Applying changes");
  }
  if (!steady(c.controlPlane)) {
    return moving(rollout("Control plane", c.controlPlane));
  }
  if (!steady(c.workers)) {
    return moving(rollout("Workers", c.workers));
  }
  const waiting = (c.machines ?? []).filter((m) => !m.ready);
  if (waiting.length > 0) {
    return moving(
      waiting.length === 1 ? `${waiting[0]?.name ?? ""} not ready` : `${waiting.length} machines not ready`,
    );
  }
  return undefined;
}

// finding is the worst its health page found in a cluster, if it found anything.
function finding(h: FleetHealth | undefined): Standing | undefined {
  if (h === undefined) {
    return undefined;
  }
  if (h.verdict === "unreachable") {
    return { group: "needs", state: "error", reason: "API server unreachable", view: "health", verdict: "Unreachable" };
  }
  const [worst] = h.attention;
  if (worst === undefined) {
    return undefined;
  }
  const others = h.attention.length - 1 + (h.more ?? 0);
  const reason = others > 0 ? `${worst.what} and ${others} more` : worst.what;
  if (worst.severity === "progress") {
    return { group: "running", state: "progress", reason, view: "health" };
  }
  return { group: "needs", state: worst.severity, reason, view: "health", verdict: "Degraded" };
}

/** badge is what a cluster's status badge shows: the fleet's verdict where it needs someone. */
export function badge(c: Cluster, s: Standing): { state: Standing["state"]; text: string } {
  return s.verdict === undefined ? health(c) : { state: s.state, text: s.verdict };
}

/** ClusterStatus is a cluster's badge, with what its health page found where the fleet has read it. */
export function ClusterStatus({ cluster }: { cluster: Cluster }): JSX.Element {
  const found = useFleet()?.health?.get(fleetKey(cluster));
  const b = badge(cluster, standing(cluster, found));
  return <Status state={b.state} text={b.text} />;
}

const severity: Record<Standing["state"], number> = { error: 0, warning: 1, progress: 2, deleting: 3, ok: 4 };

/** ranked is the fleet in the rail's order: by group, the worst first within it, then by name. */
export function ranked(
  clusters: Cluster[],
  found?: Map<string, FleetHealth>,
): { cluster: Cluster; standing: Standing }[] {
  const order = new Map(groups.map((g, i) => [g.group, i]));
  return clusters
    .map((cluster) => ({ cluster, standing: standing(cluster, found?.get(fleetKey(cluster))) }))
    .toSorted(
      (a, b) =>
        (order.get(a.standing.group) ?? 0) - (order.get(b.standing.group) ?? 0) ||
        severity[a.standing.state] - severity[b.standing.state] ||
        a.cluster.name.localeCompare(b.cluster.name) ||
        a.cluster.namespace.localeCompare(b.cluster.namespace),
    );
}

/** FleetRail lists every cluster by where it stands, each with why, beside every page. */
export function FleetRail({
  current,
  canCreate,
  onCreate,
}: {
  current: { namespace: string; name: string } | undefined;
  canCreate: boolean | undefined;
  onCreate: () => void;
}): JSX.Element {
  const fleet = useFleet();
  const list = fleet?.clusters === undefined ? undefined : ranked(fleet.clusters, fleet.health);
  return (
    <nav className="rail" aria-label="Fleet">
      <div className="rail-head">
        <h2>Fleet {list !== undefined && <span className="count">{list.length}</span>}</h2>
        <button
          type="button"
          className="icon"
          aria-label="New cluster"
          title={canCreate === false ? "Needs VM.Allocate" : "New cluster (G N)"}
          disabled={canCreate === false}
          onClick={onCreate}
        >
          +
        </button>
      </div>
      {fleet?.error !== undefined && list === undefined && <p className="rail-note">{fleet.error}</p>}
      {list?.length === 0 && <p className="rail-note">No clusters yet</p>}
      {list !== undefined &&
        groups.map(({ group, label }) => {
          const members = list.filter((m) => m.standing.group === group);
          if (members.length === 0) {
            return null;
          }
          return (
            <section key={group} className={`rail-group rail-${group}`} aria-label={label}>
              <h3>
                {label} <span className="count">{members.length}</span>
              </h3>
              <ul>
                {members.map(({ cluster: c, standing: s }) => (
                  <li key={`${c.namespace}/${c.name}`}>
                    <Link
                      to={clusterPath(c.namespace, c.name, s.view)}
                      className="rail-item"
                      current={current?.namespace === c.namespace && current.name === c.name}
                    >
                      <span className={`dot dot-${s.state}`} aria-hidden="true" />
                      <span className="rail-name">
                        {c.namespace !== "default" && <span className="ns">{c.namespace}/</span>}
                        {c.name}
                      </span>
                      <span className="rail-why">{s.reason}</span>
                    </Link>
                  </li>
                ))}
              </ul>
            </section>
          );
        })}
    </nav>
  );
}
