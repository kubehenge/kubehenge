import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import { PlanSteps, restoreState } from "./EtcdSnapshots";
import type { Cluster, Condition, ControlPlaneRecovery } from "./types";
import { Dialog, ErrorBox, Field, Status, age, errorMessage, filled, sentence } from "./ui";

/** The condition Kubehenge's controller sets on a cluster whose only control plane's roll stalled. */
export const rollStalledCondition = "ControlPlaneRollStalled";

// The controller's message ends in the kubectl command that does what Recover does; the page
// offers the button, and the command beside it.
const recoverOr = " with Recover on the cluster's page or: ";

/** rollStall is the cluster's stalled-roll condition while it holds. */
export function rollStall(c: Cluster): Condition | undefined {
  return c.conditions.find((x) => x.type === rollStalledCondition && x.status === "True");
}

/** stallText parts the controller's message into what happened and the kubectl command. */
export function stallText(message: string): { said: string; command?: string } {
  const at = message.indexOf(recoverOr);
  if (at === -1) {
    return { said: sentence(message) };
  }
  return { said: sentence(message.slice(0, at).replace(/,$/u, "")), command: message.slice(at + recoverOr.length) };
}

// The control-plane machines that can be kept, the oldest first: the one a roll replaces.
function keepable(c: Cluster): { name: string; ready: boolean; created: string }[] {
  return (c.machines ?? [])
    .filter((m) => m.role === "control-plane" && m.phase !== "Deleting")
    .toSorted((a, b) => a.created.localeCompare(b.created))
    .map((m) => ({ name: m.name, ready: m.ready, created: m.created }));
}

/** RollStalled says that the roll of a cluster's only control plane stalled and offers the way out. */
export function RollStalled({
  cluster: c,
  why,
  onRecovered,
}: {
  cluster: Cluster;
  why: string | undefined;
  onRecovered: () => void;
}): JSX.Element | null {
  const [recovering, setRecovering] = useState(false);
  const stall = rollStall(c);
  if (stall === undefined) {
    return null;
  }
  const { said, command } = stallText(stall.message ?? "");
  const down = stall.reason === "APIUnreachable";
  const successor = keepable(c).findLast((m) => !m.ready);
  // Recover takes what an etcd restore takes: Sys.Modify on the pool, which the kubeconfig needs too.
  const allowed = c.allowed.kubeconfig && !c.deleting;
  return (
    <section aria-label="Stalled control-plane roll" className={`roll-stalled ${down ? "roll-down" : "roll-held"}`}>
      <p className="roll-title">{down ? "Control-plane roll stalled, API down" : "Control-plane roll stalled"}</p>
      {successor !== undefined && (
        <p className="roll-said">
          <span className="id">{successor.name}</span> not ready for {age(successor.created)}.
        </p>
      )}
      <div className="roll-actions">
        <details className="advanced">
          <summary>Details</summary>
          <p className="roll-said">{said}</p>
          {command !== undefined && (
            <pre className="restore-command" aria-label="Recover with kubectl">
              {command}
            </pre>
          )}
        </details>
        <button
          type="button"
          className="danger"
          disabled={!allowed}
          title={allowed ? undefined : (why ?? "Needs Sys.Modify on the pool")}
          onClick={() => setRecovering(true)}
        >
          Recover…
        </button>
      </div>
      {recovering && (
        <RecoverDialog
          cluster={c}
          onClose={() => setRecovering(false)}
          onDone={() => {
            setRecovering(false);
            onRecovered();
          }}
        />
      )}
    </section>
  );
}

/** RecoveryState is how the last recovery of the control plane went, or is going. */
export function RecoveryState({ cluster: c }: { cluster: Cluster }): JSX.Element | null {
  const r = c.recovery;
  if (r === undefined) {
    return null;
  }
  const s = restoreState(r);
  const done = r.phase === "Succeeded" || r.phase === "Failed";
  return (
    <dl className="summary">
      <dt>Recovery</dt>
      <dd>
        <Status state={s.state} text={s.text} />
        <span className="muted" title={r.message}>
          {" "}
          {done && `kept ${r.machine ?? "a control plane"}, `}
          {new Date(r.finished ?? r.started).toLocaleString()}
          {filled(r.by) && `, by ${r.by}`}
        </span>
        {r.phase === "Failed" && filled(r.message) && <div className="hint message">{sentence(r.message)}</div>}
      </dd>
    </dl>
  );
}

function RecoverDialog({
  cluster: c,
  onClose,
  onDone,
}: {
  cluster: Cluster;
  onClose: () => void;
  onDone: () => void;
}): JSX.Element {
  const machines = keepable(c);
  const [keep, setKeep] = useState(machines[0]?.name ?? "");
  // What the server said, with the machine it was asked about, so that a newly picked machine shows
  // as being checked until its own answer comes.
  const [answer, setAnswer] = useState<{ keep: string; how: ControlPlaneRecovery }>();
  const how = answer?.keep === keep ? answer.how : undefined;
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const got = await api.controlPlaneRecovery(c.namespace, c.name, keep);
        if (current) {
          setAnswer({ keep, how: got });
          setError(undefined);
        }
      } catch (err) {
        if (current) {
          setError(errorMessage(err));
        }
      }
    }
    if (keep !== "") {
      void load();
    }
    return () => {
      current = false;
    };
  }, [c.namespace, c.name, keep]);
  const recover = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.recoverControlPlane(c, { machine: keep, confirm: typed });
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  const others = machines.filter((m) => m.name !== keep).map((m) => m.name);
  const automatic = how?.automatic === true;
  return (
    <Dialog
      title={`Recover ${c.name}'s control plane`}
      onClose={onClose}
      onSubmit={() => {
        if (automatic) {
          void recover();
        }
      }}
      actions={
        automatic || how === undefined ? (
          <>
            <button type="button" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="danger" disabled={busy || !automatic || typed !== c.name || keep === ""}>
              Recover
            </button>
          </>
        ) : (
          <button type="button" onClick={onClose}>
            Close
          </button>
        )
      }
    >
      <p className="warning">
        Replaces the other control planes. If the API is down, etcd is reset to the kept machine. Data only the others
        held is lost.
      </p>
      <Field label="Keep">
        {(id) => (
          <select id={id} value={keep} onChange={(e) => setKeep(e.target.value)}>
            {machines.map((m) => (
              <option key={m.name} value={m.name}>
                {m.name}
                {m.ready ? " (ready)" : " (not ready)"}
              </option>
            ))}
          </select>
        )}
      </Field>
      {others.length > 0 && (
        <p className="muted">
          Replaced: <span className="id">{others.join(", ")}</span>
        </p>
      )}
      {how === undefined && !filled(error) && <p className="muted">Checking…</p>}
      {automatic && (
        <Field label="Confirm name" hint={`Type ${c.name}.`}>
          {(id) => <input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />}
        </Field>
      )}
      {how?.plan !== undefined && (
        <div className="restore-steps">
          <p>Run these steps by hand.</p>
          <PlanSteps plan={how.plan} />
        </div>
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}
