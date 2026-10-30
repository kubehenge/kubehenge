import { Fragment, useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import { CertificatesDialog, certificatesOf, certificatesText } from "./Certificates";
import { ClusterMeshDialog, meshText, runsCilium } from "./ClusterMesh";
import { UpgradesDialog, describe, refreshesNodes, upgradePolicy } from "./ClusterUpgrades";
import { csiOn, csiWith, snapshotsOn, useCSIOffer, useSnapshotsOffer } from "./CsiPlugin";
import { GitOpsDialog, gitOpsOf, gitOpsText } from "./GitOps";
import { LoadBalancerDialog, loadBalancerOf, loadBalancerText } from "./LoadBalancer";
import { LogsDialog, logsOf, logsText } from "./Logs";
import { choosable, cpuNote, findNodeOS, nodeOSKey, nodeOSRef } from "./node-os";
import { OIDCProviders, cleanProviders, oidcProblems, oidcProviders } from "./OIDCProviders";
import {
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
import type { ChangeRequest, Cluster, NodeOSChoice, OIDCProvider } from "./types";
import {
  Dialog,
  ErrorBox,
  Field,
  Info,
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
  Skeleton,
} from "./ui";
import { flag, record, text, texts } from "./values";

/** What the Options view changes, each through a dialog of its own. */
export type OptionAction =
  | "nodeOS"
  | "placement"
  | "headlamp"
  | "monitoring"
  | "logs"
  | "podSecurity"
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
  /** The part of the Options view the setting is listed in. */
  group: OptionGroup;
  action?: A;
  button?: string;
  /** How the row's button names what it changes, for those who hear it out of the grid. */
  name?: string;
  /** A setting that is on or off, switched in its row instead of through a dialog. */
  toggle?: Toggle;
}

/** The parts of the Options view, in their order. */
export const optionGroups = ["Nodes", "Network", "Storage", "Add-ons", "Security", "Maintenance"] as const;
export type OptionGroup = (typeof optionGroups)[number];

/** Toggle is a setting switched in its row: it stages the change, as a dialog's Save does. */
export interface Toggle {
  on: (c: Cluster) => boolean;
  /** The change that switches it, from the cluster as staged so far. */
  request: (c: Cluster, on: boolean) => ChangeRequest;
  /** What keeps it from being switched on, when it is off. */
  problem?: string | undefined;
  /** What switching it on brings, shown while that is staged. */
  consequence?: string;
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
    return filled(r.hostname) ? `Installed at ${r.hostname}` : "Installed";
  }
  return r.register === false ? "Not registered" : registeredWithRancher;
}

// nodeOSRows is the node OS's row, for every cluster but a Talos one, whose nodes run Talos.
const nodeOSRows = (c: Cluster): OptionRow[] =>
  c.nodeOS ? [{ label: "Node OS", value: c.nodeOS.title, group: "Nodes", action: "nodeOS", name: "the node OS" }] : [];

/** Which of the settings that a class or the user's rights offer only at times a cluster offers. */
export interface Offered {
  ha: boolean;
  csi: boolean;
  snapshots: boolean;
  haGroups: boolean;
  /** What keeps the CSI plugin's newest release or volume snapshots from being switched on. */
  csiProblem?: string | undefined;
  snapshotsProblem?: string | undefined;
}

/** everything offers every setting, for a list that has to show whatever a change sets. */
export const everything: Offered = { ha: true, csi: true, snapshots: true, haGroups: true };

/** useOffered reads which of the settings offered only at times a cluster offers. */
export function useOffered(c: Cluster): Offered {
  const ha = useHAOffered(c.class, text(c.variables["storage"]));
  const csi = useCSIOffer(c);
  const snapshots = useSnapshotsOffer(c);
  const haGroups = useHAGroupsOffered(c);
  return {
    ha,
    csi: csi.offered,
    snapshots: snapshots.offered,
    haGroups,
    csiProblem: csi.problem,
    snapshotsProblem: snapshots.problem,
  };
}

const istioOn = (c: Cluster): boolean => flag(record(c.variables["istio"])["enabled"]) === true;

const haToggle: Toggle = {
  on: haOn,
  request: (_, on) => ({ variables: { proxmoxHA: on } }),
  consequence: "Nodes running HA VMs reboot if they lose quorum",
};

