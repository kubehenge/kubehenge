import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { fakeServer, type Route } from "./fake-server";
import { TakeOver } from "./TakeOver";

describe("taking the management cluster over", () => {
  it("takes over on Enter only once the words are typed", async () => {
    const routes: Record<string, Route> = { "POST /api/setup/take-over": { status: 204 } };
    const calls = fakeServer(routes);
    const done = vi.fn<() => void>();
    const user = userEvent.setup({ delay: null });
    render(<TakeOver stale={{ local: 2, cluster: 3 }} onDone={done} />);

    await user.click(screen.getByRole("button", { name: "Take over…" }));
    const dialog = screen.getByRole("dialog", { name: "Take over management" });
    expect(dialog).toHaveTextContent("This state is from apply 2, the cluster at apply 3.");
    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple{Enter}");
    expect(calls.filter((c) => c.key === "POST /api/setup/take-over")).toHaveLength(0);

    await user.type(screen.getByLabelText("Type overwrite to confirm"), "overwrite{Enter}");
    await waitFor(() => expect(done).toHaveBeenCalledWith());
    expect(calls.filter((c) => c.key === "POST /api/setup/take-over").map((c) => c.body)).toStrictEqual([
      { passphrase: "correct horse battery staple" },
    ]);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("says why it was refused and stays open", async () => {
    fakeServer({
      "POST /api/setup/take-over": {
        status: 403,
        body: { error: "the passphrase does not open the management cluster's state" },
      },
    });
    const done = vi.fn<() => void>();
    const user = userEvent.setup({ delay: null });
    render(<TakeOver stale={{ local: 0, cluster: 5 }} onDone={done} />);
    await user.click(screen.getByRole("button", { name: "Take over…" }));
    await user.type(screen.getByLabelText("State passphrase"), "not the passphrase of this state");
    await user.type(screen.getByLabelText("Type overwrite to confirm"), "overwrite{Enter}");
    await expect(screen.findByText(/does not open the management cluster's state/u)).resolves.toBeInTheDocument();
    expect(done).not.toHaveBeenCalled();
  });
});
