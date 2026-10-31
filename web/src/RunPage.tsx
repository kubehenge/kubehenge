import { useState, type JSX } from "react";

import { api } from "./api";
import { useClusterHealth } from "./cluster-health";
import { clusterPath } from "./cluster-paths";
import { useFleet } from "./fleet";
import { Link } from "./router";
import { TryAgain } from "./RunAgain";
import { runTitle, runView } from "./Runs";
import type { HealthEvent, Job, RunInfo, RunStep } from "./types";
import { age, ErrorBox, logLines, pollIdle, pollRunning, Skeleton, Status, usePoll } from "./ui";

const states: Record<RunInfo["state"], { state: "progress" | "warning" | "ok" | "error"; text: string }> = {
  running: { state: "progress", text: "Running" },
  stalled: { state: "warning", text: "Stalled" },
  succeeded: { state: "ok", text: "Succeeded" },
  failed: { state: "error", text: "Failed" },
};

const stepStates: Record<RunStep["state"], string> = {
  done: "Done",
  running: "Running",
  waiting: "Waiting",
  failed: "Failed",
};
const stepDots: Record<RunStep["state"], string> = {
  done: "ok",
  running: "progress",
  waiting: "idle",
  failed: "error",
};

const msPerMinute = 60_000;

// took says how long a run took, or has taken so far.
function took(r: RunInfo): string | undefined {
  if (r.started === undefined) {
    return undefined;
  }
  const end = r.finished === undefined ? Date.now() : new Date(r.finished).getTime();
  const minutes = Math.max(0, Math.round((end - new Date(r.started).getTime()) / msPerMinute));
  return minutes < 1 ? "<1 min" : `${minutes} min`;
}

/**
 * RunPage follows one run: what it does and for which cluster, who started it, how long it has
 * taken, its first error pinned above its steps, and the log the appliance keeps of its own work.
 */
export function RunPage({ id }: { id: string }): JSX.Element {
  const fleet = useFleet();
  const [retryError, setRetryError] = useState<string>();
  const run = fleet?.runs?.find((r) => r.id === id);
  if (run === undefined) {
    return (
      <section className="page">
        <div className="page-head">
          <h2>Run</h2>
        </div>
        <p className="muted">
          {fleet?.runs === undefined ? "Loading…" : "Run no longer kept. Runs stay 1 h, failed ones 1 day."}
        </p>
      </section>
    );
  }
  const { state, text } = states[run.state];
  const { to, place } = runView(run);
  const spent = took(run);
  return (
    <section className="page run-page">
      <div className="page-head">
        <div className="page-title">
          <h2>
            {runTitle(run)} <Status state={state} text={text} />
          </h2>
          <p className="facts">
            <span>
              {run.cluster === undefined ? (
                "Appliance"
              ) : (
                <Link to={clusterPath(run.namespace ?? "", run.cluster, "")}>{run.cluster}</Link>
              )}
            </span>
            {run.by !== undefined && <span>by {run.by}</span>}
            {run.started !== undefined && <span>started {age(run.started)} ago</span>}
            {spent !== undefined && <span>{run.finished === undefined ? `${spent} so far` : `took ${spent}`}</span>}
          </p>
        </div>
        <div className="page-actions">
          <Link to={to} className="button">
            {place}
          </Link>
          <TryAgain run={run} to={to} onError={setRetryError} />
        </div>
      </div>
      <ErrorBox message={retryError} />
      <PinnedError run={run} />
      {run.steps !== undefined && run.steps.length > 0 && (
        <ol className="steps" aria-label="Steps">
          {run.steps.map((s) => (
            <li key={s.name} className={`step step-${s.state}`}>
              <span className={`dot dot-${stepDots[s.state]}`} aria-hidden="true" />
              <span className="step-name">{s.name}</span>
              <span className="step-state">{stepStates[s.state]}</span>
              {s.detail !== undefined && <span className="step-detail">{s.detail}</span>}
            </li>
          ))}
        </ol>
      )}
      {run.steps === undefined && run.phase !== undefined && <p className="muted">{run.phase}</p>}
      <RunLog run={run} />
    </section>
  );
}

// RunLog is whichever log a run's kind keeps.
function RunLog({ run }: { run: RunInfo }): JSX.Element | null {
  const of = eventsOf[run.kind];
  if (run.kind === "management-cluster" || run.kind === "template") {
    return <JobLog run={run} />;
  }
  if (run.kind === "backup" || run.kind === "restore") {
    return <BackupLog run={run} />;
  }
  if (of === undefined || run.namespace === undefined || run.cluster === undefined) {
    return null;
  }
  return <RunEvents run={run} namespace={run.namespace} cluster={run.cluster} of={of} />;
}

