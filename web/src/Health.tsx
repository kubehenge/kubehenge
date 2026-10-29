import { useState, type JSX } from "react";

import { api } from "./api";
import type {
  AddonRelease,
  Attention,
  Cluster,
  ClusterHealth,
  ClusterRef,
  Condition,
  HealthCheck,
  HealthEvent,
  HealthNode,
  HealthSection,
  HealthVM,
  PendingVolume,
  PodLogs,
  PoolState,
  Severity,
  UnwellPod,
} from "./types";
import {
  Detail,
  Dialog,
  ErrorBox,
  Field,
  Status,
  abnormal,
  age,
  bytes,
  filled,
  pollIdle,
  sentence,
  usePoll,
} from "./ui";

// A cluster that is not healthy is looked at more often, to follow it getting better.
const pollUnwell = 10_000;

const msPerSecond = 1000;
const percent = 100;
const millicoresPerCore = 1000;

type State = "ok" | "progress" | "warning" | "error" | "idle";

const severityState: Record<Severity, State> = { error: "error", warning: "warning", progress: "progress" };
const severityText: Record<Severity, string> = { error: "Error", warning: "Warning", progress: "In progress" };
const severityRank: Severity[] = ["error", "warning", "progress"];

/** The id of the part of the page a section's details are in, which the tiles and the list link to. */
const anchor = (section: string): string => `health-${section}`;

type PodRef = NonNullable<Attention["pod"]>;

// What may be done with an unwell pod from this page: its logs to those who may read them, and its
// page in Headlamp when one runs.
interface PodActions {
  logs: ((pod: UnwellPod) => void) | undefined;
  headlamp: string | undefined;
}

// worst is the gravest of severities, or ok for none.
function worst(severities: Severity[]): State {
  const found = severityRank.find((s) => severities.includes(s));
  return found === undefined ? "ok" : severityState[found];
}

// since shows how long ago, with the time itself on hover.
function Since({ at }: { at?: string | undefined }): JSX.Element {
  return <td title={filled(at) ? new Date(at).toLocaleString() : undefined}>{filled(at) ? age(at) : ""}</td>;
}

// SectionNote says why a section shows less than it should: it could not be read, or only in part.
function SectionNote<T>({ section, what }: { section: HealthSection<T>; what: string }): JSX.Element | null {
  if (filled(section.error)) {
    return (
      <div className="warning">
        Couldn&apos;t read {what}
        <Detail>{section.error}</Detail>
      </div>
    );
  }
  if (section.truncated === true) {
    return <p className="muted">First {section.items.length} shown</p>;
  }
  return null;
}

const verdicts: Record<ClusterHealth["verdict"], string> = {
  healthy: "Healthy",
  changing: "Changing",
  degraded: "Degraded",
  unreachable: "Unreachable",
};

function verdictState(h: ClusterHealth): State {
  if (h.verdict === "healthy") {
    return "ok";
  }
  if (h.verdict === "changing") {
    return "progress";
  }
  if (h.verdict === "unreachable") {
    return "error";
  }
  return worst(h.attention.map((a) => a.severity));
}

// Verdict carries, behind its state, why nothing from inside the cluster shows when nothing does.
function Verdict({ health: h }: { health: ClusterHealth }): JSX.Element {
  return (
    <div className="verdict">
      <Status state={verdictState(h)} text={verdicts[h.verdict]} />
      {filled(h.workload) && <Detail>{h.workload}</Detail>}
      <span className="spacer" />
      <span className="muted" title={new Date(h.read).toLocaleString()}>
        Read at {new Date(h.read).toLocaleTimeString()}
      </span>
    </div>
  );
}

interface Tile {
  section: string;
  label: string;
  value: string;
  state: State;
  detail?: string | undefined;
}

// readyState is how a group of nodes stands: well with all ready, badly when half or more are not.
function readyState(ready: number, total: number): State {
  if (ready === total) {
    return "ok";
  }
  return ready * 2 <= total ? "error" : "warning";
}

const notRead = { value: "Not read", state: "idle" } as const;

// etcdNote is what the control plane's provider says of etcd, which kubeadm's does and the others do not.
function etcdNote(h: ClusterHealth): { detail: string; healthy: boolean } | undefined {
  const etcd = h.controlPlane.conditions.find((x) => x.type === "EtcdClusterHealthy");
  if (etcd === undefined) {
    return undefined;
  }
  if (etcd.status === "True") {
    return { detail: "etcd healthy", healthy: true };
  }
  return { detail: "etcd unhealthy", healthy: false };
}

