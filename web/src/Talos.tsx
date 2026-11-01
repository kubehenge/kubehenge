import { useEffect, useState, type JSX } from "react";

import { api } from "./api";
import type { Cluster, TalosReleases, TalosStatus, Versions } from "./types";
import { Detail, Dialog, ErrorBox, Field, Status, errorMessage, filled, sentence, useChange } from "./ui";
import { text } from "./values";

/** talosRelease is what a Talos release looks like, such as v1.14.4. */
export const talosRelease = /^v1\.\d+\.\d+$/u;

/** talosExample is a Talos release written as the fields ask for one. */
export const talosExample = "v1.14.4";

function parts(v: string): { minor: number; patch: number } | undefined {
  const g = /^v1\.(?<minor>\d+)\.(?<patch>\d+)$/u.exec(v)?.groups;
  return g ? { minor: Number(g["minor"]), patch: Number(g["patch"]) } : undefined;
}

const none = { minor: -1, patch: -1 };

function newestFirst(a: string, b: string): number {
  const x = parts(a) ?? none;
  const y = parts(b) ?? none;
  return y.minor - x.minor || y.patch - x.patch;
}

/**
 * offeredReleases are the Talos releases a cluster or a template can name: those with a template,
 * those the Image Factory builds and the default, newest first.
 */
export function offeredReleases(r: TalosReleases): string[] {
  return [...new Set([...r.templates, ...r.releases, ...(filled(r.default) ? [r.default] : [])])].toSorted(newestFirst);
}

/** kubernetesMinor is a Kubernetes version's minor as Talos's table names it: "1.36" for v1.36.5. */
export const kubernetesMinor = (v: string): string => /^v?(?<minor>\d+\.\d+)/u.exec(v)?.groups?.["minor"] ?? v;

/**
 * runsKubernetes tells whether a Talos release runs a Kubernetes version, by the table the API
 * hands out. A release the table does not name is left to the server to judge.
 */
export function runsKubernetes(talos: TalosReleases | undefined, release: string, version: string): boolean {
  const minors = talos?.kubernetes[release];
  return minors === undefined || minors.includes(kubernetesMinor(version));
}

/** forRelease narrows the Kubernetes versions offered to those a Talos release runs. */
export function forRelease(
  offered: Versions | undefined,
  talos: TalosReleases | undefined,
  release: string,
): Versions | undefined {
  if (offered === undefined || talos?.kubernetes[release] === undefined) {
    return offered;
  }
  const versions = offered.versions.filter((v) => runsKubernetes(talos, release, v));
  return versions.length > 0
    ? { ...offered, versions }
    : { versions, error: `Talos ${release} runs none of the Kubernetes versions offered` };
}

/** missingReleases are the offered releases the template pool has no template of. */
export const missingReleases = (r: TalosReleases): string[] =>
  offeredReleases(r).filter((v) => !r.templates.includes(v));

/**
 * releaseToMake is the release a new template is made of unless another is chosen: the default
 * while it has none, else the newest release without one, as remaking a release's template adds
 * nothing.
 */
export function releaseToMake(r: TalosReleases): string {
  if (!filled(r.default) || !r.templates.includes(r.default)) {
    return r.default;
  }
  return missingReleases(r)[0] ?? r.default;
}

/**
 * loadingReleases stands in for a release field while the releases load, so that it is not typed
 * into first. A plain element, so that Field ties its hint to it.
 */
export const loadingReleases = (id: string): JSX.Element => (
  <select id={id} disabled value="">
    <option value="">Loading…</option>
  </select>
);

/** useTalosReleases reads the Talos releases once they are wanted. A failed read offers none and says why. */
export function useTalosReleases(wanted: boolean): TalosReleases | undefined {
  const [releases, setReleases] = useState<TalosReleases>();
  useEffect(() => {
    let current = true;
    async function load(): Promise<void> {
      try {
        const r = await api.talosReleases();
        if (current) {
          setReleases(r);
        }
      } catch (err) {
        if (current) {
          setReleases({ templates: [], releases: [], default: "", kubernetes: {}, error: errorMessage(err) });
        }
      }
    }
    if (wanted) {
      void load();
    }
    return () => {
      current = false;
    };
  }, [wanted]);
  return releases;
}

