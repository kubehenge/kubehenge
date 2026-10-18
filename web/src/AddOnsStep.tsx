import type { JSX } from "react";

import { modeChoices, upgradeModes } from "./ClusterUpgrades";
import { rancherMinimumCores, variable } from "./create-form";
import type { StepProps } from "./CreateSteps";
import { PodSecurityFields } from "./PodSecurity";
import type { ClusterClass } from "./types";
import { Field, browserZone, registerHint } from "./ui";
import { oneOf } from "./values";

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
      {variable(cls, "podSecurity") && (
        <fieldset className="group">
          <legend>Pod security</legend>
          <PodSecurityFields value={form.podSecurity} onChange={(p) => set("podSecurity", p)} />
        </fieldset>
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
