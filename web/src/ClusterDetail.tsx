import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { AutoscalerDialog, ScaleDialog, autoscalerText, poolText } from "./Autoscaling";
import { BackupsDialog, BackupsView, backupsOf, backupsRows, useClassTargets } from "./Backups";
import { issuesFromCA, saveCA } from "./Certificates";
import { OptionDialog, Options, type OptionAction, type OptionRow } from "./ClusterOptions";
import { Deleting } from "./Deletion";
import { EtcdSnapshotsDialog, EtcdSnapshotsView, etcdSnapshotsOf, etcdSnapshotsRows } from "./EtcdSnapshots";
import { HealthView } from "./Health";
import { Machines, Placement } from "./Machines";
import { nodeOSKey } from "./node-os";
import { RecoveryState, RollStalled } from "./Recovery";
import { Link, navigate } from "./router";
import { StagedBar, StagingProvider } from "./Staging";
import { useStaging } from "./staging-context";
import { TalosFailure, TalosRows, runsKubernetes, useTalosReleases } from "./Talos";
import type { Cluster, Versions } from "./types";
import {
  Dialog,
  ErrorBox,
  Field,
  StatusBadge,
  distributionName,
  errorMessage,
  filled,
  pollIdle,
  saveFile,
  settled,
  useChange,
  usePoll,
  Views,
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
  return `build ${new Date(at).toLocaleString()}`;
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
  let next = "None due";
  if (!image.refreshes) {
    next = "Off";
  } else if (image.next?.build !== undefined) {
    next = `${new Date(image.next.due).toLocaleString()}, to ${buildText(image.next.build)}`;
  } else if (image.next !== undefined) {
    next = `With the next build, from ${new Date(image.next.due).toLocaleString()}`;
  }
  return (
    <>
      <dt>Node image</dt>
      <dd>
        {image.builds.length === 0
          ? "Unknown"
          : image.builds.map((b) => <div key={b.template}>{`${b.template}, ${buildText(b.build)}`}</div>)}
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
    return "Being deleted";
  }
  return c.allowed.change ? undefined : `Changes need VM.Allocate on pool ${poolName(c.pool)}`;
}

// pageNote is why the actions are off, under them; a cluster being deleted says so in its status.
const pageNote = (c: Cluster): string | undefined => (c.deleting ? undefined : whyNot(c));

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
          Cluster not found. <Link to="/">All clusters</Link>
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
    <StagingProvider
      key={`${c.namespace}/${c.name}`}
      cluster={c}
      onApplied={() => {
        void reload();
      }}
    >
      <section className="page">
        <div className="page-head">
          <div className="page-title">
            <h2>
              <span className="id">{c.name}</span> <StatusBadge cluster={c} />
            </h2>
            <Facts cluster={c} />
          </div>
          <div className="page-actions">
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
            {c.allowed.kubeconfig && c.distribution === "talos" && (
              <button
                type="button"
                title="For talosctl"
                onClick={() => {
                  void download(async () => {
                    saveFile(await api.talosconfig(c.namespace, c.name), `${c.name}.talosconfig`);
                  });
                }}
              >
                Talosconfig
              </button>
            )}
            <SupportBundleButton cluster={c} download={download} />
            {issuesFromCA(c) && (
              <button
                type="button"
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
        </div>
        {pageNote(c) !== undefined && <p className="muted page-note">{pageNote(c)}</p>}
        <HeldUp cluster={c} />
        <ErrorBox message={error} />
        <ErrorBox message={downloadError} />
        <Deleting
          cluster={c}
          onAbandoned={() => {
            void reload();
          }}
        />
        <RollStalled
          cluster={c}
          why={why}
          onRecovered={() => {
            void reload();
          }}
        />

        <Views label={`${c.name}'s views`} open={view}>
          {views.map((v, i) => (
            <Link key={v.path} to={clusterPath(c.namespace, c.name, v.path)} current={view === v.path}>
              {v.label}
              <kbd aria-hidden="true">{i + 1}</kbd>
            </Link>
          ))}
        </Views>
        <StagedBar cluster={c} />
        <div className="object">
          <div className="object-view">
            {view === "" && <Placement cluster={c} />}
            {view === "" && (
              <Summary
                cluster={c}
                can={can}
                why={why}
                onChanged={() => {
                  void reload();
                }}
              />
            )}
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
    </StagingProvider>
  );
}

