import { useState, type JSX } from "react";

import { api } from "./api";
import type { Cluster, EtcdMember, EtcdSizes } from "./types";
import { Dialog, ErrorBox, Field, Status, age, bytes, errorMessage, filled, useChange } from "./ui";
import { quantity, record } from "./values";

/** The bounds and default of the etcd variable's quotaGiB, as the chart's schema has them. */
export const minEtcdQuotaGiB = 2;
export const maxEtcdQuotaGiB = 8;
export const defaultEtcdQuotaGiB = 8;

/** The share of the limit at which the controller warns, and the whole in percent. */
const warnPercent = 60;
const wholePercent = 100;

/** etcdQuotaOf is the quota a cluster's etcd variable sets, in GiB. */
export function etcdQuotaOf(v: unknown): number {
  return quantity(record(v)["quotaGiB"]) ?? defaultEtcdQuotaGiB;
}

export const etcdText = (gib: number): string => `${gib} GiB database limit`;

/** etcdQuotaProblem is what is wrong with a quota typed into a field, or nothing. */
export function etcdQuotaProblem(value: string): string | undefined {
  const n = Number(value);
  if (value.trim() === "" || !Number.isInteger(n) || n < minEtcdQuotaGiB || n > maxEtcdQuotaGiB) {
    return `A whole number from ${minEtcdQuotaGiB} to ${maxEtcdQuotaGiB}`;
  }
  return undefined;
}

/** The quota can be raised and not lowered, which the class refuses once the cluster runs. */
export function EtcdDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = etcdQuotaOf(c.variables["etcd"]);
  const [value, setValue] = useState(String(current));
  const [touched, setTouched] = useState(false);
  const { busy, error, send } = useChange(c, onClose);
  const problem = etcdQuotaProblem(value);
  const lowered = problem === undefined && Number(value) < current;
  const shown = touched ? (problem ?? (lowered ? `Not lower than ${current}` : undefined)) : undefined;
  return (
    <Dialog
      title="Edit etcd"
      onClose={onClose}
      onSubmit={() => {
        setTouched(true);
        if (problem !== undefined || lowered) {
          return;
        }
        void send({ variables: { etcd: { quotaGiB: Number(value) } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || Number(value) === current}>
            Save
          </button>
        </>
      }
    >
      <Field label="Database limit (GiB)" hint="It can be raised, never lowered." error={shown}>
        {(id) => (
          <input
            id={id}
            type="number"
            min={minEtcdQuotaGiB}
            max={maxEtcdQuotaGiB}
            step={1}
            value={value}
            onChange={(e) => {
              setValue(e.target.value);
              setTouched(true);
            }}
          />
        )}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}

const percentOf = (part: number, whole: number): number => (whole > 0 ? Math.round((part * wholePercent) / whole) : 0);

function memberState(m: EtcdMember, quota: number): { state: "ok" | "error" | "warning"; text: string } {
  if (m.alarms?.includes("NOSPACE") === true) {
    return { state: "error", text: "Full" };
  }
  if ((m.errors?.length ?? 0) > 0) {
    return { state: "error", text: m.errors?.[0] ?? "Unwell" };
  }
  if (m.learner === true) {
    return { state: "warning", text: "Learner" };
  }
  const used = percentOf(m.dbSize, quota);
  return used >= warnPercent ? { state: "warning", text: `${used}% of the limit` } : { state: "ok", text: "Healthy" };
}

/** EtcdSizeView lists each etcd member's database against the limit, and defragments them on request. */
export function EtcdSizeView({
  cluster: c,
  can,
  onEdit,
  onChanged,
}: {
  cluster: Cluster;
  can: boolean;
  onEdit: (a: "etcd") => void;
  onChanged: () => void;
}): JSX.Element | null {
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string>();
  const sizes: EtcdSizes | undefined = c.etcd;
  const defragment = async (): Promise<void> => {
    setBusy(true);
    setActionError(undefined);
    try {
      await api.defragmentEtcd(c.namespace, c.name);
      onChanged();
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  if (sizes === undefined && !("etcd" in c.variables)) {
    return null;
  }
  return (
    <div className="box scroll">
      <h3>etcd size</h3>
      {sizes === undefined ? (
        <p className="muted">Not read yet</p>
      ) : (
        <>
          {filled(sizes.warning) && <p className="warning">{sizes.warning}</p>}
          {filled(sizes.error) && <p className="warning">{sizes.error}</p>}
          <table className="grid">
            <thead>
              <tr>
                <th>Member</th>
                <th>Database</th>
                <th>In use</th>
                <th>State</th>
              </tr>
            </thead>
            <tbody>
              {sizes.members.map((m) => {
                const s = memberState(m, sizes.quotaBytes);
                return (
                  <tr key={m.name}>
                    <td>
                      {m.name}
                      {m.leader === true && <span className="muted"> (leader)</span>}
                    </td>
                    <td>{bytes(m.dbSize)}</td>
                    <td>{bytes(m.dbSizeInUse)}</td>
                    <td>
                      <Status state={s.state} text={s.text} />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          <p className="muted">
            Limit {bytes(sizes.quotaBytes)}, read {age(sizes.read)} ago
          </p>
          {sizes.defrag !== undefined && (
            <p className={filled(sizes.defrag.error) ? "warning" : "muted"}>
              {filled(sizes.defrag.error)
                ? sizes.defrag.error
                : `Defragmented ${sizes.defrag.members.join(", ")} ${age(sizes.defrag.at)} ago`}
            </p>
          )}
        </>
      )}
      <ErrorBox message={actionError} />
      <div className="actions">
        <button
          type="button"
          disabled={busy || !can || sizes === undefined || sizes.requested === true}
          title={can ? undefined : "You cannot change it"}
          onClick={() => {
            void defragment();
          }}
        >
          {sizes?.requested === true ? "Defragmenting…" : "Defragment now"}
        </button>
        <button type="button" disabled={!can} onClick={() => onEdit("etcd")}>
          Edit limit…
        </button>
      </div>
    </div>
  );
}
