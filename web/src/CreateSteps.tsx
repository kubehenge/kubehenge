import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { WorkerFields } from "./Autoscaling";
import {
  chosenNodeOS,
  controlPlaneSizes,
  formLoadBalancerValue,
  formReleaseMirror,
  machineLeast,
  memoryNodeWarning,
  mostWorkers,
  setLoadBalancer,
  templateMissing,
  variable,
  versionExamples,
  versionLabel,
  type FieldKey,
  type Form,
} from "./create-form";
import { CSIField, SnapshotsField } from "./CsiPlugin";
import { maxEtcdQuotaGiB, minEtcdQuotaGiB } from "./EtcdSize";
import { FamiliesField } from "./Families";
import { SecurityGroupsField } from "./Firewall";
import { HostOutageField, graceVariable } from "./HostOutage";
import { LoadBalancerFields, passwordInfo, passwordLabel } from "./LoadBalancer";
import { MachineFields } from "./MachineFields";
import {
  addressIn,
  bitsOf,
  bridgeLabel,
  familiesOf,
  familyFieldLabel,
  familyNames,
  gatewayNetwork,
  type Family,
  type FamilyChoice,
} from "./network";
import { baseTitle, choosable, cpuNote, findNodeOS, fipsCNI, macName, nodeOSKey, nodeOSLabel } from "./node-os";
import { NodeCustomisationFields } from "./NodeCustomisation";
import { NodeSettingsFields } from "./NodeSettings";
import { MirrorFields, ReleaseMirrorFields, TrustedCAField } from "./NodeSite";
import { NodeTimeoutFields } from "./NodeTimeouts";
import { OIDCProviders } from "./OIDCProviders";
import { HAField, HAGroupsField, PlacementFields, type HARulesRead } from "./Placement";
import { clusterKey } from "./start-from";
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
  Cluster,
  ClusterClass,
  FreeAddresses,
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
// any kubeadm version, or a Talos release without a template.
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
    talos: "Baking takes about 2 to 4 minutes",
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
        // Without the answer there is nothing to offer. The class's own message stays.
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

/** nodeOSInfo says what a node OS holds, and for FIPS mode that the OS is not a validated module of its own. */
export function nodeOSInfo(os: NodeOSChoice, oses: NodeOSChoice[]): string {
  const from = `Minimal, from ${baseTitle(os, oses)}'s signed packages, with ${macName(os.mac)} enforcing`;
  return os.fips === true ? `${from} and FIPS mode on. The OS itself is not CMVP validated.` : `${from}.`;
}