const haGroupsToggle: Toggle = {
  on: haGroupsOn,
  request: (_, on) => ({ variables: { ccm: { haGroups: on } } }),
  consequence: "The token gets Sys.Audit on /",
};

const istioToggle: Toggle = {
  on: istioOn,
  request: (_, on) => ({ variables: { istio: { enabled: on } } }),
  consequence: istioHint,
};

const csiToggle = (problem: string | undefined): Toggle => ({
  on: csiOn,
  request: (c, on) => ({ variables: { csi: csiWith(c, "datastoreAllocate", on) } }),
  problem,
  consequence: "The token gets Datastore.Allocate on the storage",
});

const snapshotsToggle = (problem: string | undefined): Toggle => ({
  on: snapshotsOn,
  request: (c, on) => ({ variables: { csi: csiWith(c, "volumeSnapshots", on) } }),
  problem,
  consequence: "Copies stay on their volume's node",
});

/**
 * optionRows are what can be set on a cluster after it was created. more are the rows of settings
 * whose dialogs live elsewhere, with views of their own or the Scale dialog's, after logs.
 */
export function optionRows<A extends string = never>(
  c: Cluster,
  offered: Offered,
  more: OptionRow<A>[] = [],
): OptionRow<OptionAction | A>[] {
  const headlamp = hostedValue(c.variables["headlamp"]);
  const monitoring = hostedValue(c.variables["monitoring"]);
  const rancher = rancherValue(c.variables["rancher"]);
  const upgrades = upgradePolicy(c.variables["upgrades"]);
  const cis = c.variables["cis"] === true;
  const podSecurity = enforced(podSecurityOf(c.variables["podSecurity"]), cis);
  const talos: OptionRow[] =
    c.distribution === "talos"
      ? [
          {
            label: "Talos release",
            value: talosOf(c),
            group: "Nodes",
            action: "talos",
            button: "Upgrade",
            name: "Talos",
          },
        ]
      : [];
  const rows: OptionRow<OptionAction | A>[] = [
    ...nodeOSRows(c),
    ...talos,
    { label: "Pod network", value: cniName(text(c.variables["cni"]) ?? "") || "Default", group: "Network" },
    {
      label: "Load balancer",
      value: loadBalancerText(loadBalancerOf(c.variables["loadBalancer"]), c.distribution),
      group: "Network",
      action: "loadBalancer",
      name: "the load balancer",
    },
    { label: "CIS hardening", value: cisText(cis), group: "Security" },
    {
      label: "Placement",
      value: placeText(placeOf(texts(c.variables["allowedNodes"]))),
      group: "Nodes",
      action: "placement",
      name: "placement",
    },
  ];
  if (offered.ha) {
    rows.push({ label: "Proxmox VE HA", value: haText(haOn(c)), group: "Nodes", toggle: haToggle });
  }
  // A cluster that runs the newest release keeps the row after its administrator withdraws the
  // choice, so that it can be switched back.
  if (offered.csi || csiOn(c)) {
    rows.push({
      label: "CSI plugin",
      value: csiText(csiOn(c)),
      group: "Storage",
      toggle: csiToggle(offered.csiProblem),
    });
  }
  if (offered.snapshots || snapshotsOn(c)) {
    rows.push({
      label: "Volume snapshots",
      value: snapshotsText(snapshotsOn(c)),
      group: "Storage",
      toggle: snapshotsToggle(offered.snapshotsProblem),
    });
  }
  if (offered.haGroups || haGroupsOn(c)) {
    rows.push({ label: "HA group labels", value: haGroupsText(haGroupsOn(c)), group: "Nodes", toggle: haGroupsToggle });
  }
  rows.push(
    {
      label: "Headlamp",
      value: headlampText(headlamp.enabled === true, headlamp.hostname, headlamp.access === "view"),
      group: "Add-ons",
      action: "headlamp",
      name: "Headlamp",
    },
    {
      label: "Monitoring",
      value:
        monitoring.enabled === true
          ? `On${filled(monitoring.hostname) ? `, Grafana at ${monitoring.hostname}` : ""}`
          : "Off",
      group: "Add-ons",
      action: "monitoring",
      name: "monitoring",
    },
    { label: "Logs", value: logsText(logsOf(c.variables["logs"])), group: "Add-ons", action: "logs", name: "logs" },
    ...more,
    {
      label: "Certificates",
      value: certificatesText(certificatesOf(c.variables["certificates"])),
      group: "Add-ons",
      action: "certificates",
      name: "certificates",
    },
    {
      label: "Pod security",
      value: podSecurityText(podSecurity),
      group: "Security",
      action: "podSecurity",
      name: "pod security",
    },
    { label: "Istio", value: istioText(istioOn(c)), group: "Add-ons", toggle: istioToggle },
  );
  if (runsCilium(c)) {
    rows.push({
      label: "Cluster Mesh",
      value: meshText(c),
      group: "Network",
      action: "clusterMesh",
      name: "Cluster Mesh",
    });
  }
  rows.push(
    {
      label: "Rancher",
      value: rancherText(rancher),
      group: "Add-ons",
      action: "rancher",
      button: rancher.install === true ? "Uninstall" : "Edit",
      name: "Rancher",
    },
    {
      label: "GitOps",
      value: gitOpsText(gitOpsOf(c.variables["gitops"])),
      group: "Add-ons",
      action: "gitops",
      name: "GitOps",
    },
    {
      label: "Automatic upgrades",
      value: describe(upgrades, refreshesNodes(upgrades), c.distribution),
      group: "Maintenance",
      action: "upgrades",
      name: "automatic upgrades",
    },
  );
  if ("oidc" in c.variables) {
    const hosts = providers(c).map((p) => (URL.canParse(p.issuerURL) ? new URL(p.issuerURL).host : p.issuerURL));
    rows.push({
      label: "Identity providers",
      value: hosts.length > 0 ? hosts.join(", ") : "None",
      group: "Security",
      action: "oidc",
      name: "identity providers",
    });
  }
  return rows;
}

