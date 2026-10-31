import { useEffect, useState, type JSX, type KeyboardEvent, type ReactNode } from "react";

import { api } from "./api";
import type { ClusterRef, StoredCredential } from "./types";
import { ErrorBox, Field, Skeleton, errorMessage, filled } from "./ui";

/** One value a credential holds. */
export interface CredentialKey {
  key: string;
  label: string;
  /** Shown as typed, as a user name is. The others are masked. */
  plain?: boolean;
}

/** setText tells when and by whom a credential was stored, or that none is. */
export function setText(c: StoredCredential): string {
  if (!c.set) {
    return "Not set";
  }
  const on =
    c.setAt === undefined ? "" : ` on ${new Date(c.setAt).toLocaleDateString(undefined, { dateStyle: "long" })}`;
  const by = filled(c.setBy) ? ` by ${c.setBy}` : "";
  return `Set${on}${by}`;
}

const refOf = (namespace?: string, name?: string): ClusterRef | undefined =>
  namespace === undefined || name === undefined ? undefined : { namespace, name };

/**
 * CredentialField stores, replaces or removes one credential: a cluster's, or with no cluster, the
 * one for every cluster. What is typed goes to the server and never comes back. The field shows
 * only whether one is set, when and by whom, and offers changes only to those who may make them.
 * It is no form of its own, so it sits in a dialog's form without submitting it.
 */
export function CredentialField({
  label,
  purpose,
  cluster,
  keys,
  hint,
}: {
  label: string;
  purpose: string;
  cluster?: ClusterRef | undefined;
  keys: CredentialKey[];
  hint?: ReactNode;
}): JSX.Element {
  const namespace = cluster?.namespace;
  const name = cluster?.name;
  const [stored, setStored] = useState<StoredCredential>();
  // The values being typed. None while the field only shows what is stored.
  const [values, setValues] = useState<Record<string, string>>();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const c = await api.storedCredential(purpose, refOf(namespace, name));
        if (current) {
          setStored(c);
        }
      } catch (err) {
        if (current) {
          setError(errorMessage(err));
        }
      }
    }
    void load();
    return () => {
      current = false;
    };
  }, [purpose, namespace, name]);

  const complete = values !== undefined && keys.every((k) => filled(values[k.key]));

  const run = async (step: () => Promise<StoredCredential>): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      setStored(await step());
      // What was typed does not linger in the page once the server has it.
      setValues(undefined);
      setConfirming(false);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const save = (): void => {
    if (values !== undefined && complete) {
      void run(async () => api.storeCredential(purpose, values, refOf(namespace, name)));
    }
  };

  const remove = (): void => {
    void run(async () => {
      await api.removeCredential(purpose, refOf(namespace, name));
      return { purpose, set: false, canChange: true };
    });
  };

  // Enter stores what was typed rather than submitting the form around the field, whose own Save is
  // a different action.
  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>): void => {
    if (e.key === "Enter") {
      e.preventDefault();
      save();
    }
  };

  return (
    <fieldset className="group">
      <legend>{label}</legend>
      {hint !== undefined && <p className="hint">{hint}</p>}
      {stored === undefined && !filled(error) && <Skeleton rows={1} />}
      {stored !== undefined && <p className="muted">{setText(stored)}</p>}
      {values !== undefined && (
        <>
          {keys.map((k) => (
            <Field key={k.key} label={k.label}>
              {(id) => (
                <input
                  id={id}
                  type={k.plain === true ? "text" : "password"}
                  autoComplete={k.plain === true ? "off" : "new-password"}
                  spellCheck={false}
                  value={values[k.key] ?? ""}
                  onChange={(e) => setValues({ ...values, [k.key]: e.target.value })}
                  onKeyDown={onKeyDown}
                />
              )}
            </Field>
          ))}
          <div className="actions">
            <button
              type="button"
              onClick={() => {
                setValues(undefined);
                setError(undefined);
              }}
            >
              Cancel
            </button>
            <button type="button" className="primary" disabled={busy || !complete} onClick={save}>
              Store
            </button>
          </div>
        </>
      )}
      {values === undefined && confirming && (
        <>
          <p>Delete this credential? What uses it stops working.</p>
          <div className="actions">
            <button type="button" onClick={() => setConfirming(false)}>
              Keep
            </button>
            <button type="button" className="danger" disabled={busy} onClick={remove}>
              Delete
            </button>
          </div>
        </>
      )}
      {values === undefined && !confirming && stored?.canChange === true && (
        <div className="actions">
          {stored.set && (
            <button type="button" onClick={() => setConfirming(true)}>
              Delete…
            </button>
          )}
          <button type="button" onClick={() => setValues({})}>
            {stored.set ? "Replace…" : "Set…"}
          </button>
        </div>
      )}
      <ErrorBox message={error} />
    </fieldset>
  );
}
