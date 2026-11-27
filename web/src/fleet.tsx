import { createContext, use, useCallback, useEffect, useMemo, useRef, useState, type JSX, type ReactNode } from "react";

import { api } from "./api";
import { clusterPath, type View } from "./cluster-paths";
import { keptCache } from "./kept-state";
import { useClusterChanges } from "./live";
import { Link } from "./router";
import type { Cluster, FleetHealth, Replicas, RunInfo } from "./types";
import { ReadError, Skeleton, Status, distributionName, filled, pollIdle, settled, usePoll } from "./ui";

// How often the fleet is read again while any cluster is changing.
const pollChanging = 10_000;

// A cluster that is still not available an hour after it was created is no longer coming up on
// its own.
const provisioningGrace = 3_600_000;

/** The clusters the user may see, read once for the whole page: the rail, the list and the runs. */
export interface Fleet {
  clusters: Cluster[] | undefined;
  /** How each cluster's health page sums it up, by cluster key. Undefined until read. */
  health: Map<string, FleetHealth> | undefined;
  /** The work running or lately finished that the user may see. Undefined until read. */
  runs: RunInfo[] | undefined;
  error: string | undefined;
  /** How the health read stands: why it failed, if it did, and how to read it again. */
  healthRead: ReadOf;
  /** The same for the runs. */
  runsRead: ReadOf;
  /** Whether the clusters shown may no longer hold, as usePoll tells. */
  stale: boolean;
  /** Whether what is shown is what this tab kept from before a refresh, not yet read again. */
  kept: boolean;
  reload: () => Promise<void>;
  /** When the clusters are next read. */
  next: () => number;
  /** show puts a cluster as a request just returned it into the list, before the list is read again. */
  show: (c: Cluster) => void;
}

/** What a failed read offers: its message, when it is next read, and reading it now. */
export interface ReadOf {
  error: string | undefined;
  /** What is shown was read before and a later read failed. */
  stale: boolean;
  next: () => number;
  reload: () => Promise<void>;
}

const withCluster = (list: Cluster[] | undefined, c: Cluster): Cluster[] | undefined =>
  list && [...list.filter((x) => fleetKey(x) !== fleetKey(c)), c];

const FleetContext = createContext<Fleet | null>(null);

export const fleetKey = (c: { namespace: string; name: string }): string => `${c.namespace}/${c.name}`;

// The fleet as this tab last read it, shown at once after a refresh.
const keptClusters = keptCache<Cluster[]>("clusters");
const keptHealth = keptCache<FleetHealth[]>("fleet-health");

// Until the appliance is set up there is no fleet to read, and what the tab kept stays shown.
const noFleet =
  <T,>(cache: Map<string, T>) =>
  (): Promise<T | undefined> =>
    Promise.resolve(cache.get(""));
const noRuns = (): Promise<undefined> => Promise.resolve(undefined);

/**
 * FleetProvider reads the fleet for everything inside it, once `reads` is on. It stays in place
 * while it is off, so the page round it is not built again when reading starts.
 */
