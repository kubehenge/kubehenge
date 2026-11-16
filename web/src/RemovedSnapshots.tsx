import { useState, type JSX } from "react";

import { api } from "./api";
import { when } from "./backup-runs";
import type { RemovedCluster } from "./types";
import { Dialog, ErrorBox, Field, errorMessage, usePoll } from "./ui";

const pollRemoved = 60_000;

/**
 * RemovedSnapshots lists the removed clusters of a namespace whose etcd snapshots the backup volume
 * keeps, and deletes them with their backup keys once a name is typed. It shows nothing while none
 * are kept.
 */
export function RemovedSnapshots({ namespace }: { namespace: string }): JSX.Element | null {
  const { data: removed, reload } = usePoll(
    () => api.removedClusters(namespace),
    () => pollRemoved,
  );
  const [deleting, setDeleting] = useState<RemovedCluster>();
  if (removed === undefined || removed.length === 0) {
    return null;
  }
  return (
    <div className="box">
      <h3>Deleted clusters</h3>
      <table>
        <thead>
          <tr>
            <th scope="col">Cluster</th>
            <th scope="col">Snapshots</th>
            <th scope="col">Newest</th>
            <th scope="col">
              <span className="visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {removed.map((r) => (
            <tr key={r.name}>
              <td>{r.name}</td>
              <td>{r.snapshots.length}</td>
              <td>{r.snapshots[0] === undefined ? "None" : when(r.snapshots[0].time)}</td>
              <td>
                <button
                  type="button"
                  className="danger"
                  aria-label={`Delete ${r.name}'s snapshots`}
                  onClick={() => setDeleting(r)}
                >
                  Delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {deleting !== undefined && (
        <DeleteRemovedDialog
          removed={deleting}
          onClose={() => setDeleting(undefined)}
          onDone={() => {
            setDeleting(undefined);
            void reload();
          }}
        />
      )}
    </div>
  );
}

function DeleteRemovedDialog({
  removed: r,
  onClose,
  onDone,
}: {
  removed: RemovedCluster;
  onClose: () => void;
  onDone: () => void;
}): JSX.Element {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const remove = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.deleteRemoved(r.namespace, r.name, typed);
      onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={`Delete ${r.name}'s snapshots`}
      onClose={onClose}
      onSubmit={() => {
        void remove();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="danger" disabled={busy || typed !== r.name}>
            Delete snapshots
          </button>
        </>
      }
    >
      <p className="warning">Deletes its snapshots and backup keys. This can&apos;t be undone.</p>
      <Field label="Confirm name" hint={`Type ${r.name}.`}>
        {(id) => <input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
