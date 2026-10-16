import { useEffect, useState, type JSX } from "react";

import { api, bridgesOf } from "./api";
import { isVLAN, list } from "./network";
import type { Bridge, PveNode, Storage, TemplateRequest, VMTemplate } from "./types";
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
  sole,
  usePoll,
} from "./ui";
import { oneOf } from "./values";

const kubernetesVersion = /^v1\.\d+\.\d+$/u;

// Templates lists the VM templates clusters are cloned from and builds them with the signed-in
// administrator's Proxmox VE session.
export function Templates(): JSX.Element {
  const { data, error, reload } = usePoll(api.templates, (d) => (d?.job?.state === "running" ? pollRunning : pollIdle));
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<VMTemplate>();
  const job = data?.job;
  const building = job?.state === "running";

  return (
    <div className="box">
      <h3>VM templates</h3>
      <p className="muted">
        Every cluster's machines are clones of one of these. RKE2 and k3s share one template; kubeadm needs one for each
        Kubernetes version its clusters run or upgrade to.
      </p>
      <ErrorBox message={error} />
      {data && data.templates.length === 0 && <p>No templates yet: clusters cannot be created until there is one.</p>}
      {data && data.templates.length > 0 && (
        <table className="grid">
          <thead>
            <tr>
              <th>Template</th>
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
                  {t.name} <span className="muted">({t.vmid})</span>
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
      {job && (
        <div aria-live="polite">
          <p className={job.state === "failed" ? "error" : "muted"}>
            {job.state === "running" && `Building, started by ${job.user}…`}
            {job.state === "succeeded" && `The last build, by ${job.user}, succeeded.`}
            {job.state === "failed" && `The last build, by ${job.user}, failed: ${job.error ?? ""}`}
          </p>
          {(job.log ?? []).length > 0 && (
            <details open={building}>
              <summary>Log</summary>
              <pre className="log" aria-label="Template log">
                {(job.log ?? []).slice(-logLines).join("\n")}
              </pre>
            </details>
          )}
        </div>
      )}
      <div className="actions">
        <button type="button" className="primary" disabled={!data || building} onClick={() => setCreating(true)}>
          Build a template
        </button>
      </div>
      {creating && (
        <BuildDialog
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

const distributions = ["rke2", "kubeadm"] as const;

interface BuildForm {
  distribution: (typeof distributions)[number];
  kubernetesVersion: string;
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
  if (f.isoStorage) {
    req.isoStorage = f.isoStorage;
  }
  if (f.vlan) {
    req.vlan = Number(f.vlan);
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
  if (f.vlan && !isVLAN(f.vlan)) {
    p.vlan = "1 to 4094.";
  }
  if (f.buildAddress.trim() && (!f.buildAddress.includes("/") || !f.buildGateway.trim() || !f.buildDNS.trim())) {
    p.buildAddress = "A static address needs its prefix length, a gateway and a DNS server.";
  }
  return p;
}

function BuildDialog({ onClose, onStarted }: { onClose: () => void; onStarted: () => void }): JSX.Element {
  const [nodes, setNodes] = useState<PveNode[]>([]);
  const [storages, setStorages] = useState<Storage[]>([]);
  const [bridges, setBridges] = useState<Bridge[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [touched, setTouched] = useState<ReadonlySet<keyof BuildForm>>(new Set());
  const [entered, setEntered] = useState<BuildForm>({
    distribution: "rke2",
    kubernetesVersion: "",
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
  // A lone choice is taken until the user picks; the cloud-init storage is optional, so stays the node's first.
  const form: BuildForm = {
    ...entered,
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
        Builds with your Proxmox VE session, in a few minutes. The image is checked against its publisher's signed
        checksums, booted once to install updates and what the distribution needs, then turned into a template.
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
            <option value="rke2">RKE2 and k3s</option>
            <option value="kubeadm">kubeadm</option>
          </select>
        )}
      </Field>
      {form.distribution === "kubeadm" && (
        <Field
          label="Kubernetes version"
          hint="Such as v1.36.5: a kubeadm cluster can be created at, or upgraded to, versions with a template."
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
      <Field
        label="Bridge"
        hint="The VM that prepares the image needs to reach Ubuntu's and Kubernetes' package mirrors."
        error={shown("bridge")}
      >
        {(id) => <Choice id={id} value={form.bridge} options={bridgeNames} onChoose={(v) => set("bridge", v)} />}
      </Field>
      <Field label="VLAN tag" hint="Optional." error={shown("vlan")}>
        {(id) => (
          <input id={id} inputMode="numeric" value={form.vlan} onChange={(e) => set("vlan", e.target.value.trim())} />
        )}
      </Field>
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
        Deletes template {t.name} (VM {t.vmid}) on node {t.node}. No cluster clones from it now; one created later at a
        version it served needs it built again.
      </p>
      <ErrorBox message={error} />
    </Dialog>
  );
}
