import { useState, type JSX } from "react";

import { api } from "./api";
import type { ReleasePin } from "./types";
import { ErrorBox, Field, errorMessage, filled, pollIdle, usePoll } from "./ui";

// ReleasePins lists the RKE2 and k3s releases an administrator pinned beside the ones this Kubehenge
// carries. A release is pinned only after its digests are shown.
export function ReleasePins(): JSX.Element {
  const { data, error, reload } = usePoll(api.releasePins, () => pollIdle);
  const [version, setVersion] = useState("");
  const [shown, setShown] = useState<ReleasePin>();
  const [problem, setProblem] = useState("");
  const [busy, setBusy] = useState(false);

  async function run(step: () => Promise<void>): Promise<void> {
    setBusy(true);
    setProblem("");
    try {
      await step();
    } catch (e) {
      setProblem(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="box">
      <h3>Release pins</h3>
      <ErrorBox message={error} />
      {data && data.pins.length === 0 && <p>No releases pinned</p>}
      {data && data.pins.length > 0 && (
        <table className="grid">
          <thead>
            <tr>
              <th>Release</th>
              <th>Pinned by</th>
              <th>
                <span className="visually-hidden">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {data.pins.map((p) => (
              <tr key={p.version}>
                <td>
                  {p.version}
                  {filled(p.inUse) && <div className="muted">In use by {p.inUse}</div>}
                </td>
                <td>
                  {p.by ?? "Unknown"}
                  {filled(p.at) && <div className="muted">{new Date(p.at).toLocaleDateString()}</div>}
                </td>
                <td className="row-action">
                  <button
                    type="button"
                    disabled={filled(p.inUse) || busy}
                    aria-label={`Unpin ${p.version}`}
                    title={filled(p.inUse) ? `In use by ${p.inUse}` : undefined}
                    onClick={() =>
                      void run(async () => {
                        await api.removeReleasePin(p.version);
                        await reload();
                      })
                    }
                  >
                    Unpin
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {data?.checks === true && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void run(async () => {
              setShown(await api.checkReleasePin(version.trim()));
            });
          }}
        >
          <Field label="Release">
            {(id) => (
              <input
                id={id}
                value={version}
                placeholder="v1.36.6+rke2r1"
                onChange={(e) => {
                  setVersion(e.target.value);
                  setShown(undefined);
                }}
              />
            )}
          </Field>
          <div className="actions">
            <button type="submit" disabled={busy || !filled(version.trim())}>
              Check release
            </button>
          </div>
        </form>
      )}
      {shown && (
        <>
          <dl className="summary" aria-label="Digests">
            <dt>Checksum file</dt>
            <dd>
              <code>{shown.file}</code>
            </dd>
            <dt>Install script</dt>
            <dd>
              <code>{shown.script}</code>
            </dd>
          </dl>
          <div className="actions">
            <button
              type="button"
              className="primary"
              disabled={busy}
              onClick={() =>
                void run(async () => {
                  await api.addReleasePin(shown);
                  setShown(undefined);
                  setVersion("");
                  await reload();
                })
              }
            >
              Pin {shown.version}
            </button>
          </div>
        </>
      )}
      {filled(problem) && <ErrorBox message={problem} />}
    </div>
  );
}
