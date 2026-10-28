import { Fragment, useEffect, useRef, useState, type JSX } from "react";

import { AddOnsStep } from "./AddOnsStep";
import { api, bridgesOf } from "./api";
import { clusterPath } from "./ClusterDetail";
import {
  classBridges,
  classStorages,
  csiOffer,
  csiProblems,
  fieldProblems,
  snapshotsOffer,
  snapshotsProblems,
  newForm,
  offeredBridges,
  offeredStorages,
  pendingCredentials,
  problems,
  request,
  steps,
  summary,
  variable,
  versionsNodeOS,
  withBackupChoices,
  withClass,
  namespaceOf,
  withNamespace,
  withRoom,
  withTalosRelease,
  type FieldKey,
  type Form,
  type Step,
} from "./create-form";
import { GeneralStep, IdentityStep, MachinesStep, NetworkStep, useClassTalos } from "./CreateSteps";
import { offersHA, offersHAGroups, useHARules } from "./Placement";
import { navigate } from "./router";
import type { Bridge, Cluster, ClusterClass, PveNode, Storage, Versions } from "./types";
import { Dialog, ErrorBox, errorMessage, filled, sole, usePrivilege } from "./ui";

// versionsKey is where the versions offered are kept: a distribution's, kubeadm's per node OS.
const versionsKey = (distribution: string, nodeOS: string | undefined): string =>
  nodeOS === undefined ? distribution : `${distribution} ${nodeOS}`;

// The choices that can ask more of the machines, which then grow to it.
const raising = new Set<keyof Form>([
  "controlPlaneReplicas",
  "workerReplicas",
  "workerAutoscale",
  "workerMax",
  "headlamp",
  "monitoring",
  "podSecurity",
  "istio",
  "cni",
  "clusterMesh",
  "loadBalancer",
  "rancherInstall",
  "rancherRegister",
  "backups",
  "certificates",
]);

