import { useEffect, useRef, useState, type JSX } from "react";

import { AddOnsStep, PoliciesStep } from "./AddOnsStep";
import { api, bridgesOf, dnsOf } from "./api";
import { clusterPath } from "./cluster-paths";
import {
  bridgeMtuProblems,
  bridgeNodeProblems,
  gatewayProblems,
  classBridges,
  classStorages,
  createRoom,
  spreadOver,
  csiOffer,
  capacityProblems,
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
  addressesOf,
  bridgeNetwork,
  bridgeOf,
  nodesNeeded,
  withBackupChoices,
  withBridgeNetwork,
  withFreeAddresses,
  withClass,
  namespaceOf,
  withNamespace,
  withRoom,
  withTalosRelease,
  type FieldKey,
  type Form,
  type Step,
} from "./create-form";
import { ConfirmStep, useCreateCheck } from "./CreateCheck";
import { GeneralStep, IdentityStep, MachinesStep, NetworkStep, useClassTalos, useFreeAddresses } from "./CreateSteps";
import { useDatacenterFirewall } from "./Firewall";
import { useFleet } from "./fleet";
import { offersHA, offersHAGroups, useHARules } from "./Placement";
import { navigate } from "./router";
import { clusterKey, fromCluster, startable, startKeys } from "./start-from";
import type { Bridge, Cluster, ClusterClass, NodeDNS, PveNode, Storage, Versions } from "./types";
import { CopiedFrom, Detail, Dialog, ErrorBox, errorMessage, filled, sole } from "./ui";
import { record } from "./values";

// customisationRefusal is the server's error where it names the node customisation, which then shows with it.
const customisationRefusal = (error: string | undefined): string | undefined =>
  error !== undefined && /node customisation|nodeCustomisation/iu.test(error) ? error : undefined;

// checkRefusal is the node customisation's refusal, from a create that failed or from the last check
// of the form as it stands now.
const checkRefusal = (
  error: string | undefined,
  checked: ReturnType<typeof useCreateCheck>,
  of: { form: Parameters<typeof request>[0]; cls: Parameters<typeof request>[1] | undefined },
): string | undefined =>
  customisationRefusal(error ?? (of.cls ? checked.refusalOf(request(of.form, of.cls)) : undefined));

// versionsKey is where the versions offered are kept: a distribution's, kubeadm's per node OS.
const versionsKey = (distribution: string, nodeOS: string | undefined): string =>
  nodeOS === undefined ? distribution : `${distribution} ${nodeOS}`;

const noNetwork: ReturnType<typeof bridgeNetwork> = {};

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
  "existingCertManager",
]);

const haControlPlanes = 3;

// copiedLabels are the labels of the fields whose values a form copies from the cluster it started
// from, by the form key. Those that the cluster left at the wizard's own value are not marked.
const copiedLabels: Partial<Record<keyof Form, string>> = {
  nodeOS: "Node OS",
  controlPlaneReplicas: "Control-plane nodes",
  workerReplicas: "Workers",
  bridge: "Bridge",
  vlan: "VLAN tag",
  mtu: "MTU",
  queues: "Queues",
  prefix: "Prefix length",
  gateway: "Gateway",
  dnsServers: "DNS servers",
  searchDomains: "Search domains",
  ntpServers: "Time servers",
  storage: "Storage",
  cni: "Pod network",
  loadBalancer: "Load balancer",
};

// copiedFields lists the labels of the copied values that differ from a new form's.
function copiedFields(form: Form, from: Cluster | undefined): string[] {
  if (from === undefined) {
    return [];
  }
  const fresh = newForm();
  return Object.entries(copiedLabels).flatMap(([key, label]) =>
    isCopiedKey(key) && startKeys.has(key) && JSON.stringify(form[key]) !== JSON.stringify(fresh[key]) ? [label] : [],
  );
}

// storageOf is the storage entered, or else the only one offered.
function storageOf(entered: Form, usable: Storage[]): string {
  return filled(entered.storage) ? entered.storage : (sole(usable.map((s) => s.storage)) ?? "");
}

// lacksSharedKey tells whether the namespace the cluster goes to, or else the user's own, is known
// to hold no access key for every cluster.
function lacksSharedKey(
  sharedKeys: Record<string, boolean>,
  namespace: string,
  sessionNamespace: string | undefined,
): boolean {
  return sharedKeys[namespace || (sessionNamespace ?? "")] === false;
}

