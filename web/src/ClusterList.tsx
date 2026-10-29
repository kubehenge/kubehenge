import { useEffect, useRef, type JSX } from "react";

import { clusterPath } from "./ClusterDetail";
import { FleetProvider, badge, fleetKey, standing, useFleet } from "./fleet";
import { Link, navigate } from "./router";
import type { Cluster, FleetHealth } from "./types";
import { ErrorBox, Status, age, distributionName, filled } from "./ui";

// The privilege creating a cluster needs, as Proxmox VE names it.
const createNeeds = "Creating clusters needs VM.Allocate on the workload pool.";

export function ClusterList({ canCreate }: { canCreate?: boolean | undefined }): JSX.Element {
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
      <FleetList canCreate={canCreate} />
    </FleetProvider>
  ) : (
    <FleetList canCreate={canCreate} />
  );
}

function FleetList({ canCreate }: { canCreate: boolean | undefined }): JSX.Element {
  const fleet = useFleet();
  const clusters = fleet?.clusters;
  const error = fleet?.error;
  const refused = canCreate === false;

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
          aria-describedby={refused ? "create-needs" : undefined}
          onClick={() => navigate("/clusters/new")}
        >
          Create cluster
        </button>
      </div>
      {refused && (
        <p className="muted pad" id="create-needs">
          {createNeeds}
        </p>
      )}
      <ErrorBox message={error} />
      {clusters === undefined ? (
        error === undefined && <p className="muted pad">Loading…</p>
      ) : (
        <Clusters clusters={clusters} found={fleet?.health} />
      )}
    </section>
  );
}

function Clusters({
  clusters,
  found,
}: {
  clusters: Cluster[];
  found: Map<string, FleetHealth> | undefined;
}): JSX.Element {
  if (clusters.length === 0) {
    return <p className="muted pad">No clusters yet. Seeing one needs VM.Audit on its pool.</p>;
  }
  return (
    <div className="fleet-table">
      <table className="grid">
        <thead>
          <tr>
            <th>Cluster</th>
            <th>Status</th>
            <th>Kubernetes</th>
            <th className="number">Control plane</th>
            <th className="number">Workers</th>
            <th className="fleet-endpoint">Endpoint</th>
            <th className="fleet-age">Age</th>
          </tr>
        </thead>
        <tbody>
          {clusters.map((c) => {
            const s = standing(c, found?.get(fleetKey(c)));
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
