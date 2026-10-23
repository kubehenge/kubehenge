import { useEffect, useState, type JSX } from "react";

import { api, bridgesOf } from "./api";
import { isVLAN, list } from "./network";
import { choosable, cpuNote, defaultNodeOS, findNodeOS, nodeOSKey, nodeOSLabel } from "./node-os";
import { offeredReleases, talosExample, talosRelease, useTalosReleases } from "./Talos";
import type {
  Bridge,
  Job,
  NodeOS,
  PveNode,
  Storage,
  TalosReleases,
  TemplateRefresh,
  TemplateRequest,
  VMTemplate,
} from "./types";
import {
  Choice,
  Dialog,
  ErrorBox,
  Field,
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
      <p className="muted">
        Every cluster's machines are clones of one of these. RKE2 and k3s need a template each for every node OS their
        clusters run; kubeadm needs one for each node OS and Kubernetes version its clusters run or upgrade to; Talos,
        which is its own OS, one for each Talos release its clusters start on. Each build of a node OS template is a
        template of its own; clusters move to a newer build inside their maintenance windows. A Talos template is made
        once: its nodes take updates by upgrading Talos in place.
      </p>
      <ErrorBox message={error} />
      {data && data.templates.length === 0 && <p>No templates yet: clusters cannot be created until there is one.</p>}
      {data && data.templates.length > 0 && (
        <table className="grid">
          <thead>
            <tr>
              <th>Template</th>
              <th>Node OS</th>
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
                <td>{t.nodeOS ?? "Talos Linux"}</td>
                <td>
                  {t.built === undefined ? (
                    <span className="muted">Not rebuilt</span>
                  ) : (
                    <>
                      {new Date(t.built).toLocaleString()}
                      {t.newest === true && <span className="muted"> (newest)</span>}
                    </>
                  )}
                </td>
                <td>{t.node}</td>
                <td>{t.inUse ?? "None"}</td>
                <td className="row-action">
                  <button
                    type="button"
                    disabled={filled(t.inUse) || building}
                    aria-label={`Delete ${t.name}`}
                    title={filled(t.inUse) ? `${t.inUse} clones its machines from it` : undefined}
                    onClick={() => setDeleting(t)}
                  >
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {job && <BuildJob job={job} />}
      <div className="actions">
        <button type="button" className="primary" disabled={!data || building} onClick={() => setCreating(true)}>
          Build a template
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
function BuildJob({ job }: { job: Job }): JSX.Element {
  const log = job.log ?? [];
  return (
    <div aria-live="polite">
      <p className={job.state === "failed" ? "error" : "muted"}>
        {job.state === "running" && `Building, started by ${job.user}…`}
        {job.state === "succeeded" && `The last build, by ${job.user}, succeeded.`}
        {job.state === "failed" && `The last build, by ${job.user}, failed: ${job.error ?? ""}`}
      </p>
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
  if (last.state === "succeeded") {
    return `The last run${by} succeeded${at}.`;
  }
  return `The last run${by} failed${at}: ${last.error ?? ""}`;
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
        <p className="muted">
          Off. With them on, Kubehenge builds the node OS templates again every week with each node OS's updates, and
          clusters move to them in their maintenance windows; switch them on in the management cluster's settings.
        </p>
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
      <p>
        Every {weekdayName(refresh.weekday)} at {refresh.time} ({refresh.timeZone}), Kubehenge builds the node OS
        templates clusters run again, with each node OS's updates. A cluster's nodes move to a new build in its
        maintenance window once the build is {refresh.soakHours} hours old, unless the cluster switches that off. The
        two newest builds of each template stay, and any older one a cluster still runs.
      </p>
      {refresh.next !== undefined && <p className="muted">Next build: {new Date(refresh.next).toLocaleString()}.</p>}
      <div aria-live="polite">
        {asked && !running && <p className="muted">Build now was asked for; it starts within a minute.</p>}
        {refresh.last && (
          <>
            <p className={refresh.last.state === "failed" ? "error" : "muted"}>{lastRun(refresh.last)}</p>
            {refresh.last.log.length > 0 && (
              <details open={running}>
                <summary>Log of the last run</summary>
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

function talosHint(releases: TalosReleases | undefined): string {
  const reason = releases?.error;
  return `A Talos cluster can start on the releases with a template, and is upgraded from there.${
    filled(reason) ? ` ${sentence(reason)} Type the release, such as ${talosExample}.` : ""
  }`;
}

interface BuildForm {
  distribution: (typeof distributions)[number];
  /** The node OS as one word, such as almalinux-10; empty, the default. */
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
}

export function buildRequest(f: BuildForm): TemplateRequest {
  const req: TemplateRequest = {
    distribution: f.distribution,
    node: f.node,
    storage: f.storage,
    importStorage: f.importStorage,
    bridge: f.bridge,
  };
  if (f.distribution === "kubeadm") {
    req.kubernetesVersion = f.kubernetesVersion.trim();
  }
  // A Talos image becomes a template as it is, without booting the VM that prepares the others, and
  // Talos is its own OS.
  if (f.distribution === "talos") {
    req.talosVersion = f.talosVersion.trim();
    return req;
  }
  if (f.nodeOS) {
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
    p.kubernetesVersion = "A version such as v1.36.5.";
  }
  if (f.distribution === "talos" && !talosRelease.test(f.talosVersion.trim())) {
    p.talosVersion = `A release such as ${talosExample}.`;
  }
  if (!f.node) {
    p.node = "Choose a node.";
  }
  if (!f.storage) {
    p.storage = "Choose a storage.";
  }
  if (!f.importStorage) {
    p.importStorage = "Choose a storage.";
  }
  if (!f.bridge) {
    p.bridge = "Choose a bridge.";
  }
  if (f.distribution !== "talos" && f.vlan && !isVLAN(f.vlan)) {
    p.vlan = "1 to 4094.";
  }
  if (
    f.distribution !== "talos" &&
    f.buildAddress.trim() &&
    (!f.buildAddress.includes("/") || !f.buildGateway.trim() || !f.buildDNS.trim())
  ) {
    p.buildAddress = "A static address needs its prefix length, a gateway and a DNS server.";
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
  const talos = entered.distribution === "talos";
  const releases = useTalosReleases(talos);
  // A lone choice is taken until the user picks; the cloud-init storage is optional, so stays the
  // node's first; and the Talos release is the one templates are made of by default.
  const form: BuildForm = {
    ...entered,
    talosVersion: filled(entered.talosVersion) ? entered.talosVersion : (releases?.default ?? ""),
    storage: filled(entered.storage) ? entered.storage : (sole(diskStorages) ?? ""),
    importStorage: filled(entered.importStorage) ? entered.importStorage : (sole(onNode("import")) ?? ""),
    bridge: filled(entered.bridge) ? entered.bridge : (sole(bridgeNames) ?? ""),
  };
  const problems = buildProblems(form);
  const shown = (k: keyof BuildForm): string | undefined => (touched.has(k) ? problems[k] : undefined);

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
      title="Build a template"
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
      <p className="muted">
        {talos
          ? "Makes the template with your Proxmox VE session, in a few minutes. The Talos Image Factory's image of the release, with the QEMU guest agent, is turned into a template as it is: Talos is its own OS, so no node OS is chosen. A release's template is made once; nodes cloned from it move on by upgrading Talos in place."
          : "Builds with your Proxmox VE session, in a few minutes. The node OS's image is checked against its publisher's signed checksums and booted once to compose the node from the node OS's own signed packages, only what a node needs and what the distribution needs, then turned into a template. A template that exists already gets a new build beside it, which clusters move to in their maintenance windows."}
      </p>
      <Field label="For">
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
      {!talos && <NodeOSField nodeOSes={nodeOSes} value={form.nodeOS} onChange={(v) => set("nodeOS", v)} />}
      {talos && (
        <Field label="Talos release" hint={talosHint(releases)} error={shown("talosVersion")}>
          {(id) =>
            releases && offeredReleases(releases).length > 0 ? (
              <select id={id} value={form.talosVersion} onChange={(e) => set("talosVersion", e.target.value)}>
                {offeredReleases(releases).map((v) => (
                  <option key={v} value={v}>
                    {v}
                    {v === releases.default ? " (default)" : ""}
                    {releases.templates.includes(v) ? " (has a template)" : ""}
                  </option>
                ))}
              </select>
            ) : (
              <input id={id} value={form.talosVersion} onChange={(e) => set("talosVersion", e.target.value.trim())} />
            )
          }
        </Field>
      )}
      {form.distribution === "kubeadm" && (
        <Field
          label="Kubernetes version"
          hint="Such as v1.36.5: a kubeadm cluster can be created at, or upgraded to, versions with a template on its node OS."
          error={shown("kubernetesVersion")}
        >
          {(id) => (
            <input id={id} value={form.kubernetesVersion} onChange={(e) => set("kubernetesVersion", e.target.value)} />
          )}
        </Field>
      )}
      <Field label="Node" hint="Where the image is prepared." error={shown("node")}>
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
      <Field label="Template disk" hint="Shared storage lets clones start on any node." error={shown("storage")}>
        {(id) => <Choice id={id} value={form.storage} options={diskStorages} onChoose={(v) => set("storage", v)} />}
      </Field>
      <Field label="Downloaded image" hint="A storage with the import content type." error={shown("importStorage")}>
        {(id) => (
          <Choice
            id={id}
            value={form.importStorage}
            options={onNode("import")}
            onChoose={(v) => set("importStorage", v)}
          />
        )}
      </Field>
      {!talos && (
        <Field label="Cloud-init image" hint="A storage with the ISO content type, while the image is prepared.">
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
      )}
      <Field
        label="Bridge"
        hint={
          talos
            ? "The template's network device; clones are attached to their cluster's own bridge."
            : "The VM that prepares the image needs to reach the node OS's and Kubernetes' package repositories."
        }
        error={shown("bridge")}
      >
        {(id) => <Choice id={id} value={form.bridge} options={bridgeNames} onChoose={(v) => set("bridge", v)} />}
      </Field>
      {!talos && (
        <Field label="VLAN tag" hint="Optional." error={shown("vlan")}>
          {(id) => (
            <input id={id} inputMode="numeric" value={form.vlan} onChange={(e) => set("vlan", e.target.value.trim())} />
          )}
        </Field>
      )}
      {!talos && (
        <details className="advanced">
          <summary>Static address</summary>
          <p className="muted">Without one, the VM that prepares the image asks DHCP.</p>
          <Field label="Address" hint="With prefix length, such as 192.0.2.50/24." error={shown("buildAddress")}>
            {(id) => <input id={id} value={form.buildAddress} onChange={(e) => set("buildAddress", e.target.value)} />}
          </Field>
          <Field label="Gateway">
            {(id) => <input id={id} value={form.buildGateway} onChange={(e) => set("buildGateway", e.target.value)} />}
          </Field>
          <Field label="DNS servers" hint="Separated by commas.">
            {(id) => <input id={id} value={form.buildDNS} onChange={(e) => set("buildDNS", e.target.value)} />}
          </Field>
        </details>
      )}
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
      title={`Delete ${t.name}`}
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
        Deletes template {t.name} (VM {t.vmid}) on node {t.node}. No cluster clones from it now; a cluster created later
        takes another build of {t.line}, or needs it built again when this is the last.
      </p>
      <ErrorBox message={error} />
    </Dialog>
  );
}
