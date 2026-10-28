import { useEffect, type JSX } from "react";

import { adminPath, adminViews, type AdminView } from "./admin-views";
import { AdminPage } from "./AdminPages";
import { api } from "./api";
import { ManagementBehind } from "./ManagementUpgrade";
import { Link } from "./router";
import { ErrorBox, pollIdle, pollRunning, usePoll } from "./ui";

// Administration is the appliance's own pages for those who may set it up: the management cluster's
// settings and removal, Kubehenge's upgrades and repair access, VM templates, and the DNS-01
// credential for every cluster. One poll of the setup status serves them all, so they agree on
// whether a run is going on.
export function Administration({ view, onGone }: { view: AdminView; onGone: () => void }): JSX.Element {
  const {
    data: status,
    error,
    reload,
  } = usePoll(api.setupStatus, (s) => (s?.job?.state === "running" ? pollRunning : pollIdle));
  // Once a removal is over there is nothing left to administer here: the setup takes over.
  const gone =
    status !== undefined && !status.ready && status.job?.kind === "remove" && status.job.state === "succeeded";
  useEffect(() => {
    if (gone) {
      onGone();
    }
  }, [gone, onGone]);

  if (!status) {
    return (
      <section className="panel">
        {error === undefined ? <p className="loading">Loading…</p> : <ErrorBox message={error} />}
      </section>
    );
  }
  const running = status.job?.state === "running";
  return (
    <section className="page">
      <div className="page-head">
        <div className="page-title">
          <h2>Administration</h2>
        </div>
      </div>
      <nav className="views" aria-label="Administration's pages">
        {adminViews.map((v) => (
          <Link key={v.path} to={adminPath(v.path)} current={view === v.path}>
            {v.label}
          </Link>
        ))}
      </nav>
      <ErrorBox message={error} />
      <ManagementBehind
        status={status}
        onStarted={() => {
          void reload();
        }}
      />
      {running && view !== "" && (
        <p className="page-note">
          {status.job?.kind === "remove"
            ? "The management cluster is being removed"
            : "The management cluster is being applied"}
          : <Link to="/runs/management-cluster">follow the run</Link>.
        </p>
      )}
      <div className="object-view">
        <AdminPage view={view} status={status} running={running} reload={reload} />
      </div>
    </section>
  );
}