function controlPlaneTile(h: ClusterHealth): Tile {
  const nodes = h.nodes.items.filter((n) => n.role === "control-plane");
  const etcd = etcdNote(h);
  if (nodes.length === 0) {
    return { section: "nodes", label: "Control plane", ...notRead, detail: etcd?.detail };
  }
  const ready = nodes.filter((n) => n.ready).length;
  const state = etcd?.healthy === false ? "error" : readyState(ready, nodes.length);
  return {
    section: "nodes",
    label: "Control plane",
    value: `${ready} of ${nodes.length} ready`,
    state,
    detail: etcd?.detail,
  };
}

function workersTile(h: ClusterHealth): Tile {
  const nodes = h.nodes.items.filter((n) => n.role === "worker");
  if (nodes.length === 0) {
    return filled(h.nodes.error)
      ? { section: "nodes", label: "Workers", ...notRead }
      : {
          section: "nodes",
          label: "Workers",
          value: "None",
          state: "ok",
          // The control planes stay tainted while a pool may grow, so pods wait for a worker instead.
          detail: h.pools.items.some((p) => p.autoscaling !== undefined)
            ? "Autoscaled from 0"
            : "Workloads on the control plane",
        };
  }
  const ready = nodes.filter((n) => n.ready).length;
  return {
    section: "nodes",
    label: "Workers",
    value: `${ready} of ${nodes.length} ready`,
    state: readyState(ready, nodes.length),
  };
}

function pressureTile(h: ClusterHealth): Tile {
  const joined = h.nodes.items.filter((n) => n.joined);
  if (joined.length === 0) {
    return { section: "nodes", label: "Node pressure", ...notRead };
  }
  const pressed = joined.filter((n) => (n.pressure ?? []).length > 0 || n.unschedulable === true);
  if (pressed.length === 0) {
    return { section: "nodes", label: "Node pressure", value: "None", state: "ok" };
  }
  return {
    section: "nodes",
    label: "Node pressure",
    value: `${pressed.length} of ${joined.length} nodes`,
    state: "warning",
  };
}

function podsTile(h: ClusterHealth): Tile {
  if (filled(h.workload) || (filled(h.pods.error) && h.pods.items.length === 0)) {
    return { section: "pods", label: "Pods not well", ...notRead };
  }
  const n = h.pods.items.length;
  return {
    section: "pods",
    label: "Pods not well",
    value: n === 0 ? "None" : `${n}${h.pods.truncated === true ? "+" : ""}`,
    state: worst(h.pods.items.map((p) => p.severity)),
  };
}

function addonsTile(h: ClusterHealth): Tile {
  const releases = h.addons.items;
  if (filled(h.addons.error)) {
    return { section: "addons", label: "Add-ons", ...notRead };
  }
  if (releases.length === 0) {
    return { section: "addons", label: "Add-ons", value: "None", state: "ok" };
  }
  const deployed = releases.filter((r) => r.ready).length;
  return {
    section: "addons",
    label: "Add-ons",
    value: `${deployed} of ${releases.length} deployed`,
    state: worst(h.attention.filter((a) => a.section === "addons").map((a) => a.severity)),
  };
}

function healthChecksTile(h: ClusterHealth): Tile {
  const checks = h.healthChecks.items;
  if (filled(h.healthChecks.error)) {
    return { section: "cluster-api", label: "Health checks", ...notRead };
  }
  if (checks.length === 0) {
    return { section: "cluster-api", label: "Health checks", value: "None", state: "ok" };
  }
  let expected = 0;
  let healthy = 0;
  for (const x of checks) {
    expected += x.expected;
    healthy += x.healthy;
  }
  let state: State = "ok";
  if (checks.some((x) => !x.allowed)) {
    state = "error";
  } else if (checks.some((x) => (x.remediating ?? []).length > 0)) {
    state = "progress";
  }
  return { section: "cluster-api", label: "Health checks", value: `${healthy} of ${expected} healthy`, state };
}

