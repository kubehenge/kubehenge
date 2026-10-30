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

  it("says an upgrade was rolled back, and why", async () => {
    const error = "release 0.2.0 did not start: the interface did not answer within 2m0s; rolled back to 0.1.0";
    const status = { state: "failed", from: "0.1.0", to: "0.2.0", time: "2027-03-01T12:00:00Z", error };
    const routes: Record<string, Route> = {
      "GET /api/appliance/upgrades": {
        body: {
          version: "0.1.0",
          available: ["0.2.0"],
          settings: { policy: "off", hour: 3, timeZone: "UTC" },
          status: { ...status, rolledBack: true },
        },
      },
    };
    fakeServer(routes);
    const { unmount } = render(<ApplianceUpgrades />);
    await expect(screen.findByText(/^Rolled back to 0\.1\.0/u)).resolves.toBeInTheDocument();
    expect(
      screen.getByText("Release 0.2.0 did not start: the interface did not answer within 2m0s."),
    ).toBeInTheDocument();
    unmount();

    routes["GET /api/appliance/upgrades"] = {
      body: { version: "0.1.0", available: [], settings: { policy: "off", hour: 3, timeZone: "UTC" }, status },
    };
    render(<ApplianceUpgrades />);
    await expect(screen.findByText(/^Failed \(/u)).resolves.toBeInTheDocument();
  });
});
