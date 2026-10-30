import { useState, type JSX } from "react";

import { api } from "./api";
import { ApplyDialog } from "./Setup";
import type { ManagementAutomatic, ManagementUpgrade, SetupStatus } from "./types";
import { Dialog, ErrorBox, Field, errorMessage, passphraseShort } from "./ui";

// The module's names for the management cluster's components, in words.
const componentNames: Record<string, string> = {
  talos: "Talos",
  kubernetes: "Kubernetes",
  cluster_api: "Cluster API",
  capmox: "Proxmox VE provider",
  rke2: "RKE2 providers",
  k3s: "k3s providers",
  ipam_in_cluster: "IP address management",
  helm_addon: "Add-on Provider for Helm",
  talos_bootstrap: "Talos bootstrap provider",
  talos_control_plane: "Talos control-plane provider",
  cert_manager: "cert-manager",
  capi_operator: "Cluster API Operator",
  kubehenge: "Kubehenge",
};

export const componentName = (c: string): string => componentNames[c] ?? c;

function Changes({ upgrade: u }: { upgrade: ManagementUpgrade }): JSX.Element {
  const changes = u.changes ?? [];
  if (changes.length === 0) {
    return <p>No component changes version</p>;
  }
  return (
    <table className="grid">
      <thead>
        <tr>
          <th scope="col">Component</th>
          <th scope="col">{u.applied}</th>
          <th scope="col">{u.carried}</th>
        </tr>
      </thead>
      <tbody>
        {changes.map((c) => (
          <tr key={c.component}>
            <td>{componentName(c.component)}</td>
            <td>{c.from ?? "Unknown"}</td>
            <td>{c.to ?? "Not installed"}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// ManagementBehind says when the management cluster runs what an older release applied, and
// upgrades it with the settings it has, as Apply again does.
export function ManagementBehind({
  status,
  onStarted,
}: {
  status: SetupStatus;
  onStarted: () => void;
}): JSX.Element | null {
  const [open, setOpen] = useState(false);
  const { upgrade: u, settings } = status;
  if (u?.behind !== true || !settings || status.job?.state === "running") {
    return null;
  }
  // A failed automatic run is otherwise only seen on the Management cluster page, with its log.
  const failed = status.job?.automatic === true && status.job.state === "failed";
  return (
    <>
      <output className="warning notice">
        Management cluster upgrade: {u.applied} to {u.carried}
        {failed ? ". Automatic upgrade failed. " : " "}
        <button type="button" className="primary" onClick={() => setOpen(true)}>
          Upgrade…
        </button>
      </output>
      {open && (
        <ApplyDialog
          title="Upgrade management cluster"
          action="Upgrade"
          settings={settings}
          onClose={() => setOpen(false)}
          onStarted={() => {
            setOpen(false);
            onStarted();
          }}
        >
          <Changes upgrade={u} />
          <p>Upgrades the nodes one at a time. Clusters keep running.</p>
        </ApplyDialog>
      )}
    </>
  );
}

// AutomaticManagement lets the management cluster follow Kubehenge's own upgrades, which needs
// the appliance to keep what an apply needs; off by default, and saying what switching it on grants.
export function AutomaticManagement({
  management: m,
  hour,
  timeZone,
  onChanged,
}: {
  management: ManagementAutomatic;
  hour: number;
  timeZone: string;
  onChanged: () => Promise<void>;
}): JSX.Element | null {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  if (!m.possible && !m.on) {
    return null;
  }
  const switchOff = async (): Promise<void> => {
    setBusy(true);
    setError(undefined);
    try {
      await api.stopAutomaticManagement();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      await onChanged();
      setBusy(false);
    }
  };
  const since = m.since === undefined ? "" : new Date(m.since).toLocaleString();
  return (
    <>
      <Field
        label="Management cluster"
        hint={m.on ? `Switched on ${since} by ${m.by ?? ""}` : undefined}
        info={m.on ? `Upgrades at the hour above, as ${m.user}, after each Kubehenge upgrade.` : undefined}
      >
        {(id) => (
          <select
            id={id}
            value={m.on ? "on" : "off"}
            disabled={busy}
            onChange={(e) => {
              if (e.target.value === "on") {
                setAsking(true);
              } else {
                void switchOff();
              }
            }}
          >
            <option value="off">Upgrade by hand</option>
            <option value="on">Upgrade automatically</option>
          </select>
        )}
      </Field>
      <ErrorBox message={error} />
      {asking && (
        <SwitchOn
          user={m.user}
          when={`${String(hour).padStart(2, "0")}:00 (${timeZone})`}
          onClose={() => setAsking(false)}
          onDone={async () => {
            setAsking(false);
            await onChanged();
          }}
        />
      )}
    </>
  );
}

function SwitchOn({
  user,
  when,
  onClose,
  onDone,
}: {
  user: string;
  when: string;
  onClose: () => void;
  onDone: () => Promise<void>;
}): JSX.Element {
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const short = passphraseShort(passphrase);
  const submit = async (): Promise<void> => {
    if (short) {
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      await api.automaticManagement(passphrase);
      await onDone();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Dialog
      title="Upgrade management cluster automatically"
      onClose={onClose}
      onSubmit={() => {
        void submit();
      }}
      actions={
        <>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={busy || short}>
            Switch on
          </button>
        </>
      }
    >
      <p>Upgrades after each Kubehenge upgrade, at {when}.</p>
      <p className="warning">
        Stores the state passphrase and creates the Proxmox VE user <code>{user}</code> with Administrator on{" "}
        <code>/</code>.
      </p>
      <p className="warning">Whoever takes over the appliance then controls the management cluster and Proxmox VE.</p>
      <p>Switching off deletes the passphrase, the user and its token.</p>
      <Field label="State passphrase">
        {(id) => (
          <input
            id={id}
            type="password"
            autoComplete="current-password"
            value={passphrase}
            onChange={(e) => setPassphrase(e.target.value)}
          />
        )}
      </Field>
      <ErrorBox message={error} />
    </Dialog>
  );
}
