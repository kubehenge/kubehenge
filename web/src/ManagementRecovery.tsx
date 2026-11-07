import { useId, useState, type JSX } from "react";

import { adminPath } from "./admin-views";
import { api } from "./api";
import { Link } from "./router";
import type { EtcdSnapshot, ManagementBackups, StaleState } from "./types";
import { Dialog, ErrorBox, Field, errorMessage, passphraseShort, saveFile, usePoll, pollIdle } from "./ui";

const day = (iso: string): string => new Date(iso).toLocaleDateString();

const password = (value: string, set: (v: string) => void) =>
  function PasswordInput(id: string): JSX.Element {
    return (
      <input id={id} type="password" autoComplete="new-password" value={value} onChange={(e) => set(e.target.value)} />
    );
  };

/** BackupWarning is the standing warning that the management cluster's etcd is not backed up. */
export function BackupWarning({
  backups: b,
  onChanged,
}: {
  backups: ManagementBackups;
  onChanged: () => void;
}): JSX.Element | null {
  const [error, setError] = useState<string>();
  if (b.warning === undefined) {
    return null;
  }
  const { dismissal } = b.warning;
  if (dismissal !== undefined) {
    return (
      <p className="muted">
        Backup warning dismissed by {dismissal.by} on {day(dismissal.at)}, back on {day(dismissal.until)}.
      </p>
    );
  }
  return (
    <div className="warning backup-warning">
      <p>Not backed up. Every cluster&apos;s CAs, kubeconfigs and backup keys live only in this etcd.</p>
      <div className="actions">
        <button
          type="button"
          onClick={() => {
            void (async () => {
              try {
                await api.dismissBackupWarning();
                onChanged();
              } catch (err) {
                setError(errorMessage(err));
              }
            })();
          }}
        >
          Snooze 30 days
        </button>
      </div>
      <ErrorBox message={error} />
    </div>
  );
}

/** FleetBackupWarning shows the warning above the cluster list, to those who may set backups up. */
export function FleetBackupWarning(): JSX.Element | null {
  const { data: b, reload } = usePoll(api.managementBackups, pollIdle);
  if (b?.exportDue === true && b.specKey === true) {
    return (
      <div className="pad">
        <p className="warning">Export due. Stored Secrets open only with this appliance&apos;s key.</p>
        <Link to={adminPath("backups")}>Export the appliance</Link>
      </div>
    );
  }
  if (b?.warning === undefined || b.warning.dismissal !== undefined) {
    return null;
  }
  return (
    <div className="pad">
      <BackupWarning
        backups={b}
        onChanged={() => {
          void reload();
        }}
      />
      <Link to={adminPath("backups")}>Set up backups</Link>
    </div>
  );
}

/** ExportBox saves the appliance's state, so that a new appliance can restore the management cluster. */
export function ExportBox({ backups: b }: { backups: ManagementBackups }): JSX.Element {
  const [open, setOpen] = useState(false);
  return (
    <div className="box">
      <h3>Export</h3>
      {b.exportDue === true && (
        <p className="warning">
          No export since the settings or the recovery key changed. Stored Secrets open only with this appliance&apos;s
          key.
        </p>
      )}
      <p className="muted">The export opens every stored Secret. Keep it like a password vault.</p>
      <p className="muted">{b.exportedAt === undefined ? "Never exported." : `Last exported ${day(b.exportedAt)}.`}</p>
      <div className="actions">
        <button type="button" onClick={() => setOpen(true)}>
          Export…
        </button>
      </div>
      {open && <ExportDialog onClose={() => setOpen(false)} />}
    </div>
  );
}