export function FleetProvider({ reads = true, children }: { reads?: boolean; children: ReactNode }): JSX.Element {
  // Which kept answers show and have not been read again since.
  const [kept, setKept] = useState(() => ({ clusters: keptClusters.has(""), health: keptHealth.has("") }));
  const readClusters = useCallback(async () => {
    const list = await api.clusters();
    setKept((k) => (k.clusters ? { ...k, clusters: false } : k));
    return list;
  }, []);
  const readHealth = useCallback(async () => {
    const found = await api.fleetHealth();
    setKept((k) => (k.health ? { ...k, health: false } : k));
    return found;
  }, []);
  const { data, error, stale, reload, next, put } = usePoll<Cluster[] | undefined>(
    reads ? readClusters : noFleet(keptClusters),
    (list) => (list?.every((c) => settled(c)) === true ? pollIdle : pollChanging),
    { cache: keptClusters, key: "" },
  );
  // The clusters show without their health, which only adds why some need someone.
  const {
    data: read,
    error: healthError,
    stale: healthStale,
    next: nextHealth,
    reload: reloadHealth,
  } = usePoll<FleetHealth[] | undefined>(reads ? readHealth : noFleet(keptHealth), pollIdle, {
    cache: keptHealth,
    key: "",
  });
  const {
    data: runs,
    error: runsError,
    stale: runsStale,
    next: nextRuns,
    reload: reloadRuns,
  } = usePoll<RunInfo[] | undefined>(reads ? api.runs : noRuns, (list) =>
    list?.some((r) => r.state === "running" || r.state === "stalled") === true ? pollChanging : pollIdle,
  );
  useClusterChanges(() => {
    void reload();
    void reloadRuns();
  }, reads);
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
  // Not read again at once: the server's view of the clusters may not have the change yet, and the
  // list polls often while a cluster is not settled anyway.
  const show = useCallback(
    (c: Cluster) => {
      put((list) => withCluster(list, c));
      void reloadRuns();
    },
    [put, reloadRuns],
  );
  const fleet = useMemo(
    () => ({
      clusters: data,
      health: read && new Map(read.map((h) => [fleetKey(h), h])),
      runs,
      error,
      healthRead: { error: healthError, stale: healthStale, next: nextHealth, reload: reloadHealth },
      runsRead: { error: runsError, stale: runsStale, next: nextRuns, reload: reloadRuns },
      stale,
      kept: kept.clusters || kept.health,
      reload,
      next,
      show,
    }),
    [
      data,
      read,
      runs,
      error,
      healthError,
      healthStale,
      nextHealth,
      reloadHealth,
      runsError,
      runsStale,
      nextRuns,
      reloadRuns,
      stale,
      kept,
      reload,
      next,
      show,
    ],
  );
  return <FleetContext value={fleet}>{children}</FleetContext>;
}

/** useFleet is the fleet the page reads, or null outside a FleetProvider. */
export function useFleet(): Fleet | null {
  return use(FleetContext);
}

/** Where a cluster stands in the fleet: whether it needs someone, is on its way, or is well. */
export type Group = "needs" | "running" | "healthy" | "unknown";

export interface Standing {
  group: Group;
  state: "ok" | "progress" | "error" | "warning" | "deleting" | "idle";
  /** Why it stands there, in a few words. */
  reason: string;
  /** The view of the cluster's page that shows more of why. */
  view: View;
  /** What its status badge says, so that the badge, the rail and the header never disagree. */
  verdict: string;
}

