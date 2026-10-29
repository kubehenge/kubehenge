import type { JSX } from "react";

import type { AdminView } from "./admin-views";
import { ApplianceUpgrades } from "./ApplianceUpgrades";
import { SharedDNS01 } from "./Certificates";
import { Credentials } from "./Credentials";
import { ManagementBackups } from "./ManagementBackups";
import { RemoveManagement } from "./RemoveManagement";
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
              <p>
                Certificate verification is off: the appliance and the clusters reach the Proxmox VE API without
                checking its certificate, as the appliance was installed with <code>--proxmox-insecure</code>. To check
                it against a CA, run{" "}
                <code>
                  sh install.sh --upgrade --ctid {status.applianceVMID ?? "<appliance ID>"} --proxmox-ca &lt;file&gt;
                </code>{" "}
                in the shell of the Proxmox VE node, then apply the management cluster again.
              </p>
            </section>
          )}
        </>
      );
    }
    case "backups": {
      return (
        <section className="panel">
          <ManagementBackups running={running} />
        </section>
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
      <ManagementCluster status={status} reload={reload} />
      <section className="panel">
        <RemoveManagement
          running={running}
          onStarted={() => {
            void reload();
          }}
        />
      </section>
    </>
  );
}
