import { useState, type JSX } from "react";

import { adminPath } from "./admin-views";
import { api } from "./api";
import { Link } from "./router";
import type { EtcdSnapshot, ManagementBackups as Backups, ManagementBackupSettings } from "./types";
import {
  Choice,
  Dialog,
  ErrorBox,
  Field,
  Status,
  age,
  bytes,
  errorMessage,
  passphraseShort,
  pollIdle,
  pollRunning,
  sentence,
  shownNumber,
  typedNumber,
  usePoll,
} from "./ui";

const when = (iso: string | undefined): string => (iso === undefined ? "" : new Date(iso).toLocaleString());

const defaults = { everyHours: 6, keep: 28 };

// ManagementBackups snapshots the management cluster's etcd into a backup target on a schedule,
// lists the snapshots there and restores one. The appliance does all of it through the nodes'
// Talos API, so a restore works while the management cluster's API is down.
export function ManagementBackups({ running }: { running: boolean }): JSX.Element {
  const {
    data: b,
    error,
    reload,
  } = usePoll(api.managementBackups, (v) => (v?.taking === true ? pollRunning : pollIdle));
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string>();
  const act = async (call: () => Promise<unknown>): Promise<void> => {
    setBusy(true);
    setActionError(undefined);
    try {
      await call();
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      await reload();
      setBusy(false);
    }
  };
  if (!b) {
    return error === undefined ? <p className="loading">Loading…</p> : <ErrorBox message={error} />;
  }
  return (
    <>
      <div className="box">
        <h3>Management cluster etcd</h3>
        {b.settings === undefined ? (
          <NotSetUp backups={b} onSetUp={() => setEditing(true)} />
        ) : (
          <>
            <Summary backups={b} settings={b.settings} />
            <div className="actions">
              <button
                type="button"
                className="primary"
                disabled={busy || b.taking === true}
                onClick={() => {
                  void act(api.takeManagementSnapshot);
                }}
              >
                Take snapshot
              </button>
              <button type="button" disabled={busy} onClick={() => setEditing(true)}>
                Change…
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => {
                  void act(api.stopManagementBackups);
                }}
              >
                Switch off
              </button>
            </div>
          </>
        )}
        <ErrorBox message={actionError} />
      </div>
      {b.settings !== undefined && <Snapshots running={running} />}
      {editing && (
        <SetUpDialog
          backups={b}
          onClose={() => setEditing(false)}
          onDone={async () => {
            setEditing(false);
            await reload();
          }}
        />
      )}
    </>
  );
}

function NotSetUp({ backups: b, onSetUp }: { backups: Backups; onSetUp: () => void }): JSX.Element {
  if (b.targets.length === 0) {
    return (
      <p className="muted">
        Off. Needs a backup target: add one under <Link to={adminPath("")}>Management cluster</Link>.
      </p>
    );
  }
  return (
    <>
      <p className="muted">Off.</p>
      <div className="actions">
        <button type="button" className="primary" onClick={onSetUp}>
          Set up…
        </button>
      </div>
    </>
  );
}

function Summary({ backups: b, settings: s }: { backups: Backups; settings: ManagementBackupSettings }): JSX.Element {
  const failed = b.error !== undefined && b.error !== "";
  let last = <Status state="idle" text="None yet" />;
  if (b.taking === true) {
    last = <Status state="progress" text="Taking" />;
  } else if (failed) {
    last = <Status state="error" text="Failed" />;
  } else if (b.last !== undefined) {
    last = <Status state="ok" text="Taken" />;
  }
  return (
    <dl className="summary">
      <dt>Target</dt>
      <dd>{s.target}</dd>
      <dt>Schedule</dt>
      <dd>
        Every {s.everyHours} h, newest {s.keep} kept
      </dd>
      <dt>Last snapshot</dt>
      <dd>
        {last}
        {b.last !== undefined && (
          <span className="muted">
            {" "}
            {when(b.last.time)} ({age(b.last.time)} ago)
          </span>
        )}
        {failed && <div className="hint message">{sentence(b.error ?? "")}</div>}
      </dd>
      {b.next !== undefined && b.taking !== true && (
        <>
          <dt>Next</dt>
          <dd>{when(b.next)}</dd>
        </>
      )}
    </dl>
  );
}

