import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { WorkerFields } from "./Autoscaling";
import {
  chosenNodeOS,
  controlPlaneSizes,
  machineLeast,
  memoryStepMiB,
  templateMissing,
  variable,
  versionExamples,
  versionLabel,
  type FieldKey,
  type Form,
  type Machine,
} from "./create-form";
import { CSIField, SnapshotsField } from "./CsiPlugin";
import { LoadBalancerFields } from "./LoadBalancer";
import { addressBits, addressIn } from "./network";
import { choosable, cpuNote, findNodeOS, macName, nodeOSKey, nodeOSLabel } from "./node-os";
import { OIDCProviders } from "./OIDCProviders";
import { HAField, HAGroupsField, PlacementFields, type HARulesRead } from "./Placement";
import {
  TalosReleaseSelect,
  forRelease,
  missingReleases,
  talosExample,
  talosRelease,
  talosReleaseHint,
  useTalosReleases,
} from "./Talos";
import type {
  Bridge,
  ClusterClass,
  NodeOSChoice,
  PveNode,
  Storage,
  TalosReleases,
  TemplateBuildRequest,
  TemplateBuilds,
  Versions,
} from "./types";
import {
  ErrorBox,
  Field,
  bytes,
  cniName,
  distributionName,
  errorMessage,
  filled,
  sentence,
  shownNumber,
  typedNumber,
} from "./ui";

const kubeadmVersion = /^v1\.\d+\.\d+$/u;

// The template a distribution asks for: one per Kubernetes version and node OS for kubeadm, one per
// Talos release for Talos, which is its own OS, and RKE2's or k3s's own on each node OS.
type TemplateKind = "kubeadm" | "talos" | "distribution";

const templateKind = (distribution: string): TemplateKind =>
  distribution === "kubeadm" || distribution === "talos" ? distribution : "distribution";

// The version a missing template is asked for by: chosen from the releases missing one, or typed
// in when none could be read.
interface VersionToBuild {
  label: string;
  pattern: RegExp;
  example: string;
  missing: string[];
  /** The release templates are made of by default, which the list marks. */
  preferred?: string;
}

// What a class's missing template is asked for with.
interface Asking {
  kind: TemplateKind;
  distribution: string;
  field?: VersionToBuild;
}

// The node OS chosen, but for Talos, and why it has no template, if it has none.
interface ChosenNodeOS {
  os: NodeOSChoice | undefined;
  missing: string | undefined;
}

// templateBuildRequest is the request for a missing template: the distribution's on the node OS,
// kubeadm's of a version on it, or Talos's of a release.
function templateBuildRequest(
  { kind, distribution }: Asking,
  version: string,
  os: NodeOSChoice | undefined,
): TemplateBuildRequest {
  if (kind === "talos") {
    return { distribution, talosVersion: version };
  }
  return {
    distribution,
    ...(kind === "kubeadm" && { kubernetesVersion: version }),
    ...(os !== undefined && { nodeOS: nodeOSKey(os) }),
  };
}

// requestKey is how the server lists a request: the template line's name without its kubehenge- prefix.
const requestKey = (req: TemplateBuildRequest): string =>
  [req.distribution, req.kubernetesVersion ?? req.talosVersion ?? "", req.nodeOS ?? ""].filter(Boolean).join("-");

// askFor is what a class's missing template is asked for with, or undefined when there is nothing
// to ask for: on a node OS that can be chosen, the distribution's template while it is missing or
// any kubeadm version; or a Talos release without a template.
function askFor(
  cls: ClusterClass | undefined,
  talos: TalosReleases | undefined,
  { os, missing: missingTemplate }: ChosenNodeOS,
): Asking | undefined {
  if (cls === undefined) {
    return undefined;
  }
  const { distribution } = cls;
  const kind = templateKind(distribution);
  if (kind !== "talos" && os !== undefined && !choosable(os)) {
    return undefined;
  }
  if (kind === "distribution") {
    return filled(missingTemplate) ? { kind, distribution } : undefined;
  }
  if (kind === "kubeadm") {
    const field = { label: "Kubernetes version to build", pattern: kubeadmVersion, example: "v1.36.5", missing: [] };
    return { kind, distribution, field };
  }
  const missing = talos ? missingReleases(talos) : [];
  if (!talos || (missing.length === 0 && !filled(talos.error))) {
    return undefined;
  }
  const field = {
    label: "Talos release to build",
    pattern: talosRelease,
    example: talosExample,
    missing,
    preferred: talos.default,
  };
  return { kind, distribution, field };
}

