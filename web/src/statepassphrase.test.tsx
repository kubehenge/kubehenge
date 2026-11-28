import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { fakeServer, type Route } from "./fake-server";
import { TakeOver } from "./ManagementRecovery";
import { RequirePassphrase } from "./StatePassphrase";
import type { SetupStatus } from "./types";
import { StatePassphrase } from "./ui";

const route = "PUT /api/setup/state-passphrase";
const base: SetupStatus = { appliance: true, ready: true, allowed: true };
const toggle = "Require a passphrase";

describe("requiring a state passphrase", () => {
  it("requires a new passphrase typed twice", async () => {
    const routes: Record<string, Route> = { [route]: { body: { required: true } } };
    const calls = fakeServer(routes);
    const changed = vi.fn<() => void>();
    const user = userEvent.setup({ delay: null });
    render(<RequirePassphrase status={base} onChanged={changed} />);

    await user.click(screen.getByRole("checkbox", { name: toggle }));
    expect(screen.queryByLabelText("State passphrase")).not.toBeInTheDocument();
    await user.type(screen.getByLabelText("New passphrase"), "a passphrase only the admin knows");
    await user.type(screen.getByLabelText("Repeat passphrase"), "a passphrase only the admin know");
    expect(screen.getByRole("button", { name: "Require" })).toBeDisabled();
    await user.type(screen.getByLabelText("Repeat passphrase"), "s{Enter}");
    await waitFor(() => expect(changed).toHaveBeenCalledWith());
    expect(calls.filter((c) => c.key === route).map((c) => c.body)).toStrictEqual([
      { required: true, passphrase: "", newPassphrase: "a passphrase only the admin knows" },
    ]);
  });

  it("stops requiring it with the passphrase", async () => {
    const calls = fakeServer({ [route]: { body: { required: false } } });
    const changed = vi.fn<() => void>();
    const user = userEvent.setup({ delay: null });
    render(<RequirePassphrase status={{ ...base, passphrase: true, passphraseRequired: true }} onChanged={changed} />);

    expect(screen.getByRole("checkbox", { name: toggle })).toBeChecked();
    await user.click(screen.getByRole("checkbox", { name: toggle }));
    expect(screen.queryByLabelText("New passphrase")).not.toBeInTheDocument();
    await user.type(screen.getByLabelText("State passphrase"), "a passphrase only the admin knows{Enter}");
    await waitFor(() => expect(changed).toHaveBeenCalledWith());
    expect(calls.filter((c) => c.key === route).map((c) => c.body)).toStrictEqual([
      { required: false, passphrase: "a passphrase only the admin knows", newPassphrase: "" },
    ]);
  });

  it("asks for no passphrase where the appliance keeps the key", async () => {
    const calls = fakeServer({ "POST /api/setup/take-over": { status: 204 } });
    const done = vi.fn<() => void>();
    const user = userEvent.setup({ delay: null });
    render(
      <StatePassphrase value={false}>
        <TakeOver stale={{ local: 2, cluster: 3 }} onDone={done} />
      </StatePassphrase>,
    );
    await user.click(screen.getByRole("button", { name: "Take over…" }));
    expect(screen.queryByLabelText("State passphrase")).not.toBeInTheDocument();
    await user.type(screen.getByLabelText("Type overwrite to confirm"), "overwrite{Enter}");
    await waitFor(() => expect(done).toHaveBeenCalledWith());
    expect(calls.filter((c) => c.key === "POST /api/setup/take-over").map((c) => c.body)).toStrictEqual([
      { passphrase: "" },
    ]);
  });
});
