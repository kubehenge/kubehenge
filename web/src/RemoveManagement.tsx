import { useState, type JSX } from "react";

import { api } from "./api";
import { ErrorBox, Field, errorMessage, passphraseShort } from "./ui";

// RemoveManagement takes Kubehenge off Proxmox VE: the management cluster, its templates, pools,
// users and roles. Every cluster has to be deleted first.
export function RemoveManagement({ running, onStarted }: { running: boolean; onStarted: () => void }): JSX.Element {
  const [typed, setTyped] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const remove = async () => {
    setBusy(true);
    setError(undefined);
    try {
      await api.removeManagement(passphrase);
      setPassphrase("");
      setTyped("");
      onStarted();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  // A form, so that password managers fill in the passphrase. Enter removes only once the button
  // would: a disabled default button submits nothing.
  return (
    <form
      className="box"
      onSubmit={(e) => {
        e.preventDefault();
        void remove();
      }}
    >
      <h3>Remove Kubehenge</h3>
      <p className="muted">
        Deletes the management cluster, templates, pools, users and roles, and keeps the appliance. Delete every cluster
        first.
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
      <Field label="Type remove to confirm">
        {(id) => <input id={id} autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />}
      </Field>
      {running && <p className="muted">Wait for the current run to finish.</p>}
      <div className="actions">
        <button
          type="submit"
          className="danger"
          disabled={busy || running || typed !== "remove" || passphraseShort(passphrase)}
        >
          Remove Kubehenge
        </button>
      </div>
      <ErrorBox message={error} />
    </form>
  );
}