function VersionField({
  field,
  version,
  chosen,
  onChange,
}: {
  field: VersionToBuild;
  version: string;
  chosen: string;
  onChange: (v: string) => void;
}): JSX.Element {
  if (field.missing.length > 0) {
    return (
      <Field label={field.label}>
        {(id) => (
          <select id={id} value={chosen} onChange={(e) => onChange(e.target.value)}>
            {field.missing.map((v) => (
              <option key={v} value={v}>
                {v === field.preferred ? `${v} (default)` : v}
              </option>
            ))}
          </select>
        )}
      </Field>
    );
  }
  return (
    <Field
      label={field.label}
      hint={`Like ${field.example}`}
      error={version !== "" && !field.pattern.test(version) ? `Enter a version like ${field.example}` : undefined}
    >
      {(id) => <input id={id} value={version} onChange={(e) => onChange(e.target.value.trim())} />}
    </Field>
  );
}

function askText(kind: TemplateKind, os: NodeOSChoice | undefined): string {
  const on = os === undefined ? "" : ` on ${os.title}`;
  const words: Record<TemplateKind, string> = {
    kubeadm: `A version not listed${on} takes about 20 minutes`,
    talos: "A release not listed takes a few minutes",
    distribution: `Building the VM template${on} takes about 20 minutes`,
  };
  return words[kind];
}

/**
 * requestName is how a template build request's key reads: talos-v1.14.4 as Talos v1.14.4,
 * kubeadm-v1.37.1-almalinux-10 as kubeadm v1.37.1 on AlmaLinux 10, rke2-rocky-10 as RKE2 on Rocky
 * Linux 10, with the node OS's title where oses has it.
 */
export function requestName(key: string, oses?: NodeOSChoice[]): string {
  if (key === "refresh") {
    return "the weekly build";
  }
  const { dist, rest } = /^(?<dist>rke2|k3s|kubeadm|talos)-(?<rest>.+)$/u.exec(key)?.groups ?? {};
  if (dist === undefined || rest === undefined) {
    return key;
  }
  const name = distributionName(dist);
  if (dist === "talos") {
    return `${name} ${rest}`;
  }
  // A Kubernetes version has no dash, so the first one ends it.
  const parts: Partial<Record<"version" | "os", string>> =
    (dist === "kubeadm" ? /^(?<version>[^-]+)-(?<os>.+)$/u.exec(rest)?.groups : { os: rest }) ?? {};
  const { version, os } = parts;
  if (os === undefined) {
    return key;
  }
  const title = findNodeOS(oses, os)?.title ?? os;
  return version === undefined ? `${name} on ${title}` : `${name} ${version} on ${title}`;
}