// HeldUp is what holds a cluster up as the controller reports it on the Cluster, above every view:
// the error it last met there, and machines that are stuck.
function HeldUp({ cluster: c }: { cluster: Cluster }): JSX.Element | null {
  if (!filled(c.controllerError) && !filled(c.stalled)) {
    return null;
  }
  return (
    <>
      {filled(c.controllerError) && (
        <div className="error held-up" role="alert">
          <strong>Controller error</strong>
          <pre>{c.controllerError}</pre>
        </div>
      )}
      {filled(c.stalled) && (
        <div className="warning held-up">
          <strong>Machines stuck for 20 minutes</strong>
          <pre>{c.stalled}</pre>
        </div>
      )}
    </>
  );
}

/** bundleName is what a cluster's support bundle is saved as, the time in UTC as the command line names it. */
export const bundleName = (ns: string, name: string, at = new Date()): string =>
  `${ns}-${name}-${at
    .toISOString()
    .replaceAll(/[-:]/gu, "")
    .replace(/\.\d+Z$/u, "Z")}.tar.gz`;

// SupportBundleButton goes to those the admin kubeconfig goes to: a bundle's logs may hold what the
// redaction misses.
function SupportBundleButton({
  cluster: c,
  download,
}: {
  cluster: Cluster;
  download: (save: () => Promise<void>) => Promise<void>;
}): JSX.Element | null {
  const [bundling, setBundling] = useState(false);
  if (!c.allowed.kubeconfig) {
    return null;
  }
  const save = async (): Promise<void> => {
    setBundling(true);
    try {
      await download(async () => {
        saveFile(await api.supportBundle(c.namespace, c.name), bundleName(c.namespace, c.name));
      });
    } finally {
      setBundling(false);
    }
  };
  return (
    <button
      type="button"
      disabled={bundling}
      onClick={() => {
        void save();
      }}
    >
      {bundling ? "Writing bundle…" : "Support bundle"}
    </button>
  );
}

const counted = (n: number, what: string): string => `${n} ${what}${n === 1 ? "" : "s"}`;

// Facts are what a cluster is, in a line under its name.
function Facts({ cluster: c }: { cluster: Cluster }): JSX.Element {
  return (
    <p className="facts">
      <span>{distributionName(c.distribution)}</span>
      <span className="id">{c.version}</span>
      {c.talos && <span className="id">Talos {c.talos.version}</span>}
      <span>
        {counted(c.controlPlane.desired, "control plane")}, {counted(c.workers.desired, "worker")}
      </span>
      {filled(c.endpoint) && <span className="id">{c.endpoint}</span>}
      {c.namespace !== "default" && <span>namespace {c.namespace}</span>}
    </p>
  );
}

function ActionDialog({
  action,
  cluster: current,
  onClose,
}: {
  action: Action;
  cluster: Cluster;
  onClose: () => void;
}): JSX.Element {
  // A dialog starts from what is staged, so that a second change builds on the first.
  const c = useStaging()?.staged?.plan.cluster ?? current;
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
  const staged = useStaging()?.staged?.plan.cluster;
  const more = (shown: Cluster): OptionRow<"backups" | "etcdSnapshots" | "autoscaler">[] => [
    ...backupsRows(shown, offered),
    ...etcdSnapshotsRows(shown, offered),
    { label: "Autoscaler", value: autoscalerText(shown), action: "autoscaler", name: "the autoscaler" },
  ];
  return <Options cluster={c} staged={staged} can={can} onEdit={onEdit} more={more} />;
}

