import { Fragment, useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import { CertificatesDialog, certificatesOf, certificatesText } from "./Certificates";
import { ClusterMeshDialog, meshText, runsCilium } from "./ClusterMesh";
import { UpgradesDialog, describe, refreshesNodes, upgradePolicy } from "./ClusterUpgrades";
import { CSIDialog, SnapshotsDialog, csiOn, snapshotsOn, useCSIOffer, useSnapshotsOffer } from "./CsiPlugin";
import { GitOpsDialog, gitOpsOf, gitOpsText } from "./GitOps";
import { LoadBalancerDialog, loadBalancerOf, loadBalancerText } from "./LoadBalancer";
import { LogsDialog, logsOf, logsText } from "./Logs";
import { choosable, cpuNote, findNodeOS, nodeOSKey, nodeOSRef } from "./node-os";
import { OIDCProviders, cleanProviders, oidcProblems, oidcProviders } from "./OIDCProviders";
import {
  HADialog,
  HAGroupsDialog,
  PlacementDialog,
  haGroupsOn,
  haOn,
  haText,
  placeOf,
  placeText,
  useHAGroupsOffered,
  useHAOffered,
} from "./Placement";
import { PodSecurityFields, cisText, enforced, podSecurityOf, podSecurityText } from "./PodSecurity";
import { TalosDialog, talosOf } from "./Talos";
import type { Cluster, NodeOSChoice, OIDCProvider } from "./types";
import {
  Dialog,
  ErrorBox,
  Field,
  cniName,
  errorMessage,
  filled,
  sentence,
  headlampHint,
  headlampText,
  headlampViewHint,
  headlampViewLabel,
  istioHint,
  istioText,
  registerHint,
  registeredWithRancher,
  csiText,
  haGroupsText,
  snapshotsText,
  useChange,
} from "./ui";
import { flag, record, text, texts } from "./values";

/** What the Options view changes, each through a dialog of its own. */
export type OptionAction =
  | "nodeOS"
  | "placement"
  | "ha"
  | "csi"
  | "snapshots"
  | "haGroups"
  | "headlamp"
  | "monitoring"
  | "logs"
  | "podSecurity"
  | "istio"
  | "clusterMesh"
  | "loadBalancer"
  | "rancher"
  | "gitops"
  | "upgrades"
  | "certificates"
  | "talos"
  | "oidc";

/** A setting the Options view lists, and the action of the dialog that changes it. */
export interface OptionRow<A extends string = OptionAction> {
  label: string;
  value: string;
  action?: A;
  button?: string;
  /** How the row's button names what it changes, for those who hear it out of the grid. */
  name?: string;
}

export function OptionDialog({
  action,
  cluster: c,
  onClose,
}: {
  action: OptionAction;
  cluster: Cluster;
  onClose: () => void;
}): JSX.Element {
  switch (action) {
    case "headlamp":
    case "monitoring": {
      return <HostedAddonDialog cluster={c} addon={action} onClose={onClose} />;
    }
    case "logs": {
      return <LogsDialog cluster={c} onClose={onClose} />;
    }
    case "podSecurity": {
      return <PodSecurityDialog cluster={c} onClose={onClose} />;
    }
    case "istio": {
      return <IstioDialog cluster={c} onClose={onClose} />;
    }
    case "clusterMesh": {
      return <ClusterMeshDialog cluster={c} onClose={onClose} />;
    }
    case "loadBalancer": {
      return <LoadBalancerDialog cluster={c} onClose={onClose} />;
    }
    case "rancher": {
      return <RancherDialog cluster={c} onClose={onClose} />;
    }
    case "gitops": {
      return <GitOpsDialog cluster={c} onClose={onClose} />;
    }
    case "upgrades": {
      return <UpgradesDialog cluster={c} onClose={onClose} />;
    }
    case "certificates": {
      return <CertificatesDialog cluster={c} onClose={onClose} />;
    }
    case "nodeOS": {
      return <NodeOSDialog cluster={c} onClose={onClose} />;
    }
    case "placement": {
      return <PlacementDialog cluster={c} onClose={onClose} />;
    }
    case "ha": {
      return <HADialog cluster={c} onClose={onClose} />;
    }
    case "csi": {
      return <CSIDialog cluster={c} onClose={onClose} />;
    }
    case "snapshots": {
      return <SnapshotsDialog cluster={c} onClose={onClose} />;
    }
    case "haGroups": {
      return <HAGroupsDialog cluster={c} onClose={onClose} />;
    }
    case "talos": {
      return <TalosDialog cluster={c} onClose={onClose} />;
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
  /** Headlamp's access: token, or view for everyone through its own account. */
  access?: string | undefined;
}

function hostedValue(v: unknown): HostedValue {
  const r = record(v);
  return {
    enabled: flag(r["enabled"]),
    hostname: text(r["hostname"]),
    tlsSecret: text(r["tlsSecret"]),
    access: text(r["access"]),
  };
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

// nodeOSRows is the node OS's row, for every cluster but a Talos one, whose nodes run Talos.
const nodeOSRows = (c: Cluster): OptionRow[] =>
  c.nodeOS ? [{ label: "Node OS", value: c.nodeOS.title, action: "nodeOS", name: "the node OS" }] : [];

/**
 * Options lists what can be set on a cluster after it was created, each with its Edit button, as
 * Proxmox VE's Options panels do. more are the rows of settings whose dialogs live elsewhere, with
 * views of their own or the Scale dialog's, after logs.
 */
export function Options<A extends string = never>({
  cluster: c,
  can,
  onEdit,
  more,
}: {
  cluster: Cluster;
  can: boolean;
  onEdit: (a: OptionAction | A) => void;
  more?: OptionRow<A>[];
}): JSX.Element {
  const headlamp = hostedValue(c.variables["headlamp"]);
  const monitoring = hostedValue(c.variables["monitoring"]);
  const rancher = rancherValue(c.variables["rancher"]);
  const upgrades = upgradePolicy(c.variables["upgrades"]);
  const haOffered = useHAOffered(c.class, text(c.variables["storage"]));
  const csi = useCSIOffer(c);
  const snapshots = useSnapshotsOffer(c);
  const haGroups = useHAGroupsOffered(c);
  const cis = c.variables["cis"] === true;
  const podSecurity = enforced(podSecurityOf(c.variables["podSecurity"]), cis);
  const talos: OptionRow[] =
    c.distribution === "talos"
      ? [{ label: "Talos release", value: talosOf(c), action: "talos", button: "Upgrade", name: "Talos" }]
      : [];
  const rows: OptionRow<OptionAction | A>[] = [
    ...nodeOSRows(c),
    ...talos,
    { label: "Pod network", value: cniName(text(c.variables["cni"]) ?? "") || "The distribution's own" },
    {
      label: "Load balancer",
      value: loadBalancerText(loadBalancerOf(c.variables["loadBalancer"]), c.distribution),
      action: "loadBalancer",
      name: "the load balancer",
    },
    { label: "CIS hardening", value: cisText(cis) },
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
  // A cluster that runs the newest release keeps the row after its administrator withdraws the
  // choice, so that it can be switched back.
  if (csi.offered || csiOn(c)) {
    rows.push({ label: "CSI plugin", value: csiText(csiOn(c)), action: "csi", name: "the CSI plugin" });
  }
  if (snapshots.offered || snapshotsOn(c)) {
    rows.push({
      label: "Volume snapshots",
      value: snapshotsText(snapshotsOn(c)),
      action: "snapshots",
      name: "volume snapshots",
    });
  }
  if (haGroups || haGroupsOn(c)) {
    rows.push({
      label: "HA group labels",
      value: haGroupsText(haGroupsOn(c)),
      action: "haGroups",
      name: "the HA group labels",
    });
  }
  rows.push(
    {
      label: "Headlamp",
      value: headlampText(headlamp.enabled === true, headlamp.hostname, headlamp.access === "view"),
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
    { label: "Logs", value: logsText(logsOf(c.variables["logs"])), action: "logs", name: "logs" },
    ...(more ?? []),
    {
      label: "Certificates",
      value: certificatesText(certificatesOf(c.variables["certificates"])),
      action: "certificates",
      name: "certificates",
    },
    {
      label: "Pod security",
      value: podSecurityText(podSecurity),
      action: "podSecurity",
      name: "pod security",
    },
    {
      label: "Istio",
      value: istioText(istioOn(c)),
      action: "istio",
      name: "Istio",
    },
  );
  if (runsCilium(c)) {
    rows.push({ label: "Cluster Mesh", value: meshText(c), action: "clusterMesh", name: "Cluster Mesh" });
  }
  rows.push(
    {
      label: "Rancher",
      value: rancherText(rancher),
      action: "rancher",
      button: rancher.install === true ? "Remove" : "Edit",
      name: "Rancher",
    },
    {
      label: "GitOps",
      value: gitOpsText(gitOpsOf(c.variables["gitops"])),
      action: "gitops",
      name: "GitOps",
    },
    {
      label: "Automatic upgrades",
      value: describe(upgrades, refreshesNodes(upgrades), c.distribution),
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

// A node OS the cluster can move to: released, and with a template of it built.
const movable = (o: NodeOSChoice): boolean => choosable(o) && !filled(o.noTemplate);

// nodeOSNote says why a node OS cannot be moved to, or else what it asks of the nodes' CPUs.
function nodeOSNote(o: NodeOSChoice, oses: NodeOSChoice[]): string | undefined {
  if (filled(o.unreleased)) {
    return sentence(`It cannot be chosen yet: ${o.unreleased}`);
  }
  return filled(o.noTemplate) ? sentence(o.noTemplate) : cpuNote(o, oses);
}

function NodeOSDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const running = c.nodeOS === undefined ? "" : nodeOSKey(c.nodeOS);
  const [oses, setOses] = useState<NodeOSChoice[]>();
  const [loadError, setLoadError] = useState<string>();
  const [chosen, setChosen] = useState(running);
  const { busy, error, send } = useChange(c, onClose);
  const group = useId();
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const classes = await api.classes();
        if (current) {
          setOses(classes.find((x) => x.name === c.class)?.nodeOSes ?? []);
        }
      } catch (err) {
        if (current) {
          setLoadError(errorMessage(err));
        }
      }
    }
    void load();
    return () => {
      current = false;
    };
  }, [c.class]);
  const os = findNodeOS(oses, chosen);
  return (
    <Dialog
      title="Node OS"
      onClose={onClose}
      onSubmit={() => {
        if (os !== undefined) {
          void send({ variables: { nodeOS: nodeOSRef(os) } });
        }
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className="primary"
            disabled={busy || chosen === running || os === undefined || !movable(os)}
          >
            Save
          </button>
        </>
      }
    >
      <p className="muted">
        What the cluster&apos;s nodes run. Saving another replaces every node with a clone of its VM template, one at a
        time as in an upgrade; that template has to be built first.
      </p>
      {oses === undefined && !filled(loadError) && <p className="muted">Loading…</p>}
      <ErrorBox message={loadError} />
      {oses !== undefined && (
        <div role="radiogroup" aria-label="Node OS">
          {oses.map((o, i) => {
            const key = nodeOSKey(o);
            const note = key === running ? undefined : nodeOSNote(o, oses);
            const noteId = `${group}-${i}`;
            return (
              <Fragment key={key}>
                <label className="check">
                  <input
                    type="radio"
                    name={group}
                    checked={chosen === key}
                    disabled={key !== running && !movable(o)}
                    aria-describedby={note === undefined ? undefined : noteId}
                    onChange={() => setChosen(key)}
                  />{" "}
                  {o.title}
                  {key === running && " (running)"}
                </label>
                {note !== undefined && (
                  <p className="hint" id={noteId}>
                    {note}
                  </p>
                )}
              </Fragment>
            );
          })}
        </div>
      )}
      <ErrorBox message={error} />
    </Dialog>
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
  const cis = c.variables["cis"] === true;
  const current = enforced(podSecurityOf(c.variables["podSecurity"]), cis);
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
      <PodSecurityFields value={value} onChange={setValue} cis={cis} />
      <ErrorBox message={error} />
    </Dialog>
  );
}

const istioOn = (c: Cluster): boolean => flag(record(c.variables["istio"])["enabled"]) === true;

function IstioDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [on, setOn] = useState(istioOn(c));
  const { busy, error, send } = useChange(c, onClose);
  const hint = useId();
  return (
    <Dialog
      title="Istio"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { istio: { enabled: on } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || on === istioOn(c)}>
            Save
          </button>
        </>
      }
    >
      <label className="check">
        <input type="checkbox" checked={on} aria-describedby={hint} onChange={(e) => setOn(e.target.checked)} /> Install
        Istio
      </label>
      <p className="hint" id={hint}>
        {istioHint}
      </p>
      <ErrorBox message={error} />
    </Dialog>
  );
}

/** The add-ons switched on and off by an enabled flag, and served at a host name when one is set. */
const hostedAddons = {
  headlamp: {
    title: "Headlamp",
    install: "Install Headlamp",
    hint: headlampHint,
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
  const [view, setView] = useState(current.access === "view");
  const { busy, error, send } = useChange(c, onClose);
  const value: HostedValue = { enabled };
  if (enabled && hostname) {
    value.hostname = hostname;
  }
  if (enabled && hostname && filled(current.tlsSecret)) {
    value.tlsSecret = current.tlsSecret;
  }
  if (enabled && view) {
    value.access = "view";
  }
  const changed =
    enabled !== (current.enabled === true) ||
    (enabled && (hostname !== (current.hostname ?? "") || view !== (current.access === "view")));
  const hint = useId();
  const viewHint = useId();
  // Logs are read in monitoring's Grafana, so they go with it.
  const heldLogs = record(c.variables["logs"]);
  const logsGo = addon === "monitoring" && !enabled && logsOf(heldLogs).enabled;
  return (
    <Dialog
      title={words.title}
      onClose={onClose}
      onSubmit={() => {
        void send({
          variables: logsGo ? { [addon]: value, logs: { ...heldLogs, enabled: false } } : { [addon]: value },
        });
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
      {enabled && addon === "headlamp" && (
        <>
          <label className="check">
            <input
              type="checkbox"
              checked={view}
              aria-describedby={viewHint}
              onChange={(e) => setView(e.target.checked)}
            />{" "}
            {headlampViewLabel}
          </label>
          <p className="hint" id={viewHint}>
            {headlampViewHint}
          </p>
        </>
      )}
      {logsGo && (
        <output className="warning notice">
          Logs are switched off with monitoring: they are read in its Grafana. Loki&apos;s volume stays.
        </output>
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
