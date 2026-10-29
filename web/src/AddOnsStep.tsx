import type { JSX } from "react";

import { BackupFieldset, backupKeys, backupsHint, classTargets, noTargets } from "./Backups";
import { CertificatesFields } from "./Certificates";
import { MeshPeersField, meshInfo } from "./ClusterMesh";
import { ModeSelect } from "./ClusterUpgrades";
import {
  addonCPU,
  addonMemory,
  addonsProblem,
  logsChosen,
  machineLeast,
  offersMesh,
  rancherMinimumCores,
  variable,
} from "./create-form";
import type { StepProps } from "./CreateSteps";
import { GitOpsFields, gitOpsFields, gitOpsTrustHint } from "./GitOps";
import { LogsFields, logsHint, logsNeedMonitoring } from "./Logs";
import { PodSecurityFields, cisHint, cisInfo } from "./PodSecurity";
import type { ClusterClass } from "./types";
import {
  Field,
  Info,
  browserZone,
  headlampHint,
  headlampViewHint,
  headlampViewLabel,
  istioHint,
  registerHint,
} from "./ui";

// Talos nodes take Talos patch releases in place and the others move to new builds of their VM
// template, both in the window whatever the mode.
const upgradesInfo = "Nodes also get OS updates in this window, whatever the mode.";

// HeadlampAddOn installs Headlamp, at a host name of its own if one is given, and read-only for
// everyone if asked.
function HeadlampAddOn({ form, set }: Pick<StepProps, "form" | "set">): JSX.Element {
  return (
    <>
      <label className="check">
        <input type="checkbox" checked={form.headlamp} onChange={(e) => set("headlamp", e.target.checked)} /> Install
        Headlamp
      </label>
      <Info about="Headlamp">{headlampHint}</Info>
      {form.headlamp && (
        <fieldset className="group">
          <legend>Headlamp</legend>
          <Field label="Host name" hint="Optional">
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
          aria-describedby={form.monitoring ? undefined : "logs-hint"}
          onChange={(e) => set("logs", { ...form.logs, enabled: e.target.checked })}
        />{" "}
        Keep logs
      </label>
      <Info about="logs">{logsHint}</Info>
      {!form.monitoring && (
        <p className="hint" id="logs-hint">
          {logsNeedMonitoring}
        </p>
      )}
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
}: StepProps & { cls: ClusterClass | undefined; canTrust: boolean | undefined }): JSX.Element {
  const targets = classTargets(cls);
  const none = targets.length === 0;
  const own = form.backupOwnKey && canTrust !== false;
  return (
    <>
      <label className="check">
        <input
          type="checkbox"
          checked={form.backups}
          disabled={none && !form.backups}
          aria-describedby={none ? "backups-hint" : undefined}
          onChange={(e) => set("backups", e.target.checked)}
        />{" "}
        Back up with Velero
      </label>
      <Info about="backups">{backupsHint}</Info>
      {none && (
        <p className="hint" id="backups-hint">
          {noTargets}
        </p>
      )}
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
          <fieldset className="group">
            <legend>Access key</legend>
            <label className="check">
              <input
                type="radio"
                name="backup-key"
                checked={own}
                disabled={canTrust === false}
                onChange={() => set("backupOwnKey", true)}
              />{" "}
              Cluster&apos;s own key
            </label>
            {canTrust === false && <p className="hint">Needs Sys.Modify on the workload pool</p>}
            <label className="check">
              <input type="radio" name="backup-key" checked={!own} onChange={() => set("backupOwnKey", false)} /> Shared
              key
            </label>
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
const tokenRefusal = "Needs Sys.Modify on the workload pool";

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
        shareRefusal="You don't hold it"
        renderCredential={(label, hint) => (
          <Field
            label={label}
            hint={canTrust === false ? tokenRefusal : hint}
            info={canTrust === false ? undefined : "Add it later in Options. The cluster waits for it."}
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
          {form.gitops.engine === "flux"
            ? "Set the secret in Options to turn on Flux's interface"
            : "Argo CD uses PKCE until the secret is in Options"}
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
          aria-describedby={canTrust === false ? "rancher-hint" : undefined}
          onChange={(e) => set("rancherInstall", e.target.checked)}
        />{" "}
        Install Rancher
      </label>
      <Info about="Rancher">
        Other clusters register with it, and it holds their admin credentials. Needs {rancherMinimumCores} cores per
        node.
      </Info>
      {canTrust === false && (
        <p className="hint" id="rancher-hint">
          Needs Sys.Modify on the workload pool
        </p>
      )}
      {form.rancherInstall ? (
        <Field label="Rancher host name" hint="Must resolve to the API address" error={shown("rancherHostname")}>
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
              onChange={(e) => set("rancherRegister", e.target.checked)}
            />{" "}
            Register with Rancher
          </label>
          <Info about="registering with Rancher">{registerHint}</Info>
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
  const cores = form.rancherInstall ? Math.max(least.cores, rancherMinimumCores) : least.cores;
  const amounts = [
    ...(used.mib > 0 ? [`${used.mib} MiB of memory`] : []),
    ...(requested.millicores > 0 ? [`${requested.millicores} millicores of CPU`] : []),
  ];
  const names = [...new Set([...used.names, ...requested.names])];
  return (
    <p className="hint" id="addon-room">
      {`Each ${node} needs at least ${least.memoryMiB} MiB and ${cores} cores`}
      <Info about="add-on resources">{`Add-ons use about ${amounts.join(" and ")}: ${names.join(", ")}.`}</Info>
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
        <input type="checkbox" checked={form.monitoring} onChange={(e) => set("monitoring", e.target.checked)} />{" "}
        Install monitoring
      </label>
      <Info about="monitoring">
        Prometheus, Alertmanager and Grafana. Grafana&apos;s admin password is in Secret
        monitoring/kube-prometheus-stack-grafana.
      </Info>
      {form.monitoring && (
        <Field label="Grafana host name" hint="Optional">
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
            CIS hardening
          </label>
          <Info about="CIS hardening">{cisInfo}</Info>
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
            <input type="checkbox" checked={form.istio} onChange={(e) => set("istio", e.target.checked)} /> Install
            Istio
          </label>
          <Info about="Istio">{istioHint}</Info>
        </>
      )}
      {offersMesh(form, cls) && (
        <>
          <label className="check">
            <input type="checkbox" checked={form.clusterMesh} onChange={(e) => set("clusterMesh", e.target.checked)} />{" "}
            Join a Cluster Mesh
          </label>
          <Info about="Cluster Mesh">{meshInfo}</Info>
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
      {variable(cls, "backups") && <BackupsChoice form={form} set={set} shown={shown} cls={cls} canTrust={canTrust} />}
      {variable(cls, "certificates") && (
        <CertificatesAddOn form={form} set={set} canTrust={canTrust} showAll={showAll} />
      )}
      {variable(cls, "gitops") && <GitOpsChoices form={form} set={set} shown={shown} canTrust={canTrust} />}
      <AddonRoom form={form} cls={cls} />

      {variable(cls, "upgrades") && (
        <Field label="Upgrade automatically" hint={`Daily 02:00 to 06:00, ${browserZone()} time`} info={upgradesInfo}>
          {(id) => <ModeSelect id={id} value={form.upgradeMode} onChange={(m) => set("upgradeMode", m)} />}
        </Field>
      )}
    </>
  );
}