export const groups: { group: Group; label: string }[] = [
  { group: "needs", label: "Needs you" },
  { group: "running", label: "Running" },
  { group: "healthy", label: "Healthy" },
  { group: "unknown", label: "Unknown" },
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
    if (
      c.deletion?.abandoned === undefined &&
      c.conditions.some((x) => x.type === "DeletionStalled" && x.status === "True")
    ) {
      return { group: "needs", state: "warning", reason: "Deletion stalled", view: "", verdict: "Deletion stalled" };
    }
    return { group: "running", state: "deleting", reason: "Deleting", view: "", verdict: "Deleting" };
  }
  return (
    failure(c) ??
    (c.available ? underWay(c) : comingUp(c, now)) ??
    finding(found) ?? {
      group: "healthy",
      state: "ok",
      reason: restingReason(c),
      view: "",
      verdict: "Available",
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
  if (filled(c.controllerError)) {
    return { group: "needs", state: "error", reason: "Controller error", view: "", verdict: "Error" };
  }
  if (filled(c.stalled)) {
    return {
      group: "needs",
      state: "warning",
      reason: "Machines stuck for 20 minutes",
      view: "machines",
      verdict: "Stuck",
    };
  }
  return undefined;
}

// comingUp is how far a cluster that is not available yet is, or why it stopped coming up.
function comingUp(c: Cluster, now: number): Standing {
  if (c.conditions.some((x) => x.type === "TopologyReconciled" && x.reason === "ClusterClassNotReconciled")) {
    return { group: "running", state: "progress", reason: "Waiting for ClusterClass", view: "", verdict: "Waiting" };
  }
  if (now - Date.parse(c.created) < provisioningGrace) {
    const cp = c.controlPlane;
    return {
      group: "running",
      state: "progress",
      reason: `Creating: ${cp.ready} of ${cp.desired} control planes`,
      view: "machines",
      verdict: "Creating",
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

const moving = (reason: string, verdict = "Updating"): Standing => ({
  group: "running",
  state: "progress",
  reason,
  view: "machines",
  verdict,
});

/** The condition Kubehenge sets while it holds back the Kubernetes upgrade a cluster asks for. */
export const upgradeHeldCondition = "UpgradeHeld";

// The reasons of upgradeHeldCondition for which Rancher holds the upgrade.
const rancherHolds = new Set(["RancherKubernetes", "RancherElsewhere"]);

// held is why the cluster's upgrade is held back, if it is.
function held(c: Cluster): Standing | undefined {
  const hold = c.conditions.find((x) => x.type === upgradeHeldCondition && x.status === "True");
  if (hold === undefined) {
    return undefined;
  }
  if (hold.reason === "TalosUpgrade") {
    return moving("Upgrade waits for Talos", "Upgrading");
  }
  const reason = rancherHolds.has(hold.reason ?? "") ? "Upgrade held for Rancher" : "Upgrade held";
  return { group: "needs", state: "warning", reason, view: "health", verdict: "Upgrade held" };
}

/** The condition Kubehenge sets while an add-on is not installed because the cluster already holds that software. */
export const addonConflictCondition = "AddonConflict";

/** The condition Kubehenge sets while add-ons wait, and its reason when only a volume they need is missing. */
export const addonsWaitingCondition = "AddonsWaiting";
export const volumeMissingReason = "VolumeMissing";

/** The condition Kubehenge sets while add-on chart changes wait for the upgrade window. */
export const addonChangesPendingCondition = "AddonChangesPending";

const isTrue = (c: Cluster, type: string): boolean => c.conditions.some((x) => x.type === type && x.status === "True");

/** windowTime is when an upgrade window opens, to the minute. */
export const windowTime = (iso: string): string =>
  new Date(iso).toLocaleString(undefined, { dateStyle: "short", timeStyle: "short" });

// windowShort is windowTime without the year, for the rail's narrow lines.
const windowShort = (iso: string): string =>
  new Date(iso).toLocaleString(undefined, { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });

// addonStanding is what the add-ons hold up: a conflict, or a volume an add-on cannot get, which
// needs someone. Changes that wait for the upgrade window leave the cluster as it is, since nothing
// runs until the window opens.
function addonStanding(c: Cluster): Standing | undefined {
  if (isTrue(c, addonConflictCondition)) {
    return { group: "needs", state: "warning", reason: "Add-on conflict", view: "", verdict: "Add-on conflict" };
  }
  const waiting = c.conditions.some(
    (x) => x.type === addonsWaitingCondition && x.status === "True" && x.reason === volumeMissingReason,
  );
  return waiting
    ? { group: "needs", state: "warning", reason: "Add-ons wait for a volume", view: "health", verdict: "Waiting" }
    : undefined;
}

// restingReason is the line of a cluster that is available and has nothing going on.
function restingReason(c: Cluster): string {
  if (isTrue(c, addonChangesPendingCondition) && c.addonChanges !== undefined) {
    return `Add-on changes ${windowShort(c.addonChanges.opens)}`;
  }
  return c.talos ? `Talos ${c.talos.version}, ${c.version}` : `${distributionName(c.distribution)} ${c.version}`;
}

// underWay is the change going on in an available cluster, if one is.
function underWay(c: Cluster): Standing | undefined {
  if (filled(c.talos?.upgrading)) {
    return moving(`Upgrading Talos on ${firstWord(c.talos.upgrading)}`, "Upgrading");
  }
  const hold = held(c) ?? addonStanding(c);
  if (hold !== undefined) {
    return hold;
  }
  const reconciling = c.conditions.find((x) => x.type === "TopologyReconciled" && x.status !== "True");
  if (reconciling?.reason === "ClusterUpgrading") {
    return moving(`Upgrading to ${c.version}`, "Upgrading");
  }
  if (reconciling) {
    return moving("Applying changes");
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
  // The backups view, not the health page, has the details of the cluster's backups.
  const view: View = worst.section === "backups" ? "backups" : "health";
  if (worst.severity === "progress") {
    return { group: "running", state: "progress", reason, view, verdict: "Updating" };
  }
  return { group: "needs", state: worst.severity, reason, view, verdict: "Degraded" };
}

/**
 * ClusterStatus is a cluster's badge, with what its health page found where the fleet has read it.
 * Unread, as when the cluster's own read failed, it is Unknown whatever the fleet list says.
 */
export function ClusterStatus({
  cluster,
  unread: notRead = false,
}: {
  cluster: Cluster;
  unread?: boolean;
}): JSX.Element {
  const fleet = useFleet();
  if (notRead) {
    return <Status state="idle" text="Unknown" />;
  }
  // Without a fleet, as on a page shown on its own, no health has been read.
  const s = standingIn(cluster, fleet?.health, fleet?.healthRead.error !== undefined);
  return fleet?.kept === true ? (
    <span className="status-kept" aria-busy="true">
      <Status state={s.state} text={s.verdict} />
    </span>
  ) : (
    <Status state={s.state} text={s.verdict} />
  );
}

/** groupLabel names a group: the unknown one says Loading while the health is still being read. */
export const groupLabel = (g: { group: Group; label: string }, failed: boolean): string =>
  g.group === "unknown" && !failed ? "Loading" : g.label;

/** healthReadAt is when the newest health shown was read. */
export function healthReadAt(health: Map<string, FleetHealth> | undefined): string | undefined {
  const times = [...(health?.values() ?? [])].map((h) => h.read).toSorted();
  return times.at(-1);
}

const severity: Record<Standing["state"], number> = { error: 0, warning: 1, progress: 2, deleting: 3, ok: 4, idle: 5 };

// unread is where a cluster stands that its own answer calls well, while its health is not read: a
// cluster is known to be well only once its health page has said so.
const unread = (failed: boolean): Standing => ({
  group: "unknown",
  state: "idle",
  reason: failed ? "Health not read" : "Reading health",
  view: "",
  verdict: failed ? "Unknown" : "Loading…",
});

// standingIn is standing in the fleet, where a missing health read leaves a well cluster unknown. A
// cluster that a health answer left out is not being read: it stays unknown until one names it.
function standingIn(c: Cluster, health: Map<string, FleetHealth> | undefined, failed: boolean): Standing {
  const found = health?.get(fleetKey(c));
  const s = standing(c, found);
  return found === undefined && s.group === "healthy" ? unread(failed || health !== undefined) : s;
}

/** ranked is the fleet in the rail's order: by group, the worst first within it, then by name. */
export function ranked(
  clusters: Cluster[],
  found?: Map<string, FleetHealth>,
  failed = false,
): { cluster: Cluster; standing: Standing }[] {
  const order = new Map(groups.map((g, i) => [g.group, i]));
  return clusters
    .map((cluster) => ({ cluster, standing: standingIn(cluster, found, failed) }))
    .toSorted(
      (a, b) =>
        (order.get(a.standing.group) ?? 0) - (order.get(b.standing.group) ?? 0) ||
        severity[a.standing.state] - severity[b.standing.state] ||
        a.cluster.name.localeCompare(b.cluster.name) ||
        a.cluster.namespace.localeCompare(b.cluster.namespace),
    );
}

// RailRead is the rail's loading look while the clusters are read, and the error of a read that failed.
function RailRead({ fleet, listed }: { fleet: Fleet; listed: boolean }): JSX.Element | null {
  if (!listed) {
    return fleet.error === undefined ? (
      <Skeleton rows={4} />
    ) : (
      <div className="rail-note">
        <ReadError read={fleet} />
      </div>
    );
  }
  if (fleet.healthRead.error === undefined) {
    return null;
  }
  return (
    <div className="rail-note">
      <ReadError read={fleet.healthRead} />
    </div>
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
  const list =
    fleet?.clusters === undefined
      ? undefined
      : ranked(fleet.clusters, fleet.health, fleet.healthRead.error !== undefined);
  // The last list read stays shown, greyed, once it or its health may no longer hold, and so does
  // what the tab kept until it is read again.
  const stale = (fleet?.stale === true || fleet?.healthRead.stale === true) && list !== undefined;
  const kept = fleet?.kept === true && list !== undefined;
  const failed = fleet?.healthRead.error !== undefined;
  return (
    <nav className={stale || kept ? "rail rail-stale" : "rail"} aria-label="Fleet" aria-busy={kept}>
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
      {fleet !== null && <RailRead fleet={fleet} listed={list !== undefined} />}
      {stale && (
        <p className="rail-note hint-warning" title={fleet.error ?? fleet.healthRead.error}>
          Stale
        </p>
      )}
      {list?.length === 0 && <p className="rail-note">No clusters yet</p>}
      {list !== undefined &&
        groups.map((g) => {
          const { group } = g;
          const label = groupLabel(g, failed || fleet?.health !== undefined);
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
