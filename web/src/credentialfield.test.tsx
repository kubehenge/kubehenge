import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { CredentialField, setText, type CredentialKey } from "./CredentialField";
import { fakeServer, type Route } from "./fake-server";

const s3: CredentialKey[] = [
  { key: "accessKeyID", label: "Access key ID", plain: true },
  { key: "secretAccessKey", label: "Secret access key" },
];

const path = "/api/clusters/clusters/one/credentials/backup";
const setAt = "2026-10-22T09:30:00Z";

describe("credential field", () => {
  it("stores a credential, and shows only that it is set, when and by whom", async () => {
    const routes: Record<string, Route> = {
      [`GET ${path}`]: { body: { purpose: "backup", set: false, canChange: true } },
      [`PUT ${path}`]: { body: { purpose: "backup", set: true, setBy: "dev@pve", setAt, canChange: true } },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(
      <CredentialField
        label="Backup target keys"
        purpose="backup"
        cluster={{ namespace: "clusters", name: "one" }}
        keys={s3}
      />,
    );

    await expect(screen.findByText("Not set")).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Set…" }));
    const store = screen.getByRole("button", { name: "Store" });
    expect(store).toBeDisabled();
    expect(screen.getByLabelText("Access key ID")).toHaveAttribute("type", "text");
    expect(screen.getByLabelText("Secret access key")).toHaveAttribute("type", "password");
    await user.type(screen.getByLabelText("Access key ID"), "AKIA1");
    expect(store).toBeDisabled();
    await user.type(screen.getByLabelText("Secret access key"), "s3cr3t{Enter}");

    await expect(
      screen.findByText(setText({ purpose: "backup", set: true, setBy: "dev@pve", setAt, canChange: true })),
    ).resolves.toBeInTheDocument();
    expect(calls.at(-1)).toMatchObject({
      key: `PUT ${path}`,
      body: { values: { accessKeyID: "AKIA1", secretAccessKey: "s3cr3t" } },
    });
    // The values do not linger in the page once the server has them.
    expect(screen.queryByLabelText("Secret access key")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Replace…" })).toBeInTheDocument();
  });

  it("replaces and removes, and shows a refusal", async () => {
    const stored = { purpose: "backup", set: true, setBy: "admin@pve", setAt, canChange: true };
    const routes: Record<string, Route> = {
      "GET /api/credentials/backup": { body: stored },
      "PUT /api/credentials/backup": { status: 422, body: { error: "a value for secretAccessKey is required" } },
      "DELETE /api/credentials/backup": { status: 204 },
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<CredentialField label="Backup target keys" purpose="backup" keys={s3} />);

    await user.click(await screen.findByRole("button", { name: "Replace…" }));
    await user.type(screen.getByLabelText("Access key ID"), "AKIA2");
    await user.type(screen.getByLabelText("Secret access key"), "x");
    await user.click(screen.getByRole("button", { name: "Store" }));
    await expect(screen.findByRole("alert")).resolves.toHaveTextContent("A value for secretAccessKey is required.");
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Delete…" }));
    await user.click(screen.getByRole("button", { name: "Keep" }));
    expect(calls.filter((c) => c.key === "DELETE /api/credentials/backup")).toHaveLength(0);
    await user.click(screen.getByRole("button", { name: "Delete…" }));
    await user.click(screen.getByRole("button", { name: "Delete" }));
    await expect(screen.findByText("Not set")).resolves.toBeInTheDocument();
    expect(calls.at(-1)?.key).toBe("DELETE /api/credentials/backup");
  });

  it("offers no change to those who may not make one", async () => {
    fakeServer({
      [`GET ${path}`]: { body: { purpose: "backup", set: true, setBy: "dev@pve", setAt, canChange: false } },
    });
    render(
      <CredentialField
        label="Backup target keys"
        purpose="backup"
        cluster={{ namespace: "clusters", name: "one" }}
        keys={s3}
      />,
    );

    await expect(screen.findByText(/^Set on .* by dev@pve$/u)).resolves.toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("tells what is known of a stored credential", () => {
    expect(setText({ purpose: "dns01", set: false, canChange: true })).toBe("Not set");
    expect(setText({ purpose: "dns01", set: true, canChange: true })).toBe("Set");
    expect(setText({ purpose: "dns01", set: true, setBy: "admin@pve", canChange: true })).toBe("Set by admin@pve");
    expect(setText({ purpose: "dns01", set: true, setAt, canChange: true })).toMatch(/^Set on .*2026$/u);
  });
});
