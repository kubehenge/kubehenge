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
import { addressBits } from "./network";
import { choosable, cpuNote, macName, nodeOSKey, nodeOSLabel } from "./node-os";
import { OIDCProviders } from "./OIDCProviders";
import { HAField, HAGroupsField, PlacementFields, type HARulesRead } from "./Placement";
import type {
  Bridge,
  ClusterClass,
  NodeOSChoice,
  PveNode,
  Storage,
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

// offersAsk says whether a cluster creator is offered a template build: the administrators allow
// it, the node OS can be chosen, and its template is missing or, for kubeadm, may be at the version.
function offersAsk({
  builds,
  distribution,
  os,
  missing,
}: {
  builds: TemplateBuilds;
  distribution: string;
  os: NodeOSChoice | undefined;
  missing: string | undefined;
}): boolean {
  if (!builds.allowed || distribution === "talos" || (os !== undefined && !choosable(os))) {
    return false;
  }
  return distribution === "kubeadm" || filled(missing);
}

// templateAsk is the request for a missing template: the distribution's, kubeadm's of a version, on the node OS.
function templateAsk(distribution: string, version: string, os: NodeOSChoice | undefined): TemplateBuildRequest {
  return {
    distribution,
    ...(distribution === "kubeadm" && { kubernetesVersion: version }),
    ...(os !== undefined && { nodeOS: nodeOSKey(os) }),
  };
}

// requestKey is how the server lists a request: the template line's name without its kubehenge- prefix.
const requestKey = (req: TemplateBuildRequest): string =>
  [req.distribution, req.kubernetesVersion ?? "", req.nodeOS ?? ""].filter(Boolean).join("-");

function askText(kubeadm: boolean, os: NodeOSChoice | undefined): string {
  const on = os === undefined ? "" : ` on ${os.title}`;
  return kubeadm
    ? `Kubeadm offers the versions that have a VM template${on}. Another one gets its template built on request, which takes about twenty minutes.`
    : `Kubehenge builds the missing template${on} on request, which takes about twenty minutes.`;
}

// TemplateAsk lets one who may create clusters have a missing template built on the node OS chosen,
// when the administrators allow it: the distribution's template, or a kubeadm version's.
function TemplateAsk({
  cls,
  os,
  missing,
}: {
  cls: ClusterClass | undefined;
  os: NodeOSChoice | undefined;
  missing: string | undefined;
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
  const distribution = cls?.distribution;
  if (builds === undefined || distribution === undefined || !offersAsk({ builds, distribution, os, missing })) {
    return null;
  }
  const kubeadm = distribution === "kubeadm";
  const req = templateAsk(distribution, version, os);
  const asked = builds.requested.includes(requestKey(req));
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
      <legend>Missing template</legend>
      <p className="muted">
        {askText(kubeadm, os)}
        {builds.building.length > 0 && ` Building now: ${builds.building.join(", ")}.`}
        {builds.requested.length > 0 && ` Asked for: ${builds.requested.join(", ")}.`}
      </p>
      {kubeadm && (
        <Field
          label="Kubernetes version to build"
          hint="Such as v1.36.5."
          error={version !== "" && !kubeadmVersion.test(version) ? "A version such as v1.36.5." : undefined}
        >
          {(id) => <input id={id} value={version} onChange={(e) => setVersion(e.target.value.trim())} />}
        </Field>
      )}
      <ErrorBox message={error} />
      <div className="actions">
        <button
          type="button"
          disabled={busy || asked || (kubeadm && !kubeadmVersion.test(version))}
          onClick={() => {
            void ask();
          }}
        >
          {asked ? "Asked for" : "Have the template built"}
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
  const note = cpuNote(os, oses);
  return (
    <Field
      label="Node OS"
      error={shown("nodeOS")}
      hint={`Kubehenge builds its templates from ${os.title}'s own signed packages, only what a node needs, with ${macName(os.mac)} enforcing.${note === undefined ? "" : ` ${note}`}`}
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

function versionHint(cls: ClusterClass | undefined, offered: Versions | undefined): string | undefined {
  if (offered && offered.versions.length > 0) {
    return cls?.distribution === "kubeadm"
      ? "The versions with a VM template."
      : "The newest release of each minor version Kubernetes still patches.";
  }
  if (!cls) {
    return undefined;
  }
  const reason = offered?.error;
  return `For example ${versionExamples[cls.distribution] ?? ""}.${filled(reason) ? ` ${sentence(reason)}` : ""}`;
}

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
  onChooseClass,
  namespaces,
  onChooseNamespace,
}: StepProps & {
  classes: ClusterClass[];
  cls: ClusterClass | undefined;
  offered: Versions | undefined;
  onChooseClass: (name: string) => void;
  namespaces: string[];
  onChooseNamespace: (namespace: string) => void;
}): JSX.Element {
  const alone = machineLeast({ ...form, workerReplicas: 0, workerAutoscale: false }, cls, "controlPlaneMachine");
  return (
    <>
      {namespaces.length > 1 && (
        <Field
          label="Namespace"
          hint="Where the cluster's objects live in the management cluster; Cluster Mesh links clusters of one namespace."
        >
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
      <Field label="Name" hint="Also names its Proxmox VE pool and VMs." error={shown("name")}>
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
      <Field label="Kubernetes version" error={shown("version")} hint={versionHint(cls, offered)}>
        {(id) => (
          <VersionInput id={id} value={form.version} onChange={(v) => set("version", v)} cls={cls} offered={offered} />
        )}
      </Field>
      <TemplateAsk cls={cls} os={chosenNodeOS(form, cls)} missing={templateMissing(form, cls)} />
      <Field label="Control-plane nodes" hint="Three or more keep the cluster running when one node fails.">
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
      <WorkerFields
        form={form}
        set={set}
        shown={shown}
        cls={cls}
        alone={`With none, the control planes run the workloads too, so they have at least ${alone.cores} cores and ${alone.memoryMiB} MiB.`}
      />
    </>
  );
}

export function NetworkStep({
  form,
  set,
  shown,
  cls,
  bridgeOptions,
}: StepProps & { cls: ClusterClass | undefined; bridgeOptions: Bridge[] }): JSX.Element {
  const cni = variable(cls, "cni");
  const rangeHint =
    form.cni === "cilium"
      ? "Leave empty for the distribution's default, or the first free range if that overlaps the nodes' network or another cluster's ranges, so the cluster can join a Cluster Mesh."
      : "Leave empty for the distribution's default, or the first free range if that overlaps the nodes' network.";
  return (
    <>
      <Field label="Bridge" hint="Only the bridges allowed for clusters are listed." error={shown("bridge")}>
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
      <Field label="VLAN tag" hint="Optional." error={shown("vlan")}>
        {(id) => (
          <input id={id} inputMode="numeric" value={form.vlan} onChange={(e) => set("vlan", e.target.value.trim())} />
        )}
      </Field>
      <Field
        label="API address"
        hint="An unused address the control plane holds, outside the node addresses."
        error={shown("endpoint")}
      >
        {(id) => (
          <input
            id={id}
            value={form.endpoint}
            onChange={(e) => set("endpoint", e.target.value.trim())}
            placeholder="10.0.0.20"
          />
        )}
      </Field>
      <Field
        label="Node addresses"
        hint="Ranges (10.0.0.21-10.0.0.29), networks or single addresses, one per line or separated by commas. One more than there are nodes, for rollouts."
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
      <Field label="DNS servers" hint="One per line or separated by commas." error={shown("dnsServers")}>
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
        <Field label="Pod network" hint={cni.description ?? "Fixed once the cluster exists."} error={shown("cni")}>
          {(id) => (
            <select id={id} value={form.cni} onChange={(e) => set("cni", e.target.value)}>
              <option value="" disabled>
                Choose…
              </option>
              {(cni.enum ?? []).map((v) => (
                <option key={String(v)} value={String(v)}>
                  {cniName(String(v))}
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
  haGroups: { offered: boolean; problem: string | undefined };
  nodes: PveNode[];
  rules: HARulesRead;
}): JSX.Element {
  return (
    <>
      <Field label="Storage" hint="For the nodes' disks and the cluster's persistent volumes." error={shown("storage")}>
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
          hint="The Proxmox VE nodes the cluster's VMs may run on. Its control-plane VMs go one to a node, as far as the nodes and their memory allow."
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
      {haGroups.offered && (
        <HAGroupsField checked={form.haGroups} onChange={(on) => set("haGroups", on)} problem={haGroups.problem} />
      )}
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
  cls,
  canTrust,
  showAll,
}: Omit<StepProps, "shown"> & {
  cls: ClusterClass | undefined;
  canTrust: boolean | undefined;
  showAll: boolean;
}): JSX.Element {
  return (
    <>
      <p className="muted">
        Besides tokens and certificates, which always work, the cluster can accept sign-ins from OpenID Connect
        providers.
        {variable(cls, "oidc")?.default !== undefined &&
          " The list starts with the providers Kubehenge's administrators set for every cluster."}{" "}
        Adding a provider of your own needs Sys.Modify on the workload pool, as whoever runs it can sign in as anyone.
      </p>
      <OIDCProviders
        initial={form.oidc}
        fixed={canTrust === false}
        showAll={showAll}
        onChange={(v) => set("oidc", v)}
      />
    </>
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
