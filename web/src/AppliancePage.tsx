import type { JSX } from "react";

import { ApplianceUpgrades } from "./ApplianceUpgrades";
import { Credentials } from "./Credentials";
import { RequirePassphrase } from "./StatePassphrase";
import type { SetupStatus } from "./types";

/** AppliancePage is Administration's page of the appliance itself. */
export function AppliancePage({
  status,
  running,
  onChanged,
}: {
  status: SetupStatus;
  running: boolean;
  onChanged: () => void;
}): JSX.Element {
  return (
    <>
      <section className="panel columns">
        <ApplianceUpgrades />
        <Credentials running={running} />
        <RequirePassphrase status={status} onChanged={onChanged} />
      </section>
      {status.proxmoxInsecure === true && (
        <section className="panel">
          <h3>Proxmox VE API</h3>
          <dl className="summary">
            <dt>Certificate check</dt>
            <dd>Off</dd>
          </dl>
          <p>To switch it on, run this on the Proxmox VE node, then apply the management cluster again.</p>
          <p>
            <code>sh install.sh --upgrade --proxmox-ca &lt;file&gt;</code>
          </p>
        </section>
      )}
    </>
  );
}
