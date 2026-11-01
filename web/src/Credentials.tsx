import { useState, type JSX } from "react";

import { api } from "./api";
import { ErrorBox, Field, errorMessage, passphraseShort, saveFile } from "./ui";

// Credentials hands an administrator the management cluster's own credentials, for repairs beyond
// the interface. They come out of the encrypted state, so the passphrase is needed.
export function Credentials({ running }: { running: boolean }): JSX.Element {
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const short = passphraseShort(passphrase);

  const download = async (name: "kubeconfig" | "talosconfig") => {
    setBusy(true);
    setError(undefined);
    try {
      saveFile(await api.credential(name, passphrase), `management.${name}`);
      setPassphrase("");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  // A form, so that password managers fill in the passphrase. It has two actions and no default one.
  return (
    <form className="box" onSubmit={(e) => e.preventDefault()}>
      <h3>Repair access</h3>
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
      {running && <p className="muted">Available once the management cluster run ends</p>}
      <div className="actions">
        <button
          type="button"
          disabled={busy || short || running}
          onClick={() => {
            void download("kubeconfig");
          }}
        >
          Download admin kubeconfig
        </button>
        <button
          type="button"
          disabled={busy || short || running}
          onClick={() => {
            void download("talosconfig");
          }}
        >
          Download admin talosconfig
        </button>
      </div>
      <ErrorBox message={error} />
    </form>
  );
}
