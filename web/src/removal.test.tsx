import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { fakeServer, type Route } from "./fake-server";
import { RemoveManagement } from "./RemoveManagement";

describe("removing Kubehenge", () => {
  it("removes on Enter only once the word is typed", async () => {
    const routes: Record<string, Route> = { "POST /api/setup/remove": { status: 202, body: {} } };
    const calls = fakeServer(routes);
    const started = vi.fn<() => void>();
    const user = userEvent.setup({ delay: null });
    render(<RemoveManagement running={false} onStarted={started} />);

    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple{Enter}");
    expect(calls.filter((c) => c.key === "POST /api/setup/remove")).toHaveLength(0);

    await user.type(screen.getByLabelText("Type remove to confirm"), "remove{Enter}");
    await waitFor(() => expect(started).toHaveBeenCalledWith());
    expect(calls.filter((c) => c.key === "POST /api/setup/remove").map((c) => c.body)).toStrictEqual([
      { passphrase: "correct horse battery staple", confirm: "remove" },
    ]);
  });
});
