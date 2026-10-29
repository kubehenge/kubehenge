import { useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import { csiOffer, snapshotsOffer, type CSIOffer, type SnapshotsOffer } from "./create-form";
import type { Cluster } from "./types";
import { Dialog, ErrorBox, Info, useChange } from "./ui";
import { flag, record, text } from "./values";

/** csiOn reads whether the cluster runs the plugin's newest release. */
export const csiOn = (c: Cluster): boolean => flag(record(c.variables["csi"])["datastoreAllocate"]) === true;

/** snapshotsOn reads whether the cluster's CSI plugin takes volume snapshots. */
export const snapshotsOn = (c: Cluster): boolean => flag(record(c.variables["csi"])["volumeSnapshots"]) === true;

/** csiWith is the cluster's csi variable with one switch set, the others as they are. */
const csiWith = (c: Cluster, key: "datastoreAllocate" | "volumeSnapshots", on: boolean): Record<string, unknown> => ({
  ...record(c.variables["csi"]),
  [key]: on,
});

/** CSIField chooses the plugin's newest release; one with a problem can be switched off, not on. */
export function CSIField({
  checked,
  onChange,
  storage,
  problem,
}: {
  checked: boolean;
  onChange: (on: boolean) => void;
  storage: string | undefined;
  problem: string | undefined;
}): JSX.Element {
  const hint = useId();
  const reason = useId();
  const refused = problem !== undefined;
  return (
    <>
      <label className="check">
        <input
          type="checkbox"
          checked={checked}
          disabled={refused && !checked}
          aria-invalid={refused && checked}
          aria-describedby={refused ? `${hint} ${reason}` : hint}
          onChange={(e) => onChange(e.target.checked)}
        />{" "}
        Newest CSI plugin release
      </label>
      <Info about="the CSI plugin release">
        Without it, the cluster runs the last release that needs only Datastore.AllocateSpace.
      </Info>
      <p className="hint" id={hint}>
        Grants Datastore.Allocate on {storage ?? "its storage"}: delete volumes, change settings
      </p>
      {refused && (
        <p className={checked ? "field-error" : "hint"} id={reason} role={checked ? "alert" : undefined}>
          {problem}
        </p>
      )}
    </>
  );
}

/** useCSIOffer reads whether an existing cluster may choose the newest release, and what stands in the way. */
export function useCSIOffer(c: Cluster): CSIOffer {
  const [offer, setOffer] = useState<CSIOffer>({ offered: false, problem: undefined });
  const storage = text(c.variables["storage"]);
  const { class: className, namespace, name } = c;
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        const [classes, storages, clusters] = await Promise.all([
          api.classes(),
          api.storages(),
          // The controller refuses a shared storage in any case; the list only says so sooner.
          api.clusters().catch((): Cluster[] => []),
        ]);
        const cls = classes.find((x) => x.name === className);
        setOffer(csiOffer({ cls, storages, storage, clusters, self: { namespace, name } }));
      } catch {
        // Without the classes or storages the option is not shown; the cluster keeps its setting.
        setOffer({ offered: false, problem: undefined });
      }
    }
    void load();
  }, [className, namespace, name, storage]);
  return offer;
}

export function CSIDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [on, setOn] = useState(csiOn(c));
  const { problem } = useCSIOffer(c);
  const { busy, error, send } = useChange(c, onClose);
  // Switching it off needs nothing; switching it on, nothing in the way.
  const blocked = on && !csiOn(c) && problem !== undefined;
  return (
    <Dialog
      title="Edit CSI plugin"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { csi: csiWith(c, "datastoreAllocate", on) } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || blocked || on === csiOn(c)}>
            Save
          </button>
        </>
      }
    >
      <CSIField
        checked={on}
        onChange={setOn}
        storage={text(c.variables["storage"])}
        problem={csiOn(c) ? undefined : problem}
      />
      <ErrorBox message={error} />
    </Dialog>
  );
}

/** SnapshotsField chooses volume snapshots; one with a problem can be switched off, not on. */
export function SnapshotsField({
  checked,
  onChange,
  problem,
}: {
  checked: boolean;
  onChange: (on: boolean) => void;
  problem: string | undefined;
}): JSX.Element {
  const hint = useId();
  const reason = useId();
  const refused = problem !== undefined;
  return (
    <>
      <label className="check">
        <input
          type="checkbox"
          checked={checked}
          disabled={refused && !checked}
          aria-invalid={refused && checked}
          aria-describedby={refused ? `${hint} ${reason}` : hint}
          onChange={(e) => onChange(e.target.checked)}
        />{" "}
        Take volume snapshots
      </label>
      <Info about="volume snapshots">
        A proxy copies only the cluster&apos;s own volumes, with its own token. Needs ZFS, LVM, LVM-thin, directory
        or btrfs storage.
      </Info>
      <p className="hint" id={hint}>
        Copies stay on their volume&apos;s node
      </p>
      {refused && (
        <p className={checked ? "field-error" : "hint"} id={reason} role={checked ? "alert" : undefined}>
          {problem}
        </p>
      )}
    </>
  );
}

/** useSnapshotsOffer reads whether an existing cluster may take volume snapshots, and what stands in the way. */
export function useSnapshotsOffer(c: Cluster): SnapshotsOffer {
  const [offer, setOffer] = useState<SnapshotsOffer>({ offered: false, problem: undefined });
  const storage = text(c.variables["storage"]);
  const className = c.class;
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        const classes = await api.classes();
        setOffer(
          snapshotsOffer(
            classes.find((x) => x.name === className),
            storage,
          ),
        );
      } catch {
        // Without the classes the option is not shown; the cluster keeps its setting.
        setOffer({ offered: false, problem: undefined });
      }
    }
    void load();
  }, [className, storage]);
  return offer;
}

export function SnapshotsDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const [on, setOn] = useState(snapshotsOn(c));
  const { problem } = useSnapshotsOffer(c);
  const { busy, error, send } = useChange(c, onClose);
  // Switching it off needs nothing; switching it on, nothing in the way.
  const blocked = on && !snapshotsOn(c) && problem !== undefined;
  return (
    <Dialog
      title="Edit volume snapshots"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { csi: csiWith(c, "volumeSnapshots", on) } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || blocked || on === snapshotsOn(c)}>
            Save
          </button>
        </>
      }
    >
      <SnapshotsField checked={on} onChange={setOn} problem={snapshotsOn(c) ? undefined : problem} />
      <ErrorBox message={error} />
    </Dialog>
  );
}
