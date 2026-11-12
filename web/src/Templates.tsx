import { useEffect, useState, type JSX } from "react";

import { api, bridgesOf } from "./api";
import { bridgeLabel, isVLAN, list } from "./network";
import { choosable, cpuNote, defaultNodeOS, findNodeOS, forDistribution, nodeOSKey, nodeOSLabel } from "./node-os";
import { ReleasePins } from "./ReleasePins";
import { loadingReleases, offeredReleases, releaseToMake, talosExample, talosRelease, useTalosReleases } from "./Talos";
import type {
  Bridge,
  Job,
  NodeOS,
  PveNode,
  Storage,
  TalosReleases,
  TemplateHardware,
  TemplateRefresh,
  TemplateRequest,
  VMTemplate,
} from "./types";
import {
  Choice,
  Detail,
  Dialog,
  ErrorBox,
  Field,
  Status,
  errorMessage,
  filled,
  logLines,
  pollIdle,
  pollRunning,
  sentence,
  sole,
  usePoll,
  weekdayName,
} from "./ui";
import { oneOf } from "./values";

const kubernetesVersion = /^v1\.\d+\.\d+$/u;

// Templates lists the VM templates clusters are cloned from and builds them with the signed-in
// administrator's Proxmox VE session.
export function Templates(): JSX.Element {
  return (
    <>
      <TemplatesBox />
      <ReleasePins />
    </>
  );
}