// TemplateAsk lets one who may create clusters have a missing template built, when the
// administrators allow it: the distribution's template on the node OS chosen, a kubeadm version's on
// it, or a Talos release's.
function TemplateAsk({
  cls,
  talos,
  os,
  missing: missingTemplate,
  oses,
}: {
  cls: ClusterClass | undefined;
  talos: TalosReleases | undefined;
  os: NodeOSChoice | undefined;
  missing: string | undefined;
  /** Every class's node OSes, to name those of the requests others made. */
  oses: NodeOSChoice[];
}): JSX.Element | null {
  const [builds, setBuilds] = useState<TemplateBuilds>();
  const [version, setVersion] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const b = await api.templateBuilds();
        if (current) {
          setBuilds(b);
        }
      } catch {
        // Without the answer there is nothing to offer; the class's own message stays.
      }
    }
    void load();
    return () => {
      current = false;
    };
  }, []);
  const asking = askFor(cls, talos, { os, missing: missingTemplate });
  if (builds?.allowed !== true || asking === undefined) {
    return null;
  }
  const { kind, field } = asking;
  const missing = field?.missing ?? [];
  const chosen = missing.length > 0 && !missing.includes(version) ? (missing[0] ?? "") : version;
  const valid = field === undefined || field.pattern.test(chosen);
  const req = templateBuildRequest(asking, chosen, os);
  const key = requestKey(req);
  // The run under way lists the templates it builds by their line's name, the key with kubehenge-
  // before it, not by the keys they were asked for by.
  const asked = builds.requested.includes(key) || builds.building.includes(`kubehenge-${key}`);
  const ask = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.requestTemplateBuild(req);
      setBuilds(await api.templateBuilds());
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <fieldset className="group">
      <legend>Build a VM template</legend>
      <p className="muted">{askText(kind, os)}</p>
      {builds.building.length > 0 && <p className="muted">{`Building: ${builds.building.join(", ")}`}</p>}
      {builds.requested.length > 0 && (
        <p className="muted">{`Requested: ${builds.requested.map((k) => requestName(k, oses)).join(", ")}`}</p>
      )}
      {field && <VersionField field={field} version={version} chosen={chosen} onChange={setVersion} />}
      <ErrorBox message={error} />
      <div className="actions">
        <button
          type="button"
          disabled={busy || asked || !valid}
          onClick={() => {
            void ask();
          }}
        >
          Build template
        </button>
      </div>
    </fieldset>
  );
}

// NodeOSField chooses what the nodes run, for every distribution but Talos, whose nodes run Talos.
function NodeOSField({ form, set, shown, cls }: StepProps & { cls: ClusterClass | undefined }): JSX.Element | null {
  const oses = cls?.nodeOSes ?? [];
  const os = chosenNodeOS(form, cls);
  if (os === undefined) {
    return null;
  }
  return (
    <Field
      label="Node OS"
      error={shown("nodeOS")}
      hint={cpuNote(os, oses)}
      info={`Minimal, from ${os.title}'s signed packages, with ${macName(os.mac)} enforcing.`}
    >
      {(id) => (
        <select id={id} value={nodeOSKey(os)} onChange={(e) => set("nodeOS", e.target.value)}>
          {oses.map((o) => (
            <option key={nodeOSKey(o)} value={nodeOSKey(o)} disabled={!choosable(o)}>
              {nodeOSLabel(o)}
            </option>
          ))}
        </select>
      )}
    </Field>
  );
}

/** useClassTalos reads the Talos releases for a class whose clusters name one. */
export const useClassTalos = (cls: ClusterClass | undefined): TalosReleases | undefined =>
  useTalosReleases(variable(cls, "talosVersion") !== undefined);

// A version is typed in only when none could be listed: the hint gives its format, and why the list
// is missing sits behind the field's info.
const typedVersion = (cls: ClusterClass | undefined, offered: Versions | undefined): boolean =>
  cls !== undefined && (offered === undefined || offered.versions.length === 0);

// The version is chosen from those offered, typed in when none can be, and waits for the distribution.
function VersionInput({
  id,
  value,
  onChange,
  cls,
  offered,
}: {
  id: string;
  value: string;
  onChange: (v: string) => void;
  cls: ClusterClass | undefined;
  offered: Versions | undefined;
}): JSX.Element {
  if (!cls) {
    return (
      <select id={id} disabled value="">
        <option value="">Choose the distribution first</option>
      </select>
    );
  }
  if (!offered || offered.versions.length === 0) {
    return <input id={id} value={value} onChange={(e) => onChange(e.target.value.trim())} />;
  }
  return (
    <select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="" disabled>
        Choose…
      </option>
      {offered.versions.map((v) => (
        <option key={v} value={v}>
          {versionLabel(v, offered.stable)}
        </option>
      ))}
    </select>
  );
}

// The props every step of the wizard takes: the form, how to change one field, and the problem a
// field shows, if any.
export interface StepProps {
  form: Form;
  set: <K extends keyof Form>(k: K, v: Form[K]) => void;
  shown: (k: FieldKey) => string | undefined;
}

