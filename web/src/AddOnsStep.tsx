import type { JSX } from "react";

import {
  BackupFieldset,
  BackupSourcesField,
  backupCredentialHint,
  backupKeys,
  backupsHint,
  classTargets,
} from "./Backups";
import { CertificatesFields } from "./Certificates";
import { MeshPeersField, meshHint } from "./ClusterMesh";
import { ModeSelect } from "./ClusterUpgrades";
import {
  addonCPU,
  addonMemory,
  addonsProblem,
  logsChosen,
  machineLeast,
  mostWorkers,
  offersMesh,
  rancherMinimumCores,
  variable,
} from "./create-form";
import type { StepProps } from "./CreateSteps";
import { GitOpsFields, gitOpsFields, gitOpsTrustHint } from "./GitOps";
import { LogsFields, logsHint, logsNeedMonitoring } from "./Logs";
import { PodSecurityFields, cisHint } from "./PodSecurity";
import type { ClusterClass } from "./types";
import {
  Field,
  browserZone,
  filled,
  headlampHint,
  headlampViewHint,
  headlampViewLabel,
  istioHint,
  registerHint,
} from "./ui";

// Talos nodes take their updates as Talos patch releases, upgraded in place, where the others move
// to new builds of their VM template.
function upgradesHint(cls: ClusterClass | undefined): string {
  const window = `Every day from 02:00 for four hours, ${browserZone()} time; the cluster's Options change the window later.`;
  return cls?.distribution === "talos"
    ? `${window} The nodes are upgraded to new Talos patch releases of their minor version in the same window, whatever this says.`
    : `${window} The nodes move to new builds of their VM template in the same window, whatever this says.`;
}

// HeadlampAddOn installs Headlamp, at a host name of its own if one is given, and read-only for
// everyone if asked.
function HeadlampAddOn({ form, set }: Pick<StepProps, "form" | "set">): JSX.Element {
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
        {headlampHint}
      </p>
      {form.headlamp && (
        <fieldset className="group">
          <legend>Headlamp</legend>
          <Field
            label="Host name"
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
          <label className="check">
            <input
              type="checkbox"
              checked={form.headlampView}
              aria-describedby="headlamp-view-hint"
              onChange={(e) => set("headlampView", e.target.checked)}
            />{" "}
            {headlampViewLabel}
          </label>
          <p className="hint" id="headlamp-view-hint">
            {headlampViewHint}
          </p>
        </fieldset>
      )}
    </>
  );
}

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

