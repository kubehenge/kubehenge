import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { ApplianceUpgrades } from "./ApplianceUpgrades";
import { fakeServer, type Answer, type Route } from "./fake-server";

const upgrades = (settings: unknown): Answer => ({
  body: { version: "0.1.0", available: ["0.1.4", "0.2.0"], settings },
});

describe("appliance upgrades", () => {
  it("upgrades to the newest release and sets the automatic policy", async () => {
    const browser = new Intl.DateTimeFormat().resolvedOptions().timeZone;
    const routes: Record<string, Route> = {
      "GET /api/appliance/upgrades": upgrades({ policy: "off", hour: 3, timeZone: "UTC" }),
      "POST /api/appliance/upgrades": { status: 202 },
      "PUT /api/appliance/upgrades/settings": (settings) => {
        routes["GET /api/appliance/upgrades"] = upgrades(settings);
        return { body: settings };
      },
    };
    const calls = fakeServer(routes);
    const saved = (): unknown => calls.findLast((c) => c.key === "PUT /api/appliance/upgrades/settings")?.body;
    const user = userEvent.setup({ delay: null });
    render(<ApplianceUpgrades />);

    await expect(screen.findByRole("option", { name: "Patch releases (0.1.x)" })).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Upgrade to 0.2.0" }));
    expect(calls.map((c) => c.key)).not.toContain("POST /api/appliance/upgrades");
    await user.click(
      within(screen.getByRole("dialog", { name: "Upgrade Kubehenge to 0.2.0" })).getByRole("button", {
        name: "Upgrade",
      }),
    );
    await waitFor(() =>
      expect(calls.find((c) => c.key === "POST /api/appliance/upgrades")?.body).toStrictEqual({ version: "0.2.0" }),
    );
    // Asked for, the upgrade cannot be asked for again before the updater reports.
    await expect(screen.findByRole("button", { name: "Upgrading…" })).resolves.toBeDisabled();

    await user.selectOptions(screen.getByLabelText("Automatic upgrades"), "minor");
    await waitFor(() => expect(saved()).toStrictEqual({ policy: "minor", hour: 3, timeZone: browser }));
    await expect(screen.findByText("Saved")).resolves.toBeInTheDocument();
    await user.selectOptions(await screen.findByLabelText("Hour"), "4");
    await waitFor(() => expect(saved()).toStrictEqual({ policy: "minor", hour: 4, timeZone: browser }));
    await user.selectOptions(screen.getByLabelText("Time zone"), "Asia/Tokyo");
    await waitFor(() => expect(saved()).toStrictEqual({ policy: "minor", hour: 4, timeZone: "Asia/Tokyo" }));
  });

  it("offers nothing when the installed release is the newest", async () => {
    fakeServer({
      "GET /api/appliance/upgrades": {
        body: { version: "0.2.0", available: [], settings: { policy: "off", hour: 3, timeZone: "UTC" } },
      },
    });
    render(<ApplianceUpgrades />);
    await expect(screen.findByText("None")).resolves.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Upgrade to/u })).not.toBeInTheDocument();
  });
});
