import { useRef, useState, type JSX } from "react";

import { api } from "./api";
import type { BootLog, ClusterRef, Machine } from "./types";
import { Dialog, ErrorBox, errorMessage, filled, pollIdle, pollRunning, usePoll } from "./ui";

/**
 * BootLogDialog has the controller read a machine's boot log, through its VM's guest agent or its
 * Talos API, and shows it once read: what a node that never joined says about why.
 */
export function BootLogDialog({
  cluster,
  machine,
  onClose,
}: {
  cluster: ClusterRef;
  machine: Machine;
  onClose: () => void;
}): JSX.Element {
  // Opening the dialog asks for a fresh read. The polls after it wait for the controller's answer.
  const asking = useRef(true);
  const [askError, setAskError] = useState<string>();
  const { data, error, reload } = usePoll(
    (): Promise<BootLog> => {
      if (asking.current) {
        asking.current = false;
        return api.askBootLog(cluster.namespace, cluster.name, machine.name);
      }
      return api.bootLog(cluster.namespace, cluster.name, machine.name);
    },
    (shown) => (shown?.asked === false ? pollIdle : pollRunning),
  );
  const readAgain = async (): Promise<void> => {
    setAskError(undefined);
    try {
      await api.askBootLog(cluster.namespace, cluster.name, machine.name);
      await reload();
    } catch (err) {
      setAskError(errorMessage(err));
    }
  };
  const log = data?.log;
  return (
    <Dialog
      title={`Boot log of ${machine.node ?? machine.name}`}
      onClose={onClose}
      actions={
        <>
          <button
            type="button"
            disabled={data?.asked !== false}
            onClick={() => {
              void readAgain();
            }}
          >
            Read again
          </button>
          <button type="button" onClick={onClose}>
            Close
          </button>
        </>
      }
    >
      <ErrorBox message={error} />
      <ErrorBox message={askError} />
      {data?.asked === true && <p className="muted">Reading…</p>}
      {log !== undefined && <p className="muted">Read {new Date(log.read).toLocaleString()}</p>}
      <ErrorBox message={log?.error} />
      {(log?.sections ?? []).map((s) => (
        <section key={s.name} className="boot-log-part" aria-label={s.name}>
          <h3>{s.name}</h3>
          <ErrorBox message={s.error} />
          {(!filled(s.error) || (s.lines ?? []).length > 0) && (
            <pre className="log pod-log" aria-label={`Log of ${s.name}`}>
              {(s.lines ?? []).length === 0 ? "Nothing logged" : (s.lines ?? []).join("\n")}
            </pre>
          )}
        </section>
      ))}
    </Dialog>
  );
}
