import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { JSX } from "react";
import { describe, expect, it, vi } from "vitest";

import { BackupsDialog } from "./Backups";
import { UpgradesDialog } from "./ClusterUpgrades";
import { EtcdSnapshotsDialog } from "./EtcdSnapshots";
import { fakeServer } from "./fake-server";
import type { Cluster } from "./types";

// A zone no browser or test machine sits in, so that it can only come from the server.
const siteZone = "Pacific/Chatham";

const cluster: Cluster = {
  namespace: "default",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5",
  generation: 1,
  created: "2026-10-01T00:00:00Z",
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: {},
  pool: "/pool/kubehenge/default.web",
  allowed: { change: true, kubeconfig: true },
};

const classes = [
  {
    name: "kubehenge-rke2",
    distribution: "rke2",
    versionFormat: { pattern: ".*" },
    annotations: {
      "kubehenge.dev/time-zone": siteZone,
      "kubehenge.dev/backup-targets": JSON.stringify([{ name: "store", bucket: "backups" }]),
    },
    variables: [],
  },
];

// on is the cluster with these variables.
const on = (variables: Cluster["variables"]): Cluster => ({ ...cluster, variables });

// zoneIn opens a dialog and reads its time zone.
async function zoneIn(dialog: JSX.Element): Promise<string> {
  const { unmount } = render(dialog);
  const field = await screen.findByLabelText("Time zone");
  const zone = "value" in field ? String(field.value) : "";
  unmount();
  return zone;
}

describe("a page that starts a schedule", () => {
  it("starts it in the site's zone, read from the server, whatever page was open before", async () => {
    const user = userEvent.setup({ delay: null });
    fakeServer({ "GET /api/classes": { body: classes } });
    const close = vi.fn<() => void>();
    const startedIn = async (dialog: JSX.Element, switchOn: string): Promise<void> => {
      const { unmount } = render(dialog);
      await user.click(await screen.findByLabelText(switchOn));
      expect(screen.getByLabelText("Time zone")).toHaveValue(siteZone);
      unmount();
    };
    await startedIn(<EtcdSnapshotsDialog cluster={cluster} onClose={close} />, "Take etcd snapshots");
    await startedIn(<BackupsDialog cluster={cluster} onClose={close} />, "Back up with Velero");
    const off = { ...cluster, variables: { upgrades: { mode: "off", nodeImages: false } } };
    render(<UpgradesDialog cluster={off} onClose={close} />);
    await expect(screen.findByLabelText("Upgrade automatically")).resolves.toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText("Upgrade automatically"), "patch");
    expect(screen.getByLabelText("Time zone")).toHaveValue(siteZone);
  });

  it("shows a schedule that names no zone in the site's, where it runs", async () => {
    fakeServer({ "GET /api/classes": { body: classes } });
    const close = vi.fn<() => void>();
    const etcd = on({ etcdSnapshots: { enabled: true, target: "store" } });
    const backups = on({ backups: { enabled: true, target: "store" } });
    const upgrades = on({ upgrades: { mode: "patch", window: { start: "02:00" } } });
    await expect(zoneIn(<EtcdSnapshotsDialog cluster={etcd} onClose={close} />)).resolves.toBe(siteZone);
    await expect(zoneIn(<BackupsDialog cluster={backups} onClose={close} />)).resolves.toBe(siteZone);
    await expect(zoneIn(<UpgradesDialog cluster={upgrades} onClose={close} />)).resolves.toBe(siteZone);
  });

  it("shows a schedule that names no zone in UTC where the site set none", async () => {
    fakeServer({ "GET /api/classes": { body: [{ ...classes[0], annotations: {} }] } });
    render(
      <BackupsDialog
        cluster={{ ...cluster, variables: { backups: { enabled: true, target: "store" } } }}
        onClose={vi.fn<() => void>()}
      />,
    );
    await expect(screen.findByLabelText("Time zone")).resolves.toHaveValue("UTC");
  });
});
