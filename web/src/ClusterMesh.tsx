import { useEffect, useId, useState, type JSX } from "react";

import { api } from "./api";
import type { Cluster, MeshPeer } from "./types";
import { Dialog, ErrorBox, errorMessage, filled, useChange } from "./ui";
import { flag, record, text, texts } from "./values";

/** A cluster's clusterMesh variable. */
export interface Mesh {
  enabled: boolean;
  peers: string[];
}

export function meshOf(v: unknown): Mesh {
  const r = record(v);
  return { enabled: flag(r["enabled"]) === true, peers: texts(r["peers"]) ?? [] };
}

/** runsCilium tells whether a cluster runs Cilium, which Cluster Mesh links clusters of. */
export const runsCilium = (c: Cluster): boolean => text(c.variables["cni"]) === "cilium";

/** meshHint says what Cluster Mesh does, and what a link lets each side read. */
export const meshHint =
  "Cilium Cluster Mesh: pods reach those of the linked clusters, by address and through global services, and Cilium's network policies can select them. Two clusters of one namespace are linked while both are on and list each other, with pod ranges apart; each then reads the other's pod names, labels and identities.";

/** meshText says in a line whether a cluster is in a mesh, and what it is linked with. */
export function meshText(c: Cluster): string {
  if (!meshOf(c.variables["clusterMesh"]).enabled) {
    return "Off";
  }
  const peers = c.mesh ?? [];
  const linked = peers.filter((p) => !filled(p.refused)).map((p) => p.name);
  const waiting = peers.filter((p) => filled(p.refused)).map((p) => p.name);
  if (peers.length === 0) {
    return "On, no peers chosen";
  }
  if (linked.length === 0) {
    return `On, not yet linked with ${waiting.join(", ")}`;
  }
  const parts = [`linked with ${linked.join(", ")}`];
  if (waiting.length > 0) {
    parts.push(`not yet with ${waiting.join(", ")}`);
  }
  return `On, ${parts.join("; ")}`;
}

/** standingText says how a listed peer stands. */
function standingText(standing: MeshPeer[] | undefined, name: string): string | undefined {
  const s = standing?.find((p) => p.name === name);
  if (s === undefined) {
    return undefined;
  }
  return filled(s.refused) ? `not linked: ${s.refused}` : "linked";
}

/**
 * MeshPeersField chooses the clusters to link with: the other clusters of the namespace that run
 * Cilium, and those already listed, which stay offered even where the user cannot see them.
 */
export function MeshPeersField({
  namespace,
  self,
  value,
  standing,
  onChange,
}: {
  namespace: string;
  self: string;
  value: string[];
  standing?: MeshPeer[] | undefined;
  onChange: (peers: string[]) => void;
}): JSX.Element {
  const [clusters, setClusters] = useState<Cluster[]>();
  const [error, setError] = useState<string>();
  useEffect(() => {
    async function load(): Promise<void> {
      try {
        setClusters(await api.clusters());
      } catch (err) {
        setError(errorMessage(err));
      }
    }
    void load();
  }, []);
  const others = (clusters ?? []).filter((c) => c.namespace === namespace && c.name !== self && runsCilium(c));
  const names = [...new Set([...others.map((c) => c.name), ...value])].toSorted();
  return (
    <fieldset className="group">
      <legend>Peers</legend>
      {clusters === undefined && !filled(error) && <p className="muted">Loading…</p>}
      {clusters !== undefined && names.length === 0 && (
        <p className="muted">No other cluster of this namespace runs Cilium yet.</p>
      )}
      {names.map((name) => {
        const standingNote = standingText(standing, name);
        return (
          <label key={name} className="check">
            <input
              type="checkbox"
              checked={value.includes(name)}
              onChange={(e) => onChange(e.target.checked ? [...value, name] : value.filter((x) => x !== name))}
            />{" "}
            {name}
            {standingNote !== undefined && <span className="muted"> ({standingNote})</span>}
          </label>
        );
      })}
      <ErrorBox message={error} />
    </fieldset>
  );
}

export function ClusterMeshDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = meshOf(c.variables["clusterMesh"]);
  const [value, setValue] = useState(current);
  const { busy, error, send } = useChange(c, onClose);
  const hint = useId();
  const peers = value.enabled ? value.peers.toSorted() : [];
  const changed =
    value.enabled !== current.enabled || JSON.stringify(peers) !== JSON.stringify(current.peers.toSorted());
  return (
    <Dialog
      title="Cluster Mesh"
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { clusterMesh: value.enabled ? { enabled: true, peers } : { enabled: false } } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || !changed}>
            Save
          </button>
        </>
      }
    >
      <label className="check">
        <input
          type="checkbox"
          checked={value.enabled}
          aria-describedby={hint}
          onChange={(e) => setValue({ ...value, enabled: e.target.checked })}
        />{" "}
        Join a Cluster Mesh
      </label>
      <p className="hint" id={hint}>
        {meshHint} Peers come and go without restarting anything.
      </p>
      {value.enabled && (
        <MeshPeersField
          namespace={c.namespace}
          self={c.name}
          value={value.peers}
          standing={c.mesh}
          onChange={(p) => setValue({ ...value, peers: p })}
        />
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}