// NodeOSField chooses what the nodes run, for every distribution but Talos, whose nodes run Talos.
function NodeOSField({ form, set, shown, cls }: StepProps & { cls: ClusterClass | undefined }): JSX.Element | null {
  const oses = cls?.nodeOSes ?? [];
  const os = chosenNodeOS(form, cls);
  if (os === undefined) {
    return null;
  }
  return (
    <Field label="Node OS" error={shown("nodeOS")} hint={cpuNote(os, oses)} info={nodeOSInfo(os, oses)}>
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

// How long after the network settings last change the wizard probes them.
const probeAfter = 500;

/**
 * ProbeOn is where addresses are probed: the bridge of a node that reports its network, the VLAN
 * tag typed, and the address families asked for. The gateway and prefix length of a family give
 * the network searched, else the bridge's is.
 */
export interface ProbeOn {
  node: string | undefined;
  bridge: string;
  vlan: string;
  families: FamilyChoice;
  gateway: string;
  prefix: number;
  gateway6: string;
  prefix6: number;
}

/**
 * useFreeAddresses asks, a moment after its inputs last change, for free addresses on the nodes'
 * network: a run for the API and the nodes where nodes is above 0, and which of check a host
 * answers on. It is advice, so where the question fails there is none.
 */
export function useFreeAddresses(on: ProbeOn, nodes: number, check: string[]): FreeAddresses | undefined {
  const { node, bridge, gateway, prefix, gateway6, prefix6 } = on;
  const vlan = filled(on.vlan) ? Number(on.vlan) : 0;
  const checked = check.join("\n");
  const asked = familyNames(on.families).join(",");
  const key = [node, bridge, vlan, nodes, checked, asked, gateway, prefix, gateway6, prefix6].join(" ");
  const [found, setFound] = useState<{ key: string; free: FreeAddresses }>();
  useEffect(() => {
    let current = true;
    async function probe(n: string): Promise<void> {
      try {
        const free = await api.freeAddresses(n, bridge, {
          vlan,
          nodes,
          check: checked === "" ? [] : checked.split("\n"),
          ...(asked === "IPv4" ? {} : { families: asked.split(",") }),
          ...(filled(gateway) && Number.isInteger(prefix) && { gateway, prefix }),
          ...(filled(gateway6) && Number.isInteger(prefix6) && { gateway6, prefix6 }),
        });
        if (current) {
          setFound({ key, free });
        }
      } catch {
        // The fields stay as they are.
      }
    }
    let timer: ReturnType<typeof setTimeout> | undefined;
    if (node !== undefined && bridge !== "" && Number.isInteger(vlan) && (nodes > 0 || checked !== "")) {
      timer = setTimeout(() => {
        void probe(node);
      }, probeAfter);
    }
    return () => {
      current = false;
      clearTimeout(timer);
    };
  }, [node, bridge, vlan, nodes, checked, asked, gateway, prefix, gateway6, prefix6, key]);
  return found?.key === key ? found.free : undefined;
}

// At most this many addresses a host answers on are named. The rest are counted.
const inUseNamed = 3;

function inUse(addresses: string[]): JSX.Element | undefined {
  if (addresses.length === 0) {
    return undefined;
  }
  const more = addresses.length > inUseNamed ? ` +${addresses.length - inUseNamed}` : "";
  return (
    <span className="hint-warning">
      In use: {addresses.slice(0, inUseNamed).join(", ")}
      {more}
    </span>
  );
}

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

// StartFromField chooses the cluster a new one starts as, or none for the class's defaults.
function StartFromField({
  from,
  starts,
  onChoose,
  namespaced,
}: {
  from: string;
  starts: Cluster[];
  onChoose: (key: string) => void;
  namespaced: boolean;
}): JSX.Element | null {
  if (starts.length === 0) {
    return null;
  }
  return (
    <Field label="Start from">
      {(id) => (
        <select id={id} value={from} onChange={(e) => onChoose(e.target.value)}>
          <option value="">Defaults</option>
          {starts.map((c) => (
            <option key={clusterKey(c)} value={clusterKey(c)}>
              {namespaced ? clusterKey(c) : c.name}
            </option>
          ))}
        </select>
      )}
    </Field>
  );
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
  starts,
  onChooseFrom,
}: StepProps & {
  classes: ClusterClass[];
  cls: ClusterClass | undefined;
  offered: Versions | undefined;
  /** The Talos releases, for a Talos class. Undefined while they load. */
  talos?: TalosReleases | undefined;
  onChooseClass: (name: string) => void;
  namespaces: string[];
  onChooseNamespace: (namespace: string) => void;
  /** The clusters a new one may start from, the newest first. */
  starts: Cluster[];
  onChooseFrom: (key: string) => void;
}): JSX.Element {
  const runs = forRelease(offered, talos, form.talosVersion);
  const typed = typedVersion(cls, runs);
  const unlisted = runs?.error;
  return (
    <>
      <StartFromField from={form.from} starts={starts} onChoose={onChooseFrom} namespaced={namespaces.length > 1} />
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
      <details className="advanced" open={shown("timeouts") === undefined ? undefined : true}>
        <summary>Node timeouts</summary>
        <h4 className="subsection">Control planes</h4>
        <NodeTimeoutFields
          value={form.controlPlaneTimeouts}
          onChange={(v) => set("controlPlaneTimeouts", v)}
          fallback={cls?.controlPlaneTimeouts}
        />
        <h4 className="subsection">Workers</h4>
        <NodeTimeoutFields
          value={form.workerTimeouts}
          onChange={(v) => set("workerTimeouts", v)}
          fallback={cls?.workerTimeouts}
        />
      </details>
    </>
  );
}

// Talos brings Flannel itself. Kubehenge installs the other pod networks its class offers.
const ownNetworks: Record<string, string> = { talos: "flannel" };

// Where in the bridge's network the examples of the API and node addresses lie.
const exampleEndpoint = 20;
const exampleFirst = 21;
const exampleLast = 29;

// The ranges the controller takes for an IPv6 family when none is asked for.
const defaultPodsIPv6 = "fd5e:4b1c:9a20::/56";
const defaultServicesIPv6 = "fd5e:4b1c:9a21::/112";

// FamilyFields are the node addresses, prefix length and gateway of one address family. Their
// labels name the family where both families show.
function FamilyFields({
  family,
  shownFamilies,
  form,
  set,
  shown,
  inUse: busy,
  full,
  first,
  last,
}: StepProps & {
  family: Family;
  shownFamilies: number;
  inUse: JSX.Element | undefined;
  /** The network searched, where it has no free addresses for the cluster. */
  full: string | undefined;
  first: string;
  last: string;
}): JSX.Element {
  const [addresses, prefix, gateway] =
    family === "IPv6"
      ? (["addresses6", "prefix6", "gateway6"] as const)
      : (["addresses", "prefix", "gateway"] as const);
  const label = (plain: string): string => familyFieldLabel(plain, family, shownFamilies);
  const none =
    full === undefined || filled(form[addresses]) ? undefined : (
      <span className="hint-warning">No free addresses found in {full}</span>
    );
  return (
    <>
      <Field
        label={label("Node addresses")}
        hint={busy ?? none ?? `Ranges like ${first}-${last}, networks or addresses`}
        error={shown(addresses)}
      >
        {(id) => <textarea id={id} rows={3} value={form[addresses]} onChange={(e) => set(addresses, e.target.value)} />}
      </Field>
      <Field label={label("Prefix length")} error={shown(prefix)}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={1}
            max={bitsOf(family)}
            value={shownNumber(form[prefix])}
            onChange={(e) => set(prefix, typedNumber(e.target.value))}
          />
        )}
      </Field>
      <Field label={label("Gateway")} error={shown(gateway)}>
        {(id) => <input id={id} value={form[gateway]} onChange={(e) => set(gateway, e.target.value.trim())} />}
      </Field>
    </>
  );
}

