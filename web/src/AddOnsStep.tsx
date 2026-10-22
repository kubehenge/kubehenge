import type { JSX } from "react";

import { MeshPeersField, meshHint } from "./ClusterMesh";
import { modeChoices, upgradeModes } from "./ClusterUpgrades";
import { addonMemory, logsChosen, machineLeast, offersMesh, rancherMinimumCores, variable } from "./create-form";
import type { StepProps } from "./CreateSteps";
import { LogsFields, logsHint, logsNeedMonitoring } from "./Logs";
import { PodSecurityFields, cisHint } from "./PodSecurity";
import type { ClusterClass } from "./types";
import { Field, browserZone, istioHint, registerHint } from "./ui";
import { oneOf } from "./values";

// LogsAddOn keeps logs, offered only beside monitoring, whose Grafana reads them.
function LogsAddOn({ form, set }: Pick<StepProps, "form" | "set">): JSX.Element {
  return (
    <>
      <label className="check">
        <input
          type="checkbox"
          checked={logsChosen(form)}
          disabled={!form.monitoring}
          aria-describedby="logs-hint"
          onChange={(e) => set("logs", { ...form.logs, enabled: e.target.checked })}
        />{" "}
        Keep logs
      </label>
      <p className="hint" id="logs-hint">
        {logsHint}
        {!form.monitoring && ` ${logsNeedMonitoring}`}
      </p>
      {logsChosen(form) && (
        <fieldset className="group">
          <legend>Logs</legend>
          <LogsFields value={form.logs} onChange={(l) => set("logs", l)} />
        </fieldset>
      )}
    </>
  );
}

export function AddOnsStep({
  form,
  set,
  shown,
  cls,
  canTrust,
  namespace,
}: StepProps & {
  cls: ClusterClass | undefined;
  canTrust: boolean | undefined;
  namespace: string | undefined;
}): JSX.Element {
  const used = addonMemory(form, cls);
  const carrier = form.workerReplicas > 0 ? "workerMachine" : "controlPlaneMachine";
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
          checked={form.monitoring}
          aria-describedby="monitoring-hint"
          onChange={(e) => set("monitoring", e.target.checked)}
        />{" "}
        Install monitoring
      </label>
      <p className="hint" id="monitoring-hint">
        Prometheus, Alertmanager and Grafana, with dashboards and alerts for every node and control-plane component.
        Grafana&apos;s admin password is in the kube-prometheus-stack-grafana Secret in the monitoring namespace.
      </p>
      {form.monitoring && (
        <Field
          label="Grafana host name"
          hint="Optional: serve Grafana at this name through the cluster's ingress controller."
        >
          {(id) => (
            <input
              id={id}
              value={form.grafanaHostname}
              onChange={(e) => set("grafanaHostname", e.target.value.trim())}
            />
          )}
        </Field>
      )}
      {variable(cls, "logs") && <LogsAddOn form={form} set={set} />}
      {variable(cls, "cis") && (
        <>
          <label className="check">
            <input
              type="checkbox"
              checked={form.cis}
              aria-describedby="cis-hint"
              onChange={(e) => set("cis", e.target.checked)}
            />{" "}
            Harden to the CIS Kubernetes Benchmark
          </label>
          <p className="hint" id="cis-hint">
            {cisHint}
          </p>
        </>
      )}
      {variable(cls, "podSecurity") && (
        <fieldset className="group">
          <legend>Pod security</legend>
          <PodSecurityFields
            value={form.podSecurity}
            onChange={(p) => set("podSecurity", p)}
            cis={form.cis && variable(cls, "cis") !== undefined}
          />
        </fieldset>
      )}
      {variable(cls, "istio") && (
        <>
          <label className="check">
            <input
              type="checkbox"
              checked={form.istio}
              aria-describedby="istio-hint"
              onChange={(e) => set("istio", e.target.checked)}
            />{" "}
            Install Istio
          </label>
          <p className="hint" id="istio-hint">
            {istioHint}
          </p>
        </>
      )}
      {offersMesh(form, cls) && (
        <>
          <label className="check">
            <input
              type="checkbox"
              checked={form.clusterMesh}
              aria-describedby="mesh-hint"
              onChange={(e) => set("clusterMesh", e.target.checked)}
            />{" "}
            Join a Cluster Mesh
          </label>
          <p className="hint" id="mesh-hint">
            {meshHint}
          </p>
          {form.clusterMesh && namespace !== undefined && (
            <MeshPeersField
              namespace={namespace}
              self={form.name}
              value={form.meshPeers}
              onChange={(p) => set("meshPeers", p)}
            />
          )}
        </>
      )}
      <label className="check">
        <input
          type="checkbox"
          checked={form.rancherInstall}
          disabled={canTrust === false && !form.rancherInstall}
          aria-describedby="rancher-hint"
          onChange={(e) => set("rancherInstall", e.target.checked)}
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
      {used.mib > 0 && (
        <p className="hint" id="addon-memory">
          The add-ons chosen use about {used.mib} MiB of memory ({used.names.join(", ")}), so each{" "}
          {form.workerReplicas > 0 ? "worker" : "control plane"} has at least{" "}
          {machineLeast(form, cls, carrier).memoryMiB} MiB.
        </p>
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
