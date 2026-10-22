import { useState, type JSX } from "react";

import { api } from "./api";
import { AutomaticManagement } from "./ManagementUpgrade";
import type { UpgradePolicy, Upgrades } from "./types";
import { Dialog, ErrorBox, Field, browserZone, errorMessage, filled, pollRunning, timeZones, usePoll } from "./ui";
import { oneOf } from "./values";

const policies: readonly UpgradePolicy[] = ["off", "patch", "minor"];

// Releases come out seldom, so they are looked for once a minute while no upgrade is under way.
const pollReleases = 60_000;

const hoursPerDay = 24;

// series names the releases a policy follows from an installed version: 0.1.x, or 0.x.
function series(version: string, policy: "patch" | "minor"): string {
  const [major, minor] = version.replace(/^v/u, "").split(".");
  if (major === undefined || minor === undefined || !/^\d+$/u.test(major) || !/^\d+$/u.test(minor)) {
    return "";
  }
  return policy === "patch" ? ` (${major}.${minor}.x)` : ` (${major}.x)`;
}

// ApplianceUpgrades upgrades Kubehenge itself: the appliance's software, not the clusters. Only
// releases signed by Kubehenge's release key are offered, and never an older one.
export function ApplianceUpgrades(): JSX.Element {
  // The release asked for, until the updater reports on it.
  const [asked, setAsked] = useState<string>();
  // An upgrade under way is followed closely.
  const { data, error, reload } = usePoll(api.upgrades, (d) =>
    d?.status?.state === "updating" || (asked !== undefined && d?.version !== asked) ? pollRunning : pollReleases,
  );
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string>();
  const [saved, setSaved] = useState(false);
  const [confirming, setConfirming] = useState<string>();

  if (!data) {
    return (
      <div className="box">
        <h3>Kubehenge</h3>
        {filled(error) ? <ErrorBox message={error} /> : <p className="muted">Loading…</p>}
      </div>
    );
  }
  const newest = data.available.at(-1);
  const updating = data.status?.state === "updating";
  const waiting = asked !== undefined && !updating && data.status?.to !== asked && data.version !== asked;

  const run = async (f: () => Promise<unknown>): Promise<boolean> => {
    setBusy(true);
    setActionError(undefined);
    setSaved(false);
    try {
      await f();
      await reload();
      return true;
    } catch (err) {
      setActionError(errorMessage(err));
      return false;
    } finally {
      setBusy(false);
    }
  };
  const save = async (settings: Upgrades["settings"]): Promise<void> => {
    setSaved(await run(() => api.upgradeSettings(settings)));
  };
  const upgrade = async (version: string): Promise<void> => {
    if (await run(() => api.upgrade(version))) {
      setAsked(version);
    }
  };

  return (
    <div className="box">
      <h3>Kubehenge</h3>
      <Releases upgrades={data} />
      {newest !== undefined && (
        <div className="actions">
          <button
            type="button"
            className="primary"
            disabled={busy || updating || waiting}
            onClick={() => setConfirming(newest)}
          >
            {updating || waiting ? "Upgrading…" : `Upgrade to ${newest}`}
          </button>
        </div>
      )}
      <AutomaticUpgrades
        version={data.version}
        settings={data.settings}
        management={data.management}
        busy={busy}
        onSave={(settings) => {
          void save(settings);
        }}
        onManagementChanged={reload}
      />
      <output className="muted notice">{saved ? "Saved." : ""}</output>
      <ErrorBox message={actionError} />
      {confirming !== undefined && (
        <Dialog
          title={`Upgrade Kubehenge to ${confirming}`}
          onClose={() => setConfirming(undefined)}
          onSubmit={() => {
            setConfirming(undefined);
            void upgrade(confirming);
          }}
          actions={
            <>
              <button type="button" onClick={() => setConfirming(undefined)}>
                Cancel
              </button>
              <button type="submit" className="primary">
                Upgrade
              </button>
            </>
          }
        >
          <p>
            The appliance installs {confirming} and restarts the interface, which is away for about a minute. Clusters
            keep running. Reload the page afterwards.
          </p>
        </Dialog>
      )}
    </div>
  );
}

function newerReleases(u: Upgrades): string {
  if (filled(u.error)) {
    return `Could not be checked: ${u.error}`;
  }
  return u.available.length > 0 ? u.available.join(", ") : "None";
}

function Releases({ upgrades: u }: { upgrades: Upgrades }): JSX.Element {
  return (
    <dl className="summary">
      <dt>Installed</dt>
      <dd>{u.version}</dd>
      <dt>Newer releases</dt>
      <dd>{newerReleases(u)}</dd>
      {u.status && (
        <>
          <dt>Last upgrade</dt>
          <dd>
            {u.status.state === "succeeded" && `${u.status.from} to ${u.status.to}`}
            {u.status.state === "current" && "Already the newest allowed"}
            {u.status.state === "updating" && "In progress"}
            {u.status.state === "failed" && `Failed: ${u.status.error ?? ""}`} (
            {new Date(u.status.time).toLocaleString()})
          </dd>
        </>
      )}
    </dl>
  );
}

function AutomaticUpgrades({
  version,
  settings,
  management,
  busy,
  onSave,
  onManagementChanged,
}: {
  version: string;
  settings: Upgrades["settings"];
  management: Upgrades["management"];
  busy: boolean;
  onSave: (settings: Upgrades["settings"]) => void;
  onManagementChanged: () => Promise<void>;
}): JSX.Element {
  // The hour is the management cluster's too once it follows Kubehenge's upgrades on its own.
  const timed = settings.policy !== "off" || management?.on === true;
  // Switched on from nothing timed, the hour starts out in the browser's time zone.
  const timeZone = (): string => (timed ? settings.timeZone : browserZone());
  return (
    <>
      <Field
        label="Automatic upgrades"
        hint="Kubehenge's own releases only, once a day at the hour chosen. The interface restarts briefly."
      >
        {(id) => (
          <select
            id={id}
            value={settings.policy}
            disabled={busy}
            onChange={(e) => {
              const policy = e.target.value;
              if (!oneOf(policies, policy)) {
                return;
              }
              onSave({ ...settings, policy, timeZone: timeZone() });
            }}
          >
            <option value="off">Off</option>
            <option value="patch">Patch releases{series(version, "patch")}</option>
            <option value="minor">Minor releases{series(version, "minor")}</option>
          </select>
        )}
      </Field>
      {timed && (
        <Field label="At hour">
          {(id) => (
            <select
              id={id}
              value={settings.hour}
              disabled={busy}
              onChange={(e) => onSave({ ...settings, hour: Number(e.target.value) })}
            >
              {Array.from({ length: hoursPerDay }, (_, h) => (
                <option key={h} value={h}>
                  {String(h).padStart(2, "0")}:00
                </option>
              ))}
            </select>
          )}
        </Field>
      )}
      {timed && (
        <Field label="Time zone" hint="The hour is in this time zone.">
          {(id) => (
            <select
              id={id}
              value={settings.timeZone}
              disabled={busy}
              onChange={(e) => onSave({ ...settings, timeZone: e.target.value })}
            >
              {timeZones(settings.timeZone).map((z) => (
                <option key={z} value={z}>
                  {z}
                </option>
              ))}
            </select>
          )}
        </Field>
      )}
      {management && (
        <AutomaticManagement
          management={management}
          hour={settings.hour}
          timeZone={timeZone()}
          onChanged={async () => {
            if (!timed && !management.on) {
              onSave({ ...settings, timeZone: timeZone() });
            }
            await onManagementChanged();
          }}
        />
      )}
    </>
  );
}