/** talosReleaseHint says why the wizard's list of Talos releases may be short. */
export function talosReleaseHint(releases: TalosReleases | undefined): string | undefined {
  const reason = releases?.error;
  return filled(reason) ? sentence(reason) : undefined;
}

/** TalosReleaseSelect chooses a new cluster's Talos release: one the template pool has a template of. */
export function TalosReleaseSelect({
  id,
  value,
  releases,
  onChange,
}: {
  id: string;
  value: string;
  releases: TalosReleases | undefined;
  onChange: (v: string) => void;
}): JSX.Element {
  if (!releases) {
    return loadingReleases(id);
  }
  return (
    <select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="" disabled>
        {releases.templates.length > 0 ? "Choose…" : "No templates yet"}
      </option>
      {offeredReleases(releases).map((v) => {
        const built = releases.templates.includes(v);
        return (
          <option key={v} value={v} disabled={!built}>
            {built ? v : `${v} (no template yet)`}
          </option>
        );
      })}
    </select>
  );
}

const shownTime = (at: string): string => (Number.isNaN(Date.parse(at)) ? at : new Date(at).toLocaleString());

/** upgradingText reads "<node> to <release> since <time>[, rebooted at <time>]" in the reader's own terms. */
export function upgradingText(s: string): string {
  const g = /^(?<node>\S+) to (?<to>\S+) since (?<since>[^\s,]+)(?:, rebooted at (?<rebooted>\S+))?$/u.exec(s)?.groups;
  const { node, to, since, rebooted } = g ?? {};
  if (node === undefined || to === undefined || since === undefined) {
    return s;
  }
  return `${node} to ${to}, since ${shownTime(since)}${rebooted === undefined ? "" : `, rebooted ${shownTime(rebooted)}`}`;
}

/** upgradedText reads "<node> to <release> at <time>" in the reader's own terms. */
export function upgradedText(s: string): string {
  const { node, to, at } = /^(?<node>\S+) to (?<to>\S+) at (?<at>\S+)$/u.exec(s)?.groups ?? {};
  if (node === undefined || to === undefined || at === undefined) {
    return s;
  }
  return `${node} to ${to}, ${shownTime(at)}`;
}

/** failedText reads "<node> to <release> at <time>: <why>" as a short state and the detail behind it. */
export function failedText(s: string): { state: string; detail: string } {
  const { node, to, at, why } = /^(?<node>\S+) to (?<to>\S+) at (?<at>\S+): (?<why>.+)$/su.exec(s)?.groups ?? {};
  if (node === undefined || to === undefined || at === undefined || why === undefined) {
    return { state: "Talos upgrade failed", detail: s };
  }
  return { state: `Couldn't upgrade ${node} to Talos ${to}`, detail: `${shownTime(at)}: ${why}` };
}

/** TalosRows say, among a cluster's summary, which Talos release its nodes run and how its upgrades stand. */
export function TalosRows({ talos }: { talos: TalosStatus }): JSX.Element {
  return (
    <>
      <dt>Talos release</dt>
      <dd>{talos.version}</dd>
      {filled(talos.image) && talos.image !== talos.version && (
        <>
          <dt>New nodes boot</dt>
          <dd>
            {talos.image}, then {talos.version}
          </dd>
        </>
      )}
      {filled(talos.upgrading) && (
        <>
          <dt>Talos upgrade</dt>
          <dd>
            <Status state="progress" text={upgradingText(talos.upgrading)} />
          </dd>
        </>
      )}
      {filled(talos.upgraded) && (
        <>
          <dt>Last Talos upgrade</dt>
          <dd>{upgradedText(talos.upgraded)}</dd>
        </>
      )}
    </>
  );
}

