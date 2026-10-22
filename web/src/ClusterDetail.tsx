import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { OptionDialog, Options, type OptionAction } from "./ClusterOptions";
import { HealthView } from "./Health";
import { Link, navigate } from "./router";
import type { Cluster, Machine, Versions } from "./types";
import {
  Dialog,
  ErrorBox,
  Field,
  Status,
  StatusBadge,
  age,
  distributionName,
  errorMessage,
  filled,
  maxWorkers,
  pollIdle,
  saveFile,
  settled,
  useChange,
  usePoll,
  type Health,
} from "./ui";

type Action = "scale" | "upgrade" | "delete" | OptionAction;

// A cluster on its way somewhere is looked at more often than a settled one.
const pollChanging = 5000;

const httpNotFound = 404;

// Odd sizes keep etcd's quorum.
// oxlint-disable-next-line eslint/no-magic-numbers -- the sizes themselves, named by the constant
export const controlPlaneSizes = [1, 3, 5, 7];

/** headlampURL is a cluster's page in the shared Headlamp, served on its own port of this host. */
export function headlampURL(port: number, cluster?: string): string {
  const host = location.hostname.includes(":") ? `[${location.hostname}]` : location.hostname;
  return `https://${host}:${port}/${filled(cluster) ? `c/${encodeURIComponent(cluster)}` : ""}`;
}

// headlampOf is the cluster's page in the shared Headlamp, when one runs and can reach the cluster.
function headlampOf(port: number | undefined, c: Cluster): string | undefined {
  return port !== undefined && port > 0 && filled(c.headlamp) && c.available
    ? headlampURL(port, c.headlamp)
    : undefined;
}

