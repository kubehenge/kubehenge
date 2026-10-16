import type { JSX } from "react";

import { controlPlaneSizes, maxWorkers } from "./ClusterDetail";
import { modeChoices, upgradeModes } from "./ClusterUpgrades";
import {
  memoryStepMiB,
  minMemoryMiB,
  rancherMinimumCores,
  variable,
  versionExamples,
  versionLabel,
  type FieldKey,
  type Form,
  type Machine,
} from "./create-form";
import { addressBits } from "./network";
import { OIDCProviders } from "./OIDCProviders";
import { HAField, PlacementFields, type HARulesRead } from "./Placement";
import type { Bridge, ClusterClass, PveNode, Storage, Versions } from "./types";
import { Field, browserZone, bytes, distributionName, filled, registerHint, sentence } from "./ui";
import { oneOf } from "./values";

// Number fields keep an empty or unfinished entry as NaN, which the checks refuse, rather than
// turning it into 0 under the cursor.
const shownNumber = (n: number): string => (Number.isNaN(n) ? "" : String(n));
const typedNumber = (s: string): number => (s.trim() === "" ? Number.NaN : Number(s));

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
}: StepProps & {
  classes: ClusterClass[];
  cls: ClusterClass | undefined;
  offered: Versions | undefined;
  onChooseClass: (name: string) => void;
}): JSX.Element {
  return (
    <>
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
      <Field label="Kubernetes version" error={shown("version")} hint={versionHint(cls, offered)}>
        {(id) => (
          <VersionInput id={id} value={form.version} onChange={(v) => set("version", v)} cls={cls} offered={offered} />
        )}
      </Field>
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
      <Field label="Workers" error={shown("workerReplicas")}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={0}
            max={maxWorkers}
            value={shownNumber(form.workerReplicas)}
            onChange={(e) => set("workerReplicas", typedNumber(e.target.value))}
          />
        )}
      </Field>
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
      {cni && (
        <Field label="Pod network" hint={cni.description ?? "Fixed once the cluster exists."} error={shown("cni")}>
          {(id) => (
            <select id={id} value={form.cni} onChange={(e) => set("cni", e.target.value)}>
              <option value="" disabled>
                Choose…
              </option>
              {(cni.enum ?? []).map((v) => (
                <option key={String(v)} value={String(v)}>
                  {String(v)}
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
        <Field
          label="Pod range"
          hint="Leave empty for the distribution's default, or the first free range if that overlaps the nodes' network."
          error={shown("pods")}
        >
          {(id) => (
            <input
              id={id}
              value={form.pods}
              placeholder="10.244.0.0/16"
              onChange={(e) => set("pods", e.target.value.trim())}
            />
          )}
        </Field>
        <Field
          label="Service range"
          hint="Leave empty for the distribution's default, or the first free range if that overlaps the nodes' network."
          error={shown("services")}
        >
          {(id) => (
            <input
              id={id}
              value={form.services}
              placeholder="10.96.0.0/16"
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
  usableStorages,
  placed,
  haOffered,
  nodes,
  rules,
}: StepProps & {
  usableStorages: Storage[];
  placed: boolean;
  haOffered: boolean;
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
      <MachineFields
        label="Control plane"
        value={form.controlPlaneMachine}
        error={shown("controlPlaneMachine")}
        onChange={(m) => set("controlPlaneMachine", m)}
      />
      <MachineFields
        label="Workers"
        value={form.workerMachine}
        error={shown("workerMachine")}
        onChange={(m) => set("workerMachine", m)}
      />
    </>
  );
}

export function AddOnsStep({
  form,
  set,
  shown,
  cls,
  canTrust,
}: StepProps & { cls: ClusterClass | undefined; canTrust: boolean | undefined }): JSX.Element {
  return (
    <>
      <label className="check">
        <input
          type="checkbox"
          checked={form.headlamp}
          aria-describedby="headlamp-hint"
          onChange={(e) => set("headlamp", e.target.checked)}
        />{" "}
        Install Headlamp
      </label>
      <p className="hint" id="headlamp-hint">
        A web interface inside the cluster. People sign in to it with their own token; it has no rights of its own.
      </p>
      {form.headlamp && (
        <Field
          label="Headlamp host name"
          hint="Optional: also serve it at this name through the cluster's ingress controller."
        >
          {(id) => (
            <input
              id={id}
              value={form.headlampHostname}
              onChange={(e) => set("headlampHostname", e.target.value.trim())}
            />
          )}
        </Field>
      )}
      <label className="check">
        <input
          type="checkbox"
          checked={form.rancherInstall}
          disabled={canTrust === false && !form.rancherInstall}
          aria-describedby="rancher-hint"
          onChange={(e) => {
            set("rancherInstall", e.target.checked);
            if (!e.target.checked) {
              return;
            }
            for (const key of ["controlPlaneMachine", "workerMachine"] as const) {
              if (form[key].cores < rancherMinimumCores) {
                set(key, { ...form[key], cores: rancherMinimumCores });
              }
            }
          }}
        />{" "}
        Install Rancher
      </label>
      <p className="hint" id="rancher-hint">
        Into this cluster; every other cluster then registers with it.
        {form.rancherInstall &&
          ` Rancher needs at least ${rancherMinimumCores} cores on every node, so the machines have at least that many.`}
        {canTrust === false &&
          " Rancher holds every registered cluster's admin credentials, so installing it needs Sys.Modify on the workload pool."}
      </p>
      {form.rancherInstall ? (
        <Field
          label="Rancher host name"
          hint="Resolves to this cluster's API address, where its ingress controller answers."
          error={shown("rancherHostname")}
        >
          {(id) => (
            <input
              id={id}
              value={form.rancherHostname}
              onChange={(e) => set("rancherHostname", e.target.value.trim())}
            />
          )}
        </Field>
      ) : (
        <>
          <label className="check">
            <input
              type="checkbox"
              checked={form.rancherRegister}
              aria-describedby="register-hint"
              onChange={(e) => set("rancherRegister", e.target.checked)}
            />{" "}
            Register with Rancher
          </label>
          <p className="hint" id="register-hint">
            {registerHint}
          </p>
        </>
      )}
      {variable(cls, "upgrades") && (
        <Field
          label="Upgrade automatically"
          hint={`Every day from 02:00 for four hours, ${browserZone()} time; the cluster's Options change the window later.`}
        >
          {(id) => (
            <select
              id={id}
              value={form.upgradeMode}
              onChange={(e) => {
                const mode = e.target.value;
                if (oneOf(upgradeModes, mode)) {
                  set("upgradeMode", mode);
                }
              }}
            >
              {modeChoices}
            </select>
          )}
        </Field>
      )}
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
  error,
  onChange,
}: {
  label: string;
  value: Machine;
  error?: string | undefined;
  onChange: (m: Machine) => void;
}): JSX.Element {
  // fieldProblems names the first of cores, memory and disk that is wrong.
  const on = (part: string): string | undefined => (error?.includes(part) === true ? error : undefined);
  return (
    <fieldset className="group">
      <legend>{label}</legend>
      <Field label="Cores" error={on("cores")}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={1}
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
            min={minMemoryMiB}
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
            min={1}
            value={shownNumber(value.diskGiB)}
            onChange={(e) => onChange({ ...value, diskGiB: typedNumber(e.target.value) })}
          />
        )}
      </Field>
    </fieldset>
  );
}