// PinnedError is what holds a stalled run up, or a run's first error, above its steps.
function PinnedError({ run }: { run: RunInfo }): JSX.Element | null {
  if (run.error === undefined) {
    return null;
  }
  const stalled = run.state === "stalled";
  return (
    <div className={stalled ? "pinned-error stalled" : "pinned-error"} role="alert">
      <strong>{stalled ? "Held up by" : "Error"}</strong>
      <p>{run.error}</p>
    </div>
  );
}

async function jobOf(kind: RunInfo["kind"]): Promise<Job | undefined> {
  if (kind === "template") {
    const templates = await api.templates();
    return templates.job;
  }
  const status = await api.setupStatus();
  return status.job;
}

// BackupLog is the end of a finished backup's or restore's log, for those who may read the cluster's
// logs: Velero writes it to the target once the run is over.
function BackupLog({ run }: { run: RunInfo }): JSX.Element | null {
  const cluster = useFleet()?.clusters?.find((c) => c.namespace === run.namespace && c.name === run.cluster);
  if (cluster?.allowed.kubeconfig !== true || run.state === "running" || run.state === "stalled") {
    return null;
  }
  return <VeleroLogBox run={run} namespace={cluster.namespace} cluster={cluster.name} />;
}

function VeleroLogBox({ run, namespace, cluster }: { run: RunInfo; namespace: string; cluster: string }): JSX.Element {
  const name = run.id.split("/").at(-1) ?? "";
  const read = run.kind === "restore" ? api.restoreLog : api.backupLog;
  const { data: log, error } = usePoll(async () => read(namespace, cluster, name), pollIdle);
  return (
    <section className="box run-log">
      <h3>Log</h3>
      <ErrorBox message={error} />
      {log === undefined && error === undefined && <Skeleton rows={6} />}
      {log !== undefined && (
        <pre className="log" aria-label="Log">
          {log.lines.join("\n")}
        </pre>
      )}
    </section>
  );
}

// The runs the controller reports in events on the cluster, and which of its events are theirs.
const eventsOf: Partial<Record<RunInfo["kind"], (e: HealthEvent) => boolean>> = {
  "etcd-snapshot": (e) => e.action === "SnapshotEtcd" || e.action === "UploadEtcdSnapshots",
  "etcd-restore": (e) => e.action === "RestoreEtcd",
  change: () => true,
};

/** eventLines are a run's events in the management cluster since it started, oldest first. */
export function eventLines(events: HealthEvent[], run: RunInfo, of: (e: HealthEvent) => boolean): string[] {
  const since = run.started === undefined ? 0 : new Date(run.started).getTime();
  return events
    .filter((e) => e.source === "management" && of(e) && new Date(e.last).getTime() >= since)
    .toSorted((a, b) => a.last.localeCompare(b.last))
    .map((e) => {
      const times = e.count > 1 ? ` (${e.count} times)` : "";
      return `${new Date(e.last).toLocaleTimeString()} ${e.object}: ${e.message}${times}`;
    });
}

// RunEvents is what the controller and Cluster API reported of a run, from the last hour's events.
function RunEvents({
  run,
  namespace,
  cluster,
  of,
}: {
  run: RunInfo;
  namespace: string;
  cluster: string;
  of: (e: HealthEvent) => boolean;
}): JSX.Element | null {
  const { data: health } = useClusterHealth({ namespace, name: cluster });
  const lines = eventLines(health?.events.items ?? [], run, of);
  if (lines.length === 0) {
    return null;
  }
  return (
    <section className="box run-log">
      <h3>Events</h3>
      <pre className="log" aria-label="Events">
        {lines.join("\n")}
      </pre>
    </section>
  );
}

// JobLog is the end of the log the appliance keeps of a run of its own.
function JobLog({ run }: { run: RunInfo }): JSX.Element | null {
  const { data: job } = usePoll(
    async () => jobOf(run.kind),
    (shown) => (shown?.state === "running" ? pollRunning : pollIdle),
  );
  const log = job?.log ?? [];
  if (log.length === 0) {
    return null;
  }
  return (
    <section className="box run-log">
      <h3>Log</h3>
      <pre className="log" aria-label="Log">
        {log.slice(-logLines).join("\n")}
      </pre>
    </section>
  );
}