function Tiles({ health: h }: { health: ClusterHealth }): JSX.Element {
  const tiles = [controlPlaneTile(h), workersTile(h), pressureTile(h), podsTile(h), addonsTile(h), healthChecksTile(h)];
  return (
    <ul className="tiles" aria-label="At a glance">
      {tiles.map((t) => (
        <li key={t.label}>
          <a className={`tile tile-${t.state}`} href={`#${anchor(t.section)}`}>
            <span className="tile-label">{t.label}</span>
            <Status state={t.state} text={t.value} />
            {filled(t.detail) && <span className="tile-detail">{t.detail}</span>}
          </a>
        </li>
      ))}
    </ul>
  );
}

const podKey = (p: PodRef): string => `${p.namespace}/${p.name}`;

function PodButtons({ pod, actions }: { pod: UnwellPod | undefined; actions: PodActions }): JSX.Element {
  const { logs, headlamp } = actions;
  return (
    <td className="row-action">
      {pod !== undefined && logs !== undefined && pod.containers.length > 0 && (
        <button type="button" onClick={() => logs(pod)}>
          Logs
        </button>
      )}
      {pod !== undefined && filled(headlamp) && (
        <a
          className="button"
          href={`${headlamp}/pods/${encodeURIComponent(pod.namespace)}/${encodeURIComponent(pod.name)}`}
          target="_blank"
          rel="noopener noreferrer"
        >
          Headlamp
        </a>
      )}
    </td>
  );
}