export function GeneralStep({
  form,
  set,
  shown,
  classes,
  cls,
  offered,
  talos,
  onChooseClass,
  namespaces,
  onChooseNamespace,
}: StepProps & {
  classes: ClusterClass[];
  cls: ClusterClass | undefined;
  offered: Versions | undefined;
  /** The Talos releases, for a Talos class; undefined while they load. */
  talos?: TalosReleases | undefined;
  onChooseClass: (name: string) => void;
  namespaces: string[];
  onChooseNamespace: (namespace: string) => void;
}): JSX.Element {
  const runs = forRelease(offered, talos, form.talosVersion);
  const typed = typedVersion(cls, runs);
  const unlisted = runs?.error;
  return (
    <>
      {namespaces.length > 1 && (
        <Field label="Namespace">
          {(id) => (
            <select id={id} value={form.namespace} onChange={(e) => onChooseNamespace(e.target.value)}>
              {namespaces.map((ns) => (
                <option key={ns} value={ns}>
                  {ns}
                </option>
              ))}
            </select>
          )}
        </Field>
      )}
      <Field label="Name" error={shown("name")}>
        {(id) => <input id={id} value={form.name} onChange={(e) => set("name", e.target.value.trim())} />}
      </Field>
      <Field label="Distribution" error={shown("class")}>
        {(id) => (
          <select id={id} value={form.className} onChange={(e) => onChooseClass(e.target.value)}>
            <option value="" disabled>
              Choose…
            </option>
            {classes.map((c) => (
              <option key={c.name} value={c.name}>
                {distributionName(c.distribution)}
              </option>
            ))}
          </select>
        )}
      </Field>
      <NodeOSField form={form} set={set} shown={shown} cls={cls} />
      {variable(cls, "talosVersion") !== undefined && (
        <Field label="Talos release" error={shown("talosVersion")} hint={talosReleaseHint(talos)}>
          {(id) => (
            <TalosReleaseSelect
              id={id}
              value={form.talosVersion}
              releases={talos}
              onChange={(v) => set("talosVersion", v)}
            />
          )}
        </Field>
      )}
      <Field
        label="Kubernetes version"
        error={shown("version")}
        hint={typed ? `Like ${versionExamples[cls?.distribution ?? ""] ?? "v1.36.5"}` : undefined}
        info={typed && filled(unlisted) ? sentence(unlisted) : undefined}
      >
        {(id) => (
          <VersionInput id={id} value={form.version} onChange={(v) => set("version", v)} cls={cls} offered={runs} />
        )}
      </Field>
      <TemplateAsk
        cls={cls}
        talos={talos}
        os={chosenNodeOS(form, cls)}
        missing={templateMissing(form, cls)}
        oses={classes.flatMap((c) => c.nodeOSes ?? [])}
      />
      <Field label="Control-plane nodes">
        {(id) => (
          <select
            id={id}
            value={form.controlPlaneReplicas}
            onChange={(e) => set("controlPlaneReplicas", Number(e.target.value))}
          >
            {controlPlaneSizes.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        )}
      </Field>
      <WorkerFields form={form} set={set} shown={shown} cls={cls} />
    </>
  );
}

// Talos brings Flannel itself; Kubehenge installs the other pod networks its class offers.
const ownNetworks: Record<string, string> = { talos: "flannel" };

// Where in the bridge's network the examples of the API and node addresses lie.
const exampleEndpoint = 20;
const exampleFirst = 21;
const exampleLast = 29;

export function NetworkStep({
  form,
  set,
  shown,
  cls,
  bridgeOptions,
  network,
}: StepProps & {
  cls: ClusterClass | undefined;
  bridgeOptions: Bridge[];
  /** The bridge's network, like 192.0.2.0/24, where a node reports it; the examples are taken from it. */
  network?: string | undefined;
}): JSX.Element {
  const cni = variable(cls, "cni");
  const example = (offset: number, otherwise: string): string =>
    (network === undefined ? undefined : addressIn(network, offset)) ?? otherwise;
  // Cilium's ranges also keep clear of the other clusters', so the cluster can join a Cluster Mesh.
  const rangeHint =
    form.cni === "cilium" ? "Leave empty to choose one clear of other clusters" : "Leave empty to choose a free one";
  return (
    <>
      <Field label="Bridge" error={shown("bridge")}>
        {(id) =>
          bridgeOptions.length > 0 ? (
            <select id={id} value={form.bridge} onChange={(e) => set("bridge", e.target.value)}>
              <option value="" disabled>
                Choose…
              </option>
              {bridgeOptions.map((b) => (
                <option key={b.bridge} value={b.bridge} disabled={!b.usable}>
                  {b.bridge}
                  {filled(b.comment) ? ` (${b.comment})` : ""}
                  {b.usable ? "" : " (no SDN.Use)"}
                </option>
              ))}
            </select>
          ) : (
            <input
              id={id}
              value={form.bridge}
              onChange={(e) => set("bridge", e.target.value.trim())}
              placeholder="vmbr0"
            />
          )
        }
      </Field>
      <Field label="VLAN tag" hint="Optional" error={shown("vlan")}>
        {(id) => (
          <input id={id} inputMode="numeric" value={form.vlan} onChange={(e) => set("vlan", e.target.value.trim())} />
        )}
      </Field>
      <Field label="API address" hint="A free address outside the node addresses" error={shown("endpoint")}>
        {(id) => (
          <input
            id={id}
            value={form.endpoint}
            onChange={(e) => set("endpoint", e.target.value.trim())}
            placeholder={example(exampleEndpoint, "10.0.0.20")}
          />
        )}
      </Field>
      <Field
        label="Node addresses"
        hint={`Ranges like ${example(exampleFirst, "10.0.0.21")}-${example(exampleLast, "10.0.0.29")}, networks or addresses`}
        error={shown("addresses")}
      >
        {(id) => (
          <textarea id={id} rows={3} value={form.addresses} onChange={(e) => set("addresses", e.target.value)} />
        )}
      </Field>
      <Field label="Prefix length" error={shown("prefix")}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={1}
            max={addressBits}
            value={shownNumber(form.prefix)}
            onChange={(e) => set("prefix", typedNumber(e.target.value))}
          />
        )}
      </Field>
      <Field label="Gateway" error={shown("gateway")}>
        {(id) => <input id={id} value={form.gateway} onChange={(e) => set("gateway", e.target.value.trim())} />}
      </Field>
      <Field label="DNS servers" error={shown("dnsServers")}>
        {(id) => <input id={id} value={form.dnsServers} onChange={(e) => set("dnsServers", e.target.value)} />}
      </Field>
      {variable(cls, "loadBalancer") && (
        <LoadBalancerFields
          distribution={cls?.distribution}
          implementation={form.loadBalancer}
          addresses={form.loadBalancerAddresses}
          error={shown("loadBalancerAddresses")}
          onImplementation={(i) => set("loadBalancer", i)}
          onAddresses={(a) => set("loadBalancerAddresses", a)}
        />
      )}
      {cni && (
        <Field label="Pod network" hint="Fixed once the cluster exists" error={shown("cni")}>
          {(id) => (
            <select id={id} value={form.cni} onChange={(e) => set("cni", e.target.value)}>
              <option value="" disabled>
                Choose…
              </option>
              {(cni.enum ?? []).map((v) => (
                <option key={String(v)} value={String(v)}>
                  {cniName(String(v))}
                  {v === ownNetworks[cls?.distribution ?? ""]
                    ? ` (${distributionName(cls?.distribution ?? "")}'s own)`
                    : ""}
                </option>
              ))}
            </select>
          )}
        </Field>
      )}
      <details
        className="advanced"
        open={shown("pods") !== undefined || shown("services") !== undefined ? true : undefined}
      >
        <summary>Pod and service ranges</summary>
        <Field label="Pod range" hint={rangeHint} error={shown("pods")}>
          {(id) => (
            <input
              id={id}
              value={form.pods}
              placeholder={cls?.annotations?.["kubehenge.dev/default-pods"]}
              onChange={(e) => set("pods", e.target.value.trim())}
            />
          )}
        </Field>
        <Field label="Service range" hint={rangeHint} error={shown("services")}>
          {(id) => (
            <input
              id={id}
              value={form.services}
              placeholder={cls?.annotations?.["kubehenge.dev/default-services"]}
              onChange={(e) => set("services", e.target.value.trim())}
            />
          )}
        </Field>
      </details>
    </>
  );
}