/** InterfaceFields are the options of the nodes' interface the class offers. */
function InterfaceFields({ form, set, shown, cls }: StepProps & { cls: ClusterClass | undefined }): JSX.Element {
  const network = variable(cls, "network")?.properties;
  return (
    <>
      <Field label="VLAN tag" hint="Optional" error={shown("vlan")}>
        {(id) => (
          <input id={id} inputMode="numeric" value={form.vlan} onChange={(e) => set("vlan", e.target.value.trim())} />
        )}
      </Field>
      {network?.["mtu"] === undefined ? null : (
        <Field label="MTU" hint="Optional" error={shown("mtu")}>
          {(id) => (
            <input
              id={id}
              inputMode="numeric"
              value={form.mtu}
              onChange={(e) => set("mtu", e.target.value.trim())}
              placeholder="1500"
            />
          )}
        </Field>
      )}
      {network?.["queues"] === undefined ? null : (
        <Field label="Queues" hint="Optional" error={shown("queues")}>
          {(id) => (
            <input
              id={id}
              inputMode="numeric"
              value={form.queues}
              onChange={(e) => set("queues", e.target.value.trim())}
            />
          )}
        </Field>
      )}
      {variable(cls, "securityGroups") === undefined ? null : (
        <SecurityGroupsField
          value={form.securityGroups}
          onChange={(s) => set("securityGroups", s)}
          error={shown("securityGroups")}
        />
      )}
    </>
  );
}