/**
 * Options lists what can be set on a cluster after it was created, each with its Edit button, and
 * what a staged change sets each to. more makes the rows of the settings whose dialogs live
 * elsewhere, for the cluster as it is and as staged.
 */
export function Options<A extends string = never>({
  cluster: c,
  staged,
  can,
  onEdit,
  more,
}: {
  cluster: Cluster;
  staged?: Cluster | undefined;
  can: boolean;
  onEdit: (a: OptionAction | A) => void;
  more?: (c: Cluster) => OptionRow<A>[];
}): JSX.Element {
  const offered = useOffered(c);
  const rows = optionRows(c, offered, more?.(c));
  const after = staged === undefined ? [] : optionRows(staged, offered, more?.(staged));
  return (
    <div className="option-groups">
      {optionGroups.map((group) => {
        const listed = rows.filter((r) => r.group === group);
        return (
          listed.length > 0 && (
            <div key={group} className="box scroll">
              <table className="grid options">
                <caption>{group}</caption>
                <tbody>
                  {listed.map((row) => (
                    <OptionLine
                      key={row.label}
                      row={row}
                      after={after.find((r) => r.label === row.label)?.value}
                      at={{ cluster: c, staged, can, onEdit }}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          )
        );
      })}
    </div>
  );
}

// Where an option's row is shown: the cluster as it is and as staged, and whether the user may change it.
interface RowAt<A extends string> {
  cluster: Cluster;
  staged: Cluster | undefined;
  can: boolean;
  onEdit: (a: A) => void;
}

function OptionLine<A extends string>({
  row: { label, value, action, button = "Edit", name = label, toggle },
  after,
  at,
}: {
  row: OptionRow<A>;
  after: string | undefined;
  at: RowAt<A>;
}): JSX.Element {
  return (
    <tr>
      <th scope="row">{label}</th>
      <td>
        {toggle === undefined ? (
          <>
            {value}
            <StagedValue now={value} after={after} />
          </>
        ) : (
          <Switch toggle={toggle} label={label} text={after ?? value} at={at} />
        )}
      </td>
      <td className="row-action">
        {action !== undefined && (
          <button type="button" disabled={!at.can} aria-label={`${button} ${name}`} onClick={() => at.onEdit(action)}>
            {button}
          </button>
        )}
      </td>
    </tr>
  );
}

const stay = (): void => {
  // A switch has no dialog to close: its row shows what it staged.
};

// switchNote is what keeps a switch from being turned on, or what turning it on brings.
function switchNote(toggle: Toggle, on: boolean, was: boolean): string | undefined {
  if (toggle.problem !== undefined && !on) {
    return toggle.problem;
  }
  return on && !was ? toggle.consequence : undefined;
}

// Switch turns a setting on or off in its row, staged with the page's other changes.
function Switch<A extends string>({
  toggle,
  label,
  text: shown,
  at: { cluster: c, staged, can },
}: {
  toggle: Toggle;
  label: string;
  text: string;
  at: RowAt<A>;
}): JSX.Element {
  const { busy, error, send } = useChange(c, stay);
  const hint = useId();
  const from = staged ?? c;
  const on = toggle.on(from);
  const refused = toggle.problem !== undefined && !on;
  const note = switchNote(toggle, on, toggle.on(c));
  return (
    <>
      <label className={on === toggle.on(c) ? "switch" : "switch switch-staged"}>
        <input
          type="checkbox"
          role="switch"
          aria-checked={on}
          aria-label={label}
          aria-describedby={note === undefined ? undefined : hint}
          checked={on}
          disabled={!can || busy || refused}
          onChange={(e) => {
            void send(toggle.request(from, e.target.checked));
          }}
        />
        <span>{shown}</span>
      </label>
      {note !== undefined && (
        <span className="hint" id={hint}>
          {note}
        </span>
      )}
      {error !== undefined && (
        <span className="field-error" role="alert">
          {error}
        </span>
      )}
    </>
  );
}

// StagedValue is what a staged change sets a setting to, where it sets it anew.
function StagedValue({ now, after }: { now: string; after: string | undefined }): JSX.Element | null {
  if (after === undefined || after === now) {
    return null;
  }
  return <span className="staged-value">Staged: {after}</span>;
}

// A node OS the cluster can move to: released, and with a template of it built.
const movable = (o: NodeOSChoice): boolean => choosable(o) && !filled(o.noTemplate);

// nodeOSNote says why a node OS cannot be moved to, or else what it asks of the nodes' CPUs.
function nodeOSNote(o: NodeOSChoice, oses: NodeOSChoice[]): string | undefined {
  if (filled(o.unreleased)) {
    return sentence(o.unreleased);
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
      title="Edit node OS"
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
      <p className="muted">Saving replaces every node, one at a time.</p>
      {oses === undefined && !filled(loadError) && <Skeleton rows={2} />}
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
      title="Edit identity providers"
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
      <p className="muted">Saving replaces the control-plane nodes, one at a time.</p>
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
      title="Edit pod security"
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
      {!cis && <p className="muted">Changing admission replaces the control-plane nodes, one at a time.</p>}
      <PodSecurityFields value={value} onChange={setValue} cis={cis} />
      <ErrorBox message={error} />
    </Dialog>
  );
}

/** The add-ons switched on and off by an enabled flag, and served at a host name when one is set. */
const hostedAddons = {
  headlamp: {
    title: "Edit Headlamp",
    name: "Headlamp",
    install: "Install Headlamp",
    info: headlampHint,
    hostLabel: "Host name",
  },
  monitoring: {
    title: "Edit monitoring",
    name: "Monitoring",
    install: "Install monitoring",
    info: "Grafana's admin password: Secret kube-prometheus-stack-grafana in monitoring.",
    hostLabel: "Grafana host name",
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
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} /> {words.install}
      </label>
      <Info about={words.name}>{words.info}</Info>
      {enabled && (
        <Field label={words.hostLabel} hint="Optional, through the cluster's ingress">
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
      {logsGo && <output className="warning notice">Logs switch off too. Loki&apos;s volume stays.</output>}
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
        title="Uninstall Rancher"
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
              Uninstall Rancher
            </button>
          </>
        }
      >
        <p>
          Deletes Rancher&apos;s settings, users and projects, and unregisters every cluster. The clusters keep running.
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
      title="Edit Rancher"
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
        {registerHint}
      </div>
      <ErrorBox message={error} />
    </Dialog>
  );
}
