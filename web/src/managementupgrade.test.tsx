import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { ApplianceUpgrades } from "./ApplianceUpgrades";
import { fakeServer, type Answer, type Route } from "./fake-server";

const saved = {
  nodes: ["pve1"],
  imageDatastore: "local",
  vmDatastore: "local-lvm",
  bridge: "vmbr0",
  nodeAddresses: ["192.0.2.11/24"],
  apiAddress: "192.0.2.10",
  gateway: "192.0.2.1",
  dnsServers: ["192.0.2.1"],
  workloadDatastores: ["local-lvm"],
  workloadBridges: ["vmbr0"],
  bootstrapDatastore: "local",
};

const behind = {
  applied: "0.1.0",
  carried: "0.2.0",
  behind: true,
  changes: [
    { component: "talos", from: "v1.14.4", to: "v1.15.0" },
    { component: "cluster_api", from: "v1.14.3", to: "v1.15.0" },
  ],
};

const administrationPage = (setup: unknown): Record<string, Route> => ({
  "GET /api/session": { body: { user: "root@pam", csrf: "c1" } },
  "GET /api/setup": { body: setup },
  "GET /api/appliance/upgrades": {
    body: { version: "0.2.0", available: [], settings: { policy: "off", hour: 3, timeZone: "UTC" } },
  },
  "GET /api/templates": { body: { templates: [] } },
});

