import { Fragment, useEffect, useRef, useState, type JSX } from "react";

import { AddOnsStep } from "./AddOnsStep";
import { api, bridgesOf } from "./api";
import { clusterPath } from "./ClusterDetail";
import {
  classBridges,
  classStorages,
  fieldProblems,
  newForm,
  offeredBridges,
  offeredStorages,
  problems,
  request,
  steps,
  summary,
  variable,
  withClass,
  withRoom,
  type FieldKey,
  type Form,
  type Step,
} from "./create-form";
import { GeneralStep, IdentityStep, MachinesStep, NetworkStep } from "./CreateSteps";
import { offersHA, useHARules } from "./Placement";
import { navigate } from "./router";
import type { Bridge, ClusterClass, PveNode, Storage, Versions } from "./types";
import { Dialog, ErrorBox, errorMessage, filled, sole } from "./ui";

// The choices that can ask more of the machines, which then grow to it.
const raising = new Set<keyof Form>([
  "controlPlaneReplicas",
  "workerReplicas",
  "headlamp",
  "monitoring",
  "podSecurity",
  "istio",
  "rancherInstall",
  "rancherRegister",
]);

export function CreateCluster({ canTrust }: { canTrust?: boolean | undefined }): JSX.Element {
  const [classes, setClasses] = useState<ClusterClass[]>([]);
  const [discarding, setDiscarding] = useState(false);
  const [bridgesByNode, setBridgesByNode] = useState<Record<string, Bridge[]>>({});
  const [storages, setStorages] = useState<Storage[]>([]);
  const [nodes, setNodes] = useState<PveNode[]>([]);
  const rules = useHARules();
  const [step, setStep] = useState<Step>("General");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
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
  const offered = cls ? versionsBy[cls.distribution] : undefined;

  useEffect(() => {
    const d = cls?.distribution;
    if (!filled(d)) {
      return;
    }
    const store = (v: Versions): void => {
      setVersionsBy((all) => ({ ...all, [d]: v }));
      // A distribution's stable release, or kubeadm's newest template, unless one was chosen.
      const first = v.stable ?? v.versions[0];
      if (filled(first)) {
        setEntered((f) => (f.version ? f : { ...f, version: first }));
      }
    };
    async function loadVersions(distribution: string): Promise<void> {
      try {
        store(await api.versions(distribution));
      } catch (err) {
        store({ versions: [], error: errorMessage(err) });
      }
    }
    void loadVersions(d);
  }, [cls?.distribution]);

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
    void loadClasses();
    void loadStorages();
    void loadNodes();
  }, []);

  // Without Sys.Audit on a node Proxmox VE lists none of its bridges, and with nothing listed the
  // bridge is typed in.
  const bridgeOptions = offeredBridges(bridgesByNode, classBridges(cls));
  const usableStorages = offeredStorages(storages, classStorages(cls));
  // Where there is only one bridge or storage to choose, it is chosen until another is.
  const form: Form = {
    ...entered,
    bridge: filled(entered.bridge)
      ? entered.bridge
      : (sole(bridgeOptions.filter((b) => b.usable).map((b) => b.bridge)) ?? ""),
    storage: filled(entered.storage) ? entered.storage : (sole(usableStorages.map((s) => s.storage)) ?? ""),
  };
  const haOffered = offersHA(cls, usableStorages, form.storage);

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

  const index = steps.indexOf(step);
  const previous = steps[index - 1];
  const following = steps[index + 1];
  const byField = fieldProblems(step, form, cls);
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
    try {
      const c = await api.create(request(form, cls));
      navigate(clusterPath(c.namespace, c.name));
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
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
              onChooseClass={chooseClass}
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
              nodes={nodes}
              rules={rules}
            />
          )}
          {step === "Add-ons" && <AddOnsStep form={form} set={set} shown={shown} cls={cls} canTrust={canTrust} />}
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
                {summary(form, cls, haOffered).map(([k, v]) => (
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