// copiedContext is what the steps say was copied from the cluster the form started from, if any.
function copiedContext(form: Form, from: Cluster | undefined): { from: string; labels: string[] } | undefined {
  return from === undefined ? undefined : { from: from.name, labels: copiedFields(form, from) };
}

// isCopiedKey narrows a key of copiedLabels, which Object.entries gives as a string, back to the
// form field it labels.
function isCopiedKey(key: string): key is keyof Form {
  return Object.hasOwn(copiedLabels, key);
}

// createLabelOf is Confirm's create button: it says so when the cluster waits for memory, or when
// its disks may exceed the free space.
function createLabelOf(room: ReturnType<typeof createRoom>): string {
  if (room.nodeWarning !== undefined) {
    return "Create and wait";
  }
  return room.diskWarning === undefined ? "Create" : "Create anyway";
}

// controlPlanesFor is three control planes where the cluster has three online nodes it may use to
// spread them over, unless the user or the cluster started from chose a count.
function controlPlanesFor(entered: Form, nodes: PveNode[], kept: boolean): number {
  if (kept || spreadOver(nodes, entered) < haControlPlanes) {
    return entered.controlPlaneReplicas;
  }
  return haControlPlanes;
}

// CreateButton is Confirm's primary button. It says so when the cluster waits for memory, or when
// its disks exceed the free space of storage that may take them.
function CreateButton({
  blocked,
  label,
  onCreate,
}: {
  blocked: boolean;
  label: string;
  onCreate: () => void;
}): JSX.Element {
  return (
    <button type="button" className="primary" disabled={blocked} onClick={onCreate}>
      {label}
    </button>
  );
}

