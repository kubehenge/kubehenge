import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { fakeServer, type Route } from "./fake-server";
import { ManagementBackups } from "./ManagementBackups";
import type { ManagementBackups as Backups } from "./types";

const passphrase = "correct horse battery staple";

const off: Backups = { targets: ["minio", "aws"], keysStored: false };

const on: Backups = {
  settings: {
    target: "minio",
    everyHours: 6,
    keep: 28,
    by: "root@pam",
    since: "2027-03-01T12:00:00Z",
  },
  last: {
    name: "20270301T120000Z-v1.36.5.db.age",
    time: "2027-03-01T12:00:00Z",
    kubernetesVersion: "v1.36.5",
  },
  next: "2027-03-01T18:00:00Z",
  targets: ["minio", "aws"],
  keysStored: true,
};

const snapshots = {
  snapshots: [
    {
      name: "20270301T120000Z-v1.36.5.db.age",
      time: "2027-03-01T12:00:00Z",
      kubernetesVersion: "v1.36.5",
      size: 2048,
    },
    {
      name: "20270301T060000Z-v1.36.4.db.age",
      time: "2027-03-01T06:00:00Z",
      kubernetesVersion: "v1.36.4",
    },
  ],
};

describe("management cluster backups", () => {
  it("sets backups up with a target, the keys and the passphrase", async () => {
    const routes: Record<string, Route> = {
      "GET /api/appliance/etcd-backups": { body: off },
      "PUT /api/appliance/etcd-backups": { body: on },
      "GET /api/appliance/etcd-backups/snapshots": { body: snapshots },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<ManagementBackups running={false} />);

    await user.click(await screen.findByRole("button", { name: "Set up…" }));
    const dialog = screen.getByRole("dialog", {
      name: "Back up the management cluster",
    });
    const save = within(dialog).getByRole("button", { name: "Save" });
    await user.selectOptions(within(dialog).getByLabelText("Target"), "minio");
    await user.type(within(dialog).getByLabelText("Access key ID"), "AKID");
    await user.type(within(dialog).getByLabelText("Secret access key"), "secret");
    expect(save).toBeDisabled();
    await user.type(within(dialog).getByLabelText("State passphrase"), passphrase);
    routes["GET /api/appliance/etcd-backups"] = { body: on };
    await user.click(save);

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.find((c) => c.key === "PUT /api/appliance/etcd-backups")?.body).toStrictEqual({
      target: "minio",
      everyHours: 6,
      keep: 28,
      accessKeyID: "AKID",
      secretAccessKey: "secret",
      passphrase,
    });
    await expect(screen.findByText("Every 6 h, newest 28 kept")).resolves.toBeInTheDocument();
    await expect(screen.findAllByRole("button", { name: /^Restore from /u })).resolves.toHaveLength(2);
  });

  it("keeps the stored keys when they are left empty", async () => {
    const calls = fakeServer({
      "GET /api/appliance/etcd-backups": { body: on },
      "PUT /api/appliance/etcd-backups": { body: on },
      "GET /api/appliance/etcd-backups/snapshots": { body: snapshots },
    });
    const user = userEvent.setup({ delay: null });
    render(<ManagementBackups running={false} />);

    await user.click(await screen.findByRole("button", { name: "Change…" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByLabelText("Access key ID")).toHaveAttribute("placeholder", "Stored");
    await user.clear(within(dialog).getByLabelText("Keep"));
    await user.type(within(dialog).getByLabelText("Keep"), "10");
    await user.type(within(dialog).getByLabelText("State passphrase"), `${passphrase}{Enter}`);
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("PUT /api/appliance/etcd-backups"));
    expect(calls.find((c) => c.key === "PUT /api/appliance/etcd-backups")?.body).toMatchObject({
      keep: 10,
      accessKeyID: "",
      secretAccessKey: "",
    });
  });

  it("says why a snapshot failed, and points to a target when there is none", async () => {
    const routes: Record<string, Route> = {
      "GET /api/appliance/etcd-backups": {
        body: {
          ...on,
          error: "talos: no node answered",
          failed: "2027-03-01T18:00:00Z",
        },
      },
      "GET /api/appliance/etcd-backups/snapshots": {
        body: { snapshots: [], error: "bucket not found" },
      },
    };
    fakeServer(routes);
    const { unmount } = render(<ManagementBackups running={false} />);
    await expect(screen.findByText("Failed")).resolves.toBeInTheDocument();
    expect(screen.getByText("Talos: no node answered.")).toBeInTheDocument();
    await expect(screen.findByText("Bucket not found.")).resolves.toBeInTheDocument();
    unmount();

    routes["GET /api/appliance/etcd-backups"] = {
      body: { targets: [], keysStored: false },
    };
    render(<ManagementBackups running={false} />);
    await expect(screen.findByRole("link", { name: "Management cluster" })).resolves.toHaveAttribute(
      "href",
      "/administration",
    );
    expect(screen.queryByRole("button", { name: "Set up…" })).not.toBeInTheDocument();
  });

  it("restores a snapshot with the passphrase, and not during a run", async () => {
    const calls = fakeServer({
      "GET /api/appliance/etcd-backups": { body: on },
      "GET /api/appliance/etcd-backups/snapshots": { body: snapshots },
      "POST /api/appliance/etcd-backups/snapshots/20270301T060000Z-v1.36.4.db.age/restore": {
        status: 202,
        body: {
          user: "root@pam",
          kind: "etcd-restore",
          started: "2027-03-01T19:00:00Z",
          state: "running",
          log: [],
        },
      },
    });
    const user = userEvent.setup({ delay: null });
    const { rerender } = render(<ManagementBackups running={false} />);

    await user.click(
      await screen.findByRole("button", {
        name: "Restore from 20270301T060000Z-v1.36.4.db.age",
      }),
    );
    const dialog = screen.getByRole("dialog", {
      name: "Restore the management cluster's etcd",
    });
    expect(within(dialog).getByText(/Wipes etcd on every control-plane node/u)).toBeInTheDocument();
    const restore = within(dialog).getByRole("button", { name: "Restore" });
    expect(restore).toBeDisabled();
    await user.type(within(dialog).getByLabelText("State passphrase"), passphrase);
    await user.click(restore);
    await expect(screen.findByRole("link", { name: "Follow the run" })).resolves.toHaveAttribute(
      "href",
      "/runs/management-cluster",
    );
    expect(calls.at(-1)?.body).toStrictEqual({ passphrase });

    await user.click(screen.getByRole("button", { name: "Done" }));
    rerender(<ManagementBackups running />);
    for (const b of screen.getAllByRole("button", { name: /^Restore from /u })) {
      expect(b).toBeDisabled();
    }
  });
});
