import type { JSX } from "react";

import { ApplianceActivity } from "./Activity";
import type { AdminView } from "./admin-views";
import { AppliancePage } from "./AppliancePage";
import { SharedDNS01 } from "./Certificates";
import { ManagementBackups } from "./ManagementBackups";
import { ManagementHealthCard } from "./ManagementHealth";
import { Notifications } from "./Notifications";
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
      return <AppliancePage status={status} running={running} />;
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
    case "activity": {
      return (
        <section className="panel">
          <ApplianceActivity />
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
    case "notifications": {
      return (
        <section className="panel">
          <Notifications />
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
