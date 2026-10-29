import { useState, type JSX } from "react";

import { api } from "./api";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Field, age, errorMessage, filled } from "./ui";

/** The condition Kubehenge's controller sets on a cluster whose deletion waited too long. */
export const deletionStalledCondition = "DeletionStalled";

/** Deleting says what a cluster's deletion waits on, and offers to abandon it. */
export function Deleting({
  cluster: c,
  onAbandoned,
}: {
  cluster: Cluster;
  onAbandoned: () => void;
}): JSX.Element | null {
  const [abandoning, setAbandoning] = useState(false);
  if (!c.deleting) {
    return null;
  }
  const d = c.deletion ?? {};
  const stalled = c.conditions.some((x) => x.type === deletionStalledCondition && x.status === "True");
  const abandoned = d.abandoned !== undefined;
  const waiting = d.waiting ?? [];
  let title = "Deleting";
  if (abandoned) {
    title = "Abandoning deletion";
  } else if (stalled) {
    title = "Deletion stalled";
  }
  return (
    <section aria-label="Deletion" className={stalled || abandoned ? "callout" : "callout callout-run"}>
      <p className="callout-title">
        {title}
        {c.deletedAt !== undefined && <span className="muted"> for {age(c.deletedAt)}</span>}
      </p>
      {waiting.length > 0 && (
        <ul className="callout-list" aria-label="Waiting on">
          {waiting.map((w) => (
            <li key={`${w.kind}/${w.name}`}>
              <span className="id">
                {w.kind} {w.name}
              </span>
              {filled(w.reason) && <span className="muted"> {w.reason}</span>}
            </li>
          ))}
        </ul>
      )}
      {(d.leftBehind ?? []).length > 0 && (
        <p className="callout-said">
          Left behind: <span className="id">{(d.leftBehind ?? []).join(", ")}</span>
        </p>
      )}
      {!abandoned && (
        <div className="callout-actions callout-end">
          <button
            type="button"
            className="danger"
            disabled={!c.allowed.change}
            title={c.allowed.change ? undefined : "Needs VM.Allocate on the pool"}
            onClick={() => setAbandoning(true)}
          >
            Abandon…
          </button>
        </div>
      )}
      {abandoning && (
        <AbandonDialog
          cluster={c}
          onClose={() => setAbandoning(false)}
          onDone={() => {
            setAbandoning(false);
            onAbandoned();
          }}
        />
      )}
    </section>
  );
}

function AbandonDialog({
  cluster: c,
  onClose,
  onDone,
}: {
  cluster: Cluster;
  onClose: () => void;
  onDone: () => void;
}): JSX.Element {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const mayLeave = c.deletion?.mayLeave ?? [];
  const abandon = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.abandonDeletion(c, typed);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={`Abandon ${c.name}'s deletion`}
      onClose={onClose}
      onSubmit={() => {
        void abandon();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={busy || typed !== c.name}>
            Abandon
          </button>
        </>
      }
    >
      <p className="warning">Stops waiting. What is not deleted within 5 minutes is left behind.</p>
      {mayLeave.length > 0 && (
        <div className="may-leave">
          <p className="muted">May be left behind</p>
          <ul aria-label="May be left behind">
            {mayLeave.map((m) => (
              <li key={m} className="id">
                {m}
              </li>
            ))}
          </ul>
        </div>
      )}
      <Field label="Confirm name" hint={`Type ${c.name}.`}>
        {(id) => <input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
