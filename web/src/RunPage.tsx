import { useState, type JSX } from "react";

import { api } from "./api";
import { clusterPath } from "./cluster-paths";
import { useFleet } from "./fleet";
import { Link, navigate } from "./router";
import { runTitle, runView } from "./Runs";
import type { Job, RunInfo, RunStep } from "./types";
import { age, ErrorBox, errorMessage, logLines, pollIdle, pollRunning, Status, usePoll } from "./ui";

const states: Record<RunInfo["state"], { state: "progress" | "ok" | "error"; text: string }> = {
  running: { state: "progress", text: "Running" },
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

// The runs that can be started again as they were: those that take nothing but the cluster.
const retries: Partial<Record<RunInfo["kind"], (r: RunInfo) => Promise<unknown>>> = {
  backup: async (r) => api.backUp(r.namespace ?? "", r.cluster ?? ""),
  "etcd-snapshot": async (r) => api.takeEtcdSnapshot(r.namespace ?? "", r.cluster ?? ""),
  "template-refresh": async () => api.refreshTemplates(),
};

// TryAgain starts a failed run again, where it takes nothing to choose, and goes where the new run shows.
function TryAgain({
  run,
  to,
  onError,
}: {
  run: RunInfo;
  to: string;
  onError: (message: string | undefined) => void;
}): JSX.Element | null {
  const [busy, setBusy] = useState(false);
  const again = retries[run.kind];
  if (run.state !== "failed" || again === undefined) {
    return null;
  }
  const start = async (): Promise<void> => {
    setBusy(true);
    onError(undefined);
    try {
      await again(run);
      navigate(to);
    } catch (err) {
      onError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <button
      type="button"
      className="primary"
      disabled={busy}
      onClick={() => {
        void start();
      }}
    >
      Try again
    </button>
  );
}

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
      {run.error !== undefined && (
        <div className="pinned-error" role="alert">
          <strong>Error</strong>
          <p>{run.error}</p>
        </div>
      )}
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
      {(run.kind === "management-cluster" || run.kind === "template") && <JobLog run={run} />}
    </section>
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
