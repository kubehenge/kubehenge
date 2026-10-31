import { lazy, Suspense, useState, type JSX } from "react";

import { api } from "./api";
import { EtcdRestoreDialog } from "./EtcdSnapshots";
import { useFleet } from "./fleet";
import { ManagementRestoreDialog } from "./ManagementBackups";
import { RemoveDialog } from "./RemoveManagement";
import { navigate } from "./router";
import type { Cluster, EtcdSnapshot, ManagementSettings, RunInfo } from "./types";
import { errorMessage } from "./ui";

// The setup page loads on its own, so its dialog does too.
const ApplyDialog = lazy(async () => {
  const m = await import("./Setup");
  return { default: m.ApplyDialog };
});

// A run that needs a passphrase or a confirmation asks for it in the dialog it was started from.
type Asked =
  | { dialog: "apply"; settings: ManagementSettings }
  | { dialog: "remove" }
  | { dialog: "management-restore"; snapshot: EtcdSnapshot }
  | { dialog: "etcd-restore"; cluster: Cluster; snapshot: EtcdSnapshot; automatic: boolean };

// Backups and restores start under new names, which show first in the cluster's lists; the
// others start again under the run's own.
const elsewhere = new Set<RunInfo["kind"]>(["backup", "restore"]);

type Start = () => Promise<unknown>;

const starts: Partial<Record<RunInfo["kind"], (r: RunInfo, ns: string, name: string) => Start | undefined>> = {
  backup: (_r, ns, name) => async () => api.backUp(ns, name),
  "etcd-snapshot": (_r, ns, name) => async () => api.takeEtcdSnapshot(ns, name),
  "template-refresh": () => async () => api.refreshTemplates(),
  restore: ({ restore }, ns, name) => (restore === undefined ? undefined : async () => api.restore(ns, name, restore)),
  template: ({ template }) => (template === undefined ? undefined : async () => api.createTemplate(template)),
  "appliance-upgrade": ({ release }) => (release === undefined ? undefined : async () => api.upgrade(release)),
};

/** startOf is how a failed run starts again as it was, where that needs nothing more. */
export function startOf(r: RunInfo): Start | undefined {
  return starts[r.kind]?.(r, r.namespace ?? "", r.cluster ?? "");
}

function missing(what: string): never {
  throw new Error(`${what} is no longer there.`);
}

async function managementAsked(r: RunInfo): Promise<Asked> {
  if (r.action === "remove") {
    return { dialog: "remove" };
  }
  if (r.action === "etcd-restore") {
    const { snapshots } = await api.managementSnapshots();
    const snapshot = snapshots.find((s) => s.name === r.snapshot) ?? missing(`Snapshot ${r.snapshot ?? ""}`);
    return { dialog: "management-restore", snapshot };
  }
  const { settings } = await api.setupStatus();
  return { dialog: "apply", settings: settings ?? missing("The management cluster's settings") };
}

async function etcdAsked(r: RunInfo, cluster: Cluster): Promise<Asked> {
  const v = await api.etcdSnapshots(cluster.namespace, cluster.name);
  const snapshot = v.snapshots.find((s) => s.name === r.snapshot) ?? missing(`Snapshot ${r.snapshot ?? ""}`);
  return { dialog: "etcd-restore", cluster, snapshot, automatic: v.automatic };
}

/** askOf is how a failed run that asks for something first finds what its dialog needs. */
export function askOf(r: RunInfo, cluster: Cluster | undefined): (() => Promise<Asked>) | undefined {
  if (r.kind === "management-cluster" && r.action !== undefined) {
    return async () => managementAsked(r);
  }
  if (r.kind === "etcd-restore" && r.snapshot !== undefined && cluster?.allowed.kubeconfig === true) {
    return async () => etcdAsked(r, cluster);
  }
  return undefined;
}

function AskedDialog({
  asked,
  title,
  onClose,
  onStarted,
}: {
  asked: Asked;
  title: string;
  onClose: () => void;
  onStarted: () => void;
}): JSX.Element {
  if (asked.dialog === "apply") {
    return (
      <Suspense fallback={null}>
        <ApplyDialog title={title} action="Apply" settings={asked.settings} onClose={onClose} onStarted={onStarted} />
      </Suspense>
    );
  }
  if (asked.dialog === "remove") {
    return <RemoveDialog onClose={onClose} onStarted={onStarted} />;
  }
  if (asked.dialog === "management-restore") {
    return <ManagementRestoreDialog snapshot={asked.snapshot} onClose={onClose} onStarted={onStarted} />;
  }
  return (
    <EtcdRestoreDialog
      cluster={asked.cluster}
      snapshot={asked.snapshot}
      automatic={asked.automatic}
      onClose={onClose}
      onDone={onStarted}
    />
  );
}

/**
 * TryAgain starts a failed run again with what it was started with. One that needs a passphrase
 * or a confirmation opens the dialog that asks for it; a backup or restore then goes to the list
 * its new run shows in.
 */
export function TryAgain({
  run,
  to,
  onError,
}: {
  run: RunInfo;
  to: string;
  onError: (message: string | undefined) => void;
}): JSX.Element | null {
  const cluster = useFleet()?.clusters?.find((c) => c.namespace === run.namespace && c.name === run.cluster);
  const [busy, setBusy] = useState(false);
  const [asked, setAsked] = useState<Asked>();
  const start = startOf(run);
  const ask = askOf(run, cluster);
  if (run.state !== "failed" || (start === undefined && ask === undefined)) {
    return null;
  }
  const started = (): void => {
    setAsked(undefined);
    if (elsewhere.has(run.kind)) {
      navigate(to);
    }
  };
  const press = async (): Promise<void> => {
    setBusy(true);
    onError(undefined);
    try {
      if (start === undefined) {
        setAsked(await ask?.());
      } else {
        await start();
        started();
      }
    } catch (err) {
      onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <button
        type="button"
        className="primary"
        disabled={busy}
        onClick={() => {
          void press();
        }}
      >
        Try again
      </button>
      {asked !== undefined && (
        <AskedDialog
          asked={asked}
          title={run.what}
          onClose={() => {
            setAsked(undefined);
          }}
          onStarted={started}
        />
      )}
    </>
  );
}
