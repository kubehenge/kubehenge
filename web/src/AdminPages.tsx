import type { JSX } from "react";

import type { AdminView } from "./admin-views";
import { ApplianceUpgrades } from "./ApplianceUpgrades";
import { SharedDNS01 } from "./Certificates";
import { Credentials } from "./Credentials";
import { ManagementHealthCard } from "./ManagementHealth";
import { DangerZone } from "./RemoveManagement";
import { ManagementCluster } from "./Setup";
import { Templates } from "./Templates";
import type { SetupStatus } from "./types";

/** AdminPage is one page of Administration. */
export function AdminPage({
  view,
  status,
  running,
  reload,
}: {
  view: AdminView;
  status: SetupStatus;
  running: boolean;
  reload: () => Promise<void>;
}): JSX.Element {
  switch (view) {
    case "appliance": {
      return (
        <>
          <section className="panel columns">
            <ApplianceUpgrades />
            <Credentials running={running} />
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
                <code>
                  sh install.sh --upgrade --ctid {status.applianceVMID ?? "<appliance ID>"} --proxmox-ca &lt;file&gt;
                </code>
              </p>
            </section>
          )}
        </>
      );
    }
    case "templates": {
      return (
        <section className="panel">
          <Templates />
        </section>
      );
    }
    case "certificates": {
      return (
        <section className="panel">
          <SharedDNS01 />
        </section>
      );
    }
    case "": {
      break;
    }
  }
  return (
    <>
      {status.ready && !running && (
        <section className="panel">
          <ManagementHealthCard />
        </section>
      )}
      <ManagementCluster status={status} reload={reload} />
      <DangerZone
        running={running}
        onStarted={() => {
          void reload();
        }}
      />
    </>
  );
}
