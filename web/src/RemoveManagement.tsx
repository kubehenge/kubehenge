import { useState, type JSX } from "react";

import { api } from "./api";
import { ErrorBox, Field, errorMessage, minPassphrase, passphraseShort } from "./ui";

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
        Deletes the management cluster, the VM templates and the pools, users and roles Kubehenge made on Proxmox VE.
        Delete every cluster first. The appliance stays, ready to set up again.
      </p>
      <Field label="State passphrase" hint={`At least ${minPassphrase} characters.`}>
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
      <Field label="Confirm" hint="Type remove.">
        {(id) => <input id={id} autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />}
      </Field>
      {running && (
        <p className="muted">A run of the management cluster is going on; removing waits until it is over.</p>
      )}
      <div className="actions">
        <button
          type="submit"
          className="danger"
          disabled={busy || running || typed !== "remove" || passphraseShort(passphrase)}
        >
          Remove
        </button>
      </div>
      <ErrorBox message={error} />
    </form>
  );
}