function NeedsAttention({ health: h, actions }: { health: ClusterHealth; actions: PodActions }): JSX.Element | null {
  if (h.attention.length === 0) {
    return null;
  }
  const pods = new Map(h.pods.items.map((p) => [podKey(p), p]));
  return (
    <div className="box scroll">
      <h3>Needs attention</h3>
      <table className="grid">
        <thead>
          <tr>
            <th>Severity</th>
            <th>What</th>
            <th>Since</th>
            <th>
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {h.attention.map((a) => (
            <tr key={`${a.section}/${a.what}/${a.message ?? ""}`}>
              <td>
                <Status state={severityState[a.severity]} text={severityText[a.severity]} />
              </td>
              <td>
                <a href={`#${anchor(a.section)}`}>{a.what}</a>
                {filled(a.message) && <Detail>{a.message}</Detail>}
              </td>
              <Since at={a.since} />
              <PodButtons pod={a.pod === undefined ? undefined : pods.get(podKey(a.pod))} actions={actions} />
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// The sparkline's drawing box: an hour of minutes across, the load from none to all cores up.
const sparkWidth = 60;
const sparkHeight = 16;

// sparkPath draws the load as a line, broken where the VM did not run.
function sparkPath(load: (number | null)[]): string {
  const step = load.length > 1 ? sparkWidth / (load.length - 1) : 0;
  let path = "";
  let drawing = false;
  for (const [i, v] of load.entries()) {
    if (v !== null) {
      const y = sparkHeight - Math.min(1, Math.max(0, v)) * sparkHeight;
      path += `${drawing ? "L" : "M"}${(i * step).toFixed(1)} ${y.toFixed(1)} `;
    }
    drawing = v !== null;
  }
  return path.trim();
}

function Sparkline({ load }: { load: (number | null)[] }): JSX.Element | null {
  if (!load.some((v) => v !== null)) {
    return null;
  }
  return (
    <svg
      className="sparkline"
      viewBox={`0 0 ${sparkWidth} ${sparkHeight}`}
      preserveAspectRatio="none"
      aria-hidden="true"
    >
      <path d={sparkPath(load)} />
    </svg>
  );
}

const share = (part: number, whole: number): string => `${Math.round((part / whole) * percent)}%`;

// uptime is how long a VM has run, as the page says how long ago things happened.
function uptime(seconds: number): string {
  const now = Date.now();
  return age(new Date(now - seconds * msPerSecond).toISOString(), now);
}

const cores = (millicores: number): string => (millicores / millicoresPerCore).toFixed(1);

function CPUCell({ node: n, vm }: { node: HealthNode; vm: HealthVM | undefined }): JSX.Element {
  return (
    <td>
      {vm !== undefined && vm.status === "running" && (
        <div className="use">
          <Sparkline load={vm.load ?? []} />
          <span>
            {share(vm.cpu, 1)} of {vm.cores}
          </span>
        </div>
      )}
      {n.cpu !== undefined && n.cpu.allocatable > 0 && (
        <div className="hint">
          Requested {cores(n.cpu.requested)} of {cores(n.cpu.allocatable)} cores
        </div>
      )}
    </td>
  );
}

function MemoryCell({ node: n, vm }: { node: HealthNode; vm: HealthVM | undefined }): JSX.Element {
  return (
    <td>
      {vm !== undefined && vm.status === "running" && vm.maxMemory > 0 && (
        <div className="use">
          <meter className="bar" min={0} max={vm.maxMemory} value={vm.memory} aria-label={`${n.name}'s memory use`} />
          <span>
            {bytes(vm.memory)} of {bytes(vm.maxMemory)}
          </span>
        </div>
      )}
      {n.memory !== undefined && n.memory.allocatable > 0 && (
        <div className="hint">
          Requested {bytes(n.memory.requested)} of {bytes(n.memory.allocatable)}
        </div>
      )}
    </td>
  );
}

// nodeStatus says how a node stands; inside is whether the cluster itself was read, without which a
// node not ready may have joined long ago.
function nodeStatus(n: HealthNode, inside: boolean): { state: State; text: string } {
  if (!n.joined) {
    if (n.ready) {
      return { state: "ok", text: "Ready" };
    }
    return inside ? { state: "progress", text: "Joining" } : { state: "idle", text: "Unknown" };
  }
  if (!n.ready) {
    return { state: "error", text: "Not ready" };
  }
  if ((n.pressure ?? []).length > 0 || n.unschedulable === true) {
    return { state: "warning", text: "Ready" };
  }
  return { state: "ok", text: "Ready" };
}

// How the node conditions reporting a shortage read.
const pressureText: Record<string, string> = {
  MemoryPressure: "Memory pressure",
  DiskPressure: "Disk pressure",
  PIDPressure: "PID pressure",
  NetworkUnavailable: "No network",
};

// nodeNotes lists what keeps work off a node: its pressures, a cordon and its taints.
function nodeNotes(n: HealthNode): string[] {
  return [
    ...(n.pressure ?? []).map((p) => pressureText[p] ?? p),
    ...(n.unschedulable === true ? ["Cordoned"] : []),
    ...(n.taints ?? []).map((t) => `Taint ${t}`),
  ];
}

function NodeRow({ node: n, inside }: { node: HealthNode; inside: boolean }): JSX.Element {
  const s = nodeStatus(n, inside);
  const { vm } = n;
  return (
    <tr className={s.state === "ok" ? "" : "attention"}>
      <td className="nowrap">
        {n.name}
        <div className="hint">
          {n.role === "control-plane" ? "Control plane" : `Worker${filled(n.pool) ? `, pool ${n.pool}` : ""}`}
        </div>
      </td>
      <td>
        <Status state={s.state} text={s.text} />
        {filled(n.message) && <Detail>{n.message}</Detail>}
        {nodeNotes(n).map((x) => (
          <div key={x} className="hint">
            {x}
          </div>
        ))}
      </td>
      <CPUCell node={n} vm={vm} />
      <MemoryCell node={n} vm={vm} />
      <td>{n.joined ? n.pods : ""}</td>
      <td>{n.kubelet ?? ""}</td>
      <td>{vm !== undefined && vm.status === "running" ? uptime(vm.uptime) : (vm?.status ?? "")}</td>
    </tr>
  );
}

function Nodes({ health: h }: { health: ClusterHealth }): JSX.Element {
  return (
    <div className="box scroll" id={anchor("nodes")}>
      <h3>Nodes</h3>
      <SectionNote section={h.nodes} what="nodes" />
      {filled(h.vms) && (
        <div className="muted">
          No figures from Proxmox VE
          <Detail>{h.vms}</Detail>
        </div>
      )}
      <table className="grid">
        <thead>
          <tr>
            <th>Name</th>
            <th>Status</th>
            <th>CPU</th>
            <th>Memory</th>
            <th>Pods</th>
            <th>Kubelet</th>
            <th>Up</th>
          </tr>
        </thead>
        <tbody>
          {h.nodes.items.length === 0 && (
            <tr>
              <td colSpan={7} className="muted">
                No nodes yet
              </td>
            </tr>
          )}
          {h.nodes.items.map((n) => (
            <NodeRow key={n.name} node={n} inside={!filled(h.workload)} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Pods({ health: h, actions }: { health: ClusterHealth; actions: PodActions }): JSX.Element | null {
  if (filled(h.workload)) {
    return null;
  }
  return (
    <div className="box scroll" id={anchor("pods")}>
      <h3>Pods not well</h3>
      <SectionNote section={h.pods} what="pods" />
      <table className="grid">
        <thead>
          <tr>
            <th>Pod</th>
            <th>Node</th>
            <th>Status</th>
            <th>Restarts</th>
            <th>Since</th>
            <th>
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {h.pods.items.length === 0 && (
            <tr>
              <td colSpan={6} className="muted">
                None
              </td>
            </tr>
          )}
          {h.pods.items.map((p) => (
            <tr key={podKey(p)}>
              <td>
                {p.name}
                <div className="hint">{p.namespace}</div>
              </td>
              <td>{p.node ?? ""}</td>
              <td>
                <Status state={severityState[p.severity]} text={p.reason} />
                {filled(p.message) && <Detail>{p.message}</Detail>}
              </td>
              <td>{p.restarts}</td>
              <Since at={p.since} />
              <PodButtons pod={p} actions={actions} />
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Volumes({ health: h }: { health: ClusterHealth }): JSX.Element | null {
  if (h.volumes.items.length === 0 && !filled(h.volumes.error)) {
    return null;
  }
  return (
    <div className="box scroll" id={anchor("volumes")}>
      <h3>Volume claims waiting</h3>
      <SectionNote section={h.volumes} what="volume claims" />
      {h.volumes.items.length > 0 && (
        <table className="grid">
          <thead>
            <tr>
              <th>Claim</th>
              <th>Storage class</th>
              <th>Waiting</th>
            </tr>
          </thead>
          <tbody>
            {h.volumes.items.map((v: PendingVolume) => (
              <tr key={`${v.namespace}/${v.name}`} className="attention">
                <td>
                  {v.name}
                  <div className="hint">{v.namespace}</div>
                </td>
                <td>{v.storageClass ?? "Default"}</td>
                <Since at={v.since} />
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function addonStatus(r: AddonRelease): { state: State; text: string } {
  if (r.ready) {
    return { state: "ok", text: "Deployed" };
  }
  if (r.status === "failed") {
    return { state: "error", text: "Failed" };
  }
  return { state: "progress", text: filled(r.status) ? sentence(r.status).replace(/\.$/u, "") : "Installing" };
}

function Addons({ health: h }: { health: ClusterHealth }): JSX.Element {
  return (
    <div className="box scroll" id={anchor("addons")}>
      <h3>Add-ons</h3>
      <SectionNote section={h.addons} what="add-ons" />
      <table className="grid">
        <thead>
          <tr>
            <th>Release</th>
            <th>Chart</th>
            <th>Status</th>
            <th>Revision</th>
            <th>Since</th>
          </tr>
        </thead>
        <tbody>
          {h.addons.items.length === 0 && (
            <tr>
              <td colSpan={5} className="muted">
                No add-ons
              </td>
            </tr>
          )}
          {h.addons.items.map((r) => {
            const s = addonStatus(r);
            return (
              <tr key={r.name} className={s.state === "ok" ? "" : "attention"}>
                <td>
                  {r.release}
                  {filled(r.namespace) && <div className="hint">{r.namespace}</div>}
                </td>
                <td>
                  {r.chart}
                  {filled(r.version) && ` ${r.version}`}
                </td>
                <td>
                  <Status state={s.state} text={s.text} />
                  {filled(r.message) && <Detail>{r.message}</Detail>}
                </td>
                <td>{r.revision ?? ""}</td>
                <Since at={r.since} />
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function poolStatus(p: PoolState): { state: State; text: string } {
  if (p.rollingOut) {
    return { state: "progress", text: "Rolling out" };
  }
  if (p.ready < p.desired || p.available < p.desired) {
    return { state: "warning", text: "Short of machines" };
  }
  return { state: "ok", text: "Steady" };
}

function Pools({ pools }: { pools: HealthSection<PoolState> }): JSX.Element | null {
  if (pools.items.length === 0 && !filled(pools.error)) {
    return null;
  }
  return (
    <>
      <h4>Worker pools</h4>
      <SectionNote section={pools} what="worker pools" />
      <table className="grid">
        <thead>
          <tr>
            <th>Pool</th>
            <th>Status</th>
            <th>Desired</th>
            <th>Ready</th>
            <th>Available</th>
            <th>Up to date</th>
          </tr>
        </thead>
        <tbody>
          {pools.items.map((p) => {
            const s = poolStatus(p);
            return (
              <tr key={p.name} className={s.state === "ok" ? "" : "attention"}>
                <td>{p.name}</td>
                <td>
                  <Status state={s.state} text={s.text} />
                  {filled(p.message) && <Detail>{p.message}</Detail>}
                </td>
                <td>
                  {p.desired}
                  {p.autoscaling && (
                    <div className="hint">
                      Autoscaled, {p.autoscaling.min} to {p.autoscaling.max}
                    </div>
                  )}
                </td>
                <td>{p.ready}</td>
                <td>{p.available}</td>
                <td>{p.upToDate}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}

function checkStatus(x: HealthCheck): { state: State; text: string } {
  if (!x.allowed) {
    return { state: "error", text: "Replacing paused" };
  }
  if ((x.remediating ?? []).length > 0) {
    return { state: "progress", text: "Replacing machines" };
  }
  return { state: x.healthy === x.expected ? "ok" : "warning", text: `${x.healthy} of ${x.expected} healthy` };
}

function HealthChecks({ checks }: { checks: HealthSection<HealthCheck> }): JSX.Element {
  return (
    <>
      <h4>Machine health checks</h4>
      <SectionNote section={checks} what="health checks" />
      <table className="grid">
        <thead>
          <tr>
            <th>Check</th>
            <th>Status</th>
            <th>Replacing</th>
          </tr>
        </thead>
        <tbody>
          {checks.items.length === 0 && (
            <tr>
              <td colSpan={3} className="muted">
                None: failed machines stay
              </td>
            </tr>
          )}
          {checks.items.map((x) => {
            const s = checkStatus(x);
            return (
              <tr key={x.name} className={s.state === "ok" ? "" : "attention"}>
                <td>{x.name}</td>
                <td>
                  <Status state={s.state} text={s.text} />
                  {filled(x.message) && <Detail>{x.message}</Detail>}
                </td>
                <td>{(x.remediating ?? []).join(", ")}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}

function ConditionTable({ conditions, empty }: { conditions: Condition[]; empty: string }): JSX.Element {
  return (
    <table className="grid">
      <thead>
        <tr>
          <th>Type</th>
          <th>Status</th>
          <th>Reason</th>
          <th>Message</th>
          <th>Since</th>
        </tr>
      </thead>
      <tbody>
        {conditions.length === 0 && (
          <tr>
            <td colSpan={5} className="muted">
              {empty}
            </td>
          </tr>
        )}
        {conditions.map((x) => {
          const attention = abnormal(x.type, x.status);
          return (
            <tr key={x.type} className={attention ? "attention" : ""}>
              <td>
                {attention && <span className="visually-hidden">Needs attention: </span>}
                {x.type}
              </td>
              <td>{x.status}</td>
              <td>{x.reason ?? ""}</td>
              <td className="message">{x.message ?? ""}</td>
              <Since at={x.since} />
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

function ClusterAPI({ health: h }: { health: ClusterHealth }): JSX.Element {
  const cp = h.controlPlane;
  return (
    <div className="box scroll" id={anchor("cluster-api")}>
      <h3>Cluster API</h3>
      <Pools pools={h.pools} />
      <h4 id={anchor("control-plane")}>Control plane{filled(cp.kind) && ` (${cp.kind})`}</h4>
      {filled(cp.error) && (
        <div className="warning">
          Couldn&apos;t read the control plane
          <Detail>{cp.error}</Detail>
        </div>
      )}
      <ConditionTable conditions={cp.conditions} empty="No conditions yet" />
      <HealthChecks checks={h.healthChecks} />
    </div>
  );
}

type Source = "" | HealthEvent["source"];

const sources: { value: Source; label: string }[] = [
  { value: "", label: "All" },
  { value: "workload", label: "In the cluster" },
  { value: "management", label: "Cluster API" },
];

function Events({ events }: { events: HealthSection<HealthEvent> }): JSX.Element {
  const [source, setSource] = useState<Source>("");
  const shown = events.items.filter((e) => source === "" || e.source === source);
  return (
    <div className="box scroll" id={anchor("events")}>
      <div className="section-head">
        <h3>Last hour&apos;s events</h3>
        <label className="filter">
          From{" "}
          <select
            value={source}
            onChange={(e) => setSource(sources.find((s) => s.value === e.target.value)?.value ?? "")}
          >
            {sources.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </select>
        </label>
      </div>
      <SectionNote section={events} what="events" />
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
          {shown.length === 0 && (
            <tr>
              <td colSpan={5} className="muted">
                No events
              </td>
            </tr>
          )}
          {shown.map((e) => (
            <tr
              key={`${e.source}/${e.object}/${e.reason}/${e.message}`}
              className={e.type === "Warning" ? "attention" : ""}
            >
              <Since at={e.last} />
              <td>
                {e.reason}
                <div className="hint">{e.source === "workload" ? "In the cluster" : "Cluster API"}</div>
              </td>
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

// LogsDialog shows the last lines of an unwell pod's container, the unwell one first.
function LogsDialog({
  cluster,
  pod,
  onClose,
}: {
  cluster: ClusterRef;
  pod: UnwellPod;
  onClose: () => void;
}): JSX.Element {
  const [container, setContainer] = useState(pod.containers[0] ?? "");
  return (
    <Dialog
      title={`Logs of ${pod.name}`}
      onClose={onClose}
      actions={
        <button type="button" onClick={onClose}>
          Close
        </button>
      }
    >
      {pod.containers.length > 1 && (
        <Field label="Container">
          {(id) => (
            <select id={id} value={container} onChange={(e) => setContainer(e.target.value)}>
              {pod.containers.map((x) => (
                <option key={x} value={x}>
                  {x}
                </option>
              ))}
            </select>
          )}
        </Field>
      )}
      {/* Its own poll for each container, so lines of the one shown before never stand under another's name. */}
      <PodLog key={container} cluster={cluster} pod={pod} container={container} />
    </Dialog>
  );
}

// PodLog follows a container's last lines while its dialog is open, as a crash loop writes new ones.
function PodLog({ cluster, pod, container }: { cluster: ClusterRef; pod: UnwellPod; container: string }): JSX.Element {
  const { data: logs, error } = usePoll((): Promise<PodLogs> => api.podLogs(cluster, pod, container), pollUnwell);
  return (
    <>
      <ErrorBox message={error} />
      {logs === undefined && !filled(error) && <p className="muted">Loading…</p>}
      {logs?.previous === true && <p className="muted">Previous run</p>}
      {logs !== undefined && (
        <pre className="log pod-log" aria-label={`Log of ${container}`}>
          {logs.lines.length === 0 ? "No lines yet" : logs.lines.join("\n")}
        </pre>
      )}
    </>
  );
}

/**
 * HealthView is a cluster's health page: what needs attention first, then its nodes, unwell pods, add-ons,
 * what Cluster API reports, the last hour's events and, last, the cluster's own conditions.
 */
export function HealthView({ cluster: c, headlamp }: { cluster: Cluster; headlamp?: string | undefined }): JSX.Element {
  const ref = { namespace: c.namespace, name: c.name };
  const { data: h, error } = usePoll(
    () => api.health(ref.namespace, ref.name),
    (shown) => (shown?.verdict === "healthy" ? pollIdle : pollUnwell),
  );
  const [logsOf, setLogsOf] = useState<UnwellPod>();
  if (!h) {
    return (
      <div className="box">
        <ErrorBox message={error} />
        {!filled(error) && <p className="muted">Loading…</p>}
      </div>
    );
  }
  const actions: PodActions = { logs: h.logs ? setLogsOf : undefined, headlamp };
  return (
    <>
      <div className="box">
        <Verdict health={h} />
        <ErrorBox message={error} />
        <Tiles health={h} />
      </div>
      <NeedsAttention health={h} actions={actions} />
      <Nodes health={h} />
      <Pods health={h} actions={actions} />
      <Volumes health={h} />
      <Addons health={h} />
      <ClusterAPI health={h} />
      <Events events={h.events} />
      <div className="box scroll" id={anchor("conditions")}>
        <h3>Conditions</h3>
        <ConditionTable conditions={c.conditions} empty="No conditions yet" />
      </div>
      {logsOf !== undefined && <LogsDialog cluster={ref} pod={logsOf} onClose={() => setLogsOf(undefined)} />}
    </>
  );
}
