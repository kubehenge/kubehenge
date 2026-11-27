import { useId, type JSX } from "react";

import { api } from "./api";
import { csiOffer, csiOffered, snapshotsOffer, type CSIOffer, type SnapshotsOffer } from "./create-form";
import type { Cluster } from "./types";
import { Info, useClasses, useOnce, type Once } from "./ui";
import { flag, record, text } from "./values";

/** csiOn reads whether the cluster runs the plugin's newest release. */
export const csiOn = (c: Cluster): boolean => flag(record(c.variables["csi"])["datastoreAllocate"]) === true;

/** snapshotsOn reads whether the cluster's CSI plugin takes volume snapshots. */
export const snapshotsOn = (c: Cluster): boolean => flag(record(c.variables["csi"])["volumeSnapshots"]) === true;

/** csiWith is the cluster's csi variable with one switch set, the others as they are. */
export const csiWith = (
  c: Cluster,
  key: "datastoreAllocate" | "volumeSnapshots",
  on: boolean,
): Record<string, unknown> => ({
  ...record(c.variables["csi"]),
  [key]: on,
});

/** CSIField chooses the plugin's newest release. One with a problem can be switched off, not on. */
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

// anyClusters is the clusters, none where they cannot be read.
async function anyClusters(): Promise<Cluster[]> {
  try {
    return await api.clusters();
  } catch {
    return [];
  }
}

/** useCSIOffer reads whether an existing cluster may choose the newest release, and what stands in the way. */
export function useCSIOffer(c: Cluster): CSIOffer & { unread?: Once<unknown> } {
  const storages = useOnce(api.storages);
  // The controller refuses a shared storage in any case. The list only says so sooner.
  const clusters = useOnce(anyClusters);
  // Without the classes the option is not shown, the page shows that read's error, and the cluster keeps its setting.
  const cls = useClasses().data?.find((x) => x.name === c.class);
  // A class that offers the release waits on the storages, so its row shows that read instead of dropping.
  if (csiOffered(cls) && (storages.data === undefined || clusters.data === undefined)) {
    return { offered: false, problem: undefined, unread: storages.data === undefined ? storages : clusters };
  }
  return csiOffer({
    cls,
    storages: storages.data ?? [],
    storage: text(c.variables["storage"]),
    clusters: clusters.data ?? [],
    self: { namespace: c.namespace, name: c.name },
  });
}

/** SnapshotsField chooses volume snapshots. One with a problem can be switched off, not on. */
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
        A proxy copies only the cluster&apos;s own volumes, with its own token. Needs ZFS, LVM, LVM-thin, directory or
        btrfs storage.
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
export function useSnapshotsOffer(c: Cluster): SnapshotsOffer & { unread?: Once<unknown> } {
  // Until the classes answer, the row shows that read instead of dropping. The cluster keeps its setting.
  const classes = useClasses();
  const { data } = classes;
  if (data === undefined) {
    return { offered: false, problem: undefined, unread: classes };
  }
  return snapshotsOffer(
    data.find((x) => x.name === c.class),
    text(c.variables["storage"]),
  );
}