function Summary({
  cluster: c,
  can,
  why,
  onChanged,
}: {
  cluster: Cluster;
  can: boolean;
  why: string | undefined;
  onChanged: () => void;
}): JSX.Element {
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
        {c.talos && <TalosRows talos={c.talos} />}
        <dt>API endpoint</dt>
        <dd>{c.endpoint ?? "Not assigned yet"}</dd>
        <dt>Pod range</dt>
        <dd>{(c.pods ?? []).join(", ") || "Default"}</dd>
        <dt>Service range</dt>
        <dd>{(c.services ?? []).join(", ") || "Default"}</dd>
        <dt>Control plane</dt>
        <dd>
          {c.controlPlane.ready} of {c.controlPlane.desired} ready
        </dd>
        <dt>Workers</dt>
        <dd>
          {c.workers.desired === 0 && c.workers.ready === 0
            ? "None"
            : `${c.workers.ready} of ${c.workers.desired} ready`}
        </dd>
        {c.workerPools.some((p) => p.autoscaling) && (
          <>
            <dt>Worker pools</dt>
            <dd>
              {c.workerPools.map((p) => (
                <div key={p.name}>{`${p.name}: ${poolText(p)}`}</div>
              ))}
            </dd>
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
      {filled(c.talos?.failed) && (
        <TalosFailure cluster={c} failed={c.talos.failed} can={can} why={why} onRetried={onChanged} />
      )}
      <RecoveryState cluster={c} />
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
  const talos = useTalosReleases(c.talos !== undefined);
  const running = c.talos?.version ?? "";
  const held = c.talos ? targets.filter((v) => !runsKubernetes(talos, running, v)) : [];
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
      <Field label="Kubernetes version" hint="One minor version at a time">
        {(id) =>
          targets.length > 0 ? (
            <select id={id} value={version} onChange={(e) => setVersion(e.target.value)}>
              <option value={c.version} disabled>
                {c.version} (running)
              </option>
              {targets.map((v) => (
                <option key={v} value={v} disabled={held.includes(v)}>
                  {held.includes(v) ? `${v} (needs newer Talos)` : v}
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
              ? "No newer VM template. Build one under Administration."
              : "No newer version")}
        </p>
      )}
      {c.talos && <TalosNote running={running} held={held} upgrading={filled(c.talos.upgrading)} />}
      <ErrorBox message={error} />
    </Dialog>
  );
}

// TalosNote says what the Talos release in the way holds back: versions it does not run, or the
// whole upgrade while Talos itself upgrades.
function TalosNote({
  running,
  held,
  upgrading,
}: {
  running: string;
  held: string[];
  upgrading: boolean;
}): JSX.Element | null {
  const notes = [
    ...(held.length > 0
      ? [`Talos ${running} does not run ${held.join(" or ")}: upgrade Talos first, under Options.`]
      : []),
    ...(upgrading ? ["Waits for the Talos upgrade."] : []),
  ];
  return notes.length > 0 ? <p className="muted">{notes.join(" ")}</p> : null;
}

function DeleteDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  // A new cluster of the same name and target restores what stays behind.
  const kept = [
    ...(backupsOf(c.variables["backups"]).enabled ? ["Backups"] : []),
    ...(etcdSnapshotsOf(c.variables["etcdSnapshots"]).enabled ? ["etcd snapshots"] : []),
  ];
  const remove = async (): Promise<void> => {
    setBusy(true);
    try {
      await api.remove(c.namespace, c.name, typed);
      navigate("/");
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={`Delete cluster ${c.name}`}
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
            Delete cluster
          </button>
        </>
      }
    >
      <p>Deletes its VMs and persistent volumes, and removes it from Rancher. This can&apos;t be undone.</p>
      {kept.length > 0 && <p>{`${kept.join(" and ")} stay in the target.`}</p>}
      <Field label="Confirm name" hint={`Type ${c.name}.`}>
        {(id) => <input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
