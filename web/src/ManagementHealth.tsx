import type { JSX } from "react";

import { api } from "./api";
import type { ManagementHealth, RebootReadiness } from "./types";
import { Detail, ErrorBox, Skeleton, Status, filled, pollCache, pollIdle, usePoll } from "./ui";

const managementCache = pollCache<ManagementHealth>();
const managementKept = { cache: managementCache, key: "" };
const rebootCache = pollCache<RebootReadiness>();
const rebootKept = { cache: rebootCache, key: "" };

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
    (h.proxmox?.tokens ?? []).length > 0 ||
    (h.etcd !== undefined && h.etcd.healthy < h.etcd.members);
  return short ? { state: "warning", text: "Degraded" } : { state: "ok", text: "Healthy" };
}

type TokenProblem = NonNullable<NonNullable<ManagementHealth["proxmox"]>["tokens"]>[number];

// TokenProblems lists the Proxmox VE tokens that failed their last test, each with what is wrong.
function TokenProblems({ tokens }: { tokens: TokenProblem[] }): JSX.Element | null {
  if (tokens.length === 0) {
    return null;
  }
  return (
    <ul className="health-short" aria-label="Proxmox VE tokens">
      {tokens.map((t) => (
        <li key={`token/${t.name}`} className="token-problem">
          <Status state="error" text={t.problem} />
          <span>{t.message}</span>
        </li>
      ))}
    </ul>
  );
}

// ProxmoxNotes says what is wrong with the controller's access to Proxmox VE, beneath the figures.
function ProxmoxNotes({ proxmox }: { proxmox: ManagementHealth["proxmox"] }): JSX.Element {
  return (
    <>
      {proxmox?.ok === false && proxmox.error !== undefined && (
        <Detail>
          <p className="mono">{proxmox.error}</p>
        </Detail>
      )}
      {filled(proxmox?.blind) && (
        <p className="warning">Host maintenance is not visible: the token lacks {proxmox?.blind}</p>
      )}
      <TokenProblems tokens={proxmox?.tokens ?? []} />
    </>
  );
}

// RebootReady answers, for each Proxmox VE node, whether it may be rebooted now. The controller's
// own record can be late, which the answer then says once for every node.
function RebootReady({ ready }: { ready: RebootReadiness }): JSX.Element {
  // A degraded cluster holds back only the nodes that carry its members, so it is a note while some
  // node may still go. When none may, the blocker is the reason for them all.
  const noneMayGo = ready.nodes.every((n) => !n.ready);
  return (
    <>
      {!ready.ready && filled(ready.blocker) && <p className={noneMayGo ? "warning" : "muted"}>{ready.blocker}</p>}
      <ul className="health-short" aria-label="Ready for reboot">
        {ready.nodes.map((n) => (
          <li key={`reboot/${n.node ?? ""}`}>
            <Status state={n.ready ? "ok" : "warning"} text={n.ready ? "Ready" : "Wait"} />{" "}
            <span className="mono">{n.node}</span>
            {!n.ready && filled(n.blocker) && n.blocker !== ready.blocker && (
              <span className="muted"> {n.blocker}</span>
            )}
          </li>
        ))}
      </ul>
    </>
  );
}

function UnownedVMs({ vms }: { vms: { vmid: number }[] }): JSX.Element | null {
  if (vms.length === 0) {
    return null;
  }
  return (
    <div className="unowned-vms">
      <p className="warning">
        Unowned VMs: <span className="mono">{vms.map((v) => v.vmid).join(", ")}</span>
      </p>
      <p className="muted">In Kubehenge&apos;s pools, owned by no cluster. Nothing deletes them.</p>
    </div>
  );
}

// ManagementHealthCard is the management cluster's own health, read on its own so that it answers
// while the cluster does not, when the Administration page is what repairs it.
export function ManagementHealthCard(): JSX.Element {
  const { data: h, error, stale } = usePoll(api.managementHealth, pollIdle, managementKept);
  const { data: reboot } = usePoll(api.rebootReady, pollIdle, rebootKept);
  if (!h) {
    return (
      <div className="box">
        <h3>Health</h3>
        {error === undefined ? <Skeleton rows={3} /> : <ErrorBox message={error} />}
      </div>
    );
  }
  const verdict = healthVerdict(h);
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
        <AnsweringHealth health={h} reboot={reboot} />
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

function AnsweringHealth({ health: h, reboot }: { health: ManagementHealth; reboot: RebootReadiness | undefined }): JSX.Element {
  const nodes = h.nodes ?? [];
  const controllers = h.controllers ?? [];
  const nodesDown = nodes.filter((n) => !n.ready);
  const controllersShort = controllers.filter((c) => c.ready < c.wanted);
  return (
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
        {h.etcd !== undefined && (
          <>
            <dt>etcd</dt>
            <dd className="mono">
              {h.etcd.healthy} of {h.etcd.members} members
            </dd>
          </>
        )}
        {h.proxmox !== undefined && (
          <>
            <dt>Proxmox VE API</dt>
            <dd>
              <Status state={h.proxmox.ok ? "ok" : "error"} text={h.proxmox.ok ? "Reachable" : "Not reachable"} />
            </dd>
          </>
        )}
        {reboot !== undefined && (
          <>
            <dt>Ready for reboot</dt>
            <dd>
              <Status state={reboot.ready ? "ok" : "warning"} text={reboot.ready ? "Ready" : "Wait"} />
            </dd>
          </>
        )}
      </dl>
      {reboot !== undefined && <RebootReady ready={reboot} />}
      <UnownedVMs vms={h.unowned?.vms ?? []} />
      <ProxmoxNotes proxmox={h.proxmox} />
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
  );
}
