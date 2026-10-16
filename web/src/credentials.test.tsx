import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { Credentials } from "./Credentials";
import { fakeServer, type Route } from "./fake-server";

describe("repair access", () => {
  it("downloads a credential with the passphrase, and shows a refusal", async () => {
    const routes: Record<string, Route> = { "POST /api/setup/credential": { text: "apiVersion: v1\n" } };
    const calls = fakeServer(routes);
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockReturnValue(undefined);
    const user = userEvent.setup({ delay: null });
    const { rerender } = render(<Credentials running={false} />);

    const button = screen.getByRole("button", { name: "Download admin kubeconfig" });
    expect(button).toBeDisabled();
    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(button);
    await waitFor(() => expect(click).toHaveBeenCalledWith());
    expect(calls.at(-1)?.body).toStrictEqual({ name: "kubeconfig", passphrase: "correct horse battery staple" });
    // The passphrase does not linger in the page once it has served.
    expect(screen.getByLabelText("State passphrase")).toHaveValue("");

    routes["POST /api/setup/credential"] = {
      status: 403,
      body: { error: "the passphrase does not open the management cluster's state" },
    };
    await user.type(screen.getByLabelText("State passphrase"), "a wrong passphrase here");
    await user.click(screen.getByRole("button", { name: "Download admin talosconfig" }));
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("does not open");

    rerender(<Credentials running />);
    expect(screen.getByRole("button", { name: "Download admin talosconfig" })).toBeDisabled();
  });

  it("downloads nothing on Enter, which names neither credential", async () => {
    const calls = fakeServer({ "POST /api/setup/credential": { text: "apiVersion: v1\n" } });
    const user = userEvent.setup({ delay: null });
    render(<Credentials running={false} />);

    await user.type(screen.getByLabelText("State passphrase"), "correct horse battery staple{Enter}");
    expect(calls).toHaveLength(0);
  });
});
