import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { AutoscalerDialog, ScaleDialog, autoscalerText, poolText } from "./Autoscaling";
import { BackupsDialog, BackupsView, backupsOf, backupsRows, useClassTargets } from "./Backups";
import { issuesFromCA, saveCA } from "./Certificates";
import { OptionDialog, Options, type OptionAction } from "./ClusterOptions";
import { EtcdSnapshotsDialog, EtcdSnapshotsView, etcdSnapshotsOf, etcdSnapshotsRows } from "./EtcdSnapshots";
import { HealthView } from "./Health";
import { nodeOSKey } from "./node-os";
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
  pollIdle,
  saveFile,
  settled,
  useChange,
  usePoll,
  type Health,
} from "./ui";

type Action = "scale" | "upgrade" | "delete" | "backups" | "autoscaler" | "etcdSnapshots" | OptionAction;

// A cluster on its way somewhere is looked at more often than a settled one.
const pollChanging = 5000;

const httpNotFound = 404;

/**
 * headlampURL is a cluster's page in the shared Headlamp, served on its own port of this host.
 * A cluster's goes through the gate's opening of it, which signs in again a session that signed
 * out of the cluster in Headlamp, and leads on to the cluster's page there.
 */
export function headlampURL(port: number, cluster?: string): string {
  const host = location.hostname.includes(":") ? `[${location.hostname}]` : location.hostname;
  return `https://${host}:${port}/${filled(cluster) ? `kubehenge/open/${encodeURIComponent(cluster)}` : ""}`;
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
  { path: "backups", label: "Backups" },
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

// buildText reads a template build's tag, build-YYYYMMDDhhmm in UTC, as a time in the reader's own
// terms.
export function buildText(build: string): string {
  const m = /^build-(?<y>\d{4})(?<mo>\d{2})(?<d>\d{2})(?<h>\d{2})(?<mi>\d{2})$/u.exec(build)?.groups;
  if (m === undefined) {
    return build;
  }
  const at = Date.UTC(Number(m["y"]), Number(m["mo"]) - 1, Number(m["d"]), Number(m["h"]), Number(m["mi"]));
  return `the build of ${new Date(at).toLocaleString()}`;
}

// nodeRefreshText reads the controller's record of the last node refresh, "from to to at time".
function nodeRefreshText(refreshed: string): string {
  const { from, to, at } = /^(?<from>\S+) to (?<to>\S+) at (?<at>\S+)$/u.exec(refreshed)?.groups ?? {};
  if (from === undefined || to === undefined || at === undefined || Number.isNaN(Date.parse(at))) {
    return refreshed;
  }
  return `${new Date(at).toLocaleString()}, from ${buildText(from)} to ${buildText(to)}`;
}

// NodeImageRows say which template builds a cluster's nodes are clones of, and when they move to a
// newer one.
function NodeImageRows({ image }: { image: NonNullable<Cluster["nodeImage"]> }): JSX.Element {
  let next = "None due: the nodes run the newest builds";
  if (!image.refreshes) {
    next = "Off: switched off in the cluster's automatic upgrades";
  } else if (image.next?.build !== undefined) {
    next = `${new Date(image.next.due).toLocaleString()}, to ${buildText(image.next.build)}`;
  } else if (image.next !== undefined) {
    next = `Not before ${new Date(image.next.due).toLocaleString()}, with the next template build`;
  }
  return (
    <>
      <dt>Node image</dt>
      <dd>
        {image.builds.length === 0
          ? "Not known yet"
          : image.builds.map((b) => `${b.template}, ${buildText(b.build)}`).join("; ")}
      </dd>
      <dt>Next node refresh</dt>
      <dd>{next}</dd>
      {filled(image.last) && (
        <>
          <dt>Last node refresh</dt>
          <dd>{nodeRefreshText(image.last)}</dd>
        </>
      )}
    </>
  );
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
  const download = async (save: () => Promise<void>): Promise<void> => {
    setDownloadError(undefined);
    try {
      await save();
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
              void download(async () => {
                saveFile(await api.kubeconfig(c.namespace, c.name), `${c.name}.kubeconfig`);
              });
            }}
          >
            Kubeconfig
          </button>
        )}
        {issuesFromCA(c) && (
          <button
            type="button"
            title="The certificate of the CA the cluster's certificates come from, for browsers and clients to trust"
            onClick={() => {
              void download(async () => saveCA(c));
            }}
          >
            CA certificate
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
          {view === "backups" && (
            <>
              <BackupsView cluster={c} can={can} onEdit={setAction} />
              <EtcdSnapshotsView cluster={c} can={can} onEdit={setAction} />
            </>
          )}
          {view === "options" && <OptionsView cluster={c} can={can} onEdit={setAction} />}
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
  if (action === "backups") {
    return <BackupsDialog cluster={c} onClose={onClose} />;
  }
  if (action === "autoscaler") {
    return <AutoscalerDialog cluster={c} onClose={onClose} />;
  }
  if (action === "etcdSnapshots") {
    return <EtcdSnapshotsDialog cluster={c} onClose={onClose} />;
  }
  return <OptionDialog action={action} cluster={c} onClose={onClose} />;
}

// OptionsView is the Options view, with the rows of the settings whose dialogs live with views of
// their own, and the autoscaler's, which lives with the Scale dialog's.
function OptionsView({
  cluster: c,
  can,
  onEdit,
}: {
  cluster: Cluster;
  can: boolean;
  onEdit: (a: Action) => void;
}): JSX.Element {
  const offered = (useClassTargets(c.class) ?? []).length > 0;
  const more = [
    ...backupsRows(c, offered),
    ...etcdSnapshotsRows(c, offered),
    { label: "Autoscaler", value: autoscalerText(c), action: "autoscaler" as const, name: "the autoscaler" },
  ];
  return <Options cluster={c} can={can} onEdit={onEdit} more={more} />;
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
        {c.nodeOS && (
          <>
            <dt>Node OS</dt>
            <dd>{c.nodeOS.title}</dd>
          </>
        )}
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
        {c.workerPools.some((p) => p.autoscaling) && (
          <>
            <dt>Worker pools</dt>
            <dd>{c.workerPools.map((p) => `${p.name}: ${poolText(p)}`).join("; ")}</dd>
          </>
        )}
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
        {c.nodeImage && <NodeImageRows image={c.nodeImage} />}
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
  // A kubeadm cluster moves only to versions with a template on its node OS.
  const kubeadmOS = c.distribution === "kubeadm" && c.nodeOS ? nodeOSKey(c.nodeOS) : undefined;
  useEffect(() => {
    async function loadVersions(): Promise<void> {
      try {
        setOffered(await api.versions(c.distribution, kubeadmOS));
      } catch (err) {
        setOffered({ versions: [], error: errorMessage(err) });
      }
    }
    void loadVersions();
  }, [c.distribution, kubeadmOS]);
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
        {backupsOf(c.variables["backups"]).enabled &&
          " Its backups stay in the target, and a cluster created again under the same name and target can restore them."}
        {etcdSnapshotsOf(c.variables["etcdSnapshots"]).enabled && " Its etcd snapshots stay in the target too."}
      </p>
      <Field label="Confirm name" hint={`Type ${c.name}.`}>
        {(id) => <input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
