import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { describe as describePolicy, windowProblems } from "./ClusterUpgrades";
import { fakeServer } from "./fake-server";

const cluster = {
  namespace: "clusters",
  name: "web",
  class: "kubehenge-rke2",
  distribution: "rke2",
  version: "v1.36.5+rke2r1",
  created: new Date().toISOString(),
  autoUpgraded: "v1.36.2+rke2r1 to v1.36.5+rke2r1 at 2026-10-13T03:00:00Z",
  deleting: false,
  phase: "Provisioned",
  available: true,
  conditions: [],
  controlPlane: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workers: { desired: 1, ready: 1, available: 1, upToDate: 1 },
  workerPools: [{ name: "md-0", class: "worker", replicas: 1 }],
  variables: {},
  pool: "/pool/kubehenge/clusters.web",
  allowed: { change: true, kubeconfig: false },
  machines: [],
};

describe("cluster upgrades", () => {
  it("describes a policy", () => {
    expect(describePolicy({})).toBe("Off");
    expect(describePolicy({ mode: "patch" })).toBe("Patch releases, every day from 02:00 for 4 h (UTC)");
    expect(
      describePolicy({
        mode: "minor",
        window: { weekdays: ["Sat"], monthDays: [1], start: "22:00", durationMinutes: 90, timeZone: "Europe/Berlin" },
      }),
    ).toBe("Patch and minor releases, Sat, day 1 of the month from 22:00 for 1.5 h (Europe/Berlin)");
  });

  it("sets a policy with a window", async () => {
    window.history.replaceState(null, "", "/clusters/clusters/web");
    const calls = fakeServer({
      "GET /api/session": { body: { user: "dev@pve", csrf: "c" } },
      "GET /api/clusters/clusters/web": { body: cluster },
      "PATCH /api/clusters/clusters/web": { body: cluster },
    });
    const sent = (): unknown => calls.find((c) => c.key === "PATCH /api/clusters/clusters/web")?.body;
    const user = userEvent.setup({ delay: null });
    render(<App />);
    await expect(screen.findByText(/v1.36.2\+rke2r1 to v1.36.5\+rke2r1, /u)).resolves.toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: "Options" }));
    expect(document.title).toBe("web: Options - Kubehenge");
    await user.click(screen.getByRole("button", { name: "Edit automatic upgrades" }));
    const dialog = screen.getByRole("dialog", { name: "Automatic upgrades" });
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.selectOptions(within(dialog).getByLabelText("Upgrade automatically"), "minor");
    await user.click(within(dialog).getByLabelText("Sat"));
    await user.click(within(dialog).getByLabelText("Mon"));
    await user.selectOptions(within(dialog).getByLabelText("Time zone"), "Europe/Berlin");
    await user.type(within(dialog).getByLabelText("Days of the month"), "32");
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled();
    expect(within(dialog).getByLabelText("Days of the month")).toHaveAttribute("aria-invalid", "true");
    await user.clear(within(dialog).getByLabelText("Days of the month"));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(sent()).toStrictEqual({
        // Weekdays in calendar order, whatever order they were ticked in.
        variables: {
          upgrades: {
            mode: "minor",
            window: { start: "02:00", durationMinutes: 240, timeZone: "Europe/Berlin", weekdays: ["Mon", "Sat"] },
          },
        },
      }),
    );
  });

  it("checks a window as the class will", () => {
    expect(windowProblems("1, 15", "4")).toStrictEqual({});
    expect(windowProblems("0", "4").monthDays).toBeDefined();
    expect(windowProblems("x", "4").monthDays).toBeDefined();
    expect(windowProblems("", "0").hours).toBeDefined();
    expect(windowProblems("", "").hours).toBeDefined();
    expect(windowProblems("", "1.25").hours).toBeDefined();
    expect(windowProblems("", "1.5")).toStrictEqual({});
  });
});