function ExportDialog({ onClose }: { onClose: () => void }): JSX.Element {
  const [state, setState] = useState("");
  const [exportPassphrase, setExportPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const invalid = passphraseShort(state) || passphraseShort(exportPassphrase);
  const submit = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      saveFile(await api.exportManagement(state, exportPassphrase), "kubehenge-export.age");
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      title="Export state"
      onClose={onClose}
      onSubmit={() => {
        void submit();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || invalid}>
            Export
          </button>
        </>
      }
    >
      <Field label="State passphrase">{password(state, setState)}</Field>
      <Field label="Export passphrase">{password(exportPassphrase, setExportPassphrase)}</Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}

/** ImportBox is the appliance's setup offering to restore from an export. */
export function ImportBox({ onImported }: { onImported: () => void }): JSX.Element {
  const [open, setOpen] = useState(false);
  const headingId = useId();
  return (
    <section className="panel setup-import" aria-labelledby={headingId}>
      <div className="box">
        <h3 id={headingId}>Restore from an export</h3>
        <p className="muted">For a new appliance whose management cluster still runs.</p>
        <div className="actions">
          <button type="button" onClick={() => setOpen(true)}>
            Import…
          </button>
        </div>
        {open && (
          <ImportDialog
            onClose={() => setOpen(false)}
            onImported={() => {
              setOpen(false);
              onImported();
            }}
          />
        )}
      </div>
    </section>
  );
}

const toBase64 = (bytes: Uint8Array): string => {
  let text = "";
  for (const byte of bytes) {
    text += String.fromCodePoint(byte);
  }
  return btoa(text);
};

function ImportDialog({ onClose, onImported }: { onClose: () => void; onImported: () => void }): JSX.Element {
  const [file, setFile] = useState<File>();
  const [exportPassphrase, setExportPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const submit = async (): Promise<void> => {
    if (file === undefined) {
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      const bundle = toBase64(new Uint8Array(await file.arrayBuffer()));
      await api.importManagement(bundle, exportPassphrase);
      onImported();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog
      title="Import export"
      onClose={onClose}
      onSubmit={() => {
        void submit();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className="primary"
            disabled={busy || file === undefined || passphraseShort(exportPassphrase)}
          >
            Import
          </button>
        </>
      }
    >
      <Field label="Export file">
        {(id) => <input id={id} type="file" accept=".age" onChange={(e) => setFile(e.target.files?.[0])} />}
      </Field>
      <Field label="Export passphrase">{password(exportPassphrase, setExportPassphrase)}</Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}

/** RecreateDialog makes the management cluster's VMs again and restores a snapshot into them. */
export function RecreateDialog({ snapshot: s, onClose }: { snapshot: EtcdSnapshot; onClose: () => void }): JSX.Element {
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [started, setStarted] = useState(false);
  const submit = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.recreateManagement(s.name, passphrase);
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
        title="Recreating"
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
      title="Recreate management cluster"
      onClose={onClose}
      onSubmit={() => {
        void submit();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={busy || passphraseShort(passphrase)}>
            Recreate
          </button>
        </>
      }
    >
      <p className="warning">
        Makes the VMs again and restores etcd to {new Date(s.time).toLocaleString()}. Clusters changed since go back to
        then.
      </p>
      <Field label="State passphrase">{password(passphrase, setPassphrase)}</Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}

// TakeOver lets an administrator apply this appliance's state after a newer one changed the
// management cluster, as after a snapshot rollback, an older backup or a second appliance.
export function TakeOver({ stale, onDone }: { stale: StaleState; onDone: () => void }): JSX.Element {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" className="danger" onClick={() => setOpen(true)}>
        Take over…
      </button>
      {open && (
        <TakeOverDialog
          stale={stale}
          onClose={() => setOpen(false)}
          onDone={() => {
            setOpen(false);
            onDone();
          }}
        />
      )}
    </>
  );
}

function TakeOverDialog({
  stale,
  onClose,
  onDone,
}: {
  stale: StaleState;
  onClose: () => void;
  onDone: () => void;
}): JSX.Element {
  const [typed, setTyped] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const ready = !busy && typed === "overwrite" && !passphraseShort(passphrase);

  const takeOver = async () => {
    if (!ready) {
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.takeOverManagement(passphrase);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="Take over management"
      onClose={onClose}
      onSubmit={() => {
        void takeOver();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={!ready}>
            Take over
          </button>
        </>
      }
    >
      <p>
        This state is from apply {stale.local}, the cluster at apply {stale.cluster}. The next apply undoes the newer
        changes.
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
      <Field label="Type overwrite to confirm">
        {(id) => <input id={id} autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
