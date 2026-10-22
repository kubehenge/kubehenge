import { useEffect, type JSX } from "react";

import { api } from "./api";
import { ApplianceUpgrades } from "./ApplianceUpgrades";
import { SharedDNS01 } from "./Certificates";
import { Credentials } from "./Credentials";
import { RemoveManagement } from "./RemoveManagement";
import { ManagementCluster } from "./Setup";
import { Templates } from "./Templates";
import { ErrorBox, pollIdle, pollRunning, usePoll } from "./ui";

// Administration is the appliance's own page for those who may set it up: Kubehenge's upgrades,
// repair access, VM templates, the DNS-01 credential for every cluster, the management cluster's
// settings and removal. One poll of the setup status serves them all, so they agree on whether a
// run is going on.
export function Administration({ onGone }: { onGone: () => void }): JSX.Element {
  const {
    data: status,
    error,
    reload,
  } = usePoll(api.setupStatus, (s) => (s?.job?.state === "running" ? pollRunning : pollIdle));
  // Once a removal is over there is nothing left to administer here: the setup takes over.
  const gone =
    status !== undefined && !status.ready && status.job?.kind === "remove" && status.job.state === "succeeded";
  useEffect(() => {
    if (gone) {
      onGone();
    }
  }, [gone, onGone]);

  if (!status) {
    return (
      <section className="panel">
        {error === undefined ? <p className="loading">Loading…</p> : <ErrorBox message={error} />}
      </section>
    );
  }
  const running = status.job?.state === "running";
  return (
    <>
      <ErrorBox message={error} />
      <section className="panel columns">
        <ApplianceUpgrades />
        <Credentials running={running} />
      </section>
      {status.proxmoxInsecure === true && (
        <section className="panel">
          <h3>Proxmox VE API</h3>
          <p>
            Certificate verification is off: the appliance and the clusters reach the Proxmox VE API without checking
            its certificate, as the appliance was installed with <code>--proxmox-insecure</code>. To check it against a
            CA, run{" "}
            <code>
              sh install.sh --upgrade --ctid {status.applianceVMID ?? "<appliance ID>"} --proxmox-ca &lt;file&gt;
            </code>{" "}
            in the shell of the Proxmox VE node, then apply the management cluster again.
          </p>
        </section>
      )}
      <section className="panel">
        <Templates />
      </section>
      <section className="panel">
        <SharedDNS01 />
      </section>
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
