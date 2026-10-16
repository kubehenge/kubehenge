import type { JSX } from "react";

import { api } from "./api";
import { clusterPath } from "./ClusterDetail";
import { Link, navigate } from "./router";
import type { Cluster } from "./types";
import { ErrorBox, StatusBadge, age, distributionName, pollIdle, settled, usePoll } from "./ui";

// The privilege creating a cluster needs, as Proxmox VE names it.
const createNeeds = "Creating a cluster needs VM.Allocate on Kubehenge's workload pool.";

// How often the list asks again while any cluster is changing.
const pollChanging = 10_000;

export function ClusterList({ canCreate }: { canCreate?: boolean | undefined }): JSX.Element {
  const { data: clusters, error } = usePoll(api.clusters, (list) =>
    list?.every((c) => settled(c)) === true ? pollIdle : pollChanging,
  );
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
        <Clusters clusters={clusters} refused={refused} />
      )}
    </section>
  );
}

function Clusters({ clusters, refused }: { clusters: Cluster[]; refused: boolean }): JSX.Element {
  if (clusters.length === 0) {
    return (
      <p className="muted pad">
        No clusters yet. {refused ? "" : "Create one, or "}
        {refused ? "To see one, ask" : "ask"} an administrator for VM.Audit on its pool.
      </p>
    );
  }
  return (
    <div className="scroll">
      <table className="grid">
        <thead>
          <tr>
            <th>Name</th>
            <th>Status</th>
            <th>Distribution</th>
            <th>Version</th>
            <th>Control plane</th>
            <th>Workers</th>
            <th>Endpoint</th>
            <th>Created by</th>
            <th>Age</th>
          </tr>
        </thead>
        <tbody>
          {clusters.map((c) => (
            <tr key={`${c.namespace}/${c.name}`}>
              <td>
                <Link to={clusterPath(c.namespace, c.name)}>{c.name}</Link>
                {c.namespace !== "default" && <span className="muted"> ({c.namespace})</span>}
              </td>
              <td>
                <StatusBadge cluster={c} />
              </td>
              <td>{distributionName(c.distribution)}</td>
              <td>{c.version}</td>
              <td>
                {c.controlPlane.ready}/{c.controlPlane.desired}
              </td>
              <td>
                {c.workers.ready}/{c.workers.desired}
              </td>
              <td>{c.endpoint ?? ""}</td>
              <td>{c.createdBy ?? ""}</td>
              <td title={new Date(c.created).toLocaleString()}>{age(c.created)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