function TemplatesBox(): JSX.Element {
  const { data, error, reload } = usePoll(api.templates, (d) =>
    d?.job?.state === "running" || refreshBusy(d?.refresh) ? pollRunning : pollIdle,
  );
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<VMTemplate>();
  const job = data?.job;
  const building = job?.state === "running";

  return (
    <div className="box">
      <h3>VM templates</h3>
      <ErrorBox message={error} />
      {data?.down?.map((d) => (
        <p className="warning" key={d.vmid}>
          Template {d.name ?? `VM ${d.vmid}`} is on node {d.node}, which is down.
        </p>
      ))}
      {data && data.templates.length === 0 && <p>No templates yet</p>}
      {data && data.templates.length > 0 && (
        <div className="scroll">
          <table className="grid stack">
            <thead>
              <tr>
                <th>Template</th>
                <th>Node OS</th>
                <th>Hardware</th>
                <th>Build</th>
                <th>Node</th>
                <th>Used by</th>
                <th>
                  <span className="visually-hidden">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {data.templates.map((t) => (
                <tr key={t.vmid}>
                  <td>
                    {t.line} <span className="muted">({t.vmid})</span>
                  </td>
                  <td data-label="Node OS">{t.nodeOS ?? "Talos Linux"}</td>
                  <td data-label="Hardware">{hardwareText(t.hardware)}</td>
                  <td data-label="Build">
                    {t.built === undefined ? (
                      <span className="muted">{t.tags.includes("talos") ? "Made once" : "Not rebuilt"}</span>
                    ) : (
                      <>
                        {new Date(t.built).toLocaleString()}
                        {t.newest === true && <span className="muted"> (newest)</span>}
                        {t.older === true && (
                          <div>
                            <Status state="warning" text="Older than the previous build" />
                          </div>
                        )}
                      </>
                    )}
                  </td>
                  <td data-label="Node">{t.node}</td>
                  <td data-label="Used by">{t.inUse ?? "None"}</td>
                  <td className="row-action">
                    <button
                      type="button"
                      disabled={filled(t.inUse) || building}
                      aria-label={`Delete ${t.name}`}
                      title={filled(t.inUse) ? `In use by ${t.inUse}` : undefined}
                      onClick={() => setDeleting(t)}
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {job && <BuildJob job={job} />}
      <div className="actions">
        <button type="button" className="primary" disabled={!data || building} onClick={() => setCreating(true)}>
          Build template
        </button>
      </div>
      {data && (
        <AutomaticBuilds
          refresh={data.refresh}
          onAsked={() => {
            void reload();
          }}
        />
      )}
      {creating && (
        <BuildDialog
          nodeOSes={data?.nodeOSes ?? []}
          onClose={() => setCreating(false)}
          onStarted={() => {
            setCreating(false);
            void reload();
          }}
        />
      )}
      {deleting && (
        <DeleteDialog
          template={deleting}
          onClose={() => setDeleting(undefined)}
          onDeleted={() => {
            setDeleting(undefined);
            void reload();
          }}
        />
      )}
    </div>
  );
}

// BuildJob says how the last build an administrator started went, with its log.
/** hardwareText lists a template's CPU type and the options its clones carry over. */
export function hardwareText(h: TemplateHardware | undefined): JSX.Element | string {
  if (h === undefined) {
    return <span className="muted">Not recorded</span>;
  }
  const options = [h.ssd && "SSD", h.discard && "discard", h.ioThread && "IO thread", h.numa && "NUMA"].filter(
    (o): o is string => typeof o === "string",
  );
  return [h.cpuType, ...options].join(", ");
}

function BuildJob({ job }: { job: Job }): JSX.Element {
  const log = job.log ?? [];
  return (
    <div aria-live="polite">
      <div className={job.state === "failed" ? "error" : "muted"}>
        {job.state === "running" && `Building, started by ${job.user}…`}
        {job.state === "succeeded" && `Built by ${job.user}`}
        {job.state === "failed" && `Build by ${job.user} failed`}
        {job.state === "failed" && filled(job.error) && <Detail>{job.error}</Detail>}
      </div>
      {log.length > 0 && (
        <details open={job.state === "running"}>
          <summary>Log</summary>
          <pre className="log" aria-label="Template log">
            {log.slice(-logLines).join("\n")}
          </pre>
        </details>
      )}
    </div>
  );
}

/** Whether the weekly build runs, or was asked for and has not started. */
export function refreshBusy(r: TemplateRefresh | undefined): boolean {
  return r !== undefined && (r.last?.state === "running" || r.requested.length > 0);
}

function lastRun(last: NonNullable<TemplateRefresh["last"]>): string {
  const by = filled(last.trigger) && last.trigger !== "schedule" ? ` (${last.trigger})` : "";
  const when = last.finished ?? last.started;
  const at = when === undefined ? "" : `, ${new Date(when).toLocaleString()}`;
  if (last.state === "running") {
    return `Building ${last.lines.length > 0 ? last.lines.join(", ") : "the templates"}${by}…`;
  }
  return `Last run${by} ${last.state}${at}`;
}

// AutomaticBuilds is the weekly build of the node OS templates: when it runs, how its last run went,
// and Build now.
function AutomaticBuilds({
  refresh,
  onAsked,
}: {
  refresh?: TemplateRefresh | undefined;
  onAsked: () => void;
}): JSX.Element {
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  if (refresh === undefined) {
    return (
      <section aria-label="Automatic builds">
        <h4>Automatic builds</h4>
        <p className="muted">Off. Switch them on in the management cluster settings.</p>
      </section>
    );
  }
  const asked = refresh.requested.includes("refresh");
  const running = refresh.last?.state === "running";
  const buildNow = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.refreshTemplates();
      onAsked();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section aria-label="Automatic builds">
      <h4>Automatic builds</h4>
      <dl className="summary">
        <dt>Schedule</dt>
        <dd>
          Every {weekdayName(refresh.weekday)}, {refresh.time} {refresh.timeZone}
        </dd>
        {refresh.next !== undefined && (
          <>
            <dt>Next build</dt>
            <dd>{new Date(refresh.next).toLocaleString()}</dd>
          </>
        )}
        <dt>Rollout</dt>
        <dd>After {refresh.soakHours} h, in upgrade windows</dd>
        <dt>Kept</dt>
        <dd>2 newest builds, and any in use</dd>
      </dl>
      <div aria-live="polite">
        {asked && !running && <p className="muted">Starting within a minute</p>}
        {refresh.last && (
          <>
            <div className={refresh.last.state === "failed" ? "error" : "muted"}>
              {lastRun(refresh.last)}
              {refresh.last.state === "failed" && filled(refresh.last.error) && <Detail>{refresh.last.error}</Detail>}
            </div>
            {refresh.last.log.length > 0 && (
              <details open={running}>
                <summary>Log</summary>
                <pre className="log" aria-label="Automatic build log">
                  {refresh.last.log.slice(-logLines).join("\n")}
                </pre>
              </details>
            )}
          </>
        )}
      </div>
      <ErrorBox message={error} />
      <div className="actions">
        <button
          type="button"
          disabled={busy || asked || running}
          onClick={() => {
            void buildNow();
          }}
        >
          Build now
        </button>
      </div>
    </section>
  );
}

const distributions = ["rke2", "k3s", "kubeadm", "talos"] as const;

// talosHint says why the release is typed rather than chosen, when the releases could not be read.
function talosHint(releases: TalosReleases | undefined): string | undefined {
  const reason = releases?.error;
  return filled(reason) ? `${sentence(reason)} Such as ${talosExample}` : undefined;
}

// releaseLabel names a release as the build dialog offers it: v1.14.4 (default, has a template).
function releaseLabel(r: TalosReleases, v: string): string {
  const notes = [...(v === r.default ? ["default"] : []), ...(r.templates.includes(v) ? ["has a template"] : [])];
  return notes.length > 0 ? `${v} (${notes.join(", ")})` : v;
}

interface BuildForm {
  distribution: (typeof distributions)[number];
  /** The node OS as one word, such as almalinux-10. Empty, the default. */
  nodeOS: string;
  kubernetesVersion: string;
  talosVersion: string;
  node: string;
  storage: string;
  importStorage: string;
  isoStorage: string;
  bridge: string;
  vlan: string;
  buildAddress: string;
  buildGateway: string;
  buildDNS: string;
  /** The QEMU CPU type. Empty keeps the newest build's. */
  cpuType: string;
  ssd: HardwareChoice;
  discard: HardwareChoice;
  ioThread: HardwareChoice;
  numa: HardwareChoice;
}

/** HardwareChoice sets a template option: unset keeps the newest build's, else it is on or off. */
type HardwareChoice = "" | "on" | "off";

const hardwareOptions = [
  { key: "ssd", label: "SSD" },
  { key: "discard", label: "Discard" },
  { key: "ioThread", label: "IO thread" },
  { key: "numa", label: "NUMA" },
] as const;

const cpuTypePattern = /^[\w.-]+$/u;

export function buildRequest(f: BuildForm): TemplateRequest {
  const req: TemplateRequest = {
    distribution: f.distribution,
    node: f.node,
    storage: f.storage,
    importStorage: f.importStorage,
    bridge: f.bridge,
  };
  if (f.cpuType.trim()) {
    req.cpuType = f.cpuType.trim();
  }
  for (const { key } of hardwareOptions) {
    if (f[key]) {
      req[key] = f[key] === "on";
    }
  }
  if (f.distribution === "kubeadm") {
    req.kubernetesVersion = f.kubernetesVersion.trim();
  }
  // Talos is its own OS, and a build VM of the default one writes its image.
  if (f.distribution === "talos") {
    req.talosVersion = f.talosVersion.trim();
  } else if (f.nodeOS) {
    req.nodeOS = f.nodeOS;
  }
  if (f.vlan) {
    req.vlan = Number(f.vlan);
  }
  if (f.isoStorage) {
    req.isoStorage = f.isoStorage;
  }
  if (f.buildAddress.trim()) {
    req.buildAddress = f.buildAddress.trim();
    req.buildGateway = f.buildGateway.trim();
    req.buildDNS = list(f.buildDNS);
  }
  return req;
}

/** buildProblems finds, field by field, what the build would be refused for. */
export function buildProblems(f: BuildForm): Partial<Record<keyof BuildForm, string>> {
  const p: Partial<Record<keyof BuildForm, string>> = {};
  if (f.distribution === "kubeadm" && !kubernetesVersion.test(f.kubernetesVersion.trim())) {
    p.kubernetesVersion = "Enter a version such as v1.36.5";
  }
  if (f.distribution === "talos" && !talosRelease.test(f.talosVersion.trim())) {
    p.talosVersion = `Enter a release such as ${talosExample}`;
  }
  if (!f.node) {
    p.node = "Choose a Proxmox VE node";
  }
  if (!f.storage) {
    p.storage = "Choose a storage";
  }
  if (!f.importStorage) {
    p.importStorage = "Choose a storage";
  }
  if (!f.bridge) {
    p.bridge = "Choose a bridge";
  }
  if (f.cpuType.trim() && !cpuTypePattern.test(f.cpuType.trim())) {
    p.cpuType = "Enter a CPU type such as host or x86-64-v3";
  }
  if (f.vlan && !isVLAN(f.vlan)) {
    p.vlan = "A whole number from 1 to 4094";
  }
  if (f.buildAddress.trim() && (!f.buildAddress.includes("/") || !f.buildGateway.trim() || !f.buildDNS.trim())) {
    p.buildAddress = "Enter the prefix length, a gateway and a DNS server";
  }
  return p;
}

// NodeOSField chooses the node OS a template is built of, for every distribution but Talos, which
// is its own OS.
function NodeOSField({
  nodeOSes,
  value,
  onChange,
}: {
  nodeOSes: NodeOS[];
  value: string;
  onChange: (v: string) => void;
}): JSX.Element | null {
  if (nodeOSes.length === 0) {
    return null;
  }
  return (
    <Field label="Node OS" hint={cpuNote(findNodeOS(nodeOSes, value), nodeOSes)}>
      {(id) => (
        <select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
          {nodeOSes.map((o) => (
            <option key={nodeOSKey(o)} value={nodeOSKey(o)} disabled={!choosable(o)}>
              {nodeOSLabel(o)}
            </option>
          ))}
        </select>
      )}
    </Field>
  );
}

const typedOrSole = (typed: string, offered: string[]): string => (filled(typed) ? typed : (sole(offered) ?? ""));

/**
 * withChoices fills what the user has not picked. A lone choice is taken. The cloud-init storage is
 * optional, so stays the node's first. The Talos release is the one templates are made of by
 * default, unless it has one already.
 */
function withChoices(
  entered: BuildForm,
  o: {
    osOffered: NodeOS[];
    usual: NodeOS | undefined;
    release: string;
    diskStorages: string[];
    importStorages: string[];
    bridges: string[];
  },
): BuildForm {
  return {
    ...entered,
    nodeOS:
      findNodeOS(o.osOffered, entered.nodeOS) === undefined && o.usual !== undefined
        ? nodeOSKey(o.usual)
        : entered.nodeOS,
    talosVersion: filled(entered.talosVersion) ? entered.talosVersion : o.release,
    storage: typedOrSole(entered.storage, o.diskStorages),
    importStorage: typedOrSole(entered.importStorage, o.importStorages),
    bridge: typedOrSole(entered.bridge, o.bridges),
  };
}

function BuildDialog({
  nodeOSes,
  onClose,
  onStarted,
}: {
  nodeOSes: NodeOS[];
  onClose: () => void;
  onStarted: () => void;
}): JSX.Element {
  const [nodes, setNodes] = useState<PveNode[]>([]);
  const [storages, setStorages] = useState<Storage[]>([]);
  const [bridges, setBridges] = useState<Bridge[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [touched, setTouched] = useState<ReadonlySet<keyof BuildForm>>(new Set());
  const usual = defaultNodeOS(nodeOSes);
  const [entered, setEntered] = useState<BuildForm>({
    distribution: "rke2",
    nodeOS: usual === undefined ? "" : nodeOSKey(usual),
    kubernetesVersion: "",
    talosVersion: "",
    node: "",
    storage: "",
    importStorage: "",
    isoStorage: "",
    bridge: "",
    vlan: "",
    buildAddress: "",
    buildGateway: "",
    buildDNS: "",
    cpuType: "",
    ssd: "",
    discard: "",
    ioThread: "",
    numa: "",
  });
  const set = <K extends keyof BuildForm>(k: K, v: BuildForm[K]): void => {
    setEntered((f) => ({ ...f, [k]: v }));
    setTouched((t) => new Set([...t, k]));
  };

  useEffect(() => {
    async function loadNodes(): Promise<void> {
      try {
        const n = await api.nodes();
        setNodes(n);
        const first = n.find((x) => x.online)?.node;
        if (filled(first)) {
          setEntered((f) => (f.node ? f : { ...f, node: first }));
        }
      } catch (err) {
        setError(errorMessage(err));
      }
    }
    async function loadStorages(): Promise<void> {
      try {
        setStorages(await api.storages());
      } catch (err) {
        setError(errorMessage(err));
      }
    }
    void loadNodes();
    void loadStorages();
  }, []);

  useEffect(() => {
    if (!entered.node) {
      return;
    }
    async function loadBridges(node: string): Promise<void> {
      setBridges(await bridgesOf(node));
    }
    void loadBridges(entered.node);
  }, [entered.node]);

  const onNode = (content: string): string[] =>
    storages.filter((s) => s.node === entered.node && s.content.includes(content)).map((s) => s.storage);
  // Shared storage first: clones of a template on it start on any node.
  const diskStorages = storages
    .filter((s) => s.node === entered.node && s.content.includes("images"))
    .toSorted((a, b) => Number(b.shared) - Number(a.shared))
    .map((s) => s.storage);
  const bridgeNames = bridges.map((b) => b.bridge);
  const bridgeLabels = Object.fromEntries(bridges.map((b) => [b.bridge, bridgeLabel(b)]));
  const talos = entered.distribution === "talos";
  const releases = useTalosReleases(talos);
  const osOffered = forDistribution(nodeOSes, entered.distribution);
  const form = withChoices(entered, {
    osOffered,
    usual,
    release: releases ? releaseToMake(releases) : "",
    diskStorages,
    importStorages: onNode("import"),
    bridges: bridgeNames,
  });
  const unshared = storages.some((s) => s.storage === form.storage && s.node === form.node && !s.shared);
  const problems = buildProblems(form);
  const shown = (k: keyof BuildForm): string | undefined => (touched.has(k) ? problems[k] : undefined);
  const releaseField = (id: string): JSX.Element => {
    if (!releases) {
      return loadingReleases(id);
    }
    const offered = offeredReleases(releases);
    if (offered.length === 0) {
      return <input id={id} value={form.talosVersion} onChange={(e) => set("talosVersion", e.target.value.trim())} />;
    }
    return (
      <select id={id} value={form.talosVersion} onChange={(e) => set("talosVersion", e.target.value)}>
        {offered.map((v) => (
          <option key={v} value={v}>
            {releaseLabel(releases, v)}
          </option>
        ))}
      </select>
    );
  };

  const start = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.createTemplate(buildRequest(form));
      onStarted();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="Build template"
      onClose={onClose}
      onSubmit={() => {
        void start();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || Object.keys(problems).length > 0}>
            Build
          </button>
        </>
      }
    >
      <Field label="Distribution">
        {(id) => (
          <select
            id={id}
            value={form.distribution}
            onChange={(e) => {
              const distribution = e.target.value;
              if (oneOf(distributions, distribution)) {
                set("distribution", distribution);
              }
            }}
          >
            <option value="rke2">RKE2</option>
            <option value="k3s">k3s</option>
            <option value="kubeadm">kubeadm</option>
            <option value="talos">Talos</option>
          </select>
        )}
      </Field>
      {!talos && <NodeOSField nodeOSes={osOffered} value={form.nodeOS} onChange={(v) => set("nodeOS", v)} />}
      {talos && (
        <Field label="Talos release" hint={talosHint(releases)} error={shown("talosVersion")}>
          {releaseField}
        </Field>
      )}
      {form.distribution === "kubeadm" && (
        <Field label="Kubernetes version" hint="Such as v1.36.5" error={shown("kubernetesVersion")}>
          {(id) => (
            <input id={id} value={form.kubernetesVersion} onChange={(e) => set("kubernetesVersion", e.target.value)} />
          )}
        </Field>
      )}
      <Field label="Proxmox VE node" error={shown("node")}>
        {(id) => (
          <Choice
            id={id}
            value={form.node}
            options={nodes.filter((n) => n.online).map((n) => n.node)}
            onChoose={(v) => {
              setEntered((f) => ({ ...f, node: v, storage: "", importStorage: "", isoStorage: "", bridge: "" }));
              setTouched((t) => new Set([...t, "node"]));
            }}
          />
        )}
      </Field>
      <Field
        label="Template disk"
        hint={unshared ? `Clones start only on ${form.node}` : "Shared storage lets clones start on any node"}
        error={shown("storage")}
      >
        {(id) => <Choice id={id} value={form.storage} options={diskStorages} onChoose={(v) => set("storage", v)} />}
      </Field>
      <Field label="Downloaded image" error={shown("importStorage")}>
        {(id) => (
          <Choice
            id={id}
            value={form.importStorage}
            options={onNode("import")}
            onChoose={(v) => set("importStorage", v)}
          />
        )}
      </Field>
      <Field label="Cloud-init image">
        {(id) => (
          <Choice
            id={id}
            value={form.isoStorage}
            options={onNode("iso")}
            optional="The node's first"
            onChoose={(v) => set("isoStorage", v)}
          />
        )}
      </Field>
      <Field label="Bridge" hint="Needs to reach the package repositories" error={shown("bridge")}>
        {(id) => (
          <Choice
            id={id}
            value={form.bridge}
            options={bridgeNames}
            labels={bridgeLabels}
            onChoose={(v) => set("bridge", v)}
          />
        )}
      </Field>
      <Field label="VLAN tag" error={shown("vlan")}>
        {(id) => (
          <input id={id} inputMode="numeric" value={form.vlan} onChange={(e) => set("vlan", e.target.value.trim())} />
        )}
      </Field>
      <details className="advanced">
        <summary>Hardware</summary>
        <Field label="CPU type" hint="Empty keeps the newest build's" error={shown("cpuType")}>
          {(id) => <input id={id} value={form.cpuType} onChange={(e) => set("cpuType", e.target.value.trim())} />}
        </Field>
        {hardwareOptions.map(({ key, label }) => (
          <Field key={key} label={label}>
            {(id) => (
              <select
                id={id}
                value={form[key]}
                onChange={(e) => {
                  const choice = e.target.value;
                  if (oneOf(["", "on", "off"] as const, choice)) {
                    set(key, choice);
                  }
                }}
              >
                <option value="">Keep newest build&apos;s</option>
                <option value="on">On</option>
                <option value="off">Off</option>
              </select>
            )}
          </Field>
        ))}
      </details>
      <details className="advanced">
        <summary>Static address</summary>
        <Field label="Address" hint="Such as 192.0.2.50/24" error={shown("buildAddress")}>
          {(id) => <input id={id} value={form.buildAddress} onChange={(e) => set("buildAddress", e.target.value)} />}
        </Field>
        <Field label="Gateway">
          {(id) => <input id={id} value={form.buildGateway} onChange={(e) => set("buildGateway", e.target.value)} />}
        </Field>
        <Field label="DNS servers" hint="Separated by commas">
          {(id) => <input id={id} value={form.buildDNS} onChange={(e) => set("buildDNS", e.target.value)} />}
        </Field>
      </details>
      <ErrorBox message={error} />
    </Dialog>
  );
}

function DeleteDialog({
  template: t,
  onClose,
  onDeleted,
}: {
  template: VMTemplate;
  onClose: () => void;
  onDeleted: () => void;
}): JSX.Element {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const remove = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.deleteTemplate(t.vmid);
      onDeleted();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={`Delete template ${t.name}`}
      onClose={onClose}
      onSubmit={() => {
        void remove();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={busy}>
            Delete
          </button>
        </>
      }
    >
      <p>
        Deletes VM {t.vmid} on {t.node}. This can&apos;t be undone.
      </p>
      <ErrorBox message={error} />
    </Dialog>
  );
}
