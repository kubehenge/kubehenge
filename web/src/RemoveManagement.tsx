import { useState, type JSX } from "react";

import { api } from "./api";
import { useFleet } from "./fleet";
import { Dialog, ErrorBox, Field, errorMessage, passphraseShort, useStatePassphrase } from "./ui";

// DangerZone holds what cannot be taken back, apart from the settings above it: removing
// Kubehenge takes the management cluster, its templates, pools, users and roles off Proxmox VE.
// Every cluster has to be deleted first.
export function DangerZone({ running, onStarted }: { running: boolean; onStarted: () => void }): JSX.Element {
  const [open, setOpen] = useState(false);
  // The server refuses a removal while clusters exist, so the page does not offer one, nor while
  // it does not know whether any do.
  const fleet = useFleet();
  const clusters = fleet?.clusters?.length;
  let note = "Keeps the appliance";
  if (clusters === undefined) {
    note = fleet?.error === undefined ? "Loading…" : "Clusters not read.";
  } else if (clusters > 0) {
    note = `Delete ${clusters} ${clusters === 1 ? "cluster" : "clusters"} first`;
  }
  return (
    <section className="panel danger-zone" aria-labelledby="danger-zone">
      <div className="box">
        <h3 id="danger-zone">Danger zone</h3>
        <div className="danger-row">
          <div>
            <strong>Remove Kubehenge</strong>
            <p className="muted">{note}</p>
          </div>
          <button type="button" className="danger" disabled={running || clusters !== 0} onClick={() => setOpen(true)}>
            Remove…
          </button>
        </div>
      </div>
      {open && (
        <RemoveDialog
          onClose={() => setOpen(false)}
          onStarted={() => {
            setOpen(false);
            onStarted();
          }}
        />
      )}
    </section>
  );
}

export function RemoveDialog({ onClose, onStarted }: { onClose: () => void; onStarted: () => void }): JSX.Element {
  const [typed, setTyped] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [removeBackups, setRemoveBackups] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const ask = useStatePassphrase();
  const ready = !busy && typed === "remove" && !(ask && passphraseShort(passphrase));

  const remove = async () => {
    if (!ready) {
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.removeManagement(passphrase, removeBackups);
      onStarted();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="Remove Kubehenge"
      onClose={onClose}
      onSubmit={() => {
        void remove();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={!ready}>
            Remove
          </button>
        </>
      }
    >
      <p>Deletes the management cluster, templates, pools, users and roles.</p>
      <p>Keeps the backup volume, VM kubehenge-backups, with its etcd snapshots and exports.</p>
      <label className="check">
        <input type="checkbox" checked={removeBackups} onChange={(e) => setRemoveBackups(e.target.checked)} /> Delete
        the backup volume
      </label>
      {ask && (
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
      )}
      <Field label="Type remove to confirm">
        {(id) => <input id={id} autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
