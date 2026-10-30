import { useEffect, useMemo, useRef, useState, type JSX } from "react";

import { clusterPath } from "./ClusterDetail";
import { FirstCluster } from "./FirstCluster";
import { FleetProvider, badge, fleetKey, groups, ranked, useFleet, type Group, type Standing } from "./fleet";
import { score } from "./Palette";
import { Link, navigate } from "./router";
import type { Cluster, FleetHealth } from "./types";
import { ErrorBox, Status, age, distributionName, filled } from "./ui";

// The privilege creating a cluster needs, as Proxmox VE names it.
const createNeeds = "Creating clusters needs VM.Allocate on the workload pool.";

export function ClusterList({
  canCreate,
  canManage = false,
}: {
  canCreate?: boolean | undefined;
  canManage?: boolean;
}): JSX.Element {
  const fleet = useFleet();
  // Opening the list shows the fleet as it is now, not as the console last read it.
  const readBefore = useRef(fleet?.clusters !== undefined);
  const reload = fleet?.reload;
  useEffect(() => {
    if (readBefore.current) {
      void reload?.();
    }
  }, [reload]);
  // The console reads the fleet for the rail beside the list; on its own the list reads it itself.
  return fleet === null ? (
    <FleetProvider>
      <FleetList canCreate={canCreate} canManage={canManage} />
    </FleetProvider>
  ) : (
    <FleetList canCreate={canCreate} canManage={canManage} />
  );
}

function FleetList({ canCreate, canManage }: { canCreate: boolean | undefined; canManage: boolean }): JSX.Element {
  const fleet = useFleet();
  const clusters = fleet?.clusters;
  const error = fleet?.error;
  const refused = canCreate === false;
  const first = clusters?.length === 0;

  const listed = (shown: Cluster[]): JSX.Element =>
    shown.length === 0 ? (
      <FirstCluster canCreate={canCreate} canManage={canManage} />
    ) : (
      <Clusters clusters={shown} found={fleet?.health} />
    );
  return (
    <section className="panel">
      <div className="toolbar">
        <h2>Clusters</h2>
        <span className="spacer" />
        <button
          type="button"
          className="primary"
          disabled={refused}
          title={refused ? createNeeds : undefined}
          aria-describedby={refused && !first ? "create-needs" : undefined}
          onClick={() => navigate("/clusters/new")}
        >
          Create cluster
        </button>
      </div>
      {refused && !first && (
        <p className="muted pad" id="create-needs">
          {createNeeds}
        </p>
      )}
      <ErrorBox message={error} />
      {clusters === undefined ? error === undefined && <p className="muted pad">Loading…</p> : listed(clusters)}
    </section>
  );
}

type SortKey = "status" | "name" | "version" | "age";

interface Sort {
  key: SortKey;
  descending: boolean;
}

// The list's filter, group and order live in the address, so a filtered fleet can be shared.
function readView(): { query: string; show: Group | ""; sort: Sort } {
  const p = new URLSearchParams(window.location.search);
  const show = groups.find((g) => g.group === p.get("show"))?.group ?? "";
  const key = (["status", "name", "version", "age"] as const).find((k) => k === p.get("sort")) ?? "status";
  return { query: p.get("q") ?? "", show, sort: { key, descending: p.get("order") === "desc" } };
}

function writeView(query: string, show: Group | "", sort: Sort): void {
  const p = new URLSearchParams();
  if (query !== "") {
    p.set("q", query);
  }
  if (show !== "") {
    p.set("show", show);
  }
  if (sort.key !== "status" || sort.descending) {
    p.set("sort", sort.key);
    p.set("order", sort.descending ? "desc" : "asc");
  }
  const search = p.size > 0 ? `?${p.toString()}` : "";
  if (search !== window.location.search) {
    window.history.replaceState(window.history.state, "", `${window.location.pathname}${search}`);
  }
}

const byVersion = new Intl.Collator(undefined, { numeric: true });

function SortHeader({
  label,
  by,
  sort,
  onSort,
  className,
}: {
  label: string;
  by: SortKey;
  sort: Sort;
  onSort: (s: Sort) => void;
  className?: string;
}): JSX.Element {
  const on = sort.key === by;
  const direction = sort.descending ? "descending" : "ascending";
  const mark = sort.descending ? "↓" : "↑";
  return (
    <th className={className} aria-sort={on ? direction : undefined}>
      <button type="button" className="sort" onClick={() => onSort({ key: by, descending: on && !sort.descending })}>
        {label}
        <span className="sort-mark" aria-hidden="true">
          {on ? mark : ""}
        </span>
      </button>
    </th>
  );
}

