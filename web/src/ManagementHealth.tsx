import type { JSX } from "react";

import { api } from "./api";
import type { ManagementHealth } from "./types";
import { Detail, ErrorBox, Skeleton, Status, filled, pollCache, pollIdle, usePoll } from "./ui";

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
  const short =
    nodes.some((n) => !n.ready) ||
    (h.controllers ?? []).some((c) => c.ready < c.wanted) ||
    h.proxmox?.ok === false ||
    filled(h.proxmox?.blind) ||
    (h.proxmox?.tokens ?? []).length > 0;
  return short ? { state: "warning", text: "Degraded" } : { state: "ok", text: "Healthy" };
}

// ManagementHealthCard is the management cluster's own health, read on its own so that it answers
// while the cluster does not, when the Administration page is what repairs it.
export function ManagementHealthCard(): JSX.Element {
  const { data: h, error, stale } = usePoll(api.managementHealth, pollIdle, managementKept);
  if (!h) {
    return (
      <div className="box">
        <h3>Health</h3>
        {error === undefined ? <Skeleton rows={3} /> : <ErrorBox message={error} />}
      </div>
    );
  }
  const verdict = healthVerdict(h);
  const nodes = h.nodes ?? [];
  const controllers = h.controllers ?? [];
  const nodesDown = nodes.filter((n) => !n.ready);
  const controllersShort = controllers.filter((c) => c.ready < c.wanted);
  const tokensFailing = h.proxmox?.tokens ?? [];
  return (
    <div className={stale ? "box management-health management-health-stale" : "box management-health"}>
      <h3>
        Health <Status state={verdict.state} text={verdict.text} />
        {stale && (
          <span className="hint-warning" title={error}>
            Stale
          </span>
        )}
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
            {h.proxmox !== undefined && (
              <>
                <dt>Proxmox VE API</dt>
                <dd>
                  <Status state={h.proxmox.ok ? "ok" : "error"} text={h.proxmox.ok ? "Reachable" : "Not reachable"} />
                </dd>
              </>
            )}
          </dl>
          {h.proxmox?.ok === false && h.proxmox.error !== undefined && (
            <Detail>
              <p className="mono">{h.proxmox.error}</p>
            </Detail>
          )}
          {filled(h.proxmox?.blind) && (
            <p className="warning">Host maintenance is not visible: the token lacks {h.proxmox?.blind}</p>
          )}
          {tokensFailing.length > 0 && (
            <ul className="health-short" aria-label="Proxmox VE tokens">
              {tokensFailing.map((t) => (
                <li key={`token/${t.name}`}>
                  <Status state="error" text={t.problem} /> {t.message}
                </li>
              ))}
            </ul>
          )}
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
