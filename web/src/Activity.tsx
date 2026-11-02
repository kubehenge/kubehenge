import { useState, type JSX } from "react";

import { api } from "./api";
import { clusterPath } from "./cluster-paths";
import { Link } from "./router";
import type { Activity, ActivityEntry, HealthEvent, HealthSection } from "./types";
import { age, Detail, ErrorBox, filled, pollIdle, Skeleton, Status, usePoll } from "./ui";

const windows = [
  { days: 1, label: "Last day" },
  { days: 7, label: "Last 7 days" },
  { days: 30, label: "Last 30 days" },
  { days: 90, label: "Last 90 days" },
] as const;

// Details the page leaves out: the ids match an entry to a line in the journal or the audit log.
const hiddenDetails = new Set(["requestId", "auditId"]);

/** ApplianceActivity is everything done through the appliance's interface. */
export function ApplianceActivity(): JSX.Element {
  return <ActivityPanel load={api.activity} withCluster />;
}

/** ClusterActivity is what was done to one cluster. */
export function ClusterActivity({ cluster: c }: { cluster: { namespace: string; name: string } }): JSX.Element {
  return <ActivityPanel load={(days) => api.clusterActivity(c.namespace, c.name, days)} withCluster={false} />;
}

/**
 * ActivityPanel lists who did what through the interface over a chosen window, and the management
 * cluster's events of the last day. withCluster adds the cluster each entry was about.
 */
function ActivityPanel({
  load,
  withCluster,
}: {
  load: (days: number) => Promise<Activity>;
  withCluster: boolean;
}): JSX.Element {
  const [days, setDays] = useState<number>(windows[1].days);
  const choice = (
    <label className="filter">
      Show{" "}
      <select value={days} onChange={(e) => setDays(Number(e.target.value))}>
        {windows.map((w) => (
          <option key={w.days} value={w.days}>
            {w.label}
          </option>
        ))}
      </select>
    </label>
  );
  return <Read key={days} load={() => load(days)} withCluster={withCluster} choice={choice} />;
}

function Read({
  load,
  withCluster,
  choice,
}: {
  load: () => Promise<Activity>;
  withCluster: boolean;
  choice: JSX.Element;
}): JSX.Element {
  const { data, error } = usePoll(load, pollIdle);
  return (
    <>
      <div className="box scroll">
        <div className="section-head">
          <h3>Who did what</h3>
          {choice}
        </div>
        {!data && (error === undefined ? <Skeleton rows={6} /> : <ErrorBox message={error} />)}
        {data && <ErrorBox message={data.error} />}
        {data && <Entries entries={data.entries} withCluster={withCluster} />}
      </div>
      {data && <Events events={data.events} />}
    </>
  );
}

function When({ at }: { at: string }): JSX.Element {
  return <td title={new Date(at).toLocaleString()}>{age(at)}</td>;
}

function Entries({ entries, withCluster }: { entries: ActivityEntry[]; withCluster: boolean }): JSX.Element {
  const columns = withCluster ? ["When", "Who", "What", "Cluster"] : ["When", "Who", "What"];
  return (
    <table className="grid">
      <thead>
        <tr>
          {columns.map((c) => (
            <th key={c}>{c}</th>
          ))}
        </tr>
      </thead>
      <tbody>
        {entries.length === 0 && (
          <tr>
            <td colSpan={columns.length} className="muted">
              Nothing done
            </td>
          </tr>
        )}
        {entries.map((e) => (
          <tr key={`${e.at}/${e.user}/${e.what}/${e.cluster ?? ""}`} className={refused(e) ? "attention" : ""}>
            <When at={e.at} />
            <td>{e.user}</td>
            <td>
              {e.what.charAt(0).toUpperCase() + e.what.slice(1)}
              {e.failed === true && (
                <div>
                  <Status state="error" text="Failed" />
                </div>
              )}
              <Details details={e.details} />
            </td>
            {withCluster && <td>{filled(e.cluster) && <ClusterLink at={e.cluster} />}</td>}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// refused tells an attempt turned down or failed: a refused sign-in or second factor, a change the
// API server refused, or an action that ended in an error.
const refused = (e: ActivityEntry): boolean =>
  e.what.endsWith(" refused") || filled(e.details?.["refused"]) || e.failed === true;

function Details({ details }: { details: Record<string, string> | undefined }): JSX.Element | null {
  const shown = Object.entries(details ?? {}).filter(([k]) => !hiddenDetails.has(k));
  if (shown.length === 0) {
    return null;
  }
  return <div className="hint">{shown.map(([k, v]) => `${k} ${v}`).join(", ")}</div>;
}

function ClusterLink({ at }: { at: string }): JSX.Element {
  const [namespace = "", name = ""] = at.split("/");
  return <Link to={clusterPath(namespace, name)}>{name}</Link>;
}

function Events({ events }: { events: HealthSection<HealthEvent> }): JSX.Element {
  return (
    <div className="box scroll">
      <h3>Last day&apos;s events</h3>
      {filled(events.error) && (
        <div className="warning">
          Couldn&apos;t read events
          <Detail>{events.error}</Detail>
        </div>
      )}
      {events.truncated === true && <p className="muted">First {events.items.length} shown</p>}
      <table className="grid">
        <thead>
          <tr>
            <th>Last</th>
            <th>Reason</th>
            <th>Object</th>
            <th>Message</th>
            <th>Count</th>
          </tr>
        </thead>
        <tbody>
          {events.items.length === 0 && (
            <tr>
              <td colSpan={5} className="muted">
                No events
              </td>
            </tr>
          )}
          {events.items.map((e) => (
            <tr key={`${e.object}/${e.reason}/${e.message}`} className={e.type === "Warning" ? "attention" : ""}>
              <When at={e.last} />
              <td>{e.reason}</td>
              <td>{e.object}</td>
              <td className="message">{e.message}</td>
              <td>{e.count}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