describe("the management cluster behind the appliance", () => {
  it("says so and upgrades it with the passphrase, saying what changes", async () => {
    window.history.replaceState(null, "", "/administration");
    const calls = fakeServer({
      ...administrationPage({ appliance: true, ready: true, allowed: true, settings: saved, upgrade: behind }),
      "POST /api/setup": {
        status: 202,
        body: { user: "root@pam", kind: "apply", started: new Date().toISOString(), state: "running", log: [] },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<App />);

    await expect(
      screen.findByText("The management cluster runs what Kubehenge 0.1.0 applied; this appliance carries 0.2.0.", {
        exact: false,
      }),
    ).resolves.toBeInTheDocument();
    expect(screen.getByText("Kubehenge 0.1.0")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Upgrade the management cluster" }));
    const dialog = within(screen.getByRole("dialog", { name: "Upgrade the management cluster" }));
    expect(dialog.getByText("From what Kubehenge 0.1.0 applied to what 0.2.0 carries:")).toBeInTheDocument();
    const talos = dialog.getByRole("row", { name: "Talos v1.14.4 v1.15.0" });
    expect(talos).toBeInTheDocument();
    expect(dialog.getByRole("row", { name: "Cluster API v1.14.3 v1.15.0" })).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: "Upgrade" })).toBeDisabled();

    await user.type(dialog.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(dialog.getByRole("button", { name: "Upgrade" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "POST /api/setup")?.body).toStrictEqual({
        settings: saved,
        passphrase: "correct horse battery staple",
      }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("says nothing while it runs what this appliance carries", async () => {
    window.history.replaceState(null, "", "/administration");
    fakeServer(
      administrationPage({
        appliance: true,
        ready: true,
        allowed: true,
        settings: saved,
        upgrade: { applied: "0.2.0", carried: "0.2.0", behind: false },
      }),
    );
    render(<App />);
    await expect(screen.findByRole("heading", { name: "Management cluster" })).resolves.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Upgrade the management cluster" })).not.toBeInTheDocument();
  });

  it("tells of a failed automatic upgrade, and that it is tried again", async () => {
    window.history.replaceState(null, "", "/administration");
    const job = {
      user: "kubehenge-upgrader@pve",
      automatic: true,
      kind: "apply",
      started: "2026-10-14T04:00:00Z",
      finished: "2026-10-14T04:12:00Z",
      state: "failed",
      error: "tofu apply: exit status 1",
      log: ["Error: no such node"],
    };
    fakeServer(
      administrationPage({ appliance: true, ready: true, allowed: true, settings: saved, upgrade: behind, job }),
    );
    render(<App />);
    const alert = await screen.findByText(/The automatic upgrade of .* failed, and is tried again/u);
    expect(alert).toHaveTextContent("tofu apply: exit status 1");
    // The notice at the top of the page says so too.
    expect(screen.getByText(/Upgrading it automatically failed/u)).toBeInTheDocument();
    // Upgrading by hand stays open meanwhile.
    expect(screen.getByRole("button", { name: "Upgrade the management cluster" })).toBeInTheDocument();
  });
});

const upgrades = (management: unknown, policy = "off"): Answer => ({
  body: { version: "0.2.0", available: [], settings: { policy, hour: 3, timeZone: "UTC" }, management },
});

describe("automatic upgrades of the management cluster", () => {
  it("are switched on only once the dialog says what they grant, and off again", async () => {
    const browser = new Intl.DateTimeFormat().resolvedOptions().timeZone;
    const off = { possible: true, on: false, user: "kubehenge-upgrader@pve" };
    const on = { ...off, on: true, by: "root@pam", since: "2026-10-22T09:00:00Z" };
    const routes: Record<string, Route> = {
      "GET /api/appliance/upgrades": upgrades(off),
      "PUT /api/appliance/upgrades/management": () => {
        routes["GET /api/appliance/upgrades"] = upgrades(on);
        return { body: on };
      },
      "DELETE /api/appliance/upgrades/management": () => {
        routes["GET /api/appliance/upgrades"] = upgrades(off);
        return { body: off };
      },
      "PUT /api/appliance/upgrades/settings": (settings) => ({ body: settings }),
    };
    const calls = fakeServer(routes);
    const user = userEvent.setup({ delay: null });
    render(<ApplianceUpgrades />);

    const select = await screen.findByLabelText("Management cluster");
    expect(select).toHaveValue("off");
    expect(screen.queryByLabelText("At hour")).not.toBeInTheDocument();

    // Cancelled, nothing is kept.
    await user.selectOptions(select, "on");
    await user.click(
      within(screen.getByRole("dialog", { name: "Upgrade the management cluster automatically" })).getByRole("button", {
        name: "Cancel",
      }),
    );
    expect(calls.map((c) => c.key)).not.toContain("PUT /api/appliance/upgrades/management");
    expect(select).toHaveValue("off");

    await user.selectOptions(select, "on");
    const dialog = within(screen.getByRole("dialog", { name: "Upgrade the management cluster automatically" }));
    expect(dialog.getByText("kubehenge-upgrader@pve")).toBeInTheDocument();
    expect(dialog.getByText(/Administrator role/u)).toBeInTheDocument();
    expect(dialog.getByText(/Anyone who gets root on the appliance/u)).toHaveTextContent(
      "admin kubeconfig and talosconfig",
    );
    expect(dialog.getByRole("button", { name: "Keep them and switch on" })).toBeDisabled();
    await user.type(dialog.getByLabelText("State passphrase"), "correct horse battery staple");
    await user.click(dialog.getByRole("button", { name: "Keep them and switch on" }));
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PUT /api/appliance/upgrades/management")?.body).toStrictEqual({
        passphrase: "correct horse battery staple",
      }),
    );
    // Nothing else was timed, so the hour starts out in the browser's time zone.
    await waitFor(() =>
      expect(calls.find((c) => c.key === "PUT /api/appliance/upgrades/settings")?.body).toStrictEqual({
        policy: "off",
        hour: 3,
        timeZone: browser,
      }),
    );
    await waitFor(() => expect(screen.getByLabelText("Management cluster")).toHaveValue("on"));
    expect(screen.getByText(/Switched on .* by root@pam/u)).toBeInTheDocument();
    expect(screen.getByLabelText("At hour")).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText("Management cluster"), "off");
    await waitFor(() => expect(calls.map((c) => c.key)).toContain("DELETE /api/appliance/upgrades/management"));
    await waitFor(() => expect(screen.getByLabelText("Management cluster")).toHaveValue("off"));
  });

  it("show a wrong passphrase in the dialog", async () => {
    fakeServer({
      "GET /api/appliance/upgrades": upgrades({ possible: true, on: false, user: "kubehenge-upgrader@pve" }, "patch"),
      "PUT /api/appliance/upgrades/management": {
        status: 403,
        body: { error: "the passphrase does not open the management cluster's state" },
      },
    });
    const user = userEvent.setup({ delay: null });
    render(<ApplianceUpgrades />);
    await user.selectOptions(await screen.findByLabelText("Management cluster"), "on");
    const dialog = within(screen.getByRole("dialog", { name: "Upgrade the management cluster automatically" }));
    await user.type(dialog.getByLabelText("State passphrase"), "the wrong passphrase entirely");
    await user.click(dialog.getByRole("button", { name: "Keep them and switch on" }));
    await expect(
      dialog.findByText("the passphrase does not open the management cluster's state", { exact: false }),
    ).resolves.toBeInTheDocument();
  });

  it("are not offered before there is a management cluster", async () => {
    fakeServer({
      "GET /api/appliance/upgrades": upgrades({ possible: false, on: false, user: "kubehenge-upgrader@pve" }),
    });
    render(<ApplianceUpgrades />);
    await expect(screen.findByLabelText("Automatic upgrades")).resolves.toBeInTheDocument();
    expect(screen.queryByLabelText("Management cluster")).not.toBeInTheDocument();
  });
});