function Snapshots({ running }: { running: boolean }): JSX.Element {
  const { data: v, error } = usePoll(api.managementSnapshots, pollIdle);
  const [restoring, setRestoring] = useState<EtcdSnapshot>();
  const list = v?.snapshots ?? [];
  return (
    <div className="box scroll">
      <h3>Snapshots in the target</h3>
      <ErrorBox message={error ?? v?.error} />
      <table className="grid">
        <thead>
          <tr>
            <th>Taken</th>
            <th>Kubernetes</th>
            <th>Size</th>
            <th>
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {list.length === 0 && (
            <tr>
              <td colSpan={4} className="muted">
                {v === undefined ? "Loading…" : "None yet."}
              </td>
            </tr>
          )}
          {list.map((s) => (
            <tr key={s.name}>
              <td title={s.name}>
                {when(s.time)} <span className="muted">({age(s.time)} ago)</span>
              </td>
              <td>{s.kubernetesVersion ?? ""}</td>
              <td>{s.size === undefined ? "" : bytes(s.size)}</td>
              <td className="row-action">
                <button
                  type="button"
                  disabled={running}
                  title={running ? "A run of the management cluster is going on." : undefined}
                  aria-label={`Restore from ${s.name}`}
                  onClick={() => setRestoring(s)}
                >
                  Restore…
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {restoring !== undefined && <RestoreDialog snapshot={restoring} onClose={() => setRestoring(undefined)} />}
    </div>
  );
}

function SetUpDialog({
  backups: b,
  onClose,
  onDone,
}: {
  backups: Backups;
  onClose: () => void;
  onDone: () => Promise<void>;
}): JSX.Element {
  const s = b.settings;
  const [target, setTarget] = useState(s?.target ?? (b.targets.length === 1 ? (b.targets[0] ?? "") : ""));
  const [everyHours, setEveryHours] = useState(s?.everyHours ?? defaults.everyHours);
  const [keep, setKeep] = useState(s?.keep ?? defaults.keep);
  const [accessKeyID, setAccessKeyID] = useState("");
  const [secretAccessKey, setSecretAccessKey] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const keepKeys = b.keysStored && accessKeyID === "" && secretAccessKey === "";
  const ready =
    target !== "" &&
    everyHours >= 1 &&
    keep >= 1 &&
    (keepKeys || (accessKeyID !== "" && secretAccessKey !== "")) &&
    !passphraseShort(passphrase);
  const submit = async (): Promise<void> => {
    if (!ready) {
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.configureManagementBackups({
        target,
        everyHours,
        keep,
        accessKeyID,
        secretAccessKey,
        passphrase,
      });
      await onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={s === undefined ? "Back up the management cluster" : "Change the management cluster's backups"}
      onClose={onClose}
      onSubmit={() => {
        void submit();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !ready}>
            Save
          </button>
        </>
      }
    >
      <Field label="Target">{(id) => <Choice id={id} value={target} options={b.targets} onChoose={setTarget} />}</Field>
      <Field label="Every (hours)">
        {(id) => (
          <input
            id={id}
            type="number"
            min={1}
            max={168}
            value={shownNumber(everyHours)}
            onChange={(e) => setEveryHours(typedNumber(e.target.value))}
          />
        )}
      </Field>
      <Field label="Keep">
        {(id) => (
          <input
            id={id}
            type="number"
            min={1}
            max={500}
            value={shownNumber(keep)}
            onChange={(e) => setKeep(typedNumber(e.target.value))}
          />
        )}
      </Field>
      <Field label="Access key ID">
        {(id) => (
          <input
            id={id}
            autoComplete="off"
            placeholder={b.keysStored ? "Stored" : undefined}
            value={accessKeyID}
            onChange={(e) => setAccessKeyID(e.target.value)}
          />
        )}
      </Field>
      <Field label="Secret access key">
        {(id) => (
          <input
            id={id}
            type="password"
            autoComplete="off"
            placeholder={b.keysStored ? "Stored" : undefined}
            value={secretAccessKey}
            onChange={(e) => setSecretAccessKey(e.target.value)}
          />
        )}
      </Field>
      <Field label="State passphrase">
        {(id) => (
          <input
            id={id}
            type="password"
            autoComplete="current-password"
            value={passphrase}
            onChange={(e) => setPassphrase(e.target.value)}
          />
        )}
      </Field>
      <p className="warning">
        The appliance keeps the access key and a talosconfig that can only take etcd snapshots. Restoring needs this
        passphrase.
      </p>
      <ErrorBox message={error} />
    </Dialog>
  );
}

function RestoreDialog({ snapshot: s, onClose }: { snapshot: EtcdSnapshot; onClose: () => void }): JSX.Element {
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [started, setStarted] = useState(false);
  const short = passphraseShort(passphrase);
  const submit = async (): Promise<void> => {
    if (short) {
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.restoreManagement(s.name, passphrase);
      setStarted(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  if (started) {
    return (
      <Dialog
        title="Restoring etcd"
        onClose={onClose}
        actions={
          <button type="button" className="primary" onClick={onClose}>
            Done
          </button>
        }
      >
        <p>
          Started. <Link to="/runs/management-cluster">Follow the run</Link>.
        </p>
      </Dialog>
    );
  }
  return (
    <Dialog
      title="Restore the management cluster's etcd"
      onClose={onClose}
      onSubmit={() => {
        void submit();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={busy || short}>
            Restore
          </button>
        </>
      }
    >
      <p className="warning">
        Wipes etcd on every control-plane node and restores it to {when(s.time)}. Clusters and every change since go
        back to then.
      </p>
      <Field label="State passphrase">
        {(id) => (
          <input
            id={id}
            type="password"
            autoComplete="current-password"
            value={passphrase}
            onChange={(e) => setPassphrase(e.target.value)}
          />
        )}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