const poolName = (pool: string): string => pool.replace(/^\/pool\//u, "");

/** The views of a cluster's page, as the last part of its address; the summary has none. */
export const views = [
  { path: "", label: "Summary" },
  { path: "health", label: "Health" },
  { path: "machines", label: "Machines" },
  { path: "options", label: "Options" },
] as const;

export type View = (typeof views)[number]["path"];

/** clusterPath is the address of a cluster's page, or of one of its views. */
export const clusterPath = (namespace: string, name: string, view: View = ""): string =>
  `/clusters/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}${view ? `/${view}` : ""}`;

// autoUpgradeText reads the controller's record of the last automatic upgrade, "from to to at
// time", with the time in the reader's own terms.
function autoUpgradeText(upgraded: string): string {
  const { from, to, at } = /^(?<from>\S+) to (?<to>\S+) at (?<at>\S+)$/u.exec(upgraded)?.groups ?? {};
  if (at === undefined || Number.isNaN(Date.parse(at))) {
    return upgraded;
  }
  return `${from} to ${to}, ${new Date(at).toLocaleString()}`;
}

// whyNot says why a cluster's actions are off, for the disabled buttons to say.
function whyNot(c: Cluster): string | undefined {
  if (c.deleting) {
    return "The cluster is being deleted.";
  }
  return c.allowed.change ? undefined : `Changing it needs VM.Allocate on pool ${poolName(c.pool)}.`;
}

export function ClusterDetail({
  namespace,
  name,
  view = "",
  headlamp,
}: {
  namespace: string;
  name: string;
  view?: View;
  headlamp?: number | undefined;
}): JSX.Element {
  const {
    data: c,
    error,
    errorStatus,
    reload,
  } = usePoll(
    () => api.cluster(namespace, name),
    (shown) => (shown && settled(shown) ? pollIdle : pollChanging),
  );
  const [action, setAction] = useState<Action>();
  const [downloadError, setDownloadError] = useState<string>();

  if (errorStatus === httpNotFound) {
    return (
      <section className="panel">
        <div className="toolbar">
          <h2>{name}</h2>
        </div>
        <p className="pad">
          This cluster no longer exists, or you may no longer see it. <Link to="/">Go to the clusters</Link>.
        </p>
      </section>
    );
  }
  if (!c) {
    return (
      <section className="panel">
        <ErrorBox message={error} />
        {!filled(error) && <p className="muted pad">Loading…</p>}
      </section>
    );
  }
  const can = c.allowed.change && !c.deleting;
  const why = whyNot(c);
  const inHeadlamp = headlampOf(headlamp, c);
  const close = (): void => {
    setAction(undefined);
    void reload();
  };
  // Fetched rather than linked, so that a refusal shows here instead of as a broken download.
  const downloadKubeconfig = async (): Promise<void> => {
    setDownloadError(undefined);
    try {
      saveFile(await api.kubeconfig(c.namespace, c.name), `${c.name}.kubeconfig`);
    } catch (err) {
      setDownloadError(errorMessage(err));
    }
  };

  return (
    <section className="panel">
      <div className="toolbar">
        <h2>
          {c.name} <StatusBadge cluster={c} />
        </h2>
        <span className="spacer" />
        <button type="button" disabled={!can} title={why} onClick={() => setAction("scale")}>
          Scale
        </button>
        <button type="button" disabled={!can} title={why} onClick={() => setAction("upgrade")}>
          Upgrade
        </button>
        {c.allowed.kubeconfig && c.available && (
          <button
            type="button"
            onClick={() => {
              void downloadKubeconfig();
            }}
          >
            Kubeconfig
          </button>
        )}
        {inHeadlamp !== undefined && (
          <a className="button" href={inHeadlamp} target="_blank" rel="noopener noreferrer">
            Headlamp
          </a>
        )}
        <button type="button" className="danger" disabled={!can} title={why} onClick={() => setAction("delete")}>
          Delete
        </button>
      </div>
      {why !== undefined && <p className="muted pad">{why}</p>}
      <ErrorBox message={error} />
      <ErrorBox message={downloadError} />

      <div className="object">
        <nav className="object-menu" aria-label={`${c.name}'s views`}>
          {views.map((v) => (
            <Link key={v.path} to={clusterPath(c.namespace, c.name, v.path)} current={view === v.path}>
              {v.label}
            </Link>
          ))}
        </nav>
        <div className="object-view">
          {view === "" && <Summary cluster={c} />}
          {view === "health" && <HealthView cluster={c} headlamp={inHeadlamp} />}
          {view === "machines" && <Machines cluster={c} />}
          {view === "options" && <Options cluster={c} can={can} onEdit={setAction} />}
        </div>
      </div>

      {action !== undefined && <ActionDialog action={action} cluster={c} onClose={close} />}
    </section>
  );
}

function ActionDialog({
  action,
  cluster: c,
  onClose,
}: {
  action: Action;
  cluster: Cluster;
  onClose: () => void;
}): JSX.Element {
  if (action === "scale") {
    return <ScaleDialog cluster={c} onClose={onClose} />;
  }
  if (action === "upgrade") {
    return <UpgradeDialog cluster={c} onClose={onClose} />;
  }
  if (action === "delete") {
    return <DeleteDialog cluster={c} onClose={onClose} />;
  }
  return <OptionDialog action={action} cluster={c} onClose={onClose} />;
}

function Summary({ cluster: c }: { cluster: Cluster }): JSX.Element {
  return (
    <div className="box">
      <dl className="summary">
        <dt>Status</dt>
        <dd>
          <StatusBadge cluster={c} />
        </dd>
        <dt>Distribution</dt>
        <dd>{distributionName(c.distribution)}</dd>
        <dt>Kubernetes</dt>
        <dd>{c.version}</dd>
        <dt>API endpoint</dt>
        <dd>{c.endpoint ?? "Not assigned yet"}</dd>
        <dt>Pod range</dt>
        <dd>{(c.pods ?? []).join(", ") || "The distribution's own"}</dd>
        <dt>Service range</dt>
        <dd>{(c.services ?? []).join(", ") || "The distribution's own"}</dd>
        <dt>Control plane</dt>
        <dd>
          {c.controlPlane.ready} of {c.controlPlane.desired} ready
        </dd>
        <dt>Workers</dt>
        <dd>
          {c.workers.desired === 0 && c.workers.ready === 0
            ? "None; the control planes run the workloads"
            : `${c.workers.ready} of ${c.workers.desired} ready`}
        </dd>
        <dt>Proxmox VE pool</dt>
        <dd>{poolName(c.pool)}</dd>
        <dt>Namespace</dt>
        <dd>{c.namespace}</dd>
        <dt>Created</dt>
        <dd>
          {new Date(c.created).toLocaleString()}
          {filled(c.createdBy) && ` by ${c.createdBy}`}
        </dd>
        {filled(c.autoUpgraded) && (
          <>
            <dt>Last automatic upgrade</dt>
            <dd>{autoUpgradeText(c.autoUpgraded)}</dd>
          </>
        )}
      </dl>
    </div>
  );
}

// machineStatus says how far a machine is, as the cluster list says it of clusters.
function machineStatus(m: Machine): { state: Health; text: string } {
  if (m.ready) {
    return { state: "ok", text: "Ready" };
  }
  if (m.phase === "Failed") {
    return { state: "error", text: "Failed" };
  }
  if (m.phase === "Deleting") {
    return { state: "deleting", text: "Deleting" };
  }
  return { state: "progress", text: "Not ready" };
}

/**
 * crowding says which Proxmox VE node runs more than one of the cluster's control-plane VMs, and
 * what losing it costs, or undefined when none does.
 */
export function crowding(machines: Machine[]): string | undefined {
  const controlPlane = machines.filter((m) => m.role === "control-plane");
  const perNode = new Map<string, number>();
  for (const m of controlPlane) {
    if (filled(m.proxmoxNode)) {
      perNode.set(m.proxmoxNode, (perNode.get(m.proxmoxNode) ?? 0) + 1);
    }
  }
  const [node, count] = [...perNode.entries()].toSorted((a, b) => b[1] - a[1])[0] ?? ["", 0];
  if (count < 2) {
    return undefined;
  }
  // etcd needs a majority of the control-plane nodes.
  const cost =
    count * 2 > controlPlane.length
      ? "losing that node stops the control plane"
      : `losing that node takes ${count} of them`;
  return `${node} runs ${count} of the ${controlPlane.length} control-plane VMs, so ${cost}. A new control-plane VM goes to the allowed node with the fewest of them; Proxmox VE can migrate one there now.`;
}

function Machines({ cluster: c }: { cluster: Cluster }) {
  const machines = c.machines ?? [];
  const crowded = crowding(machines);
  return (
    <div className="box scroll">
      {crowded !== undefined && <p className="warning">{crowded}</p>}
      <table className="grid">
        <thead>
          <tr>
            <th>Name</th>
            <th>Role</th>
            <th>Proxmox VE node</th>
            <th>Status</th>
            <th>Phase</th>
            <th>Version</th>
            <th>Addresses</th>
            <th>Age</th>
          </tr>
        </thead>
        <tbody>
          {machines.length === 0 && (
            <tr>
              <td colSpan={8} className="muted">
                No machines yet.
              </td>
            </tr>
          )}
          {machines.map((m) => {
            const s = machineStatus(m);
            return (
              <tr key={m.name}>
                <td>
                  {m.node ?? m.name}
                  {filled(m.waiting) && <div className="hint message">{m.waiting}</div>}
                </td>
                <td>{m.role === "control-plane" ? "Control plane" : "Worker"}</td>
                <td>{m.proxmoxNode ?? ""}</td>
                <td>
                  <Status state={s.state} text={s.text} />
                </td>
                <td>{m.phase}</td>
                <td>{m.version ?? ""}</td>
                <td>{(m.addresses ?? []).join(", ")}</td>
                <td title={new Date(m.created).toLocaleString()}>{age(m.created)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// Options lists what can be set on a cluster after it was created, each with its Edit button, as
// Proxmox VE's Options panels do.
function ScaleDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = c.controlPlane.desired || 1;
  const [controlPlane, setControlPlane] = useState(current);
  // Kept as typed, so a cleared field stays empty instead of turning into 0.
  const [pools, setPools] = useState<Record<string, string>>(
    Object.fromEntries(c.workerPools.map((p) => [p.name, String(p.replicas)])),
  );
  const { busy, error, send } = useChange(c, onClose);
  const invalid = Object.fromEntries(
    Object.entries(pools).map(([k, v]) => [
      k,
      /^\d+$/u.test(v) && Number(v) <= maxWorkers ? undefined : `A whole number from 0 to ${maxWorkers}.`,
    ]),
  );
  const changed = controlPlane !== current || c.workerPools.some((p) => pools[p.name] !== String(p.replicas));
  return (
    <Dialog
      title={`Scale ${c.name}`}
      onClose={onClose}
      onSubmit={() => {
        void send({
          controlPlaneReplicas: controlPlane,
          workerPools: Object.fromEntries(Object.entries(pools).map(([k, v]) => [k, Number(v)])),
        });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || Object.values(invalid).some(Boolean)}>
            Scale
          </button>
        </>
      }
    >
      <Field label="Control-plane nodes" hint="Three or more keep the cluster running when one node fails.">
        {(id) => (
          <select id={id} value={controlPlane} onChange={(e) => setControlPlane(Number(e.target.value))}>
            {[...new Set([...controlPlaneSizes, current])]
              .toSorted((a, b) => a - b)
              .map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
          </select>
        )}
      </Field>
      {c.workerPools.map((p) => (
        <Field key={p.name} label={`Workers (${p.name})`} error={invalid[p.name]}>
          {(id) => (
            <input
              id={id}
              type="number"
              min={0}
              max={maxWorkers}
              value={pools[p.name] ?? ""}
              onChange={(e) => setPools({ ...pools, [p.name]: e.target.value })}
            />
          )}
        </Field>
      ))}
      <ErrorBox message={error} />
    </Dialog>
  );
}

// parse reads a Kubernetes version, with RKE2's or k3s's build of it.
function parse(v: string): { minor: number; patch: number; build: number } | undefined {
  const g = /^v1\.(?<minor>\d+)\.(?<patch>\d+)(?:\+(?:rke2r|k3s)(?<build>\d+))?$/u.exec(v)?.groups;
  return g ? { minor: Number(g["minor"]), patch: Number(g["patch"]), build: Number(g["build"] ?? 0) } : undefined;
}

// upgradeTargets are the offered versions a cluster may move to: a newer release of its own minor
// version, or of the next one.
export function upgradeTargets(current: string, offered: string[]): string[] {
  const cur = parse(current);
  if (!cur) {
    return [];
  }
  return offered.filter((v) => {
    const t = parse(v);
    if (!t) {
      return false;
    }
    if (t.minor === cur.minor + 1) {
      return true;
    }
    return t.minor === cur.minor && (t.patch > cur.patch || (t.patch === cur.patch && t.build > cur.build));
  });
}

function UpgradeDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [version, setVersion] = useState(c.version);
  const [offered, setOffered] = useState<Versions>();
  const { busy, error, send } = useChange(c, onClose);
  useEffect(() => {
    async function loadVersions(): Promise<void> {
      try {
        setOffered(await api.versions(c.distribution));
      } catch (err) {
        setOffered({ versions: [], error: errorMessage(err) });
      }
    }
    void loadVersions();
  }, [c.distribution]);
  const targets = upgradeTargets(c.version, [...(offered?.versions ?? []), ...(offered?.older ?? [])]);
  return (
    <Dialog
      title={`Upgrade ${c.name}`}
      onClose={onClose}
      onSubmit={() => {
        void send({ version });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || version === c.version}>
            Upgrade
          </button>
        </>
      }
    >
      <Field
        label="Kubernetes version"
        hint="One minor version at a time. The control plane is replaced node by node first, then the workers."
      >
        {(id) =>
          targets.length > 0 ? (
            <select id={id} value={version} onChange={(e) => setVersion(e.target.value)}>
              <option value={c.version} disabled>
                {c.version} (running)
              </option>
              {targets.map((v) => (
                <option key={v} value={v}>
                  {v}
                </option>
              ))}
            </select>
          ) : (
            <input id={id} value={version} onChange={(e) => setVersion(e.target.value.trim())} />
          )
        }
      </Field>
      {offered && targets.length === 0 && (
        <p className="muted">
          {offered.error ??
            (c.distribution === "kubeadm"
              ? "No VM template of a newer version: build one on the Administration page first."
              : "This is the newest release of its minor version and the next.")}
        </p>
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}

function DeleteDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const remove = async (): Promise<void> => {
    setBusy(true);
    try {
      await api.remove(c.namespace, c.name);
      navigate("/");
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={`Delete ${c.name}`}
      onClose={onClose}
      onSubmit={() => {
        void remove();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={busy || typed !== c.name}>
            Delete
          </button>
        </>
      }
    >
      <p>
        This deletes the cluster's VMs and its persistent volumes, and removes it from Rancher. It cannot be undone.
      </p>
      <Field label="Confirm name" hint={`Type ${c.name}.`}>
        {(id) => <input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
