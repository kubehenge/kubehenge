import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { fakeServer } from "./fake-server";
import { ManagementBackups } from "./ManagementBackups";
import { BackupLine, healthVerdict } from "./ManagementHealth";
import { RemoveDialog } from "./RemoveManagement";
import type { ManagementBackups as Backups, ManagementHealth } from "./types";

const passphrase = "correct horse battery staple";

const lostWithHost = /Losing one Proxmox VE host can lose every backup/u;

const onVolume: NonNullable<Backups["volume"]> = {
  claim: "kubehenge-system/kubehenge-backups",
  last: { name: "20270301T120000Z-v1.37.2.db.age", time: "2027-03-01T12:00:00Z" },
  next: "2027-03-01T18:00:00Z",
  copy: { snapshot: "20270301T120000Z-v1.37.2.db.age", bytes: 2048, cap: 1_073_741_824 },
  export: { name: "20270301T120000Z-v1.37.2.json.age", time: "2027-03-01T12:00:00Z", bytes: 512 },
};

const volume: Backups = { targets: [], keysStored: false, volume: onVolume };

describe("the backup volume", () => {
  it("shows where the backups are and the last one", async () => {
    fakeServer({ "GET /api/appliance/etcd-backups": { body: volume } });
    render(<ManagementBackups running={false} />);
    await screen.findByRole("heading", { name: "Backup volume" });
    expect(screen.getByRole("heading", { name: "Backup volume" })).toBeVisible();
    expect(screen.getByText("kubehenge-system/kubehenge-backups")).toBeVisible();
    expect(screen.getAllByText("20270301T120000Z-v1.37.2.db.age")).toHaveLength(2);
    expect(screen.getByText("20270301T120000Z-v1.37.2.json.age")).toBeVisible();
  });

  it("shows a failed backup", async () => {
    const failed = {
      ...volume,
      volume: { ...onVolume, error: "the backup volume's token: not found", failed: "2027-03-01T18:00:00Z" },
    };
    fakeServer({ "GET /api/appliance/etcd-backups": { body: failed } });
    render(<ManagementBackups running={false} />);
    await screen.findByRole("alert");
    expect(screen.getByRole("alert")).toHaveTextContent("Failed");
    expect(screen.getByRole("alert")).toHaveTextContent("The backup volume's token: not found.");
  });

  it("warns on Administration while one host's loss can take every backup", async () => {
    const sameHost = { ...volume, summary: { where: ["backup volume x"], sameHost: true } };
    fakeServer({ "GET /api/appliance/etcd-backups": { body: sameHost } });
    const { unmount } = render(<ManagementBackups running={false} />);
    await screen.findByRole("heading", { name: "Backup volume" });
    expect(screen.getByText(lostWithHost)).toBeVisible();
    unmount();

    fakeServer({ "GET /api/appliance/etcd-backups": { body: { ...volume, summary: { where: ["backup volume x"] } } } });
    render(<ManagementBackups running={false} />);
    await screen.findByRole("heading", { name: "Backup volume" });
    expect(screen.queryByText(lostWithHost)).not.toBeInTheDocument();
  });

  it("warns on Health while one host's loss can take every backup, without degrading it", () => {
    const { unmount } = render(<BackupLine backups={{ where: ["backup volume x"], sameHost: true }} />);
    expect(screen.getByText(lostWithHost)).toBeVisible();
    unmount();
    render(<BackupLine backups={{ where: ["backup volume x", "target minio"] }} />);
    expect(screen.queryByText(lostWithHost)).not.toBeInTheDocument();
    const h: ManagementHealth = { answers: true, nodes: [{ name: "cp-0", ready: true }], controllers: [] };
    expect(healthVerdict({ ...h, backups: { where: ["backup volume x"], sameHost: true } }).state).toBe("ok");
  });

  it("degrades Health while a backup fails", () => {
    const h: ManagementHealth = { answers: true, nodes: [{ name: "cp-0", ready: true }], controllers: [] };
    expect(healthVerdict(h).state).toBe("ok");
    expect(
      healthVerdict({ ...h, backups: { where: ["backup volume x"], error: "backup volume: unreachable" } }).state,
    ).toBe("warning");
  });

  it("removes the backup volume only when asked", async () => {
    const calls = fakeServer({ "POST /api/setup/remove": { body: {} } });
    const user = userEvent.setup({ delay: null });
    const closed = vi.fn<() => void>();
    const started = vi.fn<() => void>();
    render(<RemoveDialog onClose={closed} onStarted={started} />);
    expect(screen.getByText(/Keeps the backup volume, VM kubehenge-backups/u)).toBeVisible();
    const box = screen.getByRole("checkbox", { name: "Delete the backup volume" });
    expect(box).not.toBeChecked();
    await user.type(screen.getByLabelText("State passphrase"), passphrase);
    await user.type(screen.getByLabelText("Type remove to confirm"), "remove");
    await user.click(screen.getByRole("button", { name: "Remove" }));
    expect(calls.at(-1)?.body).toStrictEqual({ passphrase, confirm: "remove", removeBackups: false });
  });

  it("recovers the management cluster from its own backups", async () => {
    const calls = fakeServer({
      "GET /api/appliance/etcd-backups": { body: volume },
      "POST /api/appliance/etcd-backups/recover": { status: 202, body: {} },
    });
    const user = userEvent.setup({ delay: null });
    render(<ManagementBackups running={false} />);
    await user.click(await screen.findByRole("button", { name: "Recover…" }));
    const recover = screen.getByRole("button", { name: "Recover" });
    expect(recover).toBeDisabled();
    await user.type(screen.getByLabelText("State passphrase"), passphrase);
    await user.click(recover);
    await screen.findByRole("link", { name: "Follow the run" });
    expect(screen.getByRole("link", { name: "Follow the run" })).toBeVisible();
    const sent = calls.find((c) => c.key === "POST /api/appliance/etcd-backups/recover");
    expect(sent?.body).toStrictEqual({ passphrase });
  });
});
