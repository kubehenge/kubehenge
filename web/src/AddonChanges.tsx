import { useState, type JSX } from "react";

import { api } from "./api";
import { addonConflictCondition } from "./fleet";
import type { AddonChange, Cluster } from "./types";
import { Detail, ErrorBox, errorMessage, filled } from "./ui";

/** changeText is one waiting change. Equal versions mean only the add-on's values change. */
export const changeText = (c: AddonChange): string =>
  c.from === c.to ? `${c.addon}: new values` : `${c.addon}: ${c.chart} ${c.from} to ${c.to}`;

/**
 * AddonChangesWaiting lists the add-on chart changes that wait for the maintenance window, with a
 * button that applies them now. It shows nothing while none wait.
 */
export function AddonChangesWaiting({
  cluster: c,
  can,
  why,
  onApplied,
}: {
  cluster: Cluster;
  can: boolean;
  why?: string | undefined;
  onApplied: () => void;
}): JSX.Element | null {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const waiting = c.addonChanges;
  if (waiting === undefined || waiting.changes.length === 0) {
    return null;
  }
  const apply = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.applyAddonChanges(c.namespace, c.name);
      onApplied();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <fieldset className="group">
      <legend>Add-on changes</legend>
      <ul>
        {waiting.changes.map((x) => (
          <li key={`${x.addon}/${x.chart}`}>{changeText(x)}</li>
        ))}
      </ul>
      <p>Window opens {new Date(waiting.opens).toLocaleString()}</p>
      <p className="muted">Otherwise the changes apply when the maintenance window opens.</p>
      <ErrorBox message={error} />
      <div className="actions">
        {c.allowed.change && (
          <button
            type="button"
            disabled={!can || busy}
            title={why}
            onClick={() => {
              void apply();
            }}
          >
            Apply now
          </button>
        )}
      </div>
    </fieldset>
  );
}

/** AddonConflict says an add-on is not installed because the cluster holds an install of that software. */
export function AddonConflict({ cluster: c }: { cluster: Cluster }): JSX.Element | null {
  const found = c.conditions.find((x) => x.type === addonConflictCondition && x.status === "True");
  if (found === undefined) {
    return null;
  }
  return (
    <div className="warning held-up">
      <strong>Add-on conflict</strong>
      {filled(found.message) && <Detail>{found.message}</Detail>}
    </div>
  );
}