export function CreateCluster({
  canTrust,
  namespaces,
}: {
  canTrust?: boolean | undefined;
  namespaces: string[];
}): JSX.Element {
  const [classes, setClasses] = useState<ClusterClass[]>([]);
  const [discarding, setDiscarding] = useState(false);
  const [bridgesByNode, setBridgesByNode] = useState<Record<string, Bridge[]>>({});
  const [storages, setStorages] = useState<Storage[]>([]);
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [nodes, setNodes] = useState<PveNode[]>([]);
  const rules = useHARules();
  const canModifyPermissions = usePrivilege("canModifyPermissions");
  const [step, setStep] = useState<Step>("General");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  // The cluster created, while the page stays to tell what of the rest failed.
  const [created, setCreated] = useState<Cluster>();
  const [entered, setEntered] = useState<Form>(newForm);
  // A field's problem shows once the field has been changed, or Next was pressed on its step.
  const [touched, setTouched] = useState<ReadonlySet<string>>(new Set());
  const [attempted, setAttempted] = useState<ReadonlySet<Step>>(new Set());
  const cls = classes.find((c) => c.name === entered.className);
  const set = <K extends keyof Form>(k: K, v: Form[K]): void => {
    setEntered((f) => (raising.has(k) ? withRoom({ ...f, [k]: v }, cls) : { ...f, [k]: v }));
    setTouched((t) => (t.has(k) ? t : new Set([...t, k])));
  };
  const [versionsBy, setVersionsBy] = useState<Record<string, Versions>>({});
  // Kubeadm offers the versions with a template on the node OS chosen.
  const kubeadmOS = versionsNodeOS(entered, cls);
  const offered = cls ? versionsBy[versionsKey(cls.distribution, kubeadmOS)] : undefined;
  const talos = useClassTalos(cls);

  useEffect(() => {
    let current = true;
    const store = (distribution: string, v: Versions): void => {
      if (!current) {
        return;
      }
      setVersionsBy((all) => ({ ...all, [versionsKey(distribution, kubeadmOS)]: v }));
      // A distribution's stable release, or kubeadm's newest template, unless one was chosen; a
      // kubeadm version without a template on the node OS chosen now is not kept.
      const first = v.stable ?? v.versions[0];
      if (filled(first)) {
        setEntered((f) =>
          f.version && (kubeadmOS === undefined || v.versions.includes(f.version)) ? f : { ...f, version: first },
        );
      }
    };
    async function loadVersions(distribution: string): Promise<void> {
      try {
        store(distribution, await api.versions(distribution, kubeadmOS));
      } catch (err) {
        store(distribution, { versions: [], error: errorMessage(err) });
      }
    }
    const d = cls?.distribution;
    if (filled(d)) {
      void loadVersions(d);
    }
    return () => {
      current = false;
    };
  }, [cls?.distribution, kubeadmOS]);

  useEffect(() => {
    async function loadClasses(): Promise<void> {
      try {
        setClasses(await api.classes());
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
    async function loadBridges(node: string): Promise<void> {
      const b = await bridgesOf(node);
      setBridgesByNode((all) => ({ ...all, [node]: b }));
    }
    async function loadNodes(): Promise<void> {
      try {
        const listed = await api.nodes();
        setNodes(listed);
        await Promise.all(listed.filter((x) => x.online).map(async ({ node }) => loadBridges(node)));
      } catch (err) {
        setError(errorMessage(err));
      }
    }
    async function loadClusters(): Promise<void> {
      try {
        setClusters(await api.clusters());
      } catch {
        // The controller refuses a storage another cluster uses in any case; the list only says so sooner.
        setClusters([]);
      }
    }
    void loadClasses();
    void loadStorages();
    void loadNodes();
    void loadClusters();
  }, []);

  // Without Sys.Audit on a node Proxmox VE lists none of its bridges, and with nothing listed the
  // bridge is typed in.
  const bridgeOptions = offeredBridges(bridgesByNode, classBridges(cls));
  const usableStorages = offeredStorages(storages, classStorages(cls));
  // Where there is only one bridge or storage to choose, it is chosen until another is.
  const form: Form = withBackupChoices(
    {
      ...withTalosRelease(entered, talos, offered),
      namespace: namespaces.length > 1 ? namespaceOf(entered, namespaces) : "",
      bridge: filled(entered.bridge)
        ? entered.bridge
        : (sole(bridgeOptions.filter((b) => b.usable).map((b) => b.bridge)) ?? ""),
      storage: filled(entered.storage) ? entered.storage : (sole(usableStorages.map((s) => s.storage)) ?? ""),
    },
    cls,
    canTrust,
  );
  const haOffered = offersHA(cls, usableStorages, form.storage);
  const csi = csiOffer({ cls, storages: usableStorages, storage: form.storage, clusters });
  const snapshots = snapshotsOffer(cls, form.storage, canModifyPermissions);
  const haGroups = offersHAGroups(cls);

  const chooseClass = (name: string): void => {
    const c = classes.find((x) => x.name === name);
    setEntered((f) =>
      withRoom(
        withClass(f, c, (k) => touched.has(k)),
        c,
      ),
    );
    setTouched((t) => new Set([...t, "class"]));
  };
  const chooseNamespace = (namespace: string): void => {
    setEntered((f) => withNamespace(f, namespace, namespaceOf(f, namespaces)));
  };

  const index = steps.indexOf(step);
  const previous = steps[index - 1];
  const following = steps[index + 1];
  const byField = {
    ...fieldProblems(step, form, cls),
    ...csiProblems(step, form, csi),
    ...snapshotsProblems(step, form, snapshots),
  };
  const open = Object.values(byField);
  const shown = (k: FieldKey): string | undefined => (attempted.has(step) || touched.has(k) ? byField[k] : undefined);
  const wizardRef = useRef<HTMLDivElement>(null);

  // Next on an incomplete step shows every problem of it and puts the cursor on the first.
  const next = (): void => {
    if (open.length === 0) {
      if (following !== undefined) {
        setStep(following);
      }
      return;
    }
    setAttempted((a) => new Set([...a, step]));
    requestAnimationFrame(() => wizardRef.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus());
  };

  const create = async (): Promise<void> => {
    if (!cls) {
      return;
    }
    setBusy(true);
    setError(undefined);
    let c: Cluster;
    try {
      c = await api.create(request(form, cls));
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
      return;
    }
    const pending = pendingCredentials(form, cls);
    const stored = await Promise.allSettled(
      pending.map(async (p) => api.storeCredential(p.purpose, p.values, { namespace: c.namespace, name: c.name })),
    );
    const failures = pending.flatMap((p, i) => {
      const s = stored[i];
      return s?.status === "rejected"
        ? [
            {
              ...p,
              message: `its ${p.label} was not stored: ${errorMessage(s.reason)}. Store it under the cluster's Options, ${p.section}.`,
            },
          ]
        : [];
    });
    if (failures.length > 0) {
      // The cluster exists: creating it again would fail, so the page offers only the way to it,
      // and the summary says the cluster waits for what was not stored rather than that it is.
      setCreated(c);
      setEntered((f) => {
        const kept = { ...f };
        for (const p of failures) {
          Object.assign(kept, p.forget);
        }
        return kept;
      });
      setError(`The cluster was created, but ${failures.map((p) => p.message).join(" Also, ")}`);
      setBusy(false);
      return;
    }
    navigate(clusterPath(c.namespace, c.name));
  };

  return (
    <section className="panel">
      <div className="toolbar">
        <h2>Create cluster</h2>
      </div>
      <div className="wizard">
        <nav className="wizard-steps" aria-label="Steps">
          {steps.map((s, i) => (
            <button
              key={s}
              type="button"
              className={s === step ? "tab active" : "tab"}
              aria-current={s === step ? "step" : undefined}
              disabled={i > index && steps.slice(0, i).some((p) => problems(p, form, cls).length > 0)}
              onClick={() => setStep(s)}
            >
              {s}
            </button>
          ))}
        </nav>
        <div className="wizard-body" ref={wizardRef}>
          {step === "General" && (
            <GeneralStep
              form={form}
              set={set}
              shown={shown}
              classes={classes}
              cls={cls}
              offered={offered}
              talos={talos}
              onChooseClass={chooseClass}
              namespaces={namespaces}
              onChooseNamespace={chooseNamespace}
            />
          )}
          {step === "Network" && (
            <NetworkStep form={form} set={set} shown={shown} cls={cls} bridgeOptions={bridgeOptions} />
          )}
          {step === "Machines" && (
            <MachinesStep
              form={form}
              set={set}
              shown={shown}
              cls={cls}
              usableStorages={usableStorages}
              placed={variable(cls, "allowedNodes") !== undefined}
              haOffered={haOffered}
              csi={csi}
              snapshots={snapshots}
              haGroups={haGroups}
              nodes={nodes}
              rules={rules}
            />
          )}
          {step === "Add-ons" && (
            <AddOnsStep
              form={form}
              set={set}
              shown={shown}
              cls={cls}
              canTrust={canTrust}
              namespace={namespaceOf(form, namespaces)}
              showAll={attempted.has("Add-ons")}
            />
          )}
          {step === "Identity providers" && (
            <IdentityStep
              form={form}
              set={set}
              cls={cls}
              canTrust={canTrust}
              showAll={attempted.has("Identity providers")}
            />
          )}
          {step === "Confirm" && cls && (
            <>
              <dl className="summary" aria-label="Summary">
                {summary(form, cls, {
                  ha: haOffered,
                  csi: csi.offered,
                  snapshots: snapshots.offered,
                  haGroups,
                }).map(([k, v]) => (
                  <Fragment key={k}>
                    <dt>{k}</dt>
                    <dd>{v}</dd>
                  </Fragment>
                ))}
              </dl>
              <details className="advanced">
                <summary>Request</summary>
                <pre className="confirm" aria-label="Request">
                  {JSON.stringify(request(form, cls), null, 2)}
                </pre>
              </details>
            </>
          )}

          <ErrorBox message={error} />
          {created === undefined ? (
            <div className="actions">
              <button type="button" onClick={() => (touched.size > 0 ? setDiscarding(true) : navigate("/"))}>
                Cancel
              </button>
              <span className="spacer" />
              <button
                type="button"
                disabled={previous === undefined}
                onClick={() => {
                  if (previous !== undefined) {
                    setStep(previous);
                  }
                }}
              >
                Back
              </button>
              {step === "Confirm" ? (
                <button
                  type="button"
                  className="primary"
                  disabled={busy || !cls}
                  onClick={() => {
                    void create();
                  }}
                >
                  Create
                </button>
              ) : (
                <button type="button" className="primary" onClick={next}>
                  Next
                </button>
              )}
            </div>
          ) : (
            <div className="actions">
              <button
                type="button"
                className="primary"
                onClick={() => navigate(clusterPath(created.namespace, created.name, "options"))}
              >
                Open the cluster
              </button>
            </div>
          )}
        </div>
      </div>
      {discarding && (
        <Dialog
          title="Discard this cluster?"
          onClose={() => setDiscarding(false)}
          onSubmit={() => navigate("/")}
          actions={
            <>
              <button type="button" onClick={() => setDiscarding(false)}>
                Keep editing
              </button>
              <button type="submit" className="danger">
                Discard
              </button>
            </>
          }
        >
          <p>What was entered here is lost; nothing has been created yet.</p>
        </Dialog>
      )}
    </section>
  );
}
