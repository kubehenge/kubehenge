import { useState, type JSX } from "react";

import { api } from "./api";
import { Dialog, ErrorBox, Field, errorMessage, passphraseShort } from "./ui";

// DangerZone holds what cannot be taken back, apart from the settings above it: removing
// Kubehenge takes the management cluster, its templates, pools, users and roles off Proxmox VE.
// Every cluster has to be deleted first.
export function DangerZone({ running, onStarted }: { running: boolean; onStarted: () => void }): JSX.Element {
  const [open, setOpen] = useState(false);
  return (
    <section className="panel danger-zone" aria-labelledby="danger-zone">
      <div className="box">
        <h3 id="danger-zone">Danger zone</h3>
        <div className="danger-row">
          <div>
            <strong>Remove Kubehenge</strong>
            <p className="muted">Keeps the appliance</p>
          </div>
          <button type="button" className="danger" disabled={running} onClick={() => setOpen(true)}>
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

function RemoveDialog({ onClose, onStarted }: { onClose: () => void; onStarted: () => void }): JSX.Element {
  const [typed, setTyped] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const ready = !busy && typed === "remove" && !passphraseShort(passphrase);

  const remove = async () => {
    if (!ready) {
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.removeManagement(passphrase);
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
      <p>Deletes the management cluster, templates, pools, users and roles. Delete every cluster first.</p>
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
      <Field label="Type remove to confirm">
        {(id) => <input id={id} autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