export function CreateCluster({
  canTrust,
  namespaces,
}: {
  canTrust?: boolean | undefined;
  namespaces: string[];
}): JSX.Element {
  const fleet = useFleet();
  const [classes, setClasses] = useState<ClusterClass[]>([]);
  const [discarding, setDiscarding] = useState(false);
  const [bridgesByNode, setBridgesByNode] = useState<Record<string, Bridge[]>>({});
  const [dnsByNode, setDNSByNode] = useState<Record<string, NodeDNS>>({});
  const [storages, setStorages] = useState<Storage[]>([]);
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [nodes, setNodes] = useState<PveNode[]>([]);
  // Whether each namespace holds the access key for every cluster.
  const [sharedKeys, setSharedKeys] = useState<Record<string, boolean>>({});
  const [sessionNamespace, setSessionNamespace] = useState<string>();
  const rules = useHARules();
  const datacenterFirewall = useDatacenterFirewall();
  const [step, setStep] = useState<Step>("General");
  const [error, setError] = useState<string>();
  // The server's own words for the credentials that were not stored, behind the error's Detail.
  const [reasons, setReasons] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  // The cluster created, while the page stays to tell what of the rest failed.
  const [created, setCreated] = useState<Cluster>();
  const [entered, setEntered] = useState<Form>(newForm);
  // A field's problem shows once the field has been changed, or Next was pressed on its step.
  const [touched, setTouched] = useState<ReadonlySet<string>>(new Set());
  const [attempted, setAttempted] = useState<ReadonlySet<Step>>(new Set());
  const cls = classes.find((c) => c.name === entered.className);
  // What the user set, and what the cluster started from set, stays when a default would replace it.
  const kept = (k: keyof Form): boolean => touched.has(k) || (filled(entered.from) && startKeys.has(k));
  // The gateway the free addresses were looked for on before the user typed their own, so that a
  // gateway typed over it moves no address the user left alone.
  const [gatewayBase, setGatewayBase] = useState<string>();
  const set = <K extends keyof Form>(k: K, v: Form[K]): void => {
    if (k === "gateway") {
      setGatewayBase((b) => b ?? chosen.gateway);
    } else if (k === "bridge") {
      setGatewayBase(undefined);
    }
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
      // A distribution's stable release, or kubeadm's newest template, unless one was chosen. A
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
      const [b, d] = await Promise.all([bridgesOf(node), dnsOf(node)]);
      setBridgesByNode((all) => ({ ...all, [node]: b }));
      setDNSByNode((all) => ({ ...all, [node]: d }));
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
        // The controller refuses a storage another cluster uses in any case. The list only says so sooner.
        setClusters([]);
      }
    }
    async function loadSharedKeys(): Promise<void> {
      try {
        const s = await api.session();
        setSharedKeys({ ...s.sharedBackupKeys });
        setSessionNamespace(s.namespace);
      } catch {
        // Unknown counts as stored: the server refuses a cluster that has no key, so nothing is lost.
        setSharedKeys({});
      }
    }
    void loadSharedKeys();
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
  const bridge = bridgeOf(entered.bridge, bridgeOptions, bridgesByNode);
  // A VLAN's network is not the one a node reports on the bridge.
  const { net, network, network6, node } = filled(entered.vlan)
    ? noNetwork
    : bridgeNetwork(bridge, bridgesByNode, dnsByNode);
  // Registering needs a Rancher that one of the clusters runs.
  const rancherExists = clusters.some((x) => record(x.variables["rancher"])["install"] === true);
  const formNamespace = namespaces.length > 1 ? namespaceOf(entered, namespaces) : "";
  const chosen: Form = withBackupChoices(
    {
      ...withBridgeNetwork(withTalosRelease(entered, talos, offered), net, kept),
      namespace: formNamespace,
      taken: clusters.filter((x) => formNamespace === "" || x.namespace === formNamespace).map((x) => x.name),
      rancherExists,
      controlPlaneReplicas: controlPlanesFor(entered, nodes, kept("controlPlaneReplicas")),
      noSharedKey: lacksSharedKey(sharedKeys, namespaceOf(entered, namespaces), sessionNamespace),
      rancherRegister: entered.rancherRegister,
      bridge,
      storage: storageOf(entered, usableStorages),
    },
    cls,
    canTrust,
  );
  const probeOn = {
    node,
    bridge,
    vlan: entered.vlan,
    families: entered.families,
    // A gateway the user mistypes must not move the addresses they left alone to its network.
    gateway: gatewayBase ?? chosen.gateway,
    prefix: chosen.prefix,
    gateway6: chosen.gateway6,
    prefix6: chosen.prefix6,
  };
  const free = useFreeAddresses(probeOn, nodesNeeded(chosen), []);
  const typed = useFreeAddresses(probeOn, 0, addressesOf(entered));
  const form = withFreeAddresses(chosen, free, (k) => touched.has(k));
  const haOffered = offersHA(cls, usableStorages, form.storage);
  const csi = csiOffer({ cls, storages: usableStorages, storage: form.storage, clusters });
  const snapshots = snapshotsOffer(cls, form.storage);
  const haGroups = offersHAGroups(cls);

  const chooseClass = (name: string): void => {
    const c = classes.find((x) => x.name === name);
    setEntered((f) => withRoom(withClass(f, c, kept), c));
    setTouched((t) => new Set([...t, "class"]));
  };
  // Starting from another cluster, or from none, replaces all but the name and namespace.
  const chooseFrom = (key: string): void => {
    setGatewayBase(undefined);
    const c = clusters.find((x) => clusterKey(x) === key);
    setEntered((f) => (c === undefined ? { ...newForm(), name: f.name, namespace: f.namespace } : fromCluster(c, f)));
    setTouched((t) => new Set(["from", ...[...t].filter((k) => k === "name")]));
  };
  const chooseNamespace = (namespace: string): void => {
    setEntered((f) => withNamespace(f, namespace, namespaceOf(f, namespaces)));
  };

  const copiedFrom = clusters.find((x) => clusterKey(x) === entered.from);
  // React Compiler keeps the context's value while its inputs stay.
  const copied = copiedContext(form, copiedFrom);
  const index = steps.indexOf(step);
  const previous = steps[index - 1];
  const following = steps[index + 1];
  const byField = {
    ...fieldProblems(step, form, cls),
    ...bridgeMtuProblems(step, form, bridgeOptions),
    ...gatewayProblems(step, form, net),
    ...bridgeNodeProblems(step, form, {
      bridgesByNode,
      online: nodes.filter((x) => x.online).map((x) => x.node),
    }),
    ...capacityProblems(step, form, { nodes, storages: usableStorages }),
    ...csiProblems(step, form, csi),
    ...snapshotsProblems(step, form, snapshots),
  };
  const open = Object.values(byField);
  // A cluster that fits the nodes but not what they have free now waits for room.
  const room = createRoom(form, nodes, usableStorages);
  const createLabel = createLabelOf(room);
  const refused = Object.keys(capacityProblems("Machines", form, { nodes, storages: usableStorages })).length > 0;
  const shown = (k: FieldKey): string | undefined => (attempted.has(step) || touched.has(k) ? byField[k] : undefined);
  const wizardRef = useRef<HTMLDivElement>(null);

  // Confirm has the API server check the cluster each time it opens: nothing on it changes the form.
  const checked = useCreateCheck();
  const go = (s: Step): void => {
    setStep(s);
    if (s === "Confirm" && cls) {
      checked.run(request(form, cls));
    }
  };

  // Next on an incomplete step shows every problem of it and puts the cursor on the first.
  const next = (): void => {
    if (open.length === 0) {
      if (following !== undefined) {
        go(following);
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
    setReasons([]);
    let c: Cluster;
    try {
      c = await api.create(request(form, cls));
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
      return;
    }
    fleet?.show(c);
    // The access key went with the request.
    const pending = pendingCredentials(form, cls).filter((p) => p.purpose !== "backup");
    const stored = await Promise.allSettled(
      pending.map(async (p) => api.storeCredential(p.purpose, p.values, { namespace: c.namespace, name: c.name })),
    );
    const failures = pending.flatMap((p, i) => {
      const s = stored[i];
      return s?.status === "rejected"
        ? [
            {
              ...p,
              message: `the ${p.label} was not stored. Add it in Options, ${p.section}.`,
              reason: errorMessage(s.reason),
            },
          ]
        : [];
    });
    if (failures.length > 0) {
      // The cluster exists: creating it again would fail, so the page offers only the way to it,
      // and the summary says the cluster waits for what was not stored rather than that it is.
      setCreated(c);
      setEntered((f) => {
        const left = { ...f };
        for (const p of failures) {
          Object.assign(left, p.forget);
        }
        return left;
      });
      setError(`Cluster created, but ${failures.map((p) => p.message).join(" Also, ")}`);
      setReasons(failures.map((p) => p.reason));
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
              onClick={() => go(s)}
            >
              {s}
            </button>
          ))}
        </nav>
        <div className="wizard-body" ref={wizardRef}>
          <CopiedFrom.Provider value={copied}>
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
                starts={startable(clusters)}
                onChooseFrom={chooseFrom}
                growthWarning={room.growthWarning}
              />
            )}
            {step === "Network" && (
              <NetworkStep
                form={form}
                set={set}
                shown={shown}
                cls={cls}
                bridgeOptions={bridgeOptions}
                network={network}
                network6={network6}
                free={free}
                checked={typed}
                datacenter={datacenterFirewall}
              />
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
                attempted={attempted.has(step)}
                refusal={checkRefusal(error, checked, { form, cls })}
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
            {step === "Policies" && (
              <PoliciesStep
                form={form}
                set={set}
                shown={shown}
                cls={cls}
                canTrust={canTrust}
                namespace={namespaceOf(form, namespaces)}
              />
            )}
            {step === "Identity providers" && (
              <IdentityStep form={form} set={set} canTrust={canTrust} showAll={attempted.has("Identity providers")} />
            )}
            {step === "Confirm" && cls && (
              <ConfirmStep
                read={checked.read}
                room={createRoom(form, nodes, usableStorages)}
                summary={summary(form, cls, {
                  ha: haOffered,
                  csi: csi.offered,
                  snapshots: snapshots.offered,
                  haGroups,
                  datacenterFirewall,
                })}
              />
            )}

            <ErrorBox message={error} />
            {reasons.length > 0 && <Detail>{reasons.join(" ")}</Detail>}
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
                  <CreateButton
                    blocked={[busy, !checked.accepted, refused].some(Boolean)}
                    label={createLabel}
                    onCreate={() => {
                      void create();
                    }}
                  />
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
                  Open cluster
                </button>
              </div>
            )}
          </CopiedFrom.Provider>
        </div>
      </div>
      {discarding && (
        <Dialog
          title="Discard new cluster"
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
          <p>Everything entered so far is lost.</p>
        </Dialog>
      )}
    </section>
  );
}
