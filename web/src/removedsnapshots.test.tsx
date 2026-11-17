import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { fakeServer } from "./fake-server";
import { RemovedSnapshots } from "./RemovedSnapshots";
import type { RemovedCluster } from "./types";

const old: RemovedCluster = {
  namespace: "clusters",
  name: "old",
  keys: true,
  snapshots: [
    { name: "20261102T060000Z-v1.37.1.db.age", time: "2026-11-02T06:00:00Z" },
    { name: "20261101T060000Z-v1.37.1.db.age", time: "2026-11-01T06:00:00Z" },
  ],
};
const listed = "GET /api/removed-etcd-snapshots/clusters";
const deleted = "DELETE /api/removed-etcd-snapshots/clusters/old";

describe("a removed cluster's etcd snapshots", () => {
  it("are listed with their count", async () => {
    fakeServer({ [listed]: { body: [old] } });
    render(<RemovedSnapshots namespace="clusters" />);
    const cell = await screen.findByRole("cell", { name: "old" });
    expect(cell.nextElementSibling).toHaveTextContent(/^2$/u);
  });

  it("are deleted once the name is typed", async () => {
    const calls = fakeServer({ [listed]: { body: [old] }, [deleted]: { status: 204 } });
    const user = userEvent.setup({ delay: null });
    render(<RemovedSnapshots namespace="clusters" />);
    await user.click(await screen.findByRole("button", { name: "Delete old's snapshots" }));
    const dialog = screen.getByRole("dialog", { name: "Delete old's snapshots" });
    const confirm = within(dialog).getByRole("button", { name: "Delete snapshots" });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Confirm name"), "old");
    await user.click(confirm);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.find((c) => c.key === deleted)?.body).toStrictEqual({ confirm: "old" });
  });

  it("stay while the name typed is another", async () => {
    const calls = fakeServer({ [listed]: { body: [old] }, [deleted]: { status: 204 } });
    const user = userEvent.setup({ delay: null });
    render(<RemovedSnapshots namespace="clusters" />);
    await user.click(await screen.findByRole("button", { name: "Delete old's snapshots" }));
    const dialog = screen.getByRole("dialog", { name: "Delete old's snapshots" });
    await user.type(within(dialog).getByLabelText("Confirm name"), "web");
    expect(within(dialog).getByRole("button", { name: "Delete snapshots" })).toBeDisabled();
    await user.keyboard("{Enter}");
    expect(calls.map((c) => c.key)).not.toContain(deleted);
  });

  it("show nothing while none are kept", async () => {
    const calls = fakeServer({ [listed]: { body: [] } });
    const { container } = render(<RemovedSnapshots namespace="clusters" />);
    await waitFor(() => expect(calls.map((c) => c.key)).toContain(listed));
    expect(container).toBeEmptyDOMElement();
  });
});