export function MachinesStep({
  form,
  set,
  shown,
  cls,
  usableStorages,
  placed,
  haOffered,
  csi,
  snapshots,
  haGroups,
  nodes,
  rules,
}: StepProps & {
  cls: ClusterClass | undefined;
  usableStorages: Storage[];
  placed: boolean;
  haOffered: boolean;
  csi: { offered: boolean; problem: string | undefined };
  snapshots: { offered: boolean; problem: string | undefined };
  haGroups: boolean;
  nodes: PveNode[];
  rules: HARulesRead;
}): JSX.Element {
  return (
    <>
      <Field label="Storage" error={shown("storage")}>
        {(id) => (
          <select id={id} value={form.storage} onChange={(e) => set("storage", e.target.value)}>
            <option value="" disabled>
              Choose…
            </option>
            {usableStorages.map((s) => (
              <option key={s.storage} value={s.storage}>
                {s.storage}
                {s.total > 0 ? ` (${bytes(s.avail)} free${s.shared ? ", shared" : ""})` : ""}
              </option>
            ))}
          </select>
        )}
      </Field>
      {placed && (
        <PlacementFields
          value={form.place}
          onChange={(p) => set("place", p)}
          problem={shown("placement")}
          nodes={nodes}
          rules={rules}
        />
      )}
      {haOffered && <HAField checked={form.proxmoxHA} onChange={(on) => set("proxmoxHA", on)} />}
      {csi.offered && (
        <CSIField
          checked={form.datastoreAllocate}
          onChange={(on) => set("datastoreAllocate", on)}
          storage={filled(form.storage) ? form.storage : undefined}
          problem={csi.problem}
        />
      )}
      {snapshots.offered && (
        <SnapshotsField
          checked={form.volumeSnapshots}
          onChange={(on) => set("volumeSnapshots", on)}
          problem={snapshots.problem}
        />
      )}
      {haGroups && <HAGroupsField checked={form.haGroups} onChange={(on) => set("haGroups", on)} />}
      <MachineFields
        label="Control plane"
        value={form.controlPlaneMachine}
        min={machineLeast(form, cls, "controlPlaneMachine")}
        error={shown("controlPlaneMachine")}
        onChange={(m) => set("controlPlaneMachine", m)}
      />
      <MachineFields
        label="Workers"
        value={form.workerMachine}
        min={machineLeast(form, cls, "workerMachine")}
        error={shown("workerMachine")}
        onChange={(m) => set("workerMachine", m)}
      />
    </>
  );
}