/** TalosFailure says which node upgrade failed and why, and has the controller try it again. */
export function TalosFailure({
  cluster: c,
  failed,
  can,
  why,
  onRetried,
}: {
  cluster: Cluster;
  failed: string;
  can: boolean;
  why: string | undefined;
  onRetried: () => void;
}): JSX.Element {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const retry = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.retryTalosUpgrade(c.namespace, c.name);
      onRetried();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  const f = failedText(failed);
  return (
    <section aria-label="Failed Talos upgrade">
      <div className="error">
        {f.state}. The other nodes wait. <Detail>{f.detail}</Detail>
      </div>
      <ErrorBox message={error} />
      <div className="actions">
        <button
          type="button"
          disabled={!can || busy}
          title={why}
          onClick={() => {
            void retry();
          }}
        >
          Try again
        </button>
      </div>
    </section>
  );
}

/** talosTargets are the releases a cluster's nodes may move to: a newer one of its minor version, or the next minor's. */
export function talosTargets(current: string, offered: string[]): string[] {
  const cur = parts(current);
  if (!cur) {
    return [];
  }
  return offered.filter((v) => {
    const t = parts(v);
    return t !== undefined && ((t.minor === cur.minor && t.patch > cur.patch) || t.minor === cur.minor + 1);
  });
}

/** talosOf is the Talos release a cluster's nodes are upgraded to. */
export const talosOf = (c: Cluster): string => c.talos?.version ?? text(c.variables["talosVersion"]) ?? "";

/** TalosDialog upgrades a cluster's nodes to another Talos release, in place and one at a time. */
export function TalosDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }): JSX.Element {
  const current = talosOf(c);
  const releases = useTalosReleases(true);
  const [version, setVersion] = useState(current);
  const { busy, error, send } = useChange(c, onClose);
  const targets = talosTargets(current, releases ? offeredReleases(releases) : []);
  const typed = version !== "" && !talosRelease.test(version);
  const kubernetes = kubernetesMinor(c.version);
  const held = targets.filter((v) => !runsKubernetes(releases, v, c.version));
  const field = (id: string): JSX.Element => {
    if (!releases) {
      return loadingReleases(id);
    }
    if (targets.length === 0) {
      return <input id={id} value={version} onChange={(e) => setVersion(e.target.value.trim())} />;
    }
    return (
      <select id={id} value={version} onChange={(e) => setVersion(e.target.value)}>
        <option value={current} disabled>
          {current} (running)
        </option>
        {targets.map((v) => (
          <option key={v} value={v} disabled={held.includes(v)}>
            {held.includes(v) ? `${v} (upgrade Kubernetes first)` : v}
          </option>
        ))}
      </select>
    );
  };
  return (
    <Dialog
      title={`Upgrade Talos on ${c.name}`}
      onClose={onClose}
      onSubmit={() => {
        void send({ variables: { talosVersion: version } });
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className="primary"
            disabled={busy || version === current || !talosRelease.test(version)}
          >
            Upgrade
          </button>
        </>
      }
    >
      <p className="muted">Reboots the nodes one at a time, control planes first.</p>
      {filled(c.talos?.upgrading) && <p className="warning">Upgrading {upgradingText(c.talos.upgrading)}</p>}
      <Field
        label="Talos release"
        hint={releases && targets.length === 0 && !typed ? `Such as ${talosExample}` : undefined}
        error={typed ? `Enter a release such as ${talosExample}` : undefined}
      >
        {field}
      </Field>
      {held.length > 0 && (
        <p className="muted">
          {held.join(", ")} {held.length === 1 ? "doesn't" : "don't"} run Kubernetes {kubernetes}. Upgrade Kubernetes
          first.
        </p>
      )}
      {releases && targets.length === 0 && (
        <p className="muted">{filled(releases.error) ? sentence(releases.error) : "No newer release"}</p>
      )}
      <ErrorBox message={error} />
    </Dialog>
  );
}
