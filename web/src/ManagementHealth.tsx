import type { JSX } from "react";

import { api } from "./api";
import type { ManagementHealth } from "./types";
import { Detail, ErrorBox, Status, pollCache, pollIdle, usePoll } from "./ui";

const managementCache = pollCache<ManagementHealth>();
const managementKept = { cache: managementCache, key: "" };

export function healthVerdict(h: ManagementHealth): { state: "ok" | "warning" | "error"; text: string } {
  if (!h.answers) {
    return { state: "error", text: "Not answering" };
  }
  const nodes = h.nodes ?? [];
  if (nodes.length > 0 && nodes.every((n) => !n.ready)) {
    return { state: "error", text: "Down" };
  }
  const short = nodes.some((n) => !n.ready) || (h.controllers ?? []).some((c) => c.ready < c.wanted);
  return short ? { state: "warning", text: "Degraded" } : { state: "ok", text: "Healthy" };
}

// ManagementHealthCard is the management cluster's own health, read on its own so that it answers
// while the cluster does not, when the Administration page is what repairs it.
export function ManagementHealthCard(): JSX.Element {
  const { data: h, error } = usePoll(api.managementHealth, pollIdle, managementKept);
  if (!h) {
    return (
      <div className="box">
        <h3>Health</h3>
        {error === undefined ? <p className="loading">Loading…</p> : <ErrorBox message={error} />}
      </div>
    );
  }
  const verdict = healthVerdict(h);
  const nodes = h.nodes ?? [];
  const controllers = h.controllers ?? [];
  const nodesDown = nodes.filter((n) => !n.ready);
  const controllersShort = controllers.filter((c) => c.ready < c.wanted);
  return (
    <div className="box management-health">
      <h3>
        Health <Status state={verdict.state} text={verdict.text} />
      </h3>
      {h.answers ? (
        <>
          <dl className="summary">
            <dt>Nodes</dt>
            <dd className="mono">
              {nodes.length - nodesDown.length} of {nodes.length} ready
            </dd>
            <dt>Controllers</dt>
            <dd className="mono">
              {controllers.length - controllersShort.length} of {controllers.length} ready
            </dd>
          </dl>
          {(nodesDown.length > 0 || controllersShort.length > 0) && (
            <ul className="health-short">
              {nodesDown.map((n) => (
                <li key={`node/${n.name}`}>
                  <Status state="error" text="Not ready" /> <span className="mono">{n.name}</span>
                  {n.message !== undefined && <span className="muted"> {n.message}</span>}
                </li>
              ))}
              {controllersShort.map((c) => (
                <li key={`deploy/${c.namespace}/${c.name}`}>
                  <Status state="warning" text={`${c.ready} of ${c.wanted}`} />{" "}
                  <span className="mono">
                    {c.namespace}/{c.name}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </>
      ) : (
        h.error !== undefined && (
          <Detail>
            <p className="mono">{h.error}</p>
          </Detail>
        )
      )}
    </div>
  );
}
