import { Fragment, useEffect, useState, type JSX } from "react";

import { api } from "./api";
import type { Cluster, MeshPeer } from "./types";
import { Detail, Dialog, ErrorBox, Info, errorMessage, filled, useChange } from "./ui";
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

/** meshInfo says what Cluster Mesh links, and what a link lets each side read. */
export const meshInfo =
  "Links clusters of one namespace that list each other. They reach each other's pods and read their labels and identities.";

/** meshText says in a line whether a cluster is in a mesh, and what it is linked with. */
export function meshText(c: Cluster): string {
  if (!meshOf(c.variables["clusterMesh"]).enabled) {
    return "Off";
  }
  const peers = c.mesh ?? [];
  const linked = peers.filter((p) => !filled(p.refused)).map((p) => p.name);
  const waiting = peers.filter((p) => filled(p.refused)).map((p) => p.name);
  if (peers.length === 0) {
    return "On, no peers";
  }
  const parts = [
    ...(linked.length > 0 ? [`linked with ${linked.join(", ")}`] : []),
    ...(waiting.length > 0 ? [`waiting for ${waiting.join(", ")}`] : []),
  ];
  return `On, ${parts.join(", ")}`;
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
        <p className="muted">No other Cilium clusters in this namespace</p>
      )}
      {names.map((name) => {
        const s = standing?.find((p) => p.name === name);
        return (
          <Fragment key={name}>
            <label className="check">
              <input
                type="checkbox"
                checked={value.includes(name)}
                onChange={(e) => onChange(e.target.checked ? [...value, name] : value.filter((x) => x !== name))}
              />{" "}
              {name}
              {s !== undefined && <span className="muted"> ({filled(s.refused) ? "Not linked" : "Linked"})</span>}
            </label>
            {s !== undefined && filled(s.refused) && <Detail>{s.refused}</Detail>}
          </Fragment>
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
  const peers = value.enabled ? value.peers.toSorted() : [];
  const changed =
    value.enabled !== current.enabled || JSON.stringify(peers) !== JSON.stringify(current.peers.toSorted());
  return (
    <Dialog
      title="Edit Cluster Mesh"
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
          onChange={(e) => setValue({ ...value, enabled: e.target.checked })}
        />{" "}
        Join a Cluster Mesh
      </label>
      <Info about="Cluster Mesh">{meshInfo}</Info>
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
