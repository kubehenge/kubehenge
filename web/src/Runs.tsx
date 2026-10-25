import type { JSX } from "react";

import { clusterPath } from "./ClusterDetail";
import { ranked, useFleet, type Standing } from "./fleet";
import { Link } from "./router";
import type { Cluster } from "./types";

/** A piece of work under way somewhere in the fleet. */
export interface Run {
  key: string;
  title: string;
  what: string;
  state: Standing["state"];
  to: string;
}

/** useRuns is the work under way, as the fleet's clusters show it. */
export function useRuns(): Run[] {
  const fleet = useFleet();
  return ranked(fleet?.clusters ?? [])
    .filter(({ standing }) => standing.group === "running")
    .map(({ cluster: c, standing: s }: { cluster: Cluster; standing: Standing }) => ({
      key: `${c.namespace}/${c.name}`,
      title: c.namespace === "default" ? c.name : `${c.namespace}/${c.name}`,
      what: s.reason,
      state: s.state,
      to: clusterPath(c.namespace, c.name, s.state === "deleting" ? "" : "machines"),
    }));
}

/** RunsPanel follows the work under way beside whatever page is open, and folds away. */
export function RunsPanel({ runs, onFold }: { runs: Run[]; onFold: () => void }): JSX.Element {
  return (
    <aside className="runs" aria-label="Running">
      <div className="runs-head">
        <h2>
          Running <span className="count">{runs.length}</span>
        </h2>
        <button
          type="button"
          className="icon"
          aria-label="Fold the running work away"
          title="Fold away (R)"
          onClick={onFold}
        >
          ›
        </button>
      </div>
      {runs.length === 0 ? (
        <p className="rail-note">Nothing is running.</p>
      ) : (
        <ul>
          {runs.map((r) => (
            <li key={r.key}>
              <Link to={r.to} className="run">
                <span className={`dot dot-${r.state}`} aria-hidden="true" />
                <span className="run-title">{r.title}</span>
                <span className="run-what">{r.what}</span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </aside>
  );
}