// BackupsChoice switches a new cluster's backups on, with their settings and the access key it
// gets. A key of its own is stored once the cluster exists, which needs Sys.Modify on the pool.
function BackupsChoice({
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
  const targets = classTargets(cls);
  const none = targets.length === 0;
  const own = form.storeBackupKey && canTrust !== false;
  return (
    <>
      <label className="check">
        <input
          type="checkbox"
          checked={form.backups}
          disabled={none && !form.backups}
          aria-describedby="backups-hint"
          onChange={(e) => set("backups", e.target.checked)}
        />{" "}
        Back up with Velero
      </label>
      <p className="hint" id="backups-hint">
        {none
          ? "An administrator has set no backup target yet; they add one in the appliance's settings."
          : backupsHint}
      </p>
      {form.backups && (
        <>
          <BackupFieldset
            value={form.backupFields}
            onChange={(f) => set("backupFields", f)}
            targets={targets}
            problems={{
              target: shown("backupTarget"),
              schedule: shown("backupSchedule"),
              retention: shown("backupRetention"),
            }}
          />
          {namespace !== undefined && filled(form.backupFields.target) && (
            <BackupSourcesField
              namespace={namespace}
              self={form.name}
              target={form.backupFields.target}
              value={form.backupFields.readFrom}
              creating
              onChange={(readFrom) => set("backupFields", { ...form.backupFields, readFrom })}
            />
          )}
          <fieldset className="group">
            <legend>Access key</legend>
            <p className="hint">{backupCredentialHint}</p>
            {canTrust === false && (
              <p className="hint">
                A cluster&apos;s own key reaches as far as its admin kubeconfig, so storing one needs Sys.Modify on the
                workload pool. Backups wait for it.
              </p>
            )}
            {own &&
              backupKeys.map((k, i) => (
                <Field key={k.key} label={k.label} error={i === backupKeys.length - 1 ? shown("backupKey") : undefined}>
                  {(id) => (
                    <input
                      id={id}
                      type={k.plain === true ? "text" : "password"}
                      autoComplete={k.plain === true ? "off" : "new-password"}
                      spellCheck={false}
                      value={form.backupKey[k.key] ?? ""}
                      onChange={(e) => set("backupKey", { ...form.backupKey, [k.key]: e.target.value })}
                    />
                  )}
                </Field>
              ))}
          </fieldset>
        </>
      )}
    </>
  );
}

// A credential of the new cluster's reaches as far into what it runs as its admin kubeconfig,
// which Sys.Modify on its pool grants.
const tokenRefusal =
  "Storing it needs Sys.Modify on the workload pool, which you do not hold; someone who does stores it under the cluster's Options once it is created.";

// CertificatesAddOn sets the certificates up, with the DNS provider's credential typed in here for
// the new cluster.
function CertificatesAddOn({
  form,
  set,
  canTrust,
  showAll = false,
}: Pick<StepProps, "form" | "set"> & { canTrust: boolean | undefined; showAll: boolean | undefined }): JSX.Element {
  return (
    <fieldset className="group">
      <legend>Certificates</legend>
      <CertificatesFields
        value={form.certificates}
        onChange={(f) => set("certificates", f)}
        showAll={showAll}
        mayShare={canTrust !== false}
        shareRefusal="You do not hold it on the workload pool."
        renderCredential={(label, hint) => (
          <Field
            label={label}
            hint={
              <>
                {hint}{" "}
                {canTrust === false
                  ? tokenRefusal
                  : "Empty: store it later under the cluster's Options; until then the cluster waits for it."}
              </>
            }
          >
            {(id) => (
              <input
                id={id}
                type="password"
                autoComplete="new-password"
                spellCheck={false}
                disabled={canTrust === false}
                value={form.dns01Token}
                onChange={(e) => set("dns01Token", e.target.value)}
              />
            )}
          </Field>
        )}
      />
    </fieldset>
  );
}

// GitOpsChoices are the wizard's GitOps settings. The credentials they name are stored once the
// cluster exists, in its options.
function GitOpsChoices({ form, set, shown, canTrust }: StepProps & { canTrust: boolean | undefined }): JSX.Element {
  return (
    <>
      <GitOpsFields
        value={form.gitops}
        onChange={(g) => set("gitops", g)}
        problems={Object.fromEntries(gitOpsFields.map((f) => [f, shown(`gitops.${f}`)]))}
        locked={canTrust === false && form.gitops.engine === ""}
      />
      {canTrust === false && <p className="hint">{gitOpsTrustHint}</p>}
      {form.gitops.engine !== "" && form.gitops.clientSecret === "stored" && (
        <p className="hint">
          Store the client secret in the cluster&apos;s GitOps options once it is created; until then{" "}
          {form.gitops.engine === "flux"
            ? "Flux's web interface is not served."
            : "Argo CD signs in without one, with PKCE."}
        </p>
      )}
    </>
  );
}

function RancherAddOn({ form, set, shown, canTrust }: StepProps & { canTrust: boolean | undefined }): JSX.Element {
  return (
    <>
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
    </>
  );
}

/**
 * AddonRoom says what keeps the add-ons chosen from running, or else how much memory and CPU they
 * take from the machines that carry them.
 */
function AddonRoom({ form, cls }: { form: StepProps["form"]; cls: ClusterClass | undefined }): JSX.Element | null {
  const stranded = addonsProblem(form, cls);
  if (stranded !== undefined) {
    return (
      <p className="field-error" role="alert">
        {stranded}
      </p>
    );
  }
  const used = addonMemory(form, cls);
  const requested = addonCPU(form, cls);
  if (used.mib === 0 && requested.millicores === 0) {
    return null;
  }
  const least = machineLeast(form, cls, form.workerReplicas > 0 ? "workerMachine" : "controlPlaneMachine");
  const node = form.workerReplicas > 0 ? "worker" : "control plane";
  const sentences: string[] = [];
  if (used.mib > 0) {
    const controlPlanes =
      mostWorkers(form) > 0
        ? ` and each control plane, whose API server holds their objects, at least ${machineLeast(form, cls, "controlPlaneMachine").memoryMiB} MiB`
        : "";
    sentences.push(
      `The add-ons chosen use about ${used.mib} MiB of memory (${used.names.join(", ")}), so each ${node} has at least ${least.memoryMiB} MiB${controlPlanes}.`,
    );
  }
  if (requested.millicores > 0) {
    const cores = form.rancherInstall ? Math.max(least.cores, rancherMinimumCores) : least.cores;
    sentences.push(
      `Their pods ask for ${requested.millicores} millicores of CPU (${requested.names.join(", ")}), so each ${node} has at least ${cores} cores.`,
    );
  }
  return (
    <p className="hint" id="addon-room">
      {sentences.join(" ")}
    </p>
  );
}

export function AddOnsStep({
  form,
  set,
  shown,
  cls,
  canTrust,
  namespace,
  showAll,
}: StepProps & {
  cls: ClusterClass | undefined;
  canTrust: boolean | undefined;
  namespace: string | undefined;
  showAll?: boolean;
}): JSX.Element {
  return (
    <>
      <HeadlampAddOn form={form} set={set} />
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
      <RancherAddOn form={form} set={set} shown={shown} canTrust={canTrust} />
      {variable(cls, "backups") && (
        <BackupsChoice form={form} set={set} shown={shown} cls={cls} canTrust={canTrust} namespace={namespace} />
      )}
      {variable(cls, "certificates") && (
        <CertificatesAddOn form={form} set={set} canTrust={canTrust} showAll={showAll} />
      )}
      {variable(cls, "gitops") && <GitOpsChoices form={form} set={set} shown={shown} canTrust={canTrust} />}
      <AddonRoom form={form} cls={cls} />

      {variable(cls, "upgrades") && (
        <Field label="Upgrade automatically" hint={upgradesHint(cls)}>
          {(id) => <ModeSelect id={id} value={form.upgradeMode} onChange={(m) => set("upgradeMode", m)} />}
        </Field>
      )}
    </>
  );
}
