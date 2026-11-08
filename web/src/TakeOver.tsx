import { useState, type JSX } from "react";

import { api } from "./api";
import type { StaleState } from "./types";
import { Dialog, ErrorBox, Field, errorMessage, passphraseShort } from "./ui";

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