function Clusters({
  clusters,
  found,
}: {
  clusters: Cluster[];
  found: Map<string, FleetHealth> | undefined;
}): JSX.Element {
  const [query, setQuery] = useState(() => readView().query);
  const [show, setShow] = useState<Group | "">(() => readView().show);
  const [sort, setSort] = useState<Sort>(() => readView().sort);
  useEffect(() => writeView(query, show, sort), [query, show, sort]);

  const rows = useMemo(() => {
    const all = ranked(clusters, found);
    const rank = new Map(all.map((r, i) => [fleetKey(r.cluster), i]));
    const counts = new Map<Group, number>(
      groups.map((g) => [g.group, all.filter((r) => r.standing.group === g.group).length]),
    );
    const shown = all
      .filter((r) => show === "" || r.standing.group === show)
      .filter((r) => {
        const c = r.cluster;
        return score(query, `${c.namespace}/${c.name} ${c.version} ${distributionName(c.distribution)}`) >= 0;
      });
    const orders: Record<SortKey, (a: Cluster, b: Cluster) => number> = {
      name: (a, b) => fleetKey(a).localeCompare(fleetKey(b)),
      version: (a, b) => byVersion.compare(a.version, b.version),
      age: (a, b) => Date.parse(b.created) - Date.parse(a.created),
      status: (a, b) => (rank.get(fleetKey(a)) ?? 0) - (rank.get(fleetKey(b)) ?? 0),
    };
    const order = (a: (typeof all)[number], b: (typeof all)[number]): number => orders[sort.key](a.cluster, b.cluster);
    return {
      counts,
      total: all.length,
      shown: shown.toSorted((a, b) => (sort.descending ? -order(a, b) : order(a, b))),
    };
  }, [clusters, found, query, show, sort]);

  // The group's own button counts what it shows; a typed filter needs the count said.
  const narrowed = query !== "";
  return (
    <>
      <div className="fleet-controls">
        <label className="filter">
          Filter <input type="search" value={query} onChange={(e) => setQuery(e.target.value)} spellCheck={false} />
        </label>
        <fieldset className="segments">
          <legend className="visually-hidden">Show</legend>
          <button type="button" aria-pressed={show === ""} onClick={() => setShow("")}>
            All <span className="count">{rows.total}</span>
          </button>
          {groups.map((g) => (
            <button key={g.group} type="button" aria-pressed={show === g.group} onClick={() => setShow(g.group)}>
              {g.label} <span className="count">{rows.counts.get(g.group) ?? 0}</span>
            </button>
          ))}
        </fieldset>
      </div>
      {rows.shown.length === 0 ? (
        <div className="pad fleet-none">
          <span className="muted">No clusters match</span>
          <button
            type="button"
            onClick={() => {
              setQuery("");
              setShow("");
            }}
          >
            Clear filter
          </button>
        </div>
      ) : (
        <ClusterTable rows={rows.shown} sort={sort} onSort={setSort} />
      )}
      {narrowed && rows.shown.length > 0 && (
        <p className="muted pad fleet-shown">
          {rows.shown.length} of {rows.total}
        </p>
      )}
    </>
  );
}

function ClusterTable({
  rows,
  sort,
  onSort,
}: {
  rows: { cluster: Cluster; standing: Standing }[];
  sort: Sort;
  onSort: (s: Sort) => void;
}): JSX.Element {
  return (
    <div className="fleet-table">
      <table className="grid">
        <thead>
          <tr>
            <SortHeader label="Cluster" by="name" sort={sort} onSort={onSort} />
            <SortHeader label="Status" by="status" sort={sort} onSort={onSort} />
            <SortHeader label="Kubernetes" by="version" sort={sort} onSort={onSort} />
            <th className="number">Control plane</th>
            <th className="number">Workers</th>
            <th className="fleet-endpoint">Endpoint</th>
            <SortHeader label="Age" by="age" sort={sort} onSort={onSort} className="fleet-age" />
          </tr>
        </thead>
        <tbody>
          {rows.map(({ cluster: c, standing: s }) => {
            const b = badge(c, s);
            const id = `${c.namespace}/${c.name}`;
            return (
              <tr key={id}>
                <td className="id fleet-name">
                  <span title={id}>
                    {c.namespace !== "default" && <span className="muted">{c.namespace}/</span>}
                    <Link to={clusterPath(c.namespace, c.name)}>{c.name}</Link>
                  </span>
                </td>
                <td className="state fleet-status">
                  <Status state={b.state} text={b.text} />
                  {s.group !== "healthy" && s.reason !== b.text && <div className="hint">{s.reason}</div>}
                </td>
                <td className="nowrap fleet-version">
                  <span className="id">{c.version}</span>
                  <div className="hint">
                    {distributionName(c.distribution)}
                    {c.talos && ` ${c.talos.version}`}
                  </div>
                </td>
                <td className="number" data-label="Control plane">
                  {c.controlPlane.ready}/{c.controlPlane.desired}
                </td>
                <td className="number" data-label="Workers">
                  {c.workers.ready}/{c.workers.desired}
                </td>
                <td className="id nowrap fleet-endpoint">{c.endpoint ?? ""}</td>
                <td
                  className="nowrap fleet-age"
                  title={`${new Date(c.created).toLocaleString()}${filled(c.createdBy) ? ` by ${c.createdBy}` : ""}`}
                >
                  {age(c.created)}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
