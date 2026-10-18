import { useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import { UpgradesDialog, describe, upgradePolicy } from "./ClusterUpgrades";
import { OIDCProviders, cleanProviders, oidcProblems, oidcProviders } from "./OIDCProviders";
import {
  HAField,
  PlacementFields,
  allowedNodes,
  haText,
  placeOf,
  placeProblem,
  placeText,
  useHAOffered,
  useHARules,
} from "./Placement";
import { PodSecurityFields, podSecurityOf, podSecurityText } from "./PodSecurity";
import { Link, navigate } from "./router";
import type { ChangeRequest, Cluster, Machine, OIDCProvider, PveNode, Versions } from "./types";
import {
  Dialog,
  ErrorBox,
  Field,
  Status,
  StatusBadge,
  abnormal,
  age,
  distributionName,
  errorMessage,
  filled,
  pollIdle,
  registerHint,
  registeredWithRancher,
  saveFile,
  settled,
  usePoll,
  type Health,
} from "./ui";
import { flag, record, text, texts } from "./values";

type Action =
  | "scale"
  | "upgrade"
  | "delete"
  | "placement"
  | "ha"
  | "headlamp"
  | "monitoring"
  | "podSecurity"
  | "rancher"
  | "upgrades"
  | "oidc";

// A cluster on its way somewhere is looked at more often than a settled one.
const pollChanging = 5000;

const httpNotFound = 404;

export const maxWorkers = 100;

// Odd sizes keep etcd's quorum.
// oxlint-disable-next-line eslint/no-magic-numbers -- the sizes themselves, named by the constant
export const controlPlaneSizes = [1, 3, 5, 7];

/** headlampURL is a cluster's page in the shared Headlamp, served on its own port of this host. */
export function headlampURL(port: number, cluster?: string): string {
  const host = location.hostname.includes(":") ? `[${location.hostname}]` : location.hostname;
  return `https://${host}:${port}/${filled(cluster) ? `c/${encodeURIComponent(cluster)}` : ""}`;
}

const poolName = (pool: string): string => pool.replace(/^\/pool\//u, "");

/** The views of a cluster's page, as the last part of its address; the summary has none. */
export const views = [
  { path: "", label: "Summary" },
  { path: "machines", label: "Machines" },
  { path: "conditions", label: "Conditions" },
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
        {headlamp !== undefined && headlamp > 0 && filled(c.headlamp) && c.available && (
          <a className="button" href={headlampURL(headlamp, c.headlamp)} target="_blank" rel="noopener noreferrer">
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
          {view === "machines" && <Machines cluster={c} />}
          {view === "conditions" && <Conditions cluster={c} />}
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
  switch (action) {
    case "scale": {
      return <ScaleDialog cluster={c} onClose={onClose} />;
    }
    case "upgrade": {
      return <UpgradeDialog cluster={c} onClose={onClose} />;
    }
    case "delete": {
      return <DeleteDialog cluster={c} onClose={onClose} />;
    }
    case "headlamp":
    case "monitoring": {
      return <HostedAddonDialog cluster={c} addon={action} onClose={onClose} />;
    }
    case "podSecurity": {
      return <PodSecurityDialog cluster={c} onClose={onClose} />;
    }
    case "rancher": {
      return <RancherDialog cluster={c} onClose={onClose} />;
    }
    case "upgrades": {
      return <UpgradesDialog cluster={c} onClose={onClose} />;
    }
    case "placement": {
      return <PlacementDialog cluster={c} onClose={onClose} />;
    }
    case "ha": {
      return <HADialog cluster={c} onClose={onClose} />;
    }
    case "oidc": {
      break;
    }
  }
  return <OIDCDialog cluster={c} onClose={onClose} />;
}

interface HostedValue {
  enabled?: boolean | undefined;
  hostname?: string | undefined;
  tlsSecret?: string | undefined;
}

function hostedValue(v: unknown): HostedValue {
  const r = record(v);
  return { enabled: flag(r["enabled"]), hostname: text(r["hostname"]), tlsSecret: text(r["tlsSecret"]) };
}

interface RancherValue {
  install?: boolean | undefined;
  hostname?: string | undefined;
  register?: boolean | undefined;
}

function rancherValue(v: unknown): RancherValue {
  const r = record(v);
  return { install: flag(r["install"]), hostname: text(r["hostname"]), register: flag(r["register"]) };
}

function rancherText(r: RancherValue): string {
  if (r.install === true) {
    return `Installed here, at ${r.hostname ?? ""}`;
  }
  return r.register === false ? "Not registered" : registeredWithRancher;
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
          {c.workers.ready} of {c.workers.desired} ready
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

function Conditions({ cluster: c }: { cluster: Cluster }) {
  return (
    <div className="box scroll">
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
          {c.conditions.length === 0 && (
            <tr>
              <td colSpan={5} className="muted">
                No conditions reported yet.
              </td>
            </tr>
          )}
          {c.conditions.map((x) => {
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
                <td title={new Date(x.since).toLocaleString()}>{age(x.since)}</td>
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
function Options({
  cluster: c,
  can,
  onEdit,
}: {
  cluster: Cluster;
  can: boolean;
  onEdit: (a: Action) => void;
}): JSX.Element {
  const headlamp = hostedValue(c.variables["headlamp"]);
  const monitoring = hostedValue(c.variables["monitoring"]);
  const rancher = rancherValue(c.variables["rancher"]);
  const haOffered = useHAOffered(c.class, text(c.variables["storage"]));
  // name is how the row's button names what it changes, for those who hear it out of the grid.
  const rows: { label: string; value: string; action?: Action; button?: string; name?: string }[] = [
    { label: "Pod network", value: text(c.variables["cni"]) ?? "The distribution's own" },
    {
      label: "Placement",
      value: placeText(placeOf(texts(c.variables["allowedNodes"]))),
      action: "placement",
      name: "placement",
    },
  ];
  if (haOffered) {
    rows.push({ label: "Proxmox VE HA", value: haText(haOn(c)), action: "ha", name: "Proxmox VE HA" });
  }
  rows.push(
    {
      label: "Headlamp",
      value: headlamp.enabled === true ? `On${filled(headlamp.hostname) ? `, at ${headlamp.hostname}` : ""}` : "Off",
      action: "headlamp",
      name: "Headlamp",
    },
    {
      label: "Monitoring",
      value:
        monitoring.enabled === true
          ? `On${filled(monitoring.hostname) ? `, Grafana at ${monitoring.hostname}` : ""}`
          : "Off",
      action: "monitoring",
      name: "monitoring",
    },
    {
      label: "Pod security",
      value: podSecurityText(podSecurityOf(c.variables["podSecurity"])),
      action: "podSecurity",
      name: "pod security",
    },
    {
      label: "Rancher",
      value: rancherText(rancher),
      action: "rancher",
      button: rancher.install === true ? "Remove" : "Edit",
      name: "Rancher",
    },
    {
      label: "Automatic upgrades",
      value: describe(upgradePolicy(c.variables["upgrades"])),
      action: "upgrades",
      name: "automatic upgrades",
    },
  );
  if ("oidc" in c.variables) {
    const hosts = providers(c).map((p) => (URL.canParse(p.issuerURL) ? new URL(p.issuerURL).host : p.issuerURL));
    rows.push({
      label: "Identity providers",
      value: ["Tokens and certificates", ...hosts].join(", "),
      action: "oidc",
      name: "identity providers",
    });
  }
  return (
    <div className="box scroll">
      <table className="grid options">
        <tbody>
          {rows.map(({ label, value, action, button = "Edit", name = label }) => (
            <tr key={label}>
              <th scope="row">{label}</th>
              <td>{value}</td>
              <td className="row-action">
                {action !== undefined && (
                  <button type="button" disabled={!can} aria-label={`${button} ${name}`} onClick={() => onEdit(action)}>
                    {button}
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// providers are the cluster's own, every one: one with a broken issuer stays, for the dialog to
// flag, so that saving never drops one unseen.
const providers = (c: Cluster): OIDCProvider[] => oidcProviders(c.variables["oidc"]);

function OIDCDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [value, setValue] = useState<OIDCProvider[]>(providers(c));
  const { busy, error, send } = useChange(c, onClose);
  const open = oidcProblems(value);
  const changed = JSON.stringify(cleanProviders(value)) !== JSON.stringify(cleanProviders(providers(c)));
  return (
    <Dialog
      title="Identity providers"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { oidc: cleanProviders(value) } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || open.length > 0}>
            Save
          </button>
        </>
      }
    >
      <p className="muted">
        OpenID Connect providers the cluster accepts sign-ins from, besides tokens and certificates. Saving replaces the
        control-plane nodes one at a time. Adding a provider beyond those Kubehenge's administrators set needs
        Sys.Modify on the cluster's pool.
      </p>
      <OIDCProviders initial={value} fixed={!c.allowed.kubeconfig} onChange={setValue} />
      <ErrorBox message={error} />
    </Dialog>
  );
}

function PodSecurityDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = podSecurityOf(c.variables["podSecurity"]);
  const [value, setValue] = useState(current);
  const { busy, error, send } = useChange(c, onClose);
  return (
    <Dialog
      title="Pod security"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { podSecurity: { admission: value.admission, kyverno: value.kyverno } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className="primary"
            disabled={busy || (value.admission === current.admission && value.kyverno === current.kyverno)}
          >
            Save
          </button>
        </>
      }
    >
      <PodSecurityFields value={value} onChange={setValue} />
      <ErrorBox message={error} />
    </Dialog>
  );
}

// haOn reads the cluster's proxmoxHA variable, which takes part unless it is false.
const haOn = (c: Cluster): boolean => flag(c.variables["proxmoxHA"]) !== false;

function HADialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [on, setOn] = useState(haOn(c));
  const { busy, error, send } = useChange(c, onClose);
  return (
    <Dialog
      title="Proxmox VE HA"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { proxmoxHA: on } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || on === haOn(c)}>
            Save
          </button>
        </>
      }
    >
      <HAField checked={on} onChange={setOn} />
      <ErrorBox message={error} />
    </Dialog>
  );
}

function PlacementDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const kept = texts(c.variables["allowedNodes"]);
  const initial = placeOf(kept);
  const [value, setValue] = useState(initial);
  const [nodes, setNodes] = useState<PveNode[]>([]);
  const [nodesError, setNodesError] = useState<string>();
  const rules = useHARules();
  const { busy, error, send } = useChange(c, onClose);
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        setNodes(await api.nodes());
      } catch (err) {
        setNodesError(errorMessage(err));
      }
    }
    void load();
  }, []);
  const problem = placeProblem(value);
  const next = allowedNodes(value) ?? null;
  const changed = JSON.stringify(next) !== JSON.stringify(allowedNodes(initial) ?? null);
  return (
    <Dialog
      title="Placement"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { allowedNodes: next } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed || problem !== undefined}>
            Save
          </button>
        </>
      }
    >
      <PlacementFields
        value={value}
        onChange={setValue}
        problem={problem}
        hint="The Proxmox VE nodes the cluster's VMs may run on. A change applies to the VMs created after it, by scaling up, an upgrade or a replacement; those running stay where they are."
        nodes={nodes}
        kept={kept}
        rules={rules}
        framed={false}
      />
      <ErrorBox message={nodesError ?? error} />
    </Dialog>
  );
}

// useChange sends one change and closes the dialog when it is accepted.
function useChange(
  c: Cluster,
  onClose: () => void,
): { busy: boolean; error: string | undefined; send: (req: ChangeRequest) => Promise<void> } {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const send = async (req: ChangeRequest): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.change(c.namespace, c.name, req);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, send };
}

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

/** The add-ons switched on and off by an enabled flag, and served at a host name when one is set. */
const hostedAddons = {
  headlamp: {
    title: "Headlamp",
    install: "Install Headlamp",
    hint: "A web interface inside the cluster. People sign in to it with their own token; it has no rights of its own.",
    hostLabel: "Host name",
    hostHint: "Optional: also serve it at this name through the cluster's ingress controller.",
  },
  monitoring: {
    title: "Monitoring",
    install: "Install monitoring",
    hint: "Prometheus, Alertmanager and Grafana, with dashboards and alerts for every node and control-plane component. Grafana's admin password is in the kube-prometheus-stack-grafana Secret in the monitoring namespace.",
    hostLabel: "Grafana host name",
    hostHint: "Optional: serve Grafana at this name through the cluster's ingress controller.",
  },
} as const;

type HostedAddon = keyof typeof hostedAddons;

function HostedAddonDialog({
  cluster: c,
  addon,
  onClose,
}: {
  cluster: Cluster;
  addon: HostedAddon;
  onClose: () => void;
}): JSX.Element {
  const words = hostedAddons[addon];
  const current = hostedValue(c.variables[addon]);
  const [enabled, setEnabled] = useState(current.enabled === true);
  const [hostname, setHostname] = useState(current.hostname ?? "");
  const { busy, error, send } = useChange(c, onClose);
  const value: HostedValue = { enabled };
  if (enabled && hostname) {
    value.hostname = hostname;
  }
  if (enabled && hostname && filled(current.tlsSecret)) {
    value.tlsSecret = current.tlsSecret;
  }
  const changed = enabled !== (current.enabled === true) || (enabled && hostname !== (current.hostname ?? ""));
  const hint = useId();
  return (
    <Dialog
      title={words.title}
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { [addon]: value } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed}>
            Save
          </button>
        </>
      }
    >
      <label className="check">
        <input
          type="checkbox"
          checked={enabled}
          aria-describedby={hint}
          onChange={(e) => setEnabled(e.target.checked)}
        />{" "}
        {words.install}
      </label>
      <div className="hint" id={hint}>
        {words.hint}
      </div>
      {enabled && (
        <Field label={words.hostLabel} hint={words.hostHint}>
          {(id) => <input id={id} value={hostname} onChange={(e) => setHostname(e.target.value.trim())} />}
        </Field>
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}

function RancherDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  // Changed as a whole, with whatever else the variable holds kept as it is.
  const held = record(c.variables["rancher"]);
  const current = rancherValue(held);
  const [register, setRegister] = useState(current.register !== false);
  const [typed, setTyped] = useState("");
  const { busy, error, send } = useChange(c, onClose);
  const hint = useId();
  if (current.install === true) {
    return (
      <Dialog
        title="Remove Rancher"
        onClose={onClose}
        onSubmit={() => {
          void send({ variables: { rancher: { ...held, install: false } } });
        }}
        actions={
          <>
            <button type="button" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="danger" disabled={busy || typed !== c.name}>
              Remove Rancher
            </button>
          </>
        }
      >
        <p>
          Rancher runs in this cluster{filled(current.hostname) ? `, at ${current.hostname}` : ""}. Removing it first
          takes every cluster registered with it out of Rancher, which cleans up inside each, and then uninstalls it.
          Rancher's own settings, users and projects are lost; the clusters themselves keep running.
        </p>
        <Field label="Confirm name" hint={`Type ${c.name}.`}>
          {(id) => <input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />}
        </Field>
        <ErrorBox message={error} />
      </Dialog>
    );
  }
  return (
    <Dialog
      title="Rancher"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { rancher: { ...held, register } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || register === (current.register !== false)}>
            Save
          </button>
        </>
      }
    >
      <label className="check">
        <input
          type="checkbox"
          checked={register}
          aria-describedby={hint}
          onChange={(e) => setRegister(e.target.checked)}
        />{" "}
        Register with Rancher
      </label>
      <div className="hint" id={hint}>
        {registerHint} Leaving Rancher removes Rancher&apos;s agent from the cluster.
      </div>
      <ErrorBox message={error} />
    </Dialog>
  );
}
