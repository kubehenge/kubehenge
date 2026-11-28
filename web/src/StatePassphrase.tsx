import { useState, type JSX } from "react";

import { api } from "./api";
import type { SetupStatus } from "./types";
import { Dialog, ErrorBox, Field, errorMessage, passphraseMissing, passphraseShort } from "./ui";

// RequirePassphrase switches the passphrase that opens the management cluster's state on or off.
export function RequirePassphrase({ status, onChanged }: { status: SetupStatus; onChanged: () => void }): JSX.Element {
  const [asking, setAsking] = useState(false);
  const on = status.passphraseRequired === true;
  return (
    <div className="box">
      <h3>State passphrase</h3>
      <label className="check">
        <input type="checkbox" checked={on} onChange={() => setAsking(true)} /> Require a passphrase
      </label>
      {asking && (
        <SwitchDialog
          on={on}
          current={status.passphrase === true}
          onClose={() => setAsking(false)}
          onDone={() => {
            setAsking(false);
            onChanged();
          }}
        />
      )}
    </div>
  );
}

// SwitchDialog asks for the passphrase in force, when there is one, and a new one to require.
function SwitchDialog({
  on,
  current,
  onClose,
  onDone,
}: {
  on: boolean;
  current: boolean;
  onClose: () => void;
  onDone: () => void;
}): JSX.Element {
  const [passphrase, setPassphrase] = useState("");
  const [next, setNext] = useState("");
  const [repeat, setRepeat] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const invalid = passphraseMissing(current, passphrase) || (!on && (passphraseShort(next) || next !== repeat));
  const submit = async (): Promise<void> => {
    if (invalid) {
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.statePassphrase(!on, passphrase, next);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={on ? "Keep the state's key" : "Require a passphrase"}
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
            {on ? "Stop requiring" : "Require"}
          </button>
        </>
      }
    >
      {current && (
        <PasswordField label="State passphrase" value={passphrase} onChange={setPassphrase} auto="current-password" />
      )}
      {!on && <PasswordField label="New passphrase" value={next} onChange={setNext} auto="new-password" />}
      {!on && <PasswordField label="Repeat passphrase" value={repeat} onChange={setRepeat} auto="new-password" />}
      <ErrorBox message={error} />
    </Dialog>
  );
}

function PasswordField({
  label,
  value,
  onChange,
  auto,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  auto: string;
}): JSX.Element {
  return (
    <Field label={label}>
      {(id) => (
        <input id={id} type="password" autoComplete={auto} value={value} onChange={(e) => onChange(e.target.value)} />
      )}
    </Field>
  );
}