export function IdentityStep({
  form,
  set,
  canTrust,
  showAll,
}: Omit<StepProps, "shown"> & {
  canTrust: boolean | undefined;
  showAll: boolean;
}): JSX.Element {
  return (
    <OIDCProviders initial={form.oidc} fixed={canTrust === false} showAll={showAll} onChange={(v) => set("oidc", v)} />
  );
}

function MachineFields({
  label,
  value,
  min,
  error,
  onChange,
}: {
  label: string;
  value: Machine;
  min: Machine;
  error?: string | undefined;
  onChange: (m: Machine) => void;
}): JSX.Element {
  // fieldProblems names the first of cores, memory and disk that is wrong.
  const on = (part: string): string | undefined => (error?.toLowerCase().includes(part) === true ? error : undefined);
  return (
    <fieldset className="group">
      <legend>{label}</legend>
      <Field label="Cores" error={on("cores")}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={min.cores}
            value={shownNumber(value.cores)}
            onChange={(e) => onChange({ ...value, cores: typedNumber(e.target.value) })}
          />
        )}
      </Field>
      <Field label="Memory (MiB)" error={on("memory")}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={min.memoryMiB}
            step={memoryStepMiB}
            value={shownNumber(value.memoryMiB)}
            onChange={(e) => onChange({ ...value, memoryMiB: typedNumber(e.target.value) })}
          />
        )}
      </Field>
      <Field label="Disk (GiB)" error={on("disk")}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={min.diskGiB}
            value={shownNumber(value.diskGiB)}
            onChange={(e) => onChange({ ...value, diskGiB: typedNumber(e.target.value) })}
          />
        )}
      </Field>
    </fieldset>
  );
}