export function NetworkStep({
  form,
  set,
  shown,
  cls,
  bridgeOptions,
  network,
  network6,
  free,
  checked,
}: StepProps & {
  cls: ClusterClass | undefined;
  bridgeOptions: Bridge[];
  /** The bridge's network, like 192.0.2.0/24, where a node reports it. The examples are taken from it where the gateway gives none. */
  network?: string | undefined;
  /** The same for IPv6, like 2001:db8::/64. */
  network6?: string | undefined;
  /** The free addresses found for the cluster. */
  free?: FreeAddresses | undefined;
  /** What probing the addresses typed found. */
  checked?: FreeAddresses | undefined;
}): JSX.Element {
  const fullIn = (family: Family): string | undefined => {
    const [full, searched] = family === "IPv6" ? [free?.full6, free?.network6] : [free?.full, free?.network];
    return full === true ? searched : undefined;
  };
  const cni = variable(cls, "cni");
  const fips = chosenNodeOS(form, cls)?.fips === true;
  const answering = checked?.answering ?? [];
  const endpointInUse = inUse(answering.filter((a) => a === form.endpoint));
  const nodesInUse = inUse(answering.filter((a) => a !== form.endpoint));
  const families = familiesOf(form.families);
  const examples = exampleNetworks(form, { IPv4: network, IPv6: network6 });
  const sample = (family: Family, offset: number): string => exampleAddress(family, offset, examples[family]);
  const [primary = "IPv4"] = families;
  const offersFamilies = variable(cls, "network")?.properties?.["ipFamilies"] !== undefined;
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
                  {bridgeLabel(b)}
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
      <InterfaceFields form={form} set={set} shown={shown} cls={cls} />
      {offersFamilies && (
        <FamiliesField
          value={form.families}
          hint="Fixed once the cluster exists"
          onChoose={(choice) => {
            set("families", choice);
          }}
        />
      )}
      <Field
        label="API address"
        hint={endpointInUse ?? "A free address outside the node addresses"}
        error={shown("endpoint")}
      >
        {(id) => (
          <input
            id={id}
            value={form.endpoint}
            onChange={(e) => set("endpoint", e.target.value.trim())}
            placeholder={sample(primary, exampleEndpoint)}
          />
        )}
      </Field>
      {families.map((family) => (
        <FamilyFields
          key={family}
          family={family}
          shownFamilies={families.length}
          form={form}
          set={set}
          shown={shown}
          inUse={nodesInUse}
          full={fullIn(family)}
          first={sample(family, exampleFirst)}
          last={sample(family, exampleLast)}
        />
      ))}
      <Field label="DNS servers" error={shown("dnsServers")}>
        {(id) => <input id={id} value={form.dnsServers} onChange={(e) => set("dnsServers", e.target.value)} />}
      </Field>
      <Field label="Search domains" error={shown("searchDomains")}>
        {(id) => (
          <input
            id={id}
            value={form.searchDomains}
            placeholder="corp.example"
            onChange={(e) => set("searchDomains", e.target.value)}
          />
        )}
      </Field>
      <Field label="Time servers" hint="Optional, separated by commas" error={shown("ntpServers")}>
        {(id) => <input id={id} value={form.ntpServers} onChange={(e) => set("ntpServers", e.target.value)} />}
      </Field>
      {variable(cls, "registryMirrors") && (
        <MirrorFields
          value={form.registryMirrors}
          onChange={(rows) => set("registryMirrors", rows)}
          error={shown("registryMirrors")}
        />
      )}
      {variable(cls, "trustedCAs") && (
        <TrustedCAField value={form.trustedCAs} onChange={(s) => set("trustedCAs", s)} error={shown("trustedCAs")} />
      )}
      {variable(cls, "releaseMirror") && (
        <ReleaseMirrorFields
          value={formReleaseMirror(form)}
          onChange={(m) => {
            if (m.url !== form.releaseMirror) {
              set("releaseMirror", m.url);
            }
            if (m.caBundle !== form.releaseMirrorCA) {
              set("releaseMirrorCA", m.caBundle);
            }
          }}
          fallback={cls === undefined ? undefined : (cls.releaseMirror ?? "")}
          errors={{ url: shown("releaseMirror"), caBundle: shown("releaseMirrorCA") }}
        />
      )}
      {variable(cls, "loadBalancer") && (
        <LoadBalancerFields
          distribution={cls?.distribution}
          families={form.families}
          value={formLoadBalancerValue(form)}
          errors={{
            addresses: shown("loadBalancerAddresses"),
            asn: shown("loadBalancerAsn"),
            peers: shown("loadBalancerPeers"),
            routerID: shown("loadBalancerRouterID"),
          }}
          onChange={(patch) => {
            setLoadBalancer(set, patch);
          }}
          password={
            <Field label={passwordLabel} hint={passwordInfo} error={shown("loadBalancerPassword")}>
              {(id) => (
                <input
                  id={id}
                  type="password"
                  autoComplete="new-password"
                  spellCheck={false}
                  value={form.loadBalancerPassword}
                  onChange={(e) => set("loadBalancerPassword", e.target.value)}
                />
              )}
            </Field>
          }
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
                <option key={String(v)} value={String(v)} disabled={fips && v !== fipsCNI}>
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
      <RangeFields form={form} set={set} shown={shown} cls={cls} />
    </>
  );
}

// exampleNetworks are the networks the examples of each family are taken from: the gateway's, the
// one the free addresses come from, else the bridge's where a node reports it.
const exampleNetworks = (
  form: Form,
  bridge: Record<Family, string | undefined>,
): Record<Family, string | undefined> => ({
  IPv4: gatewayNetwork(form.gateway, form.prefix, "IPv4") ?? bridge.IPv4,
  IPv6: gatewayNetwork(form.gateway6, form.prefix6, "IPv6") ?? bridge.IPv6,
});

// exampleAddress is the address that many into the example network, else in a documentation network.
function exampleAddress(family: Family, offset: number, network: string | undefined): string {
  const fallback = family === "IPv4" ? `10.0.0.${offset}` : `2001:db8::${offset}`;
  return (network === undefined ? undefined : addressIn(network, offset)) ?? fallback;
}

// RangeFields are the pod and service ranges, one per family when the cluster has both.
function RangeFields({ form, set, shown, cls }: StepProps & { cls: ClusterClass | undefined }): JSX.Element {
  const families = familiesOf(form.families);
  const dual = families.length > 1;
  // The ranges the class would choose, one per family in order, for the fields to show as examples.
  const defaultRanges = (annotation: string, v6: string): string | undefined =>
    families
      .map((f) => (f === "IPv4" ? cls?.annotations?.[annotation] : v6))
      .filter((r) => r !== undefined)
      .join(", ") || undefined;
  // Cilium's ranges also keep clear of the other clusters', so the cluster can join a Cluster Mesh.
  const rangeHint =
    form.cni === "cilium" ? "Leave empty to choose one clear of other clusters" : "Leave empty to choose a free one";
  return (
    <details
      className="advanced"
      open={shown("pods") !== undefined || shown("services") !== undefined ? true : undefined}
    >
      <summary>Pod and service ranges</summary>
      <Field label={dual ? "Pod ranges" : "Pod range"} hint={rangeHint} error={shown("pods")}>
        {(id) => (
          <input
            id={id}
            value={form.pods}
            placeholder={defaultRanges("kubehenge.dev/default-pods", defaultPodsIPv6)}
            onChange={(e) => set("pods", e.target.value)}
          />
        )}
      </Field>
      <Field label={dual ? "Service ranges" : "Service range"} hint={rangeHint} error={shown("services")}>
        {(id) => (
          <input
            id={id}
            value={form.services}
            placeholder={defaultRanges("kubehenge.dev/default-services", defaultServicesIPv6)}
            onChange={(e) => set("services", e.target.value)}
          />
        )}
      </Field>
    </details>
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
  attempted = false,
  refusal,
}: StepProps & {
  /** Next was pressed on the step: every row's problem shows. */
  attempted?: boolean;
  /** The server's refusal of the node customisation, shown with it. */
  refusal?: string | undefined;
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
      <Field
        label="Storage"
        error={shown("storage")}
        hint={
          usableStorages.some((s) => s.storage === form.storage && !s.shared)
            ? "Volumes on node-local storage stay on one node."
            : undefined
        }
      >
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
          warning={memoryNodeWarning(form, nodes)}
          nodes={nodes}
          rules={rules}
        />
      )}
      {variable(cls, graceVariable) !== undefined && (
        <HostOutageField
          value={form.hostOutageGrace}
          onChange={(minutes) => set("hostOutageGrace", minutes)}
          problem={shown("hostOutageGrace")}
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
      {variable(cls, "etcd") !== undefined && (
        <details className="advanced" open={shown("etcdQuotaGiB") === undefined ? undefined : true}>
          <summary>etcd</summary>
          <Field
            label="Database limit (GiB)"
            hint="It can be raised later, never lowered."
            error={shown("etcdQuotaGiB")}
          >
            {(id) => (
              <input
                id={id}
                type="number"
                min={minEtcdQuotaGiB}
                max={maxEtcdQuotaGiB}
                step={1}
                value={shownNumber(form.etcdQuotaGiB)}
                onChange={(e) => set("etcdQuotaGiB", typedNumber(e.target.value))}
              />
            )}
          </Field>
        </details>
      )}
      {variable(cls, "workerNodes") !== undefined && mostWorkers(form) > 0 && (
        <NodeSettingsFields
          value={form.workerNodes}
          onChange={(v) => set("workerNodes", v)}
          showAll={attempted}
          workers
        />
      )}
      {variable(cls, "nodeCustomisation") !== undefined && (
        <details
          className="advanced"
          open={(attempted && shown("nodeCustomisation") !== undefined) || refusal !== undefined ? true : undefined}
        >
          <summary>Advanced</summary>
          <NodeCustomisationFields
            value={form.nodeCustomisation}
            onChange={(v) => set("nodeCustomisation", v)}
            talos={cls?.distribution === "talos"}
            showAll={attempted}
            refusal={refusal}
          />
        </details>
      )}
    </>
  );
}

export function IdentityStep({
  form,
  set,
  canTrust,
  showAll,
}: Omit<StepProps, "shown"> & { canTrust: boolean | undefined; showAll: boolean }): JSX.Element {
  return (
    <OIDCProviders initial={form.oidc} fixed={canTrust === false} showAll={showAll} onChange={(v) => set("oidc", v)} />
  );
}
