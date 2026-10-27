import { useState, type JSX } from "react";

import { api } from "./api";
import { ApplyDialog } from "./Setup";
import type { ManagementAutomatic, ManagementUpgrade, SetupStatus } from "./types";
import { Dialog, ErrorBox, Field, errorMessage, minPassphrase, passphraseShort } from "./ui";

// The module's names for the management cluster's components, in words.
const componentNames: Record<string, string> = {
  talos: "Talos",
  kubernetes: "Kubernetes",
  cluster_api: "Cluster API",
  capmox: "Cluster API's Proxmox VE provider",
  rke2: "RKE2's Cluster API providers",
  k3s: "k3s's Cluster API providers",
  ipam_in_cluster: "IP address management",
  helm_addon: "Add-on Provider for Helm",
  talos_bootstrap: "Talos's Cluster API bootstrap provider",
  talos_control_plane: "Talos's Cluster API control-plane provider",
  cert_manager: "cert-manager",
  capi_operator: "Cluster API Operator",
  kubehenge: "Kubehenge's ClusterClasses and controller",
};

export const componentName = (c: string): string => componentNames[c] ?? c;

function Changes({ upgrade: u }: { upgrade: ManagementUpgrade }): JSX.Element {
  const changes = u.changes ?? [];
  if (changes.length === 0) {
    return (
      <p>
        From what Kubehenge {u.applied} applied to what {u.carried} carries. No component of the management cluster
        changes its version; the release may still change how they are set up.
      </p>
    );
  }
  return (
    <>
      <p>
        From what Kubehenge {u.applied} applied to what {u.carried} carries:
      </p>
      <table className="grid">
        <thead>
          <tr>
            <th scope="col">Component</th>
            <th scope="col">Now</th>
            <th scope="col">After the upgrade</th>
          </tr>
        </thead>
        <tbody>
          {changes.map((c) => (
            <tr key={c.component}>
              <td>{componentName(c.component)}</td>
              <td>{c.from ?? "not recorded"}</td>
              <td>{c.to ?? "no longer installed"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
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
        The management cluster runs what Kubehenge {u.applied} applied; this appliance carries {u.carried}.{" "}
        {failed &&
          "Upgrading it automatically failed (the log is on the Management cluster page); the next day's upgrade hour tries again. "}
        <button type="button" className="primary" onClick={() => setOpen(true)}>
          Upgrade the management cluster
        </button>
      </output>
      {open && (
        <ApplyDialog
          title="Upgrade the management cluster"
          action="Upgrade"
          settings={settings}
          onClose={() => setOpen(false)}
          onStarted={() => {
            setOpen(false);
            onStarted();
          }}
        >
          <Changes upgrade={u} />
          <p>
            It applies the management cluster's settings as they are: Talos node by node, then Kubernetes, the Cluster
            API providers and Kubehenge's own chart. Clusters keep running.
          </p>
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
        hint={
          m.on
            ? `Applied again on its own, as ${m.user}, at the hour above once Kubehenge runs a newer release than the one that last applied it. Switched on ${since} by ${m.by ?? ""}.`
            : "Stays as it is when Kubehenge upgrades, until it is upgraded on the Management cluster page."
        }
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
            <option value="off">Upgraded by hand</option>
            <option value="on">Upgraded automatically too</option>
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
      title="Upgrade the management cluster automatically"
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
            Keep them and switch on
          </button>
        </>
      }
    >
      <p>
        Once Kubehenge runs a newer release than the one that last applied the management cluster, the appliance applies
        it again on its own, at {when}, at most once a day. A run that fails is shown on this page and tried again the
        next day.
      </p>
      <p>Nobody is signed in then, so the appliance has to keep what an apply needs, which it otherwise never does:</p>
      <ul>
        <li>the state passphrase, which opens the management cluster's state;</li>
        <li>
          an API token of a Proxmox VE user of its own, <code>{user}</code>, with the Administrator role on{" "}
          <code>/</code>, which switching this on creates with your session.
        </li>
      </ul>
      <p className="warning">
        Anyone who gets root on the appliance, or takes over its interface, can then read the management cluster's
        state, which holds its admin kubeconfig and talosconfig, and act as an administrator of the whole Proxmox VE
        cluster with the token. Switching this off deletes both, and the user.
      </p>
      <Field
        label="State passphrase"
        hint={`The passphrase given when the management cluster was created; at least ${minPassphrase} characters.`}
      >
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
